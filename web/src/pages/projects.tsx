import { useState } from "react";
import { useMutation, useQuery } from "@tanstack/react-query";
import { Link, useNavigate } from "@tanstack/react-router";
import { FolderKanban, Plus } from "lucide-react";
import { authClient, errorMessage, nodeClient, projectClient, settingsClient, toDate } from "@/lib/api";
import { Breadcrumb, PageHeader } from "@/pages/layout";
import { formatDate, slugName } from "@/lib/utils";
import { t } from "@/i18n";
import { ProjectState, stateOf } from "@/components/status";
import { cn } from "@/lib/utils";
import { useContainerForm } from "@/components/container-form";
import {
  Alert,
  Badge,
  Button,
  CardBody,
  cardClass,
  CardFooter,
  CardGrid,
  CardHeader,
  Check,
  Empty,
  Field,
  Input,
  Loading,
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
      title: t.projects.reminders.totpTitle,
      text: t.projects.reminders.totpText(
        <Link to="/settings" search={{ tab: "security" }} className="mx-1 text-foreground underline underline-offset-2">
          {t.projects.reminders.totpLink}
        </Link>,
      ),
    });
  }
  const path = settings.data?.settings?.masterKeyPath;
  if (path && !hidden.key) {
    items.push({
      key: "key",
      storage: "nagi.hide.masterkey",
      title: t.projects.reminders.masterKeyTitle,
      text: t.projects.reminders.masterKeyText(<span className="font-mono text-foreground">{path}</span>),
    });
  }
  if (items.length === 0) return null;
  // 一项一行的待办提醒，不用大块的提示框。标题和说明上下两行，窄屏上说明不会被挤成窄窄的一列。
  return (
    <div className="divide-y rounded-lg border">
      {items.map((it) => (
        <div key={it.key} className="flex items-start gap-3 px-4 py-3 text-[13px]">
          <span className="mt-1.5 size-1.5 shrink-0 rounded-full bg-warning" />
          <div className="grid min-w-0 flex-1 gap-0.5">
            <span className="font-medium">{it.title}</span>
            <span className="text-pretty break-words text-muted-foreground">{it.text}</span>
          </div>
          <Button size="xs" variant="ghost" className="-my-0.5 -mr-2" onClick={() => hide(it.key, it.storage)}>
            {t.projects.reminders.dismiss}
          </Button>
        </div>
      ))}
    </div>
  );
}


// 卡片里最多列几个容器（超过时最后一行显示“还有 N 个”）。
const cardContainers = 5;

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
        {t.projects.newProject}
      </Button>
    </Link>
  );

  return (
    <>
      <PageHeader title={t.projects.title} description={t.projects.description} actions={newButton} />
      <div className="grid gap-6">
        <Reminders />
        {projects.isPending ? (
          <Loading />
        ) : projects.error ? (
          <Alert>{errorMessage(projects.error)}</Alert>
        ) : projects.data.projects.length === 0 ? (
          <Empty title={t.projects.empty.title} icon={<FolderKanban className="size-5" />}>
            {t.projects.empty.text}
          </Empty>
        ) : (
          <CardGrid>
            {projects.data.projects.map((summary) => {
              const p = summary.project;
              if (!p) return null;
              const services = summary.status?.services ?? [];
              const deployed = p.containers.some((m) => m.deployedVersion > 0n);
              // 容器多时只列前几个，免得同一行的卡片高低悬殊。
              const shown = p.containers.length > cardContainers ? p.containers.slice(0, cardContainers - 1) : p.containers;
              return (
                <Link key={p.id} to="/projects/$projectId" params={{ projectId: p.id }} className={cardClass}>
                  <CardHeader
                    title={<span className="truncate">{p.name}</span>}
                    subtitle={
                      <>
                        <span className="truncate font-mono">{p.networkName}</span>
                        {p.stateful && <Badge>{t.projects.card.stateful}</Badge>}
                      </>
                    }
                    aside={<ProjectState services={services} deployed={deployed} />}
                  />
                  <CardBody className="gap-1.5">
                    {p.containers.length === 0 ? (
                      <span className="text-[13px] text-muted-foreground">{t.projects.card.noContainers}</span>
                    ) : (
                      shown.map((c) => {
                        const st = offline(c.nodeId)
                          ? { tone: "warning" as const, label: t.projects.card.nodeOffline }
                          : stateOf(
                              services.filter((x) => x.container === c.name),
                              c.deployedVersion > 0n,
                            );
                        const tookOver = c.failover && c.failover.a?.nodeId !== c.nodeId;
                        return (
                          <div key={c.name} className="flex items-center gap-2 text-[13px]" title={st.label}>
                            <span className={cn("size-2 shrink-0 rounded-full", dot[st.tone])} />
                            <span className="min-w-0 flex-1 truncate">{c.name}</span>
                            {c.failover && <Badge tone={tookOver ? "warning" : "neutral"}>{tookOver ? t.projects.card.tookOver : t.projects.card.failover}</Badge>}
                            <span className="shrink-0 text-xs text-muted-foreground">{nodeName(c.nodeId)}</span>
                          </div>
                        );
                      })
                    )}
                    {shown.length < p.containers.length && (
                      <span className="pl-4 text-xs text-muted-foreground">{t.projects.card.moreContainers(p.containers.length - shown.length)}</span>
                    )}
                  </CardBody>
                  <CardFooter>
                    <span>
                      {t.projects.card.containers(p.containers.length)}
                      {p.routes.length > 0 && ` · ${t.projects.card.portMappings(p.routes.length)}`}
                    </span>
                    <span className="tabular-nums">
                      {p.rebuiltAt
                                              ? t.projects.card.rebuiltAt(formatDate(toDate(p.rebuiltAt)) ?? "—")
                                              : t.projects.card.createdAt(formatDate(toDate(p.createdAt)) ?? "—")}
                    </span>
                  </CardFooter>
                </Link>
              );
            })}
          </CardGrid>
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
      <Field label={t.projects.nodeChoice.node} htmlFor="container-node" help={t.projects.nodeChoice.nodeHelp}>
        <Select id="container-node" value={selected} onChange={(e) => setNodeId(e.target.value)} required>
          {onlineNodes.length === 0 && <option value="">{t.projects.nodeChoice.noOnlineNodes}</option>}
          {onlineNodes.map((n) => (
            <option key={n.id} value={n.id}>
              {n.name}
            </option>
          ))}
        </Select>
      </Field>
      <Field
        label={t.projects.nodeChoice.dir}
        htmlFor="container-dir"
        help={t.projects.nodeChoice.dirHelp}
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
      <Breadcrumb items={[{ label: t.projects.title, to: "/projects" }, { label: t.projects.newProject }]} />
      <PageHeader title={t.projects.newProject} description={t.projects.create.description} />
      <form
        className="grid gap-6"
        onSubmit={(e) => {
          e.preventDefault();
          create.mutate();
        }}
      >
        <Section title={t.projects.create.basics}>
          <div className="grid gap-4 sm:grid-cols-2">
            <Field label={t.projects.create.name} htmlFor="name">
              <Input id="name" value={name} onChange={(e) => setName(e.target.value)} placeholder={t.projects.create.namePlaceholder} required maxLength={64} />
            </Field>
            <Field
              label={t.projects.create.networkName}
              htmlFor="network"
              hint={t.projects.create.slugHint}
              help={t.projects.create.networkHelp}
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
          title={t.projects.create.firstContainer}
          description={t.projects.create.firstContainerDescription}
          actions={
            <Check checked={withContainer} onChange={setWithContainer}>
              {t.projects.create.withContainer}
            </Check>
          }
        >
          {withContainer && (
            <div className="grid gap-4">
              <Field
                label={container.mode === "compose" ? t.projects.create.groupName : t.projects.create.containerName}
                htmlFor="container"
                hint={t.projects.create.slugHint}
                help={t.projects.create.containerNameHelp}
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
              {t.common.actions.cancel}
            </Button>
          </Link>
          <Button type="submit" disabled={create.isPending || (withContainer && !node.nodeId)}>
            {create.isPending && <Spinner />}
            {t.projects.create.submit}
          </Button>
        </div>
      </form>
    </div>
  );
}
