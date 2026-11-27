import { useState, type ReactNode } from "react";
import type { Approval, HistoryRecord } from "../lib/api";
import { duration } from "../lib/useLive";
import { sourceIcon, sourceLong } from "../lib/source";
import { Icon, type IconName } from "../orbit/icons";

type Call = NonNullable<HistoryRecord["calls"]>[number];
type ApprovalEvent = NonNullable<HistoryRecord["approvals"]>[number];

const OUTCOME: Record<string, { label: string; tone: string }> = {
  "allowed-once": { label: "You allowed it once", tone: "live" },
  "allowed-always": { label: "You allowed it (always)", tone: "live" },
  denied: { label: "You denied it", tone: "error" },
  "denied-always": { label: "You blocked it (always)", tone: "error" },
  "timed-out": { label: "Not approved in time", tone: "error" },
  cancelled: { label: "Run stopped while asking", tone: "error" },
  blocked: { label: "Blocked by your settings", tone: "error" },
};

const STATE_TONE: Record<string, string> = {
  succeeded: "live",
  failed: "error",
  cancelled: "error",
  running: "pending",
  queued: "pending",
};

interface Props {
  rec: HistoryRecord;
  /** Approvals this run is waiting on right now. */
  waiting?: Approval[];
  onReview?: (approvalId: string) => void;
  onOpenExtension?: (namespace: string) => void;
  actions?: ReactNode;
}

/** Everything known about one run: who, what it called, with which
 *  arguments, what you approved, and what came out. */
export function RunDetail({ rec, waiting = [], onReview, onOpenExtension, actions }: Props) {
  const live = rec.state === "queued" || rec.state === "running";
  const calls = rec.calls ?? [];
  const events = pairEvents(calls, rec.approvals ?? []);
  const errored = calls.filter((c) => c.status === "error").length;

  return (
    <div className="run-detail">
      <div className="run-head">
        <span className={`insp-icon tone-${STATE_TONE[rec.state] ?? "dim"}`}>
          <Icon name={sourceIcon(rec.source)} size={22} />
        </span>
        <div className="insp-title">
          <h2>{sourceLong(rec.source)}{rec.scriptName && <span className="muted"> · {rec.scriptName}</span>}</h2>
          <span className="mono muted">
            {new Date(rec.createdAt).toLocaleString()} · <CopyText text={rec.id} label={rec.id.slice(0, 16)} />
          </span>
        </div>
        <span className={`badge tone-${STATE_TONE[rec.state] ?? "dim"}`}>{rec.state.toUpperCase()}</span>
      </div>

      <div className="stat-grid">
        <Stat label="Took" value={duration(rec.startedAt, rec.finishedAt) || (live ? "…" : "–")} />
        <Stat label="Tool calls" value={String(rec.toolCalls)} />
        <Stat label="Errors" value={String(errored)} />
        <Stat label="Approvals" value={String((rec.approvals ?? []).filter((a) => a.outcome !== "blocked").length)} />
      </div>

      {waiting.length > 0 && (
        <div className="notice">
          <span>
            Paused: waiting for you to approve <code>{waiting.map((w) => `${w.namespace}.${w.tool}()`).join(", ")}</code>
          </span>
          {onReview && <button className="btn primary" onClick={() => onReview(waiting[0].id)}>Review</button>}
        </div>
      )}

      {rec.error && (
        <Block title="ERROR">
          <pre className="code error">{rec.error}</pre>
        </Block>
      )}

      <Block title="TIMELINE" aside={calls.length < rec.toolCalls ? `first ${calls.length} of ${rec.toolCalls}` : undefined}>
        {calls.length === 0 ? (
          <p className="hint">{live ? "Tool calls appear here when the run finishes." : "This run called no tools."}</p>
        ) : (
          <ol className="timeline">
            {calls.map((c, i) => (
              <CallRow key={i} n={i + 1} c={c} ev={events.get(i)} t0={rec.startedAt} onOpenExtension={onOpenExtension} />
            ))}
          </ol>
        )}
      </Block>

      {rec.state === "succeeded" && (
        <Block title="RESULT" aside={!rec.truncated && <CopyText text={JSON.stringify(rec.result ?? null, null, 2)} label="Copy" />}>
          {rec.truncated ? (
            <p className="hint">The result was larger than 32 KB, so it wasn't kept in history.</p>
          ) : (
            <pre className="code">{JSON.stringify(rec.result ?? null, null, 2)}</pre>
          )}
        </Block>
      )}

      {(rec.logs ?? []).length > 0 && (
        <Block title="CONSOLE">
          <pre className="code">{(rec.logs ?? []).join("\n")}</pre>
        </Block>
      )}

      {rec.args && Object.keys(rec.args).length > 0 && (
        <Block title="ARGUMENTS">
          <pre className="code">{JSON.stringify(rec.args, null, 2)}</pre>
        </Block>
      )}

      <Block title="SCRIPT" aside={<CopyText text={rec.code} label="Copy" />}>
        <pre className="code code-tall">{rec.code}</pre>
      </Block>

      {actions && <div className="insp-actions">{actions}</div>}
    </div>
  );
}

/** Matches each approval event to the call it happened in: same tool, the
 *  first unmatched call that started before the prompt. */
function pairEvents(calls: Call[], events: ApprovalEvent[]): Map<number, ApprovalEvent> {
  const out = new Map<number, ApprovalEvent>();
  for (const e of events) {
    const asked = new Date(e.askedAt).getTime();
    const i = calls.findIndex((c, idx) => !out.has(idx) && c.tool === e.tool && new Date(c.startedAt).getTime() <= asked + 5);
    if (i >= 0) out.set(i, e);
  }
  return out;
}

function CallRow({ n, c, ev, t0, onOpenExtension }: {
  n: number; c: Call; ev?: ApprovalEvent; t0?: string | null; onOpenExtension?: (ns: string) => void;
}) {
  const [open, setOpen] = useState(false);
  const offset = t0 ? Math.max(0, new Date(c.startedAt).getTime() - new Date(t0).getTime()) : 0;
  const outcome = ev ? OUTCOME[ev.outcome] ?? { label: ev.outcome, tone: "dim" } : null;
  const waitedMs = ev ? new Date(ev.decidedAt).getTime() - new Date(ev.askedAt).getTime() : 0;
  const [ns] = c.tool.split(".");
  return (
    <li className={`tl-item tl-${c.status}`}>
      <span className="tl-n">{n}</span>
      <div className="tl-body">
        <div className="tl-line">
          <button className="tl-tool mono" aria-expanded={open} onClick={() => setOpen(!open)}>
            <Icon name={open ? "chevronDown" : "chevronRight"} size={14} stroke={2} />
            {c.tool}()
          </button>
          <span className={`state-dot state-${c.status === "ok" ? "succeeded" : c.status === "error" ? "failed" : "running"}`} />
          <span className="tl-meta mono">+{fmtMs(offset)} · {fmtMs(c.durationMs)}</span>
        </div>
        {outcome && (
          <div className={`tl-approval tone-text-${outcome.tone === "live" ? "ext" : outcome.tone}`}>
            <Icon name={ev!.outcome === "blocked" ? "ban" : "hand"} size={13} stroke={2} />
            {outcome.label}
            {ev!.outcome !== "blocked" && <span className="muted"> · after {fmtMs(waitedMs)}</span>}
          </div>
        )}
        {c.error && <div className="tl-error">{c.error}</div>}
        {open && (
          <div className="tl-detail">
            <span className="insp-label"><span>ARGUMENTS</span></span>
            {c.argsTruncated ? (
              <p className="hint">Arguments larger than 4 KB aren't recorded.</p>
            ) : (
              <pre className="code">{JSON.stringify(c.args ?? {}, null, 2)}</pre>
            )}
            <span className="hint">
              Result: {c.status === "ok" ? `${fmtBytes(c.resultBytes ?? 0)}` : c.status}
              {onOpenExtension && (
                <> · <button className="link" onClick={() => onOpenExtension(ns)}>open {ns}</button></>
              )}
            </span>
          </div>
        )}
      </div>
    </li>
  );
}

function Block({ title, aside, children }: { title: string; aside?: ReactNode; children: ReactNode }) {
  return (
    <section className="insp-section">
      <div className="insp-label">
        <span>{title}</span>
        {aside}
      </div>
      {children}
    </section>
  );
}

function Stat({ label, value }: { label: string; value: string }) {
  return (
    <div className="stat">
      <span className="stat-value">{value}</span>
      <span className="stat-label">{label}</span>
    </div>
  );
}

export function CopyText({ text, label, icon = "copy" }: { text: string; label: string; icon?: IconName }) {
  const [done, setDone] = useState(false);
  return (
    <button className="copy-text" title="Copy"
      onClick={async () => { await navigator.clipboard.writeText(text); setDone(true); setTimeout(() => setDone(false), 1200); }}>
      <Icon name={done ? "check" : icon} size={12} stroke={2} />
      {done ? "Copied" : label}
    </button>
  );
}

function fmtMs(ms: number): string {
  if (ms < 1000) return `${ms} ms`;
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)} s`;
  return `${Math.floor(ms / 60_000)}m ${Math.round((ms % 60_000) / 1000)}s`;
}

function fmtBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1 << 20) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / (1 << 20)).toFixed(1)} MB`;
}
