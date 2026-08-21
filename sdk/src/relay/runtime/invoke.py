"""Container-side entrypoint: run one call and write one result.

Contract (RUNTIME_PROTOCOL 2):
  - argv[1] is an io directory containing call.bin
  - the project root is mounted at /workspace; entry modules import under
    their natural dotted names with /workspace on sys.path
  - result.bin is written next to call.bin

Ordering matters: header checks → import entry module → unpickle args.
Arguments may reference classes defined in the entry module, so the module
must exist under its canonical name before deserialization.

Exit codes: 0 success, 13 the user function raised (result.bin has the
details), anything else is an infrastructure failure.
"""

from __future__ import annotations

import importlib
import os
import sys
import traceback
from pathlib import Path

from ..exceptions import ResultTooLargeError
from . import serialize

USER_ERROR_EXIT = 13


def _resolve_callable(module, function: str):
    obj = getattr(module, function, None)
    if obj is None:
        raise AttributeError(
            f"module {module.__name__!r} has no attribute {function!r}"
        )
    # Decorated functions arrive as RelayFunction wrappers; call the
    # underlying user function, never re-enter Relay inside a container.
    raw = getattr(obj, "raw", None)
    if callable(raw) and getattr(obj, "_is_relay_function", False):
        return raw
    if not callable(obj):
        raise TypeError(f"{function!r} is not callable")
    return obj


def main(argv: list[str]) -> int:
    if len(argv) != 2:
        print("usage: python3 -m relay.runtime.invoke <io-dir>", file=sys.stderr)
        return 2
    io_dir = Path(argv[1])
    # The process starts with cwd OUTSIDE the workspace so a project-level
    # relay.py can't shadow this runtime; user code still sees the workspace
    # as cwd. Containers mount it at /workspace; native (MPS) runs pass the
    # real path via RELAY_WORKSPACE.
    workspace = os.environ.get("RELAY_WORKSPACE", "/workspace")
    if Path(workspace).is_dir():
        os.chdir(workspace)
    if workspace not in sys.path:
        sys.path.insert(0, workspace)

    header = serialize.load_call_header(io_dir / serialize.CALL_FILENAME)
    module = importlib.import_module(header["entry_module"])
    fn = _resolve_callable(module, header["function"])
    args, kwargs = serialize.load_call_args(io_dir / serialize.CALL_FILENAME)

    result_path = io_dir / serialize.RESULT_FILENAME
    try:
        value = fn(*args, **kwargs)
    except BaseException as exc:  # noqa: BLE001 (forwarding is the point)
        serialize.dump_error(
            result_path,
            exc_type=type(exc).__name__,
            message=str(exc),
            tb="".join(traceback.format_exception(exc)),
        )
        return USER_ERROR_EXIT

    try:
        serialize.dump_result(result_path, value)
    except ResultTooLargeError as exc:
        serialize.dump_error(
            result_path, exc_type="ResultTooLargeError", message=str(exc), tb=""
        )
        return USER_ERROR_EXIT
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
