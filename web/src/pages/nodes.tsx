import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate } from "@tanstack/react-router";
import { Plus, Server } from "lucide-react";
import { errorMessage, nodeClient, settingsClient, toDate } from "@/lib/api";
import { timeAgo } from "@/lib/utils";
import { Breadcrumb, PageHeader } from "@/pages/layout";
import { NodeMetrics, fmtBytes } from "@/components/metrics";
import { HandOver, NodePanels } from "@/pages/panels";
import { NodeMesh } from "@/pages/mesh";
import { Alert, Badge, Button, CodeBlock, Dialog, Empty, Field, Input, Loading, Menu, Meter, Notice, Status, Tabs } from "@/components/ui";
import type { Node, UnknownContainer } from "@/gen/nagi/v1/api_pb";

export type NodeTab = "metrics" | "panels" | "mesh" | "settings" | "replicas";

export const nodeTabs: { value: NodeTab; label: string }[] = [
  { value: "metrics", label: "监控" },
  { value: "settings", label: "设置" },
  { value: "replicas", label: "副本" },
  { value: "panels", label: "受信面板" },
  { value: "mesh", label: "Mesh" },
];

function useNodeList() {
  return useQuery({ queryKey: ["nodes"], queryFn: () => nodeClient.listNodes({}), refetchInterval: 5000 });
}

function NodeStatus({ n }: { n: Node }) {
  if (n.pending) return <Status tone="warning">待批准</Status>;
  if (n.online) return <Status tone="success">在线</Status>;
  return <Status>离线</Status>;
}

// approve 批准用接入码加入的节点。
async function approve(n: Node) {
  if (!confirm(`批准节点 ${n.name}（${n.address || n.id}）？批准后节点加入 mesh 并可由面板操作。该节点通过接入码加入，接入码泄露时他人也可加入，请确认是自有机器。`)) return false;
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
        title="节点"
        description="运行容器的机器，由节点主动连接面板；面板不可用时容器照常运行。"
        actions={
          <>
            <Button onClick={() => setDialog("add")}>
              <Plus />
              添加节点
            </Button>
            {list.length > 0 && <Menu items={[{ label: "移交给其他面板", onSelect: () => setDialog("handover") }]} />}
          </>
        }
      />
      {nodes.isPending ? (
        <Loading />
      ) : nodes.error ? (
        <Alert>{errorMessage(nodes.error)}</Alert>
      ) : list.length === 0 ? (
        <Empty title="暂无节点" icon={<Server className="size-5" />} action={<Button onClick={() => setDialog("add")}>添加节点</Button>}>
          生成安装命令，在新机器上执行后即接入本面板。
        </Empty>
      ) : (
        <div className="grid grid-cols-[repeat(auto-fill,minmax(16rem,1fr))] gap-3">
          {list.map((n) => (
            <NodeCard key={n.id} n={n} onApproved={() => nodes.refetch()} />
          ))}
        </div>
      )}
      <Dialog open={dialog === "add"} onClose={() => setDialog(null)} title="添加节点" description="生成安装命令，在新机器上执行后即接入本面板。">
        <AddNode />
      </Dialog>
      <Dialog open={dialog === "handover"} onClose={() => setDialog(null)} title="移交给其他面板" description="使用其他面板生成的接入码，将节点移交给该面板。">
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
            {n.info?.embedded && <Badge>面板内置</Badge>}
          </div>
          <div className="mt-0.5 truncate font-mono text-xs text-muted-foreground">{n.address || n.id}</div>
        </div>
        <NodeStatus n={n} />
      </div>
      <div className="grid flex-1 content-start gap-1.5 px-3.5 py-2.5">
        {n.pending ? (
          <div className="flex items-center justify-between gap-3">
            <span className="text-[13px] text-muted-foreground">通过接入码加入，批准后可用。</span>
            <Button
              size="xs"
              variant="outline"
              onClick={async (e) => {
                e.preventDefault();
                if (await approve(n)) onApproved();
              }}
            >
              批准
            </Button>
          </div>
        ) : n.online ? (
          <>
            <Meter label="CPU" value={u?.cpu} />
            <Meter label="内存" value={u?.memory} />
            <Meter label="磁盘" value={u?.disk} />
          </>
        ) : (
          <span className="text-[13px] text-muted-foreground">最后在线 {timeAgo(toDate(n.lastSeen))}</span>
        )}
      </div>
      <div className="flex items-center justify-between gap-2 border-t px-3.5 py-2 text-xs text-muted-foreground">
        <span>{n.containers} 个容器</span>
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
        <Breadcrumb items={[{ label: "节点", to: "/nodes" }]} />
        <Alert>节点不存在</Alert>
      </>
    );
  }
  const remove = async () => {
    if (!confirm(`移除节点 ${n.name}？面板将不再管理该节点并拒绝其重新连接，节点上的容器照常运行。`)) return;
    try {
      await nodeClient.removeNode({ nodeId: n.id });
      navigate({ to: "/nodes" });
    } catch (err) {
      alert(errorMessage(err));
    }
  };
  return (
    <>
      <Breadcrumb items={[{ label: "节点", to: "/nodes" }, { label: n.name }]} />
      <PageHeader
        title={
          <>
            {n.name}
            <NodeStatus n={n} />
            {n.info?.embedded && <Badge>面板内置</Badge>}
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
                <span className="font-mono">
                  Docker {n.info.dockerVersion || "?"} / Compose {n.info.composeVersion || "?"}
                </span>
                <span className="font-mono">Nagi {n.info.version || "?"}</span>
              </>
            )}
            <span>{n.containers} 个容器</span>
            {!n.online && <span>最后在线 {timeAgo(toDate(n.lastSeen))}</span>}
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
                批准
              </Button>
            )}
            <Menu items={[{ label: "移除节点", onSelect: remove, danger: true }]} />
          </>
        }
      />
      {n.unknownContainers.length > 0 && <UnknownContainers nodeId={n.id} list={n.unknownContainers} onDone={() => nodes.refetch()} />}
      {n.pending ? (
        <Notice tone="warning" title="待批准">
          该节点通过接入码加入。请确认为自有机器后批准；批准前无法查看和操作。
        </Notice>
      ) : (
        <>
          <Tabs value={tab} onChange={onTab} items={nodeTabs} className="mb-6" />
          {!n.online ? (
            <Notice title="节点离线">节点离线时无法查询监控、副本和 mesh 状态。</Notice>
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
      <Field label="面板对外地址" help="节点通过该地址连接面板，须可从节点访问。" htmlFor="public-address">
        <div className="flex flex-wrap items-center gap-2">
          <Input id="public-address" className="w-56 font-mono" value={address ?? s?.publicAddress ?? ""} onChange={(e) => setAddress(e.target.value)} />
          <span className="font-mono text-[13px] text-muted-foreground">:{s?.port}</span>
          {address !== null && (
            <Button size="sm" variant="outline" onClick={saveAddress}>
              保存
            </Button>
          )}
        </div>
      </Field>
      {command && (
        <Field label="安装命令" hint="以 root 身份在新机器上执行" help="令牌仅可使用一次：每台机器一条命令，有效期 1 小时。">
          <CodeBlock copy>{command}</CodeBlock>
        </Field>
      )}
      <div className="flex justify-end border-t pt-4">
        <Button size="sm" onClick={create}>
          {command ? "重新生成" : "生成安装命令"}
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
        label="项目根目录"
        htmlFor={`root-${nodeId}`}
        hint="绝对路径"
        help="新加入本节点的容器，默认目录为 <项目根目录>/<网络名>/<容器名>；已有容器的目录不变。"
      >
        <Input id={`root-${nodeId}`} value={value} onChange={(e) => setValue(e.target.value)} className="font-mono" required />
      </Field>
      {error && <Alert>{error}</Alert>}
      <div className="flex items-center gap-3">
        <Button type="submit" size="sm" disabled={busy || !value.trim()}>
          保存
        </Button>
        {saved && <span className="text-[13px] text-muted-foreground">已保存</span>}
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
    if (!confirm(`删除副本 ${label}？副本中的快照将被删除，且无法恢复。`)) return;
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
      <p className="text-[13px] text-muted-foreground">本节点作为备份节点或热备保存的容器副本。</p>
      {error && <Alert>{error}</Alert>}
      <div className="divide-y border-y">
        {q.data.replicas.length === 0 && <p className="py-6 text-[13px] text-muted-foreground">暂无副本</p>}
        {q.data.replicas.map(({ replica: r, inUse }) => {
          const label = `${r?.projectName || r?.projectId}/${r?.container}`;
          return (
            <div key={`${r?.projectId}-${r?.container}`} className="flex flex-wrap items-center gap-x-4 gap-y-1 py-3">
              <div className="min-w-0 flex-1">
                <div className="flex items-center gap-2 text-[13px]">
                  <span className="font-medium">{label}</span>
                  {!inUse && <Badge tone="warning">未使用</Badge>}
                </div>
                <div className="text-xs text-muted-foreground">
                  来自 {r?.sourceName || "—"} · {fmtBytes(Number(r?.bytes ?? 0))} · 最近同步 {r?.lastSync ? timeAgo(toDate(r.lastSync)) : "—"}
                </div>
              </div>
              {!inUse && (
                <Button size="xs" variant="ghost" onClick={() => remove(r!.projectId, r!.container, label)}>
                  删除
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
    if (!confirm(`将容器 ${u.container} 加入项目 ${u.projectName}？容器按节点缓存中的配置加入，所在节点为本节点。请确认该容器为自行部署。`)) return;
    setError("");
    try {
      await nodeClient.adoptNodeContainer({ nodeId, projectId: u.projectId, container: u.container });
      onDone();
    } catch (err) {
      setError(errorMessage(err));
    }
  };
  return (
    <Notice tone="warning" title="未识别的容器" className="mb-6">
      <p>以下容器的配置存在于节点上，但不在对应项目中（例如面板曾从备份恢复）。确认为自有容器后可加入项目；来源不明的容器请勿加入。</p>
      {error && <Alert className="mt-2">{error}</Alert>}
      <ul className="mt-2 grid gap-1.5">
        {list.map((u) => (
          <li key={u.projectId + "/" + u.container} className="flex flex-wrap items-center gap-3">
            <span className="font-mono text-[13px]">
              {u.projectName} / {u.container}
            </span>
            <Button size="xs" variant="outline" onClick={() => adopt(u)}>
              加入项目
            </Button>
          </li>
        ))}
      </ul>
    </Notice>
  );
}
