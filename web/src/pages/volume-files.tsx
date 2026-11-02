// 卷里的文件管理：浏览、下载、上传、删除、编辑小文本，以及查看快照里的文件并恢复单个文件。
// 在存储标签里就地换内容（地址的 volume 参数），不开对话框。
// 规则在节点上执行（只读角色看不到入口，接口也拒绝；有顶替的容器只在持有租约时能写）。
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { File, Folder, Link as LinkIcon } from "lucide-react";
import { Fragment, useEffect, useRef, useState } from "react";
import { toast } from "sonner";
import { ask, confirmAction } from "@/components/confirm";
import { Alert, Button, EmptyRow, HelpTip, Input, List, Menu, Notice, QueryError, Row, RowMenu, RowsSkeleton, Select, Spinner, Textarea } from "@/components/ui";
import { MobileBar, mobileBarText } from "@/pages/layout";
import { fmtBytes } from "@/components/metrics";
import { useUnsaved } from "@/components/unsaved";
import type { VolumeFileEntry } from "@/gen/nagi/v1/agent_pb";
import type { Container, Project } from "@/gen/nagi/v1/common_pb";
import { locale, t } from "@/i18n";
import { errorMessage, projectClient, toDate } from "@/lib/api";
import { cn, formatDateTime, timeAgo } from "@/lib/utils";

// 目录一次显示的条数，内容多时点「显示更多」。
const pageSize = 200;

const maxEditBytes = 1 << 20;
const maxUploadBytes = 8 * 1024 ** 3;

// snapshotTime：快照的绝对时间，短到能放进一行：今天 10:08、11/02 10:08，不是今年的带年份。
export function snapshotTime(date: Date | undefined): string {
  if (!date) return "";
  const time = date.toLocaleTimeString(locale, { hour: "2-digit", minute: "2-digit" });
  const now = new Date();
  if (date.toDateString() === now.toDateString()) return t.projectData.snapshots.today(time);
  const day = date.toLocaleDateString(locale, date.getFullYear() === now.getFullYear() ? { month: "2-digit", day: "2-digit" } : { year: "numeric", month: "2-digit", day: "2-digit" });
  return `${day} ${time}`;
}

type Crumb = { label: string; onClick: () => void };

// Crumbs：面包屑。最后一级是当前位置，不能点。
export function Crumbs({ parts, current }: { parts: Crumb[]; current: string }) {
  return (
    <nav aria-label={t.projectData.files.pathLabel} className="flex min-h-8 min-w-0 flex-wrap items-center gap-x-2 wrap-anywhere">
      {parts.map((p, i) => (
        <Fragment key={i}>
          <button type="button" className="cursor-pointer rounded-sm text-muted-foreground outline-none hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/40" onClick={p.onClick}>
            {p.label}
          </button>
          <span className="text-muted-foreground" aria-hidden>
            /
          </span>
        </Fragment>
      ))}
      <span className="font-medium" aria-current="page">
        {current}
      </span>
    </nav>
  );
}

type Props = {
  project: Project;
  container: Container;
  volume: string;
  label: string;
  // onClose：回到存储标签的列表。
  onClose: () => void;
  // unmount：这个卷能取消挂载时才有。
  unmount?: { onSelect: () => void; disabled?: boolean };
};

export function VolumeFiles({ project, container, volume, label, onClose, unmount }: Props) {
  const m = t.projectData.files;
  const queryClient = useQueryClient();
  const [path, setPath] = useState("");
  const [snapshot, setSnapshot] = useState("");
  const [creating, setCreating] = useState(false);
  const [dirName, setDirName] = useState("");
  const [editing, setEditing] = useState<{ path: string; text: string } | null>(null);
  const [busy, setBusy] = useState("");
  const [error, setError] = useState("");
  const inputRef = useRef<HTMLInputElement>(null);
  const dirRef = useRef<HTMLInputElement>(null);
  const ref = { projectId: project.id, container: container.name, volume, snapshotId: snapshot };

  const files = useQuery({
    queryKey: ["volume-files", project.id, container.name, volume, snapshot, path],
    queryFn: () => projectClient.listVolumeFiles({ ...ref, path }),
    retry: false,
  });
  const snapshots = useQuery({
    queryKey: ["snapshots", project.id, container.name, "files"],
    queryFn: () => projectClient.listSnapshots({ projectId: project.id, container: container.name }),
  });
  const choices = (snapshots.data?.snapshots ?? []).filter((s) => s.kind !== "version");
  const refresh = () => {
    queryClient.invalidateQueries({ queryKey: ["volume-files", project.id, container.name, volume] });
    queryClient.invalidateQueries({ queryKey: ["volumes", project.id, container.name] });
  };
  const [shown, setShown] = useState(pageSize);
  useEffect(() => {
    setShown(pageSize);
    setError("");
  }, [path, snapshot]);
  // 从菜单打开时，菜单关闭后会把焦点还给按钮，所以稍后再聚焦。
  useEffect(() => {
    if (!creating) return;
    const id = setTimeout(() => dirRef.current?.focus(), 50);
    return () => clearTimeout(id);
  }, [creating]);
  const join = (name: string) => (path ? `${path}/${name}` : name);
  const parts = path ? path.split("/") : [];

  const fileURL = (p: string) => {
    const q = new URLSearchParams({ project: project.id, container: container.name, volume, path: p });
    if (snapshot) q.set("snapshot", snapshot);
    return `/api/volume-file?${q}`;
  };
  // 先确认能下载：浏览器直接打开出错的地址会离开面板，只显示一段 JSON。
  const download = (p: string) => {
    run(`download:${p}`, async () => {
      const res = await fetch(`${fileURL(p)}&check=1`, { headers: { "Accept-Language": locale } });
      if (!res.ok) throw new Error((await res.json().catch(() => null))?.message || `HTTP ${res.status}`);
      const a = document.createElement("a");
      a.href = fileURL(p);
      a.rel = "noopener";
      a.download = "";
      a.click();
    });
  };

  // 操作前可选先做一份快照，改错了能找回来。
  const snapshotFirst = async () => {
    for await (const ev of projectClient.createSnapshot({ projectId: project.id, container: container.name })) {
      if (ev.event.case === "result" && !ev.event.value.success) throw new Error(ev.event.value.error || m.snapshotFailed);
    }
    queryClient.invalidateQueries({ queryKey: ["snapshots", project.id] });
  };

  // run 返回是否成功：失败时表单留着，错误显示在页面上。
  const run = async (key: string, fn: () => Promise<unknown>, done?: string) => {
    setBusy(key);
    setError("");
    try {
      await fn();
      if (done) toast.success(done);
      refresh();
      return true;
    } catch (err) {
      setError(errorMessage(err));
      return false;
    } finally {
      setBusy("");
    }
  };

  const uploadFile = async (p: string, body: Blob) => {
    const res = await fetch(fileURL(p), { method: "POST", headers: { "X-Nagi-Upload": "1", "Accept-Language": locale }, body });
    if (!res.ok) throw new Error((await res.json().catch(() => null))?.message || `HTTP ${res.status}`);
  };

  const upload = async (list: File[]) => {
    const tooLarge = list.filter((f) => f.size > maxUploadBytes).map((f) => f.name);
    if (tooLarge.length > 0) {
      setError(m.tooLarge(tooLarge.join(", ")));
      return;
    }
    const existing = new Set((files.data?.entries ?? []).map((e) => e.name));
    const clash = list.filter((f) => existing.has(f.name)).map((f) => f.name);
    let first = false;
    if (clash.length > 0) {
      const answer = await ask(m.overwriteConfirm(clash.join(", ")), { action: m.overwrite, danger: true, check: m.snapshotFirst, checkDefault: true });
      if (!answer) return;
      first = answer.checked;
    }
    await run(
      "upload",
      async () => {
        if (first) await snapshotFirst();
        for (const f of list) await uploadFile(join(f.name), f);
      },
      m.uploaded(list.length),
    );
  };

  const remove = async (e: VolumeFileEntry) => {
    const answer = await ask(m.deleteConfirm(e.name, e.isDir), { action: t.common.actions.delete, danger: true, check: m.snapshotFirst, checkDefault: true });
    if (!answer) return;
    await run(
      `del:${e.name}`,
      async () => {
        if (answer.checked) await snapshotFirst();
        await projectClient.deleteVolumeFile({ ...ref, snapshotId: "", path: join(e.name) });
      },
      m.deleted,
    );
  };

  const restore = async (name: string) => {
    if (!(await confirmAction(m.restoreConfirm(name), { action: m.restore, danger: true }))) return;
    await run(`restore:${name}`, () => projectClient.restoreVolumeFile({ ...ref, path: join(name) }), m.restored);
  };

  const edit = async (e: VolumeFileEntry) => {
    await run(`edit:${e.name}`, async () => {
      const res = await fetch(fileURL(join(e.name)), { headers: { "Accept-Language": locale } });
      if (!res.ok) throw new Error((await res.json().catch(() => null))?.message || `HTTP ${res.status}`);
      const bytes = await res.arrayBuffer();
      if (new Uint8Array(bytes).includes(0)) throw new Error(m.binary);
      setEditing({ path: join(e.name), text: new TextDecoder().decode(bytes) });
    });
  };

  // 面包屑。guard：编辑页有未保存的修改时先确认。回到列表走路由，由 useUnsaved 拦截。
  const trail = (guard: (fn: () => void) => void = (fn) => fn(), dirs = parts): Crumb[] => [
    { label: m.storage, onClick: onClose },
    ...[label, ...dirs].map((text, i) => ({
      label: text,
      onClick: () =>
        guard(() => {
          setEditing(null);
          setPath(dirs.slice(0, i).join("/"));
        }),
    })),
  ];

  if (editing) {
    return (
      <EditFile
        key={editing.path}
        path={editing.path}
        initial={editing.text}
        trail={trail}
        busy={busy === "save"}
        error={error}
        onCancel={() => {
          setError("");
          setEditing(null);
        }}
        onSave={async (text) => {
          if (await run("save", () => uploadFile(editing.path, new Blob([text])), t.common.feedback.saved)) setEditing(null);
        }}
      />
    );
  }

  const entries = files.data?.entries ?? [];
  const crumbs = trail();
  const current = crumbs.pop();
  const moreItems = [
    { label: m.newDir, onSelect: () => setCreating(true) },
    { label: m.downloadDir, onSelect: () => download(path) },
    ...(unmount ? [{ label: t.projectData.volumes.remove, danger: true, disabled: unmount.disabled, onSelect: unmount.onSelect }] : []),
  ];

  return (
    <section className="grid gap-4">
      <MobileBar
        back={() => (path ? setPath(parts.slice(0, -1).join("/")) : onClose())}
        title={current?.label ?? label}
        right={snapshot ? undefined : <Menu ghost title={current?.label ?? label} items={moreItems} />}
      />
      <header className="flex min-h-8 flex-wrap items-center justify-between gap-x-4 gap-y-2">
        <div className="flex min-w-0 items-center gap-2 max-md:hidden">
          <Crumbs parts={crumbs} current={current?.label ?? label} />
          {!snapshot && (
            <HelpTip>
              {m.description} {m.symlinkHint}
            </HelpTip>
          )}
        </div>
        <div className="flex flex-wrap items-center gap-3 max-md:w-full max-md:flex-nowrap max-md:gap-2">
          <Select
            aria-label={m.source}
            className="h-8 w-auto max-md:h-10 max-md:min-w-0 max-md:flex-1"
            value={snapshot || "live"}
            onChange={(e) => {
              setSnapshot(e.target.value === "live" ? "" : e.target.value);
              setPath("");
              setCreating(false);
            }}
          >
            <option value="live">{m.live}</option>
            {choices.map((s) => (
              <option key={s.id} value={s.id}>
                {m.snapshotOption(snapshotTime(toDate(s.time)))}
              </option>
            ))}
          </Select>
          {snapshot ? (
            <Button variant="outline" onClick={() => download(path)}>
              {m.downloadDir}
            </Button>
          ) : (
            <>
              <Button variant="outline" disabled={busy === "upload"} onClick={() => inputRef.current?.click()}>
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
              <div className="max-md:hidden">
                <Menu items={moreItems} />
              </div>
            </>
          )}
        </div>
      </header>

      {snapshot && <p className="text-muted-foreground">{m.snapshotMode}</p>}
      {snapshots.error && <QueryError error={errorMessage(snapshots.error)} onRetry={() => snapshots.refetch()} />}

      {creating && !snapshot && (
        <form
          className="flex items-center gap-2"
          onSubmit={(e) => {
            e.preventDefault();
            const name = dirName.trim();
            if (!name) return;
            run("mkdir", () => projectClient.createVolumeDir({ ...ref, snapshotId: "", path: join(name) }), m.created).then((ok) => {
              if (!ok) return;
              setDirName("");
              setCreating(false);
            });
          }}
        >
          <Input ref={dirRef} aria-label={m.dirName} placeholder={m.dirName} value={dirName} onChange={(e) => setDirName(e.target.value)} className="min-w-0 flex-1" />
          <Button type="submit" disabled={busy === "mkdir"}>
            {busy === "mkdir" && <Spinner />}
            {t.common.actions.create}
          </Button>
          <Button
            variant="ghost"
            onClick={() => {
              setDirName("");
              setCreating(false);
            }}
          >
            {t.common.actions.cancel}
          </Button>
        </form>
      )}

      {error && <Alert>{error}</Alert>}

      {files.isPending ? (
        <RowsSkeleton />
      ) : files.error ? (
        <QueryError error={errorMessage(files.error)} onRetry={() => files.refetch()} />
      ) : entries.length === 0 ? (
        <EmptyRow>{m.empty}</EmptyRow>
      ) : (
        <List>
          {entries.slice(0, shown).map((e) => (
            <Row
              key={e.name}
              icon={e.isDir ? <Folder className="-mr-1" strokeWidth={2.25} /> : e.isSymlink ? <LinkIcon className="-mr-1" strokeWidth={2.25} /> : <File className="-mr-1" strokeWidth={2.25} />}
              title={
                e.isDir ? (
                  <button type="button" className="min-w-0 cursor-pointer truncate rounded-sm text-left outline-none hover:underline focus-visible:ring-2 focus-visible:ring-ring/40" onClick={() => setPath(join(e.name))}>
                    {e.name}
                  </button>
                ) : (
                  <span className="truncate" title={e.isSymlink ? m.symlinkHint : undefined}>
                    {e.name}
                  </span>
                )
              }
              cols={[{ content: e.isDir || e.isSymlink ? "" : fmtBytes(Number(e.size)), num: true }, { content: <span title={formatDateTime(toDate(e.modified))}>{timeAgo(toDate(e.modified))}</span> }]}
              actionsWidth="w-28"
              actions={
                snapshot ? (
                  e.isSymlink ? null : (
                    <>
                      <Button variant="ghost" size="row" onClick={() => download(join(e.name))}>
                        {m.download}
                      </Button>
                      <Button variant="ghost" size="row" disabled={!!busy} onClick={() => restore(e.name)}>
                        {m.restore}
                      </Button>
                    </>
                  )
                ) : (
                  <RowMenu
                    title={e.name}
                    items={[
                      ...(e.isSymlink ? [] : [{ label: m.download, onSelect: () => download(join(e.name)) }]),
                      ...(!e.isDir && !e.isSymlink && Number(e.size) <= maxEditBytes ? [{ label: t.common.actions.edit, disabled: !!busy, onSelect: () => edit(e) }] : []),
                      { label: t.common.actions.delete, danger: true, disabled: !!busy, onSelect: () => remove(e) },
                    ]}
                  />
                )
              }
            />
          ))}
        </List>
      )}
      {entries.length > shown && (
        <div>
          <Button readOnlyOk variant="ghost" className="-ml-3" onClick={() => setShown(shown + pageSize)}>
            {m.showMore(entries.length - shown)}
          </Button>
        </div>
      )}
      {files.data?.truncated && <Notice tone="warning" title={m.truncated} />}
    </section>
  );
}

// EditFile：整页编辑一个小文本文件。有未保存的修改时，离开前确认。
function EditFile({
  path,
  initial,
  trail,
  busy,
  error,
  onCancel,
  onSave,
}: {
  path: string;
  initial: string;
  trail: (guard: (fn: () => void) => void, dirs: string[]) => Crumb[];
  busy: boolean;
  error: string;
  onCancel: () => void;
  onSave: (text: string) => void;
}) {
  const m = t.projectData.files;
  const [text, setText] = useState(initial);
  const dirty = useUnsaved(text);
  const guard = async (fn: () => void) => {
    if (dirty && !(await confirmAction(t.common.unsaved.confirm, { action: t.common.unsaved.leave, danger: true }))) return;
    fn();
  };
  const segments = path.split("/");
  const name = segments.pop() ?? path;
  return (
    <form
      id="edit-file"
      className="grid gap-4 max-md:-mb-12"
      onSubmit={(e) => {
        e.preventDefault();
        onSave(text);
      }}
    >
      <MobileBar
        left={
          <Button variant="ghost" className={mobileBarText} onClick={onCancel}>
            {t.common.actions.cancel}
          </Button>
        }
        title={name}
        right={
          <Button type="submit" form="edit-file" variant="ghost" className={cn(mobileBarText, "font-medium")} disabled={busy}>
            {busy && <Spinner />}
            {t.common.actions.save}
          </Button>
        }
      />
      <p className="text-[13px] leading-5 text-muted-foreground md:hidden">{m.editHint}</p>
      <header className="flex min-h-8 flex-wrap items-center justify-between gap-x-4 gap-y-2 max-md:hidden">
        <Crumbs parts={trail(guard, segments)} current={name} />
        <div className="flex flex-wrap items-center gap-3">
          <span className="text-[13px] text-muted-foreground max-md:hidden">{m.editHint}</span>
          <Button variant="outline" onClick={onCancel}>
            {t.common.actions.cancel}
          </Button>
          <Button type="submit" disabled={busy}>
            {busy && <Spinner />}
            {t.common.actions.save}
          </Button>
        </div>
      </header>
      {error && <Alert>{error}</Alert>}
      <Textarea aria-label={m.content} value={text} onChange={(e) => setText(e.target.value)} spellCheck={false} className="h-[600px] resize-none border-0 bg-muted p-3 font-mono text-[13px] leading-5 max-md:h-[calc(100dvh-7.75rem)] max-md:text-base max-md:leading-6" />
    </form>
  );
}
