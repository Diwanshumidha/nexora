import { useCallback, useEffect, useState } from "react";
import { Extensions, Permissions, Runtime, RuntimeState, Setup, Updates, openExternal, type RuntimeStatus, type UpdateInfo } from "./lib/api";
import { useLive } from "./lib/useLive";
import OrbitPage from "./pages/Orbit";
import PlaygroundPage from "./pages/Playground";
import HistoryPage from "./pages/History";
import SetupPage from "./pages/Setup";
import { Icon, Logo, type IconName } from "./orbit/icons";
import type { Selection } from "./orbit/OrbitMap";

type Page = "orbit" | "history" | "code";

/** The update the user said "Later" to; a newer one shows again. */
const DISMISSED_UPDATE_KEY = "nexora.dismissed-update";

const pages: { id: Page; label: string; icon: IconName }[] = [
  { id: "orbit", label: "Orbit", icon: "orbit" },
  { id: "history", label: "History", icon: "history" },
  { id: "code", label: "Code mode", icon: "terminal" },
];

export default function App() {
  const [page, setPage] = useState<Page>("orbit");
  const [selected, setSelected] = useState<Selection>({ kind: "core" });
  const [runId, setRunId] = useState<string | null>(null);
  const [draft, setDraft] = useState<string | null>(null);
  const takeDraft = useCallback(() => setDraft(null), []);
  // Approvals are polled app-wide: a script may pause for one while you're
  // on any page.
  const [approvals, refreshApprovals] = useLive(async () => (await Permissions.Approvals()) ?? [], 1000, []);

  const review = (id: string) => {
    setPage("orbit");
    setSelected({ kind: "approval", id });
  };
  const first = approvals[0];

  // First-run setup, also shown at launch whenever Node.js is unusable.
  const [setup, setSetup] = useState<"checking" | "show" | "hidden">("checking");
  const [runtime] = useLive(() => Runtime.Status(), 3000, null as RuntimeStatus | null);
  useEffect(() => {
    if (location.hash.startsWith("#perf")) {
      setSetup("hidden");
      return;
    }
    Promise.all([Setup.Done(), Runtime.Status()])
      .then(([done, rt]) => setSetup(!done || rt.state !== RuntimeState.StateReady ? "show" : "hidden"))
      .catch(() => setSetup("hidden"));
  }, []);
  const [update] = useLive(() => Updates.Info(), 60_000, null as UpdateInfo | null);
  const [dismissed, setDismissed] = useState<string | null>(() => {
    try { return localStorage.getItem(DISMISSED_UPDATE_KEY); } catch { return null; }
  });
  const dismissUpdate = (v: string) => {
    setDismissed(v);
    try { localStorage.setItem(DISMISSED_UPDATE_KEY, v); } catch { /* hidden for this session only */ }
  };
  const closeSetup = async () => {
    await Setup.Complete().catch(console.error);
    setSetup("hidden");
  };

  // NEXORA_PERF=1: scroll benchmark in the real webview (dev only).
  useEffect(() => {
    if (location.hash.startsWith("#perf")) {
      import("./lib/perfProbe").then((m) => m.runPerfProbe(setPage)).catch(console.error);
    }
  }, []);

  const openRun = (id: string) => {
    setRunId(id);
    setPage("history");
  };
  const openExtension = async (namespace: string) => {
    const ext = ((await Extensions.List()) ?? []).find((e) => (e.manifest?.namespace ?? e.manifest?.id) === namespace);
    if (!ext?.manifest) return;
    setSelected({ kind: "extension", id: ext.manifest.id });
    setPage("orbit");
  };
  const editScript = (code: string) => {
    setDraft(code);
    setPage("code");
  };

  if (setup === "checking") return <div className="shell" />;
  if (setup === "show") return <SetupPage onFinish={closeSetup} onSkip={closeSetup} />;

  return (
    <div className="shell">
      <nav className="rail" aria-label="Primary">
        <div className="rail-logo"><Logo size={36} /></div>
        {pages.map((p) => (
          <button key={p.id} className={`rail-item ${page === p.id ? "active" : ""}`} aria-label={p.label}
            aria-current={page === p.id ? "page" : undefined} title={p.label} onClick={() => setPage(p.id)}>
            <Icon name={p.icon} size={22} />
          </button>
        ))}
      </nav>
      <div className="page">
        {update?.available && update.latest !== dismissed && (
          <div className="approval-banner update-banner" role="status">
            <span className="banner-dot" aria-hidden="true" />
            <span>
              <strong>Nexora {update.latest} is available.</strong>{" "}
              <span className="muted">You have {update.current}.</span>
            </span>
            <button className="btn ghost" onClick={() => dismissUpdate(update.latest!)}>Later</button>
            <button className="btn primary" onClick={() => update.url && openExternal(update.url)}>Download</button>
          </div>
        )}
        {runtime && runtime.state !== RuntimeState.StateReady && (
          <div className="approval-banner runtime-banner" role="status">
            <span className="banner-dot" aria-hidden="true" />
            <span>
              <strong>Extensions are paused:</strong>{" "}
              {runtime.state === RuntimeState.StateOutdated ? `Node.js ${runtime.found} is too old.` : "they need Node.js."}
            </span>
            <button className="btn primary" onClick={() => setSetup("show")}>Finish setup</button>
          </div>
        )}
        {first && !(page === "orbit" && selected.kind === "approval") && (
          <div className="approval-banner" role="alert">
            <span className="banner-dot" aria-hidden="true" />
            <span>
              <strong>{first.source === "mcp" ? "An AI" : "Your script"}</strong> wants to run{" "}
              <code>{first.namespace}.{first.tool}()</code>
              {approvals.length > 1 && <span className="muted"> · {approvals.length - 1} more waiting</span>}
            </span>
            <button className="btn primary" onClick={() => review(first.id)}>Review</button>
          </div>
        )}
        {page === "orbit" && (
          <OrbitPage selected={selected} onSelect={setSelected} approvals={approvals} refreshApprovals={refreshApprovals}
            onOpenRun={openRun} onOpenHistory={() => { setRunId(null); setPage("history"); }} />
        )}
        {page === "history" && (
          <HistoryPage selectedId={runId} onSelect={setRunId} approvals={approvals} onReview={review}
            onOpenExtension={openExtension} onEdit={editScript} />
        )}
        {page === "code" && (
          <PlaygroundPage draft={draft} onDraftTaken={takeDraft} approvals={approvals} onReview={review}
            onOpenExtension={openExtension} onOpenRun={openRun} />
        )}
      </div>
    </div>
  );
}
