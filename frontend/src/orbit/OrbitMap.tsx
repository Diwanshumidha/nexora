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
  /** Installed extensions with a catalog update, badged on the add slot. */
  updates: number;
  selected: Selection;
  onSelect: (s: Selection) => void;
  /** Opens the History tab; shown as an "all runs" node when there are more. */
  onOpenHistory?: () => void;
}

/** Track each extension gets along its ring, in px: a node plus its label
 *  and breathing room. A ring that can't give everyone this much hands the
 *  rest to a new ring outside it. */
const NODE_SPACING = 140;
/** Extensions a ring holds before the next ring starts (space allowing). */
const NODES_PER_RING = 6;
/** Below this spacing, nodes shrink and drop their status captions; below
 *  DENSE_SPACING their names only show on hover or focus. */
const COMPACT_SPACING = 112;
const DENSE_SPACING = 84;
/** Smallest gap between neighbouring rings on the short axis, in px. */
const RING_GAP = 54;
/** Most extension rings drawn; past this the outer ring just gets denser. */
const MAX_EXT_RINGS = 4;
/** Length of track kept free at the bottom of each ring for its label and
 *  the history runs below it (at full size). */
const LABEL_GAP = 210;
/** How far the orbit may stretch into an ellipse, as width / height: wide
 *  windows get a tilted orrery, narrow ones a slightly tall one. */
const ASPECT_MIN = 0.85, ASPECT_MAX = 1.75;
/** Room around the outer ring for node labels (x) and the history captions
 *  and label under it (y). */
const MARGIN_X = 64, MARGIN_TOP = 28, MARGIN_BOTTOM = 66;
/** Runs shown on the outer ring (newest first; running ones always lead).
 *  The rest are one click away in the History tab. */
export const HISTORY_SLOTS = 3;

/**
 * An ellipse centred on the core, with lookup tables so points can be placed
 * at equal distances along it (equal angles bunch up at the ends of a wide
 * ellipse). Distance s is measured clockwise from the bottom point.
 */
class Orbit {
  readonly perimeter: number;
  private readonly lens: Float64Array;
  private static readonly STEPS = 720;
  constructor(readonly cx: number, readonly cy: number, readonly rx: number, readonly ry: number) {
    const n = Orbit.STEPS;
    this.lens = new Float64Array(n + 1);
    let prev = this.raw(0);
    for (let i = 1; i <= n; i++) {
      const p = this.raw((i / n) * 2 * Math.PI);
      this.lens[i] = this.lens[i - 1] + Math.hypot(p.x - prev.x, p.y - prev.y);
      prev = p;
    }
    this.perimeter = this.lens[n];
  }
  /** Point at angle t past the bottom, clockwise on screen. */
  private raw(t: number) {
    const a = Math.PI / 2 + t;
    return { x: this.cx + this.rx * Math.cos(a), y: this.cy + this.ry * Math.sin(a) };
  }
  /** Point at distance s along the ring from the bottom. */
  at(s: number) {
    const L = this.perimeter;
    s = ((s % L) + L) % L;
    let lo = 0, hi = Orbit.STEPS;
    while (hi - lo > 1) {
      const mid = (lo + hi) >> 1;
      if (this.lens[mid] < s) lo = mid; else hi = mid;
    }
    const f = (s - this.lens[lo]) / (this.lens[hi] - this.lens[lo] || 1);
    return this.raw(((lo + f) / Orbit.STEPS) * 2 * Math.PI);
  }
  /** Track left for nodes once the label gap at the bottom is kept free. */
  gap = LABEL_GAP;
  get usable() {
    return Math.max(this.perimeter - this.gap, 1);
  }
  /** Position of node i of n, spread evenly over the usable track. */
  spread(i: number, n: number, offset = 0) {
    return this.at(this.gap / 2 + ((i + 0.5 + offset) * this.usable) / Math.max(n, 1));
  }
}

/**
 * Extension rings between the network ring and the history ring. Each ring
 * holds what fits at NODE_SPACING; when the extensions don't fit, another
 * ring is added (up to what the space allows), and alternate rings are
 * staggered half a step so nodes never line up across rings.
 */
function planExtRings(n: number, minR: number, maxR: number, k: number, cx: number, cy: number, z: number) {
  const make = (r: number) => {
    const o = new Orbit(cx, cy, r * k, r);
    o.gap = LABEL_GAP * z;
    return o;
  };
  const radii = (count: number) =>
    count === 1 ? [minR + (maxR - minR) * 0.7] : Array.from({ length: count }, (_, j) => minR + ((maxR - minR) * j) / (count - 1));
  const fits = Math.max(1, Math.min(MAX_EXT_RINGS, Math.floor((maxR - minR) / (RING_GAP * z)) + 1));
  // Start from about NODES_PER_RING per ring, so the system reads as layered
  // orbits rather than one crowded loop; add rings if that's still too dense.
  const start = Math.min(fits, Math.max(1, Math.ceil(n / NODES_PER_RING)));
  let rings = radii(start).map(make);
  for (let count = start; count <= fits; count++) {
    rings = radii(count).map(make);
    const capacity = rings.reduce((sum, o) => sum + Math.floor(o.usable / (NODE_SPACING * z)), 0);
    if (capacity >= n) break;
  }
  // Share nodes by track length, so every ring is about as dense.
  const total = rings.reduce((sum, o) => sum + o.usable, 0);
  const counts = rings.map((o) => Math.floor((n * o.usable) / total));
  for (let left = n - counts.reduce((a, b) => a + b, 0), j = rings.length - 1; left > 0; left--, j = (j - 1 + rings.length) % rings.length) counts[j]++;
  // Turn each ring so its nodes sit as far as possible from the ring inside
  // it: with different counts, a fixed half-step can still line nodes up.
  const placed: { x: number; y: number }[] = [];
  return rings
    .map((orbit, j) => {
      const n = counts[j];
      let best = 0, bestGap = -1;
      // At most ±0.3 of a step, so no node is turned into the label gap.
      for (let step = 0; step <= 12 && placed.length; step++) {
        const offset = step / 20 - 0.3;
        let gap = Infinity;
        for (let i = 0; i < n; i++) {
          const p = orbit.spread(i, n, offset);
          for (const q of placed) gap = Math.min(gap, Math.hypot(p.x - q.x, p.y - q.y));
        }
        if (gap > bestGap) [best, bestGap] = [offset, gap];
      }
      for (let i = 0; i < n; i++) placed.push(orbit.spread(i, n, best));
      return { orbit, count: n, offset: best };
    })
    .filter((r) => r.count > 0);
}

interface Placed {
  key: string;
  sel: Selection;
  x: number;
  y: number;
}

export const MODE_LABEL: Record<string, string> = { trusted: "trusted", ask: "ask first", readonly: "read only" };

export const OrbitMap = memo(function OrbitMap({ network, extensions, history, modes, approvals, updates, selected, onSelect, onOpenHistory }: Props) {
  const box = useRef<HTMLDivElement>(null);
  const { w, h } = useSize(box);
  const W = Math.max(w, 360), H = Math.max(h, 360);

  const layout = useMemo(() => {
    const cx = W / 2;
    const cy = MARGIN_TOP + (H - MARGIN_TOP - MARGIN_BOTTOM) / 2;
    // The outer (history) ring fills the box; every ring shares its shape.
    const fitX = W / 2 - MARGIN_X, fitY = (H - MARGIN_TOP - MARGIN_BOTTOM) / 2;
    const k = Math.min(ASPECT_MAX, Math.max(ASPECT_MIN, fitX / fitY));
    const rOuter = Math.max(150, Math.min(fitY, fitX / k));
    const ring = (r: number) => new Orbit(cx, cy, r * k, r);
    // Clearances shrink with the space: full size from an outer radius of
    // 320 px down to 75% in small windows.
    const z = Math.min(1, Math.max(0.75, rOuter / 320));

    // Network nodes sit beside the core with their labels below; too small a
    // ring would put those labels on the core.
    const net = ring(Math.max(rOuter * 0.3, 124 * z, 100));
    const slots = [...extensions.map((e) => e.manifest!.id), "+add"];
    const minExt = net.ry + 78 * z;
    const extRings = planExtRings(slots.length, minExt, Math.max(minExt, rOuter - 60 * z), k, cx, cy, z);
    const lastExt = extRings[extRings.length - 1].orbit;
    const hist = ring(Math.max(rOuter, lastExt.ry + 56 * z));
    const spacing = Math.min(...extRings.map((r) => r.orbit.usable / r.count));
    const density = spacing < DENSE_SPACING ? "dense" : spacing < COMPACT_SPACING ? "compact" : "";

    const netNodes: Placed[] = network.map((n, i) => {
      // Across the upper half, so labels drawn below each node clear the core.
      const t = network.length === 1 ? 0.5 : 0.2 + (0.6 * i) / (network.length - 1);
      return { key: n.id, sel: { kind: "network", id: n.id }, ...net.at(net.perimeter * (0.5 + (t - 0.5) * 0.66)) };
    });

    let next = 0;
    const ext: Placed[] = extRings.flatMap(({ orbit, count, offset }) =>
      Array.from({ length: count }, (_, i) => {
        const id = slots[next++];
        return {
          key: id,
          sel: id === "+add" ? ({ kind: "add" } as const) : ({ kind: "extension", id } as const),
          ...orbit.spread(i, count, offset),
        };
      }),
    );

    // The latest runs, grouped at the bottom of the history ring (inside the
    // extension rings' label gap), ~56 px apart, newest on the left.
    const shown = history.slice(0, HISTORY_SLOTS);
    const more = history.length > shown.length && !!onOpenHistory;
    const histSlots = shown.length + (more ? 1 : 0);
    const onHist = (i: number) => hist.at(-(i - (histSlots - 1) / 2) * 56);
    const histNodes: Placed[] = shown.map((r, i) => ({ key: r.id, sel: { kind: "history", id: r.id }, ...onHist(i) }));
    const moreAt = more ? onHist(shown.length) : null;

    return { cx, cy, net, extRings: extRings.map((r) => r.orbit), hist, netNodes, ext, histNodes, moreAt, compact: density !== "", density };
  }, [W, H, network, extensions, history, onOpenHistory]);
  const { cx, cy } = layout;

  const extById = new Map(extensions.map((e) => [e.manifest!.id, e]));
  const extByNs = new Map(extensions.map((e) => [e.manifest!.namespace ?? e.manifest!.id, e.manifest!.id]));
  const netById = new Map(network.map((n) => [n.id, n]));
  const histById = new Map(history.map((h) => [h.id, h]));
  const liveNet = layout.netNodes.find((p) => netById.get(p.key)?.active);

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
  const tracePts = [{ x: cx, y: cy }, ...traceIds.map((id) => layout.ext.find((p) => p.key === id)!).filter(Boolean)];

  const isSel = (s: Selection) =>
    s.kind === selected.kind && ("id" in s ? "id" in selected && s.id === selected.id : true);

  const anyLive = history.some((h) => h.state === "running" || h.state === "queued");
  const waitingForYou = approvals.length > 0;

  return (
    <div ref={box} className="orbit-box">
      <div className={`orbit ${layout.compact ? "compact" : ""} ${layout.density}`} style={{ width: W, height: H }}>
        <svg className="orbit-svg" width={W} height={H} aria-hidden="true">
          {/* rings */}
          <Ellipse o={layout.hist} className="ring ring-history" />
          {layout.extRings.map((o) => <Ellipse key={o.ry} o={o} className="ring ring-ext" />)}
          <Ellipse o={layout.net} className="ring ring-net" />
          {/* live network link */}
          {liveNet && (
            <line x1={cx} y1={cy} x2={liveNet.x} y2={liveNet.y}
              className={`link-live ${netById.get(liveNet.key)?.state === "up" ? "" : "link-pending"}`} />
          )}
          {/* trace of the selected run */}
          {tracePts.slice(1).map((p, i) => (
            <line key={i} x1={tracePts[i].x} y1={tracePts[i].y} x2={p.x} y2={p.y} className="trace" />
          ))}
        </svg>

        {/* ring labels, in the gap at the bottom of each ring */}
        <RingLabel y={cy + layout.net.ry} text="NETWORK" tone="net" />
        <RingLabel y={cy + layout.extRings[layout.extRings.length - 1].ry} text="EXTENSIONS" tone="ext" />
        <RingLabel y={cy + layout.hist.ry + 44} text="HISTORY" tone="muted" />

        {/* history ring: the latest runs */}
        {layout.histNodes.map((p) => {
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

        {/* network providers */}
        {layout.netNodes.map((p) => {
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
                aria-label={updates ? `Add an extension (${updates} update${updates > 1 ? "s" : ""} available)` : "Add an extension"}
                onClick={() => onSelect(p.sel)}>
                <Icon name="plus" />
                {updates > 0 && <span className="step" aria-hidden="true">{updates}</span>}
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
              className="node-ext"
            />
          );
        })}

        {/* core */}
        <button
          className={`core ${isSel({ kind: "core" }) ? "selected" : ""} ${anyLive ? "busy" : ""}`}
          style={{ left: cx, top: cy }}
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
  className?: string;
}) {
  const { p, icon, name, meta, tone, label, selected, step, waiting, onSelect, className = "" } = props;
  return (
    <button className={`node ${className} tone-${tone} ${selected ? "selected" : ""}`} style={{ left: p.x, top: p.y }}
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

function Ellipse({ o, className }: { o: Orbit; className: string }) {
  return <ellipse cx={o.cx} cy={o.cy} rx={o.rx} ry={o.ry} className={className} />;
}

function RingLabel({ y, text, tone }: { y: number; text: string; tone: string }) {
  const style: CSSProperties = { top: y - 9 };
  return (
    <div className="ring-label" style={style} aria-hidden="true">
      <span className={`tone-text-${tone}`}>{text}</span>
    </div>
  );
}

/** The element's content box, tracked as it resizes. */
function useSize(ref: React.RefObject<HTMLDivElement | null>) {
  const [size, setSize] = useState({ w: 960, h: 720 });
  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    const ro = new ResizeObserver(([entry]) => {
      const { width, height } = entry.contentRect;
      setSize((cur) => (Math.abs(cur.w - width) < 1 && Math.abs(cur.h - height) < 1 ? cur : { w: Math.floor(width), h: Math.floor(height) }));
    });
    ro.observe(el);
    return () => ro.disconnect();
  }, [ref]);
  return size;
}
