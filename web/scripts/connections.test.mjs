// node --test scripts/*.test.mjs（Node 22.18+ 直接加载 .ts）。
import { test } from "node:test";
import assert from "node:assert/strict";
import { defaultFilter, filterConns, groupConns, ownerOf, portHint, remoteKind, remoteLabel } from "../src/lib/connections.ts";

const base = { protocol: "tcp", state: "ESTABLISHED", localIp: "10.0.0.2", localPort: 40000, remoteIp: "1.1.1.1", remotePort: 443, inbound: false, bytesSent: 0n, bytesReceived: 0n, sendRate: 0, receiveRate: 0, pid: 0, process: "", cmdline: "", dockerName: "", projectId: "", projectName: "", container: "", service: "", unit: "", remoteNodeName: "" };
const c = (o) => ({ ...base, ...o });

test("按进程分组：同名进程合成一组，没有进程的（内核、已关闭）单独一组，按速率合计排序", () => {
  const list = [
    c({ pid: 10, process: "python", sendRate: 100 }),
    c({ pid: 11, process: "python", receiveRate: 50 }),
    c({ pid: 0, remotePort: 3260, sendRate: 1000 }),
    c({ pid: 12, process: "nginx" }),
  ];
  const g = groupConns(list, "process");
  assert.deepEqual(g.map((x) => [x.kind, x.label, x.conns.length, x.sendRate + x.receiveRate]), [
    ["kernel", "", 1, 1000],
    ["process", "python", 2, 150],
    ["process", "nginx", 1, 0],
  ]);
});

test("按容器分组：Nagi 容器显示项目 / 容器（服务名和容器名不同时再加服务），别的容器用 Docker 名称，本机进程按 systemd 单元", () => {
  assert.deepEqual(ownerOf(c({ pid: 1, container: "web", projectName: "博客", projectId: "p1", dockerName: "x" })), { kind: "container", label: "博客 / web" });
  assert.deepEqual(ownerOf(c({ pid: 1, container: "stack", service: "db", projectId: "p1", dockerName: "x" })), { kind: "container", label: "p1 / stack / db" });
  assert.deepEqual(ownerOf(c({ pid: 1, dockerName: "orb-runner" })), { kind: "container", label: "orb-runner" });
  assert.deepEqual(ownerOf(c({ pid: 1, unit: "nagi-agent.service" })), { kind: "unit", label: "nagi-agent.service" });
  assert.deepEqual(ownerOf(c({ pid: 1 })), { kind: "host", label: "" });
  assert.deepEqual(ownerOf(c({})), { kind: "kernel", label: "" });
  // 两个不同项目里同名的容器不合并。
  const g = groupConns([c({ pid: 1, container: "web", projectName: "A" }), c({ pid: 2, container: "web", projectName: "B" })], "container");
  assert.equal(g.length, 2);
});

test("按远端分组，同一个 IP 不同端口合成一组；不分组时没有组", () => {
  const g = groupConns([c({ remoteIp: "8.8.8.8", remotePort: 53 }), c({ remoteIp: "8.8.8.8", remotePort: 443 }), c({ remoteIp: "1.1.1.1" })], "remote");
  assert.deepEqual(g.map((x) => [x.label, x.conns.length]), [["8.8.8.8", 2], ["1.1.1.1", 1]]);
  assert.deepEqual(groupConns([c({})], "none"), []);
});

test("筛选：默认只看出站；搜索远端 IP:端口、进程、容器、命令行、节点名；只看有流量的", () => {
  const list = [
    c({ remoteIp: "192.168.0.13", remotePort: 3260 }),
    c({ inbound: true, localPort: 80, remoteIp: "203.0.113.9", process: "nginx", pid: 5 }),
    c({ protocol: "udp", state: "", remoteIp: "8.8.8.8", remotePort: 53, process: "systemd-resolved", pid: 6, sendRate: 10 }),
    c({ remoteIp: "2001:db8::1", remotePort: 443, cmdline: "curl --token=abc", pid: 7, remoteNodeName: "HK-001" }),
  ];
  assert.equal(filterConns(list, defaultFilter).length, 3);
  assert.equal(filterConns(list, { ...defaultFilter, direction: "all" }).length, 4);
  assert.deepEqual(filterConns(list, { ...defaultFilter, query: "192.168.0.13:3260" }).map((x) => x.remotePort), [3260]);
  // IPv6 按 [地址]:端口 匹配，不会把 :443 当成别的地址的一部分误匹配。
  assert.deepEqual(filterConns(list, { ...defaultFilter, query: "[2001:db8::1]:443" }).map((x) => x.pid), [7]);
  assert.deepEqual(filterConns(list, { ...defaultFilter, query: "token=abc" }).map((x) => x.pid), [7]);
  assert.deepEqual(filterConns(list, { ...defaultFilter, query: "hk-001" }).map((x) => x.pid), [7]);
  assert.deepEqual(filterConns(list, { ...defaultFilter, protocol: "udp" }).map((x) => x.pid), [6]);
  assert.deepEqual(filterConns(list, { ...defaultFilter, activeOnly: true }).map((x) => x.pid), [6]);
  // 入站的 nginx 在只看出站时搜不到。
  assert.equal(filterConns(list, { ...defaultFilter, query: "nginx" }).length, 0);
});

test("远端类别：回环、私有网段（含 172.16/12 的边界、CGNAT）、节点网络、公网", () => {
  for (const [ip, kind] of [
    ["127.0.0.1", "loopback"], ["::1", "loopback"],
    ["10.1.2.3", "private"], ["172.16.0.1", "private"], ["172.31.255.255", "private"], ["192.168.0.13", "private"], ["100.64.0.1", "private"], ["fd00::1", "private"], ["fe80::1", "private"],
    ["172.32.0.1", "public"], ["172.15.0.1", "public"], ["100.128.0.1", "public"], ["8.8.8.8", "public"], ["2001:db8::1", "public"],
    ["fd4e:6167:1:2::3", "mesh"],
  ]) assert.equal(remoteKind(ip), kind, ip);
});

test("端口提示：出站看远端端口，入站看本地端口", () => {
  assert.equal(portHint(c({ remotePort: 3260 })), "iSCSI");
  assert.equal(portHint(c({ inbound: true, localPort: 22, remotePort: 51234 })), "SSH");
  assert.equal(portHint(c({ remotePort: 12345 })), "");
});

test("面板不给节点地址时：远端写节点名和端口，按远端分组按节点名，搜得到", () => {
  const list = [
    c({ remoteIp: "", remotePort: 7002, remoteNodeName: "HK-001", sendRate: 5 }),
    c({ remoteIp: "", remotePort: 7003, remoteNodeName: "HK-001" }),
    c({ remoteIp: "", remotePort: 7002, remoteNodeName: "其他节点" }),
  ];
  assert.equal(remoteLabel(list[0]), "HK-001:7002");
  assert.equal(remoteLabel(c({ remoteIp: "2001:db8::1", remotePort: 443 })), "[2001:db8::1]:443");
  assert.deepEqual(groupConns(list, "remote").map((g) => [g.label, g.conns.length]), [["HK-001", 2], ["其他节点", 1]]);
  assert.equal(filterConns(list, { ...defaultFilter, query: "hk-001:7003" }).length, 1);
});