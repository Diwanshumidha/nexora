import { useEffect, useState, type ReactNode } from "react";
import {
  Catalog,
  Choice,
  Connect,
  Decision,
  Extensions,
  History,
  Mode,
  Network,
  Permissions,
  type Approval,
  type CatalogItem,
  type CatalogView,
  type ConnectInfo,
  type ExtensionPermissions,
  type ToolPermission,
  type ExtensionInfo,
  type RunSummary,
  type NetworkEndpoint,
  type QueueStats,
} from "../lib/api";
import { timeAgo, useRun } from "../lib/useLive";
import { Icon, Logo, iconFor, type IconName } from "./icons";
import { RunDetail } from "../components/RunDetail";
import { SettingsForm } from "../components/SettingsForm";
import type { SettingsView } from "../../bindings/nexora/internal/services/models";
import { AuthState, type MCPServerSpec } from "../../bindings/nexora/internal/extension/models";
import type { Selection } from "./OrbitMap";

interface Props {
  selected: Selection;
  onSelect: (s: Selection) => void;
  connect: ConnectInfo | null;
  stats: QueueStats | null;
  network: NetworkEndpoint[];
  extensions: ExtensionInfo[];
  history: RunSummary[];
  approvals: Approval[];
  /** Installed extensions with a newer version in the catalog. */
  updates: CatalogItem[];
  refresh: () => void;
  onOpenRun: (id: string) => void;
}

export function Inspector(props: Props) {
  const { selected, network, extensions } = props;
  let body: ReactNode;
  switch (selected.kind) {
    case "core":
      body = <CorePanel {...props} />;
      break;
    case "network": {
      const n = network.find((x) => x.id === selected.id);
      body = n ? <NetworkPanel key={n.id} n={n} refresh={props.refresh} /> : <Gone />;
      break;
    }
    case "extension": {
      const e = extensions.find((x) => x.manifest?.id === selected.id);
      body = e ? <ExtensionPanel key={selected.id} e={e} {...props} /> : <Gone />;
      break;
    }
    case "history":
      body = <HistoryPanel key={selected.id} id={selected.id} {...props} />;
      break;
    case "approval": {
      const a = props.approvals.find((x) => x.id === selected.id);
      body = a ? <ApprovalPanel key={a.id} a={a} {...props} /> : <Answered {...props} />;
      break;
    }
    case "add":
      body = <AddPanel {...props} />;
      break;
  }
  return (
    <aside className="inspector" aria-label="Inspector">
      {body}
    </aside>
  );
}

function Gone() {
  return <p className="muted">This item no longer exists.</p>;
}

function Head({ icon, title, sub, tone, logo }: { icon?: IconName; title: string; sub?: ReactNode; tone?: string; logo?: boolean }) {
  return (
    <div className="insp-head">
      <span className={`insp-icon ${tone ? `tone-${tone}` : ""}`}>{logo ? <Logo size={30} /> : <Icon name={icon!} size={22} />}</span>
      <div className="insp-title">
        <h2>{title}</h2>
        {sub && <span className="mono muted">{sub}</span>}
      </div>
    </div>
  );
}

function Section({ title, aside, children }: { title: string; aside?: ReactNode; children: ReactNode }) {
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

function Badge({ tone, children }: { tone: string; children: ReactNode }) {
  return <span className={`badge tone-${tone}`}>{children}</span>;
}

function useAction(refresh: () => void) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const run = async (fn: () => Promise<unknown>) => {
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
  return { busy, error, run };
}

/* ---------------------------------------------------------------- core */

function CorePanel({ connect, stats, network, extensions, history, approvals, onSelect }: Props) {
  const live = network.find((n) => n.active);
  const running = extensions.filter((e) => e.status === "running").length;
  return (
    <>
      <div className="insp-kicker">INSPECTING</div>
      <Head logo title="Nexora core" sub="the AI's way into your machine" tone="ext" />
      <p className="insp-desc">
        AIs connect here over MCP. They read the typed API of your extensions and send scripts that run in a sandbox
        with access to nothing but <code>tools.*</code>.
      </p>

      <div className="stat-grid">
        <Stat label="Extensions" value={`${running}/${extensions.length}`} />
        <Stat label="Running" value={stats ? `${stats.running}/${stats.workers}` : "–"} />
        <Stat label="Queued" value={stats ? String(stats.queued) : "–"} />
        <Stat label="Runs" value={String(history.length)} />
      </div>

      {approvals.length > 0 && (
        <Section title="WAITING FOR YOU">
          <ul className="rows">
            {approvals.map((a) => (
              <li key={a.id}>
                <button className="row row-button" onClick={() => onSelect({ kind: "approval", id: a.id })}>
                  <StateDot state="queued" />
                  <span className="mono">{a.namespace}.{a.tool}()</span>
                  <span className="row-desc">{a.source === "mcp" ? "AI" : "You"}</span>
                </button>
              </li>
            ))}
          </ul>
        </Section>
      )}

      <Section title="ON THIS MACHINE">
        {connect?.enabled ? (
          <>
            <Secret url={connect.url} masked={connect.baseUrl + "/••••••••"} />
            <p className="hint">
              Streamable HTTP. Or use <code>{connect.baseUrl}</code> with <code>Authorization: Bearer &lt;token&gt;</code>.
            </p>
          </>
        ) : (
          <p className="hint">The MCP endpoint is disabled (NEXORA_MCP_ADDR=off or the port is in use).</p>
        )}
      </Section>

      <Section title="REMOTE ACCESS" aside={live && <Badge tone={live.state === "up" ? "live" : "pending"}>{live.name}</Badge>}>
        {live?.mcpUrl ? (
          <>
            <Secret url={live.mcpUrl} masked={(live.exposure?.url ?? "") + "/••••••••"} />
            <p className="hint">{live.exposure?.message}</p>
          </>
        ) : (
          <p className="hint">
            Hosted AIs like ChatGPT need a public URL. Pick a provider on the inner <span className="tone-text-net">network</span> orbit.
          </p>
        )}
      </Section>

      <SecuritySection />

      <Section title="LONG RUNS">
        <p className="hint">
          Calls wait up to 80 s for a result (under the 100 s proxy limit). Longer scripts return a job ID the AI polls with
          <code> get_job</code>; they appear on the history orbit as they run.
        </p>
      </Section>
    </>
  );
}

function SecuritySection() {
  const [strict, setStrict] = useState<boolean | null>(null);
  const [stats, setStats] = useState<{ rejected: number; originsBlocked: number; last?: { at: string; reason: string } | null } | null>(null);
  const [confirmRotate, setConfirmRotate] = useState(false);
  const [rotated, setRotated] = useState(false);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    Permissions.Strict().then(setStrict);
    const load = () => Connect.Security().then(setStats).catch(() => {});
    load();
    const t = setInterval(load, 5000);
    return () => clearInterval(t);
  }, []);

  const toggleStrict = async () => {
    setError(null);
    try {
      await Permissions.SetStrict(!strict);
      setStrict(!strict);
    } catch (e) {
      setError(String(e));
    }
  };
  const rotate = async () => {
    setError(null);
    try {
      await Connect.RotateToken();
      setConfirmRotate(false);
      setRotated(true);
      window.location.reload(); // every URL in the UI changes
    } catch (e) {
      setError(String(e));
    }
  };

  return (
    <Section title="SECURITY">
      <div className="setting setting-inline sec-row">
        <div className="setting-text">
          <span className="setting-head">Ask before every tool call</span>
          <small className="setting-help">Strict mode: even read-only and trusted tools wait for your OK. Blocked tools stay blocked.</small>
        </div>
        <label className="switch">
          <input type="checkbox" aria-label="Strict mode" checked={!!strict} disabled={strict === null} onChange={toggleStrict} />
          <span aria-hidden="true" />
        </label>
      </div>

      <div className="sec-row">
        <div className="setting-text">
          <span className="setting-head">Refused requests</span>
          <small className="setting-help">
            {stats && stats.rejected > 0
              ? `${stats.rejected} since start${stats.originsBlocked ? ` (${stats.originsBlocked} from web pages)` : ""}. Last: ${stats.last?.reason} ${stats.last ? timeAgo(stats.last.at) + " ago" : ""}`
              : "None since start. Wrong tokens are throttled; requests from web pages are blocked."}
          </small>
        </div>
      </div>

      <div className="sec-row">
        <div className="setting-text">
          <span className="setting-head">Secret token</span>
          <small className="setting-help">Rotate it if a URL leaked. Every AI connection, local and public, must be updated with the new URL.</small>
        </div>
        {confirmRotate ? (
          <div className="sec-confirm">
            <button className="btn danger" onClick={rotate}>Rotate now</button>
            <button className="btn ghost" onClick={() => setConfirmRotate(false)}>Cancel</button>
          </div>
        ) : (
          <button className="btn ghost" onClick={() => setConfirmRotate(true)}>{rotated ? "Rotated" : "Rotate"}</button>
        )}
      </div>
      {error && <p className="error">{error}</p>}
    </Section>
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

/* ------------------------------------------------------------- network */

function NetworkPanel({ n, refresh }: { n: NetworkEndpoint; refresh: () => void }) {
  const [opts, setOpts] = useState<Record<string, string>>({});
  useEffect(() => {
    setOpts(Object.fromEntries(Object.entries(n.options ?? {}).map(([k, v]) => [k, v ?? ""])));
  }, [n.id, JSON.stringify(n.options)]); // eslint-disable-line react-hooks/exhaustive-deps
  const { busy, error, run } = useAction(refresh);

  const dirty = n.active && Object.entries(opts).some(([k, v]) => (n.options ?? {})[k] !== v);
  const tone = n.active ? (n.state === "up" ? "live" : n.state === "down" ? "error" : "pending") : "net";

  return (
    <>
      <div className="insp-kicker">NETWORK PROVIDER</div>
      <Head icon={n.id === "tailscale" ? "tailscale" : n.id === "cloudflare" ? "cloud" : "shield"} title={n.name}
        sub={n.active ? `live · ${n.state}` : "off"} tone={tone} />
      <p className="insp-desc">{n.description}</p>
      {!n.available && <p className="error">{n.unavailable}</p>}

      {(n.settings ?? []).length > 0 && (
        <Section title="SETTINGS">
          <div className="form">
            {(n.settings ?? []).map((s) => (
              <label key={s.key} className="field">
                <span>{s.label}</span>
                {s.choices?.length ? (
                  <select value={opts[s.key] ?? s.default} onChange={(e) => setOpts({ ...opts, [s.key]: e.target.value })}>
                    {s.choices.map((c) => (
                      <option key={c}>{c}</option>
                    ))}
                  </select>
                ) : (
                  <input value={opts[s.key] ?? s.default} onChange={(e) => setOpts({ ...opts, [s.key]: e.target.value })} />
                )}
                {s.help && <small>{s.help}</small>}
              </label>
            ))}
          </div>
        </Section>
      )}

      {n.active && n.error && <p className="error">{n.error}</p>}
      {n.active && n.mcpUrl && (
        <Section title="PUBLIC MCP URL" aside={<Badge tone={n.exposure?.public ? "live" : "net"}>{n.exposure?.public ? "PUBLIC" : "TAILNET"}</Badge>}>
          <Secret url={n.mcpUrl} masked={(n.exposure?.url ?? "") + "/••••••••"} />
          <p className="hint">{n.exposure?.message} Anyone with this URL can run code against your extensions.</p>
        </Section>
      )}

      {error && <p className="error">{error}</p>}
      <div className="insp-actions">
        <button className="btn primary" disabled={busy || !n.available || (n.active && !dirty)}
          onClick={() => run(() => Network.Activate(n.id, opts))}>
          {busy ? "Working…" : n.active ? (dirty ? "Apply" : "Live") : "Make live"}
        </button>
        {n.active && (
          <button className="btn ghost" disabled={busy} onClick={() => run(() => Network.Deactivate())}>
            Turn off
          </button>
        )}
      </div>
      {!n.active && <p className="hint">Only one provider can be live. Making this live turns the other off.</p>}
    </>
  );
}

/* ----------------------------------------------------------- extension */

function ExtensionPanel({ e, history, updates, refresh, onSelect }: Props & { e: ExtensionInfo }) {
  const m = e.manifest!;
  const update = updates.find((u) => u.id === m.id);
  const ns = m.namespace ?? m.id;
  const { busy, error, run } = useAction(refresh);
  const [confirmRemove, setConfirmRemove] = useState(false);
  const [settings, setSettings] = useState<SettingsView | null>(null);
  // Folders the user granted through folder settings, for the sandbox list.
  const granted: [string, string][] = [];
  for (const spec of settings?.specs ?? []) {
    if (!spec.grant) continue;
    const v = settings?.values?.[spec.key];
    const dirs = Array.isArray(v) ? (v as string[]) : v ? [v as string] : [];
    for (const d of dirs) granted.push([spec.grant === "readwrite" ? "edit" : "read", d]);
  }
  const needsFolder = (settings?.specs ?? []).some((spec) => {
    const v = settings?.values?.[spec.key];
    return spec.grant && (Array.isArray(v) ? v.length === 0 : !v);
  });
  const runs = history.filter((h) => (h.tools ?? []).some((t) => t.startsWith(ns + ".")));
  const p = m.permissions;
  const perms: [string, string][] = [
    ...(p?.fsRead ?? []).map((x): [string, string] => ["read", x]),
    ...(p?.fsWrite ?? []).map((x): [string, string] => ["write", x]),
    ...(p?.network ? [["network", "outbound"] as [string, string]] : []),
    ...(p?.childProcess ? [["process", "can spawn programs"] as [string, string]] : []),
  ];

  return (
    <>
      <div className="insp-kicker">EXTENSION</div>
      <Head icon={iconFor(m.icon)} title={m.name} sub={`tools.${ns} · v${m.version}`}
        tone={e.status === "running" ? "ext" : e.status === "error" ? "error" : "dim"} />
      <p className="insp-desc">{m.description}</p>
      {e.error && e.auth !== AuthState.AuthRequired && <p className="error">{e.error}</p>}
      {e.auth === AuthState.AuthRequired && (
        <div className="notice">
          <span>{e.error || `${m.name} needs you to sign in before its tools can be used.`}</span>
          <button className="btn primary" disabled={busy} onClick={() => run(() => Extensions.SignIn(m.id))}>Sign in</button>
        </div>
      )}
      {e.auth === AuthState.AuthPending && (
        <div className="notice">
          <span>Finish signing in in your browser. Nexora connects as soon as you approve access.</span>
          <button className="btn ghost" onClick={() => Extensions.CancelSignIn(m.id).finally(refresh)}>Cancel</button>
        </div>
      )}
      {update && (
        <div className="notice">
          <span>Version {update.version} is available (you have {update.installedVersion}). Updating keeps its settings and data.</span>
          <button className="btn primary" disabled={busy} onClick={() => run(() => Catalog.Update(m.id))}>
            {busy ? "Updating…" : "Update"}
          </button>
        </div>
      )}
      {e.stale && (
        <div className="notice">
          <span>A newer build of this extension is on disk. Restart it to load the changes.</span>
          <button className="btn primary" disabled={busy} onClick={() => run(() => Extensions.Restart(m.id))}>Restart</button>
        </div>
      )}

      {needsFolder && (
        <div className="notice">
          <span>{m.name} has no folder access yet. Add a folder below so it can work with your files.</span>
        </div>
      )}

      <SettingsForm key={m.id + (e.stale ? "-stale" : "")} extId={m.id} extName={m.name}
        onSaved={(v) => { setSettings(v); refresh(); }} />

      <TrustSection extId={m.id} runsCount={runs.length} refresh={refresh} />

      {m.runtime === "mcp" ? (
        <MCPConnection m={m} auth={e.auth} busy={busy} onSignOut={() => run(() => Extensions.SignOut(m.id))} />
      ) : <Section title="SANDBOX ACCESS">
        {granted.map(([kind, dir]) => (
          <div key={kind + dir} className="row">
            <Badge tone={kind === "edit" ? "pending" : "net"}>{kind === "edit" ? "READ+EDIT" : "READ"}</Badge>
            <span className="mono row-desc" title={dir}>{dir}</span>
          </div>
        ))}
        {perms.length === 0 && granted.length === 0 ? (
          <p className="hint">Its own code and private data folder only.</p>
        ) : (
          <ul className="rows">
            {perms.map(([kind, what]) => (
              <li key={kind + what} className="row">
                <Badge tone={kind === "write" || kind === "process" ? "pending" : "net"}>{kind.toUpperCase()}</Badge>
                <span className="mono row-desc">{what}</span>
              </li>
            ))}
          </ul>
        )}
      </Section>}

      {runs.length > 0 && (
        <Section title="RECENT USE">
          <ul className="rows">
            {runs.slice(0, 4).map((h) => (
              <li key={h.id}>
                <button className="row row-button" onClick={() => onSelect({ kind: "history", id: h.id })}>
                  <StateDot state={h.state} />
                  <span>{h.source === "mcp" ? "AI" : "You"}</span>
                  <span className="mono row-desc">{(h.tools ?? []).filter((t) => t.startsWith(ns + ".")).join(", ")}</span>
                  <span className="muted">{timeAgo(h.createdAt)}</span>
                </button>
              </li>
            ))}
          </ul>
        </Section>
      )}

      {error && <p className="error">{error}</p>}
      <div className="insp-actions">
        <button className="btn ghost" disabled={busy} onClick={() => run(() => Extensions.Restart(m.id))}>
          <Icon name="refresh" size={16} /> Restart
        </button>
        {confirmRemove ? (
          <button className="btn danger" disabled={busy}
            onClick={() => run(async () => { await Extensions.Remove(m.id); onSelect({ kind: "core" }); })}>
            Confirm remove
          </button>
        ) : (
          <button className="btn ghost" onClick={() => setConfirmRemove(true)}>
            <Icon name="trash" size={16} /> Remove
          </button>
        )}
      </div>
      <p className="hint mono">{e.dir}</p>
    </>
  );
}

/* --------------------------------------------------------- permissions */

const MODES: { mode: Mode; label: string; help: string }[] = [
  { mode: Mode.ModeTrusted, label: "Trusted", help: "Every tool runs without asking." },
  { mode: Mode.ModeAsk, label: "Ask first", help: "Read-only tools run; anything that changes something waits for your OK." },
  { mode: Mode.ModeReadOnly, label: "Read only", help: "Read-only tools run; everything else is blocked." },
];

const DECISION_LABEL: Record<string, string> = { allow: "Runs", ask: "Asks you", deny: "Blocked" };

function TrustSection({ extId, runsCount, refresh }: { extId: string; runsCount: number; refresh: () => void }) {
  const [perms, setPerms] = useState<ExtensionPermissions | null>(null);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    Permissions.Get(extId).then(setPerms).catch((e) => setError(String(e)));
  }, [extId]);

  const apply = async (fn: () => Promise<ExtensionPermissions>) => {
    setError(null);
    try {
      setPerms(await fn());
      refresh();
    } catch (e) {
      setError(String(e));
    }
  };
  if (!perms) return error ? <p className="error">{error}</p> : null;
  const current = MODES.find((m) => m.mode === perms.mode);

  return (
    <>
      <Section title="TRUST">
        <div role="radiogroup" aria-label="Trust level" className="segmented">
          {MODES.map((m) => (
            <button key={m.mode} role="radio" aria-checked={perms.mode === m.mode}
              className={`seg seg-${m.mode} ${perms.mode === m.mode ? "on" : ""}`}
              onClick={() => apply(() => Permissions.SetMode(extId, m.mode))}>
              {m.label}
            </button>
          ))}
        </div>
        <p className="hint">{current?.help}</p>
      </Section>

      <Section title="TOOLS" aside={<span className="muted">{runsCount} recent runs</span>}>
        {(perms.tools ?? []).length === 0 && <p className="hint">Start the extension to see its tools.</p>}
        <ul className="tool-list">
          {(perms.tools ?? []).map((t) => (
            <ToolRow key={t.name} t={t} mode={perms.mode}
              onSet={(d) => apply(() => Permissions.SetTool(extId, t.name, d))} />
          ))}
        </ul>
        {error && <p className="error">{error}</p>}
      </Section>
    </>
  );
}

const CHOICES: { d: Decision; label: string; icon: IconName; help: string }[] = [
  { d: Decision.Allow, label: "Allow", icon: "check", help: "Runs without asking" },
  { d: Decision.Ask, label: "Ask", icon: "hand", help: "Waits for your approval each time" },
  { d: Decision.Deny, label: "Block", icon: "ban", help: "Never runs; hidden from the AI" },
];

function ToolRow({ t, mode, onSet }: { t: ToolPermission; mode: Mode; onSet: (d: Decision) => void }) {
  const fallback = MODE_DEFAULT(mode, t.readOnly);
  const custom = !!t.override;
  const modeName = MODES.find((m) => m.mode === mode)?.label ?? mode;
  return (
    <li className="tool">
      <div className="tool-head">
        <span className="mono tool-name">{t.name}()</span>
        <Badge tone={t.readOnly ? "net" : "pending"}>{t.readOnly ? "READ" : "WRITE"}</Badge>
      </div>
      {t.description && <p className="tool-desc">{t.description}</p>}
      <div className="tool-perm">
        <div role="radiogroup" aria-label={`Permission for ${t.name}`} className="tri">
          {CHOICES.map((c) => {
            const on = t.effective === c.d;
            return (
              <button key={c.d} role="radio" aria-checked={on} title={c.help}
                className={`tri-opt tri-${c.d} ${on ? (custom ? "on custom" : "on") : ""}`}
                onClick={() => onSet(c.d)}>
                <Icon name={c.icon} size={14} stroke={2} />
                {c.label}
              </button>
            );
          })}
        </div>
        {custom ? (
          <button className="link tool-reset" onClick={() => onSet(Decision.$zero)}
            title={`Follow ${modeName} again (${DECISION_LABEL[fallback].toLowerCase()})`}>
            Custom · reset
          </button>
        ) : (
          <span className="tool-source">From {modeName}</span>
        )}
      </div>
    </li>
  );
}

/** Mirrors permissions.Mode.Default in Go. */
function MODE_DEFAULT(mode: Mode, readOnly: boolean): string {
  if (mode === Mode.ModeTrusted || readOnly) return "allow";
  return mode === Mode.ModeReadOnly ? "deny" : "ask";
}

function ApprovalPanel({ a, approvals, history, onSelect, refresh }: Props & { a: Approval }) {
  const { busy, error, run } = useAction(refresh);
  const [now, setNow] = useState(Date.now());
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(t);
  }, []);
  const left = Math.max(0, Math.round((new Date(a.expiresAt).getTime() - now) / 1000));
  const job = useRun(a.jobId ?? null, "");

  const answer = (c: Choice) =>
    run(async () => {
      await Permissions.Resolve(a.id, c);
      const next = approvals.find((x) => x.id !== a.id);
      onSelect(next ? { kind: "approval", id: next.id } : a.jobId ? { kind: "history", id: a.jobId } : { kind: "core" });
    });

  return (
    <>
      <div className="insp-kicker">APPROVAL NEEDED</div>
      <Head icon={a.source === "mcp" ? "ai" : "user"} title={`${a.namespace}.${a.tool}()`}
        sub={`${a.source === "mcp" ? "an AI via MCP" : "a script you ran"} · ${left}s left`} tone="pending" />
      <p className="insp-desc">
        {a.description || "This tool may change something."} The script is paused until you decide.
      </p>

      <Section title="ARGUMENTS">
        <pre className="code">{JSON.stringify(a.args ?? {}, null, 2)}</pre>
      </Section>

      {job && (
        <Section title="FROM THIS SCRIPT">
          <pre className="code">{job.code}</pre>
        </Section>
      )}

      {error && <p className="error">{error}</p>}
      <div className="approval-actions">
        <button className="btn primary" disabled={busy} onClick={() => answer(Choice.AllowOnce)}>Allow once</button>
        <button className="btn ghost" disabled={busy} onClick={() => answer(Choice.AllowRun)}>Allow for this run</button>
        <button className="btn ghost" disabled={busy} onClick={() => answer(Choice.AllowAlways)}>Always allow {a.tool}</button>
        <button className="btn danger" disabled={busy} onClick={() => answer(Choice.DenyOnce)}>Deny</button>
        <button className="btn ghost" disabled={busy} onClick={() => answer(Choice.DenyAlways)}>Always block {a.tool}</button>
        <span />
      </div>
      <p className="hint">"Always" choices change this tool's permission; you can undo them on the extension.</p>
      <button className="link" onClick={() => onSelect({ kind: "extension", id: a.extensionId })}>
        Open {a.extensionId} permissions
      </button>
    </>
  );
}

function Answered({ approvals, onSelect }: Props) {
  return (
    <>
      <div className="insp-kicker">APPROVAL</div>
      <p className="hint">This request was answered, expired, or its script stopped.</p>
      {approvals[0] && (
        <button className="btn primary" onClick={() => onSelect({ kind: "approval", id: approvals[0].id })}>
          Next request ({approvals.length})
        </button>
      )}
    </>
  );
}

/* ------------------------------------------------------------- history */

function HistoryPanel({ id, history, extensions, approvals, refresh, onSelect, onOpenRun }: Props & { id: string }) {
  const sum = history.find((h) => h.id === id);
  // The full record is fetched once, and again only when the run changes.
  const rec = useRun(id, `${sum?.state}|${sum?.finishedAt}`);
  const { busy, error, run } = useAction(refresh);
  if (!rec) return <Gone />;

  const live = rec.state === "queued" || rec.state === "running";
  const nsToId = new Map(extensions.map((e) => [e.manifest!.namespace ?? e.manifest!.id, e.manifest!.id]));

  return (
    <>
      <div className="insp-kicker">RUN</div>
      <RunDetail
        rec={rec}
        waiting={approvals.filter((a) => a.jobId === rec.id)}
        onReview={(aid) => onSelect({ kind: "approval", id: aid })}
        onOpenExtension={(ns) => { const ext = nsToId.get(ns); if (ext) onSelect({ kind: "extension", id: ext }); }}
        actions={
          <>
            {live ? (
              <button className="btn danger" disabled={busy} onClick={() => run(() => History.Cancel(rec.id))}>Cancel run</button>
            ) : (
              <button className="btn primary" disabled={busy}
                onClick={() => run(async () => { const j = await History.Rerun(rec.id); onSelect({ kind: "history", id: j.id }); })}>
                <Icon name="refresh" size={16} /> Run again
              </button>
            )}
            <button className="btn ghost" onClick={() => onOpenRun(rec.id)}>
              <Icon name="history" size={16} /> Full details
            </button>
          </>
        }
      />
      {error && <p className="error">{error}</p>}
    </>
  );
}

export function StateDot({ state }: { state: string }) {
  return <span className={`state-dot state-${state}`} aria-label={state} role="img" />;
}

/** How an MCP-server extension is reached, and what it can touch. */
function MCPConnection({ m, auth, busy, onSignOut }: {
  m: NonNullable<ExtensionInfo["manifest"]>;
  auth?: AuthState;
  busy: boolean;
  onSignOut: () => void;
}) {
  const staticAuth = Object.keys(m.headers ?? {}).some((h) => h.toLowerCase() === "authorization");
  const command = [m.command, ...(m.args ?? [])].map((a) => (/\s/.test(a ?? "") ? JSON.stringify(a) : a)).join(" ");
  return (
    <Section title="MCP SERVER">
      <ul className="rows">
        <li className="row">
          <Badge tone={m.url ? "net" : "pending"}>{m.url ? "REMOTE" : "LOCAL"}</Badge>
          <span className="mono row-desc" title={m.url || command}>{m.url || command}</span>
        </li>
        {auth === AuthState.AuthSignedIn && (
          <li className="row">
            <Badge tone="live">SIGNED IN</Badge>
            <span className="row-desc">OAuth, refreshed automatically</span>
            <button className="btn ghost" disabled={busy} onClick={onSignOut}>Sign out</button>
          </li>
        )}
      </ul>
      {m.url && !staticAuth && auth !== AuthState.AuthSignedIn && (
        <p className="hint">If the server requires an account, you sign in through your browser. Nexora keeps the token
          private and never opens the browser on its own.</p>
      )}
      {m.url ? (
        <p className="hint">Tool calls and their arguments are sent to this server.</p>
      ) : (
        <p className="hint">
          Not sandboxed: the server runs as a program with your user's access to files and the network. It gets only a
          minimal environment plus the variables above. Only add servers you trust.
        </p>
      )}
      <p className="hint">Changing a setting restarts the server.</p>
    </Section>
  );
}

/* ----------------------------------------------------------------- add */

function AddPanel({ refresh, onSelect }: Props) {
  const [view, setView] = useState<CatalogView | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const [working, setWorking] = useState<string | null>(null);
  const [done, setDone] = useState<string | null>(null);
  const [query, setQuery] = useState("");
  const [dir, setDir] = useState("");
  const { busy, error, run } = useAction(refresh);

  // force skips the saved copy (the Refresh button); otherwise the backend
  // answers from it if it's recent, so the panel opens instantly.
  const load = (force = false) => {
    setLoading(true);
    setLoadError(null);
    Catalog.List(force)
      .then(setView)
      .catch((e) => setLoadError(String(e)))
      .finally(() => setLoading(false));
  };
  useEffect(() => load(), []);

  const opened = (info: ExtensionInfo | null | undefined) => {
    if (info?.manifest) onSelect({ kind: "extension", id: info.manifest.id });
  };
  const act = (key: string, fn: () => Promise<void>) => {
    setWorking(key);
    setDone(null);
    run(async () => {
      try {
        await fn();
      } finally {
        setWorking(null);
        load();
      }
    });
  };
  const install = (item: CatalogItem, update: boolean) =>
    act(item.id, async () => opened(await (update ? Catalog.Update(item.id) : Catalog.Install(item.id))));
  const updateAll = () =>
    act("*", async () => {
      const res = (await Catalog.UpdateAll()) ?? [];
      const failed = res.filter((r) => r.error);
      if (failed.length) throw new Error(failed.map((r) => `${r.name}: ${r.error}`).join("\n"));
      setDone(`Updated ${res.map((r) => `${r.name} to ${r.version}`).join(", ")}.`);
    });
  const installFile = () =>
    run(async () => {
      const path = await Catalog.PickPackage();
      if (!path) return;
      setWorking("file");
      try {
        opened(await Catalog.InstallFile(path));
      } finally {
        setWorking(null);
        load();
      }
    });

  const q = query.trim().toLowerCase();
  const items = (view?.items ?? []).filter(
    (i) => !q || [i.name, i.id, i.description].some((t) => t?.toLowerCase().includes(q)),
  );
  const updates = items.filter((i) => i.state === "update");
  const available = items.filter((i) => i.state === "available" || i.state === "incompatible");
  const installed = items.filter((i) => i.state === "installed" || i.state === "dev");
  const list = (group: CatalogItem[]) => (
    <ul className="catalog">
      {group.map((item) => (
        <li key={item.id} className={`catalog-item ${item.state}`}>
          <span className="catalog-icon"><Icon name={iconFor(item.icon)} size={18} /></span>
          <div className="catalog-body">
            <div className="catalog-title">
              <strong>{item.name}</strong>
              <span className="catalog-version">
                {item.state === "update" ? `${item.installedVersion} → ${item.version}` : item.version}
              </span>
            </div>
            <p>{item.description}</p>
            <p className="catalog-perms">{permSummary(item)}</p>
          </div>
          <CatalogAction item={item} working={working === item.id || (working === "*" && item.state === "update")}
            disabled={busy} onInstall={() => install(item, false)} onUpdate={() => install(item, true)}
            onOpen={() => onSelect({ kind: "extension", id: item.id })} />
        </li>
      ))}
    </ul>
  );

  const checked = view?.fetchedAt && !view.fetchedAt.startsWith("0001") ? timeAgo(view.fetchedAt) : "";
  return (
    <>
      <div className="insp-kicker">DOCK AN EXTENSION</div>
      <Head icon="plus" title="Add extension" tone="ext" />
      <p className="insp-desc">
        Each extension runs in its own sandboxed process and only gets the permissions listed here. Downloads are
        checked against the catalog's checksum before anything is installed.
      </p>

      {view && !view.configured && (
        <p className="hint">No extension catalog is configured. Set <code>NEXORA_REGISTRY_URL</code> to a catalog's <code>index.json</code>, or install a package file or folder below.</p>
      )}
      {loadError && (
        <div className="notice">
          <span>Couldn't load the catalog: {loadError}</span>
          <button className="btn" onClick={() => load(true)}>Retry</button>
        </div>
      )}
      {view?.offline && (
        <div className="notice">
          <span>Can't reach the catalog right now, so this is the copy from {checked === "now" ? "just now" : `${checked} ago`}. Installing needs a connection.</span>
          <button className="btn" disabled={loading} onClick={() => load(true)}>Retry</button>
        </div>
      )}
      {!view && !loadError && <p className="hint">Loading catalog…</p>}
      {error && <p className="error">{error}</p>}
      {done && <p className="ok-note"><Icon name="check" size={14} /> {done}</p>}

      {view?.configured && (view.items?.length ?? 0) > 0 && (
        <div className="catalog-bar">
          <label className="search">
            <Icon name="search" size={16} />
            <span className="sr-only">Search extensions</span>
            <input value={query} onChange={(e) => setQuery(e.target.value)} placeholder="Search extensions" />
          </label>
          <button className="icon-btn" aria-label="Check the catalog again" title={checked ? `Checked ${checked === "now" ? "just now" : checked + " ago"}` : "Refresh"}
            disabled={loading} onClick={() => load(true)}>
            <span className={loading ? "spin" : ""}><Icon name="refresh" size={16} /></span>
          </button>
        </div>
      )}

      {updates.length > 0 && (
        <Section title={`UPDATES · ${updates.length}`} aside={updates.length > 1 && (
          <button className="btn primary" disabled={busy} onClick={updateAll}>{working === "*" ? "Updating…" : "Update all"}</button>
        )}>
          {list(updates)}
        </Section>
      )}
      {available.length > 0 && <Section title="AVAILABLE">{list(available)}</Section>}
      {installed.length > 0 && <Section title="INSTALLED">{list(installed)}</Section>}
      {q && items.length === 0 && <p className="hint">No extensions match “{query.trim()}”.</p>}

      <Section title="MORE WAYS TO ADD">
        <div className="add-ways">
          <button className="btn" disabled={busy} onClick={installFile}>
            <Icon name="download" size={16} /> {working === "file" ? "Installing…" : "Install a package file…"}
          </button>
          <p className="hint">A <code>.nxext</code> or <code>.zip</code> extension package. Installing a newer version of an installed extension updates it and keeps its settings.</p>
        </div>
        <details className="catalog-folder">
          <summary>Add an MCP server</summary>
          <MCPForm busy={busy} onAdd={(spec) => run(async () => opened(await Extensions.AddMCPServer(spec)))} />
        </details>
        <details className="catalog-folder">
          <summary>Install from a folder</summary>
          <form className="form"
            onSubmit={(ev) => {
              ev.preventDefault();
              if (dir) run(async () => opened(await Extensions.Install(dir.trim())));
            }}>
            <label className="field">
              <span>Extension folder (with nexora.json and a built dist/)</span>
              <div className="input-row">
                <input value={dir} onChange={(e) => setDir(e.target.value)} placeholder="/home/you/my-extension" />
                <button type="button" className="btn ghost" onClick={async () => {
                  const picked = await Extensions.PickFolder("Choose an extension folder");
                  if (picked) setDir(picked);
                }}>Browse…</button>
              </div>
            </label>
            <div className="insp-actions">
              <button className="btn" type="submit" disabled={busy || !dir}>Install folder</button>
            </div>
          </form>
        </details>
      </Section>
      <p className="hint">Write your own with the SDK in the nexora-extensions repo.</p>
    </>
  );
}

/** Parses "KEY=value" (env) or "Name: value" (headers) lines. */
function parsePairs(text: string, sep: "=" | ":"): Record<string, string> {
  const out: Record<string, string> = {};
  for (const line of text.split("\n")) {
    if (!line.trim()) continue;
    const i = line.indexOf(sep);
    const key = (i < 0 ? line : line.slice(0, i)).trim();
    if (key) out[key] = i < 0 ? "" : line.slice(i + 1).trim();
  }
  return out;
}

function MCPForm({ busy, onAdd }: { busy: boolean; onAdd: (spec: MCPServerSpec) => void }) {
  const [remote, setRemote] = useState(false);
  const [name, setName] = useState("");
  const [command, setCommand] = useState("");
  const [url, setUrl] = useState("");
  const [pairs, setPairs] = useState("");
  const target = remote ? url : command;
  return (
    <form className="form"
      onSubmit={(ev) => {
        ev.preventDefault();
        onAdd(remote
          ? { name, url: url.trim(), headers: parsePairs(pairs, ":") }
          : { name, commandLine: command.trim(), env: parsePairs(pairs, "=") });
      }}>
      <p className="hint">
        Wrap any MCP server as an extension: its tools appear under <code>tools.&lt;name&gt;</code> with the same
        permission prompts as other extensions.
      </p>
      <div className="segmented two" role="radiogroup" aria-label="Server type">
        <button type="button" role="radio" aria-checked={!remote} className={`seg ${!remote ? "on seg-trusted" : ""}`}
          onClick={() => setRemote(false)}>Local command</button>
        <button type="button" role="radio" aria-checked={remote} className={`seg ${remote ? "on seg-trusted" : ""}`}
          onClick={() => setRemote(true)}>Remote URL</button>
      </div>
      <label className="field">
        <span>Name</span>
        <input value={name} onChange={(e) => setName(e.target.value)} placeholder="GitHub" required />
      </label>
      {remote ? (
        <label className="field">
          <span>Server URL (streamable HTTP)</span>
          <input value={url} onChange={(e) => setUrl(e.target.value)} placeholder="https://example.com/mcp" required />
        </label>
      ) : (
        <label className="field">
          <span>Command</span>
          <input className="mono" value={command} onChange={(e) => setCommand(e.target.value)}
            placeholder="npx -y @modelcontextprotocol/server-github" required />
          <small>Runs unsandboxed with your user's access. Only add servers you trust.</small>
        </label>
      )}
      <label className="field">
        <span>{remote ? "Headers (optional, one per line)" : "Environment variables (optional, one per line)"}</span>
        <textarea className="mono" rows={3} value={pairs} onChange={(e) => setPairs(e.target.value)}
          placeholder={remote ? "Authorization: Bearer …" : "GITHUB_PERSONAL_ACCESS_TOKEN=ghp_…"} />
        <small>Saved as settings of the extension. Tokens, keys and passwords are stored as secrets and can be changed later.</small>
      </label>
      <div className="insp-actions">
        <button className="btn primary" type="submit" disabled={busy || !name.trim() || !target.trim()}>Add server</button>
      </div>
    </form>
  );
}

function CatalogAction({ item, working, disabled, onInstall, onUpdate, onOpen }: {
  item: CatalogItem; working: boolean; disabled: boolean;
  onInstall: () => void; onUpdate: () => void; onOpen: () => void;
}) {
  switch (item.state) {
    case "available":
      return <button className="btn primary" disabled={disabled} onClick={onInstall}>{working ? "Installing…" : "Install"}</button>;
    case "update":
      return <button className="btn primary" disabled={disabled} onClick={onUpdate}>{working ? "Updating…" : "Update"}</button>;
    case "installed":
      return <button className="btn" onClick={onOpen}>Open</button>;
    case "dev":
      return <span className="badge tone-net" title="Loaded from a development folder; rebuild it there">dev</span>;
    default:
      return <span className="badge tone-error" title="Needs a newer version of Nexora">needs update</span>;
  }
}

/** What an extension may touch, in plain words. */
function permSummary(item: CatalogItem): string {
  const p = item.permissions;
  const parts: string[] = [];
  if (p?.network) parts.push("internet");
  if (p?.childProcess) parts.push("runs programs");
  if (p?.fsWrite?.length) parts.push("writes files");
  else if (p?.fsRead?.length) parts.push("reads files");
  const size = item.size > 1 << 20 ? `${(item.size / (1 << 20)).toFixed(1)} MB` : `${Math.max(1, Math.round(item.size / 1024))} KB`;
  return `${parts.length ? parts.join(" · ") : "sandboxed, no extra access"} · ${size}`;
}

/* -------------------------------------------------------------- shared */

function Secret({ url, masked }: { url: string; masked: string }) {
  const [shown, setShown] = useState(false);
  const [copied, setCopied] = useState(false);
  return (
    <div className="secret">
      <code>{shown ? url : masked}</code>
      <button className="icon-btn" aria-label={shown ? "Hide URL" : "Reveal URL"} onClick={() => setShown(!shown)}>
        <Icon name="eye" size={16} />
      </button>
      <button className="icon-btn" aria-label="Copy URL"
        onClick={async () => { await navigator.clipboard.writeText(url); setCopied(true); setTimeout(() => setCopied(false), 1500); }}>
        <Icon name={copied ? "check" : "copy"} size={16} />
      </button>
    </div>
  );
}
