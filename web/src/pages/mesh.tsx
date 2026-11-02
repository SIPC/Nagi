import { useState } from "react";
import { useQuery, type UseQueryResult } from "@tanstack/react-query";
import { Code, ConnectError } from "@connectrpc/connect";
import { errorMessage, nodeClient, toDate } from "@/lib/api";
import type { MeshPeerStatus, MeshStatus } from "@/gen/nagi/v1/common_pb";
import { fmtBytes } from "@/components/metrics";
import { Button, EmptyRow, List, QueryError, Roll, Row, RowsSkeleton, Section } from "@/components/ui";
import { cn, timeAgo } from "@/lib/utils";
import { t } from "@/i18n";

// ---- mesh 状态 ----

export const pathLabel: Record<string, string> = {
  direct: t.mesh.path.direct,
  link: t.mesh.path.link,
  "udp-relay": t.mesh.path.udpRelay,
  "tls-relay": t.mesh.path.tlsRelay,
  none: t.mesh.path.none,
};

// pathShort：列表第二行的路径。完整的路径（UDP、TLS）在悬停提示里。
const pathShort: Record<string, string> = {
  direct: t.mesh.pathShort.direct,
  link: t.mesh.path.link,
  "udp-relay": t.mesh.pathShort.relay,
  "tls-relay": t.mesh.pathShort.relay,
  none: t.mesh.path.none,
};

// pathClass：路径文字的颜色，和组网图的连线一致：直连灰，中转黄，不可达红。
function pathClass(path: string) {
  if (path === "direct" || path === "link") return undefined;
  return path === "udp-relay" || path === "tls-relay" ? "text-warning-text" : "text-destructive";
}

// meshSummary：本端在 mesh 中的地址、端口和直连地址，写在区块标题右边。标题旁只写第一个直连地址（面板不写），全部在悬停提示里。
function meshSummary(status: MeshStatus, endpoints: string[]) {
  return [status.address, `UDP ${status.port}`, endpoints.length > 0 && `${t.mesh.endpoints} ${endpoints.join(", ")}`].filter(Boolean).join(" · ");
}

type Ping = { text: string; failed?: boolean; detail?: string };

function pingFailure(err: unknown): Ping {
  const detail = errorMessage(err);
  if (err instanceof ConnectError && err.code === Code.DeadlineExceeded) return { text: t.mesh.pingTimeout, failed: true };
  return { text: t.mesh.pingFailed, failed: true, detail };
}

// PeerSub：第二行写路径和最近握手；Ping 失败的原因也写在这里，定宽的列放不下。完整的地址在 title。
function PeerSub({ peer: p, ping }: { peer: MeshPeerStatus; ping?: Ping }) {
  const handshake = toDate(p.lastHandshake);
  const where = [pathLabel[p.path] ?? p.path, p.path === "direct" && `${p.rttMs.toFixed(1)} ms`, p.address, p.endpoint || p.via].filter(Boolean).join(" · ");
  const path = pathShort[p.path] ?? p.path;
  const color = pathClass(p.path);
  const rest = handshake ? t.mesh.lastHandshakeAgo(timeAgo(handshake)) : t.mesh.noHandshake;
  return (
    <span title={where}>
      {color ? (
        <>
          <span className={color}>{path}</span>
          {" · "}
          <Roll value={rest} />
        </>
      ) : (
        <>
          {`${path} · `}
          <Roll value={rest} />
        </>
      )}
      {ping?.detail && (
        <>
          {" · "}
          <span className="text-destructive">{ping.detail}</span>
        </>
      )}
    </span>
  );
}

// MeshPeers：与各节点、面板之间的连接；nodeId 不为空时可以从该节点测试延迟。
function MeshPeers({ status, nodeId }: { status: MeshStatus; nodeId?: string }) {
  const [ping, setPing] = useState<Record<string, Ping>>({});
  // 测过之后才多一列结果，平时不留空列。
  const pinged = Object.keys(ping).length > 0;
  if (status.peers.length === 0) return <EmptyRow>{t.mesh.noPeers}</EmptyRow>;
  return (
    <List>
      {status.peers.map((p) => {
        const key = p.address;
        const result = ping[key];
        return (
          <Row
            key={key}
            title={<span className="truncate">{p.name}</span>}
            sub={<PeerSub peer={p} ping={result} />}
            cols={[
              { content: <span title={t.mesh.rxTx}>{`↓ ${fmtBytes(Number(p.rxBytes))}`}</span>, num: true },
              { content: <span title={t.mesh.rxTx}>{`↑ ${fmtBytes(Number(p.txBytes))}`}</span> },
              ...(nodeId && pinged ? [{ content: result && <span className={cn(result.failed && "text-destructive")}>{result.text}</span>, num: true, phone: true }] : []),
            ]}
            actions={
              nodeId ? (
                <Button
                  readOnlyOk
                  size="row"
                  variant="ghost"
                  onClick={async () => {
                    setPing((x) => ({ ...x, [key]: { text: "…" } }));
                    try {
                      const r = await nodeClient.pingNodeMesh({ nodeId, publicKey: p.publicKey });
                      setPing((x) => ({ ...x, [key]: { text: `${r.rttMs.toFixed(1)} ms` } }));
                    } catch (err) {
                      setPing((x) => ({ ...x, [key]: pingFailure(err) }));
                    }
                  }}
                >
                  Ping
                </Button>
              ) : undefined
            }
          />
        );
      })}
    </List>
  );
}

// MeshSection：一端（节点或面板）的 mesh 状态区块。
function MeshSection({ q, nodeId, title }: { q: UseQueryResult<MeshStatus>; nodeId?: string; title: string }) {
  return (
    <Section title={title} actions={q.data && <span title={meshSummary(q.data, q.data.endpoints)}>{meshSummary(q.data, nodeId ? q.data.endpoints.slice(0, 1) : [])}</span>}>
      {q.isPending ? <RowsSkeleton /> : q.error ? <QueryError error={errorMessage(q.error)} onRetry={() => q.refetch()} /> : <MeshPeers status={q.data} nodeId={nodeId} />}
    </Section>
  );
}

// NodeMesh：节点页里一个节点的 mesh 状态。
export function NodeMesh({ nodeId }: { nodeId: string }) {
  const q = useQuery({
    queryKey: ["node-mesh", nodeId],
    queryFn: () => nodeClient.getNodeMesh({ nodeId }),
    refetchInterval: 5000,
  });
  return <MeshSection q={q} nodeId={nodeId} title={t.mesh.title} />;
}

// PanelMeshCard：设置页上面板自己的 mesh 状态。
export function PanelMeshCard() {
  const q = useQuery({
    queryKey: ["panel-mesh"],
    queryFn: () => nodeClient.getPanelMesh({}),
    refetchInterval: 5000,
  });
  return <MeshSection q={q} title={t.settings.tabs.mesh.label} />;
}
