import { useLayoutEffect, useRef, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { LayoutGrid, Network, Settings2 } from "lucide-react";
import { errorMessage, nodeClient, projectClient, toDate } from "@/lib/api";
import { cn, timeAgo } from "@/lib/utils";
import { t } from "@/i18n";
import { Breadcrumb, PageHeader } from "@/pages/layout";
import { ProjectState, stateOf } from "@/components/status";
import { containerUsage, UsageBars, useProjectMetrics } from "@/components/metrics";
import { Alert, Badge, dotColor, Loading, Tabs } from "@/components/ui";
import { containerKind, ContainersTab, type DetailView } from "@/pages/project-containers";
import { NetworkTab } from "@/pages/project-network";
import { SettingsTab } from "@/pages/project-settings";
import type { Container } from "@/gen/nagi/v1/common_pb";

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
  const nodes = useQuery({ queryKey: ["nodes"], queryFn: () => nodeClient.listNodes({}) });
  const nodeName = (id: string) => nodes.data?.nodes.find((n) => n.id === id)?.name ?? id;

  if (project.isPending) return <Loading />;
  if (project.error) {
    return (
      <>
        <Breadcrumb items={[{ label: t.project.breadcrumb, to: "/projects" }, { label: projectId }]} />
        <Alert>{errorMessage(project.error)}</Alert>
      </>
    );
  }
  const p = project.data.project;
  const services = project.data.status?.services ?? [];
  const imageUpdates = project.data.status?.imageUpdates ?? [];
  if (!p) return null;
  const nodeIds = [...new Set(p.containers.map((c) => c.nodeId))];

  return (
    <>
      <Breadcrumb items={[{ label: t.project.breadcrumb, to: "/projects" }, { label: p.name }]} className="xl:hidden" />
      {/* 宽屏时项目名已在项目栏里：概览仍显示项目标题和元信息，网络、设置只显示这一块的标题。 */}
      <div className={cn(tab !== "containers" && "xl:hidden")}>
        <PageHeader
          title={
            <>
              {p.name}
              <ProjectState services={services} deployed={p.containers.some((m) => m.deployedVersion > 0n)} />
              {p.stateful && <Badge>{t.project.stateful}</Badge>}
            </>
          }
          meta={
            <>
              <span className="font-mono text-xs">{p.networkName}</span>
              <span>{nodeIds.length > 0 ? t.project.nodes(nodeIds.map(nodeName)) : t.project.noContainers}</span>
              <span>{countContainers(p.containers)}</span>
              {p.rebuiltAt ? (
                <span title={t.project.rebuiltHint}>{t.project.rebuiltAt(timeAgo(toDate(p.rebuiltAt)))}</span>
              ) : (
                <span>{t.project.createdAt(timeAgo(toDate(p.createdAt)))}</span>
              )}
            </>
          }
        />
      </div>
      {tab !== "containers" && (
        <div className="hidden xl:block">
          <PageHeader title={sectionTitles[tab]} />
        </div>
      )}
      <Tabs
        className="mb-6 xl:hidden"
        value={tab}
        onChange={onTab}
        items={[
          { value: "containers", label: t.project.tabs.containers, count: p.containers.length },
          { value: "network", label: t.project.tabs.network, count: p.routes.length },
          { value: "settings", label: t.project.tabs.settings },
        ]}
      />
      {tab === "containers" && <ContainersTab project={p} services={services} imageUpdates={imageUpdates} />}
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
  children,
}: {
  projectId: string;
  tab: ProjectTab;
  container?: string;
  view?: DetailView;
  children: React.ReactNode;
}) {
  return (
    <>
      <aside className="hidden motion-safe:animate-fade-in xl:sticky xl:top-0 xl:block xl:h-screen xl:w-80 xl:shrink-0 xl:overflow-y-auto xl:border-r xl:px-4 xl:pt-10 xl:pb-8">
        <ProjectColumn projectId={projectId} tab={tab} container={container} view={view} />
      </aside>
      <div className="min-w-0 flex-1 xl:px-10 xl:pt-10 xl:pb-16">
        <div key={container ? "c/" + container : tab} className="animate-fade-in motion-safe:animate-pane-in xl:mx-auto xl:max-w-6xl">
          {children}
        </div>
      </div>
    </>
  );
}

// ProjectColumn：项目栏。概览、各个容器（状态、节点、当前 CPU 和内存）、网络、设置，选中的一项由一块背景标出，换选中项时这块背景滑过去。
// 点容器时保留容器页当前的标签，方便对比几个容器的日志。
function ProjectColumn({ projectId, tab, container, view }: { projectId: string; tab: ProjectTab; container?: string; view?: DetailView }) {
  const project = useQuery({
    queryKey: ["project", projectId],
    queryFn: () => projectClient.getProject({ projectId }),
    refetchInterval: 5000,
  });
  const nodes = useQuery({ queryKey: ["nodes"], queryFn: () => nodeClient.listNodes({}) });
  const metrics = useProjectMetrics(projectId, project.data?.project?.containers.map((c) => c.nodeId) ?? []);
  const usage = containerUsage(metrics.data);
  const memFallback = Math.max(0, ...[...usage.values()].map((u) => u.mem));
  const { ref, box } = useActiveBox();

  if (project.isPending) return <Loading />;
  if (project.error) return <Alert>{errorMessage(project.error)}</Alert>;
  const p = project.data.project;
  if (!p) return null;
  const services = project.data.status?.services ?? [];
  const section = (t: ProjectTab) => !container && tab === t;
  const item = "relative flex items-center gap-2.5 rounded-md px-2.5 transition-colors [&_svg]:size-4";
  const idle = "text-muted-foreground hover:bg-accent/50 hover:text-foreground";

  return (
    <div className="grid gap-6">
      <div className="px-2.5">
        <Breadcrumb items={[{ label: t.project.breadcrumb, to: "/projects" }]} />
        <h2 className="truncate text-lg font-semibold tracking-tight">{p.name}</h2>
        <div className="mt-1.5 flex flex-wrap items-center gap-x-3 gap-y-1 text-muted-foreground">
          <ProjectState services={services} deployed={p.containers.some((m) => m.deployedVersion > 0n)} />
          <span className="font-mono text-xs">{p.networkName}</span>
        </div>
      </div>
      <nav ref={ref} className="relative grid gap-0.5" aria-label={p.name}>
        {box && (
          <div
            aria-hidden
            className={cn(
              "pointer-events-none absolute inset-x-0 top-0 rounded-md bg-accent",
              box.animate && "transition-[transform,height] duration-200 ease-[cubic-bezier(0.23,1,0.32,1)] motion-reduce:transition-none",
            )}
            style={{ transform: `translateY(${box.top}px)`, height: box.height }}
          />
        )}
        <Link
          to="/projects/$projectId"
          params={{ projectId: p.id }}
          data-active={section("containers") || undefined}
          className={cn(item, "h-8 text-sm", section("containers") ? "font-medium text-foreground" : idle)}
        >
          <LayoutGrid />
          {t.project.sections.overview}
        </Link>
        <div className="mt-4 mb-1 flex items-center px-2.5 text-xs font-medium text-muted-foreground">
          {t.project.column.containers}
          <span className="ml-1.5 tabular-nums opacity-70">{p.containers.length}</span>
        </div>
        {p.containers.length === 0 && <p className="px-2.5 py-1 text-[13px] text-muted-foreground">{t.project.noContainers}</p>}
        {p.containers.map((m) => {
          const node = nodes.data?.nodes.find((n) => n.id === m.nodeId);
          const st =
            node?.online === false
              ? { tone: "warning" as const, label: t.project.column.nodeOffline }
              : stateOf(
                  services.filter((s) => s.container === m.name),
                  m.deployedVersion > 0n,
                );
          const active = m.name === container;
          const u = node?.online === false ? undefined : usage.get(m.name);
          return (
            <Link
              key={m.name}
              to="/projects/$projectId/c/$container"
              params={{ projectId: p.id, container: m.name }}
              search={{ view }}
              data-active={active || undefined}
              className={cn("relative flex items-center gap-3 rounded-md px-2.5 py-2 transition-colors", !active && "hover:bg-accent/50")}
            >
              <span className="grid min-w-0 flex-1 gap-1">
                <span className="flex min-w-0 items-center gap-1.5">
                  <span className="truncate text-sm font-medium">{m.name}</span>
                  {m.kind === "group" && <span className="shrink-0 text-xs text-muted-foreground">{containerKind(m)}</span>}
                  {m.hasDraft && <Badge tone="warning">{t.project.column.draft}</Badge>}
                </span>
                {/* 状态只用圆点的颜色表示，文字放在悬停提示和读屏里。 */}
                <span className="flex min-w-0 items-center gap-1.5 text-muted-foreground" title={st.label}>
                  <span className={cn("size-2 shrink-0 rounded-full", dotColor[st.tone])} />
                  <span className="sr-only">{t.project.column.stateOnNode(st.label)}</span>
                  <span className="truncate text-xs">{node?.name ?? m.nodeId}</span>
                </span>
              </span>
              <UsageBars usage={u} memFallback={memFallback} className="w-32 shrink-0" />
            </Link>
          );
        })}
        <div className="my-3 border-t" />
        <Link
          to="/projects/$projectId"
          params={{ projectId: p.id }}
          search={{ tab: "network" }}
          data-active={section("network") || undefined}
          className={cn(item, "h-8 text-sm", section("network") ? "font-medium text-foreground" : idle)}
        >
          <Network />
          {t.project.sections.network}
          {p.routes.length > 0 && <span className="ml-auto text-xs tabular-nums text-muted-foreground">{p.routes.length}</span>}
        </Link>
        <Link
          to="/projects/$projectId"
          params={{ projectId: p.id }}
          search={{ tab: "settings" }}
          data-active={section("settings") || undefined}
          className={cn(item, "h-8 text-sm", section("settings") ? "font-medium text-foreground" : idle)}
        >
          <Settings2 />
          {t.project.sections.settings}
        </Link>
      </nav>
    </div>
  );
}

// useActiveBox 量出列表里选中项（data-active）的位置，给滑动的背景用。第一次只放到位、不做动画。
function useActiveBox() {
  const ref = useRef<HTMLElement>(null);
  const [box, setBox] = useState<{ top: number; height: number; animate: boolean } | null>(null);
  useLayoutEffect(() => {
    const el = ref.current?.querySelector<HTMLElement>("[data-active]");
    setBox((prev) => {
      if (!el) return null;
      const top = el.offsetTop;
      const height = el.offsetHeight;
      if (prev && prev.top === top && prev.height === height) return prev;
      return { top, height, animate: prev !== null };
    });
  });
  return { ref, box };
}

// countContainers 按种类数，例如"2 个容器，1 个容器组"。
function countContainers(cs: Container[]) {
  if (cs.length === 0) return t.project.noContainers;
  const groups = cs.filter((c) => c.kind === "group").length;
  const singles = cs.length - groups;
  return [singles > 0 && t.project.containerCount(singles), groups > 0 && t.project.groupCount(groups)].filter(Boolean).join(t.project.countSeparator);
}
