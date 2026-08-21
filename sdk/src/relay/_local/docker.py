"""Thin wrapper over the docker CLI for local mode.

Shelling out (rather than docker-py) keeps BuildKit available and the
dependency surface zero; the real fleet path speaks the Engine API from the
Go agent (M1+), so this wrapper stays local-mode-only.

Every call is bounded: no subprocess here can hang the SDK forever, and
implementation exceptions surface as ExecutorError.
"""

from __future__ import annotations

import json
import platform
import shutil
import subprocess
from typing import Callable, Iterator

from ..exceptions import DockerNotAvailableError, ExecutorError

_QUICK = 30  # seconds, for anything that should be instant


def _run(args: list[str], timeout: float | None = _QUICK) -> subprocess.CompletedProcess:
    try:
        return subprocess.run(
            ["docker", *args], capture_output=True, text=True, timeout=timeout
        )
    except subprocess.TimeoutExpired as exc:
        raise ExecutorError(
            f"`docker {args[0]}` did not answer within {timeout}s. The "
            f"Docker daemon looks wedged."
        ) from exc
    except OSError as exc:
        raise DockerNotAvailableError(str(exc)) from exc


def ensure_docker() -> None:
    if shutil.which("docker") is None:
        raise DockerNotAvailableError("`docker` CLI not found on PATH")
    proc = _run(["version", "--format", "{{.Server.Version}}"])
    if proc.returncode != 0 or not proc.stdout.strip():
        detail = proc.stderr.strip().splitlines()[-1] if proc.stderr.strip() else "daemon not reachable"
        raise DockerNotAvailableError(detail)


def image_exists(tag: str) -> bool:
    return _run(["image", "inspect", tag]).returncode == 0


def build_image(tag: str, dockerfile: str, on_line: Callable[[str], None]) -> None:
    """Build from a stdin Dockerfile with an empty context (we never COPY)."""
    proc = subprocess.Popen(
        ["docker", "build", "--progress=plain", "-t", tag, "-"],
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
    )
    assert proc.stdin is not None and proc.stdout is not None
    try:
        proc.stdin.write(dockerfile)
        proc.stdin.close()
        for line in proc.stdout:
            on_line(line.rstrip("\n"))
    finally:
        proc.stdout.close()
        if proc.poll() is None:
            proc.kill()
        proc.wait()
    if proc.returncode != 0:
        raise ExecutorError(
            f"Image build failed for {tag}. Scroll up for the failing step; "
            f"the Dockerfile is generated from your Image definition."
        )


def host_nvidia_available() -> bool:
    """Local-mode GPU heuristic: Linux with nvidia-smi and the container
    toolkit runtime registered."""
    if platform.system() != "Linux" or shutil.which("nvidia-smi") is None:
        return False
    proc = _run(["info", "--format", "{{json .Runtimes}}"])
    if proc.returncode != 0:
        return False
    try:
        return "nvidia" in json.loads(proc.stdout or "{}")
    except json.JSONDecodeError:
        return False


def run_detached(args: list[str]) -> str:
    proc = _run(["run", "-d", *args], timeout=300)
    if proc.returncode != 0:
        raise ExecutorError(f"docker run failed: {proc.stderr.strip()}")
    return proc.stdout.strip()


def stream_logs(container_id: str, follow: bool = True) -> Iterator[str]:
    cmd = ["docker", "logs"]
    if follow:
        cmd.append("-f")
    cmd.append(container_id)
    proc = subprocess.Popen(
        cmd, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True
    )
    assert proc.stdout is not None
    try:
        yield from (line.rstrip("\n") for line in proc.stdout)
    finally:
        proc.stdout.close()
        if proc.poll() is None:
            proc.kill()
        proc.wait()


def wait(container_id: str, timeout_s: float | None = None) -> int:
    try:
        proc = subprocess.run(
            ["docker", "wait", container_id],
            capture_output=True, text=True, timeout=timeout_s,
        )
    except subprocess.TimeoutExpired:
        raise TimeoutError(
            f"still waiting on container {container_id[:12]} after "
            f"{timeout_s}s"
        ) from None
    except OSError as exc:
        raise ExecutorError(f"docker wait failed: {exc}") from exc
    if proc.returncode != 0:
        raise ExecutorError(f"docker wait failed: {proc.stderr.strip()}")
    try:
        return int(proc.stdout.strip())
    except ValueError:
        raise ExecutorError(
            f"docker wait returned {proc.stdout.strip()!r}, not an exit code"
        ) from None


def inspect_state(container_id: str) -> dict:
    proc = _run(["inspect", "--format", "{{json .State}}", container_id])
    if proc.returncode != 0:
        return {}
    try:
        return json.loads(proc.stdout)
    except json.JSONDecodeError:
        return {}


def force_remove(container_id: str) -> bool:
    return _run(["rm", "-f", container_id]).returncode == 0
