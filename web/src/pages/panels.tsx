import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { errorMessage, nodeClient, projectClient } from "@/lib/api";
import type { Project } from "@/gen/nagi/v1/common_pb";
import { Alert, Badge, Button, Check, Field, Loading, Spinner, Status, Textarea } from "@/components/ui";

// ---- 移交给其他面板 ----

type HandOverResult = { name: string; ok: boolean; error: string };

// relatedNodes 返回和项目有关的节点：容器所在、备份节点、顶替的两方和作为见证人的节点。移交按整个项目，这些节点要一起选。
function relatedNodes(p: Project) {
  const out = new Set<string>();
  for (const c of p.containers) {
    out.add(c.nodeId);
    if (c.backupNodeId) out.add(c.backupNodeId);
    for (const n of [c.failover?.a?.nodeId, c.failover?.b?.nodeId, c.failover?.witness?.nodeId]) if (n) out.add(n);
  }
  return [...out];
}

// HandOver：把勾选的节点移交给另一个面板（放在对话框里）。一个节点同一时间只归一个面板管理。
export function HandOver({ nodes }: { nodes: { id: string; name: string; online: boolean }[] }) {
  const queryClient = useQueryClient();
  const [code, setCode] = useState("");
  const [selected, setSelected] = useState<string[]>([]);
  const [busy, setBusy] = useState(false);
  const [results, setResults] = useState<HandOverResult[]>([]);
  const projects = useQuery({ queryKey: ["projects"], queryFn: () => projectClient.listProjects({}) });
  const projectList = (projects.data?.projects ?? []).map((p) => p.project!).filter((p) => p && p.containers.length > 0);
  // 项目的相关节点都选中了，就算选中了这个项目。
  const projectSelected = (p: Project) => relatedNodes(p).every((n) => selected.includes(n));
  const toggleProject = (p: Project, on: boolean) => {
    const nodes = relatedNodes(p);
    setSelected(on ? [...new Set([...selected, ...nodes])] : selected.filter((n) => !nodes.includes(n)));
  };

  const run = async () => {
    if (!confirm(`移交选中的 ${selected.length} 个节点？移交后本面板将不再管理这些节点和只在这些节点上的项目，新面板从节点上的缓存恢复这些项目。`)) return;
    setBusy(true);
    const name = (id: string) => nodes.find((n) => n.id === id)?.name ?? id;
    try {
      const res = await nodeClient.handoverNodes({ nodeIds: selected, invite: code });
      setResults(res.results.map((r) => ({ name: name(r.nodeId), ok: !r.error, error: r.error })));
    } catch (err) {
      setResults([{ name: "移交", ok: false, error: errorMessage(err) }]);
    }
    setBusy(false);
    setSelected([]);
    queryClient.invalidateQueries({ queryKey: ["nodes"] });
  };

  return (
    <div className="grid gap-4">
      <p className="text-[13px] text-muted-foreground">
        接入码在新面板的「设置」页生成。新面板接管后，本面板从节点上移除自身，不再管理这些节点。移交按整个项目进行，项目相关的节点（容器所在、顶替、备份、见证）须一并选中。
      </p>
        <Field label="接入码" hint="格式：nagi-invite:…" htmlFor="handover-code">
          <Textarea
            id="handover-code"
            className="font-mono text-xs"
            rows={2}
            placeholder="粘贴其他面板生成的接入码"
            value={code}
            onChange={(e) => setCode(e.target.value)}
          />
        </Field>
        {projectList.length > 0 && (
          <Field label="按项目选择" help="选中项目会同时选中它的容器所在节点、备份节点、顶替节点和作为见证人的节点。">
            <div className="flex flex-wrap gap-x-5 gap-y-2">
              {projectList.map((p) => (
                <Check key={p.id} checked={projectSelected(p)} onChange={(v) => toggleProject(p, v)}>
                  {p.name}
                </Check>
              ))}
            </div>
          </Field>
        )}
        <Field label="节点">
          <div className="flex flex-wrap gap-x-5 gap-y-2">
            {nodes.map((n) => (
              <Check
                key={n.id}
                disabled={!n.online}
                checked={selected.includes(n.id)}
                onChange={(v) => setSelected(v ? [...selected, n.id] : selected.filter((x) => x !== n.id))}
              >
                {n.name}
                {!n.online && <span className="text-muted-foreground">（离线）</span>}
              </Check>
            ))}
          </div>
        </Field>
        {results.length > 0 && (
          <div className="grid gap-1.5 rounded-md border bg-subtle px-3.5 py-2.5">
            {results.map((r, i) => (
              <div key={i} className="flex flex-wrap items-center gap-x-3 gap-y-1">
                <Status tone={r.ok ? "success" : "danger"}>{r.name}</Status>
                <span className={r.ok ? "text-[13px] text-muted-foreground" : "text-[13px] text-destructive"}>{r.ok ? "已完成" : r.error}</span>
              </div>
            ))}
          </div>
        )}
      <div className="flex justify-end border-t pt-4">
        <Button size="sm" disabled={busy || !code || selected.length === 0} onClick={run}>
          {busy && <Spinner className="size-3.5" />}
          {busy ? "处理中…" : "移交节点"}
        </Button>
      </div>
    </div>
  );
}

// NodePanels：节点信任的面板列表。
export function NodePanels({ nodeId }: { nodeId: string }) {
  const queryClient = useQueryClient();
  const q = useQuery({
    queryKey: ["node-panels", nodeId],
    queryFn: () => nodeClient.listNodePanels({ nodeId }),
  });
  const [error, setError] = useState("");
  if (q.isPending) return <Loading />;
  if (q.error) return <Alert>{errorMessage(q.error)}</Alert>;
  return (
    <div className="grid gap-3">
      <p className="text-[13px] text-muted-foreground">节点接受这些面板的连接和操作。</p>
      {error && <Alert>{error}</Alert>}
      <div className="divide-y rounded-md border">
        {q.data.panels.length === 0 && <p className="px-4 py-6 text-center text-[13px] text-muted-foreground">暂无面板</p>}
        {q.data.panels.map((p) => (
          <div key={p.fingerprint} className="flex flex-wrap items-center gap-x-4 gap-y-1 px-4 py-2.5">
            <div className="min-w-0 flex-1">
              <div className="flex items-center gap-2">
                <span className="font-mono text-[13px]">{p.address}</span>
                {p.isSelf && <Badge>本面板</Badge>}
                {p.role === "primary" && <Badge>主面板</Badge>}
                {p.role === "standby" && <Badge>备用面板</Badge>}
              </div>
              <div className="truncate font-mono text-xs text-muted-foreground" title={p.fingerprint}>
                {p.fingerprint}
              </div>
            </div>
            {p.connected ? <Status tone="success">已连接</Status> : <Status>未连接</Status>}
            <Button
              size="xs"
              variant="ghost"
              onClick={async () => {
                const msg = p.isSelf
                  ? "从节点上移除本面板？本面板将不再管理该节点，节点由其他面板继续管理。"
                  : `从节点上移除面板 ${p.address}？该面板将无法再连接和操作此节点。`;
                if (!confirm(msg)) return;
                setError("");
                try {
                  await nodeClient.removeNodePanel({ nodeId, fingerprint: p.fingerprint });
                  queryClient.invalidateQueries({ queryKey: p.isSelf ? ["nodes"] : ["node-panels", nodeId] });
                } catch (err) {
                  setError(errorMessage(err));
                }
              }}
            >
              移除
            </Button>
          </div>
        ))}
      </div>
    </div>
  );
}
