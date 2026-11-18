import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { errorMessage, knownNodes, nodeClient, projectClient } from "@/lib/api";
import { t } from "@/i18n";
import { useRun } from "@/components/run";
import { confirmAction } from "@/components/confirm";
import { ContainerMetrics, projectMetricNodes, useProjectMetrics, useRange } from "@/components/metrics";
import { serviceDetail, ServiceState, stateOf } from "@/components/status";
import { TerminalView } from "@/components/terminal";
import { useRole } from "@/components/role";
import { ContainerAction } from "@/gen/nagi/v1/agent_pb";
import type { Container, Project, ServiceStatus } from "@/gen/nagi/v1/common_pb";
import { Breadcrumb, PageHeader } from "@/pages/layout";
import { containerLabel, ContainerSkeleton, displayImage, KvItem, Logs, uptime } from "@/pages/project-containers";
import { ServiceRoutes } from "@/pages/project-network";
import { Button, ButtonLink, List, Menu, Notice, QueryError, Roll, Row, Section, Slide, Spinner, Status, Tabs, textLink } from "@/components/ui";

// 服务页：容器组里一个服务自己的页（项目 / 容器组 / 服务），标签是概览、日志、终端、监控；当前标签记在地址的 view 参数里。
// 配置、版本、存储、高可用和告警按整个容器组算，在容器组的页里。重启、停止、启动只动这个服务，其他服务继续运行。

export const serviceTabs = ["overview", "logs", "terminal", "metrics"] as const;
export type ServiceView = (typeof serviceTabs)[number];

export function ServicePage({ projectId, container: name, service, view, onView }: { projectId: string; container: string; service: string; view: ServiceView; onView: (v: ServiceView) => void }) {
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
  const list = (project.data.status?.services ?? []).filter((s) => s.container === name && s.name === service);
  if (!container || list.length === 0) {
    return (
      <Notice
        tone="warning"
        actions={
          container ? (
            <ButtonLink to="/projects/$projectId/c/$container" params={{ projectId: p.id, container: container.name }} variant="outline" readOnlyOk>
              {t.service.backToGroup}
            </ButtonLink>
          ) : (
            <ButtonLink to="/projects/$projectId" params={{ projectId: p.id }} variant="outline" readOnlyOk>
              {t.projectContainers.page.backToProject}
            </ButtonLink>
          )
        }
      >
        {container ? t.service.notFound(service, containerLabel(container)) : t.projectContainers.page.notFound(name)}
      </Notice>
    );
  }
  return (
    <>
      <Breadcrumb
        items={[
          { label: t.projectContainers.page.projects, to: "/projects" },
          { label: p.name, to: `/projects/${p.id}` },
          { label: containerLabel(container), to: `/projects/${p.id}/c/${encodeURIComponent(container.name)}` },
          { label: service },
        ]}
        className="max-md:hidden xl:hidden"
      />
      <ServiceDetail project={p} container={container} service={service} list={list} view={view} onView={onView} />
    </>
  );
}

function ServiceDetail({ project, container, service, list, view: viewProp, onView }: { project: Project; container: Container; service: string; list: ServiceStatus[]; view: ServiceView; onView: (v: ServiceView) => void }) {
  const s = t.service;
  const d = t.projectContainers.detail;
  const { isViewer } = useRole();
  // 只读角色没有终端，直接打开这个地址时显示概览。
  const view = isViewer && viewProp === "terminal" ? "overview" : viewProp;
  const { run, busy, output, hasOutput } = useRun(project.id);
  const nodes = useQuery({ queryKey: ["nodes"], queryFn: () => nodeClient.listNodes({}) });
  const node = knownNodes(nodes.data).find((n) => n.id === container.nodeId);
  const nodeOnline = node?.online !== false;
  const deployed = container.deployedVersion > 0n;
  const running = list.some((x) => x.state === "running" || x.state === "paused" || x.state === "restarting");
  const stopped = list.some((x) => x.state !== "running" && x.state !== "paused" && x.state !== "restarting");
  const st = stateOf(list, deployed);
  const control = (action: ContainerAction) => {
    if (view !== "overview") onView("overview");
    return run(() => projectClient.controlService({ projectId: project.id, container: container.name, service, action }));
  };
  const runningNames = list.some((x) => x.state === "running") ? [service] : [];

  return (
    <>
      <PageHeader
        className="mb-4"
        actionsClassName="max-md:w-full max-md:flex-nowrap max-md:[&>button:not(:last-child)]:flex-1"
        title={
          <>
            {service}
            {node && !nodeOnline ? <Status>{t.common.status.nodesOffline([node.name])}</Status> : <Status tone={st.tone}>{st.label}</Status>}
          </>
        }
        meta={
          <span>
            {s.belongsTo}{" "}
            <Link to="/projects/$projectId/c/$container" params={{ projectId: project.id, container: container.name }} className={textLink}>
              {containerLabel(container)}
            </Link>
          </span>
        }
        actions={
          deployed && (
            <>
              {running || !nodeOnline ? (
                <Button
                  variant="outline"
                  size="sm"
                  disabled={busy || !nodeOnline}
                  onClick={async () => (await confirmAction(s.restartConfirm(service), { action: d.restart })) && control(ContainerAction.RESTART)}
                >
                  {busy && <Spinner />}
                  {d.restart}
                </Button>
              ) : (
                <Button variant="outline" size="sm" disabled={busy || !nodeOnline} onClick={() => control(ContainerAction.START)}>
                  {busy && <Spinner />}
                  {d.start}
                </Button>
              )}
              {running && (
                <Menu
                  title={service}
                  items={[
                    // 多个副本里有停了的：上面是重启，从这里启动其余的。
                    ...(stopped ? [{ label: d.start, onSelect: () => control(ContainerAction.START), disabled: busy || !nodeOnline }] : []),
                    // 设置了顶替的容器组不能停止其中的服务（顶替按整个容器算，见 docs/failover.md）。
                    {
                      label: d.stop,
                      hint: container.failover ? d.stopFailoverHint : undefined,
                      disabled: busy || !nodeOnline || !!container.failover,
                      onSelect: async () => {
                        if (await confirmAction(s.stopConfirm(service), { action: d.stop, danger: true })) control(ContainerAction.STOP);
                      },
                    },
                  ]}
                />
              )}
            </>
          )
        }
      />
      <Tabs
        className="mb-6 max-md:mb-4"
        value={view}
        onChange={onView}
        items={[
          { value: "overview", label: s.tabs.overview },
          { value: "logs", label: s.tabs.logs },
          ...(isViewer ? [] : [{ value: "terminal" as const, label: s.tabs.terminal }]),
          { value: "metrics", label: s.tabs.metrics },
        ]}
      />
      <Slide id={view} index={serviceTabs.indexOf(view)}>
        {hasOutput && <div className="mb-6">{output}</div>}
        {view === "overview" && <ServiceOverview project={project} container={container} service={service} list={list} offline={!nodeOnline} nodeName={node?.name ?? container.nodeId} />}
        {view === "logs" && (
          <Logs projectId={project.id} container={container.name} services={[service]} containers={list.map((x) => x.containerId).filter(Boolean).join(",")} fixed={service} />
        )}
        {view === "terminal" && !isViewer && <TerminalView projectId={project.id} container={container.name} services={runningNames} />}
        {view === "metrics" && <ServiceMetrics project={project} container={container} service={service} />}
      </Slide>
    </>
  );
}

function ServiceOverview({ project, container, service, list, offline, nodeName }: { project: Project; container: Container; service: string; list: ServiceStatus[]; offline: boolean; nodeName: string }) {
  const s = t.service;
  const o = t.projectContainers.overview;
  const images = [...new Set(list.map((x) => x.image).filter(Boolean))];
  const health = list.find((x) => x.health === "unhealthy")?.health ?? list.find((x) => x.health)?.health ?? "";
  // 重启次数：节点按服务记的累计值（多个副本加在一起），取最新的一个点。
  const metrics = useProjectMetrics(project.id, projectMetricNodes(project));
  const restartSeries = metrics.data?.find((m) => m.name === `c:${project.id}/${container.name}/${service}:restarts`);
  const restarts = restartSeries && restartSeries.values.length > 0 ? String(restartSeries.values[restartSeries.values.length - 1]) : "—";
  return (
    <div>
      <Section>
        <div className="grid grid-cols-[repeat(auto-fit,minmax(160px,1fr))] gap-x-6 gap-y-4 max-md:grid-cols-2">
          <KvItem
            mono
            k={s.image}
            v={images.length === 0 ? "—" : images.map((img) => <span key={img} className="block" title={img}>{displayImage(img)}</span>)}
          />
          <KvItem k={t.projectContainers.columns.node} v={nodeName} />
          <KvItem k={o.uptimeLabel} v={<Roll value={uptime(list)} />} />
          <KvItem k={s.restarts} v={<Roll value={restarts} />} />
          {health && <KvItem k={s.health} v={s.healthLabels[health] ?? health} />}
        </div>
      </Section>
      {list.length > 1 && (
        <Section title={s.replicas}>
          <List>
            {list.map((x) => (
              <Row
                key={x.containerId || x.name}
                title={<span className="font-mono text-[13px]">{x.containerId ? x.containerId.slice(0, 12) : service}</span>}
                sub={serviceDetail(x.status)}
                cols={[{ phone: true, content: offline ? <Status>{t.common.status.unknown}</Status> : <ServiceState service={x} /> }]}
              />
            ))}
          </List>
        </Section>
      )}
      <Section title={s.ports}>
        <ServiceRoutes project={project} container={container.name} service={service} />
      </Section>
    </div>
  );
}

function ServiceMetrics({ project, container, service }: { project: Project; container: Container; service: string }) {
  const { range, picker } = useRange();
  return (
    <Section title={t.service.resources} actions={picker}>
      <ContainerMetrics projectId={project.id} nodeIds={projectMetricNodes(project)} container={container.name} service={service} range={range} />
    </Section>
  );
}
