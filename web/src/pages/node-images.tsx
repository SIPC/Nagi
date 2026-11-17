// 节点页的「镜像」标签：列出节点上的镜像，删除没人用的，一键清理，拉取镜像（见 docs/agent.md#节点镜像）。只有管理员能看到。
// 能不能删由节点判断：有容器使用、Nagi 需要的镜像节点会拒绝，界面只是先置灰。
import { useContext, useEffect, useMemo, useRef, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { useNavigate, useSearch } from "@tanstack/react-router";
import { toast } from "sonner";
import { Code, ConnectError } from "@connectrpc/connect";
import { errorMessage, nodeClient, toDate } from "@/lib/api";
import { cn, formatDateTime, timeAgo } from "@/lib/utils";
import { t } from "@/i18n";
import { fmtBytes } from "@/components/metrics";
import { confirmAction } from "@/components/confirm";
import { SelectBox, SelectionBar, useSelection } from "@/components/file-manager";
import { Alert, Button, Check, Collapse, Dialog, DialogActions, EmptyRow, Field, HelpTip, Input, List, Notice, QueryError, ReadOnlyContext, Row, RowMenu, RowsSkeleton, Section, Select } from "@/components/ui";
import type { Node } from "@/gen/nagi/v1/api_pb";
import type { ImageHold, ImagePullEvent, NodeImage, NodeImages as NodeImageList } from "@/gen/nagi/v1/agent_pb";

// Ref：要删除的镜像（tag 为空）或标签。
type Ref = { id: string; tag: string };

const m = t.nodeImages;

type Sort = "size" | "created" | "name";
const sorts: Sort[] = ["size", "created", "name"];

const shortId = (id: string) => id.replace(/^sha256:/, "").slice(0, 12);
const imageName = (im: NodeImage) => im.tags[0] ?? "";
const blocked = (im: NodeImage) => im.users.length > 0 || im.holds.length > 0;

function holdLabel(h: ImageHold) {
  const kind = m.holds[h.kind as keyof typeof m.holds] ?? h.kind;
  return h.container ? `${kind} ${h.projectName || h.projectId} / ${h.container}` : kind;
}

// usage：使用者或 Nagi 需要的原因，写在第二行。
function usage(im: NodeImage) {
  if (im.users.length > 0) {
    const names = [...new Set(im.users.map((u) => (u.system ? m.nagiProxy : u.container ? `${u.projectName || u.projectId} / ${u.container}` : u.name)))];
    const text = m.usedBy(t.common.list(names));
    return im.users.some((u) => u.running) ? text : `${text} · ${m.stopped}`;
  }
  if (im.holds.length > 0) return m.needed(t.common.list([...new Set(im.holds.map(holdLabel))]));
  return m.unused;
}

function sortImages(list: NodeImage[], sort: Sort) {
  const out = [...list];
  if (sort === "created") out.sort((a, b) => Number(toDate(b.created)?.getTime() ?? 0) - Number(toDate(a.created)?.getTime() ?? 0));
  else if (sort === "name") out.sort((a, b) => (imageName(a) || "￿").localeCompare(imageName(b) || "￿") || a.id.localeCompare(b.id));
  else out.sort((a, b) => Number(b.size - a.size));
  return out;
}

function matches(im: NodeImage, q: string) {
  if (!q) return true;
  return [im.id, ...im.tags, ...im.digests, ...im.users.flatMap((u) => [u.name, u.projectName, u.container]), ...im.holds.flatMap((h) => [h.projectName, h.container])].some((v) => v?.toLowerCase().includes(q));
}

export function NodeImages({ node }: { node: Node }) {
  const readOnly = useContext(ReadOnlyContext);
  const search = useSearch({ strict: false }) as { q?: string; sort?: Sort };
  const navigate = useNavigate();
  const setSearch = (patch: { q?: string; sort?: Sort }) => navigate({ to: ".", search: ((s: Record<string, unknown>) => ({ ...s, ...patch })) as never, replace: true });
  const sort: Sort = sorts.includes(search.sort as Sort) ? (search.sort as Sort) : "size";
  const [query, setQuery] = useState(search.q ?? "");
  useEffect(() => {
    const id = setTimeout(() => setSearch({ q: query.trim() || undefined }), 300);
    return () => clearTimeout(id);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [query]);
  const supported = !!node.info?.imageOps;
  const q = useQuery({ queryKey: ["node-images", node.id], queryFn: () => nodeClient.listNodeImages({ nodeId: node.id }), enabled: supported });
  const [error, setError] = useState<string[]>([]);
  const [busy, setBusy] = useState(false);
  const [pruning, setPruning] = useState<NodeImageList | null>(null);
  const [pulling, setPulling] = useState(false);

  const all = q.data?.images ?? [];
  const needle = query.trim().toLowerCase();
  const shown = useMemo(() => sortImages(all, sort).filter((im) => matches(im, needle)), [all, sort, needle]);
  const selectable = useMemo(() => shown.filter((im) => !blocked(im)).map((im) => im.id), [shown]);
  const sel = useSelection(selectable, node.id);
  const selected = [...sel.selected];
  const total = all.reduce((n, im) => n + Number(im.size), 0);

  if (!supported) return <Notice title={m.oldTitle}>{m.oldText}</Notice>;

  const label = (id: string, tag?: string) => tag || all.find((x) => x.id === id)?.tags[0] || shortId(id);

  const remove = async (refs: Ref[], message: string, action: string) => {
    if (!(await confirmAction(message, { action, danger: true }))) return;
    setError([]);
    setBusy(true);
    try {
      const res = await nodeClient.deleteNodeImages({ nodeId: node.id, images: refs });
      const failed = res.results.filter((r) => r.error);
      const ok = res.results.length - failed.length;
      if (failed.length > 0) setError([m.failed(failed.length), ...failed.map((r) => `${label(r.id, r.tag)}：${r.error}`)]); // i18n-ignore
      if (ok > 0) toast.success(refs.length === 1 && refs[0].tag ? m.tagDeleted : m.deleted(ok));
      sel.clear();
    } catch (err) {
      setError([errorMessage(err)]);
    } finally {
      setBusy(false);
      q.refetch();
    }
  };
  const removeImage = (im: NodeImage) => remove([{ id: im.id, tag: "" }], m.deleteConfirm(imageName(im) || shortId(im.id)), m.deleteImage);
  const removeTag = (im: NodeImage, tag: string) => remove([{ id: im.id, tag }], m.deleteTagConfirm(tag), m.deleteTagAction);
  const removeSelected = () => {
    if (selected.length === 1) {
      const im = all.find((x) => x.id === selected[0]);
      if (im) return removeImage(im);
    }
    return remove(
      selected.map((id) => ({ id, tag: "" })),
      m.deleteManyConfirm(selected.length),
      m.deleteImage,
    );
  };
  // 清理前重新取一次列表：确认框里列出的就是节点这时算出的。
  const openPrune = async () => {
    setError([]);
    const res = await q.refetch();
    if (res.data) setPruning(res.data);
    else if (res.error) setError([errorMessage(res.error)]);
  };
  const copyId = async (id: string) => {
    try {
      await navigator.clipboard.writeText(id);
      toast.success(t.common.feedback.copied);
    } catch {
      toast.error(t.common.feedback.copyFailed);
    }
  };

  return (
    <Section
      title={m.title}
      help={m.help}
      actions={
        <>
          {q.data && (
            <span>
              {m.summary(all.length, fmtBytes(total))}
              {q.data.pruneIds.length > 0 && ` · ${m.reclaimable(fmtBytes(Number(q.data.pruneBytes)))}`}
            </span>
          )}
          <Button variant="outline" disabled={!q.data || busy} onClick={openPrune}>
            {m.prune}
          </Button>
          <Button variant="outline" onClick={() => setPulling(true)}>
            {m.pull}
          </Button>
        </>
      }
    >
      <div className="flex items-center justify-between gap-2">
        <Input value={query} onChange={(e) => setQuery(e.target.value)} placeholder={m.filter} aria-label={m.filter} autoComplete="off" spellCheck={false} className="max-w-[280px] max-md:max-w-none max-md:min-w-0 max-md:flex-1" />
        <Select value={sort} onChange={(e) => setSearch({ sort: e.target.value === "size" ? undefined : (e.target.value as Sort) })} aria-label={m.sort} className="w-auto">
          <option value="size">{m.bySize}</option>
          <option value="created">{m.byCreated}</option>
          <option value="name">{m.byName}</option>
        </Select>
      </div>
      {error.length > 0 && (
        <Alert>
          {error.map((line, i) => (
            <span key={i} className="block">
              {line}
            </span>
          ))}
        </Alert>
      )}
      <Collapse open={selected.length > 0} gap={4}>
        {selected.length > 0 && (
          <SelectionBar count={selected.length} total={selectable.length} onAll={(on) => sel.setAll(on)} onClear={sel.clear} actions={[{ label: t.common.actions.delete, danger: true, disabled: busy || readOnly, onSelect: removeSelected }]} />
        )}
      </Collapse>
      {q.isPending ? (
        <RowsSkeleton count={3} />
      ) : q.error ? (
        <QueryError error={errorMessage(q.error)} onRetry={() => q.refetch()} />
      ) : shown.length === 0 ? (
        <EmptyRow>{all.length === 0 ? m.empty : m.noMatch}</EmptyRow>
      ) : (
        <List>
          {shown.map((im) => {
            const name = imageName(im);
            const lock = blocked(im);
            return (
              <Row
                key={im.id}
                icon={<SelectBox name={name || shortId(im.id)} checked={sel.selected.has(im.id)} disabled={lock || readOnly} onToggle={(range) => sel.toggle(im.id, range)} />}
                title={
                  <>
                    <span className={cn("truncate", !name && "text-muted-foreground")} title={im.tags.join("\n") || undefined}>
                      {name || m.untagged}
                    </span>
                    {im.tags.length > 1 && <span className="shrink-0 text-muted-foreground">{m.moreTags(im.tags.length - 1)}</span>}
                  </>
                }
                sub={
                  <span className="md:block md:truncate" title={usage(im)}>
                    <span className="font-mono">{shortId(im.id)}</span>
                    {` · ${usage(im)}`}
                  </span>
                }
                cols={[
                  { content: <span title={im.uniqueSize !== im.size ? m.sizeTitle(fmtBytes(Number(im.uniqueSize))) : undefined}>{fmtBytes(Number(im.size))}</span>, num: true },
                  { content: <span title={m.createdTitle(formatDateTime(toDate(im.created)) ?? "")}>{timeAgo(toDate(im.created))}</span> },
                ]}
                actionsWidth="w-8"
                actions={
                  <RowMenu
                    title={name || shortId(im.id)}
                    items={[
                      { label: m.copyId, readOnlyOk: true, onSelect: () => copyId(im.id) },
                      ...(im.tags.length > 1 ? im.tags.map((tag) => ({ label: m.deleteTag(tag), disabled: lock || busy, hint: lock ? (im.users.length > 0 ? m.inUseHint : m.neededHint) : undefined, onSelect: () => removeTag(im, tag) })) : []),
                      { label: m.deleteImage, danger: true, disabled: lock || busy, hint: lock ? (im.users.length > 0 ? m.inUseHint : m.neededHint) : undefined, onSelect: () => removeImage(im) },
                    ]}
                  />
                }
              />
            );
          })}
        </List>
      )}
      <Dialog open={!!pruning} onClose={() => !busy && setPruning(null)} dismissible={!busy} title={m.pruneDialog.title} description={m.pruneDialog.description}>
        {pruning && (
          <PruneImages
            nodeId={node.id}
            list={pruning}
            busy={busy}
            setBusy={setBusy}
            onDone={(lines) => {
              setPruning(null);
              setError(lines);
              q.refetch();
            }}
            onCancel={() => setPruning(null)}
          />
        )}
      </Dialog>
      <Dialog open={pulling} onClose={() => setPulling(false)} title={m.pullDialog.title} description={m.pullDialog.description(node.name)} className="sm:max-w-[520px]">
        {pulling && <PullImage nodeId={node.id} onClose={() => setPulling(false)} onPulled={() => q.refetch()} />}
      </Dialog>
    </Section>
  );
}

// PruneImages：清理的确认框。列出节点算出的可清理镜像和能释放的空间，确认后只删这些（节点会再核对一次）。
function PruneImages({ nodeId, list, busy, setBusy, onDone, onCancel }: { nodeId: string; list: NodeImageList; busy: boolean; setBusy: (b: boolean) => void; onDone: (errors: string[]) => void; onCancel: () => void }) {
  const d = m.pruneDialog;
  const candidates = list.images.filter((im) => list.pruneIds.includes(im.id));
  const cache = Number(list.buildCacheBytes);
  const [withCache, setWithCache] = useState(cache > 0);
  const [error, setError] = useState("");
  const nothing = candidates.length === 0 && cache === 0;
  const run = async () => {
    setError("");
    setBusy(true);
    try {
      const res = await nodeClient.pruneNodeImages({ nodeId, ids: list.pruneIds, buildCache: withCache && cache > 0 });
      toast.success(withCache && cache > 0 ? d.done(res.deleted, fmtBytes(Number(res.buildCacheFreed))) : d.doneImages(res.deleted));
      const lines = res.failed.map((f) => (f.id ? `${shortId(f.id)}：${f.error}` : f.error)); // i18n-ignore
      if (res.skipped > 0) lines.unshift(d.skipped(res.skipped));
      onDone(lines);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };
  if (nothing) {
    return (
      <>
        <p className="text-muted-foreground">{d.nothing}</p>
        <DialogActions>
          <Button variant="outline" readOnlyOk onClick={onCancel}>
            {t.common.actions.close}
          </Button>
        </DialogActions>
      </>
    );
  }
  return (
    <>
      {candidates.length > 0 && (
        <div className="grid gap-2">
          <div className="flex items-center gap-1 font-medium">
            {d.images(candidates.length)}
            <span className="font-normal text-muted-foreground">{` · ${d.freed(fmtBytes(Number(list.pruneBytes)))}`}</span>
            <HelpTip>{d.freedHelp}</HelpTip>
          </div>
          <div className="grid max-h-56 gap-1 overflow-y-auto rounded-md border px-3 py-2 text-[13px]">
            {candidates.map((im) => (
              <div key={im.id} className="flex min-w-0 justify-between gap-4">
                <span className={cn("truncate", !im.tags[0] && "text-muted-foreground")}>{im.tags[0] ? t.common.list(im.tags) : `${m.untagged} · ${shortId(im.id)}`}</span>
                <span className="shrink-0 text-muted-foreground tabular-nums">{fmtBytes(Number(im.size))}</span>
              </div>
            ))}
          </div>
        </div>
      )}
      {cache > 0 && (
        <Check checked={withCache} onChange={setWithCache}>
          {d.buildCache(fmtBytes(cache))}
        </Check>
      )}
      {error && <Alert>{error}</Alert>}
      <DialogActions>
        <Button variant="outline" disabled={busy} onClick={onCancel} autoFocus>
          {t.common.actions.cancel}
        </Button>
        <Button variant="destructive" disabled={busy || (candidates.length === 0 && !withCache)} onClick={run}>
          {d.action}
        </Button>
      </DialogActions>
    </>
  );
}

// PullImage：拉取镜像的对话框。拉取中显示进度，「取消拉取」或关掉对话框时中断请求，节点随之停止拉取。
function PullImage({ nodeId, onClose, onPulled }: { nodeId: string; onClose: () => void; onPulled: () => void }) {
  const d = m.pullDialog;
  const [image, setImage] = useState("");
  const [fieldError, setFieldError] = useState("");
  const [error, setError] = useState("");
  const [state, setState] = useState<"idle" | "pulling" | "done">("idle");
  const [progress, setProgress] = useState<ImagePullEvent | null>(null);
  const [pulled, setPulled] = useState("");
  const ctrl = useRef<AbortController | null>(null);
  // 对话框关掉（卸载）时中断还在进行的拉取。
  useEffect(() => () => ctrl.current?.abort(), []);
  const start = async () => {
    const ref = image.trim();
    if (!ref) {
      setFieldError(d.required);
      return;
    }
    setFieldError("");
    setError("");
    setProgress(null);
    const c = new AbortController();
    ctrl.current = c;
    setState("pulling");
    try {
      for await (const ev of nodeClient.pullNodeImage({ nodeId, image: ref }, { signal: c.signal })) {
        setProgress(ev);
        if (ev.done) {
          setPulled(ref);
          setState("done");
          onPulled();
        }
      }
    } catch (err) {
      if (c.signal.aborted) {
        toast(d.canceled);
        setState("idle");
        return;
      }
      // 镜像格式不对时写在字段下面。
      if (err instanceof ConnectError && err.code === Code.InvalidArgument) setFieldError(errorMessage(err));
      else setError(errorMessage(err));
      setState("idle");
    } finally {
      if (ctrl.current === c) ctrl.current = null;
    }
  };
  const total = Number(progress?.total ?? 0);
  const current = Number(progress?.current ?? 0);
  const pct = state === "done" ? 100 : total > 0 ? Math.min(100, (100 * current) / total) : 0;
  return (
    <form
      className="grid gap-4"
      onSubmit={(e) => {
        e.preventDefault();
        if (state === "idle") start();
      }}
    >
      <Field label={d.image} help={d.imageHelp} htmlFor="pull-image" error={fieldError}>
        <Input
          id="pull-image"
          value={image}
          onChange={(e) => setImage(e.target.value)}
          placeholder="nginx:1.27"
          autoComplete="off"
          autoCapitalize="none"
          spellCheck={false}
          disabled={state !== "idle"}
          className="font-mono"
        />
      </Field>
      {state !== "idle" && (
        <div className="grid gap-1.5">
          <span className="block h-1 overflow-hidden rounded-full bg-muted" role="progressbar" aria-valuemin={0} aria-valuemax={100} aria-valuenow={Math.round(pct)} aria-label={d.title}>
            <span className="block h-full rounded-full bg-primary transition-[width] duration-300" style={{ width: `${pct}%` }} />
          </span>
          <span className="flex min-w-0 justify-between gap-4 text-[13px] leading-5 text-muted-foreground tabular-nums">
            <span className="truncate">{state === "done" ? d.done(pulled) : progress?.status || d.starting}</span>
            {progress && progress.layers > 0 && state !== "done" && (
              <span className="shrink-0">{[total > 0 && `${fmtBytes(current)} / ${fmtBytes(total)}`, d.layers(progress.layersDone, progress.layers)].filter(Boolean).join(" · ")}</span>
            )}
          </span>
        </div>
      )}
      {error && <Alert keepFocus>{error}</Alert>}
      <DialogActions>
        {state === "pulling" ? (
          <Button variant="outline" readOnlyOk onClick={() => ctrl.current?.abort()}>
            {d.cancel}
          </Button>
        ) : state === "done" ? (
          <Button readOnlyOk onClick={onClose}>
            {t.common.actions.done}
          </Button>
        ) : (
          <>
            <Button variant="outline" readOnlyOk onClick={onClose}>
              {t.common.actions.cancel}
            </Button>
            <Button type="submit">{d.action}</Button>
          </>
        )}
      </DialogActions>
    </form>
  );
}
