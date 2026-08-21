"""Resource specifications and the string shorthands that parse into them.

All shorthands (`gpu="24GB"`, `memory="32GB"`, `timeout="8h"`) are parsed at
decoration time with precise errors, never at submit time.

Internal canonical units: memory in MiB, time in seconds.
"""

from __future__ import annotations

import re
from dataclasses import dataclass, field
from typing import Sequence, Union

from .exceptions import SpecError

_MEMORY_RE = re.compile(r"^\s*(\d+(?:\.\d+)?)\s*([a-zA-Z]+)\s*$")

# Decimal units are treated as their binary siblings: users who write "24GB"
# mean "the 24GB on the card", which vendors label decimal but allocators
# treat binary. One convention, applied consistently.
_MEMORY_UNITS_MIB = {
    "mb": 1,
    "mib": 1,
    "gb": 1024,
    "gib": 1024,
    "tb": 1024 * 1024,
    "tib": 1024 * 1024,
}

_TIME_RE = re.compile(r"^\s*(\d+(?:\.\d+)?)\s*([smhd]?)\s*$")
_TIME_UNITS_S = {"": 1, "s": 1, "m": 60, "h": 3600, "d": 86400}


def parse_memory(value: Union[str, int], *, what: str = "memory") -> int:
    """Parse a memory amount into MiB. Ints pass through as MiB."""
    if isinstance(value, bool):
        raise SpecError(f"{what}={value!r} is not a memory amount")
    if isinstance(value, int):
        if value <= 0:
            raise SpecError(f"{what}={value} must be positive (unit: MiB)")
        return value
    m = _MEMORY_RE.match(value)
    if not m:
        raise SpecError(
            f'{what}={value!r} is not a memory amount. Use e.g. "512MB", '
            f'"32GB", or an int of MiB.'
        )
    amount, unit = float(m.group(1)), m.group(2).lower()
    if unit not in _MEMORY_UNITS_MIB:
        raise SpecError(
            f"{what}={value!r} has unknown unit {m.group(2)!r}. "
            f"Known units: MB, GB, TB (or MiB/GiB/TiB)."
        )
    mib = int(amount * _MEMORY_UNITS_MIB[unit])
    if mib <= 0:
        raise SpecError(f"{what}={value!r} must be positive")
    return mib


def parse_timeout(value: Union[str, int, float], *, what: str = "timeout") -> int:
    """Parse a duration into whole seconds. Numbers pass through as seconds."""
    if isinstance(value, bool):
        raise SpecError(f"{what}={value!r} is not a duration")
    if isinstance(value, (int, float)):
        seconds = int(value)
    else:
        m = _TIME_RE.match(value)
        if not m:
            raise SpecError(
                f'{what}={value!r} is not a duration. Use e.g. "90s", "30m", '
                f'"8h", "2d", or a number of seconds.'
            )
        seconds = int(float(m.group(1)) * _TIME_UNITS_S[m.group(2)])
    if seconds <= 0:
        raise SpecError(f"{what}={value!r} must be positive")
    return seconds


def format_mib(mib: int) -> str:
    if mib % 1024 == 0:
        return f"{mib // 1024}GB"
    return f"{mib}MB"


@dataclass(frozen=True)
class Accelerator:
    """Base for accelerator requirements. Memory is a scheduler reservation,
    not a hardware-enforced partition."""

    memory_mib: int = 0
    count: int = 1

    kind: str = field(default="", init=False)

    def describe(self) -> str:
        mem = f" ≥{format_mib(self.memory_mib)}" if self.memory_mib else ""
        cnt = f" x{self.count}" if self.count != 1 else ""
        return f"{self.kind.upper()}{mem}{cnt}"


@dataclass(frozen=True)
class CUDA(Accelerator):
    kind: str = field(default="cuda", init=False)

    def __init__(self, memory: Union[str, int, None] = None, count: int = 1):
        object.__setattr__(
            self, "memory_mib", parse_memory(memory, what="CUDA memory") if memory else 0
        )
        if count < 1:
            raise SpecError(f"CUDA count={count} must be >= 1")
        object.__setattr__(self, "count", count)
        object.__setattr__(self, "kind", "cuda")


@dataclass(frozen=True)
class MPS(Accelerator):
    """Apple Silicon Metal Performance Shaders. Unified memory: the request
    draws from the machine's single memory pool."""

    kind: str = field(default="mps", init=False)

    def __init__(self, memory: Union[str, int, None] = None):
        object.__setattr__(
            self, "memory_mib", parse_memory(memory, what="MPS memory") if memory else 0
        )
        object.__setattr__(self, "count", 1)
        object.__setattr__(self, "kind", "mps")


# `gpu="24GB"` and `gpu=relay.GPU(...)` are shorthand for a CUDA requirement.
# NVIDIA is what "gpu" means to virtually every training workload today.
class GPU(CUDA):
    pass


@dataclass(frozen=True)
class AcceleratorSet:
    """An any-of set: the scheduler may satisfy the spec with any member."""

    options: tuple[Accelerator, ...]

    def describe(self) -> str:
        return " | ".join(o.describe() for o in self.options)


def any_of(*options: Accelerator) -> AcceleratorSet:
    """UNORDERED alternatives: the scheduler may satisfy the spec with ANY
    member and optimizes placement (cache locality, packing) across them.
    listing CUDA first expresses no preference. A strict-preference form
    would be a separate `prefer=` API."""
    if not options:
        raise SpecError("any_of() needs at least one accelerator option")
    for o in options:
        if not isinstance(o, Accelerator):
            raise SpecError(
                f"any_of() takes accelerator specs like relay.CUDA(...) or "
                f"relay.MPS(...), got {o!r}"
            )
    return AcceleratorSet(options=tuple(options))


def parse_gpu(value: Union[str, int, "GPU", CUDA, None]) -> CUDA | None:
    """Normalize the `gpu=` decorator argument to a CUDA requirement."""
    if value is None:
        return None
    if isinstance(value, CUDA):
        return value
    if isinstance(value, int) and not isinstance(value, bool):
        return GPU(count=value)
    if isinstance(value, str):
        v = value.strip().lower()
        if v in {"any", "true", "yes"}:
            return GPU()
        try:
            return GPU(memory=value)
        except SpecError:
            raise SpecError(
                f'gpu={value!r} is not understood. Use a memory amount like '
                f'gpu="24GB", a count like gpu=2, "any", or relay.GPU(...).'
            ) from None
    raise SpecError(f"gpu={value!r} is not a GPU spec")


@dataclass(frozen=True)
class Resources:
    """Fully-parsed resource requirements of one function/service."""

    cpu: float | None = None
    memory_mib: int | None = None
    accelerators: AcceleratorSet | None = None  # any-of; single specs wrap into a set
    timeout_s: int | None = None

    def describe(self) -> str:
        parts: list[str] = []
        if self.accelerators:
            parts.append(self.accelerators.describe())
        if self.cpu:
            parts.append(f"{self.cpu:g} CPU")
        if self.memory_mib:
            parts.append(f"{format_mib(self.memory_mib)} RAM")
        return ", ".join(parts) if parts else "no resource constraints"


def build_resources(
    *,
    cpu: Union[int, float, None] = None,
    memory: Union[str, int, None] = None,
    gpu: Union[str, int, GPU, CUDA, None] = None,
    accelerator: Union[Accelerator, AcceleratorSet, Sequence[Accelerator], None] = None,
    timeout: Union[str, int, float, None] = None,
) -> Resources:
    if gpu is not None and accelerator is not None:
        raise SpecError(
            "Pass either gpu= or accelerator=, not both. gpu= is shorthand "
            "for a CUDA requirement; accelerator= expresses alternatives "
            "like any_of(CUDA(...), MPS(...))."
        )
    if cpu is not None:
        if isinstance(cpu, bool) or not isinstance(cpu, (int, float)) or cpu <= 0:
            raise SpecError(f"cpu={cpu!r} must be a positive number of cores")

    acc_set: AcceleratorSet | None = None
    if gpu is not None:
        parsed = parse_gpu(gpu)
        if parsed is not None:
            acc_set = AcceleratorSet(options=(parsed,))
    elif accelerator is not None:
        if isinstance(accelerator, AcceleratorSet):
            acc_set = accelerator
        elif isinstance(accelerator, Accelerator):
            acc_set = AcceleratorSet(options=(accelerator,))
        elif isinstance(accelerator, Sequence):
            acc_set = any_of(*accelerator)
        else:
            raise SpecError(f"accelerator={accelerator!r} is not an accelerator spec")

    return Resources(
        cpu=float(cpu) if cpu is not None else None,
        memory_mib=parse_memory(memory) if memory is not None else None,
        accelerators=acc_set,
        timeout_s=parse_timeout(timeout) if timeout is not None else None,
    )
