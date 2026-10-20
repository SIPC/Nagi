import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate } from "@tanstack/react-router";
import { FolderKanban, Plus } from "lucide-react";
import { authClient, errorMessage, nodeClient, projectClient, settingsClient, toDate } from "@/lib/api";
import { Breadcrumb, PageHeader } from "@/pages/layout";
import { ProjectState } from "@/components/status";
import { useContainerForm } from "@/components/container-form";
import { SharedNetworkWarning } from "@/components/shared-network-warning";
import {
  Alert,
  Badge,
  Button,
  Check,
  Empty,
  Field,
  Input,
  Loading,
  LogBox,
  Notice,
  Section,
  Select,
  Spinner,
  Table,
  Td,
  Th,
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

export function ProjectsPage() {
  const projects = useQuery({
    queryKey: ["projects"],
    queryFn: () => projectClient.listAllProjects({}),
    refetchInterval: 5000,
  });
  const nodes = useQuery({ queryKey: ["nodes"], queryFn: () => nodeClient.listNodes({}) });
  const nodeName = (id: string) => nodes.data?.nodes.find((n) => n.id === id)?.name ?? id;
  const offline = (id: string) => nodes.data?.nodes.find((n) => n.id === id)?.online === false;
  // 在线节点上正在运行的项目：项目被顶替以后，原节点离线时面板上还有它的配置备份，同一个项目会出现两次。
  const runningElsewhere = new Set(
    (projects.data?.projects ?? []).filter(({ nodeId }) => !offline(nodeId)).map(({ project }) => project?.project?.id),
  );
  const [rebuild, setRebuild] = useState<{ nodeId: string; projectId: string; name: string; dir: string; sharedNetworks: string[] } | null>(null);

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
      <PageHeader title="项目" description="项目是一组协同工作的容器，整体运行在同一个节点上。" actions={newButton} />
      <div className="grid gap-6">
        <Reminders />
        {rebuild && <RebuildCard {...rebuild} onClose={() => setRebuild(null)} />}
        {projects.isPending ? (
          <Loading />
        ) : projects.error ? (
          <Alert>{errorMessage(projects.error)}</Alert>
        ) : projects.data.projects.length === 0 ? (
          <Empty title="暂无项目" icon={<FolderKanban className="size-5" />}>
            新建项目后，可添加容器或容器组（一份 compose）并部署到节点。
          </Empty>
        ) : (
          <Section flush>
            <Table>
              <thead>
                <tr>
                  <Th>名称</Th>
                  <Th>节点</Th>
                  <Th>状态</Th>
                  <Th>容器</Th>
                  <Th>创建时间</Th>
                  <Th className="w-px" />
                </tr>
              </thead>
              <tbody>
                {projects.data.projects.map(({ nodeId, project: summary }) => {
                  const p = summary?.project;
                  if (!p) return null;
                  // 已经在别的节点上运行（被顶替或者迁移走了）的，不再显示离线节点上的那份配置备份。
                  if (offline(nodeId) && runningElsewhere.has(p.id)) return null;
                  const key = `${nodeId}/${p.id}`;
                  const link = (children: React.ReactNode) => (
                    <Link to="/projects/$nodeId/$projectId" params={{ nodeId, projectId: p.id }} className="font-medium hover:underline">
                      {children}
                    </Link>
                  );

                  if (summary.error) {
                    // 节点在线，但读不了这个项目（例如磁盘满了）：说明原因，不要显示成"未部署"。
                    return (
                      <tr key={key}>
                        <Td>
                          <div className="grid gap-0.5">
                            {link(<span className="font-mono text-[13px]">{p.dir}</span>)}
                            <span className="text-xs text-destructive">{summary.error}</span>
                          </div>
                        </Td>
                        <Td className="text-muted-foreground">{nodeName(nodeId)}</Td>
                        <Td>
                          <Badge tone="danger">读取失败</Badge>
                        </Td>
                        <Td className="text-muted-foreground">—</Td>
                        <Td className="text-muted-foreground">—</Td>
                        <Td />
                      </tr>
                    );
                  }

                  const services = summary.status?.services ?? [];
                  const deployed = p.containers.filter((m) => m.deployedVersion > 0n).length;
                  const total = p.containers.length;

                  if (offline(nodeId)) {
                    // 节点离线：显示的是面板上的配置备份，项目页打不开；可以用备份在别的节点上重建。
                    return (
                      <tr key={key}>
                        <Td>
                          <div className="grid gap-0.5">
                            <span className="font-medium">{p.name}</span>
                            <span className="text-xs text-muted-foreground">面板上的配置备份</span>
                          </div>
                        </Td>
                        <Td className="text-muted-foreground">{nodeName(nodeId)}</Td>
                        <Td>
                          <Badge tone="warning">节点离线</Badge>
                        </Td>
                        <Td className="tabular-nums">{total}</Td>
                        <Td className="text-muted-foreground tabular-nums">{formatDate(toDate(p.createdAt))}</Td>
                        <Td className="text-right">
                          <Button
                            size="xs"
                            variant="outline"
                            onClick={() => setRebuild({ nodeId, projectId: p.id, name: p.name, dir: p.dir, sharedNetworks: p.settings?.sharedNetworks ?? [] })}
                          >
                            重建
                          </Button>
                        </Td>
                      </tr>
                    );
                  }

                  return (
                    <tr key={key} className="transition-colors hover:bg-subtle">
                      <Td>
                        <div className="grid gap-0.5">
                          {link(p.name)}
                          <span className="truncate font-mono text-xs text-muted-foreground">{p.dir}</span>
                          {summary.readOnlyReason && <span className="text-xs text-destructive">只读：{summary.readOnlyReason}</span>}
                        </div>
                      </Td>
                      <Td className="text-muted-foreground">{nodeName(nodeId)}</Td>
                      <Td>
                        <ProjectState services={services} deployed={deployed > 0} />
                      </Td>
                      <Td className="tabular-nums">
                        {total}
                        {deployed > 0 && deployed < total && <span className="ml-1.5 text-xs text-muted-foreground">已部署 {deployed}</span>}
                      </Td>
                      <Td className="text-muted-foreground tabular-nums">{formatDate(toDate(p.createdAt))}</Td>
                      <Td />
                    </tr>
                  );
                })}
              </tbody>
            </Table>
          </Section>
        )}
      </div>
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
  const [result, setResult] = useState<{ ok: boolean; text: string } | null>(null);
  const [created, setCreated] = useState<{ nodeId: string; projectId: string } | null>(null);
  const [busy, setBusy] = useState(false);

  const run = async () => {
    setBusy(true);
    setLines([]);
    setResult(null);
    setCreated(null);
    try {
      for await (const ev of projectClient.rebuildNodeProject({ nodeId, projectId, targetNodeId: target, dir })) {
        if (ev.event.case === "output") {
          const l = ev.event.value;
          setLines((x) => [...x, l]);
        } else if (ev.event.case === "result") {
          const r = ev.event.value;
          if (r.projectId) setCreated({ nodeId: target, projectId: r.projectId });
          setResult(r.success ? { ok: true, text: "重建完成" } : { ok: false, text: r.error });
        }
      }
    } catch (err) {
      setResult({ ok: false, text: errorMessage(err) });
    } finally {
      setBusy(false);
      queryClient.invalidateQueries({ queryKey: ["projects"] });
    }
  };

  const others = nodes.data?.nodes.filter((n) => n.id !== nodeId && n.online) ?? [];
  const openButton = created && (
    <Button size="sm" variant="outline" onClick={() => navigate({ to: "/projects/$nodeId/$projectId", params: created })}>
      打开新项目
    </Button>
  );
  return (
    <Section
      title={`使用配置备份重建 ${name}`}
      description="在另一个节点上创建新项目（新的项目 ID），恢复各容器的配置、草稿、项目设置和端口映射。"
      help={
        <>
          项目文件、运行数据和历史版本不包含在备份中；deploy key 和 webhook 地址将重新生成。从 git 拉取文件且不使用 data/ 文件的容器会自动部署，其余容器需先上传文件。原节点恢复后，两个项目互不影响。
        </>
      }
      actions={
        <Button size="sm" variant="ghost" disabled={busy} onClick={onClose}>
          关闭
        </Button>
      }
    >
      <div className="grid gap-4">
        <div className="grid gap-4 sm:grid-cols-[minmax(0,1fr)_minmax(0,1.5fr)_auto] sm:items-end">
          <Field label="目标节点" htmlFor="rebuild-target">
            <Select id="rebuild-target" value={target} onChange={(e) => setTarget(e.target.value)} disabled={busy}>
              <option value="">选择节点</option>
              {others.map((n) => (
                <option key={n.id} value={n.id}>
                  {n.name}
                </option>
              ))}
            </Select>
          </Field>
          <Field label="项目目录" htmlFor="rebuild-dir">
            <Input id="rebuild-dir" className="font-mono" value={dir} onChange={(e) => setDir(e.target.value)} disabled={busy} />
          </Field>
          <Button size="sm" className="h-9" disabled={busy || !target} onClick={run}>
            {busy && <Spinner />}
            开始重建
          </Button>
        </div>
        <SharedNetworkWarning fromNodeId={nodeId} targetNodeId={target} networks={sharedNetworks} />
        {result &&
          (result.ok ? (
            <Notice tone="success" actions={openButton}>
              {result.text}
            </Notice>
          ) : (
            <Notice tone="danger" actions={openButton}>
              {result.text}
            </Notice>
          ))}
        {!result && created && <div>{openButton}</div>}
        {lines.length > 0 && <LogBox className="max-h-60">{lines.join("\n")}</LogBox>}
      </div>
    </Section>
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
  const [containerName, setContainerName] = useState("");
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
          <div className="grid gap-4">
            <div className="grid gap-4 sm:grid-cols-2">
              <Field label="名称" htmlFor="name">
                <Input id="name" value={name} onChange={(e) => setName(e.target.value)} placeholder="我的应用" required maxLength={64} />
              </Field>
              <Field label="节点" htmlFor="node">
                <Select id="node" value={selectedNode} onChange={(e) => setNodeId(e.target.value)} required>
                  {onlineNodes.length === 0 && <option value="">无在线节点</option>}
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
              hint="节点上的绝对路径，目录须不存在或为空"
              help="每个容器位于项目目录的 containers/<容器名>/ 下，compose 中的相对路径以容器的 data/ 目录为基准。"
            >
              <Input id="dir" value={dir} onChange={(e) => setDir(e.target.value)} placeholder="/srv/myapp" required className="font-mono" />
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
          <Button type="submit" disabled={create.isPending || !selectedNode}>
            {create.isPending && <Spinner />}
            创建项目
          </Button>
        </div>
      </form>
    </div>
  );
}
