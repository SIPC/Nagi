import { useContext, useEffect, useRef, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate, useSearch } from "@tanstack/react-router";
import { ArrowUpCircle, Plus, X } from "lucide-react";
import { toast } from "sonner";
import { errorMessage, knownNodes, nodeClient, projectClient, toDate } from "@/lib/api";
import { capitalize, cn, formatDateTime, formatTime, timeAgo } from "@/lib/utils";
import { locale, t } from "@/i18n";
import { checkName, envToText, lines, textToEnv } from "@/lib/config";
import { useRun } from "@/components/run";
import { ImportDir } from "@/pages/import-dir";
import { ContainerMetrics, containerUsage, EmptyProjectMetrics, ProjectMetrics, projectMetricNodes, UsageRings, useProjectMetrics, useRange } from "@/components/metrics";
import { offlineNodeNames, ProjectState, serviceDetail, ServiceState, stateOf } from "@/components/status";
import { ask, confirmAction } from "@/components/confirm";
import { Checkbox } from "@/components/ui/checkbox";
import { ContainerAction } from "@/gen/nagi/v1/agent_pb";
import { TerminalView } from "@/components/terminal";
import { composeInputClass, formToCompose, useImageForm, useContainerForm } from "@/components/container-form";
import { useNodeChoice } from "@/pages/projects";
import { Breadcrumb, MobileBar, PageHeader } from "@/pages/layout";
import { ContainerHA, ContainerStorage } from "@/pages/project-data";
import { ContainerNetwork } from "@/pages/project-network";
import {
  Alert,
  Badge,
  badgeVariants,
  Button,
  Check,
  CopyButton,
  Dialog,
  DialogActions,
  ButtonLink,
  EmptyRow,
  Field,
  FormActions,
  FormRow,
  HelpTip,
  Input,
  KeyValue,
  List,
  Menu,
  Notice,
  QueryError,
  ReadOnlyContext,
  Row,
  RowsSkeleton,
  Section,
  Roll,
  Segmented,
  Slide,
  Select,
  Skeleton,
  Spinner,
  Status,
  Tabs,
  Textarea,
} from "@/components/ui";
import { create } from "@bufbuild/protobuf";
import { AlertRulesSchema, type AlertRules, type Container, type Project, type ServiceStatus } from "@/gen/nagi/v1/common_pb";
import { AlertRulesFields, AlertRulesForm, containerRules } from "@/components/alert-rules";
import { useUnsaved } from "@/components/unsaved";
import { EnvEditor, type EnvMode } from "@/components/env-editor";
import { ConfigDiff } from "@/components/config-diff";
import { useRole } from "@/components/role";
import { actionLabel } from "@/pages/activity";

// 项目「概览」：容器列表、批量部署、添加容器、项目的监控；以及容器页（概览、配置、版本、日志、终端、数据）。
// 宽屏时容器列在项目栏里（含状态、CPU、内存），概览里不再重复这张表，只保留添加和部署的按钮。

type ImageUpdate = { container: string; service: string; image: string };
export const containerTabs = ["overview", "logs", "terminal", "config", "versions", "network", "storage", "ha", "alerts"] as const;
export type DetailView = (typeof containerTabs)[number];

// useProjectActions：项目概览页头的操作：更新有新版本的容器、部署全部容器、⋯。部署的输出显示在概览里。
export function useProjectActions(project: Project, imageUpdates: ImageUpdate[]) {
  const navigate = useNavigate();
  const { isViewer } = useRole();
  const { run, busy, output, hasOutput } = useRun(project.id);
  const pc = t.projectContainers;
  const updateNames = [...new Set(imageUpdates.map((u) => u.container))];
  const changed = project.containers.filter((m) => m.hasDraft || m.sharedEnvOutdated || m.deployedVersion === 0n);

  const updateImages = async () => {
    const label = pc.imageUpdates.action(updateNames.length);
    if (!(await confirmAction(pc.imageUpdates.confirm(t.common.list(updateNames)), { action: label }))) return;
    run(() =>
      projectClient.deploy({
        projectId: project.id,
        pullImages: true,
        targets: project.containers.filter((c) => updateNames.includes(c.name)).map((c) => ({ container: c.name, baseRevision: c.revision })),
      }),
    );
  };
  const deploy = async (containers: Container[]) => {
    const running = containers.filter((m) => m.deployedVersion > 0n).map((m) => m.name);
    if (running.length > 0 && !(await confirmAction(pc.table.deployConfirm(containers.length, t.common.list(running)), { action: pc.detail.deploy }))) return;
    run(() =>
      projectClient.deploy({
        projectId: project.id,
        targets: containers.map((m) => ({ container: m.name, baseRevision: m.revision })),
      }),
    );
  };

  const actions =
    project.containers.length === 0 ? undefined : (
      <>
        {updateNames.length > 0 && !isViewer && (
          <Button variant="outline" disabled={busy} onClick={updateImages} title={pc.imageUpdates.hover(t.common.list([...new Set(imageUpdates.map((u) => u.image))]))}>
            {busy ? <Spinner /> : <ArrowUpCircle className="size-3.5! text-info" />}
            {pc.imageUpdates.action(updateNames.length)}
          </Button>
        )}
        {/* 有变更时主按钮只部署有变更的容器，部署全部收进 ⋯。 */}
        {changed.length > 0 ? (
          <Button disabled={busy} onClick={() => deploy(changed)} title={pc.table.changedTitle(t.common.list(changed.map((m) => m.name)))}>
            {busy && <Spinner />}
            {pc.table.deployChanged(changed.length)}
          </Button>
        ) : (
          <Button variant="outline" disabled={busy} onClick={() => deploy(project.containers)}>
            {busy && <Spinner />}
            {pc.table.deployAll}
          </Button>
        )}
        <Menu
          items={[
            ...(changed.length > 0 ? [{ label: pc.table.deployAll, disabled: busy, onSelect: () => deploy(project.containers) }] : []),
            { label: pc.table.addContainer, onSelect: () => navigate({ to: "/projects/$projectId/add", params: { projectId: project.id } }) },
            { label: t.project.sections.settings, onSelect: () => navigate({ to: "/projects/$projectId", params: { projectId: project.id }, search: { tab: "settings" } }) },
          ]}
        />
      </>
    );
  return { actions, deploy, busy, output, hasOutput };
}

export function ContainersTab({
  project,
  services,
  deploy,
  busy,
  output,
  hasOutput,
}: {
  project: Project;
  services: ServiceStatus[];
  deploy: (containers: Container[]) => void;
  busy: boolean;
  output: React.ReactNode;
  hasOutput: boolean;
}) {
  const { range, picker } = useRange();
  const nodes = useQuery({ queryKey: ["nodes"], queryFn: () => nodeClient.listNodes({}) });

  // 还没有容器：监控照常排版但没有数据，模糊处理，上面写「暂无容器」。模糊加在内容上，边缘自然淡出，不会有截止线。
  if (project.containers.length === 0) {
    return (
      <div className="relative">
        <div inert className="pointer-events-none opacity-60 blur-[4px] select-none">
          <Section title={t.projectContainers.monitoring} actions={picker}>
            <EmptyProjectMetrics range={range} />
          </Section>
        </div>
        <div className="absolute inset-0 flex flex-col items-center justify-center gap-4 text-center">
          <h2 className="text-base font-semibold">{t.projectContainers.add.noContainers}</h2>
          <ButtonLink to="/projects/$projectId/add" params={{ projectId: project.id }}>
            <Plus />
            {t.projectContainers.table.addContainer}
          </ButtonLink>
        </div>
      </div>
    );
  }

  return (
    <div>
      {hasOutput && <div className="mb-8">{output}</div>}
      <ContainerList project={project} services={services} deploy={deploy} busy={busy} />
      <Section className="xl:mt-0 xl:border-t-0 xl:pt-0" title={t.projectContainers.monitoring} actions={picker}>
        <ProjectMetrics projectId={project.id} nodeIds={[...new Set(projectMetricNodes(project))]} range={range} offline={offlineNodeNames(project.containers, knownNodes(nodes.data))} httpRoutes={project.routes.filter((r) => r.protocol === "http").map((r) => r.name)} />
      </Section>
    </div>
  );
}

// shortImage 把固定了摘要的镜像（回滚、迁移、顶替后会固定到当时的摘要）缩短显示，例如 redis@sha256:858f009f9709。
function shortImage(image: string) {
  const i = image.indexOf("sha256:");
  return i < 0 ? image : image.slice(0, i + 7 + 12);
}

// displayImage 是概览里显示的镜像：有标签时去掉摘要（标签已经说明了版本，完整的放在悬停提示里），
// 没有标签时保留 12 位摘要；省掉 Docker Hub 的默认前缀。
function displayImage(image: string) {
  let s = image.replace(/^docker\.io\/(library\/)?/, "");
  const at = s.indexOf("@sha256:");
  if (at >= 0) s = /:[^/]+$/.test(s.slice(0, at)) ? s.slice(0, at) : shortImage(s);
  return s;
}

// ContainerNotes：容器概览上方的提示。一条时直接显示；多条时合并成一条，标题写条数和各条的标题，点「查看」再逐条列出。
function ContainerNotes({ notes }: { notes: { key: string; title: string; body: string }[] }) {
  const [open, setOpen] = useState(false);
  const f = t.common.feedback;
  if (notes.length === 1)
    return (
      <Notice tone="warning" title={notes[0].title} className="mb-6">
        {notes[0].body}
      </Notice>
    );
  return (
    <div className="mb-6 grid gap-2">
      <Notice
        tone="warning"
        title={f.notes(notes.length)}
        actions={
          <Button variant="outline" readOnlyOk aria-expanded={open} onClick={() => setOpen(!open)}>
            {open ? f.hideNotes : f.showNotes}
          </Button>
        }
      >
        {t.common.list(notes.map((n) => n.title))}
      </Notice>
      {open &&
        notes.map((n) => (
          <Notice key={n.key} tone="warning" title={n.title} className="pl-4">
            {n.body}
          </Notice>
        ))}
    </div>
  );
}

// containerKind 返回"容器"（单个容器，镜像表单）或"容器组"（一份 compose，里面有几个容器）。
// containerLabel：界面上显示的名称。设了显示名称时用它，否则用容器名（容器名创建后不能改）。
export function containerLabel(m: Container) {
  return m.displayName || m.name;
}

export function containerKind(m: Container) {
  return m.kind === "group" ? t.projectContainers.kind.group : t.projectContainers.kind.container;
}

// ---- 窄屏的容器列表 ----

// ContainerList：窄屏没有项目栏，容器列在概览里，徽标写进第二行灰字；勾选多个可批量部署。宽屏整块隐藏。
function ContainerList({ project, services, deploy, busy }: { project: Project; services: ServiceStatus[]; deploy: (containers: Container[]) => void; busy: boolean }) {
  const pc = t.projectContainers;
  const nodes = useQuery({ queryKey: ["nodes"], queryFn: () => nodeClient.listNodes({}) });
  const nodeOf = (id: string) => knownNodes(nodes.data).find((n) => n.id === id);
  const [checked, setChecked] = useState<Record<string, boolean>>({});
  const targets = project.containers.filter((m) => checked[m.name]);
  const usage = containerUsage(useProjectMetrics(project.id, projectMetricNodes(project)).data);
  const memFallback = Math.max(0, ...[...usage.values()].map((u) => u.mem));

  return (
    <Section
      className="xl:hidden"
      title={pc.table.title}
      help={pc.table.help}
      actions={
        project.containers.length > 1 &&
        targets.length > 0 && (
          <Button disabled={busy} onClick={() => deploy(targets)}>
            {busy && <Spinner />}
            {pc.table.deploySelected(targets.length)}
          </Button>
        )
      }
    >
      <List>
        {project.containers.map((m) => {
          const node = nodeOf(m.nodeId);
          const offline = node?.online === false;
          const svc = services.filter((s) => s.container === m.name);
          const version = m.deployedVersion > 0n ? `v${m.deployedVersion}${m.latestVersion !== m.deployedVersion ? pc.table.latest(String(m.latestVersion)) : ""}` : "";
          const address = m.kind !== "group" ? m.name : [...new Set(svc.filter((s) => !m.hostNetworkServices.includes(s.name)).map((s) => `${s.name}.${m.name}`))].join(" ");
          const notes: React.ReactNode[] = [
            !offline && (node?.name ?? m.nodeId),
            offline ? <span className="text-warning-text">{t.common.status.nodesOffline([node.name])}</span> : stateOf(svc, m.deployedVersion > 0n).label,
            offline && t.common.status.unknown,
            !offline && version,
            m.kind === "group" && pc.kind.group,
            m.hasDraft && pc.badges.draft,
            m.sharedEnvOutdated && pc.badges.sharedEnvPending,
            m.stateful && pc.badges.stateful,
            m.hostNetworkServices.length > 0 && <span className="text-warning-text">{pc.badges.hostNetwork}</span>,
            m.unmanagedVolumes.length > 0 && pc.badges.volumeUnmanaged,
          ].filter(Boolean);
          return (
            <Row
              key={m.name}
              className="relative"
              // 只有一个容器时没有批量部署，不显示勾选框。
              icon={
                project.containers.length > 1 && (
                  <span className="relative z-10 flex">
                    <Checkbox aria-label={pc.table.select(m.name)} checked={!!checked[m.name]} onCheckedChange={(v) => setChecked({ ...checked, [m.name]: v === true })} />
                  </span>
                )
              }
              title={
                // 真正的链接：键盘能进入，也能 Ctrl+点击在新标签打开；after 让整行可点。
                <Link to="/projects/$projectId/c/$container" params={{ projectId: project.id, container: m.name }} className="rounded-sm outline-none after:absolute after:inset-0 focus-ring">
                  {containerLabel(m)}
                </Link>
              }
              sub={
                <>
                  {notes.map((n, i) => (
                    <span key={i}>
                      {i > 0 && " · "}
                      {n}
                    </span>
                  ))}
                  {address && <span className="max-md:hidden"> · <span className="font-mono">{address}</span></span>}
                </>
              }
              actionsWidth="w-auto max-sm:hidden"
              actions={<UsageRings usage={offline ? undefined : usage.get(m.name)} memFallback={memFallback} />}
            />
          );
        })}
      </List>
      <div>
        <ButtonLink to="/projects/$projectId/add" params={{ projectId: project.id }} variant="outline">
          <Plus />
          {pc.table.addContainer}
        </ButtonLink>
      </div>
    </Section>
  );
}

// ---- 一个容器 ----

// ContainerPage：容器自己的页（项目 / 容器），标签 概览、配置、版本、日志、终端、数据；当前标签记在地址的 view 参数里。
// 宽屏时左边有项目栏（见 ProjectSplit），不再显示上级链接。
export function ContainerPage({ projectId, name, view, onView, autoDeploy, onAutoDeployed }: { projectId: string; name: string; view: DetailView; onView: (v: DetailView) => void; autoDeploy?: boolean; onAutoDeployed?: () => void }) {
  const project = useQuery({
    queryKey: ["project", projectId],
    queryFn: () => projectClient.getProject({ projectId }),
    refetchInterval: 5000,
  });
  if (project.isPending) return <ContainerSkeleton />;
  if (project.error) return <QueryError error={errorMessage(project.error)} onRetry={() => project.refetch()} />;
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
        <Breadcrumb items={crumbs} className="max-md:hidden xl:hidden" />
        <Notice
          tone="warning"
          actions={
            <ButtonLink to="/projects/$projectId" params={{ projectId: p.id }} variant="outline" readOnlyOk>
              {t.projectContainers.page.backToProject}
            </ButtonLink>
          }
        >
          {t.projectContainers.page.notFound(name)}
        </Notice>
      </>
    );
  }
  const services = (project.data.status?.services ?? []).filter((s) => s.container === name);
  const imageUpdates = (project.data.status?.imageUpdates ?? []).filter((u) => u.container === name);
  return (
    <>
      <Breadcrumb items={[...crumbs, { label: containerLabel(container) }]} className="max-md:hidden xl:hidden" />
      <ContainerDetail project={p} container={container} services={services} imageUpdates={imageUpdates} view={view} onView={onView} autoDeploy={autoDeploy} onAutoDeployed={onAutoDeployed} />
    </>
  );
}

// ContainerSkeleton：容器页加载中，和页头、标签一样的版面。
function ContainerSkeleton() {
  return (
    <div role="status" aria-label={t.common.feedback.loading}>
      <div className="mb-4 flex min-h-8 items-center justify-between gap-4">
        <Skeleton className="h-6 w-40" />
        <div className="flex gap-2">
          <Skeleton className="h-8 w-16" />
          <Skeleton className="h-8 w-20" />
        </div>
      </div>
      <Skeleton className="mb-6 h-10 w-full max-w-md" />
      <RowsSkeleton />
    </div>
  );
}

// imageTipKey：关掉提示后按「容器 + 镜像」记在浏览器里，同一版本不再自动弹。
const imageTipKey = (project: string, container: string, images: string) => `nagi:image-tip:${project}/${container}:${images}`;

function tipClosed(key: string) {
  try {
    return localStorage.getItem(key) === "1";
  } catch {
    return false;
  }
}

// useImageTip：提示是否显示。关掉后记在浏览器里；点标题旁的「可更新」重新打开。
function useImageTip(key: string) {
  const [state, setState] = useState<Record<string, boolean>>({});
  const open = state[key] ?? !tipClosed(key);
  const close = () => {
    setState((s) => ({ ...s, [key]: false }));
    try {
      localStorage.setItem(key, "1");
    } catch {
      // 浏览器不让存时，这次关掉就行。
    }
  };
  return { open, close, reopen: () => setState((s) => ({ ...s, [key]: true })) };
}

// ImageTip：进入可更新的容器时右下角的提示。不自动消失，不抢焦点（规则见 docs/ui.md「镜像更新提示」）。
// 点「更新容器」后先确认，确认了才关掉；取消时提示保留。
function ImageTip({ images, disabled, onClose, onUpdate }: { images: string; disabled: boolean; onClose: () => void; onUpdate: () => void }) {
  const close = onClose;
  const tip = t.projectContainers.imageTip;
  return (
    <aside role="status" aria-label={tip.label} className="fixed right-6 bottom-6 z-20 grid w-[340px] max-w-[calc(100vw-32px)] gap-3 rounded-lg bg-popover p-4 shadow-popover max-md:right-4 max-md:bottom-4 max-md:left-4 max-md:w-auto">
      <div className="flex items-center justify-between gap-2">
        <span className="font-medium">{tip.title}</span>
        <Button variant="ghost" size="icon" className="-my-2 -mr-2 text-muted-foreground" aria-label={t.common.actions.close} onClick={close}>
          <X />
        </Button>
      </div>
      <p className="text-muted-foreground">
        <span className="font-mono text-[13px]">{images}</span> {tip.body}
      </p>
      <div className="flex justify-end gap-2">
        <Button variant="ghost" onClick={close}>
          {tip.later}
        </Button>
        <Button disabled={disabled} onClick={onUpdate}>
          {tip.update}
        </Button>
      </div>
    </aside>
  );
}

function ContainerDetail({
  project,
  container,
  services,
  imageUpdates,
  view: viewProp,
  onView,
  autoDeploy,
  onAutoDeployed,
}: {
  project: Project;
  container: Container;
  services: ServiceStatus[];
  imageUpdates: { image: string }[];
  view: DetailView;
  onView: (v: DetailView) => void;
  autoDeploy?: boolean;
  onAutoDeployed?: () => void;
}) {
  const { run, busy, output, hasOutput, setLines } = useRun(project.id);
  const queryClient = useQueryClient();
  const { isViewer } = useRole();
  const readOnly = useContext(ReadOnlyContext);
  const hasImageUpdate = imageUpdates.length > 0;
  const updateImages = t.common.list([...new Set(imageUpdates.map((u) => shortImage(u.image)))]);
  const tip = useImageTip(imageTipKey(project.id, container.name, updateImages));
  // 正在做的事：部署类（部署、上传、拉取、更新镜像）、回滚，或启停重启。按钮上的文字和转圈按它显示。
  const [doing, setDoing] = useState<"deploy" | "rollback" | "control">("deploy");
  // 只读角色没有配置和终端，直接打开这些地址时显示概览。
  const view = isViewer && (viewProp === "config" || viewProp === "terminal") ? "overview" : viewProp;
  const fileRef = useRef<HTMLInputElement>(null);
  const [renaming, setRenaming] = useState(false);
  // 换了容器时，上一个容器的部署输出不再显示。
  useEffect(() => {
    if (!busy) setLines([]);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [container.name]);
  const cfg = container.draft ?? container.config;
  const target = [{ container: container.name, baseRevision: container.revision }];
  const isGroup = container.kind === "group";
  const serviceNames = [...new Set(services.map((s) => s.name))];

  // 存储和高可用标签有自己的操作输出，不显示这里的；开始部署时回到概览，否则看不到进度和结果。
  const showOutput = () => {
    if (view === "storage" || view === "ha") onView("overview");
  };
  const deploy = (opts: { pull?: boolean; useUpload?: boolean; pullImages?: boolean }, keep = false) => {
    showOutput();
    setDoing("deploy");
    return run(() => projectClient.deploy({ projectId: project.id, targets: target, ...opts }), keep);
  };

  // 已部署的容器重新部署会重建容器、中断服务并生成新版本，先确认。返回是否开始了。
  const confirmDeploy = async (message: string, action: string, opts: { pull?: boolean; pullImages?: boolean }) => {
    if (container.deployedVersion > 0n && !(await confirmAction(message, { action }))) return false;
    deploy(opts);
    return true;
  };
  // 添加容器时选了「添加并部署」：进入容器页后开始部署，输出显示在概览里。
  useEffect(() => {
    if (!autoDeploy) return;
    onAutoDeployed?.();
    deploy({});
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [autoDeploy]);
  const d = t.projectContainers.detail;
  const updateImage = () =>
    confirmDeploy((container.hasDraft ? d.updateImageDraftConfirm : d.updateImageConfirm)(container.name), d.updateImage, { pullImages: true });

  const running = services.some((s) => s.state === "running" || s.state === "paused" || s.state === "restarting");
  const stopped = services.some((s) => s.state !== "running" && s.state !== "paused" && s.state !== "restarting");
  const deployed = container.deployedVersion > 0n;
  const control = (action: ContainerAction) => {
    showOutput();
    setDoing("control");
    return run(() => projectClient.controlContainer({ projectId: project.id, container: container.name, action }));
  };

  // 上传压缩包：浏览器 → 面板 → mesh → 节点暂存，然后部署时替换容器的 data/。
  const upload = async (file: File) => {
    onView("overview");
    setLines([t.projectContainers.detail.uploading(file.name, (file.size / 1048576).toFixed(1))]);
    const q = new URLSearchParams({ project: project.id, container: container.name });
    const fail = (msg: string) => setLines((l) => [...l, t.projectContainers.detail.uploadFailed(msg)]);
    let res: Response;
    try {
      res = await fetch(`/api/upload?${q}`, { method: "POST", headers: { "X-Nagi-Upload": "1", "Accept-Language": locale }, body: file });
    } catch {
      fail(t.common.errors.unreachable);
      return;
    }
    // 反向代理拒绝时（例如文件太大返回 413）不是 JSON。
    const httpError = t.common.errors.http(String(res.status));
    const body: { message?: string; files: number; stripped: string } = await res.json().catch(() => ({ message: httpError, files: 0, stripped: "" }));
    if (!res.ok) {
      fail(body.message || httpError);
      return;
    }
    setLines((l) => [...l, t.projectContainers.detail.uploaded(body.files, body.stripped)]);
    await deploy({ useUpload: true }, true);
  };

  const remove = async (): Promise<boolean> => {
    const answer = await ask(d.deleteConfirm(isGroup, container.name), {
      action: d.deleteKind(isGroup),
      danger: true,
      check: d.deleteDirCheck(isGroup),
      typeToConfirm: { label: d.deleteTypeName(isGroup, container.name), value: container.name },
    });
    if (!answer) return false;
    try {
      await projectClient.deleteContainer({ projectId: project.id, container: container.name, baseRevision: container.revision, deleteDir: answer.checked });
      queryClient.invalidateQueries({ queryKey: ["project", project.id] });
      return true;
    } catch (err) {
      // 删除在页头菜单里，任何标签都能点；存储和高可用标签不显示输出，错误用 Toast 显示。
      toast.error(errorMessage(err));
      return false;
    }
  };

  const nodes = useQuery({ queryKey: ["nodes"], queryFn: () => nodeClient.listNodes({}) });
  const node = knownNodes(nodes.data).find((n) => n.id === container.nodeId);
  const nodeOnline = node?.online !== false;
  const navigate = useNavigate();
  // 手机上打开卷的文件页时，标题栏换成卷名，不再显示容器标题和标签。
  const { volume } = useSearch({ strict: false }) as { volume?: string };
  const browsing = view === "storage" && !!volume && !isViewer;
  // 现有目录的容器文件在节点上的那个目录里，不经面板上传。
  const uploadDeployable = !cfg?.projectDir && (!cfg?.form || !!cfg.form.workdir);
  const pullDeploy = () => confirmDeploy(d.pullDeployConfirm(container.name), d.pullDeploy, { pull: true });
  const updateFromTip = async () => {
    if (await updateImage()) tip.close();
  };
  const deployBusy = busy && doing !== "control";

  return (
    <>
      {hasImageUpdate && !isViewer && !readOnly && tip.open && (
        <div className={cn(browsing && "max-md:hidden")}>
          <ImageTip images={updateImages} disabled={busy || !nodeOnline} onClose={tip.close} onUpdate={updateFromTip} />
        </div>
      )}
      <PageHeader
        className={cn("mb-4", browsing && "max-md:hidden")}
        actionsClassName="max-md:w-full max-md:flex-nowrap max-md:[&>button:not(:last-child)]:flex-1"
        title={
          <>
            {containerLabel(container)}
            {container.displayName && <span className="font-mono text-sm font-normal text-muted-foreground">{container.name}</span>}
            {/* 节点离线时容器的状态是未知，不是已停止。 */}
            {node && !nodeOnline ? <Status tone="warning">{t.common.status.nodesOffline([node.name])}</Status> : <ProjectState services={services} deployed={container.deployedVersion > 0n} />}
            {container.stateful && <Badge>{t.projectContainers.badges.stateful}</Badge>}
            {container.hasDraft && <Badge tone="warning">{t.projectContainers.badges.draft}</Badge>}
            {hasImageUpdate &&
              (isViewer || readOnly ? (
                <Badge tone="info">{d.updatable}</Badge>
              ) : (
                // 弹窗关掉以后，点这里重新打开。
                <button type="button" onClick={tip.reopen} className={cn(badgeVariants({ tone: "info" }), "cursor-pointer font-normal tracking-normal outline-none focus-ring")}>
                  {d.updatable}
                </button>
              ))}
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
            {/* 页头只放重启（或启动）、重新部署和更多；上传、拉取并部署在更多里。 */}
            {deployed && (running || !nodeOnline ? (
              <Button
                variant="outline"
                size="sm"
                disabled={busy || !nodeOnline}
                onClick={async () => (await confirmAction(d.restartConfirm(isGroup, container.name), { action: d.restart })) && control(ContainerAction.RESTART)}
              >
                {busy && doing === "control" && <Spinner />}
                {d.restart}
              </Button>
            ) : (
              <Button variant="outline" size="sm" disabled={busy || !nodeOnline} onClick={() => control(ContainerAction.START)}>
                {busy && doing === "control" && <Spinner />}
                {d.start}
              </Button>
            ))}
            <Button
              size="sm"
              // 只有要部署新内容（草稿、第一次部署）或正在部署时是主按钮。
              variant={container.hasDraft || !deployed || deployBusy ? "default" : "outline"}
              onClick={() =>
                container.hasDraft
                  ? confirmDeploy(d.deployDraftConfirm(container.name), d.deployDraft, {})
                  : confirmDeploy(d.redeployConfirm(container.name), d.redeploy, {})
              }
              disabled={busy || !nodeOnline}
            >
              {deployBusy && <Spinner />}
              {deployBusy
                ? doing === "rollback"
                  ? d.rollingBack
                  : d.deploying
                : container.hasDraft
                  ? t.projectContainers.detail.deployDraft
                  : container.deployedVersion > 0n
                    ? t.projectContainers.detail.redeploy
                    : t.projectContainers.detail.deploy}
            </Button>
            <Menu
              title={container.name}
              items={[
                ...(uploadDeployable ? [{ label: t.projectContainers.detail.uploadDeploy, onSelect: () => fileRef.current?.click(), disabled: busy || !nodeOnline }] : []),
                ...(cfg?.source?.gitUrl ? [{ label: t.projectContainers.detail.pullDeploy, onSelect: pullDeploy, disabled: busy || !nodeOnline }] : []),
                { label: t.projectContainers.rename.action, onSelect: () => setRenaming(true) },
                { label: t.projectContainers.detail.updateImage, onSelect: updateImage, disabled: busy || !nodeOnline },
                // 容器组部分运行时，上面的按钮是重启，从这里启动其余的容器。
                ...(deployed && running && stopped ? [{ label: d.start, onSelect: () => control(ContainerAction.START), disabled: busy || !nodeOnline }] : []),
                // 设置了顶替的容器停止后会被代管重启或另一个节点拉起，须先取消顶替（见 docs/failover.md）。
                ...(deployed && running
                  ? [
                      {
                        label: d.stop,
                        hint: container.failover ? d.stopFailoverHint : undefined,
                        disabled: busy || !nodeOnline || !!container.failover,
                        onSelect: async () => {
                          if (await confirmAction(d.stopConfirm(isGroup, container.name), { action: d.stop, danger: true })) control(ContainerAction.STOP);
                        },
                      },
                    ]
                  : []),
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
      <Dialog open={renaming} onClose={() => setRenaming(false)} title={t.projectContainers.rename.title(isGroup)} description={t.projectContainers.rename.description}>
        <RenameContainer project={project} container={container} onDone={() => setRenaming(false)} />
      </Dialog>
      <Tabs
        className={cn("mb-6 max-md:mb-4", browsing && "max-md:hidden")}
        value={view}
        onChange={onView}
        items={[
          { value: "overview", label: t.projectContainers.tabs.overview },
          { value: "logs", label: t.projectContainers.tabs.logs },
          ...(isViewer ? [] : [{ value: "terminal" as const, label: t.projectContainers.tabs.terminal }]),
          ...(isViewer ? [] : [{ value: "config" as const, label: t.projectContainers.tabs.config }]),
          { value: "versions", label: t.projectContainers.tabs.versions },
          { value: "network", label: t.projectContainers.tabs.network },
          { value: "storage", label: t.projectContainers.tabs.storage },
          { value: "ha", label: t.projectContainers.tabs.ha },
          { value: "alerts", label: t.projectContainers.tabs.alerts },
        ]}
      />
      <Slide id={view} index={containerTabs.indexOf(view)}>
        {view !== "storage" && view !== "ha" && hasOutput && <div className="mb-6">{output}</div>}
        {view === "overview" && <Overview project={project} container={container} services={services} />}
        {view === "config" && !isViewer && <ContainerConfig
            key={`cfg-${container.revision}`}
            project={project}
            container={container}
            services={serviceNames}
            deploying={busy}
            onDeploy={(baseRevision) => {
              setDoing("deploy");
              run(() => projectClient.deploy({ projectId: project.id, targets: [{ container: container.name, baseRevision }] }));
            }}
          />}
        {view === "versions" && (
          <Versions
            container={container}
            projectId={project.id}
            busy={busy}
            deployDisabled={busy || !nodeOnline}
            onDeploy={() => confirmDeploy(d.redeployConfirm(container.name), d.deploy, {})}
            onRollback={(version, restoreData) => {
              setDoing("rollback");
              run(() => projectClient.rollback({ projectId: project.id, container: container.name, version, restoreData }));
            }}
          />
        )}
        {view === "logs" && (
          <Logs
            projectId={project.id}
            container={container.name}
            services={serviceNames}
            containers={services.map((s) => s.containerId).filter(Boolean).join(",")}
          />
        )}
        {view === "terminal" && !isViewer && (
          <TerminalView projectId={project.id} container={container.name} services={services.filter((s) => s.state === "running").map((s) => s.name)} />
        )}
        {view === "network" && <ContainerNetwork project={project} container={container} services={services} />}
        {view === "storage" && <ContainerStorage project={project} container={container} />}
        {view === "ha" && <ContainerHA project={project} container={container} />}
        {view === "alerts" && <ContainerAlerts key={`alerts-${container.revision}`} project={project} container={container} />}
      </Slide>
    </>
  );
}

// ContainerAlerts：容器的告警规则。默认用项目的；单独设置时整组替换项目的，起点是项目现在的规则。
function ContainerAlerts({ project, container }: { project: Project; container: Container }) {
  const queryClient = useQueryClient();
  const { isViewer } = useRole();
  const projectRules = project.settings?.alertRules;
  const [own, setOwn] = useState(!!container.alertRules);
  const save = async (rules: AlertRules | undefined) => {
    await projectClient.setContainerAlertRules({ projectId: project.id, container: container.name, rules });
    toast.success(t.alerts.savedAndPushed);
    await queryClient.invalidateQueries({ queryKey: ["project", project.id] });
  };
  const toggle = async (v: boolean) => {
    setOwn(v);
    // 改回用项目的规则时立即保存：没有别的要填。
    if (!v && container.alertRules) {
      try {
        await save(undefined);
      } catch (err) {
        setOwn(true);
        toast.error(errorMessage(err));
      }
    }
  };
  return (
    <Section title={t.alerts.rules} help={t.alerts.containerRulesHelp}>
      <div className="grid gap-6">
        <Check checked={own} disabled={isViewer} onChange={toggle} help={t.alerts.containerOwnHelp}>
          {t.alerts.containerOwn}
        </Check>
        {own ? (
          <AlertRulesForm id={`container-alerts-${container.name}`} defs={containerRules} initial={container.alertRules ?? projectRules} disabled={isViewer} onSave={save} />
        ) : (
          <AlertRulesFields id={`container-alerts-${container.name}`} defs={containerRules} value={projectRules ?? create(AlertRulesSchema)} onChange={() => {}} disabled />
        )}
      </div>
    </Section>
  );
}

// uptime：把 Docker 的「Up 2 hours」换成界面语言。不是运行中时是「—」。
// RenameContainer：修改容器的显示名称（放在对话框里）。容器名不变，项目内网仍用它访问。
function RenameContainer({ project, container, onDone }: { project: Project; container: Container; onDone: () => void }) {
  const queryClient = useQueryClient();
  const [name, setName] = useState(containerLabel(container));
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const r = t.projectContainers.rename;
  const save = async () => {
    const trimmed = name.trim();
    if (trimmed === containerLabel(container)) return onDone();
    setBusy(true);
    setError("");
    try {
      await projectClient.setContainerDisplayName({ projectId: project.id, container: container.name, displayName: trimmed });
      await queryClient.invalidateQueries({ queryKey: ["project", project.id] });
      onDone();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };
  return (
    <form
      className="grid gap-4"
      onSubmit={(e) => {
        e.preventDefault();
        save();
      }}
    >
      <Field label={r.label} htmlFor="container-display-name" hint={r.hint(container.name)}>
        <Input id="container-display-name" value={name} onChange={(e) => setName(e.target.value)} maxLength={32} placeholder={container.name} autoFocus />
      </Field>
      {error && <Alert>{error}</Alert>}
      <DialogActions>
        <Button variant="outline" readOnlyOk onClick={onDone}>
          {t.common.actions.cancel}
        </Button>
        <Button type="submit" disabled={busy}>
          {busy && <Spinner />}
          {t.common.actions.save}
        </Button>
      </DialogActions>
    </form>
  );
}

function uptime(services: ServiceStatus[]) {
  const up = t.projectContainers.overview.uptime;
  const m = /^Up\s+(.+)$/.exec(serviceDetail(services.find((s) => s.state === "running")?.status ?? ""));
  if (!m) return "—";
  const text = m[1].trim();
  if (/^about an hour/i.test(text)) return up.hour(1);
  if (/^about a minute/i.test(text)) return up.minute(1);
  if (/^less than a second/i.test(text)) return up.second(1);
  const n = /^(\d+)\s+(second|minute|hour|day|week|month|year)s?/i.exec(text);
  // 认不出的格式不直接显示 Docker 的英文原文。
  if (!n) return "—";
  return up[n[2].toLowerCase() as keyof typeof up](Number(n[1]));
}

function Overview({ project, container, services }: { project: Project; container: Container; services: ServiceStatus[] }) {
  const cfg = container.draft ?? container.config;
  const pc = t.projectContainers;
  const o = pc.overview;
  const { range, picker } = useRange();
  const nodes = useQuery({ queryKey: ["nodes"], queryFn: () => nodeClient.listNodes({}) });
  const node = knownNodes(nodes.data).find((n) => n.id === container.nodeId);
  const offline = !!node && !node.online;
  const isGroup = container.kind === "group";
  const images = [...new Set(services.map((s) => shortImage(s.image)).filter(Boolean))];
  const image = container.config?.form?.image || cfg?.form?.image || t.common.list(images) || "—";
  // 容器组每个服务一行：服务名和它正在用的镜像，按服务名排序。
  const serviceImages = [...new Map(services.filter((s) => s.image).map((s) => [s.name, s.image] as const)).entries()].sort(([a], [b]) => a.localeCompare(b));
  const version =
    container.deployedVersion > 0n ? `v${container.deployedVersion}${container.latestVersion !== container.deployedVersion ? pc.table.latest(String(container.latestVersion)) : ""}` : "—";
  // 单个健康的容器，页头的状态已经说明了一切；多个服务或有问题时才列出每个服务。
  const showServices = services.length !== 1 || services[0].state !== "running" || services[0].health === "unhealthy" || services[0].health === "starting" || offline;
  // 页面上方需要注意的事。同一处只放一条：多条时合并成「需要注意 N 项」，点开再逐条列出。
  const notes = [
    offline && { key: "offline", title: t.common.status.nodesOffline([node.name]), body: o.nodeOfflineBody },
    container.hostNetworkServices.length > 0 && { key: "host", title: o.hostNetworkTitle, body: o.hostNetworkBody(t.common.list(container.hostNetworkServices)) },
    container.unmanagedVolumes.length > 0 && { key: "volumes", title: pc.badges.volumeUnmanaged, body: o.volumeBody(t.common.list(container.unmanagedVolumes)) },
    container.sharedEnvOutdated && { key: "env", title: pc.badges.sharedEnvPending, body: pc.detail.sharedEnvOutdated },
  ].filter((x): x is { key: string; title: string; body: string } => !!x);
  const hasNotes = notes.length > 0;
  const mono = "font-mono text-[13px] wrap-anywhere";
  const webhookUrl = `${location.origin}/hooks/${container.webhookToken}`;
  return (
    <div>
      {hasNotes && <ContainerNotes notes={notes} />}
      <Section>
        <div className="grid grid-cols-[repeat(auto-fit,minmax(160px,1fr))] gap-x-6 gap-y-4 max-md:grid-cols-2">
          {isGroup && <KvItem k={pc.columns.kind} v={pc.kind.groupCompose} />}
          {!isGroup && <KvItem mono k={pc.columns.image} v={<span title={image}>{displayImage(image)}</span>} />}
          <KvItem mono k={pc.columns.node} v={node?.name ?? container.nodeId} />
          <KvItem mono k={o.currentVersion} v={version} />
          <KvItem k={o.uptimeLabel} v={<Roll value={uptime(services)} />} />
          {isGroup && (
            <KvItem
              className="col-span-full"
              k={pc.columns.image}
              v={
                serviceImages.length === 0 ? (
                  "—"
                ) : (
                  <span className="grid gap-1">
                    {serviceImages.map(([svc, img]) => (
                      <span key={svc} className="flex min-w-0 gap-3" title={img}>
                        <span className="w-40 flex-none truncate text-muted-foreground max-md:w-28">{svc}</span>
                        <span className="min-w-0 font-mono text-[13px] wrap-anywhere">{displayImage(img)}</span>
                      </span>
                    ))}
                  </span>
                )
              }
            />
          )}
          <KvItem mono className="col-span-2" k={o.directory} v={container.dir} />
          {container.config?.projectDir && <KvItem mono className="col-span-2" k={o.projectDir} v={container.config.projectDir} />}
        </div>
      </Section>
      <Section title={o.resources} actions={picker}>
        <ContainerMetrics projectId={project.id} nodeIds={projectMetricNodes(project)} container={container.name} range={range} />
      </Section>
      <ContainerEvents project={project} container={container} />
      {showServices && (
        <Section title={o.servicesTitle}>
          {services.length === 0 ? (
            <EmptyRow>{t.common.status.notDeployed}</EmptyRow>
          ) : (
            <List>
              {services.map((s) => (
                <Row
                  key={s.name + s.containerId}
                  title={s.name}
                  sub={<span title={s.image}>{[shortImage(s.image), serviceDetail(s.status)].filter(Boolean).join(" · ")}</span>}
                  cols={[{ phone: true, content: offline ? <Status tone="warning">{t.common.status.unknown}</Status> : <ServiceState service={s} /> }]}
                />
              ))}
            </List>
          )}
        </Section>
      )}
      {cfg?.source?.gitUrl && (
        <Section title={o.gitTitle}>
          <KeyValue
            items={[
              [
                o.gitRepo,
                <span className={mono}>
                  {cfg.source.gitUrl}
                  {cfg.source.gitRef && ` @ ${cfg.source.gitRef}`}
                </span>,
              ],
              [o.gitCommit, <span className={mono}>{container.lastCommit ? container.lastCommit.slice(0, 12) : "—"}</span>],
              [
                <KeyHelp k={o.deployKey} help={o.deployKeyHint} />,
                <>
                  <span className="min-w-0 truncate font-mono text-[13px]" title={container.deployKey}>
                    {container.deployKey}
                  </span>
                  <CopyButton text={container.deployKey} />
                </>,
              ],
              [
                <KeyHelp k={o.webhook} help={o.webhookHint} />,
                <>
                  <span className={cn(mono, "min-w-0")}>{webhookUrl}</span>
                  <CopyButton text={webhookUrl} />
                </>,
              ],
            ]}
          />
        </Section>
      )}
    </div>
  );
}

function KvItem({ k, v, mono, className }: { k: React.ReactNode; v: React.ReactNode; mono?: boolean; className?: string }) {
  return (
    <div className={cn("grid min-w-0 gap-1", className)}>
      <span className="text-muted-foreground">{k}</span>
      <span className={cn("wrap-anywhere", mono && "font-mono text-[13px]")}>{v}</span>
    </div>
  );
}

// KeyHelp：属性名加问号。
function KeyHelp({ k, help }: { k: string; help: string }) {
  return (
    <span className="flex items-center gap-1">
      {k}
      <HelpTip>{help}</HelpTip>
    </span>
  );
}

// mergeEvents：同一个人连续做的同一种操作并成一行（多次保存草稿、删除几个文件），告警不合并。
function mergeEvents<E extends { kind: string; action: string; actor: string; failed: boolean; time?: unknown }>(entries: E[]) {
  const out: { entry: E; count: number }[] = [];
  for (const e of entries) {
    const last = out[out.length - 1];
    if (last && e.kind !== "alert" && last.entry.kind === e.kind && last.entry.action === e.action && last.entry.actor === e.actor && last.entry.failed === e.failed) last.count++;
    else out.push({ entry: e, count: 1 });
  }
  return out;
}

// ContainerEvents：这个容器最近的操作和告警，一行一条：状态、详情、操作者、时间。
function ContainerEvents({ project, container }: { project: Project; container: Container }) {
  const ev = t.projectContainers.events;
  // 日志页只给管理员。
  const { isAdmin } = useRole();
  const events = useQuery({
    queryKey: ["container-events", project.id, container.name],
    queryFn: () => projectClient.listContainerEvents({ projectId: project.id, container: container.name }),
    refetchInterval: 15000,
  });
  const list = mergeEvents(events.data?.entries ?? []).slice(0, 8);
  return (
    <Section
      title={ev.title}
      actions={
        isAdmin && (
          <ButtonLink to="/activity" variant="ghost" readOnlyOk className="-mr-3">
            {ev.viewAll}
          </ButtonLink>
        )
      }
    >
      {events.isPending ? (
        <RowsSkeleton count={2} />
      ) : events.error ? (
        <QueryError error={errorMessage(events.error)} onRetry={() => events.refetch()} />
      ) : list.length === 0 ? (
        <EmptyRow>{ev.empty}</EmptyRow>
      ) : (
        <List>
          {list.map(({ entry: e, count }) => {
            // 常态的操作圆点是灰色，颜色只给失败和未恢复的告警。
            const tone = e.kind === "alert" ? (e.action === "resolved" ? "neutral" : "warning") : e.failed ? "danger" : "neutral";
            const detail = count === 1 ? e.detail : e.action.endsWith("VolumeFile") ? ev.files(count) : ev.times(count);
            return (
              <Row
                key={String(e.id)}
                title={
                  <>
                    <Status tone={tone}>{actionLabel(e)}</Status>
                    {detail && <span className="truncate text-muted-foreground">{detail}</span>}
                  </>
                }
                cols={[
                  { width: "w-24", content: e.actor },
                  { content: <span title={formatDateTime(toDate(e.time))}>{capitalize(timeAgo(toDate(e.time)))}</span> },
                ]}
              />
            );
          })}
        </List>
      )}
    </Section>
  );
}

// ---- 日志 ----

type Line = { key: number; service: string; stream: string; text: string; time?: Date };

const maxLines = 2000;

function Logs({ projectId, container, services, containers }: { projectId: string; container: string; services: string[]; containers: string }) {
  const l_ = t.projectContainers.logs;
  const [service, setService] = useState("");
  const [lines, setLines] = useState<Line[]>([]);
  const [error, setError] = useState("");
  const [connecting, setConnecting] = useState(true);
  const [attempt, setAttempt] = useState(0);
  const [query, setQuery] = useState("");
  const [stderrOnly, setStderrOnly] = useState(false);
  const [showTime, setShowTime] = useState(true);
  // 暂停时继续接收，只是显示停在暂停那一刻。
  const [frozen, setFrozen] = useState<Line[] | null>(null);
  const boxRef = useRef<HTMLDivElement>(null);
  const stick = useRef(true);
  const source = frozen ?? lines;
  const q = query.trim().toLowerCase();
  const visible = source.filter((l) => (!stderrOnly || l.stream === "stderr") && (!q || l.text.toLowerCase().includes(q)));
  const download = () => {
    const text = lines.map((l) => `${l.time ? l.time.toISOString() + " " : ""}${l.service ? l.service + " " : ""}${l.text}`).join("\n");
    const url = URL.createObjectURL(new Blob([text], { type: "text/plain;charset=utf-8" }));
    const a = document.createElement("a");
    a.href = url;
    a.download = `${container}${service ? "-" + service : ""}.log`;
    a.click();
    URL.revokeObjectURL(url);
  };

  useEffect(() => {
    const abort = new AbortController();
    let n = 0;
    setLines([]);
    setError("");
    setConnecting(true);
    // 流接上以后没有任何输出时收不到信号：等一会还没有日志，就当作「暂无日志」。
    const settle = setTimeout(() => setConnecting(false), 2000);
    (async () => {
      try {
        const stream = projectClient.streamLogs({ projectId, container, service, tail: 200, follow: true }, { signal: abort.signal });
        for await (const l of stream) {
          setConnecting(false);
          const line = { key: n++, service: l.service, stream: l.stream, text: l.text, time: toDate(l.time) };
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
    return () => {
      clearTimeout(settle);
      abort.abort();
    };
    // 容器变化（例如部署后容器被重建）时重新订阅。
  }, [projectId, container, service, containers, attempt]);

  useEffect(() => {
    const box = boxRef.current;
    if (box && stick.current && !frozen) box.scrollTop = box.scrollHeight;
  }, [lines, frozen]);

  return (
    <div className="grid gap-3">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div className="flex flex-wrap items-center gap-2">
          <Input value={query} onChange={(e) => setQuery(e.target.value)} placeholder={l_.search} aria-label={l_.searchLabel} autoComplete="off" className="w-60 max-md:w-full" />
          <Segmented
            value={stderrOnly ? "stderr" : "all"}
            onChange={(v) => setStderrOnly(v === "stderr")}
            items={[
              { value: "all", label: l_.all },
              { value: "stderr", label: l_.stderrOnly },
            ]}
          />
          {services.length > 1 && (
            <Select value={service} onChange={(e) => setService(e.target.value)} className="w-40" aria-label={l_.service}>
              <option value="">{l_.allContainers}</option>
              {services.map((s) => (
                <option key={s} value={s}>
                  {s}
                </option>
              ))}
            </Select>
          )}
        </div>
        <div className="flex items-center gap-2">
          {/* 复选框，开关状态一眼看得出。 */}
          <Check checked={showTime} onChange={setShowTime} className="mr-2">
            {l_.showTime}
          </Check>
          <Button variant="outline" readOnlyOk onClick={() => setFrozen(frozen ? null : lines)}>
            {frozen ? l_.resume : l_.pause}
          </Button>
          <Button variant="outline" readOnlyOk onClick={download} disabled={lines.length === 0}>
            {l_.download}
          </Button>
        </div>
      </div>
      {error && <QueryError error={error} onRetry={() => setAttempt((n) => n + 1)} />}
      <div
        ref={boxRef}
        onScroll={(e) => {
          const el = e.currentTarget;
          stick.current = el.scrollHeight - el.scrollTop - el.clientHeight < 40;
        }}
        // 日志框在浅色主题下也是深色底。底色取页面主题的 --log，和其他日志框一致；
        // 里面的每一行加 dark，灰字、红字、蓝字取深色主题的值，才对得上深底。
        className="h-[560px] overflow-auto rounded-lg bg-log px-4 py-3 font-mono text-[13px] leading-5 text-log-foreground max-md:h-[calc(100dvh-20rem)]"
      >
        {visible.length === 0 ? (
          <span className="opacity-50">{lines.length > 0 ? l_.noMatch : connecting && !error ? t.app.terminal.connecting : l_.empty}</span>
        ) : (
          visible.map((l) => (
            // 最多 2000 行：看不见的行跳过排版和绘制（content-visibility），滚动和追加不卡。
            <div key={l.key} className="dark break-all whitespace-pre-wrap [contain-intrinsic-size:auto_20px] [content-visibility:auto]">
              {showTime && l.time && <span className="mr-2 text-muted-foreground">{formatTime(l.time)}</span>}
              {!service && services.length > 1 && <span className="mr-2 text-info">{l.service}</span>}
              <span className={cn(l.stream === "stderr" && "text-destructive")}>{l.text}</span>
            </div>
          ))
        )}
      </div>
      {/* 实时和匹配信息放在黑框下面，工具条保持一行。 */}
      <span className="text-muted-foreground">
        {frozen ? l_.paused : l_.live(maxLines)}
        {(q || stderrOnly) && ` · ${l_.matched(visible.length, source.length)}`}
      </span>
    </div>
  );
}

// ---- 配置 ----

function ContainerConfig({ project, container, services, onDeploy, deploying }: { project: Project; container: Container; services: string[]; onDeploy: (revision: bigint) => void; deploying: boolean }) {
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
  const [backupPaths, setBackupPaths] = useState((base?.backupPaths ?? []).join(", "));
  // 现有目录（见 docs/agent.md#现有目录）：没有文件来源和保留路径，多一项快照包含的子目录。
  const projectDir = base?.projectDir ?? "";
  const [error, setError] = useState("");
  // busy 记的是哪个操作在进行：转圈只出现在被点的按钮上。
  const [busy, setBusy] = useState<"" | "save" | "deploy" | "discard">("");
  const formRef = useRef<HTMLFormElement>(null);

  const refresh = () => {
    queryClient.invalidateQueries({ queryKey: ["project", project.id] });
    queryClient.invalidateQueries({ queryKey: ["projects"] });
  };

  const call = async (kind: "save" | "deploy" | "discard", fn: () => Promise<unknown>, ok = "") => {
    setBusy(kind);
    setError("");
    try {
      await fn();
      if (ok) toast.success(ok);
      refresh();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy("");
    }
  };

  const buildConfig = () => {
    const common = {
      source: { gitUrl, gitRef },
      preserve: lines(preserve),
      env: textToEnv(env),
      dependsOn: dependsOn.split(/[\s,]+/).filter(Boolean),
      stateful: base?.stateful,
      projectDir,
      backupPaths: projectDir ? backupPaths.split(/[\s,]+/).filter(Boolean) : [],
    };
    return isForm
      ? { ...common, form: imageForm.build() }
      : {
          ...common,
          compose,
          serviceEnv: Object.fromEntries(
            Object.entries(serviceEnv)
              .map(([s, t]) => [s, { vars: textToEnv(t) }] as const)
              .filter(([, l]) => l.vars.length > 0),
          ),
        };
  };
  const dirty = useUnsaved(buildConfig());
  const [preview, setPreview] = useState(false);
  const showCompose = !isForm || preview;
  const [envMode, setEnvMode] = useState<EnvMode>("rows");

  const save = () =>
    call("save", async () => {
      const res = await projectClient.saveDraft({ projectId: project.id, container: container.name, baseRevision: container.revision, config: buildConfig() });
      toast.success(res.containers.find((c) => c.name === container.name)?.hasDraft ? t.projectContainers.config.savedDraft : t.projectContainers.config.noChanges);
    });

  // 保存为草稿后立即部署。已部署的容器会被重建，先确认。
  const saveAndDeploy = async () => {
    const form = formRef.current;
    if (form && !form.reportValidity()) return;
    if (container.deployedVersion > 0n && !(await confirmAction(t.projectContainers.detail.deployDraftConfirm(container.name), { action: t.projectContainers.config.saveAndDeploy }))) return;
    await call("deploy", async () => {
      const res = await projectClient.saveDraft({ projectId: project.id, container: container.name, baseRevision: container.revision, config: buildConfig() });
      const revision = res.containers.find((c) => c.name === container.name)?.revision;
      if (revision !== undefined) onDeploy(revision);
    });
  };

  const discard = async () => {
    if (!(await confirmAction(t.projectContainers.config.discardConfirm(container.name), { action: t.projectContainers.config.discardDraft, danger: true }))) return;
    // 放弃后对比区消失，页面上能看到结果，不加提示。
    call("discard", () => projectClient.discardDraft({ projectId: project.id, container: container.name, baseRevision: container.revision }));
  };

  const c = t.projectContainers.config;
  const textField = "font-mono text-[13px] max-md:text-base";
  const portsLink = (
    <p className="text-muted-foreground">
      <Link to="/projects/$projectId/c/$container" params={{ projectId: project.id, container: container.name }} search={{ view: "network" }} className="underline decoration-border underline-offset-4 hover:decoration-foreground">
        {c.portsLink}
      </Link>
    </p>
  );
  // 和其他表单一样：区块用 Section（之间一条细线），最宽 672，保存栏的按钮靠左。
  return (
    <form
      ref={formRef}
      className="max-w-[672px]"
      onSubmit={(e) => {
        e.preventDefault();
        save();
      }}
    >
      {container.hasDraft && (
        <Section
          title={c.diffTitle(container.deployedVersion > 0n ? String(container.deployedVersion) : "")}
          actions={
            <Button variant="ghost" className="-mr-3" onClick={discard} disabled={!!busy}>
              {c.discardDraft}
            </Button>
          }
        >
          <ConfigDiff deployed={container.config} draft={container.draft} />
        </Section>
      )}
      <Section
        title={c.groupImage}
        actions={
          // 容器创建后不能在单个容器和容器组之间切换：容器组只有 Compose，不显示切换。
          isForm && (
            <Segmented
              value={showCompose ? "compose" : "form"}
              onChange={(v) => setPreview(v === "compose")}
              items={[
                { value: "form", label: c.modeForm },
                { value: "compose", label: c.modeCompose },
              ]}
            />
          )
        }
      >
        {isForm ? (
          // 单个容器的 Compose 是表单生成的只读预览。
          <Slide id={showCompose ? "compose" : "form"} index={showCompose ? 1 : 0} inner="grid gap-4">
            {showCompose ? (
              <Field label={t.containerForm.compose} help={c.composePreviewHelp}>
                <pre className="m-0 overflow-auto rounded-lg bg-muted p-3 font-mono text-[13px] leading-5">{formToCompose(container.name, imageForm.build())}</pre>
              </Field>
            ) : (
              <>
                {imageForm.parts.image}
                {imageForm.parts.command}
              </>
            )}
          </Slide>
        ) : (
          <>
            <Field label={t.containerForm.compose} htmlFor="edit-compose" help={c.composeHelp}>
              <Textarea id="edit-compose" value={compose} onChange={(e) => setCompose(e.target.value)} spellCheck={false} className={composeInputClass} />
            </Field>
            {portsLink}
          </>
        )}
      </Section>
      <Section
        title={isForm ? t.containerForm.env : c.groupEnv}
        help={isForm ? undefined : c.groupEnvHelp}
        actions={
          <Button variant="ghost" className="-mr-3" onClick={() => setEnvMode(envMode === "rows" ? "text" : "rows")}>
            {envMode === "rows" ? t.containerForm.envText : t.containerForm.envRows}
          </Button>
        }
      >
        {isForm ? <EnvEditor id="edit-env" mode={envMode} value={imageForm.env} onChange={imageForm.setEnv} /> : <EnvEditor id="edit-env" mode={envMode} value={env} onChange={setEnv} />}
      </Section>
      {!isForm &&
        services.map((s) => (
          <Section key={s} title={c.serviceEnv(s)} help={c.serviceEnvHelp}>
            <EnvEditor id={`edit-env-${s}`} value={serviceEnv[s] ?? ""} onChange={(v) => setServiceEnv((prev) => ({ ...prev, [s]: v }))} />
          </Section>
        ))}
      {isForm && (
        <Section title={c.groupStorage}>
          <FormRow>
            {imageForm.parts.volumes}
            {imageForm.parts.workdir}
          </FormRow>
          {portsLink}
        </Section>
      )}
      <Section title={c.groupSource}>
        {projectDir ? (
          <FormRow>
            <Field label={c.backupPaths} htmlFor="edit-backup-paths" help={c.backupPathsHelp(projectDir)}>
              <Input id="edit-backup-paths" value={backupPaths} onChange={(e) => setBackupPaths(e.target.value)} placeholder="postgres" className={textField} autoComplete="off" spellCheck={false} />
            </Field>
            <Field label={c.dependsOn} htmlFor="edit-deps" help={`${c.dependsOnHint}${t.common.sentenceEnd}${c.dependsOnHelp}`}>
              <Input id="edit-deps" value={dependsOn} onChange={(e) => setDependsOn(e.target.value)} placeholder="db, cache" className={textField} autoComplete="off" spellCheck={false} />
            </Field>
          </FormRow>
        ) : (
          <FormRow>
            <Field label={c.git} htmlFor="edit-git" help={c.gitHelp}>
              <Input id="edit-git" value={gitUrl} onChange={(e) => setGitUrl(e.target.value)} placeholder="git@github.com:org/app.git" className={textField} autoComplete="off" spellCheck={false} />
            </Field>
            <Field label={c.ref} htmlFor="edit-ref" help={c.refHelp}>
              <Input id="edit-ref" value={gitRef} onChange={(e) => setGitRef(e.target.value)} placeholder="main" className={textField} autoComplete="off" spellCheck={false} />
            </Field>
            <Field label={c.preserve} htmlFor="edit-preserve" help={`${c.preserveHint}${t.common.sentenceEnd}${c.preserveHelp}`}>
              <Textarea id="edit-preserve" rows={Math.max(1, preserve.split("\n").length)} value={preserve} onChange={(e) => setPreserve(e.target.value)} spellCheck={false} placeholder="uploads/" className={cn(textField, "min-h-8 resize-none py-[5px] max-md:py-[9px]")} />
            </Field>
            <Field label={c.dependsOn} htmlFor="edit-deps" help={`${c.dependsOnHint}${t.common.sentenceEnd}${c.dependsOnHelp}`}>
              <Input id="edit-deps" value={dependsOn} onChange={(e) => setDependsOn(e.target.value)} placeholder="db, cache" className={textField} autoComplete="off" spellCheck={false} />
            </Field>
          </FormRow>
        )}
      </Section>
      {isForm && (
        <Section title={c.groupLimits}>
          <FormRow>{imageForm.parts.limits}</FormRow>
        </Section>
      )}
      {error && <Alert className="mt-6 max-md:mt-4">{error}</Alert>}
      <div className="sticky bottom-0 z-10 mt-6 flex flex-wrap items-center gap-x-4 gap-y-2 border-t bg-background py-3 max-md:mt-4">
        <FormActions>
          <Button disabled={!!busy || deploying} onClick={saveAndDeploy}>
            {(busy === "deploy" || deploying) && <Spinner />}
            {c.saveAndDeploy}
          </Button>
          <Button type="submit" variant="outline" disabled={!!busy || deploying}>
            {busy === "save" && <Spinner />}
            {c.saveDraft}
          </Button>
        </FormActions>
        <span className="text-muted-foreground">{dirty ? c.unsaved : c.notice}</span>
      </div>
    </form>
  );
}

// ---- 版本 ----

// imageDigest：镜像摘要的前 8 位；没有摘要时是镜像名。
function imageDigest(images: Record<string, string>) {
  return t.common.list(
    Object.values(images).map((image) => {
      const i = image.indexOf("sha256:");
      return i < 0 ? image : image.slice(i + 7, i + 15);
    }),
  );
}

// onRollback：回滚交给容器页执行，和部署共用输出和按钮状态，切换标签也不丢输出。
// onDeploy：还没有版本时，空状态下的「部署」。
function Versions({
  projectId,
  container,
  busy,
  deployDisabled,
  onDeploy,
  onRollback,
}: {
  projectId: string;
  container: Container;
  busy: boolean;
  deployDisabled: boolean;
  onDeploy: () => void;
  onRollback: (version: bigint, restoreData: boolean) => void;
}) {
  const versions = useQuery({
    queryKey: ["versions", projectId, container.name, String(container.revision)],
    queryFn: () => projectClient.listVersions({ projectId, container: container.name }),
  });
  const v_ = t.projectContainers.versions;
  const list = versions.data?.versions ?? [];
  const current = list.find((x) => x.version === container.deployedVersion);
  const describe = (x: (typeof list)[number]) => (
    <span>
      {`v${x.version}`}{" "}
      <span className="text-muted-foreground">
        {x.deployedAt ? `· ${timeAgo(toDate(x.deployedAt))}` : ""} · <span className="font-mono text-[13px]">{imageDigest(x.images)}</span>
      </span>
    </span>
  );
  const rollback = async (x: (typeof list)[number]) => {
    const answer = await ask(v_.rollbackConfirm(container.name, String(x.version)), {
      action: v_.rollback,
      check: v_.restoreDataCheck,
      checkHelp: v_.restoreDataHelp,
      checkDanger: true,
      details: [
        ...(current ? ([[v_.rollbackCurrent, describe(current)]] as [string, React.ReactNode][]) : []),
        [v_.rollbackTarget, describe(x)],
      ],
    });
    if (!answer) return;
    onRollback(x.version, answer.checked);
  };

  return (
    <Section className="max-md:[&>header]:hidden" title={t.projectContainers.tabs.versions} help={v_.intro}>
      {versions.isPending ? (
        <RowsSkeleton />
      ) : versions.error ? (
        <QueryError error={errorMessage(versions.error)} onRetry={() => versions.refetch()} />
      ) : list.length === 0 ? (
        <EmptyRow
          action={
            <Button variant="outline" disabled={deployDisabled} onClick={onDeploy}>
              {t.projectContainers.detail.deploy}
            </Button>
          }
        >
          {v_.empty}
        </EmptyRow>
      ) : (
        <List>
          {list.map((x) => {
            const notes = [
              x.version === container.deployedVersion ? v_.current : "",
              x.rollbackOf > 0n ? v_.rollbackOf(String(x.rollbackOf)) : "",
              x.commit ? x.commit.slice(0, 8) : "",
            ].filter(Boolean);
            return (
              <Row
                key={String(x.version)}
                title={
                  <>
                    <span>{`v${x.version}`}</span>
                    {!x.deployedAt && <span className="text-destructive">{v_.deployFailed}</span>}
                    {notes.length > 0 && <span className="truncate text-muted-foreground">{notes.join(" · ")}</span>}
                  </>
                }
                cols={[
                  { num: true, width: "w-64", content: <span className="font-mono text-[13px] leading-none" title={t.common.list(Object.values(x.images))}>{imageDigest(x.images)}</span> },
                  { content: x.deployedAt ? capitalize(timeAgo(toDate(x.deployedAt))) : "" },
                ]}
                actions={
                  x.version !== container.deployedVersion && x.deployedAt ? (
                    <Button size="row" variant="ghost" disabled={busy} onClick={() => rollback(x)}>
                      {v_.rollback}
                    </Button>
                  ) : null
                }
              />
            );
          })}
        </List>
      )}
    </Section>
  );
}

// ---- 添加容器 ----

// imageBaseName：镜像名里的短名字，作为容器名称的默认值，例如 ghcr.io/org/app:1.2 得到 app。
function imageBaseName(image: string) {
  const base = image.split("@")[0].split("/").pop()?.split(":")[0] ?? "";
  return base.toLowerCase().replace(/[^a-z0-9-]+/g, "-").replace(/^-+|-+$/g, "").slice(0, 32);
}

// AddContainerPage：占整个主区。只填镜像就能提交：名称取镜像名，节点默认是项目里容器最多的在线节点。
// 项目还没有容器时也从这里添加（概览写「暂无容器」和添加按钮）。
export function AddContainerPage({ projectId }: { projectId: string }) {
  const navigate = useNavigate();
  const project = useQuery({ queryKey: ["project", projectId], queryFn: () => projectClient.getProject({ projectId }) });
  if (project.isPending) return <RowsSkeleton />;
  if (project.error) return <QueryError error={errorMessage(project.error)} onRetry={() => project.refetch()} />;
  const p = project.data.project;
  if (!p) return null;
  return (
    <>
      <MobileBar back={() => navigate({ to: "/projects/$projectId", params: { projectId: p.id } })} title={t.projectContainers.add.title} />
      <Breadcrumb items={[{ label: t.projectContainers.page.projects, to: "/projects" }, { label: p.name, to: `/projects/${p.id}` }, { label: t.projectContainers.add.title }]} className="max-md:hidden xl:hidden" />
      <PageHeader title={t.projectContainers.add.title} className="max-md:hidden" />
      <AddContainer project={p} />
    </>
  );
}

function AddContainer({ project }: { project: Project }) {
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const [name, setName] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const deployAfter = useRef(true);
  const formRef = useRef<{ image: () => string } | null>(null);
  // 表单和节点选择互相要用对方的值，名称默认值放在提交时算。
  const node = useNodeChoice({ projectId: project.id, projectName: project.networkName, container: name || imageBaseName(formRef.current?.image() ?? "") || "web", preferNodes: project.containers.map((m) => m.nodeId) });
  const top = (mode: "compose" | "image") => (
    <FormRow>
      <Field
        label={mode === "compose" ? t.projects.create.groupName : t.projectContainers.columns.name}
        htmlFor="new-container"
        help={mode === "compose" ? t.containerForm.groupNameHelp : `${t.containerForm.nameHelpImage}${t.common.sentenceEnd}${t.projectContainers.add.nameHelp}`}
        error={checkName(name)}
      >
        <Input
          id="new-container"
          value={name}
          onChange={(e) => {
            setName(e.target.value);
            e.target.setCustomValidity(checkName(e.target.value));
          }}
          required={mode === "compose"}
          autoComplete="off"
          spellCheck={false}
          placeholder={mode === "compose" ? "web" : imageBaseName(formRef.current?.image() ?? "") || "web"}
        />
      </Field>
      {node.nodeField}
    </FormRow>
  );
  const { isAdmin } = useRole();
  const form = useContainerForm(project.containers.length === 0, top, node.dirField, isAdmin);
  formRef.current = form;
  useUnsaved({ name, config: form.mode === "dir" ? {} : form.build() });

  const add = async () => {
    setBusy(true);
    setError("");
    const finalName = name || imageBaseName(form.image());
    try {
      await projectClient.addContainer({ projectId: project.id, name: finalName, config: form.build(), nodeId: node.nodeId, dir: node.dirFor(finalName) });
      await queryClient.invalidateQueries({ queryKey: ["project", project.id] });
      navigate({
        to: "/projects/$projectId/c/$container",
        params: { projectId: project.id, container: finalName },
        search: deployAfter.current ? { deploy: true } : {},
        ignoreBlocker: true,
      });
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };

  // 类型切换在外面，下面三种内容（镜像表单、compose、现有目录）放进同一个 Slide，切换时横移。
  return (
    <div className="grid max-w-[672px] gap-4">
      {form.switcher}
      <Slide id={form.mode} index={["image", "compose", "dir"].indexOf(form.mode)} inner="grid gap-4">
        {form.mode === "dir" ? (
          <ImportDir project={project} />
        ) : (
          <form
            className="grid gap-4"
            onSubmit={(e) => {
              e.preventDefault();
              add();
            }}
          >
            {form.body}
            {error && <Alert>{error}</Alert>}
            <FormActions className="mt-2 max-md:grid max-md:[&>*]:w-full">
              <Button type="submit" disabled={busy || !node.nodeId} onClick={() => (deployAfter.current = true)}>
                {busy && deployAfter.current && <Spinner />}
                {t.projectContainers.add.addAndDeploy}
              </Button>
              <Button type="submit" variant="outline" disabled={busy || !node.nodeId} onClick={() => (deployAfter.current = false)}>
                {busy && !deployAfter.current && <Spinner />}
                {t.projectContainers.add.addOnly}
              </Button>
              <ButtonLink to="/projects/$projectId" params={{ projectId: project.id }} variant="ghost" readOnlyOk>
                {t.common.actions.cancel}
              </ButtonLink>
            </FormActions>
          </form>
        )}
      </Slide>
    </div>
  );
}
