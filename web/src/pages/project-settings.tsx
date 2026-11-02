import { useState } from "react";
import type * as React from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { errorMessage, projectClient, toDate } from "@/lib/api";
import { formatDateTime, timeAgo } from "@/lib/utils";
import { t } from "@/i18n";
import { envToText, textToEnv } from "@/lib/config";
import { useRun } from "@/components/run";
import { useRole } from "@/components/role";
import { Alert, Button, Check, EmptyRow, Field, FormActions, FormBody, FormRow, Input, List, QueryError, Row, RowsSkeleton, Section, Spinner, Status } from "@/components/ui";
import type { Project } from "@/gen/nagi/v1/common_pb";
import { ask, confirmAction } from "@/components/confirm";
import { EnvEditor } from "@/components/env-editor";
import { toast } from "sonner";

// 「设置」标签页：基本设置、共享变量、部署记录、删除项目。

export function SettingsTab({ project }: { project: Project }) {
  return (
    <div>
      <GeneralSettings project={project} />
      <SharedEnv project={project} />
      <Deployments project={project} />
      <DeleteProject project={project} />
    </div>
  );
}

// HelpParts：问号里的几段说明，一段一行。
function HelpParts({ parts }: { parts: React.ReactNode[] }) {
  return (
    <span className="grid gap-1">
      {parts.map((p, i) => (
        <span key={i}>{p}</span>
      ))}
    </span>
  );
}

// GeneralSettings 修改名称、网络名和快照间隔；内部端口照原样提交（接口每次提交完整的设置）。
function GeneralSettings({ project }: { project: Project }) {
  const m = t.projectSettings.general;
  const queryClient = useQueryClient();
  const s = project.settings;
  const [name, setName] = useState(project.name);
  const [network, setNetwork] = useState(project.networkName);
  const [interval, setInterval] = useState(s?.snapshotIntervalMinutes ? String(s.snapshotIntervalMinutes) : "");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  const save = async () => {
    setBusy(true);
    setError("");
    try {
      await projectClient.updateProjectSettings({
        projectId: project.id,
        baseRevision: project.revision,
        name,
        networkName: network,
        settings: {
          internalPorts: s?.internalPorts,
          snapshotIntervalMinutes: Number(interval) || 0,
        },
      });
      toast.success(t.common.feedback.saved);
      queryClient.invalidateQueries({ queryKey: ["project", project.id] });
      queryClient.invalidateQueries({ queryKey: ["projects"] });
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Section title={m.title}>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          save();
        }}
      >
        <FormBody>
          <FormRow>
            <Field label={m.name} htmlFor="p-name">
              <Input id="p-name" value={name} onChange={(e) => setName(e.target.value)} required />
            </Field>
            <Field label={m.networkName} htmlFor="p-network" help={<HelpParts parts={[m.networkNameHint, m.networkNameHelp]} />}>
              <Input id="p-network" value={network} onChange={(e) => setNetwork(e.target.value)} required />
            </Field>
            <Field label={m.snapshotInterval} htmlFor="p-interval" help={m.snapshotIntervalHint}>
              <Input id="p-interval" value={interval} onChange={(e) => setInterval(e.target.value)} inputMode="numeric" placeholder="60" />
            </Field>
          </FormRow>
          {error && <Alert>{error}</Alert>}
          <FormActions>
            <Button type="submit" variant="outline" disabled={busy}>
              {busy && <Spinner />}
              {t.common.actions.save}
            </Button>
          </FormActions>
        </FormBody>
      </form>
    </Section>
  );
}

// ---- 共享变量 ----

function SharedEnv({ project }: { project: Project }) {
  const m = t.projectSettings.sharedEnv;
  const queryClient = useQueryClient();
  const { isViewer } = useRole();
  const [sharedEnv, setSharedEnv] = useState(envToText(project.hasSharedEnvDraft ? project.sharedEnvDraft : project.settings?.sharedEnv));
  // 编辑器只在挂载时读一次内容，放弃草稿后换一个 key 让它重新读。
  const [editorKey, setEditorKey] = useState(0);
  const [affected, setAffected] = useState<Record<string, boolean>>({});
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const { run, busy: running, output } = useRun(project.id);

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
      const res = await projectClient.saveSharedEnv({ projectId: project.id, baseRevision: project.revision, vars: textToEnv(sharedEnv) });
      setAffected(Object.fromEntries(res.affected.map((c) => [c, true])));
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

  const help = <HelpParts parts={[m.intro, m.description, m.help]} />;
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
          {project.hasSharedEnvDraft && <span className="text-[13px] leading-5 text-muted-foreground">{m.draft}</span>}
        </>
      }
      help={help}
    >
      <FormBody>
        <EnvEditor key={editorKey} id="shared-env" value={sharedEnv} onChange={setSharedEnv} />
        {project.hasSharedEnvDraft && project.containers.length > 0 && (
          <Field label={m.scope}>
            <div title={m.scopeHelp} className="flex flex-wrap gap-x-6 gap-y-2 [&_label]:leading-[14px] [&_label]:font-medium">
              {project.containers.map((c) => (
                <Check key={c.name} checked={!!affected[c.name]} onChange={(v) => setAffected({ ...affected, [c.name]: v })}>
                  {c.name}
                </Check>
              ))}
            </div>
          </Field>
        )}
        {error && <Alert>{error}</Alert>}
        {output}
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
  const list = useQuery({
    queryKey: ["deployments", project.id],
    queryFn: () => projectClient.listDeployments({ projectId: project.id }),
  });

  return (
    <Section title={m.title}>
      {output}
      {list.isPending ? (
        <RowsSkeleton />
      ) : list.isError ? (
        <QueryError error={list.error} onRetry={() => list.refetch()} />
      ) : list.data.deployments.length === 0 ? (
        <EmptyRow>{m.empty}</EmptyRow>
      ) : (
        <List>
          {list.data.deployments.map((d, i) => {
            const source = `${sourceLabel[d.source] ?? d.source}${d.restoreOf > 0n ? ` #${d.restoreOf}` : ""}`;
            // 第二行写这次动到的容器和它们的版本，所有容器的版本在悬停提示里。
            const touched = m.containerList(d.containers.map((c) => (d.versions[c] !== undefined ? `${c} v${d.versions[c]}` : c)));
            const all = Object.entries(d.versions)
              .map(([c, v]) => `${c} v${v}`)
              .join(" · ");
            const time = toDate(d.time);
            return (
              <Row
                key={String(d.id)}
                title={
                  <>
                    <span title={`${m.description} ${m.numberHelp}`}>{`#${d.id}`}</span>
                    {!d.success && (
                      <Status tone="danger">
                        <span className="text-destructive">{m.failed}</span>
                      </Status>
                    )}
                  </>
                }
                sub={<span title={all}>{[source, touched, d.success ? "" : d.error].filter(Boolean).join(" · ")}</span>}
                cols={[{ content: <span title={formatDateTime(time)}>{timeAgo(time)}</span> }]}
                actions={
                  i > 0 ? (
                    <Button
                      size="row"
                      variant="ghost"
                      disabled={busy}
                      onClick={async () => (await confirmAction(m.restoreConfirm(d.id), { action: m.restore })) && run(() => projectClient.restoreDeployment({ projectId: project.id, deploymentId: d.id }))}
                    >
                      {m.restore}
                    </Button>
                  ) : null
                }
              />
            );
          })}
        </List>
      )}
    </Section>
  );
}

// ---- 删除项目 ----

function DeleteProject({ project }: { project: Project }) {
  const m = t.projectSettings.delete;
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [deleteDir, setDeleteDir] = useState(false);
  const [error, setError] = useState("");

  const remove = async () => {
    // 同时删除目录时数据无法恢复，确认框里要输入项目名称。
    const answer = deleteDir
      ? await ask(m.confirmDir(project.name), { action: m.submit, danger: true, checkDefault: true, typeToConfirm: { label: m.confirmName(project.name), value: project.name } })
      : await ask(m.confirm(project.name), { action: m.submit, danger: true });
    if (!answer) return;
    setError("");
    try {
      await projectClient.deleteProject({ projectId: project.id, baseRevision: project.revision, deleteDir, confirmName: deleteDir ? project.name : "" });
      queryClient.invalidateQueries({ queryKey: ["projects"] });
      navigate({ to: "/projects" });
    } catch (err) {
      setError(errorMessage(err));
    }
  };

  return (
    <Section title={m.title}>
      <p className="text-muted-foreground">{m.description}</p>
      <div className="grid justify-items-start gap-2">
        <Check checked={deleteDir} onChange={setDeleteDir}>
          {m.deleteDir}
        </Check>
      </div>
      {error && <Alert>{error}</Alert>}
      <div>
        <Button variant="destructive-outline" title={m.help} onClick={remove}>
          {m.submit}
        </Button>
      </div>
    </Section>
  );
}
