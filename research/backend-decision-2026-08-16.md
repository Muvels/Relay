# Relay execution-layer decision: dstack vs. build vs. adopt (2026-08-16)

Synthesis of three parallel research passes (dstack deep dive; Modal-like OSS platforms; orchestrator backends + homelab GPU tools). All claims below were verified against live sources in August 2026; links inline.

## Verdict

**Build the execution layer yourself: a thin Go agent on the Docker Engine API with an outbound-only gRPC connection to your control plane. Do not build on dstack. Do not fork beta9. Nothing else is closer.**

Three convergent findings drive this:

1. **The outbound-only agent model is non-negotiable and almost nobody has it.** dstack SSHes *into* machines (inbound) and polls every 7–14s with a 3s default timeout hostile to home networks. Ray requires bidirectional connectivity across ~10,000 ports. SkyPilot's "existing machines" feature secretly installs k3s via inbound SSH. Only beta9 (AGPL) and a self-built agent match the design.
2. **The genuinely hard problems are unsolved by every candidate, so you write them regardless:** GPU-memory admission control (no backend does it; k8s time-slicing has explicitly "no memory or fault-isolation"; Nomad allocates whole GPUs only), the Modal-style multi-arch `Image` builder (nobody provides one), and the macOS native-process path (Metal has no container passthrough, as confirmed by Docker and Apple).
3. **The part a backend would give you is small.** Measured from shipping prior art: Woodpecker's agent + Docker backend ≈ 3,169 LOC; drone-runner-docker ≈ 4,562 LOC total. GPU device requests are one JSON field (`HostConfig.DeviceRequests`); log streaming is a documented 5-step framing protocol. Estimated agent cost: **2,500–4,500 LOC**. Meanwhile Woodpecker's *Kubernetes* backend is 2,406 LOC vs. 998 for its Docker backend. Adopting an orchestrator replaces plumbing with more plumbing plus a permanent ops surface.

## Why not dstack (despite being healthy and Spark-validated)

- **Connectivity is backwards**: server-initiated SSH (`paramiko` in `ssh_fleets/provisioning.py`), no agent-dials-home mode; `proxy_jump` only moves the inbound requirement. You would build your own tunnel layer anyway, at which point dstack stops paying rent.
- **The Python API is the wrong surface**: orphaned from docs nav, types are literally internal Pydantic models, broke twice in 8 months (submission methods removed in 0.20.0; Pydantic v2 swap in 0.21.0), compatibility tested only N-1 at weekly cadence. Stable surface is the HTTP API → you'd write your own client.
- **Apple Silicon is foreclosed**: PR #3944 (June 2026) deleted the darwin build path and the `local` backend, calling macOS support "fragile." An MPS worker = fork against maintainer direction.
- **`@app.service` degraded on pure on-prem**: gateways require a cloud/k8s backend. Without one, there is no autoscaling, WebSocket support, or custom-domain HTTPS. "Gateways on SSH fleets" is an unchecked "requires research" roadmap line.
- **Warm-start latency unproven**: 10–15s warm until June 2026; fixes targeting ~1s shipped unbenchmarked.
- Bus factor ≈ 2, ~$0.5M raised in 2023, no round since.
- Positives worth keeping: MPL-2.0 is safe; founder is publicly anti-decorator (differentiation intact); DGX Spark validated on real GB10 hardware; on-prem is first-class (GPU blocks, instance volumes). **dstack is a reference and a possible contribution target, not a foundation.**

## Why not beta9 (Beam), the closest existing thing (9/10 fit)

Since June 2026 it has almost exactly the Relay spec: `@function`/`@endpoint`/`@task_queue` decorators, a chained `Image()` builder, and `beta9 pool join`, which provides an outbound-only agent (embedded tsnet transport) that auto-installs Docker + NVIDIA toolkit and runs containers as Docker siblings with **no k3s on the worker**. Disqualifiers:

- **AGPL-3.0** forecloses a hosted product and taints code-copying into an MIT/Apache Relay.
- **Control plane requires Kubernetes + Helm + Postgres + Redis + JuiceFS + S3**; no docker-compose path exists.
- **arm64 runner images do not exist.** Gateway and runner images are amd64-only; the fix is an unmerged community PR (#1785, open since 2026-07-18). **A DGX Spark cannot run stock beta9 today.**
- Bus factor 1 (100/100 recent commits by the CTO); self-host docs site has an expired TLS cert; BYOH is a funnel to their cloud.

**Use it as the design reference**: `pkg/agent/` (transport, preflight GPU/Docker detection) and `pkg/gateway/agent_install.go` (Tailscale-style installer) are a months-of-work head start in design, free to read.

## Everything else, one line each

| Tool | Verdict |
|---|---|
| Nomad | BUSL (Licensor: IBM) explicitly bans embedding in a *paid* competing product. Choosing it means choosing never to charge; the NVIDIA plugin has been unreleased since 2024-08, arm64 binaries are missing, and allocation is whole-GPU-only. No OpenTofu-style fork exists. |
| k3s | Works on Spark (proven configs exist) but automates only 1 of 3 GPU layers, ~1.4 GB control plane, can never include Macs. Keep as a *future second backend* behind the executor interface. |
| Docker Swarm | GPU device model dead since 2019 (NVIDIA's PR closed unmerged); the one workaround silently broke for 5 months in 2025–26. Ruled out. |
| SkyPilot | "SSH node pools" = curl-pipes get.k3s.io + Helm + GPU Operator; 8-core/16GB API server recommended; open unbounded-hang bug on DGX Spark (#10177). Competitor, not foundation. |
| Ray | Bidirectional ports (10002–19999 inbound per node) + Python *patch-level* version lock across the fleet. Incompatible. Possibly a workload Relay *launches* later. |
| GPUStack | Inference-only forever (training FR open since Jan 2025, zero comments); v2 dropped macOS workers. Competitor for the serving half; its worker-join UX is the best template for Relay's agent onboarding. |
| Kalavai | k3s wrapper at hobby scale (221★). |
| exo | Stalled (0 commits in 9+ weeks), CPU-only on Linux, inference-only. |
| Globus Compute | Right architecture (outbound AMQP-over-443 endpoints), but closed control plane, paid for commercial use, Linux-only, no decorators. Reference only. |
| Covalent | Company acquired by DataRobot 2025-02; docs/website down; abandoned. |
| Runhouse | Pivoted to k8s-only Kubetorch late 2025, acqui-hired by Anthropic Apr 2026. Market signal: this niche does not sustain VC companies, so own your stack and keep it small. |
| Determined AI | Had the exact master + dial-out-agent architecture; dead March 2025 (docs domain NXDOMAIN). Orphaned users = potential early adopters. |

## Recommended architecture

- **Copy Woodpecker's `Backend` interface verbatim** (Apache-2.0, 8 methods: SetupWorkflow/StartStep/TailStep/WaitStep/DestroyStep…). It is proven to admit Docker, Kubernetes, *and* local-process implementations. This keeps k3s and macOS-native as future backends, not rewrites.
- **Transport**: gRPC bidirectional streaming, agent dials out (Woodpecker `agent/rpc` ≈ 1,164 LOC is the reference). Read drone-runner-docker for architecture but **do not copy code** (Polyform license, not open source).
- **GPU plumbing**: `DeviceRequests` via Docker API; prefer CDI (toolkit ≥ v1.18 ships `nvidia-cdi-refresh`; stale CDI specs after driver upgrades are a known landmine).
- **Steal**: Parsl's `available_accelerators` GPU-pinning ergonomics; flyte-sdk's `TaskEnvironment` + `Image.from_debian_base()` API shape; Bacalhau's batch/ops/daemon/service job taxonomy (Apache-2.0, readable Go); GPUStack's token-join worker onboarding; beta9's installer + preflight design.

## DGX Spark landmines (verified)

- `nvidia-smi`/NVML report GPU memory as **Not Supported** on GB10 (unified memory), which NVIDIA says is expected. This broke NVIDIA's own k8s plugin, the DRA driver, HAMi, and Nomad's plugin. **Relay's inventory code needs a `/proc/meminfo` fallback.**
- **MIG is not supported on Spark and never will be** (NVIDIA statement); MPS crashes as of driver 580.126; **time-slicing is the only sharing mode** → Relay must own GPU-memory admission control.
- GB10 is `sm_121/sm_121a`; older toolchains fail (`ptxas fatal`); CUDA 13 floor. Multi-arch images are first-class concerns; build natively on the Spark, not via QEMU.
- Version floors: k8s-device-plugin ≥ v0.17.4 if k8s is ever used; avoid container toolkit v1.18.0–v1.18.2.

## Gate experiments before/while building

1. Prototype the decorator → JobSpec → local Docker executor loop (~a weekend; container lifecycle is 400–900 LOC per prior art) to validate the DX without any backend bet.
2. On the Spark: verify GPU container runs via plain Docker API `DeviceRequests` + CDI, and that the NVML fallback reports sane memory.
3. Optional (only if reconsidering dstack): measure submitted→first-log on a warm SSH fleet with pinned sshd-baked image on ≥ 0.20.26; need ≤ 3s for `.spawn()` UX.
