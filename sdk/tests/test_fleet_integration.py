"""End-to-end fleet tests: relayd server + relayd agent + Docker on this
machine, driven through the public SDK surface. Skipped without Docker/Go.

This is the M1 gate: submit → schedule → agent builds/runs container →
logs stream back → result round-trips.
"""

import shutil
import socket
import subprocess
import textwrap
import time
import urllib.error
import urllib.request
from pathlib import Path

import pytest

pytestmark = pytest.mark.timeout(900)

REPO = Path(__file__).resolve().parents[2]


def _docker_up() -> bool:
    if shutil.which("docker") is None:
        return False
    proc = subprocess.run(
        ["docker", "version", "--format", "{{.Server.Version}}"],
        capture_output=True, text=True,
    )
    return proc.returncode == 0 and bool(proc.stdout.strip())


fleet_required = pytest.mark.skipif(
    not (_docker_up() and shutil.which("go")),
    reason="fleet e2e needs Docker and Go",
)

APP_FILE = textwrap.dedent(
    """
    from __future__ import annotations

    import dataclasses
    import time

    import relay

    app = relay.App("ftest")

    @dataclasses.dataclass
    class Point:
        x: int
        y: int

    @app.function(cpu=1, memory="512MB")
    def multiply(value: int, by: int = 3) -> int:
        print(f"fleet multiply {value} x {by}")
        return value * by

    @app.function()
    def mirror(p: Point) -> Point:
        return Point(x=p.y, y=p.x)

    @app.function()
    def explode() -> None:
        raise RuntimeError("fleet kaboom")

    @app.function()
    def napper() -> None:
        print("napping", flush=True)
        time.sleep(120)

    @app.function()
    def slow_double(x: int) -> int:
        import time as _t
        print("slow_double started", flush=True)
        _t.sleep(6)
        return x * 2

    @app.function(volumes={"/state": relay.Volume("ftest-counter")})
    def bump() -> int:
        from pathlib import Path
        p = Path("/state/count.txt")
        n = int(p.read_text()) + 1 if p.exists() else 1
        p.write_text(str(n))
        return n

    @app.function(target="testbox")
    def on_testbox() -> str:
        import socket
        return socket.gethostname()

    @app.function(gpu="9999GB")
    def impossible() -> None:
        pass

    @app.function(secrets=[relay.Secret("FTEST_TOKEN")])
    def whoami() -> str:
        import os
        return os.environ["FTEST_TOKEN"]

    @app.function(accelerator=relay.MPS(memory="1GB"))
    def on_metal() -> dict:
        import os, platform, sys
        return {
            "system": platform.system(),
            "machine": platform.machine(),
            "in_container": os.path.exists("/.dockerenv"),
            "executable": sys.executable,
            "workspace": os.environ.get("RELAY_WORKSPACE", ""),
        }

    @app.service(port=8901, cpu=1, memory="256MB")
    def pinger():
        from http.server import BaseHTTPRequestHandler, HTTPServer

        class H(BaseHTTPRequestHandler):
            def do_GET(self):
                self.send_response(200)
                self.end_headers()
                self.wfile.write(b"pong")

            def log_message(self, *args):
                pass

        print("pinger listening", flush=True)
        HTTPServer(("0.0.0.0", 8901), H).serve_forever()
    """
)


def _free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


@pytest.fixture(scope="module")
def relayd_bin(tmp_path_factory) -> Path:
    out = tmp_path_factory.mktemp("bin") / "relayd"
    subprocess.run(
        ["go", "build", "-o", str(out), "./cmd/relayd"],
        cwd=REPO / "relayd", check=True, timeout=300,
    )
    return out


@pytest.fixture(scope="module")
def cluster(relayd_bin, tmp_path_factory):
    """A live server + one joined agent on this machine."""
    root = tmp_path_factory.mktemp("cluster")
    http_port, grpc_port = _free_port(), _free_port()
    server_url = f"http://127.0.0.1:{http_port}"

    server_args = [str(relayd_bin), "server",
                   "--data-dir", str(root / "server"),
                   "--http", f"127.0.0.1:{http_port}",
                   "--grpc", f"127.0.0.1:{grpc_port}"]

    def wait_healthy():
        deadline = time.monotonic() + 30
        while True:
            try:
                urllib.request.urlopen(f"{server_url}/v1/health", timeout=2)
                return
            except Exception:
                if time.monotonic() > deadline:
                    raise RuntimeError("server never became healthy")
                time.sleep(0.3)

    procs = {"server": subprocess.Popen(
        server_args, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)}
    try:
        wait_healthy()

        def stop_server():
            procs["server"].terminate()
            procs["server"].wait(timeout=10)

        def start_server():
            procs["server"] = subprocess.Popen(
                server_args, stdout=subprocess.PIPE,
                stderr=subprocess.STDOUT, text=True)
            wait_healthy()

        token = (root / "server" / "api_token").read_text().strip()

        req = urllib.request.Request(
            f"{server_url}/v1/join-tokens", data=b"{}", method="POST",
            headers={"Authorization": f"Bearer {token}"},
        )
        import json
        join = json.loads(urllib.request.urlopen(req, timeout=10).read())["token"]

        procs["agent"] = subprocess.Popen(
            [str(relayd_bin), "agent",
             "--join", f"{join}@127.0.0.1:{grpc_port}",
             "--name", "testbox",
             "--state-dir", str(root / "agent")],
            stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True,
        )

        req = urllib.request.Request(
            f"{server_url}/v1/machines",
            headers={"Authorization": f"Bearer {token}"},
        )
        deadline = time.monotonic() + 30
        while True:
            machines = json.loads(urllib.request.urlopen(req, timeout=5).read())["machines"]
            if any(m["online"] for m in machines):
                break
            if time.monotonic() > deadline:
                raise RuntimeError(f"agent never came online: {machines}")
            time.sleep(0.5)

        yield {"url": server_url, "token": token,
               "stop_server": stop_server, "start_server": start_server}
    finally:
        for p in procs.values():
            p.terminate()
        for p in procs.values():
            try:
                p.wait(timeout=10)
            except subprocess.TimeoutExpired:
                p.kill()


@pytest.fixture()
def fleet_env(cluster, monkeypatch):
    monkeypatch.setenv("RELAY_SERVER", cluster["url"])
    monkeypatch.setenv("RELAY_TOKEN", cluster["token"])
    monkeypatch.delenv("RELAY_MODE", raising=False)
    return cluster


@pytest.fixture(scope="module")
def project(tmp_path_factory) -> Path:
    root = tmp_path_factory.mktemp("ftest_project")
    (root / "ftest_app.py").write_text(APP_FILE)
    return root


def _load(project: Path):
    import importlib
    import sys

    if str(project) not in sys.path:
        sys.path.insert(0, str(project))
    sys.modules.pop("ftest_app", None)
    return importlib.import_module("ftest_app")


@fleet_required
class TestFleetExecution:
    def test_remote_round_trip(self, fleet_env, project, capsys):
        m = _load(project)
        assert m.multiply.remote(7) == 21
        assert "fleet multiply 7 x 3" in capsys.readouterr().out

    def test_user_class_round_trip(self, fleet_env, project):
        m = _load(project)
        out = m.mirror.remote(m.Point(x=1, y=2))
        assert isinstance(out, m.Point) and (out.x, out.y) == (2, 1)

    def test_error_propagates(self, fleet_env, project):
        import relay

        m = _load(project)
        with pytest.raises(relay.RemoteFunctionError) as exc_info:
            m.explode.remote()
        assert exc_info.value.exc_type == "RuntimeError"
        assert "fleet kaboom" in str(exc_info.value)

    def test_spawn_status_logs_cancel(self, fleet_env, project):
        m = _load(project)
        run = m.napper.spawn()
        deadline = time.monotonic() + 120
        while run.status() not in ("running",):
            assert time.monotonic() < deadline, f"stuck in {run.status()}"
            time.sleep(0.5)
        run.cancel()
        deadline = time.monotonic() + 60
        while run.status() not in ("canceled", "lost", "error"):
            assert time.monotonic() < deadline
            time.sleep(0.5)
        assert run.status() == "canceled"
        assert any("napping" in line for line in run.logs(follow=False))

    def test_volume_persists_across_runs(self, fleet_env, project):
        m = _load(project)
        first = m.bump.remote()
        assert m.bump.remote() == first + 1

    def test_explicit_target(self, fleet_env, project):
        m = _load(project)
        assert isinstance(m.on_testbox.remote(), str)

    def test_impossible_request_queues_with_explanation(self, fleet_env, project):
        m = _load(project)
        run = m.impossible.spawn()
        deadline = time.monotonic() + 30
        detail = ""
        while time.monotonic() < deadline:
            info = run.info()
            detail = info.get("detail", "")
            if info["state"] == "pending" and detail.startswith("queued:"):
                break
            time.sleep(0.5)
        assert detail.startswith("queued:"), f"detail: {detail!r}"
        # The per-machine reason names the machine and the actionable cause
        # (exact wording depends on this host: CUDA/MPS mismatch on a Mac,
        # memory shortfall on a CUDA box).
        assert "testbox ✗" in detail, f"detail: {detail!r}"
        run.cancel()
        deadline = time.monotonic() + 20
        while run.status() != "canceled" and time.monotonic() < deadline:
            time.sleep(0.5)
        assert run.status() == "canceled"

    def test_secret_injection(self, fleet_env, project):
        from relay._fleet.client import RelayClient
        from relay.config import resolve_server

        client = RelayClient(resolve_server())
        client.set_secret("FTEST_TOKEN", "s3cr3t-value")
        assert "FTEST_TOKEN" in client.list_secrets()

        m = _load(project)
        assert m.whoami.remote() == "s3cr3t-value"

        # Values must never be readable back over HTTP or appear in run rows.
        import relay._fleet.client as c

        with pytest.raises(c.ServerError):
            client._request("GET", "/v1/secrets/FTEST_TOKEN")
        runs = client.list_runs(limit=10)
        assert all("s3cr3t-value" not in str(r) for r in runs)

    def test_missing_secret_fails_with_fix(self, fleet_env, project):
        from relay._fleet.client import RelayClient
        from relay.config import resolve_server

        RelayClient(resolve_server()).delete_secret("FTEST_TOKEN")
        import relay

        m = _load(project)
        with pytest.raises(relay.ExecutorError, match="relay secret set"):
            m.whoami.remote()

    @pytest.mark.skipif(
        not (shutil.which("uv") and __import__("platform").machine() == "arm64"
             and __import__("platform").system() == "Darwin"),
        reason="MPS native execution needs Apple Silicon + uv",
    )
    def test_mps_runs_natively_not_in_container(self, fleet_env, project):
        m = _load(project)
        out = m.on_metal.remote()
        assert out["system"] == "Darwin" and out["machine"] == "arm64"
        assert out["in_container"] is False, "MPS must not run in Docker"
        assert "/venvs/" in out["executable"], out["executable"]
        assert out["workspace"], "native runs pass RELAY_WORKSPACE"

    def test_service_deploy_serve_replace_stop(self, fleet_env, project):
        from relay._fleet.client import RelayClient
        from relay.config import resolve_server

        client = RelayClient(resolve_server())
        m = _load(project)

        out = m.pinger.deploy()
        run_id = out["run"]["id"]
        assert out["run"]["kind"] == "service"

        deadline = time.monotonic() + 300
        endpoint = ""
        while time.monotonic() < deadline:
            info = client.get_run(run_id)
            if info["state"] == "running" and info.get("endpoint"):
                endpoint = info["endpoint"]
                break
            assert info["state"] not in ("error", "failed", "lost"), info
            time.sleep(1)
        assert endpoint, "service never reported an endpoint"

        # Key-gated by default, even for private exposure (public ≠ open,
        # and private ≠ open either).
        with pytest.raises(urllib.error.HTTPError) as httperr:
            urllib.request.urlopen(f"http://{endpoint}", timeout=10)
        assert httperr.value.code == 401

        req = urllib.request.Request(
            f"http://{endpoint}",
            headers={"Authorization": f"Bearer {out['service_key']}"},
        )
        body = urllib.request.urlopen(req, timeout=10).read()
        assert body == b"pong"

        listed = client.list_services()
        assert any(s["id"] == run_id and s["state"] == "running" for s in listed)

        # Redeploy replaces the live generation.
        out2 = m.pinger.deploy()
        assert out2["replaced"] == 1
        run2 = out2["run"]["id"]
        deadline = time.monotonic() + 300
        while time.monotonic() < deadline:
            if client.get_run(run2)["state"] == "running" and \
               client.get_run(run_id)["state"] in ("canceled", "lost"):
                break
            time.sleep(1)
        assert client.get_run(run_id)["state"] in ("canceled", "lost")

        client.cancel_run(run2)
        deadline = time.monotonic() + 60
        while client.get_run(run2)["state"] != "canceled" and time.monotonic() < deadline:
            time.sleep(0.5)
        assert client.get_run(run2)["state"] == "canceled"

    def test_map_fans_out_in_order(self, fleet_env, project):
        m = _load(project)
        assert list(m.multiply.map([1, 2, 3])) == [3, 6, 9]

    def test_shell_exec_on_machine(self, fleet_env, project):
        import json

        from relay._fleet.client import RelayClient
        from relay.config import resolve_server

        client = RelayClient(resolve_server())
        resp = client._request(
            "POST", "/v1/machines/testbox/exec",
            body=json.dumps({"argv": ["echo", "hello-from-host"]}).encode(),
            stream=True, timeout=60,
        )
        with resp:
            out = resp.read().decode()
        assert "hello-from-host" in out
        assert "\x00exit 0" in out

    def test_schedule_register_list_remove(self, fleet_env, project):
        import json

        from relay._fleet.client import RelayClient
        from relay._fleet.executor import FleetExecutor
        from relay.config import resolve_server

        m = _load(project)
        server = resolve_server()
        FleetExecutor(server).register_schedule(
            m.multiply._call_spec((2,), {}), "*/5 * * * *")

        client = RelayClient(server)
        listed = client._request("GET", "/v1/schedules")["schedules"]
        assert any(s["function"] == "multiply" and s["cron"] == "*/5 * * * *"
                   for s in listed)

        client._request("DELETE", "/v1/schedules/ftest/multiply")
        listed = client._request("GET", "/v1/schedules")["schedules"]
        assert not any(s["function"] == "multiply" for s in listed)

    def test_dashboard_serves(self, fleet_env):
        html = urllib.request.urlopen(
            fleet_env["url"] + "/", timeout=10).read().decode()
        assert "<title>Relay</title>" in html

    def test_run_survives_server_outage(self, fleet_env, project):
        """The daily-driver flow: submit a run, the control plane goes away
        mid-run (laptop off), the run FINISHES while it is gone, the server
        comes back, and the result must still arrive."""
        from relay._fleet.client import RelayClient
        from relay.config import resolve_server

        m = _load(project)
        run = m.slow_double.spawn(21)

        client = RelayClient(resolve_server())
        deadline = time.monotonic() + 120
        while client.get_run(run.run_id)["state"] != "running":
            assert time.monotonic() < deadline, client.get_run(run.run_id)
            time.sleep(0.5)

        fleet_env["stop_server"]()
        time.sleep(9)  # the 6s function completes with the server DOWN
        fleet_env["start_server"]()

        deadline = time.monotonic() + 120
        while time.monotonic() < deadline:
            info = client.get_run(run.run_id)
            if info["state"] == "succeeded":
                break
            assert info["state"] not in ("error", "failed", "lost"), info
            time.sleep(1)
        assert client.get_run(run.run_id)["state"] == "succeeded"
        assert run.result(timeout=30) == 42

    def test_ps_shows_history(self, fleet_env, project):
        from relay._fleet.client import RelayClient
        from relay.config import resolve_server

        runs = RelayClient(resolve_server()).list_runs(limit=50)
        assert any(r["function"] == "multiply" and r["state"] == "succeeded"
                   for r in runs)
        assert all(r["machine"] in ("testbox", "", "-") for r in runs)
