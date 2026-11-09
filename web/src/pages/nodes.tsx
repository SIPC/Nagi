import { useContext, useEffect, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Code, ConnectError } from "@connectrpc/connect";
import { Link, useNavigate } from "@tanstack/react-router";
import { Plus } from "lucide-react";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { errorMessage, nodeClient, projectClient, settingsClient, toDate } from "@/lib/api";
import { cn, timeAgo } from "@/lib/utils";
import { t } from "@/i18n";
import { Breadcrumb, PageHeader } from "@/pages/layout";
import { RangePicker, UsageCell, fmtBytes, fmtCpuShort, fmtRate, metricRanges, nodeUsage, type MetricRange } from "@/components/metrics";
import { TimeChart } from "@/components/chart";
import { HandOver, NodePanels } from "@/pages/panels";
import { NodeMesh, NodeQUIC, ratePair } from "@/pages/mesh";
import { NodeMap, type MapLayer } from "@/pages/node-map";
import {
  Alert,
  Badge,
  Button,
  ButtonLink,
  dotColor,
  Dialog,
  DialogActions,
  Empty,
  EmptyRow,
  Field,
  HelpTip,
  Input,
  List,
  Menu,
  Notice,
  QueryError,
  Row,
  RowsSkeleton,
  AutoHeight,
  Roll,
  Section,
  Segmented,
  SettingRow,
  SettingRows,
  Skeleton,
  Slide,
  SlideKeep,
  Spinner,
  Status,
  Tabs,
  Textarea,
  ReadOnlyContext,
  CodeBlock,
} from "@/components/ui";
import type { Mirrors } from "@/gen/nagi/v1/agent_pb";
import type { Node, StaleProject, UnknownContainer, UpdateState } from "@/gen/nagi/v1/api_pb";
import type { NodeInfo, NodeUsage } from "@/gen/nagi/v1/common_pb";
import { ask, confirmAction, notify } from "@/components/confirm";
import { cancelable, updateActive, upgradeTarget, useUpdateStatus } from "@/pages/update";
import { useRole } from "@/components/role";
import { useRun } from "@/components/run";
import { stateOf } from "@/components/status";
import { NodeFiles, NodeTerminal, nodeAccessBlock } from "@/pages/node-access";

export type NodeTab = "metrics" | "containers" | "terminal" | "files" | "panels" | "mesh" | "settings" | "replicas";

export const nodeTabs: { value: NodeTab; label: string }[] = [
  { value: "metrics", label: t.nodes.tabs.metrics },
  { value: "containers", label: t.nodes.tabs.containers },
  { value: "terminal", label: t.nodes.tabs.terminal },
  { value: "files", label: t.nodes.tabs.files },
  { value: "replicas", label: t.nodes.tabs.replicas },
  { value: "panels", label: t.nodes.tabs.panels },
  { value: "mesh", label: t.nodes.tabs.mesh },
  { value: "settings", label: t.nodes.tabs.settings },
];

function useNodeList() {
  return useQuery({ queryKey: ["nodes"], queryFn: () => nodeClient.listNodes({}), refetchInterval: 5000 });
}

function usePanelVersion() {
  const settings = useQuery({ queryKey: ["settings"], queryFn: () => settingsClient.getSettings({}) });
  return settings.data?.settings?.version ?? "";
}

// 例如 8 GB、1.0 TB：数字和单位之间留空格。
const sizeLabel = fmtBytes;

// 磁盘是所有盘加起来；旧版本的节点不报各盘，用数据目录所在的盘。
const diskTotal = (info?: NodeInfo, usage?: NodeUsage) => usage?.disks.reduce((sum, d) => sum + Number(d.totalBytes), 0) || Number(info?.diskBytes ?? 0);

// nodeSpec：节点的配置，例如「2 核 · 3.8 GB」，磁盘在悬停提示里。旧版本的节点不报配置，返回空串。
function nodeSpec(info?: NodeInfo) {
  if (!info) return "";
  const parts: string[] = [];
  if (info.cpus) parts.push(t.nodes.cores(info.cpus));
  if (info.memoryBytes) parts.push(sizeLabel(Number(info.memoryBytes)));
  return parts.join(" · ");
}

function nodeSpecTitle(info?: NodeInfo, usage?: NodeUsage) {
  const disk = diskTotal(info, usage);
  return disk ? `${t.nodes.disk} ${sizeLabel(disk)}` : undefined;
}

// lastSeenText：离线节点最后在线的时间，从未连上过时写「从未在线」。
const lastSeenText = (n: Node) => (n.lastSeen ? t.nodes.lastSeen(timeAgo(toDate(n.lastSeen))) : t.nodes.neverSeen);

const ago = (seconds: number) => timestampFromDate(new Date(Date.now() - seconds * 1000));
const fmtPercent = fmtCpuShort;

// NodeResources：节点页的监控，4 个数值块：CPU、内存、磁盘、流量，每块一个数字加一条小折线；时间范围在区块标题右侧。
// 内存和磁盘写成「已用 / 总量」；流量是入站、出站两条线，数字写成「↑ 出站 ↓ 入站」，和节点网络的测速一致。
function NodeResources({ n }: { n: Node }) {
  const [range, setRange] = useState<MetricRange>("1h");
  const r = metricRanges.find((x) => x.value === range)!;
  const q = useQuery({
    queryKey: ["node-metrics", n.id, range],
    queryFn: () => nodeClient.queryNodeMetrics({ nodeId: n.id, prefix: "node:", from: ago(r.seconds), step: BigInt(r.step) }),
    refetchInterval: 10_000,
    placeholderData: (prev) => prev,
  });
  const ofTotal = (total: number) => (total ? (v: number) => `${sizeLabel((v / 100) * total)} / ${sizeLabel(total)}` : fmtPercent);
  const pointsOf = (name: string) => {
    const s = q.data?.series.find((x) => x.name === name);
    return (s?.values ?? []).map((v, i): [number, number] => [Number(s!.times[i]), v]);
  };
  const last = (points: [number, number][]) => (points.length ? points[points.length - 1][1] : undefined);
  const one = (label: string, name: string, format: (v: number) => string, minMax?: number) => {
    const points = pointsOf(name);
    const v = last(points);
    return { label, series: [{ label, points, className: "text-muted-foreground" }], format, minMax, value: v === undefined ? "—" : format(v), n: v, title: undefined as string | undefined };
  };
  const rx = pointsOf("node:net_rx");
  const tx = pointsOf("node:net_tx");
  const lastRx = last(rx);
  const lastTx = last(tx);
  const net = lastRx !== undefined && lastTx !== undefined ? { rx: lastRx, tx: lastTx } : undefined;
  const tiles = [
    one("CPU", "node:cpu", fmtPercent, 1),
    one(t.metrics.memory, "node:mem", ofTotal(Number(n.info?.memoryBytes ?? 0))),
    one(t.metrics.disk, "node:disk", ofTotal(diskTotal(n.info, n.usage))),
    {
      label: t.metrics.traffic,
      // 两条线用深浅两档灰区分，都是正常数据。
      series: [
        { label: t.metrics.netRx, points: rx, className: "text-muted-foreground" },
        { label: t.metrics.netTx, points: tx, className: "text-foreground/60" },
      ],
      format: fmtRate,
      minMax: undefined,
      value: net ? ratePair(net.tx, net.rx) : "—",
      n: net ? net.rx + net.tx : undefined,
      title: net ? `${t.metrics.netTx} ${fmtRate(net.tx)} · ${t.metrics.netRx} ${fmtRate(net.rx)}` : undefined,
    },
  ];
  const now = Date.now() / 1000;
  return (
    <Section title={t.metrics.resourceUsage} actions={<RangePicker value={range} onChange={setRange} />}>
      {q.isPending ? (
        <TilesSkeleton />
      ) : q.error ? (
        <QueryError error={errorMessage(q.error)} onRetry={() => q.refetch()} />
      ) : (
        <div className={tileGrid}>
          {tiles.map((it) => (
            <div key={it.label} className="grid min-w-0 content-start gap-1">
              <div className="text-muted-foreground">{it.label}</div>
              <div className="truncate text-xl leading-7 font-medium tracking-tight" title={it.title}>
                <Roll value={it.value} n={it.n} />
              </div>
              <TimeChart className="mt-2" series={it.series} from={now - r.seconds} to={now} step={r.step} format={it.format} height={40} axis={false} minMax={it.minMax} />
            </div>
          ))}
        </div>
      )}
    </Section>
  );
}

// 数值块 4 列，手机 2 列，和容器的监控同一种排法（docs/ui.md「对齐」）。
const tileGrid = "grid grid-cols-2 gap-8 max-md:gap-4 md:grid-cols-4";

// TilesSkeleton：监控加载中，和数值块一样的版面，内容出来时不跳。
function TilesSkeleton() {
  return (
    <div role="status" aria-label={t.common.feedback.loading} className={tileGrid}>
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

function NodeStatus({ n }: { n: Node }) {
  if (n.pending)
    return (
      <span className="text-warning-text">
        <Status tone="warning">{t.common.status.pendingApproval}</Status>
      </span>
    );
  if (n.online) return <Status tone="success">{t.common.status.online}</Status>;
  return (
    <span className="text-destructive">
      <Status tone="danger">{t.common.status.offline}</Status>
    </span>
  );
}

// NodeContainers：运行在这个节点上的容器。节点离线时状态未知，但列表来自面板，仍然可以看。
function NodeContainers({ nodeId, online }: { nodeId: string; online: boolean }) {
  const projects = useQuery({ queryKey: ["projects"], queryFn: () => projectClient.listProjects({}), refetchInterval: 5000 });
  if (projects.isPending) return <RowsSkeleton />;
  if (projects.error) return <QueryError error={errorMessage(projects.error)} onRetry={() => projects.refetch()} />;
  const rows = projects.data.projects.flatMap((s) =>
    (s.project?.containers ?? [])
      .filter((c) => c.nodeId === nodeId)
      .map((c) => ({ project: s.project!, container: c, services: (s.status?.services ?? []).filter((v) => v.container === c.name) })),
  );
  if (rows.length === 0) return <EmptyRow>{t.nodes.noContainers}</EmptyRow>;
  return (
    <List>
      {rows.map(({ project, container, services }) => {
        const st = online ? stateOf(services, container.deployedVersion > 0n) : { tone: "warning" as const, label: t.common.status.unknown };
        return (
          <Row
            key={project.id + container.name}
            title={
              <Link to="/projects/$projectId/c/$container" params={{ projectId: project.id, container: container.name }} className="truncate underline-offset-4 hover:underline">
                {container.name}
              </Link>
            }
            sub={project.name}
            cols={[
              { content: container.deployedVersion > 0n ? `v${container.deployedVersion}` : "—", num: true },
              {
                content: <Status tone={st.tone}>{st.label}</Status>,
                phone: true,
              },
            ]}
          />
        );
      })}
    </List>
  );
}

// approve 批准用接入码加入的节点。
async function approve(n: Node) {
  if (!(await confirmAction(t.nodes.approveConfirm(n.name, n.address || n.id), { action: t.nodes.approve }))) return false;
  try {
    await nodeClient.approveNode({ nodeId: n.id });
    return true;
  } catch (err) {
    await notify(errorMessage(err));
    return false;
  }
}

export type NodesView = "cards" | MapLayer;

export function NodesPage({ view, onView, openAdd, onAddOpened }: { view: NodesView; onView: (v: NodesView) => void; openAdd?: boolean; onAddOpened?: () => void }) {
  const nodes = useNodeList();
  const panelVersion = usePanelVersion();
  const { isAdmin } = useRole();
  const status = useUpdateStatus(isAdmin);
  const [dialog, setDialog] = useState<"add" | "handover" | null>(openAdd && isAdmin ? "add" : null);
  useEffect(() => {
    if (openAdd) onAddOpened?.();
  }, [openAdd, onAddOpened]);
  const [starting, setStarting] = useState(false);
  const list = nodes.data?.nodes ?? [];
  const states = status.data?.nodes ?? {};
  const running = Object.values(states).filter(updateActive).length;
  const canCancel = Object.entries(states).some(([id, s]) => cancelable(s, list.find((n) => n.id === id)));
  const [canceling, setCanceling] = useState(false);
  const behind = list.filter((n) => upgradeTarget(n, panelVersion) && !updateActive(states[n.id])).length;
  const cancelAll = async () => {
    setCanceling(true);
    try {
      await nodeClient.cancelNodeUpgrade({});
      await status.refetch();
    } catch (err) {
      await notify(errorMessage(err));
    } finally {
      setCanceling(false);
    }
  };
  const upgradeAll = async () => {
    if (!(await confirmAction(t.nodes.upgrade.allConfirm(behind, panelVersion), { action: t.nodes.upgrade.all }))) return;
    setStarting(true);
    try {
      await nodeClient.upgradeAllNodes({});
      await status.refetch();
    } catch (err) {
      await notify(errorMessage(err));
    } finally {
      setStarting(false);
    }
  };
  const cards = (
    <NodeList>
      {list.map((n) => (
        <NodeRow key={n.id} n={n} canUpgrade={upgradeTarget(n, panelVersion) !== ""} upgrade={states[n.id]} onApproved={() => nodes.refetch()} />
      ))}
    </NodeList>
  );

  return (
    <>
      <PageHeader
        title={t.nodes.title}
        description={t.nodes.description}
        actions={
          <>
            {list.length > 0 && (
              // 窄屏上拓扑图太小，只用卡片。
              <div className="hidden sm:block">
                <Segmented
                  value={view}
                  onChange={onView}
                  items={[
                    { value: "cards", label: t.nodes.views.cards },
                    { value: "network", label: t.nodes.views.network },
                    { value: "ha", label: t.nodes.views.ha },
                  ]}
                />
              </div>
            )}
            {isAdmin && (running > 0 || behind > 0) && (
              <Button variant="outline" onClick={upgradeAll} disabled={starting || running > 0}>
                {(starting || running > 0) && <Spinner />}
                {running > 0 ? t.nodes.upgrade.allRunning(running) : t.nodes.upgrade.allCount(behind)}
              </Button>
            )}
            {isAdmin && canCancel && (
              <Button variant="outline" onClick={cancelAll} disabled={canceling}>
                {canceling ? t.nodes.upgrade.canceling : t.nodes.upgrade.cancel}
              </Button>
            )}
            <Button onClick={() => setDialog("add")}>
              <Plus />
              {t.nodes.addNode}
            </Button>
            {list.length > 0 && <Menu items={[{ label: t.nodes.handover, onSelect: () => setDialog("handover") }]} />}
          </>
        }
      />
      <AutoHeight>
      {nodes.isPending ? (
        <RowsSkeleton count={3} />
      ) : nodes.error ? (
        <QueryError error={errorMessage(nodes.error)} onRetry={() => nodes.refetch()} />
      ) : list.length === 0 ? (
        <Empty
          title={t.nodes.empty}
          action={
            <Button onClick={() => setDialog("add")}>
              <Plus />
              {t.nodes.addNode}
            </Button>
          }
        >
          {t.nodes.addNodeDescription}
        </Empty>
      ) : (
        <Slide id={view} index={["cards", "network", "ha"].indexOf(view)}>
          {view !== "cards" ? (
            <>
              <div className="hidden sm:block">
                <NodeMap nodes={list} layer={view} />
              </div>
              <div className="sm:hidden">{cards}</div>
            </>
          ) : (
            cards
          )}
        </Slide>
      )}
      </AutoHeight>
      <Dialog open={dialog === "add"} onClose={() => setDialog(null)} title={t.nodes.addNode} className="max-w-[520px]">
        <AddNode onClose={() => setDialog(null)} />
      </Dialog>
      <Dialog open={dialog === "handover"} onClose={() => setDialog(null)} title={t.nodes.handover} description={t.nodes.handoverDescription} className="max-w-[520px]">
        <HandOver nodes={list} onClose={() => setDialog(null)} />
      </Dialog>
    </>
  );
}

// 节点列表的列：名称和地址，CPU、内存、磁盘各一列（已用 / 上限加占用条），容器数靠右，各列上下居中。窄于 lg 时（平板上五列放不下）只留名称这一列，用量并进第三行。
const nodeCols = "grid grid-cols-[minmax(0,1fr)] items-center gap-x-6 lg:grid-cols-[minmax(0,1fr)_8rem_8rem_8rem_3rem]";

// NodeList：节点页的列表。字段多、要横向比较，所以有表头；表头线不超出内容区，行左右外扩 8 只为悬停底色。
function NodeList({ children }: { children: React.ReactNode }) {
  return (
    <div className="-mx-2 grid grid-cols-[minmax(0,1fr)] animate-fade-in">
      <div className={cn(nodeCols, "mx-2 h-9 content-center border-b text-[13px] text-muted-foreground max-lg:hidden")}>
        <span>{t.nodes.columns.node}</span>
        <span>CPU</span>
        <span>{t.metrics.memory}</span>
        <span>{t.metrics.disk}</span>
        <span className="text-right">{t.nodes.columns.containers}</span>
      </div>
      <div className="grid grid-cols-[minmax(0,1fr)] pt-1">{children}</div>
    </div>
  );
}

// NodeRow：一个节点一行，整行可点。链接铺满整行，「批准」按钮叠在链接上面，不放进链接里（链接里不能嵌按钮）。
// 状态只用名称前的圆点，在线时不写字；离线和待批准时在名称后面写灰字。
function NodeRow({ n, canUpgrade, upgrade, onApproved }: { n: Node; canUpgrade: boolean; upgrade?: UpdateState; onApproved: () => void }) {
  const usage = n.online && !n.pending ? nodeUsage(n.usage, n.info) : undefined;
  const tone = n.pending ? "warning" : n.online ? "success" : "neutral";
  return (
    <div className={cn(nodeCols, "relative min-h-13 rounded-md px-2 py-1.5 transition-colors hover:bg-accent has-[>a:focus-visible]:ring-2 has-[>a:focus-visible]:ring-ring/40")}>
      <Link to="/nodes/$nodeId" params={{ nodeId: n.id }} aria-label={n.name} className="absolute inset-0 rounded-md outline-none" />
      <div className="min-w-0">
        <div className="flex min-w-0 items-center gap-2 leading-5">
          <span aria-hidden className={cn("size-1.5 shrink-0 rounded-full", dotColor[tone])} />
          <span className="truncate">{n.name}</span>
          {n.info?.embedded && <Badge>{t.nodes.embedded}</Badge>}
          {upgrade ? (
            <Badge tone={upgrade.phase === "failed" ? "warning" : "neutral"} title={upgrade.error || undefined}>
              {t.nodes.upgrade.phases[upgrade.phase as keyof typeof t.nodes.upgrade.phases]}
            </Badge>
          ) : (
            canUpgrade && <Badge tone="info">{t.nodes.upgrade.badge}</Badge>
          )}
          {(n.pending || !n.online) && (
            <span className="shrink-0 text-[13px] text-muted-foreground">
              {n.pending ? t.common.status.pendingApproval : <Roll value={`${t.common.status.offline} · ${lastSeenText(n)}`} />}
            </span>
          )}
        </div>
        <div className="truncate font-mono text-[13px] leading-5 text-muted-foreground">{n.address || n.id}</div>
        <div className="text-[13px] leading-5 text-muted-foreground tabular-nums lg:hidden">
          {/* 和 Row 在手机上合并灰色信息的写法相同，用 · 隔开。 */}
          {[...(usage ?? []).map((u) => `${u.label} ${u.label === "CPU" ? u.value : u.percent === undefined ? "—" : fmtCpuShort(u.percent)}`), t.nodes.containers(n.containers)].join(" · ")}
        </div>
      </div>
      {n.pending ? (
        <div className="lg:col-span-3 max-lg:mt-2">
          <Button
            variant="outline"
            className="relative"
            onClick={async () => {
              if (await approve(n)) onApproved();
            }}
          >
            {t.nodes.approve}
          </Button>
        </div>
      ) : usage ? (
        usage.map((u) => (
          <span key={u.label} className="max-lg:hidden">
            <UsageCell {...u} />
          </span>
        ))
      ) : (
        [0, 1, 2].map((i) => (
          <span key={i} className="text-muted-foreground max-lg:hidden">
            —
          </span>
        ))
      )}
      <span className="text-right text-muted-foreground tabular-nums max-lg:hidden">
        <Roll value={String(n.containers)} n={n.containers} />
      </span>
    </div>
  );
}

// NodePage：节点详情页。
export function NodePage({ nodeId, tab, path, onTab, onFilePath, onOpenTerminal }: { nodeId: string; tab: NodeTab; path?: string; onTab: (t: NodeTab) => void; onFilePath: (p: string) => void; onOpenTerminal: (p: string) => void }) {
  const nodes = useNodeList();
  const readOnlyPanel = useContext(ReadOnlyContext);
  // 终端打开过以后留在页面上，切到别的标签再回来，shell 还在。
  const [shellOpened, setShellOpened] = useState(false);
  useEffect(() => {
    if (tab === "terminal") setShellOpened(true);
  }, [tab]);
  const { isAdmin: canSeeUpgrades } = useRole();
  const status = useUpdateStatus(canSeeUpgrades);
  const panelVersion = usePanelVersion();
  const navigate = useNavigate();
  const [renaming, setRenaming] = useState(false);
  if (nodes.isPending) return <RowsSkeleton />;
  if (nodes.error) return <QueryError error={errorMessage(nodes.error)} onRetry={() => nodes.refetch()} />;
  const n = nodes.data.nodes.find((x) => x.id === nodeId);
  if (!n) {
    return (
      <>
        <Breadcrumb className="md:hidden" items={[{ label: t.nodes.title, to: "/nodes" }]} />
        <Notice
          tone="danger"
          title={t.nodes.notFound}
          actions={
            <ButtonLink to="/nodes" variant="outline" readOnlyOk>
              {t.nodes.viewAll}
            </ButtonLink>
          }
        />
      </>
    );
  }
  // 节点访问只给管理员，备用面板上也不能用（见 docs/node-access.md）。
  const showAccess = canSeeUpgrades && !readOnlyPanel;
  const tabs = nodeTabs.filter((x) => showAccess || (x.value !== "terminal" && x.value !== "files"));
  const accessBlock = nodeAccessBlock(n);
  // 手机上进入文件页后，标题栏换成返回、目录名、更多（docs/ui.md）。
  const browsing = tab === "files" && showAccess && n.online && !accessBlock;
  const target = upgradeTarget(n, panelVersion);
  const upgradeState = status.data?.nodes[n.id];
  const remove = async () => {
    if (!(await confirmAction(t.nodes.removeConfirm(n.name), { action: t.nodes.removeNode, danger: true }))) return;
    try {
      await nodeClient.removeNode({ nodeId: n.id });
      navigate({ to: "/nodes" });
    } catch (err) {
      await notify(errorMessage(err));
    }
  };
  return (
    <>
      <Breadcrumb className={cn("md:hidden", browsing && "max-md:hidden")} items={[{ label: t.nodes.title, to: "/nodes" }, { label: n.name }]} />
      <PageHeader
        className={cn(browsing && "max-md:hidden")}
        title={
          <>
            {n.name}
            <NodeStatus n={n} />
            {n.info?.embedded && <Badge>{t.nodes.embedded}</Badge>}
          </>
        }
        meta={
          <>
            <span>{n.address || n.id}</span>
            {n.info && (
              <>
                <span>{`${n.info.os}/${n.info.arch}`}</span>
                {nodeSpec(n.info) && <span title={nodeSpecTitle(n.info, n.usage)}>{nodeSpec(n.info)}</span>}
                {n.info.dockerVersion && <span title={n.info.composeVersion ? `Compose ${n.info.composeVersion}` : undefined}>{`Docker ${n.info.dockerVersion}`}</span>}
                {n.info.version && <span>{`Nagi ${n.info.version}`}</span>}
              </>
            )}
            <button type="button" className="cursor-pointer rounded-sm underline-offset-4 outline-none hover:text-foreground hover:underline focus-ring" onClick={() => onTab("containers")}>
              <Roll value={t.nodes.containers(n.containers)} n={n.containers} />
            </button>
            {!n.online && !n.pending && <Roll value={lastSeenText(n)} />}
          </>
        }
        actions={
          <Menu
            items={[
              { label: t.nodes.rename.action, onSelect: () => setRenaming(true) },
              { label: t.nodes.removeNode, onSelect: remove, danger: true },
            ]}
          />
        }
      />
      <Dialog open={renaming} onClose={() => setRenaming(false)} title={t.nodes.rename.title} description={t.nodes.rename.description}>
        <RenameNode n={n} onDone={() => setRenaming(false)} />
      </Dialog>
      {(target || upgradeState || n.unknownContainers.length > 0 || n.staleProjects.length > 0 || n.pending) && (
        <div className="mb-6 grid gap-2">
          {(target || upgradeState) && <NodeUpgrade key={target} n={n} target={target} state={upgradeState} onDone={() => status.refetch()} />}
          {n.staleProjects.length > 0 && <StaleProjects nodeId={n.id} list={n.staleProjects} onDone={() => nodes.refetch()} />}
          {n.unknownContainers.length > 0 && <UnknownContainers nodeId={n.id} list={n.unknownContainers} onDone={() => nodes.refetch()} />}
          {n.pending && (
            <Notice
              tone="warning"
              title={t.common.status.pendingApproval}
              actions={
                <Button
                  onClick={async () => {
                    if (await approve(n)) nodes.refetch();
                  }}
                >
                  {t.nodes.approve}
                </Button>
              }
            >
              {t.nodes.pendingNotice}
            </Notice>
          )}
        </div>
      )}
      {!n.pending && (
        <>
          <Tabs value={tab} onChange={onTab} items={tabs} className={cn("mb-6", browsing && "max-md:hidden")} />
          {/* 终端和标签内容叠在同一格：切到终端时标签内容的高度从旧值过渡到 0，终端放在下面会被它从下往上推。 */}
          <div className="grid grid-cols-[minmax(0,1fr)] *:col-start-1 *:row-start-1">
            {/* 这一层不能省：区块靠 first: 去掉第一个的分隔线。 */}
            <Slide id={tab} index={tabs.findIndex((x) => x.value === tab)}>
              {tab === "containers" ? (
                <NodeContainers nodeId={n.id} online={n.online} />
              ) : (tab === "terminal" || tab === "files") && !showAccess ? (
                <Notice title={t.nodeAccess.gate.adminOnly} />
              ) : !n.online ? (
                <Notice title={t.nodes.offlineTitle}>{t.nodes.offlineNotice}</Notice>
              ) : tab === "terminal" || tab === "files" ? (
                accessBlock ?? (tab === "files" ? <NodeFiles node={n} path={path || "/"} onPath={onFilePath} onTerminal={onOpenTerminal} onExit={() => onTab("metrics")} /> : null)
              ) : tab === "metrics" ? (
                <NodeResources n={n} />
              ) : tab === "mesh" ? (
                <NodeMesh nodeId={n.id} />
              ) : tab === "replicas" ? (
                <NodeReplicas nodeId={n.id} nodes={nodes.data.nodes} />
              ) : tab === "settings" ? (
                <NodeSettings n={n} />
              ) : (
                <NodePanels nodeId={n.id} />
              )}
            </Slide>
            {/* 终端不卸载：卸载就断开了。所以放在带 key 的那层外面，进出时和其他标签一样横移。 */}
            {showAccess && n.online && !accessBlock && shellOpened && (
              <SlideKeep active={tab === "terminal"} index={tabs.findIndex((x) => x.value === "terminal")} current={tabs.findIndex((x) => x.value === tab)}>
                <NodeTerminal node={n} cwd={path} />
              </SlideKeep>
            )}
          </div>
        </>
      )}
    </>
  );
}

// RenameNode：修改节点在面板上显示的名称（放在对话框里）。
function RenameNode({ n, onDone }: { n: Node; onDone: () => void }) {
  const queryClient = useQueryClient();
  const [name, setName] = useState(n.name);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const trimmed = name.trim();
  const save = async () => {
    if (trimmed === n.name) return onDone();
    setBusy(true);
    setError("");
    try {
      await nodeClient.renameNode({ nodeId: n.id, name: trimmed });
      await queryClient.invalidateQueries({ queryKey: ["nodes"] });
      onDone();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };
  return (
    <form
      className="grid gap-4"
      onSubmit={(e) => {
        e.preventDefault();
        save();
      }}
    >
      <Field label={t.nodes.rename.label} htmlFor="node-name" hint={t.nodes.rename.hint(n.info?.hostname || "—")}>
        <Input id="node-name" value={name} onChange={(e) => setName(e.target.value)} maxLength={64} autoFocus required pattern=".*\S.*" />
      </Field>
      {error && <Alert>{error}</Alert>}
      <DialogActions>
        <Button variant="outline" readOnlyOk onClick={onDone}>
          {t.common.actions.cancel}
        </Button>
        <Button type="submit" disabled={busy}>
          {busy && <Spinner />}
          {t.common.actions.save}
        </Button>
      </DialogActions>
    </form>
  );
}

// NodeUpgrade：节点版本早于面板时的升级提示和进度。进度记在面板上，刷新页面后还在；节点用新版本回来后记录消失，提示随之消失。
function NodeUpgrade({ n, target, state, onDone }: { n: Node; target: string; state?: UpdateState; onDone: () => void }) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const upgrade = async () => {
    if (!(await confirmAction(t.nodes.upgrade.confirm(n.name, target), { action: t.nodes.upgrade.action }))) return;
    setError("");
    setBusy(true);
    try {
      await nodeClient.upgradeNode({ nodeId: n.id });
      onDone();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };
  const cancel = async () => {
    setError("");
    setBusy(true);
    try {
      await nodeClient.cancelNodeUpgrade({ nodeId: n.id });
      onDone();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };
  if (state?.phase === "restarting") {
    return (
      <Notice tone="success" title={t.nodes.upgrade.restartingTitle(state.target)}>
        {t.nodes.upgrade.restartingText}
      </Notice>
    );
  }
  if (state?.phase === "installing" || state?.phase === "queued") {
    const queued = state.phase === "queued";
    return (
      <>
        <Notice
          tone={queued ? "neutral" : "info"}
          title={queued ? t.nodes.upgrade.queuedTitle(state.target) : t.nodes.upgrade.installingTitle(state.target)}
          actions={
            cancelable(state, n) && (
              <Button variant="outline" onClick={cancel} disabled={busy}>
                {busy ? t.nodes.upgrade.canceling : t.nodes.upgrade.cancel}
              </Button>
            )
          }
        >
          {queued ? t.nodes.upgrade.queuedText : t.nodes.upgrade.installingText}
          {!cancelable(state, n) && t.nodes.upgrade.noCancel}
        </Notice>
        {error && <Alert>{error}</Alert>}
      </>
    );
  }
  const failed = state?.phase === "failed";
  return (
    <>
      <Notice
        tone={failed ? "danger" : "info"}
        title={failed ? t.nodes.upgrade.failedTitle(state.target) : t.nodes.upgrade.title(target)}
        actions={
          target && (
            <Button variant="outline" onClick={upgrade} disabled={busy}>
              {busy ? t.nodes.upgrade.running : failed ? t.nodes.upgrade.retry : t.nodes.upgrade.action}
            </Button>
          )
        }
      >
        {failed && state.error ? (
          state.error
        ) : (
          <>
            {t.nodes.upgrade.text(n.info?.version || "—")} <HelpTip>{t.nodes.upgrade.help}</HelpTip>
          </>
        )}
      </Notice>
      {error && <Alert>{error}</Alert>}
    </>
  );
}

function AddNode({ onClose }: { onClose: () => void }) {
  const queryClient = useQueryClient();
  const settings = useQuery({
    queryKey: ["settings"],
    queryFn: () => settingsClient.getSettings({}),
  });
  const [address, setAddress] = useState<string | null>(null);
  const [command, setCommand] = useState("");
  const [addressError, setAddressError] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  // saveAddress 保存修改过的对外地址。安装命令用的是已保存的地址，所以生成前先保存。
  const saveAddress = async () => {
    if (address === null) return true;
    setAddressError("");
    try {
      await settingsClient.updateSettings({ publicAddress: address });
      setAddress(null);
      await queryClient.invalidateQueries({ queryKey: ["settings"] });
      return true;
    } catch (err) {
      setAddressError(errorMessage(err));
      return false;
    }
  };

  const create = async () => {
    setError("");
    setBusy(true);
    try {
      if (!(await saveAddress())) return;
      const res = await nodeClient.createEnrollCommand({});
      setCommand(res.command);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const s = settings.data?.settings;
  if (settings.isPending) return <RowsSkeleton count={1} />;
  if (settings.error) return <QueryError error={errorMessage(settings.error)} onRetry={() => settings.refetch()} />;
  return (
    // 在地址框里按 Enter：还没生成或改了地址时生成，否则等于「完成」。
    <form
      className="grid grid-cols-[minmax(0,1fr)] gap-4"
      onSubmit={(e) => {
        e.preventDefault();
        if (command && address === null) onClose();
        else create();
      }}
    >
      <Field label={t.nodes.add.publicAddress} help={t.nodes.add.publicAddressHelp} htmlFor="public-address" hint={s?.port ? t.nodes.add.portHint(String(s.port)) : undefined} error={addressError}>
        <Input id="public-address" value={address ?? s?.publicAddress ?? ""} onChange={(e) => setAddress(e.target.value)} autoComplete="off" autoCapitalize="none" spellCheck={false} />
      </Field>
      {command && (
        <>
          <CodeBlock copy nowrap>{command}</CodeBlock>
          <p className="flex items-center gap-1 text-[13px] leading-5 text-muted-foreground">
            {t.nodes.add.commandHint}
            <HelpTip>{t.nodes.add.commandHelp}</HelpTip>
          </p>
        </>
      )}
      {error && <Alert>{error}</Alert>}
      <DialogActions>
        {command ? (
          <>
            <Button variant="outline" onClick={create} disabled={busy}>
              {busy && <Spinner />}
              {t.nodes.add.regenerate}
            </Button>
            <Button type="submit" readOnlyOk>
              {t.nodes.add.finish}
            </Button>
          </>
        ) : (
          <>
            <Button variant="outline" readOnlyOk onClick={onClose}>
              {t.common.actions.cancel}
            </Button>
            <Button type="submit" disabled={busy}>
              {busy && <Spinner />}
              {t.nodes.add.generate}
            </Button>
          </>
        )}
      </DialogActions>
    </form>
  );
}

// NodeSettings：节点「设置」标签。和设置页一样用设置行：一行状态加一个按钮，表单放进对话框（docs/ui.md「节点详情」）。
function NodeSettings({ n }: { n: Node }) {
  const { isAdmin } = useRole();
  const reclaim = useRun("");
  const [editing, setEditing] = useState<"root" | "mirrors" | null>(null);
  const root = n.info?.projectsRoot || "/srv/nagi";
  const mirrors = useQuery({ queryKey: ["node-mirrors", n.id], queryFn: () => nodeClient.getNodeMirrors({ nodeId: n.id }), enabled: isAdmin });
  const denied = !isAdmin || (mirrors.error instanceof ConnectError && mirrors.error.code === Code.PermissionDenied);
  const m = t.nodes.reclaim;
  const startReclaim = async () => {
    if (!(await confirmAction(m.confirm, { action: m.action }))) return;
    await reclaim.run(() => nodeClient.reclaimNodeSnapshots({ nodeId: n.id }));
  };
  return (
    <>
      <SettingRows>
        <SettingRow label={t.nodes.settings.root} help={t.nodes.settings.rootHelp} description={<span className="font-mono">{root}</span>}>
          <Button variant="outline" onClick={() => setEditing("root")}>
            {t.common.actions.edit}
          </Button>
        </SettingRow>
        <SettingRow
          label={t.nodes.mirrors.title}
          help={t.nodes.mirrors.description}
          description={denied ? t.nodes.mirrors.adminOnly : mirrors.isPending ? <LineSkeleton /> : mirrors.error ? <span className="text-destructive">{errorMessage(mirrors.error)}</span> : <MirrorsSummary mirrors={mirrors.data} />}
        >
          {denied || mirrors.isPending ? null : mirrors.error ? (
            <Button variant="outline" readOnlyOk onClick={() => mirrors.refetch()}>
              {t.common.actions.retry}
            </Button>
          ) : (
            <Button variant="outline" onClick={() => setEditing("mirrors")}>
              {t.common.actions.edit}
            </Button>
          )}
        </SettingRow>
        <NodeQUIC nodeId={n.id} online={n.online} />
        <SettingRow label={m.title} help={m.description} description={!isAdmin ? m.adminOnly : undefined}>
          {isAdmin && (
            <Button variant="outline" disabled={reclaim.busy || !n.online} onClick={startReclaim}>
              {reclaim.busy && <Spinner />}
              {m.action}
            </Button>
          )}
        </SettingRow>
      </SettingRows>
      {/* 清理快照仓库的输出接在设置行下面。 */}
      {reclaim.hasOutput && <div className="mt-4">{reclaim.output}</div>}
      <Dialog open={editing === "root"} onClose={() => setEditing(null)} title={t.nodes.settings.root}>
        <RootForm nodeId={n.id} root={root} onDone={() => setEditing(null)} />
      </Dialog>
      <Dialog open={editing === "mirrors" && !!mirrors.data} onClose={() => setEditing(null)} title={t.nodes.mirrors.title} description={t.nodes.mirrors.description} className="max-w-[520px]">
        {mirrors.data && <MirrorsForm nodeId={n.id} mirrors={mirrors.data} onDone={() => setEditing(null)} />}
      </Dialog>
    </>
  );
}

// LineSkeleton：设置行第二行的占位，和文字一样高。
function LineSkeleton() {
  return (
    <span className="flex h-5 items-center">
      <Skeleton className="h-3 w-24" />
    </span>
  );
}

// RootForm：编辑项目根目录（新放到这个节点的容器，目录默认是 <项目根目录>/<网络名>/<容器名>）。
function RootForm({ nodeId, root, onDone }: { nodeId: string; root: string; onDone: () => void }) {
  const queryClient = useQueryClient();
  const [value, setValue] = useState(root);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const save = async () => {
    if (value === root) return onDone();
    setBusy(true);
    setError("");
    try {
      await nodeClient.setNodeProjectsRoot({ nodeId, root: value });
      await queryClient.invalidateQueries({ queryKey: ["nodes"] });
      onDone();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };
  return (
    <form
      className="grid gap-4"
      onSubmit={(e) => {
        e.preventDefault();
        save();
      }}
    >
      <Field label={t.nodes.settings.root} htmlFor={`root-${nodeId}`} help={t.nodes.settings.rootHelp}>
        <Input id={`root-${nodeId}`} value={value} onChange={(e) => setValue(e.target.value)} placeholder={t.nodes.settings.rootHint} className="font-mono" autoFocus autoComplete="off" autoCapitalize="none" spellCheck={false} required pattern=".*\S.*" />
      </Field>
      {error && <Alert keepFocus>{error}</Alert>}
      <DialogActions>
        <Button variant="outline" readOnlyOk onClick={onDone}>
          {t.common.actions.cancel}
        </Button>
        <Button type="submit" disabled={busy}>
          {busy && <Spinner />}
          {t.common.actions.save}
        </Button>
      </DialogActions>
    </form>
  );
}

// Docker 会给加速地址补上末尾的 /，比较时忽略。
const trimSlash = (s: string) => s.replace(/\/+$/, "");

function sameMirrors(a: string[], b: string[]) {
  return a.length === b.length && a.every((m, i) => trimSlash(m) === trimSlash(b[i]));
}

// MirrorsSummary：镜像加速那一行的状态：当前的加速地址；daemon.json 里的和 Docker 正在用的不一样时（还没有重启 Docker），后面写「重启 Docker 后生效」。
function MirrorsSummary({ mirrors }: { mirrors: Mirrors }) {
  const pending = !sameMirrors(mirrors.mirrors, mirrors.active);
  const list = t.common.list(mirrors.mirrors) || t.nodes.mirrors.empty;
  return (
    <span className="flex min-w-0 items-center gap-1">
      <span className="truncate" title={list}>
        {list}
      </span>
      {pending && (
        <>
          <span className="shrink-0">·</span>
          <span className="shrink-0 text-warning-text">{t.nodes.mirrors.pendingTitle}</span>
          <HelpTip>
            <span className="break-all">{t.nodes.mirrors.pendingText(t.common.list(mirrors.active) || t.nodes.mirrors.none)}</span>
          </HelpTip>
        </>
      )}
    </span>
  );
}

// MirrorsForm：编辑节点的 Docker 镜像加速地址（见 docs/agent.md#镜像），放在对话框里。
function MirrorsForm({ nodeId, mirrors, onDone }: { nodeId: string; mirrors: Mirrors; onDone: () => void }) {
  const queryClient = useQueryClient();
  const [value, setValue] = useState(mirrors.mirrors.join("\n"));
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const save = async () => {
    setBusy(true);
    setError("");
    try {
      const list = value
        .split("\n")
        .map((s) => s.trim())
        .filter(Boolean);
      const res = await nodeClient.setNodeMirrors({ nodeId, mirrors: list });
      queryClient.setQueryData(["node-mirrors", nodeId], res);
      onDone();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };
  return (
    <form
      className="grid gap-4"
      onSubmit={(e) => {
        e.preventDefault();
        save();
      }}
    >
      <Field label={t.nodes.mirrors.label} htmlFor={`mirrors-${nodeId}`} hint={t.nodes.mirrors.hint} help={t.nodes.mirrors.help}>
        <Textarea id={`mirrors-${nodeId}`} rows={4} value={value} onChange={(e) => setValue(e.target.value)} placeholder="https://mirror.example.com" autoFocus spellCheck={false} className="py-2" />
      </Field>
      {error && <Alert keepFocus>{error}</Alert>}
      <DialogActions>
        <Button variant="outline" readOnlyOk onClick={onDone}>
          {t.common.actions.cancel}
        </Button>
        <Button type="submit" disabled={busy}>
          {busy && <Spinner />}
          {t.common.actions.save}
        </Button>
      </DialogActions>
    </form>
  );
}

// NodeReplicas：节点作为备份节点或热备保存的副本。已经没有容器使用的副本可以删除。
function NodeReplicas({ nodeId, nodes }: { nodeId: string; nodes: Node[] }) {
  const queryClient = useQueryClient();
  const q = useQuery({ queryKey: ["node-replicas", nodeId], queryFn: () => nodeClient.listNodeReplicas({ nodeId }) });
  const [error, setError] = useState("");
  const remove = async (projectId: string, container: string, label: string) => {
    const m = t.nodes.replicas;
    const answer = await ask(m.deleteConfirm(label), { action: m.deleteAction, danger: true, checkDefault: true, typeToConfirm: { label: m.deleteType(container), value: container } });
    if (!answer) return;
    setError("");
    try {
      await nodeClient.deleteNodeReplica({ nodeId, projectId, container });
      queryClient.invalidateQueries({ queryKey: ["node-replicas", nodeId] });
    } catch (err) {
      setError(errorMessage(err));
    }
  };
  return (
    <Section title={t.nodes.replicas.title} help={t.nodes.replicas.intro}>
      {error && <Alert>{error}</Alert>}
      {q.isPending ? (
        <RowsSkeleton />
      ) : q.error ? (
        <QueryError error={errorMessage(q.error)} onRetry={() => q.refetch()} />
      ) : q.data.replicas.length === 0 ? (
        <EmptyRow>{t.nodes.replicas.empty}</EmptyRow>
      ) : (
        <List>
          {q.data.replicas.map(({ replica: r, inUse }) => {
            const label = `${r?.projectName || r?.projectId} / ${r?.container}`;
            const source = nodes.find((x) => x.id === r?.sourceNodeId)?.name || r?.sourceName;
            const detail = [source && t.nodes.replicas.source(source), r?.lastSync ? t.nodes.replicas.synced(timeAgo(toDate(r.lastSync))) : t.nodes.replicas.neverSynced].filter(Boolean).join(" · ");
            return (
              <Row
                key={`${r?.projectId}-${r?.container}`}
                title={<span className="truncate">{label}</span>}
                sub={!inUse ? <span className="text-warning-text" title={detail}>{t.nodes.replicas.unused}</span> : detail}
                cols={[{ content: <Roll value={fmtBytes(Number(r?.bytes ?? 0))} n={Number(r?.bytes ?? 0)} />, num: true }]}
                actions={
                  inUse ? null : (
                    <Button size="row" variant="ghost" onClick={() => remove(r!.projectId, r!.container, label)}>
                      {t.common.actions.delete}
                    </Button>
                  )
                }
              />
            );
          })}
        </List>
      )}
    </Section>
  );
}

// StaleProjects：节点报告配置由别的面板写过的项目。以节点为准要删掉面板上的记录，节点又可能被攻破，所以由管理员确认。
function StaleProjects({ nodeId, list, onDone }: { nodeId: string; list: StaleProject[]; onDone: () => void }) {
  const [error, setError] = useState("");
  const adopt = async (p: StaleProject) => {
    if (!(await confirmAction(t.nodes.stale.adoptConfirm(p.projectName), { action: t.nodes.stale.adopt, danger: true }))) return;
    setError("");
    try {
      await nodeClient.adoptStaleProject({ nodeId, projectId: p.projectId });
      onDone();
    } catch (err) {
      setError(errorMessage(err));
    }
  };
  return (
    <div className="grid gap-4">
      <Notice tone="warning" title={t.nodes.stale.title}>
        {t.nodes.stale.text}
      </Notice>
      {error && <Alert>{error}</Alert>}
      <List>
        {list.map((p) => (
          <Row
            key={p.projectId}
            title={<span className="truncate">{p.projectName}</span>}
            actionsWidth="w-auto"
            actions={
              <Button size="row" variant="ghost" onClick={() => adopt(p)}>
                {t.nodes.stale.adopt}
              </Button>
            }
          />
        ))}
      </List>
    </div>
  );
}

// UnknownContainers：节点上有、面板却不认识的容器（所属项目是在面板上建的，所以不自动加进去，防止节点冒充）。
function UnknownContainers({ nodeId, list, onDone }: { nodeId: string; list: UnknownContainer[]; onDone: () => void }) {
  const [error, setError] = useState("");
  const adopt = async (u: UnknownContainer) => {
    if (!(await confirmAction(t.nodes.unknown.adoptConfirm(u.container, u.projectName), { action: t.nodes.unknown.adopt }))) return;
    setError("");
    try {
      await nodeClient.adoptNodeContainer({ nodeId, projectId: u.projectId, container: u.container });
      onDone();
    } catch (err) {
      setError(errorMessage(err));
    }
  };
  return (
    <div className="grid gap-4">
      <Notice tone="warning" title={t.nodes.unknown.title}>
        {t.nodes.unknown.text}
      </Notice>
      {error && <Alert>{error}</Alert>}
      <List>
        {list.map((u) => (
          <Row
            key={u.projectId + "/" + u.container}
            title={<span className="truncate">{u.container}</span>}
            sub={u.projectName}
            actionsWidth="w-auto"
            actions={
              <Button size="row" variant="ghost" onClick={() => adopt(u)}>
                {t.nodes.unknown.adopt}
              </Button>
            }
          />
        ))}
      </List>
    </div>
  );
}
