import { useState } from "react";
import { useMutation, useQuery } from "@tanstack/react-query";
import { Link, useNavigate } from "@tanstack/react-router";
import { FolderKanban, Plus } from "lucide-react";
import { authClient, errorMessage, nodeClient, projectClient, settingsClient, toDate } from "@/lib/api";
import { Breadcrumb, PageHeader } from "@/pages/layout";
import { slugName } from "@/lib/utils";
import { ProjectState } from "@/components/status";
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
          <Section flush>
            <Table>
              <thead>
                <tr>
                  <Th>名称</Th>
                  <Th>节点</Th>
                  <Th>状态</Th>
                  <Th>容器</Th>
                  <Th>创建时间</Th>
                </tr>
              </thead>
              <tbody>
                {projects.data.projects.map((summary) => {
                  const p = summary.project;
                  if (!p) return null;
                  const services = summary.status?.services ?? [];
                  const deployed = p.containers.filter((m) => m.deployedVersion > 0n).length;
                  const total = p.containers.length;
                  const nodeIds = [...new Set(p.containers.map((c) => c.nodeId))];
                  return (
                    <tr key={p.id} className="transition-colors hover:bg-subtle">
                      <Td>
                        <div className="grid gap-0.5">
                          <Link to="/projects/$projectId" params={{ projectId: p.id }} className="font-medium hover:underline">
                            {p.name}
                          </Link>
                          <span className="truncate font-mono text-xs text-muted-foreground">{p.networkName}</span>
                        </div>
                      </Td>
                      <Td className="text-muted-foreground">
                        {nodeIds.length === 0 ? (
                          "—"
                        ) : (
                          <div className="flex flex-wrap gap-x-3 gap-y-1">
                            {nodeIds.map((id) => (
                              <span key={id} className="inline-flex items-center gap-1.5">
                                {nodeName(id)}
                                {offline(id) && <Badge tone="warning">离线</Badge>}
                              </span>
                            ))}
                          </div>
                        )}
                      </Td>
                      <Td>
                        <ProjectState services={services} deployed={deployed > 0} />
                      </Td>
                      <Td className="tabular-nums">
                        {total}
                        {deployed > 0 && deployed < total && <span className="ml-1.5 text-xs text-muted-foreground">已部署 {deployed}</span>}
                      </Td>
                      <Td className="text-muted-foreground tabular-nums">{formatDate(toDate(p.createdAt))}</Td>
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

// useNodeChoice：选择容器运行的节点和容器目录（留空时显示默认值）。新建项目和添加容器共用。
export function useNodeChoice(networkName: string, containerName: string) {
  const nodes = useQuery({ queryKey: ["nodes"], queryFn: () => nodeClient.listNodes({}) });
  const onlineNodes = nodes.data?.nodes.filter((n) => n.online && !n.pending) ?? [];
  const [nodeId, setNodeId] = useState("");
  const [dir, setDir] = useState("");
  const selected = nodeId || onlineNodes[0]?.id || "";
  const root = onlineNodes.find((n) => n.id === selected)?.info?.projectsRoot || "/srv/nagi";
  const defaultDir = `${root}/${networkName || "<网络名>"}/${containerName || "<容器名>"}`;
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
      <Field label="容器目录" htmlFor="container-dir" hint="留空则使用默认目录" help="容器的运行文件、数据和快照都在这个目录里。目录须不存在或为空。">
        <Input id="container-dir" value={dir} onChange={(e) => setDir(e.target.value)} placeholder={defaultDir} className="font-mono" />
      </Field>
    </div>
  );
  return { fields, nodeId: selected, dir: dir.trim() };
}

export function NewProjectPage() {
  const navigate = useNavigate();
  const [name, setName] = useState("");
  const [network, setNetwork] = useState("");
  const [withContainer, setWithContainer] = useState(true);
  const [containerName, setContainerName] = useState("");
  const container = useContainerForm(true);
  const node = useNodeChoice(network || slugName(name, ""), containerName);

  const create = useMutation({
    mutationFn: () =>
      projectClient.createProject({
        name,
        networkName: network,
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
              help="在面板上唯一。连接本项目的项目通过「容器.网络名」访问本项目的容器；容器目录默认也按它命名。名称中没有英文字母或数字时须填写。"
            >
              <Input id="network" value={network} onChange={(e) => setNetwork(e.target.value)} placeholder={slugName(name, "shop")} className="font-mono" />
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
