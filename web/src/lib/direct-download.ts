// 直连下载（见 docs/agent.md#直连下载）：先向面板要一个直连节点的签名链接，探测能连上就让浏览器直接从节点下载，
// 不经面板和节点网络；面板说节点不能直连、探测失败或超时时照旧经面板下载。
import type { DirectDownload } from "@/gen/nagi/v1/common_pb";
import { locale, t } from "@/i18n";

// 探测最多等这么久：连不上的节点（防火墙、浏览器没有 IPv6）不要让人干等。
const probeTimeout = 2500;
// 探测失败的节点这么久内不再试，直接经面板。
const failedFor = 10 * 60_000;
const failed = new Map<string, number>();

// directLink 返回能用的直连链接，不能直连时返回 null。endpoint 是面板签发链接的地址（/api/volume-file/direct?…）。
export async function directLink(endpoint: string): Promise<string | null> {
  try {
    const res = await fetch(endpoint, { headers: { "Accept-Language": locale } });
    if (res.status !== 200) return null;
    const url = (await res.json().catch(() => null))?.url;
    if (typeof url !== "string" || !url.startsWith("https://")) return null;
    const host = new URL(url).host;
    if (Date.now() - (failed.get(host) ?? 0) < failedFor) return null;
    const ctl = new AbortController();
    const timer = setTimeout(() => ctl.abort(), probeTimeout);
    try {
      // 跨域的 HEAD：节点核对链接有效就回 204，不读文件。证书不受信任、连不上时这里抛错。
      const probe = await fetch(url, { method: "HEAD", mode: "cors", cache: "no-store", credentials: "omit", referrerPolicy: "no-referrer", signal: ctl.signal });
      if (probe.ok) return url;
    } catch {
      // 连不上，经面板下载
    } finally {
      clearTimeout(timer);
    }
    failed.set(host, Date.now());
    return null;
  } catch {
    return null;
  }
}

// saveFrom 让浏览器下载这个地址（响应是附件，不会离开当前页面）。
export function saveFrom(url: string) {
  const a = document.createElement("a");
  a.href = url;
  a.rel = "noopener noreferrer";
  a.referrerPolicy = "no-referrer";
  a.download = "";
  a.click();
}

// directEndpoint 把经面板下载的地址（/api/volume-file?…、/api/node-file?…）换成签发直连链接的地址。
export function directEndpoint(panelURL: string): string {
  const [path, query] = panelURL.split("?");
  return `${path}/direct${query ? `?${query}` : ""}`;
}

// directActions：节点现在能复制哪种下载方式。link：HTTPS 直连链接（节点拿到了 IP 证书）；command：加密 HTTP 下载的命令
// （节点开启了，有公网地址，见 docs/agent.md#加密-http-下载）。旧版本的节点不上报，两种都没有。
export function directActions(d: DirectDownload | undefined): { link: boolean; command: boolean } {
  return { link: !!d?.ready && !d.off && !!d.address, command: !!d?.httpReady && !!d.address };
}

async function failure(res: Response): Promise<Error> {
  return new Error((await res.json().catch(() => null))?.message || t.common.errors.http(String(res.status)));
}

// fetchDirectLink 向面板要一个 HTTPS 直连链接（不探测，给别的设备或下载工具用）。节点不能直连时抛错。
export async function fetchDirectLink(endpoint: string, fail = failure): Promise<string> {
  const res = await fetch(endpoint, { headers: { "Accept-Language": locale } });
  if (res.status === 204) throw new Error(t.fileManager.link.unavailable);
  if (!res.ok) throw await fail(res);
  const url = (await res.json().catch(() => null))?.url;
  if (typeof url !== "string" || !url.startsWith("https://")) throw new Error(t.fileManager.link.unavailable);
  return url;
}

// DirectCommand 是加密 HTTP 下载的命令。command 里有内容密钥，只显示，不存。
export type DirectCommand = { command: string; name: string; size: number; archive: boolean; sha256: string; hash_skipped: string; expires: number };

// fetchDirectCommand 向面板要加密 HTTP 下载的命令（节点要先算 SHA-256，最多半分钟）。
export async function fetchDirectCommand(endpoint: string, fail = failure, signal?: AbortSignal): Promise<DirectCommand> {
  const sep = endpoint.includes("?") ? "&" : "?";
  const res = await fetch(`${endpoint}${sep}mode=command`, { headers: { "Accept-Language": locale }, signal });
  if (!res.ok) throw await fail(res);
  const body = await res.json().catch(() => null);
  if (typeof body?.command !== "string") throw new Error(t.common.errors.http(String(res.status)));
  return body as DirectCommand;
}
