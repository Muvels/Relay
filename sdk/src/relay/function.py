"""RelayFunction: the object a decorated function becomes.

Rules:
  - A plain call runs the function locally as ordinary Python. Decorating
    never changes local semantics.
  - `.remote()` / `.spawn()` run it on Relay (M0: the embedded local Docker
    executor; the fleet transport replaces this in M1 without changing the
    call surface).
"""

from __future__ import annotations

import functools
import inspect
from pathlib import Path
from typing import Any, Callable, Generic, ParamSpec, TypeVar, Union

from .exceptions import SpecError
from .image import Image, default_image
from .project import entry_module_name, find_project_root
from .resources import Resources

P = ParamSpec("P")
R = TypeVar("R")

Target = Union[str, list[str], None]


class RelayFunction(Generic[P, R]):
    _is_relay_function = True

    def __init__(
        self,
        fn: Callable[P, R],
        *,
        app_name: str,
        image: Image | None,
        resources: Resources,
        target: Target,
        volumes: dict[str, str] | None = None,
        secrets: list[str] | None = None,
    ) -> None:
        if not inspect.isfunction(fn):
            raise SpecError(
                f"@app.function must decorate a plain function, got {fn!r}"
            )
        if inspect.iscoroutinefunction(fn):
            raise SpecError(
                f"@app.function does not support async functions yet. "
                f"{fn.__name__!r} is `async def`. Wrap the coroutine in a "
                f"sync function that runs asyncio.run(...)."
            )
        if "." in fn.__qualname__ and "<locals>" not in fn.__qualname__:
            raise SpecError(
                f"@app.function must decorate a module-level function; "
                f"{fn.__qualname__!r} looks like a method. Move it to module "
                f"scope."
            )
        # Nested functions can be decorated (handy in tests/experiments) but
        # can never run remotely because runners import functions by name.
        self._importable = "<locals>" not in fn.__qualname__
        self.raw: Callable[P, R] = fn
        self.name = fn.__name__
        self.app_name = app_name
        self.image = image or default_image()
        self.resources = resources
        self.target = target
        self.volumes = volumes or {}
        self.secrets = secrets or []
        self.schedule: str | None = None  # set by @app.function(schedule=)

        source = inspect.getsourcefile(fn)
        if source is None:
            raise SpecError(
                f"Cannot locate the source file of {self.name!r}. Relay "
                f"ships your project directory to the runner, so functions "
                f"must live in a real .py file."
            )
        self._source_path = Path(source).resolve()
        self._project_root = find_project_root(self._source_path.parent)
        self._entry_module = entry_module_name(self._source_path, self._project_root)
        functools.update_wrapper(self, fn)

    # -- local semantics ---------------------------------------------------

    def __call__(self, *args: P.args, **kwargs: P.kwargs) -> R:
        return self.raw(*args, **kwargs)

    # -- remote semantics --------------------------------------------------

    @property
    def project_dir(self) -> Path:
        """The discovered project root: mounted/shipped to runners whole."""
        return self._project_root

    @property
    def entry_module(self) -> str:
        """Dotted module name relative to the project root ("pkg.app")."""
        return self._entry_module

    def _call_spec(self, args: tuple, kwargs: dict) -> "CallSpec":
        if not self._importable:
            raise SpecError(
                f"{self.raw.__qualname__!r} is nested inside another "
                f"function, so runners cannot import it by name. Move it to "
                f"module scope to run it remotely."
            )
        from ._local.executor import CallSpec

        return CallSpec(
            function_name=self.name,
            entry_module=self.entry_module,
            project_dir=self.project_dir,
            image=self.image,
            resources=self.resources,
            args=args,
            kwargs=kwargs,
            app_name=self.app_name,
            target_names=self.target_names,
            volumes=self.volumes,
            secrets=self.secrets,
        )

    @property
    def target_names(self) -> list[str]:
        """target= normalized to a machine-name list ([] = auto)."""
        if self.target is None or self.target == "auto":
            return []
        if isinstance(self.target, str):
            return [self.target]
        return list(self.target)

    def _executor(self):
        """Fleet when a control plane is configured, embedded local Docker
        otherwise (see config.resolve_server for the resolution order)."""
        from .config import resolve_server

        server = resolve_server()
        if server is not None:
            from ._fleet.executor import FleetExecutor

            return FleetExecutor(server)
        from ._local.executor import LocalExecutor

        return LocalExecutor()

    def remote(self, *args: P.args, **kwargs: P.kwargs) -> R:
        """Run on Relay and block until the result is back."""
        return self._executor().run_sync(self._call_spec(args, dict(kwargs)))

    def spawn(self, *args: P.args, **kwargs: P.kwargs):
        """Fire-and-forget: returns a Run handle with
        .status() / .logs() / .result() / .cancel()."""
        return self._executor().spawn(self._call_spec(args, dict(kwargs)))

    def map(self, inputs, *, return_exceptions: bool = False, **common_kwargs):
        """Fan out one run per input item and yield results IN INPUT ORDER.

        Items are passed as the first positional argument; common_kwargs go
        to every call. On a failing item, the default raises, which ends
        the iteration (generators close on raise; remaining runs still
        execute, but their handles are gone). Pass return_exceptions=True to
        receive the exception OBJECT in that item's slot and keep going.
        """
        executor = self._executor()
        handles = [
            executor.spawn(self._call_spec((item,), dict(common_kwargs)))
            for item in inputs
        ]
        for handle in handles:
            if return_exceptions:
                try:
                    yield handle.result()
                except Exception as exc:  # noqa: BLE001 (delivered, not hidden)
                    yield exc
            else:
                yield handle.result()

    def __repr__(self) -> str:
        return (
            f"<relay.function {self.app_name}.{self.name} "
            f"[{self.resources.describe()}] image={self.image.tag()}>"
        )
