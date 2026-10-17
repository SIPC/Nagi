import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { errorMessage, nodeClient, projectClient, toDate } from "@/lib/api";
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

function fmtBytes(v: number) {
  const units = ["B", "KB", "MB", "GB"];
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(1)} ${units[i]}`;
}

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

// NodeReplicas：节点作为备份节点保存的副本，可以恢复成项目。
export function NodeReplicas({ nodeId }: { nodeId: string }) {
  const queryClient = useQueryClient();
  const q = useQuery({
    queryKey: ["replicas", nodeId],
    queryFn: () => nodeClient.listNodeReplicas({ nodeId }),
  });
  const [open, setOpen] = useState<string | null>(null);
  if (q.isPending) return <Spinner />;
  if (q.error) return <Alert>{errorMessage(q.error)}</Alert>;
  if (q.data.replicas.length === 0)
    return (
      <div className="py-2 text-xs text-muted-foreground">
        这个节点上没有副本
      </div>
    );
  return (
    <div className="grid gap-2 py-2 text-xs">
      {q.data.replicas.map((r) => (
        <div key={r.projectId} className="grid gap-2 rounded border p-2">
          <div className="flex flex-wrap items-center gap-3">
            <span className="font-medium">{r.projectName}</span>
            <span className="font-mono text-muted-foreground">
              {r.projectId}
            </span>
            <span>来自 {r.sourceName}</span>
            <span>{fmtBytes(Number(r.bytes))}</span>
            <span className="text-muted-foreground">
              {r.lastSync
                ? `同步于 ${toDate(r.lastSync)?.toLocaleString()}`
                : "还没有同步"}
            </span>
            <Button
              size="sm"
              variant="outline"
              onClick={() => setOpen(open === r.projectId ? null : r.projectId)}
            >
              恢复成项目
            </Button>
            <Button
              size="sm"
              variant="ghost"
              onClick={async () => {
                if (!confirm(`删除 ${r.projectName} 的副本？`)) return;
                await nodeClient.deleteNodeReplica({
                  nodeId,
                  projectId: r.projectId,
                });
                queryClient.invalidateQueries({
                  queryKey: ["replicas", nodeId],
                });
              }}
            >
              删除
            </Button>
          </div>
          {open === r.projectId && (
            <RestoreReplica
              nodeId={nodeId}
              projectId={r.projectId}
              name={r.projectName}
            />
          )}
        </div>
      ))}
    </div>
  );
}

function RestoreReplica({
  nodeId,
  projectId,
  name,
}: {
  nodeId: string;
  projectId: string;
  name: string;
}) {
  const queryClient = useQueryClient();
  const snaps = useQuery({
    queryKey: ["replica-snapshots", nodeId, projectId],
    queryFn: () => nodeClient.listNodeReplicaSnapshots({ nodeId, projectId }),
  });
  const [dir, setDir] = useState(`/opt/nagi/${name}`);
  const [snap, setSnap] = useState("");
  const [lines, setLines] = useState<string[]>([]);
  const [result, setResult] = useState("");
  const [busy, setBusy] = useState(false);

  const restore = async () => {
    setBusy(true);
    setLines([]);
    setResult("");
    try {
      for await (const ev of nodeClient.restoreNodeReplica({
        nodeId,
        projectId,
        dir,
        snapshotId: snap,
      })) {
        if (ev.event.case === "output") {
          const l = ev.event.value;
          setLines((x) => [...x, l]);
        } else if (ev.event.case === "result") {
          setResult(
            ev.event.value.success
              ? "✅ 已恢复并部署"
              : `❌ ${ev.event.value.error}`,
          );
        }
      }
    } catch (err) {
      setResult(`❌ ${errorMessage(err)}`);
    } finally {
      setBusy(false);
      queryClient.invalidateQueries({ queryKey: ["replicas", nodeId] });
      queryClient.invalidateQueries({ queryKey: ["projects"] });
    }
  };

  const withData =
    snaps.data?.snapshots.filter((s) => s.kind !== "version") ?? [];
  return (
    <div className="grid gap-2">
      {snaps.error && <Alert>{errorMessage(snaps.error)}</Alert>}
      <div className="flex flex-wrap items-center gap-2">
        <Input
          className="w-72 font-mono"
          value={dir}
          onChange={(e) => setDir(e.target.value)}
        />
        <Select
          className="w-72"
          value={snap}
          onChange={(e) => setSnap(e.target.value)}
        >
          <option value="">最新的快照</option>
          {withData.map((s) => (
            <option key={s.id} value={s.id}>
              {toDate(s.time)?.toLocaleString()} · {s.kind} ·{" "}
              {fmtBytes(Number(s.size))}
            </option>
          ))}
        </Select>
        <Button size="sm" disabled={busy} onClick={restore}>
          {busy ? <Spinner /> : null}恢复
        </Button>
      </div>
      {result && <div>{result}</div>}
      {lines.length > 0 && (
        <pre className="max-h-60 overflow-auto rounded bg-zinc-950 p-3 font-mono text-zinc-200">
          {lines.join("\n")}
        </pre>
      )}
    </div>
  );
}

// MigrateCard：把项目迁移到另一个节点。
export function MigrateCard({
  nodeId,
  projectId,
  name,
}: {
  nodeId: string;
  projectId: string;
  name: string;
}) {
  const queryClient = useQueryClient();
  const nodes = useQuery({
    queryKey: ["nodes"],
    queryFn: () => nodeClient.listNodes({}),
  });
  const [target, setTarget] = useState("");
  const [dir, setDir] = useState(`/opt/nagi/${name}`);
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
          setResult(
            ev.event.value.success
              ? "✅ 迁移完成，到项目列表里查看新节点上的项目"
              : `❌ ${ev.event.value.error}`,
          );
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

const roleLabel: Record<string, string> = {
  holder: "运行中（持有租约）",
  standby: "热备",
  witness: "见证人",
};

// FailoverCard：项目页上设置顶替、查看租约状态。
export function FailoverCard({
  nodeId,
  projectId,
  name,
}: {
  nodeId: string;
  projectId: string;
  name: string;
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
  const [dir, setDir] = useState(`/opt/nagi/${name}`);
  const [witness, setWitness] = useState("");
  const [auto, setAuto] = useState(false);
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
              </div>
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
                    }),
                  )
                }
              >
                设置顶替
              </Button>
            </div>
          </>
        )}
      </CardContent>
    </Card>
  );
}

// NodeFailovers：节点参与的顶替（热备节点上可以确认顶替）。
export function NodeFailovers({ nodeId }: { nodeId: string }) {
  const q = useQuery({
    queryKey: ["node-failovers", nodeId],
    queryFn: () => nodeClient.listNodeFailovers({ nodeId }),
    refetchInterval: 3000,
  });
  const [error, setError] = useState("");
  if (!q.data || q.data.failovers.length === 0) return null;
  return (
    <div className="grid gap-1 py-2 text-xs">
      <div className="font-medium">顶替</div>
      {error && <Alert>{error}</Alert>}
      {q.data.failovers.map((f) => (
        <div
          key={f.config?.projectId}
          className="flex flex-wrap items-center gap-3"
        >
          <span>{f.config?.projectName}</span>
          <Badge>{roleLabel[f.role] ?? f.role}</Badge>
          <span>
            持有者 {f.holderName}（第 {f.epoch.toString()} 轮）
          </span>
          {f.pendingConfirm && (
            <Button
              size="sm"
              onClick={async () => {
                if (!confirm("确认顶替？会丢失最后一次同步之后写入的数据。"))
                  return;
                setError("");
                try {
                  await nodeClient.confirmNodeTakeover({
                    nodeId,
                    projectId: f.config?.projectId ?? "",
                  });
                  q.refetch();
                } catch (err) {
                  setError(errorMessage(err));
                }
              }}
            >
              原节点失联，确认顶替
            </Button>
          )}
          {f.lastError && (
            <span className="text-destructive">{f.lastError}</span>
          )}
        </div>
      ))}
    </div>
  );
}
