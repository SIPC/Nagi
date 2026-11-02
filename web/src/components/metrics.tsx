import { useState, type ReactNode } from "react";
import { useQuery } from "@tanstack/react-query";
import { Cpu, HardDrive, MemoryStick } from "lucide-react";
import { cn } from "@/lib/utils";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { errorMessage, nodeClient } from "@/lib/api";
import { EmptyRow, Notice, QueryError, Section, Segmented, Select, Skeleton } from "@/components/ui";
import { Legend, TimeChart, type ChartSeries, type Pt } from "@/components/chart";
import type { MetricSeries, NodeInfo, NodeUsage, Project } from "@/gen/nagi/v1/common_pb";
import { locale, t } from "@/i18n";

const m = t.metrics;

// 监控：节点、项目、容器和端口映射的指标。可选时间范围，节点按范围对应的步长聚合后返回，每条序列不超过约 180 个点。

// 时间范围和对应的聚合步长（秒）。24 小时以内节点有 10 秒精度的数据，更长的只有 5 分钟精度。
export const metricRanges = [
  { value: "1h", label: m.ranges["1h"], seconds: 3600, step: 20 },
  { value: "6h", label: m.ranges["6h"], seconds: 6 * 3600, step: 120 },
  { value: "24h", label: m.ranges["24h"], seconds: 86400, step: 600 },
  { value: "7d", label: m.ranges["7d"], seconds: 7 * 86400, step: 3600 },
  { value: "30d", label: m.ranges["30d"], seconds: 30 * 86400, step: 14400 },
] as const;
export type MetricRange = (typeof metricRanges)[number]["value"];
const rangeOf = (r: MetricRange) => metricRanges.find((x) => x.value === r)!;

export function RangePicker({ value, onChange }: { value: MetricRange; onChange: (r: MetricRange) => void }) {
  return (
    <Select value={value} onChange={(e) => onChange(e.target.value as MetricRange)} aria-label={m.rangeLabel} className="w-auto">
      {metricRanges.map((r) => (
        <option key={r.value} value={r.value}>
          {r.label}
        </option>
      ))}
    </Select>
  );
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
  return `${v.toFixed(i === 0 ? 0 : 1).replace(/\.0$/, "")} ${units[i]}`;
}


// CPU 图的纵轴至少到 1%，空闲容器的微小波动贴在底部，不被放大成尖峰。
const cpuMinMax = 1;

// Metric：一个数值块：名称、当前值、趋势图（鼠标移上去显示各时刻的值）。没有框，靠留白分组。
function Metric({
  label,
  points,
  format,
  range,
  minMax,
  quiet,
}: {
  label: string;
  points: Pt[];
  format: (v: number) => string;
  range: MetricRange;
  minMax?: number;
  quiet?: boolean;
}) {
  const r = rangeOf(range);
  const now = Date.now() / 1000;
  return (
    <div className="grid min-w-0 content-start gap-1">
      <div className="text-muted-foreground">{label}</div>
      <div className={mcval}>{points.length ? format(points[points.length - 1][1]) : "—"}</div>
      <TimeChart
        className="mt-2"
        series={[{ label, points, className: "text-muted-foreground" }]}
        from={now - r.seconds}
        to={now}
        step={r.step}
        format={format}
        height={40}
        axis={false}
        minMax={minMax}
        quiet={quiet}
      />
    </div>
  );
}

const mcval = "block text-xl leading-7 font-medium tracking-tight";
// 数值块 4 列，手机 2 列。
const cellsC4 = "grid grid-cols-2 gap-8 max-md:gap-4 md:grid-cols-4";

// MetricsSkeleton：监控加载中，和数值块一样的版面，内容出来时不跳。
function MetricsSkeleton() {
  return (
    <div role="status" aria-label={t.common.feedback.loading} className={cellsC4}>
      {Array.from({ length: 4 }, (_, i) => (
        <div key={i} className="grid gap-2">
          <Skeleton className="h-3 w-16" />
          <Skeleton className="h-5 w-20" />
          <Skeleton className="mt-2 h-10 w-full" />
        </div>
      ))}
    </div>
  );
}

const ago = (seconds: number) => timestampFromDate(new Date(Date.now() - seconds * 1000));

// NodeMetrics：节点页的监控，时间范围在区块标题右侧。
export function NodeMetrics({ nodeId }: { nodeId: string }) {
  const [range, setRange] = useState<MetricRange>("1h");
  return (
    <Section title={m.resourceUsage} actions={<Segmented value={range} onChange={setRange} items={metricRanges.map((r) => ({ value: r.value, label: m.rangesShort[r.value] }))} />}>
      <NodeMetricCards nodeId={nodeId} range={range} />
    </Section>
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
  if (q.isPending) return <MetricsSkeleton />;
  if (q.error) return <QueryError error={errorMessage(q.error)} onRetry={() => q.refetch()} />;
  const series = new Map(q.data.series.map((s) => [s.name, s]));
  const metric = (label: string, name: string, format: (v: number) => string, minMax?: number) => (
    <Metric key={name} label={label} points={toPoints(series.get(name))} format={format} range={range} minMax={minMax} />
  );
  return (
    <div className="grid grid-cols-1 gap-8 max-md:gap-4 md:grid-cols-3">
      {metric("CPU", "node:cpu", fmtCpu, cpuMinMax)}
      {metric(m.memory, "node:mem", fmtCpu)}
      {metric(m.disk, "node:disk", fmtCpu)}
      {metric(m.netRx, "node:net_rx", fmtRate)}
      {metric(m.netTx, "node:net_tx", fmtRate)}
    </div>
  );
}

const fmtRate = (v: number) => `${fmtBytes(v)}/s`;
const fmtMs = (v: number) => `${v < 10 ? v.toFixed(1) : Math.round(v)} ms`;
const fmtCount = (v: number) => m.count(Math.round(v).toLocaleString(locale));

const containerMetricFormat: Record<string, [string, (v: number) => string]> = {
  cpu: ["CPU", (v) => fmtCpu(v)],
  mem: [m.memory, fmtBytes],
  net_rx: [m.netRx, fmtRate],
  net_tx: [m.netTx, fmtRate],
  restarts: [m.restarts, (v) => String(v)],
};

// projectMetricNodes 是存着项目监控数据的节点：容器所在的节点，以及端口映射的其他入口节点（请求统计记在收到请求的节点上）。
export function projectMetricNodes(project: Project | undefined) {
  return [...(project?.containers.map((c) => c.nodeId) ?? []), ...(project?.routes.flatMap((r) => r.entryNodes) ?? [])];
}

// useProjectMetrics 取项目在时间范围内的容器、容器资源上限和端口映射（http）监控。监控数据存在节点上，逐个节点查询。
// 同一时间范围的项目栏、项目概览和容器页用同一份缓存：nodeIds 传 projectMetricNodes。
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
  const count = new Map<number, number>();
  for (const s of list) {
    for (const [t, v] of toPoints(s)) {
      total.set(t, (total.get(t) ?? 0) + v);
      count.set(t, (count.get(t) ?? 0) + 1);
    }
  }
  const points = [...total.entries()].sort((a, b) => a[0] - b[0]);
  // 最后一个时段可能只有部分节点上报了，合计会突然变小，当作还没有数据。
  if (points.length > 1) {
    const last = points[points.length - 1][0];
    const prev = points[points.length - 2][0];
    if ((count.get(last) ?? 0) < (count.get(prev) ?? 0)) points.pop();
  }
  return points;
}

// maxSeries 按时刻取几条序列里最大的值（耗时不能相加）。
function maxSeries(list: MetricSeries[]): Pt[] {
  const top = new Map<number, number>();
  for (const s of list) for (const [t, v] of toPoints(s)) top.set(t, Math.max(top.get(t) ?? v, v));
  return [...top.entries()].sort((a, b) => a[0] - b[0]);
}

const routeLatencyMetrics = new Set(["lat", "p50", "p95", "p99"]);

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

// 不是 0 但按一位小数会显示成 0.0 的值写成 <0.1，和真正的 0 区分开。
const tiny = (v: number) => v > 0 && v < 0.05;
export const fmtCpu = (v: number) => (tiny(v) ? "<0.1%" : `${v.toFixed(1)}%`);

// 短格式，用在占用条旁：不到 10 保留一位小数，否则取整，单位自动换算，最多 5 个字符：0.2%、12%、512KB、6.0MB、1.5GB。
const short = (v: number) => (v < 10 ? v.toFixed(1) : Math.round(v).toString());
export const fmtCpuShort = (v: number) => (tiny(v) ? "<0.1%" : `${short(v)}%`);
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
        title={m.memTitle(!usage ? "—" : usage.memLimit ? `${fmtBytes(usage.mem)} / ${fmtBytes(usage.memLimit)}` : fmtBytes(usage.mem))}
        fill={usage?.memLimit ? tone(mem) : "bg-info"}
      />
    </span>
  );
}

// UsageRings：项目栏里一个容器的 CPU 和内存：小圆环加数值，一行放下。超过 75% 变黄、90% 变红。
export function UsageRings({ usage, memFallback, className }: { usage?: Usage; memFallback: number; className?: string }) {
  const cpuMax = usage?.cpuLimit ?? 100;
  const memMax = usage?.memLimit ?? memFallback;
  const cpu = usage ? usage.cpu / cpuMax : 0;
  const mem = usage && memMax > 0 ? usage.mem / memMax : 0;
  const ring = (ratio: number, value: string, title: string) => {
    const r = Math.min(Math.max(ratio, 0), 1);
    const color = r >= 0.9 ? "stroke-destructive" : r >= 0.75 ? "stroke-warning" : "stroke-primary";
    return (
      <span className="inline-flex items-center gap-1" title={title}>
        <svg viewBox="0 0 100 100" className="size-4 shrink-0 -rotate-90" aria-hidden>
          <circle cx="50" cy="50" r="42.5" fill="none" strokeWidth="15" className="stroke-primary/12" />
          <circle cx="50" cy="50" r="42.5" fill="none" strokeWidth="15" strokeLinecap="round" pathLength={100} strokeDasharray={`${Math.max(r * 100, 3)} 100`} className={color} />
        </svg>
        {value}
      </span>
    );
  };
  return (
    <span className={cn("flex items-center gap-3 text-xs text-muted-foreground", className)}>
      {ring(cpu, usage ? `CPU ${usage.cpu.toFixed(1)}%` : "CPU —", usage ? `CPU ${fmtPct(usage.cpu)} / ${fmtPct(cpuMax)}` : "CPU —")}
      {ring(mem, usage ? fmtBytes(usage.mem) : "—", m.memTitle(!usage ? "—" : usage.memLimit ? `${fmtBytes(usage.mem)} / ${fmtBytes(usage.memLimit)}` : fmtBytes(usage.mem)))}
    </span>
  );
}

// NodeUsageBars：节点的 CPU、内存、磁盘占用，样式同容器的占用条：图标、条、数值。
// CPU 显示百分比，内存和磁盘显示「已用 / 总量」（旧版本的节点不报总量，退回百分比）。三行在同一个网格里，条的长度对齐。
export function NodeUsageBars({ usage, info, className }: { usage?: NodeUsage; info?: NodeInfo; className?: string }) {
  // 例如 1.4 GB、15.6 GB、512 MB：到 1000 就进位，不到 100 保留一位小数。
  const size = (v: number) => {
    const units = ["B", "KB", "MB", "GB", "TB"];
    let i = 0;
    while (v >= 1000 && i < units.length - 1) {
      v /= 1024;
      i++;
    }
    return `${v < 100 && i > 0 ? v.toFixed(1) : Math.round(v)} ${units[i]}`;
  };

  // 每行：图标、条、「已用 / 总量」。数值是一列、靠左，几行从同一处开始；已用是重点，「/ 总量」淡一些。
  const row = (icon: ReactNode, label: string, percent: number | undefined, total: number | undefined, fmt: (v: number) => string, fmtTotal = fmt, detail?: string) => {
    const ratio = percent === undefined ? 0 : percent / 100;
    const used = percent !== undefined && total ? ratio * total : undefined;
    const r = ratio > 0 ? Math.min(Math.max(ratio, 0.03), 1) : 0;
    const summary =
      percent === undefined ? `${label} —` : used !== undefined ? `${label} ${fmt(used)} / ${fmtTotal(total!)} (${fmtPct(percent)})` : `${label} ${fmtPct(percent)}`;
    const title = detail ? `${summary}\n${detail}` : summary;
    return (
      <span className="contents">
        <span className="opacity-70 [&_svg]:size-3" title={title}>
          {icon}
        </span>
        <span className="h-1 min-w-0 overflow-hidden rounded-full bg-foreground/10" title={title}>
          <span
            className={cn("block h-full origin-left rounded-full transition-transform duration-500 ease-[cubic-bezier(0.23,1,0.32,1)] motion-reduce:transition-none", tone(ratio))}
            style={{ transform: `scaleX(${r})` }}
          />
        </span>
        <span className="truncate pl-1" title={title}>
          {percent === undefined ? "—" : used !== undefined ? fmt(used) : fmtCpuShort(percent)}
          {used !== undefined && <span className="text-muted-foreground/50"> / {fmtTotal(total!)}</span>}
        </span>
      </span>
    );
  };
  // 节点卡片的 CPU 和容器一样按每核 100% 算：16 核的机器总量是 1600%。悬停提示里同时有占整机的比例和核数。
  const cpuTotal = info?.cpus ? info.cpus * 100 : undefined;
  const cpuDetail = info?.cpus ? m.cpuCores(info.cpus) : undefined;
  // 磁盘是所有盘加起来，每块盘的用量在悬停提示里。旧版本的节点不报各盘，退回数据目录所在的盘。
  const disks = usage?.disks ?? [];
  const diskTotal = disks.reduce((sum, d) => sum + Number(d.totalBytes), 0);
  const diskUsed = disks.reduce((sum, d) => sum + Number(d.usedBytes), 0);
  const diskDetail = disks.length > 1 ? disks.map((d) => `${d.mount}  ${size(Number(d.usedBytes))} / ${size(Number(d.totalBytes))}`).join("\n") : undefined;
  return (
    <span
      className={cn(
        "grid grid-cols-[auto_1fr_auto] items-center gap-x-1.5 gap-y-2 font-mono text-xs leading-4 tabular-nums text-muted-foreground",
        className,
      )}
    >
      {row(<Cpu />, "CPU", usage?.cpu, cpuTotal, fmtCpuShort, fmtCpuShort, cpuDetail)}
      {row(<MemoryStick />, m.memory, usage?.memory, info?.memoryBytes ? Number(info.memoryBytes) : undefined, size)}
      {diskTotal > 0
        ? row(<HardDrive />, m.disk, (100 * diskUsed) / diskTotal, diskTotal, size, size, diskDetail)
        : row(<HardDrive />, m.disk, usage?.disk, info?.diskBytes ? Number(info.diskBytes) : undefined, size)}
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
      <span className="min-w-[2.125rem] shrink-0 pl-0.5 text-xs leading-4 tabular-nums">{value}</span>
    </span>
  );
}

const cellOrder = ["cpu", "mem", "net_rx", "net_tx"];

// MetricCells：一组四个数值块，顺序固定，某项没有数据时写「—」。
function MetricCells({ series, range, quiet }: { series: Map<string, Pt[]>; range: MetricRange; quiet?: boolean }) {
  return (
    <div className={cellsC4}>
      {cellOrder.map((metric) => (
        <Metric
          key={metric}
          label={containerMetricFormat[metric][0]}
          points={series.get(metric) ?? []}
          format={containerMetricFormat[metric][1]}
          range={range}
          minMax={metric === "cpu" ? cpuMinMax : undefined}
          quiet={quiet}
        />
      ))}
    </div>
  );
}

// ProjectMetrics：项目概览里的监控。各容器的占用在项目栏和容器页里看，这里只给所有容器的合计和各端口映射的请求情况。
// 渲染在 Section 的网格里：分组标题和内容都是 Section 的直接子元素。
// httpRoutes：项目的 http 端口映射，这段时间没有请求的也列出来（数字写「—」）。
export function ProjectMetrics({ nodeIds, projectId, range, offline = [], httpRoutes = [] }: { nodeIds: string[]; projectId: string; range: MetricRange; offline?: string[]; httpRoutes?: string[] }) {
  const q = useProjectMetrics(projectId, nodeIds, range);
  if (nodeIds.length === 0) return <EmptyRow>{m.noData}</EmptyRow>;
  if (q.isPending) return <MetricsSkeleton />;
  if (q.error) return <QueryError error={errorMessage(q.error)} onRetry={() => q.refetch()} />;
  const byMetric = new Map<string, MetricSeries[]>();
  const routeSeries = new Map<string, Map<string, MetricSeries[]>>();
  for (const s of q.data) {
    const { kind, path, metric } = parseName(s.name);
    if (kind === "c") {
      if (metric !== "restarts") byMetric.set(metric, [...(byMetric.get(metric) ?? []), s]);
    } else if (kind === "r") {
      if (!routeSeries.has(path)) routeSeries.set(path, new Map());
      const byName = routeSeries.get(path)!;
      byName.set(metric, [...(byName.get(metric) ?? []), s]);
    }
  }
  // 一条映射可能有多个入口节点：请求数和流量相加，耗时取最慢的那个入口。
  const routes = new Map(
    [...routeSeries].map(([path, byName]) => [
      path,
      new Map([...byName].map(([metric, list]) => [metric, routeLatencyMetrics.has(metric) ? maxSeries(list) : sumSeries(list)])),
    ]),
  );
  const total = new Map(cellOrder.filter((m) => byMetric.has(m)).map((m) => [m, sumSeries(byMetric.get(m)!)]));
  const idle = httpRoutes.filter((name) => !routes.has(name));
  if (total.size === 0 && routes.size === 0 && idle.length === 0) return <EmptyRow>{m.noData}</EmptyRow>;
  return (
    <>
      {offline.length > 0 && <Notice tone="warning">{m.offlineExcluded(offline.join(m.seriesSeparator))}</Notice>}
      {total.size > 0 && (
        <>
          <div className="font-medium">{m.allContainersTotal}</div>
          <MetricCells series={total} range={range} />
        </>
      )}
      {[...routes.entries()].map(([name, data], i) => (
        <RouteMetrics key={name} name={name} data={data} range={range} divided={i > 0 || total.size > 0} />
      ))}
      {idle.map((name, i) => (
        <RouteMetrics key={name} name={name} data={noRouteData()} range={range} divided={i > 0 || routes.size > 0 || total.size > 0} empty />
      ))}
    </>
  );
}

// EmptyProjectMetrics：还没有容器时，概览照常排版但没有数据。
const noRouteData = () => new Map<string, Pt[]>(["req", "2xx", "3xx", "4xx", "5xx", "lat", "p50", "p95", "p99", "in", "out"].map((n) => [n, []]));

export function EmptyProjectMetrics({ range }: { range: MetricRange }) {
  const none = (names: string[]) => new Map<string, Pt[]>(names.map((n) => [n, []]));
  return (
    <>
      <div className="font-medium">{m.allContainersTotal}</div>
      <MetricCells series={none(cellOrder)} range={range} quiet />
      <RouteMetrics data={noRouteData()} range={range} divided empty quiet />
    </>
  );
}

// ---- 端口映射（http）----

// 状态码分类的颜色：2xx 灰色、3xx 蓝、4xx 橙、5xx 红，一眼先看到有问题的部分。
const statusSeries = [
  { metric: "2xx", label: "2xx", className: "text-muted-foreground", faded: true },
  { metric: "3xx", label: "3xx", className: "text-info" },
  { metric: "4xx", label: "4xx", className: "text-warning" },
  { metric: "5xx", label: "5xx", className: "text-destructive" },
];
const latencySeries = [
  { metric: "lat", label: m.average, className: "text-muted-foreground" },
  { metric: "p50", label: "P50", className: "text-info" },
  { metric: "p95", label: "P95", className: "text-warning" },
  { metric: "p99", label: "P99", className: "text-destructive" },
];
const trafficSeries = [
  { metric: "in", label: m.inbound, className: "text-muted-foreground" },
  { metric: "out", label: m.outbound, className: "text-wave" },
];

// RouteMetrics：一条端口映射的请求情况。上面是时间范围内的汇总，下面是三张图：按状态码堆叠的请求数、耗时分位数、流量。
// 节点存的是每秒速率，乘以步长就是该时段内的次数或字节数。旧版本的节点只报 req、5xx、lat，只显示有数据的部分。
// divided：和上面的分组之间一条细线。empty：没有数据，数字都写「—」。quiet：图里也不写「暂无数据」（还没有容器时的占位）。
function RouteMetrics({ name, data, range, divided, empty, quiet }: { name?: string; data: Map<string, Pt[]>; range: MetricRange; divided?: boolean; empty?: boolean; quiet?: boolean }) {
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
    { label: m.requests, value: fmtCount(requests), title: m.requestsAvgTitle((requests / r.seconds).toFixed(2)) },
    { label: m.errorRate, value: requests > 0 ? `${((100 * errors) / requests).toFixed(1)}%` : "—", title: `5xx ${fmtCount(errors)}` },
    { label: m.avgLatency, value: lat === undefined ? "—" : fmtMs(lat) },
  ];
  if (data.has("p95")) stats.push({ label: m.p95Latency, value: p95 === undefined ? "—" : fmtMs(p95), title: m.p95Title });
  if (data.has("in")) stats.push({ label: m.inboundTraffic, value: fmtBytes(sum("in")) }, { label: m.outboundTraffic, value: fmtBytes(sum("out")) });

  const pick = (defs: { metric: string; label: string; className: string; faded?: boolean }[], conv: (m: string) => Pt[]): ChartSeries[] =>
    defs.filter((d) => data.has(d.metric)).map((d) => ({ label: d.label, className: d.className, faded: d.faded, points: conv(d.metric) }));
  const chart = (title: string, series: ChartSeries[], format: (v: number) => string, stacked = false) =>
    series.length > 0 && (
      <div className="grid min-w-0 content-start gap-3">
        {/* 标题和图例分两行，三张图的图形对齐。 */}
        <div className="flex flex-col items-start gap-1">
          <span className="text-[13px] text-muted-foreground">{title}</span>
          {series.length > 1 && <Legend series={series} />}
        </div>
        <TimeChart series={series} from={now - r.seconds} to={now} step={r.step} format={format} stacked={stacked} quiet={quiet} />
      </div>
    );

  return (
    <>
      <div className={cn("font-medium", divided && "mt-4 border-t pt-6")}>
        {m.portMapping}
        {name && " "}
        {name && <span className="ml-1 font-mono text-[13px] font-normal text-muted-foreground">{name}</span>}
      </div>
      <div className="grid gap-6">
        <dl className="m-0 grid grid-cols-3 gap-8 max-md:gap-4 md:grid-cols-6">
          {stats.map((s) => (
            <div key={s.label} className="grid min-w-0 content-start gap-1" title={s.title}>
              <dt className="text-muted-foreground">{s.label}</dt>
              <dd className={cn("m-0", mcval)}>{empty ? "—" : s.value}</dd>
            </div>
          ))}
        </dl>
        <div className="grid grid-cols-1 gap-8 max-md:gap-4 md:grid-cols-3">
          {chart(
            m.requestCount,
            hasClasses ? pick(statusSeries, counts) : [{ label: m.requests, className: "text-muted-foreground", points: counts("req") }],
            (v) => Math.round(v).toLocaleString(locale),
            true,
          )}
          {chart(m.latency, pick(latencySeries, get), fmtMs)}
          {chart(m.traffic, pick(trafficSeries, get), fmtRate)}
        </div>
      </div>
    </>
  );
}

// ContainerMetrics：一个容器的资源：CPU、内存、下行、上行，容器组按服务分组。重启过的话在数值块下面写一行。
export function ContainerMetrics({ projectId, nodeIds, container, range }: { projectId: string; nodeIds: string[]; container: string; range: MetricRange }) {
  const q = useProjectMetrics(projectId, nodeIds, range);
  if (q.isPending) return <MetricsSkeleton />;
  if (q.error) return <QueryError error={errorMessage(q.error)} onRetry={() => q.refetch()} />;
  const services = new Map<string, Map<string, Pt[]>>();
  for (const s of q.data) {
    const { kind, path, metric } = parseName(s.name);
    if (kind !== "c" || !path.startsWith(container + "/")) continue;
    const service = path.slice(container.length + 1);
    if (!services.has(service)) services.set(service, new Map());
    services.get(service)!.set(metric, toPoints(s));
  }
  if (services.size === 0) return <EmptyRow>{m.noData}</EmptyRow>;
  return (
    <>
      {[...services.entries()].map(([name, series]) => {
        const restarts = Math.max(0, ...(series.get("restarts") ?? []).map((p) => p[1]));
        return (
          <div key={name} className="grid gap-4">
            {/* 单个容器（只有一个服务）不再重复名称。 */}
            {services.size > 1 && <div className="font-mono text-[13px] text-muted-foreground">{name}</div>}
            <MetricCells series={series} range={range} />
            {restarts > 0 && (
              <div className="text-muted-foreground">
                {m.restarts} {restarts}
              </div>
            )}
          </div>
        );
      })}
    </>
  );
}
