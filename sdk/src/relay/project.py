"""Project-root discovery and canonical entry-module naming.

The project root is what gets mounted/shipped to runners and put on
sys.path; the entry module's dotted name is derived from its path relative
to that root. Client, CLI, and container all import the entry module under
the SAME name, so cloudpickle's by-reference classes resolve everywhere.
"""

from __future__ import annotations

from pathlib import Path

from .exceptions import SpecError

ROOT_MARKERS = (".relayignore", "pyproject.toml", ".git", "setup.py", "setup.cfg")


def find_project_root(start: Path) -> Path:
    """Walk up from `start` (a file's directory) to the nearest marker.

    Falls back to `start` itself when no marker exists, but never accepts
    the home directory or filesystem root, which would ship/mount far more
    than a project (SSH keys, credentials, everything).
    """
    start = start.resolve()
    home = Path.home().resolve()
    for candidate in (start, *start.parents):
        if candidate == home or candidate.parent == candidate:
            break
        if any((candidate / marker).exists() for marker in ROOT_MARKERS):
            return candidate
    if start == home or start.parent == start:
        raise SpecError(
            f"Refusing to treat {start} as a project root. It would expose "
            f"the entire directory to runners. Put your app in a project "
            f"folder (any folder with a .git, pyproject.toml, or an empty "
            f".relayignore file works)."
        )
    return start


def entry_module_name(source_file: Path, project_root: Path) -> str:
    """Dotted module name of `source_file` relative to the project root."""
    try:
        rel = source_file.resolve().relative_to(project_root.resolve())
    except ValueError:
        raise SpecError(
            f"{source_file} is outside its project root {project_root}. "
            f"this should not happen; move the file into the project."
        ) from None
    parts = list(rel.with_suffix("").parts)
    for part in parts:
        if not part.isidentifier():
            raise SpecError(
                f"Cannot import {rel} as a module: path segment {part!r} is "
                f"not a valid Python identifier. Rename the file/folder "
                f"(e.g. my-app.py → my_app.py)."
            )
    return ".".join(parts)
