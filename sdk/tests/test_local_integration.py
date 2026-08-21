"""End-to-end local-mode tests. Skipped when Docker is unavailable.

These are the M0 gate: decorator → container → result, logs, and error
propagation, against a real Docker daemon.
"""

import shutil
import subprocess
import textwrap
from pathlib import Path

import pytest

pytestmark = pytest.mark.timeout(600)


def _docker_up() -> bool:
    if shutil.which("docker") is None:
        return False
    proc = subprocess.run(
        ["docker", "version", "--format", "{{.Server.Version}}"],
        capture_output=True,
        text=True,
    )
    return proc.returncode == 0 and bool(proc.stdout.strip())


docker_required = pytest.mark.skipif(
    not _docker_up(), reason="Docker daemon not available"
)


@pytest.fixture(autouse=True)
def _force_local_mode(monkeypatch):
    """These tests exercise the embedded executor. A relayd server that
    happens to exist on the developer's machine must not hijack them.
    monkeypatch mutates os.environ, so CLI subprocesses inherit it too."""
    monkeypatch.setenv("RELAY_MODE", "local")

APP_FILE = textwrap.dedent(
    """
    from __future__ import annotations

    import dataclasses
    import time

    import relay

    app = relay.App("itest")

    @dataclasses.dataclass
    class Metrics:
        loss: float
        steps: int

    @app.function(cpu=1, memory="512MB")
    def echo(value: int, scale: int = 2) -> int:
        print(f"scaling {value} by {scale}")
        return value * scale

    @app.function()
    def boom() -> None:
        raise ValueError("intentional kaboom")

    @app.function()
    def uses_sibling() -> str:
        import helper
        return helper.greeting()

    @app.function()
    def roundtrip_class(m: Metrics) -> Metrics:
        return Metrics(loss=m.loss / 2, steps=m.steps + 1)

    @app.function(timeout="3s")
    def sleeper() -> None:
        time.sleep(60)
    """
)

HELPER_FILE = 'def greeting() -> str:\n    return "hello from sibling"\n'


@pytest.fixture(scope="module")
def project(tmp_path_factory) -> Path:
    root = tmp_path_factory.mktemp("itest_project")
    (root / "itest_app.py").write_text(APP_FILE)
    (root / "helper.py").write_text(HELPER_FILE)
    return root


def _load(project: Path):
    """Import the app the same way the CLI does: canonical module name with
    the project root on sys.path, so pickled classes must resolve both ways."""
    import importlib
    import sys

    if str(project) not in sys.path:
        sys.path.insert(0, str(project))
    sys.modules.pop("itest_app", None)
    return importlib.import_module("itest_app")


@docker_required
class TestLocalExecution:
    def test_remote_round_trip(self, project, capsys):
        m = _load(project)
        assert m.echo.remote(21) == 42
        assert "scaling 21 by 2" in capsys.readouterr().out

    def test_kwargs(self, project):
        m = _load(project)
        assert m.echo.remote(5, scale=10) == 50

    def test_error_propagates_with_remote_traceback(self, project):
        import relay

        m = _load(project)
        with pytest.raises(relay.RemoteFunctionError) as exc_info:
            m.boom.remote()
        assert exc_info.value.exc_type == "ValueError"
        assert "intentional kaboom" in str(exc_info.value)
        assert "remote traceback" in str(exc_info.value)

    def test_sibling_module_import(self, project):
        m = _load(project)
        assert m.uses_sibling.remote() == "hello from sibling"

    def test_spawn_handle(self, project):
        m = _load(project)
        run = m.echo.spawn(7)
        assert run.run_id.startswith("run_")
        assert run.result(timeout=300) == 14

    def test_local_call_untouched(self, project):
        m = _load(project)
        assert m.echo(3) == 6

    def test_user_defined_class_round_trip(self, project):
        """Regression for the M0 review HIGH: classes defined in the app
        module must survive arg AND result pickling."""
        m = _load(project)
        out = m.roundtrip_class.remote(m.Metrics(loss=1.0, steps=1))
        assert isinstance(out, m.Metrics)
        assert out.loss == 0.5 and out.steps == 2

    def test_timeout_kills_the_run(self, project):
        import relay

        m = _load(project)
        start = __import__("time").monotonic()
        with pytest.raises(relay.ExecutorError, match="timeout"):
            m.sleeper.remote()
        assert __import__("time").monotonic() - start < 45


@docker_required
def test_cli_run(project):
    proc = subprocess.run(
        [
            "relay",
            "run",
            str(project / "itest_app.py") + "::echo",
            "--value",
            "4",
            "--scale",
            "5",
        ],
        capture_output=True,
        text=True,
        timeout=600,
    )
    assert proc.returncode == 0, proc.stderr
    assert "20" in proc.stdout


@docker_required
def test_cli_missing_required_arg(project):
    proc = subprocess.run(
        ["relay", "run", str(project / "itest_app.py") + "::echo"],
        capture_output=True,
        text=True,
        timeout=120,
    )
    assert proc.returncode == 1
    assert "--value" in proc.stderr
