import { useEffect, useRef, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate } from "@tanstack/react-router";
import { ArrowLeft, Rocket, ScrollText } from "lucide-react";
import { errorMessage, nodeClient, projectClient, toDate } from "@/lib/api";
import { cn, slugName, timeAgo } from "@/lib/utils";
import { ProjectMetrics } from "@/components/metrics";
import { BackupCard, FailoverCard, MigrateCard } from "@/pages/backup";
import { PageHeader } from "@/pages/layout";
import { ProjectState, ServiceState } from "@/components/status";
import { Alert, Badge, Button, Card, CardContent, CardHeader, CardTitle, Field, Input, Select, Spinner, Textarea } from "@/components/ui";
import { envToText, lines, textToEnv } from "@/lib/config";
import { TerminalCard } from "@/components/terminal";
import { useImageForm, useContainerForm } from "@/components/container-form";
import type { Container, Project, ServiceStatus } from "@/gen/nagi/v1/common_pb";

// 测试界面：项目由多个容器组成，每个容器单独部署、记版本、回滚（见 docs/agent.md#项目与容器）。

export function ProjectPage({ nodeId, projectId }: { nodeId: string; projectId: string }) {
  const project = useQuery({
    queryKey: ["project", nodeId, projectId],
    queryFn: () => projectClient.getNodeProject({ nodeId, projectId }),
    refetchInterval: 5000,
  });
  const [selected, setSelected] = useState("");

  if (project.isPending) return <Spinner />;
  if (project.error) {
    return (
      <>
        <BackLink />
        <Alert>{errorMessage(project.error)}</Alert>
        <MovedProject nodeId={nodeId} projectId={projectId} />
      </>
    );
  }
  const p = project.data.project?.project;
  const services = project.data.project?.status?.services ?? [];
  const imageUpdates = project.data.project?.status?.imageUpdates ?? [];
  if (!p) return null;
  const container = p.containers.find((m) => m.name === selected) ?? p.containers[0];

  return (
    <>
      <BackLink />
      <PageHeader
        title={
          <span className="flex items-center gap-3">
            {p.name}
            <ProjectState services={services} deployed={p.containers.some((m) => m.deployedVersion > 0n)} />
            {p.stateful && <Badge>有状态</Badge>}
          </span>
        }
        description={
          <span className="font-mono text-xs">
            {p.dir} · {countContainers(p.containers)} · 项目内网{p.settings?.intranetDisabled ? "已关闭" : "开启"} · 创建于 {timeAgo(toDate(p.createdAt))}
          </span>
        }
      />

      <div className="grid gap-6">
        {project.data.project?.readOnlyReason && (
          <Alert>
            暂时只能查看：{project.data.project.readOnlyReason}。容器照常运行，查看状态、日志、终端都可以；修改配置、部署、快照这些要写入的操作会失败。
          </Alert>
        )}
        <ContainersCard nodeId={nodeId} project={p} services={services} selected={container?.name ?? ""} onSelect={setSelected} />
        <AddContainerCard nodeId={nodeId} project={p} onAdded={setSelected} />
        {imageUpdates.length > 0 && (
          <Alert>
            有新版本的镜像：
            {imageUpdates.map((u) => `${u.container}/${u.service}（${u.image}）`).join("、")}。在容器上点"更新镜像"拉取并部署。
          </Alert>
        )}
        {container && (
          <ContainerPanel
            key={container.name}
            nodeId={nodeId}
            project={p}
            container={container}
            services={services.filter((s) => s.container === container.name)}
            hasImageUpdate={imageUpdates.some((u) => u.container === container.name)}
          />
        )}
        <SettingsCard key={`settings-${p.revision}`} nodeId={nodeId} project={p} services={services} />
        <DeploymentsCard nodeId={nodeId} project={p} />
        <RoutesCard nodeId={nodeId} project={p} services={services} />
        <SnapshotsCard nodeId={nodeId} project={p} />
        <Card>
          <CardHeader>
            <CardTitle>监控（最近 1 小时）</CardTitle>
          </CardHeader>
          <CardContent>
            <ProjectMetrics nodeId={nodeId} projectId={projectId} />
          </CardContent>
        </Card>
        <ReplicaTrustCard nodeId={nodeId} projectId={projectId} trusted={project.data.replicaTrusted} />
        <BackupCard nodeId={nodeId} projectId={projectId} />
        <FailoverCard nodeId={nodeId} projectId={projectId} dir={p.dir} sharedNetworks={p.settings?.sharedNetworks ?? []} />
        <MigrateCard nodeId={nodeId} projectId={projectId} name={p.name} dir={p.dir} sharedNetworks={p.settings?.sharedNetworks ?? []} />
        <DeleteCard nodeId={nodeId} project={p} />
      </div>
    </>
  );
}

// MovedProject：项目在这个节点上找不到时（迁移、顶替、恢复到了别的节点），按项目 ID 在所有节点里找，找到了就跳过去。
function MovedProject({ nodeId, projectId }: { nodeId: string; projectId: string }) {
  const navigate = useNavigate();
  const all = useQuery({ queryKey: ["projects"], queryFn: () => projectClient.listAllProjects({}) });
  const found = all.data?.projects.find((x) => x.project?.project?.id === projectId && x.nodeId !== nodeId);
  useEffect(() => {
    if (found) navigate({ to: "/projects/$nodeId/$projectId", params: { nodeId: found.nodeId, projectId }, replace: true });
  }, [found, navigate, projectId]);
  if (all.isPending) return <Spinner />;
  return found ? <p className="mt-4 text-sm text-muted-foreground">项目已经在另一个节点上，正在跳转……</p> : null;
}

function BackLink() {
  return (
    <Link to="/projects" className="mb-4 inline-flex items-center gap-1 text-sm text-muted-foreground hover:text-foreground">
      <ArrowLeft className="size-4" />
      所有项目
    </Link>
  );
}

// ---- 执行一个会输出过程的操作（部署、回滚、快照……） ----

type RunResult = { success: boolean; error: string };
type StreamEvent = { event: { case: string | undefined; value?: unknown } };

function useRun(nodeId: string, projectId: string) {
  const queryClient = useQueryClient();
  const [lines, setLines] = useState<string[]>([]);
  const [result, setResult] = useState<RunResult>();
  const [busy, setBusy] = useState(false);
  const run = async (start: () => AsyncIterable<StreamEvent>, keep = false) => {
    setBusy(true);
    setResult(undefined);
    if (!keep) setLines([]);
    try {
      for await (const ev of start()) {
        if (ev.event.case === "output") setLines((l) => [...l, ev.event.value as string]);
        if (ev.event.case === "result") setResult(ev.event.value as RunResult);
      }
    } catch (err) {
      setResult({ success: false, error: errorMessage(err) });
    } finally {
      setBusy(false);
      queryClient.invalidateQueries({ queryKey: ["project", nodeId, projectId] });
      queryClient.invalidateQueries({ queryKey: ["deployments", nodeId, projectId] });
      queryClient.invalidateQueries({ queryKey: ["versions", nodeId, projectId] });
      queryClient.invalidateQueries({ queryKey: ["snapshots", nodeId, projectId] });
      queryClient.invalidateQueries({ queryKey: ["projects"] });
    }
  };
  const output = <RunOutput lines={lines} result={result} />;
  return { run, busy, output, setLines };
}

function RunOutput({ lines, result }: { lines: string[]; result?: RunResult }) {
  const outRef = useRef<HTMLPreElement>(null);
  useEffect(() => {
    outRef.current?.scrollTo({ top: outRef.current.scrollHeight });
  }, [lines.length]);
  if (lines.length === 0 && !result) return null;
  return (
    <div className="grid gap-3">
      {result &&
        (result.success ? (
          <Badge tone="success" dot className="w-fit">
            完成
          </Badge>
        ) : (
          <Alert>{result.error || "失败"}</Alert>
        ))}
      {lines.length > 0 && (
        <pre ref={outRef} className="max-h-72 overflow-auto rounded-md bg-zinc-950 p-4 font-mono text-xs leading-relaxed text-zinc-200">
          {lines.join("\n")}
        </pre>
      )}
    </div>
  );
}

// ---- 容器列表与一起部署 ----

// shortImage 把固定了摘要的镜像（回滚、迁移、顶替后会固定到当时的摘要）缩短显示，例如 redis@sha256:858f009f9709。
function shortImage(image: string) {
  const i = image.indexOf("@sha256:");
  return i < 0 ? image : image.slice(0, i + 8 + 12);
}

// countContainers 按种类数，例如"2 个容器 · 1 个容器组"。
function countContainers(cs: Container[]) {
  if (cs.length === 0) return "还没有容器";
  const groups = cs.filter((c) => c.kind === "group").length;
  const singles = cs.length - groups;
  return [singles > 0 && `${singles} 个容器`, groups > 0 && `${groups} 个容器组`].filter(Boolean).join(" · ");
}

// containerKind 返回"容器"（单个容器，镜像表单）或"容器组"（一份 compose，里面有几个容器）。
function containerKind(m: Container) {
  return m.kind === "group" ? "容器组" : "容器";
}

function ContainersCard({
  nodeId,
  project,
  services,
  selected,
  onSelect,
}: {
  nodeId: string;
  project: Project;
  services: ServiceStatus[];
  selected: string;
  onSelect: (name: string) => void;
}) {
  const { run, busy, output } = useRun(nodeId, project.id);
  const [checked, setChecked] = useState<Record<string, boolean>>({});
  const targets = project.containers.filter((m) => checked[m.name]);
  const changed = project.containers.filter((m) => m.hasDraft || m.sharedEnvOutdated || m.deployedVersion === 0n);

  const deploy = (containers: Container[]) =>
    run(() =>
      projectClient.deployNodeProject({
        nodeId,
        projectId: project.id,
        targets: containers.map((m) => ({ container: m.name, baseRevision: m.revision })),
      }),
    );

  return (
    <Card>
      <CardHeader className="flex-row items-center justify-between gap-4">
        <div className="grid gap-1">
          <CardTitle>容器</CardTitle>
          <p className="text-sm text-muted-foreground">每个容器单独部署；勾选几个可以一起部署（按依赖顺序，记一条部署记录）。</p>
        </div>
        <div className="flex gap-2">
          {changed.length > 0 && (
            <Button variant="outline" size="sm" disabled={busy} onClick={() => deploy(changed)}>
              部署未部署和有改动的（{changed.length}）
            </Button>
          )}
          <Button size="sm" disabled={busy || targets.length === 0} onClick={() => deploy(targets)}>
            {busy ? <Spinner /> : <Rocket />}
            部署选中的{targets.length > 0 && `（${targets.length}）`}
          </Button>
        </div>
      </CardHeader>
      <CardContent className="grid gap-4">
        {project.containers.length === 0 ? (
          <p className="text-sm text-muted-foreground">还没有容器，在下面加一个。</p>
        ) : (
          <table className="w-full text-sm">
            <thead className="text-left text-xs text-muted-foreground">
              <tr className="border-b">
                <th className="w-8 py-2" />
                <th className="py-2 pr-4 font-medium">容器</th>
                <th className="py-2 pr-4 font-medium">状态</th>
                <th className="py-2 pr-4 font-medium">版本</th>
                <th className="py-2 font-medium">内网地址</th>
              </tr>
            </thead>
            <tbody>
              {project.containers.map((m) => {
                const svc = services.filter((s) => s.container === m.name);
                return (
                  <tr
                    key={m.name}
                    className={cn("cursor-pointer border-b last:border-0 hover:bg-muted/40", m.name === selected && "bg-muted/60")}
                    onClick={() => onSelect(m.name)}
                  >
                    <td className="py-2" onClick={(e) => e.stopPropagation()}>
                      <input type="checkbox" checked={!!checked[m.name]} onChange={(e) => setChecked({ ...checked, [m.name]: e.target.checked })} />
                    </td>
                    <td className="py-2 pr-4">
                      <span className="font-medium">{m.name}</span>
                      <span className="ml-2 text-xs text-muted-foreground">{containerKind(m)}</span>
                      {m.hasDraft && (
                        <Badge tone="warning" className="ml-2">
                          草稿
                        </Badge>
                      )}
                      {m.sharedEnvOutdated && (
                        <Badge tone="warning" className="ml-2">
                          共享变量有更新
                        </Badge>
                      )}
                      {m.stateful && <Badge className="ml-2">有状态</Badge>}
                      {m.hostNetworkServices.length > 0 && (
                        <Badge tone="danger" className="ml-2">
                          host 网络
                        </Badge>
                      )}
                      {m.unmanagedVolumes.length > 0 && (
                        <Badge tone="warning" className="ml-2">
                          有 volume 不在快照里
                        </Badge>
                      )}
                    </td>
                    <td className="py-2 pr-4">
                      <ProjectState services={svc} deployed={m.deployedVersion > 0n} />
                    </td>
                    <td className="py-2 pr-4 font-mono text-xs">
                      {m.deployedVersion > 0n ? `v${m.deployedVersion}` : "未部署"}
                      {m.latestVersion !== m.deployedVersion && m.deployedVersion > 0n && ` / 最新 v${m.latestVersion}`}
                    </td>
                    <td className="py-2 font-mono text-xs text-muted-foreground">
                      {project.settings?.intranetDisabled
                        ? "—"
                        : containerKind(m) === "容器"
                          ? m.name
                          : [...new Set(svc.filter((s) => !m.hostNetworkServices.includes(s.name)).map((s) => `${s.name}.${m.name}`))].join(" ") ||
                            "—"}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        )}
        {output}
      </CardContent>
    </Card>
  );
}

// ---- 一个容器 ----

function ContainerPanel({
  nodeId,
  project,
  container,
  services,
  hasImageUpdate,
}: {
  nodeId: string;
  project: Project;
  container: Container;
  services: ServiceStatus[];
  hasImageUpdate: boolean;
}) {
  const { run, busy, output, setLines } = useRun(nodeId, project.id);
  const queryClient = useQueryClient();
  const fileRef = useRef<HTMLInputElement>(null);
  const cfg = container.draft ?? container.config;
  const target = [{ container: container.name, baseRevision: container.revision }];

  const deploy = (opts: { pull?: boolean; useUpload?: boolean; pullImages?: boolean }, keep = false) =>
    run(() => projectClient.deployNodeProject({ nodeId, projectId: project.id, targets: target, ...opts }), keep);

  // 上传压缩包：浏览器 → 面板 → mesh → 节点暂存，然后部署时替换容器的 data/。
  const upload = async (file: File) => {
    setLines([`上传 ${file.name}（${(file.size / 1048576).toFixed(1)} MB）`]);
    const q = new URLSearchParams({ node: nodeId, project: project.id, container: container.name });
    const res = await fetch(`/api/upload?${q}`, { method: "POST", headers: { "X-Nagi-Upload": "1" }, body: file });
    const body = await res.json();
    if (!res.ok) {
      setLines((l) => [...l, "✗ " + body.message]);
      return;
    }
    setLines((l) => [...l, `已上传 ${body.files} 个文件${body.stripped ? `（去掉了顶层目录 ${body.stripped}/）` : ""}`]);
    await deploy({ useUpload: true }, true);
  };

  const remove = async () => {
    const kind = containerKind(container);
    const deleteDir = confirm(`删除${kind} ${container.name}？${kind === "容器组" ? "组里的容器都会" : "容器会"}被删除。\n\n点"确定"同时删除${kind}目录（数据会删掉），点"取消"只删容器、保留目录。`);
    if (!deleteDir && !confirm(`只删除${kind} ${container.name} 的容器，保留${kind}目录？`)) return;
    try {
      await projectClient.deleteNodeProjectContainer({ nodeId, projectId: project.id, container: container.name, baseRevision: container.revision, deleteDir });
      queryClient.invalidateQueries({ queryKey: ["project", nodeId, project.id] });
    } catch (err) {
      setLines(["✗ " + errorMessage(err)]);
    }
  };

  return (
    <div className="grid gap-6 rounded-xl border-2 border-dashed p-4">
      <Card>
        <CardHeader className="flex-row items-center justify-between gap-4">
          <div className="grid gap-1">
            <CardTitle>
              {containerKind(container)} {container.name}
              {container.kind === "group" && <span className="ml-1 text-sm font-normal text-muted-foreground">（compose）</span>}
            </CardTitle>
            <p className="text-sm text-muted-foreground">
              {container.hasDraft
                ? "有未部署的修改：部署时会把草稿保存为新版本。"
                : container.deployedVersion > 0n
                  ? `运行的是版本 v${container.deployedVersion}。`
                  : "还没部署过。"}
              {container.sharedEnvOutdated && " 共享变量有更新，部署时用上。"}
            </p>
            {container.hostNetworkServices.length > 0 && (
              <p className="text-sm font-medium text-destructive">
                ⚠ 容器 {container.hostNetworkServices.join("、")} 用的是 host 网络：不经过 Caddy、不在项目内网里，端口直接对外开放，防火墙要自己管。
              </p>
            )}
            {container.unmanagedVolumes.length > 0 && (
              <p className="rounded border border-warning/40 bg-warning/15 px-2 py-1 text-sm">
                ⚠ volume {container.unmanagedVolumes.join("、")} 是 external 或用了自定义 driver，不在快照范围内：快照、迁移、顶替都不带它的数据。
              </p>
            )}
          </div>
          <div className="flex flex-wrap justify-end gap-2">
            <input
              ref={fileRef}
              type="file"
              accept=".zip,.tar,.tar.gz,.tgz"
              className="hidden"
              onChange={(e) => {
                const f = e.target.files?.[0];
                e.target.value = "";
                if (f) upload(f);
              }}
            />
            {(!cfg?.form || cfg.form.workdir) && (
              <Button variant="outline" size="sm" onClick={() => fileRef.current?.click()} disabled={busy}>
                上传并部署
              </Button>
            )}
            {cfg?.source?.gitUrl && (
              <Button variant="outline" size="sm" onClick={() => deploy({ pull: true })} disabled={busy}>
                拉取并部署
              </Button>
            )}
            {hasImageUpdate && (
              <Button variant="outline" size="sm" onClick={() => deploy({ pullImages: true })} disabled={busy}>
                更新镜像
              </Button>
            )}
            <Button size="sm" onClick={() => deploy({})} disabled={busy}>
              {busy ? <Spinner /> : <Rocket />}
              {busy ? "部署中" : container.hasDraft ? "部署草稿" : container.deployedVersion > 0n ? "重新部署" : "部署"}
            </Button>
            <Button variant="ghost" size="sm" onClick={remove} disabled={busy}>
              删除{containerKind(container)}
            </Button>
          </div>
        </CardHeader>
        <CardContent className="grid gap-4">
          {output}
          <table className="w-full text-sm">
            <tbody>
              {services.map((s) => (
                <tr key={s.name + s.containerId} className="border-b last:border-0">
                  <td className="py-2 pr-4 font-medium">{s.name}</td>
                  <td className="py-2 pr-4">
                    <ServiceState service={s} />
                  </td>
                  <td className="py-2 pr-4 font-mono text-xs text-muted-foreground" title={s.image}>
                    {shortImage(s.image)}
                  </td>
                  <td className="py-2 text-xs text-muted-foreground">{s.status}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </CardContent>
      </Card>
      <SourceInfo container={container} />
      <ContainerConfigCard key={`cfg-${container.revision}`} nodeId={nodeId} project={project} container={container} services={[...new Set(services.map((s) => s.name))]} />
      <VersionsCard nodeId={nodeId} project={project} container={container} />
      <LogsCard
        nodeId={nodeId}
        projectId={project.id}
        container={container.name}
        services={[...new Set(services.map((s) => s.name))]}
        containers={services.map((s) => s.containerId).filter(Boolean).join(",")}
      />
      <TerminalCard nodeId={nodeId} projectId={project.id} container={container.name} services={services.filter((s) => s.state === "running").map((s) => s.name)} />
    </div>
  );
}

type Line = { key: number; service: string; stream: string; text: string };

const maxLines = 2000;

function LogsCard({
  nodeId,
  projectId,
  container,
  services,
  containers,
}: {
  nodeId: string;
  projectId: string;
  container: string;
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
          { nodeId, projectId, container, service, tail: 200, follow: true },
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
  }, [nodeId, projectId, container, service, containers]);

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
        {services.length > 1 && (
          <Select value={service} onChange={(e) => setService(e.target.value)} className="w-40">
            <option value="">全部容器</option>
            {services.map((s) => (
              <option key={s} value={s}>
                {s}
              </option>
            ))}
          </Select>
        )}
      </CardHeader>
      <CardContent>
        {error && <Alert className="mb-3">{error}</Alert>}
        <div
          ref={boxRef}
          onScroll={(e) => {
            const el = e.currentTarget;
            stick.current = el.scrollHeight - el.scrollTop - el.clientHeight < 40;
          }}
          className="h-80 overflow-auto rounded-md bg-zinc-950 p-4 font-mono text-xs leading-relaxed text-zinc-200"
        >
          {lines.length === 0 ? (
            <span className="text-zinc-500">暂无日志</span>
          ) : (
            lines.map((l) => (
              <div key={l.key} className="whitespace-pre-wrap break-all">
                {!service && services.length > 1 && <span className="mr-2 text-sky-400">{l.service}</span>}
                <span className={cn(l.stream === "stderr" && "text-amber-200")}>{l.text}</span>
              </div>
            ))
          )}
        </div>
      </CardContent>
    </Card>
  );
}

function ContainerConfigCard({ nodeId, project, container, services }: { nodeId: string; project: Project; container: Container; services: string[] }) {
  const queryClient = useQueryClient();
  const base = container.draft ?? container.config;
  const isForm = !!base?.form;
  const [compose, setCompose] = useState(base?.compose ?? "");
  const [env, setEnv] = useState(envToText(base?.env));
  const [serviceEnv, setServiceEnv] = useState<Record<string, string>>(() =>
    Object.fromEntries(services.map((s) => [s, envToText(base?.serviceEnv[s]?.vars)])),
  );
  const imageForm = useImageForm(base?.form, "edit-");
  const [gitUrl, setGitUrl] = useState(base?.source?.gitUrl ?? "");
  const [gitRef, setGitRef] = useState(base?.source?.gitRef ?? "");
  const [preserve, setPreserve] = useState((base?.preserve ?? []).join("\n"));
  const [dependsOn, setDependsOn] = useState((base?.dependsOn ?? []).join(", "));
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  const refresh = () => {
    queryClient.invalidateQueries({ queryKey: ["project", nodeId, project.id] });
    queryClient.invalidateQueries({ queryKey: ["projects"] });
  };

  const save = async () => {
    setBusy(true);
    setError("");
    const common = {
      source: { gitUrl, gitRef },
      preserve: lines(preserve),
      env: textToEnv(env),
      dependsOn: dependsOn.split(/[\s,]+/).filter(Boolean),
      stateful: base?.stateful,
    };
    try {
      await projectClient.saveNodeProjectDraft({
        nodeId,
        projectId: project.id,
        container: container.name,
        baseRevision: container.revision,
        config: isForm
          ? { ...common, form: imageForm.build() }
          : {
              ...common,
              compose,
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
      await projectClient.discardNodeProjectDraft({ nodeId, projectId: project.id, container: container.name, baseRevision: container.revision });
      refresh();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Card>
      <CardHeader className="flex-row items-center justify-between gap-4">
        <CardTitle className="flex items-center gap-2">
          配置
          {container.hasDraft && <Badge tone="warning">草稿</Badge>}
        </CardTitle>
        <div className="flex gap-2">
          {container.hasDraft && (
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
          imageForm.fields
        ) : (
          <div className="grid gap-1.5">
            <span className="text-sm font-medium">compose</span>
            <span className="text-xs text-muted-foreground">
              ports 只在第一次出现时变成端口映射；已有的对外端口、域名在下面的"端口映射"里改，改这里的 ports 不会生效。
            </span>
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
        <div className="grid gap-3 sm:grid-cols-2">
          <div className="grid gap-1.5">
            <span className="text-sm font-medium">更新文件时保留的路径（相对 data/，每行一个）</span>
            <Textarea value={preserve} onChange={(e) => setPreserve(e.target.value)} spellCheck={false} className="min-h-16 font-mono text-[13px]" placeholder="uploads/" />
          </div>
          <div className="grid gap-1.5">
            <span className="text-sm font-medium">先部署的容器（一起部署时）</span>
            <Input value={dependsOn} onChange={(e) => setDependsOn(e.target.value)} className="font-mono" placeholder="db" />
          </div>
        </div>
        {!isForm && (
          <div className="grid gap-1.5">
            <span className="text-sm font-medium">容器组变量（替换 compose 里的 ${"{变量}"}，同名时覆盖项目的共享变量；KEY=VALUE 每行一个）</span>
            <Textarea value={env} onChange={(e) => setEnv(e.target.value)} spellCheck={false} className="font-mono text-[13px]" />
          </div>
        )}
        {!isForm &&
          services.map((s) => (
            <div key={s} className="grid gap-1.5">
              <span className="text-sm font-medium">容器 {s} 的环境变量（只注入这个容器）</span>
              <Textarea
                value={serviceEnv[s] ?? ""}
                onChange={(e) => setServiceEnv((prev) => ({ ...prev, [s]: e.target.value }))}
                spellCheck={false}
                className="font-mono text-[13px]"
              />
            </div>
          ))}
      </CardContent>
    </Card>
  );
}

function SourceInfo({ container }: { container: Container }) {
  const cfg = container.draft ?? container.config;
  if (!cfg?.source?.gitUrl) return null;
  const hook = `${location.origin}/hooks/${container.webhookToken}`;
  return (
    <Card>
      <CardHeader>
        <CardTitle>git</CardTitle>
      </CardHeader>
      <CardContent className="grid gap-3 text-sm">
        <div>
          仓库 <span className="font-mono">{cfg.source.gitUrl}</span>
          {cfg.source.gitRef && <span className="font-mono"> @ {cfg.source.gitRef}</span>}
          {container.lastCommit && <span className="text-muted-foreground">，当前提交 {container.lastCommit.slice(0, 12)}</span>}
        </div>
        <div className="grid gap-1">
          <span className="font-medium">deploy key（加到仓库的只读 deploy key）</span>
          <code className="break-all rounded bg-muted p-2 text-xs">{container.deployKey}</code>
        </div>
        <div className="grid gap-1">
          <span className="font-medium">webhook 地址（push 后自动拉取并部署这个容器）</span>
          <code className="break-all rounded bg-muted p-2 text-xs">{hook}</code>
        </div>
      </CardContent>
    </Card>
  );
}

function VersionsCard({ nodeId, project, container }: { nodeId: string; project: Project; container: Container }) {
  const { run, busy, output } = useRun(nodeId, project.id);
  const ref = { nodeId, projectId: project.id, container: container.name };
  const versions = useQuery({
    queryKey: ["versions", nodeId, project.id, container.name, String(container.revision)],
    queryFn: () => projectClient.listNodeProjectVersions(ref),
  });
  const [restoreData, setRestoreData] = useState(false);

  return (
    <Card>
      <CardHeader className="flex-row items-center justify-between gap-4">
        <CardTitle>版本（每次部署这个容器一个）</CardTitle>
        <label className="flex items-center gap-2 text-xs">
          <input type="checkbox" checked={restoreData} onChange={(e) => setRestoreData(e.target.checked)} />
          回滚时同时恢复数据
        </label>
      </CardHeader>
      <CardContent className="grid gap-3 text-sm">
        {output}
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
                  {v.version !== container.deployedVersion && v.deployedAt && (
                    <Button
                      size="sm"
                      variant="ghost"
                      disabled={busy}
                      onClick={() => run(() => projectClient.rollbackNodeProject({ ...ref, version: v.version, restoreData }))}
                    >
                      回滚到这里
                    </Button>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </CardContent>
    </Card>
  );
}

// ---- 加容器 ----

function AddContainerCard({ nodeId, project, onAdded }: { nodeId: string; project: Project; onAdded: (name: string) => void }) {
  const queryClient = useQueryClient();
  const [open, setOpen] = useState(project.containers.length === 0);
  const [name, setName] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const form = useContainerForm();

  const add = async () => {
    setBusy(true);
    setError("");
    try {
      await projectClient.addNodeProjectContainer({ nodeId, projectId: project.id, name, config: form.build() });
      queryClient.invalidateQueries({ queryKey: ["project", nodeId, project.id] });
      onAdded(name);
      setName("");
      setOpen(false);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Card>
      <CardHeader className="flex-row items-center justify-between gap-4">
        <CardTitle>加容器</CardTitle>
        {!open && (
          <Button size="sm" variant="outline" onClick={() => setOpen(true)}>
            加一个容器或容器组
          </Button>
        )}
      </CardHeader>
      {open && (
        <CardContent>
          <form
            className="grid gap-4"
            onSubmit={(e) => {
              e.preventDefault();
              add();
            }}
          >
            <Field label={form.mode === "compose" ? "容器组名" : "容器名"} htmlFor="new-container" hint="小写字母、数字和 -，创建后不能改。单个容器在项目内网里就叫这个名字；容器组里的容器叫“容器.容器组名”。">
              <Input id="new-container" value={name} onChange={(e) => setName(e.target.value)} required className="font-mono" placeholder="api" />
            </Field>
            {form.fields}
            {error && <Alert>{error}</Alert>}
            <div className="flex justify-end gap-2">
              <Button type="button" variant="outline" onClick={() => setOpen(false)}>
                取消
              </Button>
              <Button type="submit" disabled={busy}>
                {busy && <Spinner />}
                加入（不部署）
              </Button>
            </div>
          </form>
        </CardContent>
      )}
    </Card>
  );
}

// ---- 备份节点上的特权配置 ----

// ReplicaTrustCard：项目在别的节点上从副本启动（备份恢复、顶替、迁移、重建）时，配置里有特权设置的默认不启动，
// 防止源节点被攻破后借副本控制别的节点。管理员确认配置可信后在这里允许。
function ReplicaTrustCard({ nodeId, projectId, trusted }: { nodeId: string; projectId: string; trusted: boolean }) {
  const queryClient = useQueryClient();
  const [error, setError] = useState("");
  const toggle = async (next: boolean) => {
    if (
      next &&
      !confirm(
        "允许这个项目在备份节点、顶替节点、迁移目标上使用特权配置（特权模式、挂载宿主机路径、host 网络等）？\n\n只在确认项目配置可信时开启：源节点被攻破的话，它可以借副本在这些节点上拿到 root。",
      )
    )
      return;
    setError("");
    try {
      await projectClient.setProjectReplicaTrust({ projectId, trusted: next });
      queryClient.invalidateQueries({ queryKey: ["project", nodeId, projectId] });
    } catch (err) {
      setError(errorMessage(err));
    }
  };
  return (
    <Card>
      <CardContent className="grid gap-2 pt-5 text-sm">
        {error && <Alert>{error}</Alert>}
        <label className="flex items-center gap-2">
          <input type="checkbox" checked={trusted} onChange={(e) => toggle(e.target.checked)} />
          允许在备份节点上使用特权配置
        </label>
        <p className="text-xs text-muted-foreground">
          项目在别的节点上从副本启动（恢复备份、顶替、迁移过去、按配置备份重建）时，配置里有特权模式、挂载宿主机路径、host 网络这类设置的默认不启动：副本来自原来的节点，它被攻破的话，可以借副本控制别的节点。项目确实需要这些设置时在这里允许，由面板直接告诉各节点。
        </p>
      </CardContent>
    </Card>
  );
}

// ---- 项目设置 ----

// defaultNetworkName 由项目名生成加入共享网络用的网络名（小写字母、数字和 -，不超过 32 个字符）。
// 项目名里没有能用的字符（例如全是中文）时返回空，让用户自己起一个：别人要用它访问本项目，ID 前缀不好记。
function defaultNetworkName(name: string) {
  return slugName(name, "");
}

// sharedAddresses 列出本项目的容器在共享网络里的地址：单个容器是"容器.网络名"，容器组里的是"容器.容器组.网络名"。
function sharedAddresses(project: Project, services: ServiceStatus[], networkName: string) {
  return project.containers.flatMap((c) => {
    if (c.kind !== "group") return [`${c.name}.${networkName}`];
    const names = [...new Set(services.filter((s) => s.container === c.name).map((s) => s.name))];
    return names.length ? names.map((s) => `${s}.${c.name}.${networkName}`) : [`<容器>.${c.name}.${networkName}`];
  });
}

function SettingsCard({ nodeId, project, services }: { nodeId: string; project: Project; services: ServiceStatus[] }) {
  const queryClient = useQueryClient();
  const s = project.settings;
  const networks = useQuery({ queryKey: ["shared-networks", nodeId], queryFn: () => nodeClient.listNodeSharedNetworks({ nodeId }) });
  const [name, setName] = useState(project.name);
  const [intranet, setIntranet] = useState(!s?.intranetDisabled);
  const [networkName, setNetworkName] = useState(s?.networkName ?? "");
  const [joined, setJoined] = useState<string[]>(s?.sharedNetworks ?? []);
  const [linkPorts, setLinkPorts] = useState((s?.linkPorts ?? []).join("\n"));
  const [interval, setInterval] = useState(String(s?.snapshotIntervalMinutes ?? 0));
  const [sharedEnv, setSharedEnv] = useState(envToText(project.hasSharedEnvDraft ? project.sharedEnvDraft : s?.sharedEnv));
  const [affected, setAffected] = useState<Record<string, boolean>>({});
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const { run, busy: running, output } = useRun(nodeId, project.id);
  const refresh = () => queryClient.invalidateQueries({ queryKey: ["project", nodeId, project.id] });

  const call = async (fn: () => Promise<unknown>) => {
    setBusy(true);
    setError("");
    try {
      await fn();
      refresh();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const effectiveName = networkName || defaultNetworkName(name);
  const saveSettings = () => {
    if (joined.length > 0 && !effectiveName) {
      setError("加入共享网络需要一个网络名（小写字母、数字和 -），别的项目用它访问本项目。");
      return;
    }
    return call(() =>
      projectClient.updateNodeProjectSettings({
        nodeId,
        projectId: project.id,
        baseRevision: project.revision,
        name,
        settings: {
          intranetDisabled: !intranet,
          networkName: joined.length > 0 ? effectiveName : networkName,
          sharedNetworks: joined,
          linkPorts: linkPorts.split(/[\n,]/).map((x) => x.trim()).filter(Boolean),
          snapshotIntervalMinutes: Number(interval) || 0,
        },
      }),
    );
  };

  const saveEnv = () =>
    call(async () => {
      const res = await projectClient.saveNodeProjectSharedEnv({
        nodeId,
        projectId: project.id,
        baseRevision: project.revision,
        vars: textToEnv(sharedEnv),
      });
      setAffected(Object.fromEntries(res.affected.map((m) => [m, true])));
    });

  const deployEnv = () =>
    run(() =>
      projectClient.deployNodeProject({
        nodeId,
        projectId: project.id,
        applySharedEnv: true,
        settingsRevision: project.revision,
        targets: project.containers.filter((m) => affected[m.name]).map((m) => ({ container: m.name, baseRevision: m.revision })),
      }),
    );

  return (
    <Card>
      <CardHeader>
        <CardTitle>项目设置</CardTitle>
        <p className="text-sm text-muted-foreground">名称、内网、共享网络改了立即生效，不进容器的版本。开关内网时单个容器会重建（短暂重启）。</p>
      </CardHeader>
      <CardContent className="grid gap-4 text-sm">
        {error && <Alert>{error}</Alert>}
        <div className="grid gap-4 sm:grid-cols-2">
          <Field label="项目名" htmlFor="p-name">
            <Input id="p-name" value={name} onChange={(e) => setName(e.target.value)} />
          </Field>
          <Field label="定时快照间隔（分钟，0 表示默认 60）" htmlFor="p-interval">
            <Input id="p-interval" value={interval} onChange={(e) => setInterval(e.target.value)} />
          </Field>
        </div>
        <label className="flex items-center gap-2">
          <input type="checkbox" checked={intranet} onChange={(e) => setIntranet(e.target.checked)} />
          项目内网：容器之间用"容器名"（单个容器）或"容器.容器组"（容器组）互相访问
        </label>
        <div className="grid gap-2 rounded-md border p-3">
          <span className="font-medium">共享网络（和加入它的其他项目互通，容器在里面叫"容器.网络名"；网络和其他节点相连时也能跨节点访问）</span>
          <Input
            value={networkName}
            onChange={(e) => setNetworkName(e.target.value)}
            placeholder={defaultNetworkName(name) ? `网络名，默认 ${defaultNetworkName(name)}` : "网络名（英文），例如 shop"}
            className="max-w-60 font-mono"
          />
          {s?.networkName && networkName !== s.networkName && (s.sharedNetworks.length ?? 0) > 0 && (
            <p className="rounded border border-warning/40 bg-warning/15 px-2 py-1 text-xs">
              改网络名以后，其他项目里用"容器.{s.networkName}"访问本项目的地方，要跟着改成"容器.{effectiveName}"。
            </p>
          )}
          {networks.data?.networks.length ? (
            networks.data.networks.map((n) => (
              <label key={n.name} className="flex items-center gap-2">
                <input
                  type="checkbox"
                  checked={joined.includes(n.name)}
                  onChange={(e) => {
                    if (e.target.checked && !networkName) setNetworkName(defaultNetworkName(name));
                    setJoined(e.target.checked ? [...joined, n.name] : joined.filter((x) => x !== n.name));
                  }}
                />
                <span className="font-mono">{n.name}</span>
                <span className="text-xs text-muted-foreground">
                  {n.subnet} · {n.projects.map((p) => `${p.projectName}（${p.networkName}）`).join("、") || "还没有项目加入"}
                  {n.peers.length > 0 && ` · 和 ${n.peers.map((p) => p.name).join("、")} 相连`}
                </span>
              </label>
            ))
          ) : (
            <span className="text-xs text-muted-foreground">本节点还没有共享网络，在节点页里创建。</span>
          )}
          {s?.networkName && (s.sharedNetworks.length ?? 0) > 0 && (
            <p className="text-xs text-muted-foreground">
              其他项目这样访问本项目：<span className="font-mono">{sharedAddresses(project, services, s.networkName).join("、")}</span>
            </p>
          )}
          <Field label="跨节点端口（相连节点经代理访问本项目时，容器没声明的端口；一行一个，如 pg:5432，容器组里的写 容器.容器组:端口）" htmlFor="p-linkports">
            <textarea
              id="p-linkports"
              className="min-h-16 rounded-md border bg-transparent px-3 py-2 font-mono text-sm"
              value={linkPorts}
              onChange={(e) => setLinkPorts(e.target.value)}
            />
          </Field>
        </div>
        <div>
          <Button size="sm" onClick={saveSettings} disabled={busy}>
            {busy && <Spinner />}
            保存设置
          </Button>
        </div>

        <div className="grid gap-2 border-t pt-4">
          <span className="flex items-center gap-2 font-medium">
            共享变量（所有容器的 compose 里都能用 ${"{变量}"} 引用，同名时以容器自己的为准）
            {project.hasSharedEnvDraft && <Badge tone="warning">草稿</Badge>}
          </span>
          <Textarea value={sharedEnv} onChange={(e) => setSharedEnv(e.target.value)} spellCheck={false} className="font-mono text-[13px]" />
          {project.hasSharedEnvDraft && (
            <div className="grid gap-1">
              <span className="text-xs text-muted-foreground">部署共享变量时一起部署这些容器（默认是引用了改动变量的）：</span>
              <div className="flex flex-wrap gap-3">
                {project.containers.map((m) => (
                  <label key={m.name} className="flex items-center gap-1.5">
                    <input type="checkbox" checked={!!affected[m.name]} onChange={(e) => setAffected({ ...affected, [m.name]: e.target.checked })} />
                    {m.name}
                  </label>
                ))}
              </div>
            </div>
          )}
          <div className="flex gap-2">
            <Button size="sm" variant="outline" onClick={saveEnv} disabled={busy}>
              保存草稿
            </Button>
            {project.hasSharedEnvDraft && (
              <>
                <Button
                  size="sm"
                  variant="outline"
                  onClick={() => call(() => projectClient.discardNodeProjectSharedEnv({ nodeId, projectId: project.id, baseRevision: project.revision }))}
                  disabled={busy}
                >
                  放弃草稿
                </Button>
                <Button size="sm" onClick={deployEnv} disabled={running}>
                  {running ? <Spinner /> : <Rocket />}
                  部署共享变量
                </Button>
              </>
            )}
          </div>
          {output}
        </div>
      </CardContent>
    </Card>
  );
}

// ---- 部署记录 ----

const sourceLabel: Record<string, string> = {
  manual: "手动",
  git: "git 拉取",
  image: "镜像更新",
  rollback: "容器回滚",
  shared_env: "共享变量",
  restore: "回到某次部署",
};

function DeploymentsCard({ nodeId, project }: { nodeId: string; project: Project }) {
  const { run, busy, output } = useRun(nodeId, project.id);
  const list = useQuery({
    queryKey: ["deployments", nodeId, project.id],
    queryFn: () => projectClient.listNodeProjectDeployments({ nodeId, projectId: project.id }),
  });

  return (
    <Card>
      <CardHeader>
        <CardTitle>部署记录</CardTitle>
        <p className="text-sm text-muted-foreground">每次部署（一个或多个容器）一条，记下部署后所有容器的版本。可以把整个项目回到某次部署时的状态。</p>
      </CardHeader>
      <CardContent className="grid gap-3 text-sm">
        {output}
        {list.data?.deployments.length === 0 && <p className="text-muted-foreground">还没有部署过。</p>}
        <table className="w-full text-xs">
          <tbody>
            {list.data?.deployments.map((d, i) => (
              <tr key={String(d.id)} className="border-b last:border-0">
                <td className="py-1.5 pr-3 font-mono">#{String(d.id)}</td>
                <td className="py-1.5 pr-3 text-muted-foreground">{timeAgo(toDate(d.time))}</td>
                <td className="py-1.5 pr-3">
                  {sourceLabel[d.source] ?? d.source}
                  {d.restoreOf > 0n && ` #${d.restoreOf}`}：{d.containers.join("、")}
                  {!d.success && <span className="ml-2 text-destructive">失败：{d.error}</span>}
                </td>
                <td className="py-1.5 pr-3 font-mono text-muted-foreground">
                  {Object.entries(d.versions)
                    .map(([m, v]) => `${m} v${v}`)
                    .join(" · ")}
                </td>
                <td className="py-1.5 text-right">
                  {i > 0 && (
                  <Button
                    size="sm"
                    variant="ghost"
                    disabled={busy}
                    onClick={() =>
                      confirm(`把项目回到部署 #${d.id} 时的状态？版本不一样的容器都会回滚。`) &&
                      run(() => projectClient.restoreNodeProjectDeployment({ nodeId, projectId: project.id, deploymentId: d.id }))
                    }
                  >
                    回到这次部署时
                  </Button>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </CardContent>
    </Card>
  );
}

// ---- 端口映射 ----

function RoutesCard({ nodeId, project, services }: { nodeId: string; project: Project; services: ServiceStatus[] }) {
  const queryClient = useQueryClient();
  const empty = { name: "", container: "", service: "", target: "", protocol: "http", published: "", domains: "", httpOnly: false };
  const [form, setForm] = useState(empty);
  const [error, setError] = useState("");
  const refresh = () => queryClient.invalidateQueries({ queryKey: ["project", nodeId, project.id] });
  const container = form.container || project.containers[0]?.name || "";
  const containerServices = [...new Set(services.filter((s) => s.container === container).map((s) => s.name))];
  const nodes = useQuery({ queryKey: ["nodes"], queryFn: () => nodeClient.listNodes({}) });
  const address = nodes.data?.nodes.find((n) => n.id === nodeId)?.address ?? "";
  // 访问地址：http 绑了域名用域名，否则用"节点地址:端口"；tcp/udp 只给地址，不是网页。
  const accessURL = (r: (typeof project.routes)[number]) => {
    if (r.domains.length) return `${r.httpOnly ? "http" : "https"}://${r.domains[0]}`;
    if (!address || !r.published) return "";
    return r.protocol === "http" ? `http://${address}:${r.published}` : "";
  };

  const save = async () => {
    setError("");
    try {
      await projectClient.saveNodeProjectRoute({
        nodeId,
        projectId: project.id,
        route: {
          name: form.name,
          container,
          service: form.service || containerServices[0] || "",
          target: Number(form.target),
          protocol: form.protocol,
          published: Number(form.published || 0),
          domains: form.domains.split(/[\s,]+/).filter(Boolean),
          httpOnly: form.httpOnly,
        },
      });
      setForm(empty);
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
          保存后立即生效，不需要重新部署。文件在 {project.dir}/config/routes/，也可以直接在节点上修改。容器 compose 里新出现的 ports 会在部署时自动导入。
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
                        {r.container}
                        {r.service !== r.container && `/${r.service}`}:{r.target} →{" "}
                        {r.domains.length ? r.domains.join(", ") + (r.httpOnly ? "（仅 HTTP）" : "") : `${r.protocol}/${r.published}`}
                      </span>
                    )}
                    {!r.error && accessURL(r) && (
                      <a href={accessURL(r)} target="_blank" rel="noreferrer" className="ml-3 text-xs underline">
                        打开 {accessURL(r)}
                      </a>
                    )}
                    {!r.error && !accessURL(r) && address && r.published > 0 && (
                      <span className="ml-3 font-mono text-xs text-muted-foreground">
                        {address}:{r.published}
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
                          container: r.container,
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
        <div className="grid gap-2 rounded-md border p-3 sm:grid-cols-7">
          <Select value={container} onChange={(e) => setForm({ ...form, container: e.target.value, service: "" })}>
            {project.containers.map((m) => (
              <option key={m.name}>{m.name}</option>
            ))}
          </Select>
          <Select value={form.service || containerServices[0] || ""} onChange={(e) => setForm({ ...form, service: e.target.value })}>
            {containerServices.map((s) => (
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
          <label className="flex items-center gap-2 sm:col-span-7">
            <input type="checkbox" checked={form.httpOnly} onChange={(e) => setForm({ ...form, httpOnly: e.target.checked })} />
            只开 HTTP，不申请证书
          </label>
        </div>
      </CardContent>
    </Card>
  );
}

// ---- 快照 ----

const snapshotKinds: Record<string, string> = {
  scheduled: "定时",
  manual: "手动",
  predeploy: "部署前的数据",
  version: "版本文件",
};

function SnapshotsCard({ nodeId, project }: { nodeId: string; project: Project }) {
  const { run, busy, output } = useRun(nodeId, project.id);
  const ref = { nodeId, projectId: project.id };
  const snapshots = useQuery({ queryKey: ["snapshots", nodeId, project.id], queryFn: () => projectClient.listNodeProjectSnapshots(ref) });
  const [container, setContainer] = useState("");

  return (
    <Card>
      <CardHeader className="flex-row items-center justify-between gap-4">
        <CardTitle>快照</CardTitle>
        <Button size="sm" variant="outline" disabled={busy} onClick={() => run(() => projectClient.createNodeProjectSnapshot(ref))}>
          立即快照
        </Button>
      </CardHeader>
      <CardContent className="grid gap-3 text-sm">
        <p className="text-muted-foreground">
          定时和手动快照保存所有容器的数据（data/ 和 volumes/），可以在这里恢复，配置不变。"部署前的数据"和"版本文件"供回滚用，在容器的版本列表里回滚。
        </p>
        <label className="flex items-center gap-2">
          <span>恢复时：</span>
          <Select value={container} onChange={(e) => setContainer(e.target.value)} className="w-44">
            <option value="">恢复整个项目</option>
            {project.containers.map((m) => (
              <option key={m.name} value={m.name}>
                只恢复容器 {m.name}
              </option>
            ))}
          </Select>
        </label>
        {output}
        <table className="w-full text-xs">
          <tbody>
            {snapshots.data?.snapshots.map((sn) => (
              <tr key={sn.id} className="border-b last:border-0">
                <td className="py-1.5 pr-3">{snapshotKinds[sn.kind] ?? sn.kind}</td>
                <td className="py-1.5 pr-3 text-muted-foreground">{timeAgo(toDate(sn.time))}</td>
                <td className="py-1.5 pr-3 text-muted-foreground">
                  {sn.container && `${sn.container} `}
                  {sn.version > 0n && `v${sn.version}`} {(Number(sn.size) / 1024).toFixed(1)} KB
                </td>
                <td className="py-1.5 text-right">
                  {(sn.kind === "scheduled" || sn.kind === "manual") && (
                    <Button
                      size="sm"
                      variant="ghost"
                      disabled={busy}
                      onClick={() =>
                        confirm(`从这个快照恢复${container ? `容器 ${container} 的` : "整个项目的"} data/ 和 volumes/？会先停止。`) &&
                        run(() => projectClient.restoreNodeProjectSnapshot({ ...ref, snapshotId: sn.id, container }))
                      }
                    >
                      恢复
                    </Button>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
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
    if (!deleteDir && !confirm(`删除项目 ${project.name}？项目里的容器和容器组都会被删除，项目目录保留。`)) return;
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
        <p className="text-muted-foreground">停止并删除项目里的所有容器、容器组和项目网络。项目目录（含数据和快照）默认保留在 {project.dir}。</p>
        <label className="flex items-center gap-2">
          <input type="checkbox" checked={deleteDir} onChange={(e) => setDeleteDir(e.target.checked)} />
          同时删除项目目录（数据、快照都会删掉，无法恢复）
        </label>
        {deleteDir && <Input value={confirmName} onChange={(e) => setConfirmName(e.target.value)} placeholder={`输入项目名 ${project.name} 确认`} />}
        <div>
          <Button variant="destructive" size="sm" onClick={remove}>
            删除项目
          </Button>
        </div>
      </CardContent>
    </Card>
  );
}
