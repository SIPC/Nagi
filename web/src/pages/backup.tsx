import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { errorMessage, nodeClient, projectClient, toDate } from "@/lib/api";
import { SharedNetworkWarning } from "@/components/shared-network-warning";
import { Alert, Badge, Button, Check, Field, Input, KeyValue, LogBox, Notice, Section, Select, Spinner, Status } from "@/components/ui";

// 项目的备份节点、顶替和迁移。

const fmtTime = (t: Parameters<typeof toDate>[0]) => toDate(t)?.toLocaleString() ?? "—";

// BackupCard：设置备份节点、查看同步状态。
export function BackupCard({ nodeId, projectId }: { nodeId: string; projectId: string }) {
  const nodes = useQuery({ queryKey: ["nodes"], queryFn: () => nodeClient.listNodes({}) });
  const st = useQuery({
    queryKey: ["backup", nodeId, projectId],
    queryFn: () => projectClient.getNodeProjectBackup({ nodeId, projectId }),
    refetchInterval: 5000,
  });
  const failover = useQuery({
    queryKey: ["failover", nodeId, projectId],
    queryFn: () => projectClient.getNodeProjectFailover({ nodeId, projectId }),
  });
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  const run = async (fn: () => Promise<unknown>) => {
    setError("");
    setBusy(true);
    try {
      await fn();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
      st.refetch();
    }
  };

  const others = nodes.data?.nodes.filter((n) => n.id !== nodeId) ?? [];
  const s = st.data;
  const title = "备份节点";
  const description = "每次定时快照后，将快照和配置同步到另一个节点。";
  const help = "本节点不可用时，可在备份节点上恢复项目。";
  if (failover.data?.config && !s?.targetNodeId) {
    return (
      <Section title={title} description={description} help={help}>
        <p className="text-[13px] text-muted-foreground">已启用顶替，顶替节点上保存有实时更新的副本，无需设置备份节点。</p>
      </Section>
    );
  }
  return (
    <Section
      title={title}
      description={description}
      help={help}
      actions={
        s?.targetNodeId && (
          <Button size="sm" variant="outline" disabled={busy || s.syncing} onClick={() => run(() => projectClient.syncNodeProjectBackup({ nodeId, projectId }))}>
            {(busy || s.syncing) && <Spinner />}
            立即同步
          </Button>
        )
      }
    >
      <div className="grid gap-4">
        {error && <Alert>{error}</Alert>}
        <div className="flex flex-wrap items-end gap-x-8 gap-y-4">
          <Field label="目标节点" htmlFor="backup-node" className="w-60">
            <Select
              id="backup-node"
              value={s?.targetNodeId ?? ""}
              disabled={busy}
              onChange={(e) => run(() => projectClient.setNodeProjectBackup({ nodeId, projectId, backupNodeId: e.target.value }))}
            >
              <option value="">不备份</option>
              {others.map((n) => (
                <option key={n.id} value={n.id} disabled={!n.online}>
                  {n.name}
                  {!n.online && "（离线）"}
                </option>
              ))}
            </Select>
          </Field>
          {s?.targetNodeId && (
            <div className="grid gap-1.5 pb-2">
              <span className="text-[13px] font-medium">同步状态</span>
              {s.syncing ? (
                <Status tone="info" pulse>
                  同步中
                </Status>
              ) : s.lastError ? (
                <Status tone="danger">同步失败</Status>
              ) : s.lastSync ? (
                <Status tone="success">上次同步 {fmtTime(s.lastSync)}</Status>
              ) : (
                <Status>尚未同步</Status>
              )}
            </div>
          )}
        </div>
        {s?.targetNodeId && !s.syncing && s.lastError && <Alert>{s.lastError}</Alert>}
      </div>
    </Section>
  );
}

// MigrateCard：把项目迁移到另一个节点。
export function MigrateCard({
  nodeId,
  projectId,
  name,
  dir: currentDir,
  sharedNetworks = [],
}: {
  nodeId: string;
  projectId: string;
  name: string;
  dir: string;
  sharedNetworks?: string[];
}) {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const nodes = useQuery({ queryKey: ["nodes"], queryFn: () => nodeClient.listNodes({}) });
  const failover = useQuery({
    queryKey: ["failover", nodeId, projectId],
    queryFn: () => projectClient.getNodeProjectFailover({ nodeId, projectId }),
  });
  const [target, setTarget] = useState("");
  // 默认用项目现在的目录：换一台机器用同一个路径最自然。
  const [dir, setDir] = useState(currentDir);
  const [lines, setLines] = useState<string[]>([]);
  const [result, setResult] = useState<{ ok: boolean; text: string }>();
  const [busy, setBusy] = useState(false);

  const migrate = async () => {
    if (!confirm(`将项目 ${name} 迁移到其他节点？迁移的最后阶段项目会短暂停止。`)) return;
    setBusy(true);
    setLines([]);
    setResult(undefined);
    try {
      for await (const ev of projectClient.migrateNodeProject({ nodeId, projectId, targetNodeId: target, dir })) {
        if (ev.event.case === "output") {
          const l = ev.event.value;
          setLines((x) => [...x, l]);
        } else if (ev.event.case === "result") {
          if (ev.event.value.success) {
            setResult({ ok: true, text: "迁移完成，正在打开目标节点上的项目…" });
            setTimeout(() => navigate({ to: "/projects/$nodeId/$projectId", params: { nodeId: target, projectId } }), 1500);
          } else {
            setResult({ ok: false, text: ev.event.value.error });
          }
        }
      }
    } catch (err) {
      setResult({ ok: false, text: errorMessage(err) });
    } finally {
      setBusy(false);
      queryClient.invalidateQueries({ queryKey: ["projects"] });
    }
  };

  const others = nodes.data?.nodes.filter((n) => n.id !== nodeId) ?? [];
  const title = "迁移";
  const description = "将项目迁移到其他节点，数据完整保留。";
  const help = "项目运行时先同步一次，随后停止项目、同步剩余数据，并在目标节点启动。成功后原节点移除该项目，目录保留。";
  if (failover.data?.config) {
    return (
      <Section title={title} description={description} help={help}>
        <p className="text-[13px] text-muted-foreground">已启用顶替。如需迁移，请先取消顶替，迁移完成后在新节点上重新设置。</p>
      </Section>
    );
  }
  return (
    <Section title={title} description={description} help={help}>
      <div className="grid gap-4">
        <div className="flex flex-wrap items-end gap-3">
          <Field label="目标节点" htmlFor="migrate-node" className="w-52">
            <Select id="migrate-node" value={target} onChange={(e) => setTarget(e.target.value)} disabled={busy}>
              <option value="">选择节点</option>
              {others.map((n) => (
                <option key={n.id} value={n.id} disabled={!n.online}>
                  {n.name}
                  {!n.online && "（离线）"}
                </option>
              ))}
            </Select>
          </Field>
          <Field label="项目目录" htmlFor="migrate-dir" className="min-w-64 flex-1">
            <Input id="migrate-dir" className="font-mono" value={dir} onChange={(e) => setDir(e.target.value)} disabled={busy} />
          </Field>
          <Button size="sm" className="mb-0.5" disabled={busy || !target} onClick={migrate}>
            {busy && <Spinner />}
            开始迁移
          </Button>
        </div>
        <SharedNetworkWarning fromNodeId={nodeId} targetNodeId={target} networks={sharedNetworks} />
        {result && (result.ok ? <Notice tone="success">{result.text}</Notice> : <Alert>{result.text}</Alert>)}
        {lines.length > 0 && <LogBox className="max-h-60">{lines.join("\n")}</LogBox>}
      </div>
    </Section>
  );
}

export const roleLabel: Record<string, string> = {
  holder: "运行中（持有租约）",
  standby: "热备",
  witness: "见证人",
};

// FailoverCard：设置顶替、查看租约状态。
export function FailoverCard({
  nodeId,
  projectId,
  dir: currentDir,
  sharedNetworks = [],
}: {
  nodeId: string;
  projectId: string;
  dir: string;
  sharedNetworks?: string[];
}) {
  const nodes = useQuery({ queryKey: ["nodes"], queryFn: () => nodeClient.listNodes({}) });
  const st = useQuery({
    queryKey: ["failover", nodeId, projectId],
    queryFn: () => projectClient.getNodeProjectFailover({ nodeId, projectId }),
    refetchInterval: 3000,
  });
  const backup = useQuery({
    queryKey: ["backup", nodeId, projectId],
    queryFn: () => projectClient.getNodeProjectBackup({ nodeId, projectId }),
  });
  const [standby, setStandby] = useState("");
  const [dir, setDir] = useState(currentDir);
  const [witness, setWitness] = useState("");
  const [auto, setAuto] = useState(false);
  const [failback, setFailback] = useState(true);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  const run = async (fn: () => Promise<unknown>) => {
    setError("");
    setBusy(true);
    try {
      await fn();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
      st.refetch();
    }
  };

  const others = nodes.data?.nodes.filter((n) => n.id !== nodeId) ?? [];
  const s = st.data;
  const cfg = s?.config;
  const help = "本节点失联约 40 秒后，由顶替节点使用最近一次同步的数据启动项目。见证人在两个节点之间投出第三票，避免两边同时运行。";

  if (cfg && s) {
    const holding = s.role === "holder";
    return (
      <Section
        title={
          <>
            顶替
            <Badge tone="success">已启用</Badge>
          </>
        }
        description="本节点失联时，由热备节点接管项目。"
        help={help}
        actions={
          <Button
            size="sm"
            variant="ghost"
            className="hover:text-destructive"
            disabled={busy}
            onClick={() => {
              if (confirm("取消顶替？热备节点上已有的副本将保留。")) run(() => projectClient.setNodeProjectFailover({ nodeId, projectId, standbyNodeId: "" }));
            }}
          >
            取消顶替
          </Button>
        }
      >
        <div className="grid gap-5">
          {error && <Alert>{error}</Alert>}
          {s.failingBack && <Notice tone="warning">正在切回 {cfg.a?.name}：同步剩余数据，项目会短暂停止。</Notice>}
          {s.failbackInMs > 0n && (
            <Notice>
              原节点 {cfg.a?.name} 已恢复，约 {Math.ceil(Number(s.failbackInMs) / 60000)} 分钟后自动切回。切换时项目短暂停止，数据完整保留。
            </Notice>
          )}
          <KeyValue
            items={[
              [
                "节点",
                <span>
                  {cfg.a?.name} ⇄ {cfg.b?.name}
                </span>,
              ],
              ["见证人", cfg.witness?.name || "本面板"],
              [
                "本节点角色",
                <span className="inline-flex flex-wrap items-center gap-2">
                  {roleLabel[s.role] ?? s.role}
                  {holding &&
                    (s.leaseRemainingMs > 0n ? (
                      <Status tone="success">租约剩余 {(Number(s.leaseRemainingMs) / 1000).toFixed(0)} 秒</Status>
                    ) : (
                      <Status tone="danger">租约未续期，已停止</Status>
                    ))}
                </span>,
              ],
              [
                "当前持有者",
                <span>
                  {s.holderName} <span className="text-muted-foreground">· 第 {s.epoch.toString()} 轮</span>
                </span>,
              ],
              ["租约时长", `${cfg.leaseSeconds || 30} 秒`],
              ...(s.stateful ? [["有状态项目", cfg.autoStateful ? "自动顶替" : "顶替前需确认"] as [string, string]] : []),
              ["自动切回", cfg.autoFailback ? `${cfg.a?.name} 恢复并稳定 10 分钟后切回` : "关闭"],
              ["最近同步", s.lastSync ? fmtTime(s.lastSync) : "尚未同步"],
            ]}
          />
          {s.lastError && <Alert>{s.lastError}</Alert>}
          <div className="flex flex-wrap items-end gap-3 border-t pt-4">
            <Field label="更换见证人" htmlFor="failover-witness" className="w-52">
              <Select id="failover-witness" value={witness} onChange={(e) => setWitness(e.target.value)}>
                <option value="">本面板</option>
                {nodes.data?.nodes
                  .filter((n) => n.id !== cfg.a?.nodeId && n.id !== cfg.b?.nodeId)
                  .map((n) => (
                    <option key={n.id} value={n.id} disabled={!n.online}>
                      节点 {n.name}
                    </option>
                  ))}
              </Select>
            </Field>
            <Button size="sm" variant="outline" className="mb-0.5" disabled={busy} onClick={() => run(() => projectClient.changeNodeProjectWitness({ nodeId, projectId, witnessNodeId: witness }))}>
              更换
            </Button>
          </div>
        </div>
      </Section>
    );
  }

  return (
    <Section title="顶替" description="指定热备节点，本节点失联时自动接管项目。" help={help}>
      <div className="grid gap-4">
        {error && <Alert>{error}</Alert>}
        {backup.data?.targetNodeId ? (
          <Notice tone="warning">
            已设置备份节点 {backup.data.targetName}。顶替与备份节点只能二选一，请先将备份节点改为「不备份」（顶替节点本身保存实时更新的副本）。
          </Notice>
        ) : (
          <>
            <div className="grid gap-4 sm:grid-cols-3">
              <Field label="热备节点" htmlFor="failover-standby">
                <Select id="failover-standby" value={standby} onChange={(e) => setStandby(e.target.value)}>
                  <option value="">选择节点</option>
                  {others.map((n) => (
                    <option key={n.id} value={n.id} disabled={!n.online}>
                      {n.name}
                    </option>
                  ))}
                </Select>
              </Field>
              <Field label="热备节点上的目录" htmlFor="failover-dir">
                <Input id="failover-dir" className="font-mono" value={dir} onChange={(e) => setDir(e.target.value)} />
              </Field>
              <Field label="见证人" htmlFor="failover-new-witness">
                <Select id="failover-new-witness" value={witness} onChange={(e) => setWitness(e.target.value)}>
                  <option value="">本面板</option>
                  {others
                    .filter((n) => n.id !== standby)
                    .map((n) => (
                      <option key={n.id} value={n.id} disabled={!n.online}>
                        节点 {n.name}
                      </option>
                    ))}
                </Select>
              </Field>
            </div>
            <div className="flex flex-wrap gap-x-6 gap-y-2">
              <Check checked={auto} onChange={setAuto} help="有状态项目默认需要人工确认后才顶替，以免数据分叉。">
                有状态项目自动顶替
              </Check>
              <Check checked={failback} onChange={setFailback} help="原节点恢复并稳定 10 分钟后，自动将项目切回原节点。切换时项目短暂停止，数据完整保留。">
                原节点恢复后自动切回
              </Check>
            </div>
            <SharedNetworkWarning fromNodeId={nodeId} targetNodeId={standby} networks={sharedNetworks} />
            <div className="flex justify-end border-t pt-4">
              <Button
                size="sm"
                disabled={busy || !standby}
                onClick={() =>
                  run(() =>
                    projectClient.setNodeProjectFailover({
                      nodeId,
                      projectId,
                      standbyNodeId: standby,
                      standbyDir: dir,
                      witnessNodeId: witness,
                      autoStateful: auto,
                      autoFailback: failback,
                    }),
                  )
                }
              >
                {busy && <Spinner />}
                启用顶替
              </Button>
            </div>
          </>
        )}
      </div>
    </Section>
  );
}
