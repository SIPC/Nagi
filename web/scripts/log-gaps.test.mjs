// node --test scripts/*.test.mjs（Node 22.18+ 直接加载 .ts）。
import { test } from "node:test";
import assert from "node:assert/strict";
import { countRecentGaps, gapNotice, isReattach, recentGapMaxPages } from "../src/lib/log-gaps.ts";

const now = new Date("2026-11-23T09:00:00Z");
const since = new Date(now.getTime() - 24 * 3600 * 1000);
const at = (h) => new Date(now.getTime() - h * 3600 * 1000);

// 模拟后端分页：每页 limit 条，用 source/after 定位下一页；记下每次请求的位置。
function pager(entries, limit = 1000) {
  const calls = [];
  const read = async (page) => {
    calls.push(page);
    const start = page ? Number(page.after) : 0;
    if (page) assert.equal(page.source, "node:disk");
    const end = Math.min(start + limit, entries.length);
    return { entries: entries.slice(start, end), next: end < entries.length ? { source: "node:disk", after: BigInt(end) } : undefined };
  };
  return { read, calls };
}

test("重启留下的重新接上采集记录不算缺口", async () => {
  // 一次升级重启：12 个容器各一条输出接续，加 Docker 事件和宿主机日志各一条。
  const restart = [
    ...Array.from({ length: 12 }, () => ({ kind: "container.output.attaching", time: at(1) })),
    { kind: "docker.events.attaching", time: at(1) },
    { kind: "host.journal.attaching", time: at(1) },
    { kind: "docker.inventory.resumed", time: at(1) },
  ];
  assert.deepEqual(await countRecentGaps(pager(restart).read, since), { count: 0, complete: true });
});

test("最近的真实缺口全部计入，不受每页 10 条限制；更早的不算", async () => {
  const entries = [
    ...Array.from({ length: 15 }, () => ({ kind: "container.output.interrupted", time: at(2) })),
    { kind: "logstore.write.lost", time: at(0.5) },
    { kind: "host.journal.gap", time: at(23.9) },
    { kind: "docker.events.interrupted", time: at(25) }, // 超过 24 小时
    { kind: "container.output.attaching", time: at(2) },
    { kind: "decisions.gap", time: undefined },
  ];
  assert.deepEqual(await countRecentGaps(pager(entries).read, since), { count: 17, complete: true });
});

test("第一页全是接续记录时继续翻页，数到后面的真实缺口", async () => {
  // 1000 条接续记录占满第一页，真实缺口在第 1001 条和第 2500 条。
  const entries = Array.from({ length: 2600 }, (_, i) => ({ kind: i === 1000 || i === 2499 ? "container.output.interrupted" : "container.output.attaching", time: at(1) }));
  const { read, calls } = pager(entries);
  assert.deepEqual(await countRecentGaps(read, since), { count: 2, complete: true });
  assert.deepEqual(calls, [undefined, { source: "node:disk", after: 1000n }, { source: "node:disk", after: 2000n }]);
});

test("读到页数上限还有下一页时，只报下限，不当成数完", async () => {
  const entries = Array.from({ length: (recentGapMaxPages + 1) * 10 }, (_, i) => ({ kind: i % 10 === 0 ? "host.journal.gap" : "host.journal.attaching", time: at(1) }));
  const { read, calls } = pager(entries, 10);
  assert.deepEqual(await countRecentGaps(read, since), { count: recentGapMaxPages, complete: false });
  assert.equal(calls.length, recentGapMaxPages);
});

test("没数完时即使数到 0 条也按警告提示，不显示成没有缺口", async () => {
  // 前 20 页全是接续记录，还有下一页：真实缺口可能在后面。
  const entries = Array.from({ length: (recentGapMaxPages + 1) * 10 }, () => ({ kind: "container.output.attaching", time: at(1) }));
  const recent = await countRecentGaps(pager(entries, 10).read, since);
  assert.deepEqual(recent, { count: 0, complete: false });
  assert.deepEqual(gapNotice(recent, 0), { issues: 0, truncated: true, warn: true });
  // 数完且没有缺口才是中性提示；确认丢失写入时补 1 条。
  assert.deepEqual(gapNotice({ count: 0, complete: true }, 0), { issues: 0, truncated: false, warn: false });
  assert.deepEqual(gapNotice({ count: 0, complete: true }, 1), { issues: 1, truncated: false, warn: true });
  assert.deepEqual(gapNotice({ count: 3, complete: true }, 1), { issues: 3, truncated: false, warn: true });
  assert.deepEqual(gapNotice(undefined, 0), { issues: 0, truncated: false, warn: false });
});

test("只把接续记录当成正常", () => {
  assert.equal(isReattach("container.output.attaching"), true);
  assert.equal(isReattach("docker.inventory.resumed"), true);
  for (const kind of ["container.output.interrupted", "docker.inventory.interrupted", "container.disappeared", "host.journal.unavailable", "host.journal.gap", "logstore.write.lost", "decisions.gap"]) {
    assert.equal(isReattach(kind), false, kind);
  }
});
