import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { errorMessage, projectClient, toDate } from "@/lib/api";
import { timeAgo } from "@/lib/utils";
import { useRun } from "@/components/run";
import { fmtBytes } from "@/components/metrics";
import { Alert, Badge, Button, EmptyRow, Section, Select, Spinner, Table, Td, Th } from "@/components/ui";
import type { Project } from "@/gen/nagi/v1/common_pb";

// 「数据」标签页：按容器的快照。每个容器有自己的快照仓库，在容器所在的节点上。
// 备份节点、顶替和迁移改为按容器后放在这里（见 docs/development.md）。

const snapshotKinds: Record<string, string> = {
  scheduled: "定时",
  manual: "手动",
  predeploy: "部署前数据",
  version: "版本文件",
};

export function DataTab({ project }: { project: Project }) {
  const deployed = project.containers.filter((c) => c.deployedVersion > 0n);
  const [selected, setSelected] = useState("");
  const container = deployed.find((c) => c.name === selected) ?? deployed[0];
  const { run, busy, output } = useRun(project.id);

  if (deployed.length === 0) {
    return (
      <Section title="快照">
        <EmptyRow>还没有部署过的容器</EmptyRow>
      </Section>
    );
  }

  return (
    <div className="grid gap-6">
      <Section
        flush
        title="快照"
        description="每个容器的 data/ 和 volumes/ 单独保存，恢复时配置保持不变。"
        help="定时和手动快照可在此恢复。「部署前数据」和「版本文件」用于回滚，在容器的「版本」中操作。快照仓库在容器所在的节点上，容器目录的 backups/ 里。"
        actions={
          <>
            <Select value={container?.name ?? ""} onChange={(e) => setSelected(e.target.value)} className="h-8 w-auto min-w-40 text-[13px]" aria-label="容器">
              {deployed.map((c) => (
                <option key={c.name} value={c.name}>
                  容器：{c.name}
                </option>
              ))}
            </Select>
            <Button
              size="sm"
              variant="outline"
              disabled={busy}
              onClick={() => run(() => projectClient.createSnapshot({ projectId: project.id, container: container?.name ?? "" }))}
            >
              {busy && <Spinner />}
              创建快照
            </Button>
            <Button size="sm" variant="ghost" disabled={busy} onClick={() => run(() => projectClient.createSnapshot({ projectId: project.id, container: "" }))}>
              全部容器
            </Button>
          </>
        }
      >
        {output && <div className="px-5 pb-3 empty:hidden">{output}</div>}
        {container && <SnapshotList key={container.name} project={project} container={container.name} busy={busy} run={run} />}
      </Section>
    </div>
  );
}

function SnapshotList({
  project,
  container,
  busy,
  run,
}: {
  project: Project;
  container: string;
  busy: boolean;
  run: ReturnType<typeof useRun>["run"];
}) {
  const snapshots = useQuery({
    queryKey: ["snapshots", project.id, container],
    queryFn: () => projectClient.listSnapshots({ projectId: project.id, container }),
  });
  if (snapshots.isPending) {
    return (
      <EmptyRow>
        <Spinner />
      </EmptyRow>
    );
  }
  if (snapshots.error) return <Alert className="mx-5 mb-4">{errorMessage(snapshots.error)}</Alert>;
  if (snapshots.data.snapshots.length === 0) return <EmptyRow>暂无快照</EmptyRow>;
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
        {snapshots.data.snapshots.map((sn) => (
          <tr key={sn.id}>
            <Td className="text-[13px]">
              <Badge tone={sn.kind === "scheduled" || sn.kind === "manual" ? "neutral" : "info"}>{snapshotKinds[sn.kind] ?? sn.kind}</Badge>
            </Td>
            <Td className="text-[13px] text-muted-foreground" title={toDate(sn.time)?.toLocaleString()}>
              {timeAgo(toDate(sn.time))}
            </Td>
            <Td className="font-mono text-xs text-muted-foreground">{sn.version > 0n ? `v${sn.version}` : "—"}</Td>
            <Td className="font-mono text-xs text-muted-foreground">{fmtBytes(Number(sn.size))}</Td>
            <Td className="text-right">
              {(sn.kind === "scheduled" || sn.kind === "manual") && (
                <Button
                  size="xs"
                  variant="ghost"
                  disabled={busy}
                  onClick={() =>
                    confirm(`从此快照恢复容器 ${container} 的 data/ 和 volumes/？恢复前将先停止容器，当前数据将被覆盖。`) &&
                    run(() => projectClient.restoreSnapshot({ projectId: project.id, container, snapshotId: sn.id }))
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
  );
}
