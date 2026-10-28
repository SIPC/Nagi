import { useEffect, useRef, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { Plus, Rocket } from "lucide-react";
import { errorMessage, nodeClient, projectClient, toDate } from "@/lib/api";
import { cn, timeAgo } from "@/lib/utils";
import { envToText, lines, textToEnv } from "@/lib/config";
import { useRun } from "@/components/run";
import { ContainerMetrics, containerUsage, ProjectMetrics, UsageBars, useProjectMetrics, useRange } from "@/components/metrics";
import { ProjectState, ServiceState } from "@/components/status";
import { TerminalView } from "@/components/terminal";
import { useImageForm, useContainerForm } from "@/components/container-form";
import { useNodeChoice } from "@/pages/projects";
import { Breadcrumb, PageHeader } from "@/pages/layout";
import { ContainerData } from "@/pages/project-data";
import {
  Alert,
  Badge,
  Button,
  Check,
  CodeBlock,
  EmptyRow,
  Field,
  Input,
  KeyValue,
  Loading,
  Menu,
  Status,
  Notice,
  Section,
  Select,
  Spinner,
  Table,
  Tabs,
  Td,
  Textarea,
  Th,
} from "@/components/ui";
import type { Container, Project, ServiceStatus } from "@/gen/nagi/v1/common_pb";

// 项目「概览」：容器列表、批量部署、添加容器、项目的监控；以及容器页（概览、配置、版本、日志、终端、数据）。
// 宽屏时容器列在项目栏里（含状态、CPU、内存），概览里不再重复这张表，只保留添加和部署的按钮。

type ImageUpdate = { container: string; service: string; image: string };
export const containerTabs = ["overview", "config", "versions", "logs", "terminal", "data"] as const;
export type DetailView = (typeof containerTabs)[number];

export function ContainersTab({
  project,
  services,
  imageUpdates,
}: {
  project: Project;
  services: ServiceStatus[];
  imageUpdates: ImageUpdate[];
}) {
  const [adding, setAdding] = useState(project.containers.length === 0);
  const navigate = useNavigate();
  const { range, picker } = useRange();
  const open = (name: string) => navigate({ to: "/projects/$projectId/c/$container", params: { projectId: project.id, container: name } });

  return (
    <div className="grid gap-8">
      {imageUpdates.length > 0 && (
        <Notice tone="info" title="镜像有新版本">
          {imageUpdates.map((u) => `${u.container}/${u.service}（${u.image}）`).join("、")}。在容器页选择「更新镜像」拉取并部署。
        </Notice>
      )}
      <ContainersTable project={project} services={services} onOpen={open} onAdd={() => setAdding(true)} />
      {adding && <AddContainer project={project} onClose={() => setAdding(false)} onAdded={open} />}
      <Section
        title="监控"
        description="所有容器的资源占用合计及端口映射的请求情况。"
        help="各容器的监控见容器页。耗时图中每个时段的 P95、P99 取该时段内的最大值，其余指标取平均值。"
        actions={picker}
      >
        <ProjectMetrics projectId={project.id} nodeIds={[...new Set(project.containers.map((c) => c.nodeId))]} range={range} />
      </Section>
    </div>
  );
}

// shortImage 把固定了摘要的镜像（回滚、迁移、顶替后会固定到当时的摘要）缩短显示，例如 redis@sha256:858f009f9709。
function shortImage(image: string) {
  const i = image.indexOf("@sha256:");
  return i < 0 ? image : image.slice(0, i + 8 + 12);
}

// containerKind 返回"容器"（单个容器，镜像表单）或"容器组"（一份 compose，里面有几个容器）。
export function containerKind(m: Container) {
  return m.kind === "group" ? "容器组" : "容器";
}

// ---- 容器列表与批量部署 ----

function ContainersTable({
  project,
  services,
  onOpen,
  onAdd,
}: {
  project: Project;
  services: ServiceStatus[];
  onOpen: (name: string) => void;
  onAdd: () => void;
}) {
  const { run, busy, output } = useRun(project.id);
  const nodes = useQuery({ queryKey: ["nodes"], queryFn: () => nodeClient.listNodes({}) });
  const nodeName = (id: string) => nodes.data?.nodes.find((n) => n.id === id)?.name ?? id;
  const nodeOffline = (id: string) => nodes.data?.nodes.find((n) => n.id === id)?.online === false;
  const [checked, setChecked] = useState<Record<string, boolean>>({});
  const targets = project.containers.filter((m) => checked[m.name]);
  const changed = project.containers.filter((m) => m.hasDraft || m.sharedEnvOutdated || m.deployedVersion === 0n);
  const multi = project.containers.length > 1;
  const usage = containerUsage(useProjectMetrics(project.id, project.containers.map((c) => c.nodeId)).data);
  const memFallback = Math.max(0, ...[...usage.values()].map((u) => u.mem));

  const deploy = (containers: Container[]) =>
    run(() =>
      projectClient.deploy({ projectId: project.id,
        targets: containers.map((m) => ({ container: m.name, baseRevision: m.revision })),
      }),
    );

  return (
    <Section
      flush
      title="容器"
      description={
        <>
          <span className="xl:hidden">选择容器查看详情；勾选多个可批量部署。</span>
          <span className="hidden xl:inline">在左侧项目栏中选择容器查看详情。</span>
        </>
      }
      help="批量部署按依赖顺序依次部署所选容器，并生成一条部署记录。"
      actions={
        <>
          <Button variant="ghost" size="sm" onClick={onAdd}>
            <Plus />
            添加容器
          </Button>
          {/* 批量部署只在有多个容器时出现；只有一个容器时用下面详情里的「部署」。 */}
          {multi && changed.length > 0 && targets.length === 0 && (
            <Button
              variant="outline"
              size="sm"
              disabled={busy}
              onClick={() => deploy(changed)}
              title={"存在草稿、共享变量已变更或尚未部署的容器：" + changed.map((m) => m.name).join("、")}
            >
              {busy ? <Spinner /> : <Rocket />}
              部署全部变更（{changed.length}）
            </Button>
          )}
          {multi && targets.length > 0 && (
            <Button size="sm" disabled={busy} onClick={() => deploy(targets)}>
              {busy ? <Spinner /> : <Rocket />}
              部署所选（{targets.length}）
            </Button>
          )}
        </>
      }
    >
      {project.containers.length === 0 ? (
        <div className="xl:hidden">
          <EmptyRow>暂无容器</EmptyRow>
        </div>
      ) : (
        <Table className="xl:hidden">
          <thead>
            <tr>
              <Th className="w-10" />
              <Th>名称</Th>
              <Th>节点</Th>
              <Th>状态</Th>
              <Th>CPU / 内存</Th>
              <Th>版本</Th>
              <Th>内网地址</Th>
            </tr>
          </thead>
          <tbody>
            {project.containers.map((m) => {
              const svc = services.filter((s) => s.container === m.name);
              const u = nodeOffline(m.nodeId) ? undefined : usage.get(m.name);
              return (
                <tr key={m.name} className="group cursor-pointer transition-colors hover:bg-subtle" onClick={() => onOpen(m.name)}>
                  <Td onClick={(e) => e.stopPropagation()}>
                    <input
                      type="checkbox"
                      aria-label={`选择 ${m.name}`}
                      checked={!!checked[m.name]}
                      onChange={(e) => setChecked({ ...checked, [m.name]: e.target.checked })}
                    />
                  </Td>
                  <Td>
                    <div className="flex flex-wrap items-center gap-1.5">
                      <span className="font-medium group-hover:underline">{m.name}</span>
                      <span className="text-xs text-muted-foreground">{containerKind(m)}</span>
                      {m.hasDraft && <Badge tone="warning">草稿</Badge>}
                      {m.sharedEnvOutdated && <Badge tone="warning">共享变量待部署</Badge>}
                      {m.stateful && <Badge>有状态</Badge>}
                      {m.hostNetworkServices.length > 0 && <Badge tone="danger">host 网络</Badge>}
                      {m.unmanagedVolumes.length > 0 && <Badge tone="warning">volume 未纳入快照</Badge>}
                    </div>
                  </Td>
                  <Td className="text-[13px] text-muted-foreground">
                    <span className="inline-flex items-center gap-1.5">
                      {nodeName(m.nodeId)}
                      {nodeOffline(m.nodeId) && <Badge tone="warning">离线</Badge>}
                    </span>
                  </Td>
                  <Td>
                    {nodeOffline(m.nodeId) ? <Status tone="warning">节点离线</Status> : <ProjectState services={svc} deployed={m.deployedVersion > 0n} />}
                  </Td>
                  <Td>
                    <UsageBars usage={u} memFallback={memFallback} className="w-36" />
                  </Td>
                  <Td className="font-mono text-xs">
                    {m.deployedVersion > 0n ? `v${m.deployedVersion}` : <span className="text-muted-foreground">—</span>}
                    {m.latestVersion !== m.deployedVersion && m.deployedVersion > 0n && <span className="text-muted-foreground"> / 最新 v{String(m.latestVersion)}</span>}
                  </Td>
                  <Td className="font-mono text-xs text-muted-foreground">
                    {m.kind !== "group"
                      ? m.name
                      : [...new Set(svc.filter((s) => !m.hostNetworkServices.includes(s.name)).map((s) => `${s.name}.${m.name}`))].join(" ") || "—"}
                  </Td>
                </tr>
              );
            })}
          </tbody>
        </Table>
      )}
      {output && <div className="pt-3 empty:hidden">{output}</div>}
    </Section>
  );
}

// ---- 一个容器 ----

// ContainerPage：容器自己的页（项目 / 容器），标签 概览、配置、版本、日志、终端、数据；当前标签记在地址的 view 参数里。
// 宽屏时左边有项目栏（见 ProjectSplit），不再显示上级链接。
export function ContainerPage({ projectId, name, view, onView }: { projectId: string; name: string; view: DetailView; onView: (v: DetailView) => void }) {
  const project = useQuery({
    queryKey: ["project", projectId],
    queryFn: () => projectClient.getProject({ projectId }),
    refetchInterval: 5000,
  });
  if (project.isPending) return <Loading />;
  if (project.error) return <Alert>{errorMessage(project.error)}</Alert>;
  const p = project.data.project;
  if (!p) return null;
  const container = p.containers.find((c) => c.name === name);
  const crumbs = [
    { label: "项目", to: "/projects" },
    { label: p.name, to: `/projects/${p.id}` },
  ];
  if (!container) {
    return (
      <>
        <Breadcrumb items={crumbs} className="xl:hidden" />
        <Alert>项目中不存在容器 {name}，可能已被删除。</Alert>
      </>
    );
  }
  const services = (project.data.status?.services ?? []).filter((s) => s.container === name);
  const hasImageUpdate = (project.data.status?.imageUpdates ?? []).some((u) => u.container === name);
  return (
    <>
      <Breadcrumb items={[...crumbs, { label: name }]} className="xl:hidden" />
      <ContainerDetail project={p} container={container} services={services} hasImageUpdate={hasImageUpdate} view={view} onView={onView} />
    </>
  );
}

function ContainerDetail({
  project,
  container,
  services,
  hasImageUpdate,
  view,
  onView,
}: {
  project: Project;
  container: Container;
  services: ServiceStatus[];
  hasImageUpdate: boolean;
  view: DetailView;
  onView: (v: DetailView) => void;
}) {
  const { run, busy, output, setLines } = useRun(project.id);
  const queryClient = useQueryClient();
  const fileRef = useRef<HTMLInputElement>(null);
  // 换了容器时，上一个容器的部署输出不再显示。
  useEffect(() => {
    if (!busy) setLines([]);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [container.name]);
  const cfg = container.draft ?? container.config;
  const target = [{ container: container.name, baseRevision: container.revision }];
  const kind = containerKind(container);
  const serviceNames = [...new Set(services.map((s) => s.name))];

  const deploy = (opts: { pull?: boolean; useUpload?: boolean; pullImages?: boolean }, keep = false) =>
    run(() => projectClient.deploy({ projectId: project.id, targets: target, ...opts }), keep);

  // 上传压缩包：浏览器 → 面板 → mesh → 节点暂存，然后部署时替换容器的 data/。
  const upload = async (file: File) => {
    onView("overview");
    setLines([`上传 ${file.name}（${(file.size / 1048576).toFixed(1)} MB）`]);
    const q = new URLSearchParams({ project: project.id, container: container.name });
    const res = await fetch(`/api/upload?${q}`, { method: "POST", headers: { "X-Nagi-Upload": "1" }, body: file });
    const body = await res.json();
    if (!res.ok) {
      setLines((l) => [...l, "上传失败：" + body.message]);
      return;
    }
    setLines((l) => [...l, `已上传 ${body.files} 个文件${body.stripped ? `（已去除顶层目录 ${body.stripped}/）` : ""}`]);
    await deploy({ useUpload: true }, true);
  };

  const remove = async (): Promise<boolean> => {
    const deleteDir = confirm(
      `删除${kind} ${container.name}？${kind === "容器组" ? "组内的容器" : "容器"}将被停止并删除。\n\n选择「确定」同时删除${kind}目录及其中的数据；选择「取消」仅删除容器并保留目录。`,
    );
    if (!deleteDir && !confirm(`仅删除${kind} ${container.name} 的容器？${kind}目录及其中的数据将保留。`)) return false;
    try {
      await projectClient.deleteContainer({ projectId: project.id, container: container.name, baseRevision: container.revision, deleteDir });
      queryClient.invalidateQueries({ queryKey: ["project", project.id] });
      return true;
    } catch (err) {
      setLines(["删除失败：" + errorMessage(err)]);
      return false;
    }
  };

  const summary = container.hasDraft
    ? "存在未部署的草稿，部署时保存为新版本。"
    : container.deployedVersion > 0n
      ? `当前运行版本 v${container.deployedVersion}。`
      : "尚未部署。";

  const nodes = useQuery({ queryKey: ["nodes"], queryFn: () => nodeClient.listNodes({}) });
  const node = nodes.data?.nodes.find((n) => n.id === container.nodeId);
  const navigate = useNavigate();

  return (
    <>
      <PageHeader
        title={
          <>
            {container.name}
            <ProjectState services={services} deployed={container.deployedVersion > 0n} />
            {container.hasDraft && <Badge tone="warning">草稿</Badge>}
            {container.stateful && <Badge>有状态</Badge>}
          </>
        }
        meta={
          <>
            <span>{kind === "容器组" ? "容器组（compose）" : "容器"}</span>
            <span>{node?.name ?? container.nodeId}</span>
            <span className="font-mono">{container.dir}</span>
            <span>{summary + (container.sharedEnvOutdated ? "共享变量有更新，下次部署生效。" : "")}</span>
          </>
        }
        actions={
          <>
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
            <Menu
              items={[
                {
                  label: `删除${kind}`,
                  danger: true,
                  onSelect: async () => {
                    if (await remove()) navigate({ to: "/projects/$projectId", params: { projectId: project.id } });
                  },
                },
              ]}
            />
          </>
        }
      />
      <Tabs
        className="mb-6"
        value={view}
        onChange={onView}
        items={[
          { value: "overview", label: "概览" },
          { value: "config", label: "配置" },
          { value: "versions", label: "版本" },
          { value: "logs", label: "日志" },
          { value: "terminal", label: "终端" },
          { value: "data", label: "数据" },
        ]}
      />
      <div key={view} className="grid animate-fade-in gap-6">
        {view !== "data" && output}
        {view === "overview" && <Overview project={project} container={container} services={services} />}
        {view === "config" && <ContainerConfig key={`cfg-${container.revision}`} project={project} container={container} services={serviceNames} />}
        {view === "versions" && <Versions project={project} container={container} onRun={() => !busy && setLines([])} />}
        {view === "logs" && (
          <Logs
            projectId={project.id}
            container={container.name}
            services={serviceNames}
            containers={services.map((s) => s.containerId).filter(Boolean).join(",")}
          />
        )}
        {view === "terminal" && (
          <TerminalView projectId={project.id} container={container.name} services={services.filter((s) => s.state === "running").map((s) => s.name)} />
        )}
        {view === "data" && <ContainerData project={project} container={container} />}
      </div>
    </>
  );
}

function Overview({ project, container, services }: { project: Project; container: Container; services: ServiceStatus[] }) {
  const cfg = container.draft ?? container.config;
  const { range, picker } = useRange();
  const nodes = useQuery({ queryKey: ["nodes"], queryFn: () => nodeClient.listNodes({}) });
  const node = nodes.data?.nodes.find((n) => n.id === container.nodeId);
  return (
    <div className="grid gap-5">
      {/* 节点和目录在页面标题下面；这里只在节点离线时提示。 */}
      {node && !node.online && (
        <Notice tone="warning" title="节点离线">
          容器所在节点 {node.name} 离线，状态可能未及时更新。
        </Notice>
      )}
      {container.hostNetworkServices.length > 0 && (
        <Notice tone="warning" title="使用 host 网络">
          容器 {container.hostNetworkServices.join("、")} 不经过 Caddy，不在项目内网中，端口对外开放，需自行配置防火墙。
        </Notice>
      )}
      {container.unmanagedVolumes.length > 0 && (
        <Notice tone="warning" title="volume 未纳入快照">
          {container.unmanagedVolumes.join("、")} 为 external 或使用自定义 driver，快照、迁移和顶替均不包含其数据。
        </Notice>
      )}
      <div>
        <Table>
          <thead>
            <tr>
              <Th>容器</Th>
              <Th>状态</Th>
              <Th>镜像</Th>
              <Th>详情</Th>
            </tr>
          </thead>
          <tbody>
            {services.length === 0 ? (
              <tr>
                <Td colSpan={4} className="text-center text-[13px] text-muted-foreground">
                  尚未部署
                </Td>
              </tr>
            ) : (
              services.map((s) => (
                <tr key={s.name + s.containerId}>
                  <Td className="font-medium">{s.name}</Td>
                  <Td>
                    <ServiceState service={s} />
                  </Td>
                  <Td className="font-mono text-xs text-muted-foreground" title={s.image}>
                    {shortImage(s.image)}
                  </Td>
                  <Td className="text-xs text-muted-foreground">{s.status}</Td>
                </tr>
              ))
            )}
          </tbody>
        </Table>
      </div>
      {cfg?.source?.gitUrl && (
        <div className="grid gap-3">
          <h3 className="text-[13px] font-semibold">git 仓库</h3>
          <KeyValue
            items={[
              [
                "仓库",
                <span className="font-mono">
                  {cfg.source.gitUrl}
                  {cfg.source.gitRef && ` @ ${cfg.source.gitRef}`}
                </span>,
              ],
              ["当前提交", <span className="font-mono">{container.lastCommit ? container.lastCommit.slice(0, 12) : "—"}</span>],
              [
                <span className="inline-flex items-center gap-1">deploy key</span>,
                <div className="grid gap-1">
                  <CodeBlock copy>{container.deployKey}</CodeBlock>
                  <span className="text-xs text-muted-foreground">需在仓库中添加为只读 deploy key。</span>
                </div>,
              ],
              [
                "webhook",
                <div className="grid gap-1">
                  <CodeBlock copy>{`${location.origin}/hooks/${container.webhookToken}`}</CodeBlock>
                  <span className="text-xs text-muted-foreground">推送后自动拉取并部署此容器。</span>
                </div>,
              ],
            ]}
          />
        </div>
      )}
      <Section title="监控" description="容器的资源占用。" actions={picker}>
        <ContainerMetrics projectId={project.id} nodeIds={project.containers.map((c) => c.nodeId)} container={container.name} range={range} />
      </Section>
    </div>
  );
}

// ---- 日志 ----

type Line = { key: number; service: string; stream: string; text: string };

const maxLines = 2000;

function Logs({ projectId, container, services, containers }: { projectId: string; container: string; services: string[]; containers: string }) {
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
        const stream = projectClient.streamLogs({ projectId, container, service, tail: 200, follow: true }, { signal: abort.signal });
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
  }, [projectId, container, service, containers]);

  useEffect(() => {
    const box = boxRef.current;
    if (box && stick.current) box.scrollTop = box.scrollHeight;
  }, [lines]);

  return (
    <div className="grid gap-3">
      <div className="flex items-center justify-between gap-3">
        <span className="text-[13px] text-muted-foreground">实时日志，最近 {maxLines} 行</span>
        {services.length > 1 && (
          <Select value={service} onChange={(e) => setService(e.target.value)} className="h-8 w-40 text-[13px]">
            <option value="">全部容器</option>
            {services.map((s) => (
              <option key={s} value={s}>
                {s}
              </option>
            ))}
          </Select>
        )}
      </div>
      {error && <Alert>{error}</Alert>}
      <div
        ref={boxRef}
        onScroll={(e) => {
          const el = e.currentTarget;
          stick.current = el.scrollHeight - el.scrollTop - el.clientHeight < 40;
        }}
        className="h-96 overflow-auto rounded-md bg-log p-3.5 font-mono text-xs leading-relaxed text-log-foreground"
      >
        {lines.length === 0 ? (
          <span className="opacity-50">暂无日志</span>
        ) : (
          lines.map((l) => (
            <div key={l.key} className="break-all whitespace-pre-wrap">
              {!service && services.length > 1 && <span className="mr-2 text-info">{l.service}</span>}
              <span className={cn(l.stream === "stderr" && "text-warning")}>{l.text}</span>
            </div>
          ))
        )}
      </div>
    </div>
  );
}

// ---- 配置 ----

function ContainerConfig({ project, container, services }: { project: Project; container: Container; services: string[] }) {
  const queryClient = useQueryClient();
  const base = container.draft ?? container.config;
  const isForm = !!base?.form;
  const [compose, setCompose] = useState(base?.compose ?? "");
  const [env, setEnv] = useState(envToText(base?.env));
  const [serviceEnv, setServiceEnv] = useState<Record<string, string>>(() => Object.fromEntries(services.map((s) => [s, envToText(base?.serviceEnv[s]?.vars)])));
  const imageForm = useImageForm(base?.form, "edit-");
  const [gitUrl, setGitUrl] = useState(base?.source?.gitUrl ?? "");
  const [gitRef, setGitRef] = useState(base?.source?.gitRef ?? "");
  const [preserve, setPreserve] = useState((base?.preserve ?? []).join("\n"));
  const [dependsOn, setDependsOn] = useState((base?.dependsOn ?? []).join(", "));
  const [error, setError] = useState("");
  const [saved, setSaved] = useState(false);
  const [busy, setBusy] = useState(false);

  const refresh = () => {
    queryClient.invalidateQueries({ queryKey: ["project", project.id] });
    queryClient.invalidateQueries({ queryKey: ["projects"] });
  };

  const call = async (fn: () => Promise<unknown>, ok = false) => {
    setBusy(true);
    setError("");
    setSaved(false);
    try {
      await fn();
      setSaved(ok);
      refresh();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const save = () => {
    const common = {
      source: { gitUrl, gitRef },
      preserve: lines(preserve),
      env: textToEnv(env),
      dependsOn: dependsOn.split(/[\s,]+/).filter(Boolean),
      stateful: base?.stateful,
    };
    return call(
      () =>
        projectClient.saveDraft({
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
        }),
      true,
    );
  };

  const discard = () => call(() => projectClient.discardDraft({ projectId: project.id, container: container.name, baseRevision: container.revision }));

  return (
    <form
      className="grid gap-5"
      onSubmit={(e) => {
        e.preventDefault();
        save();
      }}
    >
      <Notice>修改保存为草稿，部署后生效。</Notice>
      {isForm ? (
        imageForm.fields
      ) : (
        <Field label="compose" htmlFor="edit-compose" help="ports 仅在首次出现时导入为端口映射；已有的对外端口和域名在「网络」标签页修改，此处修改 ports 不生效。">
          <Textarea id="edit-compose" value={compose} onChange={(e) => setCompose(e.target.value)} spellCheck={false} className="min-h-56 font-mono text-[13px]" />
        </Field>
      )}
      <div className="grid gap-4 border-t pt-5 sm:grid-cols-3">
        <Field
          label="git 仓库"
          htmlFor="edit-git"
          className="sm:col-span-2"
          help="SSH 地址使用 deploy key 拉取；HTTPS 仅支持公开仓库。留空则由上传的文件提供 data/ 目录。"
        >
          <Input id="edit-git" value={gitUrl} onChange={(e) => setGitUrl(e.target.value)} placeholder="git@github.com:org/app.git" className="font-mono" />
        </Field>
        <Field label="分支或标签" htmlFor="edit-ref" hint="留空则使用默认分支">
          <Input id="edit-ref" value={gitRef} onChange={(e) => setGitRef(e.target.value)} className="font-mono" />
        </Field>
        <Field label="保留路径" htmlFor="edit-preserve" className="sm:col-span-2" hint="相对 data/，每行一个" help="更新文件（上传或 git 拉取）时保留这些路径，例如用户上传的文件。">
          <Textarea id="edit-preserve" value={preserve} onChange={(e) => setPreserve(e.target.value)} spellCheck={false} className="min-h-16 font-mono text-[13px]" placeholder="uploads/" />
        </Field>
        <Field label="依赖" htmlFor="edit-deps" hint="容器名，逗号分隔" help="批量部署时，优先部署所列容器。">
          <Input id="edit-deps" value={dependsOn} onChange={(e) => setDependsOn(e.target.value)} className="font-mono" placeholder="db" />
        </Field>
      </div>
      {!isForm && (
        <div className="grid gap-4 border-t pt-5">
          <Field label="容器组变量" htmlFor="edit-env" hint="KEY=VALUE，每行一个" help="替换 compose 中的 ${变量}；与项目共享变量同名时，以此处为准。">
            <Textarea id="edit-env" value={env} onChange={(e) => setEnv(e.target.value)} spellCheck={false} className="font-mono text-[13px]" />
          </Field>
          {services.map((s) => (
            <Field key={s} label={`环境变量：${s}`} htmlFor={`edit-env-${s}`} hint="KEY=VALUE，每行一个" help="仅注入此容器。">
              <Textarea
                id={`edit-env-${s}`}
                value={serviceEnv[s] ?? ""}
                onChange={(e) => setServiceEnv((prev) => ({ ...prev, [s]: e.target.value }))}
                spellCheck={false}
                className="font-mono text-[13px]"
              />
            </Field>
          ))}
        </div>
      )}
      {error && <Alert>{error}</Alert>}
      <div className="flex items-center justify-end gap-2 pt-2">
        {saved && <span className="mr-auto text-[13px] text-muted-foreground">已保存为草稿</span>}
        {container.hasDraft && (
          <Button variant="ghost" size="sm" onClick={discard} disabled={busy}>
            放弃草稿
          </Button>
        )}
        <Button type="submit" size="sm" disabled={busy}>
          {busy && <Spinner />}
          保存草稿
        </Button>
      </div>
    </form>
  );
}

// ---- 版本 ----

// onRun：开始回滚时调用（上面那次部署的输出就过时了）。
function Versions({ project, container, onRun }: { project: Project; container: Container; onRun: () => void }) {
  const { run, busy, output } = useRun(project.id);
  const ref = { projectId: project.id, container: container.name };
  const versions = useQuery({
    queryKey: ["versions", project.id, container.name, String(container.revision)],
    queryFn: () => projectClient.listVersions(ref),
  });
  const [restoreData, setRestoreData] = useState(false);

  return (
    <div className="grid gap-3">
      <div className="flex items-center justify-between gap-3">
        <span className="text-[13px] text-muted-foreground">每次部署生成一个版本。</span>
        <Check checked={restoreData} onChange={setRestoreData} help="同时将 data/ 和 volumes/ 恢复为该版本部署前的快照。">
          <span className="text-[13px]">回滚时恢复数据</span>
        </Check>
      </div>
      {output}
      <div>
        <Table>
          <thead>
            <tr>
              <Th>版本</Th>
              <Th>部署时间</Th>
              <Th>镜像</Th>
              <Th />
            </tr>
          </thead>
          <tbody>
            {versions.isPending ? (
              <tr>
                <Td colSpan={4} className="text-center">
                  <Spinner className="text-muted-foreground" />
                </Td>
              </tr>
            ) : versions.data?.versions.length === 0 ? (
              <tr>
                <Td colSpan={4} className="text-center text-[13px] text-muted-foreground">
                  暂无版本
                </Td>
              </tr>
            ) : (
              versions.data?.versions.map((v) => (
                <tr key={String(v.version)}>
                  <Td className="font-mono text-xs">
                    v{String(v.version)}
                    {v.version === container.deployedVersion && (
                      <Badge tone="success" className="ml-2">
                        当前
                      </Badge>
                    )}
                  </Td>
                  <Td className="text-[13px] text-muted-foreground">
                    {v.deployedAt ? timeAgo(toDate(v.deployedAt)) : "部署失败"}
                    {v.rollbackOf > 0n && ` · 回滚自 v${v.rollbackOf}`}
                    {v.commit && <span className="font-mono"> · {v.commit.slice(0, 8)}</span>}
                  </Td>
                  <Td className="max-w-80 truncate font-mono text-xs text-muted-foreground" title={Object.values(v.images).join(", ")}>
                    {Object.values(v.images).map(shortImage).join(", ")}
                  </Td>
                  <Td className="text-right">
                    {v.version !== container.deployedVersion && v.deployedAt && (
                      <Button
                        size="xs"
                        variant="ghost"
                        disabled={busy}
                        onClick={() =>
                          confirm(`将 ${container.name} 回滚到 v${v.version}？${restoreData ? "数据将同时恢复为该版本部署前的状态。" : "数据保持不变。"}`) &&
                          (onRun(), run(() => projectClient.rollback({ ...ref, version: v.version, restoreData })))
                        }
                      >
                        回滚
                      </Button>
                    )}
                  </Td>
                </tr>
              ))
            )}
          </tbody>
        </Table>
      </div>
    </div>
  );
}

// ---- 添加容器 ----

function AddContainer({ project, onAdded, onClose }: { project: Project; onAdded: (name: string) => void; onClose: () => void }) {
  const queryClient = useQueryClient();
  const [name, setName] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const form = useContainerForm(project.containers.length === 0);
  const node = useNodeChoice({ projectId: project.id, projectName: project.name, container: name || "api", preferNodes: project.containers.map((m) => m.nodeId) });

  const add = async () => {
    setBusy(true);
    setError("");
    try {
      await projectClient.addContainer({ projectId: project.id, name, config: form.build(), nodeId: node.nodeId, dir: node.dir });
      queryClient.invalidateQueries({ queryKey: ["project", project.id] });
      onAdded(name);
      setName("");
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Section title="添加容器" description="添加后不会自动部署。">
      <form
        className="grid gap-4"
        onSubmit={(e) => {
          e.preventDefault();
          add();
        }}
      >
        <Field
          label="名称"
          htmlFor="new-container"
          hint="小写字母、数字和 -，创建后不可修改"
          help="单个容器在项目内网中以此名称访问；容器组内的容器以「容器.容器组名」访问。"
        >
          <Input id="new-container" value={name} onChange={(e) => setName(e.target.value)} required className="max-w-80 font-mono" placeholder="api" />
        </Field>
        {node.fields}
        {form.fields}
        {error && <Alert>{error}</Alert>}
        <div className="flex justify-end gap-2 pt-2">
          {project.containers.length > 0 && (
            <Button variant="ghost" size="sm" onClick={onClose}>
              取消
            </Button>
          )}
          <Button type="submit" size="sm" disabled={busy || !node.nodeId}>
            {busy && <Spinner />}
            添加
          </Button>
        </div>
      </form>
    </Section>
  );
}
