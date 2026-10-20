import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { errorMessage, nodeClient, toDate } from "@/lib/api";
import { slugName } from "@/lib/utils";
import { fmtBytes } from "@/components/metrics";
import { roleLabel } from "@/pages/backup";
import { Alert, Badge, Button, Field, Input, Loading, LogBox, Notice, Select, Spinner } from "@/components/ui";

// 节点页上的副本和顶替：节点作为备份节点、热备保存的项目副本，以及它参与的顶替。

// RestoreOutcome 是一次"恢复成项目"的结果。恢复以后副本变成了项目、从列表里消失，结果放在列表外面显示。
type RestoreOutcome = { projectId: string; name: string; ok: boolean; created: boolean; message: string; lines: string[] };

// NodeReplicas：节点作为备份节点保存的副本，可以恢复成项目。
export function NodeReplicas({ nodeId }: { nodeId: string }) {
  const queryClient = useQueryClient();
  const q = useQuery({
    queryKey: ["replicas", nodeId],
    queryFn: () => nodeClient.listNodeReplicas({ nodeId }),
  });
  const [open, setOpen] = useState<string | null>(null);
  const [outcome, setOutcome] = useState<RestoreOutcome | null>(null);
  const outcomeView = outcome && (
    <div className="grid gap-2">
      <Notice
        tone={outcome.ok ? "success" : "danger"}
        title={outcome.name}
        actions={
          <>
            {outcome.created && (
              <Link to="/projects/$nodeId/$projectId" params={{ nodeId, projectId: outcome.projectId }}>
                <Button size="xs" variant="outline">
                  打开项目
                </Button>
              </Link>
            )}
            <Button size="xs" variant="ghost" onClick={() => setOutcome(null)}>
              关闭
            </Button>
          </>
        }
      >
        {outcome.message}
      </Notice>
      {outcome.lines.length > 0 && <LogBox className="max-h-60">{outcome.lines.join("\n")}</LogBox>}
    </div>
  );
  if (q.isPending) return <Loading />;
  if (q.error) return <Alert>{errorMessage(q.error)}</Alert>;
  return (
    <div className="grid gap-3">
      <p className="text-[13px] text-muted-foreground">本节点作为备份节点保存的项目副本，可恢复为项目。</p>
      {outcomeView}
      <div className="divide-y rounded-md border">
        {q.data.replicas.length === 0 && <p className="px-4 py-6 text-center text-[13px] text-muted-foreground">本节点暂无副本</p>}
        {q.data.replicas.map((r) => (
          <div key={r.projectId} className="grid gap-3 px-4 py-2.5">
            <div className="flex flex-wrap items-center gap-x-4 gap-y-1">
              <div className="min-w-0 flex-1">
                <div className="flex items-center gap-2">
                  <span className="font-medium">{r.projectName}</span>
                  <span className="font-mono text-xs text-muted-foreground">{r.projectId}</span>
                </div>
                <div className="text-xs text-muted-foreground">
                  来自 {r.sourceName} · {fmtBytes(Number(r.bytes))} ·{" "}
                  {r.lastSync ? `同步于 ${toDate(r.lastSync)?.toLocaleString()}` : "尚未同步"}
                </div>
              </div>
              <div className="flex items-center gap-2">
                <Button size="xs" variant="outline" onClick={() => setOpen(open === r.projectId ? null : r.projectId)}>
                  恢复为项目
                </Button>
                <Button
                  size="xs"
                  variant="ghost"
                  onClick={async () => {
                    if (!confirm(`删除 ${r.projectName} 的副本？本节点上保存的副本数据将被删除。`)) return;
                    await nodeClient.deleteNodeReplica({ nodeId, projectId: r.projectId });
                    queryClient.invalidateQueries({ queryKey: ["replicas", nodeId] });
                  }}
                >
                  删除
                </Button>
              </div>
            </div>
            {open === r.projectId && <RestoreReplica nodeId={nodeId} projectId={r.projectId} name={r.projectName} onDone={setOutcome} />}
          </div>
        ))}
      </div>
    </div>
  );
}

function RestoreReplica({
  nodeId,
  projectId,
  name,
  onDone,
}: {
  nodeId: string;
  projectId: string;
  name: string;
  onDone: (o: RestoreOutcome) => void;
}) {
  const queryClient = useQueryClient();
  const snaps = useQuery({
    queryKey: ["replica-snapshots", nodeId, projectId],
    queryFn: () => nodeClient.listNodeReplicaSnapshots({ nodeId, projectId }),
  });
  const [dir, setDir] = useState(`/srv/${slugName(name, projectId.slice(0, 8))}`);
  const [snap, setSnap] = useState("");
  const [lines, setLines] = useState<string[]>([]);
  const [busy, setBusy] = useState(false);

  const restore = async () => {
    setBusy(true);
    setLines([]);
    const out: string[] = [];
    let ok = false;
    let message = "";
    try {
      for await (const ev of nodeClient.restoreNodeReplica({ nodeId, projectId, dir, snapshotId: snap })) {
        if (ev.event.case === "output") {
          const l = ev.event.value;
          out.push(l);
          setLines((x) => [...x, l]);
        } else if (ev.event.case === "result") {
          ok = ev.event.value.success;
          message = ok ? "已恢复为项目并启动" : ev.event.value.error;
        }
      }
    } catch (err) {
      message = errorMessage(err);
    } finally {
      setBusy(false);
      // 失败时项目也可能已经建好了（例如恢复了但没有启动），看错误信息开头。
      onDone({ projectId, name, ok, created: ok || message.startsWith("项目已恢复"), message, lines: out });
      queryClient.invalidateQueries({ queryKey: ["replicas", nodeId] });
      queryClient.invalidateQueries({ queryKey: ["projects"] });
    }
  };

  const withData = snaps.data?.snapshots.filter((s) => s.kind !== "version") ?? [];
  return (
    <div className="grid gap-3 rounded-md border bg-subtle p-3.5">
      {snaps.error && <Alert>{errorMessage(snaps.error)}</Alert>}
      <div className="flex flex-wrap items-end gap-3">
        <Field label="项目目录" help="项目恢复到节点上的该目录，需为绝对路径。" htmlFor={`restore-dir-${projectId}`} className="w-72">
          <Input id={`restore-dir-${projectId}`} className="font-mono" value={dir} onChange={(e) => setDir(e.target.value)} />
        </Field>
        <Field label="快照" htmlFor={`restore-snap-${projectId}`} className="w-72">
          <Select id={`restore-snap-${projectId}`} value={snap} onChange={(e) => setSnap(e.target.value)}>
            <option value="">最新快照</option>
            {withData.map((s) => (
              <option key={s.id} value={s.id}>
                {toDate(s.time)?.toLocaleString()} · {s.kind} · {fmtBytes(Number(s.size))}
              </option>
            ))}
          </Select>
        </Field>
        <Button size="sm" disabled={busy} onClick={restore}>
          {busy && <Spinner className="size-3.5" />}
          恢复
        </Button>
      </div>
      {busy && lines.length > 0 && <LogBox className="max-h-60">{lines.join("\n")}</LogBox>}
    </div>
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
    <div className="grid gap-2">
      <div className="text-[13px] font-medium">顶替</div>
      {error && <Alert>{error}</Alert>}
      <div className="divide-y rounded-md border">
        {q.data.failovers.map((f) => (
          <div key={f.config?.projectId} className="flex flex-wrap items-center gap-x-3 gap-y-1 px-4 py-2.5 text-[13px]">
            <span className="font-medium">{f.config?.projectName}</span>
            <Badge>{roleLabel[f.role] ?? f.role}</Badge>
            <span className="text-muted-foreground">
              持有者 {f.holderName}（第 {f.epoch.toString()} 轮）
            </span>
            {f.lastError && <span className="text-destructive">{f.lastError}</span>}
            {f.pendingConfirm && (
              <Button
                size="xs"
                className="ml-auto"
                title="原节点已失联，等待确认顶替"
                onClick={async () => {
                  if (!confirm(`确认顶替项目 ${f.config?.projectName ?? ""}？最后一次同步之后写入的数据将丢失。`)) return;
                  setError("");
                  try {
                    await nodeClient.confirmNodeTakeover({ nodeId, projectId: f.config?.projectId ?? "" });
                    q.refetch();
                  } catch (err) {
                    setError(errorMessage(err));
                  }
                }}
              >
                确认顶替
              </Button>
            )}
          </div>
        ))}
      </div>
    </div>
  );
}
