import { WriteGate } from "@/components/perm";
import { useState } from "react";
import type * as React from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { errorMessage, projectClient, toDate } from "@/lib/api";
import { capitalize, formatDateTime, timeAgo } from "@/lib/utils";
import { t } from "@/i18n";
import { defaultSnapshotLimit, envToText, textToEnv } from "@/lib/config";
import { projectSchedule, sameForm, scheduleErrors, ScheduleFields, scheduleForm, scheduleSummary, toSchedule } from "@/components/snapshot-schedule";
import { useRun } from "@/components/run";
import { withPrivilegedElevation } from "@/components/elevate";
import { useRole } from "@/components/role";
import { Alert, Badge, Button, Check, Dialog, DialogActions, EmptyRow, Field, FormActions, FormBody, Input, List, QueryError, Row, RowsSkeleton, Section, SettingRow, SettingRows, Spinner, Status } from "@/components/ui";
import { HelpParts } from "@/components/help-parts";
import type { Project } from "@/gen/nagi/v1/common_pb";
import { ask, confirmAction } from "@/components/confirm";
import { EnvEditor } from "@/components/env-editor";
import { AlertRulesLink } from "@/pages/alert-rules";
import { useUnsaved } from "@/components/unsaved";
import { toast } from "sonner";
import { EditProjectTags, GroupSelect, groupsOf, projectTagsOf } from "@/components/labels";
import { EgressSection } from "@/pages/project-egress";
import { containerLabel } from "@/pages/project-containers";

// 「设置」标签页：基本设置、共享变量、部署记录、删除项目。

// 每块要的权限（见 internal/server/auth/perms.go）：没有的不显示；基本设置都能看，改要「项目管理」。
export function SettingsTab({ project }: { project: Project }) {
  const { can } = useRole();
  return (
    <div>
      <WriteGate allowed={can("project.manage")}>
        <GeneralSettings project={project} />
      </WriteGate>
      {can("project.snapshots") && (
        <WriteGate allowed={can("project.manage")}>
          <SnapshotScheduleSection project={project} />
        </WriteGate>
      )}
      {can("project.alerts") && <AlertRulesSection project={project} />}
      {can("project.network") && (
        <WriteGate allowed={can("project.manage")}>
          <EgressSection key={project.revision} project={project} />
        </WriteGate>
      )}
      {can("project.config") && (
        <WriteGate allowed={can("project.deploy")}>
          <SharedEnv project={project} />
          <Deployments project={project} />
        </WriteGate>
      )}
      {can("project.manage") && <DeleteProject project={project} />}
    </div>
  );
}

// 接口每次提交完整的设置：没改的照原样带上（没带的定时快照计划、告警规则面板保留）。
type SettingsPatch = { name?: string; networkName?: string; snapshotLimit?: number; snapshotSchedule?: ReturnType<typeof toSchedule> };
function useUpdateSettings(project: Project) {
  const queryClient = useQueryClient();
  return async (patch: SettingsPatch) => {
    const s = project.settings;
    await projectClient.updateProjectSettings({
      projectId: project.id,
      baseRevision: project.revision,
      name: patch.name ?? project.name,
      networkName: patch.networkName ?? project.networkName,
      settings: {
        internalPorts: s?.internalPorts,
        snapshotIntervalMinutes: s?.snapshotIntervalMinutes ?? 0,
        snapshotLimit: patch.snapshotLimit ?? s?.snapshotLimit ?? 0,
        ...(patch.snapshotSchedule ? { snapshotSchedule: patch.snapshotSchedule } : {}),
      },
    });
    await queryClient.invalidateQueries({ queryKey: ["project", project.id] });
    queryClient.invalidateQueries({ queryKey: ["projects"] });
  };
}

// SettingDialog：设置行的「编辑」对话框：字段、错误、取消和保存。保存出错时错误显示在按钮上方。
function SettingDialog({ title, busy, error, onSubmit, onClose, children }: { title: string; busy: boolean; error: string; onSubmit: () => void; onClose: () => void; children: React.ReactNode }) {
  return (
    <Dialog open onClose={onClose} title={title}>
      <form
        className="grid gap-4"
        onSubmit={(e) => {
          e.preventDefault();
          onSubmit();
        }}
      >
        {children}
        {error && <Alert>{error}</Alert>}
        <DialogActions>
          <Button variant="outline" onClick={onClose}>
            {t.common.actions.cancel}
          </Button>
          <Button type="submit" disabled={busy}>
            {busy && <Spinner />}
            {t.common.actions.save}
          </Button>
        </DialogActions>
      </form>
    </Dialog>
  );
}

// useSave：对话框的保存。save 返回 false 表示没有提交（确认框里取消了），对话框留着。
function useSave(onClose: () => void) {
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const submit = async (save: () => Promise<boolean | void>) => {
    setBusy(true);
    setError("");
    try {
      if ((await save()) !== false) onClose();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };
  return { error, busy, submit };
}

// TextSettingDialog：一个文字字段的设置。validate 返回字段下方的错误，提交过一次以后才显示。
function TextSettingDialog({
  title,
  label,
  initial,
  hint,
  help,
  placeholder,
  numeric,
  validate,
  onSave,
  onClose,
}: {
  title: string;
  label: string;
  initial: string;
  hint?: string;
  help?: React.ReactNode;
  placeholder?: string;
  numeric?: boolean;
  validate: (v: string) => string;
  onSave: (v: string) => Promise<boolean | void>;
  onClose: () => void;
}) {
  const [value, setValue] = useState(initial);
  const [checked, setChecked] = useState(false);
  const { error, busy, submit } = useSave(onClose);
  const fieldError = checked ? validate(value) : "";
  return (
    <SettingDialog
      title={title}
      busy={busy}
      error={error}
      onClose={onClose}
      onSubmit={() => {
        setChecked(true);
        if (validate(value)) return;
        if (value.trim() === initial.trim()) return onClose();
        submit(() => onSave(value.trim()));
      }}
    >
      <Field label={label} htmlFor="setting-edit" hint={hint} help={help} error={fieldError}>
        <Input id="setting-edit" value={value} onChange={(e) => setValue(e.target.value)} placeholder={placeholder} inputMode={numeric ? "numeric" : undefined} autoComplete="off" autoCapitalize="none" spellCheck={false} />
      </Field>
    </SettingDialog>
  );
}

// GeneralSettings：名称、网络名、分组，一项一行，「编辑」打开对话框。
function GeneralSettings({ project }: { project: Project }) {
  const m = t.projectSettings.general;
  // 分组归「成员」权限。
  const isAdmin = useRole().can("panel.members");
  const update = useUpdateSettings(project);
  // 标签只整理项目页，和其他基本设置一样要「项目管理」（这一页本来就要）。
  const [editing, setEditing] = useState<"name" | "network" | "group" | "tags" | null>(null);
  const close = () => setEditing(null);
  const edit = (what: typeof editing) => (
    <Button variant="outline" onClick={() => setEditing(what)}>
      {t.common.actions.edit}
    </Button>
  );
  const networkHelp = <HelpParts parts={[m.networkNameHint, m.networkNameHelp]} />;
  return (
    <Section title={m.title}>
      <SettingRows>
        <SettingRow label={m.name} description={project.name}>
          {edit("name")}
        </SettingRow>
        <SettingRow label={m.networkName} help={networkHelp} description={project.networkName}>
          {edit("network")}
        </SettingRow>
        {/* 分组决定成员的访问范围，只有管理员能编辑（见 docs/panel.md#分组和标签）。 */}
        {isAdmin && (
          <SettingRow label={t.labels.group} help={t.labels.groupHint} description={project.group || t.labels.noGroup}>
            {edit("group")}
          </SettingRow>
        )}
        <SettingRow label={t.labels.tags} help={t.labels.projectTagsHint} description={project.tags.length > 0 ? t.common.list(project.tags) : t.labels.noGroup}>
          {edit("tags")}
        </SettingRow>
      </SettingRows>
      {editing === "name" && (
        <TextSettingDialog
          title={m.editName}
          label={m.name}
          initial={project.name}
          validate={(v) => (v.trim() ? "" : m.nameRequired)}
          onSave={(name) => update({ name })}
          onClose={close}
        />
      )}
      {editing === "network" && (
        <TextSettingDialog
          title={m.editNetworkName}
          label={m.networkName}
          initial={project.networkName}
          hint={m.networkNameFormat}
          help={networkHelp}
          validate={(v) => (v.trim() ? "" : m.networkNameRequired)}
          onSave={async (networkName) => {
            // 改网络名后，已连接项目里写的地址会失效，先确认。
            if (project.links.length > 0 && !(await confirmAction(m.renameConfirm(networkName), { action: m.rename }))) return false;
            await update({ networkName });
          }}
          onClose={close}
        />
      )}
      {editing === "group" && <GroupDialog project={project} onClose={close} />}
      <Dialog open={editing === "tags"} onClose={close} title={t.labels.editTags} description={t.labels.projectTagsHint}>
        <ProjectTagsForm project={project} onDone={close} />
      </Dialog>
    </Section>
  );
}

// ProjectTagsForm：项目的标签，别的项目已有的标签作为候选。
function ProjectTagsForm({ project, onDone }: { project: Project; onDone: () => void }) {
  const projects = useQuery({ queryKey: ["projects"], queryFn: () => projectClient.listProjects({}) });
  return <EditProjectTags project={project} allTags={projectTagsOf(projects.data?.projects).map((x) => x.name)} onDone={onDone} />;
}

// GroupDialog：编辑项目的分组，可以新建一个分组。
function GroupDialog({ project, onClose }: { project: Project; onClose: () => void }) {
  const queryClient = useQueryClient();
  const projects = useQuery({ queryKey: ["projects"], queryFn: () => projectClient.listProjects({}) });
  const groups = groupsOf(projects.data?.projects).map((g) => g.name);
  const [group, setGroup] = useState(project.group);
  const { error, busy, submit } = useSave(onClose);
  return (
    <SettingDialog
      title={t.projectSettings.general.editGroup}
      busy={busy}
      error={error}
      onClose={onClose}
      onSubmit={() => {
        if (group.trim() === project.group) return onClose();
        submit(async () => {
          await projectClient.setProjectGroup({ projectId: project.id, group: group.trim() });
          await queryClient.invalidateQueries({ queryKey: ["project", project.id] });
          queryClient.invalidateQueries({ queryKey: ["projects"] });
        });
      }}
    >
      <Field label={t.labels.group} htmlFor="p-group">
        <GroupSelect id="p-group" value={group} groups={groups} onChange={setGroup} />
      </Field>
    </SettingDialog>
  );
}

// ---- 定时快照 ----

// SnapshotScheduleSection：项目里容器的定时快照计划和份数上限。容器可以在「存储」标签里单独设置计划。
function SnapshotScheduleSection({ project }: { project: Project }) {
  const m = t.projectData.schedule;
  const g = t.projectSettings.general;
  const update = useUpdateSettings(project);
  const [editing, setEditing] = useState<"schedule" | "limit" | null>(null);
  const close = () => setEditing(null);
  const limit = project.settings?.snapshotLimit ?? 0;
  return (
    <Section title={m.title}>
      <SettingRows>
        <SettingRow label={m.plan} description={scheduleSummary(projectSchedule(project.settings))}>
          <Button variant="outline" onClick={() => setEditing("schedule")}>
            {t.common.actions.edit}
          </Button>
        </SettingRow>
        <SettingRow label={g.snapshotLimit} help={g.snapshotLimitHelp} description={limit > 0 ? g.limitValue(limit) : g.limitDefault(defaultSnapshotLimit)}>
          <Button variant="outline" onClick={() => setEditing("limit")}>
            {t.common.actions.edit}
          </Button>
        </SettingRow>
      </SettingRows>
      {editing === "schedule" && <ScheduleDialog project={project} onClose={close} />}
      {editing === "limit" && (
        <TextSettingDialog
          title={g.editSnapshotLimit}
          label={g.snapshotLimit}
          initial={limit > 0 ? String(limit) : ""}
          help={g.snapshotLimitHelp}
          placeholder={String(defaultSnapshotLimit)}
          numeric
          // 留空用默认值，否则须为正整数。
          validate={(v) => (v.trim() && !/^[1-9]\d*$/.test(v.trim()) ? g.positiveInteger : "")}
          onSave={(v) => update({ snapshotLimit: Number(v || 0) })}
          onClose={close}
        />
      )}
    </Section>
  );
}

// ScheduleDialog：编辑项目的定时快照计划。保存后节点按新计划执行，所以提示「已保存并下发到节点」。
function ScheduleDialog({ project, onClose }: { project: Project; onClose: () => void }) {
  const m = t.projectData.schedule;
  const update = useUpdateSettings(project);
  const s = project.settings;
  const saved = scheduleForm(projectSchedule(s));
  const [form, setForm] = useState(saved);
  const [checked, setChecked] = useState(false);
  const { error, busy, submit } = useSave(onClose);
  const errors = checked ? scheduleErrors(form) : {};
  return (
    <SettingDialog
      title={m.title}
      busy={busy}
      error={error}
      onClose={onClose}
      onSubmit={() => {
        setChecked(true);
        if (Object.keys(scheduleErrors(form)).length > 0) return;
        if (sameForm(form, saved)) return onClose();
        submit(async () => {
          await update({ snapshotSchedule: toSchedule(form, s?.snapshotSchedule) });
          toast.success(m.saved);
        });
      }}
    >
      <ScheduleFields id="p-schedule" value={form} onChange={setForm} errors={errors} note={m.disabledNote} />
    </SettingDialog>
  );
}

// ---- 共享变量 ----

function SharedEnv({ project }: { project: Project }) {
  const m = t.projectSettings.sharedEnv;
  const queryClient = useQueryClient();
  const { can } = useRole();
  const isViewer = !can("project.config");
  const [sharedEnv, setSharedEnv] = useState(envToText(project.hasSharedEnvDraft ? project.sharedEnvDraft : project.settings?.sharedEnv));
  // 编辑器只在挂载时读一次内容，放弃草稿后换一个 key 让它重新读。
  const [editorKey, setEditorKey] = useState(0);
  const [affected, setAffected] = useState<Record<string, boolean>>({});
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const { run, busy: running, output } = useRun(project.id);
  // 和已保存的变量（有草稿时是草稿）比较，按变量比，不管空行和顺序以外的写法差异。
  useUnsaved(envToText(textToEnv(sharedEnv)) !== envToText(project.hasSharedEnvDraft ? project.sharedEnvDraft : project.settings?.sharedEnv), !isViewer);

  const call = async (fn: () => Promise<unknown>) => {
    setBusy(true);
    setError("");
    try {
      await fn();
      queryClient.invalidateQueries({ queryKey: ["project", project.id] });
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const save = () =>
    call(async () => {
      const res = await withPrivilegedElevation(() => projectClient.saveSharedEnv({ projectId: project.id, baseRevision: project.revision, vars: textToEnv(sharedEnv) }));
      setAffected(Object.fromEntries(res.affected.map((c) => [c, true])));
      // 再次保存时页面上没有变化，所以给提示。
      toast.success(m.savedDraft);
    });

  const discard = async () => {
    if (!(await confirmAction(m.discardConfirm, { action: m.discardDraft, danger: true }))) return;
    await call(async () => {
      await projectClient.discardSharedEnv({ projectId: project.id, baseRevision: project.revision });
      setSharedEnv(envToText(project.settings?.sharedEnv));
      setEditorKey((k) => k + 1);
      setAffected({});
    });
  };

  const deploy = async () => {
    const n = project.containers.filter((c) => affected[c.name] && c.deployedVersion > 0n).length;
    if (n > 0 && !(await confirmAction(m.deployConfirm(n), { action: t.projectContainers.detail.deploy }))) return;
    run(() =>
      projectClient.deploy({
        projectId: project.id,
        applySharedEnv: true,
        settingsRevision: project.revision,
        targets: project.containers.filter((c) => affected[c.name]).map((c) => ({ container: c.name, baseRevision: c.revision })),
      }),
    );
  };

  const help = <HelpParts parts={[m.intro, m.help]} />;
  // 只读角色拿到的共享变量是空的（面板去掉了），不能显示成一个空编辑器。
  if (isViewer) {
    return (
      <Section title={m.title} help={help}>
        <p className="text-muted-foreground">{m.viewerHidden}</p>
      </Section>
    );
  }

  return (
    <Section
      title={
        <>
          {m.title}
          {project.hasSharedEnvDraft && (
            <Badge tone="warning" className="ml-1 font-normal">
              {m.draft}
            </Badge>
          )}
        </>
      }
      help={help}
    >
      {/* 操作输出和部署记录一样放在区块顶部。 */}
      {output}
      <FormBody>
        <EnvEditor key={editorKey} id="shared-env" value={sharedEnv} onChange={setSharedEnv} />
        {project.hasSharedEnvDraft && project.containers.length > 0 && (
          <Field label={m.scope} help={m.scopeHelp}>
            <div role="group" aria-label={m.scope} className="flex flex-wrap gap-x-6 gap-y-2">
              {project.containers.map((c) => (
                <Check key={c.name} checked={!!affected[c.name]} onChange={(v) => setAffected({ ...affected, [c.name]: v })}>
                  {containerLabel(c)}
                </Check>
              ))}
            </div>
          </Field>
        )}
        {error && <Alert>{error}</Alert>}
        <FormActions>
          {project.hasSharedEnvDraft && (
            <Button onClick={deploy} disabled={running}>
              {running && <Spinner />}
              {m.deploy}
            </Button>
          )}
          <Button variant="outline" onClick={save} disabled={busy}>
            {busy && <Spinner />}
            {m.saveDraft}
          </Button>
          {project.hasSharedEnvDraft && (
            <Button variant="ghost" onClick={discard} disabled={busy}>
              {m.discardDraft}
            </Button>
          )}
        </FormActions>
      </FormBody>
    </Section>
  );
}

// ---- 告警规则 ----

// AlertRulesSection：规则集中在 设置 → 告警，这里只显示本项目覆盖了几项和跳转。
function AlertRulesSection({ project }: { project: Project }) {
  return (
    <Section title={t.alerts.rules}>
      <SettingRows>
        <AlertRulesLink scope="project" projectId={project.id} />
      </SettingRows>
    </Section>
  );
}

// ---- 部署记录 ----

const sourceLabel: Record<string, string> = {
  manual: t.projectSettings.deployments.sources.manual,
  git: t.projectSettings.deployments.sources.git,
  image: t.projectSettings.deployments.sources.image,
  rollback: t.projectSettings.deployments.sources.rollback,
  shared_env: t.projectSettings.deployments.sources.sharedEnv,
  restore: t.projectSettings.deployments.sources.restore,
};

function Deployments({ project }: { project: Project }) {
  const m = t.projectSettings.deployments;
  const { run, busy, output } = useRun(project.id);
  // 默认只列最近几次，其余收起。
  const [expanded, setExpanded] = useState(false);
  const list = useQuery({
    queryKey: ["deployments", project.id],
    queryFn: () => projectClient.listDeployments({ projectId: project.id }),
  });

  return (
    <Section title={m.title} help={<HelpParts parts={[m.description, m.numberHelp]} />}>
      {output}
      {list.isPending ? (
        <RowsSkeleton />
      ) : list.isError ? (
        <QueryError error={list.error} onRetry={() => list.refetch()} />
      ) : list.data.deployments.length === 0 ? (
        <EmptyRow>{m.empty}</EmptyRow>
      ) : (
        <>
          <List>
            {(expanded ? list.data.deployments : list.data.deployments.slice(0, 6)).map((d, i) => {
              const source = `${sourceLabel[d.source] ?? d.source}${d.restoreOf > 0n ? ` #${d.restoreOf}` : ""}`;
              // 第二行写这次动到的容器和它们的版本，所有容器的版本在悬停提示里。
              const touched = t.common.list(d.containers.map((c) => (d.versions[c] !== undefined ? `${c} v${d.versions[c]}` : c)));
              const all = Object.entries(d.versions)
                .map(([c, v]) => `${c} v${v}`)
                .join(" · ");
              const time = toDate(d.time);
              return (
                <Row
                  key={String(d.id)}
                  title={
                    <>
                      <span>{`#${d.id}`}</span>
                      {!d.success && (
                        <Status tone="danger">
                          <span className="text-destructive">{m.failed}</span>
                        </Status>
                      )}
                    </>
                  }
                  sub={<span title={all}>{[source, touched, d.success ? "" : d.error].filter(Boolean).join(" · ")}</span>}
                  cols={[{ content: <span title={formatDateTime(time)}>{capitalize(timeAgo(time))}</span> }]}
                  actions={
                    i > 0 ? (
                      <Button
                        size="row"
                        variant="ghost"
                        disabled={busy}
                        onClick={async () => (await confirmAction(m.restoreConfirm(d.id), { action: m.restoreAction })) && run(() => projectClient.restoreDeployment({ projectId: project.id, deploymentId: d.id }))}
                      >
                        {m.restore}
                      </Button>
                    ) : null
                  }
                />
              );
            })}
          </List>
          {!expanded && list.data.deployments.length > 6 && (
            <div>
              <Button readOnlyOk variant="ghost" className="-ml-3" onClick={() => setExpanded(true)}>
                {m.showAll(list.data.deployments.length)}
              </Button>
            </div>
          )}
        </>
      )}
    </Section>
  );
}

// ---- 删除项目 ----

function DeleteProject({ project }: { project: Project }) {
  const m = t.projectSettings.delete;
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  const remove = async () => {
    // 「同时删除容器目录」在确认框里；勾选后数据无法恢复，要输入项目名称。
    const answer = await ask(m.confirm(project.name), { action: m.submit, danger: true, check: m.deleteDir, typeToConfirm: { label: m.confirmName(project.name), value: project.name } });
    if (!answer) return;
    const deleteDir = answer.checked;
    setError("");
    setBusy(true);
    try {
      await projectClient.deleteProject({ projectId: project.id, baseRevision: project.revision, deleteDir, confirmName: deleteDir ? project.name : "" });
      queryClient.invalidateQueries({ queryKey: ["projects"] });
      navigate({ to: "/projects" });
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };

  return (
    <Section title={m.title} tone="danger" help={m.help}>
      {error && <Alert>{error}</Alert>}
      <SettingRows>
        <SettingRow label={m.submit} description={m.description}>
          <Button variant="destructive-outline" disabled={busy} onClick={remove}>
            {busy && <Spinner />}
            {m.submit}
          </Button>
        </SettingRow>
      </SettingRows>
    </Section>
  );
}
