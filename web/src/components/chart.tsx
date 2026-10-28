import { useLayoutEffect, useRef, useState } from "react";
import { cn } from "@/lib/utils";
import { t } from "@/i18n";

// 时间图表：折线（可有多条）或堆叠柱。横轴是选定的时间范围 [from, to]，不是数据的范围，
// 所以一段时间没有数据时那一段就是空的。鼠标移上去显示该时刻各条序列的值。
// 颜色用 className 里的文字颜色（例如 text-info），图形用 currentColor。

export type Pt = [time: number, value: number]; // time 是 Unix 秒

export type ChartSeries = { label: string; points: Pt[]; className: string };

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

const pad = (n: number) => String(n).padStart(2, "0");

// fmtTime：24 小时以内只写时刻，更长的范围带上日期。
export function fmtTime(t: number, withDate: boolean, withSeconds = false) {
  const d = new Date(t * 1000);
  const time = `${pad(d.getHours())}:${pad(d.getMinutes())}${withSeconds ? ":" + pad(d.getSeconds()) : ""}`;
  return withDate ? `${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${time}` : time;
}

export function TimeChart({
  series,
  from,
  to,
  step,
  format,
  stacked,
  height = 72,
  axis = true,
  minMax = 0,
  className,
}: {
  series: ChartSeries[];
  from: number;
  to: number;
  step: number;
  format: (v: number) => string;
  stacked?: boolean;
  height?: number;
  axis?: boolean;
  // minMax：纵轴上限至少是多少。数值很小时（例如空闲容器 0.01% 的 CPU），不让微小波动撑满整个图。
  minMax?: number;
  className?: string;
}) {
  const { ref, width } = useWidth();
  const [hover, setHover] = useState<number | null>(null);
  const withDate = to - from > 86400;

  // 所有序列的时刻合在一起，鼠标对准的是其中最近的一个。
  const byTime = series.map((s) => new Map(s.points));
  const times = [...new Set(series.flatMap((s) => s.points.map((p) => p[0])))].filter((t) => t >= from - step && t <= to).sort((a, b) => a - b);
  const total = (t: number) => byTime.reduce((sum, m) => sum + (m.get(t) ?? 0), 0);
  const peak = Math.max(0, ...(stacked ? times.map(total) : series.flatMap((s) => s.points.map((p) => p[1]))));
  const yMax = Math.max(peak > 0 ? peak * 1.1 : 1, minMax);
  const span = Math.max(to - from, 1);
  const x = (t: number) => ((t - from) / span) * width;
  const y = (v: number) => height - (v / yMax) * (height - 2) - 1;
  const empty = times.length === 0;

  // 鼠标对准的时刻：柱状图取鼠标所在的时段（柱子从时段起点画到终点），折线取最近的点。
  const nearest = (px: number) => {
    const t = from + (px / Math.max(width, 1)) * span;
    if (stacked) {
      let c: number | undefined;
      for (const x of times) if (x <= t) c = x;
      if (c !== undefined && t - c < step) return c;
    }
    let best: number | null = null;
    for (const c of times) if (best === null || Math.abs(c - t) < Math.abs(best - t)) best = c;
    return best !== null && Math.abs(best - t) <= step * 1.5 ? best : null;
  };

  // 折线：相邻两点隔得太远（中间没有数据）就断开；断开后孤立的点画成小圆点。
  const lines = (s: ChartSeries, i: number) => {
    const pts = s.points.filter((p) => p[0] >= from - step && p[0] <= to);
    const segs: Pt[][] = [];
    for (const p of pts) {
      const last = segs[segs.length - 1];
      if (last && p[0] - last[last.length - 1][0] <= step * 2.5) last.push(p);
      else segs.push([p]);
    }
    return (
      <g key={i} className={s.className}>
        {segs.map((seg, j) =>
          seg.length === 1 ? (
            <circle key={j} cx={x(seg[0][0])} cy={y(seg[0][1])} r={1.5} fill="currentColor" />
          ) : (
            <polyline
              key={j}
              points={seg.map((p) => `${x(p[0])},${y(p[1])}`).join(" ")}
              fill="none"
              stroke="currentColor"
              strokeWidth={1.25}
              strokeLinejoin="round"
            />
          ),
        )}
      </g>
    );
  };

  // 堆叠柱：每个时刻一根，自下而上依次是各条序列。
  const barW = Math.max(1, (step / span) * width - 1);
  const bars = () =>
    times.map((t) => {
      let acc = 0;
      return (
        <g key={t}>
          {series.map((s, i) => {
            const v = byTime[i].get(t) ?? 0;
            if (v <= 0) return null;
            const top = y(acc + v);
            const bottom = y(acc);
            acc += v;
            return <rect key={i} className={s.className} x={x(t)} y={top} width={barW} height={Math.max(bottom - top, 0.5)} fill="currentColor" />;
          })}
        </g>
      );
    });

  const tipLeft = hover !== null && x(hover) > width / 2;

  return (
    <div className={cn("grid gap-1", className)}>
      <div
        ref={ref}
        className="relative touch-none"
        style={{ height }}
        onPointerMove={(e) => setHover(nearest(e.clientX - e.currentTarget.getBoundingClientRect().left))}
        onPointerLeave={() => setHover(null)}
      >
        {width > 0 && (
          <svg width={width} height={height} className="block overflow-visible" role="img" aria-label={series.map((s) => s.label).join(t.metrics.seriesSeparator)}>
            <line x1={0} x2={width} y1={height - 0.5} y2={height - 0.5} className="stroke-border" />
            {stacked ? bars() : series.map(lines)}
            {hover !== null && <line x1={x(hover)} x2={x(hover)} y1={0} y2={height} className="stroke-foreground/30" strokeDasharray="2 2" />}
          </svg>
        )}
        {axis && !empty && <span className="pointer-events-none absolute top-0 left-0 font-mono text-[10px] leading-none text-muted-foreground/80">{format(peak)}</span>}
        {empty && <span className="absolute inset-0 flex items-center justify-center text-xs text-muted-foreground">{t.metrics.noData}</span>}
        {hover !== null && (
          <div
            className={cn(
              "pointer-events-none absolute top-0 z-10 grid min-w-28 gap-0.5 rounded-md border bg-card px-2 py-1.5 text-[11px] shadow-md",
              tipLeft ? "-translate-x-full" : "",
            )}
            style={{ left: x(hover) + (tipLeft ? -8 : 8) }}
          >
            <span className="text-muted-foreground">{fmtTime(hover, withDate, step < 60)}</span>
            {series.map((s, i) => {
              const v = byTime[i].get(hover);
              return (
                <span key={i} className="flex items-center gap-1.5">
                  <span className={cn("size-1.5 shrink-0 rounded-full bg-current", s.className)} />
                  {series.length > 1 && <span className="text-muted-foreground">{s.label}</span>}
                  <span className="ml-auto pl-2 font-mono tabular-nums">{v === undefined ? "—" : format(v)}</span>
                </span>
              );
            })}
          </div>
        )}
      </div>
      {axis && (
        <div className="flex justify-between font-mono text-[10px] text-muted-foreground/80">
          <span>{fmtTime(from, withDate)}</span>
          <span>{fmtTime(to, withDate)}</span>
        </div>
      )}
    </div>
  );
}

// Legend：图例，彩色圆点加名称。
export function Legend({ series }: { series: { label: string; className: string }[] }) {
  return (
    <span className="flex flex-wrap items-center gap-x-2.5 gap-y-0.5 text-[11px] text-muted-foreground">
      {series.map((s) => (
        <span key={s.label} className="flex items-center gap-1">
          <span className={cn("size-1.5 rounded-full bg-current", s.className)} />
          {s.label}
        </span>
      ))}
    </span>
  );
}
