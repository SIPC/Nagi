import { useEffect, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { errorMessage, nodeClient, projectClient, toDate } from "@/lib/api";
import { timeAgo } from "@/lib/utils";
import { useRun } from "@/components/run";
import { fmtBytes } from "@/components/metrics";
import {
  Alert,
  Badge,
  Button,
  Check,
  EmptyRow,
  Field,
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
  scheduled: "定时",
  manual: "手动",
  predeploy: "部署前数据",
  version: "版本文件",
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
    name: (id: string) => list.find((n) => n.id === id)?.name ?? id,
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
    <div className="grid gap-8">
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
      title="快照"
      description="容器的 data/ 和 volumes/ 单独保存，恢复时配置保持不变。"
      help="定时和手动快照可在此恢复。「部署前数据」和「版本文件」用于回滚，在容器的「版本」中操作。快照仓库位于容器所在节点的容器目录 backups/ 下。"
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
              创建快照
            </Button>
          </>
        )
      }
    >
      {!deployed ? (
        <EmptyRow>容器尚未部署</EmptyRow>
      ) : !online ? (
        <EmptyRow>容器所在的节点离线，无法读取快照</EmptyRow>
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
  if (snapshots.length === 0) return <EmptyRow>暂无快照</EmptyRow>;
  const shown = all ? snapshots : snapshots.slice(0, 6);
  return (
    <>
      <Table>
        <thead>
          <tr>
            <Th>类型</Th>
            <Th>时间</Th>
            <Th>版本</Th>
            <Th>大小</Th>
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
                title={toDate(sn.time)?.toLocaleString()}
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
          显示全部 {snapshots.length} 份
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
              confirm(
                `从此快照恢复容器 ${container} 的 data/ 和 volumes/？恢复前将停止容器，当前数据将被覆盖。`,
              ) &&
              run(() =>
                projectClient.restoreSnapshot({
                  projectId: project.id,
                  container,
                  snapshotId: id,
                }),
              )
            }
          >
            恢复
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
        同步中
      </Status>
    );
  else if (backup?.lastError) state = <Status tone="danger">同步失败</Status>;
  else if (backup?.lastSync) state = <Status tone="success">正常</Status>;
  else if (container.backupNodeId)
    state = <Status tone="neutral">等待首次同步</Status>;

  return (
    <Section
      title="备份节点"
      description="每次定时快照后，将容器的快照仓库增量同步到另一节点。"
      help="备份节点仅保存副本，不运行容器，也不自动顶替。容器所在节点损坏时，可在备份节点上恢复容器：数据来自副本，配置来自面板。"
    >
      <div className="grid gap-4">
        {container.backupNodeId ? (
          <>
            <dl className="grid grid-cols-[7rem_1fr] gap-x-4 gap-y-2 text-[13px]">
              <dt className="text-muted-foreground">备份节点</dt>
              <dd>
                {nodes.name(container.backupNodeId)}
                {!nodes.online(container.backupNodeId) && (
                  <Badge className="ml-2">离线</Badge>
                )}
              </dd>
              <dt className="text-muted-foreground">状态</dt>
              <dd>
                {sourceOnline ? (
                  state
                ) : (
                  <span className="text-muted-foreground">
                    容器所在的节点离线
                  </span>
                )}
              </dd>
              <dt className="text-muted-foreground">最近同步</dt>
              <dd title={toDate(backup?.lastSync)?.toLocaleString()}>
                {backup?.lastSync ? timeAgo(toDate(backup.lastSync)) : "—"}
              </dd>
            </dl>
            {backup?.lastError && <Alert>{backup.lastError}</Alert>}
            {error && <Alert>{error}</Alert>}
            <div className="flex flex-wrap items-center justify-end gap-2 pt-2">
              {!sourceOnline && container.deployedVersion > 0n && (
                <Button
                  size="sm"
                  variant="outline"
                  onClick={() => setRestoring(!restoring)}
                  disabled={run.busy}
                >
                  从备份节点恢复
                </Button>
              )}
              <Button
                size="sm"
                variant="ghost"
                disabled={busy}
                onClick={() =>
                  confirm(
                    `取消容器 ${container.name} 的备份节点？备份节点上的副本将被删除。`,
                  ) && setBackup("", true)
                }
              >
                取消备份
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
                立即同步
              </Button>
            </div>
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
            容器已设置顶替，顶替节点上已有持续更新的副本，无需备份节点。
          </p>
        ) : (
          <>
            {candidates.length === 0 ? (
              <p className="text-[13px] text-muted-foreground">
                无其他在线节点可用作备份节点。
              </p>
            ) : (
              <div className="flex flex-wrap items-end gap-3">
                <Field label="节点" htmlFor="backup-node">
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
                  设为备份节点
                </Button>
              </div>
            )}
            {error && <Alert>{error}</Alert>}
          </>
        )}
      </div>
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
      `在 ${nodes.name(container.backupNodeId)} 上恢复容器 ${container.name}？恢复后容器将在该节点上运行；原节点重新上线时，其上的该容器将被停止，目录保留。`,
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
          label="恢复到目录"
          htmlFor="restore-dir"
          help="默认：<项目根目录>/<项目名>/<容器名>。须为绝对路径，且目录不存在或为空。"
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
          从最新快照恢复
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
                从此快照恢复
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
    confirm(
      `将容器 ${container.name} 迁移到 ${target}？迁移期间容器停止，完成后在新节点上启动，原节点上的目录保留。`,
    ) &&
    run.run(() =>
      projectClient.migrateContainer({
        projectId: project.id,
        container: container.name,
        nodeId: choice,
        dir: dir.trim() || defDir,
      }),
    );
  const redeploy = () =>
    confirm(
      `将容器 ${container.name} 重新部署到 ${target}？运行数据和旧版本文件均保留在原节点上，新节点上的数据为空，回滚到旧版本时不包含版本文件。`,
    ) &&
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
      title="更换节点"
      description={
        online
          ? "将容器连同数据迁移到另一节点，名称、配置、版本记录和 webhook 地址不变。"
          : "容器所在的节点离线。"
      }
      help="迁移：容器运行时进行首次同步，停止后增量同步剩余数据，停机时间短，数据不丢失。原节点离线时：已设置备份节点的，使用「从备份节点恢复」；无副本的，仅可重新部署到其他节点（数据为空）。"
    >
      <div className="grid gap-4">
        <dl className="grid grid-cols-[7rem_1fr] gap-x-4 gap-y-2 text-[13px]">
          <dt className="text-muted-foreground">所在节点</dt>
          <dd>
            {nodes.name(container.nodeId)}
            {!online && <Badge className="ml-2">离线</Badge>}
          </dd>
          <dt className="text-muted-foreground">容器目录</dt>
          <dd className="font-mono text-xs">{container.dir}</dd>
        </dl>
        {!online && container.backupNodeId && (
          <Notice tone="warning">
            容器已设置备份节点，建议在上方「备份节点」中从副本恢复，以保留数据。
          </Notice>
        )}
        {container.failover ? (
          <p className="border-t pt-4 text-[13px] text-muted-foreground">
            容器已设置顶替，运行节点由租约决定。更换节点前须取消顶替。
          </p>
        ) : candidates.length === 0 ? (
          <p className="text-[13px] text-muted-foreground">
            无其他在线节点。
          </p>
        ) : (
          <div className="flex flex-wrap items-end gap-3 border-t pt-4">
            <Field label="目标节点" htmlFor="move-node">
              <Select
                id="move-node"
                value={choice}
                onChange={(e) => setNodeId(e.target.value)}
                className="min-w-48"
              >
                {candidates.map((n) => (
                  <option key={n.id} value={n.id}>
                    {n.name}
                    {n.id === container.backupNodeId ? "（备份节点）" : ""}
                  </option>
                ))}
              </Select>
            </Field>
            <Field
              label="容器目录"
              htmlFor="move-dir"
              help="默认：<项目根目录>/<项目名>/<容器名>。须为绝对路径，且目录不存在或为空。"
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
                迁移
              </Button>
            ) : (
              <Button
                size="sm"
                variant="destructive"
                disabled={run.busy || !choice}
                onClick={redeploy}
              >
                {run.busy && <Spinner />}
                重新部署到此节点
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
  return s >= 60 ? `${Math.ceil(s / 60)} 分钟` : `${s} 秒`;
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
        title="顶替"
        description="容器所在节点故障时，由指定的顶替节点自动接管运行。"
        help="容器所在节点、顶替节点和见证人各持一票，获得两票的一方运行容器。所在节点每 15 分钟将快照同步至顶替节点，顶替节点预先拉取镜像。有状态容器默认在确认后顶替，因最后一次同步之后写入的数据将丢失。"
      >
        {container.backupNodeId ? (
          <p className="text-[13px] text-muted-foreground">
            容器已设置备份节点。顶替与备份节点不可同时设置，设置顶替前须取消备份节点。
          </p>
        ) : standbys.length === 0 ? (
          <p className="text-[13px] text-muted-foreground">
            无其他在线节点可用作顶替节点。
          </p>
        ) : (
          <div className="grid gap-4">
            <div className="flex flex-wrap items-end gap-3">
              <Field label="顶替节点" htmlFor="fo-standby">
                <Select
                  id="fo-standby"
                  value={standbyId}
                  onChange={(e) =>
                    setForm({ ...form, standby: e.target.value })
                  }
                  className="min-w-44"
                >
                  {standbys.map((n) => (
                    <option key={n.id} value={n.id}>
                      {n.name}
                    </option>
                  ))}
                </Select>
              </Field>
              <Field
                label="顶替节点目录"
                htmlFor="fo-dir"
                help="默认：<项目根目录>/<项目名>/<容器名>。须为绝对路径，且目录不存在或为空。"
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
            </div>
            <div className="flex flex-wrap items-end gap-3">
              <Field
                label="见证人"
                htmlFor="fo-witness"
                help="仅参与投票，不运行容器。默认为面板；为避免面板与节点同时故障，可指定其他节点。"
              >
                <Select
                  id="fo-witness"
                  value={form.witness}
                  onChange={(e) =>
                    setForm({ ...form, witness: e.target.value })
                  }
                  className="min-w-44"
                >
                  <option value="">本面板</option>
                  {witnesses.map((n) => (
                    <option key={n.id} value={n.id}>
                      {n.name}
                    </option>
                  ))}
                </Select>
              </Field>
              <Field
                label="租约时长（秒）"
                htmlFor="fo-lease"
                help="默认 30 秒，每 1/3 租约时长续约一次。所在节点故障后约 40 秒内完成顶替。"
              >
                <Input
                  id="fo-lease"
                  value={form.lease}
                  onChange={(e) => setForm({ ...form, lease: e.target.value })}
                  placeholder="30"
                  className="w-28"
                  inputMode="numeric"
                />
              </Field>
            </div>
            <div className="flex flex-wrap gap-x-6 gap-y-2">
              <Check
                checked={form.autoFailback}
                onChange={(v) => setForm({ ...form, autoFailback: v })}
                help="原节点恢复并稳定运行 10 分钟后，将容器切回原节点：停止容器、同步剩余数据、在原节点上启动，数据不丢失。"
              >
                原节点恢复后自动切回
              </Check>
              {container.stateful && (
                <Check
                  checked={form.autoStateful}
                  onChange={(v) => setForm({ ...form, autoStateful: v })}
                  help="无需确认即顶替，最后一次同步之后写入的数据将丢失。"
                >
                  有状态容器自动顶替
                </Check>
              )}
            </div>
            {error && <Alert>{error}</Alert>}
            <div className="flex justify-end pt-2">
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
                设置顶替
              </Button>
            </div>
          </div>
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
        正在切回原节点
      </Status>
    );
  else if (holder?.role === "holder" && holder.leaseRemainingMs > 0n)
    state = <Status tone="success">运行中</Status>;
  else if (holder?.role === "holder")
    state = <Status tone="danger">租约续约失败，容器已停止</Status>;
  else if (!holderOnline && standby?.pendingConfirm)
    state = <Status tone="warning">所在节点失联，等待确认顶替</Status>;
  else if (!holderOnline) state = <Status tone="warning">所在节点离线</Status>;
  const witnessOptions = nodes.usable.filter(
    (n) =>
      n.id !== cfg.a?.nodeId &&
      n.id !== cfg.b?.nodeId &&
      (cfg.witnessIsPanel || n.id !== cfg.witness?.nodeId),
  );

  return (
    <Section
      title="顶替"
      description="容器所在节点故障时，由另一节点自动接管运行。"
      help="容器所在节点、顶替节点和见证人各持一票，获得两票的一方运行容器。所在节点每 15 分钟将快照同步至热备。"
    >
      <div className="grid gap-4">
        <dl className="grid grid-cols-[7rem_1fr] gap-x-4 gap-y-2 text-[13px]">
          <dt className="text-muted-foreground">状态</dt>
          <dd>{state}</dd>
          <dt className="text-muted-foreground">运行节点</dt>
          <dd>
            {nodes.name(container.nodeId)}
            {cfg.a?.nodeId === container.nodeId ? (
              <Badge className="ml-2">原节点</Badge>
            ) : (
              <Badge tone="warning" className="ml-2">
                已顶替
              </Badge>
            )}
          </dd>
          <dt className="text-muted-foreground">热备</dt>
          <dd>
            {standbyNode?.name}
            {standbyNode && !nodes.online(standbyNode.nodeId) && (
              <Badge className="ml-2">离线</Badge>
            )}
          </dd>
          <dt className="text-muted-foreground">见证人</dt>
          <dd>
            {!cfg.witnessIsPanel ? (
              cfg.witness?.name
            ) : container.failoverWitnessSelf ? (
              "本面板"
            ) : (
              <span
                className="text-warning"
                title="见证人仍为更换前的旧面板。节点上报后自动切换为本面板；切换完成前，容器所在节点与顶替节点的两票仍构成多数。"
              >
                旧面板（正在切换为本面板）
              </span>
            )}
          </dd>
          <dt className="text-muted-foreground">最近同步</dt>
          <dd title={toDate(lastSync)?.toLocaleString()}>
            {lastSync ? timeAgo(toDate(lastSync)) : "尚未同步"}
          </dd>
          <dt className="text-muted-foreground">租约</dt>
          <dd>
            {cfg.leaseSeconds || 30} 秒
            {holder?.epoch ? `，第 ${holder.epoch} 轮` : ""}
          </dd>
          <dt className="text-muted-foreground">自动切回</dt>
          <dd>
            {cfg.autoFailback ? "开启" : "关闭"}
            {holder && holder.failbackInMs > 0n && (
              <span className="ml-2 text-muted-foreground">
                原节点已恢复，约 {duration(holder.failbackInMs)}后切回
              </span>
            )}
          </dd>
        </dl>
        {standby?.pendingConfirm && (
          <Notice
            tone="warning"
            title="等待确认顶替"
            actions={
              <Button
                size="sm"
                variant="destructive"
                disabled={busy}
                onClick={() =>
                  confirm(
                    `确认由 ${standbyNode?.name} 顶替容器 ${container.name}？最后一次同步之后写入的数据将丢失。`,
                  ) && call(() => projectClient.confirmTakeover(ref))
                }
              >
                确认顶替
              </Button>
            }
          >
            容器所在节点失联。该容器为有状态容器，顶替后最后一次同步之后写入的数据将丢失。
          </Notice>
        )}
        {(holder?.lastError || standby?.lastError) && (
          <Alert>{holder?.lastError || standby?.lastError}</Alert>
        )}
        {error && <Alert>{error}</Alert>}
        <div className="flex flex-wrap items-end gap-2 border-t pt-4">
          <Field label="更换见证人" htmlFor="fo-change-witness">
            <Select
              id="fo-change-witness"
              value={witness}
              onChange={(e) => setWitness(e.target.value)}
              className="min-w-40"
            >
              <option value="-" disabled>
                请选择
              </option>
              {!(cfg.witnessIsPanel && container.failoverWitnessSelf) && (
                <option value="">本面板</option>
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
            更换
          </Button>
          <Button
            size="sm"
            variant="ghost"
            className="ml-auto"
            disabled={busy}
            onClick={() =>
              confirm(
                `取消容器 ${container.name} 的顶替？热备上的副本将被删除，容器保留在 ${nodes.name(container.nodeId)} 上。`,
              ) && call(() => projectClient.clearFailover(ref))
            }
          >
            取消顶替
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
      className="min-w-80 font-mono"
    />
  );
}
