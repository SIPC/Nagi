import { Fragment, useEffect, useLayoutEffect, useRef, useState, type ReactNode } from "react";
import { useQuery } from "@tanstack/react-query";
import { Link, useNavigate, useSearch } from "@tanstack/react-router";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { errorMessage, nodeClient, projectClient } from "@/lib/api";
import { PageHeader } from "@/pages/layout";
import { fmtCpu, metricRanges, RangePicker, type MetricRange } from "@/components/metrics";
import { Legend, TimeChart, type ChartSeries, type Pt } from "@/components/chart";
import { stateOf } from "@/components/status";
import { ButtonLink, Empty, EmptyRow, HelpTip, Notice, QueryError, RowsSkeleton, layoutMotionCurve, layoutMotionMs, Roll, Section, Segmented, Status } from "@/components/ui";
import { cn } from "@/lib/utils";
import { locale, t } from "@/i18n";
import type { MetricSeries } from "@/gen/nagi/v1/common_pb";

const o = t.overview;

// 全局概览（docs/ui.md「全局概览」、docs/monitoring.md#全局概览）。数据都是节点存的监控序列：
// 网卡速率 node:net_rx、node:net_tx；和每个 mesh 成员之间的速率 m:<公钥>:rx、:tx，节点之间只取发送方的数；
// 对外 = 网卡 − 所有成员的速率（不小于 0），是估算值。

const refresh = 10_000;
const ago = (seconds: number) => timestampFromDate(new Date(Date.now() - seconds * 1000));

// ---- 格式 ----

// 到 1000 就进位，不到 10 留一位小数：640 KB/s、1.2 MB/s。
function scaled(v: number, units: string[]): [string, string] {
  let i = 0;
  while (v >= 1000 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return [v < 10 && i > 0 ? v.toFixed(1) : String(Math.round(v)), units[i]];
}
const rateParts = (v: number) => scaled(v, ["B/s", "KB/s", "MB/s", "GB/s", "TB/s"]);
const fmtRate = (v: number) => rateParts(v).join(" ");
const fmtTotal = (v: number) => scaled(v, ["B", "KB", "MB", "GB", "TB"]).join(" ");

// 已用 / 总量，两边都带单位，和节点卡片一样：2.0 GB / 15.5 GB、86 GB / 1.0 TB。
function fmtUsed(used: number, total: number) {
  const part = (v: number): [string, string] => {
    const units = ["B", "KB", "MB", "GB", "TB"];
    let i = 0;
    while (v >= 1000 && i < units.length - 1) {
      v /= 1024;
      i++;
    }
    return [v < 20 ? v.toFixed(1) : String(Math.round(v)), units[i]];
  };
  const [u, uUnit] = part(used);
  const [a, aUnit] = part(total);
  return `${u} ${uUnit} / ${a} ${aUnit}`;
}

// ---- 节点之间的流量 ----

// 一组速率。pairs 是 "发送节点|接收节点" → 字节/秒（只取发送方的数），rx、tx 是节点和所有成员（含面板）之间的合计。
type MeshRates = { pairs: Map<string, number>; rx: Map<string, number>; tx: Map<string, number> };

const newRates = (): MeshRates => ({ pairs: new Map(), rx: new Map(), tx: new Map() });
const inc = <K,>(m: Map<K, number>, k: K, v: number) => m.set(k, (m.get(k) ?? 0) + v);
const toHex = (b: Uint8Array) => Array.from(b, (x) => x.toString(16).padStart(2, "0")).join("");

// 面板没有自带节点时，节点和面板之间的流量单独算一行。
const panelRow = "__panel";

// MeshKeys：成员公钥 → 节点 ID。panel 是面板自己的 mesh 公钥，对应到面板自带的节点（同一台机器），没有自带节点时对应到 panelRow。
type MeshKeys = { peers: Map<string, string>; panel?: string };

// addRate：把节点 node 上的一条 m: 序列的值记进 rates。
// 节点之间只取发送方的数。面板自己不存指标，面板发出的（例如经面板下载节点上的文件）用节点这一端的接收量算；
// 面板在自带节点那台机器上时，这部分也计入那台机器的收发，从它的对外流量里扣掉。
function addRate(rates: MeshRates, node: string, name: string, value: number, keys: MeshKeys) {
  const m = /^m:([0-9a-f]+):(rx|tx)$/.exec(name);
  if (!m) return;
  const dir = m[2] as "rx" | "tx";
  inc(rates[dir], node, value);
  const peer = keys.peers.get(m[1]);
  if (!peer || peer === node) return;
  if (m[1] === keys.panel) {
    inc(rates.pairs, dir === "tx" ? `${node}|${peer}` : `${peer}|${node}`, value);
    if (peer !== panelRow) inc(rates[dir === "tx" ? "rx" : "tx"], peer, value);
    return;
  }
  if (dir === "tx") inc(rates.pairs, `${node}|${peer}`, value);
}

// ---- 桑基图 ----

const external = "__external";
const other = "__other";
// 节点超过这个数时只留流量最大的几个，其余合并成「其他」。
const sankeyMaxNodes = 8;

type Flow = { from: string; to: string; rate: number; kind: "ext" | "mesh" };
type FlowView = "all" | "ext" | "mesh";

function useWidth() {
  const ref = useRef<HTMLDivElement>(null);
  const [width, setWidth] = useState(0);
  useLayoutEffect(() => {
    const el = ref.current;
    if (!el) return;
    setWidth(el.clientWidth);
    const ro = new ResizeObserver(() => setWidth(el.clientWidth));
    ro.observe(el);
    return () => ro.disconnect();
  }, []);
  return { ref, width };
}

// 线宽只表示大致大小，准确数字看标注（docs/ui.md「流量桑基图」）。
const strokeOf = (rate: number) => Math.max(2, Math.sqrt(rate / 1024) * 0.24);

// 名称放不下时截短，完整名称在 title 里。汉字按 1em，其余按 0.58em 估算。
function fit(text: string, px: number, size: number) {
  let used = 0;
  for (let i = 0; i < text.length; i++) {
    used += (text.charCodeAt(i) > 255 ? 1 : 0.58) * size;
    if (used > px) return text.slice(0, Math.max(i - 1, 1)) + "…";
  }
  return text;
}

// mergeSmall：节点太多时，流量小的节点合并成「其他」。
function mergeSmall(rows: { id: string; name: string }[], flows: Flow[]) {
  const nodes = rows.filter((r) => r.id !== external);
  if (nodes.length <= sankeyMaxNodes) return { rows, flows };
  const total = new Map<string, number>();
  for (const f of flows) {
    inc(total, f.from, f.rate);
    inc(total, f.to, f.rate);
  }
  const keep = new Set(
    [...nodes]
      .sort((a, b) => (total.get(b.id) ?? 0) - (total.get(a.id) ?? 0))
      .slice(0, sankeyMaxNodes - 1)
      .map((n) => n.id),
  );
  const pick = (id: string) => (id === external || keep.has(id) ? id : other);
  const merged = new Map<string, Flow>();
  for (const f of flows) {
    const key = `${pick(f.from)}|${pick(f.to)}|${f.kind}`;
    const old = merged.get(key);
    if (old) old.rate += f.rate;
    else merged.set(key, { ...f, from: pick(f.from), to: pick(f.to) });
  }
  return { rows: [...rows.filter((r) => r.id === external || keep.has(r.id)), { id: other, name: o.flow.other }], flows: [...merged.values()] };
}

// layoutEase：和 AutoHeight 同一条曲线，CSS 的 cubic-bezier 在 JS 里的算法。
function layoutEase(p: number) {
  const [x1, y1, x2, y2] = layoutMotionCurve;
  const at = (t: number, a: number, b: number) => 3 * (1 - t) ** 2 * t * a + 3 * (1 - t) * t ** 2 * b + t ** 3;
  let lo = 0;
  let hi = 1;
  for (let i = 0; i < 16; i++) {
    const mid = (lo + hi) / 2;
    if (at(mid, x1, x2) < p) lo = mid;
    else hi = mid;
  }
  return at((lo + hi) / 2, y1, y2);
}

// useTween：一组数值变化时平滑过渡，中途又变了就从当前值接着走。
// 新出现的值从 seeds 里的起点开始，没有起点时线宽和透明度（:w、:op）从 0 开始，其他从目标值开始。
// 透明度错开：淡出在前半段走完，淡入在后半段才开始，新旧路径不会混在一起。
function useTween(target: Record<string, number>, seeds: Record<string, number>) {
  const signature = JSON.stringify(target);
  const [current, setCurrent] = useState(target);
  const latest = useRef(target);
  useEffect(() => {
    const to: Record<string, number> = JSON.parse(signature);
    const from = latest.current;
    const show = (next: Record<string, number>) => {
      latest.current = next;
      setCurrent(next);
    };
    if (matchMedia("(prefers-reduced-motion: reduce)").matches) return show(to);
    const start = performance.now();
    let frame = 0;
    const tick = (now: number) => {
      const p = Math.min(1, (now - start) / layoutMotionMs);
      const next: Record<string, number> = {};
      for (const k of Object.keys(to)) {
        const fade = k.endsWith(":op");
        const a = from[k] ?? seeds[k] ?? (fade || k.endsWith(":w") ? 0 : to[k]);
        const local = !fade || a === to[k] ? p : to[k] < a ? Math.min(1, p / 0.5) : Math.max(0, (p - 0.3) / 0.7);
        next[k] = a + (to[k] - a) * layoutEase(local);
      }
      show(next);
      if (p < 1) frame = requestAnimationFrame(tick);
    };
    frame = requestAnimationFrame(tick);
    return () => cancelAnimationFrame(frame);
    // seeds 只在这一轮新出现的值上用，和 signature 同时变。
  }, [signature]);
  return current;
}

const flowKey = (f: Flow) => `${f.from}|${f.to}|${f.kind}`;

// 路径断流后留一会儿（显示 0 B/s）再收起：流量小的路径时有时无，否则整张图隔几秒就换一次布局。
const flowHoldMs = 30000;

function useHeldFlows(flows: Flow[]) {
  const seen = useRef(new Map<string, { flow: Flow; at: number }>());
  const now = Date.now();
  for (const f of flows) seen.current.set(flowKey(f), { flow: f, at: now });
  const live = new Set(flows.map(flowKey));
  const held: Flow[] = [];
  for (const [k, v] of seen.current) {
    if (live.has(k)) continue;
    if (now - v.at > flowHoldMs) seen.current.delete(k);
    else held.push({ ...v.flow, rate: 0 });
  }
  return [...flows, ...held];
}

// Sankey：左边发出，右边接收，每个节点和外部网络单独一行，两边顺序、高度相同。按实际宽度算布局，不整体缩放。
// 切换视图时图不重建：不属于这个视图的路径和行淡出，其余的平滑换位。所以 flows 传全部路径，这里再按视图筛选；
// 断流保留也在筛选之前算，否则切走的路径会被当成断流留下来。
function Sankey({ rows: allRows, flows: allFlows, view }: { rows: { id: string; name: string }[]; flows: Flow[]; view: FlowView }) {
  const { ref, width } = useWidth();
  const held = useHeldFlows(allFlows);
  const merged = mergeSmall(allRows, held.filter((f) => view === "all" || f.kind === view));
  const rows = merged.rows;
  const flows = merged.flows;
  const order = new Map(rows.map((r, i) => [r.id, i]));
  const nameOf = (id: string) => rows.find((r) => r.id === id)?.name ?? id;
  const top = [...flows].sort((a, b) => b.rate - a.rate)[0];

  const label = Math.min(128, Math.round(width * 0.24));
  const x0 = label;
  const x3 = width - label;
  const x1 = x0 + Math.min(176, (x3 - x0) * 0.3);
  const x2 = x3 - Math.min(120, (x3 - x0) * 0.2);
  const cx = (x1 + x2) / 2;

  const leftGap = 22; // 起点上方要放标注。
  const rightGap = 4;
  const groupGap = 42;
  const stackHeight = (list: Flow[], gap: number) => (list.length === 0 ? 2 : list.reduce((sum, f) => sum + strokeOf(f.rate), 0) + (list.length - 1) * gap);
  // 先排每一行，再按行算每条线两端的位置。一行的高度取两边较高的一边。
  let y = 50;
  const groups = rows.map((r) => {
    const out = flows.filter((f) => f.from === r.id).sort((a, b) => order.get(a.to)! - order.get(b.to)!);
    const into = flows.filter((f) => f.to === r.id).sort((a, b) => order.get(a.from)! - order.get(b.from)!);
    const left = stackHeight(out, leftGap);
    const right = stackHeight(into, rightGap);
    const rowHeight = Math.max(left, right);
    const center = y + rowHeight / 2;
    y += rowHeight + groupGap;
    return { id: r.id, out, into, left, right, center };
  });
  const starts = new Map<Flow, number>();
  const ends = new Map<Flow, number>();
  for (const g of groups) {
    let py = g.center - g.left / 2;
    for (const f of g.out) {
      starts.set(f, py + strokeOf(f.rate) / 2);
      py += strokeOf(f.rate) + leftGap;
    }
    // 右边的线靠近名称那一行，比中心高 7。
    let ry = g.center - 7 - g.right / 2;
    for (const f of g.into) {
      ends.set(f, ry + strokeOf(f.rate) / 2);
      ry += strokeOf(f.rate) + rightGap;
    }
  }
  const sum = (list: Flow[]) => list.reduce((acc, f) => acc + f.rate, 0);
  // 最后一行可能有多条线，比名称那两行更低，取较低的一边，否则最下面的标注会被裁掉。
  const height = Math.max((groups[groups.length - 1]?.center ?? 0) + 13, y - groupGap + 2);
  const nameMax = Math.max(label - 8, 40);

  // 路径增减时其他线会换位置：位置、线宽都过渡（高度由外层的区块过渡），新路径淡入，消失的路径留到淡出结束。
  const leaving = useRef(new Map<string, { flow: Flow; y1: number; y2: number }>());
  const shown = useRef(new Map<string, { flow: Flow; y1: number; y2: number }>());
  type DrawnRow = { id: string; name: string; center: number; send: number; receive: number };
  const leavingRows = useRef(new Map<string, DrawnRow>());
  const shownRows = useRef(new Map<string, DrawnRow>());
  const lastNow = useRef<Record<string, number>>({});
  const next = new Map(flows.map((f) => [flowKey(f), { flow: f, y1: starts.get(f)!, y2: ends.get(f)! }]));
  for (const [k, v] of shown.current) if (!next.has(k)) leaving.current.set(k, v);
  for (const k of next.keys()) leaving.current.delete(k);
  shown.current = next;
  const goal: Record<string, number> = {};
  const nextRows = new Map(
    groups.map((g) => [
      g.id,
      { id: g.id, name: nameOf(g.id), center: g.center, send: sum(g.out), receive: sum(g.into) },
    ]),
  );
  for (const [k, v] of shownRows.current) if (!nextRows.has(k)) leavingRows.current.set(k, v);
  for (const k of nextRows.keys()) leavingRows.current.delete(k);
  shownRows.current = nextRows;
  for (const [k, v] of nextRows) Object.assign(goal, { [`row:${k}`]: v.center, [`row:${k}:op`]: 1 });
  for (const [k, v] of leavingRows.current) Object.assign(goal, { [`row:${k}`]: v.center, [`row:${k}:op`]: 0 });
  // 新路径从两端所在的行当前的位置长出来，消失的路径收回所在的行，线和行不脱节。
  const seeds: Record<string, number> = {};
  for (const [k, v] of next) {
    Object.assign(goal, { [`${k}:y1`]: v.y1, [`${k}:y2`]: v.y2, [`${k}:w`]: strokeOf(v.flow.rate), [`${k}:op`]: 1 });
    if (lastNow.current[`row:${v.flow.from}`] !== undefined) seeds[`${k}:y1`] = lastNow.current[`row:${v.flow.from}`];
    if (lastNow.current[`row:${v.flow.to}`] !== undefined) seeds[`${k}:y2`] = lastNow.current[`row:${v.flow.to}`];
  }
  for (const [k, v] of leaving.current) {
    Object.assign(goal, { [`${k}:y1`]: nextRows.get(v.flow.from)?.center ?? v.y1, [`${k}:y2`]: nextRows.get(v.flow.to)?.center ?? v.y2, [`${k}:w`]: 0, [`${k}:op`]: 0 });
  }
  const now = useTween(goal, seeds);
  lastNow.current = now;
  const [, setSettled] = useState(0);
  const allGone =
    leaving.current.size + leavingRows.current.size > 0 &&
    [...leaving.current.keys()].every((k) => (now[`${k}:op`] ?? 0) < 0.01) &&
    [...leavingRows.current.keys()].every((k) => (now[`row:${k}:op`] ?? 0) < 0.01);
  useEffect(() => {
    if (!allGone) return;
    leaving.current.clear();
    leavingRows.current.clear();
    setSettled((n) => n + 1);
  }, [allGone]);
  const drawn = [...next.entries(), ...leaving.current.entries()];

  return (
    <div ref={ref} className="relative min-w-0">
      {width > 0 && (
        <svg width={width} height={height} viewBox={`0 0 ${width} ${height}`} className="block overflow-visible" role="img" aria-label={top ? o.flow.aria(nameOf(top.from), nameOf(top.to), fmtRate(top.rate)) : o.flow.empty}>
          <text x={0} y={12} className="fill-muted-foreground text-xs">
            {o.flow.send}
          </text>
          <text x={width} y={12} textAnchor="end" className="fill-muted-foreground text-xs">
            {o.flow.receive}
          </text>
          {drawn.map(([key, { flow: f, y1: toY1, y2: toY2 }]) => {
            const w = now[`${key}:w`] ?? 0;
            const y1 = now[`${key}:y1`] ?? toY1;
            const y2 = now[`${key}:y2`] ?? toY2;
            return (
              <g key={key} className="group" opacity={now[`${key}:op`] ?? 0}>
                <title>{`${nameOf(f.from)} → ${nameOf(f.to)} ${fmtRate(f.rate)}`}</title>
                <path
                  d={`M${x0} ${y1} L${x1} ${y1} C ${cx} ${y1}, ${cx} ${y2}, ${x2} ${y2} L${x3} ${y2}`}
                  fill="none"
                  strokeWidth={w}
                  className={cn("transition-opacity duration-150 group-hover:opacity-100", f.kind === "ext" ? "stroke-wave opacity-60" : "stroke-muted-foreground opacity-45")}
                />
              </g>
            );
          })}
          {[...nextRows.values(), ...leavingRows.current.values()].map((n) => {
            const center = now[`row:${n.id}`] ?? n.center;
            return (
              <g key={n.id} opacity={now[`row:${n.id}:op`] ?? 0}>
                <title>{n.name}</title>
                <text x={0} y={center - 9} className="fill-foreground text-sm">
                  {fit(n.name, nameMax, 14)}
                </text>
                <text x={width} y={center - 9} textAnchor="end" className="fill-foreground text-sm">
                  {fit(n.name, nameMax, 14)}
                </text>
              </g>
            );
          })}
        </svg>
      )}
      {/* 数字放在图上面的一层：SVG 文字不能逐位滚动。读屏读下面的表格。 */}
      {width > 0 && (
        <div aria-hidden className="pointer-events-none absolute inset-0">
          {drawn.map(([key, { flow: f, y1: toY1 }]) => (
            <div
              key={key}
              className="absolute flex items-baseline gap-2 text-[13px] leading-4 whitespace-nowrap text-foreground"
              style={{ left: x0, top: (now[`${key}:y1`] ?? toY1) - (now[`${key}:w`] ?? 0) / 2 - 5 - 12, opacity: now[`${key}:op`] ?? 0 }}
            >
              {fit(nameOf(f.to), x1 - x0, 13)}
              <Roll value={fmtRate(f.rate)} n={f.rate} className="text-xs text-muted-foreground" />
            </div>
          ))}
          {[...nextRows.values(), ...leavingRows.current.values()].map((n) => {
            const top = (now[`row:${n.id}`] ?? n.center) + 8 - 12;
            const opacity = now[`row:${n.id}:op`] ?? 0;
            const cell = "absolute text-xs leading-4 whitespace-nowrap text-muted-foreground";
            return (
              <Fragment key={n.id}>
                <div className={cell} style={{ left: 0, top, opacity }}>
                  <Roll value={n.send > 0 ? fmtRate(n.send) : o.flow.noSend} n={n.send} />
                </div>
                <div className={cell} style={{ right: 0, top, opacity }}>
                  <Roll value={n.receive > 0 ? fmtRate(n.receive) : o.flow.noReceive} n={n.receive} />
                </div>
              </Fragment>
            );
          })}
        </div>
      )}
      {/* 读屏：图只有一句总结，具体路径在表格里。 */}
      <table className="sr-only">
        <caption>{o.flow.caption}</caption>
        <thead>
          <tr>
            <th>{o.flow.from}</th>
            <th>{o.flow.to}</th>
            <th>{o.flow.rate}</th>
          </tr>
        </thead>
        <tbody>
          {flows.map((f) => (
            <tr key={`${f.from}|${f.to}|${f.kind}`}>
              <td>{nameOf(f.from)}</td>
              <td>{nameOf(f.to)}</td>
              <td>{fmtRate(f.rate)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

// ---- 表格和数字 ----

function Stat({ label, value, n, unit, to, title }: { label: string; value: string | number; n?: number; unit?: string; to?: "/nodes" | "/projects"; title?: string }) {
  const inner = (
    <>
      <span className="text-muted-foreground">{label}</span>
      <span className="text-2xl leading-8 font-medium tracking-tight whitespace-nowrap">
        <Roll value={String(value)} n={n} />
        {unit && (
          <>
            {" "}
            <small className="text-sm leading-8 font-normal tracking-normal text-muted-foreground">
              <Roll value={unit} n={n} />
            </small>
          </>
        )}
      </span>
    </>
  );
  const box = "grid min-w-0 gap-1";
  return to ? (
    <Link to={to} className={cn(box, "rounded-sm outline-none focus-visible:ring-2 focus-visible:ring-ring/40")} title={title}>
      {inner}
    </Link>
  ) : (
    <div className={box} title={title}>
      {inner}
    </div>
  );
}

const thClass =
  "h-9 bg-[linear-gradient(var(--border),var(--border))] [background-position:bottom] [background-size:100%_1px] bg-no-repeat px-2 text-left text-[13px] font-normal whitespace-nowrap text-muted-foreground first:[background-position:right_bottom] first:[background-size:calc(100%-8px)_1px] last:[background-position:left_bottom] last:[background-size:calc(100%-8px)_1px]";
const tdClass = "h-9 truncate px-2 py-1 align-middle whitespace-nowrap text-muted-foreground transition-colors group-hover:bg-accent first:rounded-l-md last:rounded-r-md";

// 列宽跟内容走（cols 是除最后一列外的宽度），靠左排，多出来的宽度都留给最后一列。
function OverviewTable({ head, cols, children }: { head: string[]; cols: number[]; children: ReactNode }) {
  return (
    <div className="-mx-2 overflow-x-auto">
      <table className="w-full border-separate border-spacing-0 md:table-fixed">
        <colgroup>
          {head.map((h, i) => (
            <col key={h} style={{ width: cols[i] }} />
          ))}
        </colgroup>
        <thead>
          <tr>
            {head.map((h) => (
              <th key={h} className={thClass}>
                {h}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>{children}</tbody>
      </table>
    </div>
  );
}

// 图表：标题、图例，下面是图形和 5 个时间。节点之间的线多时只画最大的几条。图例折行时两张图的图形仍然对齐（subgrid）。
// 图例只占一行，左右两张图才对得齐：只列前两项，其余收成 +N，完整的在悬停提示里。
const trendLegendMax = 2;

function TrendChart({ title, help, series, from, to, step }: { title: string; help: string; series: ChartSeries[]; from: number; to: number; step: number }) {
  const rest = series.slice(trendLegendMax);
  return (
    <div className="row-span-2 grid min-w-0 grid-rows-subgrid gap-y-3">
      <div className="flex items-center justify-between gap-x-3">
        <span className="flex shrink-0 items-center gap-1 text-[13px] text-muted-foreground">
          {title}
          <HelpTip>{help}</HelpTip>
        </span>
        <span className="flex min-w-0 items-center gap-4 text-[13px] leading-4 whitespace-nowrap text-muted-foreground">
          <Legend series={series.slice(0, trendLegendMax)} className="min-w-0 flex-nowrap overflow-hidden" />
          {rest.length > 0 && (
            <span className="shrink-0" title={rest.map((s) => s.label).join("\n")}>
              +{rest.length}
            </span>
          )}
        </span>
      </div>
      <TimeChart series={series} from={from} to={to} step={step} format={fmtRate} />
    </div>
  );
}

const pairColors = ["text-foreground", "text-muted-foreground", "text-wave", "text-info"];

// ---- 页面 ----

// 节点上一条序列最新的值。
const latest = (s: MetricSeries | undefined) => (s && s.values.length > 0 ? s.values[s.values.length - 1] : undefined);

// liveRates：各节点 m: 序列最新的值合成当前速率。公钥对应还没取到时返回 undefined。
function liveRates(ids: string[], parts: ({ series: MetricSeries[] } | null)[] | undefined, keys: MeshKeys | undefined) {
  if (!parts || !keys) return undefined;
  const rates = newRates();
  parts.forEach((part, i) => {
    for (const series of part?.series ?? []) {
      const v = latest(series);
      if (v !== undefined && ids[i]) addRate(rates, ids[i], series.name, v, keys);
    }
  });
  return rates;
}

export function OverviewPage() {
  const search = useSearch({ strict: false }) as { flow?: "ext" | "mesh"; range?: MetricRange };
  const navigate = useNavigate();
  const view: FlowView = search.flow ?? "all";
  const range: MetricRange = search.range ?? "1h";
  const r = metricRanges.find((x) => x.value === range)!;

  const nodes = useQuery({ queryKey: ["nodes"], queryFn: () => nodeClient.listNodes({}), refetchInterval: refresh });
  const projects = useQuery({ queryKey: ["projects"], queryFn: () => projectClient.listProjects({}), refetchInterval: refresh });
  const all = (nodes.data?.nodes ?? []).filter((n) => !n.pending);
  const online = all.filter((n) => n.online);
  const onlineKey = online.map((n) => n.id).join(",");
  const projectList = projects.data?.projects ?? [];

  // 当前的网卡速率：取最近 1 分钟最新的一个点。
  const live = useQuery({
    queryKey: ["overview-net", onlineKey],
    queryFn: async () => {
      const parts = await Promise.all(online.map((n) => nodeClient.queryNodeMetrics({ nodeId: n.id, prefix: "node:net_", from: ago(60) }).catch(() => null)));
      return new Map(online.map((n, i) => [n.id, { rx: latest(parts[i]?.series.find((s) => s.name === "node:net_rx")), tx: latest(parts[i]?.series.find((s) => s.name === "node:net_tx")) }]));
    },
    enabled: online.length > 0,
    refetchInterval: refresh,
    placeholderData: (prev) => prev,
  });

  // 成员公钥 → 节点 ID，把 m: 序列对应到节点和面板。
  const meshKeys = useQuery({
    queryKey: ["overview-mesh-keys", onlineKey],
    queryFn: async (): Promise<MeshKeys> => {
      const [statuses, panel] = await Promise.all([
        Promise.all(online.map((n) => nodeClient.getNodeMesh({ nodeId: n.id }).catch(() => null))),
        nodeClient.getPanelMesh({}).catch(() => null),
      ]);
      const peers = new Map(statuses.flatMap((s, i) => (s ? [[toHex(s.publicKey), online[i].id] as const] : [])));
      if (!panel) return { peers };
      const key = toHex(panel.publicKey);
      peers.set(key, online.find((n) => n.ownEmbedded)?.id ?? panelRow);
      return { peers, panel: key };
    },
    enabled: online.length > 0,
    refetchInterval: 60_000,
    placeholderData: (prev) => prev,
  });
  const keys = meshKeys.data;

  // 当前的节点之间速率：最近 1 分钟最新的一个点。旧版本的节点没有 m: 序列。
  const liveMesh = useQuery({
    queryKey: ["overview-mesh", onlineKey],
    queryFn: async () => Promise.all(online.map((n) => nodeClient.queryNodeMetrics({ nodeId: n.id, prefix: "m:", from: ago(60) }).catch(() => null))),
    enabled: online.length > 0,
    refetchInterval: refresh,
    placeholderData: (prev) => prev,
  });

  // 节点之间的历史，和网卡历史同样的时间范围和步长，时刻对齐。
  const meshHistory = useQuery({
    queryKey: ["overview-mesh-history", onlineKey, range],
    queryFn: async () => Promise.all(online.map((n) => nodeClient.queryNodeMetrics({ nodeId: n.id, prefix: "m:", from: ago(r.seconds), step: BigInt(r.step) }).catch(() => null))),
    enabled: online.length > 0,
    refetchInterval: refresh,
    placeholderData: (prev) => prev,
  });

  // 趋势的网卡历史，按时间范围聚合。
  const history = useQuery({
    queryKey: ["overview-history", onlineKey, range],
    queryFn: async () => {
      const parts = await Promise.all(online.map((n) => nodeClient.queryNodeMetrics({ nodeId: n.id, prefix: "node:net_", from: ago(r.seconds), step: BigInt(r.step) }).catch(() => null)));
      return online.map((n, i) => ({ id: n.id, series: parts[i]?.series ?? [] }));
    },
    enabled: online.length > 0,
    refetchInterval: refresh,
    placeholderData: (prev) => prev,
  });

  // 端口映射的请求和流量，存在收到请求的节点上。
  const routeStep = 600;
  const routeSeries = useQuery({
    queryKey: ["overview-routes", onlineKey],
    queryFn: async () => {
      const parts = await Promise.all(online.map((n) => nodeClient.queryNodeMetrics({ nodeId: n.id, prefix: "r:", from: ago(86400), step: BigInt(routeStep) }).catch(() => null)));
      const totals = new Map<string, number>();
      for (const p of parts) {
        for (const s of p?.series ?? []) {
          inc(totals, s.name, s.values.reduce((sum, v) => sum + v * routeStep, 0));
        }
      }
      return totals;
    },
    enabled: online.length > 0,
    refetchInterval: refresh,
    placeholderData: (prev) => prev,
  });

  // ---- 汇总 ----

  const containers = projectList.flatMap((s) => (s.project?.containers ?? []).map((c) => stateOf((s.status?.services ?? []).filter((v) => v.container === c.name), c.deployedVersion > 0n)));
  const running = containers.filter((c) => c.tone === "success").length;
  const abnormal = containers.filter((c) => c.tone === "danger").length;
  const offlineNames = all.filter((n) => !n.online).map((n) => n.name);

  const sample = liveRates(online.map((n) => n.id), liveMesh.data, keys);
  const ready = !!sample && !!live.data;
  // 要升级才有节点之间的数据：查询成功但没有 m: 序列。
  const outdated = online.filter((_, i) => liveMesh.data?.[i]?.series.length === 0).map((n) => n.name);
  const extRate = (id: string, dir: "rx" | "tx") => Math.max(0, (live.data?.get(id)?.[dir] ?? 0) - (sample?.[dir].get(id) ?? 0));
  const extIn = online.reduce((sum, n) => sum + extRate(n.id, "rx"), 0);
  const extOut = online.reduce((sum, n) => sum + extRate(n.id, "tx"), 0);
  const meshTotal = [...(sample?.pairs.values() ?? [])].reduce((sum, v) => sum + v, 0);

  const flows: Flow[] = [];
  if (ready) {
    for (const n of online) {
      const inRate = extRate(n.id, "rx");
      const outRate = extRate(n.id, "tx");
      if (inRate >= 1) flows.push({ from: external, to: n.id, rate: inRate, kind: "ext" });
      if (outRate >= 1) flows.push({ from: n.id, to: external, rate: outRate, kind: "ext" });
    }
    for (const [key, rate] of sample.pairs) {
      const [from, to] = key.split("|");
      if (rate >= 1) flows.push({ from, to, rate, kind: "mesh" });
    }
  }
  const panelAlone = keys?.panel !== undefined && keys.peers.get(keys.panel) === panelRow;
  const sankeyRows = [
    ...(view === "mesh" ? [] : [{ id: external, name: o.flow.external }]),
    ...online.map((n) => ({ id: n.id, name: n.name })),
    ...(panelAlone ? [{ id: panelRow, name: o.flow.panel }] : []),
  ];

  // ---- 趋势 ----

  const now = Date.now() / 1000;
  const buckets = new Map<number, MeshRates>();
  if (keys) {
    (meshHistory.data ?? []).forEach((part, i) => {
      const id = online[i]?.id;
      if (!part || !id) return;
      for (const series of part.series) {
        series.values.forEach((v, j) => {
          const at = Number(series.times[j]);
          let b = buckets.get(at);
          if (!b) buckets.set(at, (b = newRates()));
          addRate(b, id, series.name, v, keys);
        });
      }
    });
  }
  const extSeries = (name: string, dir: "rx" | "tx"): Pt[] => {
    const total = new Map<number, number>();
    const count = new Map<number, number>();
    for (const part of history.data ?? []) {
      const s = part.series.find((x) => x.name === name);
      if (!s) continue;
      s.values.forEach((v, i) => {
        const at = Number(s.times[i]);
        const b = buckets.get(at);
        inc(total, at, Math.max(0, v - (b?.[dir].get(part.id) ?? 0)));
        inc(count, at, 1);
      });
    }
    const points = [...total.entries()].sort((a, b) => a[0] - b[0]);
    // 最后一个时段可能只有部分节点上报了，合计会突然变小，当作还没有数据。
    if (points.length > 1 && (count.get(points[points.length - 1][0]) ?? 0) < (count.get(points[points.length - 2][0]) ?? 0)) points.pop();
    return points;
  };
  const nameOf = (id: string) => (id === panelRow ? o.flow.panel : (all.find((n) => n.id === id)?.name ?? id));
  const pairTotals = new Map<string, number>();
  for (const b of buckets.values()) for (const [k, v] of b.pairs) inc(pairTotals, k, v);
  const pairKeys = [...pairTotals.entries()].sort((a, b) => b[1] - a[1]).map(([k]) => k);
  const topPairs = pairKeys.slice(0, pairColors.length);
  const pairSeries: ChartSeries[] = topPairs.map((k, i) => {
    const [a, b] = k.split("|");
    return { label: `${nameOf(a)} → ${nameOf(b)}`, className: pairColors[i], points: [...buckets.entries()].sort((x, y) => x[0] - y[0]).map(([at, v]): Pt => [at, v.pairs.get(k) ?? 0]) };
  });
  if (pairKeys.length > topPairs.length) {
    const rest = new Set(pairKeys.slice(topPairs.length));
    pairSeries.push({
      label: o.trend.other,
      className: "text-muted-foreground/50",
      points: [...buckets.entries()].sort((x, y) => x[0] - y[0]).map(([at, v]): Pt => [at, [...v.pairs.entries()].reduce((sum, [k, x]) => (rest.has(k) ? sum + x : sum), 0)]),
    });
  }

  // ---- 端口映射 ----

  const totals = routeSeries.data;
  const routeRows = projectList
    .flatMap((s) =>
      (s.project?.routes ?? []).map((route) => {
        const key = (m: string) => totals?.get(`r:${s.project!.id}/${route.name}:${m}`) ?? 0;
        return { project: s.project!, route, req: key("req"), err: key("5xx"), in: key("in"), out: key("out") };
      }),
    )
    .filter((x) => x.req > 0 || x.in + x.out > 0)
    .sort((a, b) => b.in + b.out - (a.in + a.out) || b.req - a.req)
    .slice(0, 5);

  const setSearch = (patch: { flow?: "ext" | "mesh"; range?: MetricRange }) => navigate({ to: "/overview", search: (s) => ({ ...s, ...patch }), replace: true });

  const health =
    all.length === 0 ? (
      <Status>{o.health.noNodes}</Status>
    ) : abnormal > 0 ? (
      <Status tone="danger">{o.health.containersAbnormal(abnormal)}</Status>
    ) : offlineNames.length > 0 ? (
      <Status tone="warning">{t.common.status.nodesOffline(offlineNames)}</Status>
    ) : (
      <Status tone="success">{o.health.ok}</Status>
    );

  const dash = "—";
  const [inValue, inUnit] = rateParts(extIn);
  const [outValue, outUnit] = rateParts(extOut);
  const [meshValue, meshUnit] = rateParts(meshTotal);
  const noNodes = nodes.isSuccess && nodes.data.nodes.length === 0;

  return (
    <>
      <PageHeader
        title={o.title}
        description={
          <span className="flex flex-wrap items-center gap-x-4 gap-y-1">
            {nodes.isSuccess && <span className="flex [&>span]:text-[13px] [&>span]:leading-5">{health}</span>}
            <span>{o.updates}</span>
          </span>
        }
      />
      {nodes.error && <QueryError className="mb-6" error={errorMessage(nodes.error)} onRetry={() => nodes.refetch()} />}
      <div className="grid grid-cols-[repeat(auto-fit,minmax(112px,1fr))] gap-4 md:flex md:flex-wrap md:gap-x-12">
        <Stat label={o.stats.nodes} to="/nodes" value={nodes.data ? online.length : dash} unit={nodes.data ? `/ ${all.length}` : undefined} />
        <Stat label={o.stats.containers} to="/projects" value={projects.data ? running : dash} unit={projects.data ? `/ ${containers.length}` : undefined} />
        <Stat label={o.stats.projects} to="/projects" value={projects.data ? projectList.length : dash} />
        <Stat label={o.stats.extIn} title={o.stats.estimated} value={ready ? inValue : dash} n={extIn} unit={ready ? inUnit : undefined} />
        <Stat label={o.stats.extOut} title={o.stats.estimated} value={ready ? outValue : dash} n={extOut} unit={ready ? outUnit : undefined} />
        <Stat label={o.stats.mesh} title={o.stats.meshTitle} value={sample ? meshValue : dash} n={meshTotal} unit={sample ? meshUnit : undefined} />
      </div>

      {noNodes ? (
        <div className="mt-6 border-t pt-6">
          <Empty
            title={o.noNodes.title}
            action={
              <ButtonLink to="/nodes" variant="outline" readOnlyOk>
                {o.noNodes.action}
              </ButtonLink>
            }
          >
            {o.noNodes.text}
          </Empty>
        </div>
      ) : (
        <>
          <Section
            title={o.flow.title}
            help={o.flow.help}
            actions={
              <Segmented
                label={o.flow.viewLabel}
                value={view}
                onChange={(v) => setSearch({ flow: v === "all" ? undefined : v })}
                items={[
                  { value: "all", label: o.flow.views.all },
                  { value: "ext", label: o.flow.views.ext },
                  { value: "mesh", label: o.flow.views.mesh },
                ]}
              />
            }
          >
            {outdated.length > 0 && <Notice tone="warning">{o.flow.outdated(outdated.join(o.flow.separator))}</Notice>}
            {nodes.isPending ? (
              <RowsSkeleton />
            ) : online.length === 0 ? (
              <EmptyRow>{o.flow.empty}</EmptyRow>
            ) : !ready ? (
              <RowsSkeleton />
            ) : flows.length === 0 ? (
              <EmptyRow>{o.flow.empty}</EmptyRow>
            ) : (
              <Sankey rows={sankeyRows} flows={flows} view={view} />
            )}
          </Section>

          <Section
            title={o.trend.title}
            actions={
              <RangePicker value={range} onChange={(v) => setSearch({ range: v === "1h" ? undefined : v })} />
            }
          >
            <div className="grid grid-cols-[repeat(auto-fit,minmax(min(100%,320px),1fr))] gap-8">
              <TrendChart
                title={o.trend.ext}
                help={o.trend.extHelp}
                from={now - r.seconds}
                to={now}
                step={r.step}
                series={[
                  { label: o.trend.inbound, className: "text-muted-foreground", points: extSeries("node:net_rx", "rx") },
                  { label: o.trend.outbound, className: "text-wave", points: extSeries("node:net_tx", "tx") },
                ]}
              />
              <TrendChart title={o.trend.mesh} help={o.trend.meshHelp} from={now - r.seconds} to={now} step={r.step} series={pairSeries} />
            </div>
          </Section>

          <Section
            title={o.nodes.title}
            actions={
              <ButtonLink to="/nodes" variant="ghost" className="-mr-3" readOnlyOk>
                {o.nodes.viewAll}
              </ButtonLink>
            }
          >
            {nodes.isPending ? (
              <RowsSkeleton />
            ) : (
              <OverviewTable cols={[176, 80, 72, 136, 136, 104, 104]} head={[o.nodes.name, o.nodes.status, o.nodes.cpu, o.nodes.memory, o.nodes.disk, o.nodes.inbound, o.nodes.outbound, o.nodes.containers]}>
                {(nodes.data?.nodes ?? []).map((n) => {
                  const u = n.online ? n.usage : undefined;
                  const memTotal = Number(n.info?.memoryBytes ?? 0n);
                  const disks = u?.disks ?? [];
                  const diskTotal = disks.reduce((sum, d) => sum + Number(d.totalBytes), 0) || Number(n.info?.diskBytes ?? 0n);
                  const diskUsed = disks.length > 0 ? disks.reduce((sum, d) => sum + Number(d.usedBytes), 0) : ((u?.disk ?? 0) / 100) * diskTotal;
                  const net = n.online ? live.data?.get(n.id) : undefined;
                  return (
                    <tr key={n.id} className="group">
                      <td className={cn(tdClass, "text-foreground")} title={n.name}>
                        <Link to="/nodes/$nodeId" params={{ nodeId: n.id }}>
                          {n.name}
                        </Link>
                      </td>
                      <td className={tdClass}>{n.pending ? <Status>{o.nodes.pending}</Status> : n.online ? <Status tone="success">{o.nodes.online}</Status> : <Status tone="warning">{o.nodes.offline}</Status>}</td>
                      <td className={tdClass}><Roll value={u ? fmtCpu(u.cpu) : dash} /></td>
                      <td className={tdClass}><Roll value={u ? (memTotal ? fmtUsed((u.memory / 100) * memTotal, memTotal) : fmtCpu(u.memory)) : dash} n={u?.memory} /></td>
                      <td className={tdClass}><Roll value={u ? (diskTotal ? fmtUsed(diskUsed, diskTotal) : fmtCpu(u.disk)) : dash} n={u?.disk} /></td>
                      <td className={tdClass}><Roll value={net?.rx !== undefined ? fmtRate(net.rx) : dash} n={net?.rx} /></td>
                      <td className={tdClass}><Roll value={net?.tx !== undefined ? fmtRate(net.tx) : dash} n={net?.tx} /></td>
                      <td className={tdClass}><Roll value={String(n.containers)} /></td>
                    </tr>
                  );
                })}
              </OverviewTable>
            )}
          </Section>

          <Section title={o.routes.title} help={o.routes.help}>
            {routeRows.length === 0 ? (
              online.length > 0 && routeSeries.isPending ? (
                <RowsSkeleton count={1} />
              ) : (
                <EmptyRow>{o.routes.empty}</EmptyRow>
              )
            ) : (
              <OverviewTable cols={[176, 160, 136, 104, 72, 104]} head={[o.routes.mapping, o.routes.project, o.routes.container, o.routes.requests, o.routes.errorRate, o.routes.inbound, o.routes.outbound]}>
                {routeRows.map((x) => (
                  <tr key={`${x.project.id}/${x.route.name}`} className="group">
                    <td className={cn(tdClass, "font-mono text-[13px] text-foreground")}>{x.route.name}</td>
                    <td className={cn(tdClass, "text-foreground")}>
                      <Link to="/projects/$projectId" params={{ projectId: x.project.id }}>
                        {x.project.name}
                      </Link>
                    </td>
                    <td className={tdClass}>{x.route.container}</td>
                    <td className={tdClass}><Roll value={t.metrics.count(Math.round(x.req).toLocaleString(locale))} n={x.req} /></td>
                    <td className={tdClass}><Roll value={x.req > 0 ? `${((100 * x.err) / x.req).toFixed(1)}%` : dash} /></td>
                    <td className={tdClass}><Roll value={fmtTotal(x.in)} n={x.in} /></td>
                    <td className={tdClass}><Roll value={fmtTotal(x.out)} n={x.out} /></td>
                  </tr>
                ))}
              </OverviewTable>
            )}
          </Section>
        </>
      )}
    </>
  );
}
