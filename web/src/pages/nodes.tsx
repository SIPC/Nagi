import { Fragment, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { errorMessage, nodeClient, settingsClient, toDate } from "@/lib/api";
import { timeAgo } from "@/lib/utils";
import { PageHeader } from "@/pages/layout";
import { AlertSettings, NodeMetrics } from "@/pages/alerts";
import { HandOverCard, InviteCard, NodePanels } from "@/pages/panels";
import { NodeMesh, PanelMeshCard } from "@/pages/mesh";
import { NodeReplicas } from "@/pages/backup";
import { SecurityCard } from "@/pages/security";
import {
  Alert,
  Badge,
  Button,
  Card,
  CardContent,
  CardHeader,
  CardTitle,
  Empty,
  Input,
  Spinner,
} from "@/components/ui";

export function NodesPage() {
  const nodes = useQuery({
    queryKey: ["nodes"],
    queryFn: () => nodeClient.listNodes({}),
    refetchInterval: 5000,
  });
  const [open, setOpen] = useState<{
    id: string;
    tab: "metrics" | "panels" | "mesh" | "replicas" | "networks";
  } | null>(null);
  const toggle = (
    id: string,
    tab: "metrics" | "panels" | "mesh" | "replicas" | "networks",
  ) => setOpen(open?.id === id && open.tab === tab ? null : { id, tab });

  return (
    <>
      <PageHeader
        title="节点"
        description="运行项目的机器。节点主动连接面板，面板挂了也不影响节点上的项目。"
      />
      <AddNode />
      <SecurityCard />
      <AlertSettings />
      <PanelMeshCard />
      <InviteCard />
      {nodes.data && <HandOverCard nodes={nodes.data.nodes} />}
      {nodes.isPending ? (
        <Spinner />
      ) : nodes.error ? (
        <Alert>{errorMessage(nodes.error)}</Alert>
      ) : nodes.data.nodes.length === 0 ? (
        <Empty title="还没有节点" />
      ) : (
        <Card>
          <CardContent className="overflow-x-auto pt-2">
            <table className="w-full text-sm">
              <thead className="text-left text-xs text-muted-foreground">
                <tr className="border-b">
                  <th className="py-2 pr-4 font-medium">名称</th>
                  <th className="py-2 pr-4 font-medium">状态</th>
                  <th className="py-2 pr-4 font-medium">系统</th>
                  <th className="py-2 pr-4 font-medium">Docker / Compose</th>
                  <th className="py-2 pr-4 font-medium">Nagi</th>
                  <th className="py-2 font-medium" />
                </tr>
              </thead>
              <tbody>
                {nodes.data.nodes.map((n) => (
                  <Fragment key={n.id}>
                    <tr className="border-b last:border-0">
                      <td className="py-3 pr-4">
                        <div className="font-medium">{n.name}</div>
                        <div className="font-mono text-xs text-muted-foreground">
                          {n.id}
                        </div>
                      </td>
                      <td className="py-3 pr-4">
                        {n.online ? (
                          <Badge tone="success" dot>
                            在线
                          </Badge>
                        ) : (
                          <Badge dot>
                            离线 · {timeAgo(toDate(n.lastSeen))}
                          </Badge>
                        )}
                      </td>
                      <td className="py-3 pr-4 text-muted-foreground">
                        {n.info ? `${n.info.os}/${n.info.arch}` : "—"}
                      </td>
                      <td className="py-3 pr-4 text-muted-foreground">
                        {n.info
                          ? `${n.info.dockerVersion || "?"} / ${n.info.composeVersion || "?"}`
                          : "—"}
                      </td>
                      <td className="py-3 pr-4 text-muted-foreground">
                        {n.info?.version || "—"}
                      </td>
                      <td className="py-3 text-right">
                        <Button
                          variant="ghost"
                          size="sm"
                          onClick={() => toggle(n.id, "metrics")}
                        >
                          监控
                        </Button>
                        <Button
                          variant="ghost"
                          size="sm"
                          onClick={() => toggle(n.id, "panels")}
                        >
                          面板
                        </Button>
                        <Button
                          variant="ghost"
                          size="sm"
                          onClick={() => toggle(n.id, "mesh")}
                        >
                          mesh
                        </Button>
                        <Button
                          variant="ghost"
                          size="sm"
                          onClick={() => toggle(n.id, "replicas")}
                        >
                          副本
                        </Button>
                        <Button
                          variant="ghost"
                          size="sm"
                          onClick={() => toggle(n.id, "networks")}
                        >
                          共享网络
                        </Button>
                        <Button
                          variant="ghost"
                          size="sm"
                          onClick={async () => {
                            if (
                              !confirm(
                                `移除节点 ${n.name}？面板会忘记它并拒绝它再连接，节点上的项目照常运行。`,
                              )
                            )
                              return;
                            await nodeClient.removeNode({ nodeId: n.id });
                            nodes.refetch();
                          }}
                        >
                          移除
                        </Button>
                      </td>
                    </tr>
                    {open?.id === n.id && (
                      <tr className="border-b">
                        <td colSpan={6}>
                          {n.online ? (
                            open.tab === "metrics" ? (
                              <NodeMetrics nodeId={n.id} />
                            ) : open.tab === "mesh" ? (
                              <NodeMesh nodeId={n.id} />
                            ) : open.tab === "replicas" ? (
                              <NodeReplicas nodeId={n.id} />
                            ) : open.tab === "networks" ? (
                              <SharedNetworks nodeId={n.id} />
                            ) : (
                              <NodePanels nodeId={n.id} />
                            )
                          ) : (
                            "节点离线，无法查询"
                          )}
                        </td>
                      </tr>
                    )}
                  </Fragment>
                ))}
              </tbody>
            </table>
          </CardContent>
        </Card>
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
    <Card className="mb-6">
      <CardHeader>
        <CardTitle>添加节点</CardTitle>
      </CardHeader>
      <CardContent className="grid gap-3 text-sm">
        {error && <Alert>{error}</Alert>}
        <div className="flex flex-wrap items-center gap-2">
          <span>面板对外地址（节点必须能访问到）：</span>
          <Input
            className="w-56 font-mono"
            value={address ?? s?.publicAddress ?? ""}
            onChange={(e) => setAddress(e.target.value)}
          />
          <span className="font-mono text-muted-foreground">:{s?.port}</span>
          {address !== null && (
            <Button size="sm" variant="outline" onClick={saveAddress}>
              保存
            </Button>
          )}
        </div>
        <div>
          <Button size="sm" onClick={create}>
            生成安装命令
          </Button>
        </div>
        {command && (
          <div className="grid gap-1">
            <span className="text-muted-foreground">
              在新机器上用 root 执行（令牌一次性，1 小时内有效）：
            </span>
            <code className="break-all rounded bg-muted p-3 font-mono text-xs">
              {command}
            </code>
          </div>
        )}
      </CardContent>
    </Card>
  );
}

// 节点上的共享网络：加入的项目之间可以互相访问（成员在里面叫"成员.网络名"）。只在本节点存在。
function SharedNetworks({ nodeId }: { nodeId: string }) {
  const queryClient = useQueryClient();
  const list = useQuery({ queryKey: ["shared-networks", nodeId], queryFn: () => nodeClient.listNodeSharedNetworks({ nodeId }) });
  const [name, setName] = useState("");
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
    <div className="grid gap-3 py-3 text-sm">
      {error && <Alert>{error}</Alert>}
      {list.data?.networks.length === 0 && <span className="text-muted-foreground">还没有共享网络。</span>}
      {list.data?.networks.map((n) => (
        <div key={n.name} className="flex items-center gap-3">
          <span className="font-mono">{n.name}</span>
          <span className="text-xs text-muted-foreground">
            {n.subnet} · {n.projects.map((p) => `${p.projectName}（${p.networkName}）`).join("、") || "还没有项目加入"}
          </span>
          <Button variant="ghost" size="sm" onClick={() => call(() => nodeClient.deleteNodeSharedNetwork({ nodeId, name: n.name }))}>
            删除
          </Button>
        </div>
      ))}
      <div className="flex gap-2">
        <Input value={name} onChange={(e) => setName(e.target.value)} placeholder="网络名，例如 db-shared" className="max-w-60 font-mono" />
        <Button
          size="sm"
          onClick={() =>
            call(async () => {
              await nodeClient.createNodeSharedNetwork({ nodeId, name });
              setName("");
            })
          }
        >
          创建共享网络
        </Button>
      </div>
    </div>
  );
}
