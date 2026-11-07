import { useLayoutEffect, useRef, useState } from "react";
import { useQueries, useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { Server } from "lucide-react";
import { errorMessage, nodeClient, projectClient, toDate } from "@/lib/api";
import { cn, timeAgo } from "@/lib/utils";
import { t } from "@/i18n";
import { HelpTip, QueryError, RowsSkeleton, Section, Status, Waves } from "@/components/ui";
import { Legend as ChartLegend, TimeChart, type Pt as ChartPt } from "@/components/chart";
import { RangePicker, metricRanges, type MetricRange } from "@/components/metrics";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { pathLabel } from "@/pages/mesh";
import type { Node } from "@/gen/nagi/v1/api_pb";
import type { Container, MeshStatus, Project } from "@/gen/nagi/v1/common_pb";

// 节点页的拓扑视图：面板在最上面，和节点围成一圈，两个图层的位置相同。
// - 网络：在线节点之间的 mesh 连接（见 docs/mesh.md）。
// - 高可用：容器所在节点指向它的热备或备份节点；点选一个节点，模拟它故障时各容器的结果（见 docs/failover.md）。

export type MapLayer = "network" | "ha";

export function NodeMap({ nodes, layer }: { nodes: Node[]; layer: MapLayer }) {
  return layer === "ha" ? <HaMap nodes={nodes} /> : <NetworkMap nodes={nodes} />;
}

// ---- 布局与几何 ----

const PANEL = "panel";

type Pt = { x: number; y: number };

// layout：面板和节点在圆上的位置。面板不在圆心，节点之间的线不用绕开它，都是直线。
// 按可用宽度 avail 算布局，不整体缩放（缩放会让文字小于 12）：放不下时先收窄两侧留给文字的宽度，再缩小圆，
// 圆最小到相邻节点不相碰；还是放不下时横向滚动。avail 为 0（还没量到宽度）时按默认大小。
function layout(nodes: Node[], avail = 0) {
  const count = nodes.length;
  let r = count <= 3 ? 180 : Math.min(260, 130 + count * 14);
  let padX = 170; // 两侧节点的名字和说明写在外侧，留出文字的宽度
  if (avail > 0 && 2 * (r + padX) > avail) {
    padX = Math.max(110, avail / 2 - r);
    // 相邻节点的圆心至少隔 72（圆直径 50，外加状态点和间隙）。
    const minR = Math.max(120, 36 / Math.sin(Math.PI / (count + 1)));
    r = Math.max(minR, Math.min(r, avail / 2 - padX));
  }
  const angle = (i: number) => -Math.PI / 2 + (2 * Math.PI * (i + 1)) / (count + 1);
  const pos = new Map<string, Pt>([[PANEL, { x: 0, y: -r }]]);
  nodes.forEach((n, i) => {
    pos.set(n.id, { x: r * Math.cos(angle(i)), y: r * Math.sin(angle(i)) });
  });
  const ys = [...pos.values()].map((p) => p.y);
  // 上方还要容得下向外弯的线和它的标签（见 bend）：按每对节点之间可能的线算。
  let top = Math.min(...ys) - 64;
  const pts = nodes.map((n) => pos.get(n.id)!);
  for (let i = 0; i < pts.length; i++) {
    for (let j = i + 1; j < pts.length; j++) {
      const P = bend(pts[i], pts[j], 0.16);
      top = Math.min(top, (pts[i].y + 2 * P.y + pts[j].y) / 4 - 14);
    }
  }
  const bottom = Math.max(...ys) + 64;
  // spacing：相邻两个节点圆心的距离，上下方的文字最宽不超过它。
  const spacing = 2 * r * Math.sin(Math.PI / (count + 1));
  return { pos, spacing, box: { x: -r - padX, y: top, w: 2 * (r + padX), h: bottom - top } };
}

type Box = ReturnType<typeof layout>["box"];

// labelSide：文字放在圆的外侧，连线都朝内，压不到字：两侧的节点写在旁边，其余写在上方或下方。
const labelSide = (at: Pt) => Math.abs(at.x) > 0.7 * Math.hypot(at.x, at.y);

// labelWidth：节点名字和说明最宽多少。两侧的写到画布边缘为止；上下方的居中，不超过相邻节点的间距。
function labelWidth(at: Pt, box: Box, spacing: number) {
  const half = box.w / 2;
  if (labelSide(at)) return Math.max(48, half - Math.abs(at.x) - 34 - 4);
  return Math.max(48, Math.min(spacing - 12, 2 * (half - Math.abs(at.x)) - 8));
}

// textWidth：文字的实际宽度，用页面的字体在 canvas 上量；量不了时按汉字 1em、其余 0.58em 估算。
let measure: CanvasRenderingContext2D | null | undefined;
function textWidth(text: string, size: number, weight: number) {
  if (measure === undefined) measure = document.createElement("canvas").getContext("2d");
  if (!measure) return [...text].reduce((w, ch) => w + (ch.charCodeAt(0) > 255 ? 1 : 0.58) * size, 0);
  measure.font = `${weight} ${size}px ${getComputedStyle(document.body).fontFamily}`;
  return measure.measureText(text).width;
}

// clip：放不下时截短，末尾加省略号；完整的文字放在 <title> 里。
function clip(text: string, size: number, max: number, weight = 400) {
  if (textWidth(text, size, weight) <= max) return text;
  let lo = 0;
  let hi = text.length;
  while (lo < hi) {
    const mid = Math.ceil((lo + hi) / 2);
    if (textWidth(text.slice(0, mid) + "…", size, weight) <= max) lo = mid;
    else hi = mid - 1;
  }
  return text.slice(0, Math.max(lo, 1)) + "…";
}

// useWidth：容器的可用宽度，布局按它算。
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

// curve：从 A 经控制点 P 到 B 的二次曲线；at(s) 是曲线上的点（s 从 0 到 1）。
function curve(A: Pt, B: Pt, P: Pt) {
  const at = (s: number) => ({
    x: (1 - s) ** 2 * A.x + 2 * (1 - s) * s * P.x + s ** 2 * B.x,
    y: (1 - s) ** 2 * A.y + 2 * (1 - s) * s * P.y + s ** 2 * B.y,
  });
  return { d: `M${A.x},${A.y}Q${P.x},${P.y} ${B.x},${B.y}`, at };
}

// bend：有向的线向外（远离圆心的一侧）弯的控制点，控制点离弦的距离是长度的 k 倍。弦正好穿过圆心时向上弯。
function bend(A: Pt, B: Pt, k: number): Pt {
  const M = { x: (A.x + B.x) / 2, y: (A.y + B.y) / 2 };
  const dx = B.x - A.x;
  const dy = B.y - A.y;
  const len = Math.hypot(dx, dy) || 1;
  let nx = -dy / len;
  let ny = dx / len;
  let side = nx * M.x + ny * M.y;
  if (side < -0.5 || (Math.abs(side) <= 0.5 && ny > 0)) {
    nx = -nx;
    ny = -ny;
    side = -side;
  }
  return { x: M.x + nx * k * len, y: M.y + ny * k * len };
}

const dist = (a: Pt, b: Pt) => Math.hypot(a.x - b.x, a.y - b.y);

// arrow：从 A 指向 B 的有向曲线，停在 B 的外圈外面，末端画箭头。
function arrow(A: Pt, B: Pt, P: Pt) {
  const c = curve(A, B, P);
  let lo = 0.5;
  let hi = 1;
  for (let i = 0; i < 24; i++) {
    const mid = (lo + hi) / 2;
    if (dist(c.at(mid), B) > 31) lo = mid;
    else hi = mid;
  }
  const tip = c.at(lo);
  const back = c.at(lo - 0.03);
  const len = dist(tip, back) || 1;
  const ux = (tip.x - back.x) / len;
  const uy = (tip.y - back.y) / len;
  // 截到箭头尖：de Casteljau，前一段的控制点是 A 和 P 之间的 lo 处。
  const P1 = { x: A.x + (P.x - A.x) * lo, y: A.y + (P.y - A.y) * lo };
  const head = `M${tip.x},${tip.y}L${tip.x - ux * 8 - uy * 4},${tip.y - uy * 8 + ux * 4}L${tip.x - ux * 8 + uy * 4},${tip.y - uy * 8 - ux * 4}Z`;
  return { d: `M${A.x},${A.y}Q${P1.x},${P1.y} ${tip.x},${tip.y}`, head, at: c.at };
}

// EdgeLabel：线上的标签（延迟、容器数），带圆角底框，线从下面穿过也不会露在字缝里。
// 宽度按字符估算：12px 字体每个半角字符约 7px，全角字符 12px。
function EdgeLabel({ at, text, className }: { at: Pt; text: string; className?: string }) {
  const w = [...text].reduce((s, ch) => s + (/[\u2e80-\uffff]/.test(ch) ? 12 : 7), 0) + 12;
  return (
    <g transform={`translate(${at.x},${at.y})`} className={className}>
      <rect x={-w / 2} y={-9} width={w} height={18} rx={9} strokeWidth={1} className="fill-background stroke-border" />
      <text dy="0.35em" textAnchor="middle" fontSize={12} className="fill-current tabular-nums">
        {text}
      </text>
    </g>
  );
}

// 文字下面垫一圈背景色，压在线上也看得清。
const halo = { stroke: "var(--background)", strokeWidth: 4, strokeLinejoin: "round" as const, paintOrder: "stroke" as const };

// ---- 共用的图形 ----

// MapSvg：按布局的实际大小画，不缩放；比容器宽时（节点很多、窗口很窄）横向滚动。
function MapSvg({ box, avail, label, onBackground, children }: { box: Box; avail: number; label: string; onBackground?: () => void; children: React.ReactNode }) {
  return (
    <div className={cn(avail > 0 && box.w > avail && "overflow-x-auto")}>
      <svg
        viewBox={`${box.x} ${box.y} ${box.w} ${box.h}`}
        width={box.w}
        height={box.h}
        className="mx-auto block overflow-visible select-none"
        role="group"
        aria-label={label}
        onClick={(e) => e.target === e.currentTarget && onBackground?.()}
      >
        {children}
      </svg>
    </div>
  );
}

function PanelGlyph({ at, dim, onActive }: { at: Pt; dim: boolean; onActive?: (on: boolean) => void }) {
  return (
    <g
      transform={`translate(${at.x},${at.y})`}
      className={cn("transition-opacity duration-150 ease-out", dim && "opacity-40")}
      onMouseEnter={() => onActive?.(true)}
      onMouseLeave={() => onActive?.(false)}
    >
      <rect x={-24} y={-24} width={48} height={48} rx={12} className="fill-card stroke-border" strokeWidth={1} />
      <Waves x={-13} y={-13} width={26} height={26} className="text-wave" />
      <text y={-34} textAnchor="middle" fontSize={13} {...halo} className="fill-foreground font-medium">
        {t.nodes.map.panel}
      </text>
    </g>
  );
}

const nodeStatus = (n: Node) => (n.pending ? t.common.status.pendingApproval : n.online ? t.common.status.online : t.common.status.offline);

// statusLine：离线、待批准的节点在名字下面写状态，在线时返回空，由图层自己写。
const statusLine = (n: Node) =>
  n.pending ? t.common.status.pendingApproval : !n.online ? (n.lastSeen ? t.nodes.lastSeen(timeAgo(toDate(n.lastSeen))) : t.nodes.neverSeen) : "";

const pct = (v: number) => `${Math.round(v)}%`;

// NodeGlyph：一个节点。外圈是 CPU、内存、磁盘里占用最高的一项（超过 75% 变黄、90% 变红，同占用条），右下角是状态点。
// 有 onSelect 时是按钮（高可用图层点选节点），否则是到节点详情页的链接。failed：高可用图层里模拟故障的节点。
function NodeGlyph({
  n,
  at,
  dim,
  sub,
  warn,
  maxWidth,
  failed,
  onActive,
  onSelect,
}: {
  n: Node;
  at: Pt;
  dim: boolean;
  // sub：名字下面的说明；warn：接在说明后面、用警告色写的一段（无副本的容器数）。
  sub: string;
  warn?: string;
  maxWidth: number;
  failed?: boolean;
  onActive: (on: boolean) => void;
  onSelect?: () => void;
}) {
  const live = n.online && !n.pending;
  const u = live ? n.usage : undefined;
  // CPU 按每核 100% 累加，换成占整机的比例再和内存、磁盘比。
  const cpu = u ? (n.info?.cpus ? u.cpu / n.info.cpus : u.cpu) : 0;
  const ratio = u ? Math.min(Math.max(cpu, u.memory, u.disk) / 100, 1) : 0;
  const ring = ratio >= 0.9 ? "stroke-destructive" : ratio >= 0.75 ? "stroke-warning" : "stroke-primary";
  const status = nodeStatus(n);
  const side = labelSide(at);
  const above = at.y < 0;
  const textX = side ? Math.sign(at.x) * 34 : 0;
  const anchor = !side ? "middle" : at.x > 0 ? "start" : "end";
  const nameY = side ? (sub ? -3 : 4) : above ? (sub ? -50 : -34) : 46;
  const subY = side ? 13 : above ? -34 : 62;
  const detail = [n.name, status, u && `CPU ${pct(u.cpu)} · ${t.nodes.memory} ${pct(u.memory)} · ${t.nodes.disk} ${pct(u.disk)}`, t.nodes.containers(n.containers)]
    .filter(Boolean)
    .join("\n");
  // 放不下时截短：名字和说明各自截，警告的那段跟在说明后面一起算宽度。
  const name = clip(n.name, 13, maxWidth, 500);
  const fullSub = warn ? `${sub} · ${warn}` : sub;
  const shownSub = clip(fullSub, 12, maxWidth);
  const subHead = shownSub.slice(0, sub.length);
  const subTail = shownSub.slice(sub.length);

  const inner = (
    <>
      <title>{detail}</title>
      <g transform={`translate(${at.x},${at.y})`} className={cn(!n.online && !n.pending && !failed && "opacity-60")}>
        <circle r={25} className="fill-background stroke-foreground/10" strokeWidth={3} />
        {ratio > 0 && (
          <circle
            r={25}
            pathLength={100}
            strokeDasharray={`${Math.max(ratio * 100, 2)} 100`}
            strokeLinecap="round"
            transform="rotate(-90)"
            fill="none"
            strokeWidth={3}
            className={cn(ring, "transition-[stroke-dasharray] duration-500 ease-[cubic-bezier(0.23,1,0.32,1)] motion-reduce:transition-none")}
          />
        )}
        <circle
          r={19}
          strokeWidth={failed ? 2 : 1}
          strokeDasharray={n.pending ? "3 3" : undefined}
          className={cn(
            "fill-card transition-colors",
            failed ? "stroke-destructive" : n.pending ? "stroke-warning" : "stroke-border group-hover:stroke-foreground/40",
            "group-focus-visible:stroke-ring group-focus-visible:[stroke-width:2]",
          )}
        />
        <Server x={-8} y={-8} width={16} height={16} className={failed ? "text-destructive" : live ? "text-foreground" : "text-muted-foreground"} />
        <circle
          cx={14}
          cy={14}
          r={4.5}
          strokeWidth={2}
          className={cn(
            "stroke-background",
            failed ? "fill-destructive" : n.pending ? "fill-warning" : n.online ? "fill-success" : "fill-muted-foreground/60",
          )}
        />
        <text x={textX} y={nameY} textAnchor={anchor} fontSize={13} {...halo} className="fill-foreground font-medium group-hover:underline">
          {name !== n.name && <title>{n.name}</title>}
          {name}
        </text>
        {sub && (
          <text x={textX} y={subY} textAnchor={anchor} fontSize={12} {...halo} className="fill-muted-foreground tabular-nums">
            {shownSub !== fullSub && <title>{fullSub}</title>}
            {subHead}
            {subTail && <tspan className="fill-warning-text">{subTail}</tspan>}
          </text>
        )}
      </g>
    </>
  );

  const props = {
    className: cn("group cursor-pointer outline-none transition-opacity duration-150 ease-out", dim && "opacity-40"),
    onMouseEnter: () => onActive(true),
    onMouseLeave: () => onActive(false),
    onFocus: () => onActive(true),
    onBlur: () => onActive(false),
  };
  if (onSelect) {
    return (
      <g
        {...props}
        role="button"
        tabIndex={0}
        aria-pressed={failed}
        aria-label={`${n.name} · ${status}`}
        onClick={onSelect}
        onKeyDown={(e) => {
          if (e.key === "Enter" || e.key === " ") {
            e.preventDefault();
            onSelect();
          }
        }}
      >
        {inner}
      </g>
    );
  }
  return (
    <Link to="/nodes/$nodeId" params={{ nodeId: n.id }} aria-label={`${n.name} · ${status}`} {...props}>
      {inner}
    </Link>
  );
}

// headClass：有箭头时箭头的填充色（类名要写全，Tailwind 才能生成）。
function LegendLine({ className, dash, headClass, children }: { className: string; dash?: string; headClass?: string; children: React.ReactNode }) {
  return (
    <span className="inline-flex items-center gap-2">
      <svg width={24} height={8} aria-hidden>
        <line x1={1} y1={4} x2={headClass ? 17 : 23} y2={4} strokeWidth={1.5} strokeLinecap="round" strokeDasharray={dash} className={className} />
        {headClass && <path d="M23,4L15,0L15,8Z" className={headClass} />}
      </svg>
      {children}
    </span>
  );
}

function Legend({ children }: { children: React.ReactNode }) {
  return <div className="flex flex-wrap items-center justify-center gap-x-4 gap-y-2 text-xs text-muted-foreground">{children}</div>;
}

// ---- 网络图层 ----

type Kind = "direct" | "relay" | "none";
type MeshEdge = { a: string; b: string; path: string; kind: Kind; rtt: number };

// 路径从好到差。两端看到的可能不一样（A→B 直连、B→A 中转），取差的那个。
const rank: Record<string, number> = { direct: 0, link: 0, "udp-relay": 1, "tls-relay": 2, none: 3 };
const rankOf = (path: string) => rank[path] ?? 3;
const kindOf = (path: string): Kind => (rankOf(path) === 0 ? "direct" : rankOf(path) === 3 ? "none" : "relay");

// buildMeshEdges 把各端的 mesh 状态合成无向边。成员按 mesh 地址对应到节点（地址由节点自己的状态得知），对不上时按名字。
function buildMeshEdges(nodes: Node[], statuses: Map<string, MeshStatus>, panel: MeshStatus | undefined, visible: Set<string>) {
  const byAddr = new Map<string, string>();
  for (const [id, s] of statuses) if (s.address) byAddr.set(s.address, id);
  if (panel?.address) byAddr.set(panel.address, PANEL);
  const byName = new Map(nodes.map((n) => [n.name, n.id]));

  const edges = new Map<string, MeshEdge>();
  const add = (from: string, to: string | undefined, path: string, rtt: number) => {
    if (!to || from === to || !visible.has(from) || !visible.has(to)) return;
    const [a, b] = from < to ? [from, to] : [to, from];
    const old = edges.get(a + "|" + b);
    if (!old) {
      edges.set(a + "|" + b, { a, b, path, kind: kindOf(path), rtt: rankOf(path) === 0 ? rtt : 0 });
      return;
    }
    if (rankOf(path) > rankOf(old.path)) {
      old.path = path;
      old.kind = kindOf(path);
    }
    if (old.kind === "direct" && rtt > 0) old.rtt = old.rtt > 0 ? (old.rtt + rtt) / 2 : rtt;
  };
  const scan = (from: string, s: MeshStatus) => {
    for (const p of s.peers) add(from, byAddr.get(p.address) ?? byName.get(p.name), p.path, p.rttMs);
  };
  for (const [id, s] of statuses) scan(id, s);
  if (panel) scan(PANEL, panel);
  // 在线的节点和面板之间一定有控制连接；还没查到 mesh 状态时也画上，按直连的样式。
  for (const id of visible) {
    if (id === PANEL) continue;
    const [a, b] = id < PANEL ? [id, PANEL] : [PANEL, id];
    if (!edges.has(a + "|" + b)) edges.set(a + "|" + b, { a, b, path: "", kind: "direct", rtt: 0 });
  }
  return [...edges.values()];
}

// meshGeometry：都是直线；只有节点之间中转的线经过面板（控制点取中点关于面板的对称点，曲线中点正好在面板上）。
function meshGeometry(A: Pt, B: Pt, panel: Pt, toPanel: boolean, kind: Kind) {
  const M = { x: (A.x + B.x) / 2, y: (A.y + B.y) / 2 };
  return curve(A, B, kind === "relay" && !toPanel ? { x: 2 * panel.x - M.x, y: 2 * panel.y - M.y } : M);
}

const meshLabel = (e: MeshEdge) => {
  if (e.kind === "direct") return e.rtt > 0 ? `${e.rtt.toFixed(1)} ms` : e.path ? (pathLabel[e.path] ?? e.path) : "";
  return pathLabel[e.path] ?? e.path;
};

function NetworkMap({ nodes }: { nodes: Node[] }) {
  const [active, setActive] = useState<string | null>(null);
  const live = nodes.filter((n) => n.online && !n.pending);
  const meshes = useQueries({
    queries: live.map((n) => ({
      queryKey: ["node-mesh", n.id],
      queryFn: () => nodeClient.getNodeMesh({ nodeId: n.id }),
      refetchInterval: 10000,
    })),
  });
  const panelMesh = useQuery({ queryKey: ["panel-mesh"], queryFn: () => nodeClient.getPanelMesh({}), refetchInterval: 10000 });
  // 查不到的一端只能按默认样式画，所以要写出来；有旧数据时偶尔失败一次不提示。
  const failedMesh = [...meshes, panelMesh].filter((q) => q.error && !q.data);

  const statuses = new Map<string, MeshStatus>();
  live.forEach((n, i) => meshes[i]?.data && statuses.set(n.id, meshes[i].data));
  const edges = buildMeshEdges(nodes, statuses, panelMesh.data, new Set([PANEL, ...live.map((n) => n.id)]));
  const { ref, width } = useWidth();
  const { pos, box, spacing } = layout(nodes, width);
  const nameOf = (id: string) => (id === PANEL ? t.nodes.map.panel : (nodes.find((n) => n.id === id)?.name ?? id));

  // 悬停或聚焦某个节点时，突出它的连接，其他的淡下去。
  const touches = (e: MeshEdge) => active !== null && (e.a === active || e.b === active);
  const neighbors = new Set(edges.filter(touches).flatMap((e) => [e.a, e.b]));
  const dim = (id: string) => active !== null && !neighbors.has(id) && id !== active;
  const issues = edges.filter((e) => e.kind !== "direct");
  const geometry = (e: MeshEdge) => meshGeometry(pos.get(e.a)!, pos.get(e.b)!, pos.get(PANEL)!, e.a === PANEL || e.b === PANEL, e.kind);

  return (
    <div ref={ref} className="grid gap-4">
      <MapSvg box={box} avail={width} label={t.nodes.views.network}>
        <g fill="none" strokeWidth={1.5} strokeLinecap="round">
          {edges.map((e) => {
            const on = touches(e);
            return (
              <path
                key={e.a + "|" + e.b}
                d={geometry(e).d}
                strokeDasharray={e.kind === "relay" ? "6 5" : e.kind === "none" ? "2 5" : undefined}
                className={cn(
                  "transition-opacity duration-150 ease-out",
                  // 直连的灰线和图例同一个透明度；突出时加深。
                  e.kind === "relay" ? "stroke-warning" : e.kind === "none" ? "stroke-destructive" : on ? "stroke-foreground/70" : "stroke-muted-foreground/60",
                  active !== null && !on && "opacity-15",
                )}
              />
            );
          })}
        </g>

        {edges.filter(touches).map((e) => {
          const label = meshLabel(e);
          // 中转的线中点在面板上，标签放在靠近当前节点的一段。
          const mid = geometry(e).at(e.kind === "relay" && e.a !== PANEL && e.b !== PANEL ? (e.a === active ? 0.3 : 0.7) : 0.5);
          return (
            label && <EdgeLabel key={e.a + "|" + e.b} at={mid} text={label} className={e.kind === "none" ? "text-destructive" : e.kind === "relay" ? "text-warning-text" : "text-foreground"} />
          );
        })}

        <PanelGlyph at={pos.get(PANEL)!} dim={dim(PANEL)} onActive={(on) => setActive(on ? PANEL : null)} />
        {nodes.map((n) => (
          <NodeGlyph
            key={n.id}
            n={n}
            at={pos.get(n.id)!}
            dim={dim(n.id)}
            sub={statusLine(n) || (n.usage ? `CPU ${pct(n.usage.cpu)} · ${t.nodes.memory} ${pct(n.usage.memory)}` : "")}
            maxWidth={labelWidth(pos.get(n.id)!, box, spacing)}
            onActive={(on) => setActive(on ? n.id : null)}
          />
        ))}
      </MapSvg>

      <Legend>
        <LegendLine className="stroke-muted-foreground/60">{t.nodes.map.direct}</LegendLine>
        <LegendLine className="stroke-warning" dash="6 5">
          {t.nodes.map.relay}
        </LegendLine>
        <LegendLine className="stroke-destructive" dash="2 5">
          <span className="text-destructive">{t.nodes.map.none}</span>
        </LegendLine>
        <HelpTip>{t.nodes.map.help}</HelpTip>
      </Legend>

      {/* 图下的内容和下面的区块一样靠左。 */}
      {failedMesh.length > 0 && <QueryError error={errorMessage(failedMesh[0].error)} onRetry={() => failedMesh.forEach((q) => q.refetch())} />}

      {issues.length > 0 && (
        <Section title={t.nodes.map.issues} className={belowMap}>
          {/* 悬停或聚焦一行时在图上突出这条连接，键盘也能用。 */}
          <ul className="grid">
            {issues.map((e) => (
              <li
                key={e.a + "|" + e.b}
                tabIndex={0}
                className="-mx-2 flex min-h-9 flex-wrap items-center gap-x-3 gap-y-1 rounded-md px-2 outline-none transition-colors hover:bg-accent focus-ring"
                onMouseEnter={() => setActive(e.a === PANEL ? e.b : e.a)}
                onMouseLeave={() => setActive(null)}
                onFocus={() => setActive(e.a === PANEL ? e.b : e.a)}
                onBlur={() => setActive(null)}
              >
                <span>
                  {nameOf(e.a)} ↔ {nameOf(e.b)}
                </span>
                <span className={cn("text-[13px]", e.kind === "none" ? "text-destructive" : "text-warning-text")}>
                  <Status tone={e.kind === "none" ? "danger" : "warning"}>{pathLabel[e.path] ?? e.path}</Status>
                </span>
              </li>
            ))}
          </ul>
        </Section>
      )}

      <LatencyMatrix nodes={live} statuses={statuses} />
    </div>
  );
}

// belowMap：图下的区块。外层网格已经隔了 16，上面再补 8，分隔线上下各 24（手机上各 16）。
const belowMap = "mt-2 max-md:mt-0";

// ---- 延迟矩阵 ----

type Cell = { path: string; rtt: number };

const toHex = (b: Uint8Array) => Array.from(b, (x) => x.toString(16).padStart(2, "0")).join("");
const fmtMs = (v: number) => `${v.toFixed(v < 10 ? 1 : 0)} ms`;

// heat：直连延迟的底色，越慢越深。只用灰度，有问题的连接（中转、不可达）才带颜色。
function heat(rtt: number) {
  if (rtt < 10) return "bg-foreground/[0.04]";
  if (rtt < 50) return "bg-foreground/[0.08]";
  if (rtt < 150) return "bg-foreground/[0.14]";
  return "bg-foreground/[0.22]";
}

// LatencyMatrix：节点两两之间的当前延迟，每格是行所在节点测到列所在节点的值（来自各节点的 mesh 状态）。
// 点选一格，下面显示这两个节点之间两个方向的延迟历史（节点存的 m:<成员公钥>:rtt 序列，见 docs/monitoring.md）。
function LatencyMatrix({ nodes, statuses }: { nodes: Node[]; statuses: Map<string, MeshStatus> }) {
  const k = t.nodes.latency;
  const [pick, setPick] = useState<[string, string] | null>(null);
  if (nodes.length < 2) return null;
  const byAddr = new Map<string, string>();
  for (const [id, s] of statuses) if (s.address) byAddr.set(s.address, id);
  const cells = new Map<string, Cell>();
  for (const [id, s] of statuses) {
    for (const p of s.peers) {
      const to = byAddr.get(p.address);
      if (to) cells.set(id + "|" + to, { path: p.path, rtt: p.path === "direct" ? p.rttMs : 0 });
    }
  }
  const selected = pick && nodes.some((n) => n.id === pick[0]) && nodes.some((n) => n.id === pick[1]) ? pick : null;
  const isPicked = (a: string, b: string) => !!selected && ((selected[0] === a && selected[1] === b) || (selected[0] === b && selected[1] === a));

  const cell = (from: Node, to: Node) => {
    if (from.id === to.id) return <td key={to.id} aria-hidden className="h-9" />;
    const c = cells.get(from.id + "|" + to.id);
    const on = isPicked(from.id, to.id);
    const relay = !!c && rankOf(c.path) > 0;
    const text = c && c.rtt > 0 ? fmtMs(c.rtt) : relay ? (c!.path === "none" ? k.none : k.relay) : "—";
    return (
      <td key={to.id} className="p-0">
        <button
          type="button"
          onClick={() => setPick(on ? null : [from.id, to.id])}
          aria-pressed={on}
          aria-label={k.cell(k.direction(from.name, to.name), text)}
          className={cn(
            "h-9 w-full cursor-pointer rounded-md px-2 text-[13px] tabular-nums outline-none transition-shadow duration-150 focus-ring",
            c && c.rtt > 0 ? heat(c.rtt) : "bg-muted/40",
            c?.path === "none" ? "text-destructive" : relay ? "text-warning-text" : c && c.rtt > 0 ? "text-foreground" : "text-muted-foreground",
            on && "shadow-[0_0_0_1.5px_var(--foreground)]",
          )}
        >
          {text}
        </button>
      </td>
    );
  };

  return (
    <Section title={k.title} help={k.help} className={belowMap}>
      <div className="-mx-1 overflow-x-auto">
        <table className="w-full table-fixed border-separate border-spacing-1 text-left" style={{ minWidth: 112 + nodes.length * 84 }}>
          <colgroup>
            <col className="w-28" />
          </colgroup>
          <thead>
            <tr>
              <td />
              {nodes.map((n) => (
                <th key={n.id} scope="col" className="truncate px-1 pb-1 text-center text-[13px] font-normal text-muted-foreground" title={n.name}>
                  {n.name}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {nodes.map((from) => (
              <tr key={from.id}>
                <th scope="row" className="truncate pr-2 text-[13px] font-normal text-muted-foreground" title={from.name}>
                  {from.name}
                </th>
                {nodes.map((to) => cell(from, to))}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {selected ? (
        <LatencyHistory a={nodes.find((n) => n.id === selected[0])!} b={nodes.find((n) => n.id === selected[1])!} statuses={statuses} />
      ) : (
        <p className="text-[13px] text-muted-foreground">{k.hint}</p>
      )}
    </Section>
  );
}

// LatencyHistory：两个节点之间两个方向的延迟历史，各自从测量的那一端查。
function LatencyHistory({ a, b, statuses }: { a: Node; b: Node; statuses: Map<string, MeshStatus> }) {
  const k = t.nodes.latency;
  const [range, setRange] = useState<MetricRange>("1h");
  const r = metricRanges.find((x) => x.value === range)!;
  const keyA = statuses.get(a.id)?.publicKey;
  const keyB = statuses.get(b.id)?.publicKey;
  const q = useQuery({
    queryKey: ["latency-history", a.id, b.id, range],
    queryFn: async () => {
      const from = timestampFromDate(new Date(Date.now() - r.seconds * 1000));
      const one = async (node: Node, peer: Uint8Array): Promise<ChartPt[]> => {
        const name = `m:${toHex(peer)}:rtt`;
        const res = await nodeClient.queryNodeMetrics({ nodeId: node.id, prefix: name, from, step: BigInt(r.step) });
        const s = res.series.find((x) => x.name === name);
        return s ? s.values.map((v, i) => [Number(s.times[i]), v]) : [];
      };
      return Promise.all([one(a, keyB!), one(b, keyA!)]);
    },
    enabled: !!keyA && !!keyB,
    refetchInterval: 10000,
    placeholderData: (prev) => prev,
  });
  const series = [
    // 正常数据用灰，两个方向用深浅区分。
    { label: k.direction(a.name, b.name), points: q.data?.[0] ?? [], className: "text-foreground" },
    { label: k.direction(b.name, a.name), points: q.data?.[1] ?? [], className: "text-muted-foreground" },
  ];
  const now = Date.now() / 1000;
  return (
    <div className="grid gap-3">
      <div className="flex flex-wrap items-center justify-between gap-x-4 gap-y-2">
        <ChartLegend series={series} />
        <RangePicker value={range} onChange={setRange} />
      </div>
      {q.error ? (
        <QueryError error={errorMessage(q.error)} onRetry={() => q.refetch()} />
      ) : (
        // 加载中不写「暂无数据」，图框留着，数据到了直接画上。
        <TimeChart series={series} from={now - r.seconds} to={now} step={r.step} format={fmtMs} minMax={5} quiet={q.isLoading} />
      )}
    </div>
  );
}

// ---- 高可用图层 ----

type Item = { p: Project; c: Container };
type HaEdge = { from: string; to: string; type: "failover" | "backup"; count: number };
type Tone = "success" | "warning" | "danger" | "neutral";
type Group = { key: string; label: string; tone: Tone; order: number; items: Item[] };

// standbyOf：顶替的另一方（热备）。A、B 里不是容器当前所在节点的那个。
function standbyOf(c: Container) {
  const f = c.failover;
  if (!f) return undefined;
  return f.a?.nodeId === c.nodeId ? f.b : f.a;
}

// unprotected：有状态、但没有顶替也没有备份节点的容器，所在节点坏了数据就找不回来。
const unprotected = (c: Container) => c.stateful && !c.failover && !c.backupNodeId;

// fateOf：容器所在的节点故障时会怎样（按 docs/failover.md）。顶替要热备和见证人都在：A 坏了，B 加见证人才是多数。
function fateOf(c: Container, online: (id: string) => boolean, name: (id: string) => string): Omit<Group, "items"> {
  const sb = standbyOf(c);
  if (c.failover && sb?.nodeId) {
    const target = name(sb.nodeId);
    if (!online(sb.nodeId)) return { key: "blocked-sb:" + sb.nodeId, label: t.nodes.ha.standbyOffline(target), tone: "danger", order: 1 };
    const w = c.failover.witness?.nodeId;
    if (!c.failover.witnessIsPanel && w && !online(w)) return { key: "blocked-w:" + w, label: t.nodes.ha.witnessOffline(name(w)), tone: "danger", order: 1 };
    if (c.stateful && !c.failover.autoStateful) return { key: "confirm:" + sb.nodeId, label: t.nodes.ha.confirm(target), tone: "warning", order: 3 };
    return { key: "auto:" + sb.nodeId, label: t.nodes.ha.auto(target), tone: "success", order: 5 };
  }
  if (c.backupNodeId) {
    const target = name(c.backupNodeId);
    if (!online(c.backupNodeId)) return { key: "backup-off:" + c.backupNodeId, label: t.nodes.ha.backupOffline(target), tone: "danger", order: 2 };
    return { key: "backup:" + c.backupNodeId, label: t.nodes.ha.backup(target), tone: "warning", order: 4 };
  }
  if (c.stateful) return { key: "lost", label: t.nodes.ha.lost, tone: "danger", order: 0 };
  return { key: "redeploy", label: t.nodes.ha.redeploy, tone: "neutral", order: 6 };
}

function HaMap({ nodes }: { nodes: Node[] }) {
  const [hover, setHover] = useState<string | null>(null);
  const [selected, setSelected] = useState<string | null>(null);
  const projects = useQuery({ queryKey: ["projects"], queryFn: () => projectClient.listProjects({}), refetchInterval: 10000 });

  // 只看部署过的容器：没部署过的没有在运行，也没有副本。
  const items: Item[] = (projects.data?.projects ?? []).flatMap((s) =>
    s.project ? s.project.containers.filter((c) => c.deployedVersion > 0n).map((c) => ({ p: s.project!, c })) : [],
  );
  const edgeMap = new Map<string, HaEdge>();
  const addEdge = (from: string, to: string | undefined, type: HaEdge["type"]) => {
    if (!to || from === to) return;
    const key = `${from}|${to}|${type}`;
    const e = edgeMap.get(key) ?? { from, to, type, count: 0 };
    e.count++;
    edgeMap.set(key, e);
  };
  const risky = new Map<string, number>();
  for (const { c } of items) {
    if (c.failover) addEdge(c.nodeId, standbyOf(c)?.nodeId, "failover");
    else if (c.backupNodeId) addEdge(c.nodeId, c.backupNodeId, "backup");
    if (unprotected(c)) risky.set(c.nodeId, (risky.get(c.nodeId) ?? 0) + 1);
  }
  const edges = [...edgeMap.values()].filter((e) => nodes.some((n) => n.id === e.from) && nodes.some((n) => n.id === e.to));
  const { ref, width } = useWidth();
  const { pos: layoutPos, box, spacing } = layout(nodes, width);
  // 箭头都朝外弯。两个节点互相指向时，ID 大的一方发出的弯得更多，两条一里一外；
  // 同一方向既有顶替又有备份时，备份弯得多一些。
  const directed = new Set(edges.map((e) => e.from + "|" + e.to));
  const geometry = (e: HaEdge) => {
    const A = layoutPos.get(e.from)!;
    const B = layoutPos.get(e.to)!;
    const outer = e.from > e.to && directed.has(e.to + "|" + e.from);
    const k = (e.type === "backup" ? 0.28 : 0.14) * (outer ? 2.4 : 1);
    return arrow(A, B, bend(A, B, k));
  };

  const focus = hover ?? selected;
  const touches = (e: HaEdge) => focus !== null && (e.from === focus || e.to === focus);
  const neighbors = new Set(edges.filter(touches).flatMap((e) => [e.from, e.to]));
  const dim = (id: string) => focus !== null && id !== focus && !neighbors.has(id);
  const failedNode = nodes.find((n) => n.id === focus);

  return (
    <div ref={ref} className="grid gap-4" onKeyDown={(e) => e.key === "Escape" && setSelected(null)}>
      <MapSvg box={box} avail={width} label={t.nodes.views.ha} onBackground={() => setSelected(null)}>
        {edges.map((e) => {
          const g = geometry(e);
          const out = focus !== null && e.from === focus;
          return (
            <g
              key={`${e.from}|${e.to}|${e.type}`}
              className={cn("transition-opacity duration-150 ease-out", focus !== null && !touches(e) && "opacity-15")}
            >
              <path
                d={g.d}
                fill="none"
                strokeWidth={1.5}
                strokeLinecap="round"
                strokeDasharray={e.type === "backup" ? "6 5" : undefined}
                className={out ? "stroke-foreground/80" : "stroke-muted-foreground/60"}
              />
              <path d={g.head} className={out ? "fill-foreground/80" : "fill-muted-foreground/60"} />
            </g>
          );
        })}
        {edges.map((e) => {
          const mid = geometry(e).at(0.5);
          return (
            <EdgeLabel
              key={`${e.from}|${e.to}|${e.type}`}
              at={mid}
              text={e.type === "failover" ? t.nodes.ha.failoverEdge(e.count) : t.nodes.ha.backupEdge(e.count)}
              className={cn(
                "transition-opacity duration-150 ease-out",
                focus !== null && e.from === focus ? "text-foreground" : "text-muted-foreground",
                focus !== null && !touches(e) && "opacity-15",
              )}
            />
          );
        })}

        <PanelGlyph at={layoutPos.get(PANEL)!} dim={focus !== null} />
        {nodes.map((n) => {
          const k = risky.get(n.id) ?? 0;
          return (
            <NodeGlyph
              key={n.id}
              n={n}
              at={layoutPos.get(n.id)!}
              dim={dim(n.id)}
              failed={focus === n.id}
              sub={statusLine(n) || t.nodes.containers(n.containers)}
              warn={!statusLine(n) && k > 0 ? t.nodes.ha.unprotected(k) : undefined}
              maxWidth={labelWidth(layoutPos.get(n.id)!, box, spacing)}
              onActive={(on) => setHover(on ? n.id : null)}
              onSelect={() => setSelected((s) => (s === n.id ? null : n.id))}
            />
          );
        })}
      </MapSvg>

      <Legend>
        <LegendLine className="stroke-muted-foreground/60" headClass="fill-muted-foreground/60">
          {t.nodes.ha.legendFailover}
        </LegendLine>
        <LegendLine className="stroke-muted-foreground/60" dash="6 5" headClass="fill-muted-foreground/60">
          {t.nodes.ha.legendBackup}
        </LegendLine>
        <HelpTip>{t.nodes.ha.help}</HelpTip>
      </Legend>

      <div className="mx-auto w-full max-w-xl">
        {projects.error && !projects.data ? (
          <QueryError error={errorMessage(projects.error)} onRetry={() => projects.refetch()} />
        ) : projects.isPending ? (
          <RowsSkeleton count={2} />
        ) : failedNode ? (
          <FailureReport node={failedNode} nodes={nodes} items={items} />
        ) : (
          <p className="text-center text-[13px] text-muted-foreground">{t.nodes.ha.hint}</p>
        )}
      </div>
    </div>
  );
}

// FailureReport：模拟 node 故障：它上面的容器按结果分组，最严重的在前；另外列出把它当作热备或备份节点的容器（失去副本）。
function FailureReport({ node, nodes, items }: { node: Node; nodes: Node[]; items: Item[] }) {
  const online = (id: string) => id !== node.id && nodes.some((n) => n.id === id && n.online && !n.pending);
  const name = (id: string) => nodes.find((n) => n.id === id)?.name ?? id;
  const groups = new Map<string, Group>();
  const put = (g: Omit<Group, "items">, it: Item) => {
    const old = groups.get(g.key) ?? { ...g, items: [] };
    old.items.push(it);
    groups.set(g.key, old);
  };
  for (const it of items) {
    if (it.c.nodeId === node.id) put(fateOf(it.c, online, name), it);
    else if (standbyOf(it.c)?.nodeId === node.id || it.c.backupNodeId === node.id)
      put({ key: "collateral", label: t.nodes.ha.collateral, tone: "neutral", order: 7 }, it);
  }
  const list = [...groups.values()].sort((a, b) => a.order - b.order);

  return (
    <div className="grid gap-3 border-t pt-4">
      <div className="flex items-center justify-between gap-3">
        {/* 页面标题之下的第一级，用 h2；字号是分组标题的 14 / 500。 */}
        <h2 className="text-sm font-medium">{t.nodes.ha.title(node.name)}</h2>
        <Link to="/nodes/$nodeId" params={{ nodeId: node.id }} className="text-[13px] text-muted-foreground hover:text-foreground">
          {t.nodes.ha.open} →
        </Link>
      </div>
      {list.length === 0 ? (
        <p className="text-[13px] text-muted-foreground">{t.nodes.ha.empty}</p>
      ) : (
        <ul className="grid gap-3">
          {list.map((g) => (
            <li key={g.key} className="grid gap-1">
              <span className={g.tone === "danger" ? "text-destructive" : g.tone === "warning" ? "text-warning-text" : undefined}>
                <Status tone={g.tone}>{g.label}</Status>
              </span>
              <div className="flex flex-wrap gap-x-3 gap-y-1 pl-3.5 text-[13px]">
                {g.items.map(({ p, c }) => (
                  <Link
                    key={p.id + "/" + c.name}
                    to="/projects/$projectId/c/$container"
                    params={{ projectId: p.id, container: c.name }}
                    className="text-muted-foreground hover:text-foreground hover:underline"
                  >
                    {p.name} / {c.name}
                  </Link>
                ))}
              </div>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
