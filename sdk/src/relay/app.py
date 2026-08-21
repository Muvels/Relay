"""App: the named collection of Relay functions in one project."""

from __future__ import annotations

from typing import Callable, ParamSpec, TypeVar, Union

from .exceptions import SpecError
from .function import RelayFunction, Target
from .image import Image
from .resources import (
    Accelerator,
    AcceleratorSet,
    CUDA,
    GPU,
    build_resources,
)
from .secret import Secret, normalize_secrets
from .volume import Volume, normalize_volumes

P = ParamSpec("P")
R = TypeVar("R")


class App:
    def __init__(self, name: str) -> None:
        if not name or "/" in name or " " in name:
            raise SpecError(
                f"App name {name!r} must be a simple identifier-ish string "
                f'like "gemma-trainer".'
            )
        self.name = name
        self.functions: dict[str, RelayFunction] = {}

    def function(
        self,
        *,
        image: Image | None = None,
        gpu: Union[str, int, GPU, CUDA, None] = None,
        accelerator: Union[Accelerator, AcceleratorSet, list[Accelerator], None] = None,
        cpu: Union[int, float, None] = None,
        memory: Union[str, int, None] = None,
        timeout: Union[str, int, None] = None,
        target: Target = "auto",
        volumes: dict[str, Union[Volume, str]] | None = None,
        secrets: list[Union[Secret, str]] | None = None,
        schedule: str | None = None,
    ) -> Callable[[Callable[P, R]], RelayFunction[P, R]]:
        """Declare a function's runtime requirements.

        All arguments are validated immediately (import time), so a typo
        fails the moment the module loads.
        """
        resources = build_resources(
            cpu=cpu,
            memory=memory,
            gpu=gpu,
            accelerator=accelerator,
            timeout=timeout,
        )
        volume_map = normalize_volumes(volumes)
        secret_names = normalize_secrets(secrets)
        if schedule is not None and len(schedule.split()) != 5:
            raise SpecError(
                f'schedule={schedule!r} must be a 5-field cron expression '
                f'like "0 3 * * *" (minute hour day month weekday)'
            )

        def decorator(fn: Callable[P, R]) -> RelayFunction[P, R]:
            wrapped = RelayFunction(
                fn,
                app_name=self.name,
                image=image,
                resources=resources,
                target=target,
                volumes=volume_map,
                secrets=secret_names,
            )
            wrapped.schedule = schedule
            if wrapped.name in self.functions:
                raise SpecError(
                    f"App {self.name!r} already has a function named "
                    f"{wrapped.name!r}."
                )
            self.functions[wrapped.name] = wrapped
            return wrapped

        return decorator

    def service(
        self,
        *,
        port: int,
        image: Image | None = None,
        gpu: Union[str, int, GPU, CUDA, None] = None,
        accelerator: Union[Accelerator, AcceleratorSet, list[Accelerator], None] = None,
        cpu: Union[int, float, None] = None,
        memory: Union[str, int, None] = None,
        target: Target = "auto",
        volumes: dict[str, Union[Volume, str]] | None = None,
        secrets: list[Union[Secret, str]] | None = None,
        expose: str = "private",
        auth: str = "key",
        restart: str = "always",
    ):
        """Declare a long-running service. The function must start a server
        listening on `port` and block; Relay supervises and restarts it.
        `restart="always"` is the only policy today (validated so the knob
        exists without a breaking change later)."""
        if restart != "always":
            raise SpecError(
                f'@app.service restart={restart!r}: only "always" is '
                f"supported today"
            )
        from .service_def import RelayService

        resources = build_resources(
            cpu=cpu, memory=memory, gpu=gpu, accelerator=accelerator,
            timeout=None,  # services do not time out
        )
        volume_map = normalize_volumes(volumes)
        secret_names = normalize_secrets(secrets)

        def decorator(fn):
            wrapped = RelayService(
                fn,
                port=port,
                expose=expose,
                auth=auth,
                app_name=self.name,
                image=image,
                resources=resources,
                target=target,
                volumes=volume_map,
                secrets=secret_names,
            )
            if wrapped.name in self.functions:
                raise SpecError(
                    f"App {self.name!r} already has a function named "
                    f"{wrapped.name!r}."
                )
            self.functions[wrapped.name] = wrapped
            return wrapped

        return decorator

    def __repr__(self) -> str:
        return f"<relay.App {self.name!r} functions={sorted(self.functions)}>"
