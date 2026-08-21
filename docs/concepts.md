# Concepts

## Functions

`@app.function(...)` declares a function's runtime requirements. Everything
is validated at import time. Typos fail when the module loads, not at
submit time. A decorated function called normally runs locally as ordinary
Python; only `.remote()`, `.spawn()`, and `.map()` involve Relay.

- `.remote(*args)` runs the function, blocks, and returns the deserialized result (or raises
  the remote exception with its full remote traceback attached).
- `.spawn(*args)` returns a Run handle: `.status()`, `.logs(follow=)`,
  `.result(timeout=)`, `.cancel()`. Fleet handles are server-backed and work
  from any process.
- `.map(items, return_exceptions=False)` creates one run per item, with results
  yielded in input order. With `return_exceptions=True` a failed item
  yields its exception object and iteration continues; the default raises
  (which, as with any generator, ends the iteration).

Arguments and results travel as cloudpickle inside a versioned envelope; the
container's Python minor always matches yours (the default image guarantees
it, and mismatches fail with the fix named). Results are capped at 16MB;
write big artifacts to a volume and return the path.

## Images

`relay.Image` is an immutable chained builder compiled to a Dockerfile whose
hash names the image. Identical definitions never rebuild, and any change
to compilation re-tags automatically.

```python
relay.Image.debian("12").python("3.12").apt("ffmpeg").pip("torch").env(HF_HOME="/cache")
```

Images build **natively on the machine that runs them** (the fleet is the
build farm, so an arm64 Spark builds arm64 layers without QEMU or a registry).
Warm machines win placement via cache-locality scoring.

**Native eligibility**: pip-only images (no `apt`/`run`/`from_registry`)
can also run *without* a container, which is required for MPS on Apple Silicon.

## Scheduler

Three phases, pure function, every rejection explained per machine:

1. **Filter:** online, explicit `target=` match, executor capability
   (CUDA→docker-cuda, MPS→native-mps + pip-only image, services→Docker).
2. **Admission:** against a **reservation ledger** derived from live runs
   (never live utilization: time-slicing lies and GB10 NVML is broken).
   Unified-memory machines (DGX Spark, Apple Silicon) budget ONE pool for
   RAM + accelerator; `gpu="any"` reserves the device exclusively.
3. **Score:** image-cache locality dominates, then tightest-fit packing
   (small jobs keep big cards free), then load spreading.

`relay.any_of(...)` lists **unordered** alternatives. The scheduler picks
whichever places best. Queued runs carry the reason in `relay ps`:

```
queued: dgx-spark ✗ 8GB of 128GB unified memory free, needs 110GB; macbook ✗ requires CUDA; machine has MPS → add relay.MPS(...) to accelerator=
```

## Executors

- **DockerExecutor** (Linux, macOS-CPU): per-run containers, GPUs pinned to
  exactly the scheduler-granted device indices (never `--gpus all`), cgroup
  CPU/RAM limits.
- **NativeExecutor** (Apple Silicon): uv-managed venvs keyed by
  (python, pip-set), process-group lifecycle, Metal reachable because there
  is no container in the way. There is no container isolation, so it shares the same trust domain as
  everything else in a personal fleet.

## Volumes

`relay.Volume("name")` mounts a named directory that persists **on the
machine that ran the job** (`~/.relay/volumes/...` locally, the agent state
dir on fleet machines). Not replicated. Native (MPS) runs support volumes
under `/workspace/...` only.

## Secrets

A secret is a named env var (`Secret("HF_TOKEN")` → `os.environ["HF_TOKEN"]`).
Fleet: stored AES-GCM-encrypted, write-only over HTTP, released only to the
agent whose active run declares it. Local mode: your own environment.

## Services

`@app.service(port=..., expose="private"|"public", auth="key"|"none")` runs
a supervised server: restart with backoff (reset only after 30s of stable
health), TCP health gating, fresh workspace per attempt.

Exposure: the container always binds loopback; an agent-side auth proxy is
the only listener, in every mode:

- **`private`** (default): proxy published on the machine (stable port
  across restarts). When the machine is on a **Tailscale or Headscale
  tailnet**, the reported endpoint is its MagicDNS name and is reachable from
  all your devices, anywhere, with zero extra processes. Otherwise the
  LAN IP.
- **`funnel`**: a supervised `tailscale funnel` gives a **stable** public
  `https://machine.tailnet.ts.net` URL without requiring a domain. This mode requires the
  Tailscale-hosted control plane (not Headscale) with Funnel allowed in
  the tailnet policy; bandwidth-limited by Tailscale.
- **`public`**: cloudflared quick tunnel (URL changes per deploy; the
  named-tunnel provider with stable custom-domain URLs remains the
  recorded follow-up requiring Cloudflare zone credentials).

Keys check in constant time and never forward upstream. `relay deploy`
replaces the live generation readiness-first: the old one is canceled only
once the new one is running.

## Schedules

`@app.function(schedule="0 3 * * *")` + `relay deploy` registers a 5-field
cron (Vixie dom/dow semantics). The server fires it at most once per
matching minute; `relay schedules` lists, `relay unschedule app fn` removes.

## Trust model (read once)

A Relay fleet is ONE trust domain: your code, your images, your machines.

- **The API token is a fleet-admin credential. Treat it like root on every
  machine.** By design it runs arbitrary code (jobs, native MPS runs,
  `relay shell`); anyone holding it holds your fleet. It lives in
  `~/.relay/config.toml` (0600) and the server data dir (0700).
- Results are pickles. Code executes on unpickle, matching Modal's stance.
- The fleet DB is a credential vault (0700/0600): agent tokens and service
  keys live there; user secrets additionally get AES-GCM at rest.
- Transport is TLS with join-time fingerprint pinning; exposed services are
  key-gated by default. The auth proxy is the sole LAN/public listener.
  processes and sibling containers ON the same machine can bypass it via
  the container's bridge IP, and are trusted (same domain).
- Cron is best-effort at-most-once per matching minute in the server's
  local timezone, with no catch-up after downtime.
- Blobs (code bundles, results) are content-addressed and never garbage-
  collected; at personal scale this grows slowly, and a future `relayd gc`
  is the recorded follow-up.

What Relay does NOT do: protect you from your own code, or sandbox
MPS-native runs.
