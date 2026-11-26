import { useEffect, useState } from "react";
import { Catalog, Connect, Network, Runtime, RuntimeState, Setup, openExternal, type CatalogItem, type ConnectInfo, type NetworkEndpoint, type RuntimeStatus } from "../lib/api";
import { useLive } from "../lib/useLive";
import { Icon, Logo, iconFor } from "../orbit/icons";
import { ConnectGuide } from "../orbit/Inspector";

type Step = "runtime" | "extensions" | "connect";

const STEPS: { id: Step; label: string }[] = [
  { id: "runtime", label: "Node.js runtime" },
  { id: "extensions", label: "Extensions" },
  { id: "connect", label: "Connect an AI" },
];

/** Preselected on first run: what most people want an AI to do on their
 *  computer. Everything else in the catalog is one click away. */
const STARTERS = ["files", "excel", "word", "pdf", "notes", "web"];
/** Catalog entries that only demonstrate the SDK. */
const HIDDEN = ["hello"];

/** First-run setup: get a Node.js runtime, add extensions, then connect an
 *  AI. Shown on first launch and whenever Node.js is unusable. */
export default function SetupPage({ onFinish, onSkip }: { onFinish: () => void; onSkip: () => void }) {
  const [step, setStep] = useState<Step>("runtime");
  // On by default, like a chat app: AIs can only reach Nexora while it runs.
  const [atLogin, setAtLogin] = useState(true);
  const [finishing, setFinishing] = useState(false);
  const finish = async () => {
    setFinishing(true);
    await Setup.SetAutostart(atLogin).catch(console.error);
    onFinish();
  };
  const [status, refresh] = useLive(() => Runtime.Status(), 400, null as RuntimeStatus | null);
  const ready = status?.state === RuntimeState.StateReady;

  return (
    <div className="setup">
      <header className="setup-top">
        <Logo size={28} />
        <span className="setup-brand">Nexora</span>
        <ol className="setup-steps" aria-label="Setup steps">
          {STEPS.map((s, i) => {
            const at = STEPS.findIndex((x) => x.id === step);
            const state = i === at ? "on" : i < at ? "done" : "";
            return (
              <li key={s.id} className={state} aria-current={state === "on" ? "step" : undefined}>
                <span className="setup-step-dot">{state === "done" ? <Icon name="check" size={12} stroke={3} /> : i + 1}</span>
                {s.label}
              </li>
            );
          })}
        </ol>
      </header>

      <main className="setup-main">
        {step === "runtime" ? (
          <>
            <div className="setup-intro">
              <h1>Set up Nexora</h1>
              <p>
                Extensions like Excel, Word and PDF run on Node.js. Nexora can download its own copy, kept separate from
                anything else on your computer.
              </p>
            </div>
            <RuntimeCard status={status} refresh={refresh} />
          </>
        ) : step === "extensions" ? (
          <>
            <div className="setup-intro">
              <h1>Add extensions</h1>
              <p>
                Extensions are what an AI can use on your computer. Pick a few to start; you can add or remove them any
                time with <strong>Add extension</strong>.
              </p>
            </div>
            <ExtensionsStep onDone={() => setStep("connect")} />
          </>
        ) : (
          <>
            <div className="setup-intro">
              <h1>Connect an AI</h1>
              <p>Pick the AI you use. You can come back to this any time from the core panel.</p>
            </div>
            <ConnectStep />
            <section className="setup-card setup-login">
              <div className="setting-text">
                <span className="setting-head">Start Nexora when I log in</span>
                <small className="setting-help">
                  It starts in the background, without a window, so your AIs can reach it right after a restart.
                  You can change this later in the core panel.
                </small>
              </div>
              <label className="switch">
                <input type="checkbox" aria-label="Start Nexora when I log in" checked={atLogin} onChange={(e) => setAtLogin(e.target.checked)} />
                <span aria-hidden="true" />
              </label>
            </section>
          </>
        )}
      </main>

      <footer className="setup-foot">
        {step === "runtime" ? (
          <>
            <button className="link setup-skip" onClick={onSkip}>Skip for now</button>
            <button className="btn primary" disabled={!ready} onClick={() => setStep("extensions")}>
              Continue <Icon name="chevronRight" size={16} stroke={2} />
            </button>
          </>
        ) : step === "extensions" ? (
          <>
            <button className="btn ghost" onClick={() => setStep("runtime")}>Back</button>
            {/* Continue lives in the step: it installs first. */}
            <button className="link setup-skip" onClick={() => setStep("connect")}>Skip, add them later</button>
          </>
        ) : (
          <>
            <button className="btn ghost" onClick={() => setStep("extensions")}>Back</button>
            <button className="btn primary" disabled={finishing} onClick={finish}>Open Nexora</button>
          </>
        )}
      </footer>
    </div>
  );
}

function RuntimeCard({ status, refresh }: { status: RuntimeStatus | null; refresh: () => void }) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const act = async (fn: () => Promise<unknown>) => {
    setBusy(true);
    setError(null);
    try {
      await fn();
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
      refresh();
    }
  };

  if (!status) {
    return <section className="setup-card"><p className="muted">Looking for Node.js…</p></section>;
  }
  const pinned = status.pinned?.replace(/^v/, "");
  const download = () => act(() => Runtime.Download());
  const recheck = () => act(() => Runtime.Recheck());

  switch (status.state) {
    case RuntimeState.StateReady:
      return (
        <section className="setup-card tone-ready">
          <div className="setup-card-head">
            <span className="setup-icon ok"><Icon name="check" size={20} stroke={2.5} /></span>
            <div>
              <h2>Node.js {status.version} is ready</h2>
              <p>
                {status.source === "managed" && "Downloaded and managed by Nexora."}
                {status.source === "system" && "Using the Node.js installed on this computer."}
                {status.source === "custom" && "Using the Node.js set with NEXORA_NODE."}
              </p>
            </div>
          </div>
          <code className="setup-path">{status.path}</code>
          {status.source === "system" && pinned && (
            <p className="hint">
              Prefer a copy that doesn't change when you update your system?{" "}
              <button className="link" disabled={busy} onClick={download}>Download Node.js {pinned}</button>
            </p>
          )}
        </section>
      );

    case RuntimeState.StateDownloading: {
      const total = status.total ?? 0;
      const done = status.downloaded ?? 0;
      const unpacking = total > 0 && done >= total;
      const pct = total > 0 ? Math.min(100, Math.round((done / total) * 100)) : null;
      return (
        <section className="setup-card">
          <div className="setup-card-head">
            <span className="setup-icon busy"><Icon name="download" size={20} /></span>
            <div>
              <h2>{unpacking ? "Unpacking Node.js…" : `Downloading Node.js ${pinned}`}</h2>
              <p>From nodejs.org, checked against its official checksum.</p>
            </div>
          </div>
          <div className={`setup-progress ${pct === null || unpacking ? "indeterminate" : ""}`} role="progressbar"
            aria-valuemin={0} aria-valuemax={100} aria-valuenow={pct ?? undefined} aria-label="Download progress">
            <span style={{ width: pct === null || unpacking ? undefined : `${pct}%` }} />
          </div>
          <div className="setup-progress-meta">
            <span className="mono">
              {unpacking ? "Almost done" : total > 0 ? `${mb(done)} of ${mb(total)}` : mb(done)}
            </span>
            {pct !== null && !unpacking && <span className="mono">{pct}%</span>}
          </div>
          {!unpacking && (
            <div className="setup-actions">
              <button className="btn ghost" disabled={busy} onClick={() => act(() => Runtime.Cancel())}>Cancel</button>
            </div>
          )}
        </section>
      );
    }

    default: {
      const outdated = status.state === RuntimeState.StateOutdated;
      const failed = status.state === RuntimeState.StateError;
      return (
        <section className={`setup-card ${failed ? "tone-error" : ""}`}>
          <div className="setup-card-head">
            <span className={`setup-icon ${failed ? "bad" : "warn"}`}><Icon name={failed ? "x" : "cpu"} size={20} stroke={2} /></span>
            <div>
              <h2>
                {failed ? "Couldn't set up Node.js" : outdated ? `Node.js ${status.found} is too old` : "Node.js isn't installed"}
              </h2>
              <p>
                {failed
                  ? sentence(status.error)
                  : outdated
                    ? `Extensions need Node.js ${status.required.replace(">= ", "")} or newer. Nexora can download its own copy without touching yours.`
                    : "Nexora needs it to run extensions. It's a one-time download."}
              </p>
            </div>
          </div>
          {pinned ? (
            <div className="setup-actions">
              <button className="btn primary" disabled={busy} onClick={download}>
                <Icon name="download" size={16} /> {failed ? "Try again" : `Download Node.js ${pinned}`}
              </button>
              <button className="btn ghost" disabled={busy} onClick={recheck}>I installed it, check again</button>
            </div>
          ) : (
            <div className="setup-actions">
              <button className="btn primary" onClick={() => openExternal("https://nodejs.org/en/download")}>Get Node.js</button>
              <button className="btn ghost" disabled={busy} onClick={recheck}>Check again</button>
            </div>
          )}
          <p className="hint">
            {pinned
              ? "About 60 MB from nodejs.org, verified against the official checksum and kept in Nexora's own folder."
              : `There's no download for this computer; install Node.js ${status.required.replace(">= ", "")} or newer yourself.`}
          </p>
          {error && <p className="error">{error}</p>}
        </section>
      );
    }
  }
}

type InstallState = "installing" | "installed" | "failed";

/** Pick extensions from the catalog and install them one by one. */
function ExtensionsStep({ onDone }: { onDone: () => void }) {
  const [items, setItems] = useState<CatalogItem[] | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [picked, setPicked] = useState<Set<string>>(new Set());
  const [progress, setProgress] = useState<Record<string, { state: InstallState; error?: string }>>({});
  const [running, setRunning] = useState(false);

  const load = () => {
    setLoadError(null);
    Catalog.List(false)
      .then((view) => {
        const list = (view.items ?? []).filter((i) => !HIDDEN.includes(i.id) && i.state !== "incompatible");
        setItems(list);
        setPicked(new Set(list.filter((i) => STARTERS.includes(i.id) && i.state === "available").map((i) => i.id)));
        if (!view.configured) setLoadError("No extension catalog is configured (NEXORA_REGISTRY_URL).");
      })
      .catch((e) => setLoadError(String(e)));
  };
  useEffect(load, []);

  const toggle = (id: string) => {
    const next = new Set(picked);
    if (next.has(id)) next.delete(id); else next.add(id);
    setPicked(next);
  };

  const install = async () => {
    setRunning(true);
    let failed = false;
    for (const id of picked) {
      if (progress[id]?.state === "installed") continue;
      setProgress((p) => ({ ...p, [id]: { state: "installing" } }));
      try {
        await Catalog.Install(id);
        setProgress((p) => ({ ...p, [id]: { state: "installed" } }));
      } catch (e) {
        failed = true;
        setProgress((p) => ({ ...p, [id]: { state: "failed", error: String(e) } }));
      }
    }
    setRunning(false);
    if (!failed) onDone();
  };

  if (loadError && !items?.length) {
    return (
      <section className="setup-card tone-error">
        <p className="error">Couldn't load the extension catalog: {loadError}</p>
        <div className="setup-actions">
          <button className="btn ghost" onClick={load}>Try again</button>
        </div>
      </section>
    );
  }
  if (!items) return <section className="setup-card"><p className="muted">Loading the catalog…</p></section>;

  const todo = [...picked].filter((id) => progress[id]?.state !== "installed").length;
  const anyFailed = Object.values(progress).some((p) => p.state === "failed");

  return (
    <>
      <ul className="setup-exts" aria-label="Extensions">
        {items.map((it) => {
          const have = it.state === "installed" || it.state === "update" || it.state === "dev" || progress[it.id]?.state === "installed";
          const p = progress[it.id];
          return (
            <li key={it.id}>
              <label className={`setup-ext ${picked.has(it.id) || have ? "on" : ""} ${have ? "have" : ""}`}>
                <input type="checkbox" checked={have || picked.has(it.id)} disabled={have || running} onChange={() => toggle(it.id)} />
                <span className="setup-ext-icon"><Icon name={iconFor(it.icon)} size={18} /></span>
                <span className="setup-ext-text">
                  <span className="setup-ext-name">{it.name}</span>
                  <span className="setup-ext-desc" title={it.description}>{it.description}</span>
                  {p?.state === "failed" && <span className="error">{p.error}</span>}
                </span>
                <span className="setup-ext-state mono">
                  {have ? "added" : p?.state === "installing" ? "adding…" : p?.state === "failed" ? "failed" : size(it.size)}
                </span>
              </label>
            </li>
          );
        })}
      </ul>
      <div className="setup-actions setup-ext-actions">
        <button className="btn primary" disabled={running || todo === 0} onClick={install}>
          {running ? "Adding…" : anyFailed ? "Try again" : `Add ${todo} extension${todo === 1 ? "" : "s"} and continue`}
        </button>
        {(todo === 0 || anyFailed) && !running && (
          <button className="btn ghost" onClick={onDone}>Continue</button>
        )}
      </div>
      <p className="hint">Files and similar extensions ask which folders they may use; set that on their panel after setup.</p>
    </>
  );
}

function ConnectStep() {
  const [connect, setConnect] = useState<ConnectInfo | null>(null);
  const [network] = useLive(async () => (await Network.List()) ?? [], 3000, [] as NetworkEndpoint[]);
  useEffect(() => {
    Connect.Info().then(setConnect).catch(() => {});
  }, []);
  if (!connect) return null;
  if (!connect.enabled) {
    return <section className="setup-card"><p className="hint">The MCP endpoint is disabled (NEXORA_MCP_ADDR=off or the port is in use), so AIs can't connect yet.</p></section>;
  }
  return (
    <section className="setup-card setup-guide">
      <ConnectGuide connect={connect} live={network.find((n) => n.active)} />
    </section>
  );
}

/** Go errors start lowercase; shown alone they read better capitalised. */
function sentence(s?: string) {
  return s ? s.charAt(0).toUpperCase() + s.slice(1) : "";
}

function size(bytes: number) {
  return bytes < 1e6 ? `${Math.max(1, Math.round(bytes / 1e3))} KB` : mb(bytes);
}

function mb(bytes: number) {
  return `${(bytes / 1e6).toFixed(1)} MB`;
}
