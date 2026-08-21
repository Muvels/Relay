"""The call/result wire format.

Envelope layout (RUNTIME_PROTOCOL 2):

    b"RLY1" | u32 big-endian header length | UTF-8 JSON header | payload

The header is plain JSON so protocol and Python-version guards run BEFORE
any unpickling, so an incompatible runtime reports the friendly mismatch
instead of choking on a pickle it can't read. The payload is cloudpickle
(arguments or a return value).

TRUST MODEL: deliberate and load-bearing. Unpickling executes code, so
results crossing back to the client are as trusted as the machines, images,
and code the user runs. Relay is a personal-fleet tool; the fleet is the
same trust domain as the laptop (Modal makes the same call). Strangers are
kept out by transport auth, not by sandboxing pickles.
"""

from __future__ import annotations

import json
import struct
import sys
from pathlib import Path
from typing import Any

import cloudpickle

from ..exceptions import RelayError, ResultTooLargeError

RUNTIME_PROTOCOL = 2
RESULT_LIMIT_BYTES = 16 * 1024 * 1024

CALL_FILENAME = "call.bin"
RESULT_FILENAME = "result.bin"

_MAGIC = b"RLY1"


def python_minor() -> str:
    return f"{sys.version_info.major}.{sys.version_info.minor}"


# ------------------------------------------------------------ envelope

def write_envelope(path: Path, header: dict[str, Any], payload: bytes) -> None:
    head = json.dumps(header, separators=(",", ":")).encode()
    tmp = path.with_name(path.name + ".tmp")
    with tmp.open("wb") as f:
        f.write(_MAGIC)
        f.write(struct.pack(">I", len(head)))
        f.write(head)
        f.write(payload)
    tmp.replace(path)


def read_envelope(path: Path) -> tuple[dict[str, Any], bytes]:
    with path.open("rb") as f:
        magic = f.read(4)
        if magic != _MAGIC:
            raise RelayError(
                f"{path.name} is not a Relay envelope (bad magic {magic!r}). "
                f"client and runtime are from incompatible Relay versions."
            )
        (head_len,) = struct.unpack(">I", f.read(4))
        header = json.loads(f.read(head_len).decode())
        payload = f.read()
    return header, payload


def _check_versions(header: dict[str, Any], *, side: str) -> None:
    proto = header.get("protocol")
    if proto != RUNTIME_PROTOCOL:
        raise RelayError(
            f"{side} uses runtime protocol {proto}, this Relay speaks "
            f"{RUNTIME_PROTOCOL}. Update client and fleet to the same "
            f"Relay version."
        )
    want, have = header.get("python"), python_minor()
    if want != have:
        raise RelayError(
            f"{side} was serialized on Python {want} but is being read on "
            f"{have}. Pin the image with Image.python({want!r}). The "
            f"default image already matches the client."
        )


# ------------------------------------------------------------ calls

def dump_call(
    path: Path,
    *,
    entry_module: str,
    function: str,
    args: tuple[Any, ...],
    kwargs: dict[str, Any],
) -> None:
    header = {
        "protocol": RUNTIME_PROTOCOL,
        "python": python_minor(),
        "cloudpickle": cloudpickle.__version__,
        "entry_module": entry_module,
        "function": function,
    }
    write_envelope(path, header, cloudpickle.dumps((args, kwargs)))


def dumps_call(
    *,
    entry_module: str,
    function: str,
    args: tuple[Any, ...],
    kwargs: dict[str, Any],
) -> bytes:
    """Envelope bytes for a call (the fleet path ships these as a blob)."""
    import io
    import struct as _struct

    header = {
        "protocol": RUNTIME_PROTOCOL,
        "python": python_minor(),
        "cloudpickle": cloudpickle.__version__,
        "entry_module": entry_module,
        "function": function,
    }
    head = json.dumps(header, separators=(",", ":")).encode()
    buf = io.BytesIO()
    buf.write(_MAGIC)
    buf.write(_struct.pack(">I", len(head)))
    buf.write(head)
    buf.write(cloudpickle.dumps((args, kwargs)))
    return buf.getvalue()


def load_result_bytes(data: bytes) -> dict[str, Any]:
    """load_result for in-memory envelope bytes (fleet result blobs)."""
    import io as _io
    import struct as _struct

    f = _io.BytesIO(data)
    if f.read(4) != _MAGIC:
        raise RelayError("result blob is not a Relay envelope")
    (head_len,) = _struct.unpack(">I", f.read(4))
    header = json.loads(f.read(head_len).decode())
    payload = f.read()
    _check_versions(header, side="the result")
    if header.get("ok"):
        header["value"] = cloudpickle.loads(payload)
    return header


def load_call_header(path: Path) -> dict[str, Any]:
    """Read only the header, which is safe before the entry module is imported."""
    header, _ = read_envelope(path)
    _check_versions(header, side="the call")
    return header


def load_call_args(path: Path) -> tuple[tuple[Any, ...], dict[str, Any]]:
    """Unpickle the arguments. Call ONLY after the entry module is imported
    under its canonical name, so by-reference classes resolve."""
    _, payload = read_envelope(path)
    return cloudpickle.loads(payload)


# ------------------------------------------------------------ results

def _result_header(ok: bool, **extra: Any) -> dict[str, Any]:
    return {
        "protocol": RUNTIME_PROTOCOL,
        "python": python_minor(),
        "cloudpickle": cloudpickle.__version__,
        "ok": ok,
        **extra,
    }


def dump_result(path: Path, value: Any) -> None:
    payload = cloudpickle.dumps(value)
    if len(payload) > RESULT_LIMIT_BYTES:
        raise ResultTooLargeError(len(payload), RESULT_LIMIT_BYTES)
    write_envelope(path, _result_header(True), payload)


def dump_error(path: Path, exc_type: str, message: str, tb: str) -> None:
    write_envelope(
        path,
        _result_header(False, exc_type=exc_type, message=message, traceback=tb),
        b"",
    )


def load_result(path: Path) -> dict[str, Any]:
    """Returns {"ok": True, "value": ...} or the error header dict."""
    header, payload = read_envelope(path)
    _check_versions(header, side="the result")
    if header.get("ok"):
        header["value"] = cloudpickle.loads(payload)
    return header
