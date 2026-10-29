import { useEffect, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { errorMessage, nodeClient, projectClient, toDate } from "@/lib/api";
import { formatDateTime, timeAgo } from "@/lib/utils";
import { t } from "@/i18n";
import { useRun } from "@/components/run";
import { fmtBytes } from "@/components/metrics";
import {
  Alert,
  Badge,
  Button,
  Check,
  EmptyRow,
  Field,
  FormActions,
  FormBody,
  FormRow,
  Input,
  Notice,
  Section,
  Select,
  Spinner,
  Status,
  Table,
  Td,
  Th,
} from "@/components/ui";
import type { Container, Project } from "@/gen/nagi/v1/common_pb";
import { useDefaultDir } from "@/pages/projects";

// 容器的数据：快照、顶替、备份节点、更换节点（迁移、从备份节点恢复、重新部署到其他节点）。
// 见 docs/snapshot.md、docs/failover.md。

const snapshotKinds: Record<string, string> = {
  scheduled: t.projectData.snapshotKinds.scheduled,
  manual: t.projectData.snapshotKinds.manual,
  predeploy: t.projectData.snapshotKinds.predeploy,
  version: t.projectData.snapshotKinds.version,
};

function useNodes() {
  const nodes = useQuery({
    queryKey: ["nodes"],
    queryFn: () => nodeClient.listNodes({}),
    refetchInterval: 10000,
  });
  const list = nodes.data?.nodes ?? [];
  return {
    list,
    // fallback：顶替设置里记的名字。节点改过名时设置里的是旧名字，所以先按 ID 查当前的名字。
    name: (id: string, fallback?: string) => list.find((n) => n.id === id)?.name ?? fallback ?? id,
    online: (id: string) => list.find((n) => n.id === id)?.online === true,
    usable: list.filter((n) => n.online && !n.pending),
  };
}

// ContainerData：容器页的「数据」标签：快照、顶替、备份节点、更换节点。
export function ContainerData({
  project,
  container,
}: {
  project: Project;
  container: Container;
}) {
  const run = useRun(project.id);
  return (
    <div className="grid gap-6">
      {run.output && <div className="empty:hidden">{run.output}</div>}
      <Snapshots project={project} container={container} run={run} />
      <Failover project={project} container={container} />
      <Backup project={project} container={container} run={run} />
      <Placement project={project} container={container} run={run} />
    </div>
  );
}

type Run = ReturnType<typeof useRun>;

// ---- 快照 ----

function Snapshots({
  project,
  container,
  run,
}: {
  project: Project;
  container: Container;
  run: Run;
}) {
  const nodes = useNodes();
  const deployed = container.deployedVersion > 0n;
  const online = nodes.online(container.nodeId);
  return (
    <Section
      flush
      title={t.projectData.snapshots.title}
      description={t.projectData.snapshots.description}
      help={t.projectData.snapshots.help}
      actions={
        deployed &&
        online && (
          <>
            <Button
              size="sm"
              variant="outline"
              disabled={run.busy}
              onClick={() =>
                run.run(() =>
                  projectClient.createSnapshot({
                    projectId: project.id,
                    container: container.name,
                  }),
                )
              }
            >
              {run.busy && <Spinner />}
              {t.projectData.snapshots.create}
            </Button>
          </>
        )
      }
    >
      {!deployed ? (
        <EmptyRow>{t.projectData.snapshots.notDeployed}</EmptyRow>
      ) : !online ? (
        <EmptyRow>{t.projectData.snapshots.nodeOffline}</EmptyRow>
      ) : (
        <SnapshotList
          project={project}
          container={container}
          busy={run.busy}
          run={run.run}
        />
      )}
    </Section>
  );
}

function SnapshotTable({
  snapshots,
  action,
}: {
  snapshots: {
    id: string;
    kind: string;
    time?: Parameters<typeof toDate>[0];
    version: bigint;
    size: bigint;
  }[];
  action: (id: string, kind: string) => React.ReactNode;
}) {
  // 默认只列最近几份，其余收起。
  const [all, setAll] = useState(false);
  if (snapshots.length === 0) return <EmptyRow>{t.projectData.snapshots.empty}</EmptyRow>;
  const shown = all ? snapshots : snapshots.slice(0, 6);
  return (
    <>
      <Table compact>
        <thead>
          <tr>
            <Th>{t.projectData.snapshots.kind}</Th>
            <Th>{t.projectData.snapshots.time}</Th>
            <Th>{t.projectData.snapshots.version}</Th>
            <Th>{t.projectData.snapshots.size}</Th>
            <Th />
          </tr>
        </thead>
        <tbody>
          {shown.map((sn) => (
            <tr key={sn.id}>
              <Td className="text-[13px]">
                <Badge
                  tone={
                    sn.kind === "scheduled" || sn.kind === "manual"
                      ? "neutral"
                      : "info"
                  }
                >
                  {snapshotKinds[sn.kind] ?? sn.kind}
                </Badge>
              </Td>
              <Td
                className="text-[13px] text-muted-foreground"
                title={formatDateTime(toDate(sn.time))}
              >
                {timeAgo(toDate(sn.time))}
              </Td>
              <Td className="font-mono text-xs text-muted-foreground">
                {sn.version > 0n ? `v${sn.version}` : "—"}
              </Td>
              <Td className="font-mono text-xs text-muted-foreground">
                {fmtBytes(Number(sn.size))}
              </Td>
              <Td className="text-right">{action(sn.id, sn.kind)}</Td>
            </tr>
          ))}
        </tbody>
      </Table>
      {snapshots.length > shown.length && (
        <button
          type="button"
          className="mt-3 cursor-pointer text-[13px] text-muted-foreground hover:text-foreground"
          onClick={() => setAll(true)}
        >
          {t.projectData.snapshots.showAll(snapshots.length)}
        </button>
      )}
    </>
  );
}

function SnapshotList({
  project,
  container: c,
  busy,
  run,
}: {
  project: Project;
  container: Container;
  busy: boolean;
  run: Run["run"];
}) {
  const container = c.name;
  // 容器换了节点（目录）以后是另一个快照仓库。
  const snapshots = useQuery({
    queryKey: ["snapshots", project.id, container, c.nodeId, c.dir],
    queryFn: () =>
      projectClient.listSnapshots({ projectId: project.id, container }),
  });
  if (snapshots.isPending) {
    return (
      <EmptyRow>
        <Spinner />
      </EmptyRow>
    );
  }
  if (snapshots.error)
    return <Alert className="mb-4">{errorMessage(snapshots.error)}</Alert>;
  return (
    <SnapshotTable
      snapshots={snapshots.data.snapshots}
      action={(id, kind) =>
        (kind === "scheduled" || kind === "manual") && (
          <Button
            size="xs"
            variant="ghost"
            disabled={busy}
            onClick={() =>
              confirm(t.projectData.snapshots.restoreConfirm(container)) &&
              run(() =>
                projectClient.restoreSnapshot({
                  projectId: project.id,
                  container,
                  snapshotId: id,
                }),
              )
            }
          >
            {t.projectData.snapshots.restore}
          </Button>
        )
      }
    />
  );
}

// ---- 备份节点 ----

function Backup({
  project,
  container,
  run,
}: {
  project: Project;
  container: Container;
  run: Run;
}) {
  const queryClient = useQueryClient();
  const nodes = useNodes();
  const [nodeId, setNodeId] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [restoring, setRestoring] = useState(false);
  const candidates = nodes.usable.filter((n) => n.id !== container.nodeId);
  const choice = nodeId || candidates[0]?.id || "";
  const backup = container.backup;
  const sourceOnline = nodes.online(container.nodeId);

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
  const setBackup = (id: string, deleteReplica = false) =>
    call(() =>
      projectClient.setBackupNode({
        projectId: project.id,
        container: container.name,
        nodeId: id,
        deleteReplica,
      }),
    );

  let state: React.ReactNode = <span className="text-muted-foreground">—</span>;
  if (backup?.syncing)
    state = (
      <Status tone="info" pulse>
        {t.common.status.syncing}
      </Status>
    );
  else if (backup?.lastError) state = <Status tone="danger">{t.projectData.backup.syncFailed}</Status>;
  else if (backup?.lastSync) state = <Status tone="success">{t.projectData.backup.healthy}</Status>;
  else if (container.backupNodeId)
    state = <Status tone="neutral">{t.projectData.backup.waitingFirstSync}</Status>;

  return (
    <Section
      title={t.projectData.backup.title}
      description={t.projectData.backup.description}
      help={t.projectData.backup.help}
    >
      <FormBody>
        {container.backupNodeId ? (
          <>
            <dl className="grid grid-cols-[7rem_1fr] gap-x-4 gap-y-2 text-[13px]">
              <dt className="text-muted-foreground">{t.projectData.backup.node}</dt>
              <dd>
                {nodes.name(container.backupNodeId)}
                {!nodes.online(container.backupNodeId) && (
                  <Badge className="ml-2">{t.common.status.offline}</Badge>
                )}
              </dd>
              <dt className="text-muted-foreground">{t.projectData.state}</dt>
              <dd>
                {sourceOnline ? (
                  state
                ) : (
                  <span className="text-muted-foreground">
                    {t.projectData.backup.sourceOffline}
                  </span>
                )}
              </dd>
              <dt className="text-muted-foreground">{t.projectData.lastSync}</dt>
              <dd title={formatDateTime(toDate(backup?.lastSync))}>
                {backup?.lastSync ? timeAgo(toDate(backup.lastSync)) : "—"}
              </dd>
            </dl>
            {backup?.lastError && <Alert>{backup.lastError}</Alert>}
            {error && <Alert>{error}</Alert>}
            <FormActions>
              {!sourceOnline && container.deployedVersion > 0n && (
                <Button
                  size="sm"
                  variant="outline"
                  onClick={() => setRestoring(!restoring)}
                  disabled={run.busy}
                >
                  {t.projectData.backup.restoreFromBackup}
                </Button>
              )}
              <Button
                size="sm"
                variant="ghost"
                disabled={busy}
                onClick={() =>
                  confirm(t.projectData.backup.removeConfirm(container.name)) &&
                  setBackup("", true)
                }
              >
                {t.projectData.backup.remove}
              </Button>
              <Button
                size="sm"
                variant="outline"
                disabled={busy || !sourceOnline || backup?.syncing}
                onClick={() =>
                  call(() =>
                    projectClient.syncBackup({
                      projectId: project.id,
                      container: container.name,
                    }),
                  )
                }
              >
                {busy && <Spinner />}
                {t.projectData.backup.syncNow}
              </Button>
            </FormActions>
            {restoring && (
              <RestoreFromBackup
                project={project}
                container={container}
                run={run}
              />
            )}
          </>
        ) : container.failover ? (
          <p className="text-[13px] text-muted-foreground">
            {t.projectData.backup.hasFailover}
          </p>
        ) : (
          <>
            {candidates.length === 0 ? (
              <p className="text-[13px] text-muted-foreground">
                {t.projectData.backup.noCandidates}
              </p>
            ) : (
              <div className="flex flex-wrap items-end gap-3">
                <Field label={t.projectData.backup.nodeLabel} htmlFor="backup-node">
                  <Select
                    id="backup-node"
                    value={choice}
                    onChange={(e) => setNodeId(e.target.value)}
                    className="min-w-48"
                  >
                    {candidates.map((n) => (
                      <option key={n.id} value={n.id}>
                        {n.name}
                      </option>
                    ))}
                  </Select>
                </Field>
                <Button
                  size="sm"
                  disabled={busy || !choice}
                  onClick={() => setBackup(choice)}
                >
                  {busy && <Spinner />}
                  {t.projectData.backup.set}
                </Button>
              </div>
            )}
            {error && <Alert>{error}</Alert>}
          </>
        )}
      </FormBody>
    </Section>
  );
}

function RestoreFromBackup({
  project,
  container,
  run,
}: {
  project: Project;
  container: Container;
  run: Run;
}) {
  const nodes = useNodes();
  const [dir, setDir] = useState("");
  const [defDir, setDefDir] = useState("");
  const snapshots = useQuery({
    queryKey: ["backup-snapshots", project.id, container.name],
    queryFn: () =>
      projectClient.listBackupSnapshots({
        projectId: project.id,
        container: container.name,
      }),
  });
  const restore = (snapshotId: string) =>
    confirm(
      t.projectData.restore.confirm(
        nodes.name(container.backupNodeId),
        container.name,
      ),
    ) &&
    run.run(() =>
      projectClient.restoreFromBackup({
        projectId: project.id,
        container: container.name,
        snapshotId,
        dir: dir.trim() || defDir,
      }),
    );
  return (
    <div className="grid gap-3 rounded-md bg-subtle">
      <div className="flex flex-wrap items-end gap-3 px-4 pt-4">
        <Field
          label={t.projectData.restore.dir}
          htmlFor="restore-dir"
          help={t.projectData.dirHelp}
        >
          <DirInput
            id="restore-dir"
            nodeId={container.backupNodeId}
            project={project}
            container={container.name}
            value={dir}
            onChange={setDir}
            onDefault={setDefDir}
          />
        </Field>
        <Button size="sm" disabled={run.busy} onClick={() => restore("")}>
          {t.projectData.restore.latest}
        </Button>
      </div>
      {snapshots.isPending ? (
        <EmptyRow>
          <Spinner />
        </EmptyRow>
      ) : snapshots.error ? (
        <Alert className="mx-4 mb-4">{errorMessage(snapshots.error)}</Alert>
      ) : (
        <SnapshotTable
          snapshots={snapshots.data.snapshots}
          action={(id, kind) =>
            (kind === "scheduled" || kind === "manual") && (
              <Button
                size="xs"
                variant="ghost"
                disabled={run.busy}
                onClick={() => restore(id)}
              >
                {t.projectData.restore.fromSnapshot}
              </Button>
            )
          }
        />
      )}
    </div>
  );
}

// ---- 更换节点 ----

function Placement({
  project,
  container,
  run,
}: {
  project: Project;
  container: Container;
  run: Run;
}) {
  const nodes = useNodes();
  const [nodeId, setNodeId] = useState("");
  const [dir, setDir] = useState("");
  const [defDir, setDefDir] = useState("");
  const candidates = nodes.usable.filter((n) => n.id !== container.nodeId);
  const choice =
    nodeId ||
    (container.backupNodeId &&
    candidates.some((n) => n.id === container.backupNodeId)
      ? container.backupNodeId
      : candidates[0]?.id) ||
    "";
  const online = nodes.online(container.nodeId);
  const target = nodes.name(choice);

  const migrate = () =>
    confirm(t.projectData.placement.migrateConfirm(container.name, target)) &&
    run.run(() =>
      projectClient.migrateContainer({
        projectId: project.id,
        container: container.name,
        nodeId: choice,
        dir: dir.trim() || defDir,
      }),
    );
  const redeploy = () =>
    confirm(t.projectData.placement.redeployConfirm(container.name, target)) &&
    run.run(() =>
      projectClient.redeployElsewhere({
        projectId: project.id,
        container: container.name,
        nodeId: choice,
        dir: dir.trim() || defDir,
      }),
    );

  return (
    <Section
      title={t.projectData.placement.title}
      description={
        online
          ? t.projectData.placement.description
          : t.projectData.placement.descriptionOffline
      }
      help={t.projectData.placement.help}
    >
      <div className="grid gap-4">
        <dl className="grid grid-cols-[7rem_1fr] gap-x-4 gap-y-2 text-[13px]">
          <dt className="text-muted-foreground">{t.projectData.placement.currentNode}</dt>
          <dd>
            {nodes.name(container.nodeId)}
            {!online && <Badge className="ml-2">{t.common.status.offline}</Badge>}
          </dd>
          <dt className="text-muted-foreground">{t.projectData.placement.dir}</dt>
          <dd className="font-mono text-xs">{container.dir}</dd>
        </dl>
        {!online && container.backupNodeId && (
          <Notice tone="warning">
            {t.projectData.placement.backupHint}
          </Notice>
        )}
        {container.failover ? (
          <p className="border-t pt-4 text-[13px] text-muted-foreground">
            {t.projectData.placement.hasFailover}
          </p>
        ) : candidates.length === 0 ? (
          <p className="text-[13px] text-muted-foreground">
            {t.projectData.placement.noCandidates}
          </p>
        ) : (
          <div className="flex flex-wrap items-end gap-3 border-t pt-4">
            <Field label={t.projectData.placement.target} htmlFor="move-node">
              <Select
                id="move-node"
                value={choice}
                onChange={(e) => setNodeId(e.target.value)}
                className="min-w-48"
              >
                {candidates.map((n) => (
                  <option key={n.id} value={n.id}>
                    {n.name}
                    {n.id === container.backupNodeId
                                          ? t.projectData.placement.backupSuffix
                                          : ""}
                  </option>
                ))}
              </Select>
            </Field>
            <Field
              label={t.projectData.placement.dir}
              htmlFor="move-dir"
              help={t.projectData.dirHelp}
            >
              <DirInput
                id="move-dir"
                nodeId={choice}
                project={project}
                container={container.name}
                value={dir}
                onChange={setDir}
                onDefault={setDefDir}
              />
            </Field>
            {online ? (
              <Button
                size="sm"
                disabled={run.busy || !choice}
                onClick={migrate}
              >
                {run.busy && <Spinner />}
                {t.projectData.placement.migrate}
              </Button>
            ) : (
              <Button
                size="sm"
                variant="destructive"
                disabled={run.busy || !choice}
                onClick={redeploy}
              >
                {run.busy && <Spinner />}
                {t.projectData.placement.redeploy}
              </Button>
            )}
          </div>
        )}
      </div>
    </Section>
  );
}

// ---- 顶替 ----

function duration(ms: bigint) {
  const s = Math.ceil(Number(ms) / 1000);
  return s >= 60
    ? t.common.time.minutes(Math.ceil(s / 60))
    : t.common.time.seconds(s);
}

function Failover({
  project,
  container,
}: {
  project: Project;
  container: Container;
}) {
  const queryClient = useQueryClient();
  const nodes = useNodes();
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [form, setForm] = useState({
    standby: "",
    dir: "",
    witness: "",
    lease: "",
    autoStateful: false,
    autoFailback: true,
  });
  const [defDir, setDefDir] = useState("");
  const [witness, setWitness] = useState("-");
  const cfg = container.failover;
  const holder = container.failoverHolder;
  const standby = container.failoverStandby;
  const ref = { projectId: project.id, container: container.name };

  const call = async (fn: () => Promise<unknown>) => {
    setBusy(true);
    setError("");
    try {
      await fn();
      queryClient.invalidateQueries({ queryKey: ["project", project.id] });
      return true;
    } catch (err) {
      setError(errorMessage(err));
      return false;
    } finally {
      setBusy(false);
    }
  };

  if (!cfg) {
    const standbys = nodes.usable.filter((n) => n.id !== container.nodeId);
    const standbyId = form.standby || standbys[0]?.id || "";
    const witnesses = nodes.usable.filter(
      (n) => n.id !== container.nodeId && n.id !== standbyId,
    );
    return (
      <Section
        title={t.projectData.failover.title}
        description={t.projectData.failover.setupDescription}
        help={t.projectData.failover.setupHelp}
      >
        {container.backupNodeId ? (
          <p className="text-[13px] text-muted-foreground">
            {t.projectData.failover.hasBackup}
          </p>
        ) : standbys.length === 0 ? (
          <p className="text-[13px] text-muted-foreground">
            {t.projectData.failover.noCandidates}
          </p>
        ) : (
          <FormBody>
            <FormRow>
              <Field label={t.projectData.failover.standbyNode} htmlFor="fo-standby">
                <Select
                  id="fo-standby"
                  value={standbyId}
                  onChange={(e) =>
                    setForm({ ...form, standby: e.target.value })
                  }
                >
                  {standbys.map((n) => (
                    <option key={n.id} value={n.id}>
                      {n.name}
                    </option>
                  ))}
                </Select>
              </Field>
              <Field
                label={t.projectData.failover.standbyDir}
                htmlFor="fo-dir"
                help={t.projectData.dirHelp}
              >
                <DirInput
                  id="fo-dir"
                  nodeId={standbyId}
                  project={project}
                  container={container.name}
                  value={form.dir}
                  onChange={(v) => setForm({ ...form, dir: v })}
                  onDefault={setDefDir}
                />
              </Field>
            </FormRow>
            <FormRow>
              <Field
                label={t.projectData.failover.witness}
                htmlFor="fo-witness"
                help={t.projectData.failover.witnessHelp}
              >
                <Select
                  id="fo-witness"
                  value={form.witness}
                  onChange={(e) =>
                    setForm({ ...form, witness: e.target.value })
                  }
                >
                  <option value="">{t.projectData.thisPanel}</option>
                  {witnesses.map((n) => (
                    <option key={n.id} value={n.id}>
                      {n.name}
                    </option>
                  ))}
                </Select>
              </Field>
              <Field
                label={t.projectData.failover.leaseSeconds}
                htmlFor="fo-lease"
                help={t.projectData.failover.leaseHelp}
              >
                <Input
                  id="fo-lease"
                  value={form.lease}
                  onChange={(e) => setForm({ ...form, lease: e.target.value })}
                  placeholder="30"
                  inputMode="numeric"
                />
              </Field>
            </FormRow>
            <div className="flex flex-wrap gap-x-6 gap-y-2">
              <Check
                checked={form.autoFailback}
                onChange={(v) => setForm({ ...form, autoFailback: v })}
                help={t.projectData.failover.autoFailbackHelp}
              >
                {t.projectData.failover.autoFailback}
              </Check>
              {container.stateful && (
                <Check
                  checked={form.autoStateful}
                  onChange={(v) => setForm({ ...form, autoStateful: v })}
                  help={t.projectData.failover.autoStatefulHelp}
                >
                  {t.projectData.failover.autoStateful}
                </Check>
              )}
            </div>
            {error && <Alert>{error}</Alert>}
            <FormActions>
              <Button
                size="sm"
                disabled={busy || !standbyId}
                onClick={() =>
                  call(() =>
                    projectClient.setFailover({
                      ...ref,
                      standbyNodeId: standbyId,
                      standbyDir: form.dir.trim() || defDir,
                      witnessNodeId: form.witness,
                      leaseSeconds: Number(form.lease || 0),
                      autoStateful: form.autoStateful,
                      autoFailback: form.autoFailback,
                    }),
                  )
                }
              >
                {busy && <Spinner />}
                {t.projectData.failover.set}
              </Button>
            </FormActions>
          </FormBody>
        )}
      </Section>
    );
  }

  const holderOnline = nodes.online(container.nodeId);
  // 持有者上报的是最近一次同步给热备的时间，持有者离线时用热备记录的最近一次收到同步的时间。
  const lastSync = holder?.lastSync ?? standby?.lastSync;
  const standbyNode = cfg.a?.nodeId === container.nodeId ? cfg.b : cfg.a;
  let state: React.ReactNode = <Status tone="neutral">—</Status>;
  if (holder?.failingBack)
    state = (
      <Status tone="info" pulse>
        {t.projectData.failover.failingBack}
      </Status>
    );
  else if (holder?.role === "holder" && holder.leaseRemainingMs > 0n)
    state = <Status tone="success">{t.common.status.running}</Status>;
  else if (holder?.role === "holder")
    state = <Status tone="danger">{t.projectData.failover.leaseLost}</Status>;
  else if (!holderOnline && standby?.pendingConfirm)
    state = (
      <Status tone="warning">{t.projectData.failover.awaitingConfirm}</Status>
    );
  else if (!holderOnline)
    state = <Status tone="warning">{t.projectData.failover.holderOffline}</Status>;
  const witnessOptions = nodes.usable.filter(
    (n) =>
      n.id !== cfg.a?.nodeId &&
      n.id !== cfg.b?.nodeId &&
      (cfg.witnessIsPanel || n.id !== cfg.witness?.nodeId),
  );

  return (
    <Section
      title={t.projectData.failover.title}
      description={t.projectData.failover.description}
      help={t.projectData.failover.help}
    >
      <div className="grid gap-4">
        <dl className="grid grid-cols-[7rem_1fr] gap-x-4 gap-y-2 text-[13px]">
          <dt className="text-muted-foreground">{t.projectData.state}</dt>
          <dd>{state}</dd>
          <dt className="text-muted-foreground">{t.projectData.failover.runningNode}</dt>
          <dd>
            {nodes.name(container.nodeId)}
            {cfg.a?.nodeId === container.nodeId ? (
              <Badge className="ml-2">{t.projectData.failover.originalNode}</Badge>
            ) : (
              <Badge tone="warning" className="ml-2">
                {t.projectData.failover.failedOver}
              </Badge>
            )}
          </dd>
          <dt className="text-muted-foreground">{t.projectData.failover.hotStandby}</dt>
          <dd>
            {standbyNode && nodes.name(standbyNode.nodeId, standbyNode.name)}
            {standbyNode && !nodes.online(standbyNode.nodeId) && (
              <Badge className="ml-2">{t.common.status.offline}</Badge>
            )}
          </dd>
          <dt className="text-muted-foreground">{t.projectData.failover.witness}</dt>
          <dd>
            {!cfg.witnessIsPanel ? (
              cfg.witness && nodes.name(cfg.witness.nodeId, cfg.witness.name)
            ) : container.failoverWitnessSelf ? (
              t.projectData.thisPanel
            ) : (
              <span
                className="text-warning"
                title={t.projectData.failover.oldPanelHint}
              >
                {t.projectData.failover.oldPanel}
              </span>
            )}
          </dd>
          <dt className="text-muted-foreground">{t.projectData.lastSync}</dt>
          <dd title={formatDateTime(toDate(lastSync))}>
            {lastSync
              ? timeAgo(toDate(lastSync))
              : t.projectData.failover.notSynced}
          </dd>
          <dt className="text-muted-foreground">{t.projectData.failover.lease}</dt>
          <dd>
            {t.common.time.seconds(cfg.leaseSeconds || 30)}
            {holder?.epoch ? t.projectData.failover.epoch(holder.epoch) : ""}
          </dd>
          <dt className="text-muted-foreground">
            {t.projectData.failover.autoFailbackLabel}
          </dt>
          <dd>
            {cfg.autoFailback
              ? t.projectData.failover.on
              : t.projectData.failover.off}
            {holder && holder.failbackInMs > 0n && (
              <span className="ml-2 text-muted-foreground">
                {t.projectData.failover.failbackIn(
                  duration(holder.failbackInMs),
                )}
              </span>
            )}
          </dd>
        </dl>
        {standby?.pendingConfirm && (
          <Notice
            tone="warning"
            title={t.projectData.failover.pendingTitle}
            actions={
              <Button
                size="sm"
                variant="destructive"
                disabled={busy}
                onClick={() =>
                  confirm(
                    t.projectData.failover.confirmTakeoverPrompt(
                      standbyNode ? nodes.name(standbyNode.nodeId, standbyNode.name) : "",
                      container.name,
                    ),
                  ) && call(() => projectClient.confirmTakeover(ref))
                }
              >
                {t.projectData.failover.confirmTakeover}
              </Button>
            }
          >
            {t.projectData.failover.pendingBody}
          </Notice>
        )}
        {(holder?.lastError || standby?.lastError) && (
          <Alert>{holder?.lastError || standby?.lastError}</Alert>
        )}
        {error && <Alert>{error}</Alert>}
        <div className="flex flex-wrap items-end gap-3 border-t pt-4">
          <Field
            label={t.projectData.failover.changeWitness}
            htmlFor="fo-change-witness"
          >
            <Select
              id="fo-change-witness"
              value={witness}
              onChange={(e) => setWitness(e.target.value)}
              className="min-w-40"
            >
              <option value="-" disabled>
                {t.projectData.failover.select}
              </option>
              {!(cfg.witnessIsPanel && container.failoverWitnessSelf) && (
                <option value="">{t.projectData.thisPanel}</option>
              )}
              {witnessOptions.map((n) => (
                <option key={n.id} value={n.id}>
                  {n.name}
                </option>
              ))}
            </Select>
          </Field>
          <Button
            size="sm"
            variant="outline"
            disabled={busy || witness === "-"}
            onClick={() =>
              call(() =>
                projectClient.changeWitness({ ...ref, witnessNodeId: witness }),
              ).then((ok) => ok && setWitness("-"))
            }
          >
            {t.projectData.failover.change}
          </Button>
          <Button
            size="sm"
            variant="ghost"
            className="ml-auto"
            disabled={busy}
            onClick={() =>
              confirm(
                t.projectData.failover.clearConfirm(
                  container.name,
                  nodes.name(container.nodeId),
                ),
              ) && call(() => projectClient.clearFailover(ref))
            }
          >
            {t.projectData.failover.clear}
          </Button>
        </div>
      </div>
    </Section>
  );
}

// DirInput：目标节点上的容器目录。占位文字是默认目录（项目根目录/项目名拼音/容器名），onDefault 告诉调用方，
// 不填时提交这个默认目录。
function DirInput({
  id,
  nodeId,
  project,
  container,
  value,
  onChange,
  onDefault,
}: {
  id: string;
  nodeId: string;
  project: Project;
  container: string;
  value: string;
  onChange: (v: string) => void;
  onDefault: (v: string) => void;
}) {
  const def = useDefaultDir(nodeId, project.id, project.name, container);
  useEffect(() => onDefault(def), [def, onDefault]);
  return (
    <Input
      id={id}
      value={value}
      onChange={(e) => onChange(e.target.value)}
      placeholder={def}
      className="w-full min-w-80 font-mono"
    />
  );
}
