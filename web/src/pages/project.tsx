import { useEffect } from "react";
import { useQuery } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { errorMessage, nodeClient, projectClient, toDate } from "@/lib/api";
import { timeAgo } from "@/lib/utils";
import { Breadcrumb, PageHeader } from "@/pages/layout";
import { ProjectState } from "@/components/status";
import { Alert, Badge, Loading, Notice, Tabs } from "@/components/ui";
import { ContainersTab } from "@/pages/project-containers";
import { NetworkTab } from "@/pages/project-network";
import { DataTab } from "@/pages/project-data";
import { SettingsTab } from "@/pages/project-settings";
import type { Container } from "@/gen/nagi/v1/common_pb";

// 项目页：项目由多个容器组成，每个容器单独部署、记版本、回滚（见 docs/agent.md#项目与容器）。
// 内容分四个标签页：容器、网络、数据、设置；当前标签页记在地址的 tab 参数里。

export const projectTabs = ["containers", "network", "data", "settings"] as const;
export type ProjectTab = (typeof projectTabs)[number];

export function ProjectPage({ nodeId, projectId, tab, onTab }: { nodeId: string; projectId: string; tab: ProjectTab; onTab: (t: ProjectTab) => void }) {
  const project = useQuery({
    queryKey: ["project", nodeId, projectId],
    queryFn: () => projectClient.getNodeProject({ nodeId, projectId }),
    refetchInterval: 5000,
  });
  const nodes = useQuery({ queryKey: ["nodes"], queryFn: () => nodeClient.listNodes({}) });
  const nodeName = nodes.data?.nodes.find((n) => n.id === nodeId)?.name;

  if (project.isPending) return <Loading />;
  if (project.error) {
    return (
      <>
        <Breadcrumb items={[{ label: "项目", to: "/projects" }, { label: projectId }]} />
        <div className="grid gap-4">
          <Alert>{errorMessage(project.error)}</Alert>
          <MovedProject nodeId={nodeId} projectId={projectId} />
        </div>
      </>
    );
  }
  const p = project.data.project?.project;
  const services = project.data.project?.status?.services ?? [];
  const imageUpdates = project.data.project?.status?.imageUpdates ?? [];
  if (!p) return null;
  const readOnly = project.data.project?.readOnlyReason;

  return (
    <>
      <Breadcrumb items={[{ label: "项目", to: "/projects" }, { label: p.name }]} />
      <PageHeader
        title={
          <>
            {p.name}
            <ProjectState services={services} deployed={p.containers.some((m) => m.deployedVersion > 0n)} />
            {p.stateful && <Badge>有状态</Badge>}
          </>
        }
        meta={
          <>
            <span>节点 {nodeName ?? nodeId}</span>
            <span className="font-mono text-xs">{p.dir}</span>
            <span>{countContainers(p.containers)}</span>
            <span>创建于 {timeAgo(toDate(p.createdAt))}</span>
          </>
        }
      />
      {readOnly && (
        <Notice tone="warning" title="项目处于只读状态" className="mb-6">
          {readOnly}。容器照常运行，可查看状态、日志和终端；修改配置、部署、快照等写入操作将失败。
        </Notice>
      )}
      <Tabs
        className="mb-6"
        value={tab}
        onChange={onTab}
        items={[
          { value: "containers", label: "容器", count: p.containers.length },
          { value: "network", label: "网络", count: p.routes.length },
          { value: "data", label: "数据" },
          { value: "settings", label: "设置" },
        ]}
      />
      {tab === "containers" && <ContainersTab nodeId={nodeId} project={p} services={services} imageUpdates={imageUpdates} />}
      {tab === "network" && <NetworkTab nodeId={nodeId} project={p} services={services} />}
      {tab === "data" && <DataTab nodeId={nodeId} project={p} trusted={project.data.replicaTrusted} />}
      {tab === "settings" && <SettingsTab nodeId={nodeId} project={p} />}
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
  if (all.isPending) return <Loading />;
  return found ? <Notice>项目已位于其他节点，正在跳转…</Notice> : null;
}

// countContainers 按种类数，例如"2 个容器 · 1 个容器组"。
function countContainers(cs: Container[]) {
  if (cs.length === 0) return "暂无容器";
  const groups = cs.filter((c) => c.kind === "group").length;
  const singles = cs.length - groups;
  return [singles > 0 && `${singles} 个容器`, groups > 0 && `${groups} 个容器组`].filter(Boolean).join("，");
}
