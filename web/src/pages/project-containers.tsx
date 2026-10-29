import { useEffect, useRef, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { Plus, Rocket } from "lucide-react";
import { errorMessage, nodeClient, projectClient, toDate } from "@/lib/api";
import { cn, timeAgo } from "@/lib/utils";
import { locale, t } from "@/i18n";
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
    <div className="grid gap-6">
      {imageUpdates.length > 0 && (
        <Notice tone="info" title={t.projectContainers.imageUpdates.title}>
          {t.projectContainers.imageUpdates.body(
            imageUpdates.map((u) => t.projectContainers.imageUpdates.item(u.container, u.service, u.image)).join(t.projectContainers.listSeparator),
          )}
        </Notice>
      )}
      <ContainersTable project={project} services={services} onOpen={open} onAdd={() => setAdding(true)} />
      {adding && <AddContainer project={project} onClose={() => setAdding(false)} onAdded={open} />}
      <Section
        title={t.projectContainers.monitoring}
        description={t.projectContainers.projectMetrics.description}
        help={t.projectContainers.projectMetrics.help}
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
  return m.kind === "group" ? t.projectContainers.kind.group : t.projectContainers.kind.container;
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
  const { run, busy, output, hasOutput } = useRun(project.id);
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
      // 宽屏时下面的表格不显示，标题行下面不留空（部署输出自己带上边距）。
      className="xl:[&>header]:mb-0"
      title={t.projectContainers.table.title}
      // 宽屏时容器列在左边的项目栏里，这里只剩标题和操作一行。
      description={<span className="xl:hidden">{t.projectContainers.table.descriptionNarrow}</span>}
      help={t.projectContainers.table.help}
      actions={
        <>
          <Button variant="ghost" size="sm" onClick={onAdd}>
            <Plus />
            {t.projectContainers.table.addContainer}
          </Button>
          {/* 批量部署只在有多个容器时出现；只有一个容器时用下面详情里的「部署」。 */}
          {multi && changed.length > 0 && targets.length === 0 && (
            <Button
              variant="outline"
              size="sm"
              disabled={busy}
              onClick={() => deploy(changed)}
              title={t.projectContainers.table.changedTitle(changed.map((m) => m.name).join(t.projectContainers.listSeparator))}
            >
              {busy ? <Spinner /> : <Rocket />}
              {t.projectContainers.table.deployChanged(changed.length)}
            </Button>
          )}
          {multi && targets.length > 0 && (
            <Button size="sm" disabled={busy} onClick={() => deploy(targets)}>
              {busy ? <Spinner /> : <Rocket />}
              {t.projectContainers.table.deploySelected(targets.length)}
            </Button>
          )}
        </>
      }
    >
      {project.containers.length === 0 ? (
        <div className="xl:hidden">
          <EmptyRow>{t.projectContainers.table.empty}</EmptyRow>
        </div>
      ) : (
        <Table className="xl:hidden">
          <thead>
            <tr>
              <Th className="w-10" />
              <Th>{t.projectContainers.columns.name}</Th>
              <Th>{t.projectContainers.columns.node}</Th>
              <Th>{t.projectContainers.columns.status}</Th>
              {/* 窄屏只留名称、节点、状态，不用横向滚动。 */}
              <Th className="hidden sm:table-cell">{t.projectContainers.columns.usage}</Th>
              <Th className="hidden md:table-cell">{t.projectContainers.columns.version}</Th>
              <Th className="hidden md:table-cell">{t.projectContainers.columns.address}</Th>
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
                      aria-label={t.projectContainers.table.select(m.name)}
                      checked={!!checked[m.name]}
                      onChange={(e) => setChecked({ ...checked, [m.name]: e.target.checked })}
                    />
                  </Td>
                  <Td>
                    <div className="flex flex-wrap items-center gap-1.5">
                      <span className="font-medium group-hover:underline">{m.name}</span>
                      <span className="text-xs text-muted-foreground">{containerKind(m)}</span>
                      {m.hasDraft && <Badge tone="warning">{t.projectContainers.badges.draft}</Badge>}
                      {m.sharedEnvOutdated && <Badge tone="warning">{t.projectContainers.badges.sharedEnvPending}</Badge>}
                      {m.stateful && <Badge>{t.projectContainers.badges.stateful}</Badge>}
                      {m.hostNetworkServices.length > 0 && <Badge tone="danger">{t.projectContainers.badges.hostNetwork}</Badge>}
                      {m.unmanagedVolumes.length > 0 && <Badge tone="warning">{t.projectContainers.badges.volumeUnmanaged}</Badge>}
                    </div>
                  </Td>
                  <Td className="text-[13px] text-muted-foreground">
                    <span className="inline-flex items-center gap-1.5">
                      {nodeName(m.nodeId)}
                      {nodeOffline(m.nodeId) && <Badge tone="warning">{t.common.status.offline}</Badge>}
                    </span>
                  </Td>
                  <Td>
                    {nodeOffline(m.nodeId) ? <Status tone="warning">{t.projectContainers.nodeOffline}</Status> : <ProjectState services={svc} deployed={m.deployedVersion > 0n} />}
                  </Td>
                  <Td className="hidden sm:table-cell">
                    <UsageBars usage={u} memFallback={memFallback} className="w-36" />
                  </Td>
                  <Td className="hidden font-mono text-xs md:table-cell">
                    {m.deployedVersion > 0n ? `v${m.deployedVersion}` : <span className="text-muted-foreground">—</span>}
                    {m.latestVersion !== m.deployedVersion && m.deployedVersion > 0n && <span className="text-muted-foreground">{t.projectContainers.table.latest(String(m.latestVersion))}</span>}
                  </Td>
                  <Td className="hidden font-mono text-xs text-muted-foreground md:table-cell">
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
      {hasOutput && <div className="pt-3 xl:pt-4">{output}</div>}
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
    { label: t.projectContainers.page.projects, to: "/projects" },
    { label: p.name, to: `/projects/${p.id}` },
  ];
  if (!container) {
    return (
      <>
        <Breadcrumb items={crumbs} className="xl:hidden" />
        <Alert>{t.projectContainers.page.notFound(name)}</Alert>
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
  const isGroup = container.kind === "group";
  const serviceNames = [...new Set(services.map((s) => s.name))];

  const deploy = (opts: { pull?: boolean; useUpload?: boolean; pullImages?: boolean }, keep = false) =>
    run(() => projectClient.deploy({ projectId: project.id, targets: target, ...opts }), keep);

  // 上传压缩包：浏览器 → 面板 → mesh → 节点暂存，然后部署时替换容器的 data/。
  const upload = async (file: File) => {
    onView("overview");
    setLines([t.projectContainers.detail.uploading(file.name, (file.size / 1048576).toFixed(1))]);
    const q = new URLSearchParams({ project: project.id, container: container.name });
    const res = await fetch(`/api/upload?${q}`, { method: "POST", headers: { "X-Nagi-Upload": "1", "Accept-Language": locale }, body: file });
    const body = await res.json();
    if (!res.ok) {
      setLines((l) => [...l, t.projectContainers.detail.uploadFailed(body.message)]);
      return;
    }
    setLines((l) => [...l, t.projectContainers.detail.uploaded(body.files, body.stripped)]);
    await deploy({ useUpload: true }, true);
  };

  const remove = async (): Promise<boolean> => {
    const deleteDir = confirm(t.projectContainers.detail.deleteConfirm(isGroup, container.name));
    if (!deleteDir && !confirm(t.projectContainers.detail.deleteContainersOnlyConfirm(isGroup, container.name))) return false;
    try {
      await projectClient.deleteContainer({ projectId: project.id, container: container.name, baseRevision: container.revision, deleteDir });
      queryClient.invalidateQueries({ queryKey: ["project", project.id] });
      return true;
    } catch (err) {
      setLines([t.projectContainers.detail.deleteFailed(errorMessage(err))]);
      return false;
    }
  };

  const summary = container.hasDraft
    ? t.projectContainers.detail.summaryDraft
    : container.deployedVersion > 0n
      ? t.projectContainers.detail.summaryRunning(String(container.deployedVersion))
      : t.projectContainers.detail.summaryNotDeployed;

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
            {container.hasDraft && <Badge tone="warning">{t.projectContainers.badges.draft}</Badge>}
            {container.stateful && <Badge>{t.projectContainers.badges.stateful}</Badge>}
          </>
        }
        meta={
          <>
            <span>{isGroup ? t.projectContainers.kind.groupCompose : t.projectContainers.kind.container}</span>
            <span>{node?.name ?? container.nodeId}</span>
            <span className="font-mono">{container.dir}</span>
            <span>{summary + (container.sharedEnvOutdated ? t.projectContainers.detail.sharedEnvOutdated : "")}</span>
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
                {t.projectContainers.detail.uploadDeploy}
              </Button>
            )}
            {cfg?.source?.gitUrl && (
              <Button variant="outline" size="sm" onClick={() => deploy({ pull: true })} disabled={busy}>
                {t.projectContainers.detail.pullDeploy}
              </Button>
            )}
            {hasImageUpdate && (
              <Button variant="outline" size="sm" onClick={() => deploy({ pullImages: true })} disabled={busy}>
                {t.projectContainers.detail.updateImage}
              </Button>
            )}
            <Button size="sm" onClick={() => deploy({})} disabled={busy}>
              {busy ? <Spinner /> : <Rocket />}
              {busy
                ? t.projectContainers.detail.deploying
                : container.hasDraft
                  ? t.projectContainers.detail.deployDraft
                  : container.deployedVersion > 0n
                    ? t.projectContainers.detail.redeploy
                    : t.projectContainers.detail.deploy}
            </Button>
            <Menu
              items={[
                {
                  label: t.projectContainers.detail.deleteKind(isGroup),
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
          { value: "overview", label: t.projectContainers.tabs.overview },
          { value: "config", label: t.projectContainers.tabs.config },
          { value: "versions", label: t.projectContainers.tabs.versions },
          { value: "logs", label: t.projectContainers.tabs.logs },
          { value: "terminal", label: t.projectContainers.tabs.terminal },
          { value: "data", label: t.projectContainers.tabs.data },
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
    <div className="grid gap-6">
      {/* 节点和目录在页面标题下面；这里只在节点离线时提示。 */}
      {node && !node.online && (
        <Notice tone="warning" title={t.projectContainers.nodeOffline}>
          {t.projectContainers.overview.nodeOfflineBody(node.name)}
        </Notice>
      )}
      {container.hostNetworkServices.length > 0 && (
        <Notice tone="warning" title={t.projectContainers.overview.hostNetworkTitle}>
          {t.projectContainers.overview.hostNetworkBody(container.hostNetworkServices.join(t.projectContainers.listSeparator))}
        </Notice>
      )}
      {container.unmanagedVolumes.length > 0 && (
        <Notice tone="warning" title={t.projectContainers.badges.volumeUnmanaged}>
          {t.projectContainers.overview.volumeBody(container.unmanagedVolumes.join(t.projectContainers.listSeparator))}
        </Notice>
      )}
      <Section title={t.projectContainers.overview.servicesTitle} flush>
        <Table>
          <thead>
            <tr>
              <Th>{t.projectContainers.columns.container}</Th>
              <Th>{t.projectContainers.columns.status}</Th>
              <Th>{t.projectContainers.columns.image}</Th>
              <Th>{t.projectContainers.columns.details}</Th>
            </tr>
          </thead>
          <tbody>
            {services.length === 0 ? (
              <tr>
                <Td colSpan={4} className="text-center text-[13px] text-muted-foreground">
                  {t.projectContainers.overview.notDeployed}
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
      </Section>
      {cfg?.source?.gitUrl && (
        <Section title={t.projectContainers.overview.gitTitle}>
          <KeyValue
            items={[
              [
                t.projectContainers.overview.gitRepo,
                <span className="font-mono">
                  {cfg.source.gitUrl}
                  {cfg.source.gitRef && ` @ ${cfg.source.gitRef}`}
                </span>,
              ],
              [t.projectContainers.overview.gitCommit, <span className="font-mono">{container.lastCommit ? container.lastCommit.slice(0, 12) : "—"}</span>],
              [
                <span className="inline-flex items-center gap-1">deploy key</span>,
                <div className="grid gap-1">
                  <CodeBlock copy>{container.deployKey}</CodeBlock>
                  <span className="text-xs text-muted-foreground">{t.projectContainers.overview.deployKeyHint}</span>
                </div>,
              ],
              [
                "webhook",
                <div className="grid gap-1">
                  <CodeBlock copy>{`${location.origin}/hooks/${container.webhookToken}`}</CodeBlock>
                  <span className="text-xs text-muted-foreground">{t.projectContainers.overview.webhookHint}</span>
                </div>,
              ],
            ]}
          />
        </Section>
      )}
      <Section title={t.projectContainers.monitoring} description={t.projectContainers.overview.metricsDescription} actions={picker}>
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
        <span className="text-[13px] text-muted-foreground">{t.projectContainers.logs.live(maxLines)}</span>
        {services.length > 1 && (
          <Select value={service} onChange={(e) => setService(e.target.value)} className="h-8 w-40 text-[13px]">
            <option value="">{t.projectContainers.logs.allContainers}</option>
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
          <span className="opacity-50">{t.projectContainers.logs.empty}</span>
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
      <Notice>{t.projectContainers.config.notice}</Notice>
      {isForm ? (
        imageForm.fields
      ) : (
        <Field label="compose" htmlFor="edit-compose" help={t.projectContainers.config.composeHelp}>
          <Textarea id="edit-compose" value={compose} onChange={(e) => setCompose(e.target.value)} spellCheck={false} className="min-h-56 font-mono text-[13px]" />
        </Field>
      )}
      <div className="grid gap-4 border-t pt-5 sm:grid-cols-3">
        <Field
          label={t.projectContainers.config.git}
          htmlFor="edit-git"
          className="sm:col-span-2"
          help={t.projectContainers.config.gitHelp}
        >
          <Input id="edit-git" value={gitUrl} onChange={(e) => setGitUrl(e.target.value)} placeholder="git@github.com:org/app.git" className="font-mono" />
        </Field>
        <Field label={t.projectContainers.config.ref} htmlFor="edit-ref" hint={t.projectContainers.config.refHint}>
          <Input id="edit-ref" value={gitRef} onChange={(e) => setGitRef(e.target.value)} className="font-mono" />
        </Field>
        <Field
                  label={t.projectContainers.config.preserve}
                  htmlFor="edit-preserve"
                  className="sm:col-span-2"
                  hint={t.projectContainers.config.preserveHint}
                  help={t.projectContainers.config.preserveHelp}
                >
          <Textarea id="edit-preserve" value={preserve} onChange={(e) => setPreserve(e.target.value)} spellCheck={false} className="min-h-16 font-mono text-[13px]" placeholder="uploads/" />
        </Field>
        <Field label={t.projectContainers.config.dependsOn} htmlFor="edit-deps" hint={t.projectContainers.config.dependsOnHint} help={t.projectContainers.config.dependsOnHelp}>
          <Input id="edit-deps" value={dependsOn} onChange={(e) => setDependsOn(e.target.value)} className="font-mono" placeholder="db" />
        </Field>
      </div>
      {!isForm && (
        <div className="grid gap-4 border-t pt-5">
          <Field label={t.projectContainers.config.groupEnv} htmlFor="edit-env" hint={t.projectContainers.config.envHint} help={t.projectContainers.config.groupEnvHelp}>
            <Textarea id="edit-env" value={env} onChange={(e) => setEnv(e.target.value)} spellCheck={false} className="font-mono text-[13px]" />
          </Field>
          {services.map((s) => (
            <Field
                          key={s}
                          label={t.projectContainers.config.serviceEnv(s)}
                          htmlFor={`edit-env-${s}`}
                          hint={t.projectContainers.config.envHint}
                          help={t.projectContainers.config.serviceEnvHelp}
                        >
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
        {saved && <span className="mr-auto text-[13px] text-muted-foreground">{t.projectContainers.config.savedDraft}</span>}
        {container.hasDraft && (
          <Button variant="ghost" size="sm" onClick={discard} disabled={busy}>
            {t.projectContainers.config.discardDraft}
          </Button>
        )}
        <Button type="submit" size="sm" disabled={busy}>
          {busy && <Spinner />}
          {t.projectContainers.config.saveDraft}
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
        <span className="text-[13px] text-muted-foreground">{t.projectContainers.versions.intro}</span>
        <Check checked={restoreData} onChange={setRestoreData} help={t.projectContainers.versions.restoreDataHelp}>
          <span className="text-[13px]">{t.projectContainers.versions.restoreData}</span>
        </Check>
      </div>
      {output}
      <div>
        <Table compact>
          <thead>
            <tr>
              <Th>{t.projectContainers.columns.version}</Th>
              <Th>{t.projectContainers.columns.deployedAt}</Th>
              <Th>{t.projectContainers.columns.image}</Th>
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
                  {t.projectContainers.versions.empty}
                </Td>
              </tr>
            ) : (
              versions.data?.versions.map((v) => (
                <tr key={String(v.version)}>
                  <Td className="font-mono text-xs">
                    v{String(v.version)}
                    {v.version === container.deployedVersion && (
                      <Badge tone="success" className="ml-2">
                        {t.projectContainers.versions.current}
                      </Badge>
                    )}
                  </Td>
                  <Td className="text-[13px] text-muted-foreground">
                    {v.deployedAt ? timeAgo(toDate(v.deployedAt)) : t.projectContainers.versions.deployFailed}
                    {v.rollbackOf > 0n && t.projectContainers.versions.rollbackOf(String(v.rollbackOf))}
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
                          confirm(t.projectContainers.versions.rollbackConfirm(container.name, String(v.version), restoreData)) &&
                          (onRun(), run(() => projectClient.rollback({ ...ref, version: v.version, restoreData })))
                        }
                      >
                        {t.projectContainers.versions.rollback}
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
    <Section title={t.projectContainers.add.title} description={t.projectContainers.add.description}>
      <form
        className="grid gap-4"
        onSubmit={(e) => {
          e.preventDefault();
          add();
        }}
      >
        <Field
          label={t.projectContainers.columns.name}
          htmlFor="new-container"
          hint={t.projectContainers.add.nameHint}
          help={t.projectContainers.add.nameHelp}
        >
          <Input id="new-container" value={name} onChange={(e) => setName(e.target.value)} required className="max-w-80 font-mono" placeholder="api" />
        </Field>
        {node.fields}
        {form.fields}
        {error && <Alert>{error}</Alert>}
        <div className="flex justify-end gap-2 pt-2">
          {project.containers.length > 0 && (
            <Button variant="ghost" size="sm" onClick={onClose}>
              {t.common.actions.cancel}
            </Button>
          )}
          <Button type="submit" size="sm" disabled={busy || !node.nodeId}>
            {busy && <Spinner />}
            {t.common.actions.add}
          </Button>
        </div>
      </form>
    </Section>
  );
}
