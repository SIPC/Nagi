// 日志页「可能不完整」提示的计数规则（见 pages/log-entries.tsx 的 LogCoverage）。
// 采集边界记录都带 history_complete:false，但其中大部分是进程启动或重连时的正常「重新接上采集」记录：
// 每次重启每个容器各一条，不代表这次丢了数据，只用来在详情里说明从哪里接上。提示只统计最近一段时间里
// 的真实缺口（采集中断、来源不可用、写入丢失等），不受详情列表每页条数的限制。

// 正常的重新接上采集：进程启动、断线后重连时都会写一条。
export function isReattach(kind: string) {
  return kind.endsWith(".attaching") || kind === "docker.inventory.resumed";
}

// 提示里统计的时间范围。
export const recentGapWindowMs = 24 * 60 * 60 * 1000;

// countRecentGaps 数出 since 之后的真实缺口记录。
export function countRecentGaps(entries: { kind: string; time: Date | undefined }[], since: Date) {
  return entries.filter((e) => !isReattach(e.kind) && !!e.time && e.time >= since).length;
}
