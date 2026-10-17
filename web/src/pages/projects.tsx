import { useState } from "react";
import { useMutation, useQuery } from "@tanstack/react-query";
import { Link, useNavigate } from "@tanstack/react-router";
import { FolderOpen, Plus, Server } from "lucide-react";
import { errorMessage, nodeClient, projectClient } from "@/lib/api";
import { PageHeader } from "@/pages/layout";
import { ProjectState } from "@/components/status";
import { useMemberForm } from "@/components/member-form";
import { Alert, Button, Card, CardContent, Empty, Field, Input, Select, Spinner } from "@/components/ui";

export function ProjectsPage() {
  const projects = useQuery({
    queryKey: ["projects"],
    queryFn: () => projectClient.listAllProjects({}),
    refetchInterval: 5000,
  });
  const nodes = useQuery({ queryKey: ["nodes"], queryFn: () => nodeClient.listNodes({}) });
  const nodeName = (id: string) => nodes.data?.nodes.find((n) => n.id === id)?.name ?? id;

  return (
    <>
      <PageHeader
        title="项目"
        description="项目是一组一起工作的成员（前端、后端、数据库……），整体运行在一个节点上。"
        actions={
          <Link to="/projects/new">
            <Button>
              <Plus />
              新建项目
            </Button>
          </Link>
        }
      />
      {projects.isPending ? (
        <Spinner />
      ) : projects.error ? (
        <Alert>{errorMessage(projects.error)}</Alert>
      ) : projects.data.projects.length === 0 ? (
        <Empty title="还没有项目">新建一个项目，往里加成员（单容器或 compose），就能部署到节点上。</Empty>
      ) : (
        <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
          {projects.data.projects.map(({ nodeId, project: summary }) => {
            const p = summary?.project;
            if (!p) return null;
            const services = summary.status?.services ?? [];
            const deployed = p.members.filter((m) => m.deployedVersion > 0n).length;
            return (
              <Link key={`${nodeId}/${p.id}`} to="/projects/$nodeId/$projectId" params={{ nodeId, projectId: p.id }}>
                <Card className="h-full transition-colors hover:border-ring/60">
                  <CardContent className="grid gap-3 pt-5">
                    <div className="flex items-start justify-between gap-2">
                      <span className="truncate font-medium">{p.name}</span>
                      <ProjectState services={services} deployed={deployed > 0} />
                    </div>
                    <div className="grid gap-1 text-xs text-muted-foreground">
                      <span className="flex items-center gap-1.5">
                        <Server className="size-3.5" />
                        {nodeName(nodeId)}
                      </span>
                      <span className="flex items-center gap-1.5 truncate">
                        <FolderOpen className="size-3.5 shrink-0" />
                        <span className="truncate">{p.dir}</span>
                      </span>
                    </div>
                    <div className="text-xs text-muted-foreground">
                      {p.members.length} 个成员 · {services.length} 个服务 · {deployed > 0 ? `已部署 ${deployed} 个成员` : "尚未部署"}
                    </div>
                  </CardContent>
                </Card>
              </Link>
            );
          })}
        </div>
      )}
    </>
  );
}

export function NewProjectPage() {
  const navigate = useNavigate();
  const nodes = useQuery({ queryKey: ["nodes"], queryFn: () => nodeClient.listNodes({}) });
  const onlineNodes = nodes.data?.nodes.filter((n) => n.online) ?? [];
  const [nodeId, setNodeId] = useState("");
  const [name, setName] = useState("");
  const [dir, setDir] = useState("");
  const [withMember, setWithMember] = useState(true);
  const [memberName, setMemberName] = useState("web");
  const member = useMemberForm();
  const selectedNode = nodeId || onlineNodes[0]?.id || "";

  const create = useMutation({
    mutationFn: () =>
      projectClient.createNodeProject({
        nodeId: selectedNode,
        name,
        dir,
        ...(withMember ? { memberName, member: member.build() } : {}),
      }),
    onSuccess: (res) => {
      navigate({ to: "/projects/$nodeId/$projectId", params: { nodeId: selectedNode, projectId: res.project!.id } });
    },
  });

  return (
    <>
      <PageHeader title="新建项目" description="创建后不会立即部署，可以先检查一遍再点部署。之后可以继续往项目里加成员。" />
      <Card className="max-w-3xl">
        <CardContent className="pt-5">
          <form
            className="grid gap-5"
            onSubmit={(e) => {
              e.preventDefault();
              create.mutate();
            }}
          >
            <div className="grid gap-5 sm:grid-cols-2">
              <Field label="名称" htmlFor="name">
                <Input id="name" value={name} onChange={(e) => setName(e.target.value)} placeholder="我的应用" required maxLength={64} />
              </Field>
              <Field label="节点" htmlFor="node">
                <Select id="node" value={selectedNode} onChange={(e) => setNodeId(e.target.value)} required>
                  {onlineNodes.length === 0 && <option value="">没有在线的节点</option>}
                  {onlineNodes.map((n) => (
                    <option key={n.id} value={n.id}>
                      {n.name}
                    </option>
                  ))}
                </Select>
              </Field>
            </div>
            <Field
              label="项目目录"
              htmlFor="dir"
              hint="节点上的绝对路径，例如 /srv/myapp。目录需要不存在或为空。每个成员在其中的 members/<成员>/ 下，compose 里的相对路径以成员的 data/ 为准。"
            >
              <Input id="dir" value={dir} onChange={(e) => setDir(e.target.value)} placeholder="/srv/myapp" required className="font-mono" />
            </Field>
            <label className="flex items-center gap-2 text-sm">
              <input type="checkbox" checked={withMember} onChange={(e) => setWithMember(e.target.checked)} />
              同时加入第一个成员
            </label>
            {withMember && (
              <>
                <Field label="成员名" htmlFor="member" hint="小写字母、数字和 -，创建后不能改。项目内网里其他成员用这个名字访问它。">
                  <Input id="member" value={memberName} onChange={(e) => setMemberName(e.target.value)} required className="font-mono" />
                </Field>
                {member.fields}
              </>
            )}
            {create.error && <Alert>{errorMessage(create.error)}</Alert>}
            <div className="flex justify-end gap-2">
              <Link to="/projects">
                <Button type="button" variant="outline">
                  取消
                </Button>
              </Link>
              <Button type="submit" disabled={create.isPending || !selectedNode}>
                {create.isPending && <Spinner />}
                创建项目
              </Button>
            </div>
          </form>
        </CardContent>
      </Card>
    </>
  );
}
