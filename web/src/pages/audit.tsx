// 审计日志（见 docs/audit.md#界面）：谁在什么时候、从哪里、通过什么做了什么。
// 管理员看全部；操作员和只读成员只看访问范围内项目的记录（面板按权限筛，IP 和浏览器不返回）。
import { useInfiniteQuery, useQuery } from "@tanstack/react-query";
import { useNavigate, useSearch } from "@tanstack/react-router";
import { useEffect, useMemo, useState } from "react";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { ChevronDown, ChevronRight, Download, ListFilter, Play, X } from "lucide-react";
import { AutoHeight, Badge, Button, Check, CodeBlock, Collapse, Dialog, DialogActions, EmptyRow, Field, FormBody, Input, KeyValue, List, QueryError, Row, RowsSkeleton, Select, Spinner, Status } from "@/components/ui";
import { Sheet, SheetBody, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import { RecordingDialog } from "@/components/recording";
import { PageHeader } from "@/pages/layout";
import { useRole } from "@/components/role";
import type { ActivityEntry, AuditEvent, AuditFilter, Node, PanelUser } from "@/gen/nagi/v1/api_pb";
import type { ProjectSummary } from "@/gen/nagi/v1/common_pb";
import { errorMessage, nodeClient, projectClient, settingsClient, toDate } from "@/lib/api";
import { cn, formatDateTime, timeAgo } from "@/lib/utils";
import { t } from "@/i18n";
import { toast } from "sonner";

const pageSize = 50;

export const auditRanges = ["1h", "24h", "7d", "30d"] as const;
export type AuditRange = (typeof auditRanges)[number];
export const auditCategories = ["operation", "read", "mcp", "session", "login", "alert", "security"] as const;
export const auditVias = ["web", "mcp", "cli", "system", "node"] as const;

// 地址里的筛选条件（见 main.tsx 的 auditRoute）。
export type AuditSearch = {
  range?: AuditRange;
  member?: number;
  via?: string;
  cat?: string;
  project?: string;
  node?: string;
  failed?: boolean;
  q?: string;
  id?: number;
};

const rangeMs: Record<AuditRange, number> = { "1h": 3600e3, "24h": 86400e3, "7d": 7 * 86400e3, "30d": 30 * 86400e3 };

// actionLabel：接口方法名或事件名换成给人看的动作；没有登记的显示原名。项目和容器的事件列表也用它。
export function actionLabel(e: Pick<ActivityEntry, "action" | "kind">) {
  if (e.kind === "alert") return e.action === "resolved" ? t.activity.resolved : t.activity.firing;
  return eventLabel(e.action);
}

export function eventLabel(action: string) {
  if (action.startsWith("mcp:")) return t.audit.events.mcp(action.slice(4));
  return (t.activity.actions as Record<string, string>)[action] ?? (t.audit.events as Record<string, unknown>)[action]?.toString() ?? action;
}

function resultTone(e: AuditEvent) {
  if (e.category === "alert") return e.action === "resolved" ? "neutral" : "warning";
  return e.result === "ok" ? "neutral" : "danger";
}

function resultLabel(e: AuditEvent) {
  if (e.category === "alert") return e.action === "resolved" ? t.activity.resolved : t.activity.firing;
  return (t.audit.results as Record<string, string>)[e.result] ?? e.result;
}

// 一条记录的标题：动作和对象。告警写告警的标题。
function title(e: AuditEvent) {
  let name = eventLabel(e.action);
  if (e.category === "alert") {
    try {
      name = JSON.parse(e.params).title || name;
    } catch {
      // 参数不是 JSON 时用动作名。
    }
  }
  // MCP 调用的对象是工具本身，动作里已经写了工具名。
  const target = [e.projectName || (e.projectId && e.projectId), e.targetType !== "tool" && e.targetName].filter(Boolean).join(" / ");
  return target ? `${name} ${target}` : name;
}

function actorName(e: AuditEvent) {
  if (e.actorName) return e.actorName;
  return (t.audit.actors as Record<string, string>)[e.actorType] ?? "";
}

function viaLabel(e: AuditEvent) {
  const v = (t.audit.via as Record<string, string>)[e.via] ?? e.via;
  if (e.via === "mcp" && e.tokenName) return t.audit.viaToken(v, e.tokenName);
  return v;
}

// 第二行：成员 · 来源 · IP · 时间。
function subline(e: AuditEvent) {
  return [actorName(e), viaLabel(e), e.ip, timeAgo(toDate(e.time))].filter(Boolean).join(" · ");
}

function toFilter(s: AuditSearch): AuditFilter {
  const f = {
    actorId: s.member ? String(s.member) : "",
    via: s.via ?? "",
    categories: s.cat ? [s.cat] : [],
    projectId: s.project ?? "",
    nodeId: s.node ?? "",
    failedOnly: !!s.failed,
    search: s.q ?? "",
  } as AuditFilter;
  if (s.range) f.from = timestampFromDate(new Date(Date.now() - rangeMs[s.range]));
  return f;
}

export function AuditPage() {
  const search = useSearch({ from: "/app/audit" });
  const navigate = useNavigate({ from: "/audit" });
  const { isAdmin, isViewer } = useRole();
  const set = (patch: Partial<AuditSearch>) => navigate({ search: (s) => ({ ...s, ...patch }), replace: true });

  // 搜索框输入时稍等再写进地址，免得每个字都查一次。
  const [q, setQ] = useState(search.q ?? "");
  useEffect(() => setQ(search.q ?? ""), [search.q]);
  useEffect(() => {
    if ((search.q ?? "") === q) return;
    const id = setTimeout(() => set({ q: q || undefined }), 300);
    return () => clearTimeout(id);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [q]);

  const filterKey = [search.range, search.member, search.via, search.cat, search.project, search.node, search.failed, search.q];
  const list = useInfiniteQuery({
    queryKey: ["audit", ...filterKey],
    initialPageParam: 0n,
    queryFn: ({ pageParam }) => settingsClient.listAuditEvents({ filter: toFilter(search), before: pageParam, limit: pageSize }),
    getNextPageParam: (last) => (last.events.length < pageSize ? undefined : last.events[last.events.length - 1].id),
    refetchInterval: 15000,
  });
  const projects = useQuery({ queryKey: ["projects"], queryFn: () => projectClient.listProjects({}) });
  const nodes = useQuery({ queryKey: ["nodes"], queryFn: () => nodeClient.listNodes({}), enabled: isAdmin });
  const users = useQuery({ queryKey: ["users"], queryFn: () => settingsClient.listUsers({}), enabled: isAdmin });
  const events = list.data?.pages.flatMap((p) => p.events) ?? [];
  const filtered = filterKey.some((v) => v !== undefined && v !== "" && v !== false);

  const [filtering, setFiltering] = useState(false);
  const userName = (id: number) => users.data?.users.find((u) => Number(u.id) === id)?.username ?? String(id);
  const chips: { key: string; label: string; clear: () => void }[] = [];
  if (search.cat) chips.push({ key: "cat", label: (t.audit.categories as Record<string, string>)[search.cat], clear: () => set({ cat: undefined }) });
  if (search.via) chips.push({ key: "via", label: (t.audit.via as Record<string, string>)[search.via], clear: () => set({ via: undefined }) });
  if (search.member) chips.push({ key: "member", label: userName(search.member), clear: () => set({ member: undefined }) });
  if (search.project) chips.push({ key: "project", label: projects.data?.projects.find((p) => p.project?.id === search.project)?.project?.name ?? search.project, clear: () => set({ project: undefined }) });
  if (search.node) chips.push({ key: "node", label: nodes.data?.nodes.find((n) => n.id === search.node)?.name ?? search.node, clear: () => set({ node: undefined }) });
  if (search.failed) chips.push({ key: "failed", label: t.audit.failedOnly, clear: () => set({ failed: undefined }) });
  const [exporting, setExporting] = useState(false);
  const exportJSONL = async () => {
    setExporting(true);
    try {
      const chunks: Uint8Array[] = [];
      for await (const c of settingsClient.exportAuditEvents({ filter: toFilter(search) })) chunks.push(c.data);
      const blob = new Blob(chunks as BlobPart[], { type: "application/x-ndjson" });
      const d = new Date();
      const p2 = (n: number) => String(n).padStart(2, "0");
      const stamp = `${d.getFullYear()}${p2(d.getMonth() + 1)}${p2(d.getDate())}-${p2(d.getHours())}${p2(d.getMinutes())}${p2(d.getSeconds())}`;
      const name = `nagi-audit-${stamp}.jsonl`;
      const a = document.createElement("a");
      a.href = URL.createObjectURL(blob);
      a.download = name;
      a.click();
      setTimeout(() => URL.revokeObjectURL(a.href), 1000);
      toast.success(t.audit.exported);
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setExporting(false);
    }
  };

  return (
    <>
      <PageHeader
        title={t.app.nav.audit}
        description={t.audit.description}
        actions={
          isAdmin && (
            <Button readOnlyOk variant="outline" disabled={exporting} onClick={exportJSONL}>
              {exporting ? <Spinner /> : <Download />}
              {t.audit.export}
            </Button>
          )
        }
      />
      {/* 筛选：常用的筛选框和时间范围在一行，其余条件收进「筛选」对话框，生效的条件列在下面，可以单独去掉。 */}
      <div className="mb-4 grid gap-2">
        <div className="flex items-center justify-between gap-2">
          <Input value={q} onChange={(e) => setQ(e.target.value)} placeholder={t.audit.search} aria-label={isAdmin ? t.audit.searchLabel : isViewer ? t.audit.searchLabelViewer : t.audit.searchLabelOperator} autoComplete="off" className="max-w-[280px] max-md:max-w-none max-md:min-w-0 max-md:flex-1" />
          <div className="flex shrink-0 items-center gap-2">
            <Select aria-label={t.audit.filters.range} className="w-auto" value={search.range ?? ""} onChange={(e) => set({ range: (e.target.value || undefined) as AuditRange | undefined })}>
              <option value="">{t.audit.ranges.all}</option>
              {auditRanges.map((r) => (
                <option key={r} value={r}>
                  {t.audit.ranges[r]}
                </option>
              ))}
            </Select>
            <Button readOnlyOk variant="outline" onClick={() => setFiltering(true)} aria-label={t.audit.filters.button}>
              <ListFilter />
              <span className="max-md:hidden">{t.audit.filters.button}</span>
              {chips.length > 0 && <span className="text-muted-foreground tabular-nums">{chips.length}</span>}
            </Button>
          </div>
        </div>
        {chips.length > 0 && (
          <div className="flex flex-wrap items-center gap-2">
            {chips.map((c) => (
              <Badge key={c.key} className="gap-1 pr-1 text-foreground">
                {c.label}
                <button type="button" className="rounded-full p-0.5 text-muted-foreground hover:bg-accent hover:text-foreground" aria-label={t.audit.filters.remove(c.label)} onClick={c.clear}>
                  <X className="size-3" />
                </button>
              </Badge>
            ))}
            <Button readOnlyOk variant="ghost" size="sm" className="h-6 px-2 text-xs text-muted-foreground" onClick={() => navigate({ search: { range: search.range, q: search.q }, replace: true })}>
              {t.audit.clearFilters}
            </Button>
          </div>
        )}
      </div>
      <AuditFilterDialog open={filtering} onClose={() => setFiltering(false)} search={search} isAdmin={isAdmin} users={users.data?.users ?? []} projects={projects.data?.projects ?? []} nodes={nodes.data?.nodes ?? []} onApply={(v) => set(v)} />
      <div className="grid gap-4">
        {list.isPending ? (
          <RowsSkeleton count={5} />
        ) : (
          <>
            {list.error && <QueryError error={errorMessage(list.error)} onRetry={() => list.refetch()} />}
            {list.data &&
              (events.length === 0 ? (
                <EmptyRow
                  action={
                    filtered && (
                      <Button readOnlyOk variant="outline" onClick={() => navigate({ search: {}, replace: true })}>
                        {t.audit.clearFilters}
                      </Button>
                    )
                  }
                >
                  {filtered ? t.audit.emptyFiltered : t.audit.empty}
                </EmptyRow>
              ) : (
                <List>
                  {events.map((e) => (
                    <AuditRow key={String(e.id)} e={e} onOpen={(id) => set({ id: Number(id) })} />
                  ))}
                </List>
              ))}
          </>
        )}
        {list.hasNextPage && (
          <div>
            <Button readOnlyOk variant="ghost" className="-ml-3" disabled={list.isFetchingNextPage} onClick={() => list.fetchNextPage()}>
              {list.isFetchingNextPage && <Spinner />}
              {t.audit.more}
            </Button>
          </div>
        )}
      </div>
      <AuditSheet id={search.id} onOpen={(id) => set({ id: id ? Number(id) : undefined })} />
    </>
  );
}

function AuditRow({ e, onOpen, nested }: { e: AuditEvent; onOpen: (id: bigint) => void; nested?: boolean }) {
  const [open, setOpen] = useState(false);
  const children = useQuery({
    queryKey: ["audit-children", String(e.id)],
    queryFn: () => settingsClient.listAuditEvents({ parentId: e.id, limit: 200 }),
    enabled: open,
  });
  const label = title(e);
  return (
    <div className={cn(nested && "pl-6")}>
      <button type="button" className="block w-full rounded-md text-left outline-none focus-visible:ring-2 focus-visible:ring-ring" onClick={() => onOpen(e.id)}>
        <Row
          title={
            <>
              {e.category === "security" && <Status tone="danger">{t.audit.categories.security}</Status>}
              <span className="min-w-0 truncate" title={label}>
                {label}
              </span>
            </>
          }
          sub={
            <span className="wrap-anywhere" title={formatDateTime(toDate(e.time))}>
              {subline(e)}
            </span>
          }
          cols={[{ content: <Status tone={resultTone(e)}>{resultLabel(e)}</Status>, phone: true }]}
        />
      </button>
      {e.children > 0 && (
        <div className="pl-2">
          <Button readOnlyOk variant="ghost" size="sm" className="h-7 gap-1 px-2 text-[13px] text-muted-foreground" aria-expanded={open} onClick={() => setOpen(!open)}>
            {open ? <ChevronDown /> : <ChevronRight />}
            {open ? t.audit.collapse : t.audit.expand(e.children)}
          </Button>
        </div>
      )}
      {/* 子记录展开、收起时高度过渡（Collapse）；加载完由里面的 AutoHeight 过渡到实际高度。 */}
      <Collapse open={open}>
        <AutoHeight>
          {children.isPending ? (
            <div className="pl-6">
              <RowsSkeleton count={Math.min(e.children, 3)} />
            </div>
          ) : children.error ? (
            <QueryError className="pl-6" error={errorMessage(children.error)} onRetry={() => children.refetch()} />
          ) : (
            [...(children.data?.events ?? [])].reverse().map((c) => <AuditRow key={String(c.id)} e={c} onOpen={onOpen} nested />)
          )}
        </AutoHeight>
      </Collapse>
    </div>
  );
}

function prettyParams(params: string) {
  if (!params) return "";
  try {
    return JSON.stringify(JSON.parse(params), null, 2);
  } catch {
    return params;
  }
}

function AuditSheet({ id, onOpen }: { id?: number; onOpen: (id: bigint | undefined) => void }) {
  const detail = useQuery({
    queryKey: ["audit-event", id],
    queryFn: () => settingsClient.getAuditEvent({ id: BigInt(id!) }),
    enabled: !!id,
  });
  const e = detail.data?.event;
  const items = useMemo(() => {
    if (!e) return [];
    const d = t.audit.detail;
    const rows: [React.ReactNode, React.ReactNode][] = [
      [d.time, formatDateTime(toDate(e.time))],
      [d.category, (t.audit.categories as Record<string, string>)[e.category] ?? e.category],
      [d.result, <Status tone={resultTone(e)}>{resultLabel(e)}</Status>],
    ];
    if (e.error) rows.push([d.error, <span className="text-destructive">{e.error}</span>]);
    rows.push([d.actor, actorName(e) || "—"], [d.via, viaLabel(e)]);
    if (e.tokenName || e.tokenId) rows.push([d.token, e.tokenName || String(e.tokenId)]);
    if (e.client) rows.push([d.client, e.client]);
    if (e.session) rows.push([d.session, <span className="font-mono text-[13px]">{e.session}</span>]);
    if (e.ip) rows.push([d.ip, <span className="font-mono text-[13px]">{e.ip}</span>]);
    if (e.userAgent) rows.push([d.userAgent, <span className="text-[13px] text-muted-foreground">{e.userAgent}</span>]);
    if (e.projectId) rows.push([d.project, e.projectName || e.projectId]);
    if (e.targetName || e.targetId) rows.push([d.target, e.targetName || e.targetId]);
    if (e.durationMs > 0n) rows.push([d.duration, formatDuration(Number(e.durationMs))]);
    rows.push([d.id, <span className="font-mono text-[13px]">{String(e.id)}</span>]);
    return rows;
  }, [e]);
  const params = prettyParams(e?.params ?? "");
  const [replaying, setReplaying] = useState(false);

  return (
    <Sheet open={!!id} onOpenChange={(v) => !v && onOpen(undefined)}>
      <SheetContent closeLabel={t.common.actions.close}>
        <SheetHeader>
          <SheetTitle>{e ? title(e) : t.audit.detail.title}</SheetTitle>
          <SheetDescription>{e ? subline(e) : " "}</SheetDescription>
        </SheetHeader>
        <SheetBody>
          {detail.isPending && !!id ? (
            <RowsSkeleton count={6} />
          ) : detail.error ? (
            <QueryError error={errorMessage(detail.error)} onRetry={() => detail.refetch()} />
          ) : (
            e && (
              <>
                <KeyValue items={items} />
                {e.recordingId && (
                  <div>
                    <Button readOnlyOk variant="outline" onClick={() => setReplaying(true)}>
                      <Play />
                      {t.audit.replay.button}
                    </Button>
                    <RecordingDialog id={e.recordingId} open={replaying} onClose={() => setReplaying(false)} />
                  </div>
                )}
                <div className="grid gap-2">
                  <h3 className="text-sm font-medium">{t.audit.detail.params}</h3>
                  {params ? <CodeBlock className="max-h-80 overflow-auto [&_code]:whitespace-pre-wrap">{params}</CodeBlock> : <p className="text-muted-foreground">{t.audit.detail.noParams}</p>}
                </div>
                {detail.data?.parent && (
                  <div className="grid gap-2">
                    <h3 className="text-sm font-medium">{t.audit.detail.parent}</h3>
                    <List>
                      <SheetLink e={detail.data.parent} onOpen={onOpen} />
                    </List>
                  </div>
                )}
                {(detail.data?.children.length ?? 0) > 0 && (
                  <div className="grid gap-2">
                    <h3 className="text-sm font-medium">{t.audit.detail.children}</h3>
                    <List>
                      {detail.data!.children.map((c) => (
                        <SheetLink key={String(c.id)} e={c} onOpen={onOpen} />
                      ))}
                    </List>
                  </div>
                )}
              </>
            )
          )}
        </SheetBody>
      </SheetContent>
    </Sheet>
  );
}

function SheetLink({ e, onOpen }: { e: AuditEvent; onOpen: (id: bigint) => void }) {
  return (
    <button type="button" className="block w-full rounded-md text-left outline-none focus-visible:ring-2 focus-visible:ring-ring" onClick={() => onOpen(e.id)}>
      <Row title={<span className="min-w-0 truncate">{title(e)}</span>} sub={formatDateTime(toDate(e.time))} cols={[{ content: <Status tone={resultTone(e)}>{resultLabel(e)}</Status>, phone: true }]} />
    </button>
  );
}

// AuditFilterDialog：类别、来源、成员、项目、节点和只看失败。点「应用」才生效，一次只查一遍。
function AuditFilterDialog({
  open,
  onClose,
  search,
  isAdmin,
  users,
  projects,
  nodes,
  onApply,
}: {
  open: boolean;
  onClose: () => void;
  search: AuditSearch;
  isAdmin: boolean;
  users: PanelUser[];
  projects: ProjectSummary[];
  nodes: Node[];
  onApply: (v: Partial<AuditSearch>) => void;
}) {
  const init = () => ({ cat: search.cat ?? "", via: search.via ?? "", member: search.member ? String(search.member) : "", project: search.project ?? "", node: search.node ?? "", failed: !!search.failed });
  const [v, setV] = useState(init);
  useEffect(() => {
    if (open) setV(init());
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);
  const f = t.audit.filters;
  return (
    <Dialog open={open} onClose={onClose} title={f.title}>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          onApply({ cat: v.cat || undefined, via: v.via || undefined, member: v.member ? Number(v.member) : undefined, project: v.project || undefined, node: v.node || undefined, failed: v.failed || undefined });
          onClose();
        }}
      >
        <FormBody className="gap-4">
          <Field label={f.category} htmlFor="audit-cat">
            <Select id="audit-cat" value={v.cat} onChange={(e) => setV({ ...v, cat: e.target.value })}>
              <option value="">{f.all}</option>
              {auditCategories.map((c) => (
                <option key={c} value={c}>
                  {t.audit.categories[c]}
                </option>
              ))}
            </Select>
          </Field>
          <Field label={f.via} htmlFor="audit-via">
            <Select id="audit-via" value={v.via} onChange={(e) => setV({ ...v, via: e.target.value })}>
              <option value="">{f.all}</option>
              {auditVias.map((x) => (
                <option key={x} value={x}>
                  {t.audit.via[x]}
                </option>
              ))}
            </Select>
          </Field>
          {isAdmin && (
            <Field label={f.member} htmlFor="audit-member">
              <Select id="audit-member" value={v.member} onChange={(e) => setV({ ...v, member: e.target.value })}>
                <option value="">{f.all}</option>
                {users.map((u) => (
                  <option key={String(u.id)} value={String(u.id)}>
                    {u.displayName || u.username}
                  </option>
                ))}
              </Select>
            </Field>
          )}
          <Field label={f.project} htmlFor="audit-project">
            <Select id="audit-project" value={v.project} onChange={(e) => setV({ ...v, project: e.target.value })}>
              <option value="">{f.all}</option>
              {projects.map((p) => (
                <option key={p.project?.id} value={p.project?.id ?? ""}>
                  {p.project?.name}
                </option>
              ))}
            </Select>
          </Field>
          {isAdmin && (
            <Field label={f.node} htmlFor="audit-node">
              <Select id="audit-node" value={v.node} onChange={(e) => setV({ ...v, node: e.target.value })}>
                <option value="">{f.all}</option>
                {nodes.map((n) => (
                  <option key={n.id} value={n.id}>
                    {n.name}
                  </option>
                ))}
              </Select>
            </Field>
          )}
          <Check checked={v.failed} onChange={(x) => setV({ ...v, failed: x })}>
            {t.audit.failedOnly}
          </Check>
        </FormBody>
        <DialogActions>
          <Button readOnlyOk type="button" variant="outline" onClick={onClose}>
            {t.common.actions.cancel}
          </Button>
          <Button readOnlyOk type="submit">
            {f.apply}
          </Button>
        </DialogActions>
      </form>
    </Dialog>
  );
}

// formatDuration：不到 1 秒写毫秒，不到 1 分钟写秒（一位小数），再长写分和秒。
function formatDuration(ms: number): string {
  const d = t.audit.detail;
  if (ms < 1000) return d.durationMs(String(ms));
  if (ms < 60_000) return d.durationS((Math.round(ms / 100) / 10).toLocaleString());
  const s = Math.round(ms / 1000);
  return d.durationMin(Math.floor(s / 60), s % 60);
}
