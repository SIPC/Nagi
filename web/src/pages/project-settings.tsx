import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { Rocket } from "lucide-react";
import { errorMessage, projectClient, toDate } from "@/lib/api";
import { timeAgo } from "@/lib/utils";
import { envToText, textToEnv } from "@/lib/config";
import { useRun } from "@/components/run";
import { Alert, Badge, Button, Check, EmptyRow, Field, Input, Section, Spinner, Table, Td, Textarea, Th } from "@/components/ui";
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
    <Section title="基本设置">
      <form
        className="grid gap-4"
        onSubmit={(e) => {
          e.preventDefault();
          save();
        }}
      >
        <div className="grid gap-4 sm:grid-cols-2">
          <Field label="项目名称" htmlFor="p-name">
            <Input id="p-name" value={name} onChange={(e) => setName(e.target.value)} required />
          </Field>
          <Field
            label="网络名"
            htmlFor="p-network"
            hint="小写字母、数字和 -，在面板上唯一"
            help="通过项目连接接入的其他项目以「容器.网络名」访问本项目的容器，修改后须同步更新这些项目中引用的地址。已有容器的目录不变。"
          >
            <Input id="p-network" value={network} onChange={(e) => setNetwork(e.target.value)} required className="font-mono" />
          </Field>
          <Field label="定时快照间隔（分钟）" htmlFor="p-interval" hint="留空则为 60 分钟">
            <Input id="p-interval" value={interval} onChange={(e) => setInterval(e.target.value)} inputMode="numeric" className="font-mono" />
          </Field>
        </div>
        {error && <Alert>{error}</Alert>}
        <div className="flex items-center justify-end gap-3 pt-2">
          {saved && <span className="mr-auto text-[13px] text-muted-foreground">已保存</span>}
          <Button type="submit" size="sm" disabled={busy}>
            {busy && <Spinner />}
            保存
          </Button>
        </div>
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
          共享变量
          {project.hasSharedEnvDraft && <Badge tone="warning">草稿</Badge>}
        </>
      }
      description="所有容器的 compose 中可通过 ${变量} 引用。"
      help="与容器自身变量同名时，以容器变量为准。修改保存为草稿，部署后生效。"
    >
      <div className="grid gap-4">
        <Field label="变量" htmlFor="shared-env" hint="KEY=VALUE，每行一个">
          <Textarea id="shared-env" value={sharedEnv} onChange={(e) => setSharedEnv(e.target.value)} spellCheck={false} className="font-mono text-[13px]" />
        </Field>
        {project.hasSharedEnvDraft && project.containers.length > 0 && (
          <Field label="部署范围" help="默认选中引用了已修改变量的容器。">
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
        <div className="flex justify-end gap-2 pt-2">
          {project.hasSharedEnvDraft && (
            <Button
              size="sm"
              variant="ghost"
              onClick={() => call(() => projectClient.discardSharedEnv({ projectId: project.id, baseRevision: project.revision }))}
              disabled={busy}
            >
              放弃草稿
            </Button>
          )}
          <Button size="sm" variant={project.hasSharedEnvDraft ? "outline" : "default"} onClick={save} disabled={busy}>
            {busy && <Spinner />}
            保存草稿
          </Button>
          {project.hasSharedEnvDraft && (
            <Button size="sm" onClick={deploy} disabled={running}>
              {running ? <Spinner /> : <Rocket />}
              部署
            </Button>
          )}
        </div>
      </div>
    </Section>
  );
}

// ---- 部署记录 ----

const sourceLabel: Record<string, string> = {
  manual: "手动部署",
  git: "git 拉取",
  image: "镜像更新",
  rollback: "容器回滚",
  shared_env: "共享变量",
  restore: "恢复部署",
};

function Deployments({ project }: { project: Project }) {
  const { run, busy, output } = useRun(project.id);
  const list = useQuery({
    queryKey: ["deployments", project.id],
    queryFn: () => projectClient.listDeployments({ projectId: project.id }),
  });

  return (
    <Section flush title="部署记录" description="每次部署记录所有容器的版本，可将项目恢复到任一次部署时的状态。">
      {output && <div className="pb-3 empty:hidden">{output}</div>}
      {list.isPending ? (
        <EmptyRow>
          <Spinner />
        </EmptyRow>
      ) : list.data?.deployments.length === 0 ? (
        <EmptyRow>暂无部署记录</EmptyRow>
      ) : (
        <Table>
          <thead>
            <tr>
              <Th>编号</Th>
              <Th>时间</Th>
              <Th>来源</Th>
              <Th>版本</Th>
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
                  <span className="text-muted-foreground"> · {d.containers.join("、")}</span>
                  {!d.success && (
                    <div className="mt-0.5 text-xs text-destructive">
                      <Badge tone="danger" className="mr-1.5">
                        失败
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
                        confirm(`将项目恢复到部署 #${d.id} 时的状态？版本不同的容器将被回滚。`) &&
                        run(() => projectClient.restoreDeployment({ projectId: project.id, deploymentId: d.id }))
                      }
                    >
                      恢复
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
    if (!deleteDir && !confirm(`删除项目 ${project.name}？项目内的所有容器和容器组将被删除，项目目录保留。`)) return;
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
    <Section tone="danger" title="删除项目" description="停止并删除项目内的所有容器和容器组。" help="各容器的目录（包括数据和快照）默认保留在所在节点上。容器所在的节点须在线。">
      <div className="grid gap-4">
        <Check checked={deleteDir} onChange={setDeleteDir}>
          同时删除容器目录（数据和快照将无法恢复）
        </Check>
        {deleteDir && (
          <Field label={`输入项目名称 ${project.name} 以确认`} htmlFor="confirm-name">
            <Input id="confirm-name" value={confirmName} onChange={(e) => setConfirmName(e.target.value)} className="max-w-80" />
          </Field>
        )}
        {error && <Alert>{error}</Alert>}
        <div className="flex justify-end pt-2">
          <Button variant="destructive" size="sm" onClick={remove} disabled={deleteDir && confirmName !== project.name}>
            删除项目
          </Button>
        </div>
      </div>
    </Section>
  );
}
