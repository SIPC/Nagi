// 面板的 Service Worker（见 docs/standby.md#面板不可用时）。只管页面导航，平时一律走网络；
// 连不上面板（网络错误、10 秒没有响应，或者前面的代理返回 502、503、504、52x）时显示缓存的 /offline.html，
// 上面列出浏览器里记下的备用面板。不缓存接口和其他数据。
// 离线页的文件由打开的页面每次刷新进缓存（src/lib/offline.ts），这里的列表和缓存名要和那边一致。
const cacheName = "nagi-offline-v1";
const files = ["/offline.html", "/offline.js", "/favicon.svg"];
const unavailable = new Set([502, 503, 504, 520, 521, 522, 523, 524]);
const timeoutMs = 10000;

self.addEventListener("install", (event) => {
  event.waitUntil(
    caches
      .open(cacheName)
      .then((cache) => cache.addAll(files))
      .then(() => self.skipWaiting()),
  );
});

self.addEventListener("activate", (event) => {
  event.waitUntil(
    (async () => {
      for (const key of await caches.keys()) if (key !== cacheName) await caches.delete(key);
      // 导航预加载：页面请求和 Service Worker 启动同时进行，平时不因为经过这里变慢。
      if (self.registration.navigationPreload) await self.registration.navigationPreload.enable();
      await self.clients.claim();
    })(),
  );
});

function withTimeout(promise) {
  return Promise.race([promise, new Promise((_, reject) => setTimeout(() => reject(new Error("timeout")), timeoutMs))]);
}

self.addEventListener("fetch", (event) => {
  const req = event.request;
  if (req.method !== "GET") return;
  const url = new URL(req.url);
  if (url.origin !== self.location.origin) return;
  // 离线页自己的脚本和图标：先走网络，连不上用缓存。
  if (req.mode !== "navigate") {
    if (url.pathname === "/offline.js" || url.pathname === "/favicon.svg") {
      event.respondWith(fetch(req).catch(() => caches.match(url.pathname).then((r) => r || Response.error())));
    }
    return;
  }
  event.respondWith(
    (async () => {
      try {
        const res = await withTimeout((async () => (await event.preloadResponse) || fetch(req))());
        if (!unavailable.has(res.status)) return res;
      } catch {
        // 连不上，往下显示离线页。
      }
      return (await caches.match("/offline.html")) || Response.error();
    })(),
  );
});
