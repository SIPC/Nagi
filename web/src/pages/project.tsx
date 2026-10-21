import { useQuery } from "@tanstack/react-query";
import { errorMessage, nodeClient, projectClient, toDate } from "@/lib/api";
import { timeAgo } from "@/lib/utils";
import { Breadcrumb, PageHeader } from "@/pages/layout";
import { ProjectState } from "@/components/status";
import { Alert, Badge, Loading, Tabs } from "@/components/ui";
import { ContainersTab } from "@/pages/project-containers";
import { NetworkTab } from "@/pages/project-network";
import { SettingsTab } from "@/pages/project-settings";
import type { Container } from "@/gen/nagi/v1/common_pb";

// 项目页：项目由多个容器组成，每个容器指定一个节点运行，各自部署、记版本、回滚（见 docs/agent.md#项目与容器）。
// 内容分三个标签页：容器、网络、设置；当前标签页记在地址的 tab 参数里。每个容器有自己的页（快照、顶替、迁移在容器页的「数据」里）。

export const projectTabs = ["containers", "network", "settings"] as const;
export type ProjectTab = (typeof projectTabs)[number];

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
        <Breadcrumb items={[{ label: "项目", to: "/projects" }, { label: projectId }]} />
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
            <span className="font-mono text-xs">{p.networkName}</span>
            <span>{nodeIds.length > 0 ? `节点 ${nodeIds.map(nodeName).join("、")}` : "暂无容器"}</span>
            <span>{countContainers(p.containers)}</span>
            {p.rebuiltAt ? (
              <span title="面板丢失或更换后，从节点缓存拼回的项目：原来的创建时间、旧版本和部署记录不在了">从节点缓存拼回于 {timeAgo(toDate(p.rebuiltAt))}</span>
            ) : (
              <span>创建于 {timeAgo(toDate(p.createdAt))}</span>
            )}
          </>
        }
      />
      <Tabs
        className="mb-6"
        value={tab}
        onChange={onTab}
        items={[
          { value: "containers", label: "容器", count: p.containers.length },
          { value: "network", label: "网络", count: p.routes.length },
          { value: "settings", label: "设置" },
        ]}
      />
      {tab === "containers" && <ContainersTab project={p} services={services} imageUpdates={imageUpdates} />}
      {tab === "network" && <NetworkTab project={p} services={services} />}
      {tab === "settings" && <SettingsTab project={p} />}
    </>
  );
}

// countContainers 按种类数，例如"2 个容器，1 个容器组"。
function countContainers(cs: Container[]) {
  if (cs.length === 0) return "暂无容器";
  const groups = cs.filter((c) => c.kind === "group").length;
  const singles = cs.length - groups;
  return [singles > 0 && `${singles} 个容器`, groups > 0 && `${groups} 个容器组`].filter(Boolean).join("，");
}
