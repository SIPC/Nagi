// 直连下载（见 docs/agent.md#直连下载）：先向面板要一个直连节点的签名链接，探测能连上就让浏览器直接从节点下载，
// 不经面板和节点网络；面板说节点不能直连、探测失败或超时时照旧经面板下载。
import { locale } from "@/i18n";

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
