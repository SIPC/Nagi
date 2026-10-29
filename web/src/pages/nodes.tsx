import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate } from "@tanstack/react-router";
import { Plus, Server } from "lucide-react";
import { errorMessage, nodeClient, settingsClient, toDate } from "@/lib/api";
import { timeAgo } from "@/lib/utils";
import { t } from "@/i18n";
import { Breadcrumb, PageHeader } from "@/pages/layout";
import { NodeMetrics, NodeUsageBars, fmtBytes, fmtBytesShort } from "@/components/metrics";
import { HandOver, NodePanels } from "@/pages/panels";
import { NodeMesh } from "@/pages/mesh";
import { NodeMap, type MapLayer } from "@/pages/node-map";
import {
  Alert,
  Badge,
  Button,
  CardBody,
  cardClass,
  CardFooter,
  CardGrid,
  CardHeader,
  CodeBlock,
  Dialog,
  Empty,
  EmptyRow,
  Field,
  FormActions,
  Input,
  Loading,
  Menu,
  Notice,
  Section,
  Segmented,
  Spinner,
  Status,
  Table,
  Tabs,
  Td,
  Textarea,
  Th,
} from "@/components/ui";
import type { Mirrors } from "@/gen/nagi/v1/agent_pb";
import type { Node, UnknownContainer } from "@/gen/nagi/v1/api_pb";
import type { NodeInfo } from "@/gen/nagi/v1/common_pb";

export type NodeTab = "metrics" | "panels" | "mesh" | "settings" | "replicas";

export const nodeTabs: { value: NodeTab; label: string }[] = [
  { value: "metrics", label: t.nodes.tabs.metrics },
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

// 与 internal/pkg/release.IsVersion 一致。
const versionRe = /^v(\d+)\.(\d+)\.(\d+)([-+][0-9A-Za-z.-]+)?$/;

// isOlder：节点版本 a 是否早于面板版本 b。不是正式版本号（例如本地编译的 dev）时不比较。
function isOlder(a: string, b: string) {
  const x = versionRe.exec(a);
  const y = versionRe.exec(b);
  if (!x || !y) return false;
  for (let i = 1; i <= 3; i++) {
    if (Number(x[i]) !== Number(y[i])) return Number(x[i]) < Number(y[i]);
  }
  // 数字相同时，预发布版（v0.2.0-rc.1）早于正式版（v0.2.0）。
  return a !== b && x[4]?.startsWith("-") === true && !y[4]?.startsWith("-");
}

// upgradeTarget：节点可以一键升级到的版本（即面板的版本），不能升级时返回空串。
// 只升不降；面板自带的节点随面板升级；只支持 Linux 节点（见 internal/agent/upgrade.go）。
function upgradeTarget(n: Node, panelVersion: string) {
  if (!n.online || n.pending || !n.info || n.info.embedded || n.info.os !== "linux") return "";
  return isOlder(n.info.version, panelVersion) ? panelVersion : "";
}

// nodeSpec：节点的配置，例如「2 核 · 内存 3.8GB · 磁盘 39GB」。旧版本的节点不报配置，返回空串。
function nodeSpec(info?: NodeInfo) {
  if (!info) return "";
  const parts: string[] = [];
  if (info.cpus) parts.push(t.nodes.cores(info.cpus));
  if (info.memoryBytes) parts.push(`${t.nodes.memory} ${fmtBytesShort(Number(info.memoryBytes))}`);
  if (info.diskBytes) parts.push(`${t.nodes.disk} ${fmtBytesShort(Number(info.diskBytes))}`);
  return parts.join(" · ");
}

function NodeStatus({ n }: { n: Node }) {
  if (n.pending) return <Status tone="warning">{t.common.status.pendingApproval}</Status>;
  if (n.online) return <Status tone="success">{t.common.status.online}</Status>;
  return <Status>{t.common.status.offline}</Status>;
}

// approve 批准用接入码加入的节点。
async function approve(n: Node) {
  if (!confirm(t.nodes.approveConfirm(n.name, n.address || n.id))) return false;
  await nodeClient.approveNode({ nodeId: n.id });
  return true;
}

export type NodesView = "cards" | MapLayer;

export function NodesPage({ view, onView }: { view: NodesView; onView: (v: NodesView) => void }) {
  const nodes = useNodeList();
  const panelVersion = usePanelVersion();
  const [dialog, setDialog] = useState<"add" | "handover" | null>(null);
  const list = nodes.data?.nodes ?? [];
  const cards = (
    <CardGrid>
      {list.map((n) => (
        <NodeCard key={n.id} n={n} canUpgrade={upgradeTarget(n, panelVersion) !== ""} onApproved={() => nodes.refetch()} />
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
            <Button onClick={() => setDialog("add")}>
              <Plus />
              {t.nodes.addNode}
            </Button>
            {list.length > 0 && <Menu items={[{ label: t.nodes.handover, onSelect: () => setDialog("handover") }]} />}
          </>
        }
      />
      {nodes.isPending ? (
        <Loading />
      ) : nodes.error ? (
        <Alert>{errorMessage(nodes.error)}</Alert>
      ) : list.length === 0 ? (
        <Empty title={t.nodes.empty} icon={<Server className="size-5" />} action={<Button onClick={() => setDialog("add")}>{t.nodes.addNode}</Button>}>
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
      <Dialog open={dialog === "add"} onClose={() => setDialog(null)} title={t.nodes.addNode} description={t.nodes.addNodeDescription}>
        <AddNode />
      </Dialog>
      <Dialog open={dialog === "handover"} onClose={() => setDialog(null)} title={t.nodes.handover} description={t.nodes.handoverDescription}>
        <HandOver nodes={list} />
      </Dialog>
    </>
  );
}

function NodeCard({ n, canUpgrade, onApproved }: { n: Node; canUpgrade: boolean; onApproved: () => void }) {
  const u = n.online && !n.pending ? n.usage : undefined;
  return (
    <Link to="/nodes/$nodeId" params={{ nodeId: n.id }} className={cardClass}>
      <CardHeader
        title={
          <>
            <span className="truncate">{n.name}</span>
            {n.info?.embedded && <Badge>{t.nodes.embedded}</Badge>}
            {canUpgrade && <Badge tone="info">{t.nodes.upgrade.badge}</Badge>}
          </>
        }
        subtitle={<span className="truncate font-mono">{n.address || n.id}</span>}
        aside={<NodeStatus n={n} />}
      />
      <CardBody>
        {n.pending ? (
          <div className="flex items-center justify-between gap-3">
            <span className="text-[13px] text-muted-foreground">{t.nodes.pendingHint}</span>
            <Button
              size="xs"
              variant="outline"
              onClick={async (e) => {
                e.preventDefault();
                if (await approve(n)) onApproved();
              }}
            >
              {t.nodes.approve}
            </Button>
          </div>
        ) : n.online ? (
          <NodeUsageBars usage={u} info={n.info} />
        ) : (
          <span className="text-[13px] text-muted-foreground">{t.nodes.lastSeen(timeAgo(toDate(n.lastSeen)))}</span>
        )}
      </CardBody>
      <CardFooter>
        <span>{t.nodes.containers(n.containers)}</span>
        <span className="truncate font-mono">{n.info ? `Docker ${n.info.dockerVersion || "?"}` : "—"}</span>
      </CardFooter>
    </Link>
  );
}

// NodePage：节点详情页。
export function NodePage({ nodeId, tab, onTab }: { nodeId: string; tab: NodeTab; onTab: (t: NodeTab) => void }) {
  const nodes = useNodeList();
  const panelVersion = usePanelVersion();
  const navigate = useNavigate();
  if (nodes.isPending) return <Loading />;
  if (nodes.error) return <Alert>{errorMessage(nodes.error)}</Alert>;
  const n = nodes.data.nodes.find((x) => x.id === nodeId);
  if (!n) {
    return (
      <>
        <Breadcrumb items={[{ label: t.nodes.title, to: "/nodes" }]} />
        <Alert>{t.nodes.notFound}</Alert>
      </>
    );
  }
  const target = upgradeTarget(n, panelVersion);
  const remove = async () => {
    if (!confirm(t.nodes.removeConfirm(n.name))) return;
    try {
      await nodeClient.removeNode({ nodeId: n.id });
      navigate({ to: "/nodes" });
    } catch (err) {
      alert(errorMessage(err));
    }
  };
  return (
    <>
      <Breadcrumb items={[{ label: t.nodes.title, to: "/nodes" }, { label: n.name }]} />
      <PageHeader
        title={
          <>
            {n.name}
            <NodeStatus n={n} />
            {n.info?.embedded && <Badge>{t.nodes.embedded}</Badge>}
          </>
        }
        meta={
          <>
            <span className="font-mono">{n.address || n.id}</span>
            {n.info && (
              <>
                <span>
                  {n.info.os}/{n.info.arch}
                </span>
                {nodeSpec(n.info) && <span>{nodeSpec(n.info)}</span>}
                <span className="font-mono">
                  Docker {n.info.dockerVersion || "?"} / Compose {n.info.composeVersion || "?"}
                </span>
                <span className="font-mono">Nagi {n.info.version || "?"}</span>
              </>
            )}
            <span>{t.nodes.containers(n.containers)}</span>
            {!n.online && <span>{t.nodes.lastSeen(timeAgo(toDate(n.lastSeen)))}</span>}
          </>
        }
        actions={
          <>
            {n.pending && (
              <Button
                onClick={async () => {
                  if (await approve(n)) nodes.refetch();
                }}
              >
                {t.nodes.approve}
              </Button>
            )}
            <Menu items={[{ label: t.nodes.removeNode, onSelect: remove, danger: true }]} />
          </>
        }
      />
      {target && <NodeUpgrade key={target} n={n} target={target} onDone={() => nodes.refetch()} />}
      {n.unknownContainers.length > 0 && <UnknownContainers nodeId={n.id} list={n.unknownContainers} onDone={() => nodes.refetch()} />}
      {n.pending ? (
        <Notice tone="warning" title={t.common.status.pendingApproval}>
          {t.nodes.pendingNotice}
        </Notice>
      ) : (
        <>
          <Tabs value={tab} onChange={onTab} items={nodeTabs} className="mb-6" />
          {!n.online ? (
            <Notice title={t.nodes.offlineTitle}>{t.nodes.offlineNotice}</Notice>
          ) : tab === "metrics" ? (
            <NodeMetrics nodeId={n.id} />
          ) : tab === "mesh" ? (
            <NodeMesh nodeId={n.id} />
          ) : tab === "replicas" ? (
            <NodeReplicas nodeId={n.id} />
          ) : tab === "settings" ? (
            <div className="grid gap-6">
              <NodeSettings nodeId={n.id} root={n.info?.projectsRoot || "/srv/nagi"} />
              <NodeMirrors nodeId={n.id} />
            </div>
          ) : (
            <NodePanels nodeId={n.id} />
          )}
        </>
      )}
    </>
  );
}

// NodeUpgrade：节点版本早于面板时的升级提示。节点重启后上报新版本，target 变为空，提示随之消失。
function NodeUpgrade({ n, target, onDone }: { n: Node; target: string; onDone: () => void }) {
  const [busy, setBusy] = useState(false);
  const [done, setDone] = useState(false);
  const [error, setError] = useState("");
  const upgrade = async () => {
    if (!confirm(t.nodes.upgrade.confirm(n.name, target))) return;
    setError("");
    setBusy(true);
    try {
      await nodeClient.upgradeNode({ nodeId: n.id });
      setDone(true);
      onDone();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };
  if (done) {
    return (
      <Notice tone="success" title={t.nodes.upgrade.restartingTitle(target)} className="mb-6">
        {t.nodes.upgrade.restartingText}
      </Notice>
    );
  }
  return (
    <Notice
      title={t.nodes.upgrade.title(target)}
      className="mb-6"
      actions={
        <Button size="sm" onClick={upgrade} disabled={busy}>
          {busy ? t.nodes.upgrade.running : t.nodes.upgrade.action}
        </Button>
      }
    >
      <p>{t.nodes.upgrade.text(n.info?.version || "?")}</p>
      {error && <Alert className="mt-2">{error}</Alert>}
    </Notice>
  );
}

function AddNode() {
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
  return (
    <div className="grid gap-4">
      {error && <Alert>{error}</Alert>}
      <Field label={t.nodes.add.publicAddress} help={t.nodes.add.publicAddressHelp} htmlFor="public-address">
        <div className="flex flex-wrap items-center gap-2">
          <Input id="public-address" className="w-56 font-mono" value={address ?? s?.publicAddress ?? ""} onChange={(e) => setAddress(e.target.value)} />
          <span className="font-mono text-[13px] text-muted-foreground">:{s?.port}</span>
          {address !== null && (
            <Button size="sm" variant="outline" onClick={saveAddress}>
              {t.common.actions.save}
            </Button>
          )}
        </div>
      </Field>
      {command && (
        <Field label={t.nodes.add.command} hint={t.nodes.add.commandHint} help={t.nodes.add.commandHelp}>
          <CodeBlock copy>{command}</CodeBlock>
        </Field>
      )}
      <FormActions className="border-t pt-4">
        <Button size="sm" onClick={create}>
          {command ? t.nodes.add.regenerate : t.nodes.add.generate}
        </Button>
      </FormActions>
    </div>
  );
}

// NodeSettings 是节点自己的设置：项目根目录（新放到这个节点的容器，目录默认是 <项目根目录>/<网络名>/<容器名>）。
function NodeSettings({ nodeId, root }: { nodeId: string; root: string }) {
  const queryClient = useQueryClient();
  const [value, setValue] = useState(root);
  const [error, setError] = useState("");
  const [saved, setSaved] = useState(false);
  const [busy, setBusy] = useState(false);
  const save = async () => {
    setBusy(true);
    setError("");
    setSaved(false);
    try {
      await nodeClient.setNodeProjectsRoot({ nodeId, root: value });
      setSaved(true);
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
        className="grid max-w-2xl gap-4"
        onSubmit={(e) => {
          e.preventDefault();
          save();
        }}
      >
        <Field label={t.nodes.settings.root} htmlFor={`root-${nodeId}`} hint={t.nodes.settings.rootHint} help={t.nodes.settings.rootHelp}>
          <Input id={`root-${nodeId}`} value={value} onChange={(e) => setValue(e.target.value)} className="font-mono" required />
        </Field>
        {error && <Alert>{error}</Alert>}
        <FormActions>
          {saved && <span className="text-[13px] text-muted-foreground">{t.common.feedback.saved}</span>}
          <Button type="submit" size="sm" disabled={busy || !value.trim()}>
            {busy && <Spinner />}
            {t.common.actions.save}
          </Button>
        </FormActions>
      </form>
    </Section>
  );
}

// NodeMirrors：节点的 Docker 镜像加速地址（见 docs/agent.md#镜像）。
function NodeMirrors({ nodeId }: { nodeId: string }) {
  const q = useQuery({ queryKey: ["node-mirrors", nodeId], queryFn: () => nodeClient.getNodeMirrors({ nodeId }) });
  return (
    <Section title={t.nodes.mirrors.title} description={t.nodes.mirrors.description} help={t.nodes.mirrors.help}>
      {q.isPending ? (
        <EmptyRow>
          <Spinner />
        </EmptyRow>
      ) : q.error ? (
        <Alert>{errorMessage(q.error)}</Alert>
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
  const [saved, setSaved] = useState(false);
  const [busy, setBusy] = useState(false);
  const save = async () => {
    setBusy(true);
    setError("");
    setSaved(false);
    try {
      const list = value
        .split("\n")
        .map((s) => s.trim())
        .filter(Boolean);
      const res = await nodeClient.setNodeMirrors({ nodeId, mirrors: list });
      queryClient.setQueryData(["node-mirrors", nodeId], res);
      setValue(res.mirrors.join("\n"));
      setSaved(true);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };
  // daemon.json 里的和 Docker 正在用的不一样：还没有重启 Docker。
  const pending = !sameMirrors(mirrors.mirrors, mirrors.active);
  return (
    <form
      className="grid max-w-2xl gap-4"
      onSubmit={(e) => {
        e.preventDefault();
        save();
      }}
    >
      {pending && (
        <Notice tone="warning" title={t.nodes.mirrors.pendingTitle}>
          <span className="break-all">{t.nodes.mirrors.pendingText(mirrors.active.join(", ") || t.nodes.mirrors.none)}</span>
        </Notice>
      )}
      <Field label={t.nodes.mirrors.label} htmlFor={`mirrors-${nodeId}`} hint={t.nodes.mirrors.hint}>
        <Textarea
          id={`mirrors-${nodeId}`}
          value={value}
          onChange={(e) => setValue(e.target.value)}
          placeholder="https://mirror.example.com"
          spellCheck={false}
          className="font-mono text-[13px]"
        />
      </Field>
      {error && <Alert>{error}</Alert>}
      <FormActions>
        {saved && <span className="text-[13px] text-muted-foreground">{t.common.feedback.saved}</span>}
        <Button type="submit" size="sm" disabled={busy}>
          {busy && <Spinner />}
          {t.common.actions.save}
        </Button>
      </FormActions>
    </form>
  );
}

// NodeReplicas：节点作为备份节点或热备保存的副本。已经没有容器使用的副本可以删除。
function NodeReplicas({ nodeId }: { nodeId: string }) {
  const queryClient = useQueryClient();
  const q = useQuery({ queryKey: ["node-replicas", nodeId], queryFn: () => nodeClient.listNodeReplicas({ nodeId }) });
  const [error, setError] = useState("");
  const remove = async (projectId: string, container: string, label: string) => {
    if (!confirm(t.nodes.replicas.deleteConfirm(label))) return;
    setError("");
    try {
      await nodeClient.deleteNodeReplica({ nodeId, projectId, container });
      queryClient.invalidateQueries({ queryKey: ["node-replicas", nodeId] });
    } catch (err) {
      setError(errorMessage(err));
    }
  };
  return (
    <Section flush title={t.nodes.replicas.title} description={t.nodes.replicas.intro}>
      {error && <Alert className="mb-3">{error}</Alert>}
      {q.isPending ? (
        <EmptyRow>
          <Spinner />
        </EmptyRow>
      ) : q.error ? (
        <Alert>{errorMessage(q.error)}</Alert>
      ) : q.data.replicas.length === 0 ? (
        <EmptyRow>{t.nodes.replicas.empty}</EmptyRow>
      ) : (
        <Table>
          <thead>
            <tr>
              <Th>{t.nodes.replicas.columns.container}</Th>
              <Th className="hidden sm:table-cell">{t.nodes.replicas.columns.source}</Th>
              <Th className="hidden sm:table-cell">{t.nodes.replicas.columns.size}</Th>
              <Th>{t.nodes.replicas.columns.lastSync}</Th>
              <Th />
            </tr>
          </thead>
          <tbody>
            {q.data.replicas.map(({ replica: r, inUse }) => {
              const label = `${r?.projectName || r?.projectId}/${r?.container}`;
              return (
                <tr key={`${r?.projectId}-${r?.container}`}>
                  <Td>
                    <div className="flex items-center gap-2">
                      <span className="font-medium">{label}</span>
                      {!inUse && <Badge tone="warning">{t.nodes.replicas.unused}</Badge>}
                    </div>
                  </Td>
                  <Td className="hidden text-[13px] text-muted-foreground sm:table-cell">{r?.sourceName || "—"}</Td>
                  <Td className="hidden font-mono text-xs whitespace-nowrap text-muted-foreground sm:table-cell">{fmtBytes(Number(r?.bytes ?? 0))}</Td>
                  <Td className="text-[13px] whitespace-nowrap text-muted-foreground">{r?.lastSync ? timeAgo(toDate(r.lastSync)) : "—"}</Td>
                  <Td className="text-right">
                    {!inUse && (
                      <Button size="xs" variant="ghost" onClick={() => remove(r!.projectId, r!.container, label)}>
                        {t.common.actions.delete}
                      </Button>
                    )}
                  </Td>
                </tr>
              );
            })}
          </tbody>
        </Table>
      )}
    </Section>
  );
}

// UnknownContainers：节点上有、面板却不认识的容器（所属项目是在面板上建的，所以不自动加进去，防止节点冒充）。
function UnknownContainers({ nodeId, list, onDone }: { nodeId: string; list: UnknownContainer[]; onDone: () => void }) {
  const [error, setError] = useState("");
  const adopt = async (u: UnknownContainer) => {
    if (!confirm(t.nodes.unknown.adoptConfirm(u.container, u.projectName))) return;
    setError("");
    try {
      await nodeClient.adoptNodeContainer({ nodeId, projectId: u.projectId, container: u.container });
      onDone();
    } catch (err) {
      setError(errorMessage(err));
    }
  };
  return (
    <Notice tone="warning" title={t.nodes.unknown.title} className="mb-6">
      <p>{t.nodes.unknown.text}</p>
      {error && <Alert className="mt-2">{error}</Alert>}
      <ul className="mt-2 grid gap-1.5">
        {list.map((u) => (
          <li key={u.projectId + "/" + u.container} className="flex flex-wrap items-center gap-3">
            <span className="font-mono text-[13px]">
              {u.projectName} / {u.container}
            </span>
            <Button size="xs" variant="outline" onClick={() => adopt(u)}>
              {t.nodes.unknown.adopt}
            </Button>
          </li>
        ))}
      </ul>
    </Notice>
  );
}
