"""HTTP client for the relayd control plane. Stdlib-only (urllib)."""

from __future__ import annotations

import json
import urllib.error
import urllib.request
from typing import Any, Iterator

from ..config import ServerConfig
from ..exceptions import RelayError


class ServerError(RelayError):
    pass


_DEFAULT_TIMEOUT = object()  # sentinel: `None` must mean "no timeout"


class RelayClient:
    def __init__(self, server: ServerConfig, timeout: float = 30.0):
        self.server = server
        self.timeout = timeout

    def _request(self, method: str, path: str, *, body: bytes | None = None,
                 content_type: str = "application/json",
                 stream: bool = False, timeout=_DEFAULT_TIMEOUT):
        req = urllib.request.Request(
            self.server.url + path, data=body, method=method,
            headers={
                "Authorization": f"Bearer {self.server.token}",
                "Content-Type": content_type,
            },
        )
        effective = self.timeout if timeout is _DEFAULT_TIMEOUT else timeout
        try:
            resp = urllib.request.urlopen(  # noqa: S310 (fixed scheme/host)
                req, timeout=effective)
        except urllib.error.HTTPError as exc:
            detail = ""
            try:
                detail = json.loads(exc.read().decode()).get("error", "")
            except Exception:  # noqa: BLE001 (best-effort error body)
                pass
            raise ServerError(
                f"{method} {path}: HTTP {exc.code}"
                + (f": {detail}" if detail else "")
            ) from None
        except urllib.error.URLError as exc:
            raise ServerError(
                f"Cannot reach the Relay server at {self.server.url} "
                f"({exc.reason}). Is `relayd server` running? To run in "
                f"embedded local-Docker mode instead, set RELAY_MODE=local."
            ) from None
        if stream:
            return resp
        with resp:
            data = resp.read()
        return json.loads(data) if data else {}

    # ---------------- runs ----------------

    def submit_run(self, spec: dict[str, Any]) -> dict[str, Any]:
        return self._request("POST", "/v1/runs", body=json.dumps(spec).encode())

    def get_run(self, run_id: str) -> dict[str, Any]:
        return self._request("GET", f"/v1/runs/{run_id}")

    def list_runs(self, limit: int = 50, states: list[str] | None = None) -> list[dict]:
        path = f"/v1/runs?limit={limit}"
        if states:
            path += "&state=" + ",".join(states)
        return self._request("GET", path).get("runs", [])

    def cancel_run(self, run_id: str) -> None:
        self._request("POST", f"/v1/runs/{run_id}/cancel", body=b"{}")

    def stream_logs(self, run_id: str, follow: bool = True) -> Iterator[str]:
        suffix = "" if follow else "?follow=0"
        resp = self._request(
            "GET", f"/v1/runs/{run_id}/logs{suffix}",
            stream=True, timeout=None if follow else self.timeout,
        )
        with resp:
            for raw in resp:
                yield raw.decode("utf-8", "replace").rstrip("\n")

    # ---------------- fleet ----------------

    def machines(self) -> list[dict]:
        return self._request("GET", "/v1/machines").get("machines", [])

    def create_join_token(self) -> str:
        return self._request("POST", "/v1/join-tokens", body=b"{}")["token"]

    # ---------------- services ----------------

    def deploy_service(self, spec: dict[str, Any]) -> dict[str, Any]:
        return self._request("POST", "/v1/services",
                             body=json.dumps(spec).encode(), timeout=120)

    def list_services(self) -> list[dict]:
        return self._request("GET", "/v1/services").get("services", [])

    # ---------------- secrets ----------------

    def set_secret(self, name: str, value: str) -> None:
        self._request("PUT", f"/v1/secrets/{name}",
                      body=json.dumps({"value": value}).encode())

    def list_secrets(self) -> list[str]:
        return self._request("GET", "/v1/secrets").get("secrets", [])

    def delete_secret(self, name: str) -> None:
        self._request("DELETE", f"/v1/secrets/{name}")

    # ---------------- blobs ----------------

    def has_blob(self, sha: str) -> bool:
        req = urllib.request.Request(
            f"{self.server.url}/v1/blobs/{sha}", method="HEAD",
            headers={"Authorization": f"Bearer {self.server.token}"},
        )
        try:
            with urllib.request.urlopen(req, timeout=self.timeout):  # noqa: S310
                return True
        except urllib.error.HTTPError:
            return False
        except urllib.error.URLError as exc:
            raise ServerError(
                f"Cannot reach the Relay server at {self.server.url} "
                f"({exc.reason})."
            ) from None

    def put_blob(self, sha: str, data: bytes) -> None:
        self._request(
            "PUT", f"/v1/blobs/{sha}", body=data,
            content_type="application/octet-stream",
            timeout=max(self.timeout, 30 + len(data) / 1e6),
        )

    def ensure_blob(self, sha: str, data: bytes) -> None:
        if not self.has_blob(sha):
            self.put_blob(sha, data)

    def get_blob(self, sha: str) -> bytes:
        resp = self._request("GET", f"/v1/blobs/{sha}", stream=True)
        with resp:
            return resp.read()
