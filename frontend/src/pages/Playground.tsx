import { useEffect, useRef, useState, type KeyboardEvent } from "react";
import { RunDetail } from "../components/RunDetail";
import { StoragePane } from "../components/StoragePane";
import { CodeMode, Extensions, History, type Approval, type ExtensionInfo } from "../lib/api";
import { useLive, useRun } from "../lib/useLive";
import { Icon } from "../orbit/icons";

const DRAFT_KEY = "nexora.codemode.draft";

interface Props {
  /** A script handed over from elsewhere (History's "Edit in Code mode"). */
  draft: string | null;
  onDraftTaken: () => void;
  approvals: Approval[];
  onReview: (approvalId: string) => void;
  onOpenExtension: (namespace: string) => void;
  onOpenRun: (id: string) => void;
}

export default function PlaygroundPage({ draft, onDraftTaken, approvals, onReview, onOpenExtension, onOpenRun }: Props) {
  const [extensions] = useLive(async () => (await Extensions.List()) ?? [], 4000, [] as ExtensionInfo[]);
  const [types, refreshTypes] = useLive(() => CodeMode.Types(), 4000, "");
  const [code, setCode] = useState<string>(() => readDraft() ?? "");
  const [running, setRunning] = useState(false);
  const [runId, setRunId] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  // The pane beside the editor: the API, what scripts stored, or nothing.
  const [side, setSide] = useState<"api" | "storage" | null>("api");
  const started = useRef(0);
  // Seed a sample only on the very first visit, not whenever the editor is cleared.
  const seeded = useRef(readDraft() !== null);

  // A script handed over from History replaces the editor's contents.
  useEffect(() => {
    if (draft === null) return;
    seeded.current = true;
    setCode(draft);
    onDraftTaken();
  }, [draft, onDraftTaken]);

  // First visit: a sample that works with whatever is installed.
  useEffect(() => {
    if (seeded.current || extensions.length === 0) return;
    seeded.current = true;
    setCode(sampleFor(extensions));
  }, [extensions]);

  useEffect(() => {
    try {
      localStorage.setItem(DRAFT_KEY, code);
    } catch {
      /* storage unavailable: the draft just won't survive a restart */
    }
  }, [code]);

  // While a run is in flight, find its history record so its timeline and
  // approvals show live and it can be stopped.
  const [recent] = useLive(async () => (running ? (await History.Summaries(5)) ?? [] : []), 500, []);
  useEffect(() => {
    if (!running || runId) return;
    const mine = recent.find((r) => r.source === "app" && new Date(r.createdAt).getTime() >= started.current - 1000);
    if (mine) setRunId(mine.id);
  }, [recent, running, runId]);
  const summary = recent.find((r) => r.id === runId);
  const rec = useRun(runId, `${running}:${summary?.state}:${summary?.toolCalls}`);

  const run = async () => {
    if (running || !code.trim()) return;
    setRunning(true);
    setError(null);
    setRunId(null);
    started.current = Date.now();
    try {
      const job = await CodeMode.Run(code);
      setRunId(job.id);
    } catch (e) {
      setError(String(e));
    } finally {
      setRunning(false);
      refreshTypes();
    }
  };

  const onKey = (e: KeyboardEvent<HTMLTextAreaElement>) => {
    if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) {
      e.preventDefault();
      run();
    } else if (e.key === "Tab" && !e.shiftKey) {
      e.preventDefault();
      const el = e.currentTarget;
      const { selectionStart: a, selectionEnd: b } = el;
      setCode(code.slice(0, a) + "  " + code.slice(b));
      requestAnimationFrame(() => el.setSelectionRange(a + 2, a + 2));
    }
  };

  const live = extensions.filter((e) => e.status === "running");
  const waiting = runId ? approvals.filter((a) => a.jobId === runId) : [];

  return (
    <section className="playground">
      <header className="page-header">
        <div>
          <h1>Code Mode</h1>
          <p className="muted">
            Run a script exactly as an AI would: only <code>tools</code> is available, and your trust settings apply.
          </p>
        </div>
        <div className="header-actions">
          {running && runId && (
            <button className="btn danger" onClick={() => History.Cancel(runId).catch((e) => setError(String(e)))}>
              Stop
            </button>
          )}
          <button className="btn primary" onClick={run} disabled={running || !code.trim()} title="Run (Ctrl+Enter)">
            <Icon name={running ? "clock" : "terminal"} size={16} />
            {running ? "Running…" : "Run"}
            {!running && <kbd>Ctrl ↵</kbd>}
          </button>
        </div>
      </header>

      <div className={`split ${side ? "" : "single"}`}>
        <div className="pane editor-pane">
          <div className="pane-head">
            <h3>Script</h3>
            <div className="pane-actions">
              <button className="link-btn" onClick={() => setCode(sampleFor(extensions))}>Load sample</button>
              {!side && <button className="link-btn" onClick={() => setSide("api")}>Show API</button>}
            </div>
          </div>
          <textarea spellCheck={false} value={code} onChange={(e) => setCode(e.target.value)} onKeyDown={onKey}
            aria-label="Script" placeholder="return await tools.…" />
        </div>
        {side && (
          <div className="pane">
            <div className="pane-head">
              <div className="tabs" role="tablist" aria-label="Side panel">
                <button role="tab" aria-selected={side === "api"} className={side === "api" ? "on" : ""} onClick={() => setSide("api")}>
                  API
                </button>
                <button role="tab" aria-selected={side === "storage"} className={side === "storage" ? "on" : ""} onClick={() => setSide("storage")}>
                  Storage
                </button>
              </div>
              <span className="pane-actions">
                {side === "api" && <span className="muted">{live.length} of {extensions.length} extensions running</span>}
                <button className="link-btn" onClick={() => setSide(null)}>Hide</button>
              </span>
            </div>
            {side === "storage" ? <StoragePane version={`${running}:${runId}`} /> : types.trim() ? <pre className="api">{types}</pre> : (
              <p className="hint">No extensions are running, so <code>tools</code> is empty. Add or start one on the Orbit page.</p>
            )}
          </div>
        )}
      </div>

      <div className="pane output-pane">
        {error && <p className="error">{error}</p>}
        {rec ? (
          <RunDetail rec={rec} waiting={waiting} onReview={onReview} onOpenExtension={onOpenExtension}
            actions={!running && (
              <button className="btn ghost" onClick={() => onOpenRun(rec.id)}>
                <Icon name="history" size={16} /> Open in History
              </button>
            )} />
        ) : (
          !error && (
            <p className="hint">
              {running ? "Starting…" : "Results, logs and every tool call show up here. Press Ctrl+Enter to run."}
            </p>
          )
        )}
      </div>
    </section>
  );
}

function readDraft(): string | null {
  try {
    return localStorage.getItem(DRAFT_KEY);
  } catch {
    return null;
  }
}

/** A starter script that runs whatever is installed, with an example call
 *  to a real read-only tool to edit. */
function sampleFor(extensions: ExtensionInfo[]): string {
  let example = "tools.<extension>.<tool>({ ... })";
  for (const e of extensions) {
    const ns = e.manifest?.namespace ?? e.manifest?.id;
    const tool = e.status === "running" && (e.tools ?? []).find((t) => t.readOnly);
    if (ns && tool) {
      example = `tools.${ns}.${tool.name}({})`;
      break;
    }
  }
  return `// Scripts run as the body of an async function. Only \`tools\` is
// available: no fs, network or process access. Independent calls can run
// in parallel with Promise.all. Ctrl+Enter runs.
//
// Try a call, e.g.:
//   const result = await ${example}
//   return result

const available = Object.keys(tools)
console.log("extensions:", available.join(", "))
return available
`;
}
