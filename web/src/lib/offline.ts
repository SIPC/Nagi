// 面板所在机器挂掉时的提示（见 docs/standby.md#面板不可用时）。
//
// 平时把备用面板的名称和地址记在浏览器里。开着的页面连不上面板时盖一层提示（components/panel-down.tsx）；
// 用正式证书访问（绑定了域名）时还注册一个 Service Worker，新打开、刷新也连不上时显示 /offline.html。
// 只记备用面板的地址，不记项目、节点这些数据：里面有配置和密钥，而且面板不在时也是旧的。
// 安全入口的路径同步到了备用面板，但它要保密，不记。

export type KnownStandby = { name: string; address: string };
export type KnownStandbys = { standbys: KnownStandby[]; at: number };

// storageKey 和 public/offline.js 里的一致。
const storageKey = "nagi.standbys";

export function rememberStandbys(list: KnownStandby[]) {
  try {
    if (list.length === 0) localStorage.removeItem(storageKey);
    else localStorage.setItem(storageKey, JSON.stringify({ standbys: list, at: Date.now() } satisfies KnownStandbys));
  } catch {
    // 浏览器不让用存储时就不记。
  }
}

export function knownStandbys(): KnownStandbys | null {
  try {
    const v = JSON.parse(localStorage.getItem(storageKey) ?? "null") as KnownStandbys | null;
    return v && Array.isArray(v.standbys) && v.standbys.length > 0 ? v : null;
  } catch {
    return null;
  }
}

// forgetStandbys 在退出登录时调用：公用电脑上不留备用面板的地址。
export function forgetStandbys() {
  try {
    localStorage.removeItem(storageKey);
  } catch {
    // 同上。
  }
}

// standbyURL：备用面板的网页和节点连接用同一个地址和端口。
export const standbyURL = (address: string) => `https://${address}`;

// offlineFiles 是离线页用到的文件，和 public/sw.js 里的一致。
const offlineCache = "nagi-offline-v1";
const offlineFiles = ["/offline.html", "/offline.js", "/favicon.svg"];

// registerOfflinePage 注册 Service Worker。浏览器只在证书有效时允许注册：用 IP 和自签证书访问时注册会失败，
// 这时只有开着的页面的提示。每次打开面板顺便刷新离线页的缓存，改了离线页不用改 sw.js 也能生效。
export function registerOfflinePage() {
  if (import.meta.env.DEV || !window.isSecureContext || !("serviceWorker" in navigator)) return;
  navigator.serviceWorker
    .register("/sw.js", { scope: "/", updateViaCache: "none" })
    .then(() => caches.open(offlineCache))
    .then((cache) => cache.addAll(offlineFiles.map((f) => new Request(f, { cache: "no-cache" }))))
    .catch(() => {
      // 自签证书、隐私模式或者浏览器不支持：没有离线页。
    });
}
