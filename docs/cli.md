# CLI reference

The `relay` CLI ships with the Python SDK. `relayd` is the Go binary
(server + agent). Fleet commands need a configured server (see
`relay login` / the resolution order below).

## Running code

| Command | Does |
|---|---|
| `relay run app.py::fn --arg value` | Run a function; args parsed from its signature (types from annotations, required-ness from defaults). Streams logs, prints the result. |
| `relay ps [-a]` | Active runs (`-a` includes finished) with per-machine queue explanations. |
| `relay logs <run-id> [-f/--no-follow]` | Stream or dump a run's logs. |
| `relay stop <run-id>` | Cancel a run / stop a service. |

## Fleet

| Command | Does |
|---|---|
| `relay login URL TOKEN` | Save the control plane to `~/.relay/config.toml` (verified first). |
| `relay connect [--name N]` | Mint a one-time join token and print the paste-able join line (includes the TLS fingerprint). |
| `relay fleet` | Machines: online state, OS/arch, CPU, memory (\* = unified), accelerators. |
| `relay shell MACHINE CMD…` | Run a bounded (≤120s), non-interactive host command on a machine, output streamed back. Exit code passes through. |

## Services & schedules

| Command | Does |
|---|---|
| `relay deploy app.py [--no-wait]` | Deploy every `@app.service` (replace live generations, new-first) and register every `schedule=` function. Prints endpoints and one-time service keys. |
| `relay services` | Live services with endpoints. |
| `relay serve app.py` | Dev mode: deploy, watch `.py` files, redeploy on change. |
| `relay schedules` | List cron schedules with last-run times. |
| `relay unschedule APP FN` | Remove a schedule. |

## Secrets

| Command | Does |
|---|---|
| `relay secret set NAME [--value V]` | Store a secret. Omitting `--value` prompts securely and keeps it out of shell history. |
| `relay secret list` | Names only; values are write-only. |
| `relay secret rm NAME` | Delete. |

## relayd

| Command | Does |
|---|---|
| `relayd server [--data-dir D] [--http A] [--grpc A]` | Control plane. Prints the API token; serves the dashboard at `/`. |
| `relayd agent --join TOKEN@HOST:PORT#FP [--name N]` | Join a fleet and run the agent. `--join-only` registers and exits (used by the installer). |
| `relayd version` | Version. |

## Server resolution order (SDK + CLI)

1. `RELAY_SERVER` + `RELAY_TOKEN` env vars
2. `~/.relay/config.toml`
3. A local `relayd server` on this machine (its data-dir token file)
4. Nothing → embedded local Docker mode

`RELAY_MODE=local` forces local mode regardless.

## Make targets

- `make build` builds relayd for this machine.
- `make test` runs Go vet, Go tests, and the full Python suite.
- `make release` cross-compiles relayd (linux/darwin × amd64/arm64).
- `make stage` puts release binaries and the installer into the local server's
  data dir so `curl <server>/install.sh | sh` works
