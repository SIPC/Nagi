import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { errorMessage, nodeClient, projectClient } from "@/lib/api";
import type { Project } from "@/gen/nagi/v1/common_pb";
import { Alert, Button, Check, DialogActions, EmptyRow, Field, Input, List, QueryError, Row, RowsSkeleton, Section, Spinner, Status } from "@/components/ui";
import { t } from "@/i18n";
import { confirmAction } from "@/components/confirm";

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
export function HandOver({ nodes, onClose }: { nodes: { id: string; name: string; online: boolean }[]; onClose: () => void }) {
  const queryClient = useQueryClient();
  const [code, setCode] = useState("");
  const [selected, setSelected] = useState<string[]>([]);
  const [busy, setBusy] = useState(false);
  const [results, setResults] = useState<HandOverResult[]>([]);
  // error：整个请求失败（例如接入码不对），可以改了再提交。
  const [error, setError] = useState("");
  const [missing, setMissing] = useState<{ code?: boolean; nodes?: boolean }>({});
  const projects = useQuery({ queryKey: ["projects"], queryFn: () => projectClient.listProjects({}) });
  const projectList = (projects.data?.projects ?? []).map((p) => p.project!).filter((p) => p && p.containers.length > 0);
  // 项目的相关节点都选中了，就算选中了这个项目。
  const projectSelected = (p: Project) => relatedNodes(p).every((n) => selected.includes(n));
  const toggleProject = (p: Project, on: boolean) => {
    const nodes = relatedNodes(p);
    setSelected(on ? [...new Set([...selected, ...nodes])] : selected.filter((n) => !nodes.includes(n)));
  };

  const run = async () => {
    const lacks = { code: !code.trim(), nodes: selected.length === 0 };
    setMissing(lacks);
    if (lacks.code || lacks.nodes) return;
    if (!(await confirmAction(t.panels.handover.confirm(selected.length), { action: t.panels.handover.submit, danger: true }))) return;
    setBusy(true);
    setError("");
    const name = (id: string) => nodes.find((n) => n.id === id)?.name ?? id;
    try {
      const res = await nodeClient.handoverNodes({ nodeIds: selected, invite: code });
      setResults(res.results.map((r) => ({ name: name(r.nodeId), ok: !r.error, error: r.error })));
      setSelected([]);
    } catch (err) {
      setError(errorMessage(err));
    }
    setBusy(false);
    queryClient.invalidateQueries({ queryKey: ["nodes"] });
  };

  return (
    <div className="grid gap-4">
        <Field label={t.panels.handover.code} help={t.panels.handover.intro} htmlFor="handover-code" error={missing.code && !code.trim() ? t.panels.handover.codeRequired : undefined}>
          <Input id="handover-code" placeholder={t.panels.handover.codePlaceholder} value={code} onChange={(e) => setCode(e.target.value)} autoComplete="off" spellCheck={false} />
        </Field>
        {projects.error && !projects.data && <QueryError error={errorMessage(projects.error)} onRetry={() => projects.refetch()} />}
        {projectList.length > 0 && (
          <Field label={t.panels.handover.byProject} help={t.panels.handover.byProjectHelp}>
            {/* 字段名是一组复选框的名称，没有对应的单个输入框，所以用 group 关联。 */}
            <div role="group" aria-label={t.panels.handover.byProject} className="flex flex-wrap gap-x-4 gap-y-2">
              {projectList.map((p) => (
                <Check key={p.id} checked={projectSelected(p)} onChange={(v) => toggleProject(p, v)}>
                  {p.name}
                </Check>
              ))}
            </div>
          </Field>
        )}
        <Field label={t.panels.handover.nodes} error={missing.nodes && selected.length === 0 ? t.panels.handover.nodesRequired : undefined}>
          <div role="group" aria-label={t.panels.handover.nodes} className="flex flex-wrap gap-x-4 gap-y-2">
            {nodes.map((n) => (
              <Check
                key={n.id}
                disabled={!n.online}
                checked={selected.includes(n.id)}
                onChange={(v) => setSelected(v ? [...selected, n.id] : selected.filter((x) => x !== n.id))}
              >
                {n.name}
                {!n.online && t.panels.handover.offline}
              </Check>
            ))}
          </div>
        </Field>
        {error && <Alert>{error}</Alert>}
        {results.length > 0 && (
          <List>
            {results.map((r, i) => (
              <Row
                key={i}
                title={r.name}
                sub={r.ok ? undefined : r.error}
                cols={[{ content: <Status tone={r.ok ? "success" : "danger"}>{r.ok ? t.panels.handover.handedOver : t.panels.handover.failed}</Status>, phone: true }]}
              />
            ))}
          </List>
        )}
      {/* 出了结果就是结果页：只留「完成」，不能再提交一次。 */}
      <DialogActions>
        {results.length > 0 ? (
          <Button readOnlyOk onClick={onClose}>
            {t.panels.handover.finish}
          </Button>
        ) : (
          <>
            <Button variant="outline" readOnlyOk onClick={onClose}>
              {t.common.actions.cancel}
            </Button>
            <Button variant="destructive" disabled={busy} onClick={run}>
              {busy && <Spinner />}
              {busy ? t.panels.handover.processing : t.panels.handover.submit}
            </Button>
          </>
        )}
      </DialogActions>
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
  return (
    <Section title={t.panels.nodePanels.title} help={t.panels.nodePanels.intro}>
      {error && <Alert>{error}</Alert>}
      {q.isPending ? (
        <RowsSkeleton />
      ) : q.error ? (
        <QueryError error={errorMessage(q.error)} onRetry={() => q.refetch()} />
      ) : q.data.panels.length === 0 ? (
        <EmptyRow>{t.panels.nodePanels.empty}</EmptyRow>
      ) : (
        <List>
          {q.data.panels.map((p) => {
            const key = p.fingerprint.replace("sha256//", "");
            const roles = [p.isSelf && t.panels.nodePanels.self, p.role === "primary" && t.panels.nodePanels.primary, p.role === "standby" && t.panels.nodePanels.standby].filter(Boolean).join(" · ");
            return (
              <Row
                key={p.fingerprint}
                title={
                  <>
                    <span className="truncate">{p.address}</span>
                    {roles && <span className="shrink-0 text-muted-foreground">{roles}</span>}
                  </>
                }
                sub={
                  <>
                    <span title={p.fingerprint}>{`${t.panels.nodePanels.publicKey} ${key.slice(0, 8)}`}</span>
                    {!p.connected && p.lastError && (
                      <>
                        {" · "}
                        <span className="text-warning-text">{p.lastError}</span>
                      </>
                    )}
                  </>
                }
                // 英文「Not connected」放不进默认的列宽。
                cols={[{ content: p.connected ? <Status tone="success">{t.panels.nodePanels.connected}</Status> : <Status>{t.panels.nodePanels.disconnected}</Status>, phone: true, width: "w-32" }]}
                actions={
                  <Button
                    size="row"
                    variant="ghost"
                    // 不能通过面板移除最后一个面板（见 docs/control-plane.md）。
                    disabled={q.data.panels.length === 1}
                    title={q.data.panels.length === 1 ? t.panels.nodePanels.lastPanel : undefined}
                    onClick={async () => {
                      const msg = p.isSelf ? t.panels.nodePanels.removeSelfConfirm : t.panels.nodePanels.removeConfirm(p.address, key.slice(0, 12));
                      if (!(await confirmAction(msg, { action: t.panels.nodePanels.remove, danger: true }))) return;
                      setError("");
                      try {
                        await nodeClient.removeNodePanel({ nodeId, fingerprint: p.fingerprint });
                        queryClient.invalidateQueries({ queryKey: p.isSelf ? ["nodes"] : ["node-panels", nodeId] });
                      } catch (err) {
                        setError(errorMessage(err));
                      }
                    }}
                  >
                    {t.common.actions.remove}
                  </Button>
                }
              />
            );
          })}
        </List>
      )}
    </Section>
  );
}
