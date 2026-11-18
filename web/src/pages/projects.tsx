import { useEffect, useState } from "react";
import { useMutation, useQuery } from "@tanstack/react-query";
import { Link, useNavigate, useSearch } from "@tanstack/react-router";
import { ChevronRight, CircleArrowUp, GripVertical, Plus } from "lucide-react";
import { rectSortingStrategy, verticalListSortingStrategy } from "@dnd-kit/sortable";
import { authClient, errorMessage, orOther, knownNodes, nodeClient, projectClient, settingsClient, toDate } from "@/lib/api";
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
  CardGrid,
  cardClass,
  Collapse,
  Dialog,
  DialogActions,
  Empty,
  EmptyRow,
  Field,
  HelpTip,
  Input,
  Notice,
  QueryError,
  Roll,
  Select,
  Skeleton,
  Spinner,
  dotColor,
  textLink,
} from "@/components/ui";
import type { Node } from "@/gen/nagi/v1/api_pb";
import type { ProjectSummary } from "@/gen/nagi/v1/common_pb";
import { checkName } from "@/lib/config";
import { useStandbys } from "@/pages/standby";
import { useRole } from "@/components/role";
import { GroupSelect, groupsOf, ManageLabels } from "@/components/labels";
import { Reorder, useProjectOrder, useReorderItem, type ProjectSort } from "@/pages/project-order";

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
      short: m.nodes(offlineNodes),
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
      title: t.projects.standbyOffline.title(t.common.list(offlineStandbys)),
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
        <Link to="/settings" search={{ tab: "security" }} className={cn("mx-1", textLink)}>
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
  // 只有一条时直接显示，不用再点「查看」。
  if (items.length === 1)
    return (
      <Notice tone="warning" title={items[0].title} actions={items[0].action}>
        {items[0].text}
      </Notice>
    );
  return (
    <div>
      <Notice
        tone="warning"
        title={m.title(items.length)}
        actions={
          <Button variant="ghost" className={edge} readOnlyOk aria-expanded={open} onClick={() => setOpen(!open)}>
            {open ? m.collapse : m.view}
          </Button>
        }
      >
        <span className="max-md:hidden">{t.common.list(items.map((it) => it.short))}</span>
      </Notice>
      <Collapse open={open}>
        <div className="grid gap-2 pt-2">
          {items.map((it) => (
            <Notice key={it.key} tone="warning" title={it.title} actions={it.action}>
              {it.text}
            </Notice>
          ))}
        </div>
      </Collapse>
    </div>
  );
}

// 卡片里最多列几行容器：放不下时列前两个，第三行写「还有 N 个」，同一行的卡片高低差不多。
const cardContainers = 3;

// 卡片共用 cardClass（和其他可点的卡片同一种边框、悬停和焦点），这里只加内边距和间距。
const projectCard = cn(cardClass, "min-w-0 gap-3 p-4");

function ProjectCard({ summary, nodes, draggable }: { summary: ProjectSummary; nodes: Node[] | undefined; draggable?: boolean }) {
  const p = summary.project!;
  const nodeName = (id: string) => nodes?.find((n) => n.id === id)?.name ?? orOther(id);
  const offline = (id: string) => nodes?.find((n) => n.id === id)?.online === false;
  const services = summary.status?.services ?? [];
  const deployed = p.containers.some((m) => m.deployedVersion > 0n);
  const updates = summary.status?.imageUpdates ?? [];
  const shown = p.containers.length > cardContainers ? p.containers.slice(0, cardContainers - 1) : p.containers;
  return (
    <Link to="/projects/$projectId" params={{ projectId: p.id }} className={projectCard} draggable={draggable}>
      <div className="flex min-w-0 items-center justify-between gap-2">
        <span className="min-w-0 truncate font-medium">{p.name}</span>
        {/* 没有容器时不写状态，下面的内容区已经写了「暂无容器」。 */}
        {p.containers.length > 0 && <ProjectState services={services} deployed={deployed} offline={offlineNodeNames(p.containers, nodes)} />}
      </div>
      <div className="-mt-2 truncate text-[13px] leading-5 text-muted-foreground">{p.networkName}</div>
      <div className="grid gap-1 max-md:hidden">
        {p.containers.length === 0 ? (
          <div className="text-[13px] text-muted-foreground">{t.projects.card.noContainers}</div>
        ) : (
          shown.map((c) => {
            const st = offline(c.nodeId)
              ? { tone: "warning" as const, label: t.common.status.nodesOffline([nodeName(c.nodeId)]) }
              : stateOf(
                  services.filter((x) => x.container === c.name),
                  c.deployedVersion > 0n,
                );
            const tookOver = c.failover && c.failover.a?.nodeId !== c.nodeId;
            return (
              <div key={c.name} className="flex min-w-0 items-center gap-2 text-[13px]" title={st.label}>
                <span className={cn("size-1.5 shrink-0 rounded-full", dotColor[st.tone])} />
                <span className="flex min-w-0 flex-1 items-center gap-1">
                  {/* 和项目栏、容器页一样显示显示名称，没设时是容器名。 */}
                  <span className="min-w-0 truncate">{c.displayName || c.name}</span>
                  {updates.some((u) => u.container === c.name) && (
                    <span title={t.projects.card.imageUpdate} className="inline-flex">
                      <CircleArrowUp className="size-3.5 shrink-0 text-info" role="img" aria-label={t.projects.card.imageUpdate} />
                    </span>
                  )}
                </span>
                {c.failover ? <Badge tone={tookOver ? "warning" : "neutral"}>{tookOver ? t.projects.card.tookOver : t.projects.card.failover}</Badge> : c.stateful && <Badge>{t.projects.card.stateful}</Badge>}
                <span className="max-w-[40%] min-w-6 shrink-0 truncate text-right text-[13px] leading-5 text-muted-foreground">{nodeName(c.nodeId)}</span>
              </div>
            );
          })
        )}
        {shown.length < p.containers.length && <div className="text-[13px] text-muted-foreground">{t.projects.card.moreContainers(p.containers.length - shown.length)}</div>}
      </div>
      <div className="mt-auto flex justify-between gap-3 text-[13px] leading-5 text-muted-foreground">
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
    <CardGrid role="status" aria-label={t.common.feedback.loading}>
      {[0, 1, 2].map((i) => (
        <div key={i} className={projectCard}>
          <Skeleton className={cn(line, "w-3/5")} />
          <Skeleton className={cn(line, "w-4/5")} />
        </div>
      ))}
    </CardGrid>
  );
}

// 收起的分组记在这个浏览器里。
const collapsedKey = "nagi.projects.collapsed";

function useCollapsed() {
  const [collapsed, setCollapsed] = useState<string[]>(() => {
    try {
      const v = JSON.parse(localStorage.getItem(collapsedKey) ?? "[]");
      return Array.isArray(v) ? v.filter((x) => typeof x === "string") : [];
    } catch {
      return [];
    }
  });
  const toggle = (key: string) => {
    const next = collapsed.includes(key) ? collapsed.filter((k) => k !== key) : [...collapsed, key];
    setCollapsed(next);
    try {
      localStorage.setItem(collapsedKey, JSON.stringify(next));
    } catch {
      // 存不了就只在这次有效。
    }
  };
  return { collapsed, toggle };
}

// SortableCard：调整顺序时可以拖动的项目卡片，整张卡片都能按住拖，边框变成虚线，点击不打开项目。
// 拖动中的卡片浮在上面、带阴影。
function SortableCard({ summary, nodes }: { summary: ProjectSummary; nodes: Node[] | undefined }) {
  const r = useReorderItem(summary.project!.id);
  return (
    <div
      ref={r.ref}
      style={r.style}
      {...r.handle}
      onClickCapture={r.enabled ? (e) => e.preventDefault() : undefined}
      className={cn("grid min-w-0", r.enabled && "cursor-grab touch-manipulation *:border-dashed", r.dragging && "relative z-10 cursor-grabbing *:shadow-lg")}
    >
      <ProjectCard summary={summary} nodes={nodes} draggable={r.enabled ? false : undefined} />
    </div>
  );
}

// GroupSection：一个分组：标题行和卡片。调整顺序时按住标题行上下拖动调整分组的顺序。
function GroupSection({ id, name, count, open, onToggle, children }: { id: string; name: string; count: number; open: boolean; onToggle: () => void; children: React.ReactNode }) {
  const r = useReorderItem(id);
  return (
    <section ref={r.ref} style={r.style} className={cn("min-w-0", r.dragging && "relative z-10 rounded-lg bg-background shadow-lg")}>
      <GroupHeader name={name} count={count} open={open} onToggle={onToggle} handle={r.handle} handleRef={r.handleRef} sortable={r.enabled} />
      {/* 展开、收起时高度和透明度一起过渡，下面的分组跟着移动（ui.md 的 Collapse）。间距放在里面，收起后不留空。 */}
      <Collapse open={open}>
        <div className="pt-2">{children}</div>
      </Collapse>
    </section>
  );
}

// GroupHeader：分组的标题行，点击展开或收起。参考 Grafana 的文件夹行：箭头、名称、数量。
function GroupHeader({
  name,
  count,
  open,
  onToggle,
  handle,
  handleRef,
  sortable,
}: {
  name: string;
  count: number;
  open: boolean;
  onToggle: () => void;
  handle?: React.HTMLAttributes<HTMLElement>;
  handleRef?: (el: HTMLElement | null) => void;
  sortable?: boolean;
}) {
  return (
    <button
      type="button"
      ref={handleRef}
      {...handle}
      aria-expanded={open}
      aria-label={open ? t.labels.collapse(name) : t.labels.expand(name)}
      className={cn(
        "-mx-2 flex h-8 w-fit max-w-full cursor-pointer items-center gap-2 rounded-md px-2 text-left outline-none hover:bg-accent focus-ring",
        sortable && "cursor-grab touch-manipulation active:cursor-grabbing",
      )}
      onClick={onToggle}
    >
      {sortable && <GripVertical className="size-4 shrink-0 text-muted-foreground" aria-hidden />}
      <ChevronRight className={cn("size-4 shrink-0 text-muted-foreground transition-transform duration-240", open && "rotate-90")} />
      <span className="truncate font-medium">{name}</span>
      <span className="shrink-0 text-[13px] text-muted-foreground tabular-nums">{count}</span>
    </button>
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
  const { isAdmin, can } = useRole();
  const canCreate = can("project.manage");
  // 备用面板在设置里，只有管理员能处理。
  const offlineStandbys = (useStandbys().data?.standbys ?? []).filter((s) => isAdmin && !s.connected).map((s) => s.name);
  const search = useSearch({ strict: false }) as { q?: string; group?: string; ungrouped?: true };
  const { collapsed, toggle } = useCollapsed();
  const order = useProjectOrder();
  const [managing, setManaging] = useState(false);
  const navigateList = useNavigate();
  const sort = order.sort;
  // 调整顺序：点「调整顺序」进入，可以拖动分组和卡片；点「完成」退出。进入时排序换成「自定义」。
  const [arranging, setArranging] = useState(false);
  // 输入框用本地状态，再同步到地址：直接绑地址会打断中文输入法。
  const [query, setQuery] = useState(search.q ?? "");
  useEffect(() => {
    if ((query || undefined) !== search.q) navigateList({ to: "/projects", search: (s) => ({ ...s, q: query || undefined }), replace: true });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [query]);
  const all = projects.data?.projects ?? [];
  const custom = sort === "custom";
  const groups = custom ? order.sortGroups(groupsOf(all), (g) => g.name) : groupsOf(all);
  // 筛选的分组已经没有了（例如被删除）时，当作全部。
  const groupFilter = search.ungrouped ? "" : search.group && groups.some((g) => g.name === search.group) ? search.group : undefined;
  const q = query.trim().toLowerCase();
  const shownProjects = all
    .filter(({ project: p }) => groupFilter === undefined || (p?.group ?? "") === groupFilter)
    .filter(({ project: p }) => !q || [p?.name, p?.networkName, p?.group, ...(p?.containers ?? []).map((c) => c.name)].some((v) => v?.toLowerCase().includes(q)))
    .slice()
    .sort((a, b) => (sort === "name" ? (a.project?.name ?? "").localeCompare(b.project?.name ?? "") : Number(b.project?.createdAt?.seconds ?? 0n) - Number(a.project?.createdAt?.seconds ?? 0n)));
  const idOf = (s: ProjectSummary) => s.project?.id ?? "";
  const shown = custom ? order.sortProjects(shownProjects, idOf) : shownProjects;
  // 拖动只改动所在列表里的顺序，其余项目（别的分组、被筛选掉的）保持原位。
  const allIds = () => order.sortProjects(all.slice().sort((a, b) => Number(b.project?.createdAt?.seconds ?? 0n) - Number(a.project?.createdAt?.seconds ?? 0n)), idOf).map(idOf);
  const projectName = (id: string) => all.find((s) => s.project?.id === id)?.project?.name ?? id;

  // 新建项目要「项目管理」。
  const newButton = !canCreate ? null : (
    <ButtonLink to="/projects/new">
      <Plus />
      <span className="max-md:hidden">{t.projects.newProject}</span>
      <span className="md:hidden">{t.projects.newShort}</span>
    </ButtonLink>
  );

  const filterValue = groupFilter === undefined ? "*" : groupFilter === "" ? "-" : `g:${groupFilter}`;
  // 有分组、没有筛选某个分组时按分组分段显示，未分组的放最后。
  const sections =
    groups.length > 0 && groupFilter === undefined
      ? [
          ...groups.map((g) => ({ key: `g:${g.name}`, name: g.name, items: shown.filter((s) => s.project?.group === g.name) })),
          { key: "-", name: t.labels.ungrouped, items: shown.filter((s) => !s.project?.group) },
        ].filter((s) => s.items.length > 0)
      : undefined;
  const cards = (list: ProjectSummary[]) => {
    const ids = list.map(idOf);
    return (
      <Reorder ids={ids} name={projectName} enabled={arranging} strategy={rectSortingStrategy} onMove={(a, o) => order.moveProject(allIds(), ids, a, o)}>
        <CardGrid>{list.map((summary) => summary.project && <SortableCard key={summary.project.id} summary={summary} nodes={knownNodes(nodes.data)} />)}</CardGrid>
      </Reorder>
    );
  };
  // 未分组的固定在最后，不参与分组的拖动。
  const sortableSections = sections?.filter((s) => s.key !== "-") ?? [];
  const sectionName = (key: string) => sections?.find((s) => s.key === key)?.name ?? key;

  return (
    <>
      <PageHeader
        className="max-md:flex-nowrap max-md:items-center max-md:[&_p]:hidden"
        title={t.projects.title}
        description={t.projects.description}
        actions={
          <>
            {newButton}
            {/* 只有一项操作，不放进 ⋯ 菜单。 */}
            {isAdmin && groups.length > 0 && (
              <Button variant="outline" onClick={() => setManaging(true)}>
                {t.labels.manageGroups}
              </Button>
            )}
          </>
        }
      />
      {creating && canCreate && <NewProjectDialog groups={groups.map((g) => g.name)} initialGroup={groupFilter} />}
      <Dialog open={managing} onClose={() => setManaging(false)} title={t.labels.manageGroups}>
        <ManageLabels kind="group" labels={groups} />
      </Dialog>
      <div className="grid grid-cols-1 gap-6 max-md:gap-4">
        <Attention offlineNodes={offlineNodes} offlineStandbys={offlineStandbys} />
        {projects.isPending ? (
          <CardsSkeleton />
        ) : projects.error ? (
          <QueryError error={errorMessage(projects.error)} onRetry={() => projects.refetch()} />
        ) : projects.data.projects.length === 0 ? (
          <Empty title={t.projects.empty.title} action={newButton}>
            {t.projects.empty.text}
          </Empty>
        ) : (
          <div className="grid gap-4">
            {/* 手机上筛选框和排序同一行，筛选框占剩下的宽度；有分组筛选时放不下，筛选框单独一行，两个下拉平分下一行。 */}
            <div className="flex flex-wrap items-center justify-between gap-2">
              <Input value={query} onChange={(e) => setQuery(e.target.value)} placeholder={t.projects.filter.placeholder} aria-label={t.projects.filter.label} autoComplete="off" className={cn("max-w-[280px] max-md:max-w-none max-md:min-w-0 max-md:flex-1", groups.length > 0 && "max-md:basis-full")} />
              <div className={cn("flex min-w-0 shrink-0 items-center gap-2", groups.length > 0 && "max-md:w-full max-md:*:flex-1")}>
              {groups.length > 0 && (
                <Select
                  value={filterValue}
                  onChange={(e) => {
                    const v = e.target.value;
                    navigateList({ to: "/projects", search: (s) => ({ ...s, group: v.startsWith("g:") ? v.slice(2) : undefined, ungrouped: v === "-" ? true : undefined }), replace: true });
                  }}
                  aria-label={t.labels.group}
                  className="w-auto max-w-40 shrink-0"
                >
                  <option value="*">{t.labels.allGroups}</option>
                  {groups.map((g) => (
                    <option key={g.name} value={`g:${g.name}`}>
                      {g.name}
                    </option>
                  ))}
                  <option value="-">{t.labels.ungrouped}</option>
                </Select>
              )}
              <Select
                value={sort}
                disabled={!order.loaded}
                onChange={(e) => {
                  const v = e.target.value as ProjectSort;
                  order.setSort(v);
                  if (v !== "custom") setArranging(false);
                }}
                aria-label={t.projects.filter.sort}
                className="w-auto shrink-0"
              >
                <option value="created">{t.projects.filter.byCreated}</option>
                <option value="name">{t.projects.filter.byName}</option>
                <option value="custom">{t.projects.filter.byCustom}</option>
              </Select>
              {all.length > 1 && (
                <Button
                  variant={arranging ? "default" : "outline"}
                  disabled={!order.loaded}
                  onClick={() => {
                    if (!arranging) order.setSort("custom");
                    setArranging(!arranging);
                  }}
                >
                  {arranging ? t.projects.reorder.done : t.projects.reorder.start}
                </Button>
              )}
              </div>
            </div>
            {arranging && <p className="text-[13px] text-muted-foreground">{t.projects.reorder.hint}</p>}
            {shownProjects.length === 0 ? (
              <EmptyRow
                action={
                  <Button
                    variant="outline"
                    onClick={() => {
                      setQuery("");
                      navigateList({ to: "/projects", search: (s) => ({ ...s, q: undefined, group: undefined, ungrouped: undefined }), replace: true });
                    }}
                  >
                    {t.audit.clearFilters}
                  </Button>
                }
              >
                {t.projects.filter.empty}
              </EmptyRow>
            ) : sections ? (
              // 分组之间比组内（标题到卡片 8）宽，看得出哪些卡片属于同一组。
              <div className="grid gap-6">
                <Reorder
                  ids={sortableSections.map((s) => s.key)}
                  name={sectionName}
                  enabled={arranging}
                  strategy={verticalListSortingStrategy}
                  onMove={(a, o) => order.moveGroup(groups.map((g) => g.name), a.slice(2), o.slice(2))}
                >
                  {sections.map((s) => (
                    <GroupSection key={s.key} id={s.key} name={s.name} count={s.items.length} open={!collapsed.includes(s.key)} onToggle={() => toggle(s.key)}>
                      {cards(s.items)}
                    </GroupSection>
                  ))}
                </Reorder>
              </div>
            ) : (
              cards(shown)
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
      <Field
        label={t.projects.nodeChoice.node}
        htmlFor="container-node"
        help={t.projects.nodeChoice.nodeHelp}
        // 没有在线节点时提交不了：红字写原因并给去处，不写在格式说明的位置。
        error={
          nodes.data && onlineNodes.length === 0 && (
            <>
              {t.projects.nodeChoice.noOnlineNodesHint}{" "}
              <Link to="/nodes" className={textLink}>
                {t.projects.nodeChoice.viewNodes}
              </Link>
            </>
          )
        }
      >
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
      <Input id="container-dir" value={dir} onChange={(e) => setDir(e.target.value)} placeholder={defaultDir} autoComplete="off" spellCheck={false} />
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

// NewProjectDialog：新建项目只填名称，网络名由名称生成，要改时点「编辑」。
// 字段错误提交后才显示（不靠浏览器的必填气泡，也不边输入边报错）。创建后进入项目概览，第一个容器也要点添加。
function NewProjectDialog({ groups, initialGroup }: { groups: string[]; initialGroup?: string }) {
  const navigate = useNavigate();
  // 设分组归「成员」权限。
  const isAdmin = useRole().can("panel.members");
  const [name, setName] = useState("");
  // 分组只有管理员能设；已经有分组时才显示，第一个分组在项目设置里建。
  const [group, setGroup] = useState(initialGroup ?? "");
  const [network, setNetwork] = useState("");
  const [editNetwork, setEditNetwork] = useState(false);
  const [checked, setChecked] = useState(false);
  const generated = slugName(name, "");
  const nameError = checked && !name.trim() ? t.projectSettings.general.nameRequired : "";
  const networkError = checked ? checkName(network) : "";
  const create = useMutation({
    mutationFn: async () => {
      const p = await projectClient.createProject({ name, networkName: network.trim() || generated });
      if (isAdmin && group.trim()) await projectClient.setProjectGroup({ projectId: p.id, group: group.trim() });
      return p;
    },
    onSuccess: (p) => navigate({ to: "/projects/$projectId", params: { projectId: p.id } }),
  });
  const close = () => navigate({ to: "/projects" });
  return (
    <Dialog open onClose={close} title={t.projects.newProject}>
      <form
        className="grid gap-4"
        noValidate
        onSubmit={(e) => {
          e.preventDefault();
          setChecked(true);
          if (!name.trim() || checkName(network)) return;
          create.mutate();
        }}
      >
        <Field label={t.projects.create.name} htmlFor="name" error={nameError}>
          <Input id="name" value={name} onChange={(e) => setName(e.target.value)} placeholder={t.projects.create.namePlaceholder} maxLength={64} autoFocus />
        </Field>
        {editNetwork ? (
          <Field label={t.projects.create.networkName} htmlFor="network" hint={t.projects.create.slugHint} help={t.projects.create.networkHelp} error={networkError}>
            <Input
              id="network"
              value={network}
              onChange={(e) => setNetwork(e.target.value)}
              placeholder={slugName(name, "shop")}
              autoComplete="off"
              spellCheck={false}
            />
          </Field>
        ) : (
          <div className="-mt-2 flex items-center gap-2 text-[13px] leading-5">
            <span className="text-muted-foreground">{t.projects.create.networkName}</span>
            <span className="min-w-0 truncate">{generated || "—"}</span>
            <HelpTip className="-ml-1">{`${t.projects.create.slugHint}${t.common.sentenceEnd}${t.projects.create.networkHelp}`}</HelpTip>
            <button type="button" className={textLink} onClick={() => setEditNetwork(true)}>
              {t.projects.create.editNetwork}
            </button>
          </div>
        )}
        {isAdmin && groups.length > 0 && (
          <Field label={t.labels.group} htmlFor="new-project-group">
            <GroupSelect id="new-project-group" value={group} groups={groups} onChange={setGroup} />
          </Field>
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
