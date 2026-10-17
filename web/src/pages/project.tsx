import { useEffect, useRef, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate } from "@tanstack/react-router";
import { ArrowLeft, Rocket, ScrollText } from "lucide-react";
import { errorMessage, projectClient, toDate } from "@/lib/api";
import { cn, timeAgo } from "@/lib/utils";
import { PageHeader } from "@/pages/layout";
import { ProjectState, ServiceState } from "@/components/status";
import { Alert, Badge, Button, Card, CardContent, CardHeader, CardTitle, Input, Select, Spinner, Textarea } from "@/components/ui";
import { envToText, lines, textToEnv } from "@/lib/config";
import { TerminalCard } from "@/components/terminal";
import type { Project } from "@/gen/nagi/v1/common_pb";

export function ProjectPage({ nodeId, projectId }: { nodeId: string; projectId: string }) {
  const project = useQuery({
    queryKey: ["project", nodeId, projectId],
    queryFn: () => projectClient.getNodeProject({ nodeId, projectId }),
    refetchInterval: 5000,
  });

  if (project.isPending) return <Spinner />;
  if (project.error) {
    return (
      <>
        <BackLink />
        <Alert>{errorMessage(project.error)}</Alert>
      </>
    );
  }
  const p = project.data.project?.project;
  const services = project.data.project?.status?.services ?? [];
  if (!p) return null;

  return (
    <>
      <BackLink />
      <PageHeader
        title={
          <span className="flex items-center gap-3">
            {p.name}
            <ProjectState services={services} deployed={p.deployedVersion > 0n} />
          </span>
        }
        description={
          <span className="font-mono text-xs">
            {p.dir} · 版本 {p.latestVersion.toString()}
            {p.deployedVersion > 0n ? `（已部署 ${p.deployedVersion}）` : "（尚未部署）"} · 创建于 {timeAgo(toDate(p.createdAt))}
          </span>
        }
      />

      <div className="grid gap-6">
        <DeployCard nodeId={nodeId} projectId={projectId} revision={p.revision} hasDraft={p.hasDraft} hasGit={!!(p.draft ?? p.config)?.source?.gitUrl} />
        <SourceInfo project={p} />

        <Card>
          <CardHeader>
            <CardTitle>服务</CardTitle>
          </CardHeader>
          <CardContent>
            <div className="overflow-x-auto">
              <table className="w-full text-sm">
                <thead className="text-left text-xs text-muted-foreground">
                  <tr className="border-b">
                    <th className="py-2 pr-4 font-medium">服务</th>
                    <th className="py-2 pr-4 font-medium">状态</th>
                    <th className="py-2 pr-4 font-medium">镜像</th>
                    <th className="py-2 font-medium">详情</th>
                  </tr>
                </thead>
                <tbody>
                  {services.map((s) => (
                    <tr key={s.name + s.containerId} className="border-b last:border-0">
                      <td className="py-2.5 pr-4 font-medium">{s.name}</td>
                      <td className="py-2.5 pr-4">
                        <ServiceState service={s} />
                      </td>
                      <td className="py-2.5 pr-4 font-mono text-xs text-muted-foreground">{s.image}</td>
                      <td className="py-2.5 text-xs text-muted-foreground">{s.status}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </CardContent>
        </Card>

        <LogsCard
          nodeId={nodeId}
          projectId={projectId}
          services={services.map((s) => s.name)}
          containers={services.map((s) => s.containerId).filter(Boolean).join(",")}
        />

        <TerminalCard nodeId={nodeId} projectId={projectId} services={services.filter((s) => s.state === "running").map((s) => s.name)} />
        <RoutesCard nodeId={nodeId} project={p} services={services.map((s) => s.name)} />
        <ConfigCard nodeId={nodeId} project={p} services={services.map((s) => s.name)} />
        <HistoryCard nodeId={nodeId} project={p} />
        <DeleteCard nodeId={nodeId} project={p} />
      </div>
    </>
  );
}

function BackLink() {
  return (
    <Link to="/projects" className="mb-4 inline-flex items-center gap-1 text-sm text-muted-foreground hover:text-foreground">
      <ArrowLeft className="size-4" />
      所有项目
    </Link>
  );
}

type DeployState = { running: boolean; lines: string[]; result?: { success: boolean; error: string } };

function DeployCard({
  nodeId,
  projectId,
  revision,
  hasDraft,
  hasGit,
}: {
  nodeId: string;
  projectId: string;
  revision: bigint;
  hasDraft: boolean;
  hasGit: boolean;
}) {
  const queryClient = useQueryClient();
  const [state, setState] = useState<DeployState>({ running: false, lines: [] });
  const outRef = useRef<HTMLPreElement>(null);

  useEffect(() => {
    outRef.current?.scrollTo({ top: outRef.current.scrollHeight });
  }, [state.lines.length]);

  const deploy = async (pull: boolean) => {
    setState({ running: true, lines: [] });
    try {
      for await (const ev of projectClient.deployNodeProject({ nodeId, projectId, baseRevision: revision, pull })) {
        if (ev.event.case === "output") {
          const line = ev.event.value;
          setState((s) => ({ ...s, lines: [...s.lines, line] }));
        } else if (ev.event.case === "result") {
          const r = ev.event.value;
          setState((s) => ({ ...s, result: { success: r.success, error: r.error } }));
        }
      }
    } catch (err) {
      setState((s) => ({ ...s, result: { success: false, error: errorMessage(err) } }));
    } finally {
      setState((s) => ({ ...s, running: false }));
      queryClient.invalidateQueries({ queryKey: ["project", nodeId, projectId] });
      queryClient.invalidateQueries({ queryKey: ["projects"] });
    }
  };

  return (
    <Card>
      <CardHeader className="flex-row items-center justify-between gap-4">
        <div className="grid gap-1">
          <CardTitle>部署</CardTitle>
          <p className="text-sm text-muted-foreground">
            {hasDraft ? "有未部署的修改：部署时会把草稿保存为新版本。" : "按当前版本执行 docker compose up。"}
          </p>
        </div>
        <div className="flex gap-2">
          {hasGit && (
            <Button variant="outline" onClick={() => deploy(true)} disabled={state.running}>
              拉取并部署
            </Button>
          )}
          <Button onClick={() => deploy(false)} disabled={state.running}>
            {state.running ? <Spinner /> : <Rocket />}
            {state.running ? "部署中" : hasDraft ? "部署草稿" : "重新部署"}
          </Button>
        </div>
      </CardHeader>
      {(state.lines.length > 0 || state.result) && (
        <CardContent className="grid gap-3">
          {state.result &&
            (state.result.success ? (
              <Badge tone="success" dot className="w-fit">
                部署成功
              </Badge>
            ) : (
              <Alert>{state.result.error || "部署失败"}</Alert>
            ))}
          {state.lines.length > 0 && (
            <pre
              ref={outRef}
              className="max-h-72 overflow-auto rounded-md bg-zinc-950 p-4 font-mono text-xs leading-relaxed text-zinc-200"
            >
              {state.lines.join("\n")}
            </pre>
          )}
        </CardContent>
      )}
    </Card>
  );
}

type Line = { key: number; service: string; stream: string; text: string };

const maxLines = 2000;

function LogsCard({
  nodeId,
  projectId,
  services,
  containers,
}: {
  nodeId: string;
  projectId: string;
  services: string[];
  containers: string;
}) {
  const [service, setService] = useState("");
  const [lines, setLines] = useState<Line[]>([]);
  const [error, setError] = useState("");
  const boxRef = useRef<HTMLDivElement>(null);
  const stick = useRef(true);

  useEffect(() => {
    const abort = new AbortController();
    let n = 0;
    setLines([]);
    setError("");
    (async () => {
      try {
        const stream = projectClient.streamNodeProjectLogs(
          { nodeId, projectId, service, tail: 200, follow: true },
          { signal: abort.signal },
        );
        for await (const l of stream) {
          const line = { key: n++, service: l.service, stream: l.stream, text: l.text };
          setLines((prev) => {
            const next = prev.length >= maxLines ? prev.slice(prev.length - maxLines + 1) : prev.slice();
            next.push(line);
            return next;
          });
        }
      } catch (err) {
        if (!abort.signal.aborted) setError(errorMessage(err));
      }
    })();
    return () => abort.abort();
    // 容器变化（例如部署后容器被重建）时重新订阅。
  }, [nodeId, projectId, service, containers]);

  useEffect(() => {
    const box = boxRef.current;
    if (box && stick.current) box.scrollTop = box.scrollHeight;
  }, [lines]);

  return (
    <Card>
      <CardHeader className="flex-row items-center justify-between gap-4">
        <CardTitle className="flex items-center gap-2">
          <ScrollText className="size-4" />
          日志
        </CardTitle>
        <Select value={service} onChange={(e) => setService(e.target.value)} className="w-40">
          <option value="">全部服务</option>
          {services.map((s) => (
            <option key={s} value={s}>
              {s}
            </option>
          ))}
        </Select>
      </CardHeader>
      <CardContent>
        {error && <Alert className="mb-3">{error}</Alert>}
        <div
          ref={boxRef}
          onScroll={(e) => {
            const el = e.currentTarget;
            stick.current = el.scrollHeight - el.scrollTop - el.clientHeight < 40;
          }}
          className="h-96 overflow-auto rounded-md bg-zinc-950 p-4 font-mono text-xs leading-relaxed text-zinc-200"
        >
          {lines.length === 0 ? (
            <span className="text-zinc-500">暂无日志</span>
          ) : (
            lines.map((l) => (
              <div key={l.key} className="whitespace-pre-wrap break-all">
                {!service && <span className="mr-2 text-sky-400">{l.service}</span>}
                <span className={cn(l.stream === "stderr" && "text-amber-200")}>{l.text}</span>
              </div>
            ))
          )}
        </div>
      </CardContent>
    </Card>
  );
}

function ConfigCard({ nodeId, project, services }: { nodeId: string; project: Project; services: string[] }) {
  const queryClient = useQueryClient();
  const base = project.draft ?? project.config;
  const isForm = !!base?.form;
  const [compose, setCompose] = useState(base?.compose ?? "");
  const [env, setEnv] = useState(envToText(base?.env));
  const [serviceEnv, setServiceEnv] = useState<Record<string, string>>(() =>
    Object.fromEntries(services.map((s) => [s, envToText(base?.serviceEnv[s]?.vars)])),
  );
  const [formEnv, setFormEnv] = useState(envToText(base?.form?.env));
  const [gitUrl, setGitUrl] = useState(base?.source?.gitUrl ?? "");
  const [gitRef, setGitRef] = useState(base?.source?.gitRef ?? "");
  const [preserve, setPreserve] = useState((base?.preserve ?? []).join("\n"));
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  const refresh = () => {
    queryClient.invalidateQueries({ queryKey: ["project", nodeId, project.id] });
    queryClient.invalidateQueries({ queryKey: ["projects"] });
  };

  const save = async () => {
    setBusy(true);
    setError("");
    try {
      await projectClient.saveNodeProjectDraft({
        nodeId,
        projectId: project.id,
        baseRevision: project.revision,
        config: isForm
          ? {
              env: textToEnv(env),
              form: { ...base!.form!, env: textToEnv(formEnv) },
              source: { gitUrl, gitRef },
              preserve: lines(preserve),
            }
          : {
              source: { gitUrl, gitRef },
              preserve: lines(preserve),
              compose,
              env: textToEnv(env),
              serviceEnv: Object.fromEntries(
                Object.entries(serviceEnv)
                  .map(([s, t]) => [s, { vars: textToEnv(t) }] as const)
                  .filter(([, l]) => l.vars.length > 0),
              ),
            },
      });
      refresh();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const discard = async () => {
    setBusy(true);
    setError("");
    try {
      await projectClient.discardNodeProjectDraft({ nodeId, projectId: project.id, baseRevision: project.revision });
      // 放弃草稿后用最新版本的内容重新初始化编辑框。
      location.reload();
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };

  return (
    <Card>
      <CardHeader className="flex-row items-center justify-between gap-4">
        <CardTitle className="flex items-center gap-2">
          配置
          {project.hasDraft && <Badge tone="warning">草稿</Badge>}
        </CardTitle>
        <div className="flex gap-2">
          {project.hasDraft && (
            <Button variant="outline" size="sm" onClick={discard} disabled={busy}>
              放弃草稿
            </Button>
          )}
          <Button size="sm" onClick={save} disabled={busy}>
            {busy && <Spinner />}
            保存草稿
          </Button>
        </div>
      </CardHeader>
      <CardContent className="grid gap-4">
        {error && <Alert>{error}</Alert>}
        {isForm ? (
          <div className="grid gap-1.5">
            <span className="text-sm font-medium">compose（由镜像表单生成，镜像 {base?.form?.image}）</span>
          </div>
        ) : (
          <div className="grid gap-1.5">
            <span className="text-sm font-medium">compose</span>
            <Textarea
              value={compose}
              onChange={(e) => setCompose(e.target.value)}
              spellCheck={false}
              className="min-h-56 font-mono text-[13px] leading-relaxed"
            />
          </div>
        )}
        <div className="grid gap-3 sm:grid-cols-3">
          <div className="grid gap-1.5 sm:col-span-2">
            <span className="text-sm font-medium">git 仓库（SSH 用 deploy key，HTTPS 只支持公开仓库；为空表示文件自己放进 data/）</span>
            <Input value={gitUrl} onChange={(e) => setGitUrl(e.target.value)} placeholder="git@github.com:you/app.git" className="font-mono" />
          </div>
          <div className="grid gap-1.5">
            <span className="text-sm font-medium">分支或标签</span>
            <Input value={gitRef} onChange={(e) => setGitRef(e.target.value)} placeholder="默认分支" className="font-mono" />
          </div>
        </div>
        <div className="grid gap-1.5">
          <span className="text-sm font-medium">更新文件时保留的路径（相对 data/，每行一个；挂载进容器的目录会自动保留）</span>
          <Textarea value={preserve} onChange={(e) => setPreserve(e.target.value)} spellCheck={false} className="min-h-16 font-mono text-[13px]" placeholder="uploads/" />
        </div>
        <div className="grid gap-1.5">
          <span className="text-sm font-medium">项目级变量（替换 compose 里的 ${"{变量}"}，KEY=VALUE 每行一个）</span>
          <Textarea value={env} onChange={(e) => setEnv(e.target.value)} spellCheck={false} className="font-mono text-[13px]" />
        </div>
        {isForm ? (
          <div className="grid gap-1.5">
            <span className="text-sm font-medium">容器环境变量</span>
            <Textarea value={formEnv} onChange={(e) => setFormEnv(e.target.value)} spellCheck={false} className="font-mono text-[13px]" />
          </div>
        ) : (
          services.map((s) => (
            <div key={s} className="grid gap-1.5">
              <span className="text-sm font-medium">服务 {s} 的环境变量（只注入这个服务的容器）</span>
              <Textarea
                value={serviceEnv[s] ?? ""}
                onChange={(e) => setServiceEnv((prev) => ({ ...prev, [s]: e.target.value }))}
                spellCheck={false}
                className="font-mono text-[13px]"
              />
            </div>
          ))
        )}
      </CardContent>
    </Card>
  );
}
function SourceInfo({ project }: { project: Project }) {
  const cfg = project.draft ?? project.config;
  if (!cfg?.source?.gitUrl) return null;
  const hook = `${location.origin}/hooks/${project.webhookToken}`;
  return (
    <Card>
      <CardHeader>
        <CardTitle>git</CardTitle>
      </CardHeader>
      <CardContent className="grid gap-3 text-sm">
        <div>
          仓库 <span className="font-mono">{cfg.source.gitUrl}</span>
          {cfg.source.gitRef && <span className="font-mono"> @ {cfg.source.gitRef}</span>}
          {project.lastCommit && <span className="text-muted-foreground">，当前提交 {project.lastCommit.slice(0, 12)}</span>}
        </div>
        <div className="grid gap-1">
          <span className="font-medium">deploy key（加到仓库的只读 deploy key）</span>
          <code className="break-all rounded bg-muted p-2 text-xs">{project.deployKey}</code>
        </div>
        <div className="grid gap-1">
          <span className="font-medium">webhook 地址（push 后自动拉取并部署）</span>
          <code className="break-all rounded bg-muted p-2 text-xs">{hook}</code>
        </div>
      </CardContent>
    </Card>
  );
}
function DeleteCard({ nodeId, project }: { nodeId: string; project: Project }) {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [deleteDir, setDeleteDir] = useState(false);
  const [confirmName, setConfirmName] = useState("");
  const [error, setError] = useState("");

  const remove = async () => {
    if (!deleteDir && !confirm(`删除项目 ${project.name}？容器会被删除，项目目录保留。`)) return;
    setError("");
    try {
      await projectClient.deleteNodeProject({
        nodeId,
        projectId: project.id,
        baseRevision: project.revision,
        deleteDir,
        confirmName,
      });
      queryClient.invalidateQueries({ queryKey: ["projects"] });
      navigate({ to: "/projects" });
    } catch (err) {
      setError(errorMessage(err));
    }
  };

  return (
    <Card className="border-destructive/30">
      <CardHeader>
        <CardTitle>删除项目</CardTitle>
      </CardHeader>
      <CardContent className="grid gap-3 text-sm">
        {error && <Alert>{error}</Alert>}
        <p className="text-muted-foreground">停止并删除容器。项目目录（含数据和快照）默认保留在 {project.dir}。</p>
        <label className="flex items-center gap-2">
          <input type="checkbox" checked={deleteDir} onChange={(e) => setDeleteDir(e.target.checked)} />
          同时删除项目目录（数据、快照都会删掉，无法恢复）
        </label>
        {deleteDir && (
          <Input value={confirmName} onChange={(e) => setConfirmName(e.target.value)} placeholder={`输入项目名 ${project.name} 确认`} />
        )}
        <div>
          <Button variant="destructive" size="sm" onClick={remove}>
            删除项目
          </Button>
        </div>
      </CardContent>
    </Card>
  );
}
function RoutesCard({ nodeId, project, services }: { nodeId: string; project: Project; services: string[] }) {
  const queryClient = useQueryClient();
  const [form, setForm] = useState({ name: "", service: "", target: "", protocol: "http", published: "", domains: "", httpOnly: false });
  const [error, setError] = useState("");
  const refresh = () => queryClient.invalidateQueries({ queryKey: ["project", nodeId, project.id] });

  const save = async () => {
    setError("");
    try {
      await projectClient.saveNodeProjectRoute({
        nodeId,
        projectId: project.id,
        route: {
          name: form.name,
          service: form.service || services[0] || "",
          target: Number(form.target),
          protocol: form.protocol,
          published: Number(form.published || 0),
          domains: form.domains.split(/[\s,]+/).filter(Boolean),
          httpOnly: form.httpOnly,
        },
      });
      setForm({ name: "", service: "", target: "", protocol: "http", published: "", domains: "", httpOnly: false });
      refresh();
    } catch (err) {
      setError(errorMessage(err));
    }
  };

  const remove = async (name: string) => {
    setError("");
    try {
      await projectClient.deleteNodeProjectRoute({ nodeId, projectId: project.id, name });
      refresh();
    } catch (err) {
      setError(errorMessage(err));
    }
  };

  return (
    <Card>
      <CardHeader>
        <CardTitle>端口映射</CardTitle>
        <p className="text-sm text-muted-foreground">
          保存后立即生效，不需要重新部署。文件在 {project.dir}/config/routes/，也可以直接在节点上修改。compose 里新出现的 ports 会在部署时自动导入。
        </p>
      </CardHeader>
      <CardContent className="grid gap-3 text-sm">
        {error && <Alert>{error}</Alert>}
        {project.routes.length === 0 ? (
          <p className="text-muted-foreground">还没有映射。</p>
        ) : (
          <table className="w-full">
            <tbody>
              {project.routes.map((r) => (
                <tr key={r.name} className="border-b last:border-0">
                  <td className="py-2 pr-3 font-mono text-xs">{r.name}</td>
                  <td className="py-2 pr-3">
                    {r.error ? (
                      <span className="text-destructive">{r.error}</span>
                    ) : (
                      <span className="font-mono text-xs">
                        {r.service}:{r.target} → {r.domains.length ? r.domains.join(", ") + (r.httpOnly ? "（仅 HTTP）" : "") : `${r.protocol}/${r.published}`}
                      </span>
                    )}
                  </td>
                  <td className="py-2 text-right">
                    <Button
                      variant="ghost"
                      size="sm"
                      onClick={() =>
                        setForm({
                          name: r.name,
                          service: r.service,
                          target: String(r.target),
                          protocol: r.protocol || "http",
                          published: r.published ? String(r.published) : "",
                          domains: r.domains.join(", "),
                          httpOnly: r.httpOnly,
                        })
                      }
                    >
                      编辑
                    </Button>
                    <Button variant="ghost" size="sm" onClick={() => remove(r.name)}>
                      删除
                    </Button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
        <div className="grid gap-2 rounded-md border p-3 sm:grid-cols-6">
          <Select value={form.service || services[0] || ""} onChange={(e) => setForm({ ...form, service: e.target.value })}>
            {services.map((s) => (
              <option key={s}>{s}</option>
            ))}
          </Select>
          <Input placeholder="容器端口" value={form.target} onChange={(e) => setForm({ ...form, target: e.target.value })} />
          <Select value={form.protocol} onChange={(e) => setForm({ ...form, protocol: e.target.value })}>
            <option value="http">http</option>
            <option value="tcp">tcp</option>
            <option value="udp">udp</option>
          </Select>
          <Input placeholder="节点端口" value={form.published} onChange={(e) => setForm({ ...form, published: e.target.value })} />
          <Input placeholder="域名，逗号分隔" value={form.domains} onChange={(e) => setForm({ ...form, domains: e.target.value })} />
          <Button size="sm" onClick={save}>
            {form.name ? `保存 ${form.name}` : "添加映射"}
          </Button>
          <label className="flex items-center gap-2 sm:col-span-6">
            <input type="checkbox" checked={form.httpOnly} onChange={(e) => setForm({ ...form, httpOnly: e.target.checked })} />
            只开 HTTP，不申请证书
          </label>
        </div>
      </CardContent>
    </Card>
  );
}
function HistoryCard({ nodeId, project }: { nodeId: string; project: Project }) {
  const queryClient = useQueryClient();
  const ref = { nodeId, projectId: project.id };
  const versions = useQuery({ queryKey: ["versions", nodeId, project.id, project.revision], queryFn: () => projectClient.listNodeProjectVersions(ref) });
  const snapshots = useQuery({ queryKey: ["snapshots", nodeId, project.id], queryFn: () => projectClient.listNodeProjectSnapshots(ref) });
  const [restoreData, setRestoreData] = useState(false);
  const [lines, setLines] = useState<string[]>([]);
  const [busy, setBusy] = useState(false);

  const run = async (stream: AsyncIterable<{ event: { case: string | undefined; value?: unknown } }>) => {
    setBusy(true);
    setLines([]);
    try {
      for await (const ev of stream) {
        if (ev.event.case === "output") setLines((l) => [...l, ev.event.value as string]);
        if (ev.event.case === "result") {
          const r = ev.event.value as { success: boolean; error: string };
          setLines((l) => [...l, r.success ? "✓ 完成" : "✗ " + r.error]);
        }
      }
    } catch (err) {
      setLines((l) => [...l, "✗ " + errorMessage(err)]);
    } finally {
      setBusy(false);
      queryClient.invalidateQueries({ queryKey: ["project", nodeId, project.id] });
      snapshots.refetch();
      versions.refetch();
    }
  };

  return (
    <Card>
      <CardHeader className="flex-row items-center justify-between gap-4">
        <CardTitle>版本与快照</CardTitle>
        <Button size="sm" variant="outline" disabled={busy} onClick={() => run(projectClient.createNodeProjectSnapshot(ref))}>
          立即快照
        </Button>
      </CardHeader>
      <CardContent className="grid gap-4 text-sm">
        {lines.length > 0 && (
          <pre className="max-h-48 overflow-auto rounded-md bg-zinc-950 p-3 font-mono text-xs text-zinc-200">{lines.join("\n")}</pre>
        )}
        <div className="grid gap-2">
          <div className="flex items-center justify-between">
            <span className="font-medium">版本（每次部署一个）</span>
            <label className="flex items-center gap-2 text-xs">
              <input type="checkbox" checked={restoreData} onChange={(e) => setRestoreData(e.target.checked)} />
              回滚时同时恢复数据
            </label>
          </div>
          <table className="w-full text-xs">
            <tbody>
              {versions.data?.versions.map((v) => (
                <tr key={String(v.version)} className="border-b last:border-0">
                  <td className="py-1.5 pr-3 font-mono">v{String(v.version)}</td>
                  <td className="py-1.5 pr-3 text-muted-foreground">
                    {v.deployedAt ? timeAgo(toDate(v.deployedAt)) : "未部署成功"}
                    {v.rollbackOf > 0n && ` · 回滚自 v${v.rollbackOf}`}
                    {v.commit && ` · ${v.commit.slice(0, 8)}`}
                  </td>
                  <td className="py-1.5 pr-3 font-mono text-muted-foreground">{Object.values(v.images).join(", ").slice(0, 80)}</td>
                  <td className="py-1.5 text-right">
                    {v.version !== project.deployedVersion && v.deployedAt && (
                      <Button
                        size="sm"
                        variant="ghost"
                        disabled={busy}
                        onClick={() => run(projectClient.rollbackNodeProject({ ...ref, version: v.version, restoreData }))}
                      >
                        回滚到这里
                      </Button>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        <div className="grid gap-2">
          <span className="font-medium">快照</span>
          <table className="w-full text-xs">
            <tbody>
              {snapshots.data?.snapshots.map((sn) => (
                <tr key={sn.id} className="border-b last:border-0">
                  <td className="py-1.5 pr-3">{sn.kind}</td>
                  <td className="py-1.5 pr-3 text-muted-foreground">{timeAgo(toDate(sn.time))}</td>
                  <td className="py-1.5 pr-3 text-muted-foreground">
                    {sn.version > 0n && `v${sn.version}`} {(Number(sn.size) / 1024).toFixed(1)} KB
                  </td>
                  <td className="py-1.5 text-right">
                    {(sn.kind === "scheduled" || sn.kind === "manual") && (
                      <Button
                        size="sm"
                        variant="ghost"
                        disabled={busy}
                        onClick={() => confirm("从这个快照恢复 data/ 和 volumes/？会先停止服务。") && run(projectClient.restoreNodeProjectSnapshot({ ...ref, snapshotId: sn.id }))}
                      >
                        恢复
                      </Button>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </CardContent>
    </Card>
  );
}