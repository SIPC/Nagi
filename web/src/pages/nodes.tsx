import { Fragment, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ChevronDown, ChevronRight } from "lucide-react";
import { errorMessage, nodeClient, settingsClient, toDate } from "@/lib/api";
import { timeAgo } from "@/lib/utils";
import { PageHeader } from "@/pages/layout";
import { NodeMetrics } from "@/components/metrics";
import { HandOverCard, NodePanels } from "@/pages/panels";
import { NodeMesh } from "@/pages/mesh";
import { NodeReplicas } from "@/pages/replicas";
import {
  Alert,
  Badge,
  Button,
  CodeBlock,
  EmptyRow,
  Field,
  HelpTip,
  Input,
  KeyValue,
  Loading,
  Notice,
  Section,
  Select,
  Status,
  Table,
  Tabs,
  Td,
  Th,
} from "@/components/ui";

type NodeTab = "metrics" | "panels" | "mesh" | "replicas" | "networks";

const nodeTabs: { value: NodeTab; label: string }[] = [
  { value: "metrics", label: "监控" },
  { value: "panels", label: "受信面板" },
  { value: "mesh", label: "Mesh" },
  { value: "replicas", label: "副本" },
  { value: "networks", label: "共享网络" },
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
      <PageHeader title="节点" description="运行项目的机器。节点主动连接面板，面板不可用时节点上的项目照常运行。" />
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
                                ) : open.tab === "networks" ? (
                                  <SharedNetworks nodeId={n.id} nodes={nodes.data.nodes.filter((o) => o.id !== n.id && o.online)} />
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

// 节点上的共享网络：加入的项目之间可以互相访问（容器在里面叫"容器.网络名"）。
// 可以和其他节点的同名网络连起来，对方的容器在本节点有一个同名的代理容器。
function SharedNetworks({ nodeId, nodes }: { nodeId: string; nodes: { id: string; name: string }[] }) {
  const queryClient = useQueryClient();
  const list = useQuery({
    queryKey: ["shared-networks", nodeId],
    queryFn: () => nodeClient.listNodeSharedNetworks({ nodeId }),
    refetchInterval: 10000,
  });
  const [name, setName] = useState("");
  const [peer, setPeer] = useState<Record<string, string>>({});
  const [error, setError] = useState("");
  const call = async (fn: () => Promise<unknown>) => {
    setError("");
    try {
      await fn();
      queryClient.invalidateQueries({ queryKey: ["shared-networks", nodeId] });
    } catch (err) {
      setError(errorMessage(err));
    }
  };
  return (
    <div className="grid gap-3">
      <p className="flex items-center gap-1.5 text-[13px] text-muted-foreground">
        加入同一共享网络的项目可以互相访问。
        <HelpTip>
          项目之间默认相互隔离。共享网络用于让多个项目互通，例如多个应用共用一个数据库项目：创建后在各项目的「项目设置」中加入，容器在网络内的名称为「容器.网络名」。与其他节点的同名网络连接后，不同节点上的项目也可以这样访问。
        </HelpTip>
      </p>
      {error && <Alert>{error}</Alert>}
      {list.data?.networks.length === 0 && (
        <p className="rounded-md border border-dashed px-4 py-6 text-center text-[13px] text-muted-foreground">本节点暂无共享网络</p>
      )}
      {list.data?.networks.map((n) => {
        const candidates = nodes.filter((o) => !n.peers.some((p) => p.nodeId === o.id));
        const items: [React.ReactNode, React.ReactNode][] = [
          [
            "加入的项目",
            n.projects.length ? n.projects.map((p) => `${p.projectName}（${p.networkName}）`).join("、") : <span className="text-muted-foreground">暂无</span>,
          ],
        ];
        if (n.peers.length > 0)
          items.push([
            "相连节点",
            <div className="flex flex-wrap items-center gap-2">
              {n.peers.map((p) => (
                <Badge key={p.name} tone={p.reachable ? "success" : "danger"} title={p.error}>
                  {p.name}
                  {!p.reachable && "（" + (p.error || "无法连接") + "）"}
                </Badge>
              ))}
              <Button
                variant="ghost"
                size="xs"
                onClick={() =>
                  confirm(
                    `断开共享网络 ${n.name} 与 ${n.peers.map((p) => p.name).join("、")} 的连接？断开后本节点上的项目与对方节点上的容器将无法互相访问。`,
                  ) && call(() => nodeClient.unlinkNodeSharedNetwork({ nodeId, name: n.name }))
                }
              >
                断开连接
              </Button>
            </div>,
          ]);
        if (n.remotes.length > 0)
          items.push([
            <span className="inline-flex items-center gap-1.5">
              远端容器
              <HelpTip>对方节点上的容器，本节点上有同名的代理容器。</HelpTip>
            </span>,
            n.remotes.map((r) => `${r.alias}（${r.nodeName}，${r.ports.length ? "端口 " + r.ports.join("、") : "无端口"}）`).join("、"),
          ]);
        if (nodes.length > 0)
          items.push([
            "连接节点",
            <div className="flex flex-wrap items-center gap-2">
              <Select className="h-8 w-48" value={peer[n.name] ?? ""} onChange={(e) => setPeer({ ...peer, [n.name]: e.target.value })}>
                <option value="">选择节点</option>
                {candidates.map((o) => (
                  <option key={o.id} value={o.id}>
                    {o.name}
                  </option>
                ))}
              </Select>
              <Button
                size="sm"
                variant="outline"
                disabled={!peer[n.name]}
                onClick={() => call(() => nodeClient.linkNodeSharedNetwork({ nodeId, name: n.name, peerNodeId: peer[n.name] }))}
              >
                连接同名网络
              </Button>
            </div>,
          ]);
        return (
          <div key={n.name} className="grid gap-3 rounded-md border px-4 py-3">
            <div className="flex flex-wrap items-center gap-3">
              <span className="font-mono text-[13px] font-medium">{n.name}</span>
              <span className="font-mono text-xs text-muted-foreground">{n.subnet}</span>
              <Button
                variant="ghost"
                size="xs"
                className="ml-auto"
                onClick={() => call(() => nodeClient.deleteNodeSharedNetwork({ nodeId, name: n.name }))}
              >
                删除
              </Button>
            </div>
            <KeyValue items={items} className="items-center" />
          </div>
        );
      })}
      <form
        className="flex flex-wrap gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          call(async () => {
            await nodeClient.createNodeSharedNetwork({ nodeId, name });
            setName("");
          });
        }}
      >
        <Input value={name} onChange={(e) => setName(e.target.value)} placeholder="网络名称，如 db-shared" className="h-8 max-w-60 font-mono" />
        <Button size="sm" type="submit">
          创建共享网络
        </Button>
      </form>
    </div>
  );
}
