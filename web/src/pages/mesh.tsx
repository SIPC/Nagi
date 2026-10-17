import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { errorMessage, nodeClient, toDate } from "@/lib/api";
import type { MeshStatus } from "@/gen/nagi/v1/common_pb";
import {
  Alert,
  Badge,
  Button,
  Card,
  CardContent,
  CardHeader,
  CardTitle,
  Spinner,
} from "@/components/ui";

// ---- mesh 状态（临时测试界面）----

const pathLabel: Record<string, string> = {
  direct: "UDP 直连",
  link: "控制连接",
  "udp-relay": "UDP 经面板中转",
  "tls-relay": "TLS 经面板中转",
  none: "不通",
};

function MeshTable({
  status,
  nodeId,
}: {
  status: MeshStatus;
  nodeId?: string;
}) {
  const [ping, setPing] = useState<Record<string, string>>({});
  return (
    <div className="grid gap-2 text-xs">
      <div className="text-muted-foreground">
        地址 <span className="font-mono">{status.address}</span> · UDP{" "}
        {status.port} · 直连地址{" "}
        <span className="font-mono">{status.endpoints.join(", ") || "—"}</span>
      </div>
      <table className="w-full">
        <thead className="text-left text-muted-foreground">
          <tr>
            <th className="py-1 pr-3 font-medium">成员</th>
            <th className="py-1 pr-3 font-medium">路径</th>
            <th className="py-1 pr-3 font-medium">握手</th>
            <th className="py-1 pr-3 font-medium">收 / 发</th>
            <th />
          </tr>
        </thead>
        <tbody>
          {status.peers.map((p) => {
            const key = p.address;
            return (
              <tr key={key}>
                <td className="py-1 pr-3">
                  <div>{p.name}</div>
                  <div className="font-mono text-muted-foreground">
                    {p.address}
                  </div>
                </td>
                <td className="py-1 pr-3">
                  <Badge tone={p.path === "none" ? undefined : "success"}>
                    {pathLabel[p.path] ?? p.path}
                  </Badge>
                  <div className="font-mono text-muted-foreground">
                    {p.endpoint || p.via}
                    {p.path === "direct" && ` · ${p.rttMs.toFixed(1)} ms`}
                  </div>
                </td>
                <td className="py-1 pr-3">
                  {toDate(p.lastHandshake)?.toLocaleTimeString() ?? "—"}
                </td>
                <td className="py-1 pr-3 font-mono">
                  {p.rxBytes.toString()} / {p.txBytes.toString()}
                </td>
                <td className="py-1 text-right">
                  {nodeId && (
                    <>
                      <span className="mr-2">{ping[key]}</span>
                      <Button
                        size="sm"
                        variant="ghost"
                        onClick={async () => {
                          setPing({ ...ping, [key]: "…" });
                          try {
                            const r = await nodeClient.pingNodeMesh({
                              nodeId,
                              publicKey: p.publicKey,
                            });
                            setPing((x) => ({
                              ...x,
                              [key]: `${r.rttMs.toFixed(1)} ms`,
                            }));
                          } catch (err) {
                            setPing((x) => ({
                              ...x,
                              [key]: errorMessage(err),
                            }));
                          }
                        }}
                      >
                        Ping
                      </Button>
                    </>
                  )}
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

export function NodeMesh({ nodeId }: { nodeId: string }) {
  const q = useQuery({
    queryKey: ["node-mesh", nodeId],
    queryFn: () => nodeClient.getNodeMesh({ nodeId }),
    refetchInterval: 5000,
  });
  if (q.isPending) return <Spinner />;
  if (q.error) return <Alert>{errorMessage(q.error)}</Alert>;
  return (
    <div className="py-2">
      <MeshTable status={q.data} nodeId={nodeId} />
    </div>
  );
}

export function PanelMeshCard() {
  const q = useQuery({
    queryKey: ["panel-mesh"],
    queryFn: () => nodeClient.getPanelMesh({}),
    refetchInterval: 5000,
  });
  return (
    <Card className="mb-6">
      <CardHeader>
        <CardTitle>面板的 mesh</CardTitle>
      </CardHeader>
      <CardContent>
        {q.isPending ? (
          <Spinner />
        ) : q.error ? (
          <Alert>{errorMessage(q.error)}</Alert>
        ) : (
          <MeshTable status={q.data} />
        )}
      </CardContent>
    </Card>
  );
}
