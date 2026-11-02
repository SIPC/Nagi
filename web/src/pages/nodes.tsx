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
import { NodeUsageBars, fmtBytes, fmtBytesShort, metricRanges, type MetricRange } from "@/components/metrics";
import { TimeChart } from "@/components/chart";
import { HandOver, NodePanels } from "@/pages/panels";
import { NodeMesh } from "@/pages/mesh";
import { CodeBox } from "@/pages/invite";
import { NodeMap, type MapLayer } from "@/pages/node-map";
import {
  Alert,
  Badge,
  Button,
  cardClass,
  CardGrid,
  Dialog,
  DialogActions,
  Empty,
  EmptyRow,
  Field,
  FormActions,
  FormBody,
  FormRow,
  HelpTip,
  Input,
  List,
  Loading,
  Menu,
  Notice,
  QueryError,
  Row,
  RowsSkeleton,
  Section,
  Segmented,
  Skeleton,
  Spinner,
  Status,
  Tabs,
  Textarea,
  ReadOnlyContext,
} from "@/components/ui";
import type { Mirrors } from "@/gen/nagi/v1/agent_pb";
import type { Node, UnknownContainer, UpdateState } from "@/gen/nagi/v1/api_pb";
import type { NodeInfo, NodeUsage } from "@/gen/nagi/v1/common_pb";
import { confirmAction, notify } from "@/components/confirm";
import { updateActive, upgradeTarget, useUpdateStatus } from "@/pages/update";
import { useRole } from "@/components/role";
import { toast } from "sonner";
import { stateOf } from "@/components/status";
import { NodeFiles, NodeTerminal, nodeAccessBlock } from "@/pages/node-access";

export type NodeTab = "metrics" | "containers" | "terminal" | "files" | "panels" | "mesh" | "settings" | "replicas";

export const nodeTabs: { value: NodeTab; label: string }[] = [
  { value: "metrics", label: t.nodes.tabs.metrics },
  { value: "containers", label: t.nodes.tabs.containers },
  { value: "terminal", label: t.nodes.tabs.terminal },
  { value: "files", label: t.nodes.tabs.files },
  { value: "settings", label: t.nodes.tabs.settings },
  { value: "replicas", label: t.nodes.tabs.replicas },
  { value: "panels", label: t.nodes.tabs.panels },
  { value: "mesh", label: t.nodes.tabs.mesh },
];

function useNodeList() {
  return useQuery({ queryKey: ["nodes"], queryFn: () => nodeClient.listNodes({}), refetchInterval: 5000 });
}

function usePanelVersion() {
  const settings = useQuery({ queryKey: ["settings"], queryFn: () => settingsClient.getSettings({}) });
  return settings.data?.settings?.version ?? "";
}

// 例如 8 GB、1.0 TB：数字和单位之间留空格。
const sizeLabel = (v: number) => fmtBytesShort(v).replace(/([A-Z])/, " $1");

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

const ago = (seconds: number) => timestampFromDate(new Date(Date.now() - seconds * 1000));
const fmtPercent = (v: number) => `${v < 10 ? v.toFixed(1) : Math.round(v)}%`;

// NodeResources：节点页的监控，每项一个数字加一张小图，三列；时间范围在区块标题右侧。内存和磁盘写成「已用 / 总量」。
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
  const rate = (v: number) => `${fmtBytes(v)}/s`;
  const items = [
    { label: "CPU", name: "node:cpu", format: fmtPercent, minMax: 1 },
    { label: t.metrics.memory, name: "node:mem", format: ofTotal(Number(n.info?.memoryBytes ?? 0)) },
    { label: t.metrics.disk, name: "node:disk", format: ofTotal(diskTotal(n.info, n.usage)) },
    { label: t.metrics.netRx, name: "node:net_rx", format: rate },
    { label: t.metrics.netTx, name: "node:net_tx", format: rate },
  ];
  const now = Date.now() / 1000;
  return (
    <Section title={t.metrics.resourceUsage} actions={<Segmented value={range} onChange={setRange} items={metricRanges.map((x) => ({ value: x.value, label: t.metrics.rangesShort[x.value] }))} />}>
      {q.isPending ? (
        <Loading />
      ) : q.error ? (
        <Alert>{errorMessage(q.error)}</Alert>
      ) : (
        <div className="grid grid-cols-1 gap-8 md:grid-cols-3">
          {items.map((it) => {
            const s = q.data.series.find((x) => x.name === it.name);
            const points = (s?.values ?? []).map((v, i): [number, number] => [Number(s!.times[i]), v]);
            return (
              <div key={it.name} className="grid min-w-0 gap-3">
                <div className="text-muted-foreground">{it.label}</div>
                <div className="text-[20px] leading-7 font-medium tracking-tight">{points.length ? it.format(points[points.length - 1][1]) : "—"}</div>
                <TimeChart
                  className="mt-2"
                  series={[{ label: it.label, points, className: "text-info" }]}
                  from={now - r.seconds}
                  to={now}
                  step={r.step}
                  format={it.format}
                  height={80}
                  axis={false}
                  minMax={it.minMax}
                />
              </div>
            );
          })}
        </div>
      )}
    </Section>
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
                content: (
                  <span className={cn(st.tone === "danger" && "text-destructive", st.tone === "warning" && "text-warning-text")}>
                    <Status tone={st.tone}>{st.label}</Status>
                  </span>
                ),
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

export function NodesPage({ view, onView }: { view: NodesView; onView: (v: NodesView) => void }) {
  const nodes = useNodeList();
  const panelVersion = usePanelVersion();
  const { isAdmin } = useRole();
  const status = useUpdateStatus(isAdmin);
  const [dialog, setDialog] = useState<"add" | "handover" | null>(null);
  const [starting, setStarting] = useState(false);
  const list = nodes.data?.nodes ?? [];
  const states = status.data?.nodes ?? {};
  const running = Object.values(states).filter(updateActive).length;
  const behind = list.filter((n) => upgradeTarget(n, panelVersion) && !updateActive(states[n.id])).length;
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
    <CardGrid>
      {list.map((n) => (
        <NodeCard key={n.id} n={n} canUpgrade={upgradeTarget(n, panelVersion) !== ""} upgrade={states[n.id]} onApproved={() => nodes.refetch()} />
      ))}
    </CardGrid>
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
            <Button onClick={() => setDialog("add")}>
              <Plus />
              {t.nodes.addNode}
            </Button>
            {list.length > 0 && (
              <Button variant="outline" onClick={() => setDialog("handover")}>
                {t.nodes.handover}
              </Button>
            )}
          </>
        }
      />
      {nodes.isPending ? (
        <CardsSkeleton />
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
      ) : view !== "cards" ? (
        <>
          <div className="hidden sm:block">
            <NodeMap nodes={list} layer={view} />
          </div>
          <div className="sm:hidden">{cards}</div>
        </>
      ) : (
        cards
      )}
      <Dialog open={dialog === "add"} onClose={() => setDialog(null)} title={t.nodes.addNode} className="max-w-[520px]">
        <AddNode onClose={() => setDialog(null)} />
      </Dialog>
      <Dialog open={dialog === "handover"} onClose={() => setDialog(null)} title={t.nodes.handover} className="max-w-[520px]">
        <HandOver nodes={list} onClose={() => setDialog(null)} />
      </Dialog>
    </>
  );
}

// CardsSkeleton：节点卡片加载中。
function CardsSkeleton() {
  return (
    <div role="status" aria-label={t.common.feedback.loading}>
      <CardGrid>
        {[0, 1].map((i) => (
          <div key={i} className="flex flex-col gap-3 rounded-lg border bg-background p-4">
            <Skeleton className="my-1 h-3 w-3/5" />
            <Skeleton className="my-1 h-3 w-2/5" />
            <Skeleton className="my-1 h-3 w-4/5" />
          </div>
        ))}
      </CardGrid>
    </div>
  );
}

function NodeCard({ n, canUpgrade, upgrade, onApproved }: { n: Node; canUpgrade: boolean; upgrade?: UpdateState; onApproved: () => void }) {
  const u = n.online && !n.pending ? n.usage : undefined;
  return (
    <Link to="/nodes/$nodeId" params={{ nodeId: n.id }} className={cn(cardClass, "min-w-0 gap-3 p-4")}>
      <div className="flex min-w-0 items-center justify-between gap-2">
        <span className="flex min-w-0 items-center gap-2">
          <span className="truncate font-medium">{n.name}</span>
          {n.info?.embedded && <Badge>{t.nodes.embedded}</Badge>}
        </span>
        <NodeStatus n={n} />
      </div>
      <div className="-mt-2 flex min-w-0 items-center justify-between gap-2">
        <span className="truncate font-mono text-xs leading-5 text-muted-foreground">{n.address || n.id}</span>
        {upgrade ? (
          <Badge tone={upgrade.phase === "failed" ? "warning" : "neutral"} title={upgrade.error || undefined}>
            {t.nodes.upgrade.phases[upgrade.phase as keyof typeof t.nodes.upgrade.phases]}
          </Badge>
        ) : (
          canUpgrade && <Badge tone="info">{t.nodes.upgrade.badge}</Badge>
        )}
      </div>
      {n.pending ? (
        <>
          <p className="text-[13px] leading-5 text-muted-foreground">{t.nodes.pendingHint}</p>
          <Button
            variant="outline"
            onClick={async (e) => {
              e.preventDefault();
              if (await approve(n)) onApproved();
            }}
          >
            {t.nodes.approve}
          </Button>
        </>
      ) : n.online ? (
        <NodeUsageBars usage={u} info={n.info} />
      ) : (
        <p className="text-[13px] leading-5 text-muted-foreground">{t.nodes.lastSeen(timeAgo(toDate(n.lastSeen)))}</p>
      )}
      <div className="mt-auto flex items-center justify-between gap-3 text-[13px] leading-5 text-muted-foreground">
        <span className="shrink-0">{t.nodes.containers(n.containers)}</span>
        <span className="truncate">{n.info ? `Docker ${n.info.dockerVersion || "?"}` : "—"}</span>
      </div>
    </Link>
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
        <Notice tone="danger" title={t.nodes.notFound} />
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
    if (!(await confirmAction(t.nodes.removeConfirm(n.name), { action: t.common.actions.remove, danger: true }))) return;
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
        className={cn("mb-4", browsing && "max-md:hidden")}
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
                <span title={`Compose ${n.info.composeVersion || "?"}`}>{`Docker ${n.info.dockerVersion || "?"}`}</span>
                <span>{`Nagi ${n.info.version || "?"}`}</span>
              </>
            )}
            <button type="button" className="cursor-pointer underline-offset-4 hover:text-foreground hover:underline" onClick={() => onTab("containers")}>
              {t.nodes.containers(n.containers)}
            </button>
            {!n.online && <span>{t.nodes.lastSeen(timeAgo(toDate(n.lastSeen)))}</span>}
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
      {(target || upgradeState || n.unknownContainers.length > 0 || n.pending) && (
        <div className="mb-4 grid gap-2">
          {(target || upgradeState) && <NodeUpgrade key={target} n={n} target={target} state={upgradeState} onDone={() => status.refetch()} />}
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
          {/* 这一层不能省：区块靠 first: 去掉第一个的分隔线。 */}
          <div>
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
              <>
                <NodeSettings nodeId={n.id} root={n.info?.projectsRoot || "/srv/nagi"} />
                <NodeMirrors nodeId={n.id} />
              </>
            ) : (
              <NodePanels nodeId={n.id} />
            )}
            {/* 终端不卸载：卸载就断开了。放在后面：区块靠 first: 去掉第一条分隔线。 */}
            {showAccess && n.online && !accessBlock && shellOpened && (
              <div className={tab === "terminal" ? undefined : "hidden"}>
                <NodeTerminal node={n} cwd={path} />
              </div>
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
      <Notice tone={queued ? "neutral" : "info"} title={queued ? t.nodes.upgrade.queuedTitle(state.target) : t.nodes.upgrade.installingTitle(state.target)}>
        {queued ? t.nodes.upgrade.queuedText : t.nodes.upgrade.installingText}
      </Notice>
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
            {t.nodes.upgrade.text(n.info?.version || "?")} <HelpTip>{t.nodes.upgrade.help}</HelpTip>
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
  const [error, setError] = useState("");

  const saveAddress = async () => {
    setError("");
    try {
      await settingsClient.updateSettings({ publicAddress: address ?? "" });
      setAddress(null);
      queryClient.invalidateQueries({ queryKey: ["settings"] });
    } catch (err) {
      setError(errorMessage(err));
    }
  };

  const create = async () => {
    setError("");
    try {
      const res = await nodeClient.createEnrollCommand({});
      setCommand(res.command);
    } catch (err) {
      setError(errorMessage(err));
    }
  };

  const s = settings.data?.settings;
  if (settings.isPending) return <RowsSkeleton count={1} />;
  if (settings.error) return <QueryError error={errorMessage(settings.error)} onRetry={() => settings.refetch()} />;
  return (
    <div className="grid gap-4">
      {error && <Alert>{error}</Alert>}
      <div className="grid gap-4 sm:grid-cols-2">
        <Field label={t.nodes.add.publicAddress} help={t.nodes.add.publicAddressHelp} htmlFor="public-address">
          <div className="flex items-center gap-2">
            <Input id="public-address" value={address ?? s?.publicAddress ?? ""} onChange={(e) => setAddress(e.target.value)} />
            {address !== null && (
              <Button variant="outline" onClick={saveAddress}>
                {t.common.actions.save}
              </Button>
            )}
          </div>
        </Field>
        <Field label={t.nodes.add.port} htmlFor="public-port">
          <Input id="public-port" value={s?.port ?? ""} readOnly />
        </Field>
      </div>
      {command && (
        <>
          <CodeBox text={command} />
          <p className="flex items-center gap-1 text-[13px] leading-5 text-muted-foreground">
            {t.nodes.add.commandHint}
            <HelpTip>{t.nodes.add.commandHelp}</HelpTip>
          </p>
        </>
      )}
      <DialogActions>
        {command ? (
          <>
            <Button variant="outline" onClick={create}>
              {t.nodes.add.regenerate}
            </Button>
            <Button readOnlyOk onClick={onClose}>
              {t.nodes.add.finish}
            </Button>
          </>
        ) : (
          <>
            <Button variant="outline" readOnlyOk onClick={onClose}>
              {t.common.actions.cancel}
            </Button>
            <Button onClick={create}>{t.nodes.add.generate}</Button>
          </>
        )}
      </DialogActions>
    </div>
  );
}

// NodeSettings 是节点自己的设置：项目根目录（新放到这个节点的容器，目录默认是 <项目根目录>/<网络名>/<容器名>）。
function NodeSettings({ nodeId, root }: { nodeId: string; root: string }) {
  const queryClient = useQueryClient();
  const [value, setValue] = useState(root);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const save = async () => {
    setBusy(true);
    setError("");
    try {
      await nodeClient.setNodeProjectsRoot({ nodeId, root: value });
      toast.success(t.common.feedback.saved);
      queryClient.invalidateQueries({ queryKey: ["nodes"] });
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };
  return (
    <Section title={t.nodes.settings.rootTitle}>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          save();
        }}
      >
        <FormBody>
          <FormRow>
            <Field label={t.nodes.settings.root} htmlFor={`root-${nodeId}`} help={t.nodes.settings.rootHelp}>
              <Input id={`root-${nodeId}`} value={value} onChange={(e) => setValue(e.target.value)} placeholder={t.nodes.settings.rootHint} required pattern=".*\S.*" />
            </Field>
          </FormRow>
          {error && <Alert>{error}</Alert>}
          <FormActions>
            <Button type="submit" variant="outline" disabled={busy}>
              {busy && <Spinner />}
              {t.common.actions.save}
            </Button>
          </FormActions>
        </FormBody>
      </form>
    </Section>
  );
}

// NodeMirrors：节点的 Docker 镜像加速地址（见 docs/agent.md#镜像）。
function NodeMirrors({ nodeId }: { nodeId: string }) {
  const { isAdmin } = useRole();
  const q = useQuery({ queryKey: ["node-mirrors", nodeId], queryFn: () => nodeClient.getNodeMirrors({ nodeId }), enabled: isAdmin });
  const denied = !isAdmin || (q.error instanceof ConnectError && q.error.code === Code.PermissionDenied);
  return (
    <Section title={t.nodes.mirrors.title}>
      {denied ? (
        <Notice title={t.nodes.mirrors.adminOnly} />
      ) : q.isPending ? (
        <RowsSkeleton count={1} />
      ) : q.error ? (
        <QueryError error={errorMessage(q.error)} onRetry={() => q.refetch()} />
      ) : (
        <MirrorsForm nodeId={nodeId} mirrors={q.data} />
      )}
    </Section>
  );
}

// Docker 会给加速地址补上末尾的 /，比较时忽略。
const trimSlash = (s: string) => s.replace(/\/+$/, "");

function sameMirrors(a: string[], b: string[]) {
  return a.length === b.length && a.every((m, i) => trimSlash(m) === trimSlash(b[i]));
}

function MirrorsForm({ nodeId, mirrors }: { nodeId: string; mirrors: Mirrors }) {
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
      setValue(res.mirrors.join("\n"));
      toast.success(t.common.feedback.saved);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };
  // daemon.json 里的和 Docker 正在用的不一样：还没有重启 Docker。
  const pending = !sameMirrors(mirrors.mirrors, mirrors.active);
  return (
    <>
      {pending && (
        <Notice tone="warning" title={t.nodes.mirrors.pendingTitle}>
          <span className="break-all">{t.nodes.mirrors.pendingText(mirrors.active.join(", ") || t.nodes.mirrors.none)}</span>
        </Notice>
      )}
      <form
        onSubmit={(e) => {
          e.preventDefault();
          save();
        }}
      >
        <FormBody>
          <Field
            label={t.nodes.mirrors.label}
            htmlFor={`mirrors-${nodeId}`}
            help={
              <span className="grid gap-1">
                <span>{t.nodes.mirrors.description}</span>
                <span>{t.nodes.mirrors.hint}</span>
                <span>{t.nodes.mirrors.help}</span>
              </span>
            }
          >
            <Textarea id={`mirrors-${nodeId}`} rows={3} value={value} onChange={(e) => setValue(e.target.value)} placeholder="https://mirror.example.com" spellCheck={false} className="py-2" />
          </Field>
          {error && <Alert>{error}</Alert>}
          <FormActions>
            <Button type="submit" variant="outline" disabled={busy}>
              {busy && <Spinner />}
              {t.common.actions.save}
            </Button>
          </FormActions>
        </FormBody>
      </form>
    </>
  );
}

// NodeReplicas：节点作为备份节点或热备保存的副本。已经没有容器使用的副本可以删除。
function NodeReplicas({ nodeId, nodes }: { nodeId: string; nodes: Node[] }) {
  const queryClient = useQueryClient();
  const q = useQuery({ queryKey: ["node-replicas", nodeId], queryFn: () => nodeClient.listNodeReplicas({ nodeId }) });
  const [error, setError] = useState("");
  const remove = async (projectId: string, container: string) => {
    if (!(await confirmAction(t.nodes.replicas.deleteConfirm(container), { action: t.common.actions.delete, danger: true }))) return;
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
                cols={[{ content: fmtBytes(Number(r?.bytes ?? 0)), num: true }]}
                actions={
                  inUse ? null : (
                    <Button size="row" variant="ghost" onClick={() => remove(r!.projectId, r!.container)}>
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
