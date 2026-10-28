// 把本目录下的配图 HTML 渲染成 docs/assets/<名字>.png（2 倍像素，尺寸取页面里 .frame 的实际大小）。
// 用法：node docs/assets/src/render.mjs [名字 ...]，不写名字就渲染全部。
// 需要本机装有 Edge 或 Chrome，可用 BROWSER 环境变量指定路径；字体用 MiSans，没有时退回系统中文字体。
import { spawn } from "node:child_process";
import { existsSync, mkdtempSync, readdirSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const out = resolve(here, "..");
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

const candidates = [
  process.env.BROWSER,
  "C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe",
  "C:/Program Files/Microsoft/Edge/Application/msedge.exe",
  "C:/Program Files/Google/Chrome/Application/chrome.exe",
  "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
  "/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
  "/usr/bin/google-chrome",
  "/usr/bin/chromium",
  "/usr/bin/microsoft-edge",
];
const browser = candidates.find((p) => p && existsSync(p));
if (!browser) {
  console.error("找不到 Edge 或 Chrome，请用 BROWSER 环境变量指定");
  process.exit(1);
}

const args = process.argv.slice(2);
const names = args.length
  ? args
  : readdirSync(here)
      .filter((f) => f.endsWith(".html"))
      .map((f) => f.slice(0, -".html".length));

const port = 9400 + Math.floor(Math.random() * 500);
const profile = mkdtempSync(join(tmpdir(), "nagi-render-"));
const proc = spawn(
  browser,
  ["--headless=new", `--remote-debugging-port=${port}`, `--user-data-dir=${profile}`, "--no-first-run", "--hide-scrollbars", "--disable-gpu", "about:blank"],
  { stdio: "ignore" },
);

let target;
for (let i = 0; i < 100 && !target; i++) {
  await sleep(200);
  try {
    const list = await (await fetch(`http://127.0.0.1:${port}/json`)).json();
    target = list.find((t) => t.type === "page");
  } catch {}
}
if (!target) {
  console.error("浏览器没有启动");
  proc.kill();
  process.exit(1);
}

const connect = (url) =>
  new Promise((resolveWs) => {
    const ws = new WebSocket(url);
    let id = 0;
    const pending = new Map();
    ws.addEventListener("message", (e) => {
      const msg = JSON.parse(e.data);
      if (msg.id && pending.has(msg.id)) {
        pending.get(msg.id)(msg);
        pending.delete(msg.id);
      }
    });
    const send = (method, params = {}) =>
      new Promise((r) => {
        const n = ++id;
        pending.set(n, r);
        ws.send(JSON.stringify({ id: n, method, params }));
      });
    ws.addEventListener("open", () => resolveWs({ ws, send }));
  });

const { ws, send } = await connect(target.webSocketDebuggerUrl);
const evaluate = async (expression) => (await send("Runtime.evaluate", { expression, returnByValue: true })).result?.result?.value;

let failed = false;
for (const name of names) {
  await send("Emulation.setDeviceMetricsOverride", { width: 1400, height: 1000, deviceScaleFactor: 2, mobile: false });
  await send("Page.navigate", { url: pathToFileURL(join(here, `${name}.html`)).href });
  await sleep(300);
  let ready = false;
  for (let i = 0; i < 100 && !ready; i++) {
    ready = await evaluate("document.readyState === 'complete' && window.__ready === true");
    if (!ready) await sleep(100);
  }
  if (!ready) {
    failed = true;
    console.error("failed", name, "页面没有画完（缺少 window.__ready）");
    continue;
  }
  const size = await evaluate(
    "(() => { const r = document.querySelector('.frame').getBoundingClientRect(); return { w: Math.ceil(r.width), h: Math.ceil(r.height) }; })()",
  );
  await send("Emulation.setDeviceMetricsOverride", { width: size.w, height: size.h, deviceScaleFactor: 2, mobile: false });
  await sleep(200);
  const shot = await send("Page.captureScreenshot", { format: "png", clip: { x: 0, y: 0, width: size.w, height: size.h, scale: 1 } });
  const png = join(out, `${name}.png`);
  writeFileSync(png, Buffer.from(shot.result.data, "base64"));
  console.log("saved", png, `${size.w}x${size.h}`);
}

ws.close();
try {
  const version = await (await fetch(`http://127.0.0.1:${port}/json/version`)).json();
  const b = await connect(version.webSocketDebuggerUrl);
  b.send("Browser.close");
  await sleep(500);
  b.ws.close();
} catch {}
proc.kill();
try {
  rmSync(profile, { recursive: true, force: true });
} catch {
  // 浏览器子进程可能还占着目录，留给系统清理
}
process.exit(failed ? 1 : 0);
