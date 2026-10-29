import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { Rocket } from "lucide-react";
import { errorMessage, projectClient, toDate } from "@/lib/api";
import { timeAgo } from "@/lib/utils";
import { t } from "@/i18n";
import { envToText, textToEnv } from "@/lib/config";
import { useRun } from "@/components/run";
import { Alert, Badge, Button, Check, EmptyRow, Field, FormActions, FormBody, FormRow, Input, Section, Spinner, Table, Td, Textarea, Th } from "@/components/ui";
import type { Project } from "@/gen/nagi/v1/common_pb";

// 「设置」标签页：基本设置、共享变量、部署记录、删除项目。

export function SettingsTab({ project }: { project: Project }) {
  return (
    <div className="grid gap-6">
      <GeneralSettings project={project} />
      <SharedEnv project={project} />
      <Deployments project={project} />
      <DeleteProject project={project} />
    </div>
  );
}

// GeneralSettings 修改名称、网络名和快照间隔；内部端口照原样提交（接口每次提交完整的设置）。
function GeneralSettings({ project }: { project: Project }) {
  const queryClient = useQueryClient();
  const s = project.settings;
  const [name, setName] = useState(project.name);
  const [network, setNetwork] = useState(project.networkName);
  const [interval, setInterval] = useState(s?.snapshotIntervalMinutes ? String(s.snapshotIntervalMinutes) : "");
  const [error, setError] = useState("");
  const [saved, setSaved] = useState(false);
  const [busy, setBusy] = useState(false);

  const save = async () => {
    setBusy(true);
    setError("");
    setSaved(false);
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
      setSaved(true);
      queryClient.invalidateQueries({ queryKey: ["project", project.id] });
      queryClient.invalidateQueries({ queryKey: ["projects"] });
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Section title={t.projectSettings.general.title}>
      <form
        className="grid max-w-2xl gap-4"
        onSubmit={(e) => {
          e.preventDefault();
          save();
        }}
      >
        <FormRow>
          <Field label={t.projectSettings.general.name} htmlFor="p-name">
            <Input id="p-name" value={name} onChange={(e) => setName(e.target.value)} required />
          </Field>
          <Field
            label={t.projectSettings.general.networkName}
            htmlFor="p-network"
            hint={t.projectSettings.general.networkNameHint}
            help={t.projectSettings.general.networkNameHelp}
          >
            <Input id="p-network" value={network} onChange={(e) => setNetwork(e.target.value)} required className="font-mono" />
          </Field>
          <Field label={t.projectSettings.general.snapshotInterval} htmlFor="p-interval" hint={t.projectSettings.general.snapshotIntervalHint}>
            <Input id="p-interval" value={interval} onChange={(e) => setInterval(e.target.value)} inputMode="numeric" className="font-mono" />
          </Field>
        </FormRow>
        {error && <Alert>{error}</Alert>}
        <FormActions>
          {saved && <span className="text-[13px] text-muted-foreground">{t.common.feedback.saved}</span>}
          <Button type="submit" size="sm" disabled={busy}>
            {busy && <Spinner />}
            {t.common.actions.save}
          </Button>
        </FormActions>
      </form>
    </Section>
  );
}

// ---- 共享变量 ----

function SharedEnv({ project }: { project: Project }) {
  const queryClient = useQueryClient();
  const [sharedEnv, setSharedEnv] = useState(envToText(project.hasSharedEnvDraft ? project.sharedEnvDraft : project.settings?.sharedEnv));
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
      setAffected(Object.fromEntries(res.affected.map((m) => [m, true])));
    });

  const deploy = () =>
    run(() =>
      projectClient.deploy({ projectId: project.id,
        applySharedEnv: true,
        settingsRevision: project.revision,
        targets: project.containers.filter((m) => affected[m.name]).map((m) => ({ container: m.name, baseRevision: m.revision })),
      }),
    );

  return (
    <Section
      title={
        <>
          {t.projectSettings.sharedEnv.title}
          {project.hasSharedEnvDraft && <Badge tone="warning">{t.projectSettings.sharedEnv.draft}</Badge>}
        </>
      }
      description={t.projectSettings.sharedEnv.description}
      help={t.projectSettings.sharedEnv.help}
    >
      <FormBody>
        <Field label={t.projectSettings.sharedEnv.vars} htmlFor="shared-env" hint={t.projectSettings.sharedEnv.varsHint}>
          <Textarea id="shared-env" value={sharedEnv} onChange={(e) => setSharedEnv(e.target.value)} spellCheck={false} className="font-mono text-[13px]" />
        </Field>
        {project.hasSharedEnvDraft && project.containers.length > 0 && (
          <Field label={t.projectSettings.sharedEnv.scope} help={t.projectSettings.sharedEnv.scopeHelp}>
            <div className="flex flex-wrap gap-x-5 gap-y-2">
              {project.containers.map((m) => (
                <Check key={m.name} checked={!!affected[m.name]} onChange={(v) => setAffected({ ...affected, [m.name]: v })}>
                  <span className="font-mono text-[13px]">{m.name}</span>
                </Check>
              ))}
            </div>
          </Field>
        )}
        {error && <Alert>{error}</Alert>}
        {output}
        <FormActions>
          {project.hasSharedEnvDraft && (
            <Button
              size="sm"
              variant="ghost"
              onClick={() => call(() => projectClient.discardSharedEnv({ projectId: project.id, baseRevision: project.revision }))}
              disabled={busy}
            >
              {t.projectSettings.sharedEnv.discardDraft}
            </Button>
          )}
          <Button size="sm" variant={project.hasSharedEnvDraft ? "outline" : "default"} onClick={save} disabled={busy}>
            {busy && <Spinner />}
            {t.projectSettings.sharedEnv.saveDraft}
          </Button>
          {project.hasSharedEnvDraft && (
            <Button size="sm" onClick={deploy} disabled={running}>
              {running ? <Spinner /> : <Rocket />}
              {t.projectSettings.sharedEnv.deploy}
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
  const { run, busy, output } = useRun(project.id);
  const list = useQuery({
    queryKey: ["deployments", project.id],
    queryFn: () => projectClient.listDeployments({ projectId: project.id }),
  });

  return (
    <Section flush title={t.projectSettings.deployments.title} description={t.projectSettings.deployments.description}>
      {output && <div className="pb-3 empty:hidden">{output}</div>}
      {list.isPending ? (
        <EmptyRow>
          <Spinner />
        </EmptyRow>
      ) : list.data?.deployments.length === 0 ? (
        <EmptyRow>{t.projectSettings.deployments.empty}</EmptyRow>
      ) : (
        <Table compact>
          <thead>
            <tr>
              <Th>{t.projectSettings.deployments.columns.id}</Th>
              <Th>{t.projectSettings.deployments.columns.time}</Th>
              <Th>{t.projectSettings.deployments.columns.source}</Th>
              <Th>{t.projectSettings.deployments.columns.versions}</Th>
              <Th />
            </tr>
          </thead>
          <tbody>
            {list.data?.deployments.map((d, i) => (
              <tr key={String(d.id)}>
                <Td className="font-mono text-xs">#{String(d.id)}</Td>
                <Td className="text-[13px] whitespace-nowrap text-muted-foreground">{timeAgo(toDate(d.time))}</Td>
                <Td className="text-[13px]">
                  {sourceLabel[d.source] ?? d.source}
                  {d.restoreOf > 0n && ` #${d.restoreOf}`}
                  <span className="text-muted-foreground"> · {t.projectSettings.deployments.containerList(d.containers)}</span>
                  {!d.success && (
                    <div className="mt-0.5 text-xs text-destructive">
                      <Badge tone="danger" className="mr-1.5">
                        {t.projectSettings.deployments.failed}
                      </Badge>
                      {d.error}
                    </div>
                  )}
                </Td>
                <Td className="font-mono text-xs text-muted-foreground">
                  {Object.entries(d.versions)
                    .map(([m, v]) => `${m} v${v}`)
                    .join(" · ")}
                </Td>
                <Td className="text-right">
                  {i > 0 && (
                    <Button
                      size="xs"
                      variant="ghost"
                      disabled={busy}
                      onClick={() =>
                        confirm(t.projectSettings.deployments.restoreConfirm(d.id)) &&
                        run(() => projectClient.restoreDeployment({ projectId: project.id, deploymentId: d.id }))
                      }
                    >
                      {t.projectSettings.deployments.restore}
                    </Button>
                  )}
                </Td>
              </tr>
            ))}
          </tbody>
        </Table>
      )}
    </Section>
  );
}

// ---- 删除项目 ----

function DeleteProject({ project }: { project: Project }) {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [deleteDir, setDeleteDir] = useState(false);
  const [confirmName, setConfirmName] = useState("");
  const [error, setError] = useState("");

  const remove = async () => {
    if (!deleteDir && !confirm(t.projectSettings.delete.confirm(project.name))) return;
    setError("");
    try {
      await projectClient.deleteProject({ projectId: project.id, baseRevision: project.revision, deleteDir, confirmName });
      queryClient.invalidateQueries({ queryKey: ["projects"] });
      navigate({ to: "/projects" });
    } catch (err) {
      setError(errorMessage(err));
    }
  };

  return (
    <Section tone="danger" title={t.projectSettings.delete.title} description={t.projectSettings.delete.description} help={t.projectSettings.delete.help}>
      <div className="grid gap-4">
        <Check checked={deleteDir} onChange={setDeleteDir}>
          {t.projectSettings.delete.deleteDir}
        </Check>
        {deleteDir && (
          <Field label={t.projectSettings.delete.confirmName(project.name)} htmlFor="confirm-name">
            <Input id="confirm-name" value={confirmName} onChange={(e) => setConfirmName(e.target.value)} className="max-w-80" />
          </Field>
        )}
        {error && <Alert>{error}</Alert>}
        <div className="flex justify-end pt-2">
          <Button variant="destructive" size="sm" onClick={remove} disabled={deleteDir && confirmName !== project.name}>
            {t.projectSettings.delete.submit}
          </Button>
        </div>
      </div>
    </Section>
  );
}
