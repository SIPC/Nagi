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

// 一页缺口记录：next 为下一页的位置，没有下一页时为 undefined。
export type GapPage<P> = { entries: { kind: string; time: Date | undefined }[]; next: P | undefined };

// 最多读的页数（每页 1000 条）。读到上限还有下一页时，返回的计数只是下限。
export const recentGapMaxPages = 20;

// countRecentGaps 按页读完 since 之后的缺口记录，数出其中的真实缺口。重启接续记录
// 可能多过一页，真实缺口排在后面，只读第一页会把它们漏成 0。complete 为 false 时，
// count 只是下限（「至少 N 条」）。
export async function countRecentGaps<P>(read: (page: P | undefined) => Promise<GapPage<P>>, since: Date) {
  let count = 0;
  let page: P | undefined;
  for (let i = 0; i < recentGapMaxPages; i++) {
    const got = await read(page);
    count += got.entries.filter((e) => !isReattach(e.kind) && !!e.time && e.time >= since).length;
    if (got.next === undefined) return { count, complete: true };
    page = got.next;
  }
  return { count, complete: false };
}

// gapNotice 决定提示怎么显示。issues 为 0 但没数完（读到页数上限）时不能显示成「没有缺口」：
// 前 20 页可能全是接续记录，真实缺口在后面，此时也按警告显示「没有数完」。
// lost 是本进程确认丢失写入的类别数（只在没数到缺口时补 1 条）。
export function gapNotice(recent: { count: number; complete: boolean } | undefined, lost: number) {
  const issues = (recent?.count ?? 0) + (recent?.count ? 0 : lost);
  const truncated = recent?.complete === false;
  return { issues, truncated, warn: issues > 0 || truncated };
}
