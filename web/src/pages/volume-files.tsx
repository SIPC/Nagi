// 卷里的文件管理：浏览、下载、上传、删除、编辑小文本，以及查看快照里的文件并恢复单个文件。
// 规则在节点上执行（只读角色看不到入口，接口也拒绝；有顶替的容器只在持有租约时能写）。
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ChevronRight, Download, File, Folder, FolderPlus, Link2, Upload } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { toast } from "sonner";
import { ask, confirmAction } from "@/components/confirm";
import { Alert, Button, Dialog, EmptyRow, Field, Input, Select, Spinner, Table, Td, Textarea, Th } from "@/components/ui";
import { fmtBytes } from "@/components/metrics";
import type { VolumeFileEntry } from "@/gen/nagi/v1/agent_pb";
import type { Container, Project } from "@/gen/nagi/v1/common_pb";
import { locale, t } from "@/i18n";
import { errorMessage, projectClient, toDate } from "@/lib/api";
import { formatDateTime, timeAgo } from "@/lib/utils";

// 目录一次显示的条数，内容多时点「显示更多」。
const pageSize = 200;

const maxEditBytes = 1 << 20;
const maxUploadBytes = 8 * 1024 ** 3;

type Props = {
  project: Project;
  container: Container;
  volume: string;
  label: string;
  onClose: () => void;
};

export function VolumeFiles({ project, container, volume, label, onClose }: Props) {
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
  useEffect(() => setShown(pageSize), [path, snapshot]);
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

  const run = async (key: string, fn: () => Promise<unknown>, done?: string) => {
    setBusy(key);
    setError("");
    try {
      await fn();
      if (done) toast.success(done);
      refresh();
    } catch (err) {
      setError(errorMessage(err));
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
      const answer = await ask(m.overwriteConfirm(clash.join(", ")), { action: m.overwrite, danger: true, check: m.snapshotFirst });
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
    const answer = await ask(m.deleteConfirm(e.name, e.isDir), { action: t.common.actions.delete, danger: true, check: m.snapshotFirst });
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

  return (
    <Dialog open onClose={onClose} title={m.title(label)} description={snapshot ? m.snapshotMode : m.description} className="max-w-4xl">
      <div className="grid gap-3">
        <div className="flex flex-wrap items-center gap-2">
          <Select
            aria-label={m.source}
            className="h-8 w-56 text-[13px]"
            value={snapshot || "live"}
            onChange={(e) => {
              setSnapshot(e.target.value === "live" ? "" : e.target.value);
              setPath("");
              setError("");
            }}
          >
            <option value="live">{m.live}</option>
            {choices.map((s) => (
              <option key={s.id} value={s.id}>
                {m.snapshotOption(formatDateTime(toDate(s.time)) ?? "")}
              </option>
            ))}
          </Select>
          <div className="ml-auto flex flex-wrap items-center gap-1">
            <Button size="sm" variant="ghost" onClick={() => download(path)}>
              <Download />
              {m.downloadDir}
            </Button>
            {!snapshot && (
              <>
                <Button size="sm" variant="ghost" onClick={() => setCreating(!creating)}>
                  <FolderPlus />
                  {m.newDir}
                </Button>
                <Button size="sm" variant="outline" disabled={busy === "upload"} onClick={() => inputRef.current?.click()}>
                  {busy === "upload" ? <Spinner /> : <Upload />}
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
          </div>
        </div>

        {creating && !snapshot && (
          <form
            className="flex items-center gap-2"
            onSubmit={(e) => {
              e.preventDefault();
              const name = dirName.trim();
              if (!name) return;
              run("mkdir", () => projectClient.createVolumeDir({ ...ref, snapshotId: "", path: join(name) }), m.created).then(() => {
                setDirName("");
                setCreating(false);
              });
            }}
          >
            <Input autoFocus aria-label={m.dirName} placeholder={m.dirName} value={dirName} onChange={(e) => setDirName(e.target.value)} className="h-8 max-w-64 font-mono text-[13px]" />
            <Button type="submit" size="sm" variant="outline" disabled={busy === "mkdir"}>
              {t.common.actions.create}
            </Button>
          </form>
        )}

        <nav aria-label={m.pathLabel} className="flex flex-wrap items-center gap-0.5 font-mono text-[13px]">
          <button type="button" className="cursor-pointer rounded-sm px-1 hover:bg-accent" onClick={() => setPath("")}>
            {label}
          </button>
          {parts.map((part, i) => (
            <span key={i} className="flex items-center gap-0.5">
              <ChevronRight className="size-3.5 text-muted-foreground" />
              <button type="button" className="cursor-pointer rounded-sm px-1 hover:bg-accent" onClick={() => setPath(parts.slice(0, i + 1).join("/"))}>
                {part}
              </button>
            </span>
          ))}
        </nav>

        {error && <Alert>{error}</Alert>}

        <div className="max-h-[50vh] overflow-y-auto pb-2.5">
          {files.isPending ? (
            <EmptyRow>
              <Spinner />
            </EmptyRow>
          ) : files.error ? (
            <Alert>{errorMessage(files.error)}</Alert>
          ) : files.data.entries.length === 0 ? (
            <EmptyRow>{m.empty}</EmptyRow>
          ) : (
            <Table compact>
              <thead>
                <tr>
                  <Th>{m.columns.name}</Th>
                  <Th>{m.columns.size}</Th>
                  <Th className="hidden md:table-cell">{m.columns.modified}</Th>
                  <Th />
                </tr>
              </thead>
              <tbody>
                {files.data.entries.slice(0, shown).map((e) => (
                  <tr key={e.name}>
                    <Td className="font-mono text-xs">
                      {e.isDir ? (
                        <button type="button" className="inline-flex cursor-pointer items-center gap-1.5 hover:underline" onClick={() => setPath(join(e.name))}>
                          <Folder className="size-3.5 text-muted-foreground" />
                          {e.name}
                        </button>
                      ) : (
                        <span className="inline-flex items-center gap-1.5" title={e.isSymlink ? m.symlinkHint : undefined}>
                          {e.isSymlink ? <Link2 className="size-3.5 text-muted-foreground" /> : <File className="size-3.5 text-muted-foreground" />}
                          {e.name}
                        </span>
                      )}
                    </Td>
                    <Td className="text-xs whitespace-nowrap text-muted-foreground">{e.isDir ? "—" : fmtBytes(Number(e.size))}</Td>
                    <Td className="hidden text-xs text-muted-foreground md:table-cell" title={formatDateTime(toDate(e.modified))}>
                      {timeAgo(toDate(e.modified))}
                    </Td>
                    <Td className="text-right whitespace-nowrap">
                      {!e.isSymlink && (
                        <Button size="xs" variant="ghost" onClick={() => download(join(e.name))}>
                          {m.download}
                        </Button>
                      )}
                      {snapshot
                        ? !e.isSymlink && (
                            <Button size="xs" variant="ghost" disabled={!!busy} onClick={() => restore(e.name)}>
                              {m.restore}
                            </Button>
                          )
                        : !e.isDir &&
                          !e.isSymlink &&
                          Number(e.size) <= maxEditBytes && (
                            <Button size="xs" variant="ghost" disabled={!!busy} onClick={() => edit(e)}>
                              {t.common.actions.edit}
                            </Button>
                          )}
                      {!snapshot && (
                        <Button size="xs" variant="ghost" className="hover:text-destructive" disabled={!!busy} onClick={() => remove(e)}>
                          {t.common.actions.delete}
                        </Button>
                      )}
                    </Td>
                  </tr>
                ))}
              </tbody>
            </Table>
          )}
          {(files.data?.entries.length ?? 0) > shown && (
            <div className="mt-2 flex justify-center">
              <Button readOnlyOk size="sm" variant="ghost" onClick={() => setShown(shown + pageSize)}>
                {m.showMore((files.data?.entries.length ?? 0) - shown)}
              </Button>
            </div>
          )}
          {files.data?.truncated && <p className="mt-2 text-xs text-muted-foreground">{m.truncated}</p>}
        </div>
      </div>

      <Dialog open={!!editing} onClose={() => setEditing(null)} title={m.editTitle(editing?.path ?? "")} description={m.editHint} className="max-w-3xl">
        {editing && (
          <form
            className="grid gap-3"
            onSubmit={(e) => {
              e.preventDefault();
              run("save", () => uploadFile(editing.path, new Blob([editing.text])), t.common.feedback.saved).then(() => setEditing(null));
            }}
          >
            <Field label={m.content} htmlFor="volume-file-text">
              <Textarea
                id="volume-file-text"
                value={editing.text}
                onChange={(e) => setEditing({ ...editing, text: e.target.value })}
                spellCheck={false}
                className="min-h-80 font-mono text-[13px]"
              />
            </Field>
            {error && <Alert>{error}</Alert>}
            <div className="flex justify-end gap-2">
              <Button size="sm" variant="ghost" onClick={() => setEditing(null)}>
                {t.common.actions.cancel}
              </Button>
              <Button type="submit" size="sm" disabled={busy === "save"}>
                {busy === "save" && <Spinner />}
                {t.common.actions.save}
              </Button>
            </div>
          </form>
        )}
      </Dialog>
    </Dialog>
  );
}
