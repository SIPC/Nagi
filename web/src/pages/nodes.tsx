import { Fragment, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ChevronDown, ChevronRight } from "lucide-react";
import { errorMessage, nodeClient, settingsClient, toDate } from "@/lib/api";
import { timeAgo } from "@/lib/utils";
import { PageHeader } from "@/pages/layout";
import { NodeMetrics, fmtBytes } from "@/components/metrics";
import { HandOverCard, NodePanels } from "@/pages/panels";
import { NodeMesh } from "@/pages/mesh";
import {
  Alert,
  Badge,
  Button,
  CodeBlock,
  EmptyRow,
  Field,
  Input,
  Loading,
  Notice,
  Section,
  Status,
  Table,
  Tabs,
  Td,
  Th,
} from "@/components/ui";

type NodeTab = "metrics" | "panels" | "mesh" | "settings" | "replicas";

const nodeTabs: { value: NodeTab; label: string }[] = [
  { value: "metrics", label: "监控" },
  { value: "settings", label: "设置" },
  { value: "replicas", label: "副本" },
  { value: "panels", label: "受信面板" },
  { value: "mesh", label: "Mesh" },
];

export function NodesPage() {
  const nodes = useQuery({
    queryKey: ["nodes"],
    queryFn: () => nodeClient.listNodes({}),
    refetchInterval: 5000,
  });
  const [open, setOpen] = useState<{ id: string; tab: NodeTab } | null>(null);
  const expand = (id: string) => setOpen(open?.id === id ? null : { id, tab: "metrics" });

  return (
    <>
      <PageHeader title="节点" description="运行容器的机器。节点主动连接面板，面板不可用时节点上的容器照常运行。" />
      <div className="grid gap-6">
        <Section title="全部节点" flush>
          {nodes.isPending ? (
            <Loading />
          ) : nodes.error ? (
            <div className="px-5 pb-4">
              <Alert>{errorMessage(nodes.error)}</Alert>
            </div>
          ) : nodes.data.nodes.length === 0 ? (
            <EmptyRow>暂无节点，可通过下方的安装命令添加。</EmptyRow>
          ) : (
            <Table>
              <thead>
                <tr>
                  <Th>名称</Th>
                  <Th>状态</Th>
                  <Th>系统</Th>
                  <Th>Docker / Compose</Th>
                  <Th>Nagi</Th>
                  <Th />
                </tr>
              </thead>
              <tbody>
                {nodes.data.nodes.map((n) => {
                  const expanded = open?.id === n.id;
                  return (
                    <Fragment key={n.id}>
                      <tr>
                        <Td>
                          <div className="flex items-center gap-2 font-medium">
                            {n.name}
                            {n.info?.embedded && <Badge>面板内置</Badge>}
                            {n.pending && <Badge tone="warning">待批准</Badge>}
                          </div>
                          <div className="font-mono text-xs text-muted-foreground">{n.address || n.id}</div>
                        </Td>
                        <Td>
                          {n.online ? (
                            <Status tone="success">在线</Status>
                          ) : (
                            <div className="flex flex-col">
                              <Status>离线</Status>
                              <span className="text-xs text-muted-foreground">{timeAgo(toDate(n.lastSeen))}</span>
                            </div>
                          )}
                        </Td>
                        <Td className="text-[13px] text-muted-foreground">{n.info ? `${n.info.os}/${n.info.arch}` : "—"}</Td>
                        <Td className="font-mono text-xs text-muted-foreground">
                          {n.info ? `${n.info.dockerVersion || "?"} / ${n.info.composeVersion || "?"}` : "—"}
                        </Td>
                        <Td className="font-mono text-xs text-muted-foreground">{n.info?.version || "—"}</Td>
                        <Td className="text-right whitespace-nowrap">
                          <div className="inline-flex items-center gap-1">
                            {n.pending && (
                              <Button
                                size="xs"
                                variant="outline"
                                title="该节点通过接入码加入。批准前不加入 mesh、不接收告警配置，也无法操作。"
                                onClick={async () => {
                                  if (
                                    !confirm(
                                      `批准节点 ${n.name}（${n.address || n.id}）？批准后节点加入 mesh 并可由面板操作。该节点通过接入码加入，接入码泄露时他人也可加入，请确认是自有机器。`,
                                    )
                                  )
                                    return;
                                  await nodeClient.approveNode({ nodeId: n.id });
                                  nodes.refetch();
                                }}
                              >
                                批准
                              </Button>
                            )}
                            <Button size="xs" variant="ghost" aria-expanded={expanded} onClick={() => expand(n.id)}>
                              详情
                              {expanded ? <ChevronDown /> : <ChevronRight />}
                            </Button>
                            <Button
                              size="xs"
                              variant="ghost"
                              onClick={async () => {
                                if (!confirm(`移除节点 ${n.name}？面板将不再管理该节点并拒绝其重新连接，节点上的项目照常运行。`)) return;
                                await nodeClient.removeNode({ nodeId: n.id });
                                nodes.refetch();
                              }}
                            >
                              移除
                            </Button>
                          </div>
                        </Td>
                      </tr>
                      {expanded && (
                        <tr>
                          <Td colSpan={6} className="h-auto py-0">
                            {n.pending ? (
                              <div className="py-4">
                                <Notice tone="warning" title="尚未批准">
                                  该节点通过接入码加入，尚未批准。确认是自有机器后批准，批准后才能查看和操作。
                                </Notice>
                              </div>
                            ) : !n.online ? (
                              <p className="py-6 text-center text-[13px] text-muted-foreground">节点离线，无法查询</p>
                            ) : (
                              <div className="pb-5">
                                <Tabs value={open.tab} onChange={(tab) => setOpen({ id: n.id, tab })} items={nodeTabs} className="mb-4" />
                                {open.tab === "metrics" ? (
                                  <NodeMetrics nodeId={n.id} />
                                ) : open.tab === "mesh" ? (
                                  <NodeMesh nodeId={n.id} />
                                ) : open.tab === "replicas" ? (
                                  <NodeReplicas nodeId={n.id} />
                                ) : open.tab === "settings" ? (
                                  <NodeSettings nodeId={n.id} root={n.info?.projectsRoot || "/srv/nagi"} />
                                ) : (
                                  <NodePanels nodeId={n.id} />
                                )}
                              </div>
                            )}
                          </Td>
                        </tr>
                      )}
                    </Fragment>
                  );
                })}
              </tbody>
            </Table>
          )}
        </Section>
        <AddNode />
        {nodes.data && nodes.data.nodes.length > 0 && <HandOverCard nodes={nodes.data.nodes} />}
      </div>
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
    <Section
      title="添加节点"
      description="生成安装命令，在新机器上执行后即接入本面板。"
      actions={
        <Button size="sm" onClick={create}>
          生成安装命令
        </Button>
      }
    >
      <div className="grid gap-4">
        {error && <Alert>{error}</Alert>}
        <Field label="面板对外地址" help="节点通过该地址连接面板，必须能从节点访问。" htmlFor="public-address">
          <div className="flex flex-wrap items-center gap-2">
            <Input
              id="public-address"
              className="w-56 font-mono"
              value={address ?? s?.publicAddress ?? ""}
              onChange={(e) => setAddress(e.target.value)}
            />
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
      </div>
    </Section>
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
        help="新放到这个节点的容器，目录默认是 <项目根目录>/<网络名>/<容器名>。已有容器的目录不变。"
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
    if (!confirm(`删除副本 ${label}？副本里的快照将被删除，无法恢复。`)) return;
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
      <div className="divide-y rounded-md border">
        {q.data.replicas.length === 0 && <p className="px-4 py-6 text-center text-[13px] text-muted-foreground">暂无副本</p>}
        {q.data.replicas.map(({ replica: r, inUse }) => {
          const label = `${r?.projectName || r?.projectId}/${r?.container}`;
          return (
            <div key={`${r?.projectId}-${r?.container}`} className="flex flex-wrap items-center gap-x-4 gap-y-1 px-4 py-2.5">
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

