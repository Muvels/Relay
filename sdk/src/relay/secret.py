"""Named secrets, injected as environment variables.

A Secret's name IS the env var the function sees (`Secret("HF_TOKEN")` →
`os.environ["HF_TOKEN"]`).

Fleet mode: values live encrypted on the server (`relay secret set
HF_TOKEN`), are released to an agent only for runs that declare them, and
never appear in run records or images.

Local mode: values come from the client's own environment. Export
HF_TOKEN=... before running. Explicit, no hidden vault.
"""

from __future__ import annotations

import re
from dataclasses import dataclass

from .exceptions import SpecError

_NAME_RE = re.compile(r"^[A-Z][A-Z0-9_]{0,63}$")


@dataclass(frozen=True)
class Secret:
    name: str

    def __post_init__(self) -> None:
        if not _NAME_RE.match(self.name):
            raise SpecError(
                f"Secret name {self.name!r} must be UPPER_SNAKE_CASE (it is "
                f'injected as an env var of exactly that name, e.g. '
                f'Secret("HF_TOKEN")).'
            )


def normalize_secrets(secrets: list["Secret | str"] | None) -> list[str]:
    if not secrets:
        return []
    out = []
    for s in secrets:
        out.append((s if isinstance(s, Secret) else Secret(str(s))).name)
    return out
