# Architecture

```
              Python SDK + CLI (relay)
                    │  HTTP/JSON + streamed logs, bearer token
                    ▼
        ┌───────────────────────────┐
        │   relayd server (Go)      │  SQLite · CAS blob store · log hub
        │   scheduler · ledger      │  secrets (AES-GCM) · cron · exec hub
        │   dashboard (React,       │  join tokens · TLS cert + fingerprint
        │   embedded at build time) │
        └────────────▲──────────────┘
                     │  gRPC over TLS (fingerprint-pinned),
                     │  agent DIALS OUT and holds one stream
      ┌──────────────┼────────────────┐
      ▼              ▼                ▼
  relayd agent   relayd agent    relayd agent
  DGX Spark      RTX desktop     MacBook
  Docker+CUDA    Docker+CUDA     native venvs (MPS) + Docker (CPU)
```

## The load-bearing choices

- **Outbound-only agents.** The server never connects to a machine; agents
  dial out and hold a bidirectional gRPC stream (reconnect with backoff +
  jitter + cap). Works behind NAT/CGNAT with zero router config. This is why
  Relay isn't built on dstack (inbound SSH) or Ray (bidirectional port
  ranges). See `research/backend-decision-2026-08-16.md`.
- **TLS by pinning, not PKI.** The server self-generates one cert; join
  strings carry its fingerprint (`token@host:port#fp`); agents verify on
  first join and pin the exact certificate afterward (the T3 Connect model).
- **Two transports, one model.** proto (`proto/relay/v1`) is the agent
  contract; the SDK speaks HTTP/JSON to the same server (grpcio is a heavy
  wheel and the SDK's shapes are request/response).
- **Content-addressed everything.** Code bundles (deterministic tars that
  embed the SDK, so containers need no Relay preinstalled), call envelopes,
  results, and images (Dockerfile-hash tags, built natively per machine) are
  all keyed by content. Caches never go stale; warm machines win placement.
- **State machine with conditional writes.** Run transitions are SQL-
  conditional (terminal states settle exactly once; status updates carry a
  machine-ownership predicate), assignments persist before dispatch and
  revert on failed delivery/detach, and the reservation ledger derives from
  run rows, so a server restart loses nothing.
- **Reliable statuses, lossy logs.** Terminal statuses block rather than
  drop, retry across reconnects, and hold their run in the agent's active
  set so reconcile can't mark a finished run lost. Log lines drop under
  backpressure by design.

## Run lifecycle

```
pending ── scheduler ──► assigned ──► building ──► running ──► succeeded
   ▲                        │                                   failed (user code raised)
   └── requeued on ─────────┘                                   error  (infrastructure)
       undelivered/detach                                       canceled · lost
```

The janitor sweeps runs whose machine went silent (60s grace, heartbeats
every 10s, any inbound message counts as liveness); reconcile on agent
reconnect settles runs the agent no longer owns.

## Where things live

| | |
|---|---|
| Server state | `~/.relay/server/`: `relay.db`, `blobs/`, `logs/`, `secret.key`, `tls-*.pem`, `api_token` (0700/0600 throughout) |
| Agent state | `~/.relay/agent/`: `credentials.json` (token + pinned cert), `runs/`, `volumes/`, `venvs/` |
| Client config | `~/.relay/config.toml`, local run dirs under `~/.relay/runs/` |

## Deliberate non-goals (v1)

Kubernetes/cloud backends, an image registry, multi-tenant auth, interactive
PTY shells, app-level health checks, named Cloudflare tunnels (needs account
credentials), and Windows. The executor interface and the decisions log in
`PLAN.md` §11 keep those doors open without paying for them now.
