"""Fleet executor: run functions on the relayd fleet via the control plane.

Same call surface as the local executor; the transport is HTTP to the
server, which schedules onto a connected agent.
"""

from __future__ import annotations

import hashlib
import sys
import time
from typing import Any, Iterator

from ..config import ServerConfig
from ..exceptions import ExecutorError, RemoteFunctionError
from ..runtime import serialize
from .bundle import build_bundle
from .client import RelayClient

TERMINAL_STATES = {"succeeded", "failed", "error", "canceled", "lost"}
_POLL_S = 1.0


class FleetRun:
    """Handle for a run on the fleet. Everything reads from the server, so
    handles work from any process, unlike local spawn handles."""

    def __init__(self, client: RelayClient, run_id: str):
        self._client = client
        self.run_id = run_id

    def status(self) -> str:
        return self._client.get_run(self.run_id)["state"]

    def info(self) -> dict[str, Any]:
        return self._client.get_run(self.run_id)

    def logs(self, follow: bool = True) -> Iterator[str]:
        return self._client.stream_logs(self.run_id, follow=follow)

    def result(self, timeout: float | None = None) -> Any:
        deadline = time.monotonic() + timeout if timeout else None
        while True:
            run = self._client.get_run(self.run_id)
            if run["state"] in TERMINAL_STATES:
                return _settle(self._client, run)
            if deadline and time.monotonic() > deadline:
                raise TimeoutError(
                    f"run {self.run_id} still {run['state']} after {timeout}s "
                    f"(the run keeps going; relay stop {self.run_id} to kill it)"
                )
            time.sleep(_POLL_S)

    def cancel(self) -> None:
        self._client.cancel_run(self.run_id)


def _settle(client: RelayClient, run: dict[str, Any]) -> Any:
    state = run["state"]
    if state in ("succeeded", "failed"):
        if not run.get("result_sha256"):
            raise ExecutorError(
                f"run {run['id']} is {state} but has no result blob. "
                f"server/agent inconsistency, check `relay logs {run['id']}`"
            )
        result = serialize.load_result_bytes(client.get_blob(run["result_sha256"]))
        if result["ok"]:
            return result["value"]
        raise RemoteFunctionError(
            result["exc_type"], result["message"], result["traceback"]
        )
    raise ExecutorError(
        f"run {run['id']} ended {state}"
        + (f": {run['detail']}" if run.get("detail") else "")
    )


class FleetExecutor:
    def __init__(self, server: ServerConfig):
        self.client = RelayClient(server)

    def _submit(self, spec) -> FleetRun:  # spec: _local.executor.CallSpec
        run = self.client.submit_run(self._spec_payload(spec))
        return FleetRun(self.client, run["id"])

    def _spec_payload(self, spec) -> dict[str, Any]:
        bundle_bytes, bundle_sha = build_bundle(spec.project_dir)
        self.client.ensure_blob(bundle_sha, bundle_bytes)
        call_bytes = serialize.dumps_call(
            entry_module=spec.entry_module,
            function=spec.function_name,
            args=spec.args,
            kwargs=spec.kwargs,
        )
        call_sha = hashlib.sha256(call_bytes).hexdigest()
        self.client.ensure_blob(call_sha, call_bytes)
        accelerators = []
        if spec.resources.accelerators:
            accelerators = [
                {"kind": a.kind, "memory_mib": a.memory_mib, "count": a.count}
                for a in spec.resources.accelerators.options
            ]
        return {
            "app": spec.app_name,
            "function": spec.function_name,
            "entry_file": spec.entry_module,
            "cpus": spec.resources.cpu or 0,
            "memory_mib": spec.resources.memory_mib or 0,
            "timeout_s": spec.resources.timeout_s or 0,
            "accelerators": accelerators,
            "target_names": list(spec.target_names or []),
            "volumes": spec.volumes or {},
            "secret_names": list(spec.secrets or []),
            "image_tag": spec.image.tag(),
            "image_dockerfile": spec.image.dockerfile(),
            "native_env": spec.image.native_spec(),
            "bundle_sha256": bundle_sha,
            "args_sha256": call_sha,
            "runtime_protocol": serialize.RUNTIME_PROTOCOL,
            "python_minor": serialize.python_minor(),
        }

    def register_schedule(self, spec, cron: str) -> None:
        """Upsert a cron schedule for a zero-arg function call."""
        self.client._request(
            "POST", "/v1/schedules",
            body=__import__("json").dumps({
                "cron": cron, "spec": self._spec_payload(spec),
            }).encode(),
        )

    def deploy_service(self, spec, *, port: int, expose: str, auth_none: bool) -> dict[str, Any]:
        payload = self._spec_payload(spec)
        payload.update({
            "kind": "service",
            "service_port": port,
            "expose": expose,
            "auth_none": auth_none,
        })
        return self.client.deploy_service(payload)

    def spawn(self, spec) -> FleetRun:
        run = self._submit(spec)
        print(f"→ submitted {spec.function_name} ({run.run_id})", file=sys.stderr)
        return run

    def run_sync(self, spec) -> Any:
        run = self._submit(spec)
        print(
            f"→ {spec.function_name} on fleet [{spec.resources.describe()}] "
            f"({run.run_id})",
            file=sys.stderr,
        )
        for line in run.logs(follow=True):
            print(line)
        return run.result()
