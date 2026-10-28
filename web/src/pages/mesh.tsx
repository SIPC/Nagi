import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { errorMessage, nodeClient, toDate } from "@/lib/api";
import type { MeshStatus } from "@/gen/nagi/v1/common_pb";
import { fmtBytes } from "@/components/metrics";
import { Alert, Button, Loading, Section, Status, Table, Td, Th } from "@/components/ui";
import { formatTime } from "@/lib/utils";
import { t } from "@/i18n";

// ---- mesh 状态 ----

const pathLabel: Record<string, string> = {
  direct: t.mesh.path.direct,
  link: t.mesh.path.link,
  "udp-relay": t.mesh.path.udpRelay,
  "tls-relay": t.mesh.path.tlsRelay,
  none: t.mesh.path.none,
};

// MeshSummary：本端在 mesh 中的地址、端口和直连地址。
function MeshSummary({ status }: { status: MeshStatus }) {
  return (
    <div className="flex flex-wrap items-center gap-x-5 gap-y-1 text-[13px] text-muted-foreground">
      <span>
        {t.mesh.address} <span className="font-mono text-foreground">{status.address}</span>
      </span>
      <span>
        {t.mesh.udpPort} <span className="font-mono text-foreground">{status.port}</span>
      </span>
      <span>
        {t.mesh.endpoints} <span className="font-mono text-foreground">{status.endpoints.join(", ") || "—"}</span>
      </span>
    </div>
  );
}

// MeshPeers：与各节点、面板之间的连接；nodeId 不为空时可以从该节点测试延迟。
function MeshPeers({ status, nodeId }: { status: MeshStatus; nodeId?: string }) {
  const [ping, setPing] = useState<Record<string, string>>({});
  return (
    <Table>
      <thead>
        <tr>
          <Th>{t.mesh.peer}</Th>
          <Th>{t.mesh.pathColumn}</Th>
          <Th>{t.mesh.lastHandshake}</Th>
          <Th>{t.mesh.rxTx}</Th>
          {nodeId && <Th />}
        </tr>
      </thead>
      <tbody>
        {status.peers.length === 0 && (
          <tr>
            <Td colSpan={nodeId ? 5 : 4} className="py-6 text-center text-[13px] text-muted-foreground">
              {t.mesh.noPeers}
            </Td>
          </tr>
        )}
        {status.peers.map((p) => {
          const key = p.address;
          return (
            <tr key={key}>
              <Td>
                <div className="font-medium">{p.name}</div>
                <div className="font-mono text-xs text-muted-foreground">{p.address}</div>
              </Td>
              <Td>
                <Status tone={p.path === "none" ? "neutral" : "success"}>{pathLabel[p.path] ?? p.path}</Status>
                <div className="font-mono text-xs text-muted-foreground">
                  {p.endpoint || p.via}
                  {p.path === "direct" && ` · ${p.rttMs.toFixed(1)} ms`}
                </div>
              </Td>
              <Td className="text-[13px] text-muted-foreground">{formatTime(toDate(p.lastHandshake)) ?? "—"}</Td>
              <Td className="font-mono text-xs whitespace-nowrap text-muted-foreground">
                {fmtBytes(Number(p.rxBytes))} / {fmtBytes(Number(p.txBytes))}
              </Td>
              {nodeId && (
                <Td className="text-right whitespace-nowrap">
                  {ping[key] && <span className="mr-2 font-mono text-xs text-muted-foreground">{ping[key]}</span>}
                  <Button
                    readOnlyOk
                    size="xs"
                    variant="ghost"
                    onClick={async () => {
                      setPing({ ...ping, [key]: "…" });
                      try {
                        const r = await nodeClient.pingNodeMesh({ nodeId, publicKey: p.publicKey });
                        setPing((x) => ({ ...x, [key]: `${r.rttMs.toFixed(1)} ms` }));
                      } catch (err) {
                        setPing((x) => ({ ...x, [key]: errorMessage(err) }));
                      }
                    }}
                  >
                    Ping
                  </Button>
                </Td>
              )}
            </tr>
          );
        })}
      </tbody>
    </Table>
  );
}

// NodeMesh：节点页里一个节点的 mesh 状态。
export function NodeMesh({ nodeId }: { nodeId: string }) {
  const q = useQuery({
    queryKey: ["node-mesh", nodeId],
    queryFn: () => nodeClient.getNodeMesh({ nodeId }),
    refetchInterval: 5000,
  });
  if (q.isPending) return <Loading />;
  if (q.error) return <Alert>{errorMessage(q.error)}</Alert>;
  return (
    <div className="grid gap-3">
      <MeshSummary status={q.data} />
      <div>
        <MeshPeers status={q.data} nodeId={nodeId} />
      </div>
    </div>
  );
}

// PanelMeshCard：设置页上面板自己的 mesh 状态。
export function PanelMeshCard() {
  const q = useQuery({
    queryKey: ["panel-mesh"],
    queryFn: () => nodeClient.getPanelMesh({}),
    refetchInterval: 5000,
  });
  return (
    <Section
      title={t.mesh.title}
      help={t.mesh.help}
      flush
    >
      {q.isPending ? (
        <Loading />
      ) : q.error ? (
        <div className="pb-4">
          <Alert>{errorMessage(q.error)}</Alert>
        </div>
      ) : (
        <>
          <div className="pb-3">
            <MeshSummary status={q.data} />
          </div>
          <MeshPeers status={q.data} />
        </>
      )}
    </Section>
  );
}
