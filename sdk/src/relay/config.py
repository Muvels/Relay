"""Client configuration: which control plane to talk to, if any.

Resolution order (first hit wins):
  1. RELAY_SERVER + RELAY_TOKEN environment variables
  2. ~/.relay/config.toml   ([server] url = "...", token = "...")
  3. A relayd server on this machine (~/.relay/server/api_token exists).
     the zero-config path when you ran `relayd server` locally
  4. Nothing → embedded local Docker mode (no server at all)

RELAY_MODE=local forces mode 4 regardless.
"""

from __future__ import annotations

import os
import tomllib
from dataclasses import dataclass
from pathlib import Path

CONFIG_PATH = Path.home() / ".relay" / "config.toml"
LOCAL_SERVER_TOKEN = Path.home() / ".relay" / "server" / "api_token"
LOCAL_SERVER_URL = "http://127.0.0.1:7460"


@dataclass(frozen=True)
class ServerConfig:
    url: str
    token: str

    @property
    def grpc_hint(self) -> str:
        """Best-effort agent gRPC address derived from the HTTP URL.
        used only for display in `relay connect` output."""
        host = self.url.split("://", 1)[-1].split("/", 1)[0].rsplit(":", 1)[0]
        return f"{host}:7461"


def resolve_server() -> ServerConfig | None:
    if os.environ.get("RELAY_MODE") == "local":
        return None

    env_url, env_token = os.environ.get("RELAY_SERVER"), os.environ.get("RELAY_TOKEN")
    if env_url and env_token:
        return ServerConfig(url=env_url.rstrip("/"), token=env_token)

    if CONFIG_PATH.exists():
        try:
            data = tomllib.loads(CONFIG_PATH.read_text())
            server = data.get("server", {})
            if server.get("url") and server.get("token"):
                return ServerConfig(
                    url=str(server["url"]).rstrip("/"), token=str(server["token"])
                )
        except (OSError, tomllib.TOMLDecodeError):
            pass

    if LOCAL_SERVER_TOKEN.exists():
        try:
            token = LOCAL_SERVER_TOKEN.read_text().strip()
            if token:
                return ServerConfig(url=LOCAL_SERVER_URL, token=token)
        except OSError:
            pass
    return None


def write_config(url: str, token: str) -> None:
    CONFIG_PATH.parent.mkdir(parents=True, exist_ok=True)
    CONFIG_PATH.write_text(
        f'[server]\nurl = "{url.rstrip("/")}"\ntoken = "{token}"\n'
    )
    CONFIG_PATH.chmod(0o600)
