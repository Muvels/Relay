"""Chained, immutable Image builder.

Each method returns a new Image; the op list compiles deterministically to a
Dockerfile whose sha256 names the built image. Identical compiled output
never rebuilds, and ANY change to compilation (pins, bootstrap steps)
changes the tag.

Base selection:
  - `Image.python("3.12")` → `python:3.12-slim` (exact interpreter minor,
    which the pickle protocol depends on).
  - `Image.debian("12").python("3.12")` → `python:3.12-slim-bookworm`
    (`.python()` works both as constructor and as chained modifier).
  - `Image.debian("12")` alone → `debian:12-slim` + distro python3.
  - `Image.from_registry(ref)` → your image; must provide `python3`.
"""

from __future__ import annotations

import hashlib
import re
import shlex
import sys
from dataclasses import dataclass, field
from typing import Any

from .exceptions import SpecError

# Exact pin: the client's own cloudpickle goes into every image, so pickles
# never cross cloudpickle versions. Imported lazily to keep import cheap.
def _cloudpickle_pin() -> str:
    import cloudpickle

    return f"cloudpickle=={cloudpickle.__version__}"


# Bump when the container-side invoke protocol changes shape.
# This is mirrored in runtime/serialize.py and kept here to avoid a heavy import.
RUNTIME_PROTOCOL = 2

_DEBIAN_CODENAMES = {"11": "bullseye", "12": "bookworm", "13": "trixie"}

_ENV_NAME_RE = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*$")
# Conservative image-reference charset: registry/name[:tag][@digest]
_IMAGE_REF_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._\-/:@+]*$")
_WORKDIR_RE = re.compile(r"^[A-Za-z0-9._\-/]+$")


def client_python_minor() -> str:
    return f"{sys.version_info.major}.{sys.version_info.minor}"


class _PythonMethod:
    """`.python()` dispatches on how it's accessed: on the class it's a
    constructor, on an instance it's a chained modifier."""

    def __get__(self, instance: "Image | None", owner: type):
        if instance is None:
            def construct(version: str | None = None) -> "Image":
                return Image(_ops=(("base_python", _check_py(version)),))

            return construct

        def modify(version: str | None = None) -> "Image":
            return instance._add("base_python", _check_py(version))

        return modify


@dataclass(frozen=True)
class Image:
    """Immutable image definition. Chain modifiers; never mutate."""

    _ops: tuple[tuple[str, Any], ...] = field(default_factory=tuple)

    python = _PythonMethod()

    # ---------------- constructors ----------------

    @classmethod
    def debian(cls, version: str = "12") -> "Image":
        if version not in _DEBIAN_CODENAMES:
            raise SpecError(
                f"Image.debian({version!r}): supported versions are "
                f"{', '.join(sorted(_DEBIAN_CODENAMES))}"
            )
        return cls(_ops=(("base_debian", version),))

    @classmethod
    def from_registry(cls, ref: str) -> "Image":
        """Start from an arbitrary registry image. It must contain a
        `python3` matching your client's minor version (Relay verifies at
        run time and names the fix if it doesn't)."""
        if not ref or not _IMAGE_REF_RE.match(ref):
            raise SpecError(
                f"Image.from_registry({ref!r}): not a valid image reference"
            )
        return cls(_ops=(("base_ref", ref),))

    # ---------------- chained modifiers ----------------

    def _add(self, op: str, value: Any) -> "Image":
        return Image(_ops=self._ops + ((op, value),))

    def apt(self, *packages: str) -> "Image":
        _check_names("apt", packages)
        return self._add("apt", tuple(packages))

    def pip(self, *packages: str) -> "Image":
        _check_names("pip", packages)
        return self._add("pip", tuple(packages))

    def run(self, *commands: str) -> "Image":
        """Arbitrary shell steps are intentionally uninspected. This is the
        power-user escape hatch."""
        if not commands:
            raise SpecError("Image.run() needs at least one command")
        return self._add("run", tuple(commands))

    def env(self, **variables: str) -> "Image":
        if not variables:
            raise SpecError("Image.env() needs at least one VAR=value")
        for name, value in variables.items():
            if not _ENV_NAME_RE.match(name):
                raise SpecError(f"Image.env(): {name!r} is not a valid env var name")
            if any(c in str(value) for c in "\n\r"):
                raise SpecError(f"Image.env(): value of {name!r} contains a newline")
        return self._add("env", tuple(sorted(variables.items())))

    def workdir(self, path: str) -> "Image":
        if not _WORKDIR_RE.match(path):
            raise SpecError(f"Image.workdir({path!r}): not a plain path")
        return self._add("workdir", path)

    # Alias parity with Modal muscle memory.
    apt_install = apt
    pip_install = pip

    # ---------------- compilation ----------------

    def _bases(self) -> tuple[str | None, str | None, str | None]:
        base_python = base_debian = base_ref = None
        for op, value in self._ops:
            if op == "base_python":
                base_python = value
            elif op == "base_debian":
                base_debian = value
            elif op == "base_ref":
                base_ref = value
        return base_python, base_debian, base_ref

    def _base(self) -> str:
        base_python, base_debian, base_ref = self._bases()
        if base_ref:
            if base_python:
                raise SpecError(
                    "Image.from_registry(...).python(...) is contradictory. "
                    "the registry image brings its own interpreter."
                )
            return base_ref
        if base_python and base_debian:
            return f"python:{base_python}-slim-{_DEBIAN_CODENAMES[base_debian]}"
        if base_python:
            return f"python:{base_python}-slim"
        if base_debian:
            return f"debian:{base_debian}-slim"
        return f"python:{client_python_minor()}-slim"

    def dockerfile(self) -> str:
        lines = [f"FROM {self._base()}"]
        lines.append("ENV PYTHONUNBUFFERED=1 PIP_DISABLE_PIP_VERSION_CHECK=1")

        base_python, base_debian, base_ref = self._bases()
        if base_debian and not base_python and not base_ref:
            lines.append(
                "RUN apt-get update && apt-get install -y --no-install-recommends "
                "python3 python3-pip && rm -rf /var/lib/apt/lists/*"
            )

        for op, value in self._ops:
            if op == "apt":
                pkgs = " ".join(shlex.quote(p) for p in value)
                lines.append(
                    f"RUN apt-get update && apt-get install -y --no-install-recommends "
                    f"{pkgs} && rm -rf /var/lib/apt/lists/*"
                )
            elif op == "pip":
                pkgs = " ".join(shlex.quote(p) for p in value)
                lines.append(
                    f"RUN python3 -m pip install --no-cache-dir "
                    f"--break-system-packages {pkgs} "
                    f"|| python3 -m pip install --no-cache-dir {pkgs}"
                )
            elif op == "run":
                for cmd in value:
                    lines.append(f"RUN {cmd}")
            elif op == "env":
                pairs = " ".join(f"{k}={shlex.quote(v)}" for k, v in value)
                lines.append(f"ENV {pairs}")
            elif op == "workdir":
                lines.append(f"WORKDIR {value}")

        # The runtime contract: the client's exact cloudpickle, /workspace cwd.
        pin = shlex.quote(_cloudpickle_pin())
        lines.append(
            f"RUN python3 -m pip install --no-cache-dir "
            f"--break-system-packages {pin} "
            f"|| python3 -m pip install --no-cache-dir {pin}"
        )
        lines.append("WORKDIR /workspace")
        return "\n".join(lines) + "\n"

    def native_spec(self) -> dict:
        """How and whether this image can run without a container. Apple
        Silicon MPS jobs execute natively, so only pip-installable images
        qualify. apt/run steps and registry bases are Docker-only."""
        supported = True
        python: str | None = None
        pip: list[str] = []
        env: dict[str, str] = {}
        for op, value in self._ops:
            if op == "base_python":
                python = value
            elif op == "pip":
                pip.extend(value)
            elif op == "env":
                env.update(dict(value))
            elif op in ("apt", "run", "base_ref"):
                supported = False
        return {
            "supported": supported,
            "python_minor": python or client_python_minor(),
            "pip_packages": [*pip, _cloudpickle_pin()],
            "env": env,
        }

    def content_hash(self) -> str:
        """Hash of the compiled Dockerfile and protocol. Any change to how
        compilation works retags automatically."""
        h = hashlib.sha256()
        h.update(f"protocol={RUNTIME_PROTOCOL}\n".encode())
        h.update(self.dockerfile().encode())
        return h.hexdigest()[:16]

    def tag(self) -> str:
        return f"relay-img:{self.content_hash()}"

    def describe(self) -> str:
        return self._base() + "".join(
            f" +{op}({', '.join(map(str, v)) if isinstance(v, tuple) else v})"
            for op, v in self._ops
            if not op.startswith("base_")
        )


def default_image() -> Image:
    return Image.python()


def _check_py(version: str | None) -> str:
    v = version or client_python_minor()
    parts = v.split(".")
    if len(parts) != 2 or not all(p.isdigit() for p in parts):
        raise SpecError(f'Image.python({version!r}): expected a version like "3.12"')
    return v


def _check_names(what: str, packages: tuple[str, ...]) -> None:
    if not packages:
        raise SpecError(f"Image.{what}() needs at least one package")
    for p in packages:
        if not p or p != p.strip() or any(c in p for c in "\n\r\t"):
            raise SpecError(f"Image.{what}(): invalid package name {p!r}")
