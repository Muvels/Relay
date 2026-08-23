# Getting started

## Prerequisites

- **Dev machine**: Python ≥3.10, `uv`, Docker (for local mode)
- **Fleet machines (Linux/NVIDIA)**: Docker + NVIDIA Container Toolkit
- **Fleet machines (Apple Silicon)**: `uv` (MPS jobs run natively), Docker
  optional for CPU container jobs

## Install

From the repo root:

```bash
uv venv .venv && uv pip install -p .venv/bin/python -e './sdk[dev]'
cd relayd && go build -o /usr/local/bin/relayd ./cmd/relayd && cd ..
```

## Local mode (no server)

With nothing configured, `.remote()` and `relay run` execute in a local
Docker container with the same images and protocol as the fleet:

```python
# hello.py
import relay

app = relay.App("hello")

@app.function(cpu=1, memory="512MB")
def hi(name: str = "world") -> str:
    print("computing…")
    return f"hello {name}"
```

```bash
relay run hello.py::hi --name relay
```

The first run builds the image (a hashed `python:<your minor>-slim` +
cloudpickle); subsequent runs reuse it and start in well under a second.

## Fleet mode

1. **Start the control plane** on any machine. It is tiny, using SQLite and one
   process; the dev machine or a home server both work):

   ```bash
   relayd server
   # relayd server ready
   #   http  127.0.0.1:7460
   #   grpc  0.0.0.0:7461
   #   token rly_…
   ```

   The HTTP API binds loopback by default; reach it remotely over SSH/
   Tailscale, or bind explicitly with `--http 0.0.0.0:7460` on a trusted LAN.

2. **Point your CLI at it** (same machine: automatic; remote:
   `relay login http://server:7460 <token>`).

3. **Join machines**:

   ```bash
   relay connect
   # Run this on the machine you want to add (valid 30 minutes):
   #   relayd agent --join rly_…@server:7461#<fingerprint>
   ```

   The join string carries the server's TLS fingerprint. The agent verifies
   it, pins the certificate, and every later connection checks the pin. The
   agent connects **outbound only**; the server never dials your machines.

   For machines without the binary: `make stage` on the repo puts
   cross-compiled binaries + installer into the server's data dir, then
   `curl -fsSL http://server:7460/install.sh | sh -s -- --join '<join line>'`
   installs relayd, joins verifiably, and registers a systemd/launchd
   service.

   On Linux the installer registers a **system** unit (starts at boot, ordered
   after `docker.service`) when it can use sudo without a password. Otherwise
   it falls back to a user unit and enables lingering, because a user service
   without it stops when your last SSH session closes. Watch that line of
   output on a headless box.

4. **Run things**. The same code works unchanged. With a server configured,
   `.remote()`/`.spawn()` submit to the fleet:

   ```bash
   relay fleet          # inventory + who's online
   python train.py      # train.remote() lands on the best machine
   relay ps             # runs with live per-machine queue explanations
   relay logs run_… -f
   ```

## Your first GPU function

```python
@app.function(
    image=relay.Image.python("3.12").pip("torch"),
    gpu="20GB",                 # scheduler reservation, any-CUDA
    cpu=8, memory="32GB",
    volumes={"/checkpoints": relay.Volume("ckpts")},
    secrets=[relay.Secret("HF_TOKEN")],
    timeout="8h",
)
def finetune(model: str): ...
```

- `gpu="20GB"` → any NVIDIA device with ≥20GB free (per the ledger).
- Add MPS as an alternative:
  `accelerator=relay.any_of(relay.CUDA(memory="20GB"), relay.MPS(memory="20GB"))`
  because pip-only images can run natively on a Mac.
- `relay secret set HF_TOKEN` stores the secret server-side (encrypted,
  write-only); local mode reads `HF_TOKEN` from *your* environment instead.
- Volumes are **machine-local**: checkpoints live on the machine that wrote
  them. Pin `target="dgx-spark"` when a follow-up run needs them.

## Your first service

```python
@app.service(port=8000, gpu="40GB", expose="private")
def vllm_server():
    import subprocess
    subprocess.run(["vllm", "serve", "Qwen/Qwen3-4B", "--port", "8000"])
```

GPU requests are exclusive by default, so this service owns one device with
at least 40GB. To deliberately co-locate it with other GPU work, use a fixed
VRAM reservation: `gpu=relay.GPU(memory="40GB", exclusive=False)`. Shared
reservations are scheduling promises rather than enforced CUDA memory limits.

```bash
relay deploy app.py
# → deploying trainer.vllm_server (run_…)
#   service key (shown once): rly_…
# ✓ vllm_server at 192.168.1.40:55123
```

Every exposed service is **key-gated by default**. Callers send
`Authorization: Bearer <service key>`; anonymous requests get 401 in every
mode (`auth="none"` is the explicit opt-out).

Reaching services from outside the LAN, in order of recommendation:

1. **Put your machines on a tailnet** (Tailscale or self-hosted Headscale).
   Nothing else needs configuration. Private endpoints automatically report the
   machine's MagicDNS name, so `spark.tailXXXX.ts.net:PORT` works from any
   of your devices, anywhere.
2. **`expose="funnel"`** for a *stable public* HTTPS URL with no domain:
   requires Tailscale's hosted control plane, `tailscale up` on the
   machine, and Funnel enabled in your tailnet policy.
3. **`expose="public"`** for an account-less public URL via a `cloudflared`
   quick tunnel (URL changes on each deploy).
