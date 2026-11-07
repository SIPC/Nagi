// offline.html 的脚本：读浏览器里记下的备用面板（src/lib/offline.ts 写入）、语言和深浅色，
// 每 10 秒试一次面板，连上了就重新打开原来的页面。
(function () {
  const read = (key) => {
    try {
      return localStorage.getItem(key);
    } catch {
      return null;
    }
  };
  let known = null;
  try {
    known = JSON.parse(read("nagi.standbys") || "null");
  } catch {}
  const standbys = known && Array.isArray(known.standbys) ? known.standbys : [];

  const theme = read("theme");
  const dark = theme ? theme === "dark" : matchMedia("(prefers-color-scheme: dark)").matches;
  document.documentElement.classList.toggle("dark", dark);

  const lang = (read("lang") || navigator.language || "").toLowerCase().startsWith("en") ? "en" : "zh";
  document.documentElement.lang = lang === "en" ? "en" : "zh-CN";
  const m = {
    zh: {
      title: "此面板暂时无法访问",
      withStandby: "面板所在的机器可能已离线。可以转到备用面板继续查看。",
      noStandby: "面板所在的机器可能已离线，恢复后此页面会自动刷新。",
      copy: "复制",
      copied: "已复制",
      open: "打开",
      note: "备用面板只能查看，提升为主面板后才能修改。两个面板需分别登录。开启了安全入口时，地址需加上入口路径。",
      retry: "重试",
    },
    en: {
      title: "This panel is unavailable",
      withStandby: "The panel's machine may be offline. You can continue on a standby panel.",
      noStandby: "The panel's machine may be offline. This page reloads when it comes back.",
      copy: "Copy",
      copied: "Copied",
      open: "Open",
      note: "Standby panels are read-only until you promote one to primary. Sign in to each panel separately. If secure entry is turned on, add the entry path to the address.",
      retry: "Retry",
    },
  }[lang];

  const $ = (id) => document.getElementById(id);
  $("title").textContent = m.title;
  $("text").textContent = standbys.length > 0 ? m.withStandby : m.noStandby;
  $("retry").textContent = m.retry;
  $("retry").addEventListener("click", () => location.reload());

  if (standbys.length > 0) {
    const list = $("list");
    for (const [i, s] of standbys.entries()) {
      const url = "https://" + s.address;
      const li = document.createElement("li");
      const who = document.createElement("div");
      who.className = "who";
      const name = document.createElement("div");
      name.className = "name";
      name.textContent = s.name;
      const addr = document.createElement("div");
      addr.className = "addr";
      addr.textContent = url;
      who.append(name, addr);
      const copy = document.createElement("button");
      copy.type = "button";
      copy.textContent = m.copy;
      copy.addEventListener("click", async () => {
        try {
          await navigator.clipboard.writeText(url);
          copy.textContent = m.copied;
          setTimeout(() => (copy.textContent = m.copy), 1500);
        } catch {}
      });
      const open = document.createElement("a");
      // 只有第一个备用面板的「打开」是主按钮，和面板里的 PanelDown 一致。
      open.className = i === 0 ? "button primary" : "button";
      open.href = url;
      open.target = "_blank";
      open.rel = "noopener noreferrer";
      open.textContent = m.open;
      li.append(who, copy, open);
      list.append(li);
    }
    list.hidden = false;
    $("note").textContent = m.note;
    $("note").hidden = false;
  }

  // 面板回来了就重新打开原来的页面。只看能不能连上，代理报错以外的响应都算。
  // 用不经过离线缓存的公开接口（sw.js 会用缓存回答 /favicon.svg）。
  setInterval(() => {
    fetch("/api/standby/status", { cache: "no-store" })
      .then((r) => {
        if (![502, 503, 504, 520, 521, 522, 523, 524].includes(r.status)) location.reload();
      })
      .catch(() => {});
  }, 10000);
})();
