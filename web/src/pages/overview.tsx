import { useLayoutEffect, useRef, useState, type ReactNode } from "react";
import { useQuery } from "@tanstack/react-query";
import { Link, useNavigate, useSearch } from "@tanstack/react-router";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { errorMessage, nodeClient, projectClient } from "@/lib/api";
import { PageHeader } from "@/pages/layout";
import { fmtCpu, metricRanges, RangePicker, type MetricRange } from "@/components/metrics";
import { Legend, TimeChart, type ChartSeries, type Pt } from "@/components/chart";
import { stateOf } from "@/components/status";
import { ButtonLink, Empty, EmptyRow, HelpTip, QueryError, RowsSkeleton, Section, Status } from "@/components/ui";
import { cn } from "@/lib/utils";
import { locale, t } from "@/i18n";
import type { Node } from "@/gen/nagi/v1/api_pb";
import type { MetricSeries } from "@/gen/nagi/v1/common_pb";

const o = t.overview;

// 全局概览（docs/ui.md「全局概览」、docs/monitoring.md#全局概览）。数据都来自现有接口：
// 节点网卡速率来自 node:net_rx、node:net_tx；节点之间的速率由各节点 mesh 对端的累计字节两次差值算出，只取发送方的数；
// 对外 = 网卡 − 所有对端的速率（不小于 0），是估算值。

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

// 已用 / 总量：单位相同时只写一次（1.7 / 15.5 GB），不同时各写各的（86 GB / 1 TB）。
function fmtUsed(used: number, total: number) {
  const part = (v: number): [string, string] => {
    const units = ["B", "KB", "MB", "GB", "TB"];
    let i = 0;
    while (v >= 1000 && i < units.length - 1) {
      v /= 1024;
      i++;
    }
    return [String(v < 20 ? Number(v.toFixed(1)) : Math.round(v)), units[i]];
  };
  const [u, uUnit] = part(used);
  const [a, aUnit] = part(total);
  return uUnit === aUnit ? `${u} / ${a} ${aUnit}` : `${u} ${uUnit} / ${a} ${aUnit}`;
}

// ---- 节点之间的采样 ----

// 一次采样：各方向的速率。pairs 是 "发送节点|接收节点" → 字节/秒（只取发送方的数），rx、tx 是节点和所有对端（含面板）之间的合计。
type MeshSample = { at: number; pairs: Map<string, number>; rx: Map<string, number>; tx: Map<string, number> };

// 放在模块里：切到别的页面再回来，已经累积的采样还在；刷新页面才重新开始。
const meshCounters = new Map<string, { at: number; rx: number; tx: number }>();
const meshSamples: MeshSample[] = [];
const keepSamples = 8640; // 10 秒一次，留 24 小时。
const maxSampleGap = 60; // 隔太久（标签页在后台）的两次差值是一段长时间的平均，不当作当前速率。

const inc = <K,>(m: Map<K, number>, k: K, v: number) => m.set(k, (m.get(k) ?? 0) + v);

async function sampleMesh(nodes: Node[]): Promise<MeshSample | null> {
  const statuses = await Promise.all(nodes.map((n) => nodeClient.getNodeMesh({ nodeId: n.id }).catch(() => null)));
  const byAddr = new Map<string, string>();
  statuses.forEach((s, i) => s && byAddr.set(s.address, nodes[i].id));
  const byName = new Map(nodes.map((n) => [n.name, n.id]));
  const at = Date.now() / 1000;
  const sample: MeshSample = { at, pairs: new Map(), rx: new Map(), tx: new Map() };
  let peers = 0;
  let known = false;
  statuses.forEach((s, i) => {
    if (!s) return;
    const id = nodes[i].id;
    for (const p of s.peers) {
      peers++;
      const key = `${id}|${p.address}`;
      const prev = meshCounters.get(key);
      const dt = prev ? at - prev.at : 0;
      if (prev && dt < 1) continue;
      const rx = Number(p.rxBytes);
      const tx = Number(p.txBytes);
      meshCounters.set(key, { at, rx, tx });
      // 计数器变小说明对端重启过，这一轮不算。
      if (!prev || dt > maxSampleGap || rx < prev.rx || tx < prev.tx) continue;
      known = true;
      const rxRate = (rx - prev.rx) / dt;
      const txRate = (tx - prev.tx) / dt;
      inc(sample.rx, id, rxRate);
      inc(sample.tx, id, txRate);
      const peer = byAddr.get(p.address) ?? byName.get(p.name);
      if (peer && peer !== id) inc(sample.pairs, `${id}|${peer}`, txRate);
    }
  });
  // 有对端却都还算不出速率（第一次采样）时等下一次。只有一个节点、没有对端时节点之间就是 0，不用等。
  if (peers > 0 && !known) return null;
  meshSamples.push(sample);
  if (meshSamples.length > keepSamples) meshSamples.splice(0, meshSamples.length - keepSamples);
  return sample;
}

// meshBuckets：把采样按图表步长取平均，和节点按步长聚合的时刻对齐。
function meshBuckets(step: number) {
  const buckets = new Map<number, { n: number; pairs: Map<string, number>; rx: Map<string, number>; tx: Map<string, number> }>();
  for (const s of meshSamples) {
    const key = Math.floor(s.at / step) * step;
    let b = buckets.get(key);
    if (!b) buckets.set(key, (b = { n: 0, pairs: new Map(), rx: new Map(), tx: new Map() }));
    b.n++;
    for (const [k, v] of s.pairs) inc(b.pairs, k, v);
    for (const [k, v] of s.rx) inc(b.rx, k, v);
    for (const [k, v] of s.tx) inc(b.tx, k, v);
  }
  return buckets;
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

// Sankey：左边发出，右边接收，每个节点和外部网络单独一行，两边顺序、高度相同。按实际宽度算布局，不整体缩放。
function Sankey({ rows: allRows, flows: allFlows }: { rows: { id: string; name: string }[]; flows: Flow[] }) {
  const { ref, width } = useWidth();
  const { rows, flows } = mergeSmall(allRows, allFlows);
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
  const height = (groups[groups.length - 1]?.center ?? 0) + 13;
  const nameMax = Math.max(label - 8, 40);

  return (
    <div ref={ref} className="min-w-0">
      {width > 0 && (
        <svg width={width} height={height} viewBox={`0 0 ${width} ${height}`} className="block" role="img" aria-label={top ? o.flow.aria(nameOf(top.from), nameOf(top.to), fmtRate(top.rate)) : o.flow.empty}>
          <text x={0} y={12} className="fill-muted-foreground text-xs">
            {o.flow.send}
          </text>
          <text x={width} y={12} textAnchor="end" className="fill-muted-foreground text-xs">
            {o.flow.receive}
          </text>
          {flows.map((f) => {
            const w = strokeOf(f.rate);
            const y1 = starts.get(f)!;
            const y2 = ends.get(f)!;
            return (
              <g key={`${f.from}|${f.to}|${f.kind}`} className="group">
                <title>{`${nameOf(f.from)} → ${nameOf(f.to)} ${fmtRate(f.rate)}`}</title>
                <path
                  d={`M${x0} ${y1} L${x1} ${y1} C ${cx} ${y1}, ${cx} ${y2}, ${x2} ${y2} L${x3} ${y2}`}
                  fill="none"
                  strokeWidth={w}
                  className={cn("transition-opacity duration-150 group-hover:opacity-100", f.kind === "ext" ? "stroke-wave opacity-60" : "stroke-muted-foreground opacity-45")}
                />
                <text x={x0} y={y1 - w / 2 - 5} className="fill-foreground text-[13px]">
                  {fit(nameOf(f.to), x1 - x0, 13)}
                  <tspan dx={8} className="fill-muted-foreground text-xs">
                    {fmtRate(f.rate)}
                  </tspan>
                </text>
              </g>
            );
          })}
          {groups.map((n) => (
            <g key={n.id}>
              <title>{nameOf(n.id)}</title>
              <text x={0} y={n.center - 9} className="fill-foreground text-sm">
                {fit(nameOf(n.id), nameMax, 14)}
              </text>
              <text x={0} y={n.center + 8} className="fill-muted-foreground text-xs">
                {n.out.length > 0 ? fmtRate(sum(n.out)) : o.flow.noSend}
              </text>
              <text x={width} y={n.center - 9} textAnchor="end" className="fill-foreground text-sm">
                {fit(nameOf(n.id), nameMax, 14)}
              </text>
              <text x={width} y={n.center + 8} textAnchor="end" className="fill-muted-foreground text-xs">
                {n.into.length > 0 ? fmtRate(sum(n.into)) : o.flow.noReceive}
              </text>
            </g>
          ))}
        </svg>
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

function Seg<T extends string>({ value, onChange, items, label }: { value: T; onChange: (v: T) => void; items: { value: T; label: string }[]; label: string }) {
  return (
    <div role="group" aria-label={label} className="inline-flex h-8 w-fit rounded-lg bg-muted p-[3px] max-md:h-10">
      {items.map((it) => (
        <button
          key={it.value}
          type="button"
          aria-pressed={it.value === value}
          onClick={() => onChange(it.value)}
          className={cn(
            "inline-flex h-full cursor-pointer items-center rounded-md border border-transparent px-2 whitespace-nowrap text-foreground/60 outline-none focus-visible:ring-2 focus-visible:ring-ring/40",
            it.value === value && "bg-background text-foreground shadow-[0_0_0_1px_var(--border)] dark:bg-input",
          )}
        >
          {it.label}
        </button>
      ))}
    </div>
  );
}

// ---- 表格和数字 ----

function Stat({ label, value, unit, to, title }: { label: string; value: ReactNode; unit?: string; to?: "/nodes" | "/projects"; title?: string }) {
  const inner = (
    <>
      <span className="text-muted-foreground">{label}</span>
      <span className="text-2xl leading-8 font-medium tracking-tight whitespace-nowrap">
        {value}
        {unit && (
          <>
            {" "}
            <small className="text-sm leading-8 font-normal tracking-normal text-muted-foreground">{unit}</small>
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
const tdClass = "h-10 px-2 py-1 align-middle whitespace-nowrap transition-colors group-hover:bg-accent first:rounded-l-md last:rounded-r-md";

function OverviewTable({ head, children }: { head: string[]; children: ReactNode }) {
  return (
    <div className="-mx-2 overflow-x-auto">
      <table className="w-full border-separate border-spacing-0">
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
function TrendChart({ title, help, series, from, to, step }: { title: string; help: string; series: ChartSeries[]; from: number; to: number; step: number }) {
  return (
    <div className="row-span-2 grid min-w-0 grid-rows-subgrid gap-y-3">
      <div className="flex flex-wrap items-center justify-between gap-x-3 gap-y-1">
        <span className="flex items-center gap-1 text-[13px] text-muted-foreground">
          {title}
          <HelpTip>{help}</HelpTip>
        </span>
        <Legend series={series} />
      </div>
      <TimeChart series={series} from={from} to={to} step={step} format={fmtRate} />
    </div>
  );
}

const pairColors = ["text-foreground", "text-muted-foreground", "text-wave", "text-info"];

// ---- 页面 ----

// 节点上一条序列最新的值。
const latest = (s: MetricSeries | undefined) => (s && s.values.length > 0 ? s.values[s.values.length - 1] : undefined);

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

  // 第一次只拿到累计值，算不出速率；隔几秒再取一次，不用等满 10 秒。
  const mesh = useQuery({
    queryKey: ["overview-mesh", onlineKey],
    queryFn: () => sampleMesh(online),
    enabled: online.length > 0,
    refetchInterval: (q) => (q.state.data ? refresh : 3000),
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

  const sample = mesh.data ?? undefined;
  const ready = !!sample && !!live.data;
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
  const shownFlows = flows.filter((f) => view === "all" || f.kind === view);
  const sankeyRows = [...(view === "mesh" ? [] : [{ id: external, name: o.flow.external }]), ...online.map((n) => ({ id: n.id, name: n.name }))];

  // ---- 趋势 ----

  const now = Date.now() / 1000;
  const buckets = meshBuckets(r.step);
  const extSeries = (name: string, dir: "rx" | "tx"): Pt[] => {
    const total = new Map<number, number>();
    const count = new Map<number, number>();
    for (const part of history.data ?? []) {
      const s = part.series.find((x) => x.name === name);
      if (!s) continue;
      s.values.forEach((v, i) => {
        const at = Number(s.times[i]);
        const b = buckets.get(at);
        inc(total, at, Math.max(0, v - (b ? (b[dir].get(part.id) ?? 0) / b.n : 0)));
        inc(count, at, 1);
      });
    }
    const points = [...total.entries()].sort((a, b) => a[0] - b[0]);
    // 最后一个时段可能只有部分节点上报了，合计会突然变小，当作还没有数据。
    if (points.length > 1 && (count.get(points[points.length - 1][0]) ?? 0) < (count.get(points[points.length - 2][0]) ?? 0)) points.pop();
    return points;
  };
  const nameOf = (id: string) => all.find((n) => n.id === id)?.name ?? id;
  const pairTotals = new Map<string, number>();
  for (const b of buckets.values()) for (const [k, v] of b.pairs) inc(pairTotals, k, v / b.n);
  const pairKeys = [...pairTotals.entries()].sort((a, b) => b[1] - a[1]).map(([k]) => k);
  const topPairs = pairKeys.slice(0, pairColors.length);
  const pairSeries: ChartSeries[] = topPairs.map((k, i) => {
    const [a, b] = k.split("|");
    return { label: `${nameOf(a)} → ${nameOf(b)}`, className: pairColors[i], points: [...buckets.entries()].sort((x, y) => x[0] - y[0]).map(([at, v]): Pt => [at, (v.pairs.get(k) ?? 0) / v.n]) };
  });
  if (pairKeys.length > topPairs.length) {
    const rest = new Set(pairKeys.slice(topPairs.length));
    pairSeries.push({
      label: o.trend.other,
      className: "text-muted-foreground/50",
      points: [...buckets.entries()].sort((x, y) => x[0] - y[0]).map(([at, v]): Pt => [at, [...v.pairs.entries()].reduce((sum, [k, x]) => (rest.has(k) ? sum + x : sum), 0) / v.n]),
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
      <div className="grid grid-cols-[repeat(auto-fit,minmax(112px,1fr))] gap-4">
        <Stat label={o.stats.nodes} to="/nodes" value={nodes.data ? online.length : dash} unit={nodes.data ? `/ ${all.length}` : undefined} />
        <Stat label={o.stats.containers} to="/projects" value={projects.data ? running : dash} unit={projects.data ? `/ ${containers.length}` : undefined} />
        <Stat label={o.stats.projects} to="/projects" value={projects.data ? projectList.length : dash} />
        <Stat label={o.stats.extIn} title={o.stats.estimated} value={ready ? inValue : dash} unit={ready ? inUnit : undefined} />
        <Stat label={o.stats.extOut} title={o.stats.estimated} value={ready ? outValue : dash} unit={ready ? outUnit : undefined} />
        <Stat label={o.stats.mesh} title={o.stats.meshTitle} value={sample ? meshValue : dash} unit={sample ? meshUnit : undefined} />
      </div>

      {noNodes ? (
        <div className="mt-8 border-t pt-8">
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
              <Seg
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
            {nodes.isPending ? (
              <RowsSkeleton />
            ) : online.length === 0 ? (
              <EmptyRow>{o.flow.empty}</EmptyRow>
            ) : !ready ? (
              <p role="status" className="py-4 text-center text-muted-foreground">
                {o.flow.sampling}
              </p>
            ) : shownFlows.length === 0 ? (
              <EmptyRow>{o.flow.empty}</EmptyRow>
            ) : (
              <Sankey rows={sankeyRows} flows={shownFlows} />
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
              <OverviewTable head={[o.nodes.name, o.nodes.status, o.nodes.cpu, o.nodes.memory, o.nodes.disk, o.nodes.inbound, o.nodes.outbound, o.nodes.containers]}>
                {(nodes.data?.nodes ?? []).map((n) => {
                  const u = n.online ? n.usage : undefined;
                  const memTotal = Number(n.info?.memoryBytes ?? 0n);
                  const disks = u?.disks ?? [];
                  const diskTotal = disks.reduce((sum, d) => sum + Number(d.totalBytes), 0) || Number(n.info?.diskBytes ?? 0n);
                  const diskUsed = disks.length > 0 ? disks.reduce((sum, d) => sum + Number(d.usedBytes), 0) : ((u?.disk ?? 0) / 100) * diskTotal;
                  const net = n.online ? live.data?.get(n.id) : undefined;
                  return (
                    <tr key={n.id} className="group">
                      <td className={tdClass}>
                        <Link to="/nodes/$nodeId" params={{ nodeId: n.id }} className="font-medium">
                          {n.name}
                        </Link>
                      </td>
                      <td className={tdClass}>{n.pending ? <Status>{o.nodes.pending}</Status> : n.online ? <Status tone="success">{o.nodes.online}</Status> : <Status tone="warning">{o.nodes.offline}</Status>}</td>
                      <td className={tdClass}>{u ? fmtCpu(u.cpu) : dash}</td>
                      <td className={tdClass}>{u ? (memTotal ? fmtUsed((u.memory / 100) * memTotal, memTotal) : fmtCpu(u.memory)) : dash}</td>
                      <td className={tdClass}>{u ? (diskTotal ? fmtUsed(diskUsed, diskTotal) : fmtCpu(u.disk)) : dash}</td>
                      <td className={tdClass}>{net?.rx !== undefined ? fmtRate(net.rx) : dash}</td>
                      <td className={tdClass}>{net?.tx !== undefined ? fmtRate(net.tx) : dash}</td>
                      <td className={tdClass}>{n.containers}</td>
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
              <OverviewTable head={[o.routes.mapping, o.routes.project, o.routes.container, o.routes.requests, o.routes.errorRate, o.routes.inbound, o.routes.outbound]}>
                {routeRows.map((x) => (
                  <tr key={`${x.project.id}/${x.route.name}`} className="group">
                    <td className={cn(tdClass, "font-mono text-[13px] font-medium")}>{x.route.name}</td>
                    <td className={tdClass}>
                      <Link to="/projects/$projectId" params={{ projectId: x.project.id }}>
                        {x.project.name}
                      </Link>
                    </td>
                    <td className={cn(tdClass, "text-muted-foreground")}>{x.route.container}</td>
                    <td className={tdClass}>{t.metrics.count(Math.round(x.req).toLocaleString(locale))}</td>
                    <td className={tdClass}>{x.req > 0 ? `${((100 * x.err) / x.req).toFixed(1)}%` : dash}</td>
                    <td className={tdClass}>{fmtTotal(x.in)}</td>
                    <td className={tdClass}>{fmtTotal(x.out)}</td>
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
