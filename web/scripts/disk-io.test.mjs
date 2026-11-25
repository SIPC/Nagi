// node --test scripts/*.test.mjs（Node 22.18+ 直接加载 .ts）。
import { test } from "node:test";
import assert from "node:assert/strict";
import { diskIOCell, lacksDiskIO } from "../src/lib/disk-io.ts";

test("v0.37.5 起才有磁盘读写，比较主、次、修订号，不按字符串比", () => {
  for (const v of ["v0.37.4", "v0.36.9", "v0.9.99", "v0.37.4-pre.3"]) assert.equal(lacksDiskIO(v), true, v);
  // v0.37.10 按字符串比会小于 v0.37.5；v0.38.0、v1.0.0 主版本或次版本更大。
  for (const v of ["v0.37.5", "v0.37.5-pre.1", "v0.37.10", "v0.38.0", "v1.0.0", "dev", "", undefined]) assert.equal(lacksDiskIO(v), false, String(v));
});

test("节点列表、概览的读写格：在线新节点写速率，0 是数据不是缺失", () => {
  assert.deepEqual(diskIOCell({ online: true, version: "v0.37.5", read: 0, write: 2048 }), { kind: "rates", read: 0, write: 2048 });
  assert.deepEqual(diskIOCell({ online: true, version: "v0.37.5", read: 0, write: 0 }), { kind: "rates", read: 0, write: 0 });
});

test("读写格：旧节点要升级，离线、待批准和还没上报的节点没有数据", () => {
  // 旧节点即使带着值也按升级处理（不会有，但版本判断优先）。
  assert.deepEqual(diskIOCell({ online: true, version: "v0.37.4" }), { kind: "upgrade" });
  // 离线的旧节点不催升级：状态未知。
  assert.deepEqual(diskIOCell({ online: false, version: "v0.37.4" }), { kind: "none" });
  assert.deepEqual(diskIOCell({ online: false, version: "v0.37.5", read: 1, write: 1 }), { kind: "none" });
  assert.deepEqual(diskIOCell({ online: true, pending: true, version: "v0.37.5", read: 1, write: 1 }), { kind: "none" });
  // 刚启动、只采样过一次：只有读没有写也算没有数据，不把缺的那一半写成 0。
  assert.deepEqual(diskIOCell({ online: true, version: "v0.37.5", read: 5 }), { kind: "none" });
  assert.deepEqual(diskIOCell({ online: true, version: "v0.37.5" }), { kind: "none" });
});