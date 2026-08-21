# Relay

**Modal for hardware you already own.** Write Python, press run, and your own
machines (DGX Spark, RTX desktop, and MacBook) behave like serverless compute.

```python
import relay

app = relay.App("trainer")

image = relay.Image.python("3.12").pip("torch", "transformers")

@app.function(image=image, gpu="24GB", cpu=8, memory="32GB")
def train(steps: int = 1000) -> dict:
    ...
    return {"loss": final_loss}

train(10)             # plain local call; decorators preserve local semantics
train.remote(1000)    # run on your fleet, stream logs, get the result back
run = train.spawn(1000)   # fire and forget → run.status() / .logs() / .result()
```

- **One command to add a machine**: `relay connect` prints a join line; paste
  it on the new box. The agent dials **out**, so no port forwarding, VPN, or
  Kubernetes, TLS-pinned from the first handshake.
- **A real scheduler**: `target="auto"` filters by capability (CUDA/MPS),
  admits against a reservation ledger (unified-memory aware for DGX Spark and
  Apple Silicon), and packs by best fit with image-cache locality. Queued runs
  tell you *per machine* why they're waiting.
- **CUDA and MPS in one system**: Linux boxes run jobs in Docker with pinned
  GPUs; Apple Silicon runs them natively in uv-managed venvs (Metal cannot be
  containerized). `accelerator=relay.any_of(relay.CUDA(...), relay.MPS(...))`
  lets one function use either.
- **Services with sane defaults**: `@app.service(port=8000)` supervises a
  long-running server with restarts, health gating, and key-gated exposure.
  `expose="public"` gets an HTTPS tunnel URL where strangers see 401, and
  private mode is key-gated too. Public ≠ open.
- **Volumes, secrets, cron, map()**: machine-local named volumes, encrypted
  write-only secrets injected as env vars, `schedule="0 3 * * *"`, and
  `fn.map(items)` fan-out.

## Quickstart

```bash
# 1. Install from this repo (Relay is not on PyPI yet)
uv venv .venv && uv pip install -p .venv/bin/python -e './sdk[dev]'
cd relayd && go build -o /usr/local/bin/relayd ./cmd/relayd

# 2. Zero-config local mode: needs only Docker
relay run examples/hello.py::hi          # runs in a local container
relay run examples/train_mlp.py::train --epochs 100  # tiny PyTorch training job

# 3. Fleet mode
relayd server                             # control plane (prints API token)
relay connect                             # → paste the join line on each machine
relay fleet                               # who's online, with what hardware
python train.py                           # train.remote() now uses the fleet
relay ps / relay logs <run> / relay stop <run>

# 4. Services
relay deploy app.py                       # deploy @app.service definitions
relay services                            # endpoints + state
relay shell dgx-spark nvidia-smi          # bounded host command on a machine
```

Dashboard: open the server's HTTP address (default `http://127.0.0.1:7460`)
in a browser and paste the API token once.

## Documentation

- [Getting started](docs/getting-started.md)
- [Concepts](docs/concepts.md), covering functions, images, scheduler, volumes,
  secrets, services, schedules
- [CLI reference](docs/cli.md)
- [Architecture](docs/architecture.md), plus `PLAN.md` for the full design
  history and decisions log

## Layout

```
sdk/        Python SDK + CLI (import name: relay)
relayd/     Go control plane + machine agent (one binary)
proto/      agent ↔ server contract (buf)
installer/  curl|sh agent installer served by the server
docs/       user documentation
research/   backend build-vs-adopt research (Aug 2026)
```

Licensed under Apache-2.0.
