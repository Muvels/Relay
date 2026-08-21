"""Envelope + project-root contract tests (the M0-review fixes)."""

from pathlib import Path

import pytest

from relay.exceptions import RelayError, SpecError
from relay.project import entry_module_name, find_project_root
from relay.runtime import serialize


class TestEnvelope:
    def test_call_round_trip(self, tmp_path: Path):
        p = tmp_path / "call.bin"
        serialize.dump_call(
            p, entry_module="pkg.app", function="train",
            args=(1, "x"), kwargs={"lr": 0.1},
        )
        header = serialize.load_call_header(p)
        assert header["entry_module"] == "pkg.app"
        assert header["function"] == "train"
        assert header["protocol"] == serialize.RUNTIME_PROTOCOL
        args, kwargs = serialize.load_call_args(p)
        assert args == (1, "x") and kwargs == {"lr": 0.1}

    def test_header_readable_without_unpickling(self, tmp_path: Path):
        """Version guards must run before any pickle bytes are touched."""
        p = tmp_path / "call.bin"
        serialize.dump_call(
            p, entry_module="m", function="f", args=(), kwargs={},
        )
        header, payload = serialize.read_envelope(p)
        assert isinstance(header, dict) and isinstance(payload, bytes)

    def test_bad_magic(self, tmp_path: Path):
        p = tmp_path / "junk.bin"
        p.write_bytes(b"NOPE" + b"\x00" * 16)
        with pytest.raises(RelayError, match="bad magic"):
            serialize.read_envelope(p)

    def test_result_round_trip(self, tmp_path: Path):
        p = tmp_path / "result.bin"
        serialize.dump_result(p, {"loss": 0.5})
        out = serialize.load_result(p)
        assert out["ok"] and out["value"] == {"loss": 0.5}

    def test_error_round_trip(self, tmp_path: Path):
        p = tmp_path / "result.bin"
        serialize.dump_error(p, "ValueError", "boom", "tb...")
        out = serialize.load_result(p)
        assert not out["ok"]
        assert out["exc_type"] == "ValueError"

    def test_python_mismatch_names_the_fix(self, tmp_path: Path):
        p = tmp_path / "call.bin"
        serialize.write_envelope(
            p,
            {"protocol": serialize.RUNTIME_PROTOCOL, "python": "2.7"},
            b"",
        )
        with pytest.raises(RelayError, match="Image.python"):
            serialize.load_call_header(p)

    def test_protocol_mismatch(self, tmp_path: Path):
        p = tmp_path / "call.bin"
        serialize.write_envelope(p, {"protocol": 999, "python": "3.12"}, b"")
        with pytest.raises(RelayError, match="protocol"):
            serialize.load_call_header(p)


class TestProjectRoot:
    def test_marker_discovery(self, tmp_path: Path):
        (tmp_path / "pyproject.toml").touch()
        nested = tmp_path / "src" / "pkg"
        nested.mkdir(parents=True)
        assert find_project_root(nested) == tmp_path

    def test_fallback_to_start(self, tmp_path: Path):
        d = tmp_path / "loose"
        d.mkdir()
        assert find_project_root(d) == d

    def test_home_directory_refused(self):
        with pytest.raises(SpecError, match="Refusing"):
            find_project_root(Path.home())

    def test_entry_module_nested(self, tmp_path: Path):
        (tmp_path / "pkg").mkdir()
        f = tmp_path / "pkg" / "train.py"
        f.touch()
        assert entry_module_name(f, tmp_path) == "pkg.train"

    def test_entry_module_bad_identifier(self, tmp_path: Path):
        f = tmp_path / "my-app.py"
        f.touch()
        with pytest.raises(SpecError, match="my_app"):
            entry_module_name(f, tmp_path)
