import { useEffect, useMemo, useState } from "react";
import { RunDetail } from "../components/RunDetail";
import { History, type Approval, type RunSummary } from "../lib/api";
import { duration, timeAgo, useLive, useRun } from "../lib/useLive";
import { Icon } from "../orbit/icons";

type StateFilter = "all" | "succeeded" | "failed" | "live";
type SourceFilter = "all" | "mcp" | "app";

const STATE_FILTERS: [StateFilter, string][] = [["all", "All"], ["succeeded", "Succeeded"], ["failed", "Failed"], ["live", "Running"]];
const SOURCE_FILTERS: [SourceFilter, string][] = [["all", "Anyone"], ["mcp", "AI"], ["app", "You"]];

interface Props {
  selectedId: string | null;
  onSelect: (id: string | null) => void;
  approvals: Approval[];
  onReview: (approvalId: string) => void;
  onOpenExtension: (namespace: string) => void;
}

export default function HistoryPage({ selectedId, onSelect, approvals, onReview, onOpenExtension }: Props) {
  // Summaries only: full runs (script, arguments, logs) are fetched one at a
  // time for the run being shown.
  const [records, refresh] = useLive(async () => (await History.Summaries(500)) ?? [], 3000, [] as RunSummary[]);
  const [state, setState] = useState<StateFilter>("all");
  const [source, setSource] = useState<SourceFilter>("all");
  const [query, setQuery] = useState("");
  const [confirmClear, setConfirmClear] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);

  const shown = useMemo(() => {
    const q = query.trim().toLowerCase();
    return records.filter((r) => {
      if (source !== "all" && r.source !== source) return false;
      if (state === "succeeded" && r.state !== "succeeded") return false;
      if (state === "failed" && r.state !== "failed" && r.state !== "cancelled") return false;
      if (state === "live" && r.state !== "running" && r.state !== "queued") return false;
      if (!q) return true;
      return (
        r.title.toLowerCase().includes(q) ||
        (r.tools ?? []).some((t) => t.toLowerCase().includes(q)) ||
        (r.error ?? "").toLowerCase().includes(q) ||
        r.id.includes(q)
      );
    });
  }, [records, state, source, query]);

  // Keep a selection: the requested run, else the newest shown.
  const selectedSum = records.find((r) => r.id === selectedId) ?? null;
  const selected = useRun(selectedSum ? selectedSum.id : null, `${selectedSum?.state}|${selectedSum?.finishedAt}`);
  useEffect(() => {
    if (!selectedId && shown[0]) onSelect(shown[0].id);
  }, [selectedId, shown, onSelect]);

  const act = async (fn: () => Promise<unknown>) => {
    setActionError(null);
    try {
      await fn();
    } catch (e) {
      setActionError(String(e));
    }
    refresh();
  };

  return (
    <div className="history-page">
      <section className="history-list" aria-label="Runs">
        <header className="history-header">
          <div className="history-title">
            <h1>History</h1>
            <span className="muted">{records.length} runs kept on this machine</span>
          </div>
          {confirmClear ? (
            <div className="history-clear">
              <button className="btn danger" onClick={() => act(async () => { await History.Clear(); onSelect(null); setConfirmClear(false); })}>
                Delete all
              </button>
              <button className="btn ghost" onClick={() => setConfirmClear(false)}>Keep</button>
            </div>
          ) : (
            <button className="btn ghost" disabled={records.length === 0} onClick={() => setConfirmClear(true)}>
              <Icon name="trash" size={16} /> Clear
            </button>
          )}
        </header>

        <div className="history-filters">
          <label className="search">
            <Icon name="search" size={16} />
            <span className="sr-only">Search runs</span>
            <input value={query} onChange={(e) => setQuery(e.target.value)} placeholder="Search scripts, tools, errors" />
          </label>
          <div className="filter-row">
            <Chips value={state} options={STATE_FILTERS} onChange={setState} label="Status" />
            <Chips value={source} options={SOURCE_FILTERS} onChange={setSource} label="Source" />
          </div>
        </div>

        {shown.length === 0 ? (
          <p className="hint history-empty">
            {records.length === 0 ? "No runs yet. Scripts from you and from AIs show up here." : "No runs match these filters."}
          </p>
        ) : (
          <ul className="run-list">
            {shown.map((r) => (
              <li key={r.id}>
                <button className={`run-item ${r.id === selectedId ? "selected" : ""}`} aria-current={r.id === selectedId}
                  onClick={() => onSelect(r.id)}>
                  <span className={`run-icon state-${r.state}`}>
                    <Icon name={r.source === "mcp" ? "ai" : "user"} size={15} />
                  </span>
                  <span className="run-main">
                    <span className="run-summary mono">{summary(r)}</span>
                    <span className="run-sub">
                      {r.source === "mcp" ? "AI" : "You"} · {r.state}
                      {r.finishedAt && ` · ${duration(r.startedAt, r.finishedAt)}`}
                      {approvals.some((a) => a.jobId === r.id) && <span className="tone-text-pending"> · needs you</span>}
                    </span>
                  </span>
                  <span className="run-time muted" title={new Date(r.createdAt).toLocaleString()}>{timeAgo(r.createdAt)}</span>
                </button>
              </li>
            ))}
          </ul>
        )}
      </section>

      <section className="history-detail" aria-label="Run details">
        {selected ? (
          <RunDetail
            rec={selected}
            waiting={approvals.filter((a) => a.jobId === selected.id)}
            onReview={onReview}
            onOpenExtension={onOpenExtension}
            actions={
              <>
                {selected.state === "running" || selected.state === "queued" ? (
                  <button className="btn danger" onClick={() => act(() => History.Cancel(selected.id))}>Cancel run</button>
                ) : (
                  <button className="btn primary"
                    onClick={() => act(async () => { const j = await History.Rerun(selected.id); onSelect(j.id); })}>
                    <Icon name="refresh" size={16} /> Run again
                  </button>
                )}
                {actionError && <p className="error">{actionError}</p>}
              </>
            }
          />
        ) : (
          <p className="hint">Select a run to see everything it did.</p>
        )}
      </section>
    </div>
  );
}

/** One line describing a run: the tools it used, else its first code line. */
function summary(r: RunSummary): string {
  const tools = r.tools ?? [];
  if (tools.length) return tools.slice(0, 3).join(", ") + (tools.length > 3 ? ` +${tools.length - 3}` : "");
  return r.title;
}

function Chips<T extends string>({ value, options, onChange, label }: {
  value: T; options: [T, string][]; onChange: (v: T) => void; label: string;
}) {
  return (
    <div role="radiogroup" aria-label={label} className="chips">
      {options.map(([v, text]) => (
        <button key={v} role="radio" aria-checked={value === v} className={`chip ${value === v ? "on" : ""}`} onClick={() => onChange(v)}>
          {text}
        </button>
      ))}
    </div>
  );
}
