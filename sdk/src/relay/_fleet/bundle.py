"""Project code bundles: deterministic tar.gz of the project root.

Determinism matters because the sha256 is the cache key on server and agents, so
an unchanged project must always produce the identical archive (sorted
entries, zeroed timestamps/owners).

The SDK embeds its own source under `_relay_sdk/` inside every bundle, so
containers need no Relay preinstalled; the agent sets
PYTHONPATH=/workspace/_relay_sdk. (Constant mirrored in the Go agent.)
"""

from __future__ import annotations

import fnmatch
import gzip
import hashlib
import io
import tarfile
from pathlib import Path

import relay

SDK_BUNDLE_DIR = "_relay_sdk"

DEFAULT_EXCLUDES = [
    ".git",
    ".hg",
    ".venv",
    "venv",
    "node_modules",
    "__pycache__",
    "*.pyc",
    ".pytest_cache",
    ".mypy_cache",
    ".ruff_cache",
    ".relay",
    ".DS_Store",
    SDK_BUNDLE_DIR,
]

MAX_BUNDLE_BYTES = 256 * 1024 * 1024


def _load_ignore_patterns(root: Path) -> list[str]:
    patterns = list(DEFAULT_EXCLUDES)
    ignore = root / ".relayignore"
    if ignore.exists():
        for line in ignore.read_text().splitlines():
            line = line.strip()
            if line and not line.startswith("#"):
                patterns.append(line.rstrip("/"))
    return patterns


def _excluded(rel: Path, patterns: list[str]) -> bool:
    for pattern in patterns:
        for part in rel.parts:
            if fnmatch.fnmatch(part, pattern):
                return True
        if fnmatch.fnmatch(str(rel), pattern):
            return True
    return False


def _add_tree(tar: tarfile.TarFile, src_root: Path, arc_prefix: str,
              patterns: list[str]) -> int:
    """Add a directory tree with normalized metadata. Returns bytes added.
    Symlinks are skipped because agents refuse them due to workspace-escape risk."""
    total = 0
    entries = sorted(
        p for p in src_root.rglob("*")
        if not _excluded(p.relative_to(src_root), patterns)
    )
    for path in entries:
        rel = path.relative_to(src_root)
        arcname = f"{arc_prefix}{rel.as_posix()}"
        if path.is_symlink():
            continue
        if path.is_dir():
            info = tarfile.TarInfo(arcname + "/")
            info.type = tarfile.DIRTYPE
            info.mode = 0o755
            tar.addfile(info)
        elif path.is_file():
            info = tarfile.TarInfo(arcname)
            info.size = path.stat().st_size
            info.mode = 0o755 if path.stat().st_mode & 0o100 else 0o644
            total += info.size
            with path.open("rb") as f:
                tar.addfile(info, f)
    return total


def build_bundle(project_root: Path) -> tuple[bytes, str]:
    """Returns (tar.gz bytes, sha256)."""
    patterns = _load_ignore_patterns(project_root)
    buf = io.BytesIO()
    # mtime=0 in the gzip header keeps the archive deterministic.
    with gzip.GzipFile(fileobj=buf, mode="wb", mtime=0) as gz:
        with tarfile.open(fileobj=gz, mode="w") as tar:
            total = _add_tree(tar, project_root, "", patterns)
            if total > MAX_BUNDLE_BYTES:
                raise_too_big(project_root, total)
            sdk_root = Path(relay.__file__).resolve().parent
            _add_tree(tar, sdk_root, f"{SDK_BUNDLE_DIR}/relay/", DEFAULT_EXCLUDES)
    data = buf.getvalue()
    return data, hashlib.sha256(data).hexdigest()


def raise_too_big(root: Path, total: int) -> None:
    from ..exceptions import SpecError

    raise SpecError(
        f"Project bundle would be {total / 1e6:.0f} MB (limit "
        f"{MAX_BUNDLE_BYTES / 1e6:.0f} MB). Your project root {root} likely "
        f"contains data/checkpoints. Add those directories to .relayignore; "
        f"large inputs belong in volumes, not code bundles."
    )
