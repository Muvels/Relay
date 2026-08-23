// API client for the relayd control plane, using the same JSON endpoints as the CLI
// uses, same bearer token (kept in localStorage, prompted once).

export interface Machine {
  id: string;
  name: string;
  online: boolean;
  os: string;
  arch: string;
  cpu_cores: number;
  memory_mib: number;
  unified_memory: boolean;
  executors: string[];
  accelerators: {
    kind?: string;
    name?: string;
    memory_mib?: number;
    index?: number;
    memory_unreliable?: boolean;
  }[];
  reserved: {
    cpu_cores: number;
    memory_mib: number;
    accelerator_memory_mib: Record<string, number>;
    active_runs: number;
  };
  usage?: {
    sampled_at: string;
    cpu_used_cores: number;
    memory_used_mib: number;
    disk_free_mib: number;
    disk_total_mib: number;
    cpu_usage_available: boolean;
    memory_usage_available: boolean;
    disk_usage_available: boolean;
    accelerators: {
      index: number;
      memory_used_mib: number;
      utilization: number;
      memory_usage_available: boolean;
      utilization_available: boolean;
    }[];
  };
  last_seen: string;
}

export interface RunEvent {
  state: string;
  ts: number; // unix millis
}

export interface Run {
  id: string;
  app: string;
  function: string;
  kind: string;
  state: string;
  detail: string;
  machine: string;
  endpoint?: string;
  resources?: string;
  image?: string;
  events?: RunEvent[];
  exit_code: number;
  created_at: string;
  updated_at: string;
}

export interface Schedule {
  app: string;
  function: string;
  cron: string;
  last_run?: string;
}

export interface Snapshot {
  version: string;
  machines: Machine[];
  runs: Run[];
  services: Run[];
  secrets: string[];
  schedules: Schedule[];
}

const TOKEN_KEY = "relay_token";

export function getToken(): string | null {
  return localStorage.getItem(TOKEN_KEY);
}

export function setToken(token: string) {
  localStorage.setItem(TOKEN_KEY, token);
}

export function clearToken() {
  localStorage.removeItem(TOKEN_KEY);
}

export class Unauthorized extends Error {}

async function api<T>(path: string): Promise<T> {
  const resp = await fetch(path, {
    headers: { Authorization: `Bearer ${getToken() ?? ""}` },
  });
  if (resp.status === 401) throw new Unauthorized();
  if (!resp.ok) throw new Error(`${path}: HTTP ${resp.status}`);
  return resp.json();
}

export async function fetchRun(id: string): Promise<Run> {
  return api<Run>(`/v1/runs/${encodeURIComponent(id)}`);
}

export async function fetchSnapshot(options?: {
  machineTelemetry?: boolean;
}): Promise<Snapshot> {
  const machinesPath = options?.machineTelemetry
    ? "/v1/machines?telemetry=1"
    : "/v1/machines";
  const [health, machines, runs, services, secrets, schedules] =
    await Promise.all([
      api<{ version: string }>("/v1/health"),
      api<{ machines: Machine[] }>(machinesPath),
      api<{ runs: Run[] }>("/v1/runs?limit=100"),
      api<{ services: Run[] }>("/v1/services"),
      api<{ secrets: string[] }>("/v1/secrets"),
      api<{ schedules: Schedule[] }>("/v1/schedules"),
    ]);
  return {
    version: health.version,
    machines: machines.machines ?? [],
    runs: runs.runs ?? [],
    services: services.services ?? [],
    secrets: secrets.secrets ?? [],
    schedules: schedules.schedules ?? [],
  };
}

/** Stream a run's logs; calls onChunk as text arrives. Returns an aborter. */
export function streamLogs(
  runId: string,
  onChunk: (text: string) => void,
  onDone: () => void,
): () => void {
  if (!/^run_[a-z0-9]+$/.test(runId)) return () => {};
  const controller = new AbortController();
  (async () => {
    try {
      const resp = await fetch(`/v1/runs/${runId}/logs`, {
        headers: { Authorization: `Bearer ${getToken() ?? ""}` },
        signal: controller.signal,
      });
      if (!resp.ok || !resp.body) return;
      const reader = resp.body.getReader();
      const decoder = new TextDecoder();
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        onChunk(decoder.decode(value, { stream: true }));
      }
    } catch {
      /* The drawer handles aborts and transport hiccups when it closes. */
    } finally {
      onDone();
    }
  })();
  return () => controller.abort();
}

export function timeAgo(iso: string): string {
  const then = new Date(iso).getTime();
  if (Number.isNaN(then)) return "";
  const s = Math.max(0, (Date.now() - then) / 1000);
  if (s < 60) return `${Math.floor(s)}s ago`;
  if (s < 3600) return `${Math.floor(s / 60)}m ago`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`;
  return `${Math.floor(s / 86400)}d ago`;
}
