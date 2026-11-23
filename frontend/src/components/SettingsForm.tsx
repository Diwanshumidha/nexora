import { useEffect, useState } from "react";
import { Extensions } from "../lib/api";
import type { SettingsView } from "../../bindings/nexora/internal/services/models";
import { Icon } from "../orbit/icons";

type Spec = NonNullable<SettingsView["specs"]>[number];
type Values = Record<string, unknown>;

const GRANT_LABEL: Record<string, string> = { read: "CAN READ", readwrite: "CAN READ + EDIT" };

/** Renders an extension's declared settings and saves them. Saving a
 *  setting that grants folder access restarts the extension. */
export function SettingsForm({ extId, extName, onSaved }: { extId: string; extName: string; onSaved: (v: SettingsView) => void }) {
  const [view, setView] = useState<SettingsView | null>(null);
  const [values, setValues] = useState<Values>({});
  const [secrets, setSecrets] = useState<Record<string, string | null>>({});
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);

  const load = (v: SettingsView) => {
    setView(v);
    setValues({ ...(v.values ?? {}) });
    setSecrets({});
    onSaved(v);
  };
  useEffect(() => {
    Extensions.Settings(extId).then(load).catch((e) => setError(String(e)));
  }, [extId]); // eslint-disable-line react-hooks/exhaustive-deps

  if (!view) return error ? <p className="error">{error}</p> : null;
  const specs = view.specs ?? [];
  if (specs.length === 0) return null;

  const original = view.values ?? {};
  const dirty =
    specs.some((s) => s.type !== "secret" && JSON.stringify(values[s.key]) !== JSON.stringify(original[s.key])) ||
    Object.values(secrets).some((v) => v !== null && v !== undefined);
  const restarts = specs.some((s) => s.grant && JSON.stringify(values[s.key]) !== JSON.stringify(original[s.key]));

  const set = (key: string, v: unknown) => {
    setSaved(false);
    setValues((cur) => ({ ...cur, [key]: v }));
  };

  const save = async () => {
    setSaving(true);
    setError(null);
    try {
      const out: Values = { ...values };
      for (const s of specs) if (s.type === "secret") out[s.key] = secrets[s.key] ?? null; // null keeps the stored secret
      load(await Extensions.SetSettings(extId, out));
      setSaved(true);
    } catch (e) {
      setError(String(e));
    } finally {
      setSaving(false);
    }
  };

  return (
    <section className="insp-section">
      <div className="insp-label"><span>SETTINGS</span></div>
      <div className="settings-form">
        {specs.map((s) => (
          <Field key={s.key} spec={s} value={values[s.key]} secretSet={!!view.secretsSet?.[s.key]}
            secret={secrets[s.key] ?? null} extName={extName}
            onChange={(v) => set(s.key, v)}
            onSecret={(v) => { setSaved(false); setSecrets((cur) => ({ ...cur, [s.key]: v })); }} />
        ))}
      </div>
      {error && <p className="error">{error}</p>}
      {(dirty || saved) && (
        <div className="settings-save">
          {dirty ? (
            <>
              <button className="btn primary" disabled={saving} onClick={save}>{saving ? "Saving…" : "Save settings"}</button>
              <button className="btn ghost" disabled={saving} onClick={() => load(view)}>Discard</button>
              {restarts && <span className="hint">Saving restarts {extName} so its sandbox gets the new folder access.</span>}
            </>
          ) : (
            <span className="hint tone-text-ext"><Icon name="check" size={13} stroke={2} /> Saved</span>
          )}
        </div>
      )}
    </section>
  );
}

function Field({ spec: s, value, secret, secretSet, extName, onChange, onSecret }: {
  spec: Spec; value: unknown; secret: string | null; secretSet: boolean; extName: string;
  onChange: (v: unknown) => void; onSecret: (v: string | null) => void;
}) {
  const id = `setting-${s.key}`;
  const head = (
    <div className="setting-head">
      <label htmlFor={id}>{s.label}{s.required && <span className="tone-text-error"> *</span>}</label>
      {s.grant && <span className="badge tone-pending">{GRANT_LABEL[s.grant] ?? s.grant}</span>}
    </div>
  );
  const help = s.description && <small className="setting-help">{s.description}</small>;

  switch (s.type) {
    case "boolean":
      return (
        <div className="setting setting-inline">
          <div className="setting-text">{head}{help}</div>
          <label className="switch">
            <input id={id} type="checkbox" checked={!!value} onChange={(e) => onChange(e.target.checked)} />
            <span aria-hidden="true" />
          </label>
        </div>
      );
    case "select":
      return (
        <div className="setting">
          {head}
          <div role="radiogroup" aria-label={s.label} className="chips">
            {(s.choices ?? []).map((c) => (
              <button key={c} role="radio" aria-checked={value === c} className={`chip ${value === c ? "on" : ""}`} onClick={() => onChange(c)}>
                {c}
              </button>
            ))}
          </div>
          {help}
        </div>
      );
    case "number":
      return (
        <div className="setting">
          {head}
          <input id={id} type="number" value={value === undefined || value === null ? "" : String(value)}
            onChange={(e) => onChange(e.target.value === "" ? null : Number(e.target.value))} />
          {help}
        </div>
      );
    case "secret":
      return (
        <div className="setting">
          {head}
          <input id={id} type="password" autoComplete="off" value={secret ?? ""}
            placeholder={secretSet ? "Saved · type to replace" : "Not set"}
            onChange={(e) => onSecret(e.target.value === "" ? null : e.target.value)} />
          {help}
        </div>
      );
    case "folder":
      return (
        <div className="setting">
          {head}
          <FolderRow path={(value as string) || ""} title={`${s.label} for ${extName}`}
            onPick={(p) => onChange(p)} onClear={() => onChange("")} />
          {help}
        </div>
      );
    case "folders": {
      const list = (value as string[]) ?? [];
      return (
        <div className="setting">
          {head}
          {list.length === 0 && <p className="hint">No folders yet. {extName} can't touch any of your files until you add one.</p>}
          <ul className="folder-list">
            {list.map((p) => (
              <li key={p} className="folder">
                <Icon name="folder" size={15} />
                <FolderName path={p} />
                <button className="icon-btn" aria-label={`Remove ${p}`} onClick={() => onChange(list.filter((x) => x !== p))}>
                  <Icon name="x" size={14} />
                </button>
              </li>
            ))}
          </ul>
          <AddFolder title={`Add a folder for ${extName}`} onPick={(p) => !list.includes(p) && onChange([...list, p])} />
          {help}
        </div>
      );
    }
    default:
      return (
        <div className="setting">
          {head}
          <input id={id} value={(value as string) ?? ""} onChange={(e) => onChange(e.target.value)} />
          {help}
        </div>
      );
  }
}

function FolderRow({ path, title, onPick, onClear }: { path: string; title: string; onPick: (p: string) => void; onClear: () => void }) {
  return path ? (
    <div className="folder">
      <Icon name="folder" size={15} />
      <FolderName path={path} />
      <AddFolder title={title} label="Change" onPick={onPick} />
      <button className="icon-btn" aria-label="Clear folder" onClick={onClear}><Icon name="x" size={14} /></button>
    </div>
  ) : (
    <AddFolder title={title} label="Choose folder" onPick={onPick} />
  );
}

/** Folder name first, with its parent path underneath. */
function FolderName({ path }: { path: string }) {
  const trimmed = path.replace(/\/+$/, "") || "/";
  const cut = trimmed.lastIndexOf("/");
  const name = trimmed.slice(cut + 1) || trimmed;
  const parent = cut > 0 ? trimmed.slice(0, cut) : "/";
  return (
    <span className="folder-name" title={path}>
      <span className="folder-leaf">{name}</span>
      <span className="folder-parent mono">{parent}</span>
    </span>
  );
}

/** Native folder picker, with a typed-path fallback. */
function AddFolder({ title, label = "Add folder", onPick }: { title: string; label?: string; onPick: (p: string) => void }) {
  const [typing, setTyping] = useState(false);
  const [text, setText] = useState("");
  const pick = async () => {
    try {
      const p = await Extensions.PickFolder(title);
      if (p) onPick(p);
    } catch {
      setTyping(true); // no native dialog available
    }
  };
  if (typing) {
    return (
      <form className="folder-type" onSubmit={(e) => { e.preventDefault(); if (text.trim()) { onPick(text.trim()); setText(""); setTyping(false); } }}>
        <input autoFocus value={text} onChange={(e) => setText(e.target.value)} placeholder="/home/you/Documents/Reports" aria-label="Folder path" />
        <button className="btn ghost" type="submit">Add</button>
        <button className="btn ghost" type="button" onClick={() => setTyping(false)}>Cancel</button>
      </form>
    );
  }
  return (
    <div className="folder-actions">
      <button className="btn ghost" onClick={pick}><Icon name="plus" size={15} /> {label}</button>
      <button className="link" onClick={() => setTyping(true)}>type a path</button>
    </div>
  );
}
