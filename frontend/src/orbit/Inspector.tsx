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
  Setup,
  type Approval,
  type CatalogItem,
  type CatalogView,
  type ConnectInfo,
  type Connection,
  type ExtensionPermissions,
  type ToolPermission,
  type ExtensionInfo,
  type RunSummary,
  type NetworkEndpoint,
  type QueueStats,
} from "../lib/api";
import { timeAgo, useLive, useRun } from "../lib/useLive";
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

      {connect?.enabled && <ConnectGuide connect={connect} live={live} />}

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
              The token URL, for clients without sign-in. Or use <code>{connect.baseUrl}</code> with{" "}
              <code>Authorization: Bearer &lt;token&gt;</code>.
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
            <OAuthURL url={live.exposure?.url ?? ""} />
          </>
        ) : (
          <p className="hint">
            Hosted AIs like ChatGPT need a public URL. Pick a provider on the inner <span className="tone-text-net">network</span> orbit.
          </p>
        )}
      </Section>

      <SecuritySection />

      {connect?.enabled && <ConnectionsSection />}

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
  const [stats, setStats] = useState<{
    rejected: number; originsBlocked: number; last?: { at: string; reason: string } | null;
    accepted: number; lastAccepted?: { at: string; client: string; host: string; public: boolean } | null;
  } | null>(null);
  const [atLogin, setAtLogin] = useState<boolean | null>(null);
  const [confirmRotate, setConfirmRotate] = useState(false);
  const [rotated, setRotated] = useState(false);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    Permissions.Strict().then(setStrict);
    Setup.Autostart().then(setAtLogin).catch(() => setAtLogin(false));
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
  const toggleAtLogin = async () => {
    setError(null);
    try {
      await Setup.SetAutostart(!atLogin);
      setAtLogin(!atLogin);
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
          <span className="setting-head">Last AI request</span>
          <small className="setting-help">
            {stats?.lastAccepted
              ? `${stats.lastAccepted.client} via ${stats.lastAccepted.public ? stats.lastAccepted.host : "this computer"}, ${timeAgo(stats.lastAccepted.at)} ago · ${stats.accepted} since start`
              : "None since start. Once an AI connects, its latest request shows here."}
          </small>
        </div>
      </div>

      <div className="setting setting-inline sec-row">
        <div className="setting-text">
          <span className="setting-head">Start at login</span>
          <small className="setting-help">Runs in the background after a restart, so AIs can reach Nexora without opening it.</small>
        </div>
        <label className="switch">
          <input type="checkbox" aria-label="Start at login" checked={!!atLogin} disabled={atLogin === null} onChange={toggleAtLogin} />
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

type Client = "claude-code" | "claude-ai" | "chatgpt" | "other";

const CLIENTS: { id: Client; label: string }[] = [
  { id: "claude-code", label: "Claude Code" },
  { id: "claude-ai", label: "Claude.ai" },
  { id: "chatgpt", label: "ChatGPT" },
  { id: "other", label: "Other" },
];

const GUIDE_KEY = "nexora.connect-guide";

/** Step-by-step instructions for connecting a specific AI client, with this
 *  machine's real URLs filled in. Hosted clients need a public URL, so their
 *  steps start by pointing at the network orbit until one is live. */
export function ConnectGuide({ connect, live }: { connect: ConnectInfo; live?: NetworkEndpoint }) {
  const [client, setClient] = useState<Client>(() => {
    try {
      return (localStorage.getItem(GUIDE_KEY) as Client) || "claude-code";
    } catch {
      return "claude-code";
    }
  });
  const pick = (c: Client) => {
    setClient(c);
    try { localStorage.setItem(GUIDE_KEY, c); } catch { /* remembered for this view only */ }
  };

  const publicURL = live?.state === "up" && live.exposure?.public ? live.exposure.url : "";
  const tailnetURL = live?.state === "up" && live.exposure && !live.exposure.public ? live.exposure.url : "";
  const approve = (
    <>
      Your browser opens a page with a short code, and Nexora pops up a <strong>Connect</strong> window with the same
      code. Click <strong>Connect</strong> only if the codes match.
    </>
  );
  const needPublic = (
    <li>
      <span>
        Hosted AIs run in the cloud, so they need a <strong>public URL</strong>. Make a provider live on the inner{" "}
        <span className="tone-text-net">network</span> orbit: Tailscale in <code>funnel</code> mode, or Cloudflare.
        {tailnetURL && <> Tailscale is live in <code>tailnet</code> mode, which hosted AIs can't reach.</>}
      </span>
    </li>
  );

  return (
    <Section title="CONNECT AN AI">
      <div className="segmented four" role="radiogroup" aria-label="AI client">
        {CLIENTS.map((c) => (
          <button key={c.id} type="button" role="radio" aria-checked={client === c.id}
            className={`seg ${client === c.id ? "on seg-trusted" : ""}`} onClick={() => pick(c.id)}>
            {c.label}
          </button>
        ))}
      </div>

      {client === "claude-code" && (
        <ol className="steps">
          <li>
            <span>Add Nexora in a terminal:</span>
            <CopyLine text={`claude mcp add --transport http nexora ${connect.baseUrl}`} />
            {publicURL && <small className="hint">On another computer, use <code>{publicURL}</code> instead.</small>}
          </li>
          <li><span>Start <code>claude</code>, run <code>/mcp</code>, pick <strong>nexora</strong> and choose <strong>Authenticate</strong>.</span></li>
          <li><span>{approve}</span></li>
          <li><span>Done. Ask Claude to use Nexora; it reads the API with <code>get_api</code> and runs scripts with <code>run_code</code>.</span></li>
        </ol>
      )}

      {client === "claude-ai" && (
        <ol className="steps">
          {publicURL ? (
            <li>
              <span>Copy the public URL:</span>
              <CopyLine text={publicURL} />
            </li>
          ) : needPublic}
          <li>
            <span>
              On claude.ai open <strong>Settings → Connectors</strong> and click <strong>Add custom connector</strong>.
              Name it <em>Nexora</em> and paste the URL. Leave the advanced OAuth fields empty.
            </span>
          </li>
          <li><span>Click <strong>Connect</strong> next to Nexora. {approve}</span></li>
          <li><span>In a chat, turn Nexora on from the tools menu. The desktop and mobile apps pick it up too.</span></li>
        </ol>
      )}

      {client === "chatgpt" && (
        <ol className="steps">
          {publicURL ? (
            <li>
              <span>Copy the public URL:</span>
              <CopyLine text={publicURL} />
            </li>
          ) : needPublic}
          <li>
            <span>
              In ChatGPT open <strong>Settings → Apps &amp; Connectors → Advanced settings</strong> and turn on{" "}
              <strong>Developer mode</strong>.
            </span>
          </li>
          <li>
            <span>
              Back in <strong>Apps &amp; Connectors</strong>, click <strong>Create</strong>. Name it <em>Nexora</em>, paste the
              URL, set <strong>Authentication</strong> to <strong>OAuth</strong> and confirm you trust it.
            </span>
          </li>
          <li><span>{approve}</span></li>
          <li><span>In a chat, enable Nexora from the <strong>+ → Developer mode</strong> menu.</span></li>
        </ol>
      )}

      {client === "other" && (
        <ol className="steps">
          <li>
            <span>
              Most MCP clients (Cursor, VS Code, Windsurf, ...) take a Streamable HTTP URL. Add Nexora to their MCP config:
            </span>
            <CopyLine text={JSON.stringify({ mcpServers: { nexora: { url: publicURL || connect.baseUrl } } }, null, 2)} multiline />
          </li>
          <li><span>If the client supports sign-in (OAuth), it starts it on first use. {approve}</span></li>
          <li>
            <span>
              No sign-in support? Use the token URL instead (under <strong>On this machine</strong>
              {publicURL ? <> or <strong>Remote access</strong></> : null} below), or send{" "}
              <code>Authorization: Bearer &lt;token&gt;</code>. Treat it like a password.
            </span>
          </li>
        </ol>
      )}

      <p className="hint">
        Approved apps appear under <strong>Signed-in apps</strong>, where you can sign them out. Every tool call still
        follows your extension permissions.
      </p>
    </Section>
  );
}

/** A command or URL with a copy button. Not for secrets: see Secret. */
function CopyLine({ text, multiline }: { text: string; multiline?: boolean }) {
  const [copied, setCopied] = useState(false);
  return (
    <div className={`secret ${multiline ? "secret-multi" : ""}`}>
      {multiline ? <pre className="mono">{text}</pre> : <code>{text}</code>}
      <button className="icon-btn" aria-label="Copy"
        onClick={async () => { await navigator.clipboard.writeText(text); setCopied(true); setTimeout(() => setCopied(false), 1500); }}>
        <Icon name={copied ? "check" : "copy"} size={16} />
      </button>
    </div>
  );
}

/** The token-free URL for clients that sign in with OAuth (claude.ai and
 *  ChatGPT connectors, Claude Code). It is safe to share: on its own it only
 *  starts a sign-in the user has to approve in the app. */
function OAuthURL({ url }: { url: string }) {
  const [copied, setCopied] = useState(false);
  if (!url) return null;
  return (
    <>
      <div className="secret">
        <code>{url}</code>
        <button className="icon-btn" aria-label="Copy sign-in URL"
          onClick={async () => { await navigator.clipboard.writeText(url); setCopied(true); setTimeout(() => setCopied(false), 1500); }}>
          <Icon name={copied ? "check" : "copy"} size={16} />
        </button>
      </div>
      <p className="hint">For clients that sign in (OAuth): add this URL without the token and approve the request that pops up here.</p>
    </>
  );
}

/** AI clients signed in over OAuth, each revocable. */
function ConnectionsSection() {
  const [conns, refresh] = useLive(async () => (await Connect.Connections()) ?? [], 5000, [] as Connection[]);
  const [error, setError] = useState<string | null>(null);
  const revoke = async (id: string) => {
    setError(null);
    try {
      await Connect.RevokeConnection(id);
    } catch (e) {
      setError(String(e));
    }
    refresh();
  };
  return (
    <Section title="SIGNED-IN APPS">
      {conns.length === 0 ? (
        <p className="hint">None yet. Clients that sign in with OAuth appear here once you approve them.</p>
      ) : (
        conns.map((c) => (
          <div key={c.id} className="sec-row">
            <div className="setting-text">
              <span className="setting-head">{c.clientName}</span>
              <small className="setting-help">
                via {c.via} · approved {timeAgo(c.createdAt)} ago · last used {timeAgo(c.lastUsed)} ago
              </small>
            </div>
            <button className="btn ghost" onClick={() => revoke(c.id)}>Sign out</button>
          </div>
        ))
      )}
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
          <OAuthURL url={n.exposure?.url ?? ""} />
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
        {TOOL_GROUPS.map((g) => {
          const tools = (perms.tools ?? []).filter((t) => t.readOnly === g.readOnly);
          if (tools.length === 0) return null;
          const rule = MODE_DEFAULT(perms.mode, g.readOnly);
          return (
            <div key={g.title} className="tool-group">
              <div className="tool-group-head">
                <span>{g.title}</span>
                <span className={`tool-rule rule-${rule}`}><Icon name={CHOICE_ICON[rule]} size={13} stroke={2} /> {RULE_LABEL[rule]}</span>
              </div>
              <ul className="tool-list">
                {tools.map((t) => (
                  <ToolRow key={t.name} t={t} rule={rule}
                    onSet={(d) => apply(() => Permissions.SetTool(extId, t.name, d))} />
                ))}
              </ul>
            </div>
          );
        })}
        {error && <p className="error">{error}</p>}
      </Section>
    </>
  );
}

const TOOL_GROUPS = [
  { title: "Read only", readOnly: true },
  { title: "Make changes", readOnly: false },
];
const RULE_LABEL: Record<string, string> = { allow: "Run without asking", ask: "Ask you first", deny: "Blocked" };
const CHOICE_ICON: Record<string, IconName> = { allow: "check", ask: "hand", deny: "ban" };

const CHOICES: { d: Decision; label: string; icon: IconName; help: string }[] = [
  { d: Decision.Allow, label: "Allow", icon: "check", help: "Runs without asking" },
  { d: Decision.Ask, label: "Ask", icon: "hand", help: "Waits for your approval each time" },
  { d: Decision.Deny, label: "Block", icon: "ban", help: "Never runs; hidden from the AI" },
];

/** One tool. The group header states the trust level's rule; a row only
 *  stands out when you've made an exception for it. */
function ToolRow({ t, rule, onSet }: { t: ToolPermission; rule: string; onSet: (d: Decision) => void }) {
  const [open, setOpen] = useState(false);
  const custom = !!t.override;
  return (
    <li className={`tool ${custom ? "custom" : ""}`}>
      <button className={`tool-text ${open ? "open" : ""}`} aria-expanded={open} onClick={() => setOpen(!open)}>
        <span className="mono tool-name">{t.name}</span>
        {t.description && <span className="tool-desc">{t.description}</span>}
      </button>
      <div className="tool-perm">
        {custom && (
          <button className="icon-btn small tool-reset" onClick={() => onSet(Decision.$zero)}
            aria-label={`Reset ${t.name} to the default (${RULE_LABEL[rule].toLowerCase()})`}
            title={`Back to the default: ${RULE_LABEL[rule].toLowerCase()}`}>
            <Icon name="refresh" size={13} />
          </button>
        )}
        <div role="radiogroup" aria-label={`Permission for ${t.name}`} className="tri">
          {CHOICES.map((c) => {
            const on = t.effective === c.d;
            return (
              <button key={c.d} role="radio" aria-checked={on} aria-label={c.label} title={`${c.label}: ${c.help}`}
                className={`tri-opt tri-${c.d} ${on ? (custom ? "on custom" : "on") : ""}`}
                onClick={() => onSet(c.d)}>
                <Icon name={c.icon} size={14} stroke={2} />
              </button>
            );
          })}
        </div>
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
      .then((v) => {
        setView(v);
        // An old saved copy shows at once; fetch a fresh one behind it.
        if (v.stale && !force) load(true);
        else setLoading(false);
      })
      .catch((e) => {
        setLoadError(String(e));
        setLoading(false);
      });
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
      {group.map((item) => <CatalogRow key={item.id} item={item}
        working={working === item.id || (working === "*" && item.state === "update")} disabled={busy}
        onInstall={() => install(item, false)} onUpdate={() => install(item, true)}
        onOpen={() => onSelect({ kind: "extension", id: item.id })} />)}
    </ul>
  );

  const checked = view?.fetchedAt && !view.fetchedAt.startsWith("0001") ? timeAgo(view.fetchedAt) : "";
  const ago = checked === "now" ? "just now" : `${checked} ago`;
  const notInstalled = (view?.items ?? []).filter((i) => i.state === "available").length;
  const status = !view
    ? loadError ? "catalog unavailable" : "checking the catalog…"
    : !view.configured ? "no catalog configured"
    : view.offline ? `offline, copy from ${ago}`
    : loading ? "checking for updates…"
    : `${notInstalled} to install, checked ${ago}`;
  return (
    <>
      <div className="insp-kicker">EXTENSION CATALOG</div>
      <Head icon="puzzle" title="Add an extension" tone="ext" sub={<span aria-live="polite">{status}</span>} />
      <p className="insp-desc">
        Each one runs sandboxed with only the access shown on it. Downloads are verified before they're installed.
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
          <span>Can't reach the catalog, so this is the copy from {ago}. Installing needs a connection.</span>
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
            <button type="button" className="search-action" aria-label="Check the catalog for updates"
              title={checked ? `Checked ${ago}. Check again` : "Check again"} disabled={loading} onClick={() => load(true)}>
              <span className={loading ? "spin" : ""}><Icon name="refresh" size={15} /></span>
            </button>
          </label>
        </div>
      )}

      {updates.length > 0 && (
        <Section title={`UPDATES · ${updates.length}`} aside={updates.length > 1 && (
          <button className="btn primary" disabled={busy} onClick={updateAll}>{working === "*" ? "Updating…" : "Update all"}</button>
        )}>
          {list(updates)}
        </Section>
      )}
      {available.length > 0 && <Section title={`AVAILABLE · ${available.length}`}>{list(available)}</Section>}
      {installed.length > 0 && <Section title={`INSTALLED · ${installed.length}`}>{list(installed)}</Section>}
      {q && items.length === 0 && <p className="hint">No extensions match “{query.trim()}”.</p>}

      <Section title="MORE WAYS TO ADD">
        <div className="add-ways">
          <button className="btn ghost" disabled={busy} onClick={installFile}>
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

interface RowProps {
  item: CatalogItem; working: boolean; disabled: boolean;
  onInstall: () => void; onUpdate: () => void; onOpen: () => void;
}

/** One catalog entry: what it is, what it can reach, and the one action
 *  that fits its state. Clicking the text shows the full description. */
function CatalogRow({ item, ...action }: RowProps) {
  const [open, setOpen] = useState(false);
  const access = accessOf(item);
  return (
    <li className={`catalog-item ${item.state}`}>
      <span className="catalog-icon"><Icon name={iconFor(item.icon)} size={18} /></span>
      <button className={`catalog-body ${open ? "open" : ""}`} aria-expanded={open} onClick={() => setOpen(!open)}>
        <span className="catalog-title">
          <strong>{item.name}</strong>
          <span className="catalog-version">
            {item.state === "update" ? <>{item.installedVersion} <span aria-label="to">→</span> {item.version}</> : item.version}
          </span>
        </span>
        <span className="catalog-desc">{item.description}</span>
        <span className="catalog-access">
          {access.length === 0 ? (
            <span className="access-none"><Icon name="shield" size={13} /> Sandbox only</span>
          ) : access.map((a) => <span key={a} className="access-tag">{a}</span>)}
          <span className="catalog-size">{fileSize(item.size)}</span>
        </span>
      </button>
      <CatalogAction item={item} {...action} />
    </li>
  );
}

function CatalogAction({ item, working, disabled, onInstall, onUpdate, onOpen }: RowProps) {
  switch (item.state) {
    case "available":
      return <button className="btn ghost catalog-btn" disabled={disabled} onClick={onInstall}>{working ? "Installing…" : "Install"}</button>;
    case "update":
      return <button className="btn primary catalog-btn" disabled={disabled} onClick={onUpdate}>{working ? "Updating…" : "Update"}</button>;
    case "installed":
      return (
        <span className="catalog-installed">
          <span><Icon name="check" size={13} /> Installed</span>
          <button className="link" onClick={onOpen}>Open</button>
        </span>
      );
    case "dev":
      return <span className="badge tone-net" title="Loaded from a development folder; rebuild it there">dev</span>;
    default:
      return <span className="catalog-note" title="Needs a newer version of Nexora">Needs a newer Nexora</span>;
  }
}

/** What an extension can reach beyond its sandbox, in plain words. */
function accessOf(item: CatalogItem): string[] {
  const p = item.permissions;
  const out: string[] = [];
  if (p?.network) out.push("Internet");
  if (p?.childProcess) out.push("Runs programs");
  if (p?.fsWrite?.length) out.push("Writes files");
  else if (p?.fsRead?.length) out.push("Reads files");
  return out;
}

function fileSize(n: number): string {
  return n > 1 << 20 ? `${(n / (1 << 20)).toFixed(1)}\u00a0MB` : `${Math.max(1, Math.round(n / 1024))}\u00a0KB`;
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
