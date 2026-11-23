// node --test scripts/*.test.mjs（Node 22.18+ 直接加载 .ts）。
import { test } from "node:test";
import assert from "node:assert/strict";
import { countRecentGaps, isReattach } from "../src/lib/log-gaps.ts";

const now = new Date("2026-11-23T09:00:00Z");
const since = new Date(now.getTime() - 24 * 3600 * 1000);
const at = (h) => new Date(now.getTime() - h * 3600 * 1000);

test("重启留下的重新接上采集记录不算缺口", () => {
  // 一次升级重启：12 个容器各一条输出接续，加 Docker 事件和宿主机日志各一条。
  const restart = [
    ...Array.from({ length: 12 }, () => ({ kind: "container.output.attaching", time: at(1) })),
    { kind: "docker.events.attaching", time: at(1) },
    { kind: "host.journal.attaching", time: at(1) },
    { kind: "docker.inventory.resumed", time: at(1) },
  ];
  assert.equal(countRecentGaps(restart, since), 0);
});

test("最近的真实缺口全部计入，不受每页 10 条限制；更早的不算", () => {
  const entries = [
    ...Array.from({ length: 15 }, () => ({ kind: "container.output.interrupted", time: at(2) })),
    { kind: "logstore.write.lost", time: at(0.5) },
    { kind: "host.journal.gap", time: at(23.9) },
    { kind: "docker.events.interrupted", time: at(25) }, // 超过 24 小时
    { kind: "container.output.attaching", time: at(2) },
    { kind: "decisions.gap", time: undefined },
  ];
  assert.equal(countRecentGaps(entries, since), 17);
});

test("只把接续记录当成正常", () => {
  assert.equal(isReattach("container.output.attaching"), true);
  assert.equal(isReattach("docker.inventory.resumed"), true);
  for (const kind of ["container.output.interrupted", "docker.inventory.interrupted", "container.disappeared", "host.journal.unavailable", "host.journal.gap", "logstore.write.lost", "decisions.gap"]) {
    assert.equal(isReattach(kind), false, kind);
  }
});
