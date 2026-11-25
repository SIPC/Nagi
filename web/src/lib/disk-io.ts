// 节点的磁盘读写（见 docs/monitoring.md 的 node:disk_read、node:disk_write）。节点列表和概览的节点表用节点上报里
// 最新的读写速率（NodeUsage.disk_read、disk_write），不用另外查图表历史。

// 与 internal/pkg/release.IsVersion 一致。
const versionRe = /^v(\d+)\.(\d+)\.(\d+)([-+][0-9A-Za-z.-]+)?$/;

// lacksDiskIO：节点早于 v0.37.5，没有磁盘读写（图表历史里没有 node:disk_read 等、容器的 :disk_read 等，上报里也没有）。
// 和 lacksLogService 一样只比较主、次、修订号（v0.37.5 的预发布版算支持）；本地编译的 dev 当作支持。
export function lacksDiskIO(version: string | undefined) {
  const x = versionRe.exec(version ?? "");
  return !!x && Number(x[1]) * 1e6 + Number(x[2]) * 1e3 + Number(x[3]) < 37005;
}

// diskIOCell：节点列表、概览节点表里「读写」一格的内容。kind 决定写法：rates 是两个速率（↑ 写入 ↓ 读取，
// 和流量的写法一样，写入是往盘里去），upgrade 是版本过旧要升级，none 是没有数据（离线、待批准、刚启动还没有第二次采样、
// 读不到 /proc/diskstats）。读写都是 0 时照常写 0 B/s，不当成没有数据。
export type DiskIOCell = { kind: "rates"; read: number; write: number } | { kind: "upgrade" } | { kind: "none" };

export function diskIOCell(n: { online: boolean; pending?: boolean; version?: string; read?: number; write?: number }): DiskIOCell {
  if (!n.online || n.pending) return { kind: "none" };
  if (lacksDiskIO(n.version)) return { kind: "upgrade" };
  if (n.read === undefined || n.write === undefined) return { kind: "none" };
  return { kind: "rates", read: n.read, write: n.write };
}