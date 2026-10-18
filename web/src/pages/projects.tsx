import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate } from "@tanstack/react-router";
import { FolderOpen, Plus, Server } from "lucide-react";
import { authClient, errorMessage, nodeClient, projectClient, settingsClient } from "@/lib/api";
import { PageHeader } from "@/pages/layout";
import { ProjectState } from "@/components/status";
import { useContainerForm } from "@/components/container-form";
import { SharedNetworkWarning } from "@/pages/backup";
import {
  Alert,
  Badge,
  Button,
  Card,
  CardContent,
  CardHeader,
  CardTitle,
  Empty,
  Field,
  Input,
  Select,
  Spinner,
} from "@/components/ui";

// Reminders：首页提醒开启两步验证、单独备份面板主密钥。可以"不再提醒"（只记在这个浏览器里）。
function Reminders() {
  const me = useQuery({ queryKey: ["me"], queryFn: () => authClient.me({}) });
  const settings = useQuery({ queryKey: ["settings"], queryFn: () => settingsClient.getSettings({}) });
  const read = (k: string) => {
    try {
      return localStorage.getItem(k) === "1";
    } catch {
      return false;
    }
  };
  const [hidden, setHidden] = useState({ totp: read("nagi.hide.totp"), key: read("nagi.hide.masterkey") });
  const hide = (k: "totp" | "key", storageKey: string) => {
    try {
      localStorage.setItem(storageKey, "1");
    } catch {
      // 存不了就只在这次隐藏。
    }
    setHidden({ ...hidden, [k]: true });
  };
  const items: { key: "totp" | "key"; storage: string; text: React.ReactNode }[] = [];
  if (me.data && !me.data.totpEnabled && !hidden.totp) {
    items.push({
      key: "totp",
      storage: "nagi.hide.totp",
      text: (
        <>
          还没有开启两步验证。建议在<Link to="/settings" className="mx-1 underline">设置的"账号安全"</Link>里开启。
        </>
      ),
    });
  }
  const path = settings.data?.settings?.masterKeyPath;
  if (path && !hidden.key) {
    items.push({
      key: "key",
      storage: "nagi.hide.masterkey",
      text: (
        <>
          请单独备份面板主密钥 <span className="font-mono">{path}</span>：面板数据库里的密钥都用它加密，丢了就解不开。
        </>
      ),
    });
  }
  if (items.length === 0) return null;
  return (
    <div className="mb-4 grid gap-2">
      {items.map((it) => (
        <div key={it.key} className="flex flex-wrap items-center gap-2 rounded-md border border-warning/40 bg-warning/15 px-3 py-2 text-sm">
          <span className="flex-1">{it.text}</span>
          <Button size="sm" variant="ghost" onClick={() => hide(it.key, it.storage)}>
            不再提醒
          </Button>
        </div>
      ))}
    </div>
  );
}

export function ProjectsPage() {
  const projects = useQuery({
    queryKey: ["projects"],
    queryFn: () => projectClient.listAllProjects({}),
    refetchInterval: 5000,
  });
  const nodes = useQuery({ queryKey: ["nodes"], queryFn: () => nodeClient.listNodes({}) });
  const nodeName = (id: string) => nodes.data?.nodes.find((n) => n.id === id)?.name ?? id;
  const offline = (id: string) => nodes.data?.nodes.find((n) => n.id === id)?.online === false;
  const [rebuild, setRebuild] = useState<{ nodeId: string; projectId: string; name: string; dir: string; sharedNetworks: string[] } | null>(null);

  return (
    <>
      <PageHeader
        title="项目"
        description="项目是一组一起工作的容器（前端、后端、数据库……），整体运行在一个节点上。"
        actions={
          <Link to="/projects/new">
            <Button>
              <Plus />
              新建项目
            </Button>
          </Link>
        }
      />
      <Reminders />
      {rebuild && <RebuildCard {...rebuild} onClose={() => setRebuild(null)} />}
      {projects.isPending ? (
        <Spinner />
      ) : projects.error ? (
        <Alert>{errorMessage(projects.error)}</Alert>
      ) : projects.data.projects.length === 0 ? (
        <Empty title="还没有项目">新建一个项目，往里加容器（单容器或 compose），就能部署到节点上。</Empty>
      ) : (
        <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
          {projects.data.projects.map(({ nodeId, project: summary }) => {
            const p = summary?.project;
            if (!p) return null;
            const services = summary.status?.services ?? [];
            const deployed = p.containers.filter((m) => m.deployedVersion > 0n).length;
            if (offline(nodeId)) {
              // 节点离线：显示的是面板上的配置备份，项目页打不开；可以用备份在别的节点上重建。
              return (
                <Card key={`${nodeId}/${p.id}`} className="h-full border-dashed">
                  <CardContent className="grid gap-3 pt-5">
                    <div className="flex items-start justify-between gap-2">
                      <span className="truncate font-medium">{p.name}</span>
                      <Badge tone="warning">节点离线</Badge>
                    </div>
                    <div className="grid gap-1 text-xs text-muted-foreground">
                      <span className="flex items-center gap-1.5">
                        <Server className="size-3.5" />
                        {nodeName(nodeId)}
                      </span>
                      <span>{p.containers.length} 个容器 · 显示的是面板上的配置备份</span>
                    </div>
                    <div>
                      <Button size="sm" variant="outline" onClick={() => setRebuild({ nodeId, projectId: p.id, name: p.name, dir: p.dir, sharedNetworks: p.settings?.sharedNetworks ?? [] })}>
                        用配置备份重建
                      </Button>
                    </div>
                  </CardContent>
                </Card>
              );
            }
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
                      {p.containers.length} 个容器和容器组 · {deployed > 0 ? `已部署 ${deployed} 个` : "尚未部署"}
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

// RebuildCard：用面板上的配置备份，把离线节点上的项目重建到另一个节点（新项目 ID）。
function RebuildCard({
  nodeId,
  projectId,
  name,
  dir: defaultDir,
  sharedNetworks,
  onClose,
}: {
  nodeId: string;
  projectId: string;
  name: string;
  dir: string;
  sharedNetworks: string[];
  onClose: () => void;
}) {
  const queryClient = useQueryClient();
  const nodes = useQuery({ queryKey: ["nodes"], queryFn: () => nodeClient.listNodes({}) });
  const navigate = useNavigate();
  const [target, setTarget] = useState("");
  const [dir, setDir] = useState(defaultDir);
  const [lines, setLines] = useState<string[]>([]);
  const [result, setResult] = useState("");
  const [created, setCreated] = useState<{ nodeId: string; projectId: string } | null>(null);
  const [busy, setBusy] = useState(false);

  const run = async () => {
    setBusy(true);
    setLines([]);
    setResult("");
    setCreated(null);
    try {
      for await (const ev of projectClient.rebuildNodeProject({ nodeId, projectId, targetNodeId: target, dir })) {
        if (ev.event.case === "output") {
          const l = ev.event.value;
          setLines((x) => [...x, l]);
        } else if (ev.event.case === "result") {
          const r = ev.event.value;
          if (r.projectId) setCreated({ nodeId: target, projectId: r.projectId });
          setResult(r.success ? "✅ 重建完成" : `❌ ${r.error}`);
        }
      }
    } catch (err) {
      setResult(`❌ ${errorMessage(err)}`);
    } finally {
      setBusy(false);
      queryClient.invalidateQueries({ queryKey: ["projects"] });
    }
  };

  const others = nodes.data?.nodes.filter((n) => n.id !== nodeId && n.online) ?? [];
  return (
    <Card className="mb-4">
      <CardHeader>
        <CardTitle>用配置备份重建 {name}</CardTitle>
      </CardHeader>
      <CardContent className="grid gap-3 text-sm">
        <p className="text-muted-foreground">
          在另一个节点上建一个新项目（新的项目 ID），带上各容器最新的配置、草稿、项目设置和端口映射。
          项目文件、运行数据和历史版本不在备份里。deploy key 和 webhook 地址都是新的。
          从 git 拉文件的、不用 data/ 文件的容器会自动部署，其余的要先上传文件。原节点修好回来时，两个项目互不干扰。
        </p>
        <div className="flex flex-wrap items-center gap-2">
          <Select className="w-48" value={target} onChange={(e) => setTarget(e.target.value)} disabled={busy}>
            <option value="">选择目标节点</option>
            {others.map((n) => (
              <option key={n.id} value={n.id}>
                {n.name}
              </option>
            ))}
          </Select>
          <Input className="w-72 font-mono" value={dir} onChange={(e) => setDir(e.target.value)} disabled={busy} />
          <Button size="sm" disabled={busy || !target} onClick={run}>
            {busy ? <Spinner /> : null}重建
          </Button>
          <Button size="sm" variant="ghost" disabled={busy} onClick={onClose}>
            关闭
          </Button>
        </div>
        <SharedNetworkWarning fromNodeId={nodeId} targetNodeId={target} networks={sharedNetworks} />
        {result && <div>{result}</div>}
        {created && (
          <div>
            <Button size="sm" variant="outline" onClick={() => navigate({ to: "/projects/$nodeId/$projectId", params: created })}>
              打开新项目
            </Button>
          </div>
        )}
        {lines.length > 0 && (
          <pre className="max-h-60 overflow-auto rounded bg-zinc-950 p-3 font-mono text-xs text-zinc-200">{lines.join("\n")}</pre>
        )}
      </CardContent>
    </Card>
  );
}

export function NewProjectPage() {
  const navigate = useNavigate();
  const nodes = useQuery({ queryKey: ["nodes"], queryFn: () => nodeClient.listNodes({}) });
  const onlineNodes = nodes.data?.nodes.filter((n) => n.online) ?? [];
  const [nodeId, setNodeId] = useState("");
  const [name, setName] = useState("");
  const [dir, setDir] = useState("");
  const [withContainer, setWithContainer] = useState(true);
  const [containerName, setContainerName] = useState("web");
  const container = useContainerForm(true);
  const selectedNode = nodeId || onlineNodes[0]?.id || "";

  const create = useMutation({
    mutationFn: () =>
      projectClient.createNodeProject({
        nodeId: selectedNode,
        name,
        dir,
        ...(withContainer ? { containerName, container: container.build() } : {}),
      }),
    onSuccess: (res) => {
      navigate({ to: "/projects/$nodeId/$projectId", params: { nodeId: selectedNode, projectId: res.project!.id } });
    },
  });

  return (
    <>
      <PageHeader title="新建项目" description="创建后不会立即部署，可以先检查一遍再点部署。之后可以继续往项目里加容器。" />
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
              hint="节点上的绝对路径，例如 /srv/myapp。目录需要不存在或为空。每个容器在其中的 containers/<容器名>/ 下，compose 里的相对路径以容器的 data/ 为准。"
            >
              <Input id="dir" value={dir} onChange={(e) => setDir(e.target.value)} placeholder="/srv/myapp" required className="font-mono" />
            </Field>
            <label className="flex items-center gap-2 text-sm">
              <input type="checkbox" checked={withContainer} onChange={(e) => setWithContainer(e.target.checked)} />
              同时加入第一个容器
            </label>
            {withContainer && (
              <>
                <Field label="容器名" htmlFor="container" hint="小写字母、数字和 -，创建后不能改。单个容器在项目内网里就叫这个名字；容器组里的容器叫“容器.容器组名”。">
                  <Input id="container" value={containerName} onChange={(e) => setContainerName(e.target.value)} required className="font-mono" />
                </Field>
                {container.fields}
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
