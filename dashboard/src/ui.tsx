// Small presentational primitives shared by every view.

import type { ReactNode } from "react";

export function stateColor(state: string): string {
  switch (state) {
    case "running":
    case "succeeded":
      return "var(--color-good)";
    case "pending":
    case "assigned":
    case "building":
      return "var(--color-warn)";
    case "failed":
    case "error":
    case "lost":
      return "var(--color-bad)";
    default:
      return "var(--color-ink-faint)";
  }
}

export function StatusDot({ state }: { state: string }) {
  const active = ["pending", "assigned", "building", "running"].includes(state);
  return (
    <span
      className={`inline-block size-2 rounded-full shrink-0 ${active ? "animate-pulse" : ""}`}
      style={{
        backgroundColor:
          state === "running" || state === "succeeded"
            ? stateColor(state)
            : "transparent",
        border: `1.5px solid ${stateColor(state)}`,
      }}
      title={state}
    />
  );
}

export function Chip({ children }: { children: ReactNode }) {
  return (
    <span className="inline-flex items-center rounded-xs bg-panel-2 px-2 py-0.5 text-xs font-medium text-ink-dim">
      {children}
    </span>
  );
}

export function stateIcon(state: string): string {
  switch (state) {
    case "succeeded":
      return "✓";
    case "running":
      return "●";
    case "failed":
    case "error":
      return "✕";
    case "lost":
      return "?";
    case "canceled":
      return "⊘";
    default:
      return "◌";
  }
}

/** Modal-style state: icon + colored text, no capsule. */
export function StateText({ state }: { state: string }) {
  return (
    <span
      className="inline-flex items-center gap-1.5 text-sm"
      style={{ color: stateColor(state) }}
    >
      <span aria-hidden className="text-xs">
        {stateIcon(state)}
      </span>
      {state.charAt(0).toUpperCase() + state.slice(1)}
    </span>
  );
}

export function Card({
  children,
  className = "",
}: {
  children: ReactNode;
  className?: string;
}) {
  // Modal-style elevation uses background contrast without a border.
  return (
    <div className={`rounded-md bg-panel ${className}`}>{children}</div>
  );
}

export function Empty({ children }: { children: ReactNode }) {
  return (
    <Card className="px-6 py-10 text-center text-sm text-ink-dim">
      {children}
    </Card>
  );
}

/** Recent-activity wall (Modal-style): a fixed-width strip of ticks, one
 * per recent run, with the newest on the right. Ticks are dim gray unless notable. */
export function ActivityTicks({ states }: { states: string[] }) {
  const N = 30;
  const ticks = states.slice(0, N).reverse();
  const pad = Math.max(0, N - ticks.length);
  return (
    <div className="flex h-4 w-56 items-end justify-end gap-[3px]" aria-hidden>
      {Array.from({ length: pad }).map((_, i) => (
        <span key={`p${i}`} className="h-3 w-1 rounded-[1px] bg-hair" />
      ))}
      {ticks.map((s, i) => {
        const notable = !["succeeded", "canceled"].includes(s);
        return (
          <span
            key={i}
            className="w-1 rounded-[1px]"
            style={{
              height: 12,
              backgroundColor: notable
                ? `color-mix(in srgb, ${stateColor(s)} 80%, transparent)`
                : "var(--color-hair-2)",
            }}
            title={s}
          />
        );
      })}
    </div>
  );
}
