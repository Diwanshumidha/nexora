import { useEffect, useState } from "react";
import { Storage, type StorageView } from "../lib/api";
import { useLive } from "../lib/useLive";
import { Icon } from "../orbit/icons";

const empty: StorageView = {
  stats: { entries: 0, bytes: 0, maxEntries: 0, maxBytes: 0, defaultTtl: 0, maxTtl: 0, evicted: 0, expired: 0 },
  entries: [],
};

/** What scripts have kept in tools.store, with its limits, and controls to
 *  remove entries. */
export function StoragePane({ version }: { version: string }) {
  // version changes after each run, so new entries show up straight away.
  const [view, refresh] = useLive(() => Storage.List(""), 3000, empty);
  useEffect(() => {
    refresh();
  }, [version, refresh]);
  const [confirmClear, setConfirmClear] = useState(false);
  const stats = view.stats;
  const entries = view.entries ?? [];
  const used = stats.maxBytes ? Math.min(1, stats.bytes / stats.maxBytes) : 0;

  return (
    <div className="storage">
      <div className="storage-usage">
        <span>
          {stats.entries} {stats.entries === 1 ? "key" : "keys"} · {bytes(stats.bytes)} of {bytes(stats.maxBytes)}
        </span>
        {entries.length > 0 && (confirmClear ? (
          <span className="pane-actions">
            <button className="link-btn danger" onClick={() => Storage.Clear().then(() => { setConfirmClear(false); refresh(); })}>
              Clear {stats.entries}
            </button>
            <button className="link-btn" onClick={() => setConfirmClear(false)}>Keep</button>
          </span>
        ) : (
          <button className="link-btn" onClick={() => setConfirmClear(true)}>Clear all</button>
        ))}
      </div>
      <div className="meter" role="meter" aria-label="Storage used" aria-valuemin={0} aria-valuemax={100} aria-valuenow={Math.round(used * 100)}>
        <span style={{ width: `${Math.max(used * 100, used > 0 ? 1 : 0)}%` }} />
      </div>
      <p className="hint">
        Scripts keep JSON here between runs with <code>tools.store</code>. It's a cache: entries expire
        (default {ttl(stats.defaultTtl)}, at most {ttl(stats.maxTtl)}) and the least recently used are dropped when it's
        full{stats.evicted + stats.expired > 0 && <> ({stats.expired} expired, {stats.evicted} evicted since Nexora started)</>}.
      </p>

      {entries.length === 0 ? (
        <pre className="api storage-example">{`await tools.store.set({ key: "last-run", value: { at: Date.now() }, ttl: "7d" })
const last = await tools.store.get({ key: "last-run" })  // null once expired`}</pre>
      ) : (
        <ul className="storage-list">
          {entries.map((e) => (
            <li key={e.key}>
              <div className="storage-row">
                <code className="storage-key" title={e.key}>{e.key}</code>
                <span className="muted">{bytes(e.size)} · expires {until(e.expiresAt)}</span>
                <button className="icon-btn small" aria-label={`Delete ${e.key}`} title="Delete"
                  onClick={() => Storage.Delete(e.key).then(refresh)}>
                  <Icon name="trash" size={14} />
                </button>
              </div>
              <code className="storage-preview">{e.preview}</code>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

function bytes(n: number): string {
  if (n >= 1 << 20) return `${(n / (1 << 20)).toFixed(n >= 10 << 20 ? 0 : 1)} MB`;
  if (n >= 1 << 10) return `${Math.round(n / (1 << 10))} KB`;
  return `${n} B`;
}

/** A Go time.Duration (nanoseconds) as "1 day" / "30 days" / "6 h". */
function ttl(ns: number): string {
  const h = ns / 3.6e12;
  if (!h) return "";
  if (h >= 24 && h % 24 === 0) return `${h / 24} ${h === 24 ? "day" : "days"}`;
  return `${h} h`;
}

function until(iso: string): string {
  const s = (new Date(iso).getTime() - Date.now()) / 1000;
  if (s < 60) return "in under a minute";
  if (s < 3600) return `in ${Math.round(s / 60)} min`;
  if (s < 86400) return `in ${Math.round(s / 3600)} h`;
  return `in ${Math.round(s / 86400)} d`;
}
