import { useState } from "react";
import { useQuery, useQueryClient, type UseQueryResult } from "@tanstack/react-query";
import { toast } from "sonner";
import { confirmAction } from "@/components/confirm";
import { Code, ConnectError } from "@connectrpc/connect";
import { errorMessage, nodeClient, toDate } from "@/lib/api";
import type { MeshPeerStatus, MeshSpeedResponse, MeshStatus } from "@/gen/nagi/v1/common_pb";
import { fmtBytes } from "@/components/metrics";
import { Button, EmptyRow, List, QueryError, Roll, Row, RowsSkeleton, Section, Spinner } from "@/components/ui";
import { useRole } from "@/components/role";
import { cn, timeAgo } from "@/lib/utils";
import { locale, t } from "@/i18n";

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

// Speed：测速结果。tip 是有更快的走法时的提示（见 docs/mesh.md#测速），写在第二行。
type Speed = { text: string; failed?: boolean; detail?: string; title?: string; tip?: string };

// speedTip：另一种走法快 20% 以上、或者有一端关了 QUIC、或者经面板中转时，提示怎么改。
function speedTip(p: MeshPeerStatus, r: MeshSpeedResponse) {
  if (p.path === "udp-relay" || p.path === "tls-relay") return t.mesh.tipRelay;
  if (r.quicOffHere) return t.mesh.tipQuicOffHere;
  if (r.quicOffPeer) return t.mesh.tipQuicOffPeer;
  if (r.quic && r.wireguardTested && r.wireguardUpload + r.wireguardDownload > 1.2 * (r.upload + r.download)) {
    return t.mesh.tipWireGuard(rates(r.wireguardUpload, r.wireguardDownload));
  }
  return undefined;
}

const rates = (up: number, down: number) => `↑ ${fmtRate(up)} ↓ ${fmtRate(down)}`;

const fmtRate = (v: number) => `${fmtBytes(v)}/s`;

// fmtLoss：丢包率（百万分之几）写成百分数，1% 以下保留两位小数。
const fmtLoss = (ppm: number) => (ppm === 0 ? "0%" : `${(ppm / 10000).toFixed(ppm >= 10000 ? 1 : 2)}%`);

// lossText：两个方向的丢包率，↑ 是发给对方的包，↓ 是对方发来的包；都没测到时为空。
function lossText(p: MeshPeerStatus) {
  const parts = [p.txLossKnown && `↑ ${fmtLoss(p.txLossPpm)}`, p.rxLossKnown && `↓ ${fmtLoss(p.rxLossPpm)}`].filter(Boolean);
  return parts.length > 0 ? t.mesh.loss(parts.join(" ")) : "";
}

// PeerSub：第二行写路径、QUIC、纠错（FEC，见 docs/mesh.md#抗丢包）和最近握手；Ping 失败的原因也写在这里，定宽的列放不下。
// 完整的地址、丢包率在 title。
function PeerSub({ peer: p, ping, speed }: { peer: MeshPeerStatus; ping?: Ping; speed?: Speed }) {
  const handshake = toDate(p.lastHandshake);
  const fecDetail = p.fec && t.mesh.fecDetail(p.fecRecovered.toLocaleString(locale));
  const loss = lossText(p);
  const where = [pathLabel[p.path] ?? p.path, p.path === "direct" && `${p.rttMs.toFixed(1)} ms`, p.address, p.endpoint || p.via, p.quic && t.mesh.quicHint, fecDetail, loss && t.mesh.lossHint]
    .filter(Boolean)
    .join(" · ");
  const path = [pathShort[p.path] ?? p.path, p.quic && t.mesh.quic, p.fec && p.fecParity > 0 && t.mesh.fec(p.fecParity), loss].filter(Boolean).join(" · ");
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
      {[ping?.detail, speed?.detail].filter(Boolean).map((d) => (
        <span key={d}>
          {" · "}
          <span className="text-destructive">{d}</span>
        </span>
      ))}
      {speed?.tip && (
        <>
          {" · "}
          <span className="text-warning-text">{speed.tip}</span>
        </>
      )}
    </span>
  );
}

// MeshPeers：与各节点、面板之间的连接；nodeId 不为空时可以从该节点测试延迟和速度（测速只有管理员能用）。
function MeshPeers({ status, nodeId }: { status: MeshStatus; nodeId?: string }) {
  const { isAdmin } = useRole();
  const [ping, setPing] = useState<Record<string, Ping>>({});
  const [speed, setSpeed] = useState<Record<string, Speed>>({});
  // 测过之后才多一列结果，平时不留空列。
  const pinged = Object.keys(ping).length > 0;
  const tested = Object.keys(speed).length > 0;
  const testing = Object.values(speed).some((s) => s.text === t.mesh.speedTesting);
  const canSpeed = !!nodeId && isAdmin;
  const runSpeed = async (key: string, p: MeshPeerStatus) => {
    if (!nodeId) return;
    setSpeed((x) => ({ ...x, [key]: { text: t.mesh.speedTesting } }));
    try {
      const r = await nodeClient.testNodeMeshSpeed({ nodeId, publicKey: p.publicKey });
      const title = [t.mesh.speedResult(r.quic ? t.mesh.viaQuic : t.mesh.viaWireGuard), r.wireguardTested && t.mesh.speedCompare(rates(r.wireguardUpload, r.wireguardDownload))]
        .filter(Boolean)
        .join(" · ");
      setSpeed((x) => ({ ...x, [key]: { text: rates(r.upload, r.download), title, tip: speedTip(p, r) } }));
    } catch (err) {
      const f = pingFailure(err);
      setSpeed((x) => ({ ...x, [key]: { ...f } }));
    }
  };
  if (status.peers.length === 0) return <EmptyRow>{t.mesh.noPeers}</EmptyRow>;
  return (
    <List>
      {status.peers.map((p) => {
        const key = p.address;
        const result = ping[key];
        const sp = speed[key];
        return (
          <Row
            key={key}
            title={<span className="truncate">{p.name}</span>}
            sub={<PeerSub peer={p} ping={result} speed={sp} />}
            cols={[
              { content: <span title={t.mesh.rxTx}>{`↓ ${fmtBytes(Number(p.rxBytes))}`}</span>, num: true },
              { content: <span title={t.mesh.rxTx}>{`↑ ${fmtBytes(Number(p.txBytes))}`}</span> },
              ...(nodeId && pinged ? [{ content: result && <span className={cn(result.failed && "text-destructive")}>{result.text}</span>, num: true, phone: true }] : []),
              ...(canSpeed && tested
                ? [{ content: sp && <span className={cn(sp.failed && "text-destructive")} title={sp.title}>{sp.text}</span>, num: true, width: "w-52" }]
                : []),
            ]}
            actionsWidth={canSpeed ? "w-36" : undefined}
            actions={
              nodeId ? (
                <div className="flex justify-end gap-1">
                {canSpeed && (
                  <Button size="row" variant="ghost" disabled={testing} onClick={() => runSpeed(key, p)}>
                    {t.mesh.speedTest}
                  </Button>
                )}
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
                </div>
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

// NodeQUIC：节点设置里的 QUIC 快速通道开关（见 docs/mesh.md#quic-快速通道），保存在节点上。
export function NodeQUIC({ nodeId, online }: { nodeId: string; online: boolean }) {
  const { isAdmin } = useRole();
  const queryClient = useQueryClient();
  const [busy, setBusy] = useState(false);
  const q = useQuery({ queryKey: ["node-mesh", nodeId], queryFn: () => nodeClient.getNodeMesh({ nodeId }), enabled: online });
  const off = q.data?.quicOff;
  const toggle = async () => {
    if (!off && !(await confirmAction(t.mesh.quicConfirm, { action: t.mesh.quicDisable }))) return;
    setBusy(true);
    try {
      const r = await nodeClient.setNodeMeshOptions({ nodeId, options: { quicOff: !off } });
      queryClient.setQueryData(["node-mesh", nodeId], (s: MeshStatus | undefined) => s && { ...s, quicOff: r.quicOff });
      queryClient.invalidateQueries({ queryKey: ["node-mesh", nodeId] });
      toast.success(t.common.feedback.saved);
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };
  return (
    <Section
      title={t.mesh.quicTitle}
      description={t.mesh.quicDescription}
      actions={
        q.data && (
          <div className="flex items-center gap-3">
            <span className="text-muted-foreground">{off ? t.mesh.quicOff : t.mesh.quicOn}</span>
            {isAdmin && (
              <Button variant="outline" disabled={busy || !online} onClick={toggle}>
                {busy && <Spinner />}
                {off ? t.mesh.quicEnable : t.mesh.quicDisable}
              </Button>
            )}
          </div>
        )
      }
    />
  );
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
