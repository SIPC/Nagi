import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { errorMessage, projectClient, toDate } from "@/lib/api";
import { timeAgo } from "@/lib/utils";
import { useRun } from "@/components/run";
import { fmtBytes } from "@/components/metrics";
import { BackupCard, FailoverCard, MigrateCard } from "@/pages/backup";
import { Alert, Badge, Button, Check, EmptyRow, Section, Select, Spinner, Table, Td, Th } from "@/components/ui";
import type { Project } from "@/gen/nagi/v1/common_pb";

// 「数据」标签页：快照、备份节点、顶替、迁移，以及副本上的特权配置。

export function DataTab({ nodeId, project, trusted }: { nodeId: string; project: Project; trusted: boolean }) {
  const networks = project.settings?.sharedNetworks ?? [];
  return (
    <div className="grid gap-6">
      <Snapshots nodeId={nodeId} project={project} />
      <BackupCard nodeId={nodeId} projectId={project.id} />
      <FailoverCard nodeId={nodeId} projectId={project.id} dir={project.dir} sharedNetworks={networks} />
      <MigrateCard nodeId={nodeId} projectId={project.id} name={project.name} dir={project.dir} sharedNetworks={networks} />
      <ReplicaTrust nodeId={nodeId} projectId={project.id} trusted={trusted} />
    </div>
  );
}

// ---- 快照 ----

const snapshotKinds: Record<string, string> = {
  scheduled: "定时",
  manual: "手动",
  predeploy: "部署前数据",
  version: "版本文件",
};

function Snapshots({ nodeId, project }: { nodeId: string; project: Project }) {
  const { run, busy, output } = useRun(nodeId, project.id);
  const ref = { nodeId, projectId: project.id };
  const snapshots = useQuery({ queryKey: ["snapshots", nodeId, project.id], queryFn: () => projectClient.listNodeProjectSnapshots(ref) });
  const [container, setContainer] = useState("");

  return (
    <Section
      flush
      title="快照"
      description="保存所有容器的 data/ 和 volumes/，恢复时配置保持不变。"
      help="定时和手动快照可在此恢复。「部署前数据」和「版本文件」用于回滚，在容器的「版本」中操作。"
      actions={
        <>
          <Select value={container} onChange={(e) => setContainer(e.target.value)} className="h-8 w-auto min-w-44 text-[13px]" aria-label="恢复范围">
            <option value="">恢复范围：整个项目</option>
            {project.containers.map((m) => (
              <option key={m.name} value={m.name}>
                恢复范围：{m.name}
              </option>
            ))}
          </Select>
          <Button size="sm" variant="outline" disabled={busy} onClick={() => run(() => projectClient.createNodeProjectSnapshot(ref))}>
            {busy && <Spinner />}
            创建快照
          </Button>
        </>
      }
    >
      {output && <div className="px-5 pb-3 empty:hidden">{output}</div>}
      {snapshots.isPending ? (
        <EmptyRow>
          <Spinner />
        </EmptyRow>
      ) : snapshots.data?.snapshots.length === 0 ? (
        <EmptyRow>暂无快照</EmptyRow>
      ) : (
        <Table>
          <thead>
            <tr>
              <Th>类型</Th>
              <Th>时间</Th>
              <Th>范围</Th>
              <Th>大小</Th>
              <Th />
            </tr>
          </thead>
          <tbody>
            {snapshots.data?.snapshots.map((sn) => (
              <tr key={sn.id}>
                <Td className="text-[13px]">
                  <Badge tone={sn.kind === "scheduled" || sn.kind === "manual" ? "neutral" : "info"}>{snapshotKinds[sn.kind] ?? sn.kind}</Badge>
                </Td>
                <Td className="text-[13px] text-muted-foreground" title={toDate(sn.time)?.toLocaleString()}>
                  {timeAgo(toDate(sn.time))}
                </Td>
                <Td className="font-mono text-xs text-muted-foreground">
                  {sn.container || "整个项目"}
                  {sn.version > 0n && ` v${sn.version}`}
                </Td>
                <Td className="font-mono text-xs text-muted-foreground">{fmtBytes(Number(sn.size))}</Td>
                <Td className="text-right">
                  {(sn.kind === "scheduled" || sn.kind === "manual") && (
                    <Button
                      size="xs"
                      variant="ghost"
                      disabled={busy}
                      onClick={() =>
                        confirm(`从此快照恢复${container ? `容器 ${container}` : "整个项目"}的 data/ 和 volumes/？恢复前将先停止相关容器，当前数据将被覆盖。`) &&
                        run(() => projectClient.restoreNodeProjectSnapshot({ ...ref, snapshotId: sn.id, container }))
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

// ---- 副本上的特权配置 ----

// ReplicaTrust：项目在别的节点上从副本启动（备份恢复、顶替、迁移、重建）时，配置里有特权设置的默认不启动，
// 防止源节点被攻破后借副本控制别的节点。管理员确认配置可信后在这里允许。
function ReplicaTrust({ nodeId, projectId, trusted }: { nodeId: string; projectId: string; trusted: boolean }) {
  const queryClient = useQueryClient();
  const [error, setError] = useState("");
  const toggle = async (next: boolean) => {
    if (
      next &&
      !confirm(
        "允许此项目在备份节点、顶替节点和迁移目标上使用特权配置？\n\n仅在确认项目配置可信时开启：源节点被攻破时，攻击者可借副本在这些节点上取得 root 权限。",
      )
    )
      return;
    setError("");
    try {
      await projectClient.setProjectReplicaTrust({ projectId, trusted: next });
      queryClient.invalidateQueries({ queryKey: ["project", nodeId, projectId] });
    } catch (err) {
      setError(errorMessage(err));
    }
  };
  return (
    <Section
      title="副本特权配置"
      description="项目从副本在其他节点启动时，默认拒绝特权配置。"
      help="适用于恢复备份、顶替、迁移和按配置备份重建。特权配置包括特权模式、挂载宿主机路径、host 网络等。副本来自原节点，原节点被攻破时可借此控制其他节点。允许后由面板直接通知各节点。"
    >
      <div className="grid gap-3">
        {error && <Alert>{error}</Alert>}
        <Check checked={trusted} onChange={toggle}>
          允许在其他节点上使用特权配置
        </Check>
      </div>
    </Section>
  );
}
