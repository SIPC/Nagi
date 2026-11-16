// 审计日志（见 docs/audit.md#界面）：谁在什么时候、从哪里、通过什么做了什么。
// 管理员看全部；操作员和只读成员只看访问范围内项目的记录（面板按权限筛，IP 和浏览器不返回）。
import { useInfiniteQuery, useQuery } from "@tanstack/react-query";
import { useNavigate, useSearch } from "@tanstack/react-router";
import { useEffect, useMemo, useState } from "react";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { ChevronDown, ChevronRight, Download, Search } from "lucide-react";
import { Button, Check, CodeBlock, EmptyRow, Input, KeyValue, List, QueryError, Row, RowsSkeleton, Select, Spinner, Status } from "@/components/ui";
import { Sheet, SheetBody, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import { PageHeader } from "@/pages/layout";
import { useRole } from "@/components/role";
import type { ActivityEntry, AuditEvent, AuditFilter } from "@/gen/nagi/v1/api_pb";
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
  const target = [e.projectName || (e.projectId && e.projectId), e.targetName].filter(Boolean).join(" / ");
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
  const { isAdmin } = useRole();
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
      <div className="mb-4 grid gap-2">
        <div className="relative">
          <Search className="pointer-events-none absolute top-1/2 left-3 size-4 -translate-y-1/2 text-muted-foreground" />
          <Input className="pl-9" value={q} onChange={(e) => setQ(e.target.value)} placeholder={t.audit.search} aria-label={t.audit.search} />
        </div>
        <div className="flex flex-wrap items-center gap-2 max-md:grid max-md:grid-cols-2">
          <Select aria-label={t.audit.filters.range} className="w-36 max-md:w-full" value={search.range ?? ""} onChange={(e) => set({ range: (e.target.value || undefined) as AuditRange | undefined })}>
            <option value="">{t.audit.ranges.all}</option>
            {auditRanges.map((r) => (
              <option key={r} value={r}>
                {t.audit.ranges[r]}
              </option>
            ))}
          </Select>
          <Select aria-label={t.audit.filters.category} className="w-36 max-md:w-full" value={search.cat ?? ""} onChange={(e) => set({ cat: e.target.value || undefined })}>
            <option value="">{t.audit.filters.allCategories}</option>
            {auditCategories.map((c) => (
              <option key={c} value={c}>
                {t.audit.categories[c]}
              </option>
            ))}
          </Select>
          <Select aria-label={t.audit.filters.via} className="w-32 max-md:w-full" value={search.via ?? ""} onChange={(e) => set({ via: e.target.value || undefined })}>
            <option value="">{t.audit.filters.allVia}</option>
            {auditVias.map((v) => (
              <option key={v} value={v}>
                {t.audit.via[v]}
              </option>
            ))}
          </Select>
          {isAdmin && (
            <Select aria-label={t.audit.filters.member} className="w-36 max-md:w-full" value={search.member ? String(search.member) : ""} onChange={(e) => set({ member: e.target.value ? Number(e.target.value) : undefined })}>
              <option value="">{t.audit.filters.allMembers}</option>
              {(users.data?.users ?? []).map((u) => (
                <option key={String(u.id)} value={String(u.id)}>
                  {u.displayName || u.username}
                </option>
              ))}
            </Select>
          )}
          <Select aria-label={t.audit.filters.project} className="w-36 max-md:w-full" value={search.project ?? ""} onChange={(e) => set({ project: e.target.value || undefined })}>
            <option value="">{t.audit.filters.allProjects}</option>
            {(projects.data?.projects ?? []).map((p) => (
              <option key={p.project?.id} value={p.project?.id ?? ""}>
                {p.project?.name}
              </option>
            ))}
          </Select>
          {isAdmin && (
            <Select aria-label={t.audit.filters.node} className="w-36 max-md:w-full" value={search.node ?? ""} onChange={(e) => set({ node: e.target.value || undefined })}>
              <option value="">{t.audit.filters.allNodes}</option>
              {(nodes.data?.nodes ?? []).map((n) => (
                <option key={n.id} value={n.id}>
                  {n.name}
                </option>
              ))}
            </Select>
          )}
          <Check className="ml-2 max-md:ml-0" checked={!!search.failed} onChange={(v) => set({ failed: v || undefined })}>
            {t.audit.failedOnly}
          </Check>
        </div>
      </div>
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
      {open &&
        (children.isPending ? (
          <div className="pl-6">
            <RowsSkeleton count={Math.min(e.children, 3)} />
          </div>
        ) : children.error ? (
          <QueryError className="pl-6" error={errorMessage(children.error)} onRetry={() => children.refetch()} />
        ) : (
          [...(children.data?.events ?? [])].reverse().map((c) => <AuditRow key={String(c.id)} e={c} onOpen={onOpen} nested />)
        ))}
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
    if (e.durationMs > 0n) rows.push([d.duration, d.durationMs(e.durationMs.toLocaleString())]);
    rows.push([d.id, <span className="font-mono text-[13px]">{String(e.id)}</span>]);
    return rows;
  }, [e]);
  const params = prettyParams(e?.params ?? "");

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
