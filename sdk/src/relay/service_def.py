"""RelayService: the object an @app.service-decorated function becomes.

A service is a supervised, long-running process: the decorated function is
expected to start a server on `port` and block. The agent restarts it on
exit, health-gates on the port opening, and exposes it per `expose`:

  expose="private"  (default) reachable at machine:port over your
                    Tailscale/Headscale tailnet when one is up (MagicDNS
                    endpoint), otherwise the LAN IP
  expose="funnel"   STABLE public https://machine.tailnet.ts.net URL via
                    Tailscale Funnel with no domain needed (Tailscale-hosted
                    control plane only; enable Funnel in tailnet policy)
  expose="public"   public HTTPS via a Cloudflare quick tunnel (URL changes
                    per deploy; needs cloudflared installed)

Every mode fronts through an auth proxy. A minted service key is required
unless auth="none" is explicitly chosen (public ≠ open, private ≠ open).
"""

from __future__ import annotations

from typing import Callable

from .exceptions import SpecError
from .function import RelayFunction


class RelayService(RelayFunction):
    _is_relay_service = True

    def __init__(self, fn: Callable, *, port: int, expose: str, auth: str,
                 **kwargs) -> None:
        if not isinstance(port, int) or not (0 < port < 65536):
            raise SpecError(f"@app.service port={port!r} must be a TCP port")
        if expose not in ("private", "funnel", "public"):
            raise SpecError(
                f'@app.service expose={expose!r} must be "private" (tailnet/'
                f'LAN), "funnel" (stable public URL via Tailscale Funnel), '
                f'or "public" (Cloudflare quick tunnel)'
            )
        if auth not in ("key", "none"):
            raise SpecError(
                f'@app.service auth={auth!r} must be "key" (default) or '
                f'"none". Anonymous public access is an explicit opt-out'
            )
        super().__init__(fn, **kwargs)
        self.port = port
        self.expose = expose
        self.auth = auth

    # Services deploy; they are not invoked ad hoc.
    def remote(self, *args, **kwargs):
        raise SpecError(
            f"{self.name} is a service. Deploy it with `relay deploy` (or "
            f".deploy()), then call its endpoint."
        )

    spawn = remote

    def deploy(self):
        """Deploy (or replace) this service on the fleet. Returns the
        server's response dict incl. run info and the one-time service key."""
        from .config import resolve_server
        from ._fleet.executor import FleetExecutor

        server = resolve_server()
        if server is None:
            raise SpecError(
                "Services need a control plane: start `relayd server`, join "
                "a machine with `relay connect`, then deploy again."
            )
        return FleetExecutor(server).deploy_service(
            self._call_spec((), {}), port=self.port, expose=self.expose,
            auth_none=(self.auth == "none"),
        )

    def __repr__(self) -> str:
        return (
            f"<relay.service {self.app_name}.{self.name} port={self.port} "
            f"expose={self.expose} [{self.resources.describe()}]>"
        )
