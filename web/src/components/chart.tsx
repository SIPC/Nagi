import { useLayoutEffect, useRef, useState } from "react";
import { cn } from "@/lib/utils";
import { locale, t } from "@/i18n";

// 时间图表：折线（可有多条）或堆叠柱。横轴是选定的时间范围 [from, to]，不是数据的范围，
// 所以一段时间没有数据时那一段就是空的。鼠标移上去显示该时刻各条序列的值。
// 没有背景网格线和纵坐标，只留图形和 5 个时间刻度；具体数值看上面的数字和悬停提示（见 docs/ui.md）。
// 颜色用 className 里的文字颜色（例如 text-info），图形用 currentColor。

export type Pt = [time: number, value: number]; // time 是 Unix 秒

// faded：堆叠柱里的常规部分（2xx）画淡一些，让 4xx、5xx 显眼。
export type ChartSeries = { label: string; points: Pt[]; className: string; faded?: boolean };

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

const clock = { hour: "2-digit", minute: "2-digit", hourCycle: "h23" } as const;
const timeFormat = new Intl.DateTimeFormat(locale, clock);
const secondsFormat = new Intl.DateTimeFormat(locale, { ...clock, second: "2-digit" });
const dateFormat = new Intl.DateTimeFormat(locale, { ...clock, month: "2-digit", day: "2-digit" });
const dateSecondsFormat = new Intl.DateTimeFormat(locale, { ...clock, second: "2-digit", month: "2-digit", day: "2-digit" });
const dayFormat = new Intl.DateTimeFormat(locale, { month: "2-digit", day: "2-digit" });

// fmtTime：24 小时以内只写时刻，更长的范围带上日期。
export function fmtTime(t: number, withDate: boolean, withSeconds = false) {
  const format = withDate ? (withSeconds ? dateSecondsFormat : dateFormat) : withSeconds ? secondsFormat : timeFormat;
  return format.format(t * 1000);
}

// smoothPath：单调三次曲线，曲线不会冲过数据点，平稳的数据不会画出假的起伏。
function smoothPath(pts: [number, number][]) {
  const n = pts.length;
  const slope = pts.slice(1).map((p, i) => (p[1] - pts[i][1]) / Math.max(p[0] - pts[i][0], 1e-6));
  const tangent = pts.map((_, i) => {
    if (i === 0) return slope[0];
    if (i === n - 1) return slope[n - 2];
    const a = slope[i - 1];
    const b = slope[i];
    return a * b <= 0 ? 0 : (2 * a * b) / (a + b);
  });
  let d = `M${pts[0][0]} ${pts[0][1]}`;
  for (let i = 0; i < n - 1; i++) {
    const h = (pts[i + 1][0] - pts[i][0]) / 3;
    d += `C${pts[i][0] + h} ${pts[i][1] + tangent[i] * h} ${pts[i + 1][0] - h} ${pts[i + 1][1] - tangent[i + 1] * h} ${pts[i + 1][0]} ${pts[i + 1][1]}`;
  }
  return d;
}

export function TimeChart({
  series,
  from,
  to,
  step,
  format,
  stacked,
  height = 112,
  axis = true,
  minMax = 0,
  quiet,
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
  // quiet：没有数据时也不写「暂无数据」（还没有容器时的占位）。
  quiet?: boolean;
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
            <path key={j} d={smoothPath(seg.map((p) => [x(p[0]), y(p[1])]))} fill="none" stroke="currentColor" strokeWidth={1.5} strokeLinejoin="round" strokeLinecap="round" />
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
            return <rect key={i} className={s.className} x={x(t)} y={top} width={barW} height={Math.max(bottom - top, 0.5)} fill="currentColor" opacity={s.faded ? 0.5 : undefined} />;
          })}
        </g>
      );
    });

  const tipLeft = hover !== null && x(hover) > width / 2;
  const ticks = Array.from({ length: 5 }, (_, i) => from + (span * i) / 4);

  return (
    <div className={cn("grid gap-3", className)}>
      <div
        ref={ref}
        // 触屏上横向拖动查看数值，竖向滑动照常滚动页面。
        className="relative touch-pan-y"
        style={{ height }}
        onPointerDown={(e) => setHover(nearest(e.clientX - e.currentTarget.getBoundingClientRect().left))}
        onPointerMove={(e) => setHover(nearest(e.clientX - e.currentTarget.getBoundingClientRect().left))}
        onPointerLeave={() => setHover(null)}
        onPointerCancel={() => setHover(null)}
      >
        {width > 0 && (
          <svg width={width} height={height} className="absolute inset-0 block overflow-visible" role="img" aria-label={t.common.list(series.map((s) => s.label))}>
            {stacked ? bars() : series.map(lines)}
            {hover !== null && <line x1={x(hover)} x2={x(hover)} y1={0} y2={height} className="stroke-muted-foreground" strokeDasharray="3 3" />}
          </svg>
        )}
        {empty && !quiet && <span className="absolute inset-0 flex items-center justify-center text-xs leading-4 text-muted-foreground">{t.metrics.noData}</span>}
        {hover !== null && (
          <div
            className={cn(
              "pointer-events-none absolute top-0 z-10 grid w-max min-w-28 gap-1.5 rounded-lg bg-popover shadow-popover px-2.5 py-1.5 text-xs leading-4 whitespace-nowrap shadow-xl",
              tipLeft ? "-translate-x-full" : "",
            )}
            style={{ left: x(hover) + (tipLeft ? -8 : 8) }}
          >
            <span className="text-muted-foreground">{fmtTime(hover, withDate, step < 60)}</span>
            {series.map((s, i) => {
              const v = byTime[i].get(hover);
              return (
                <div key={i} className="flex items-center gap-2">
                  <i className={cn("size-2.5 shrink-0 rounded-[2px] bg-current", s.className)} />
                  <span className="text-muted-foreground">{s.label}</span>
                  <b className="ml-auto pl-3 font-mono font-medium">{v === undefined ? "—" : format(v)}</b>
                </div>
              );
            })}
          </div>
        )}
      </div>
      {axis && (
        <div aria-hidden className="flex justify-between text-xs leading-4 text-muted-foreground">
          {ticks.map((tick) => (
            <span key={tick}>{withDate ? dayFormat.format(tick * 1000) : fmtTime(tick, false)}</span>
          ))}
        </div>
      )}
    </div>
  );
}

// Legend：图例，10px 的方块加名称。
export function Legend({ series, className }: { series: { label: string; className: string }[]; className?: string }) {
  return (
    <span className={cn("flex flex-wrap gap-4 text-[13px] leading-4 text-muted-foreground", className)}>
      {series.map((s) => (
        <span key={s.label} className="inline-flex shrink-0 items-center gap-1">
          <i className={cn("size-2.5 rounded-[2px] bg-current", s.className)} />
          {s.label}
        </span>
      ))}
    </span>
  );
}
