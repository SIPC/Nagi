import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { errorMessage, nodeClient, toDate } from "@/lib/api";
import { slugName } from "@/lib/utils";
import { fmtBytes } from "@/components/metrics";
import { roleLabel } from "@/pages/backup";
import { Alert, Badge, Button, Input, Select, Spinner } from "@/components/ui";

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
    <div className="grid gap-2 rounded border p-2">
      <div className={outcome.ok ? "text-success" : "text-destructive"}>
        {outcome.ok ? "✅" : "❌"} {outcome.name}：{outcome.message}
      </div>
      <div className="flex gap-2">
        {outcome.created && (
          <Link to="/projects/$nodeId/$projectId" params={{ nodeId, projectId: outcome.projectId }}>
            <Button size="sm" variant="outline">
              打开项目
            </Button>
          </Link>
        )}
        <Button size="sm" variant="ghost" onClick={() => setOutcome(null)}>
          关闭
        </Button>
      </div>
      {outcome.lines.length > 0 && (
        <pre className="max-h-60 overflow-auto rounded bg-zinc-950 p-3 font-mono text-zinc-200">{outcome.lines.join("\n")}</pre>
      )}
    </div>
  );
  if (q.isPending) return <Spinner />;
  if (q.error) return <Alert>{errorMessage(q.error)}</Alert>;
  if (q.data.replicas.length === 0)
    return (
      <div className="grid gap-2 py-2 text-xs">
        {outcomeView}
        <div className="text-muted-foreground">这个节点上没有副本</div>
      </div>
    );
  return (
    <div className="grid gap-2 py-2 text-xs">
      {outcomeView}
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
              onDone={setOutcome}
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
      for await (const ev of nodeClient.restoreNodeReplica({
        nodeId,
        projectId,
        dir,
        snapshotId: snap,
      })) {
        if (ev.event.case === "output") {
          const l = ev.event.value;
          out.push(l);
          setLines((x) => [...x, l]);
        } else if (ev.event.case === "result") {
          ok = ev.event.value.success;
          message = ok ? "已恢复成项目并启动" : ev.event.value.error;
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
      {busy && lines.length > 0 && (
        <pre className="max-h-60 overflow-auto rounded bg-zinc-950 p-3 font-mono text-zinc-200">
          {lines.join("\n")}
        </pre>
      )}
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
