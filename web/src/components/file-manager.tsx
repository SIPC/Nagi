// 卷文件和节点文件共用的文件管理（见 docs/ui.md「文件管理」）：多选、剪贴板（复制、剪切、粘贴）、改名、压缩、解压、
// 分块上传的进度、目录大小。两个页面各自提供 FileOpsAdapter（路径格式、接口），交互和文案一致。
import { File as FileIcon, Pause, Play, X } from "lucide-react";
import { useEffect, useMemo, useRef, useState, useSyncExternalStore } from "react";
import { useQuery } from "@tanstack/react-query";
import type * as React from "react";
import { toast } from "sonner";
import { Alert, Button, Check, Collapse, Dialog, DialogActions, Field, Input, List, Menu, Notice, Row, Segmented, Skeleton, Spinner, useIsMobile } from "@/components/ui";
import { Checkbox } from "@/components/ui/checkbox";
import { fmtBytes, fmtRate } from "@/components/metrics";
import type { DirSize, FileOpEvent } from "@/gen/nagi/v1/agent_pb";
import { locale, t } from "@/i18n";
import { errorMessage } from "@/lib/api";
import { forgetPending, pendingUploads, UploadQueue, type PendingUpload, type UploadItem, type UploadTarget } from "@/lib/chunked-upload";
import { cn, uploadConcurrency } from "@/lib/utils";

const fm = t.fileManager;

// ---- 名字 ----

const archiveExts = [".tar.gz", ".tgz", ".zip", ".tar"];

// isArchive：能解压的压缩包（zip、tar.gz、tgz、tar）。
export function isArchive(name: string) {
  const lower = name.toLowerCase();
  return archiveExts.some((e) => lower.endsWith(e) && lower.length > e.length);
}

// archiveBase：压缩包去掉后缀的名字，解压到新目录时用它做目录名。
export function archiveBase(name: string) {
  const lower = name.toLowerCase();
  const e = archiveExts.find((x) => lower.endsWith(x) && lower.length > x.length);
  return e ? name.slice(0, name.length - e.length) : name;
}

// stem：去掉后缀的文件名（.env 这样的没有后缀）。
function stem(name: string) {
  const i = name.lastIndexOf(".");
  return i > 0 ? name.slice(0, i) : name;
}

// freeName：目录里还没有的名字，a.txt → a (2).txt，和节点的规则一致。
export function freeName(name: string, taken: Set<string>, dir = false) {
  const lower = name.toLowerCase();
  const double = [".tar.gz", ".tar.bz2", ".tar.xz", ".tar.zst"].find((e) => lower.endsWith(e) && lower.length > e.length);
  const ext = dir ? "" : (double ?? (name.lastIndexOf(".") > 0 ? name.slice(name.lastIndexOf(".")) : ""));
  const base = name.slice(0, name.length - ext.length);
  for (let i = 2; ; i++) {
    const cand = `${base} (${i})${ext}`;
    if (!taken.has(cand)) return cand;
  }
}

const badName = (n: string) => !n || n === "." || n === ".." || /[/\\]/.test(n);

// ---- 多选 ----

// useSelection：列表的多选。点复选框选一项，按住 Shift 点选一段。切换目录（reset 变化）时清空。
export function useSelection(names: string[], reset: string) {
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const anchor = useRef<string | null>(null);
  useEffect(() => {
    setSelected(new Set());
    anchor.current = null;
  }, [reset]);
  // 列表刷新后已经不在的去掉。
  const live = useMemo(() => new Set([...selected].filter((n) => names.includes(n))), [selected, names]);
  const toggle = (name: string, range: boolean) => {
    setSelected((cur) => {
      const next = new Set([...cur].filter((n) => names.includes(n)));
      const on = !next.has(name);
      if (range && anchor.current && names.includes(anchor.current)) {
        const [a, b] = [names.indexOf(anchor.current), names.indexOf(name)].sort((x, y) => x - y);
        for (const n of names.slice(a, b + 1)) {
          if (on) next.add(n);
          else next.delete(n);
        }
      } else if (on) next.add(name);
      else next.delete(name);
      return next;
    });
    anchor.current = name;
  };
  return {
    selected: live,
    toggle,
    clear: () => setSelected(new Set()),
    // setAll：全选（only 是能选的那些，例如节点上禁止访问的不能选）或全不选。
    setAll: (on: boolean, only = names) => setSelected(on ? new Set(only) : new Set()),
  };
}

// SelectBox：行首的复选框。
export function SelectBox({ name, checked, disabled, onToggle }: { name: string; checked: boolean; disabled?: boolean; onToggle: (range: boolean) => void }) {
  return (
    <Checkbox
      aria-label={fm.select(name)}
      checked={checked}
      disabled={disabled}
      className="mt-0.5 [&_svg]:!size-3"
      onClick={(e) => {
        e.preventDefault();
        if (!disabled) onToggle(e.shiftKey);
      }}
    />
  );
}

// ---- 剪贴板 ----

// 剪贴板在页面之间保留（切换目录不丢），按范围（哪个卷、哪个节点）分开，只能粘贴到同一个范围里。
export type Clip = { scope: string; dir: string; names: string[]; cut: boolean };
let clip: Clip | null = null;
const clipListeners = new Set<() => void>();
const subscribeClip = (fn: () => void) => {
  clipListeners.add(fn);
  return () => clipListeners.delete(fn);
};
export function setClipboard(c: Clip | null) {
  clip = c;
  clipListeners.forEach((fn) => fn());
}
export function useClipboard(scope: string) {
  const c = useSyncExternalStore(subscribeClip, () => clip);
  return c && c.scope === scope ? c : null;
}

// ---- 操作 ----

export type FileOpResult = { conflicts: string[]; paths: string[]; skipped: number; skippedLinks: number };
export type OpOptions = { conflict: string; signal: AbortSignal; progress: (bytes: number, files: number) => void };

// streamOp 读完一个长操作的进度流，返回结果。
export async function streamOp(stream: AsyncIterable<FileOpEvent>, o: OpOptions): Promise<FileOpResult> {
  let res: FileOpResult = { conflicts: [], paths: [], skipped: 0, skippedLinks: 0 };
  for await (const ev of stream) {
    o.progress(Number(ev.bytes), Number(ev.files));
    if (ev.result) res = ev.result;
  }
  return res;
}

// FileOpsAdapter：一个文件管理页怎么执行各项操作。路径由页面拼（卷是相对路径，节点是绝对路径）。
export type FileOpsAdapter = {
  scope: string;
  join: (dir: string, name: string) => string;
  rename: (path: string, name: string) => Promise<FileOpResult>;
  transfer: (paths: string[], destDir: string, copy: boolean, o: OpOptions) => Promise<FileOpResult>;
  compress: (paths: string[], dest: string, format: string, o: OpOptions) => Promise<FileOpResult>;
  extract: (path: string, destDir: string, intoNewDir: boolean, o: OpOptions) => Promise<FileOpResult>;
  // onError：页面自己处理的错误（例如身份验证过期）返回 true。
  onError?: (err: unknown) => boolean;
  // snapshotFirst：覆盖前可以先做一份快照（卷文件），同名对话框里选覆盖时出现「先创建快照」。
  snapshotFirst?: { label: string; run: () => Promise<void> };
  // overwriteHint：同名对话框里选覆盖时的说明（节点文件：哪些能从历史版本找回）。
  overwriteHint?: string;
};

export type ConflictChoice = { choice: "rename" | "overwrite" | "skip"; checked: boolean };

type OpKind = "copy" | "move" | "compress" | "extract";
type Running = { kind: OpKind; bytes: number; files: number };

// useFileOps：粘贴、改名、压缩、解压的流程和对话框。同名时先问一次（覆盖、保留两者、跳过），整批按同一个选择处理。
export function useFileOps(adapter: FileOpsAdapter, { dir, entries, refresh, setError }: { dir: string; entries: { name: string; isDir: boolean }[]; refresh: () => void; setError: (e: string) => void }) {
  const [running, setRunning] = useState<Running | null>(null);
  const ctrl = useRef<AbortController | null>(null);
  const [conflict, setConflict] = useState<{ names: string[]; resolve: (v: ConflictChoice | null) => void } | null>(null);
  const [renaming, setRenaming] = useState<string | null>(null);
  const [compressing, setCompressing] = useState<string[] | null>(null);
  const [extracting, setExtracting] = useState<string | null>(null);
  const clipboard = useClipboard(adapter.scope);

  const askConflict = (names: string[]) => new Promise<ConflictChoice | null>((resolve) => setConflict({ names, resolve }));

  const fail = (err: unknown) => {
    if (adapter.onError?.(err)) return;
    setError(errorMessage(err));
  };

  // run 执行一个长操作：先不指定同名处理，有同名时问一次再重试。返回是否完成。
  const run = async (kind: OpKind, call: (o: OpOptions) => Promise<FileOpResult>) => {
    const c = new AbortController();
    ctrl.current = c;
    setError("");
    const opts = (conflict: string): OpOptions => ({ conflict, signal: c.signal, progress: (bytes, files) => setRunning({ kind, bytes, files }) });
    setRunning({ kind, bytes: 0, files: 0 });
    try {
      let res = await call(opts(""));
      if (res.conflicts.length > 0) {
        setRunning(null);
        const answer = await askConflict(res.conflicts);
        if (!answer) return false;
        setRunning({ kind, bytes: 0, files: 0 });
        if (answer.checked && answer.choice === "overwrite") await adapter.snapshotFirst?.run();
        res = await call(opts(answer.choice));
      }
      if (res.skippedLinks > 0) toast.info(fm.skippedLinks(res.skippedLinks));
      if (res.skipped > 0) toast.info(fm.skipped(res.skipped));
      return true;
    } catch (err) {
      if (!c.signal.aborted) fail(err);
      return false;
    } finally {
      setRunning(null);
      ctrl.current = null;
      refresh();
    }
  };

  const paste = async () => {
    if (!clipboard) return;
    const paths = clipboard.names.map((n) => adapter.join(clipboard.dir, n));
    const ok = await run(clipboard.cut ? "move" : "copy", (o) => adapter.transfer(paths, dir, !clipboard.cut, o));
    if (ok && clipboard.cut) setClipboard(null);
  };

  const taken = new Set(entries.map((e) => e.name));
  const elements = (
    <>
      <Collapse open={!!running} gap={4}>
        {running && (
          <div className="flex min-h-8 items-center gap-2" role="status">
            <Spinner />
            <span className="min-w-0 flex-1 truncate">
              {fm.progress[running.kind]}
              {running.bytes > 0 && <span className="text-muted-foreground">{` · ${fm.progress.detail(fmtBytes(running.bytes), running.files)}`}</span>}
            </span>
            <Button variant="ghost" className="-mr-3" readOnlyOk onClick={() => ctrl.current?.abort()}>
              {fm.cancel}
            </Button>
          </div>
        )}
      </Collapse>
      {conflict && (
        <ConflictDialog
          names={conflict.names}
          check={adapter.snapshotFirst?.label}
          hint={adapter.overwriteHint}
          onClose={(v) => {
            conflict.resolve(v);
            setConflict(null);
          }}
        />
      )}
      {renaming !== null && (
        <NameDialog
          title={fm.renameTitle(renaming)}
          initial={renaming}
          select={entries.find((e) => e.name === renaming)?.isDir ? renaming.length : stem(renaming).length}
          action={fm.rename}
          onClose={() => setRenaming(null)}
          onSubmit={async (name) => {
            if (name === renaming) return true;
            const res = await adapter.rename(adapter.join(dir, renaming), name);
            if (res.conflicts.length > 0) throw new Error(fm.conflictTitle(res.conflicts));
            refresh();
            return true;
          }}
          onError={(err) => adapter.onError?.(err) ?? false}
        />
      )}
      {compressing && (
        <CompressDialog
          names={compressing}
          dirName={dir.split("/").filter(Boolean).pop() ?? ""}
          onClose={() => setCompressing(null)}
          onSubmit={(name, format) => {
            const names = compressing;
            setCompressing(null);
            void run("compress", (o) =>
              adapter.compress(
                names.map((n) => adapter.join(dir, n)),
                adapter.join(dir, name),
                format,
                o,
              ),
            );
          }}
        />
      )}
      {extracting && (
        <ExtractDialog
          name={extracting}
          taken={taken}
          onClose={() => setExtracting(null)}
          onSubmit={(intoNewDir) => {
            const name = extracting;
            setExtracting(null);
            void run("extract", (o) => adapter.extract(adapter.join(dir, name), intoNewDir ? adapter.join(dir, archiveBase(name)) : dir, intoNewDir, o));
          }}
        />
      )}
    </>
  );

  return {
    busy: !!running,
    clipboard,
    paste,
    rename: (name: string) => setRenaming(name),
    compress: (names: string[]) => setCompressing(names),
    extract: (name: string) => setExtracting(name),
    askConflict,
    copy: (names: string[]) => setClipboard({ scope: adapter.scope, dir, names, cut: false }),
    cut: (names: string[]) => setClipboard({ scope: adapter.scope, dir, names, cut: true }),
    elements,
  };
}

function ConflictDialog({ names, check, hint, onClose }: { names: string[]; check?: string; hint?: string; onClose: (v: ConflictChoice | null) => void }) {
  const [choice, setChoice] = useState<ConflictChoice["choice"]>("rename");
  const [checked, setChecked] = useState(true);
  const shown = names.slice(0, 5);
  const list = t.common.list(shown) + (names.length > shown.length ? ` ${fm.more(names.length - shown.length)}` : "");
  return (
    <Dialog open onClose={() => onClose(null)} title={fm.conflictTitle(names)} description={names.length > 1 ? fm.conflictText(list) : undefined}>
      <Segmented
        value={choice}
        onChange={setChoice}
        label={fm.conflictTitle(names)}
        items={[
          { value: "rename", label: fm.keepBoth },
          { value: "overwrite", label: fm.overwrite },
          { value: "skip", label: fm.skip },
        ]}
      />
      {/* 覆盖是整个替换，目录不合并（见 docs/agent.md#存储）。 */}
      <Collapse open={choice === "overwrite"}>
        <div className="grid gap-4">
          <p className="leading-5 text-muted-foreground">{`${fm.replaceNote}${t.common.sentenceEnd}${hint ?? ""}`.trim()}</p>
          {check && (
            <Check checked={checked} onChange={setChecked}>
              {check}
            </Check>
          )}
        </div>
      </Collapse>
      <DialogActions>
        <Button variant="outline" readOnlyOk onClick={() => onClose(null)}>
          {t.common.actions.cancel}
        </Button>
        <Button variant={choice === "overwrite" ? "destructive" : "default"} onClick={() => onClose({ choice, checked: !!check && checked })}>
          {fm.continue}
        </Button>
      </DialogActions>
    </Dialog>
  );
}

// NameDialog：填一个新名字（改名）。select：默认选中前面多少个字符（文件名不选后缀）。
function NameDialog({
  title,
  initial,
  select,
  action,
  onClose,
  onSubmit,
  onError,
}: {
  title: string;
  initial: string;
  select: number;
  action: string;
  onClose: () => void;
  onSubmit: (name: string) => Promise<boolean>;
  onError: (err: unknown) => boolean;
}) {
  const [name, setName] = useState(initial);
  const [checked, setChecked] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const ref = useRef<HTMLInputElement>(null);
  useEffect(() => {
    const id = setTimeout(() => ref.current?.setSelectionRange(0, select), 0);
    return () => clearTimeout(id);
  }, [select]);
  const invalid = checked && badName(name.trim()) ? fm.nameRequired : "";
  return (
    <Dialog open onClose={onClose} title={title}>
      <form
        className="grid gap-4"
        onSubmit={async (e) => {
          e.preventDefault();
          setChecked(true);
          const n = name.trim();
          if (badName(n)) return;
          setBusy(true);
          setError("");
          try {
            if (await onSubmit(n)) onClose();
          } catch (err) {
            if (!onError(err)) setError(errorMessage(err));
          } finally {
            setBusy(false);
          }
        }}
      >
        {error && <Alert keepFocus>{error}</Alert>}
        <Field label={fm.name} htmlFor="file-name" error={invalid}>
          <Input ref={ref} id="file-name" value={name} onChange={(e) => setName(e.target.value)} autoComplete="off" autoCapitalize="none" spellCheck={false} />
        </Field>
        <DialogActions>
          <Button variant="outline" onClick={onClose}>
            {t.common.actions.cancel}
          </Button>
          <Button type="submit" disabled={busy}>
            {busy && <Spinner />}
            {action}
          </Button>
        </DialogActions>
      </form>
    </Dialog>
  );
}

const formatExt = { zip: ".zip", "tar.gz": ".tar.gz" } as const;
type Format = keyof typeof formatExt;

function CompressDialog({ names, dirName, onClose, onSubmit }: { names: string[]; dirName: string; onClose: () => void; onSubmit: (name: string, format: Format) => void }) {
  const [format, setFormat] = useState<Format>("zip");
  const base = names.length === 1 ? names[0] : dirName || "archive";
  const [name, setName] = useState(base + formatExt.zip);
  const [checked, setChecked] = useState(false);
  const invalid = checked && badName(name.trim()) ? fm.nameRequired : "";
  const switchFormat = (f: Format) => {
    const old = formatExt[format];
    setName((n) => (n.endsWith(old) ? n.slice(0, n.length - old.length) + formatExt[f] : n));
    setFormat(f);
  };
  return (
    <Dialog open onClose={onClose} title={fm.compressTitle(names.length)} description={fm.noLinks}>
      <form
        className="grid gap-4"
        onSubmit={(e) => {
          e.preventDefault();
          setChecked(true);
          const n = name.trim();
          if (!badName(n)) onSubmit(n, format);
        }}
      >
        <Field label={fm.format}>
          <Segmented
            value={format}
            onChange={switchFormat}
            label={fm.format}
            items={[
              { value: "zip", label: "zip" },
              { value: "tar.gz", label: "tar.gz" },
            ]}
          />
        </Field>
        <Field label={fm.name} htmlFor="archive-name" error={invalid}>
          <Input id="archive-name" value={name} onChange={(e) => setName(e.target.value)} autoComplete="off" autoCapitalize="none" spellCheck={false} />
        </Field>
        <DialogActions>
          <Button variant="outline" onClick={onClose}>
            {t.common.actions.cancel}
          </Button>
          <Button type="submit">{fm.compress}</Button>
        </DialogActions>
      </form>
    </Dialog>
  );
}

function ExtractDialog({ name, taken, onClose, onSubmit }: { name: string; taken: Set<string>; onClose: () => void; onSubmit: (intoNewDir: boolean) => void }) {
  const [mode, setMode] = useState<"dir" | "here">("dir");
  const folder = archiveBase(name);
  return (
    <Dialog open onClose={onClose} title={fm.extractTitle(name)}>
      <Field label={fm.extractTo} hint={mode === "dir" ? fm.extractHint(`${taken.has(folder) ? freeName(folder, taken, true) : folder}/`) : undefined}>
        <Segmented
          value={mode}
          onChange={setMode}
          label={fm.extractTo}
          items={[
            { value: "dir", label: fm.newDir },
            { value: "here", label: fm.here },
          ]}
        />
      </Field>
      <DialogActions>
        <Button variant="outline" onClick={onClose}>
          {t.common.actions.cancel}
        </Button>
        <Button onClick={() => onSubmit(mode === "dir")}>{fm.extract}</Button>
      </DialogActions>
    </Dialog>
  );
}

// ---- 选择栏、剪贴板栏 ----

export type BarAction = { label: string; onSelect: () => void; danger?: boolean; disabled?: boolean };

// SelectionBar：选中了文件时列表上方的一行：全选、已选几项，右边是操作。手机上操作收进底部操作面板。
export function SelectionBar({ count, total, onAll, onClear, actions }: { count: number; total: number; onAll: (on: boolean) => void; onClear: () => void; actions: BarAction[] }) {
  const mobile = useIsMobile();
  return (
    <div className="flex min-h-8 items-center justify-between gap-4">
      <label className="flex min-w-0 cursor-pointer items-center gap-2">
        <Checkbox checked={count === total ? true : count > 0 ? "indeterminate" : false} onCheckedChange={(v) => onAll(v === true)} aria-label={fm.selectAll} className="[&_svg]:!size-3" />
        <span className="truncate font-medium">{fm.selected(count)}</span>
      </label>
      <div className="flex shrink-0 items-center gap-2">
        {mobile ? (
          <Menu label={fm.actions} title={fm.selected(count)} items={actions} />
        ) : (
          actions.map((a) => (
            <Button key={a.label} variant="outline" className={cn(a.danger && "text-destructive hover:text-destructive")} disabled={a.disabled} onClick={a.onSelect}>
              {a.label}
            </Button>
          ))
        )}
        <Button variant="ghost" className="-mr-3" readOnlyOk onClick={onClear}>
          {fm.clearSelection}
        </Button>
      </div>
    </div>
  );
}

// ClipboardBar：剪贴板里有这个范围的文件时，列表上方的一行：复制或剪切了哪些，右边是粘贴到此处、清空。
export function ClipboardBar({ clip: c, onPaste, disabled, busy }: { clip: Clip; onPaste: () => void; disabled?: boolean; busy?: boolean }) {
  return (
    <div className="flex min-h-8 items-center justify-between gap-4">
      <span className="min-w-0 truncate">
        <span className="font-medium">{c.cut ? fm.cutItems(c.names.length) : fm.copiedItems(c.names.length)}</span>
        <span className="text-muted-foreground max-md:hidden">{` · ${t.common.list(c.names.slice(0, 3))}${c.names.length > 3 ? ` ${fm.more(c.names.length - 3)}` : ""}`}</span>
      </span>
      <div className="flex shrink-0 items-center gap-2">
        <Button disabled={disabled || busy} onClick={onPaste}>
          {busy && <Spinner />}
          {fm.paste}
        </Button>
        <Button variant="ghost" className="-mr-3" readOnlyOk onClick={() => setClipboard(null)}>
          {fm.clearClipboard}
        </Button>
      </div>
    </div>
  );
}

// ---- 目录大小 ----

// formatModified：修改时间写具体的日期和时间（到分钟），例如 2026/11/17 14:35，不写「3 天前」。
export function formatModified(date: Date | undefined): string {
  if (!date) return "—";
  return date.toLocaleString(locale, { year: "numeric", month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit", hour12: false });
}

// useDirSizes：目录列表出来以后统计子目录的大小，分两遍：先很快地统计一遍；没统计完的目录（文件多）再带 full
// 请求一次，在后台算出真实的大小，算完之前显示占位。full 也没算完的（极大的目录）不显示大小。
// fetch 要把 signal 交给请求：离开页面或换目录时取消，节点上的统计随之停下，不再占着名额。
export function useDirSizes(
  key: unknown[],
  names: string[],
  enabled: boolean,
  fetch: (names: string[], full: boolean, signal: AbortSignal) => Promise<{ sizes: DirSize[] }>,
) {
  const quick = useQuery({
    queryKey: [...key, "quick", names.join("/")],
    queryFn: ({ signal }) => fetch(names, false, signal),
    enabled: enabled && names.length > 0,
    staleTime: 30_000,
    retry: false,
  });
  const rest = (quick.data?.sizes ?? []).filter((s) => !s.complete).map((s) => s.name);
  const full = useQuery({
    queryKey: [...key, "full", rest.join("/")],
    queryFn: ({ signal }) => fetch(rest, true, signal),
    enabled: enabled && rest.length > 0,
    staleTime: 30_000,
    retry: false,
  });
  return (name: string): { size?: DirSize; pending: boolean } => {
    const q = quick.data?.sizes.find((s) => s.name === name);
    if (!q) return { pending: quick.isFetching && !quick.data };
    if (q.complete) return { size: q, pending: false };
    const f = full.data?.sizes.find((s) => s.name === name);
    if (f) return { size: f, pending: false };
    return { pending: full.isFetching || full.isPending };
  };
}

// DirSizeText：列表里目录的大小。统计中显示占位；极大的目录在限额内没统计完时不显示大小，悬停说明。
export function DirSizeText({ size, pending }: { size?: DirSize; pending: boolean }) {
  if (pending) return <Skeleton className="ml-auto inline-block h-3 w-12 align-middle" />;
  if (!size) return null;
  if (!size.complete) return <span className="text-muted-foreground" title={fm.sizeIncomplete}>—</span>;
  return <>{fmtBytes(Number(size.size))}</>;
}

// ---- 上传 ----

// useUploads：一个文件管理页的上传队列（scope 是卷或节点）。有正在进行的上传时，关闭或刷新页面前提醒。
// 上传的目标拼进地址里，所以 UploadTarget 由页面给。
export function useUploads(scope: string) {
  const queue = useMemo(() => new UploadQueue(scope, uploadConcurrency), [scope]);
  const items = useSyncExternalStore(queue.subscribe, queue.items);
  useEffect(() => {
    const warn = (e: BeforeUnloadEvent) => {
      if (queue.active()) e.preventDefault();
    };
    window.addEventListener("beforeunload", warn);
    return () => {
      window.removeEventListener("beforeunload", warn);
      // 离开页面：暂停正在进行的上传，回来后重新选择同一文件接着传。
      for (const it of queue.items()) queue.pause(it.key);
    };
  }, [queue]);
  return { queue, items };
}

function remaining(sec: number) {
  if (sec < 60) return t.common.time.seconds(Math.max(1, Math.round(sec)));
  if (sec < 3600) return t.common.time.minutes(Math.round(sec / 60));
  return t.common.time.hours(Math.round(sec / 3600));
}

function uploadStatus(it: UploadItem): React.ReactNode {
  const u = fm.upload;
  const amount = `${fmtBytes(it.sent)} / ${fmtBytes(it.size)}`;
  switch (it.state) {
    case "queued":
      return u.queued;
    case "preparing":
      return u.preparing;
    case "verifying":
      return it.verified >= it.size && it.sent >= it.size ? u.checking : u.verifying(it.size ? Math.floor((100 * it.verified) / Math.max(1, it.sent || it.verified)) : 100);
    case "retrying":
      return `${u.retrying} · ${amount}`;
    case "paused":
      return `${u.paused} · ${amount}`;
    case "error":
      return <span className="text-destructive">{it.error}</span>;
    case "done":
      return u.done;
  }
  return [amount, it.speed > 0 ? fmtRate(it.speed) : "", it.speed > 0 ? u.remaining(remaining((it.size - it.sent) / it.speed)) : ""].filter(Boolean).join(" · ");
}

// UploadList：上传的进度。一行是文件名、进度条、已传 / 总大小、速度、剩余时间，右边是暂停或继续、取消。
export function UploadList({ queue, items }: { queue: UploadQueue; items: UploadItem[] }) {
  const u = fm.upload;
  const done = items.filter((x) => x.state === "done").length;
  return (
    <Collapse open={items.length > 0} gap={4}>
      {items.length > 0 && (
        <div className="grid gap-1">
          <div className="flex min-h-8 items-center justify-between">
            <span className="font-medium">{u.title}</span>
            {done > 0 && (
              <Button variant="ghost" className="-mr-3" readOnlyOk onClick={() => queue.clearDone()}>
                {u.clear}
              </Button>
            )}
          </div>
          <List>
            {items.map((it) => {
              const active = ["preparing", "verifying", "uploading", "retrying", "queued"].includes(it.state);
              const pct = it.size > 0 ? (100 * it.sent) / it.size : it.state === "done" ? 100 : 0;
              return (
                <Row
                  key={it.key}
                  icon={<FileIcon className="-mr-1" strokeWidth={2.25} />}
                  title={
                    <span className="grid min-w-0 flex-1 gap-1.5">
                      <span className="truncate">{it.name}</span>
                      <span className="block h-1 overflow-hidden rounded-full bg-muted" role="progressbar" aria-valuemin={0} aria-valuemax={100} aria-valuenow={Math.round(pct)} aria-label={it.name}>
                        <span className={cn("block h-full rounded-full transition-[width] duration-300", it.state === "error" ? "bg-destructive" : "bg-primary")} style={{ width: `${pct}%` }} />
                      </span>
                    </span>
                  }
                  sub={<span className="tabular-nums">{uploadStatus(it)}</span>}
                  actionsWidth="w-auto"
                  actions={
                    it.state === "done" ? null : (
                      <>
                        {active ? (
                          <Button variant="ghost" size="rowIcon" aria-label={u.pause} title={u.pause} onClick={() => queue.pause(it.key)}>
                            <Pause />
                          </Button>
                        ) : (
                          <Button variant="ghost" size="rowIcon" aria-label={u.resume} title={u.resume} onClick={() => queue.resume(it.key)}>
                            <Play />
                          </Button>
                        )}
                        <Button variant="ghost" size="rowIcon" aria-label={u.cancel} title={u.cancel} onClick={() => queue.cancel(it.key)}>
                          <X />
                        </Button>
                      </>
                    )
                  }
                />
              );
            })}
          </List>
        </div>
      )}
    </Collapse>
  );
}

// PendingUploads：上次没传完的上传（刷新页面、关闭浏览器以后），提示重新选择同一文件可以接着传。
export function PendingUploads({ scope, dir, inDir, items, target }: { scope: string; dir: string; inDir: (path: string) => boolean; items: UploadItem[]; target: UploadTarget }) {
  const u = fm.upload;
  const [version, setVersion] = useState(0);
  // version：放弃以后重新读。
  const list = useMemo<PendingUpload[]>(() => pendingUploads(scope).filter((p) => inDir(p.path) && !items.some((x) => x.path === p.path)), [scope, dir, items, version]); // eslint-disable-line react-hooks/exhaustive-deps
  if (list.length === 0) return null;
  const text = t.common.list(list.slice(0, 3).map((p) => u.unfinishedItem(p.name, p.size ? Math.floor((100 * p.sent) / p.size) : 0)));
  const discard = () => {
    for (const p of list) {
      if (p.id) void fetch(target.url(p.path, { op: "abort", id: p.id }), { method: "DELETE", headers: { "X-Nagi-Upload": "1" } }).catch(() => {});
      forgetPending(scope, p.path);
    }
    setVersion((v) => v + 1);
  };
  return (
    <Notice
      tone="warning"
      title={u.unfinishedTitle}
      actions={
        <Button variant="outline" onClick={discard}>
          {u.discard}
        </Button>
      }
    >
      {u.unfinished(text)}
    </Notice>
  );
}
