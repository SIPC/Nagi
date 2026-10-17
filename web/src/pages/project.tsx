import { useEffect, useRef, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { ArrowLeft, Rocket, ScrollText } from "lucide-react";
import { errorMessage, projectClient, toDate } from "@/lib/api";
import { cn, timeAgo } from "@/lib/utils";
import { PageHeader } from "@/pages/layout";
import { ProjectState, ServiceState } from "@/components/status";
import { Alert, Badge, Button, Card, CardContent, CardHeader, CardTitle, Input, Select, Spinner, Textarea } from "@/components/ui";
import { envToText, lines, textToEnv } from "@/lib/config";
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

        <ConfigCard nodeId={nodeId} project={p} services={services.map((s) => s.name)} />
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
  const [exposures, setExposures] = useState(() =>
    JSON.stringify(
      (base?.exposures ?? []).map((e) => ({
        service: e.service,
        target: e.target,
        protocol: e.protocol,
        ...(e.published ? { published: e.published } : {}),
        ...(e.domains.length ? { domains: e.domains } : {}),
        ...(e.httpOnly ? { httpOnly: true } : {}),
      })),
      null,
      2,
    ),
  );
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
    let parsedExposures: { service: string; target: number; protocol: string; published?: number; domains?: string[]; httpOnly?: boolean }[];
    try {
      parsedExposures = JSON.parse(exposures || "[]");
    } catch {
      setError("开放设置不是有效的 JSON");
      setBusy(false);
      return;
    }
    try {
      await projectClient.saveNodeProjectDraft({
        nodeId,
        projectId: project.id,
        baseRevision: project.revision,
        config: isForm
          ? {
              env: textToEnv(env),
              form: { ...base!.form!, env: textToEnv(formEnv) },
              exposures: parsedExposures,
              source: { gitUrl, gitRef },
              preserve: lines(preserve),
            }
          : {
              source: { gitUrl, gitRef },
              preserve: lines(preserve),
              compose,
              env: textToEnv(env),
              exposures: parsedExposures,
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
          <span className="text-sm font-medium">
            对外开放（JSON）：protocol 为 http / tcp / udp；http 可填 domains（经 80/443，自动证书），或填 published 端口；httpOnly 表示不申请证书
          </span>
          <Textarea value={exposures} onChange={(e) => setExposures(e.target.value)} spellCheck={false} className="min-h-32 font-mono text-[13px]" />
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