"""M0/M1 embedded local executor: run one decorated function in a local
Docker container, stream its logs, and return its result.

Deliberately the smallest thing that makes the DX real. The fleet path
(relayd agent, Go) implements the same io-dir + invoke protocol; only the
transport differs.

Timeouts are enforced by an in-process watchdog that force-removes the
container at the deadline for both `.remote()` and `.spawn()` (the
watchdog lives as long as this Python process; the fleet path enforces
timeouts server-side regardless of any client).
"""

from __future__ import annotations

import json
import secrets
import sys
import threading
import time
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterator

import relay

from ..exceptions import ExecutorError, RemoteFunctionError
from ..image import Image
from ..resources import Resources
from ..runtime import serialize
from ..runtime.invoke import USER_ERROR_EXIT
from . import docker

RUNS_DIR = Path.home() / ".relay" / "runs"

_UNSET = object()


@dataclass(frozen=True)
class CallSpec:
    function_name: str
    entry_module: str
    project_dir: Path
    image: Image
    resources: Resources
    args: tuple[Any, ...]
    kwargs: dict[str, Any]
    app_name: str = "app"
    target_names: tuple[str, ...] | list[str] = ()
    volumes: dict[str, str] | None = None  # mount path → volume name
    secrets: list[str] | None = None       # env-var names


def _new_run_id() -> str:
    return f"run_{int(time.time()):x}{secrets.token_hex(3)}"


def _relay_pkg_root() -> Path:
    """Directory containing the `relay` package, mounted at /opt/relay so
    containers need no Relay install."""
    return Path(relay.__file__).resolve().parent.parent


def _docker_args(spec: CallSpec, run_id: str, io_dir: Path) -> list[str]:
    args = [
        "--name", f"relay-{run_id}",
        "--entrypoint", "python3",  # registry images keep their own otherwise
        "-v", f"{io_dir}:/relay/io",
        "-v", f"{spec.project_dir.resolve()}:/workspace:ro",
        "-v", f"{_relay_pkg_root()}:/opt/relay:ro",
        "-e", "PYTHONPATH=/opt/relay",
        "-e", f"RELAY_RUN_ID={run_id}",
        # cwd starts outside /workspace (shadowing guard); invoke chdirs in.
        "--workdir", "/relay/io",
    ]
    for mount, name in (spec.volumes or {}).items():
        vol_dir = Path.home() / ".relay" / "volumes" / name
        vol_dir.mkdir(parents=True, exist_ok=True)
        args += ["-v", f"{vol_dir}:{mount}"]
    # Local mode resolves secrets from the client's own environment.
    import os as _os

    for name in spec.secrets or []:
        if name not in _os.environ:
            raise ExecutorError(
                f"{spec.function_name} declares Secret({name!r}) but local "
                f"mode reads it from YOUR environment. Export {name}=... "
                f"(fleet mode uses `relay secret set {name}` instead)."
            )
        args += ["-e", f"{name}={_os.environ[name]}"]
    if spec.resources.cpu:
        args += [f"--cpus={spec.resources.cpu:g}"]
    if spec.resources.memory_mib:
        args += [f"--memory={spec.resources.memory_mib}m"]

    wants_cuda = spec.resources.accelerators is not None and any(
        acc.kind == "cuda" for acc in spec.resources.accelerators.options
    )
    if wants_cuda:
        if docker.host_nvidia_available():
            args += ["--gpus", "all"]
        else:
            print(
                f"⚠ relay: {spec.function_name} asks for "
                f"{spec.resources.accelerators.describe()}, but LOCAL mode "
                f"on this machine has no CUDA. Running without a GPU. "
                f"(Local mode never enforces resource specs; the fleet "
                f"scheduler does.)",
                file=sys.stderr,
            )
    args += [spec.image.tag(), "-m", "relay.runtime.invoke", "/relay/io"]
    return args


def _ensure_image(image: Image) -> None:
    tag = image.tag()
    if docker.image_exists(tag):
        return
    print(f"→ building image {tag} (first run for this definition)", file=sys.stderr)
    docker.build_image(tag, image.dockerfile(), on_line=lambda l: print(f"  {l}", file=sys.stderr))
    print(f"✓ image {tag} ready", file=sys.stderr)


class _Watchdog:
    """Force-removes the container when the deadline passes."""

    def __init__(self, container: str, timeout_s: int | None):
        self.fired = False
        self._timer: threading.Timer | None = None
        if timeout_s:
            def fire() -> None:
                self.fired = True
                docker.force_remove(container)

            self._timer = threading.Timer(timeout_s, fire)
            self._timer.daemon = True
            self._timer.start()

    def cancel(self) -> None:
        if self._timer is not None:
            self._timer.cancel()


def _read_result(io_dir: Path, exit_code: int, run_id: str) -> Any:
    result_path = io_dir / serialize.RESULT_FILENAME
    if exit_code in (0, USER_ERROR_EXIT) and result_path.exists():
        result = serialize.load_result(result_path)
        if result["ok"]:
            return result["value"]
        raise RemoteFunctionError(
            result["exc_type"], result["message"], result["traceback"]
        )
    raise ExecutorError(
        f"Run {run_id} exited with code {exit_code} before producing a "
        f"result. This is a runtime failure, not your function raising. "
        f"Check the log output above; io dir kept at {io_dir.parent}."
    )


class LocalRun:
    """Handle for a spawned local run. Mirrors the fleet Run API surface.

    Terminal state is persisted to meta.json, so status()/result() keep
    answering after the container is gone.
    """

    def __init__(self, run_id: str, container_id: str, io_dir: Path,
                 spec: CallSpec, watchdog: _Watchdog):
        self.run_id = run_id
        self._container = container_id
        self._io_dir = io_dir
        self._spec = spec
        self._watchdog = watchdog
        self._result: Any = _UNSET

    # -- metadata ---------------------------------------------------------

    @property
    def _meta_path(self) -> Path:
        return self._io_dir.parent / "meta.json"

    def _load_meta(self) -> dict:
        try:
            return json.loads(self._meta_path.read_text())
        except (OSError, json.JSONDecodeError):
            return {}

    def _save_meta(self, **fields: Any) -> None:
        meta = self._load_meta()
        meta.update(fields)
        self._meta_path.write_text(json.dumps(meta))

    # -- public surface ---------------------------------------------------

    def status(self) -> str:
        if state := self._load_meta().get("state"):
            return state
        state = docker.inspect_state(self._container)
        if not state:
            return "unknown"
        if state.get("Running"):
            return "running"
        code = state.get("ExitCode", -1)
        if code == 0:
            return "succeeded"
        if code == USER_ERROR_EXIT:
            return "failed"
        return f"error({code})"

    def logs(self, follow: bool = True) -> Iterator[str]:
        return docker.stream_logs(self._container, follow=follow)

    def result(self, timeout: float | None = None) -> Any:
        """Wait for and return the function's result.

        `timeout` bounds the WAIT, not the run: on expiry the run keeps
        going and this raises TimeoutError (cancel() to kill it). The run's
        own `timeout=` from the decorator is enforced by the watchdog.
        """
        if self._result is not _UNSET:
            return self._result
        exit_code = docker.wait(self._container, timeout_s=timeout)
        self._watchdog.cancel()
        if self._watchdog.fired:
            self._save_meta(state="error", detail="timeout")
            raise ExecutorError(
                f"Run {self.run_id} exceeded its "
                f"{self._spec.resources.timeout_s}s timeout and was killed."
            )
        try:
            value = _read_result(self._io_dir, exit_code, self.run_id)
        except RemoteFunctionError:
            self._save_meta(state="failed", exit_code=exit_code)
            docker.force_remove(self._container)
            raise
        except ExecutorError:
            self._save_meta(state="error", exit_code=exit_code)
            docker.force_remove(self._container)
            raise
        self._save_meta(state="succeeded", exit_code=0)
        docker.force_remove(self._container)
        self._result = value
        return value

    def cancel(self) -> None:
        self._watchdog.cancel()
        self._save_meta(state="canceled")
        docker.force_remove(self._container)


class LocalExecutor:
    def _prepare(self, spec: CallSpec) -> tuple[str, Path]:
        docker.ensure_docker()
        _ensure_image(spec.image)
        run_id = _new_run_id()
        io_dir = RUNS_DIR / run_id / "io"
        io_dir.mkdir(parents=True, exist_ok=True)
        serialize.dump_call(
            io_dir / serialize.CALL_FILENAME,
            entry_module=spec.entry_module,
            function=spec.function_name,
            args=spec.args,
            kwargs=spec.kwargs,
        )
        return run_id, io_dir

    def run_sync(self, spec: CallSpec) -> Any:
        run_id, io_dir = self._prepare(spec)
        print(
            f"→ {spec.function_name} on local-docker "
            f"[{spec.resources.describe()}] ({run_id})",
            file=sys.stderr,
        )
        container = docker.run_detached(_docker_args(spec, run_id, io_dir))
        watchdog = _Watchdog(container, spec.resources.timeout_s)
        try:
            for line in docker.stream_logs(container):
                print(line)
            exit_code = docker.wait(container, timeout_s=60)
            watchdog.cancel()
            if watchdog.fired:
                raise ExecutorError(
                    f"Run {run_id} exceeded its "
                    f"{spec.resources.timeout_s}s timeout and was killed."
                )
            return _read_result(io_dir, exit_code, run_id)
        finally:
            watchdog.cancel()
            docker.force_remove(container)

    def spawn(self, spec: CallSpec) -> LocalRun:
        run_id, io_dir = self._prepare(spec)
        container = docker.run_detached(_docker_args(spec, run_id, io_dir))
        watchdog = _Watchdog(container, spec.resources.timeout_s)
        print(f"→ spawned {spec.function_name} ({run_id})", file=sys.stderr)
        return LocalRun(run_id, container, io_dir, spec, watchdog)
