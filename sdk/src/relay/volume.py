"""Machine-local named volumes.

A Volume is a named directory that persists across runs ON THE MACHINE THAT
RAN THEM (~/.relay/volumes/<name> locally; the agent state dir on fleet
machines). Volumes are not replicated between machines, so checkpoints written
on dgx-spark live on dgx-spark. Pin `target=` when a follow-up run must see
a previous run's volume contents.
"""

from __future__ import annotations

import re
from dataclasses import dataclass

from .exceptions import SpecError

_NAME_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._\-]*$")


@dataclass(frozen=True)
class Volume:
    name: str

    def __post_init__(self) -> None:
        if not _NAME_RE.match(self.name):
            raise SpecError(
                f"Volume name {self.name!r} must be alphanumeric with . _ - "
                f'(e.g. Volume("gemma-ckpts")).'
            )


def normalize_volumes(volumes: dict[str, "Volume | str"] | None) -> dict[str, str]:
    """decorator input → {mount_path: volume_name} with validation."""
    if not volumes:
        return {}
    import posixpath

    out: dict[str, str] = {}
    for mount, vol in volumes.items():
        if not isinstance(mount, str) or not mount.startswith("/"):
            raise SpecError(
                f"volumes mount point {mount!r} must be an absolute container "
                f'path like "/checkpoints"'
            )
        clean = posixpath.normpath(mount)  # collapse //, /./ and /../
        reserved = ("/workspace", "/relay/io", "/opt/relay")
        conflict = (
            clean == "/"
            or clean in reserved
            or any((r + "/").startswith(clean + "/") for r in reserved)
            or any(clean.startswith(t + "/") for t in ("/relay", "/opt/relay"))
        )
        if conflict:
            raise SpecError(
                f"volumes mount point {mount!r} collides with a Relay-reserved path"
            )
        v = vol if isinstance(vol, Volume) else Volume(str(vol))
        out[clean] = v.name
    return out
