import { useState } from "react";
import { useQueries, useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { Server } from "lucide-react";
import { nodeClient, projectClient, toDate } from "@/lib/api";
import { cn, timeAgo } from "@/lib/utils";
import { t } from "@/i18n";
import { HelpTip, Status, Waves } from "@/components/ui";
import { pathLabel } from "@/pages/mesh";
import type { Node } from "@/gen/nagi/v1/api_pb";
import type { Container, MeshStatus, Project } from "@/gen/nagi/v1/common_pb";

// 节点页的拓扑视图：面板在中间，节点围成一圈，两个图层的位置相同。
// - 网络：在线节点之间的 mesh 连接（见 docs/mesh.md）。
// - 高可用：容器所在节点指向它的热备或备份节点；点选一个节点，模拟它故障时各容器的结果（见 docs/failover.md）。

export type MapLayer = "network" | "ha";

export function NodeMap({ nodes, layer }: { nodes: Node[]; layer: MapLayer }) {
  return layer === "ha" ? <HaMap nodes={nodes} /> : <NetworkMap nodes={nodes} />;
}

// ---- 布局与几何 ----

const PANEL = "panel";

type Pt = { x: number; y: number };

// CLEAR：线离面板中心至少这么远，不压住面板和它下面的名字。
const CLEAR = 64;

// layout：节点在圆上的位置。偶数个时转半格；两个节点放在左下、右下，和面板成三角形
// （左右一字排开时，它们之间的线只能绕过面板）。
function layout(nodes: Node[]) {
  const count = nodes.length;
  const r = count <= 3 ? 180 : Math.min(260, 130 + count * 14);
  const start = -Math.PI / 2 + (count % 2 === 0 ? Math.PI / count : 0);
  const angle = (i: number) => (count === 2 ? [(5 * Math.PI) / 6, Math.PI / 6][i] : start + (2 * Math.PI * i) / count);
  const pos = new Map<string, Pt>([[PANEL, { x: 0, y: 0 }]]);
  nodes.forEach((n, i) => {
    pos.set(n.id, { x: r * Math.cos(angle(i)), y: r * Math.sin(angle(i)) });
  });
  const ys = [...pos.values()].map((p) => p.y);
  const padX = 100; // 名字和说明写在节点下面，左右留出文字的宽度
  // 上方还要容得下从面板上方绕过的线和它的标签（见 bend）：按每对节点之间可能的线算。
  let top = Math.min(...ys) - 44;
  const pts = nodes.map((n) => pos.get(n.id)!);
  for (let i = 0; i < pts.length; i++) {
    for (let j = i + 1; j < pts.length; j++) {
      const P = bend(pts[i], pts[j], 0.16);
      top = Math.min(top, (pts[i].y + 2 * P.y + pts[j].y) / 4 - 14);
    }
  }
  const bottom = Math.max(...ys) + 64;
  return { pos, box: { x: -r - padX, y: top, w: 2 * (r + padX), h: bottom - top } };
}

// curve：从 A 经控制点 P 到 B 的二次曲线；at(s) 是曲线上的点（s 从 0 到 1）。
function curve(A: Pt, B: Pt, P: Pt) {
  const at = (s: number) => ({
    x: (1 - s) ** 2 * A.x + 2 * (1 - s) * s * P.x + s ** 2 * B.x,
    y: (1 - s) ** 2 * A.y + 2 * (1 - s) * s * P.y + s ** 2 * B.y,
  });
  return { d: `M${A.x},${A.y}Q${P.x},${P.y} ${B.x},${B.y}`, at };
}

// bend：节点之间的线向外（远离面板的一侧）弯的控制点，弯曲程度至少是长度的 k 倍。
// 节点都在同一个圆上，弦离圆心（面板）最近的点就是它的中点；二次曲线的中点在弦中点和控制点正中间，
// 所以把控制点推到曲线中点离面板至少 CLEAR。弦正好穿过面板时从上面绕，面板的名字在下面。
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
  const offset = 2 * Math.max((k * len) / 2, CLEAR - side);
  return { x: M.x + nx * offset, y: M.y + ny * offset };
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
// 宽度按字符估算：11px 等宽字体每个半角字符约 6.6px，全角字符 11px。
function EdgeLabel({ at, text, className }: { at: Pt; text: string; className?: string }) {
  const w = [...text].reduce((s, ch) => s + (/[\u2e80-\uffff]/.test(ch) ? 11 : 6.6), 0) + 12;
  return (
    <g transform={`translate(${at.x},${at.y})`} className={className}>
      <rect x={-w / 2} y={-9} width={w} height={18} rx={9} strokeWidth={1} className="fill-background stroke-border" />
      <text dy="0.35em" textAnchor="middle" fontSize={11} className="fill-current font-mono tabular-nums">
        {text}
      </text>
    </g>
  );
}

// 文字下面垫一圈背景色，压在线上也看得清。
const halo = { stroke: "var(--background)", strokeWidth: 4, strokeLinejoin: "round" as const, paintOrder: "stroke" as const };

// ---- 共用的图形 ----

function MapSvg({ box, label, onBackground, children }: { box: { x: number; y: number; w: number; h: number }; label: string; onBackground?: () => void; children: React.ReactNode }) {
  return (
    <svg
      viewBox={`${box.x} ${box.y} ${box.w} ${box.h}`}
      width={box.w}
      className="mx-auto h-auto max-w-full overflow-visible select-none"
      role="group"
      aria-label={label}
      onClick={(e) => e.target === e.currentTarget && onBackground?.()}
    >
      {children}
    </svg>
  );
}

function PanelGlyph({ dim, onActive }: { dim: boolean; onActive?: (on: boolean) => void }) {
  return (
    <g
      className={cn("transition-opacity duration-150 ease-out", dim && "opacity-40")}
      onMouseEnter={() => onActive?.(true)}
      onMouseLeave={() => onActive?.(false)}
    >
      <rect x={-24} y={-24} width={48} height={48} rx={12} className="fill-card stroke-border" strokeWidth={1} />
      <Waves x={-13} y={-13} width={26} height={26} className="text-wave" />
      <text y={42} textAnchor="middle" fontSize={13} {...halo} className="fill-foreground font-medium">
        {t.nodes.map.panel}
      </text>
    </g>
  );
}

const nodeStatus = (n: Node) => (n.pending ? t.common.status.pendingApproval : n.online ? t.common.status.online : t.common.status.offline);

// statusLine：离线、待批准的节点在名字下面写状态，在线时返回空，由图层自己写。
const statusLine = (n: Node) =>
  n.pending ? t.common.status.pendingApproval : !n.online ? t.nodes.lastSeen(timeAgo(toDate(n.lastSeen))) : "";

const pct = (v: number) => `${Math.round(v)}%`;

// NodeGlyph：一个节点。外圈是 CPU、内存、磁盘里占用最高的一项（超过 75% 变黄、90% 变红，同占用条），右下角是状态点。
// 有 onSelect 时是按钮（高可用图层点选节点），否则是到节点详情页的链接。failed：高可用图层里模拟故障的节点。
function NodeGlyph({
  n,
  at,
  dim,
  sub,
  failed,
  onActive,
  onSelect,
}: {
  n: Node;
  at: Pt;
  dim: boolean;
  sub: React.ReactNode;
  failed?: boolean;
  onActive: (on: boolean) => void;
  onSelect?: () => void;
}) {
  const live = n.online && !n.pending;
  const u = live ? n.usage : undefined;
  const ratio = u ? Math.min(Math.max(u.cpu, u.memory, u.disk) / 100, 1) : 0;
  const ring = ratio >= 0.9 ? "stroke-destructive" : ratio >= 0.75 ? "stroke-warning" : "stroke-info";
  const status = nodeStatus(n);
  const detail = [n.name, status, u && `CPU ${pct(u.cpu)} · ${t.nodes.memory} ${pct(u.memory)} · ${t.nodes.disk} ${pct(u.disk)}`, t.nodes.containers(n.containers)]
    .filter(Boolean)
    .join("\n");

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
        <text y={46} textAnchor="middle" fontSize={13} {...halo} className="fill-foreground font-medium group-hover:underline">
          {n.name}
        </text>
        {sub && (
          <text y={62} textAnchor="middle" fontSize={11} {...halo} className="fill-muted-foreground font-mono tabular-nums">
            {sub}
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
    <span className="inline-flex items-center gap-1.5">
      <svg width={24} height={8} aria-hidden>
        <line x1={1} y1={4} x2={headClass ? 17 : 23} y2={4} strokeWidth={1.5} strokeLinecap="round" strokeDasharray={dash} className={className} />
        {headClass && <path d="M23,4L15,0L15,8Z" className={headClass} />}
      </svg>
      {children}
    </span>
  );
}

function Legend({ children }: { children: React.ReactNode }) {
  return <div className="flex flex-wrap items-center justify-center gap-x-5 gap-y-2 text-xs text-muted-foreground">{children}</div>;
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

// meshGeometry：连到面板的是直线；节点之间中转的线经过面板（控制点取中点关于面板的对称点，曲线中点正好在面板上），
// 直连的线向外弯一点，免得两个对面的节点之间的直线穿过面板、看起来像中转。
function meshGeometry(A: Pt, B: Pt, toPanel: boolean, kind: Kind) {
  if (toPanel) return { d: `M${A.x},${A.y}L${B.x},${B.y}`, at: (s: number) => ({ x: A.x + (B.x - A.x) * s, y: A.y + (B.y - A.y) * s }) };
  const P = kind === "relay" ? { x: -(A.x + B.x) / 2, y: -(A.y + B.y) / 2 } : bend(A, B, 0.16);
  return curve(A, B, P);
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

  const statuses = new Map<string, MeshStatus>();
  live.forEach((n, i) => meshes[i]?.data && statuses.set(n.id, meshes[i].data));
  const edges = buildMeshEdges(nodes, statuses, panelMesh.data, new Set([PANEL, ...live.map((n) => n.id)]));
  const { pos, box } = layout(nodes);
  const nameOf = (id: string) => (id === PANEL ? t.nodes.map.panel : (nodes.find((n) => n.id === id)?.name ?? id));

  // 悬停或聚焦某个节点时，突出它的连接，其他的淡下去。
  const touches = (e: MeshEdge) => active !== null && (e.a === active || e.b === active);
  const neighbors = new Set(edges.filter(touches).flatMap((e) => [e.a, e.b]));
  const dim = (id: string) => active !== null && !neighbors.has(id) && id !== active;
  const issues = edges.filter((e) => e.kind !== "direct");
  const geometry = (e: MeshEdge) => meshGeometry(pos.get(e.a)!, pos.get(e.b)!, e.a === PANEL || e.b === PANEL, e.kind);

  return (
    <div className="grid gap-4">
      <MapSvg box={box} label={t.nodes.views.network}>
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
                  e.kind === "relay" ? "stroke-warning" : e.kind === "none" ? "stroke-destructive" : on ? "stroke-foreground/60" : "stroke-muted-foreground/35",
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
            label && <EdgeLabel key={e.a + "|" + e.b} at={mid} text={label} className={e.kind === "none" ? "text-destructive" : "text-foreground"} />
          );
        })}

        <PanelGlyph dim={dim(PANEL)} onActive={(on) => setActive(on ? PANEL : null)} />
        {nodes.map((n) => (
          <NodeGlyph
            key={n.id}
            n={n}
            at={pos.get(n.id)!}
            dim={dim(n.id)}
            sub={statusLine(n) || (n.usage ? `CPU ${pct(n.usage.cpu)} · ${t.nodes.memory} ${pct(n.usage.memory)}` : "")}
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
          {t.nodes.map.none}
        </LegendLine>
        <HelpTip>{t.nodes.map.help}</HelpTip>
      </Legend>

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
  const { pos: layoutPos, box } = layout(nodes);
  // 箭头都朝外弯，不压在面板上。两个节点互相指向时，ID 大的一方发出的弯得更多，两条一里一外；
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
    <div className="grid gap-4" onKeyDown={(e) => e.key === "Escape" && setSelected(null)}>
      <MapSvg box={box} label={t.nodes.views.ha} onBackground={() => setSelected(null)}>
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
                className={out ? "stroke-foreground/80" : "stroke-muted-foreground/50"}
              />
              <path d={g.head} className={out ? "fill-foreground/80" : "fill-muted-foreground/50"} />
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

        <PanelGlyph dim={focus !== null} />
        {nodes.map((n) => {
          const k = risky.get(n.id) ?? 0;
          return (
            <NodeGlyph
              key={n.id}
              n={n}
              at={layoutPos.get(n.id)!}
              dim={dim(n.id)}
              failed={focus === n.id}
              sub={
                statusLine(n) || (
                  <>
                    {t.nodes.containers(n.containers)}
                    {k > 0 && <tspan className="fill-warning"> · {t.nodes.ha.unprotected(k)}</tspan>}
                  </>
                )
              }
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
        {failedNode ? (
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
        <h3 className="text-sm font-semibold">{t.nodes.ha.title(node.name)}</h3>
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
              <Status tone={g.tone}>{g.label}</Status>
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
