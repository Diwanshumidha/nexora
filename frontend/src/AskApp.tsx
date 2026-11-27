import { useEffect, useRef, useState } from "react";
import { Choice, Connect, History, Permissions, type Approval, type ConsentRequest } from "./lib/api";
import { useLive } from "./lib/useLive";
import { askingWho, sourceIcon } from "./lib/source";
import { Icon, Logo } from "./orbit/icons";

/** How long the Allow buttons stay locked after a request appears, so a
 *  keystroke or click meant for another window can't approve it. */
const ARM_MS = 1200;

/** The separate ask window: one pending tool call or AI sign-in at a time,
 *  with everything needed to decide. Opened and closed by the Go side.
 *  Sign-ins come first: a browser is waiting on them. */
export default function AskApp() {
  const [approvals] = useLive(async () => (await Permissions.Approvals()) ?? [], 600, [] as Approval[]);
  const [requests] = useLive(async () => (await Connect.ConnectionRequests()) ?? [], 600, [] as ConsentRequest[]);
  const current = approvals[0];
  const total = approvals.length + requests.length;

  return (
    <div className="ask-shell">
      <header className="ask-top">
        <Logo size={26} />
        <span className="ask-brand">Nexora</span>
        {total > 1 && <span className="ask-count">1 of {total}</span>}
      </header>
      {requests[0] ? <ConnectCard key={requests[0].id} r={requests[0]} /> : current ? <AskCard key={current.id} a={current} /> : (
        <div className="ask-empty">
          <Icon name="check" size={28} />
          <p>Nothing waiting. This window closes by itself.</p>
        </div>
      )}
    </div>
  );
}

/** An AI client asking to sign in over OAuth. Anyone can register any app
 *  name, so the code (shown on the browser page too) is what ties this
 *  request to the browser the user is looking at. */
function ConnectCard({ r }: { r: ConsentRequest }) {
  const [armed, setArmed] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [now, setNow] = useState(Date.now());
  const denyRef = useRef<HTMLButtonElement>(null);

  useEffect(() => {
    const t = setTimeout(() => setArmed(true), ARM_MS);
    const tick = setInterval(() => setNow(Date.now()), 1000);
    denyRef.current?.focus();
    return () => { clearTimeout(t); clearInterval(tick); };
  }, [r.id]);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => { if (e.key === "Escape" && !busy) answer(false); };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  });

  const answer = async (allow: boolean) => {
    setBusy(true);
    setError(null);
    try {
      await Connect.AnswerConnection(r.id, allow);
    } catch (e) {
      setError(String(e));
      setBusy(false);
    }
  };

  const left = Math.max(0, Math.round((new Date(r.expiresAt).getTime() - now) / 1000));

  return (
    <main className="ask-card">
      <div className="ask-who">
        <span className="ask-who-icon"><Icon name="plug" size={18} /></span>
        <span><strong>{r.clientName}</strong> wants to connect to Nexora</span>
      </div>

      <p className="ask-desc">
        Once connected it can read your extensions' API and run scripts with them, like any AI you gave the URL to.
        Tool calls still follow your permissions.
      </p>

      <section className="ask-section">
        <div className="insp-label"><span>ALLOW ONLY IF YOUR BROWSER SHOWS</span></div>
        <div className="ask-pair mono">{r.code}</div>
      </section>

      <section className="ask-section">
        <div className="insp-label"><span>DETAILS</span></div>
        <dl className="ask-facts">
          <dt>Sends access to</dt><dd className="mono">{r.redirectHost}</dd>
          <dt>Signed in through</dt><dd className="mono">{r.via}</dd>
        </dl>
      </section>

      {error && <p className="error">{error}</p>}

      <div className="ask-actions ask-actions-2">
        <button className={`btn primary ask-allow ${armed ? "" : "arming"}`} disabled={!armed || busy} onClick={() => answer(true)}>
          Connect
        </button>
        <button ref={denyRef} className="btn danger" disabled={busy} onClick={() => answer(false)}>
          Deny
        </button>
      </div>

      <p className="ask-foot">
        <span>Didn't start this? Deny it. It expires in {left}s.</span>
        <span className="muted">Esc denies</span>
      </p>
    </main>
  );
}

function AskCard({ a }: { a: Approval }) {
  const [armed, setArmed] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [code, setCode] = useState<string | null>(null);
  const [showCode, setShowCode] = useState(false);
  const [now, setNow] = useState(Date.now());
  const denyRef = useRef<HTMLButtonElement>(null);

  useEffect(() => {
    const t = setTimeout(() => setArmed(true), ARM_MS);
    const tick = setInterval(() => setNow(Date.now()), 1000);
    denyRef.current?.focus(); // the safe choice has focus, never Allow
    if (a.jobId) History.Get(a.jobId).then((r) => setCode(r?.code ?? null)).catch(() => {});
    return () => { clearTimeout(t); clearInterval(tick); };
  }, [a.id, a.jobId]);

  useEffect(() => {
    // Escape denies; there is deliberately no keyboard shortcut to allow.
    const onKey = (e: KeyboardEvent) => { if (e.key === "Escape" && !busy) answer(Choice.DenyOnce); };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  });

  const answer = async (c: Choice) => {
    setBusy(true);
    setError(null);
    try {
      await Permissions.Resolve(a.id, c);
    } catch (e) {
      setError(String(e));
      setBusy(false);
    }
  };

  const left = Math.max(0, Math.round((new Date(a.expiresAt).getTime() - now) / 1000));
  
  return (
    <main className="ask-card">
      <div className="ask-who">
        <span className="ask-who-icon"><Icon name={sourceIcon(a.source)} size={18} /></span>
        <span>
          <strong>{askingWho(a.source, a.scriptName)}</strong> wants to use{" "}
          <strong>{a.extensionId}</strong>
        </span>
      </div>

      <h1 className="ask-tool mono">{a.namespace}.{a.tool}()</h1>
      {a.description && <p className="ask-desc">{a.description}</p>}

      <section className="ask-section">
        <div className="insp-label"><span>WITH THESE ARGUMENTS</span></div>
        <pre className="code ask-args">{JSON.stringify(a.args ?? {}, null, 2)}</pre>
      </section>

      {code && (
        <section className="ask-section">
          <button className="link ask-toggle" aria-expanded={showCode} onClick={() => setShowCode(!showCode)}>
            <Icon name={showCode ? "chevronDown" : "chevronRight"} size={14} stroke={2} /> The script that's asking
          </button>
          {showCode && <pre className="code ask-args">{code}</pre>}
        </section>
      )}

      {error && <p className="error">{error}</p>}

      <div className="ask-actions">
        <button className={`btn primary ask-allow ${armed ? "" : "arming"}`} disabled={!armed || busy} onClick={() => answer(Choice.AllowOnce)}>
          Allow once
        </button>
        <button className="btn ghost" disabled={!armed || busy} onClick={() => answer(Choice.AllowRun)}
          title="Allow this tool until this script finishes">
          Allow for this run
        </button>
        <button ref={denyRef} className="btn danger" disabled={busy} onClick={() => answer(Choice.DenyOnce)}>
          Deny
        </button>
      </div>
      <div className="ask-always">
        <button className="link" disabled={!armed || busy} onClick={() => answer(Choice.AllowAlways)}>Always allow {a.tool}</button>
        <span className="muted">·</span>
        <button className="link ask-danger-link" disabled={busy} onClick={() => answer(Choice.DenyAlways)}>Always block {a.tool}</button>
      </div>

      <p className="ask-foot">
        <span>The script is paused. Not answering denies it in {left}s.</span>
        <span className="muted">Esc denies</span>
      </p>
    </main>
  );
}
