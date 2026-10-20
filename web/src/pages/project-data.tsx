import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { errorMessage, nodeClient, projectClient, toDate } from "@/lib/api";
import { timeAgo } from "@/lib/utils";
import { useRun } from "@/components/run";
import { fmtBytes } from "@/components/metrics";
import {
  Alert,
  Badge,
  Button,
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

// 「数据」标签页，按容器：快照、备份节点、更换节点（迁移、从备份节点恢复、重新部署到其他节点）。
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

export function DataTab({ project }: { project: Project }) {
  const [selected, setSelected] = useState("");
  const container =
    project.containers.find((c) => c.name === selected) ??
    project.containers[0];
  const run = useRun(project.id);

  if (!container) {
    return (
      <Section title="快照">
        <EmptyRow>暂无容器</EmptyRow>
      </Section>
    );
  }

  return (
    <div className="grid gap-6">
      <div className="flex flex-wrap items-center gap-3">
        <Select
          value={container.name}
          onChange={(e) => setSelected(e.target.value)}
          className="h-8 w-auto min-w-48 text-[13px]"
          aria-label="容器"
        >
          {project.containers.map((c) => (
            <option key={c.name} value={c.name}>
              容器：{c.name}
            </option>
          ))}
        </Select>
        <span className="text-[13px] text-muted-foreground">
          快照、备份和迁移都按容器进行。
        </span>
      </div>
      {run.output && <div className="empty:hidden">{run.output}</div>}
      <Snapshots
        key={`s-${container.name}`}
        project={project}
        container={container}
        run={run}
      />
      <Backup
        key={`b-${container.name}`}
        project={project}
        container={container}
        run={run}
      />
      <Placement
        key={`p-${container.name}`}
        project={project}
        container={container}
        run={run}
      />
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
      help="定时和手动快照可在此恢复。「部署前数据」和「版本文件」用于回滚，在容器的「版本」中操作。快照仓库在容器所在的节点上，容器目录的 backups/ 里。"
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
            <Button
              size="sm"
              variant="ghost"
              disabled={run.busy}
              onClick={() =>
                run.run(() =>
                  projectClient.createSnapshot({
                    projectId: project.id,
                    container: "",
                  }),
                )
              }
            >
              全部容器
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
  if (snapshots.length === 0) return <EmptyRow>暂无快照</EmptyRow>;
  return (
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
        {snapshots.map((sn) => (
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
    return <Alert className="mx-5 mb-4">{errorMessage(snapshots.error)}</Alert>;
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
                `从此快照恢复容器 ${container} 的 data/ 和 volumes/？恢复前将先停止容器，当前数据将被覆盖。`,
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
      description="每次定时快照后，把容器的快照仓库增量同步到另一个节点。"
      help="备份节点只保存副本，不运行容器、不会自动顶替。容器所在的节点损坏时，可以在备份节点上把容器恢复出来；数据来自副本，配置来自面板。"
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
            <div className="flex flex-wrap items-center justify-end gap-2 border-t pt-4">
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
        ) : (
          <>
            {candidates.length === 0 ? (
              <p className="text-[13px] text-muted-foreground">
                没有其他在线节点可用作备份节点。
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
  const snapshots = useQuery({
    queryKey: ["backup-snapshots", project.id, container.name],
    queryFn: () =>
      projectClient.listBackupSnapshots({
        projectId: project.id,
        container: container.name,
      }),
  });
  const root =
    nodes.list.find((n) => n.id === container.backupNodeId)?.info
      ?.projectsRoot || "/srv/nagi";
  const restore = (snapshotId: string) =>
    confirm(
      `在 ${nodes.name(container.backupNodeId)} 上恢复容器 ${container.name}？恢复后容器改在该节点上运行，原节点恢复上线时会停止其上的这个容器并保留目录。`,
    ) &&
    run.run(() =>
      projectClient.restoreFromBackup({
        projectId: project.id,
        container: container.name,
        snapshotId,
        dir: dir.trim(),
      }),
    );
  return (
    <div className="grid gap-3 rounded-md border">
      <div className="flex flex-wrap items-end gap-3 px-4 pt-4">
        <Field
          label="恢复到目录"
          htmlFor="restore-dir"
          help="留空则使用默认目录：备份节点的项目根目录下按网络名和容器名生成。目录须不存在或为空。"
        >
          <Input
            id="restore-dir"
            value={dir}
            onChange={(e) => setDir(e.target.value)}
            placeholder={`${root}/${project.networkName}/${container.name}`}
            className="min-w-80 font-mono"
          />
        </Field>
        <Button size="sm" disabled={run.busy} onClick={() => restore("")}>
          用最新的快照恢复
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
                用此快照恢复
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
  const candidates = nodes.usable.filter((n) => n.id !== container.nodeId);
  const choice =
    nodeId ||
    (container.backupNodeId &&
    candidates.some((n) => n.id === container.backupNodeId)
      ? container.backupNodeId
      : candidates[0]?.id) ||
    "";
  const root =
    candidates.find((n) => n.id === choice)?.info?.projectsRoot || "/srv/nagi";
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
        dir: dir.trim(),
      }),
    );
  const redeploy = () =>
    confirm(
      `将容器 ${container.name} 重新部署到 ${target}？运行数据和旧版本的文件都在原节点上，新节点上的数据为空，旧版本无法带文件回滚。`,
    ) &&
    run.run(() =>
      projectClient.redeployElsewhere({
        projectId: project.id,
        container: container.name,
        nodeId: choice,
        dir: dir.trim(),
      }),
    );

  return (
    <Section
      title="更换节点"
      description={
        online
          ? "把容器连同数据迁移到另一个节点，名称、配置、版本记录和 webhook 地址不变。"
          : "容器所在的节点离线。"
      }
      help="迁移：先在容器运行时同步一次，停止后再增量同步最后的数据，停机时间很短，不丢数据。原节点离线时：设置了备份节点的，用「从备份节点恢复」；没有副本的，只能重新部署到其他节点（数据为空）。"
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
            容器设置了备份节点，建议在上方「备份节点」中从副本恢复，可保留数据。
          </Notice>
        )}
        {candidates.length === 0 ? (
          <p className="text-[13px] text-muted-foreground">
            没有其他在线节点。
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
              help="留空则使用默认目录：目标节点的项目根目录下按网络名和容器名生成。目录须不存在或为空。"
            >
              <Input
                id="move-dir"
                value={dir}
                onChange={(e) => setDir(e.target.value)}
                placeholder={`${root}/${project.networkName}/${container.name}`}
                className="min-w-80 font-mono"
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
