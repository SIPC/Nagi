import { useEffect, useState } from "react";
import { useMutation, useQuery } from "@tanstack/react-query";
import { Link, useNavigate, useSearch } from "@tanstack/react-router";
import { Plus } from "lucide-react";
import { authClient, errorMessage, nodeClient, projectClient, settingsClient, toDate } from "@/lib/api";
import { PageHeader } from "@/pages/layout";
import { formatDate, slugName } from "@/lib/utils";
import { t } from "@/i18n";
import { offlineNodeNames, ProjectState, stateOf } from "@/components/status";
import { cn } from "@/lib/utils";
import {
  Alert,
  Badge,
  Button,
  ButtonLink,
  Dialog,
  DialogActions,
  Empty,
  EmptyRow,
  Field,
  HelpTip,
  Input,
  Notice,
  Roll,
  Select,
  Skeleton,
  Spinner,
  Status,
} from "@/components/ui";
import type { Node } from "@/gen/nagi/v1/api_pb";
import type { ProjectSummary } from "@/gen/nagi/v1/common_pb";
import { checkName } from "@/lib/config";
import { useStandbys } from "@/pages/standby";
import { useRole } from "@/components/role";

// Attention：需要处理的事聚合成一行：数量、名单和「查看」，点开后逐项显示（说明和操作）。
// 顺序固定：节点离线、备用面板未连接、两步验证、主密钥。后两项可以「不再提醒」（只记在这个浏览器里）。
function Attention({ offlineNodes, offlineStandbys }: { offlineNodes: string[]; offlineStandbys: string[] }) {
  const me = useQuery({ queryKey: ["me"], queryFn: () => authClient.me({}) });
  const settings = useQuery({ queryKey: ["settings"], queryFn: () => settingsClient.getSettings({}) });
  const [open, setOpen] = useState(false);
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
  const m = t.projects.attention;
  const edge = "-mr-3";
  const dismiss = (k: "totp" | "key", storageKey: string) => (
    <Button variant="ghost" className={edge} readOnlyOk onClick={() => hide(k, storageKey)}>
      {t.projects.reminders.dismiss}
    </Button>
  );
  const items: { key: string; short: string; title: string; text: React.ReactNode; action: React.ReactNode }[] = [];
  if (offlineNodes.length > 0) {
    items.push({
      key: "nodes",
      short: t.common.status.nodesOffline(offlineNodes),
      title: t.common.status.nodesOffline(offlineNodes),
      text: t.projects.offline.body,
      action: (
        <ButtonLink to="/nodes" variant="ghost" className={edge} readOnlyOk>
          {t.projects.offline.action}
        </ButtonLink>
      ),
    });
  }
  if (offlineStandbys.length > 0) {
    items.push({
      key: "standby",
      short: m.standby,
      title: t.projects.standbyOffline.title(offlineStandbys.join(", ")),
      text: t.projects.standbyOffline.body,
      action: (
        <ButtonLink to="/settings" search={{ tab: "standby" }} variant="ghost" className={edge} readOnlyOk>
          {t.projects.standbyOffline.action}
        </ButtonLink>
      ),
    });
  }
  if (me.data && !me.data.totpEnabled && !hidden.totp) {
    items.push({
      key: "totp",
      short: m.totp,
      title: t.projects.reminders.totpTitle,
      text: t.projects.reminders.totpText(
        <Link to="/settings" search={{ tab: "security" }} className="mx-1 text-foreground underline underline-offset-2">
          {t.projects.reminders.totpLink}
        </Link>,
      ),
      action: dismiss("totp", "nagi.hide.totp"),
    });
  }
  const path = settings.data?.settings?.masterKeyPath;
  if (path && !hidden.key) {
    items.push({
      key: "key",
      short: m.masterKey,
      title: t.projects.reminders.masterKeyTitle,
      text: t.projects.reminders.masterKeyText(<span className="font-mono text-foreground">{path}</span>),
      action: dismiss("key", "nagi.hide.masterkey"),
    });
  }
  if (items.length === 0) return null;
  return (
    <div className="grid gap-2">
      <div role="status" className="flex min-h-8 flex-wrap items-center justify-between gap-x-4 gap-y-2">
        <span>
          <span className="mr-2 mb-0.5 inline-block size-1.5 rounded-full bg-warning align-middle" />
          <span className="font-medium text-warning-text">{m.title(items.length)}</span> <span className="text-muted-foreground max-md:hidden">{items.map((it) => it.short).join(m.separator)}</span>
        </span>
        <Button variant="ghost" className={edge} readOnlyOk aria-expanded={open} onClick={() => setOpen(!open)}>
          {open ? m.collapse : m.view}
        </Button>
      </div>
      {open &&
        items.map((it) => (
          <Notice key={it.key} tone="warning" title={it.title} actions={it.action}>
            {it.text}
          </Notice>
        ))}
    </div>
  );
}

// 卡片里最多列几行容器：放不下时列前两个，第三行写「还有 N 个」，同一行的卡片高低差不多。
const cardContainers = 3;

const dot = {
  success: "bg-success",
  warning: "bg-warning",
  danger: "bg-destructive",
  neutral: "bg-muted-foreground",
};

function ImageUpdateIcon() {
  return (
    <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth={1.5} strokeLinecap="round" strokeLinejoin="round" className="size-3.5 shrink-0 text-info" role="img" aria-label={t.projects.card.imageUpdate}>
      <title>{t.projects.card.imageUpdate}</title>
      <circle cx="8" cy="8" r="6.5" />
      <path d="M8 11V5M5.5 7.5 8 5l2.5 2.5" />
    </svg>
  );
}

const projectCard = "flex min-w-0 flex-col gap-3 rounded-lg border bg-background p-4 outline-none transition-colors focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/15";
const projectGrid = "grid grid-cols-[repeat(auto-fill,minmax(min(100%,240px),1fr))] gap-4";

function ProjectCard({ summary, nodes }: { summary: ProjectSummary; nodes: Node[] | undefined }) {
  const p = summary.project!;
  const nodeName = (id: string) => nodes?.find((n) => n.id === id)?.name ?? id;
  const offline = (id: string) => nodes?.find((n) => n.id === id)?.online === false;
  const services = summary.status?.services ?? [];
  const deployed = p.containers.some((m) => m.deployedVersion > 0n);
  const updates = summary.status?.imageUpdates ?? [];
  const shown = p.containers.length > cardContainers ? p.containers.slice(0, cardContainers - 1) : p.containers;
  return (
    <Link to="/projects/$projectId" params={{ projectId: p.id }} className={cn(projectCard, "hover:border-ring")}>
      <div className="flex min-w-0 items-center justify-between gap-2">
        <span className="min-w-0 truncate font-medium">{p.name}</span>
        {p.containers.length === 0 ? <Status>{t.projects.card.noContainers}</Status> : <ProjectState services={services} deployed={deployed} offline={offlineNodeNames(p.containers, nodes)} />}
      </div>
      <div className="-mt-2 truncate font-mono text-xs leading-5 text-muted-foreground">{p.networkName}</div>
      <div className="grid gap-1 max-md:hidden">
        {p.containers.length === 0 ? (
          <div className="text-[13px] text-muted-foreground">{t.projects.card.noContainers}</div>
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
              <div key={c.name} className="flex min-w-0 items-center gap-2 text-[13px]" title={st.label}>
                <span className={cn("size-1.5 shrink-0 rounded-full", dot[st.tone])} />
                <span className="flex min-w-0 flex-1 items-center gap-1">
                  <span className="min-w-0 truncate">{c.name}</span>
                  {updates.some((u) => u.container === c.name) && <ImageUpdateIcon />}
                </span>
                {c.failover ? <Badge tone={tookOver ? "warning" : "neutral"}>{tookOver ? t.projects.card.tookOver : t.projects.card.failover}</Badge> : c.stateful && <Badge>{t.projects.card.stateful}</Badge>}
                <span className="max-w-[40%] min-w-6 shrink-0 truncate text-right font-mono text-xs leading-5 text-muted-foreground">{nodeName(c.nodeId)}</span>
              </div>
            );
          })
        )}
        {shown.length < p.containers.length && <div className="text-[13px] text-muted-foreground">{t.projects.card.moreContainers(p.containers.length - shown.length)}</div>}
      </div>
      <div className="mt-auto flex justify-between gap-3 text-xs leading-4 text-muted-foreground">
        <Roll value={p.routes.length > 0 ? `${t.projects.card.containers(p.containers.length)} · ${t.projects.card.portMappings(p.routes.length)}` : t.projects.card.containers(p.containers.length)} />
        <span
          className="shrink-0 tabular-nums"
          title={p.rebuiltAt ? t.projects.card.rebuiltAt(formatDate(toDate(p.rebuiltAt)) ?? "—") : t.projects.card.createdAt(formatDate(toDate(p.createdAt)) ?? "—")}
        >
          {formatDate(toDate(p.rebuiltAt ?? p.createdAt)) ?? "—"}
        </span>
      </div>
    </Link>
  );
}

function CardsSkeleton() {
  const line = "my-1 h-3";
  return (
    <div role="status" aria-label={t.common.feedback.loading} className={projectGrid}>
      {[0, 1, 2].map((i) => (
        <div key={i} className={projectCard}>
          <Skeleton className={cn(line, "w-3/5")} />
          <Skeleton className={cn(line, "w-4/5")} />
        </div>
      ))}
    </div>
  );
}

export function ProjectsPage({ creating }: { creating?: boolean }) {
  const projects = useQuery({
    queryKey: ["projects"],
    queryFn: () => projectClient.listProjects({}),
    refetchInterval: 5000,
  });
  const nodes = useQuery({ queryKey: ["nodes"], queryFn: () => nodeClient.listNodes({}) });
  const offlineNodes = (nodes.data?.nodes ?? []).filter((n) => !n.online && !n.pending).map((n) => n.name);
  const { isAdmin } = useRole();
  // 备用面板在设置里，只有管理员能处理。
  const offlineStandbys = (useStandbys().data?.standbys ?? []).filter((s) => isAdmin && !s.connected).map((s) => s.name);
  const search = useSearch({ strict: false }) as { q?: string; sort?: "name" };
  const navigateList = useNavigate();
  const sort = search.sort ?? "created";
  // 输入框用本地状态，再同步到地址：直接绑地址会打断中文输入法。
  const [query, setQuery] = useState(search.q ?? "");
  useEffect(() => {
    if ((query || undefined) !== search.q) navigateList({ to: "/projects", search: (s) => ({ ...s, q: query || undefined }), replace: true });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [query]);
  const all = projects.data?.projects ?? [];
  const q = query.trim().toLowerCase();
  const shownProjects = all
    .filter(({ project: p }) => !q || [p?.name, p?.networkName, ...(p?.containers ?? []).map((c) => c.name)].some((v) => v?.toLowerCase().includes(q)))
    .slice()
    .sort((a, b) => (sort === "name" ? (a.project?.name ?? "").localeCompare(b.project?.name ?? "") : Number(b.project?.createdAt?.seconds ?? 0n) - Number(a.project?.createdAt?.seconds ?? 0n)));

  const newButton = (
    <ButtonLink to="/projects/new">
      <Plus />
      <span className="max-md:hidden">{t.projects.newProject}</span>
      <span className="md:hidden">{t.projects.newShort}</span>
    </ButtonLink>
  );

  return (
    <>
      <PageHeader className="max-md:flex-nowrap max-md:items-center max-md:[&_p]:hidden" title={t.projects.title} description={t.projects.description} actions={newButton} />
      {creating && <NewProjectDialog />}
      <div className="grid grid-cols-1 gap-8 max-md:gap-6">
        <Attention offlineNodes={offlineNodes} offlineStandbys={offlineStandbys} />
        {projects.isPending ? (
          <CardsSkeleton />
        ) : projects.error ? (
          <Notice
            tone="danger"
            title={t.projects.loadFailed.title}
            actions={
              <Button variant="outline" readOnlyOk onClick={() => projects.refetch()}>
                {t.common.actions.retry}
              </Button>
            }
          >
            {errorMessage(projects.error)}
          </Notice>
        ) : projects.data.projects.length === 0 ? (
          <Empty title={t.projects.empty.title} action={newButton}>
            {t.projects.empty.text}
          </Empty>
        ) : (
          <div className="grid gap-4">
            <div className="flex flex-wrap items-center justify-between gap-2">
              <Input value={query} onChange={(e) => setQuery(e.target.value)} placeholder={t.projects.filter.placeholder} aria-label={t.projects.filter.label} autoComplete="off" className="max-w-[280px] max-md:max-w-none" />
              <Select value={sort} onChange={(e) => navigateList({ to: "/projects", search: (s) => ({ ...s, sort: e.target.value === "name" ? "name" : undefined }), replace: true })} aria-label={t.projects.filter.sort} className="w-auto max-md:hidden">
                <option value="created">{t.projects.filter.byCreated}</option>
                <option value="name">{t.projects.filter.byName}</option>
              </Select>
            </div>
            {shownProjects.length === 0 ? (
              <EmptyRow>{t.projects.filter.empty}</EmptyRow>
            ) : (
              <div className={projectGrid}>
                {shownProjects.map((summary) => summary.project && <ProjectCard key={summary.project.id} summary={summary} nodes={nodes.data?.nodes} />)}
              </div>
            )}
          </div>
        )}
      </div>
    </>
  );
}

// useNodeChoice：选择容器运行的节点和容器目录（留空时显示默认值）。新建项目和添加容器共用。
// useDefaultDir 返回容器在节点上的默认目录：<节点的项目根目录>/<项目网络名>/<容器名>（见 docs/agent.md#容器目录）。
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
  const nodeField = (
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
  );
  const dirField = (
    <Field label={t.projects.nodeChoice.dir} htmlFor="container-dir" help={t.projects.nodeChoice.dirHelp}>
      <Input id="container-dir" value={dir} onChange={(e) => setDir(e.target.value)} placeholder={defaultDir} />
    </Field>
  );
  // dirFor：容器名称可能到提交时才定下来（留空时取镜像名），目录按最终的名称算。
  const dirFor = (container: string) => dir.trim() || `${defaultDir.slice(0, defaultDir.lastIndexOf("/"))}/${container}`;
  return { nodeField, dirField, nodeId: selected, dir: dir.trim() || defaultDir, dirFor };
}

// NewProjectPage：/projects/new 是项目列表上打开新建对话框，能直接链接。
export function NewProjectPage() {
  return <ProjectsPage creating />;
}

// NewProjectDialog：新建项目只填名称，网络名由名称生成，要改时点「修改」。创建后进入项目概览，第一个容器也要点添加。
function NewProjectDialog() {
  const navigate = useNavigate();
  const [name, setName] = useState("");
  const [network, setNetwork] = useState("");
  const [editNetwork, setEditNetwork] = useState(false);
  const generated = slugName(name, "");
  const create = useMutation({
    mutationFn: () => projectClient.createProject({ name, networkName: network.trim() || generated }),
    onSuccess: (p) => navigate({ to: "/projects/$projectId", params: { projectId: p.id } }),
  });
  const close = () => navigate({ to: "/projects" });
  return (
    <Dialog open onClose={close} title={t.projects.newProject}>
      <form
        className="grid gap-4"
        onSubmit={(e) => {
          e.preventDefault();
          create.mutate();
        }}
      >
        <Field label={t.projects.create.name} htmlFor="name">
          <Input id="name" value={name} onChange={(e) => setName(e.target.value)} placeholder={t.projects.create.namePlaceholder} required maxLength={64} autoFocus />
        </Field>
        {editNetwork ? (
          <Field label={t.projects.create.networkName} htmlFor="network" help={`${t.projects.create.slugHint}${t.common.sentenceEnd}${t.projects.create.networkHelp}`} error={checkName(network)}>
            <Input
              id="network"
              value={network}
              onChange={(e) => {
                setNetwork(e.target.value);
                e.target.setCustomValidity(checkName(e.target.value));
              }}
              placeholder={slugName(name, "shop")}
            />
          </Field>
        ) : (
          <div className="-mt-2 flex items-center gap-2 text-[13px] leading-5">
            <span className="text-muted-foreground">{t.projects.create.networkName}</span>
            <span className="min-w-0 truncate">{generated || "—"}</span>
            <HelpTip className="-ml-1">{`${t.projects.create.networkHelp}${t.projects.create.slugHint}${t.common.sentenceEnd}`}</HelpTip>
            <button type="button" className="cursor-pointer underline underline-offset-4 decoration-border hover:decoration-foreground" onClick={() => setEditNetwork(true)}>
              {t.projects.create.editNetwork}
            </button>
          </div>
        )}
        {create.error && <Alert>{errorMessage(create.error)}</Alert>}
        <DialogActions>
          <Button variant="outline" onClick={close}>
            {t.common.actions.cancel}
          </Button>
          <Button type="submit" disabled={create.isPending}>
            {create.isPending && <Spinner />}
            {t.projects.create.submit}
          </Button>
        </DialogActions>
      </form>
    </Dialog>
  );
}
