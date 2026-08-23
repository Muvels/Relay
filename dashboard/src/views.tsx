import { useEffect, useMemo, useRef, useState } from "react";
import type { Machine, Run, RunEvent, Schedule, Snapshot } from "./api";
import { fetchRun, streamLogs, timeAgo } from "./api";
import {
  ActivityTicks,
  Card,
  Chip,
  Empty,
  StateText,
  StatusDot,
  stateColor,
} from "./ui";

const ACTIVE = new Set(["pending", "assigned", "building", "running"]);

export interface DrawerTarget {
  id: string;
  title: string;
}

// ------------------------------------------------------------------ Apps

interface AppFn {
  name: string;
  kind: string;
  resources: string;
  lastState: string;
  lastAt: string;
  endpoint?: string;
  states: string[];
  lastRunId: string;
}

interface AppGroup {
  name: string;
  fns: AppFn[];
  live: boolean;
  lastAt: string;
}

function groupApps(snapshot: Snapshot): AppGroup[] {
  const byApp = new Map<string, Map<string, AppFn>>();
  const consider = (r: Run) => {
    const fns = byApp.get(r.app) ?? new Map<string, AppFn>();
    byApp.set(r.app, fns);
    const fn = fns.get(r.function);
    if (!fn) {
      fns.set(r.function, {
        name: r.function,
        kind: r.kind,
        resources: r.resources || "CPU",
        lastState: r.state,
        lastAt: r.updated_at,
        endpoint: r.endpoint,
        states: [r.state],
        lastRunId: r.id,
      });
    } else {
      fn.states.push(r.state);
      if (r.updated_at > fn.lastAt) {
        fn.lastAt = r.updated_at;
        fn.lastState = r.state;
        fn.endpoint = r.endpoint || fn.endpoint;
        fn.lastRunId = r.id;
      }
    }
  };
  for (const r of [...snapshot.services, ...snapshot.runs]) consider(r);

  return [...byApp.entries()].map(([name, fns]) => {
    const list = [...fns.values()].sort((a, b) => a.name.localeCompare(b.name));
    return {
      name,
      fns: list,
      live: list.some((f) => ACTIVE.has(f.lastState)),
      lastAt: list.reduce((m, f) => (f.lastAt > m ? f.lastAt : m), ""),
    };
  });
}

export function AppsView({
  snapshot,
  openRun,
}: {
  snapshot: Snapshot;
  openRun: (target: DrawerTarget) => void;
}) {
  const [query, setQuery] = useState("");
  const [filter, setFilter] = useState<"live" | "stopped" | null>("live");
  const [sort, setSort] = useState<"recent" | "name">("recent");
  const apps = useMemo(() => groupApps(snapshot), [snapshot]);

  const shown = apps
    .filter((a) => {
      if (filter === "live" && !a.live) return false;
      if (filter === "stopped" && a.live) return false;
      const q = query.toLowerCase();
      return (
        !q ||
        a.name.toLowerCase().includes(q) ||
        a.fns.some((f) => f.name.toLowerCase().includes(q))
      );
    })
    .sort((a, b) =>
      sort === "name"
        ? a.name.localeCompare(b.name)
        : b.lastAt.localeCompare(a.lastAt),
    );
  const liveCount = apps.filter((a) => a.live).length;

  return (
    <div>
      <h1 className="mb-5 text-[26px] font-semibold tracking-tight text-ink">Apps</h1>
      <div className="mb-5 flex flex-wrap items-center gap-3">
        <div className="relative">
          <span className="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-ink-faint">
            ⌕
          </span>
          <input
            id="apps-search"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            onKeyDown={(e) => e.key === "Escape" && (e.target as HTMLInputElement).blur()}
            placeholder="Search or filter"
            className="w-80 rounded-md border border-hair bg-transparent py-1.5 pl-8 pr-9 text-sm outline-none placeholder:text-ink-faint focus:border-hair-2"
          />
          <kbd className="pointer-events-none absolute right-2.5 top-1/2 -translate-y-1/2 rounded-xs bg-panel-2 px-1.5 text-[10px] text-ink-faint">
            /
          </kbd>
        </div>
        <FilterPill
          active={filter === "live"}
          onClick={() => setFilter(filter === "live" ? null : "live")}
          tone="good"
        >
          <span aria-hidden className="text-accent">☁</span> Live Apps{" "}
          <Count n={liveCount} tone="good" />
        </FilterPill>
        <FilterPill
          active={filter === "stopped"}
          onClick={() => setFilter(filter === "stopped" ? null : "stopped")}
          tone="dim"
        >
          <span aria-hidden>⊘</span> Stopped Apps{" "}
          <Count n={apps.length - liveCount} tone="dim" />
        </FilterPill>
        <label className="ml-auto text-sm text-ink-dim">
          Sort By:{" "}
          <select
            value={sort}
            onChange={(e) => setSort(e.target.value as "recent" | "name")}
            className="cursor-pointer appearance-none bg-transparent text-ink outline-none"
          >
            <option value="recent">Most recent</option>
            <option value="name">Name</option>
          </select>
        </label>
      </div>

      {shown.length === 0 ? (
        apps.length > 0 ? (
          <Empty>Nothing matches this filter.</Empty>
        ) : (
          <Empty>
            No apps here yet. Run something:{" "}
            <code className="font-mono text-ink">
              relay run examples/service_demo.py::hello
            </code>
          </Empty>
        )
      ) : (
        <div className="space-y-4">
          {shown.map((app) => (
            <Card key={app.name}>
              <div className="flex items-center justify-between px-5 py-3.5">
                <span className="text-[15px] font-semibold text-ink">{app.name}</span>
                <span className="flex items-center gap-2 text-xs text-ink-dim">
                  <span
                    className="flex size-5 items-center justify-center rounded-full bg-accent-dim text-[10px] font-bold text-accent"
                    aria-hidden
                  >
                    {app.name.slice(0, 1).toUpperCase()}
                  </span>
                  fleet
                  <span className="text-ink-faint">
                    {app.lastAt ? timeAgo(app.lastAt) : ""}
                  </span>
                </span>
              </div>
              <div className="pb-2">
                {app.fns.map((fn) => (
                  <button
                    key={fn.name}
                    onClick={() =>
                      openRun({ id: fn.lastRunId, title: `${app.name}.${fn.name}` })
                    }
                    className="grid w-full grid-cols-[minmax(0,1fr)_11rem_auto] items-center gap-6 px-5 py-2 text-left hover:bg-panel-2"
                  >
                    <span className="flex min-w-0 items-center gap-3">
                      <StatusDot state={fn.lastState} />
                      <span className="truncate text-sm">{fn.name}</span>
                      {fn.kind === "service" && fn.endpoint ? (
                        <span className="truncate font-mono text-xs text-ink-faint">
                          {fn.endpoint}
                        </span>
                      ) : null}
                    </span>
                    <span>
                      <Chip>{fn.kind === "service" ? "service" : fn.resources}</Chip>
                    </span>
                    <ActivityTicks states={fn.states} />
                  </button>
                ))}
              </div>
            </Card>
          ))}
        </div>
      )}
    </div>
  );
}

function Count({ n, tone }: { n: number; tone: "good" | "dim" }) {
  return (
    <span
      className="ml-0.5 rounded-full px-2 text-xs leading-5"
      style={{
        backgroundColor:
          tone === "good" ? "var(--color-accent-mid)" : "var(--color-panel-2)",
        color: tone === "good" ? "var(--color-ink)" : "var(--color-ink-body)",
      }}
    >
      {n}
    </span>
  );
}

function FilterPill({
  active,
  onClick,
  tone,
  children,
}: {
  active: boolean;
  onClick: () => void;
  tone: "good" | "dim";
  children: React.ReactNode;
}) {
  // Modal's pills: no border; active = a dark wash with WHITE text.
  return (
    <button
      onClick={onClick}
      className="inline-flex items-center gap-1.5 rounded-md px-2 py-1.5 text-sm transition-colors hover:text-ink"
      style={{
        color: active ? "var(--color-ink)" : "var(--color-ink-body)",
        backgroundColor: active
          ? tone === "good"
            ? "var(--color-accent-dim)"
            : "var(--color-panel-2)"
          : "transparent",
      }}
    >
      {children}
    </button>
  );
}

// ------------------------------------------------------------------ Runs

export function RunsView({
  snapshot,
  openRun,
}: {
  snapshot: Snapshot;
  openRun: (target: DrawerTarget) => void;
}) {
  const runs = snapshot.runs.filter((r) => r.kind !== "service");
  return (
    <div>
      <h1 className="mb-5 text-[26px] font-semibold tracking-tight text-ink">Runs</h1>
      {runs.length === 0 ? (
        <Empty>No runs yet.</Empty>
      ) : (
        <Card>
          <table className="w-full text-sm">
            <thead>
              <tr className="bg-panel-2 text-left text-sm text-ink-dim">
                <th className="px-5 py-3 font-medium">Function</th>
                <th className="px-3 py-3 font-medium">Resources</th>
                <th className="px-3 py-3 font-medium">Machine</th>
                <th className="px-3 py-3 font-medium">State</th>
                <th className="px-3 py-3 font-medium">Detail</th>
                <th className="px-5 py-3 text-right font-medium">Updated</th>
              </tr>
            </thead>
            <tbody>
              {runs.map((r) => (
                <tr
                  key={r.id}
                  onClick={() => openRun({ id: r.id, title: `${r.app}.${r.function}` })}
                  className="cursor-pointer border-b border-hair last:border-0 hover:bg-panel-2"
                >
                  <td className="px-5 py-2.5 font-mono text-[13px]">
                    {r.app}.{r.function}
                  </td>
                  <td className="px-3 py-2.5">
                    <Chip>{r.resources || "CPU"}</Chip>
                  </td>
                  <td className="px-3 py-2.5 text-ink-dim">{r.machine || "Not assigned"}</td>
                  <td className="px-3 py-2.5">
                    <StateText state={r.state} />
                  </td>
                  <td
                    className="max-w-72 truncate px-3 py-2.5 text-xs text-ink-dim"
                    title={r.detail}
                  >
                    {r.detail}
                  </td>
                  <td className="px-5 py-2.5 text-right text-xs text-ink-faint">
                    {timeAgo(r.updated_at)}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </Card>
      )}
    </div>
  );
}

// -------------------------------------------------------------- Services

export function ServicesView({
  snapshot,
  openRun,
}: {
  snapshot: Snapshot;
  openRun: (target: DrawerTarget) => void;
}) {
  return (
    <div>
      <h1 className="mb-5 text-[26px] font-semibold tracking-tight text-ink">Services</h1>
      {snapshot.services.length === 0 ? (
        <Empty>
          No services.{" "}
          <code className="font-mono text-ink">relay deploy app.py</code> brings
          one up.
        </Empty>
      ) : (
        <div className="grid gap-4 md:grid-cols-2">
          {snapshot.services.map((s) => (
            <Card key={s.id} className="px-5 py-4">
              <div className="mb-3 flex items-center justify-between">
                <span className="font-mono text-sm">
                  {s.app}.{s.function}
                </span>
                <StateText state={s.state} />
              </div>
              <dl className="space-y-1.5 text-sm">
                <Row k="Endpoint">
                  {s.endpoint ? (
                    <span className="font-mono text-[13px] text-accent">
                      {s.endpoint}
                    </span>
                  ) : (
                    <span className="text-ink-faint">starting…</span>
                  )}
                </Row>
                <Row k="Machine">{s.machine || "Not assigned"}</Row>
                <Row k="Updated">{timeAgo(s.updated_at)}</Row>
              </dl>
              <button
                onClick={() => openRun({ id: s.id, title: `${s.app}.${s.function}` })}
                className="mt-3 text-xs text-ink-dim underline-offset-2 hover:text-ink hover:underline"
              >
                details
              </button>
            </Card>
          ))}
        </div>
      )}
    </div>
  );
}

function Row({ k, children }: { k: string; children: React.ReactNode }) {
  return (
    <div className="flex gap-3">
      <dt className="w-20 shrink-0 text-ink-faint">{k}</dt>
      <dd className="min-w-0 truncate">{children}</dd>
    </div>
  );
}

// -------------------------------------------------------------- Machines

export function MachinesView({
  snapshot,
  openRun,
}: {
  snapshot: Snapshot;
  openRun: (target: DrawerTarget) => void;
}) {
  const all = [
    ...new Map(
      [...snapshot.services, ...snapshot.runs].map((run) => [run.id, run]),
    ).values(),
  ];
  const online = snapshot.machines.filter((machine) => machine.online);
  const totalCPUs = online.reduce((sum, machine) => sum + machine.cpu_cores, 0);
  const reservedCPUs = online.reduce(
    (sum, machine) => sum + machine.reserved.cpu_cores,
    0,
  );
  const totalMemory = online.reduce((sum, machine) => sum + machine.memory_mib, 0);
  const reservedMemory = online.reduce(
    (sum, machine) => sum + reservedMemoryMiB(machine),
    0,
  );
  const activeRuns = online.reduce(
    (sum, machine) => sum + machine.reserved.active_runs,
    0,
  );

  return (
    <div>
      <div className="mb-5 flex items-end justify-between gap-4">
        <div>
          <h1 className="text-[26px] font-semibold tracking-tight text-ink">
            Machines
          </h1>
          <p className="mt-1 text-sm text-ink-faint">
            Live OS telemetry and Relay capacity. Sampling runs only while this
            page is open.
          </p>
        </div>
        <span className="hidden text-xs text-ink-faint md:inline">
          refreshes every 3 seconds
        </span>
      </div>
      {snapshot.machines.length === 0 ? (
        <Empty>
          No machines joined.{" "}
          <code className="font-mono text-ink">relay connect</code> mints a join
          line.
        </Empty>
      ) : (
        <>
          <div className="mb-4 grid grid-cols-2 gap-3 md:grid-cols-4">
            <FleetStat
              label="Machines online"
              value={`${online.length} / ${snapshot.machines.length}`}
              detail={online.length === snapshot.machines.length ? "fleet ready" : "some unavailable"}
            />
            <FleetStat
              label="CPU schedulable"
              value={`${formatCores(Math.max(totalCPUs - reservedCPUs, 0))}`}
              detail={`${formatCores(totalCPUs)} total`}
            />
            <FleetStat
              label="Memory schedulable"
              value={formatMiB(Math.max(totalMemory - reservedMemory, 0))}
              detail={`${formatMiB(totalMemory)} total`}
            />
            <FleetStat
              label="Active allocations"
              value={`${activeRuns}`}
              detail={activeRuns === 1 ? "run or service" : "runs and services"}
            />
          </div>

          <div className="space-y-4">
            {snapshot.machines.map((m) => (
              <MachineCard
                key={m.id}
                machine={m}
                runs={all.filter((r) => r.machine === m.name)}
                openRun={openRun}
              />
            ))}
          </div>
        </>
      )}
    </div>
  );
}

function FleetStat({
  label,
  value,
  detail,
}: {
  label: string;
  value: string;
  detail: string;
}) {
  return (
    <Card className="px-4 py-3">
      <div className="text-xs text-ink-faint">{label}</div>
      <div className="mt-1 text-xl font-semibold tracking-tight text-ink">{value}</div>
      <div className="mt-1 text-xs text-ink-faint">{detail}</div>
    </Card>
  );
}

function formatMiB(mib: number): string {
  if (!Number.isFinite(mib) || mib <= 0) return "0 GB";
  const gib = mib / 1024;
  return `${gib >= 10 ? Math.round(gib) : Math.round(gib * 10) / 10} GB`;
}

function formatCores(cores: number): string {
  const rounded = Math.round(cores * 10) / 10;
  return `${rounded} ${rounded === 1 ? "core" : "cores"}`;
}

function reservedAcceleratorMiB(machine: Machine): number {
  return Object.values(machine.reserved.accelerator_memory_mib ?? {}).reduce(
    (sum, value) => sum + value,
    0,
  );
}

function reservedMemoryMiB(machine: Machine): number {
  return (
    machine.reserved.memory_mib +
    (machine.unified_memory ? reservedAcceleratorMiB(machine) : 0)
  );
}

function percentage(used: number, total: number): number {
  if (total <= 0) return 0;
  return Math.min(100, Math.max(0, (used / total) * 100));
}

function ResourceMeter({
  label,
  reserved,
  liveUsed = 0,
  liveAvailable = false,
  liveDetail,
  total,
  unit,
  unavailable = false,
}: {
  label: string;
  reserved: number;
  liveUsed?: number;
  liveAvailable?: boolean;
  liveDetail?: string;
  total: number;
  unit: "cores" | "memory";
  unavailable?: boolean;
}) {
  const free = Math.max(total - reserved, 0);
  const osFree = Math.max(total - liveUsed, 0);
  const reservedPercent = percentage(reserved, total);
  // Live OS use includes Relay processes. Render Relay's reservation first,
  // then only OS use beyond it, avoiding a visually double-counted bar.
  const additionalOSUsed = Math.max(liveUsed - reserved, 0);
  const additionalOSPercent = Math.min(
    100 - reservedPercent,
    percentage(additionalOSUsed, total),
  );
  const format = unit === "cores" ? formatCores : formatMiB;

  return (
    <div>
      <div className="mb-1.5 flex items-center justify-between gap-4 text-sm">
        <span className="text-ink-dim">
          {label}
          {liveDetail ? (
            <span className="ml-2 text-xs text-ink-faint">{liveDetail}</span>
          ) : null}
        </span>
        <span className="text-right font-mono text-xs text-ink">
          {unavailable
            ? "unavailable"
            : liveAvailable
              ? `${format(osFree)} OS free`
              : "collecting live sample…"}
        </span>
      </div>
      <div className="flex h-2 overflow-hidden rounded-full bg-panel-2">
        <div
          className="h-full bg-accent transition-[width]"
          style={{ width: unavailable ? "0%" : `${reservedPercent}%` }}
        />
        <div
          className="h-full bg-ink-faint transition-[width]"
          style={{
            width:
              unavailable || !liveAvailable ? "0%" : `${additionalOSPercent}%`,
          }}
        />
      </div>
      <div className="mt-1 flex flex-wrap justify-between gap-x-3 text-[10px] text-ink-faint">
        <span>
          {unavailable
            ? "machine offline"
            : liveAvailable
              ? `${format(liveUsed)} used by OS`
              : "collecting live sample…"}
        </span>
        <span className="text-accent">{format(reserved)} reserved by Relay</span>
        <span>
          {format(free)} schedulable · {format(total)} total
        </span>
      </div>
    </div>
  );
}

function acceleratorUsage(machine: Machine, index: number) {
  return machine.usage?.accelerators?.find((usage) => usage.index === index);
}

function SpecRow({
  label,
  children,
}: {
  label: string;
  children: React.ReactNode;
}) {
  return (
    <tr className="border-b border-hair last:border-0">
      <th className="w-32 py-2 pr-4 text-left text-xs font-medium text-ink-faint">
        {label}
      </th>
      <td className="py-2 text-sm text-ink-dim">{children}</td>
    </tr>
  );
}

function MachineCard({
  machine: m,
  runs,
  openRun,
}: {
  machine: Machine;
  runs: Run[];
  openRun: (target: DrawerTarget) => void;
}) {
  const active = runs.filter((r) => ACTIVE.has(r.state));
  const succeeded = runs.filter((r) => r.state === "succeeded").length;
  const failed = runs.filter((r) =>
    ["failed", "error", "lost"].includes(r.state),
  ).length;
  const lastRun = runs.reduce<string>(
    (max, r) => (r.updated_at > max ? r.updated_at : max),
    "",
  );
  const sharedMemoryReserved = reservedMemoryMiB(m);
  const diskUsed = m.usage?.disk_usage_available
    ? Math.max(m.usage.disk_total_mib - m.usage.disk_free_mib, 0)
    : 0;

  return (
    <Card className="overflow-hidden">
      <div className="flex flex-wrap items-center justify-between gap-3 border-b border-hair px-5 py-4">
        <span className="flex items-center gap-2.5">
          <StatusDot state={m.online ? "running" : "lost"} />
          <span>
            <span className="block font-semibold text-ink">{m.name}</span>
            <span className="mt-0.5 block font-mono text-[10px] text-ink-faint">
              {m.id}
            </span>
          </span>
        </span>
        <span className="flex items-center gap-2 text-xs text-ink-faint">
          <Chip>{m.online ? "available" : "offline"}</Chip>
          <span>{m.online ? `seen ${timeAgo(m.last_seen)}` : `last seen ${timeAgo(m.last_seen)}`}</span>
        </span>
      </div>

      <div className="grid lg:grid-cols-[minmax(0,1.15fr)_minmax(20rem,0.85fr)]">
        <section className="border-b border-hair px-5 py-4 lg:border-b-0 lg:border-r">
          <div className="mb-4 flex items-start justify-between gap-4">
            <div>
              <h2 className="text-sm font-semibold text-ink">
                Live utilization &amp; capacity
              </h2>
              <p className="mt-1 text-xs text-ink-faint">
                OS use is live; Relay reservations determine schedulable capacity.
              </p>
            </div>
            <span className="shrink-0 font-mono text-xs text-ink-dim">
              {m.reserved.active_runs} active
            </span>
          </div>

          <div className="mb-4 flex flex-wrap gap-4 text-[10px] text-ink-faint">
            <span className="flex items-center gap-1.5">
              <span className="size-2 rounded-full bg-ink-faint" /> OS used
            </span>
            <span className="flex items-center gap-1.5">
              <span className="size-2 rounded-full bg-accent" /> Relay reserved
            </span>
            <span className="flex items-center gap-1.5">
              <span className="size-2 rounded-full bg-panel-2" /> free
            </span>
          </div>

          <div className="space-y-4">
            <ResourceMeter
              label="CPU"
              reserved={m.reserved.cpu_cores}
              liveUsed={m.usage?.cpu_used_cores}
              liveAvailable={m.usage?.cpu_usage_available}
              total={m.cpu_cores}
              unit="cores"
              unavailable={!m.online}
            />
            <ResourceMeter
              label={m.unified_memory ? "Unified memory" : "System memory"}
              reserved={sharedMemoryReserved}
              liveUsed={m.usage?.memory_used_mib}
              liveAvailable={m.usage?.memory_usage_available}
              total={m.memory_mib}
              unit="memory"
              unavailable={!m.online}
            />
            {!m.unified_memory &&
              (m.accelerators ?? []).map((accelerator, position) => {
                const index = accelerator.index ?? position;
                const reserved =
                  m.reserved.accelerator_memory_mib?.[String(index)] ?? 0;
                const live = acceleratorUsage(m, index);
                return (
                  <ResourceMeter
                    key={`${accelerator.kind ?? "accelerator"}-${index}`}
                    label={`${(accelerator.kind ?? "accelerator").toUpperCase()} ${index} memory`}
                    reserved={reserved}
                    liveUsed={live?.memory_used_mib}
                    liveAvailable={live?.memory_usage_available}
                    liveDetail={
                      live?.utilization_available
                        ? `${Math.round(live.utilization * 100)}% compute`
                        : undefined
                    }
                    total={accelerator.memory_mib ?? 0}
                    unit="memory"
                    unavailable={!m.online}
                  />
                );
              })}
          </div>
        </section>

        <section className="px-5 py-4">
          <h2 className="mb-2 text-sm font-semibold text-ink">Specifications</h2>
          <table className="w-full">
            <tbody>
              <SpecRow label="Platform">
                <span className="capitalize">{m.os}</span> · {m.arch}
              </SpecRow>
              <SpecRow label="CPU">{formatCores(m.cpu_cores)}</SpecRow>
              <SpecRow label="Memory">
                {formatMiB(m.memory_mib)}{m.unified_memory ? " · unified" : ""}
              </SpecRow>
              <SpecRow label="Storage">
                {m.usage?.disk_usage_available
                  ? `${formatMiB(m.usage.disk_free_mib)} free of ${formatMiB(m.usage.disk_total_mib)}`
                  : m.online
                    ? "Collecting live sample…"
                    : "Unavailable while offline"}
                {diskUsed > 0 ? (
                  <span className="ml-1.5 text-xs text-ink-faint">
                    · {formatMiB(diskUsed)} used
                  </span>
                ) : null}
              </SpecRow>
              <SpecRow label="Accelerator">
                {(m.accelerators ?? []).length === 0
                  ? "None detected"
                  : m.accelerators.map((accelerator, index) => (
                      <div key={`${accelerator.kind ?? "accelerator"}-${index}`}>
                        <span className="text-ink">{accelerator.name || "Unknown"}</span>
                        <span className="ml-1.5 text-xs text-ink-faint">
                          {(accelerator.kind ?? "?").toUpperCase()}
                          {m.unified_memory
                            ? " · shared memory"
                            : accelerator.memory_mib
                              ? ` · ${formatMiB(accelerator.memory_mib)}`
                              : ""}
                        </span>
                      </div>
                    ))}
              </SpecRow>
              <SpecRow label="Executors">
                <div className="flex flex-wrap gap-1.5">
                  {(m.executors ?? []).map((executor) => (
                    <Chip key={executor}>{executor}</Chip>
                  ))}
                </div>
              </SpecRow>
            </tbody>
          </table>
        </section>
      </div>

      <div className="border-t border-hair px-5 py-3">
        <div className="grid grid-cols-3 gap-4 text-sm">
          <span>
            <span className="text-ink">{active.length}</span>{" "}
            <span className="text-ink-faint">active</span>
          </span>
          <span>
            <span className="text-ink">{succeeded}</span>{" "}
            <span className="text-ink-faint">succeeded</span>
          </span>
          <span>
            <span className="text-ink">{failed}</span>{" "}
            <span className="text-ink-faint">failed</span>
          </span>
        </div>

        {active.length > 0 && (
          <div className="mt-3 space-y-1">
          {active.slice(0, 4).map((r) => (
            <button
              key={r.id}
              onClick={() => openRun({ id: r.id, title: `${r.app}.${r.function}` })}
              className="flex w-full items-center gap-2.5 rounded-sm px-2 py-1 text-left text-sm hover:bg-panel-2"
            >
              <StatusDot state={r.state} />
              <span className="truncate font-mono text-xs">
                {r.app}.{r.function}
              </span>
              <Chip>{r.kind === "service" ? "service" : r.resources || "CPU"}</Chip>
              <span className="ml-auto shrink-0 text-xs text-ink-faint">
                {timeAgo(r.updated_at)}
              </span>
            </button>
          ))}
          {active.length > 4 && (
            <div className="px-2 text-xs text-ink-faint">
              +{active.length - 4} more
            </div>
          )}
          </div>
        )}

        {active.length === 0 && (
          <div className="mt-3 px-0.5 text-xs text-ink-faint">
          {runs.length === 0
            ? "no recent runs"
            : `idle · last run ${timeAgo(lastRun)}`}
          </div>
        )}
      </div>
    </Card>
  );
}

// ------------------------------------------------- Secrets & Schedules

export function SecretsView({ snapshot }: { snapshot: Snapshot }) {
  return (
    <div>
      <h1 className="mb-5 text-[26px] font-semibold tracking-tight text-ink">Secrets</h1>
      {snapshot.secrets.length === 0 ? (
        <Empty>
          No secrets.{" "}
          <code className="font-mono text-ink">relay secret set HF_TOKEN</code>
        </Empty>
      ) : (
        <Card>
          {snapshot.secrets.map((name) => (
            <div
              key={name}
              className="flex items-center justify-between border-b border-hair px-5 py-3 last:border-0"
            >
              <span className="font-mono text-sm">{name}</span>
              <span className="text-xs text-ink-faint">
                value write-only · injected as env var
              </span>
            </div>
          ))}
        </Card>
      )}
    </div>
  );
}

export function SchedulesView({ snapshot }: { snapshot: Snapshot }) {
  const schedules: Schedule[] = snapshot.schedules;
  return (
    <div>
      <h1 className="mb-5 text-[26px] font-semibold tracking-tight text-ink">Schedules</h1>
      {schedules.length === 0 ? (
        <Empty>
          No schedules. Add{" "}
          <code className="font-mono text-ink">schedule="0 3 * * *"</code> to a
          function and deploy.
        </Empty>
      ) : (
        <Card>
          {schedules.map((s) => (
            <div
              key={`${s.app}.${s.function}`}
              className="grid grid-cols-[1fr_auto_auto] items-center gap-6 border-b border-hair px-5 py-3 last:border-0"
            >
              <span className="font-mono text-sm">
                {s.app}.{s.function}
              </span>
              <Chip>{s.cron}</Chip>
              <span className="text-xs text-ink-faint">
                {s.last_run ? `last ${timeAgo(s.last_run)}` : "never fired"}
              </span>
            </div>
          ))}
        </Card>
      )}
    </div>
  );
}

// ------------------------------------------------------------ Run drawer
// Modeled on Modal's function-call drawer: ID header with copy, stat boxes,
// Logs / Execution / Details tabs.

const STATE_LABELS: Record<string, string> = {
  pending: "Queued",
  assigned: "Assigned",
  building: "Preparing",
  running: "Execution started",
  succeeded: "Execution finished",
  failed: "Function raised",
  error: "Infrastructure error",
  canceled: "Canceled",
  lost: "Lost",
};

function fmtDuration(ms: number): string {
  if (ms < 1000) return `${Math.max(0, Math.round(ms))}ms`;
  if (ms < 60_000) return `${(ms / 1000).toFixed(2)}s`;
  const m = Math.floor(ms / 60_000);
  return `${m}m ${Math.round((ms % 60_000) / 1000)}s`;
}

function fmtClock(tsMs: number): string {
  return new Date(tsMs).toLocaleTimeString(undefined, { hour12: false });
}

function fmtStamp(tsMs: number): string {
  const d = new Date(tsMs);
  return `${d.toLocaleDateString(undefined, { month: "short", day: "numeric" })}, ${d.toLocaleTimeString(undefined, { hour12: false })}.${String(d.getMilliseconds()).padStart(3, "0")}`;
}

function executionTime(run: Run, now: number): string {
  const events = run.events ?? [];
  const started = events.find((e) => e.state === "running");
  if (!started) return "Not started";
  const terminal = events.filter((e) => !ACTIVE.has(e.state)).at(-1);
  const end = terminal ? terminal.ts : now;
  return fmtDuration(end - started.ts);
}

function CopyButton({ value }: { value: string }) {
  const [copied, setCopied] = useState(false);
  return (
    <button
      onClick={() => {
        navigator.clipboard?.writeText(value);
        setCopied(true);
        setTimeout(() => setCopied(false), 1200);
      }}
      className="rounded-xs border border-hair px-1.5 py-0.5 text-[10px] text-ink-faint hover:text-ink"
      title="copy"
    >
      {copied ? "copied" : "⧉"}
    </button>
  );
}

function StatBox({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="rounded-md bg-panel px-4 py-3">
      <div className="mb-1.5 text-xs text-ink-dim">{label}</div>
      <div className="text-sm text-ink">{children}</div>
    </div>
  );
}

function Timeline({ events, terminal, now }: { events: RunEvent[]; terminal: boolean; now: number }) {
  if (events.length < 2 && !(events.length === 1 && !terminal)) {
    return <div className="text-sm text-ink-faint">not enough data yet</div>;
  }
  const points = [...events];
  const segments: { label: string; ms: number; color: string }[] = [];
  for (let i = 0; i < points.length - 1; i++) {
    segments.push({
      label: STATE_LABELS[points[i].state] ?? points[i].state,
      ms: points[i + 1].ts - points[i].ts,
      color: stateColor(points[i].state),
    });
  }
  if (!terminal) {
    const last = points[points.length - 1];
    segments.push({
      label: STATE_LABELS[last.state] ?? last.state,
      ms: now - last.ts,
      color: stateColor(last.state),
    });
  }
  const total = Math.max(1, segments.reduce((s, x) => s + x.ms, 0));
  const last = points[points.length - 1];
  return (
    <div className="rounded-md bg-panel px-4 py-4">
      <div className="mb-2 flex justify-between text-xs text-ink-dim">
        <span>{STATE_LABELS[points[0].state] ?? points[0].state}</span>
        {terminal && (
          <span style={{ color: stateColor(last.state) }}>
            {STATE_LABELS[last.state] ?? last.state}
          </span>
        )}
      </div>
      <div className="flex h-4 items-stretch gap-1">
        {segments.map((seg, i) => (
          <div
            key={i}
            className="flex min-w-14 items-center justify-center rounded-xs text-[10px] text-ink"
            style={{
              flexGrow: Math.max(seg.ms / total, 0.08),
              backgroundColor: `color-mix(in srgb, ${seg.color} 22%, var(--color-panel-2))`,
              border: `1px solid color-mix(in srgb, ${seg.color} 35%, transparent)`,
            }}
            title={seg.label}
          >
            {fmtDuration(seg.ms)}
          </div>
        ))}
      </div>
      <div className="mt-2 flex justify-between font-mono text-xs text-ink-faint">
        <span>{fmtClock(points[0].ts)}</span>
        <span>{terminal ? fmtClock(last.ts) : "now"}</span>
      </div>
    </div>
  );
}

export function RunDrawer({
  target,
  initial,
  visible,
  onClose,
}: {
  target: DrawerTarget;
  initial?: Run;
  visible: boolean;
  onClose: () => void;
}) {
  const [run, setRun] = useState<Run | null>(initial ?? null);
  const [tab, setTab] = useState<"Logs" | "Execution" | "Details">("Logs");
  const [text, setText] = useState("");
  const [done, setDone] = useState(false);
  const [now, setNow] = useState(() => Date.now());
  const preRef = useRef<HTMLPreElement>(null);

  useEffect(() => {
    if (!visible) return;
    let alive = true;
    const load = () =>
      fetchRun(target.id)
        .then((r) => alive && setRun(r))
        .catch(() => {});
    load();
    const id = setInterval(() => {
      load();
      setNow(Date.now());
    }, 2000);
    return () => {
      alive = false;
      clearInterval(id);
    };
  }, [target.id, visible]);

  useEffect(() => {
    if (!visible) return;
    setText("");
    setDone(false);
    const abort = streamLogs(
      target.id,
      (chunk) => setText((t) => t + chunk),
      () => setDone(true),
    );
    return abort;
  }, [target.id, visible]);

  useEffect(() => {
    const el = preRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [text, tab]);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => e.key === "Escape" && onClose();
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose]);

  const terminal = run ? !ACTIVE.has(run.state) : false;
  const events = run?.events ?? [];

  return (
    <div className="fixed inset-0 z-40">
      <div className="absolute inset-0 bg-black/60" onClick={onClose} />
      <div className="absolute inset-y-0 right-0 flex w-full max-w-3xl flex-col border-l border-hair bg-canvas shadow-2xl">
        {/* Header */}
        <div className="px-6 pb-4 pt-5">
          <div className="mb-1 flex items-center justify-between">
            <span className="text-sm text-ink-dim">
              {run?.kind === "service" ? "Service" : "Run"}
            </span>
            <button
              onClick={onClose}
              className="rounded-xs border border-hair px-2 py-0.5 text-xs text-ink-dim hover:text-ink"
              title="close (esc)"
            >
              ⇥
            </button>
          </div>
          <div className="flex items-center gap-2.5">
            <h2 className="truncate font-mono text-xl font-semibold tracking-tight text-ink">
              {target.id}
            </h2>
            <CopyButton value={target.id} />
          </div>
          <div className="mt-1 flex items-center gap-2 text-sm text-ink-faint">
            <span className="font-mono">{target.title}</span>
            {run?.machine ? <span>· {run.machine}</span> : null}
          </div>
        </div>

        {/* Stat boxes */}
        <div className="grid grid-cols-2 gap-3 px-6 pb-5">
          <StatBox label="Status">
            {run ? <StateText state={run.state} /> : "…"}
            {run?.detail && ACTIVE.has(run.state) ? (
              <div className="mt-1 truncate text-xs text-ink-faint" title={run.detail}>
                {run.detail}
              </div>
            ) : null}
          </StatBox>
          <StatBox label="Execution time">
            {run ? executionTime(run, now) : "…"}
          </StatBox>
        </div>

        {/* Tabs */}
        <div className="border-b border-hair px-6">
          <div className="flex gap-6">
            {(["Logs", "Execution", "Details"] as const).map((t) => (
              <button
                key={t}
                onClick={() => setTab(t)}
                className={`border-b-2 pb-2 text-sm ${
                  tab === t
                    ? "border-accent font-medium text-ink"
                    : "border-transparent text-ink-dim hover:text-ink"
                }`}
              >
                {t}
              </button>
            ))}
          </div>
        </div>

        {/* Body */}
        {tab === "Logs" && (
          <div className="flex min-h-0 flex-1 flex-col">
            <div className="flex items-center gap-3 px-6 pb-2 pt-4">
              <span className="text-lg font-semibold">Logs</span>
              <span className="rounded-xs border border-hair px-2 py-0.5 text-xs text-ink-dim">
                {done ? "stream ended" : "live"}
              </span>
            </div>
            <pre
              ref={preRef}
              className="min-h-0 flex-1 overflow-auto px-6 py-2 font-mono text-xs leading-5 text-ink-dim"
            >
              {text ||
                (done ? "(this run produced no log output)" : "waiting for logs…")}
            </pre>
          </div>
        )}

        {tab === "Execution" && (
          <div className="min-h-0 flex-1 space-y-5 overflow-auto px-6 py-4">
            {run?.image ? (
              <div className="flex items-center gap-2 text-sm text-ink-dim">
                Image: <span className="font-mono text-ink">{run.image}</span>
                <CopyButton value={run.image} />
                {run.resources ? <Chip>{run.resources}</Chip> : null}
              </div>
            ) : null}
            <Timeline events={events} terminal={terminal} now={now} />
            <div className="overflow-hidden rounded-md bg-panel">
              <table className="w-full text-sm">
                <thead>
                  <tr className="bg-panel-2 text-left text-xs text-ink-dim">
                    <th className="px-4 py-2.5 font-medium">Event</th>
                    <th className="px-4 py-2.5 font-medium">Timestamp</th>
                  </tr>
                </thead>
                <tbody>
                  {events.map((e, i) => (
                    <tr key={i} className="border-t border-hair">
                      <td className="px-4 py-2.5">
                        {STATE_LABELS[e.state] ?? e.state}
                      </td>
                      <td className="px-4 py-2.5 font-mono text-xs text-ink-dim">
                        {fmtStamp(e.ts)}
                      </td>
                    </tr>
                  ))}
                  {events.length === 0 && (
                    <tr>
                      <td className="px-4 py-3 text-ink-faint" colSpan={2}>
                        no events recorded
                      </td>
                    </tr>
                  )}
                </tbody>
              </table>
            </div>
          </div>
        )}

        {tab === "Details" && run && (
          <div className="min-h-0 flex-1 overflow-auto px-6 py-4">
            <dl className="space-y-2.5 text-sm">
              <DetailRow k="Function">
                <span className="font-mono">{run.app}.{run.function}</span>
              </DetailRow>
              <DetailRow k="Kind">{run.kind}</DetailRow>
              <DetailRow k="Machine">{run.machine || "Not assigned"}</DetailRow>
              <DetailRow k="Resources">{run.resources || "CPU"}</DetailRow>
              {run.image ? (
                <DetailRow k="Image">
                  <span className="font-mono text-xs">{run.image}</span>
                </DetailRow>
              ) : null}
              {run.endpoint ? (
                <DetailRow k="Endpoint">
                  <span className="font-mono text-accent">{run.endpoint}</span>
                </DetailRow>
              ) : null}
              {terminal ? (
                <DetailRow k="Exit code">{run.exit_code}</DetailRow>
              ) : null}
              {run.detail ? <DetailRow k="Detail">{run.detail}</DetailRow> : null}
              <DetailRow k="Created">
                {new Date(run.created_at).toLocaleString()}
              </DetailRow>
              <DetailRow k="Updated">
                {new Date(run.updated_at).toLocaleString()}
              </DetailRow>
            </dl>
          </div>
        )}
      </div>
    </div>
  );
}

function DetailRow({ k, children }: { k: string; children: React.ReactNode }) {
  return (
    <div className="flex gap-4 border-b border-hair pb-2.5 last:border-0">
      <dt className="w-28 shrink-0 text-ink-faint">{k}</dt>
      <dd className="min-w-0 break-words">{children}</dd>
    </div>
  );
}
