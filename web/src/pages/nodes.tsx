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
import { Alert, Badge, Button, CodeBlock, Dialog, Empty, Field, Input, Loading, Menu, Notice, Status, Tabs } from "@/components/ui";
import type { Node, UnknownContainer } from "@/gen/nagi/v1/api_pb";
import type { NodeInfo } from "@/gen/nagi/v1/common_pb";

export type NodeTab = "metrics" | "panels" | "mesh" | "settings" | "replicas";

export const nodeTabs: { value: NodeTab; label: string }[] = [
  { value: "metrics", label: t.nodes.tabs.metrics },
  { value: "settings", label: t.nodes.tabs.settings },
  { value: "replicas", label: t.nodes.tabs.replicas },
  { value: "panels", label: t.nodes.tabs.panels },
  { value: "mesh", label: "Mesh" },
];

function useNodeList() {
  return useQuery({ queryKey: ["nodes"], queryFn: () => nodeClient.listNodes({}), refetchInterval: 5000 });
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

export function NodesPage() {
  const nodes = useNodeList();
  const [dialog, setDialog] = useState<"add" | "handover" | null>(null);
  const list = nodes.data?.nodes ?? [];

  return (
    <>
      <PageHeader
        title={t.nodes.title}
        description={t.nodes.description}
        actions={
          <>
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
      ) : (
        <div className="grid grid-cols-[repeat(auto-fill,minmax(16rem,1fr))] gap-3">
          {list.map((n) => (
            <NodeCard key={n.id} n={n} onApproved={() => nodes.refetch()} />
          ))}
        </div>
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

function NodeCard({ n, onApproved }: { n: Node; onApproved: () => void }) {
  const u = n.online && !n.pending ? n.usage : undefined;
  return (
    <Link to="/nodes/$nodeId" params={{ nodeId: n.id }} className="group flex flex-col rounded-lg border bg-card transition-colors hover:border-foreground/25">
      <div className="flex items-start justify-between gap-2 px-3.5 pt-3">
        <div className="min-w-0">
          <div className="flex items-center gap-2">
            <span className="truncate text-[13px] font-medium group-hover:underline">{n.name}</span>
            {n.info?.embedded && <Badge>{t.nodes.embedded}</Badge>}
          </div>
          <div className="mt-0.5 truncate font-mono text-xs text-muted-foreground">{n.address || n.id}</div>

        </div>
        <NodeStatus n={n} />
      </div>
      <div className="grid flex-1 content-start gap-1.5 px-3.5 py-2.5">
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
      </div>
      <div className="flex items-center justify-between gap-2 border-t px-3.5 py-2 text-xs text-muted-foreground">
        <span>{t.nodes.containers(n.containers)}</span>
        <span className="truncate font-mono">
          {n.info ? `Docker ${n.info.dockerVersion || "?"}` : "—"}
        </span>
      </div>
    </Link>
  );
}

// NodePage：节点详情页。
export function NodePage({ nodeId, tab, onTab }: { nodeId: string; tab: NodeTab; onTab: (t: NodeTab) => void }) {
  const nodes = useNodeList();
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
            <NodeSettings nodeId={n.id} root={n.info?.projectsRoot || "/srv/nagi"} />
          ) : (
            <NodePanels nodeId={n.id} />
          )}
        </>
      )}
    </>
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
      <div className="flex justify-end border-t pt-4">
        <Button size="sm" onClick={create}>
          {command ? t.nodes.add.regenerate : t.nodes.add.generate}
        </Button>
      </div>
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
    <form
      className="grid max-w-xl gap-4"
      onSubmit={(e) => {
        e.preventDefault();
        save();
      }}
    >
      <Field
        label={t.nodes.settings.root}
        htmlFor={`root-${nodeId}`}
        hint={t.nodes.settings.rootHint}
        help={t.nodes.settings.rootHelp}
      >
        <Input id={`root-${nodeId}`} value={value} onChange={(e) => setValue(e.target.value)} className="font-mono" required />
      </Field>
      {error && <Alert>{error}</Alert>}
      <div className="flex items-center gap-3">
        <Button type="submit" size="sm" disabled={busy || !value.trim()}>
          {t.common.actions.save}
        </Button>
        {saved && <span className="text-[13px] text-muted-foreground">{t.common.feedback.saved}</span>}
      </div>
    </form>
  );
}

// NodeReplicas：节点作为备份节点或热备保存的副本。已经没有容器使用的副本可以删除。
function NodeReplicas({ nodeId }: { nodeId: string }) {
  const queryClient = useQueryClient();
  const q = useQuery({ queryKey: ["node-replicas", nodeId], queryFn: () => nodeClient.listNodeReplicas({ nodeId }) });
  const [error, setError] = useState("");
  if (q.isPending) return <Loading />;
  if (q.error) return <Alert>{errorMessage(q.error)}</Alert>;
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
    <div className="grid gap-3">
      <p className="text-[13px] text-muted-foreground">{t.nodes.replicas.intro}</p>
      {error && <Alert>{error}</Alert>}
      <div className="divide-y border-y">
        {q.data.replicas.length === 0 && <p className="py-6 text-[13px] text-muted-foreground">{t.nodes.replicas.empty}</p>}
        {q.data.replicas.map(({ replica: r, inUse }) => {
          const label = `${r?.projectName || r?.projectId}/${r?.container}`;
          return (
            <div key={`${r?.projectId}-${r?.container}`} className="flex flex-wrap items-center gap-x-4 gap-y-1 py-3">
              <div className="min-w-0 flex-1">
                <div className="flex items-center gap-2 text-[13px]">
                  <span className="font-medium">{label}</span>
                  {!inUse && <Badge tone="warning">{t.nodes.replicas.unused}</Badge>}
                </div>
                <div className="text-xs text-muted-foreground">
                  {t.nodes.replicas.detail(
                    r?.sourceName || "—",
                    fmtBytes(Number(r?.bytes ?? 0)),
                    r?.lastSync ? timeAgo(toDate(r.lastSync)) : "—",
                  )}
                </div>
              </div>
              {!inUse && (
                <Button size="xs" variant="ghost" onClick={() => remove(r!.projectId, r!.container, label)}>
                  {t.common.actions.delete}
                </Button>
              )}
            </div>
          );
        })}
      </div>
    </div>
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
