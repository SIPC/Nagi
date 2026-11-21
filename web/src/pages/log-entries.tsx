import { useInfiniteQuery, useQuery } from "@tanstack/react-query";
import { useNavigate, useSearch } from "@tanstack/react-router";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { useEffect, useMemo, useState } from "react";
import { Play, RefreshCw, Search } from "lucide-react";
import { Badge, Button, CodeBlock, EmptyRow, Field, Input, KeyValue, List, Notice, QueryError, Row, RowsSkeleton, Select, Spinner } from "@/components/ui";
import { Sheet, SheetBody, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import { RecordingDialog } from "@/components/recording";
import { logClient, nodeClient, projectClient, toDate } from "@/lib/api";
import { formatDateTime } from "@/lib/utils";
import type { LogEntry, LogQuery, NodeLogQuery } from "@/gen/nagi/v1/logs_pb";
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
  // Pin the time window and source across all pages. Refresh starts a new query.
  const request = useMemo<Request>(() => ({
    panelLocal: !search.node,
    nodeId: search.node ?? "",
    recentCopy: !!search.node && !!search.copy,
    query: {
      category: search.category === "host" ? (search.copy ? "runtime" : "") : search.category || (search.copy ? "audit" : ""),
      kindPrefix: search.category === "host" ? "host." : "",
      project: search.project ?? "", search: search.q ?? "", limit: 50,
      from: search.from && Number.isFinite(Date.parse(search.from)) ? timestampFromDate(new Date(search.from)) : undefined,
      to: search.to && Number.isFinite(Date.parse(search.to)) ? timestampFromDate(new Date(search.to)) : undefined,
    },
  }), [search.node, search.copy, search.category, search.project, search.q, search.from, search.to, revision]);
  useEffect(() => { setSelected(undefined); setReplaying(false); }, [request]);
  const invalid = !!(search.from && !Number.isFinite(Date.parse(search.from)) || search.to && !Number.isFinite(Date.parse(search.to)) || search.from && search.to && Date.parse(search.from) >= Date.parse(search.to));
  const logs = useInfiniteQuery({
    // Protobuf timestamps contain bigint seconds; keep cache keys JSON-safe.
    queryKey: ["logs", search.node, search.copy, search.category, search.project, search.q, search.from, search.to, revision],
    initialPageParam: { source: "", after: 0n },
    queryFn: ({ pageParam, signal }) => logClient.queryNodeLogs({ ...request, query: { ...request.query, ...pageParam } }, { signal }),
    getNextPageParam: (last) => last.more ? { source: last.source, after: last.nextAfter } : undefined,
    enabled: !invalid, retry: false, refetchOnWindowFocus: false,
  });
  const pages = logs.data?.pages ?? [];
  const events = pages.flatMap((page) => page.entries);
  const state = pages.at(-1);
  const data = selected && body(selected);
  const nodeRecording = data?.recording && typeof data.recording === "object" && "id" in data.recording ? data.recording.id : undefined;
  const recordingNode = request.nodeId || nodes.data?.nodes.find((node) => node.ownEmbedded)?.id;
  const recording = data?.recording_id || (typeof nodeRecording === "string" && recordingNode ? `node:${recordingNode}:${nodeRecording}` : undefined);
  const offline = nodes.data?.nodes.find((node) => node.id === search.node)?.online === false;
  const reset = () => { setSelected(undefined); setRevision((v) => v + 1); };
  return <div className="flex min-w-0 flex-col gap-4">
    <form className="grid gap-3" onSubmit={(event) => {
      event.preventDefault(); setSelected(undefined);
      navigate({ search: { node: draft.node, category: draft.category, copy: draft.node && draft.copy ? true : undefined, project: draft.project, q: draft.q, from: draft.from, to: draft.to }, replace: true });
    }}>
      <div className="grid gap-3 md:grid-cols-3">
        <Field label={t.logs.source} htmlFor="log-node"><Select id="log-node" value={draft.node ?? ""} onChange={(e) => setDraft({ ...draft, node: e.target.value || undefined, copy: undefined })}>
          <option value="">{t.logs.panel}</option>
          {nodes.data?.nodes.map((node) => <option key={node.id} value={node.id}>{node.name}{!node.online ? ` · ${t.logs.offline}` : ""}</option>)}
        </Select></Field>
        <Field label={t.audit.filters.category} htmlFor="log-category"><Select id="log-category" value={draft.category ?? (draft.copy ? "audit" : "")} onChange={(e) => setDraft({ ...draft, category: e.target.value || undefined })}>
          {!draft.copy && <option value="">{t.audit.filters.allCategories}</option>}
          {categories.map((category) => <option key={category} value={category}>{categoryName(category)}</option>)}
        </Select></Field>
        <Field label={t.audit.filters.project} htmlFor="log-project"><Select id="log-project" value={draft.project ?? ""} onChange={(e) => setDraft({ ...draft, project: e.target.value || undefined })}>
          <option value="">{t.audit.filters.allProjects}</option>
          {projects.data?.projects.map(({ project }) => project && <option key={project.id} value={project.id}>{project.name}</option>)}
        </Select></Field>
      </div>
      <div className="grid gap-3 md:grid-cols-3">
        <Field label={t.logs.keyword} htmlFor="log-search"><Input id="log-search" value={draft.q ?? ""} placeholder={t.logs.searchHelp} onChange={(e) => setDraft({ ...draft, q: e.target.value || undefined })} /></Field>
        <Field label={t.logs.from} htmlFor="log-from"><Input id="log-from" type="datetime-local" value={draft.from ?? ""} onChange={(e) => setDraft({ ...draft, from: e.target.value || undefined })} /></Field>
        <Field label={t.logs.to} htmlFor="log-to"><Input id="log-to" type="datetime-local" value={draft.to ?? ""} onChange={(e) => setDraft({ ...draft, to: e.target.value || undefined })} /></Field>
      </div>
      <div className="flex flex-wrap items-end gap-2">
        {draft.node && <Field className="min-w-48" label={t.logs.readFrom} htmlFor="log-copy"><Select id="log-copy" value={draft.copy ? "copy" : "live"} onChange={(e) => setDraft({ ...draft, copy: e.target.value === "copy" || undefined, category: e.target.value === "copy" ? draft.category || "audit" : draft.category })}>
          <option value="live">{t.logs.live}</option><option value="copy">{t.logs.copy}</option>
        </Select></Field>}
        <Button readOnlyOk type="submit"><Search />{t.audit.filters.apply}</Button>
        <Button readOnlyOk variant="outline" onClick={reset} disabled={logs.isFetching}><RefreshCw />{t.logs.refresh}</Button>
        <Button readOnlyOk variant="ghost" onClick={() => navigate({ search: {}, replace: true })}>{t.audit.clearFilters}</Button>
        <span className="ml-auto self-center text-muted-foreground">{t.logs.order}</span>
      </div>
    </form>
    {(nodes.error || projects.error) && <QueryError error={nodes.error || projects.error} />}
    {invalid && <Notice tone="danger">{t.logs.invalidTime}</Notice>}
    {request.recentCopy && <Notice tone="warning" title={t.logs.copy}>{t.logs.copyHelp}</Notice>}
    {offline && !request.recentCopy && <Notice tone="warning" actions={<Button readOnlyOk variant="outline" onClick={() => navigate({ search: { ...search, copy: true, category: search.category || "audit" }, replace: true })}>{t.logs.useCopy}</Button>}>{t.logs.offlineHelp}</Notice>}
    <Notice>{t.logs.coverage}</Notice>
    {!invalid && <div><p className="mb-2 font-medium">{t.logs.gaps}</p>
      <GapEvidence key={`runtime-${revision}`} request={request} category="runtime" onOpen={(e, source) => { setSelected(e); setSelectedSource(source); }} />
      <GapEvidence key={`logstore-${revision}`} request={request} category="logstore" onOpen={(e, source) => { setSelected(e); setSelectedSource(source); }} />
    </div>}
    {state && <>
      <p className="wrap-anywhere text-xs text-muted-foreground">{t.logs.watermark(state.source, String(state.latestSeq))}</p>
      {state.writeErrors > 0n && <Notice tone="warning">{t.logs.writeErrors(String(state.writeErrors))}</Notice>}
      {[...state.pruned.map((p) => ({ ...p, origin: request.recentCopy ? t.logs.copyPruned : t.logs.sourcePruned })), ...state.sourcePruned.map((p) => ({ ...p, origin: t.logs.sourcePruned }))].map((p) => <Notice key={`${p.origin}-${p.category}`} tone="warning">{t.logs.pruned(p.origin, categoryName(p.category), String(p.first), String(p.last), String(p.count))}</Notice>)}
    </>}
    {logs.error && <QueryError error={logs.error} onRetry={reset} />}
    {!invalid && logs.isPending ? <RowsSkeleton count={5} /> : state && <>
      {!events.length ? <EmptyRow>{t.logs.empty}</EmptyRow> : <List>{events.map((entry) => <button key={String(entry.seq)} type="button" className="block w-full rounded-md text-left outline-none focus-ring" onClick={() => { setSelected(entry); setSelectedSource(state.source); setReplaying(false); }}>
        <Row title={<><Badge>{categoryName(entry.category)}</Badge><span className="truncate">{entry.kind}</span>{body(entry).history_complete === false && <Badge>{t.logs.gap}</Badge>}</>} sub={<span className="line-clamp-2 wrap-anywhere">{summary(entry) || "—"}</span>} cols={[{ content: formatDateTime(toDate(entry.time)), width: "w-48 text-right" }]} />
      </button>)}</List>}
      {logs.hasNextPage && <div><Button readOnlyOk variant="outline" disabled={logs.isFetchingNextPage} onClick={() => logs.fetchNextPage()}>{logs.isFetchingNextPage && <Spinner />}{t.audit.more}</Button></div>}
    </>}
    <Sheet open={!!selected} onOpenChange={(open) => { if (!open) { setSelected(undefined); setReplaying(false); } }}><SheetContent closeLabel={t.common.actions.close}>
      <SheetHeader><SheetTitle>{selected?.kind || t.logs.detail}</SheetTitle><SheetDescription>{selected ? formatDateTime(toDate(selected.time)) : " "}</SheetDescription></SheetHeader>
      <SheetBody>{selected && <>
        {body(selected).history_complete === false && <Notice tone="warning">{t.logs.gapHelp}</Notice>}
        {body(selected).truncated === true && <Notice tone="warning">{t.logs.truncated}</Notice>}
        <KeyValue items={[[t.logs.source, selectedSource], [t.logs.sequence, String(selected.seq)], [t.audit.detail.category, categoryName(selected.category)], [t.audit.detail.project, selected.project || "—"], [t.audit.detail.target, selected.object || "—"], [t.logs.received, formatDateTime(toDate(selected.received))]]} />
        {typeof recording === "string" && recording && <div><Button readOnlyOk variant="outline" onClick={() => setReplaying(true)}><Play />{t.audit.replay.button}</Button><RecordingDialog id={recording} open={replaying} onClose={() => setReplaying(false)} /></div>}
        {nodeRecording && !recording && <Notice tone="warning">{t.logs.recordingNodeHelp}</Notice>}
        <h3 className="font-medium">{t.logs.payload}</h3><CodeBlock className="max-h-[60vh] overflow-auto [&_code]:whitespace-pre-wrap [&_code]:break-all">{JSON.stringify(body(selected), null, 2)}</CodeBlock>
      </>}</SheetBody>
    </SheetContent></Sheet>
  </div>;
}

// Node-wide evidence intentionally ignores the user's text/project/category filters.
// Otherwise selecting container output would hide runtime collector interruptions.
function GapEvidence({ request, category, onOpen }: { request: Request; category: string; onOpen: (entry: LogEntry, source: string) => void }) {
  const query = { from: request.query?.from, to: request.query?.to, category, search: '"history_complete":false', limit: 10 };
  const gaps = useInfiniteQuery({
    queryKey: ["log-gaps", request.nodeId, request.panelLocal, request.recentCopy, category, query.from?.seconds.toString(), query.from?.nanos, query.to?.seconds.toString(), query.to?.nanos],
    initialPageParam: { source: "", after: 0n },
    queryFn: ({ pageParam, signal }) => logClient.queryNodeLogs({ ...request, query: { ...query, ...pageParam } }, { signal }),
    getNextPageParam: (last) => last.more ? { source: last.source, after: last.nextAfter } : undefined,
    retry: false, refetchOnWindowFocus: false,
  });
  return <><details className="py-1"><summary className="cursor-pointer text-muted-foreground">{categoryName(category)} · {t.logs.collectionEvidence} {gaps.data?.pages.some((p) => p.entries.length) && <Badge>{t.logs.gap}</Badge>}</summary>
    {gaps.isPending ? <RowsSkeleton count={1} /> : gaps.error ? <QueryError error={gaps.error} /> : <div className="mt-2 flex flex-col gap-2">
      {gaps.data?.pages.flatMap((p) => p.entries.map((e) => <button key={`${p.source}-${e.seq}`} type="button" className="wrap-anywhere text-left outline-none focus-ring" onClick={() => onOpen(e, p.source)}><Badge>{t.logs.gap}</Badge> {formatDateTime(toDate(e.time))} · {e.kind}</button>))}
      {!gaps.data?.pages.some((p) => p.entries.length) && <p className="text-muted-foreground">{t.logs.noGapEvidence}</p>}
      {gaps.hasNextPage && <Button readOnlyOk variant="outline" disabled={gaps.isFetchingNextPage} onClick={() => gaps.fetchNextPage()}>{t.audit.more}</Button>}
    </div>}
  </details>{gaps.error && <QueryError error={gaps.error} />}</>;
}
