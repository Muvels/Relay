"""The `relay` CLI.

M0/M1 surface: `relay run app.py::function [--arg value ...]`.
Function arguments are parsed from the function's own signature, including
resolved type hints and required parameters from defaults, so the CLI stays in sync with the
code automatically.

The app file is imported under its canonical dotted module name (relative
to the discovered project root), the same name the container uses, so
pickled user-defined classes resolve on both sides.
"""

from __future__ import annotations

import importlib
import inspect
import sys
import typing
from pathlib import Path
from typing import Any

import typer

from .. import __version__
from ..exceptions import RelayError
from ..function import RelayFunction
from ..project import entry_module_name, find_project_root

app = typer.Typer(
    name="relay",
    help="Run Python on your own machines.",
    no_args_is_help=True,
    pretty_exceptions_enable=False,
)


def _fail(message: str) -> "typer.Exit":
    typer.secho(f"✗ {message}", fg=typer.colors.RED, err=True)
    return typer.Exit(code=1)


def _import_app_file(path: Path):
    if not path.exists():
        raise _fail(f"{path} does not exist")
    try:
        root = find_project_root(path.resolve().parent)
        module_name = entry_module_name(path.resolve(), root)
    except RelayError as exc:
        raise _fail(str(exc)) from None
    root_str = str(root)
    if root_str not in sys.path:
        sys.path.insert(0, root_str)
    try:
        return importlib.import_module(module_name)
    except ImportError as exc:
        raise _fail(f"cannot import {path} (as module {module_name!r}): {exc}") from None


def _find_function(module, name: str | None, target: str) -> RelayFunction:
    functions = {
        attr: obj
        for attr, obj in vars(module).items()
        if isinstance(obj, RelayFunction)
    }
    if not functions:
        raise _fail(f"{target}: no @app.function definitions found in this file")
    if name is None:
        if len(functions) == 1:
            return next(iter(functions.values()))
        raise _fail(
            f"{target} has {len(functions)} functions "
            f"({', '.join(sorted(functions))}). Pick one with {target}::<name>"
        )
    if name not in functions:
        raise _fail(
            f"{target}: no function {name!r}. "
            f"Available: {', '.join(sorted(functions))}"
        )
    return functions[name]


_NoneType = type(None)


def _unwrap_optional(annotation: Any) -> Any:
    """Optional[T] / T | None → T (so --flag value still parses as T)."""
    origin = typing.get_origin(annotation)
    if origin is typing.Union or str(origin) == "types.UnionType":
        non_none = [a for a in typing.get_args(annotation) if a is not _NoneType]
        if len(non_none) == 1:
            return non_none[0]
    return annotation


def _convert(raw: str, annotation: Any, param: str) -> Any:
    annotation = _unwrap_optional(annotation)
    if annotation in (inspect.Parameter.empty, str, Any):
        return raw
    if annotation is bool:
        if raw.lower() in {"true", "1", "yes"}:
            return True
        if raw.lower() in {"false", "0", "no"}:
            return False
        raise _fail(f"--{param} expects true/false, got {raw!r}")
    if annotation in (int, float):
        try:
            return annotation(raw)
        except ValueError:
            raise _fail(f"--{param} expects {annotation.__name__}, got {raw!r}") from None
    if annotation is Path:
        return Path(raw)
    # Unknown annotations pass through as strings; the function will see
    # exactly what was typed.
    return raw


def _parse_function_args(fn: RelayFunction, extra: list[str]) -> dict[str, Any]:
    sig = inspect.signature(fn.raw)
    # Resolve string annotations (`from __future__ import annotations`).
    try:
        hints = typing.get_type_hints(fn.raw)
    except Exception:
        hints = {}

    def annotation_for(name: str) -> Any:
        return hints.get(name, sig.parameters[name].annotation)

    values: dict[str, Any] = {}
    i = 0
    while i < len(extra):
        token = extra[i]
        if not token.startswith("--"):
            raise _fail(
                f"unexpected argument {token!r}. Pass function arguments as --name value"
            )
        body = token[2:]
        if "=" in body:
            key, raw = body.split("=", 1)
            key = key.replace("-", "_")
            i += 1
        else:
            key = body.replace("-", "_")
            if key not in sig.parameters:
                raise _fail(
                    f"{fn.name}() has no parameter {key!r}. "
                    f"Parameters: {', '.join(sig.parameters)}"
                )
            is_bool = _unwrap_optional(annotation_for(key)) is bool
            if i + 1 < len(extra) and not extra[i + 1].startswith("--"):
                raw = extra[i + 1]
                i += 2
            elif is_bool:
                raw = "true"  # bare flag only for booleans
                i += 1
            else:
                raise _fail(f"--{key} needs a value (e.g. --{key} 42)")
        if key not in sig.parameters:
            raise _fail(
                f"{fn.name}() has no parameter {key!r}. "
                f"Parameters: {', '.join(sig.parameters)}"
            )
        values[key] = _convert(raw, annotation_for(key), key)

    try:
        sig.bind(**values)
    except TypeError as exc:
        missing = [
            p.name
            for p in sig.parameters.values()
            if p.default is inspect.Parameter.empty
            and p.kind in (p.POSITIONAL_OR_KEYWORD, p.KEYWORD_ONLY)
            and p.name not in values
        ]
        if missing:
            raise _fail(
                f"{fn.name}() is missing required arguments: "
                + ", ".join(f"--{m}" for m in missing)
            ) from None
        raise _fail(f"{fn.name}(): {exc}") from None
    return values


@app.command(
    context_settings={"allow_extra_args": True, "ignore_unknown_options": True}
)
def run(
    ctx: typer.Context,
    target: str = typer.Argument(
        ..., help="app file and function: path/to/app.py::train"
    ),
) -> None:
    """Run a decorated function on Relay."""
    file_part, _, fn_part = target.partition("::")
    module = _import_app_file(Path(file_part))
    fn = _find_function(module, fn_part or None, file_part)
    kwargs = _parse_function_args(fn, list(ctx.args))

    try:
        result = fn.remote(**kwargs)
    except RelayError as exc:
        raise _fail(str(exc)) from None

    if result is not None:
        typer.echo(repr(result))


def _client():
    from ..config import resolve_server
    from .._fleet.client import RelayClient

    server = resolve_server()
    if server is None:
        raise _fail(
            "No Relay server configured. Start one with `relayd server` on "
            "this machine, or point at one with `relay login URL TOKEN`."
        )
    return RelayClient(server)


@app.command()
def login(
    url: str = typer.Argument(..., help="server HTTP url, e.g. http://spark.local:7460"),
    token: str = typer.Argument(..., help="API token printed by `relayd server`"),
) -> None:
    """Save the control-plane address and token to ~/.relay/config.toml."""
    from ..config import ServerConfig, write_config
    from .._fleet.client import RelayClient

    try:
        RelayClient(ServerConfig(url=url.rstrip("/"), token=token))._request(
            "GET", "/v1/machines"
        )
    except RelayError as exc:
        raise _fail(f"could not verify the server: {exc}") from None
    write_config(url, token)
    typer.secho(f"✓ logged in to {url}", fg=typer.colors.GREEN)


@app.command()
def connect(
    name: str = typer.Option("", "--name", help="name for the new machine"),
) -> None:
    """Mint a join token and print the command to run on the new machine."""
    client = _client()
    token = client.create_join_token()
    grpc = client.server.grpc_hint
    fp = client._request("GET", "/v1/health").get("grpc_fingerprint", "")
    join = f"{token}@{grpc}" + (f"#{fp}" if fp else "")
    name_flag = f" --name {name}" if name else ""
    typer.echo("Run this on the machine you want to add (valid 30 minutes):\n")
    typer.secho(f"  relayd agent --join {join}{name_flag}\n", bold=True)
    typer.echo(
        "No relayd there yet? Build it from this repo:\n"
        "  cd relayd && go build -o /usr/local/bin/relayd ./cmd/relayd\n"
        f"or fetch a staged binary: curl -fsSL {client.server.url}/install.sh "
        f"| sh -s -- --join '{join}'"
    )


@app.command()
def fleet() -> None:
    """List machines: who's online and what they have."""
    machines = _client().machines()
    if not machines:
        typer.echo("No machines joined yet. Run `relay connect` to add one.")
        return
    typer.echo(f"{'MACHINE':<16} {'STATUS':<8} {'OS/ARCH':<14} {'CPU':>4} "
               f"{'MEMORY':>8}  ACCELERATOR")
    for m in machines:
        status = "online" if m["online"] else "offline"
        accs = ", ".join(
            f"{a.get('kind', '?').upper()} {a.get('name', '')}".strip()
            for a in m.get("accelerators") or []
        ) or "-"
        mem = f"{m['memory_mib'] // 1024}GB" if m.get("memory_mib") else "?"
        if m.get("unified_memory"):
            mem += "*"
        typer.echo(
            f"{m['name']:<16} {status:<8} {m['os']}/{m['arch']:<8} "
            f"{m['cpu_cores']:>4} {mem:>8}  {accs}"
        )
    if any(m.get("unified_memory") for m in machines):
        typer.echo("\n* unified memory (CPU+GPU share one pool)")


@app.command()
def ps(
    all_: bool = typer.Option(False, "--all", "-a", help="include finished runs"),
) -> None:
    """List runs."""
    states = None if all_ else ["pending", "assigned", "building", "running"]
    runs = _client().list_runs(limit=50, states=states)
    if not runs:
        typer.echo("No runs." if all_ else "No active runs (try --all).")
        return
    typer.echo(f"{'RUN':<22} {'FUNCTION':<20} {'MACHINE':<14} {'STATE':<10} DETAIL")
    for r in runs:
        typer.echo(
            f"{r['id']:<22} {r['app'] + '.' + r['function']:<20} "
            f"{r['machine'] or '-':<14} {r['state']:<10} {r['detail']}"
        )


@app.command()
def logs(
    run_id: str = typer.Argument(...),
    follow: bool = typer.Option(True, "--follow/--no-follow", "-f"),
) -> None:
    """Stream a run's logs."""
    try:
        for line in _client().stream_logs(run_id, follow=follow):
            typer.echo(line)
    except RelayError as exc:
        raise _fail(str(exc)) from None


@app.command()
def stop(run_id: str = typer.Argument(...)) -> None:
    """Cancel a run."""
    try:
        _client().cancel_run(run_id)
    except RelayError as exc:
        raise _fail(str(exc)) from None
    typer.secho(f"✓ stop requested for {run_id}", fg=typer.colors.GREEN)


def _services_in(module) -> list:
    from ..service_def import RelayService

    return [obj for obj in vars(module).values() if isinstance(obj, RelayService)]


def _deploy_file(path: Path, wait: bool) -> None:
    import time as _time

    from ..function import RelayFunction
    from ..config import resolve_server
    from .._fleet.executor import FleetExecutor

    module = _import_app_file(path)
    services = _services_in(module)
    scheduled = [
        obj for obj in vars(module).values()
        if isinstance(obj, RelayFunction) and obj.schedule
    ]
    if not services and not scheduled:
        raise _fail(f"{path}: no @app.service or scheduled functions found")
    client = _client()

    for fn in scheduled:
        server = resolve_server()
        FleetExecutor(server).register_schedule(fn._call_spec((), {}), fn.schedule)
        typer.secho(f"⏰ scheduled {fn.app_name}.{fn.name} [{fn.schedule}]",
                    fg=typer.colors.CYAN)
    for svc in services:
        try:
            out = svc.deploy()
        except RelayError as exc:
            raise _fail(str(exc)) from None
        run = out["run"]
        typer.secho(f"→ deploying {svc.app_name}.{svc.name} ({run['id']})",
                    fg=typer.colors.CYAN)
        if out.get("service_key"):
            typer.echo(
                f"  service key (shown once): {out['service_key']}\n"
                f"  call with: Authorization: Bearer <key>"
            )
        if not wait:
            continue
        deadline = _time.monotonic() + 300
        last = ""
        while _time.monotonic() < deadline:
            info = client.get_run(run["id"])
            status_line = info["state"] + (f": {info['detail']}" if info["detail"] else "")
            if status_line != last:
                typer.echo(f"  {status_line}")
                last = status_line
            if info["state"] == "running" and info.get("endpoint"):
                typer.secho(f"✓ {svc.name} at {info['endpoint']}",
                            fg=typer.colors.GREEN)
                break
            if info["state"] in ("error", "lost", "canceled", "failed"):
                raise _fail(f"{svc.name} deploy ended {info['state']}: {info['detail']}")
            _time.sleep(1.0)
        else:
            typer.echo("  still starting. Watch with `relay services`")


@app.command()
def deploy(
    target: str = typer.Argument(..., help="app file: path/to/app.py"),
    wait: bool = typer.Option(True, "--wait/--no-wait", help="wait until running"),
) -> None:
    """Deploy every @app.service and register every scheduled function."""
    _deploy_file(Path(target), wait)


@app.command()
def schedules() -> None:
    """List cron schedules."""
    out = _client()._request("GET", "/v1/schedules").get("schedules", [])
    if not out:
        typer.echo("No schedules. Add schedule=\"0 3 * * *\" to a function "
                   "and `relay deploy` the file.")
        return
    typer.echo(f"{'SCHEDULE':<28} {'CRON':<18} LAST RUN")
    for s in out:
        typer.echo(f"{s['app'] + '.' + s['function']:<28} {s['cron']:<18} "
                   f"{s.get('last_run') or '-'}")


@app.command()
def unschedule(
    app_name: str = typer.Argument(..., metavar="APP"),
    function: str = typer.Argument(...),
) -> None:
    """Remove a cron schedule."""
    _client()._request("DELETE", f"/v1/schedules/{app_name}/{function}")
    typer.secho(f"✓ unscheduled {app_name}.{function}", fg=typer.colors.GREEN)


@app.command()
def shell(
    machine: str = typer.Argument(..., help="machine name (see `relay fleet`)"),
    command: list[str] = typer.Argument(..., help="command to run on the host"),
) -> None:
    """Run a command on a fleet machine's host (bounded, non-interactive).

    Example: relay shell dgx-spark nvidia-smi
    """
    import json as _json

    client = _client()
    resp = client._request(
        "POST", f"/v1/machines/{machine}/exec",
        body=_json.dumps({"argv": command}).encode(),
        stream=True, timeout=180,
    )
    exit_code = 0
    with resp:
        for raw in resp:
            line = raw.decode("utf-8", "replace")
            if line.startswith("\x00exit"):
                parts = line[1:].split()
                try:
                    exit_code = int(parts[1])
                except (IndexError, ValueError):
                    exit_code = -1
                if "(" in line:
                    typer.secho(line[1:].strip(), fg=typer.colors.RED, err=True)
                break
            typer.echo(line, nl=False)
    raise typer.Exit(code=exit_code if exit_code >= 0 else 1)


@app.command()
def services() -> None:
    """List running services and their endpoints."""
    services = _client().list_services()
    if not services:
        typer.echo("No services. Deploy one: relay deploy app.py")
        return
    typer.echo(f"{'RUN':<22} {'SERVICE':<22} {'MACHINE':<12} {'STATE':<9} ENDPOINT")
    for s in services:
        typer.echo(
            f"{s['id']:<22} {s['app'] + '.' + s['function']:<22} "
            f"{s['machine'] or '-':<12} {s['state']:<9} {s.get('endpoint') or '-'}"
        )


@app.command()
def serve(
    target: str = typer.Argument(..., help="app file: path/to/app.py"),
) -> None:
    """Dev mode: deploy, then redeploy on file changes (Ctrl+C stops)."""
    import time as _time

    path = Path(target)
    module = _import_app_file(path)
    services = _services_in(module)
    if not services:
        raise _fail(f"{path}: no @app.service definitions found")
    project_dir = services[0].project_dir

    def snapshot() -> dict[str, float]:
        out = {}
        for p in project_dir.rglob("*.py"):
            if any(part in {".venv", "venv", "__pycache__", ".git"} for part in p.parts):
                continue
            try:
                out[str(p)] = p.stat().st_mtime
            except OSError:
                pass
        return out

    _deploy_file(path, wait=True)
    typer.echo("watching for changes. Press Ctrl+C to stop")
    seen = snapshot()
    try:
        while True:
            _time.sleep(1.0)
            now = snapshot()
            if now != seen:
                seen = now
                typer.secho("↻ change detected. Redeploying", fg=typer.colors.CYAN)
                try:
                    _deploy_file(path, wait=True)
                except typer.Exit:
                    typer.secho("deploy failed; still watching", fg=typer.colors.RED)
    except KeyboardInterrupt:
        typer.echo("\nstopping watch (services keep running; `relay stop <id>` to stop them)")


secret_app = typer.Typer(name="secret", help="Manage fleet secrets.", no_args_is_help=True)
app.add_typer(secret_app)


@secret_app.command("set")
def secret_set(
    name: str = typer.Argument(..., help="env-var-style name, e.g. HF_TOKEN"),
    value: str = typer.Option(
        None, "--value", help="omit to be prompted (keeps it out of shell history)"
    ),
) -> None:
    """Store a secret on the control plane (write-only; encrypted at rest)."""
    if value is None:
        value = typer.prompt(f"value for {name}", hide_input=True)
    try:
        _client().set_secret(name, value)
    except RelayError as exc:
        raise _fail(str(exc)) from None
    typer.secho(f"✓ secret {name} set", fg=typer.colors.GREEN)


@secret_app.command("list")
def secret_list() -> None:
    """List secret names (values are never shown)."""
    names = _client().list_secrets()
    if not names:
        typer.echo("No secrets. Add one: relay secret set HF_TOKEN")
        return
    for n in names:
        typer.echo(n)


@secret_app.command("rm")
def secret_rm(name: str = typer.Argument(...)) -> None:
    """Delete a secret."""
    _client().delete_secret(name)
    typer.secho(f"✓ secret {name} removed", fg=typer.colors.GREEN)


@app.command()
def version() -> None:
    """Print the Relay version."""
    typer.echo(f"relay {__version__}")


if __name__ == "__main__":
    app()
