import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { errorMessage, nodeClient, projectClient, toDate } from "@/lib/api";
import { SharedNetworkWarning } from "@/components/shared-network-warning";
import {
  Alert,
  Badge,
  Button,
  Card,
  CardContent,
  CardHeader,
  CardTitle,
  Input,
  Select,
  Spinner,
} from "@/components/ui";

// ---- 备份节点（临时测试界面）----


// BackupCard：项目页上设置备份节点、查看同步状态。
export function BackupCard({
  nodeId,
  projectId,
}: {
  nodeId: string;
  projectId: string;
}) {
  const nodes = useQuery({
    queryKey: ["nodes"],
    queryFn: () => nodeClient.listNodes({}),
  });
  const st = useQuery({
    queryKey: ["backup", nodeId, projectId],
    queryFn: () => projectClient.getNodeProjectBackup({ nodeId, projectId }),
    refetchInterval: 5000,
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
  const failover = useQuery({
    queryKey: ["failover", nodeId, projectId],
    queryFn: () => projectClient.getNodeProjectFailover({ nodeId, projectId }),
  });
  if (failover.data?.config && !s?.targetNodeId) {
    return (
      <Card>
        <CardHeader>
          <CardTitle>备份节点</CardTitle>
        </CardHeader>
        <CardContent className="text-sm text-muted-foreground">
          这个项目设了顶替，顶替节点上已经有一份随时更新的副本，不用再设备份节点。
        </CardContent>
      </Card>
    );
  }
  return (
    <Card>
      <CardHeader>
        <CardTitle>备份节点</CardTitle>
      </CardHeader>
      <CardContent className="grid gap-3 text-sm">
        {error && <Alert>{error}</Alert>}
        <p className="text-muted-foreground">
          每次定时快照后，把快照和配置同步到另一个节点。这个节点挂了时，可以在备份节点上把项目恢复出来。
        </p>
        <div className="flex flex-wrap items-center gap-2">
          <Select
            className="w-56"
            value={s?.targetNodeId ?? ""}
            disabled={busy}
            onChange={(e) =>
              run(() =>
                projectClient.setNodeProjectBackup({
                  nodeId,
                  projectId,
                  backupNodeId: e.target.value,
                }),
              )
            }
          >
            <option value="">不备份</option>
            {others.map((n) => (
              <option key={n.id} value={n.id} disabled={!n.online}>
                {n.name}
                {!n.online && "（离线）"}
              </option>
            ))}
          </Select>
          {s?.targetNodeId && (
            <Button
              size="sm"
              variant="outline"
              disabled={busy || s.syncing}
              onClick={() =>
                run(() =>
                  projectClient.syncNodeProjectBackup({ nodeId, projectId }),
                )
              }
            >
              {busy || s.syncing ? <Spinner /> : null}立即同步
            </Button>
          )}
        </div>
        {s?.targetNodeId && (
          <div className="text-xs">
            {s.syncing ? (
              <Badge>同步中</Badge>
            ) : s.lastError ? (
              <Alert>上次同步失败：{s.lastError}</Alert>
            ) : s.lastSync ? (
              <Badge tone="success">
                上次同步 {toDate(s.lastSync)?.toLocaleString()}
              </Badge>
            ) : (
              <Badge>还没有同步过</Badge>
            )}
          </div>
        )}
      </CardContent>
    </Card>
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
  const nodes = useQuery({
    queryKey: ["nodes"],
    queryFn: () => nodeClient.listNodes({}),
  });
  const [target, setTarget] = useState("");
  // 默认用项目现在的目录：换一台机器用同一个路径最自然。
  const [dir, setDir] = useState(currentDir);
  const [lines, setLines] = useState<string[]>([]);
  const [result, setResult] = useState("");
  const [busy, setBusy] = useState(false);

  const migrate = async () => {
    if (!confirm(`把 ${name} 迁移到另一个节点？迁移最后阶段项目会短暂停止。`))
      return;
    setBusy(true);
    setLines([]);
    setResult("");
    try {
      for await (const ev of projectClient.migrateNodeProject({
        nodeId,
        projectId,
        targetNodeId: target,
        dir,
      })) {
        if (ev.event.case === "output") {
          const l = ev.event.value;
          setLines((x) => [...x, l]);
        } else if (ev.event.case === "result") {
          if (ev.event.value.success) {
            setResult("✅ 迁移完成，正在打开新节点上的项目……");
            setTimeout(() => navigate({ to: "/projects/$nodeId/$projectId", params: { nodeId: target, projectId } }), 1500);
          } else {
            setResult(`❌ ${ev.event.value.error}`);
          }
        }
      }
    } catch (err) {
      setResult(`❌ ${errorMessage(err)}`);
    } finally {
      setBusy(false);
      queryClient.invalidateQueries({ queryKey: ["projects"] });
    }
  };

  const others = nodes.data?.nodes.filter((n) => n.id !== nodeId) ?? [];
  const failover = useQuery({
    queryKey: ["failover", nodeId, projectId],
    queryFn: () => projectClient.getNodeProjectFailover({ nodeId, projectId }),
  });
  if (failover.data?.config) {
    return (
      <Card>
        <CardHeader>
          <CardTitle>迁移到其他节点</CardTitle>
        </CardHeader>
        <CardContent className="text-sm text-muted-foreground">
          这个项目设了顶替。迁移和顶替各管一件事：要迁移，先在上面取消顶替，迁移完成后在新节点上重新设置。
        </CardContent>
      </Card>
    );
  }
  return (
    <Card>
      <CardHeader>
        <CardTitle>迁移到其他节点</CardTitle>
      </CardHeader>
      <CardContent className="grid gap-3 text-sm">
        <p className="text-muted-foreground">
          先在项目运行时同步一次，再停止项目、同步最后的数据，在目标节点上启动。不丢数据；成功后原节点上的项目移出，目录保留。
        </p>
        <div className="flex flex-wrap items-center gap-2">
          <Select
            className="w-48"
            value={target}
            onChange={(e) => setTarget(e.target.value)}
            disabled={busy}
          >
            <option value="">选择目标节点</option>
            {others.map((n) => (
              <option key={n.id} value={n.id} disabled={!n.online}>
                {n.name}
                {!n.online && "（离线）"}
              </option>
            ))}
          </Select>
          <Input
            className="w-72 font-mono"
            value={dir}
            onChange={(e) => setDir(e.target.value)}
            disabled={busy}
          />
          <Button size="sm" disabled={busy || !target} onClick={migrate}>
            {busy ? <Spinner /> : null}迁移
          </Button>
        </div>
        <SharedNetworkWarning fromNodeId={nodeId} targetNodeId={target} networks={sharedNetworks} />
        {result && <div>{result}</div>}
        {lines.length > 0 && (
          <pre className="max-h-60 overflow-auto rounded bg-zinc-950 p-3 font-mono text-xs text-zinc-200">
            {lines.join("\n")}
          </pre>
        )}
      </CardContent>
    </Card>
  );
}

export const roleLabel: Record<string, string> = {
  holder: "运行中（持有租约）",
  standby: "热备",
  witness: "见证人",
};

// FailoverCard：项目页上设置顶替、查看租约状态。
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
  const nodes = useQuery({
    queryKey: ["nodes"],
    queryFn: () => nodeClient.listNodes({}),
  });
  const st = useQuery({
    queryKey: ["failover", nodeId, projectId],
    queryFn: () => projectClient.getNodeProjectFailover({ nodeId, projectId }),
    refetchInterval: 3000,
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
  const backup = useQuery({
    queryKey: ["backup", nodeId, projectId],
    queryFn: () => projectClient.getNodeProjectBackup({ nodeId, projectId }),
  });
  const s = st.data;
  const cfg = s?.config;
  return (
    <Card>
      <CardHeader>
        <CardTitle>顶替</CardTitle>
      </CardHeader>
      <CardContent className="grid gap-3 text-sm">
        {error && <Alert>{error}</Alert>}
        {cfg ? (
          <>
            <div className="grid gap-1 text-xs">
              <div>
                {cfg.a?.name} ⇄ {cfg.b?.name}，见证人 {cfg.witness?.name}，租约{" "}
                {cfg.leaseSeconds || 30} 秒
                {s.stateful &&
                  (cfg.autoStateful
                    ? "，有状态项目自动顶替"
                    : "，有状态项目顶替前需要确认")}
                {cfg.autoFailback ? `，${cfg.a?.name} 恢复后自动切回` : ""}
              </div>
              {s.failingBack && <div>正在切回 {cfg.a?.name}：同步最后的数据，项目会短暂停止。</div>}
              {s.failbackInMs > 0n && (
                <div>
                  原节点 {cfg.a?.name} 已经恢复，再稳定约 {Math.ceil(Number(s.failbackInMs) / 60000)} 分钟后自动切回（项目会短暂停止，不丢数据）。
                </div>
              )}
              <div>
                本节点：<Badge>{roleLabel[s.role] ?? s.role}</Badge> 持有者{" "}
                {s.holderName}（第 {s.epoch.toString()} 轮）
                {s.role === "holder" &&
                  (s.leaseRemainingMs > 0n ? (
                    <Badge tone="success">
                      租约剩 {(Number(s.leaseRemainingMs) / 1000).toFixed(0)} 秒
                    </Badge>
                  ) : (
                    <Badge>续不上租约，已停止</Badge>
                  ))}
              </div>
              <div className="text-muted-foreground">
                {s.lastSync
                  ? `最近一次同步给热备 ${toDate(s.lastSync)?.toLocaleString()}`
                  : "还没有同步给热备"}
              </div>
              {s.lastError && <Alert>{s.lastError}</Alert>}
            </div>
            <div className="flex flex-wrap items-center gap-2">
              <Select className="w-44" value={witness} onChange={(e) => setWitness(e.target.value)}>
                <option value="">本面板</option>
                {nodes.data?.nodes
                  .filter((n) => n.id !== cfg.a?.nodeId && n.id !== cfg.b?.nodeId)
                  .map((n) => (
                    <option key={n.id} value={n.id} disabled={!n.online}>
                      节点 {n.name}
                    </option>
                  ))}
              </Select>
              <Button
                size="sm"
                variant="outline"
                disabled={busy}
                onClick={() =>
                  run(() => projectClient.changeNodeProjectWitness({ nodeId, projectId, witnessNodeId: witness }))
                }
              >
                更换见证人
              </Button>
              <Button
                size="sm"
                variant="outline"
                disabled={busy}
                onClick={() => {
                  if (confirm("取消顶替？热备上已有的副本保留。"))
                    run(() =>
                      projectClient.setNodeProjectFailover({
                        nodeId,
                        projectId,
                        standbyNodeId: "",
                      }),
                    );
                }}
              >
                取消顶替
              </Button>
            </div>
          </>
        ) : (
          <>
            {backup.data?.targetNodeId && (
              <p className="rounded-md border px-3 py-2 text-sm text-muted-foreground">
                这个项目设了备份节点 {backup.data.targetName}。顶替和备份节点二选一：要设顶替，先在上面把备份节点改成"不备份"（顶替节点本身就有一份随时更新的副本）。
              </p>
            )}
            <p className="text-muted-foreground">
              指定一个顶替节点：这个节点失联约 40
              秒后，由顶替节点用最近一次同步的数据启动项目。见证人负责投第三票。
            </p>
            <div className="flex flex-wrap items-center gap-2">
              <Select
                className="w-44"
                value={standby}
                onChange={(e) => setStandby(e.target.value)}
              >
                <option value="">选择顶替节点</option>
                {others.map((n) => (
                  <option key={n.id} value={n.id} disabled={!n.online}>
                    {n.name}
                  </option>
                ))}
              </Select>
              <Input
                className="w-64 font-mono"
                value={dir}
                onChange={(e) => setDir(e.target.value)}
              />
              <Select
                className="w-44"
                value={witness}
                onChange={(e) => setWitness(e.target.value)}
              >
                <option value="">见证人：面板</option>
                {others
                  .filter((n) => n.id !== standby)
                  .map((n) => (
                    <option key={n.id} value={n.id} disabled={!n.online}>
                      见证人：{n.name}
                    </option>
                  ))}
              </Select>
              <label className="text-xs">
                <input
                  type="checkbox"
                  className="mr-1"
                  checked={auto}
                  onChange={(e) => setAuto(e.target.checked)}
                />
                有状态项目也自动顶替
              </label>
              <label
                className="text-xs"
                title="项目被顶替到另一个节点以后，原节点恢复并稳定 10 分钟，自动把项目切回原节点。切换时项目短暂停止，不丢数据。"
              >
                <input
                  type="checkbox"
                  className="mr-1"
                  checked={failback}
                  onChange={(e) => setFailback(e.target.checked)}
                />
                原节点恢复后自动切回
              </label>
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
                设置顶替
              </Button>
            </div>
            <SharedNetworkWarning fromNodeId={nodeId} targetNodeId={standby} networks={sharedNetworks} />
          </>
        )}
      </CardContent>
    </Card>
  );
}

