import { useEffect, useState } from "react";
import { useQuery, useQueryClient, type UseQueryResult } from "@tanstack/react-query";
import { confirmAction } from "@/components/confirm";
import { Code, ConnectError } from "@connectrpc/connect";
import { errorMessage, nodeClient, toDate } from "@/lib/api";
import type { MeshPeerStatus, MeshStatus } from "@/gen/nagi/v1/common_pb";
import { fmtBytes } from "@/components/metrics";
import { Alert, Button, ButtonLink, Dialog, DialogActions, EmptyRow, List, Notice, QueryError, Roll, Row, RowsSkeleton, Section, Spinner, Table, Td, Th } from "@/components/ui";
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
  return [status.address, `UDP ${status.port}`, endpoints.length > 0 && `${t.mesh.endpoints} ${t.common.list(endpoints)}`].filter(Boolean).join(" · ");
}

type Ping = { text: string; failed?: boolean; detail?: string };

function pingFailure(err: unknown): Ping {
  const detail = errorMessage(err);
  if (err instanceof ConnectError && err.code === Code.DeadlineExceeded) return { text: t.mesh.pingTimeout, failed: true };
  return { text: t.mesh.pingFailed, failed: true, detail };
}

// ---- 测速（见 docs/mesh.md#测速） ----

// SpeedStep：测速的一步。path 走平时的路；wireguard 经 WireGuard 隧道，平时走 QUIC 时用来对比。
type SpeedStep = { via: "path" | "wireguard"; state: "wait" | "run" | "done" | "fail"; quic?: boolean; up?: number; down?: number; error?: string; started?: number };
// SpeedRun：和一个成员的一次测速，关掉对话框也接着测，列表里留最近一次的结果。
type SpeedRun = { steps: SpeedStep[]; quicOffHere?: boolean; quicOffPeer?: boolean };

const speedRunning = (r?: SpeedRun) => !!r && r.steps.some((s) => s.state === "run" || s.state === "wait");

// speedTip：有更快的走法时提示怎么改：经面板中转、有一端关了 QUIC、WireGuard 隧道比 QUIC 快 20% 以上。
// settings：要改的是本节点的 QUIC 设置，提示旁给出到节点设置的链接。
function speedTip(p: MeshPeerStatus, r: SpeedRun, port: number): { text: string; settings?: boolean } | undefined {
  if (p.path === "udp-relay" || p.path === "tls-relay") return { text: t.mesh.tipRelay(port) };
  if (r.quicOffHere) return { text: t.mesh.tipQuicOffHere, settings: true };
  if (r.quicOffPeer) return { text: t.mesh.tipQuicOffPeer };
  const [a, b] = r.steps;
  if (a?.quic && a.state === "done" && b?.state === "done" && b.up! + b.down! > 1.2 * (a.up! + a.down!)) return { text: t.mesh.tipWireGuard, settings: true };
  return undefined;
}

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
function PeerSub({ peer: p, ping }: { peer: MeshPeerStatus; ping?: Ping }) {
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
      {ping?.detail && (
        <>
          {" · "}
          <span className="text-destructive">{ping.detail}</span>
        </>
      )}
    </span>
  );
}

// MeshPeers：与各节点、面板之间的连接；nodeId 不为空时可以从该节点测试延迟和速度（测速只有管理员能用）。
function MeshPeers({ status, nodeId }: { status: MeshStatus; nodeId?: string }) {
  const { isAdmin } = useRole();
  const [ping, setPing] = useState<Record<string, Ping>>({});
  const [speed, setSpeed] = useState<Record<string, SpeedRun>>({});
  const [open, setOpen] = useState<string | null>(null);
  // 测过之后才多一列结果，平时不留空列。
  const pinged = Object.keys(ping).length > 0;
  const tested = Object.keys(speed).length > 0;
  // 同一时间只测一个：对方同时只接受一个测速。
  const testing = Object.values(speed).some(speedRunning);
  const canSpeed = !!nodeId && isAdmin;
  // runSpeed 先走平时的路测；走的是 QUIC 再经 WireGuard 隧道测一次对比。每一步测完就显示。
  const runSpeed = async (key: string, p: MeshPeerStatus) => {
    if (!nodeId) return;
    const set = (fn: (r: SpeedRun) => SpeedRun) => setSpeed((x) => ({ ...x, [key]: fn(x[key]) }));
    const first: SpeedStep = { via: "path", state: "run", quic: p.quic, started: Date.now() };
    setSpeed((x) => ({ ...x, [key]: { steps: p.quic ? [first, { via: "wireguard", state: "wait" }] : [first] } }));
    let r;
    try {
      r = await nodeClient.testNodeMeshSpeed({ nodeId, publicKey: p.publicKey, speedVia: "path" });
    } catch (err) {
      set(() => ({ steps: [{ via: "path", state: "fail", quic: p.quic, error: speedFailure(err) }] }));
      return;
    }
    const done: SpeedStep = { via: "path", state: "done", quic: r.quic, up: r.upload, down: r.download };
    const base = { quicOffHere: r.quicOffHere, quicOffPeer: r.quicOffPeer };
    // v0.20.0 以前的节点不分步，一次就带着对比的结果。
    if (r.wireguardTested) {
      set(() => ({ ...base, steps: [done, { via: "wireguard", state: "done", up: r.wireguardUpload, down: r.wireguardDownload }] }));
      return;
    }
    if (!r.quic) {
      set(() => ({ ...base, steps: [done] }));
      return;
    }
    set(() => ({ ...base, steps: [done, { via: "wireguard", state: "run", started: Date.now() }] }));
    let last: SpeedStep;
    try {
      const w = await nodeClient.testNodeMeshSpeed({ nodeId, publicKey: p.publicKey, speedVia: "wireguard" });
      last = { via: "wireguard", state: "done", up: w.upload, down: w.download };
    } catch (err) {
      last = { via: "wireguard", state: "fail", error: speedFailure(err) };
    }
    set(() => ({ ...base, steps: [done, last] }));
  };
  if (status.peers.length === 0) return <EmptyRow>{t.mesh.noPeers}</EmptyRow>;
  const openPeer = status.peers.find((p) => p.address === open);
  return (
    <>
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
                ...(canSpeed && tested ? [{ content: speed[key] && <SpeedSummary run={speed[key]} />, num: true, phone: true, width: "w-40" }] : []),
              ]}
              actionsWidth={canSpeed ? "w-36" : undefined}
              actions={
                nodeId ? (
                  <div className="flex justify-end gap-1">
                    {canSpeed && (
                      <Button
                        size="row"
                        variant="ghost"
                        disabled={testing && !speedRunning(speed[key])}
                        onClick={() => {
                          setOpen(key);
                          if (!speedRunning(speed[key])) void runSpeed(key, p);
                        }}
                      >
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
                      {t.mesh.ping}
                    </Button>
                  </div>
                ) : undefined
              }
            />
          );
        })}
      </List>
      {openPeer && speed[openPeer.address] && nodeId && (
        <SpeedDialog nodeId={nodeId} port={status.port} peer={openPeer} run={speed[openPeer.address]} onRetry={() => runSpeed(openPeer.address, openPeer)} onClose={() => setOpen(null)} />
      )}
    </>
  );
}

function speedFailure(err: unknown) {
  return err instanceof ConnectError && err.code === Code.DeadlineExceeded ? t.mesh.speedTimeout : errorMessage(err);
}

// SpeedSummary：列表里的测速结果：测速中，或者平时那条路最近一次的上传（↑，和丢包率的箭头一致）、下载。
function SpeedSummary({ run }: { run: SpeedRun }) {
  const a = run.steps[0];
  if (speedRunning(run)) return <span className="text-muted-foreground">{t.mesh.speedTesting}</span>;
  if (a.state !== "done") return <span className="text-destructive">{t.mesh.speedFailed}</span>;
  return <span title={t.mesh.speedLast}>{ratePair(a.up!, a.down!)}</span>;
}

// ratePair：↑ 31.9 ↓ 31.5 MB/s，单位一样时只写一次。
function ratePair(up: number, down: number) {
  const [u, uu] = fmtRate(up).split(" ");
  const [d, du] = fmtRate(down).split(" ");
  return uu === du ? `↑ ${u} ↓ ${d} ${du}` : `↑ ${u} ${uu} ↓ ${d} ${du}`;
}

// useSeconds：测速进行中时每秒刷新一次，显示已经测了几秒。
function useSeconds(active: boolean) {
  const [, tick] = useState(0);
  useEffect(() => {
    if (!active) return;
    const id = setInterval(() => tick((n) => n + 1), 1000);
    return () => clearInterval(id);
  }, [active]);
}

// SpeedDialog：测速的进度和结果。一行一种走法，测完一步就显示；下面是这条连接的路径、延迟、丢包和建议。
function SpeedDialog({ nodeId, port, peer: p, run, onRetry, onClose }: { nodeId: string; port: number; peer: MeshPeerStatus; run: SpeedRun; onRetry: () => void; onClose: () => void }) {
  const busy = speedRunning(run);
  useSeconds(busy);
  const tip = busy ? undefined : speedTip(p, run, port);
  const facts = [pathLabel[p.path] ?? p.path, p.path === "direct" && `${p.rttMs.toFixed(1)} ms`, lossText(p)].filter(Boolean).join(" · ");
  const label = (s: SpeedStep) => (s.via === "wireguard" ? t.mesh.viaWireGuardCompare : `${s.quic ? t.mesh.viaQuic : t.mesh.viaWireGuardShort}${t.mesh.speedCurrent}`);
  const status = (s: SpeedStep) => {
    switch (s.state) {
      case "run":
        return (
          <span className="inline-flex items-center gap-2">
            <Spinner />
            {t.mesh.speedRunning(Math.floor((Date.now() - (s.started ?? Date.now())) / 1000))}
          </span>
        );
      case "wait":
        return t.mesh.speedWaiting;
      default:
        return s.via === "wireguard" ? t.mesh.speedCompareFailed : t.mesh.speedFailed;
    }
  };
  const errors = run.steps.filter((s) => s.state === "fail" && s.error);
  return (
    <Dialog open onClose={onClose} title={t.mesh.speedTitle(p.name)} description={t.mesh.speedDescription} className="max-w-[520px]">
      <Table>
        <thead>
          <tr>
            <Th>{t.mesh.speedVia}</Th>
            <Th className="w-28 text-right max-md:w-auto">{t.mesh.speedUp}</Th>
            <Th className="w-28 text-right max-md:w-auto">{t.mesh.speedDown}</Th>
          </tr>
        </thead>
        <tbody>
          {run.steps.map((s) => (
            <tr key={s.via} className="whitespace-nowrap">
              <Td>{label(s)}</Td>
              {s.state === "done" ? (
                <>
                  <Td className="text-right tabular-nums">{fmtRate(s.up!)}</Td>
                  <Td className="text-right tabular-nums">{fmtRate(s.down!)}</Td>
                </>
              ) : (
                <Td colSpan={2} className={cn("text-right tabular-nums", s.state === "fail" ? "text-destructive" : "text-muted-foreground")}>
                  {status(s)}
                </Td>
              )}
            </tr>
          ))}
        </tbody>
      </Table>
      <p className="text-[13px] text-muted-foreground">{facts}</p>
      {errors.map((s) => (
        <Alert key={s.via}>{s.error}</Alert>
      ))}
      {tip && (
        <Notice
          tone="warning"
          title={tip.text}
          actions={
            tip.settings && (
              <ButtonLink to="/nodes/$nodeId" params={{ nodeId }} search={{ tab: "settings" }} variant="outline" readOnlyOk>
                {t.mesh.openSettings}
              </ButtonLink>
            )
          }
        />
      )}
      <DialogActions>
        <Button variant="outline" onClick={onClose} readOnlyOk>
          {t.common.actions.close}
        </Button>
        <Button onClick={onRetry} disabled={busy}>
          {busy && <Spinner />}
          {busy ? t.mesh.speedTesting : t.mesh.speedRetest}
        </Button>
      </DialogActions>
    </Dialog>
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
  const [error, setError] = useState("");
  const q = useQuery({ queryKey: ["node-mesh", nodeId], queryFn: () => nodeClient.getNodeMesh({ nodeId }), enabled: online });
  const off = q.data?.quicOff;
  // 结果直接显示在开关旁，不另外提示「已保存」。
  const toggle = async () => {
    if (!off && !(await confirmAction(t.mesh.quicConfirm, { action: t.mesh.quicDisable }))) return;
    setBusy(true);
    setError("");
    try {
      const r = await nodeClient.setNodeMeshOptions({ nodeId, options: { quicOff: !off } });
      queryClient.setQueryData(["node-mesh", nodeId], (s: MeshStatus | undefined) => s && { ...s, quicOff: r.quicOff });
      queryClient.invalidateQueries({ queryKey: ["node-mesh", nodeId] });
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };
  return (
    <Section
      title={t.mesh.quicTitle}
      description={t.mesh.quicDescription}
      help={t.mesh.quicHelp}
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
    >
      {q.error && !q.data ? <QueryError error={errorMessage(q.error)} onRetry={() => q.refetch()} /> : error ? <Alert>{error}</Alert> : null}
    </Section>
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
