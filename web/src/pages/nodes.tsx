import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { errorMessage, nodeClient, settingsClient, toDate } from "@/lib/api";
import { timeAgo } from "@/lib/utils";
import { PageHeader } from "@/pages/layout";
import { Alert, Badge, Button, Card, CardContent, CardHeader, CardTitle, Empty, Input, Spinner } from "@/components/ui";

export function NodesPage() {
  const nodes = useQuery({ queryKey: ["nodes"], queryFn: () => nodeClient.listNodes({}), refetchInterval: 5000 });

  return (
    <>
      <PageHeader title="节点" description="运行项目的机器。节点主动连接面板，面板挂了也不影响节点上的项目。" />
      <AddNode />
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
                  <th className="py-2 font-medium">Nagi</th>
                </tr>
              </thead>
              <tbody>
                {nodes.data.nodes.map((n) => (
                  <tr key={n.id} className="border-b last:border-0">
                    <td className="py-3 pr-4">
                      <div className="font-medium">{n.name}</div>
                      <div className="font-mono text-xs text-muted-foreground">{n.id}</div>
                    </td>
                    <td className="py-3 pr-4">
                      {n.online ? (
                        <Badge tone="success" dot>
                          在线
                        </Badge>
                      ) : (
                        <Badge dot>离线 · {timeAgo(toDate(n.lastSeen))}</Badge>
                      )}
                    </td>
                    <td className="py-3 pr-4 text-muted-foreground">
                      {n.info ? `${n.info.os}/${n.info.arch}` : "—"}
                    </td>
                    <td className="py-3 pr-4 text-muted-foreground">
                      {n.info ? `${n.info.dockerVersion || "?"} / ${n.info.composeVersion || "?"}` : "—"}
                    </td>
                    <td className="py-3 text-muted-foreground">{n.info?.version || "—"}</td>
                  </tr>
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
  const settings = useQuery({ queryKey: ["settings"], queryFn: () => settingsClient.getSettings({}) });
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
            <span className="text-muted-foreground">在新机器上用 root 执行（令牌一次性，1 小时内有效）：</span>
            <code className="break-all rounded bg-muted p-3 font-mono text-xs">{command}</code>
          </div>
        )}
      </CardContent>
    </Card>
  );
}