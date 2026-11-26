// 节点「网络连接」的分组和筛选（见 docs/agent.md#网络连接）。界面在 pages/node-connections.tsx。
// 不依赖界面文案：分组的名称由调用方按 kind 翻译。

// Conn 是界面要用的连接字段（NodeConnection 的子集，测试里直接写对象）。
export type Conn = {
  protocol: string;
  state: string;
  localIp: string;
  localPort: number;
  remoteIp: string;
  remotePort: number;
  inbound: boolean;
  bytesSent: bigint;
  bytesReceived: bigint;
  sendRate: number;
  receiveRate: number;
  pid: number;
  process: string;
  cmdline: string;
  dockerName: string;
  projectId: string;
  projectName: string;
  container: string;
  service: string;
  unit: string;
  remoteNodeName: string;
};

export type GroupBy = "process" | "container" | "remote" | "none";
export const groupBys: GroupBy[] = ["process", "container", "remote", "none"];

// Owner 是连接属于谁：容器、systemd 单元、本机进程，或者没有进程（内核或已关闭的连接）。
export type Owner = { kind: "container" | "unit" | "host" | "kernel"; label: string };

export function ownerOf(c: Conn): Owner {
  if (c.container) return { kind: "container", label: c.service && c.service !== c.container ? `${c.projectName || c.projectId} / ${c.container} / ${c.service}` : `${c.projectName || c.projectId} / ${c.container}` };
  if (c.dockerName) return { kind: "container", label: c.dockerName };
  if (c.unit) return { kind: "unit", label: c.unit };
  if (c.pid) return { kind: "host", label: "" };
  return { kind: "kernel", label: "" };
}

// Group 是一组连接：key 唯一，kind 和 label 由界面显示（kind 为 kernel、host 时 label 为空，界面写「内核」「本机」）。
export type Group = { key: string; kind: Owner["kind"] | "process" | "remote"; label: string; conns: Conn[]; sendRate: number; receiveRate: number };

export function groupConns(list: Conn[], by: GroupBy): Group[] {
  if (by === "none") return [];
  const groups = new Map<string, Group>();
  for (const c of list) {
    let key: string;
    let kind: Group["kind"];
    let label: string;
    if (by === "process") {
      // 同名进程（多个 worker）合成一组；没有进程的连接一组。
      kind = c.pid ? "process" : "kernel";
      label = c.pid ? c.process : "";
      key = `${kind}:${label}`;
    } else if (by === "container") {
      const o = ownerOf(c);
      kind = o.kind;
      label = o.label;
      key = `${kind}:${label}`;
    } else {
      kind = "remote";
      // 没有地址（面板不给）时按节点名分组。
      label = c.remoteIp || c.remoteNodeName;
      key = `remote:${label}`;
    }
    let g = groups.get(key);
    if (!g) {
      g = { key, kind, label, conns: [], sendRate: 0, receiveRate: 0 };
      groups.set(key, g);
    }
    g.conns.push(c);
    g.sendRate += c.sendRate;
    g.receiveRate += c.receiveRate;
  }
  // 按收发速率合计从高到低；一样时连接多的在前。
  return [...groups.values()].sort((a, b) => b.sendRate + b.receiveRate - (a.sendRate + a.receiveRate) || b.conns.length - a.conns.length || a.label.localeCompare(b.label));
}

export type Filter = { query: string; direction: "out" | "all"; protocol: "" | "tcp" | "udp"; activeOnly: boolean };

export const defaultFilter: Filter = { query: "", direction: "out", protocol: "", activeOnly: false };

// filterConns：搜索匹配远端、端口、进程名、容器、命令行（有的话）；默认只看出站。
export function filterConns(list: Conn[], f: Filter): Conn[] {
  const q = f.query.trim().toLowerCase();
  return list.filter((c) => {
    if (f.direction === "out" && c.inbound) return false;
    if (f.protocol && c.protocol !== f.protocol) return false;
    if (f.activeOnly && c.sendRate + c.receiveRate <= 0) return false;
    if (!q) return true;
    return [remoteLabel(c), String(c.localPort), c.process, c.cmdline, c.dockerName, c.projectName, c.container, c.service, c.unit, c.remoteNodeName, String(c.pid || "")].some((v) => v && v.toLowerCase().includes(q));
  });
}

// RemoteKind 是远端地址的类别，不用外部服务：本机回环、内网（私有、链路本地、Docker 网段）、节点网络（mesh）、公网。
export type RemoteKind = "loopback" | "private" | "mesh" | "public";

export function remoteKind(ip: string): RemoteKind {
  const v4 = ip.match(/^(\d+)\.(\d+)\.(\d+)\.(\d+)$/);
  if (v4) {
    const [a, b] = [Number(v4[1]), Number(v4[2])];
    if (a === 127) return "loopback";
    if (a === 10 || (a === 172 && b >= 16 && b < 32) || (a === 192 && b === 168) || (a === 169 && b === 254) || (a === 100 && b >= 64 && b < 128)) return "private";
    return "public";
  }
  const s = ip.toLowerCase();
  if (s === "::1") return "loopback";
  if (s.startsWith("fd4e:6167:")) return "mesh";
  if (/^f[cd][0-9a-f]{2}:/.test(s) || /^fe[89ab][0-9a-f]:/.test(s)) return "private";
  return "public";
}

// 常见的端口：只用来在远端后面加个提示（iSCSI 这类内核发起的连接没有进程，看端口才知道是什么）。
const wellKnown: Record<number, string> = { 22: "SSH", 53: "DNS", 80: "HTTP", 443: "HTTPS", 445: "SMB", 2049: "NFS", 3260: "iSCSI", 3306: "MySQL", 5432: "PostgreSQL", 6379: "Redis", 27017: "MongoDB", 51820: "WireGuard" };

export function portHint(c: Conn): string {
  return wellKnown[c.inbound ? c.localPort : c.remotePort] ?? "";
}

export function hostPort(ip: string, port: number) {
  return ip.includes(":") ? `[${ip}]:${port}` : `${ip}:${port}`;
}

// remoteLabel 是远端的显示：Nagi 节点、面板的地址没有「查看访问地址」时面板不给（看不到的节点一律不给），
// 这时写节点名和端口。
export function remoteLabel(c: Conn) {
  return c.remoteIp ? hostPort(c.remoteIp, c.remotePort) : `${c.remoteNodeName}:${c.remotePort}`;
}
