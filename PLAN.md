# Relay Build Plan

> **Modal for hardware you already own.** Write Python, press run, and your own machines (DGX Spark, RTX desktop, and MacBook) behave like serverless compute.

Companion documents: `research/backend-decision-2026-08-16.md` (why we build the execution layer ourselves), `references/` (cloned study repos).

---

## 1. Product in one paragraph

Relay is a Python SDK + CLI (`pip install relay`) and a single Go binary. Developers decorate functions with resource requirements (`@app.function(gpu="24GB")`), and Relay runs them on the right machine in their personal fleet. CUDA workloads run in containers on Linux, while MPS workloads run as native processes on Apple Silicon. Machines join the fleet with one command (`relay connect`), dialing **out** to the control plane; no port forwarding, VPN setup, or Kubernetes is required. A scheduler picks the best-fit machine when `target="auto"`. Local-first: everything works on one laptop with no account and no cloud.

---

## 2. Reference repos and what we take from each

Cloned (shallow) into `references/`. **License rules are load-bearing.** See the last column. Relay itself is **Apache-2.0**.

| Repo | License | What we take | Code or design only? |
|---|---|---|---|
| `references/beta9` | **AGPL-3.0** | Agent design: `pkg/agent/` (outbound transport, preflight GPU/Docker/NVIDIA detection), `pkg/gateway/agent_install.go` (Tailscale-style curl\|sh installer), pool join-token flow, decorator surface (`sdk/src/beta9/abstractions/`), CLI command taxonomy | **DESIGN ONLY. Never copy code.** The AGPL would infect our Apache-2.0 codebase. |
| `references/woodpecker` | Apache-2.0 | The 8-method `Backend` interface (`pipeline/backend/types/backend.go` area), Docker backend (`pipeline/backend/docker/`, ~1k LOC), outbound gRPC agent (`agent/`, `agent/rpc/`, including dial, auth interceptor, and retry/reconnect) | Code may be adapted with attribution |
| `references/bacalhau` | Apache-2.0 | Outbound-only compute-node posture (single NATS/gRPC dial-out), job taxonomy `batch / daemon / service / ops`, NVIDIA GPU detection (`pkg/compute/capacity/system/gpu/nvidia.go`) | Code may be adapted with attribution |
| `references/modal-client` | Apache-2.0 | Decorator ergonomics, `Image` chained builder API shape, `.remote()/.spawn()/.map()` semantics, error-message quality, CLI UX (`modal run`, `modal deploy`, `modal app list`) | Design primarily; small adaptations OK with attribution |
| `references/flyte-sdk` | Apache-2.0 | `TaskEnvironment` + `Image.from_debian_base()` API shape, the closest published prior art to our decorator spec | Design; adaptations OK |
| `references/parsl` | Apache-2.0 | `available_accelerators` GPU-pinning ergonomics (`["0,1", "2,3"]` → per-worker `CUDA_VISIBLE_DEVICES`, `nvidia-smi -L` autodetect) | Code may be adapted with attribution |
| `references/dstack` | MPL-2.0 | Shim/runner split, GPU blocks (fractional host sharing), instance-volume semantics, `gpuhunt` resource catalog ideas | Design; file-level copyleft applies if code is copied, so prefer design only |
| `references/t3code` | **MIT** | Cloudflare named-tunnel provisioning (`infra/relay/src/environments/ManagedEndpointProvider.ts`, including deterministic hostnames, allocation/tunnel decoupling, and loopback validation), pinned+checksummed cloudflared download (`packages/shared/src/relayClient.ts`), connector supervisor (`apps/server/src/cloud/ManagedEndpointRuntime.ts`), split-authority auth flow (`docs/internals/`) | Code may be adapted with attribution (TS → Go port for our agent; direct reuse patterns for M4) |

Also study without cloning: GPUStack's token-join worker onboarding docs (Apache-2.0), Globus Compute's AMQP-over-443 endpoint docs, and Burla's `relay/README.md` (frp reverse-tunnel topology). **Do not copy Burla code** because it uses the FSL license.

---

## 3. Architecture

```
                        ┌──────────────────────────────┐
                        │   relay-server (Go binary)    │
                        │                               │
   Python SDK ──gRPC──▶ │  API · fleet registry ·       │
   + CLI (dev laptop)   │  scheduler · job queue ·      │
                        │  blob store (code bundles,    │
                        │  results) · SQLite state ·    │
                        │  token auth · log fan-out     │
                        └───────────▲──────────────────┘
                                    │  outbound gRPC (TLS),
                                    │  agent dials server
              ┌─────────────────────┼──────────────────────┐
              │                     │                      │
     ┌────────┴────────┐   ┌────────┴────────┐   ┌─────────┴───────┐
     │ relay-agent (Go) │   │ relay-agent (Go) │   │ relay-agent (Go)│
     │ DGX Spark        │   │ RTX Desktop      │   │ MacBook         │
     │ DockerExecutor   │   │ DockerExecutor   │   │ NativeExecutor  │
     │ (CUDA, arm64)    │   │ (CUDA, amd64)    │   │ (MPS, uv venv)  │
     └─────────────────┘   └─────────────────┘   └─────────────────┘
```

**Components:**

1. **`relay` Python package.** SDK (decorators, Image, Volume, Secret, Resources) + CLI (Typer/Click). The CLI must be Python because `relay run app.py::train` imports user code.
2. **`relayd` Go binary.** One binary, two modes: `relayd server` (control plane) and `relayd agent` (worker). Cross-compiled for `linux/amd64`, `linux/arm64`, `darwin/arm64`. Distributed via `get.relay.dev/install.sh` (beta9-style installer: detects OS/arch, installs binary, registers systemd/launchd service).
3. **`proto/`.** Single protobuf source of truth for SDK↔server and agent↔server contracts. Every stream includes a version handshake. The agent and SDK report their versions, and the server rejects incompatible versions with a clear upgrade message. This avoids Ray's silent version failures.

**Executor interface** (Woodpecker's `Backend`, adapted):

```go
type Executor interface {
    Name() string
    Available(ctx) (bool, error)          // preflight: docker? nvidia toolkit? uv?
    Capabilities() []Capability            // cuda, mps, cpu; arch; max resources
    Setup(ctx, *JobSpec) error             // pull/build image OR create venv
    Start(ctx, *JobSpec) (Handle, error)
    Tail(ctx, Handle) (io.ReadCloser, error)
    Wait(ctx, Handle) (ExitState, error)
    Destroy(ctx, Handle) error
}
```

Implementations: `DockerExecutor` (Linux, CUDA via `DeviceRequests` + CDI) in M1, `NativeExecutor` (macOS, MPS, uv-managed venvs, process groups) in M5. This interface is the single decision that keeps CUDA and MPS in one system without either being a hack.

---

## 4. SDK & decorator DX (the product)

### 4.1 Core surface

```python
import relay

app = relay.App("trainer")

image = (
    relay.Image.debian("12")
    .python("3.12")                    # defaults to client's Python minor version
    .apt("git", "ffmpeg")
    .pip("torch", "transformers", "trl")
    .env(HF_HOME="/cache/hf")
)

@app.function(
    image=image,
    gpu="24GB",                        # shorthand → relay.GPU(memory="24GB")
    cpu=8,
    memory="32GB",
    target="auto",                     # or "dgx-spark" | ["dgx-spark", "rtx"] | relay.Target(labels=["training"])
    volumes={"/checkpoints": relay.Volume("gemma-ckpts")},
    secrets=[relay.Secret("huggingface")],
    timeout="8h",
    retries=1,
)
def train(model: str, lr: float = 1e-4) -> dict:
    ...
    return {"loss": final_loss}
```

Calling conventions (Modal-compatible muscle memory):

```python
train("gemma-270m")                     # plain local call with no Relay involved
train.remote("gemma-270m")              # run on fleet, block, return deserialized result
run = train.spawn("gemma-270m")         # fire-and-forget → Run handle
run.status(); run.logs(follow=True); run.result(timeout=...); run.cancel()
train.map(["a", "b", "c"])              # fan-out (M6)
```

Services:

```python
@app.service(
    gpu="40GB",
    port=8000,
    restart="always",
    expose="private",                   # default. "private" = URL reachable only inside your
                                        #   Tailscale mesh; "public" = real HTTPS URL via
                                        #   Cloudflare Tunnel. NEITHER opens inbound ports.
                                        #   service binds 127.0.0.1, connectivity is outbound-only.
    # auth="key",                       # public default: Relay mints an API key and fronts the
                                        #   service with it; strangers who find the URL get 401.
                                        #   auth="none" is an explicit, deliberate opt-out.
)
def vllm_server():
    subprocess.run(["vllm", "serve", "Qwen/Qwen3-4B", "--port", "8000"])
```

Heterogeneous accelerator targeting is our differentiator. Design it in from day one and ship MPS in M5:

```python
@app.function(
    accelerator=relay.any_of(
        relay.CUDA(memory="20GB"),
        relay.MPS(memory="20GB"),       # unified memory on Apple Silicon
    ),
)
def finetune(): ...
```

### 4.2 DX principles (enforced, not aspirational)

- **Zero-config local mode.** `pip install relay; relay run app.py::train` works with no account, server, or YAML. The CLI auto-starts an embedded local server + agent on first use. Connecting more machines is the upgrade path, not the entry bar.
- **Plain calls stay plain.** A decorated function called normally runs locally as ordinary Python. Decorators never change local semantics.
- **Errors name the fix.** Every scheduler rejection says *why per machine* and what to change:
  ```
  ✗ No machine can run `train` (needs CUDA ≥24GB, 8 CPU, 32GB RAM)
    dgx-spark    ✗ 21GB GPU memory free (18GB reserved by job hot-rod-01)
    rtx-desktop  ✗ GPU has 24GB total but 16GB free
    macbook      ✗ requires CUDA; machine has MPS  → add relay.MPS(...) to accelerator=
  Queue it anyway with --queue, or free capacity with `relay stop hot-rod-01`.
  ```
- **Fully typed.** Strict mypy/pyright on the SDK; `.remote()` preserves the wrapped function's signature and return type via `ParamSpec`/`TypeVar`.
- **No magic strings without parsers.** `gpu="24GB"`, `timeout="8h"`, `memory="32GB"` all parse at decoration time with precise errors, not at submit time.
- **Serialization honesty.** Args/results via cloudpickle with the version pinned and the container's Python matched to the client's (Image builder enforces). Results >16MB rejected with "return relay.Artifact(path) instead" guidance. This is where the Modal illusion usually dies; we handle it explicitly.
- **Code sync like Modal.** Content-addressed tarball of the project (honoring `.relayignore`), uploaded once per hash to the server blob store; agents cache by hash. No git required, committed or not.

---

## 5. CLI spec (Modal/Beam-shaped)

| Command | Does | Analogue |
|---|---|---|
| `relay run app.py::train --model gemma` | Execute function on fleet, stream logs, exit with its status | `modal run` |
| `relay deploy app.py` | Register app + start its services | `modal deploy` |
| `relay serve app.py` | Dev-mode services with hot-reload on file change | `modal serve` |
| `relay ps` | Running/queued jobs + services across fleet | `beta9 task list` |
| `relay logs <run-id> [-f]` | Stream logs | `modal app logs` |
| `relay stop <run-id>` | Cancel a run / stop a service | None |
| `relay fleet` | Machines: name, status, arch, accelerator, free/total resources, current jobs | `beta9 machine list` |
| `relay connect` | Print a one-line join command (token embedded) to paste on a new machine | `beta9 pool join-command` |
| `relay connect <token/url>` | On the new machine, install/register the agent and dial home. This flow is also embedded in the curl\|sh installer. | `beta9 pool join` |
| `relay disconnect <machine>` | Remove a machine from the fleet | None |
| `relay volume list/put/get` | Manage volumes | `modal volume` |
| `relay secret set/list` | Manage secrets (server-stored encrypted; `--scope <machine>` keeps value machine-local) | `modal secret` |
| `relay server` | Run the control plane in the foreground (advanced; normally auto-managed) | None |
| `relay dashboard` | Open the web UI (M6) | None |

**`relay connect` flow. This onboarding moment must be perfect:**

```
laptop $ relay connect
  Run this on the machine you want to add:

    curl -fsSL https://get.relay.dev | sh -s -- --join rly_tok_8f3a...@relay.example.com

spark $ curl -fsSL https://get.relay.dev | sh -s -- --join rly_tok_8f3a...@...
  ✓ Detected: linux/arm64, NVIDIA GB10 (unified memory, 128GB), Docker 27.3, NVIDIA toolkit 1.20
  ✓ Installed relayd → /usr/local/bin, registered systemd service
  ✓ Connected to control plane (outbound, no ports opened)

laptop $ relay fleet
  MACHINE      STATUS  ARCH    ACCELERATOR         FREE / TOTAL
  dgx-spark    online  arm64   CUDA GB10 (unified) 121GB / 128GB
  macbook      online  arm64   MPS M3 Max          38GB / 48GB (local)
```

Tokens are single-use, minted by the server (beta9's `CreatePoolJoinToken` pattern); agent auth thereafter via mTLS cert issued at join.

---

## 6. Scheduler ("smart selector")

Runs in `relayd server`. Fleet state comes from agent heartbeats (~10s: liveness, utilization) and a server-side **reservation ledger**. The ledger is the source of truth for allocation, never live NVML, which lies under time-slicing and is broken on GB10.

**Three phases, in order:**

1. **Filter (hard constraints).** Machine online ∧ executor supports the spec (CUDA spec → DockerExecutor machine; MPS spec → NativeExecutor machine; `any_of` → either) ∧ arch has an image/venv path (never send an amd64-only image to the Spark) ∧ explicit `target`/labels match ∧ total capacity ≥ request.
2. **Admission (capacity now).** `free = total − Σ reservations` per resource (GPU memory, CPU, RAM). On unified-memory machines (Spark, Macs), GPU + system memory draw from **one pool** that is modeled as one budget, not two. If nothing fits, the job queues and is re-evaluated on every completion, heartbeat, or join.
3. **Score (best fit among survivors).** Weighted:
   - **Image/venv cache locality** (the biggest latency win because a machine that already has the image beats a faster empty one)
   - **Best-fit packing**: minimize leftover GPU memory (keep big slots open for big jobs)
   - **Keep scarce hardware free**: small CUDA jobs prefer the RTX box over the Spark when both fit
   - Lower current utilization; `prefer=`/label affinity as tiebreak

Every decision is explainable: `relay ps` shows *why* a job landed where it did or why it is queued (the per-machine rejection table from §4.2). The scheduler is a pure function, `(spec, fleet_snapshot) → placement | queue(reasons)`, that can be unit-tested without a real machine.

---

## 7. Connectivity and tunnels (decided 2026-08-16 from T3 Connect's source)

We dissected T3 Code's T3 Connect implementation at the source level (`pingdotgg/t3code`, MIT, so code may be adapted with attribution). Their verified architecture uses **named Cloudflare Tunnels provisioned via API inside T3's own Cloudflare account** (zero quick-tunnel/`trycloudflare` code anywhere) to carry the *entire* client↔machine data plane. Their hosted relay is a control plane that stays out of the hot path and even performs its sparse control calls *through* the tunnel. The machine, not the relay, is the sole auth authority.

**A. Control channel (agent ↔ server, `relay connect`): plain outbound gRPC over TLS. Decided.**
T3's evidence *strengthens* this choice rather than contradicting it:
- T3 gets away with no persistent control socket because their control ops are sparse request/response (health checks, credential minting). Relay's control plane is the opposite: latency-sensitive push (job dispatch), frequent heartbeats, live fleet state for the scheduler.
- Their measured tunnel-flap cost: replacing a tunnel UUID takes **1–2 minutes of DNS/route propagation** (their own code comments call it "the dominant cost of an update restart"). A gRPC stream reconnects in seconds. For a system where a machine dropping means jobs stall, that's decisive.
- Their issue #3054: hand-rolled WebSocket keepalives tore down connections that TCP was riding out fine over ordinary WireGuard. Conclusion for us: gRPC/HTTP2 with tuned keepalives + gRPC's native reconnect/backoff, and the agent supervisor gets **exponential backoff + jitter + a restart cap**. T3's connector supervisor has neither and hot-loops on dead tokens, a known gap we fix instead of copying.

**B. Public HTTPS exposure for `@app.service` (`expose="public"`): named Cloudflare Tunnels under OUR Cloudflare account, T3's exact pattern. Decided.**
Design elements adopted directly from their implementation:
- **Deterministic hostnames**: `hash(stage, userId, machineOrServiceId)` → first 16 hex → `prod-<16hex>.<tunnel-zone>`; idempotent re-provisioning reuses the URL. Tunnel zone is a **separate registrable domain** from anything else we run (isolation; PSL registration becomes relevant only if this ever goes public).
- **Decouple hostname reservation from tunnel lifetime** (their allocation-row vs tunnel-row split): delete idle tunnels (Cloudflare bills per provisioned tunnel) while the URL stays reserved and comes back on next deploy.
- **cloudflared lifecycle**: downloaded on demand, version-pinned + SHA-256 verified, version-namespaced install dir, atomic two-stage rename, `tunnel run` with `TUNNEL_TOKEN` env, token scrubbed from logs. Never vendored, never a user install step.
- **The tunnel is never the security boundary**: ingress is pinned to `127.0.0.1:<port>` and validated on *both* sides (agent and server), forwarded-host headers are rejected, and the split-authority credential model prevents the control plane from minting machine access tokens by itself. It can only ask the agent to mint a short-lived, key-bound credential. A compromised control plane ≠ live sessions on our GPUs.
- **Typed, terminal, non-retryable quota errors** (T3's worst bug class: a tunnel quota surfacing as an opaque 403 the client retried for 10 minutes).
- **Public ≠ open (decided 2026-08-16):** `expose="public"` means *publicly reachable via Cloudflare's edge*, never publicly *accessible*. Auth is on by default: Relay injects an auth proxy in front of the service and mints an API key at deploy (`relay deploy` prints it; `relay secret` rotates it); anonymous access requires explicit `auth="none"`. In both modes the service binds loopback only, all connectivity is outbound, and no inbound port ever exists. A stranger gets 401 at the edge in public mode and cannot route to the hostname at all in private mode.
- **Provider abstraction from day one** (their enum: `manual | cloudflare_tunnel | ...`): cloudflared is a transport detail, not the product contract. **Tailscale Serve** is the optional `expose="private"` provider, matching the mesh-for-private / tunnel-for-public split that T3 shipped and recommends. Self-hosted frp stays a documented escape hatch.

**Private-use note (current mode):** while Relay is just for us, this needs one personal Cloudflare account + one cheap domain for the tunnel zone; the free tier covers personal-scale tunnel usage. LAN-only operation requires nothing in A or B because the agent finds the server directly.

---

## 8. Milestones

Each milestone ends with something demoable. Cut scope, never quality.

**M0: Walking skeleton (DX validation gate)** ~1–2 weekends
`proto/` contracts; Python SDK skeleton (`App`, `@app.function`, resource parsing, cloudpickle runtime); embedded local mode: `relay run app.py::train` executes the function in a local Docker container with GPU if present, streams logs, returns the result. No server/agent split yet. *Gate: does the decorator DX feel like Modal? Iterate here until yes because this is the cheapest place to change the API.*

**M1: Agent/server split + first remote machine** ~2–3 weeks
`relayd server` (SQLite, blob store, token auth) + `relayd agent` (outbound gRPC, DockerExecutor, CUDA via DeviceRequests/CDI, NVML inventory **with `/proc/meminfo` fallback for GB10**); `relay connect` + installer script; `relay ps/logs/stop/fleet`; code-bundle sync; results round-trip. *Demo: laptop submits `train.spawn()`, RTX desktop runs it.*

**M2: Scheduler + fleet + Spark** ~2 weeks
Reservation ledger, three-phase scheduler, `target="auto"`, queueing, per-machine rejection explanations; volumes (host-local, dstack instance-volume semantics); multi-arch awareness; DGX Spark onboarded end-to-end (arm64 agent, CUDA 13/sm_121 image, unified-memory budget). *Demo: two jobs race; scheduler packs them sensibly; third queues with a clear reason.*

**M3: Image builder** ~2 weeks
Chained `Image` API compiling to a Dockerfile; **builds run natively on the target-arch machine** (the fleet is the build farm, with no QEMU); content-hash caching; embedded registry in the server (or per-agent cache exchange); `.python()` pinned to client minor version; secrets injection. *Demo: cold build once, warm run in seconds on both amd64 and arm64.*

**M4: Services + exposure** ~2 weeks
`@app.service`: deploy, health checks, `restart="always"`, port registry; `relay deploy` / `relay serve`; exposure providers including Cloudflare Tunnel (`expose="public"`) and a fleet-internal proxy for `private`. *Demo: vLLM on the Spark with a public HTTPS URL, zero router config.*

**M5: Apple Silicon / MPS** ~2–3 weeks
`NativeExecutor`: uv-managed per-job venvs, process-group lifecycle, MPS detection, unified-memory budget; `relay.MPS` + `any_of` scheduling; macOS installer path (launchd). *Demo: the same `finetune()` with `any_of(CUDA, MPS)` lands on the Mac when the GPUs are busy.*

**M6: Polish & breadth**
`train.map()`, cron schedules, `relay dashboard` (single-page web UI from server), `relay shell <machine>`, metrics history, docs site, public release prep.

---

## 9. Key decisions & risks

- **License:** Apache-2.0. Hard rule: no code from beta9 (AGPL), Burla (FSL), or drone-runner-docker (Polyform). Use designs only. Add an attribution file for adapted Apache code (Woodpecker, Parsl, Bacalhau, modal-client).
- **GPU admission is ours to own** (no backend anywhere does it): reservation ledger + explicit unified-memory modeling. Time-slicing has no isolation, so we schedule conservatively by memory and document that co-located jobs share compute.
- **Naming and distribution:** the name is **Relay**. The source repository became public on 2026-08-22; PyPI publishing and a dedicated domain remain future decisions. Until a package is published, install from the repository (`pip install -e sdk/`, `go build`).
- **Serialization drift** is the top DX killer: pin cloudpickle, match Python minors, version-handshake every stream, fail loudly with fix-naming errors.
- **Spark quirks** (from research): NVML memory `Not Supported` → meminfo fallback; no MIG ever, MPS crashes on current drivers → time-slicing only; `sm_121` needs CUDA 13 toolchain floor in default images.
- **Scope discipline:** no Kubernetes backend, no cloud provisioning, no multi-tenant auth until the core loop is loved by its first user (you). The Executor interface keeps those doors open without paying for them now.

## 10. Repo layout

```
Relay/
├── PLAN.md
├── proto/                    # protobuf: relay.v1 (jobs, fleet, logs, blobs)
├── sdk/                      # Python package (import name: relay)
│   ├── src/relay/
│   │   ├── app.py  function.py  service.py  image.py  resources.py
│   │   ├── runtime/          # in-container: invoke, serialize, results
│   │   └── cli/              # Typer app: run, deploy, connect, ps, ...
│   └── tests/
├── relayd/                   # Go module
│   ├── cmd/relayd/           # main: server | agent subcommands
│   ├── internal/server/      # api, scheduler, ledger, blob, sqlite, tokens
│   ├── internal/agent/       # dial, heartbeat, executors/{docker,native}, inventory
│   └── internal/proto/       # generated
├── installer/                # install.sh (get.relay.dev)
├── references/               # cloned study repos (git-ignored, never shipped)
└── research/                 # decision docs
```

## 11. Decisions log

0. **Implementation decisions (2026-08-16, during build):**
   - *SDK↔server transport*: HTTP/JSON + streamed text logs instead of gRPC (grpcio wheel friction on Python 3.14; shapes are request/response). proto stays the agent↔server contract.
   - *Agent transport security*: TLS with T3-style fingerprint pinning. The server self-generates a cert, `relay connect` join strings carry `token@host:port#fp`, agents verify on first join, and they pin the exact cert thereafter. The HTTP API binds loopback by default; the full HTTPS/remote story lands with M4 tunnels.
   - *No embedded image registry* (M3): images build natively on each machine and cache there by content-hash tag; the scheduler's cache-locality scoring makes warm machines win. A registry adds moving parts with marginal benefit at personal-fleet scale.
   - *Secrets model* (M3): a secret is a named env var (UPPER_SNAKE_CASE), stored AES-GCM-encrypted server-side, write-only over HTTP, released only to the agent whose active run declares it. Local mode reads the client's own environment instead.
   - *Known residual*: a network partition in the seconds between assignment delivery and the agent's BUILDING status can theoretically double-run a job after requeue; the agent dedups by run ID in-process, so same-machine duplicates are impossible. Accepted at personal-fleet scale (documented in the M1 review response).
   - *M4 exposure redesign (post-review)*: the auth proxy fronts BOTH exposure modes. Containers always bind loopback, private mode is key-gated by default too, and the proxy port stays stable across restarts. Public tunnels are cloudflared quick tunnels as the recorded interim (named tunnels under an owned CF account await account credentials).
   - *any_of semantics*: alternatives are UNORDERED by definition. The scheduler optimizes across them; a strict-preference `prefer=` API is a possible later addition.
   - *Tailscale/Headscale integration (2026-08-17, user-requested)*: private endpoints prefer the machine's MagicDNS name when a tailnet is up. This works with both Tailscale and Headscale through plain reachability with zero configuration. The new `expose="funnel"` mode runs a supervised `tailscale funnel` for a stable public ts.net URL with no domain (Tailscale-hosted control plane only). This supersedes the named-Cloudflare-tunnel need for now, since letab.eu's DNS must stay on Vercel and no dedicated domain is available; the CF named-tunnel provider remains the recorded follow-up if a domain materializes.
   - *M6 scope (decided during build)*: `relay shell` is bounded non-interactive host exec (`relay shell spark nvidia-smi`). Full PTY shells are out because machines are SSH-reachable and a full-duplex Python path would need heavy dependencies. Metrics history was dropped because the reservation ledger is the operative signal at this scale. Cron is a 5-field parser with Vixie dom/dow semantics, at-most-once per matching minute. The dashboard was originally one embedded HTML page. It was rebuilt on 2026-08-18 (by user request) as Vite+React+Tailwind styled after modal.com, built by `make dashboard` into `relayd/internal/server/webui/` and embedded with `go:embed`; the binary still ships everything.

1. **Tunnels (§7, decided 2026-08-16):** control channel = plain outbound gRPC/TLS; public service exposure = named Cloudflare Tunnels under our own CF account following T3 Connect's verified pattern (deterministic hostnames, reservation/tunnel decoupling, pinned+checksummed cloudflared, loopback-only ingress, split-authority credentials); Tailscale Serve as optional private-exposure provider. T3's repo is MIT, so its `relayClient.ts` / `ManagedEndpointProvider.ts` / `ManagedEndpointRuntime.ts` may be adapted with attribution.
2. **Naming:** "Relay"; source published publicly on 2026-08-22, with PyPI and domain decisions deferred (see §9).
3. **Sequencing (decided 2026-08-16):** training jobs first (M1–M3), services in M4. The vLLM-public-endpoint story matters, but training is the higher priority, so the order stands as planned.
