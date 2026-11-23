import { useEffect, useMemo, useRef, useState, type CSSProperties } from "react";
import { memo } from "react";
import type { Approval, ExtensionInfo, NetworkEndpoint, RunSummary } from "../lib/api";
import { timeAgo } from "../lib/useLive";
import { Icon, Logo, iconFor, type IconName } from "./icons";

export type Selection =
  | { kind: "core" }
  | { kind: "network"; id: string }
  | { kind: "extension"; id: string }
  | { kind: "history"; id: string }
  | { kind: "approval"; id: string }
  | { kind: "add" };

interface Props {
  network: NetworkEndpoint[];
  extensions: ExtensionInfo[];
  history: RunSummary[];
  /** Permission mode per extension id. */
  modes: Record<string, string | undefined>;
  approvals: Approval[];
  selected: Selection;
  onSelect: (s: Selection) => void;
  /** Opens the History tab; shown as an "all runs" node when there are more. */
  onOpenHistory?: () => void;
}

/** Minimum distance between neighbouring extension nodes along a ring, in
 *  px: a node plus its label. */
const NODE_SPACING = 112;
/** The arc kept free at the bottom of each ring for its label. */
const GAP_DEG = 50;
/** Runs shown on the outer ring (newest first; running ones always lead).
 *  The rest are one click away in the History tab. */
export const HISTORY_SLOTS = 3;

/**
 * Angle in degrees for item i of n, spread over the ring but leaving a gap
 * at the bottom (90°) where the ring's label sits.
 */
function spread(i: number, n: number, offset = 0): number {
  return 90 + GAP_DEG / 2 + offset + ((i + 0.5) * (360 - GAP_DEG)) / Math.max(n, 1);
}

/** Length of the usable arc (everything but the bottom gap) of a ring. */
const arc = (r: number) => (2 * Math.PI * r * (360 - GAP_DEG)) / 360;

/**
 * Extension rings: one ring whose radius grows to fit everything at
 * NODE_SPACING; only past its maximum size is a second ring added, staggered
 * half a step so nodes never sit in line with each other.
 */
function planExtRings(n: number, C: number): { r: number; count: number; offset: number }[] {
  const min = C * 0.58, max = C * 0.76;
  const needed = (n * NODE_SPACING) / ((2 * Math.PI * (360 - GAP_DEG)) / 360);
  if (needed <= max) return [{ r: Math.max(min, needed), count: n, offset: 0 }];
  const inner = C * 0.5, outer = C * 0.71;
  const innerCount = Math.min(n - 1, Math.round((n * arc(inner)) / (arc(inner) + arc(outer))));
  const outerCount = n - innerCount;
  const outerStep = (360 - GAP_DEG) / outerCount;
  return [
    { r: inner, count: innerCount, offset: 0 },
    { r: outer, count: outerCount, offset: outerStep / 2 },
  ];
}

interface Placed {
  key: string;
  sel: Selection;
  x: number;
  y: number;
}

export const MODE_LABEL: Record<string, string> = { trusted: "trusted", ask: "ask first", readonly: "read only" };

export const OrbitMap = memo(function OrbitMap({ network, extensions, history, modes, approvals, selected, onSelect, onOpenHistory }: Props) {
  const box = useRef<HTMLDivElement>(null);
  const size = useSize(box);
  const S = Math.max(size, 360);
  const C = S / 2;

  const layout = useMemo(() => {
    const slots = [...extensions.map((e) => e.manifest!.id), "+add"];
    const rings = planExtRings(slots.length, C);
    const extRadii = rings.map((r) => r.r);
    const rNet = C * 0.3;
    // Room below the history ring for its runs' captions and the ring label.
    const rHist = Math.min(C - 58, extRadii[extRadii.length - 1] + C * 0.17);
    // Two rings are dense: smaller planets, no status captions.
    const compact = rings.length > 1;

    const at = (r: number, deg: number) => {
      const a = (deg * Math.PI) / 180;
      return { x: C + r * Math.cos(a), y: C + r * Math.sin(a) };
    };

    const net: Placed[] = network.map((n, i) => ({
      key: n.id,
      sel: { kind: "network", id: n.id },
      // Across the upper half (200°–340°), so labels drawn below each node
      // stay clear of the core.
      ...at(rNet, network.length === 1 ? 270 : 200 + (i * 140) / (network.length - 1)),
    }));

    let next = 0;
    const ext: Placed[] = rings.flatMap((ring) =>
      Array.from({ length: ring.count }, (_, i) => {
        const id = slots[next++];
        return {
          key: id,
          sel: id === "+add" ? ({ kind: "add" } as const) : ({ kind: "extension", id } as const),
          ...at(ring.r, spread(i, ring.count, ring.offset)),
        };
      }),
    );

    const shown = history.slice(0, HISTORY_SLOTS);
    const more = history.length > shown.length && !!onOpenHistory;
    const histSlots = shown.length + (more ? 1 : 0);
    // Grouped at the bottom of the outer ring, inside the extension rings'
    // gap so they never meet an extension; ~56 px apart, newest on the left.
    const histStep = ((56 / rHist) * 180) / Math.PI;
    const onHistRing = (i: number) => {
      const a = ((90 - (i - (histSlots - 1) / 2) * histStep) * Math.PI) / 180;
      return { x: rHist + rHist * Math.cos(a), y: rHist + rHist * Math.sin(a) };
    };
    const hist: Placed[] = shown.map((h, i) => ({ key: h.id, sel: { kind: "history", id: h.id }, ...onHistRing(i) }));
    const moreAt = more ? onHistRing(shown.length) : null;

    return { rNet, rHist, extRadii, net, ext, hist, moreAt, compact };
  }, [C, network, extensions, history]);

  const extById = new Map(extensions.map((e) => [e.manifest!.id, e]));
  const extByNs = new Map(extensions.map((e) => [e.manifest!.namespace ?? e.manifest!.id, e.manifest!.id]));
  const netById = new Map(network.map((n) => [n.id, n]));
  const histById = new Map(history.map((h) => [h.id, h]));
  const liveNet = layout.net.find((p) => netById.get(p.key)?.active);

  // Trace for the selected run: core → each extension it used, in order.
  const traceIds = useMemo(() => {
    if (selected.kind !== "history") return [];
    const rec = histById.get(selected.id);
    const order: string[] = [];
    for (const t of rec?.tools ?? []) {
      const id = extByNs.get(t.split(".")[0]);
      if (id && !order.includes(id)) order.push(id);
    }
    return order;
  }, [selected, history, extensions]); // eslint-disable-line react-hooks/exhaustive-deps
  const tracePts = [{ x: C, y: C }, ...traceIds.map((id) => layout.ext.find((p) => p.key === id)!).filter(Boolean)];

  const isSel = (s: Selection) =>
    s.kind === selected.kind && ("id" in s ? "id" in selected && s.id === selected.id : true);

  const anyLive = history.some((h) => h.state === "running" || h.state === "queued");
  const waitingForYou = approvals.length > 0;

  return (
    <div ref={box} className="orbit-box">
      <div className={`orbit ${layout.compact ? "compact" : ""}`} style={{ width: S, height: S }}>
        <svg className="orbit-svg" width={S} height={S} aria-hidden="true">
          {/* rings */}
          <circle cx={C} cy={C} r={layout.rHist} className="ring ring-history" />
          {layout.extRadii.map((r) => (
            <circle key={r} cx={C} cy={C} r={r} className="ring ring-ext" />
          ))}
          <circle cx={C} cy={C} r={layout.rNet} className="ring ring-net" />
          {/* live network link */}
          {liveNet && (
            <line x1={C} y1={C} x2={liveNet.x} y2={liveNet.y}
              className={`link-live ${netById.get(liveNet.key)?.state === "up" ? "" : "link-pending"}`} />
          )}
          {/* trace of the selected run */}
          {tracePts.slice(1).map((p, i) => (
            <line key={i} x1={tracePts[i].x} y1={tracePts[i].y} x2={p.x} y2={p.y} className="trace" />
          ))}
        </svg>

        {/* ring labels */}
        <RingLabel C={C} r={layout.rNet} text="NETWORK" tone="net" />
        <RingLabel C={C} r={layout.extRadii[layout.extRadii.length - 1]} text="EXTENSIONS" tone="ext" />
        <RingLabel C={C} r={layout.rHist + 44} text="HISTORY" tone="muted" />

        {/* history ring: the latest runs */}
        <div
          className="history-ring"
          style={{ left: C - layout.rHist, top: C - layout.rHist, width: layout.rHist * 2, height: layout.rHist * 2 }}
        >
          {layout.hist.map((p) => {
            const h = histById.get(p.key)!;
            return (
              <div key={p.key} className="history-slot" style={{ left: p.x, top: p.y }}>
                <button
                  className={`node node-history state-${h.state} ${isSel(p.sel) ? "selected" : ""}`}
                  aria-pressed={isSel(p.sel)}
                  aria-label={`${h.source === "mcp" ? "AI" : "You"} run, ${h.state}, ${timeAgo(h.createdAt)} ago`}
                  title={`${h.state} · ${h.source === "mcp" ? "AI via MCP" : "You"} · ${(h.tools ?? []).join(", ") || "no tools"}`}
                  onClick={() => onSelect(p.sel)}
                >
                  <span className="dot">
                    <Icon name={h.source === "mcp" ? "ai" : "user"} size={14} />
                  </span>
                  <span className="node-meta">{timeAgo(h.createdAt)}</span>
                </button>
              </div>
            );
          })}
          {layout.moreAt && (
            <div className="history-slot" style={{ left: layout.moreAt.x, top: layout.moreAt.y }}>
              <button className="node node-history node-more" aria-label="Open all runs in History" onClick={onOpenHistory}>
                <span className="dot"><Icon name="history" size={14} /></span>
                <span className="node-meta">all runs</span>
              </button>
            </div>
          )}
        </div>

        {/* network providers */}
        {layout.net.map((p) => {
          const n = netById.get(p.key)!;
          return (
            <NodeButton key={p.key} p={p} selected={isSel(p.sel)} onSelect={onSelect}
              icon={p.key === "tailscale" ? "tailscale" : p.key === "cloudflare" ? "cloud" : "shield"}
              name={n.name}
              meta={n.active ? (n.state === "up" ? (n.exposure?.public ? "live · public" : "live · tailnet") : n.state) : n.available ? "off" : "unavailable"}
              tone={n.active ? (n.state === "up" ? "live" : n.state === "down" ? "error" : "pending") : n.available ? "net" : "dim"}
              label={`${n.name} network provider, ${n.active ? n.state : "off"}`}
            />
          );
        })}

        {/* extensions + add slot */}
        {layout.ext.map((p) => {
          if (p.sel.kind === "add") {
            return (
              <button key="add" className={`node-add ${isSel(p.sel) ? "selected" : ""}`} style={{ left: p.x, top: p.y }}
                aria-label="Add an extension" onClick={() => onSelect(p.sel)}>
                <Icon name="plus" />
              </button>
            );
          }
          const e = extById.get(p.key)!;
          const step = traceIds.indexOf(p.key);
          const mode = modes[p.key] ?? "ask";
          const waiting = approvals.find((a) => a.extensionId === p.key);
          return (
            <NodeButton key={p.key} p={p}
              selected={isSel(p.sel) || (selected.kind === "approval" && approvals.some((a) => a.id === selected.id && a.extensionId === p.key))}
              onSelect={() => onSelect(waiting ? { kind: "approval", id: waiting.id } : p.sel)}
              icon={iconFor(e.manifest!.icon)}
              name={e.manifest!.name}
              // Packed rings have no room for captions; the planet's tone and the
              // tooltip still say stopped/error.
              meta={layout.compact ? "" : e.status !== "running" ? e.status : e.stale ? "update ready" : ""}
              tone={e.status === "running" ? `mode-${mode}` : e.status === "error" ? "error" : "dim"}
              label={`${e.manifest!.name} extension, ${e.status}, ${MODE_LABEL[mode] ?? mode}${waiting ? ", waiting for your approval" : ""}`}
              step={step >= 0 ? step + 1 : undefined}
              waiting={!!waiting}
            />
          );
        })}

        {/* core */}
        <button
          className={`core ${isSel({ kind: "core" }) ? "selected" : ""} ${anyLive ? "busy" : ""}`}
          style={{ left: C, top: C }}
          aria-label="Nexora core: AI connection settings"
          aria-pressed={isSel({ kind: "core" })}
          onClick={() => onSelect({ kind: "core" })}
        >
          <span className="core-pulse" aria-hidden="true" />
          <Logo size={44} />
          <span className="core-status">{waitingForYou ? "needs you" : anyLive ? "working…" : "ready"}</span>
        </button>
      </div>
    </div>
  );
});

function NodeButton(props: {
  p: Placed;
  icon: IconName;
  name: string;
  meta: string;
  tone: string;
  label: string;
  selected: boolean;
  step?: number;
  waiting?: boolean;
  onSelect: (s: Selection) => void;
}) {
  const { p, icon, name, meta, tone, label, selected, step, waiting, onSelect } = props;
  return (
    <button className={`node tone-${tone} ${selected ? "selected" : ""}`} style={{ left: p.x, top: p.y }}
      aria-label={label} title={label} aria-pressed={selected} onClick={() => onSelect(p.sel)}>
      <span className="planet">
        <Icon name={icon} size={22} />
        {step !== undefined && <span className="step">{step}</span>}
        {waiting && <span className="ask-badge" aria-hidden="true">?</span>}
      </span>
      <span className="node-name">{name}</span>
      {meta && <span className="node-meta">{meta}</span>}
    </button>
  );
}

function RingLabel({ C, r, text, tone }: { C: number; r: number; text: string; tone: string }) {
  const style: CSSProperties = { top: C + r - 9 };
  return (
    <div className="ring-label" style={style} aria-hidden="true">
      <span className={`tone-text-${tone}`}>{text}</span>
    </div>
  );
}

/** Largest square that fits the element. */
function useSize(ref: React.RefObject<HTMLDivElement | null>) {
  const [size, setSize] = useState(720);
  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    const ro = new ResizeObserver(([entry]) => {
      const { width, height } = entry.contentRect;
      setSize(Math.floor(Math.min(width, height)));
    });
    ro.observe(el);
    return () => ro.disconnect();
  }, [ref]);
  return size;
}
