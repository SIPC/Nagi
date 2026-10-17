import { useState } from "react";
import { useMutation, useQuery } from "@tanstack/react-query";
import { Link, useNavigate } from "@tanstack/react-router";
import { FolderOpen, Plus, Server } from "lucide-react";
import { errorMessage, nodeClient, projectClient } from "@/lib/api";
import { lines, textToEnv, textToPorts } from "@/lib/config";
import { PageHeader } from "@/pages/layout";
import { ProjectState } from "@/components/status";
import { Alert, Button, Card, CardContent, Empty, Field, Input, Select, Spinner, Textarea } from "@/components/ui";

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
        description="每个项目是一份 compose，整体运行在一个节点上。"
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
        <Empty title="还没有项目">粘贴一份 compose，就能把它部署到节点上。</Empty>
      ) : (
        <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
          {projects.data.projects.map(({ nodeId, project: summary }) => {
            const p = summary?.project;
            if (!p) return null;
            const services = summary.status?.services ?? [];
            return (
              <Link key={`${nodeId}/${p.id}`} to="/projects/$nodeId/$projectId" params={{ nodeId, projectId: p.id }}>
                <Card className="h-full transition-colors hover:border-ring/60">
                  <CardContent className="grid gap-3 pt-5">
                    <div className="flex items-start justify-between gap-2">
                      <span className="truncate font-medium">{p.name}</span>
                      <ProjectState services={services} deployed={p.deployedVersion > 0n} />
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
                      {services.length} 个服务 · {p.deployedVersion > 0n ? `已部署版本 ${p.deployedVersion}` : "尚未部署"}
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

const exampleCompose = `services:
  web:
    image: nginx:alpine
    ports:
      - "8080:80"
`;

export function NewProjectPage() {
  const navigate = useNavigate();
  const nodes = useQuery({ queryKey: ["nodes"], queryFn: () => nodeClient.listNodes({}) });
  const onlineNodes = nodes.data?.nodes.filter((n) => n.online) ?? [];
  const [nodeId, setNodeId] = useState("");
  const [name, setName] = useState("");
  const [dir, setDir] = useState("");
  const [compose, setCompose] = useState(exampleCompose);
  const [mode, setMode] = useState<"compose" | "image">("compose");
  const [image, setImage] = useState("nginx:alpine");
  const [command, setCommand] = useState("");
  const [ports, setPorts] = useState("8080:80");
  const [volumes, setVolumes] = useState("");
  const [workdir, setWorkdir] = useState("");
  const [env, setEnv] = useState("");
  const selectedNode = nodeId || onlineNodes[0]?.id || "";

  const create = useMutation({
    mutationFn: () =>
      projectClient.createNodeProject({
        nodeId: selectedNode,
        name,
        dir,
        config:
          mode === "compose"
            ? { compose }
            : {
                form: {
                  image,
                  command,
                  ports: textToPorts(ports),
                  volumes: lines(volumes),
                  workdir,
                  env: textToEnv(env),
                },
              },
      }),
    onSuccess: (res) => {
      navigate({ to: "/projects/$nodeId/$projectId", params: { nodeId: selectedNode, projectId: res.project!.id } });
    },
  });

  return (
    <>
      <PageHeader title="新建项目" description="创建后不会立即部署，可以先检查一遍再点部署。" />
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
              hint="节点上的绝对路径，例如 /srv/myapp。目录需要不存在或为空。compose 里的相对路径以其中的 data/ 为准。"
            >
              <Input id="dir" value={dir} onChange={(e) => setDir(e.target.value)} placeholder="/srv/myapp" required className="font-mono" />
            </Field>
            <div className="flex gap-2">
              <Button type="button" size="sm" variant={mode === "compose" ? "default" : "outline"} onClick={() => setMode("compose")}>
                写 compose
              </Button>
              <Button type="button" size="sm" variant={mode === "image" ? "default" : "outline"} onClick={() => setMode("image")}>
                镜像表单
              </Button>
            </div>
            {mode === "compose" ? (
              <Field label="compose" htmlFor="compose">
                <Textarea
                  id="compose"
                  value={compose}
                  onChange={(e) => setCompose(e.target.value)}
                  spellCheck={false}
                  className="min-h-72 font-mono text-[13px] leading-relaxed"
                  required
                />
              </Field>
            ) : (
              <div className="grid gap-4 sm:grid-cols-2">
                <Field label="镜像" htmlFor="image">
                  <Input id="image" value={image} onChange={(e) => setImage(e.target.value)} required className="font-mono" />
                </Field>
                <Field label="运行命令" htmlFor="command" hint="为空时使用镜像默认命令">
                  <Input id="command" value={command} onChange={(e) => setCommand(e.target.value)} className="font-mono" />
                </Field>
                <Field label="端口" htmlFor="ports" hint="节点端口:容器端口，空格分隔，例如 8080:80 53:53/udp">
                  <Input id="ports" value={ports} onChange={(e) => setPorts(e.target.value)} className="font-mono" />
                </Field>
                <Field label="工作目录" htmlFor="workdir" hint="把项目的 data/ 挂到容器内这个路径；为空表示不挂载文件">
                  <Input id="workdir" value={workdir} onChange={(e) => setWorkdir(e.target.value)} placeholder="/app" className="font-mono" />
                </Field>
                <Field label="持久化目录" htmlFor="volumes" hint="容器内路径，每行一个">
                  <Textarea id="volumes" value={volumes} onChange={(e) => setVolumes(e.target.value)} className="font-mono" placeholder="/data" />
                </Field>
                <Field label="环境变量" htmlFor="env" hint="KEY=VALUE，每行一个">
                  <Textarea id="env" value={env} onChange={(e) => setEnv(e.target.value)} className="font-mono" />
                </Field>
              </div>
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
