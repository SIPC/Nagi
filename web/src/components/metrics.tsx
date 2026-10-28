import { useState, type ReactNode } from "react";
import { useQuery } from "@tanstack/react-query";
import { Cpu, MemoryStick } from "lucide-react";
import { cn } from "@/lib/utils";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { errorMessage, nodeClient } from "@/lib/api";
import { Alert, EmptyRow, Loading, Segmented } from "@/components/ui";
import { Legend, TimeChart, type ChartSeries, type Pt } from "@/components/chart";
import type { MetricSeries } from "@/gen/nagi/v1/common_pb";

// 监控：节点、项目、容器和端口映射的指标。可选时间范围，节点按范围对应的步长聚合后返回，每条序列不超过约 180 个点。

// 时间范围和对应的聚合步长（秒）。24 小时以内节点有 10 秒精度的数据，更长的只有 5 分钟精度。
export const metricRanges = [
  { value: "1h", label: "1 小时", seconds: 3600, step: 20 },
  { value: "6h", label: "6 小时", seconds: 6 * 3600, step: 120 },
  { value: "24h", label: "24 小时", seconds: 86400, step: 600 },
  { value: "7d", label: "7 天", seconds: 7 * 86400, step: 3600 },
  { value: "30d", label: "30 天", seconds: 30 * 86400, step: 14400 },
] as const;
export type MetricRange = (typeof metricRanges)[number]["value"];
const rangeOf = (r: MetricRange) => metricRanges.find((x) => x.value === r)!;

export function RangePicker({ value, onChange }: { value: MetricRange; onChange: (r: MetricRange) => void }) {
  return <Segmented value={value} onChange={onChange} items={metricRanges.map((r) => ({ value: r.value, label: r.label }))} />;
}

// useRange：一个监控区块的时间范围和选择器。
export function useRange(initial: MetricRange = "1h") {
  const [range, setRange] = useState<MetricRange>(initial);
  return { range, picker: <RangePicker value={range} onChange={setRange} /> };
}

const toPoints = (s: MetricSeries | undefined): Pt[] => (s ? s.values.map((v, i) => [Number(s.times[i]), v]) : []);

export function fmtBytes(v: number) {
  const units = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(i === 0 ? 0 : 1)} ${units[i]}`;
}


// Metric：一个指标的方块：名称、当前值、趋势图（鼠标移上去显示各时刻的值）。
function Metric({ label, points, format, range }: { label: string; points: Pt[]; format: (v: number) => string; range: MetricRange }) {
  const r = rangeOf(range);
  const now = Date.now() / 1000;
  return (
    <div className="grid gap-1 rounded-md bg-subtle px-3 py-2.5">
      <div className="text-xs text-muted-foreground">{label}</div>
      <div className="font-mono text-sm font-medium">{points.length ? format(points[points.length - 1][1]) : "—"}</div>
      <TimeChart
        series={[{ label, points, className: "text-info" }]}
        from={now - r.seconds}
        to={now}
        step={r.step}
        format={format}
        height={32}
        axis={false}
      />
    </div>
  );
}

const ago = (seconds: number) => timestampFromDate(new Date(Date.now() - seconds * 1000));

// NodeMetrics：节点页的监控，右上角选时间范围。
export function NodeMetrics({ nodeId }: { nodeId: string }) {
  const { range, picker } = useRange();
  return (
    <div className="grid gap-3">
      <div className="flex justify-end">{picker}</div>
      <NodeMetricCards nodeId={nodeId} range={range} />
    </div>
  );
}

function NodeMetricCards({ nodeId, range }: { nodeId: string; range: MetricRange }) {
  const r = rangeOf(range);
  const q = useQuery({
    queryKey: ["node-metrics", nodeId, range],
    queryFn: () => nodeClient.queryNodeMetrics({ nodeId, prefix: "node:", from: ago(r.seconds), step: BigInt(r.step) }),
    refetchInterval: 10_000,
    placeholderData: (prev) => prev,
  });
  if (q.isPending) return <Loading />;
  if (q.error) return <Alert>{errorMessage(q.error)}</Alert>;
  const series = new Map(q.data.series.map((s) => [s.name, s]));
  const metric = (label: string, name: string, format: (v: number) => string) => (
    <Metric key={name} label={label} points={toPoints(series.get(name))} format={format} range={range} />
  );
  return (
    <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-5">
      {metric("CPU", "node:cpu", fmtCpu)}
      {metric("内存", "node:mem", fmtCpu)}
      {metric("磁盘", "node:disk", fmtCpu)}
      {metric("下行", "node:net_rx", fmtRate)}
      {metric("上行", "node:net_tx", fmtRate)}
    </div>
  );
}

const fmtRate = (v: number) => `${fmtBytes(v)}/s`;
const fmtMs = (v: number) => `${v < 10 ? v.toFixed(1) : Math.round(v)} ms`;
const fmtCount = (v: number) => `${Math.round(v).toLocaleString()} 次`;

const containerMetricFormat: Record<string, [string, (v: number) => string]> = {
  cpu: ["CPU", (v) => fmtCpu(v)],
  mem: ["内存", fmtBytes],
  net_rx: ["下行", fmtRate],
  net_tx: ["上行", fmtRate],
  restarts: ["重启次数", (v) => String(v)],
};

// useProjectMetrics 取项目在时间范围内的容器、容器资源上限和端口映射（http）监控。监控数据存在容器所在的节点上，逐个节点查询。
// 同一时间范围的项目栏、项目概览和容器页用同一份缓存：nodeIds 传项目里所有容器的节点。
export function useProjectMetrics(projectId: string, nodeIds: string[], range: MetricRange = "1h") {
  const ids = [...new Set(nodeIds)].sort();
  const r = rangeOf(range);
  return useQuery({
    queryKey: ["project-metrics", projectId, ids.join(","), range],
    queryFn: async () => {
      const step = BigInt(r.step);
      const parts = await Promise.all(
        ids.flatMap((nodeId) => [
          nodeClient.queryNodeMetrics({ nodeId, prefix: `c:${projectId}/`, from: ago(r.seconds), step }).catch(() => null),
          nodeClient.queryNodeMetrics({ nodeId, prefix: `r:${projectId}/`, from: ago(r.seconds), step }).catch(() => null),
          // 上限只用最新的值，取最近 1 分钟就够了。
          nodeClient.queryNodeMetrics({ nodeId, prefix: `l:${projectId}/`, from: ago(60) }).catch(() => null),
        ]),
      );
      return parts.flatMap((p) => p?.series ?? []);
    },
    enabled: ids.length > 0,
    refetchInterval: 10_000,
    placeholderData: (prev) => prev,
  });
}

// parseName 拆开序列名：c:<项目>/<容器>/<服务>:cpu → { kind: "c", path: "<容器>/<服务>", metric: "cpu" }；
// l:<项目>/<容器>:mem（容器的资源上限）、r:<项目>/<映射>:req 同理。
function parseName(name: string) {
  const rest = name.slice(name.indexOf("/") + 1);
  const i = rest.lastIndexOf(":");
  return { kind: name[0], path: rest.slice(0, i), metric: rest.slice(i + 1) };
}

// sumSeries 把几条序列加成一条。节点按同一步长聚合，窗口起点对齐，所以直接按时刻相加。
function sumSeries(list: MetricSeries[]): Pt[] {
  const total = new Map<number, number>();
  for (const s of list) for (const [t, v] of toPoints(s)) total.set(t, (total.get(t) ?? 0) + v);
  return [...total.entries()].sort((a, b) => a[0] - b[0]);
}

// Usage 是一个容器当前的占用和上限。CPU 是百分比（每核 100），内存是字节。
// 上限：设了限制按限制，没设按主机（节点算好了报上来）；旧版本的节点不报，没有这两项。
export type Usage = { cpu: number; mem: number; cpuLimit?: number; memLimit?: number };

// containerUsage 是各容器当前的 CPU 和内存（容器组把组里的服务加起来）及其上限。最近 1 分钟没有采样的不算（已停止或节点离线）。
export function containerUsage(series: MetricSeries[] | undefined) {
  const usage = new Map<string, Usage>();
  const limits = new Map<string, { cpu?: number; mem?: number }>();
  const fresh = Date.now() / 1000 - 60;
  for (const s of series ?? []) {
    const { kind, path, metric } = parseName(s.name);
    if ((kind !== "c" && kind !== "l") || (metric !== "cpu" && metric !== "mem") || s.values.length === 0) continue;
    if (Number(s.times[s.times.length - 1]) < fresh) continue;
    const last = s.values[s.values.length - 1];
    if (kind === "l") {
      limits.set(path, { ...limits.get(path), [metric]: last });
      continue;
    }
    const container = path.slice(0, path.indexOf("/"));
    const u = usage.get(container) ?? { cpu: 0, mem: 0 };
    u[metric] += last;
    usage.set(container, u);
  }
  for (const [container, u] of usage) {
    const l = limits.get(container);
    if (l?.cpu) u.cpuLimit = l.cpu;
    if (l?.mem) u.memLimit = l.mem;
  }
  return usage;
}

export const fmtCpu = (v: number) => `${v.toFixed(1)}%`;

// 短格式，用在占用条旁：不到 10 保留一位小数，否则取整，单位自动换算，最多 5 个字符：0.2%、12%、512KB、6.0MB、1.5GB。
const short = (v: number) => (v < 10 ? v.toFixed(1) : Math.round(v).toString());
export const fmtCpuShort = (v: number) => `${short(v)}%`;
export function fmtBytesShort(v: number) {
  const units = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  // 到 1000 就进位（而不是 1024），避免出现 1010MB 这样的 4 位数。
  while (v >= 1000 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return short(v) + units[i];
}

// 悬停提示里的百分比：最多一位小数，整数不带 .0（如 0.3%、1200%）。
const fmtPct = (v: number) => `${Number(v.toFixed(1))}%`;
const tone = (r: number) => (r >= 0.9 ? "bg-destructive" : r >= 0.75 ? "bg-warning" : "bg-info");

// UsageBars：一个容器的 CPU 和内存占用条，上下两行。宽度由调用方定；数值列定宽、左对齐紧跟在条后面，几个容器之间对齐。
// 满格是容器的上限（设了限制按限制，没设按主机），超过 75% 变黄、90% 变红。
// 旧版本的节点不报上限：CPU 满格按 1 核，内存满格是 memFallback（同一列表里占用最多的容器），只用来相互对比。
export function UsageBars({ usage, memFallback, className }: { usage?: Usage; memFallback: number; className?: string }) {
  const cpuMax = usage?.cpuLimit ?? 100;
  const memMax = usage?.memLimit ?? memFallback;
  const cpu = usage ? usage.cpu / cpuMax : 0;
  const mem = usage && memMax > 0 ? usage.mem / memMax : 0;
  return (
    <span className={cn("grid gap-1", className)}>
      <UsageBar
        icon={<Cpu />}
        ratio={cpu}
        value={usage ? fmtCpuShort(usage.cpu) : "—"}
        title={usage ? `CPU ${fmtPct(usage.cpu)} / ${fmtPct(cpuMax)}` : "CPU —"}
        fill={tone(cpu)}
      />
      <UsageBar
        icon={<MemoryStick />}
        ratio={mem}
        value={usage ? fmtBytesShort(usage.mem) : "—"}
        title={!usage ? "内存 —" : usage.memLimit ? `内存 ${fmtBytes(usage.mem)} / ${fmtBytes(usage.memLimit)}` : `内存 ${fmtBytes(usage.mem)}`}
        fill={usage?.memLimit ? tone(mem) : "bg-info"}
      />
    </span>
  );
}

function UsageBar({ icon, ratio, value, title, fill }: { icon: ReactNode; ratio: number; value: string; title: string; fill: string }) {
  // 很小但不是 0 的占用也留一点长度，和没有数据区分开。
  const r = ratio > 0 ? Math.min(Math.max(ratio, 0.03), 1) : 0;
  return (
    <span className="flex min-w-0 items-center gap-1 text-muted-foreground" title={title}>
      <span className="shrink-0 opacity-70 [&_svg]:size-3">{icon}</span>
      <span className="h-1 min-w-0 flex-1 overflow-hidden rounded-full bg-foreground/10">
        <span
          className={cn("block h-full origin-left rounded-full transition-transform duration-500 ease-[cubic-bezier(0.23,1,0.32,1)] motion-reduce:transition-none", fill)}
          style={{ transform: `scaleX(${r})` }}
        />
      </span>
      <span className="min-w-[2.125rem] shrink-0 pl-0.5 font-mono text-[11px] leading-4 tabular-nums">{value}</span>
    </span>
  );
}

function MetricGroups({ groups, range }: { groups: [string, Map<string, Pt[]>][]; range: MetricRange }) {
  return (
    <div className="grid gap-5">
      {groups.map(([name, series]) => (
        <div key={name} className="grid gap-2">
          {name && <div className="font-mono text-xs text-muted-foreground">{name}</div>}
          <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-5">
            {[...series.entries()].map(([metric, points]) => (
              <Metric
                key={metric}
                label={containerMetricFormat[metric]?.[0] ?? metric}
                points={points}
                format={containerMetricFormat[metric]?.[1] ?? String}
                range={range}
              />
            ))}
          </div>
        </div>
      ))}
    </div>
  );
}

// ProjectMetrics：项目概览里的监控。各容器的占用在项目栏和容器页里看，这里只给所有容器的合计和各端口映射的请求情况。
export function ProjectMetrics({ nodeIds, projectId, range }: { nodeIds: string[]; projectId: string; range: MetricRange }) {
  const q = useProjectMetrics(projectId, nodeIds, range);
  if (nodeIds.length === 0) return <EmptyRow>暂无数据</EmptyRow>;
  if (q.isPending) return <Loading />;
  if (q.error) return <Alert>{errorMessage(q.error)}</Alert>;
  const byMetric = new Map<string, MetricSeries[]>();
  const routes = new Map<string, Map<string, Pt[]>>();
  for (const s of q.data) {
    const { kind, path, metric } = parseName(s.name);
    if (kind === "c") {
      if (metric !== "restarts") byMetric.set(metric, [...(byMetric.get(metric) ?? []), s]);
    } else if (kind === "r") {
      if (!routes.has(path)) routes.set(path, new Map());
      routes.get(path)!.set(metric, toPoints(s));
    }
  }
  const order = ["cpu", "mem", "net_rx", "net_tx"];
  const total = new Map(order.filter((m) => byMetric.has(m)).map((m) => [m, sumSeries(byMetric.get(m)!)]));
  if (total.size === 0 && routes.size === 0) return <EmptyRow>暂无数据</EmptyRow>;
  return (
    <div className="grid gap-8">
      {total.size > 0 && <MetricGroups groups={[["全部容器合计", total]]} range={range} />}
      {[...routes.entries()].map(([name, data]) => (
        <RouteMetrics key={name} name={name} data={data} range={range} />
      ))}
    </div>
  );
}

// ---- 端口映射（http）----

// 状态码分类的颜色与状态色一致：2xx 正常、3xx 信息、4xx 警告、5xx 错误。
const statusSeries = [
  { metric: "2xx", label: "2xx", className: "text-success" },
  { metric: "3xx", label: "3xx", className: "text-info" },
  { metric: "4xx", label: "4xx", className: "text-warning" },
  { metric: "5xx", label: "5xx", className: "text-destructive" },
];
const latencySeries = [
  { metric: "lat", label: "平均", className: "text-muted-foreground" },
  { metric: "p50", label: "P50", className: "text-info" },
  { metric: "p95", label: "P95", className: "text-warning" },
  { metric: "p99", label: "P99", className: "text-destructive" },
];
const trafficSeries = [
  { metric: "in", label: "入站", className: "text-info" },
  { metric: "out", label: "出站", className: "text-success" },
];

// RouteMetrics：一条端口映射的请求情况。上面是时间范围内的汇总，下面是三张图：按状态码堆叠的请求数、耗时分位数、流量。
// 节点存的是每秒速率，乘以步长就是该时段内的次数或字节数。旧版本的节点只报 req、5xx、lat，只显示有数据的部分。
function RouteMetrics({ name, data, range }: { name: string; data: Map<string, Pt[]>; range: MetricRange }) {
  const r = rangeOf(range);
  const now = Date.now() / 1000;
  const get = (m: string) => data.get(m) ?? [];
  const sum = (m: string) => get(m).reduce((acc, [, v]) => acc + v * r.step, 0);
  const counts = (m: string): Pt[] => get(m).map(([t, v]) => [t, v * r.step]);

  const requests = sum("req");
  const errors = sum("5xx");
  // 耗时按各时段的请求数加权。
  const reqAt = new Map(get("req"));
  const weighted = (m: string) => {
    let num = 0;
    let den = 0;
    for (const [t, v] of get(m)) {
      const w = reqAt.get(t) ?? 0;
      num += v * w;
      den += w;
    }
    return den > 0 ? num / den : undefined;
  };
  const lat = weighted("lat");
  const p95 = weighted("p95");
  const hasClasses = statusSeries.some((s) => data.has(s.metric) && s.metric !== "5xx");

  const stats: { label: string; value: string; title?: string }[] = [
    { label: "请求", value: fmtCount(requests), title: `平均 ${(requests / r.seconds).toFixed(2)}/s` },
    { label: "错误率", value: requests > 0 ? `${((100 * errors) / requests).toFixed(1)}%` : "—", title: `5xx ${fmtCount(errors)}` },
    { label: "平均耗时", value: lat === undefined ? "—" : fmtMs(lat) },
  ];
  if (data.has("p95")) stats.push({ label: "P95 耗时", value: p95 === undefined ? "—" : fmtMs(p95), title: "各时段 P95 按请求数加权平均" });
  if (data.has("in")) stats.push({ label: "入站流量", value: fmtBytes(sum("in")) }, { label: "出站流量", value: fmtBytes(sum("out")) });

  const pick = (defs: { metric: string; label: string; className: string }[], conv: (m: string) => Pt[]): ChartSeries[] =>
    defs.filter((d) => data.has(d.metric)).map((d) => ({ label: d.label, className: d.className, points: conv(d.metric) }));
  const chart = (title: string, series: ChartSeries[], format: (v: number) => string, stacked = false) =>
    series.length > 0 && (
      <div className="grid min-w-0 gap-2 rounded-md bg-subtle px-3 py-2.5">
        <div className="flex flex-wrap items-center justify-between gap-x-3 gap-y-1">
          <span className="text-xs text-muted-foreground">{title}</span>
          {series.length > 1 && <Legend series={series} />}
        </div>
        <TimeChart series={series} from={now - r.seconds} to={now} step={r.step} format={format} stacked={stacked} height={88} />
      </div>
    );

  return (
    <div className="grid gap-3">
      <div className="font-mono text-xs text-muted-foreground">端口映射 {name}</div>
      <dl className={cn("grid grid-cols-3 gap-px overflow-hidden rounded-md bg-border", stats.length === 6 && "sm:grid-cols-6")}>
        {stats.map((s) => (
          <div key={s.label} className="grid gap-0.5 bg-subtle px-3 py-2" title={s.title}>
            <dt className="text-xs text-muted-foreground">{s.label}</dt>
            <dd className="font-mono text-sm font-medium tabular-nums">{s.value}</dd>
          </div>
        ))}
      </dl>
      <div className="grid gap-3 lg:grid-cols-3">
        {chart(
          hasClasses ? "请求数（按状态码）" : "请求数",
          hasClasses ? pick(statusSeries, counts) : [{ label: "请求", className: "text-info", points: counts("req") }],
          fmtCount,
          true,
        )}
        {chart("耗时", pick(latencySeries, get), fmtMs)}
        {chart("流量", pick(trafficSeries, get), fmtRate)}
      </div>
    </div>
  );
}

// ContainerMetrics：一个容器的监控，容器组按服务分组。
export function ContainerMetrics({ projectId, nodeIds, container, range }: { projectId: string; nodeIds: string[]; container: string; range: MetricRange }) {
  const q = useProjectMetrics(projectId, nodeIds, range);
  if (q.isPending) return <Loading />;
  if (q.error) return <Alert>{errorMessage(q.error)}</Alert>;
  const services = new Map<string, Map<string, Pt[]>>();
  for (const s of q.data) {
    const { kind, path, metric } = parseName(s.name);
    if (kind !== "c" || !path.startsWith(container + "/")) continue;
    const service = path.slice(container.length + 1);
    if (!services.has(service)) services.set(service, new Map());
    services.get(service)!.set(metric, toPoints(s));
  }
  if (services.size === 0) return <EmptyRow>暂无数据</EmptyRow>;
  // 单个容器（只有一个服务）不再重复名称。
  const groups = [...services.entries()].map(([name, m]): [string, Map<string, Pt[]>] => [services.size > 1 ? name : "", m]);
  return <MetricGroups groups={groups} range={range} />;
}

