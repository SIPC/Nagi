// 节点访问：节点的终端和文件（见 docs/node-access.md）。规则在节点和面板上执行，这里只是把用不了的入口藏起来、把原因说清楚。
// 文件页和卷的文件管理共用 components/file-manager 的多选、剪贴板、改名、压缩解压、分块上传和目录大小。
import { Code, ConnectError } from "@connectrpc/connect";
import { directEndpoint, directLink, saveFrom } from "@/lib/direct-download";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { File, Folder, Link as LinkIcon } from "lucide-react";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { toast } from "sonner";
import { LineDiff } from "@/components/config-diff";
import { ask, confirmAction } from "@/components/confirm";
import { isElevationRequired, requireElevation } from "@/components/elevate";
import { ClipboardBar, DirSizeText, formatModified, useDirSizes, type FileOpsAdapter, freeName, isArchive, PendingUploads, SelectBox, SelectionBar, streamOp, UploadList, useFileOps, useSelection, useUploads } from "@/components/file-manager";
import type { UploadTarget } from "@/lib/chunked-upload";
import { fmtBytes } from "@/components/metrics";
import { TerminalSession } from "@/components/terminal";
import { Alert, Button, Collapse, Dialog, EmptyRow, HelpTip, Input, List, Menu, Notice, QueryError, Row, RowMenu, RowsSkeleton, Spinner, Textarea, CodeBlock } from "@/components/ui";
import { useUnsaved } from "@/components/unsaved";
import { MobileBar, mobileBarText } from "@/pages/layout";
import { Crumbs } from "@/pages/volume-files";
import type { Node } from "@/gen/nagi/v1/api_pb";
import type { NodeFileEntry, ReadNodeFileResponse } from "@/gen/nagi/v1/agent_pb";
import { locale, t } from "@/i18n";
import { errorMessage, nodeClient, toDate } from "@/lib/api";
import { cn, eachLimit, formatDateTime, uploadConcurrency } from "@/lib/utils";

const pageSize = 200;
const maxEditBytes = 1 << 20;
const maxUploadBytes = 8 * 1024 ** 3;

// NodeAccessGate：节点访问用不了时说明原因。能用时返回 null。
export function nodeAccessBlock(n: Node) {
  const m = t.nodeAccess.gate;
  if (n.info?.nodeAccessDisabled) {
    return (
      <div className="grid gap-3">
        <Notice title={m.disabledTitle}>{m.disabledText}</Notice>
        <CodeBlock copy nowrap>nagi node access enable</CodeBlock>
      </div>
    );
  }
  if (!n.info?.nodeAccess) return <Notice title={m.oldTitle}>{m.oldText}</Notice>;
  return null;
}

function ContainerHint({ n }: { n: Node }) {
  if (!n.info?.inContainer) return null;
  return <Notice tone="warning" title={t.nodeAccess.gate.inContainer} />;
}

// NodeTerminal：节点的终端。cwd 是「在此打开终端」传过来的目录。
export function NodeTerminal({ node, cwd }: { node: Node; cwd?: string }) {
  const m = t.nodeAccess.terminal;
  const [opened, setOpened] = useState<string | undefined>(undefined);
  const [connected, setConnected] = useState(false);
  const cwdRef = useRef(cwd);
  cwdRef.current = cwd;
  return (
    <div className="grid gap-4">
      <ContainerHint n={node} />
      {connected && cwd && cwd !== opened && <p className="leading-5 text-muted-foreground">{m.otherCwd(cwd)}</p>}
      <TerminalSession
        url={() => `/ws/node-shell?${new URLSearchParams({ node: node.id, cwd: cwdRef.current ?? "" })}`}
        beforeConnect={requireElevation}
        intro={m.intro}
        label={`root · ${node.name}`}
        onSession={() => {
          setOpened(cwdRef.current);
          setConnected(true);
        }}
      />
    </div>
  );
}

type Crumb = { label: string; path: string };

function crumbsOf(path: string): Crumb[] {
  const parts = path.split("/").filter(Boolean);
  return parts.map((label, i) => ({ label, path: "/" + parts.slice(0, i + 1).join("/") }));
}

const join = (dir: string, name: string) => (dir === "/" ? `/${name}` : `${dir}/${name}`);
const parentOf = (p: string) => p.slice(0, p.lastIndexOf("/")) || "/";

// useElevated：进入页面先确认一次身份。取消时显示重试按钮。
function useElevated() {
  const [state, setState] = useState<"checking" | "ok" | "cancelled">("checking");
  const check = useCallback(async () => {
    setState("checking");
    try {
      setState((await requireElevation()) ? "ok" : "cancelled");
    } catch {
      setState("cancelled");
    }
  }, []);
  useEffect(() => {
    check();
  }, [check]);
  return { state, check };
}

// NodeFiles：节点上的文件。path 是当前目录（地址的 path 参数）。和卷的文件管理同一种写法（docs/ui.md 文件管理）。
export function NodeFiles({ node, path, onPath, onTerminal, onExit }: { node: Node; path: string; onPath: (p: string) => void; onTerminal: (p: string) => void; onExit: () => void }) {
  const m = t.nodeAccess.files;
  const { state, check } = useElevated();
  if (state === "checking") return <RowsSkeleton />;
  if (state === "cancelled") {
    return (
      <Notice
        title={m.needVerify}
        actions={
          <Button variant="outline" readOnlyOk onClick={check}>
            {m.verify}
          </Button>
        }
      />
    );
  }
  return <Browser node={node} path={path} onPath={onPath} onTerminal={onTerminal} onExit={onExit} onExpired={check} />;
}

// accessLabel：路径的访问级别，写成灰字。普通路径不写。
function accessLabel(access: string) {
  const m = t.nodeAccess.files;
  return access === "denied" ? m.deniedHint : access === "readonly" ? m.readonlyHint : access === "confirm" ? m.sensitiveHint : "";
}

function Browser({ node, path, onPath, onTerminal, onExit, onExpired }: { node: Node; path: string; onPath: (p: string) => void; onTerminal: (p: string) => void; onExit: () => void; onExpired: () => void }) {
  const m = t.nodeAccess.files;
  const fm = t.fileManager;
  const queryClient = useQueryClient();
  const [editing, setEditing] = useState<{ file: ReadNodeFileResponse; text: string } | null>(null);
  const [creating, setCreating] = useState<"dir" | "file" | null>(null);
  const [name, setName] = useState("");
  const [goto, setGoto] = useState<string | null>(null);
  const [busy, setBusy] = useState("");
  const [error, setError] = useState("");
  const [shown, setShown] = useState(pageSize);
  const inputRef = useRef<HTMLInputElement>(null);
  const nameRef = useRef<HTMLInputElement>(null);
  const gotoRef = useRef<HTMLInputElement>(null);
  // 节点支持改名、复制、压缩解压、目录大小和断点续传（旧版本的节点不支持）。
  const ops = !!node.info?.fileOps;
  const scope = `n:${node.id}`;
  const q = useQuery({
    queryKey: ["node-dir", node.id, path],
    queryFn: () => nodeClient.listNodeDir({ nodeId: node.id, path }),
    retry: false,
  });
  useEffect(() => {
    if (isElevationRequired(q.error)) onExpired();
  }, [q.error, onExpired]);
  useEffect(() => {
    setShown(pageSize);
    setError("");
    setGoto(null);
    setCreating(null);
  }, [path]);
  // 从菜单打开时，菜单关闭后会把焦点还给按钮，所以稍后再聚焦。
  useEffect(() => {
    if (!creating) return;
    const id = setTimeout(() => nameRef.current?.focus(), 50);
    return () => clearTimeout(id);
  }, [creating]);
  const gotoOpen = goto !== null;
  useEffect(() => {
    if (!gotoOpen) return;
    const id = setTimeout(() => gotoRef.current?.focus(), 50);
    return () => clearTimeout(id);
  }, [gotoOpen]);
  // 符号链接解开后的真实路径。
  const real = q.data?.path ?? path;
  const readonly = q.data?.access === "readonly";
  const entries = q.data?.entries ?? [];
  const names = useMemo(() => entries.map((e) => e.name), [entries]);
  const dirNames = entries.filter((e) => e.isDir && !e.isSymlink && e.access !== "denied").map((e) => e.name);
  // 目录大小在列表出来以后再统计，不拖慢列表。
  const dirSize = useDirSizes(["node-dir-sizes", node.id, real], dirNames, ops && !!q.data, (names, full) =>
    nodeClient.listNodeDirSizes({ nodeId: node.id, path: real, names, full }),
  );
  const refresh = () => {
    queryClient.invalidateQueries({ queryKey: ["node-dir", node.id] });
    queryClient.invalidateQueries({ queryKey: ["node-dir-sizes", node.id] });
  };

  // 上传、删除、新建的结果在列表里看得到，不再弹成功提示（ui.md：页面上已能看到变化的不加）。
  const run = async (key: string, fn: () => Promise<unknown>) => {
    setBusy(key);
    setError("");
    try {
      await fn();
      refresh();
      return true;
    } catch (err) {
      if (isElevationRequired(err)) onExpired();
      else setError(errorMessage(err));
      return false;
    } finally {
      setBusy("");
    }
  };

  const fileURL = (p: string, confirmed = false) => {
    const query = new URLSearchParams({ node: node.id, path: p });
    if (confirmed) query.set("confirmed", "1");
    return `/api/node-file?${query}`;
  };
  const failure = async (res: Response) => {
    if (res.headers.get("X-Nagi-Elevation") === "required") {
      onExpired();
      return new Error(t.nodeAccess.elevate.expired);
    }
    return new Error((await res.json().catch(() => null))?.message || t.common.errors.http(String(res.status)));
  };
  // 确认后交给浏览器下载，大文件可以断点续传；节点能直连时直接从节点下载（见 lib/direct-download.ts）。
  const download = (p: string) =>
    run(`download:${p}`, async () => {
      const res = await fetch(`${fileURL(p)}&check=1`, { headers: { "Accept-Language": locale } });
      if (!res.ok) throw await failure(res);
      saveFrom((await directLink(directEndpoint(fileURL(p)))) ?? fileURL(p));
    });

  // withSensitive：动到敏感路径时先确认。看得出来的（当前目录或选中的项是敏感的）先问；看不出来的由节点拒绝，问过再重试。
  const confirmedRef = useRef(false);
  const withSensitive = async <T,>(known: boolean, label: string, first: boolean, fn: (confirmed: boolean) => Promise<T>): Promise<T> => {
    if (first) confirmedRef.current = false;
    if (known && !confirmedRef.current) {
      if (!(await confirmAction(m.sensitiveOp(label), { action: fm.continue, danger: true }))) throw new DOMException("aborted", "AbortError");
      confirmedRef.current = true;
    }
    try {
      return await fn(confirmedRef.current);
    } catch (err) {
      if (confirmedRef.current || !(err instanceof ConnectError) || err.code !== Code.FailedPrecondition) throw err;
      if (!(await confirmAction(m.sensitiveOp(label), { action: fm.continue, danger: true }))) throw new DOMException("aborted", "AbortError");
      confirmedRef.current = true;
      return fn(true);
    }
  };
  const sensitiveIn = (paths: string[]) => q.data?.access === "confirm" || paths.some((p) => entries.find((e) => join(real, e.name) === p)?.access === "confirm");
  const adapter: FileOpsAdapter = {
    scope,
    join,
    rename: (p, newName) => withSensitive(sensitiveIn([p]), p, true, (confirmed) => nodeClient.renameNodeFile({ nodeId: node.id, paths: [p], destDir: parentOf(p), newName, confirmed })),
    transfer: (paths, destDir, copy, o) =>
      withSensitive(sensitiveIn(copy ? [] : paths), destDir, o.conflict === "", (confirmed) =>
        streamOp((copy ? nodeClient.copyNodeFiles : nodeClient.moveNodeFiles)({ nodeId: node.id, paths, destDir, conflict: o.conflict, confirmed }, { signal: o.signal }), o),
      ),
    compress: (paths, dest, format, o) => withSensitive(sensitiveIn([]), dest, o.conflict === "", (confirmed) => streamOp(nodeClient.compressNodeFiles({ nodeId: node.id, paths, dest, format, conflict: o.conflict, confirmed }, { signal: o.signal }), o)),
    extract: (p, destDir, intoNewDir, o) =>
      withSensitive(sensitiveIn([]), destDir, o.conflict === "", (confirmed) => streamOp(nodeClient.extractNodeFile({ nodeId: node.id, path: p, destDir, intoNewDir, conflict: o.conflict, confirmed }, { signal: o.signal }), o)),
    onError: (err) => {
      if (err instanceof DOMException && err.name === "AbortError") return true;
      if (!isElevationRequired(err)) return false;
      onExpired();
      return true;
    },
    overwriteHint: m.overwriteHint,
  };
  const fops = useFileOps(adapter, { dir: real, entries, refresh, setError });
  const sel = useSelection(names, real);
  const uploads = useUploads(scope);
  const sensitiveDir = q.data?.access === "confirm";
  const target: UploadTarget = { url: (p, params) => `${fileURL(p, sensitiveDir || entries.find((e) => join(real, e.name) === p)?.access === "confirm")}&${new URLSearchParams(params)}`, fail: async (res) => (res.headers.get("X-Nagi-Elevation") === "required" ? failure(res) : undefined) };

  const upload = async (list: File[]) => {
    if (!ops) {
      const tooLarge = list.filter((f) => f.size > maxUploadBytes).map((f) => f.name);
      if (tooLarge.length > 0) {
        setError(m.tooLarge(t.common.list(tooLarge)));
        return;
      }
    }
    const existing = new Map(entries.map((e) => [e.name, e]));
    const clash = list.filter((f) => existing.has(f.name));
    const sensitive = sensitiveDir || clash.some((f) => existing.get(f.name)?.access === "confirm");
    let picked = list.map((file) => ({ file, name: file.name }));
    if (ops) {
      if (clash.length > 0) {
        const answer = await fops.askConflict(clash.map((f) => f.name));
        if (!answer) return;
        const taken = new Set(names);
        if (answer.choice === "skip") picked = picked.filter((x) => !taken.has(x.name));
        if (answer.choice === "rename") {
          picked = picked.map((x) => {
            if (!taken.has(x.name)) return x;
            const n = freeName(x.name, taken);
            taken.add(n);
            return { ...x, name: n };
          });
        }
      }
      if (sensitive && !(await confirmAction(m.sensitiveOp(real), { action: fm.continue, danger: true }))) return;
      if (picked.length === 0) return;
      setError("");
      await uploads.queue.add(
        picked.map((x) => ({ file: x.file, path: join(real, x.name) })),
        target,
      );
      refresh();
      return;
    }
    if (clash.length > 0 || sensitive) {
      const names = t.common.list((clash.length ? clash : list).map((f) => f.name));
      const large = clash.some((f) => Number(existing.get(f.name)?.size ?? 0) > maxEditBytes);
      if (!(await confirmAction(m.overwriteConfirm(names, large), { action: m.overwrite, danger: true }))) return;
    }
    await run("upload", async () => {
      await eachLimit(list, uploadConcurrency, async (f) => {
        const res = await fetch(fileURL(join(real, f.name), sensitive), { method: "POST", headers: { "X-Nagi-Upload": "1", "Accept-Language": locale }, body: f });
        if (!res.ok) throw await failure(res);
      });
    });
  };

  const remove = async (e: NodeFileEntry) => {
    const typed = (e.isDir && !e.isSymlink) || e.access === "confirm";
    const answer = await ask(m.deleteConfirm(e.name, e.isDir && !e.isSymlink), {
      action: t.common.actions.delete,
      danger: true,
      ...(typed ? { checkDefault: true, typeToConfirm: { label: m.deleteType(e.name), value: e.name } } : {}),
    });
    if (!answer) return;
    await run(`del:${e.name}`, () => nodeClient.deleteNodeFile({ nodeId: node.id, path: join(real, e.name), confirmName: typed ? e.name : "" }));
  };

  // removeSelected：一次删除几项。有目录或敏感文件时输入项数确认（单项删除时是输入名称）。
  const removeSelected = async (list: NodeFileEntry[]) => {
    const typed = list.some((e) => (e.isDir && !e.isSymlink) || e.access === "confirm");
    const answer = await ask(fm.deleteMany(list.length), {
      action: t.common.actions.delete,
      danger: true,
      ...(typed ? { checkDefault: true, typeToConfirm: { label: fm.deleteType(list.length), value: String(list.length) } } : {}),
    });
    if (!answer) return;
    await run("del", async () => {
      for (const e of list) {
        const confirm = (e.isDir && !e.isSymlink) || e.access === "confirm";
        await nodeClient.deleteNodeFile({ nodeId: node.id, path: join(real, e.name), confirmName: confirm ? e.name : "" });
      }
    });
    sel.clear();
  };

  const edit = async (e: NodeFileEntry) => {
    await run(`edit:${e.name}`, async () => {
      const file = await nodeClient.readNodeFile({ nodeId: node.id, path: join(real, e.name) });
      if (file.binary) throw new Error(m.binary);
      setEditing({ file, text: new TextDecoder().decode(file.content) });
    });
  };

  if (editing) {
    return (
      <EditNodeFile
        key={editing.file.path}
        node={node}
        file={editing.file}
        initial={editing.text}
        onClose={() => setEditing(null)}
        onPath={(p) => {
          setEditing(null);
          onPath(p);
        }}
        onExpired={onExpired}
        onSaved={refresh}
      />
    );
  }

  const segments = crumbsOf(real);
  const current = segments.pop();
  const crumbs = [...(current ? [{ label: m.root, path: "/" }] : []), ...segments];
  const moreItems = [
    { label: m.goto, onSelect: () => setGoto(real) },
    ...(readonly ? [] : [{ label: m.newDir, onSelect: () => setCreating("dir" as const) }, { label: m.newFile, onSelect: () => setCreating("file" as const) }]),
    { label: m.openTerminal, onSelect: () => onTerminal(real) },
    ...(real === "/" ? [] : [{ label: m.downloadDir, onSelect: () => download(real) }]),
  ];
  const selectedEntries = entries.filter((e) => sel.selected.has(e.name));
  const selected = selectedEntries.map((e) => e.name);
  const lockedSelected = selectedEntries.some((e) => e.access === "readonly");
  const bar = ops && selected.length > 0 ? "select" : ops && fops.clipboard ? "clip" : null;
  const working = !!busy || fops.busy;

  return (
    <section className="grid gap-4">
      <MobileBar back={() => (real === "/" ? onExit() : onPath(parentOf(real)))} title={current?.label ?? m.root} right={<Menu ghost title={current?.label ?? m.root} items={moreItems} />} />
      <p className="font-mono text-[13px] leading-5 wrap-anywhere text-muted-foreground md:hidden">{real}</p>
      <header className="flex min-h-8 flex-wrap items-center justify-between gap-x-4 gap-y-2">
        <div className="flex min-w-0 items-center gap-2 max-md:hidden">
          <Crumbs parts={crumbs.map((c) => ({ label: c.label, onClick: () => onPath(c.path) }))} current={current?.label ?? m.root} />
          <HelpTip>{m.rules}</HelpTip>
        </div>
        <div className="flex flex-wrap items-center gap-2 max-md:w-full">
          {!readonly && q.data && (
            <>
              <Button variant="outline" className="max-md:flex-1" disabled={busy === "upload"} onClick={() => inputRef.current?.click()}>
                {busy === "upload" && <Spinner />}
                {m.upload}
              </Button>
              <input
                ref={inputRef}
                type="file"
                multiple
                hidden
                onChange={(e) => {
                  // 先复制一份：清空 value 会让 FileList 也变空。
                  const list = [...(e.target.files ?? [])];
                  e.target.value = "";
                  if (list.length > 0) upload(list);
                }}
              />
            </>
          )}
          <div className="max-md:hidden">
            <Menu items={moreItems} />
          </div>
        </div>
      </header>

      <ContainerHint n={node} />
      {readonly && <Notice title={m.readonlyDir}>{m.readonlyText}</Notice>}

      <Collapse open={goto !== null} gap={4}>
        {goto !== null && (
          <form
            className="flex items-center gap-2"
            onSubmit={(e) => {
              e.preventDefault();
              const p = goto.trim();
              if (p) onPath(p);
              setGoto(null);
            }}
          >
            <Input ref={gotoRef} aria-label={m.pathLabel} value={goto} onChange={(e) => setGoto(e.target.value)} className="min-w-0 flex-1 font-mono" spellCheck={false} />
            <Button type="submit" readOnlyOk>
              {m.gotoSubmit}
            </Button>
            <Button variant="ghost" readOnlyOk onClick={() => setGoto(null)}>
              {t.common.actions.cancel}
            </Button>
          </form>
        )}
      </Collapse>

      <Collapse open={!!creating} gap={4}>
        {creating && (
          <form
            className="flex items-center gap-2"
            onSubmit={(e) => {
              e.preventDefault();
              const n = name.trim();
              if (!n) return;
              const op =
                creating === "dir"
                  ? () => nodeClient.createNodeDir({ nodeId: node.id, path: join(real, n) })
                  : () => nodeClient.writeNodeFile({ nodeId: node.id, path: join(real, n), create: true, confirmed: q.data?.access === "confirm" });
              run("create", op).then((ok) => {
                if (!ok) return;
                setName("");
                setCreating(null);
              });
            }}
          >
            {/* 字段名给读屏，占位符写示例。 */}
            <Input ref={nameRef} aria-label={creating === "dir" ? m.dirName : m.fileName} placeholder={creating === "dir" ? "data" : "config.yml"} autoComplete="off" autoCapitalize="none" spellCheck={false} value={name} onChange={(e) => setName(e.target.value)} className="min-w-0 flex-1" />
            <Button type="submit" disabled={busy === "create"}>
              {busy === "create" && <Spinner />}
              {t.common.actions.create}
            </Button>
            <Button
              variant="ghost"
              onClick={() => {
                setName("");
                setCreating(null);
              }}
            >
              {t.common.actions.cancel}
            </Button>
          </form>
        )}
      </Collapse>

      {error && <Alert>{error}</Alert>}
      {ops && !readonly && <PendingUploads scope={scope} dir={real} inDir={(p) => parentOf(p) === real} items={uploads.items} target={target} />}
      <UploadList queue={uploads.queue} items={uploads.items} />
      {fops.elements}
      <Collapse open={!!bar} gap={4}>
        {bar === "select" && (
          <SelectionBar
            count={selected.length}
            total={entries.filter((e) => e.access !== "denied").length}
            onAll={(on) => sel.setAll(on, entries.filter((e) => e.access !== "denied").map((e) => e.name))}
            onClear={sel.clear}
            actions={[
              { label: fm.copy, onSelect: () => (fops.copy(selected), sel.clear()) },
              { label: fm.cut, disabled: lockedSelected || readonly, onSelect: () => (fops.cut(selected), sel.clear()) },
              { label: fm.compress, disabled: working || readonly, onSelect: () => fops.compress(selected) },
              { label: t.common.actions.delete, danger: true, disabled: working || lockedSelected || readonly, onSelect: () => removeSelected(selectedEntries) },
            ]}
          />
        )}
        {bar === "clip" && fops.clipboard && <ClipboardBar clip={fops.clipboard} busy={fops.busy} disabled={!!busy || readonly} onPaste={fops.paste} />}
      </Collapse>

      {q.isPending || isElevationRequired(q.error) ? (
        <RowsSkeleton />
      ) : q.error ? (
        <QueryError error={errorMessage(q.error)} onRetry={() => q.refetch()} />
      ) : entries.length === 0 ? (
        <EmptyRow>{m.empty}</EmptyRow>
      ) : (
        <List>
          {entries.slice(0, shown).map((e) => {
            const blocked = e.access === "denied";
            const locked = e.access === "readonly";
            const label = accessLabel(e.access);
            const kind = e.isSymlink ? <LinkIcon className="-mr-1" strokeWidth={2.25} /> : e.isDir ? <Folder className="-mr-1" strokeWidth={2.25} /> : <File className="-mr-1" strokeWidth={2.25} />;
            return (
              <Row
                key={e.name}
                icon={
                  ops ? (
                    <span className="flex items-start gap-3">
                      <SelectBox name={e.name} checked={sel.selected.has(e.name)} disabled={blocked} onToggle={(range) => sel.toggle(e.name, range)} />
                      {kind}
                    </span>
                  ) : (
                    kind
                  )
                }
                title={
                  <>
                    {e.isDir && !blocked ? (
                      <button type="button" title={[e.owner, e.mode].filter(Boolean).join(" · ")} className="min-w-0 cursor-pointer truncate rounded-sm text-left outline-none hover:underline focus-ring" onClick={() => onPath(join(real, e.name))}>
                        {e.name}
                      </button>
                    ) : (
                      <span title={[e.owner, e.mode].filter(Boolean).join(" · ")} className={cn("truncate", blocked && "text-muted-foreground")}>
                        {e.name}
                      </span>
                    )}
                    {e.isSymlink && <span className="min-w-0 truncate text-muted-foreground">{`→ ${e.linkTarget}`}</span>}
                    {label && <span className="shrink-0 text-muted-foreground">{label}</span>}
                  </>
                }
                cols={[
                  {
                    content: e.isSymlink ? "" : e.isDir ? ops && !blocked && <DirSizeText {...dirSize(e.name)} /> : fmtBytes(Number(e.size)),
                    num: true,
                  },
                  { content: <span className="tabular-nums">{formatModified(toDate(e.modified))}</span> },
                ]}
                actionsWidth="w-8"
                actions={
                  blocked ? null : (
                    <RowMenu
                      title={e.name}
                      items={[
                        { label: m.download, onSelect: () => download(join(real, e.name)) },
                        ...(!e.isDir && Number(e.size) <= maxEditBytes ? [{ label: locked ? m.view : t.common.actions.edit, disabled: working, onSelect: () => edit(e) }] : []),
                        ...(ops
                          ? [
                              ...(locked || readonly ? [] : [{ label: fm.rename, disabled: working, onSelect: () => fops.rename(e.name) }]),
                              ...(e.isSymlink ? [] : [{ label: fm.copy, onSelect: () => fops.copy([e.name]) }]),
                              ...(locked || readonly ? [] : [{ label: fm.cut, onSelect: () => fops.cut([e.name]) }]),
                              ...(e.isSymlink || readonly ? [] : [{ label: fm.compress, disabled: working, onSelect: () => fops.compress([e.name]) }]),
                              ...(!e.isDir && !e.isSymlink && !readonly && isArchive(e.name) ? [{ label: fm.extract, disabled: working, onSelect: () => fops.extract(e.name) }] : []),
                            ]
                          : []),
                        ...(locked ? [] : [{ label: t.common.actions.delete, danger: true, disabled: working, onSelect: () => remove(e) }]),
                      ]}
                    />
                  )
                }
              />
            );
          })}
        </List>
      )}
      {entries.length > shown && (
        <div>
          <Button readOnlyOk variant="ghost" className="-ml-3" onClick={() => setShown(shown + pageSize)}>
            {m.showMore(entries.length - shown)}
          </Button>
        </div>
      )}
      {q.data?.truncated && <Notice tone="warning" title={m.truncated} />}
    </section>
  );
}

// EditNodeFile：整页编辑一个小文本文件，和卷的文件编辑同一种写法。有未保存的修改时，离开前确认。
function EditNodeFile({ node, file, initial, onClose, onPath, onExpired, onSaved }: { node: Node; file: ReadNodeFileResponse; initial: string; onClose: () => void; onPath: (p: string) => void; onExpired: () => void; onSaved: () => void }) {
  const m = t.nodeAccess.files;
  const [text, setText] = useState(initial);
  const [hash, setHash] = useState(file.sha256);
  const [baseline, setBaseline] = useState(initial);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [conflict, setConflict] = useState<{ text: string; sha256: string } | null>(null);
  const [history, setHistory] = useState(false);
  const readonly = file.access === "readonly";
  const dirty = useUnsaved(text !== baseline);
  const sensitive = file.access === "confirm";
  const segments = crumbsOf(file.path);
  const name = segments.pop()?.label ?? file.path;

  const guard = async (fn: () => void) => {
    if (dirty && !(await confirmAction(t.common.unsaved.confirm, { action: t.common.unsaved.leave, danger: true }))) return;
    fn();
  };

  // expected 为空表示用户确认覆盖，不再检查。
  const save = async (expected: string) => {
    if (!dirty && !conflict) return onClose();
    if (expected === "" && !(await confirmAction(m.overwriteNode, { action: m.overwrite, danger: true }))) return;
    if (sensitive && !(await confirmAction(m.sensitiveConfirm(file.path), { action: t.common.actions.save, danger: true }))) return;
    setBusy(true);
    setError("");
    try {
      const res = await nodeClient.writeNodeFile({ nodeId: node.id, path: file.path, content: new TextEncoder().encode(text), expectedSha256: expected, confirmed: sensitive });
      setHash(res.sha256);
      setBaseline(text);
      onSaved();
      if (res.inPlace) toast.warning(m.savedInPlace);
      else toast.success(t.common.feedback.saved);
      // 等「没有修改」生效再离开，否则离开时还会问一次。
      setTimeout(onClose);
    } catch (err) {
      if (isElevationRequired(err)) onExpired();
      else if (err instanceof ConnectError && err.code === Code.Aborted) {
        const cur = await nodeClient.readNodeFile({ nodeId: node.id, path: file.path }).catch(() => null);
        setConflict({ text: cur && !cur.binary ? new TextDecoder().decode(cur.content) : "", sha256: cur?.sha256 ?? "" });
      } else setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const reload = () => {
    if (!conflict) return;
    setText(conflict.text);
    setBaseline(conflict.text);
    setHash(conflict.sha256);
    setConflict(null);
  };

  return (
    <form
      id="edit-node-file"
      className="grid gap-4 max-md:-mb-12"
      onSubmit={(e) => {
        e.preventDefault();
        if (!readonly) save(conflict ? "" : hash);
      }}
    >
      <MobileBar
        left={
          <Button variant="ghost" className={mobileBarText} readOnlyOk onClick={() => guard(onClose)}>
            {readonly ? t.common.actions.close : t.common.actions.cancel}
          </Button>
        }
        title={name}
        right={
          readonly ? undefined : (
            <Button type="submit" form="edit-node-file" variant="ghost" className={cn(mobileBarText, "font-medium")} disabled={busy}>
              {busy && <Spinner />}
              {t.common.actions.save}
            </Button>
          )
        }
      />
      <header className="flex min-h-8 flex-wrap items-center justify-between gap-x-4 gap-y-2 max-md:hidden">
        <Crumbs parts={[{ label: m.root, path: "/" }, ...segments].map((c) => ({ label: c.label, onClick: () => guard(() => onPath(c.path)) }))} current={name} />
        <div className="flex flex-wrap items-center gap-2">
          <span className="text-muted-foreground">{[file.owner, file.mode].filter(Boolean).join(" · ")}</span>
          {!readonly && <Menu items={[{ label: m.history, onSelect: () => setHistory(true) }]} />}
          <Button variant="outline" readOnlyOk onClick={() => guard(onClose)}>
            {readonly ? t.common.actions.close : t.common.actions.cancel}
          </Button>
          {!readonly && (
            <Button type="submit" disabled={busy}>
              {busy && <Spinner />}
              {t.common.actions.save}
            </Button>
          )}
        </div>
      </header>
      {readonly && <Notice title={m.readonlyDir}>{m.readonlyText}</Notice>}
      {sensitive && <Notice tone="warning" title={m.sensitiveFile} />}
      {error && <Alert>{error}</Alert>}
      {conflict && (
        <div className="grid gap-3">
          <Notice
            tone="warning"
            title={m.conflictTitle}
            // 右边只放「使用节点内容」；要覆盖时点保存，保存前确认（docs/ui.md「节点 · 终端、文件」）。
            actions={
              <Button variant="outline" readOnlyOk onClick={reload}>
                {m.conflictReload}
              </Button>
            }
          >
            {m.conflictText}
          </Notice>
          <LineDiff before={conflict.text} after={text} none={m.conflictSame} />
        </div>
      )}
      <Textarea aria-label={m.content} value={text} readOnly={readonly} onChange={(e) => setText(e.target.value)} spellCheck={false} className="h-[600px] resize-none border-0 bg-muted p-3 font-mono text-[13px] leading-5 max-md:h-[calc(100dvh-7.75rem)] max-md:text-base max-md:leading-6" />
      {history && (
        <BackupsDialog
          node={node}
          path={file.path}
          onClose={() => setHistory(false)}
          onExpired={onExpired}
          confirmed={sensitive}
          onRestored={async () => {
            const cur = await nodeClient.readNodeFile({ nodeId: node.id, path: file.path });
            const content = new TextDecoder().decode(cur.content);
            setText(content);
            setBaseline(content);
            setHash(cur.sha256);
            setConflict(null);
            setHistory(false);
            onSaved();
          }}
        />
      )}
    </form>
  );
}

function BackupsDialog({ node, path, confirmed, onClose, onExpired, onRestored }: { node: Node; path: string; confirmed: boolean; onClose: () => void; onExpired: () => void; onRestored: () => void }) {
  const m = t.nodeAccess.files;
  const q = useQuery({ queryKey: ["node-file-backups", node.id, path], queryFn: () => nodeClient.listNodeFileBackups({ nodeId: node.id, path }), retry: false });
  const [error, setError] = useState("");
  const restore = async (id: string, time: string) => {
    if (!(await confirmAction(m.restoreConfirm(time), { action: m.restore, danger: true }))) return;
    setError("");
    try {
      await nodeClient.restoreNodeFileBackup({ nodeId: node.id, path, id, confirmed });
      toast.success(m.restored);
      onRestored();
    } catch (err) {
      if (isElevationRequired(err)) onExpired();
      else setError(errorMessage(err));
    }
  };
  return (
    <Dialog open onClose={onClose} title={m.history} description={m.historyHint}>
      {error && <Alert>{error}</Alert>}
      {q.isPending ? (
        <RowsSkeleton count={3} />
      ) : q.error ? (
        <QueryError error={errorMessage(q.error)} onRetry={() => q.refetch()} />
      ) : q.data.backups.length === 0 ? (
        <EmptyRow>{m.historyEmpty}</EmptyRow>
      ) : (
        <List>
          {q.data.backups.map((b) => {
            const time = formatDateTime(toDate(b.time)) ?? "";
            return (
              <Row
                key={b.id}
                title={<span className="truncate">{time}</span>}
                cols={[{ content: fmtBytes(Number(b.size)), num: true }]}
                actionsWidth="w-auto"
                actions={
                  <Button type="button" size="row" variant="ghost" onClick={() => restore(b.id, time)}>
                    {m.restore}
                  </Button>
                }
              />
            );
          })}
        </List>
      )}
    </Dialog>
  );
}
