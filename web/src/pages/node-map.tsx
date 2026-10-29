import { useState } from "react";
import { useQueries, useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { Server } from "lucide-react";
import { nodeClient, toDate } from "@/lib/api";
import { cn, timeAgo } from "@/lib/utils";
import { t } from "@/i18n";
import { HelpTip, Status, Waves } from "@/components/ui";
import { pathLabel } from "@/pages/mesh";
import type { Node } from "@/gen/nagi/v1/api_pb";
import type { MeshStatus } from "@/gen/nagi/v1/common_pb";

// 节点页的拓扑视图：面板在中间，节点围成一圈，线条是 mesh 连接（见 docs/mesh.md）。
// 只画在线、已批准的节点之间的线：离线节点的连接没有意义，待批准的节点还没进 mesh。

const PANEL = "panel";

type Kind = "direct" | "relay" | "none";
type Edge = { a: string; b: string; path: string; kind: Kind; rtt: number };
type Pt = { x: number; y: number };

// 路径从好到差。两端看到的可能不一样（A→B 直连、B→A 中转），取差的那个。
const rank: Record<string, number> = { direct: 0, link: 0, "udp-relay": 1, "tls-relay": 2, none: 3 };
const rankOf = (path: string) => rank[path] ?? 3;
const kindOf = (path: string): Kind => (rankOf(path) === 0 ? "direct" : rankOf(path) === 3 ? "none" : "relay");

// buildEdges 把各端的 mesh 状态合成无向边。成员按 mesh 地址对应到节点（地址由节点自己的状态得知），对不上时按名字。
function buildEdges(nodes: Node[], statuses: Map<string, MeshStatus>, panel: MeshStatus | undefined, visible: Set<string>) {
  const byAddr = new Map<string, string>();
  for (const [id, s] of statuses) if (s.address) byAddr.set(s.address, id);
  if (panel?.address) byAddr.set(panel.address, PANEL);
  const byName = new Map(nodes.map((n) => [n.name, n.id]));

  const edges = new Map<string, Edge>();
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

// layout：节点在圆上的位置。偶数个时转半格，两个节点左右排开，不压在面板上下。
function layout(count: number) {
  const r = count <= 3 ? 180 : Math.min(260, 130 + count * 14);
  const start = -Math.PI / 2 + (count % 2 === 0 ? Math.PI / count : 0);
  const pts: Pt[] = [];
  for (let i = 0; i < count; i++) {
    const a = start + (2 * Math.PI * i) / count;
    pts.push({ x: r * Math.cos(a), y: r * Math.sin(a) });
  }
  const ys = [0, ...pts.map((p) => p.y)];
  const padX = 100; // 名字和占用写在节点下面，左右留出文字的宽度
  const top = Math.min(...ys) - 44;
  const bottom = Math.max(...ys) + 64;
  return { pts, box: { x: -r - padX, y: top, w: 2 * (r + padX), h: bottom - top } };
}

// edgeGeometry：边的路径和曲线上的点 at(s)（s 从 0 到 1，放标签用）。连到面板的是直线；节点之间中转的线经过面板
// （二次曲线的中点正好在面板上），直连的线向外弯一点，免得两个对面的节点之间的直线穿过面板、看起来像中转。
function edgeGeometry(A: Pt, B: Pt, toPanel: boolean, kind: Kind) {
  if (toPanel) return { d: `M${A.x},${A.y}L${B.x},${B.y}`, at: (s: number) => ({ x: A.x + (B.x - A.x) * s, y: A.y + (B.y - A.y) * s }) };
  const M = { x: (A.x + B.x) / 2, y: (A.y + B.y) / 2 };
  let P: Pt;
  if (kind === "relay") {
    P = { x: -M.x, y: -M.y };
  } else {
    const dx = B.x - A.x;
    const dy = B.y - A.y;
    const len = Math.hypot(dx, dy) || 1;
    let nx = -dy / len;
    let ny = dx / len;
    if (nx * M.x + ny * M.y < 0) {
      nx = -nx;
      ny = -ny;
    }
    P = { x: M.x + nx * len * 0.16, y: M.y + ny * len * 0.16 };
  }
  const at = (s: number) => ({
    x: (1 - s) ** 2 * A.x + 2 * (1 - s) * s * P.x + s ** 2 * B.x,
    y: (1 - s) ** 2 * A.y + 2 * (1 - s) * s * P.y + s ** 2 * B.y,
  });
  return { d: `M${A.x},${A.y}Q${P.x},${P.y} ${B.x},${B.y}`, at };
}

const edgeLabel = (e: Edge) => {
  if (e.kind === "direct") return e.rtt > 0 ? `${e.rtt.toFixed(1)} ms` : e.path ? (pathLabel[e.path] ?? e.path) : "";
  return pathLabel[e.path] ?? e.path;
};

// 文字下面垫一圈背景色，压在线上也看得清。
const halo = { stroke: "var(--background)", strokeWidth: 4, strokeLinejoin: "round" as const, paintOrder: "stroke" as const };

export function NodeMap({ nodes }: { nodes: Node[] }) {
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

  const statuses = new Map<string, MeshStatus>();
  live.forEach((n, i) => meshes[i]?.data && statuses.set(n.id, meshes[i].data));
  const visible = new Set([PANEL, ...live.map((n) => n.id)]);
  const edges = buildEdges(nodes, statuses, panelMesh.data, visible);

  const { pts, box } = layout(nodes.length);
  const pos = new Map<string, Pt>([[PANEL, { x: 0, y: 0 }]]);
  nodes.forEach((n, i) => pos.set(n.id, pts[i]));
  const nameOf = (id: string) => (id === PANEL ? t.nodes.map.panel : (nodes.find((n) => n.id === id)?.name ?? id));

  // 悬停或聚焦某个节点时，突出它的连接，其他的淡下去。
  const touches = (e: Edge) => active !== null && (e.a === active || e.b === active);
  const neighbors = new Set(edges.filter(touches).flatMap((e) => [e.a, e.b]));
  const dim = (id: string) => active !== null && !neighbors.has(id) && id !== active;
  const issues = edges.filter((e) => e.kind !== "direct");

  return (
    <div className="grid gap-4">
      <svg
        viewBox={`${box.x} ${box.y} ${box.w} ${box.h}`}
        width={box.w}
        className="mx-auto h-auto max-w-full overflow-visible select-none"
        role="group"
        aria-label={t.nodes.views.map}
      >
        <g fill="none" strokeWidth={1.5} strokeLinecap="round">
          {edges.map((e) => {
            const g = edgeGeometry(pos.get(e.a)!, pos.get(e.b)!, e.a === PANEL || e.b === PANEL, e.kind);
            const on = touches(e);
            return (
              <path
                key={e.a + "|" + e.b}
                d={g.d}
                strokeDasharray={e.kind === "relay" ? "6 5" : e.kind === "none" ? "2 5" : undefined}
                className={cn(
                  "transition-opacity duration-150 ease-out",
                  e.kind === "relay" ? "stroke-warning" : e.kind === "none" ? "stroke-destructive" : on ? "stroke-foreground/60" : "stroke-muted-foreground/35",
                  active !== null && !on && "opacity-15",
                )}
              />
            );
          })}
        </g>

        {edges.filter(touches).map((e) => {
          const g = edgeGeometry(pos.get(e.a)!, pos.get(e.b)!, e.a === PANEL || e.b === PANEL, e.kind);
          const label = edgeLabel(e);
          // 中转的线中点在面板上，标签放在靠近当前节点的一段。
          const mid = g.at(e.kind === "relay" && e.a !== PANEL && e.b !== PANEL ? (e.a === active ? 0.3 : 0.7) : 0.5);
          return (
            label && (
              <text
                key={e.a + "|" + e.b}
                x={mid.x}
                y={mid.y}
                dy="0.35em"
                textAnchor="middle"
                fontSize={11}
                {...halo}
                className={cn("font-mono tabular-nums", e.kind === "none" ? "fill-destructive" : "fill-foreground")}
              >
                {label}
              </text>
            )
          );
        })}

        <g
          className={cn("transition-opacity duration-150 ease-out", dim(PANEL) && "opacity-40")}
          onMouseEnter={() => setActive(PANEL)}
          onMouseLeave={() => setActive(null)}
        >
          <rect x={-24} y={-24} width={48} height={48} rx={12} className="fill-card stroke-border" strokeWidth={1} />
          <Waves x={-13} y={-13} width={26} height={26} className="text-wave" />
          <text y={42} textAnchor="middle" fontSize={13} {...halo} className="fill-foreground font-medium">
            {t.nodes.map.panel}
          </text>
        </g>

        {nodes.map((n) => (
          <NodeGlyph
            key={n.id}
            n={n}
            at={pos.get(n.id)!}
            dim={dim(n.id)}
            onActive={(on) => setActive(on ? n.id : null)}
          />
        ))}
      </svg>

      <div className="flex flex-wrap items-center justify-center gap-x-5 gap-y-2 text-xs text-muted-foreground">
        <LegendLine className="stroke-muted-foreground/60">{t.nodes.map.direct}</LegendLine>
        <LegendLine className="stroke-warning" dash="6 5">
          {t.nodes.map.relay}
        </LegendLine>
        <LegendLine className="stroke-destructive" dash="2 5">
          {t.nodes.map.none}
        </LegendLine>
        <HelpTip>{t.nodes.map.help}</HelpTip>
      </div>

      {issues.length > 0 && (
        <div className="mx-auto grid gap-2">
          <div className="text-xs font-medium text-muted-foreground">{t.nodes.map.issues}</div>
          <ul className="grid gap-2">
            {issues.map((e) => (
              <li
                key={e.a + "|" + e.b}
                className="flex flex-wrap items-center gap-x-3 gap-y-1 text-[13px]"
                onMouseEnter={() => setActive(e.a === PANEL ? e.b : e.a)}
                onMouseLeave={() => setActive(null)}
              >
                <span className="font-medium">
                  {nameOf(e.a)} ↔ {nameOf(e.b)}
                </span>
                <Status tone={e.kind === "none" ? "danger" : "warning"}>{pathLabel[e.path] ?? e.path}</Status>
              </li>
            ))}
          </ul>
        </div>
      )}
    </div>
  );
}

// NodeGlyph：一个节点。外圈是 CPU、内存、磁盘里占用最高的一项（超过 75% 变黄、90% 变红，同占用条），右下角是状态点。
function NodeGlyph({ n, at, dim, onActive }: { n: Node; at: Pt; dim: boolean; onActive: (on: boolean) => void }) {
  const live = n.online && !n.pending;
  const u = live ? n.usage : undefined;
  const ratio = u ? Math.min(Math.max(u.cpu, u.memory, u.disk) / 100, 1) : 0;
  const ring = ratio >= 0.9 ? "stroke-destructive" : ratio >= 0.75 ? "stroke-warning" : "stroke-info";
  const status = n.pending ? t.common.status.pendingApproval : n.online ? t.common.status.online : t.common.status.offline;
  const pct = (v: number) => `${Math.round(v)}%`;
  const sub = n.pending
    ? t.common.status.pendingApproval
    : !n.online
      ? t.nodes.lastSeen(timeAgo(toDate(n.lastSeen)))
      : u
        ? `CPU ${pct(u.cpu)} · ${t.nodes.memory} ${pct(u.memory)}`
        : "";
  const detail = [n.name, status, u && `CPU ${pct(u.cpu)} · ${t.nodes.memory} ${pct(u.memory)} · ${t.nodes.disk} ${pct(u.disk)}`, t.nodes.containers(n.containers)]
    .filter(Boolean)
    .join("\n");

  return (
    <Link
      to="/nodes/$nodeId"
      params={{ nodeId: n.id }}
      aria-label={`${n.name} · ${status}`}
      className={cn("group cursor-pointer outline-none transition-opacity duration-150 ease-out", dim && "opacity-40")}
      onMouseEnter={() => onActive(true)}
      onMouseLeave={() => onActive(false)}
      onFocus={() => onActive(true)}
      onBlur={() => onActive(false)}
    >
      <title>{detail}</title>
      <g transform={`translate(${at.x},${at.y})`} className={cn(!n.online && !n.pending && "opacity-60")}>
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
          strokeWidth={1}
          strokeDasharray={n.pending ? "3 3" : undefined}
          className={cn(
            "fill-card transition-colors",
            n.pending ? "stroke-warning" : "stroke-border group-hover:stroke-foreground/40",
            "group-focus-visible:stroke-ring group-focus-visible:[stroke-width:2]",
          )}
        />
        <Server x={-8} y={-8} width={16} height={16} className={live ? "text-foreground" : "text-muted-foreground"} />
        <circle
          cx={14}
          cy={14}
          r={4.5}
          strokeWidth={2}
          className={cn("stroke-background", n.pending ? "fill-warning" : n.online ? "fill-success" : "fill-muted-foreground/60")}
        />
        <text y={46} textAnchor="middle" fontSize={13} {...halo} className="fill-foreground font-medium group-hover:underline">
          {n.name}
        </text>
        {sub && (
          <text y={62} textAnchor="middle" fontSize={11} {...halo} className="fill-muted-foreground font-mono tabular-nums">
            {sub}
          </text>
        )}
      </g>
    </Link>
  );
}

function LegendLine({ className, dash, children }: { className: string; dash?: string; children: React.ReactNode }) {
  return (
    <span className="inline-flex items-center gap-1.5">
      <svg width={24} height={6} aria-hidden>
        <line x1={1} y1={3} x2={23} y2={3} strokeWidth={1.5} strokeLinecap="round" strokeDasharray={dash} className={className} />
      </svg>
      {children}
    </span>
  );
}
