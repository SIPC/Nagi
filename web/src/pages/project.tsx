import { useQuery } from "@tanstack/react-query";
import { Link, useRouterState } from "@tanstack/react-router";
import { ArrowUpCircle, ChevronLeft, Plus } from "lucide-react";
import { errorMessage, knownNodes, nodeClient, projectClient, toDate } from "@/lib/api";
import { cn, timeAgo } from "@/lib/utils";
import { t } from "@/i18n";
import { Breadcrumb, PageHeader } from "@/pages/layout";
import { offlineNodeNames, ProjectState, stateOf } from "@/components/status";
import { containerUsage, projectMetricNodes, UsageRings, useProjectMetrics } from "@/components/metrics";
import { ActiveBox, Badge, ButtonLink, dotColor, HelpTip, Loading, QueryError, Roll, Slide, Tabs, useActiveBox } from "@/components/ui";
import { containerKind, containerLabel, ContainersTab, useProjectActions, type DetailView } from "@/pages/project-containers";
import { NetworkTab } from "@/pages/project-network";
import { SettingsTab } from "@/pages/project-settings";
import type { Node } from "@/gen/nagi/v1/api_pb";
import type { Container } from "@/gen/nagi/v1/common_pb";

type ProjectData = Awaited<ReturnType<typeof projectClient.getProject>>;

// 项目页：项目由多个容器组成，每个容器指定一个节点运行，各自部署、记版本、回滚（见 docs/agent.md#项目与容器）。
// 内容分三块：概览（容器列表和监控）、网络、设置；当前一块记在地址的 tab 参数里。每个容器有自己的页（快照、顶替、迁移在容器页的「数据」里）。
// 宽屏（xl）是三栏：导航、项目栏（ProjectColumn）、右边的内容（概览、网络、设置或某个容器）；窄屏没有项目栏，改用标签页和上级链接。

export const projectTabs = ["containers", "network", "settings"] as const;
export type ProjectTab = (typeof projectTabs)[number];

const sectionTitles: Record<ProjectTab, string> = { containers: t.project.sections.overview, network: t.project.sections.network, settings: t.project.sections.settings };

export function ProjectPage({ projectId, tab, onTab }: { projectId: string; tab: ProjectTab; onTab: (t: ProjectTab) => void }) {
  const project = useQuery({
    queryKey: ["project", projectId],
    queryFn: () => projectClient.getProject({ projectId }),
    refetchInterval: 5000,
  });

  if (project.isPending) return <Loading />;
  // 加载失败：一行错误加重试，回项目列表用上面的上级链接。
  if (project.error) {
    return (
      <>
        <Breadcrumb items={[{ label: t.project.breadcrumb, to: "/projects" }, { label: projectId }]} />
        <QueryError error={errorMessage(project.error)} onRetry={() => project.refetch()} />
      </>
    );
  }
  const p = project.data.project;
  if (!p) return null;
  return <ProjectBody p={p} services={project.data.status?.services ?? []} imageUpdates={project.data.status?.imageUpdates ?? []} tab={tab} onTab={onTab} />;
}

function ProjectBody({ p, services, imageUpdates, tab, onTab }: { p: NonNullable<ProjectData["project"]>; services: NonNullable<ProjectData["status"]>["services"]; imageUpdates: NonNullable<ProjectData["status"]>["imageUpdates"]; tab: ProjectTab; onTab: (t: ProjectTab) => void }) {
  const nodes = useQuery({ queryKey: ["nodes"], queryFn: () => nodeClient.listNodes({}) });
  const nodeName = (id: string) => knownNodes(nodes.data).find((n) => n.id === id)?.name ?? id;
  const nodeIds = [...new Set(p.containers.map((c) => c.nodeId))];
  const { actions, deploy, busy, output, hasOutput } = useProjectActions(p, imageUpdates);

  return (
    <>
      <Breadcrumb items={[{ label: t.project.breadcrumb, to: "/projects" }, { label: p.name }]} className="max-md:hidden xl:hidden" />
      {/* 宽屏和手机上项目名已在项目栏、标题栏里：概览仍显示项目标题和元信息，网络、设置只显示这一块的标题。
          PageHeader 把标题里的 span 设成 400，这两个 span 是标题本身，改回 600，和网络、设置页一致。 */}
      <div className={cn(tab !== "containers" && "max-md:hidden xl:hidden")}>
        <PageHeader
          title={
            <>
              <span className="hidden font-semibold! tracking-tight! max-md:inline xl:inline">{t.project.sections.overview}</span>
              <span className="font-semibold! tracking-tight! max-md:hidden xl:hidden">{p.name}</span>
              <span className="flex items-center gap-3 max-md:hidden xl:hidden">
                <ProjectState services={services} deployed={p.containers.some((m) => m.deployedVersion > 0n)} offline={offlineNodeNames(p.containers, knownNodes(nodes.data))} />
              </span>
            </>
          }
          actions={tab === "containers" ? actions : undefined}
          meta={
            <>
              <span className="max-md:hidden xl:hidden">{p.networkName}</span>
              {nodeIds.length > 0 && <span>{t.project.nodes(nodeIds.length, t.common.list(nodeIds.map(nodeName)))}</span>}
              <span>{countContainers(p.containers)}</span>
              {p.rebuiltAt ? (
                <span className="inline-flex items-center gap-1">
                  {t.project.rebuiltAt(timeAgo(toDate(p.rebuiltAt)))}
                  <HelpTip>{t.project.rebuiltHint}</HelpTip>
                </span>
              ) : (
                <span>{t.project.createdAt(timeAgo(toDate(p.createdAt)))}</span>
              )}
            </>
          }
        />
      </div>
      {tab !== "containers" && (
        <div className="hidden max-md:block xl:block">
          <PageHeader title={sectionTitles[tab]} />
        </div>
      )}
      <Tabs
        className="mb-6 max-md:hidden xl:hidden"
        value={tab}
        onChange={onTab}
        items={[
          { value: "containers", label: t.project.sections.overview },
          { value: "network", label: t.project.sections.network, count: p.routes.length },
          { value: "settings", label: t.project.sections.settings },
        ]}
      />
      {tab === "containers" && <ContainersTab project={p} services={services} deploy={deploy} busy={busy} output={output} hasOutput={hasOutput} />}
      {tab === "network" && <NetworkTab project={p} services={services} />}
      {tab === "settings" && <SettingsTab project={p} />}
    </>
  );
}

// ProjectSplit：项目的三栏布局。宽屏（xl）左边是项目栏，页面滚动时固定在原处；右边的内容换了（另一块或另一个容器）时淡入。
// 窄屏没有项目栏，只剩右边的内容。
export function ProjectSplit({
  projectId,
  tab,
  container,
  view,
  adding,
  children,
}: {
  projectId: string;
  tab: ProjectTab;
  container?: string;
  view?: DetailView;
  adding?: boolean;
  children: React.ReactNode;
}) {
  // 按项目栏的顺序比较：概览、添加容器、各个容器、网络、设置。
  const containers = useQuery({ queryKey: ["project", projectId], queryFn: () => projectClient.getProject({ projectId }) }).data?.project?.containers ?? [];
  const position = adding ? 1 : container ? 2 + containers.findIndex((m) => m.name === container) : tab === "containers" ? 0 : tab === "network" ? 2 + containers.length : 3 + containers.length;
  return (
    <>
      <aside className="hidden motion-safe:animate-fade-in xl:sticky xl:top-0 xl:block xl:h-screen xl:w-70 xl:shrink-0 xl:overflow-y-auto xl:border-r xl:px-3 xl:pt-8 xl:pb-4">
        <ProjectColumn projectId={projectId} tab={tab} container={container} view={view} adding={adding} />
      </aside>
      <div className="min-w-0 flex-1 xl:px-8 xl:pt-8 xl:pb-16">
        <Slide id={adding ? "add" : container ? "c/" + container : tab} index={position} variant="project" className="xl:mx-auto xl:max-w-[1120px]">
          {children}
        </Slide>
      </div>
    </>
  );
}

// navState：项目导航里容器的状态点。节点离线时容器的状态是未知。
function navState(m: Container, node: Node | undefined, services: NonNullable<ProjectData["status"]>["services"]) {
  if (node?.online === false) return { tone: "warning" as const, label: t.common.status.nodesOffline([node.name]) };
  return stateOf(
    services.filter((s) => s.container === m.name),
    m.deployedVersion > 0n,
  );
}

// MobileProjectNav：手机的全屏导航在项目里的内容，和项目栏同样的入口，行高 44。点容器时同样保留当前的标签。
export function MobileProjectNav({ projectId }: { projectId: string }) {
  const location = useRouterState({ select: (s) => s.location });
  const project = useQuery({ queryKey: ["project", projectId], queryFn: () => projectClient.getProject({ projectId }) });
  const nodes = useQuery({ queryKey: ["nodes"], queryFn: () => nodeClient.listNodes({}) });
  if (project.isPending) return <Loading />;
  if (project.error) return <QueryError error={errorMessage(project.error)} onRetry={() => project.refetch()} />;
  const p = project.data.project;
  if (!p) return null;
  const parts = location.pathname.split("/").filter(Boolean);
  const search = location.search as { tab?: ProjectTab; view?: DetailView };
  const container = parts[2] === "c" ? decodeURIComponent(parts[3] ?? "") : "";
  const onSection = parts.length === 2;
  const tab = search.tab ?? "containers";
  const item = (active: boolean) => cn("flex min-h-11 items-center gap-2 rounded-md px-2 text-sm transition-colors", active ? "bg-accent" : "hover:bg-accent/50");
  const label = "mt-1 flex h-6 items-center px-2 text-[13px] text-muted-foreground";
  return (
    <nav className="grid gap-1" aria-label={p.name}>
      <Link to="/projects" className={item(false)}>
        <ChevronLeft className="size-4" />
        {t.project.column.allProjects}
      </Link>
      <Link to="/projects/$projectId" params={{ projectId: p.id }} className={item(onSection && tab === "containers")}>
        {t.project.sections.overview}
      </Link>
      <div className={label}>
        {t.project.column.containers}
        <Roll value={String(p.containers.length)} className="ml-1 tabular-nums" />
      </div>
      {p.containers.map((m) => {
        const node = knownNodes(nodes.data).find((n) => n.id === m.nodeId);
        const st = navState(m, node, project.data.status?.services ?? []);
        return (
          <Link key={m.name} to="/projects/$projectId/c/$container" params={{ projectId: p.id, container: m.name }} search={{ view: search.view }} className={item(m.name === container)} title={st.label}>
            <span className={cn("size-1.5 shrink-0 rounded-full", dotColor[st.tone])} />
            <span className="min-w-0 truncate">{containerLabel(m)}</span>
            <span className="sr-only">{t.project.column.stateOnNode(st.label, node?.name ?? m.nodeId)}</span>
            {m.hasDraft && <Badge tone="warning">{t.project.column.draft}</Badge>}
            <span aria-hidden className="ml-auto shrink-0 font-mono text-xs leading-5 text-muted-foreground">{node?.name ?? m.nodeId}</span>
          </Link>
        );
      })}
      <div className={label}>{t.project.column.project}</div>
      <Link to="/projects/$projectId" params={{ projectId: p.id }} search={{ tab: "network" }} className={item(onSection && tab === "network")}>
        {t.project.sections.network}
        {p.routes.length > 0 && <Roll value={String(p.routes.length)} className="ml-auto text-xs tabular-nums text-muted-foreground" />}
      </Link>
      <Link to="/projects/$projectId" params={{ projectId: p.id }} search={{ tab: "settings" }} className={item(onSection && tab === "settings")}>
        {t.project.sections.settings}
      </Link>
    </nav>
  );
}

// ProjectColumn：项目栏。概览、各个容器（状态、节点、当前 CPU 和内存）、网络、设置，选中的一项由一块背景标出，换选中项时这块背景滑过去。
// 点容器时保留容器页当前的标签，方便对比几个容器的日志。
function ProjectColumn({ projectId, tab, container, view, adding }: { projectId: string; tab: ProjectTab; container?: string; view?: DetailView; adding?: boolean }) {
  const project = useQuery({
    queryKey: ["project", projectId],
    queryFn: () => projectClient.getProject({ projectId }),
    refetchInterval: 5000,
  });
  const nodes = useQuery({ queryKey: ["nodes"], queryFn: () => nodeClient.listNodes({}) });
  const metrics = useProjectMetrics(projectId, projectMetricNodes(project.data?.project));
  const usage = containerUsage(metrics.data);
  const memFallback = Math.max(0, ...[...usage.values()].map((u) => u.mem));
  const { ref, box } = useActiveBox();

  if (project.isPending) return <Loading />;
  if (project.error) return <QueryError error={errorMessage(project.error)} onRetry={() => project.refetch()} />;
  const p = project.data.project;
  if (!p) return null;
  const services = project.data.status?.services ?? [];
  const imageUpdates = project.data.status?.imageUpdates ?? [];
  const section = (t: ProjectTab) => !container && !adding && tab === t;
  const item = "relative flex h-8 items-center gap-2 rounded-md px-2 text-sm transition-colors";
  const idle = "hover:bg-accent/50";

  return (
    <div className="grid grid-cols-1 gap-4">
      <div className="grid gap-1 px-2">
        <Link to="/projects" className="w-fit text-[13px] leading-5 text-muted-foreground hover:text-foreground">
          {t.project.breadcrumb}
        </Link>
        <h2 className="truncate text-base leading-6 font-semibold">{p.name}</h2>
        <div className="flex flex-wrap items-center gap-x-2 gap-y-1 text-muted-foreground">
          <span className="contents [&_span]:text-[13px] [&_span]:leading-5">
          <ProjectState services={services} deployed={p.containers.some((m) => m.deployedVersion > 0n)} offline={offlineNodeNames(p.containers, knownNodes(nodes.data))} />
          </span>
          <span className="font-mono text-[13px] leading-5 text-muted-foreground">{p.networkName}</span>
        </div>
      </div>
      <nav ref={ref} className="relative grid gap-1" aria-label={p.name}>
        <ActiveBox box={box} />
        <Link to="/projects/$projectId" params={{ projectId: p.id }} data-active={section("containers") || undefined} className={cn(item, !section("containers") && idle)}>
          {t.project.sections.overview}
        </Link>
        <div className="relative mt-2 flex h-6 items-center justify-between px-2 text-[13px] text-muted-foreground">
          <span>
            {t.project.column.containers}
            <Roll value={String(p.containers.length)} className="ml-1 tabular-nums" />
          </span>
          <ButtonLink to="/projects/$projectId/add" params={{ projectId: p.id }} variant="ghost" size="icon" className="-my-2 -mr-2 text-foreground" aria-label={t.projectContainers.table.addContainer}>
            <Plus />
          </ButtonLink>
        </div>
        {p.containers.map((m) => {
          const node = knownNodes(nodes.data).find((n) => n.id === m.nodeId);
          const st = navState(m, node, services);
          const active = m.name === container;
          const u = node?.online === false ? undefined : usage.get(m.name);
          return (
            <Link
              key={m.name}
              to="/projects/$projectId/c/$container"
              params={{ projectId: p.id, container: m.name }}
              search={{ view }}
              data-active={active || undefined}
              className={cn("relative grid gap-1 rounded-md p-2 transition-colors", !active && "hover:bg-accent/50")}
            >
              <span className="flex min-w-0 items-center gap-2 leading-5" title={st.label}>
                <span className={cn("size-1.5 shrink-0 rounded-full", dotColor[st.tone])} />
                <span className={cn("truncate", active && "font-medium")}>{containerLabel(m)}</span>
                <span className="sr-only">{t.project.column.stateOnNode(st.label, node?.name ?? m.nodeId)}</span>
                {m.kind === "group" && <span className="shrink-0 text-xs text-muted-foreground">{containerKind(m)}</span>}
                {m.hasDraft && <Badge tone="warning">{t.project.column.draft}</Badge>}
                {imageUpdates.some((x) => x.container === m.name) && (
                  <span title={t.projects.card.imageUpdate} className="inline-flex shrink-0">
                    <ArrowUpCircle role="img" aria-label={t.projects.card.imageUpdate} className="size-3.5 text-info" />
                  </span>
                )}
                <span aria-hidden className={cn("ml-auto shrink-0 font-mono text-xs leading-5 text-muted-foreground", active && "font-medium")}>{node?.name ?? m.nodeId}</span>
              </span>
              {node?.online === false ? (
                <span className="pl-3.5 text-xs text-warning-text">{t.common.status.nodesOffline([node.name])}</span>
              ) : (
                <UsageRings usage={u} memFallback={memFallback} className="pl-3.5" />
              )}
            </Link>
          );
        })}
        <div className="h-1" />
        <Link to="/projects/$projectId" params={{ projectId: p.id }} search={{ tab: "network" }} data-active={section("network") || undefined} className={cn(item, !section("network") && idle)}>
          {t.project.sections.network}
          {p.routes.length > 0 && <Roll value={String(p.routes.length)} className="ml-auto text-xs leading-5 tabular-nums text-muted-foreground" />}
        </Link>
        <Link to="/projects/$projectId" params={{ projectId: p.id }} search={{ tab: "settings" }} data-active={section("settings") || undefined} className={cn(item, !section("settings") && idle)}>
          {t.project.sections.settings}
        </Link>
      </nav>
    </div>
  );
}

// countContainers 按种类数，例如"2 个容器、1 个容器组、1 个有状态"。
function countContainers(cs: Container[]) {
  if (cs.length === 0) return t.project.noContainers;
  const groups = cs.filter((c) => c.kind === "group").length;
  const singles = cs.length - groups;
  const stateful = cs.filter((c) => c.stateful).length;
  return t.common.list([singles > 0 && t.project.containerCount(singles), groups > 0 && t.project.groupCount(groups), stateful > 0 && t.project.statefulCount(stateful)].filter((s): s is string => !!s));
}
