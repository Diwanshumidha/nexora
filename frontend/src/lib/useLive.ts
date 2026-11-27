import { useCallback, useEffect, useRef, useState } from "react";
import { History, type HistoryRecord } from "./api";

/**
 * Polls fetcher every `interval` ms while the window is visible. Returns the
 * latest value and a refresh function for after user actions. Unchanged
 * results are dropped, so an idle app doesn't re-render on every poll.
 */
export function useLive<T>(fetcher: () => Promise<T>, interval: number, initial: T) {
  const [value, setValue] = useState<T>(initial);
  const fetchRef = useRef(fetcher);
  fetchRef.current = fetcher;
  const last = useRef<string | null>(null);

  const refresh = useCallback(async () => {
    try {
      const next = await fetchRef.current();
      const key = JSON.stringify(next);
      if (key === last.current) return;
      last.current = key;
      setValue(next);
    } catch (e) {
      console.error(e);
    }
  }, []);

  useEffect(() => {
    refresh();
    const timer = setInterval(() => {
      if (document.visibilityState === "visible") refresh();
    }, interval);
    return () => clearInterval(timer);
  }, [interval, refresh]);

  return [value, refresh] as const;
}

export function timeAgo(iso: string | null | undefined): string {
  if (!iso) return "";
  const s = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000);
  if (s < 60) return "now";
  if (s < 3600) return `${Math.floor(s / 60)}m`;
  if (s < 86400) return `${Math.floor(s / 3600)}h`;
  return `${Math.floor(s / 86400)}d`;
}

/** Time left until iso, e.g. "5m"; "<1m" when due or past. */
export function timeUntil(iso: string | null | undefined): string {
  if (!iso) return "";
  const s = (new Date(iso).getTime() - Date.now()) / 1000;
  if (s < 60) return "<1m";
  if (s < 3600) return `${Math.round(s / 60)}m`;
  if (s < 86400) return `${Math.round(s / 3600)}h`;
  return `${Math.round(s / 86400)}d`;
}

export function duration(start?: string | null, end?: string | null): string {
  if (!start) return "";
  const ms = (end ? new Date(end) : new Date()).getTime() - new Date(start).getTime();
  if (ms < 1000) return `${ms} ms`;
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)} s`;
  return `${Math.floor(ms / 60_000)} min ${Math.round((ms % 60_000) / 1000)} s`;
}

/**
 * The full record of one run, fetched when `id` is set and refetched only
 * when `version` changes (e.g. the run's state or finish time).
 */
export function useRun(id: string | null, version: string) {
  const [rec, setRec] = useState<HistoryRecord | null>(null);
  useEffect(() => {
    if (!id) {
      setRec(null);
      return;
    }
    let live = true;
    History.Get(id).then((r) => live && setRec(r)).catch(() => live && setRec(null));
    return () => {
      live = false;
    };
  }, [id, version]);
  return rec;
}
