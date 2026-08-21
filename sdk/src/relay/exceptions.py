"""Relay exception hierarchy.

Every user-facing error must say what went wrong *and* what to change.
"""

from __future__ import annotations


class RelayError(Exception):
    """Base class for all Relay errors."""


class SpecError(RelayError):
    """A decorator argument or resource string could not be parsed.

    Raised at decoration time, never at submit time, so mistakes surface
    the moment the module is imported.
    """


class ExecutorError(RelayError):
    """The local/remote executor failed outside the user's function."""


class DockerNotAvailableError(ExecutorError):
    def __init__(self, detail: str = "") -> None:
        msg = (
            "Docker is not available. Relay's local mode runs functions in "
            "Docker containers.\n"
            "  - macOS: start Docker Desktop (or `open -a Docker`)\n"
            "  - Linux: `systemctl start docker` and ensure your user is in "
            "the `docker` group"
        )
        if detail:
            msg += f"\n  Details: {detail}"
        super().__init__(msg)


class RemoteFunctionError(RelayError):
    """The user's function raised inside the container.

    Carries the remote exception type name and full remote traceback so the
    local stack trace is never a dead end.
    """

    def __init__(self, exc_type: str, message: str, remote_traceback: str) -> None:
        self.exc_type = exc_type
        self.message = message
        self.remote_traceback = remote_traceback
        super().__init__(
            f"{exc_type}: {message}\n"
            f"--- remote traceback ---\n{remote_traceback}"
        )


class ResultTooLargeError(RelayError):
    def __init__(self, size_bytes: int, limit_bytes: int) -> None:
        super().__init__(
            f"Function returned {size_bytes / 1e6:.1f} MB, over the "
            f"{limit_bytes / 1e6:.0f} MB result limit. Write large outputs to "
            "a volume and return a path (relay.Artifact support lands with "
            "volumes) instead of returning the data itself."
        )
