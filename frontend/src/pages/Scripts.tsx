import { useEffect, useMemo, useState, type ReactNode } from "react";
import { RunDetail } from "../components/RunDetail";
import { History, Scripts, type Approval, type RunSummary, type SavedScript, type ScriptParam, type ScriptSchedule } from "../lib/api";
import { sourceShort } from "../lib/source";
import { duration, timeAgo, timeUntil, useLive, useRun } from "../lib/useLive";
import { Icon } from "../orbit/icons";

/** What to open: a saved script, or a new one seeded with code. */
export type ScriptTarget = { id: string } | { code: string };

interface Props {
  target: ScriptTarget | null;
  onTargetTaken: () => void;
  approvals: Approval[];
  onReview: (approvalId: string) => void;
  onOpenExtension: (namespace: string) => void;
  onOpenRun: (id: string) => void;
}

const NEW = "new";
const TYPES = ["string", "number", "boolean", "json"] as const;
const DAYS: [number, string][] = [[1, "Mon"], [2, "Tue"], [3, "Wed"], [4, "Thu"], [5, "Fri"], [6, "Sat"], [0, "Sun"]];
const STARTER = `// Saved scripts read their inputs from \`args\`, e.g. args.folder.
// Add parameters below; scheduled runs use the values set under Schedule.
return Object.keys(tools)
`;

function blank(code = STARTER): SavedScript {
  return { id: "", name: "", code, params: [], createdBy: "app", createdAt: "", updatedAt: "" };
}

/** The fields the editor changes, for spotting unsaved edits. */
function editable(s: SavedScript) {
  return JSON.stringify([s.name, s.description ?? "", s.code, s.params ?? [], s.schedule ?? null, s.scheduleArgs ?? null]);
}

export default function ScriptsPage({ target, onTargetTaken, approvals, onReview, onOpenExtension, onOpenRun }: Props) {
  const [list, refreshList] = useLive(async () => (await Scripts.List()) ?? [], 3000, [] as SavedScript[]);
  const [runs] = useLive(async () => (await History.Summaries(500)) ?? [], 2000, [] as RunSummary[]);
  const [selected, setSelected] = useState<string | null>(null);
  const [draft, setDraft] = useState<SavedScript | null>(null);
  const [baseline, setBaseline] = useState("");
  const [pending, setPending] = useState<string | null>(null); // a switch waiting on unsaved edits
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [askArgs, setAskArgs] = useState(false);
  const [runArgs, setRunArgs] = useState<Record<string, string>>({});
  const [confirmDelete, setConfirmDelete] = useState(false);
  const [shownRun, setShownRun] = useState<string | null>(null);

  const dirty = draft !== null && editable(draft) !== baseline;

  const open = (id: string, seed?: SavedScript) => {
    const s = seed ?? (id === NEW ? blank() : list.find((x) => x.id === id));
    if (!s) return;
    setSelected(id);
    setDraft(structuredClone(s));
    setBaseline(id === NEW ? editable(blank()) : editable(s));
    setError(null);
    setAskArgs(false);
    setConfirmDelete(false);
    setShownRun(null);
    setPending(null);
  };
  const request = (id: string) => (dirty && id !== selected ? setPending(id) : open(id));

  // A script handed over from Code mode or History.
  useEffect(() => {
    if (!target) return;
    if ("code" in target) {
      open(NEW, blank(target.code));
      onTargetTaken();
    } else if (list.some((s) => s.id === target.id)) {
      open(target.id);
      onTargetTaken();
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [target, list]);

  // Keep a selection: the first script, once they've loaded.
  useEffect(() => {
    if (selected === null && !target && list[0]) open(list[0].id);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [list, selected, target]);

  const stored = list.find((s) => s.id === selected);
  const scriptRuns = useMemo(() => (selected && selected !== NEW ? runs.filter((r) => r.scriptId === selected) : []), [runs, selected]);
  const runId = shownRun ?? scriptRuns[0]?.id ?? null;
  const runSum = runs.find((r) => r.id === runId);
  const rec = useRun(runId, `${runSum?.state}|${runSum?.finishedAt}|${runSum?.toolCalls}`);

  const save = async (): Promise<SavedScript | null> => {
    if (!draft) return null;
    setBusy(true);
    setError(null);
    try {
      const saved = await Scripts.Save({
        id: draft.id, name: draft.name, description: draft.description ?? "", code: draft.code,
        params: draft.params ?? [], schedule: draft.schedule ?? null, scheduleArgs: draft.scheduleArgs ?? null,
      });
      setSelected(saved.id);
      setDraft(structuredClone(saved));
      setBaseline(editable(saved));
      refreshList();
      return saved;
    } catch (e) {
      setError(String(e));
      return null;
    } finally {
      setBusy(false);
    }
  };

  const startRun = async (values: Record<string, string>) => {
    const saved = dirty || !draft?.id ? await save() : draft;
    if (!saved) return;
    const parsed = parseArgs(saved.params ?? [], values);
    if (typeof parsed === "string") {
      setError(parsed);
      return;
    }
    setError(null);
    try {
      const job = await Scripts.Run(saved.id, JSON.stringify(parsed));
      setShownRun(job.id);
      setAskArgs(false);
      refreshList();
    } catch (e) {
      setError(String(e));
    }
  };

  const onRun = () => {
    const params = draft?.params ?? [];
    if (params.length === 0) return startRun({});
    setRunArgs(Object.fromEntries(params.map((p) => [p.name, toRaw(p.type, p.default)])));
    setAskArgs(true);
  };

  const remove = async () => {
    if (!draft?.id) return;
    try {
      await Scripts.Delete(draft.id);
      setSelected(null);
      setDraft(null);
      refreshList();
    } catch (e) {
      setError(String(e));
    }
  };

  const update = (patch: Partial<SavedScript>) => draft && setDraft({ ...draft, ...patch });

  return (
    <div className="history-page scripts-page">
      <section className="history-list" aria-label="Saved scripts">
        <header className="history-header">
          <div className="history-title">
            <h1>Scripts</h1>
            <span className="muted">Saved, reusable and scheduled scripts</span>
          </div>
          <button className="btn primary" onClick={() => request(NEW)}>
            <Icon name="plus" size={16} /> New
          </button>
        </header>
        {pending !== null && (
          <div className="notice scripts-unsaved">
            <span>Unsaved changes to <strong>{draft?.name || "this script"}</strong>.</span>
            <button className="btn ghost" onClick={() => setPending(null)}>Keep editing</button>
            <button className="btn danger" onClick={() => open(pending)}>Discard</button>
          </div>
        )}
        {list.length === 0 && selected !== NEW ? (
          <p className="hint history-empty">
            No saved scripts yet. Save one from Code mode or History, ask an AI to save one with <code>save_script</code>,
            or start a new one here. Scripts can take arguments and run on a schedule.
          </p>
        ) : (
          <ul className="run-list">
            {selected === NEW && (
              <li>
                <button className="run-item selected" aria-current>
                  <span className="run-icon"><Icon name="plus" size={15} /></span>
                  <span className="run-main">
                    <span className="run-summary">{draft?.name || "New script"}</span>
                    <span className="run-sub">Not saved yet</span>
                  </span>
                </button>
              </li>
            )}
            {list.map((s) => (
              <li key={s.id}>
                <button className={`run-item ${s.id === selected ? "selected" : ""}`} aria-current={s.id === selected}
                  onClick={() => request(s.id)}>
                  <span className={`run-icon state-${s.lastRun?.state ?? "none"}`}>
                    <Icon name={s.schedule?.enabled ? "clock" : "scroll"} size={15} />
                  </span>
                  <span className="run-main">
                    <span className="run-summary">{s.name}</span>
                    <span className="run-sub">
                      {s.schedule ? (s.schedule.enabled ? describeSchedule(s.schedule) : "Schedule paused") : "Runs when asked"}
                      {s.createdBy === "mcp" && " · saved by an AI"}
                    </span>
                  </span>
                  <span className="run-time muted" title={s.nextRun ? `Next run ${new Date(s.nextRun).toLocaleString()}` : undefined}>
                    {s.nextRun ? `in ${timeUntil(s.nextRun)}` : s.lastRun ? timeAgo(s.lastRun.at) : ""}
                  </span>
                </button>
              </li>
            ))}
          </ul>
        )}
      </section>

      <section className="history-detail" aria-label="Script">
        {!draft ? (
          <p className="hint">Select a script, or create a new one.</p>
        ) : (
          <div className="script-editor">
            <div className="script-head">
              <div className="script-title">
                <input className="script-name" value={draft.name} placeholder="Script name" aria-label="Name"
                  onChange={(e) => update({ name: e.target.value })} />
                <input value={draft.description ?? ""} placeholder="What it does (shown to AIs too)" aria-label="Description"
                  onChange={(e) => update({ description: e.target.value })} />
              </div>
              <div className="header-actions">
                <button className="btn ghost" onClick={onRun} disabled={busy} title={dirty ? "Saves your changes, then runs" : "Run now"}>
                  <Icon name="play" size={15} /> {dirty || !draft.id ? "Save & run" : "Run"}
                </button>
                <button className="btn primary" onClick={save} disabled={busy || !dirty}>
                  {draft.id ? (dirty ? "Save" : "Saved") : "Save"}
                </button>
              </div>
            </div>

            {stored?.createdBy === "mcp" && (
              <p className="hint">An AI saved this script. Read it before you schedule it or give its tools more trust.</p>
            )}
            {error && <p className="error">{error}</p>}

            {askArgs && (
              <Section title="RUN WITH">
                <ArgsForm params={draft.params ?? []} values={runArgs} onChange={setRunArgs} />
                <div className="insp-actions">
                  <button className="btn primary" onClick={() => startRun(runArgs)}>
                    <Icon name="play" size={15} /> Start
                  </button>
                  <button className="btn ghost" onClick={() => setAskArgs(false)}>Cancel</button>
                </div>
              </Section>
            )}

            <div className="pane editor-pane">
              <div className="pane-head">
                <h3>Script</h3>
                <span className="muted mono">{argsType(draft.params ?? [])}</span>
              </div>
              <textarea spellCheck={false} value={draft.code} aria-label="Script code"
                onChange={(e) => update({ code: e.target.value })} onKeyDown={indentOnTab((code) => update({ code }))} />
            </div>

            <Section title="PARAMETERS" aside={
              <button className="link-btn" onClick={() => update({ params: [...(draft.params ?? []), { name: nextName(draft.params ?? []), type: "string" }] })}>
                + Add parameter
              </button>}>
              {(draft.params ?? []).length === 0 ? (
                <p className="hint">No parameters. Add some to pass values such as a folder or a date as <code>args</code>, instead of hard-coding them.</p>
              ) : (
                <ParamsEditor params={draft.params ?? []} onChange={(params) => update({ params })} />
              )}
            </Section>

            <ScheduleEditor script={draft} onChange={update} />

            {draft.id && (
              <Section title="RUNS" aside={stored?.lastRun && <span className="muted">last {timeAgo(stored.lastRun.at)} ago · {sourceShort(stored.lastRun.source)}</span>}>
                {stored?.lastRun?.error && !stored.lastRun.jobId && <p className="error">{stored.lastRun.error}</p>}
                {scriptRuns.length === 0 ? (
                  <p className="hint">No runs kept in history yet.</p>
                ) : (
                  <ul className="rows">
                    {scriptRuns.slice(0, 6).map((r) => (
                      <li key={r.id}>
                        <button className={`row row-button ${r.id === runId ? "row-on" : ""}`} onClick={() => setShownRun(r.id)}>
                          <span className={`state-dot state-${r.state}`} />
                          <span>{sourceShort(r.source)}</span>
                          <span className="row-desc">{r.state}{r.finishedAt && ` · ${duration(r.startedAt, r.finishedAt)}`}{r.error && ` · ${r.error}`}</span>
                          <span className="muted">{timeAgo(r.createdAt)}</span>
                        </button>
                      </li>
                    ))}
                  </ul>
                )}
                {rec && (
                  <div className="pane output-pane">
                    <RunDetail rec={rec} waiting={approvals.filter((a) => a.jobId === rec.id)} onReview={onReview}
                      onOpenExtension={onOpenExtension}
                      actions={
                        <button className="btn ghost" onClick={() => onOpenRun(rec.id)}>
                          <Icon name="history" size={16} /> Open in History
                        </button>
                      } />
                  </div>
                )}
              </Section>
            )}

            {draft.id && (
              <div className="insp-actions">
                {confirmDelete ? (
                  <>
                    <button className="btn danger" onClick={remove}>Delete {draft.name}</button>
                    <button className="btn ghost" onClick={() => setConfirmDelete(false)}>Keep</button>
                  </>
                ) : (
                  <button className="btn danger" onClick={() => setConfirmDelete(true)}>
                    <Icon name="trash" size={16} /> Delete script
                  </button>
                )}
              </div>
            )}
          </div>
        )}
      </section>
    </div>
  );
}

function Section({ title, aside, children }: { title: string; aside?: ReactNode; children: ReactNode }) {
  return (
    <section className="script-section">
      <div className="insp-label"><span>{title}</span>{aside}</div>
      {children}
    </section>
  );
}

function ParamsEditor({ params, onChange }: { params: ScriptParam[]; onChange: (p: ScriptParam[]) => void }) {
  const set = (i: number, patch: Partial<ScriptParam>) => onChange(params.map((p, j) => (j === i ? { ...p, ...patch } : p)));
  return (
    <ul className="param-list">
      {params.map((p, i) => (
        <li key={i} className="param">
          <div className="param-row">
            <input className="mono" value={p.name} aria-label="Parameter name" placeholder="name"
              onChange={(e) => set(i, { name: e.target.value })} />
            <select value={p.type} aria-label="Type" onChange={(e) => set(i, { type: e.target.value, default: undefined })}>
              {TYPES.map((t) => <option key={t} value={t}>{t}</option>)}
            </select>
            <label className="param-req">
              <input type="checkbox" checked={!!p.required} onChange={(e) => set(i, { required: e.target.checked })} /> Required
            </label>
            <button className="icon-btn" aria-label={`Remove ${p.name}`} onClick={() => onChange(params.filter((_, j) => j !== i))}>
              <Icon name="x" size={14} />
            </button>
          </div>
          <div className="param-row">
            <input value={p.description ?? ""} placeholder="Description (optional)" aria-label="Description"
              onChange={(e) => set(i, { description: e.target.value })} />
            <DefaultInput key={p.type} type={p.type} value={p.default} onChange={(v) => set(i, { default: v })} />
          </div>
        </li>
      ))}
    </ul>
  );
}

/** A parameter's default, edited as text so half-typed JSON isn't lost. */
function DefaultInput({ type, value, onChange }: { type: string; value: unknown; onChange: (v: unknown) => void }) {
  const [raw, setRaw] = useState(() => toRaw(type, value));
  const parsed = fromRaw(type, raw);
  return (
    <span className="default-input">
      <ValueInput type={type} raw={raw} placeholder="Default (optional)" onChange={(r) => {
        setRaw(r);
        const res = fromRaw(type, r);
        if (res.ok) onChange(res.value);
      }} />
      {!parsed.ok && <small className="error">Default {parsed.error}</small>}
    </span>
  );
}

function ScheduleEditor({ script, onChange }: { script: SavedScript; onChange: (p: Partial<SavedScript>) => void }) {
  const sch = script.schedule;
  const params = script.params ?? [];
  const [preview, setPreview] = useState<string[]>([]);
  const [previewError, setPreviewError] = useState<string | null>(null);
  const [args, setArgs] = useState<Record<string, string>>(() =>
    Object.fromEntries(params.map((p) => [p.name, toRaw(p.type, script.scheduleArgs?.[p.name])])));

  // Re-seed the argument fields when another script is opened.
  useEffect(() => {
    setArgs(Object.fromEntries(params.map((p) => [p.name, toRaw(p.type, script.scheduleArgs?.[p.name])])));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [script.id]);

  const key = JSON.stringify(sch ?? null);
  useEffect(() => {
    if (!sch?.enabled) {
      setPreview([]);
      setPreviewError(null);
      return;
    }
    const t = setTimeout(() => {
      Scripts.PreviewSchedule(sch, 3)
        .then((p) => { setPreview(p ?? []); setPreviewError(null); })
        .catch((e) => { setPreview([]); setPreviewError(String(e)); });
    }, 250);
    return () => clearTimeout(t);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key]);

  const set = (patch: Partial<ScriptSchedule>) =>
    onChange({ schedule: { ...(sch ?? { enabled: true, kind: "daily", at: "09:00" }), ...patch } });
  const [argsError, setArgsError] = useState<string | null>(null);
  const setArg = (next: Record<string, string>) => {
    setArgs(next);
    // Required arguments are checked when saving, with the defaults applied.
    const parsed = parseArgs(params.map((p) => ({ ...p, required: false })), next);
    setArgsError(typeof parsed === "string" ? parsed : null);
    if (typeof parsed !== "string") onChange({ scheduleArgs: parsed });
  };
  const every = parseEvery(sch?.every);

  return (
    <Section title="SCHEDULE" aside={
      <label className="switch" title={sch?.enabled ? "Turn the schedule off" : "Run this script on a schedule"}>
        <input type="checkbox" aria-label="Run on a schedule" checked={!!sch?.enabled}
          onChange={(e) => set({ enabled: e.target.checked })} />
        <span />
      </label>}>
      {!sch?.enabled ? (
        <p className="hint">
          {sch ? "Paused. Turn it on to run this script by itself again." : "Off. Turn it on to run this script by itself while Nexora is running, even with the window closed."}
        </p>
      ) : (
        <div className="form">
          <div className="segmented" role="radiogroup" aria-label="Repeat">
            {([["every", "Every…"], ["daily", "Daily"], ["weekly", "Weekly"]] as const).map(([k, label]) => (
              <button key={k} role="radio" aria-checked={sch.kind === k} className={`seg seg-plain ${sch.kind === k ? "on" : ""}`}
                onClick={() => set(k === "every" ? { kind: k, every: sch.every || "1h" } : { kind: k, at: sch.at || "09:00", days: k === "weekly" ? (sch.days?.length ? sch.days : [1]) : [] })}>
                {label}
              </button>
            ))}
          </div>
          {sch.kind === "every" ? (
            <div className="schedule-row">
              <span>Every</span>
              <input type="number" min={1} value={every.n} aria-label="Interval" className="schedule-n"
                onChange={(e) => set({ every: toEvery(Math.max(1, Number(e.target.value) || 1), every.unit) })} />
              <select value={every.unit} aria-label="Unit" onChange={(e) => set({ every: toEvery(every.n, e.target.value as Unit) })}>
                <option value="m">minutes</option>
                <option value="h">hours</option>
                <option value="d">days</option>
              </select>
            </div>
          ) : (
            <div className="schedule-row">
              {sch.kind === "weekly" && (
                <div className="chips" role="group" aria-label="Days">
                  {DAYS.map(([d, label]) => {
                    const on = (sch.days ?? []).includes(d);
                    return (
                      <button key={d} className={`chip ${on ? "on" : ""}`} aria-pressed={on}
                        onClick={() => set({ days: on ? (sch.days ?? []).filter((x) => x !== d) : [...(sch.days ?? []), d] })}>
                        {label}
                      </button>
                    );
                  })}
                </div>
              )}
              <span>at</span>
              <input type="time" value={sch.at ?? "09:00"} aria-label="Time" onChange={(e) => set({ at: e.target.value })} />
            </div>
          )}
          {params.length > 0 && (
            <div className="field">
              Scheduled runs use these arguments
              <ArgsForm params={params} values={args} onChange={setArg} />
              {argsError && <small className="error">{argsError}</small>}
            </div>
          )}
          <label className="param-req">
            <input type="checkbox" checked={!!sch.catchUp} onChange={(e) => set({ catchUp: e.target.checked })} />
            If a run was missed while Nexora was closed, run once when it starts
          </label>
          {previewError ? <p className="error">{previewError}</p> : preview.length > 0 && (
            <p className="hint">Next: {preview.map((p) => new Date(p).toLocaleString([], { weekday: "short", month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" })).join(" · ")}</p>
          )}
          <p className="hint">Tools set to <em>Ask first</em> still ask, so a scheduled run may wait for you. Make the tools it uses trusted to let it run on its own.</p>
        </div>
      )}
    </Section>
  );
}

function ArgsForm({ params, values, onChange }: { params: ScriptParam[]; values: Record<string, string>; onChange: (v: Record<string, string>) => void }) {
  return (
    <div className="args-form">
      {params.map((p) => (
        <label key={p.name} className="field">
          <span className="mono">{p.name}{p.required && <span className="tone-text-pending"> *</span>} <span className="muted">{p.type}</span></span>
          <ValueInput type={p.type} raw={values[p.name] ?? ""} placeholder={p.description}
            onChange={(raw) => onChange({ ...values, [p.name]: raw })} />
          {p.description && <small>{p.description}</small>}
        </label>
      ))}
    </div>
  );
}

/** One argument's input. Values are edited as text and parsed on use. */
function ValueInput({ type, raw, placeholder, onChange }: { type: string; raw: string; placeholder?: string; onChange: (raw: string) => void }) {
  if (type === "boolean") {
    return (
      <select value={raw} onChange={(e) => onChange(e.target.value)}>
        <option value="">(not set)</option>
        <option value="true">true</option>
        <option value="false">false</option>
      </select>
    );
  }
  if (type === "json") {
    return <textarea className="mono json-input" rows={2} value={raw} placeholder={placeholder ?? "JSON"} onChange={(e) => onChange(e.target.value)} />;
  }
  return <input type={type === "number" ? "number" : "text"} value={raw} placeholder={placeholder} onChange={(e) => onChange(e.target.value)} />;
}

function toRaw(type: string, v: unknown): string {
  if (v === undefined || v === null) return "";
  if (type === "string") return String(v);
  if (type === "json") return JSON.stringify(v);
  return String(v);
}

type Parsed = { ok: true; value: unknown } | { ok: false; error: string };

/** Parses a field. An empty field gives value undefined. */
function fromRaw(type: string, raw: string): Parsed {
  if (raw.trim() === "") return { ok: true, value: undefined };
  switch (type) {
    case "number": {
      const n = Number(raw);
      return Number.isFinite(n) ? { ok: true, value: n } : { ok: false, error: "is not a number" };
    }
    case "boolean":
      return { ok: true, value: raw === "true" };
    case "json":
      try {
        return { ok: true, value: JSON.parse(raw) };
      } catch {
        return { ok: false, error: "is not valid JSON" };
      }
    default:
      return { ok: true, value: raw };
  }
}

/** Builds the args object, or returns an error message. */
function parseArgs(params: ScriptParam[], values: Record<string, string>): Record<string, unknown> | string {
  const out: Record<string, unknown> = {};
  for (const p of params) {
    const r = fromRaw(p.type, values[p.name] ?? "");
    if (!r.ok) return `${p.name} ${r.error}`;
    if (r.value === undefined) {
      if (p.required && p.default === undefined) return `${p.name} is required`;
      continue;
    }
    out[p.name] = r.value;
  }
  return out;
}

/** `declare const args: {...}` for the editor heading. */
function argsType(params: ScriptParam[]): string {
  if (params.length === 0) return "args: {}";
  const ts: Record<string, string> = { string: "string", number: "number", boolean: "boolean", json: "any" };
  return `args: { ${params.map((p) => `${p.name}${p.required || p.default !== undefined ? "" : "?"}: ${ts[p.type] ?? "any"}`).join("; ")} }`;
}

function nextName(params: ScriptParam[]): string {
  for (let i = 1; ; i++) {
    const n = i === 1 ? "input" : `input${i}`;
    if (!params.some((p) => p.name === n)) return n;
  }
}

type Unit = "m" | "h" | "d";

function parseEvery(every?: string): { n: number; unit: Unit } {
  const m = /^(\d+)([mh])$/.exec(every ?? "");
  if (!m) return { n: 1, unit: "h" };
  const n = Number(m[1]);
  if (m[2] === "h" && n % 24 === 0) return { n: n / 24, unit: "d" };
  return { n, unit: m[2] as Unit };
}

function toEvery(n: number, unit: Unit): string {
  return unit === "d" ? `${n * 24}h` : `${n}${unit}`;
}

export function describeSchedule(s: ScriptSchedule): string {
  if (s.kind === "every") {
    const { n, unit } = parseEvery(s.every);
    const word = { m: "minute", h: "hour", d: "day" }[unit];
    return n === 1 ? `Every ${word}` : `Every ${n} ${word}s`;
  }
  if (s.kind === "daily") return `Daily at ${s.at}`;
  const days = DAYS.filter(([d]) => (s.days ?? []).includes(d)).map(([, l]) => l);
  return `${days.length === 7 ? "Every day" : days.join(", ")} at ${s.at}`;
}

/** Tab inserts two spaces in a textarea instead of moving focus. */
function indentOnTab(set: (v: string) => void) {
  return (e: React.KeyboardEvent<HTMLTextAreaElement>) => {
    if (e.key !== "Tab" || e.shiftKey) return;
    e.preventDefault();
    const el = e.currentTarget;
    const { selectionStart: a, selectionEnd: b, value } = el;
    set(value.slice(0, a) + "  " + value.slice(b));
    requestAnimationFrame(() => el.setSelectionRange(a + 2, a + 2));
  };
}
