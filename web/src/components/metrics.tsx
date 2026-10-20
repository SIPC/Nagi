import { useQuery } from "@tanstack/react-query";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { errorMessage, nodeClient } from "@/lib/api";
import { Alert, EmptyRow, Loading } from "@/components/ui";

// 监控：节点和项目最近 1 小时的指标，每项一个数值加一条趋势线。

export function fmtBytes(v: number) {
  const units = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(i === 0 ? 0 : 1)} ${units[i]}`;
}

export function Sparkline({ values, className }: { values: number[]; className?: string }) {
  if (values.length < 2) return <div className={className} />;
  const max = Math.max(...values, 1);
  const w = 100;
  const h = 28;
  const pts = values.map((v, i) => `${(i / (values.length - 1)) * w},${h - 1 - (v / max) * (h - 2)}`);
  return (
    <svg viewBox={`0 0 ${w} ${h}`} preserveAspectRatio="none" className={className} aria-hidden>
      <polygon points={`0,${h} ${pts.join(" ")} ${w},${h}`} className="fill-info/10" />
      <polyline points={pts.join(" ")} fill="none" className="stroke-info" strokeWidth="1.25" vectorEffect="non-scaling-stroke" />
    </svg>
  );
}

// Metric：一个指标的方块：名称、当前值、趋势线。
function Metric({ label, value, values }: { label: string; value: string; values: number[] }) {
  return (
    <div className="grid gap-1 rounded-md border bg-subtle px-3 py-2.5">
      <div className="text-xs text-muted-foreground">{label}</div>
      <div className="font-mono text-sm font-medium">{value}</div>
      <Sparkline values={values} className="h-7 w-full" />
    </div>
  );
}

const lastHour = () => timestampFromDate(new Date(Date.now() - 3600_000));

export function NodeMetrics({ nodeId }: { nodeId: string }) {
  const q = useQuery({
    queryKey: ["node-metrics", nodeId],
    queryFn: () => nodeClient.queryNodeMetrics({ nodeId, prefix: "node:", from: lastHour() }),
    refetchInterval: 10_000,
  });
  if (q.isPending) return <Loading />;
  if (q.error) return <Alert>{errorMessage(q.error)}</Alert>;
  const series = Object.fromEntries(q.data.series.map((s) => [s.name, s.values]));
  const metric = (label: string, name: string, fmt: (v: number) => string) => {
    const values = series[name] ?? [];
    return <Metric key={name} label={label} value={values.length ? fmt(values[values.length - 1]) : "—"} values={values} />;
  };
  return (
    <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-5">
      {metric("CPU", "node:cpu", (v) => `${v.toFixed(1)}%`)}
      {metric("内存", "node:mem", (v) => `${v.toFixed(1)}%`)}
      {metric("磁盘", "node:disk", (v) => `${v.toFixed(1)}%`)}
      {metric("下行", "node:net_rx", (v) => `${fmtBytes(v)}/s`)}
      {metric("上行", "node:net_tx", (v) => `${fmtBytes(v)}/s`)}
    </div>
  );
}

const projectMetricFormat: Record<string, [string, (v: number) => string]> = {
  cpu: ["CPU", (v) => `${v.toFixed(1)}%`],
  mem: ["内存", fmtBytes],
  net_rx: ["下行", (v) => `${fmtBytes(v)}/s`],
  net_tx: ["上行", (v) => `${fmtBytes(v)}/s`],
  restarts: ["重启次数", (v) => String(v)],
  req: ["请求", (v) => `${v.toFixed(2)}/s`],
  "5xx": ["5xx", (v) => `${v.toFixed(2)}/s`],
  lat: ["平均耗时", (v) => `${v.toFixed(0)} ms`],
};

// 项目的容器和端口映射（http）的监控，按容器、映射分组。
export function ProjectMetrics({ nodeId, projectId }: { nodeId: string; projectId: string }) {
  const q = useQuery({
    queryKey: ["project-metrics", nodeId, projectId],
    queryFn: async () => {
      const [c, r] = await Promise.all([
        nodeClient.queryNodeMetrics({ nodeId, prefix: `c:${projectId}/`, from: lastHour() }),
        nodeClient.queryNodeMetrics({ nodeId, prefix: `r:${projectId}/`, from: lastHour() }),
      ]);
      return [...c.series, ...r.series];
    },
    refetchInterval: 10_000,
  });
  if (q.isPending) return <Loading />;
  if (q.error) return <Alert>{errorMessage(q.error)}</Alert>;
  // 序列名形如 c:<项目>/<容器>/<服务>:cpu、r:<项目>/<映射>:req。
  const groups = new Map<string, Map<string, number[]>>();
  for (const s of q.data) {
    const rest = s.name.slice(s.name.indexOf("/") + 1);
    const i = rest.lastIndexOf(":");
    const key = `${s.name[0] === "c" ? "容器" : "端口映射"} ${rest.slice(0, i)}`;
    if (!groups.has(key)) groups.set(key, new Map());
    groups.get(key)!.set(rest.slice(i + 1), s.values);
  }
  if (groups.size === 0) return <EmptyRow>暂无数据</EmptyRow>;
  return (
    <div className="grid gap-5">
      {[...groups.entries()].map(([name, series]) => (
        <div key={name} className="grid gap-2">
          <div className="font-mono text-xs text-muted-foreground">{name}</div>
          <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-5">
            {[...series.entries()].map(([kind, values]) => (
              <Metric
                key={kind}
                label={projectMetricFormat[kind]?.[0] ?? kind}
                value={(projectMetricFormat[kind]?.[1] ?? String)(values[values.length - 1])}
                values={values}
              />
            ))}
          </div>
        </div>
      ))}
    </div>
  );
}
