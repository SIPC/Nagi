import { useInfiniteQuery, useQuery } from "@tanstack/react-query";
import { useNavigate, useSearch } from "@tanstack/react-router";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { useEffect, useMemo, useState } from "react";
import { ListFilter, Play, RefreshCw, Search, X } from "lucide-react";
import { Badge, Button, CodeBlock, Collapse, Dialog, DialogActions, EmptyRow, Field, FormBody, FormRow, HelpTip, Input, KeyValue, List, Notice, QueryError, Row, RowsSkeleton, Select, Spinner, Status, Table, Td, Th } from "@/components/ui";
import { Sheet, SheetBody, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import { RecordingDialog } from "@/components/recording";
import { errorMessage, logClient, nodeClient, projectClient, toDate } from "@/lib/api";
import { formatDateTime } from "@/lib/utils";
import type { LogEntry, LogPage, LogQuery, NodeLogQuery } from "@/gen/nagi/v1/logs_pb";
import { lacksLogService } from "@/pages/update";
import { t } from "@/i18n";

type Request = Pick<NodeLogQuery, "nodeId" | "panelLocal" | "recentCopy"> & { query: Pick<LogQuery, "category" | "kindPrefix" | "project" | "search" | "limit" | "from" | "to"> };
const categories = ["audit", "access", "container", "task", "security", "alert", "runtime", "host", "metrics", "logstore"] as const;
const categoryName = (category: string) => (t.logs.categories as Record<string, string>)[category] ?? category;
const decoder = new TextDecoder();
function body(e: LogEntry): Record<string, unknown> {
  try { const data = JSON.parse(decoder.decode(e.dataJson)); return data && typeof data === "object" ? data : { value: data }; }
  catch { return { value: decoder.decode(e.dataJson) }; }
}
function summary(e: LogEntry) {
  const data = body(e);
  return [data.message ?? data.text ?? data.action ?? data.error ?? "", e.object].filter(Boolean).join(" · ");
}

export function LogEntries() {
  const search = useSearch({ from: "/app/logs" });
  const navigate = useNavigate({ from: "/logs" });
  const nodes = useQuery({ queryKey: ["nodes"], queryFn: () => nodeClient.listNodes({}) });
  const projects = useQuery({ queryKey: ["projects"], queryFn: () => projectClient.listProjects({}) });
  const [draft, setDraft] = useState(search);
  useEffect(() => setDraft(search), [search]);
  const [revision, setRevision] = useState(0);
  const [selected, setSelected] = useState<LogEntry>();
  const [selectedSource, setSelectedSource] = useState("");
  const [replaying, setReplaying] = useState(false);
  const [filtering, setFiltering] = useState(false);
  // Pin the time window and source across all pages. Refresh starts a new query.
  const request = useMemo<Request>(() => ({
    panelLocal: !search.node,
    nodeId: search.node ?? "",
    recentCopy: !!search.node && (!!search.copy || search.node.startsWith("panel:")),
    query: {
      category: search.category === "host" ? (search.copy || search.node?.startsWith("panel:") ? "runtime" : "") : search.category || (search.copy || search.node?.startsWith("panel:") ? "audit" : ""),
      kindPrefix: search.category === "host" ? "host." : "",
      project: search.project ?? "", search: search.q ?? "", limit: 50,
      from: search.from && Number.isFinite(Date.parse(search.from)) ? timestampFromDate(new Date(search.from)) : undefined,
      to: search.to && Number.isFinite(Date.parse(search.to)) ? timestampFromDate(new Date(search.to)) : undefined,
    },
  }), [search.node, search.copy, search.category, search.project, search.q, search.from, search.to, revision]);
  useEffect(() => { setSelected(undefined); setReplaying(false); }, [request]);
  const invalid = !!(search.from && !Number.isFinite(Date.parse(search.from)) || search.to && !Number.isFinite(Date.parse(search.to)) || search.from && search.to && Date.parse(search.from) >= Date.parse(search.to));
  // 版本过旧的节点没有统一日志：不查，只显示一条升级提示（旧审计在「审计与录制」）。
  const tooOld = !request.recentCopy && lacksLogService(nodes.data?.nodes.find((node) => node.id === search.node)?.info?.version);
  const logs = useInfiniteQuery({
    // Protobuf timestamps contain bigint seconds; keep cache keys JSON-safe.
    queryKey: ["logs", search.node, search.copy, search.category, search.project, search.q, search.from, search.to, revision],
    initialPageParam: { source: "", after: 0n },
    queryFn: ({ pageParam, signal }) => logClient.queryNodeLogs({ ...request, query: { ...request.query, ...pageParam } }, { signal }),
    getNextPageParam: (last) => last.more ? { source: last.source, after: last.nextAfter } : undefined,
    // 选了节点时先等节点列表，才知道它是不是版本过旧。
    enabled: !invalid && !tooOld && (!search.node || !nodes.isPending), retry: false, refetchOnWindowFocus: false,
  });
  const pages = logs.data?.pages ?? [];
  const events = pages.flatMap((page) => page.entries);
  const state = pages.at(-1);
  const data = selected && body(selected);
  const nodeRecording = data?.recording && typeof data.recording === "object" && "id" in data.recording ? data.recording.id : undefined;
  const recordingNode = request.nodeId.startsWith("panel:") ? undefined : request.nodeId || nodes.data?.nodes.find((node) => node.ownEmbedded)?.id;
  const recording = request.nodeId.startsWith("panel:") ? undefined : data?.recording_id || (typeof nodeRecording === "string" && recordingNode ? `node:${recordingNode}:${nodeRecording}` : undefined);
  const offline = nodes.data?.nodes.find((node) => node.id === search.node)?.online === false;
  const reset = () => { setSelected(undefined); setRevision((v) => v + 1); };
  const sourceName = nodes.data?.nodes.find((node) => node.id === search.node)?.name || search.node || t.logs.panel;
  const projectName = (id: string) => projects.data?.projects.find(({ project }) => project?.id === id)?.project?.name || id;
  const closeFilters = () => { setFiltering(false); setDraft({ ...search, q: draft.q }); };
  const apply = () => {
    setSelected(undefined);
    navigate({ search: { node: draft.node, category: draft.category, copy: draft.node && draft.copy ? true : undefined, project: draft.project, q: draft.q, from: draft.from, to: draft.to }, replace: true });
  };
  const chips = [
    { key: "node" as const, label: search.node && sourceName },
    { key: "copy" as const, label: search.copy && t.logs.copy },
    { key: "category" as const, label: search.category && categoryName(search.category) },
    { key: "project" as const, label: search.project && projectName(search.project) },
    { key: "from" as const, label: search.from && `${t.logs.from}: ${search.from.replace("T", " ")}` },
    { key: "to" as const, label: search.to && `${t.logs.to}: ${search.to.replace("T", " ")}` },
  ].filter((chip) => chip.label);
  return <div className="flex min-w-0 flex-col gap-4">
    {!tooOld && <MetricsMigration request={request} online={nodes.data?.nodes.find((node) => node.id === request.nodeId)?.online === true} />}
    <form className="flex items-center gap-2" onSubmit={(event) => {
      event.preventDefault(); apply();
    }}>
      <Input id="log-search" type="search" aria-label={t.logs.keyword} autoComplete="off" className="min-w-0 flex-1 md:max-w-[280px]" value={draft.q ?? ""} placeholder={t.logs.searchHelp} onChange={(e) => setDraft({ ...draft, q: e.target.value || undefined })} />
      <Button readOnlyOk type="submit" size="icon" variant="outline" aria-label={t.logs.search} title={t.logs.search}><Search /></Button>
      <div className="ml-auto flex shrink-0 items-center gap-2">
        <Button readOnlyOk variant="outline" onClick={() => setFiltering(true)} aria-label={t.audit.filters.button}>
          <ListFilter /><span className="max-md:hidden">{t.audit.filters.button}</span>{chips.length > 0 && <span className="tabular-nums text-muted-foreground">{chips.length}</span>}
        </Button>
        <Button readOnlyOk size="icon" variant="ghost" onClick={reset} disabled={logs.isFetching} aria-label={t.logs.refresh} title={t.logs.refresh}><RefreshCw /></Button>
      </div>
    </form>
    {chips.length > 0 && <div className="flex flex-wrap items-center gap-2">
      {chips.map((chip) => <Badge key={chip.key} className="max-w-full gap-1 pr-1">
        <span className="truncate">{chip.label}</span>
        <button type="button" className="-my-1 -mr-1 inline-flex size-6 shrink-0 cursor-pointer items-center justify-center rounded-full text-muted-foreground outline-none hover:bg-accent hover:text-foreground focus-ring" aria-label={t.audit.filters.remove(String(chip.label))} onClick={() => navigate({ search: { ...search, [chip.key]: undefined, ...(chip.key === "node" ? { copy: undefined } : {}) }, replace: true })}><X className="size-3" /></button>
      </Badge>)}
      <Button readOnlyOk variant="ghost" onClick={() => navigate({ search: {}, replace: true })}>{t.audit.clearFilters}</Button>
    </div>}
    <Dialog open={filtering} onClose={closeFilters} title={t.audit.filters.title} className="md:max-w-[672px]">
      <form className="grid gap-6" onSubmit={(event) => { event.preventDefault(); apply(); setFiltering(false); }}>
        <FormBody>
          <Field label={t.logs.source} htmlFor="log-node"><Select id="log-node" value={draft.node?.startsWith("panel:") ? "panel:" : draft.node ?? ""} onChange={(e) => setDraft({ ...draft, node: e.target.value || undefined, copy: e.target.value === "panel:" || undefined })}>
            <option value="">{t.logs.panel}</option>
            <option value="panel:">{t.logs.panelCopy}</option>
            {nodes.data?.nodes.map((node) => <option key={node.id} value={node.id}>{node.name}{!node.online ? ` · ${t.logs.offline}` : ""}</option>)}
          </Select></Field>
          {draft.node?.startsWith("panel:") && <Field label={t.logs.panelFingerprint} help={t.logs.panelCopyHelp} htmlFor="log-panel"><Input id="log-panel" required value={draft.node.slice(6)} onChange={(e) => setDraft({ ...draft, node: `panel:${e.target.value.trim()}`, copy: true })} /></Field>}
          {draft.node && !draft.node.startsWith("panel:") && <Field label={t.logs.readFrom} help={t.logs.copyHelp} htmlFor="log-copy"><Select id="log-copy" value={draft.copy ? "copy" : "live"} onChange={(e) => setDraft({ ...draft, copy: e.target.value === "copy" || undefined, category: e.target.value === "copy" ? draft.category || "audit" : draft.category })}>
            <option value="live">{t.logs.live}</option><option value="copy">{t.logs.copy}</option>
          </Select></Field>}
          <FormRow>
            <Field label={t.audit.filters.category} htmlFor="log-category"><Select id="log-category" value={draft.category ?? (draft.copy || draft.node?.startsWith("panel:") ? "audit" : "")} onChange={(e) => setDraft({ ...draft, category: e.target.value || undefined })}>
              {!draft.copy && !draft.node?.startsWith("panel:") && <option value="">{t.audit.filters.allCategories}</option>}
              {categories.map((category) => <option key={category} value={category}>{categoryName(category)}</option>)}
            </Select></Field>
            <Field label={t.audit.filters.project} htmlFor="log-project"><Select id="log-project" value={draft.project ?? ""} onChange={(e) => setDraft({ ...draft, project: e.target.value || undefined })}>
              <option value="">{t.audit.filters.allProjects}</option>
              {projects.data?.projects.map(({ project }) => project && <option key={project.id} value={project.id}>{project.name}</option>)}
            </Select></Field>
          </FormRow>
          <FormRow>
            <Field label={t.logs.from} htmlFor="log-from"><Input id="log-from" type="datetime-local" value={draft.from ?? ""} onChange={(e) => setDraft({ ...draft, from: e.target.value || undefined })} /></Field>
            <Field label={t.logs.to} htmlFor="log-to"><Input id="log-to" type="datetime-local" value={draft.to ?? ""} onChange={(e) => setDraft({ ...draft, to: e.target.value || undefined })} /></Field>
          </FormRow>
        </FormBody>
        <DialogActions>
          <Button readOnlyOk type="button" variant="outline" onClick={closeFilters}>{t.common.actions.cancel}</Button>
          <Button readOnlyOk type="submit">{t.audit.filters.apply}</Button>
        </DialogActions>
      </form>
    </Dialog>
    {(nodes.error || projects.error) && <QueryError error={nodes.error || projects.error} />}
    {invalid && <Notice tone="danger">{t.logs.invalidTime}</Notice>}
    {tooOld && <Notice tone="warning">{t.logs.tooOld(sourceName)}</Notice>}
    {offline && !request.recentCopy && <Notice tone="warning" actions={<Button readOnlyOk variant="outline" onClick={() => navigate({ search: { ...search, copy: true, category: search.category || "audit" }, replace: true })}>{t.logs.useCopy}</Button>}>{t.logs.offlineHelp}</Notice>}
    {!invalid && !tooOld && <LogCoverage key={revision} request={request} state={state} onOpen={(e, source) => { setSelected(e); setSelectedSource(source); setReplaying(false); }} />}
    {logs.error && <QueryError error={logs.error} onRetry={reset} />}
    {tooOld ? null : !invalid && logs.isPending ? <RowsSkeleton count={5} /> : state && <>
      <div className="flex flex-wrap items-center justify-between gap-2 text-[13px] text-muted-foreground"><span>{sourceName}</span><span>{t.logs.order}</span></div>
      {!events.length ? <EmptyRow action={<Button readOnlyOk variant="outline" onClick={() => navigate({ search: {}, replace: true })}>{t.audit.clearFilters}</Button>}>{t.logs.empty}</EmptyRow> : <Table compact className="table-fixed">
        <thead><tr><Th className="md:w-1/2">{t.logs.event}</Th><Th className="hidden w-28 lg:table-cell">{t.audit.detail.category}</Th><Th className="hidden w-32 xl:table-cell">{t.audit.detail.project}</Th><Th className="hidden md:table-cell">{t.audit.detail.time}</Th></tr></thead>
        <tbody>{events.map((entry) => {
          const open = () => { setSelected(entry); setSelectedSource(state.source); setReplaying(false); };
          return <tr key={String(entry.seq)} className="cursor-pointer" onClick={open}>
            <Td className="align-top"><button type="button" className="block w-full rounded-sm py-1 text-left outline-none focus-ring" onClick={(event) => { event.stopPropagation(); open(); }}>
              <span className="flex min-w-0 items-center gap-2 leading-5"><span className="truncate">{entry.kind}</span>{body(entry).history_complete === false && <Status tone="warning">{t.logs.gap}</Status>}</span>
              <span className="block truncate text-[13px] leading-5 text-muted-foreground">{summary(entry) || "—"}</span>
              <span className="flex flex-wrap gap-x-2 text-[13px] leading-5 wrap-anywhere text-muted-foreground xl:hidden"><span className="lg:hidden">{categoryName(entry.category)}</span>{entry.project && <span>{projectName(entry.project)}</span>}<span className="tabular-nums md:hidden">{formatDateTime(toDate(entry.time))}</span></span>
            </button></Td>
            <Td className="hidden pt-2 align-top text-muted-foreground lg:table-cell">{categoryName(entry.category)}</Td>
            <Td className="hidden truncate pt-2 align-top text-muted-foreground xl:table-cell" title={entry.project}>{projectName(entry.project) || "—"}</Td>
            <Td className="hidden pt-2 align-top whitespace-nowrap text-muted-foreground tabular-nums md:table-cell">{formatDateTime(toDate(entry.time))}</Td>
          </tr>;
        })}</tbody>
      </Table>}
      <div className="flex flex-wrap items-center justify-between gap-2">
        {logs.hasNextPage && <Button readOnlyOk variant="ghost" className="-ml-3" disabled={logs.isFetchingNextPage} onClick={() => logs.fetchNextPage()}>{logs.isFetchingNextPage && <Spinner />}{t.audit.more}</Button>}
        <p className="min-w-0 wrap-anywhere text-[13px] text-muted-foreground">{t.logs.watermark(state.source, String(state.latestSeq))}</p>
      </div>
    </>}
    <Sheet open={!!selected} onOpenChange={(open) => { if (!open) { setSelected(undefined); setReplaying(false); } }}><SheetContent closeLabel={t.common.actions.close}>
      <SheetHeader><SheetTitle>{t.logs.detail}</SheetTitle><SheetDescription>{selected ? formatDateTime(toDate(selected.time)) : " "}</SheetDescription></SheetHeader>
      <SheetBody>{selected && <>
        {body(selected).history_complete === false && <Notice tone="warning">{t.logs.gapHelp}</Notice>}
        {body(selected).truncated === true && <Notice tone="warning">{t.logs.truncated}</Notice>}
        <KeyValue items={[[t.logs.event, selected.kind], [t.logs.source, <span className="font-mono text-[13px]">{selectedSource}</span>], [t.logs.sequence, <span className="tabular-nums">{String(selected.seq)}</span>], [t.audit.detail.category, categoryName(selected.category)], [t.audit.detail.project, selected.project || "—"], [t.audit.detail.target, selected.object || "—"], [t.logs.received, <span className="tabular-nums">{formatDateTime(toDate(selected.received))}</span>]]} />
        {typeof recording === "string" && recording && <div><Button readOnlyOk variant="outline" onClick={() => setReplaying(true)}><Play />{t.audit.replay.button}</Button><RecordingDialog id={recording} open={replaying} onClose={() => setReplaying(false)} /></div>}
        {nodeRecording && !recording && <Notice tone="warning">{t.logs.recordingNodeHelp}</Notice>}
        <div className="grid gap-2"><h3 className="text-sm font-medium">{t.logs.payload}</h3><CodeBlock className="max-h-80 overflow-auto">{JSON.stringify(body(selected), null, 2)}</CodeBlock></div>
      </>}</SheetBody>
    </SheetContent></Sheet>
  </div>;
}

function MetricsMigration({ request, online }: { request: Request; online: boolean }) {
  // Only poll the live source, independently of log filters and pagination.
  const enabled = !request.recentCopy && (request.panelLocal || online);
  const status = useQuery({
    queryKey: ["log-migrations", request.nodeId, request.panelLocal],
    queryFn: ({ signal }) => logClient.queryNodeLogs({ nodeId: request.nodeId, panelLocal: request.panelLocal, query: { limit: 1, migrationsOnly: true } }, { signal }),
    enabled, refetchInterval: 2000, retry: false, refetchOnWindowFocus: false, gcTime: 0,
  });
  if (request.recentCopy) return <Notice>{t.logs.migration.copyHelp}</Notice>;
  if (!enabled) return null;
  if (status.isError) return <Notice tone="warning" title={t.logs.migration.title}>{t.logs.migration.unavailable}<span className="block wrap-anywhere">{errorMessage(status.error)}</span></Notice>;
  const migration = status.data?.migrations?.find((item) => item.id === "metrics");
  if (!migration) return null;
  const { phase, completed, total, error } = migration;
  if (phase !== "pending" && phase !== "copying" && phase !== "cleanup" && phase !== "complete" && phase !== "failed") return null;
  const failed = phase === "failed";
  const complete = phase === "complete";
  const running = !failed && !complete;
  return <Notice tone={failed ? "danger" : complete ? "success" : "info"} title={<span className="inline-flex items-center gap-1">{t.logs.migration.title}<HelpTip>{t.logs.migration.scope}</HelpTip></span>}>
    <span className="inline-flex max-w-full items-center gap-2">{running && <Spinner className="shrink-0" />}{t.logs.migration.phases[phase]}</span>
    {phase === "copying" && <span className="block tabular-nums wrap-anywhere">{total > 0n ? t.logs.migration.progress(String(completed), String(total), String(completed * 100n / total)) : t.logs.migration.copied(String(completed))}</span>}
    {running && <span className="block">{t.logs.migration.background}</span>}
    {failed && <span className="block">{t.logs.migration.failedHelp}</span>}
    {failed && <span className="block whitespace-pre-wrap wrap-anywhere">{error || t.logs.migration.unknownError}</span>}
  </Notice>;
}

// Node-wide evidence intentionally ignores the user's text/project/category filters.
// Otherwise selecting container output would hide runtime collector interruptions.
function useGapEvidence(request: Request, category: string) {
  const query = { from: request.query?.from, to: request.query?.to, category, search: '"history_complete":false', limit: 10 };
  return useInfiniteQuery({
    queryKey: ["log-gaps", request.nodeId, request.panelLocal, request.recentCopy, category, query.from?.seconds.toString(), query.from?.nanos, query.to?.seconds.toString(), query.to?.nanos],
    initialPageParam: { source: "", after: 0n },
    queryFn: ({ pageParam, signal }) => logClient.queryNodeLogs({ ...request, query: { ...query, ...pageParam } }, { signal }),
    getNextPageParam: (last) => last.more ? { source: last.source, after: last.nextAfter } : undefined,
    retry: false, refetchOnWindowFocus: false,
  });
}

function LogCoverage({ request, state, onOpen }: { request: Request; state?: LogPage; onOpen: (entry: LogEntry, source: string) => void }) {
  // These queries stay mounted even while the explanation is collapsed, so gaps
  // still surface immediately and are never hidden by the main list's filters.
  const runtime = useGapEvidence(request, "runtime");
  const logstore = useGapEvidence(request, "logstore");
  const [open, setOpen] = useState(false);
  const evidence = [{ category: "runtime", result: runtime }, { category: "logstore", result: logstore }];
  const pruned = [...(state?.pruned ?? []).map((p) => ({ ...p, origin: request.recentCopy ? t.logs.copyPruned : t.logs.sourcePruned })), ...(state?.sourcePruned ?? []).map((p) => ({ ...p, origin: t.logs.sourcePruned }))];
  const issues = evidence.reduce((count, { result }) => count + (result.data?.pages.reduce((n, page) => n + page.entries.length, 0) ?? 0), 0) + pruned.length + (state && state.writeErrors > 0n ? 1 : 0);
  return <div className="grid gap-2">
    <Notice tone={issues || request.recentCopy ? "warning" : "neutral"} title={<span className="inline-flex items-center gap-1">{request.recentCopy ? t.logs.copy : issues ? t.logs.gap : t.logs.gaps}<HelpTip>{t.logs.coverage}</HelpTip></span>} actions={<Button readOnlyOk variant="ghost" className="-mr-3" onClick={() => setOpen(!open)} aria-expanded={open}>{open ? t.logs.hideCoverage : t.logs.showCoverage}</Button>}>
      {request.recentCopy ? <>{t.logs.copySummary}{issues > 0 && <> {t.logs.coverageIssues(issues)}</>}</> : issues ? t.logs.coverageIssues(issues) : t.logs.coverageSummary}
    </Notice>
    {evidence.map(({ category, result }) => result.error && <QueryError key={category} error={result.error} onRetry={() => result.refetch()} />)}
    <Collapse open={open}>
      <div className="grid gap-4 pt-2">
        <p className="text-muted-foreground">{request.recentCopy ? t.logs.copyHelp : t.logs.coverage}</p>
        {state && state.writeErrors > 0n && <Notice tone="warning">{t.logs.writeErrors(String(state.writeErrors))}</Notice>}
        {pruned.map((p) => <Notice key={`${p.origin}-${p.category}`} tone="warning">{t.logs.pruned(p.origin, categoryName(p.category), String(p.first), String(p.last), String(p.count))}</Notice>)}
        {evidence.map(({ category, result }) => <div key={category} className="grid gap-2">
          <h3 className="flex items-center gap-1 text-sm font-medium">{categoryName(category)}<HelpTip>{t.logs.collectionEvidence}</HelpTip></h3>
          {result.isPending ? <RowsSkeleton count={1} /> : !result.error && <>
            <List>{result.data?.pages.flatMap((page) => page.entries.map((entry) => <button key={`${page.source}-${entry.seq}`} type="button" className="block w-full rounded-md text-left outline-none focus-ring" onClick={() => onOpen(entry, page.source)}>
              <Row title={<span className="truncate">{entry.kind}</span>} sub={formatDateTime(toDate(entry.time))} />
            </button>))}</List>
            {!result.data?.pages.some((page) => page.entries.length) && <p className="text-muted-foreground">{t.logs.noGapEvidence}</p>}
            {result.hasNextPage && <div><Button readOnlyOk variant="ghost" className="-ml-3" disabled={result.isFetchingNextPage} onClick={() => result.fetchNextPage()}>{result.isFetchingNextPage && <Spinner />}{t.audit.more}</Button></div>}
          </>}
        </div>)}
      </div>
    </Collapse>
  </div>;
}
