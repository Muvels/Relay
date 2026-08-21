import { useCallback, useEffect, useState } from "react";
import type { Snapshot } from "./api";
import { clearToken, fetchSnapshot, getToken, setToken, Unauthorized } from "./api";
import {
  AppsView,
  MachinesView,
  RunDrawer,
  RunsView,
  SchedulesView,
  SecretsView,
  ServicesView,
  type DrawerTarget,
} from "./views";

const TABS = [
  "Apps",
  "Runs",
  "Services",
  "Machines",
  "Secrets",
  "Schedules",
] as const;
type Tab = (typeof TABS)[number];

export default function App() {
  const [authed, setAuthed] = useState(() => Boolean(getToken()));
  const [snapshot, setSnapshot] = useState<Snapshot | null>(null);
  const [offline, setOffline] = useState(false);
  const [tab, setTab] = useState<Tab>("Apps");
  const [drawer, setDrawer] = useState<DrawerTarget | null>(null);

  const refresh = useCallback(async () => {
    try {
      setSnapshot(await fetchSnapshot());
      setOffline(false);
    } catch (err) {
      if (err instanceof Unauthorized) {
        clearToken();
        setAuthed(false);
      } else {
        setOffline(true);
      }
    }
  }, []);

  useEffect(() => {
    if (!authed) return;
    refresh();
    const id = setInterval(refresh, 3000);
    return () => clearInterval(id);
  }, [authed, refresh]);

  // "/" focuses the Apps search, Modal-style.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== "/" || e.metaKey || e.ctrlKey) return;
      const target = e.target as HTMLElement;
      if (["INPUT", "TEXTAREA"].includes(target.tagName)) return;
      setTab("Apps");
      requestAnimationFrame(() =>
        document.getElementById("apps-search")?.focus(),
      );
      e.preventDefault();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  if (!authed) {
    return (
      <TokenGate
        onSubmit={(token) => {
          setToken(token);
          setAuthed(true);
        }}
      />
    );
  }

  return (
    <div className="flex min-h-full flex-col">
      <header className="border-b border-hair">
        <nav className="mx-auto flex max-w-6xl items-center gap-6 px-6 pt-3">
          {TABS.map((t) => (
            <button
              key={t}
              onClick={() => setTab(t)}
              className={`border-b-2 pb-2.5 text-sm transition-colors ${
                tab === t
                  ? "border-accent font-medium text-ink"
                  : "border-transparent text-ink-dim hover:text-ink"
              }`}
            >
              {t}
            </button>
          ))}
          <span className="ml-auto flex items-center gap-3 pb-2.5">
            {offline && (
              <span className="text-xs text-bad">server unreachable</span>
            )}
            <button
              onClick={() => {
                clearToken();
                setAuthed(false);
              }}
              className="text-xs text-ink-faint transition-colors hover:text-ink"
            >
              token
            </button>
          </span>
        </nav>
      </header>

      <main className="mx-auto w-full max-w-6xl flex-1 px-6 py-8">
        {!snapshot ? (
          <div className="py-24 text-center text-sm text-ink-faint">
            connecting…
          </div>
        ) : tab === "Apps" ? (
          <AppsView snapshot={snapshot} openRun={setDrawer} />
        ) : tab === "Runs" ? (
          <RunsView snapshot={snapshot} openRun={setDrawer} />
        ) : tab === "Services" ? (
          <ServicesView snapshot={snapshot} openRun={setDrawer} />
        ) : tab === "Machines" ? (
          <MachinesView snapshot={snapshot} openRun={setDrawer} />
        ) : tab === "Secrets" ? (
          <SecretsView snapshot={snapshot} />
        ) : (
          <SchedulesView snapshot={snapshot} />
        )}
      </main>

      {drawer && (
        <RunDrawer
          target={drawer}
          initial={[...(snapshot?.runs ?? []), ...(snapshot?.services ?? [])].find(
            (r) => r.id === drawer.id,
          )}
          onClose={() => setDrawer(null)}
        />
      )}
    </div>
  );
}

function TokenGate({ onSubmit }: { onSubmit: (token: string) => void }) {
  const [value, setValue] = useState("");
  return (
    <div className="flex min-h-full items-center justify-center px-6">
      <div className="w-full max-w-sm rounded-md border border-hair bg-panel p-6">
        <div className="mb-1">
          <span className="text-lg font-bold tracking-tight text-accent">relay</span>
        </div>
        <p className="mb-5 text-sm text-ink-dim">
          Paste the API token printed by{" "}
          <code className="font-mono text-ink">relayd server</code> (also in
          its data dir as <code className="font-mono">api_token</code>).
        </p>
        <form
          onSubmit={(e) => {
            e.preventDefault();
            if (value.trim()) onSubmit(value.trim());
          }}
        >
          <input
            autoFocus
            type="password"
            value={value}
            onChange={(e) => setValue(e.target.value)}
            placeholder="rly_…"
            className="mb-3 w-full rounded-sm border border-hair bg-canvas px-3 py-2 font-mono text-sm outline-none placeholder:text-ink-faint focus:border-hair-2"
          />
          <button
            type="submit"
            className="w-full rounded-sm bg-accent py-2 text-sm font-semibold text-black transition-opacity hover:opacity-90"
          >
            Connect
          </button>
        </form>
      </div>
    </div>
  );
}
