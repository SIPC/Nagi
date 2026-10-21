import { useState } from "react";
import { useMutation, useQuery } from "@tanstack/react-query";
import { Link, useNavigate } from "@tanstack/react-router";
import { FolderKanban, Plus } from "lucide-react";
import { authClient, errorMessage, nodeClient, projectClient, settingsClient, toDate } from "@/lib/api";
import { Breadcrumb, PageHeader } from "@/pages/layout";
import { slugName } from "@/lib/utils";
import { ProjectState, stateOf } from "@/components/status";
import { cn } from "@/lib/utils";
import { useContainerForm } from "@/components/container-form";
import {
  Alert,
  Badge,
  Button,
  Check,
  Empty,
  Field,
  Input,
  Loading,
  Notice,
  Section,
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
  const items: { key: "totp" | "key"; storage: string; title: string; text: React.ReactNode }[] = [];
  if (me.data && !me.data.totpEnabled && !hidden.totp) {
    items.push({
      key: "totp",
      storage: "nagi.hide.totp",
      title: "未开启两步验证",
      text: (
        <>
          建议在
          <Link to="/settings" className="mx-1 text-foreground underline underline-offset-2">
            设置 › 账号安全
          </Link>
          中开启。
        </>
      ),
    });
  }
  const path = settings.data?.settings?.masterKeyPath;
  if (path && !hidden.key) {
    items.push({
      key: "key",
      storage: "nagi.hide.masterkey",
      title: "备份面板主密钥",
      text: (
        <>
          面板数据库中的密钥均由主密钥加密，主密钥丢失后无法解密。请单独备份 <span className="font-mono text-foreground">{path}</span>。
        </>
      ),
    });
  }
  if (items.length === 0) return null;
  return (
    <div className="grid gap-2">
      {items.map((it) => (
        <Notice
          key={it.key}
          tone="warning"
          title={it.title}
          actions={
            <Button size="xs" variant="ghost" onClick={() => hide(it.key, it.storage)}>
              不再提醒
            </Button>
          }
        >
          {it.text}
        </Notice>
      ))}
    </div>
  );
}

function formatDate(d: Date | undefined) {
  if (!d) return "—";
  return d.toLocaleDateString("zh-CN", { year: "numeric", month: "2-digit", day: "2-digit" });
}

const dot = {
  success: "bg-success",
  warning: "bg-warning",
  danger: "bg-destructive",
  neutral: "bg-muted-foreground/50",
};

export function ProjectsPage() {
  const projects = useQuery({
    queryKey: ["projects"],
    queryFn: () => projectClient.listProjects({}),
    refetchInterval: 5000,
  });
  const nodes = useQuery({ queryKey: ["nodes"], queryFn: () => nodeClient.listNodes({}) });
  const nodeName = (id: string) => nodes.data?.nodes.find((n) => n.id === id)?.name ?? id;
  const offline = (id: string) => nodes.data?.nodes.find((n) => n.id === id)?.online === false;

  const newButton = (
    <Link to="/projects/new">
      <Button>
        <Plus />
        新建项目
      </Button>
    </Link>
  );

  return (
    <>
      <PageHeader title="项目" description="项目是一组协同工作的容器，容器可以分布在不同节点上，互相按名称访问。" actions={newButton} />
      <div className="grid gap-6">
        <Reminders />
        {projects.isPending ? (
          <Loading />
        ) : projects.error ? (
          <Alert>{errorMessage(projects.error)}</Alert>
        ) : projects.data.projects.length === 0 ? (
          <Empty title="暂无项目" icon={<FolderKanban className="size-5" />}>
            新建项目后，可添加容器或容器组（一份 compose），并为每个容器选择运行的节点。
          </Empty>
        ) : (
          <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-4">
            {projects.data.projects.map((summary) => {
              const p = summary.project;
              if (!p) return null;
              const services = summary.status?.services ?? [];
              const deployed = p.containers.some((m) => m.deployedVersion > 0n);
              return (
                <Link
                  key={p.id}
                  to="/projects/$projectId"
                  params={{ projectId: p.id }}
                  className="group flex flex-col rounded-lg border bg-card transition-colors hover:border-foreground/25"
                >
                  <div className="flex items-start justify-between gap-2 px-3.5 pt-3">
                    <div className="min-w-0">
                      <div className="truncate text-[13px] font-medium group-hover:underline">{p.name}</div>
                      <div className="mt-0.5 flex items-center gap-2">
                        <span className="truncate font-mono text-xs text-muted-foreground">{p.networkName}</span>
                        {p.stateful && <Badge>有状态</Badge>}
                      </div>
                    </div>
                    <ProjectState services={services} deployed={deployed} />
                  </div>
                  <div className="grid flex-1 content-start gap-1 px-3.5 py-2.5">
                    {p.containers.length === 0 ? (
                      <span className="text-[13px] text-muted-foreground">暂无容器</span>
                    ) : (
                      p.containers.map((c) => {
                        const st = offline(c.nodeId)
                          ? { tone: "warning" as const, label: "节点离线" }
                          : stateOf(
                              services.filter((x) => x.container === c.name),
                              c.deployedVersion > 0n,
                            );
                        const tookOver = c.failover && c.failover.a?.nodeId !== c.nodeId;
                        return (
                          <div key={c.name} className="flex items-center gap-2 text-[13px]" title={st.label}>
                            <span className={cn("size-2 shrink-0 rounded-full", dot[st.tone])} />
                            <span className="min-w-0 flex-1 truncate">{c.name}</span>
                            {c.failover && <Badge tone={tookOver ? "warning" : "neutral"}>{tookOver ? "已顶替" : "顶替"}</Badge>}
                            <span className="shrink-0 text-xs text-muted-foreground">{nodeName(c.nodeId)}</span>
                          </div>
                        );
                      })
                    )}
                  </div>
                  <div className="flex items-center justify-between gap-2 border-t px-3.5 py-2 text-xs text-muted-foreground">
                    <span>
                      {p.containers.length} 个容器{p.routes.length > 0 && ` · ${p.routes.length} 个端口映射`}
                    </span>
                    <span className="tabular-nums">
                      {p.rebuiltAt ? `拼回于 ${formatDate(toDate(p.rebuiltAt))}` : `创建于 ${formatDate(toDate(p.createdAt))}`}
                    </span>
                  </div>
                </Link>
              );
            })}
          </div>
        )}
      </div>
    </>
  );
}

// useNodeChoice：选择容器运行的节点和容器目录（留空时显示默认值）。新建项目和添加容器共用。
// useDefaultDir 返回容器在节点上的默认目录：<节点的项目根目录>/<项目名的拼音>/<容器名>（见 docs/agent.md#容器目录）。
//   - 项目在这个节点上已经有容器时，沿用它们所在的项目文件夹；
//   - 项目文件夹被别的项目用了（例如两个项目重名），在后面加 -2、-3……
// 留空提交时就用这个路径（界面上看到的就是实际用的）。
export function useDefaultDir(nodeId: string, projectId: string, projectName: string, container: string) {
  const nodes = useQuery({ queryKey: ["nodes"], queryFn: () => nodeClient.listNodes({}) });
  const projects = useQuery({ queryKey: ["projects"], queryFn: () => projectClient.listProjects({}) });
  const root = nodes.data?.nodes.find((n) => n.id === nodeId)?.info?.projectsRoot || "/srv/nagi";
  const others: { projectId: string; dir: string }[] = [];
  for (const sum of projects.data?.projects ?? []) {
    for (const c of sum.project?.containers ?? []) {
      if (c.nodeId === nodeId) others.push({ projectId: sum.project!.id, dir: c.dir });
    }
  }
  const parentOf = (d: string) => d.slice(0, d.lastIndexOf("/"));
  const mine = others.find((o) => o.projectId === projectId && parentOf(parentOf(o.dir)) === root);
  if (projectId && mine) return `${parentOf(mine.dir)}/${container}`;
  const base = slugName(projectName, "project");
  const taken = (folder: string) => others.some((o) => o.projectId !== projectId && (o.dir === folder || o.dir.startsWith(folder + "/")));
  let folder = `${root}/${base}`;
  for (let i = 2; taken(folder); i++) folder = `${root}/${base}-${i}`;
  return `${folder}/${container}`;
}

// useNodeChoice：选择容器运行的节点和容器目录。目录的占位文字是默认目录，随节点、项目名、容器名实时变化；
// 不填时提交的就是这个默认目录。新建项目和添加容器共用。
// preferNodes：项目已有容器所在的节点，默认选其中容器最多的那个（在线时）。
export function useNodeChoice(args: { projectId?: string; projectName: string; container: string; preferNodes?: string[] }) {
  const nodes = useQuery({ queryKey: ["nodes"], queryFn: () => nodeClient.listNodes({}) });
  const onlineNodes = nodes.data?.nodes.filter((n) => n.online && !n.pending) ?? [];
  const [nodeId, setNodeId] = useState("");
  const [dir, setDir] = useState("");
  const counts = new Map<string, number>();
  for (const id of args.preferNodes ?? []) counts.set(id, (counts.get(id) ?? 0) + 1);
  const preferred = onlineNodes
    .filter((n) => counts.has(n.id))
    .sort((a, b) => (counts.get(b.id) ?? 0) - (counts.get(a.id) ?? 0))[0]?.id;
  const selected = nodeId || preferred || onlineNodes[0]?.id || "";
  const defaultDir = useDefaultDir(selected, args.projectId ?? "", args.projectName, args.container);
  const fields = (
    <div className="grid gap-4 sm:grid-cols-2">
      <Field label="节点" htmlFor="container-node" help="容器运行在这个节点上，之后可以迁移到其他节点。">
        <Select id="container-node" value={selected} onChange={(e) => setNodeId(e.target.value)} required>
          {onlineNodes.length === 0 && <option value="">无在线节点</option>}
          {onlineNodes.map((n) => (
            <option key={n.id} value={n.id}>
              {n.name}
            </option>
          ))}
        </Select>
      </Field>
      <Field
        label="容器目录"
        htmlFor="container-dir"
        help="容器的运行文件、数据和快照都在这个目录里，可以填任意绝对路径，须不存在或为空。不填则使用默认目录：节点的项目根目录下「项目名/容器名」，项目名转成拼音，重名时加 -2。"
      >
        <Input id="container-dir" value={dir} onChange={(e) => setDir(e.target.value)} placeholder={defaultDir} className="font-mono" />
      </Field>
    </div>
  );
  return { fields, nodeId: selected, dir: dir.trim() || defaultDir };
}

export function NewProjectPage() {
  const navigate = useNavigate();
  const [name, setName] = useState("");
  const [network, setNetwork] = useState("");
  const [withContainer, setWithContainer] = useState(true);
  const [containerName, setContainerName] = useState("");
  const container = useContainerForm(true);
  const node = useNodeChoice({ projectName: name || "shop", container: containerName || "web" });

  const create = useMutation({
    mutationFn: () =>
      projectClient.createProject({
        name,
        networkName: network.trim() || slugName(name, ""),
        ...(withContainer ? { containerName, container: container.build(), nodeId: node.nodeId, dir: node.dir } : {}),
      }),
    onSuccess: (p) => {
      navigate({ to: "/projects/$projectId", params: { projectId: p.id } });
    },
  });

  return (
    <div className="max-w-3xl">
      <Breadcrumb items={[{ label: "项目", to: "/projects" }, { label: "新建项目" }]} />
      <PageHeader title="新建项目" description="创建后不会立即部署，检查配置后可手动部署。" />
      <form
        className="grid gap-6"
        onSubmit={(e) => {
          e.preventDefault();
          create.mutate();
        }}
      >
        <Section title="基本信息">
          <div className="grid gap-4 sm:grid-cols-2">
            <Field label="名称" htmlFor="name">
              <Input id="name" value={name} onChange={(e) => setName(e.target.value)} placeholder="我的应用" required maxLength={64} />
            </Field>
            <Field
              label="网络名"
              htmlFor="network"
              hint="小写字母、数字和 -；留空则由名称生成"
              help="在面板上唯一。连接本项目的项目通过「容器.网络名」访问本项目的容器。留空时由项目名生成，汉字转成拼音。"
            >
              <Input
                id="network"
                value={network}
                onChange={(e) => setNetwork(e.target.value)}
                placeholder={slugName(name, "shop")}
                className="font-mono"
              />
            </Field>
          </div>
        </Section>

        <Section
          title="第一个容器"
          description="可选，创建项目后也可继续添加容器。"
          actions={
            <Check checked={withContainer} onChange={setWithContainer}>
              同时创建
            </Check>
          }
        >
          {withContainer && (
            <div className="grid gap-4">
              <Field
                label={container.mode === "compose" ? "容器组名称" : "容器名称"}
                htmlFor="container"
                hint="小写字母、数字和 -"
                help="创建后不可修改。单个容器在项目内网中使用此名称；容器组内的容器名称为「容器.容器组名」。"
              >
                <Input id="container" value={containerName} onChange={(e) => setContainerName(e.target.value)} required className="font-mono" placeholder="web" />
              </Field>
              {node.fields}
              {container.fields}
            </div>
          )}
        </Section>

        {create.error && <Alert>{errorMessage(create.error)}</Alert>}
        <div className="flex justify-end gap-2">
          <Link to="/projects">
            <Button type="button" variant="ghost">
              取消
            </Button>
          </Link>
          <Button type="submit" disabled={create.isPending || (withContainer && !node.nodeId)}>
            {create.isPending && <Spinner />}
            创建项目
          </Button>
        </div>
      </form>
    </div>
  );
}
