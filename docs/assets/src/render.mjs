// 把本目录下的宣传图 HTML 渲染成 docs/assets/<名字>.png（1280×640，2 倍像素）。
// 用法：node docs/assets/src/render.mjs [名字 ...]，不写名字就渲染全部。
// 需要本机装有 Edge 或 Chrome，可用 BROWSER 环境变量指定路径；字体用 MiSans，没有时退回系统中文字体。
import { spawnSync } from "node:child_process";
import { existsSync, mkdtempSync, readdirSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const out = resolve(here, "..");

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

const profile = mkdtempSync(join(tmpdir(), "nagi-render-"));
let failed = false;
for (const name of names) {
  const png = join(out, `${name}.png`);
  const r = spawnSync(
    browser,
    [
      "--headless=new",
      "--disable-gpu",
      "--hide-scrollbars",
      "--no-first-run",
      `--user-data-dir=${profile}`,
      "--force-device-scale-factor=2",
      "--window-size=1280,640",
      "--virtual-time-budget=3000",
      `--screenshot=${png}`,
      pathToFileURL(join(here, `${name}.html`)).href,
    ],
    { stdio: "ignore", timeout: 60_000 },
  );
  if (r.status === 0 && existsSync(png)) {
    console.log("saved", png);
  } else {
    failed = true;
    console.error("failed", name, r.error ?? r.status);
  }
}
try {
  rmSync(profile, { recursive: true, force: true });
} catch {
  // 浏览器子进程可能还占着目录，留给系统清理
}
process.exit(failed ? 1 : 0);
