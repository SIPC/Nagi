// 项目分组和节点标签（见 docs/panel.md#分组和标签）。分组和标签都没有单独的列表：有项目、节点用着就存在。
// 只有管理员能修改，因为它们决定成员的访问范围。
import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { X } from "lucide-react";
import { toast } from "sonner";
import { ask, confirmAction } from "@/components/confirm";
import { Alert, Button, badgeVariants, buttonVariants, DialogActions, EmptyRow, Field, Input, List, Row, RowMenu, Select, Spinner } from "@/components/ui";
import type { Node, PanelUser } from "@/gen/nagi/v1/api_pb";
import type { Project, ProjectSummary } from "@/gen/nagi/v1/common_pb";
import { errorMessage, nodeClient, projectClient, settingsClient } from "@/lib/api";
import { cn } from "@/lib/utils";
import { t } from "@/i18n";

export const labelMax = 32;
export const maxNodeTags = 10;

export type Label = { name: string; count: number };

const byName = (a: string, b: string) => a.localeCompare(b);

function count(names: string[]): Label[] {
  const m = new Map<string, number>();
  for (const n of names) if (n) m.set(n, (m.get(n) ?? 0) + 1);
  return [...m.keys()].sort(byName).map((name) => ({ name, count: m.get(name)! }));
}

// groupsOf：项目用到的分组，按名称排序。
export function groupsOf(projects: ProjectSummary[] | undefined): Label[] {
  return count((projects ?? []).map((s) => s.project?.group ?? ""));
}

// projectTagsOf：项目用到的标签，按名称排序。
export function projectTagsOf(projects: ProjectSummary[] | undefined): Label[] {
  return count((projects ?? []).flatMap((s) => s.project?.tags ?? []));
}

// nodeGroupsOf：节点用到的分组，按名称排序。
export function nodeGroupsOf(nodes: Node[] | undefined): Label[] {
  return count((nodes ?? []).map((n) => n.group));
}

// tagsOf：节点用到的标签，按名称排序。
export function tagsOf(nodes: Node[] | undefined): Label[] {
  return count((nodes ?? []).flatMap((n) => n.tags));
}

const tidy = (s: string) => s.trim();
const labelError = (s: string) => ([...tidy(s)].length > labelMax ? t.labels.tooLong : "");

// GroupSelect：选择分组：无、已有的分组，或者新建（换成输入框）。
export function GroupSelect({ id, value, groups, onChange }: { id: string; value: string; groups: string[]; onChange: (v: string) => void }) {
  const newValue = "\u0000new";
  const [creating, setCreating] = useState(false);
  // 新建分组前选的值：取消新建时恢复，不变成「无」。
  const [before, setBefore] = useState(value);
  if (creating) {
    return (
      <div className="flex gap-2">
        <Input id={id} value={value} onChange={(e) => onChange(e.target.value)} placeholder={t.labels.groupPlaceholder} maxLength={labelMax} autoFocus autoComplete="off" />
        <Button
          variant="ghost"
          onClick={() => {
            setCreating(false);
            onChange(before);
          }}
        >
          {t.common.actions.cancel}
        </Button>
      </div>
    );
  }
  const options = value && !groups.includes(value) ? [...groups, value].sort(byName) : groups;
  return (
    <Select
      id={id}
      value={value}
      onChange={(e) => {
        if (e.target.value === newValue) {
          setBefore(value);
          setCreating(true);
          onChange("");
        } else onChange(e.target.value);
      }}
    >
      <option value="">{t.labels.noGroup}</option>
      {options.map((g) => (
        <option key={g} value={g}>
          {g}
        </option>
      ))}
      <option value={newValue}>{t.labels.newGroup}</option>
    </Select>
  );
}

// TagChip：一个标签。onRemove 给了时带移除按钮。
export function TagChip({ name, onRemove, className }: { name: string; onRemove?: () => void; className?: string }) {
  return (
    // 外观同 Badge：全圆、px-2、12 号。
    <span className={cn(badgeVariants(), "max-w-full gap-0.5", onRemove && "pr-0.5", className)}>
      <span className="truncate">{name}</span>
      {onRemove && (
        <button type="button" aria-label={t.labels.removeTag(name)} className="inline-flex size-4 shrink-0 cursor-pointer items-center justify-center rounded-full outline-none hover:bg-accent hover:text-foreground focus-ring" onClick={onRemove}>
          <X className="size-3" />
        </button>
      )}
    </span>
  );
}

// TagEditor：编辑一组标签：输入后按回车（或逗号）添加，退格删掉最后一个。suggestions 是别的节点上已有的标签。
// 字段名、格式和错误都由这里的 Field 显示：出错时错误替换格式说明，不同时出现。
export function TagEditor({ id, label, hint, value, suggestions, onChange }: { id: string; label: string; hint?: string; value: string[]; suggestions: string[]; onChange: (v: string[]) => void }) {
  const [text, setText] = useState("");
  const listId = `${id}-suggestions`;
  const add = () => {
    const v = tidy(text);
    if (!v || labelError(v)) return;
    if (!value.includes(v) && value.length < maxNodeTags) onChange([...value, v]);
    setText("");
  };
  const error = labelError(text) || (value.length >= maxNodeTags && tidy(text) ? t.labels.tooMany : "");
  return (
    <Field label={label} htmlFor={id} hint={hint} error={error || undefined}>
      {/* 外框和 Input 一样：高 32（手机 40），1px 边框，聚焦时边框变 ring 色加 3px 光晕，出错时红框。 */}
      <div className={cn("flex min-h-8 flex-wrap items-center gap-1 rounded-md border border-input bg-transparent px-2 py-1 transition-[border-color,box-shadow] focus-within:border-ring focus-within:ring-3 focus-within:ring-ring/50 max-md:min-h-10", error && "border-destructive")}>
        {value.map((v) => (
          <TagChip key={v} name={v} onRemove={() => onChange(value.filter((x) => x !== v))} />
        ))}
        <input
          id={id}
          list={listId}
          value={text}
          onChange={(e) => setText(e.target.value)}
          onKeyDown={(e) => {
            if ((e.key === "Enter" || e.key === ",") && !e.nativeEvent.isComposing) {
              e.preventDefault();
              add();
            } else if (e.key === "Backspace" && !text && value.length > 0) {
              onChange(value.slice(0, -1));
            }
          }}
          onBlur={add}
          placeholder={value.length < maxNodeTags ? t.labels.tagPlaceholder : ""}
          aria-invalid={!!error || undefined}
          autoComplete="off"
          className="h-6 min-w-24 flex-1 bg-transparent px-1 text-sm outline-none placeholder:text-muted-foreground max-md:text-base"
        />
        <datalist id={listId}>
          {suggestions
            .filter((s) => !value.includes(s))
            .map((s) => (
              <option key={s} value={s} />
            ))}
        </datalist>
      </div>
    </Field>
  );
}

// EditNodeTags：节点的标签（放在对话框里）。
export function EditNodeTags({ node, allTags, onDone }: { node: Node; allTags: string[]; onDone: () => void }) {
  const queryClient = useQueryClient();
  const [tags, setTags] = useState(node.tags);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const save = async () => {
    setBusy(true);
    setError("");
    try {
      await nodeClient.setNodeTags({ nodeId: node.id, tags });
      await queryClient.invalidateQueries({ queryKey: ["nodes"] });
      onDone();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };
  return (
    <form
      className="grid gap-4"
      onSubmit={(e) => {
        e.preventDefault();
        save();
      }}
    >
      <TagEditor id="node-tags" label={t.labels.tags} hint={t.labels.tagsHint} value={tags} suggestions={allTags} onChange={setTags} />
      {error && <Alert>{error}</Alert>}
      <DialogActions>
        <Button variant="outline" onClick={onDone}>
          {t.common.actions.cancel}
        </Button>
        <Button type="submit" disabled={busy}>
          {busy && <Spinner />}
          {t.common.actions.save}
        </Button>
      </DialogActions>
    </form>
  );
}

// EditNodeGroup：节点的分组（放在对话框里）。
export function EditNodeGroup({ node, groups, onDone }: { node: Node; groups: string[]; onDone: () => void }) {
  const queryClient = useQueryClient();
  const [group, setGroup] = useState(node.group);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const save = async () => {
    setBusy(true);
    setError("");
    try {
      await nodeClient.setNodeGroup({ nodeId: node.id, group: tidy(group) });
      await queryClient.invalidateQueries({ queryKey: ["nodes"] });
      onDone();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };
  return (
    <form
      className="grid gap-4"
      onSubmit={(e) => {
        e.preventDefault();
        save();
      }}
    >
      <Field label={t.labels.group} htmlFor="node-group" error={labelError(group) || undefined}>
        <GroupSelect id="node-group" value={group} groups={groups} onChange={setGroup} />
      </Field>
      {error && <Alert>{error}</Alert>}
      <DialogActions>
        <Button variant="outline" onClick={onDone}>
          {t.common.actions.cancel}
        </Button>
        <Button type="submit" disabled={busy || !!labelError(group)}>
          {busy && <Spinner />}
          {t.common.actions.save}
        </Button>
      </DialogActions>
    </form>
  );
}

// EditProjectTags：项目的标签（放在对话框里）。
export function EditProjectTags({ project, allTags, onDone }: { project: Project; allTags: string[]; onDone: () => void }) {
  const queryClient = useQueryClient();
  const [tags, setTags] = useState(project.tags);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const save = async () => {
    setBusy(true);
    setError("");
    try {
      await projectClient.setProjectTags({ projectId: project.id, tags });
      await queryClient.invalidateQueries({ queryKey: ["project", project.id] });
      queryClient.invalidateQueries({ queryKey: ["projects"] });
      onDone();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };
  return (
    <form
      className="grid gap-4"
      onSubmit={(e) => {
        e.preventDefault();
        save();
      }}
    >
      <TagEditor id="project-tags" label={t.labels.tags} hint={t.labels.tagsHint} value={tags} suggestions={allTags} onChange={setTags} />
      {error && <Alert>{error}</Alert>}
      <DialogActions>
        <Button variant="outline" onClick={onDone}>
          {t.common.actions.cancel}
        </Button>
        <Button type="submit" disabled={busy}>
          {busy && <Spinner />}
          {t.common.actions.save}
        </Button>
      </DialogActions>
    </form>
  );
}

// TagFilter：按标签筛选，可以多选，带任一选中标签的节点都显示。
export function TagFilter({ tags, value, onChange }: { tags: Label[]; value: string[]; onChange: (v: string[]) => void }) {
  // 和其他按钮一样高 32、14 号的次要按钮；选中的用 accent 底色，不反白。
  const chip = buttonVariants({ variant: "outline" });
  const on = "bg-accent";
  const off = "text-muted-foreground hover:text-foreground";
  return (
    <div role="group" aria-label={t.labels.filterByTag} className="flex flex-wrap gap-2">
      <button type="button" aria-pressed={value.length === 0} className={cn(chip, value.length === 0 ? on : off)} onClick={() => onChange([])}>
        {t.labels.allTags}
      </button>
      {tags.map((tag) => {
        const pressed = value.includes(tag.name);
        return (
          <button
            key={tag.name}
            type="button"
            aria-pressed={pressed}
            className={cn(chip, pressed ? on : off)}
            onClick={() => onChange(pressed ? value.filter((v) => v !== tag.name) : [...value, tag.name])}
          >
            {tag.name}
            <span className={cn("tabular-nums", pressed ? "text-background/70" : "text-muted-foreground/70")}>{tag.count}</span>
          </button>
        );
      })}
    </div>
  );
}

// ---- 管理分组和标签 ----

// nodeGroup、projectTag：节点分组、项目标签，只整理列表，不在成员的访问范围里。
type Kind = "group" | "tag" | "nodeGroup" | "projectTag";
const inScope = (kind: Kind) => kind === "group" || kind === "tag";

function membersWith(users: PanelUser[] | undefined, kind: Kind, name: string) {
  if (!inScope(kind)) return [];
  return (users ?? [])
    .filter((u) => u.role !== "admin" && u.scope && (kind === "group" ? !u.scope.allProjects && u.scope.projectGroups.includes(name) : !u.scope.allNodes && u.scope.nodeTags.includes(name)))
    .map((u) => u.displayName || u.username);
}

// ManageLabels：重命名、删除分组或标签。访问范围用到它的成员一起改，删除前列出受影响的成员。
export function ManageLabels({ kind, labels }: { kind: Kind; labels: Label[] }) {
  const queryClient = useQueryClient();
  // 节点分组、项目标签不涉及成员，不用读成员列表。
  const users = useQuery({ queryKey: ["users"], queryFn: () => settingsClient.listUsers({}), enabled: inScope(kind) });
  const [renaming, setRenaming] = useState<string | null>(null);
  const m = t.labels;
  const ofProjects = kind === "group" || kind === "projectTag";
  const refresh = () => {
    queryClient.invalidateQueries({ queryKey: [ofProjects ? "projects" : "nodes"] });
    queryClient.invalidateQueries({ queryKey: ["users"] });
  };
  const rename = (name: string, newName: string) =>
    ({
      group: () => projectClient.renameProjectGroup({ name, newName }),
      tag: () => nodeClient.renameNodeTag({ name, newName }),
      nodeGroup: () => nodeClient.renameNodeGroup({ name, newName }),
      projectTag: () => projectClient.renameProjectTag({ name, newName }),
    })[kind]();
  const remove = async (l: Label) => {
    const members = membersWith(users.data?.users, kind, l.name);
    const confirm = { group: m.deleteGroupConfirm, tag: m.deleteTagConfirm, nodeGroup: m.deleteNodeGroupConfirm, projectTag: m.deleteProjectTagConfirm }[kind];
    const message = confirm(l.name, l.count) + (members.length > 0 ? m.affectedMembers(t.common.list(members)) : "");
    if (!(await confirmAction(message, { action: m.delete, danger: true }))) return;
    try {
      await rename(l.name, "");
      refresh();
    } catch (err) {
      toast.error(errorMessage(err));
    }
  };
  if (renaming !== null) {
    return <RenameLabel kind={kind} name={renaming} labels={labels} members={membersWith(users.data?.users, kind, renaming)} rename={rename} onDone={() => (refresh(), setRenaming(null))} onCancel={() => setRenaming(null)} />;
  }
  if (labels.length === 0) return <EmptyRow>{kind === "tag" || kind === "projectTag" ? m.noTags : m.noGroups}</EmptyRow>;
  return (
    <List className="max-h-[60vh] overflow-y-auto">
      {labels.map((l) => {
        const members = membersWith(users.data?.users, kind, l.name).length;
        return (
          <Row
            key={l.name}
            title={l.name}
            sub={[ofProjects ? m.projectCount(l.count) : m.nodeCount(l.count), members > 0 ? m.memberCount(members) : ""].filter(Boolean).join(" · ")}
            actions={
              <RowMenu
                items={[
                  { label: m.rename, onSelect: () => setRenaming(l.name) },
                  { label: m.delete, onSelect: () => remove(l), danger: true },
                ]}
              />
            }
            actionsWidth="w-9"
          />
        );
      })}
    </List>
  );
}

function RenameLabel({
  kind,
  name,
  labels,
  members,
  rename,
  onDone,
  onCancel,
}: {
  kind: Kind;
  name: string;
  labels: Label[];
  members: string[];
  rename: (name: string, newName: string) => Promise<unknown>;
  onDone: () => void;
  onCancel: () => void;
}) {
  const m = t.labels;
  const [value, setValue] = useState(name);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const next = tidy(value);
  const merging = next !== name && labels.some((l) => l.name === next);
  const save = async () => {
    if (!next || next === name) return onCancel();
    const merge = { group: m.mergeGroup, tag: m.mergeTag, nodeGroup: m.mergeNodeGroup, projectTag: m.mergeProjectTag }[kind];
    if (merging && !(await ask(merge(name, next), { action: m.merge }))) return;
    setBusy(true);
    setError("");
    try {
      await rename(name, next);
      onDone();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };
  return (
    <form
      className="grid gap-4"
      onSubmit={(e) => {
        e.preventDefault();
        save();
      }}
    >
      <Field label={m.newName} htmlFor="label-name" error={labelError(value)} help={members.length > 0 ? m.renamedMembers(t.common.list(members)) : undefined}>
        <Input id="label-name" value={value} onChange={(e) => setValue(e.target.value)} maxLength={labelMax} autoFocus required pattern=".*\S.*" autoComplete="off" />
      </Field>
      {error && <Alert>{error}</Alert>}
      <DialogActions>
        <Button variant="outline" onClick={onCancel}>
          {t.common.actions.cancel}
        </Button>
        <Button type="submit" disabled={busy || !!labelError(value)}>
          {busy && <Spinner />}
          {m.rename}
        </Button>
      </DialogActions>
    </form>
  );
}
