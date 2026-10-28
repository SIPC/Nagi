// 把本目录下的配图 HTML 渲染成 docs/assets/<名字>.png（2 倍像素，尺寸取页面里 .frame 的实际大小）。
// 很长的图会降低倍率，让成图高度不超过 16000 像素，否则不少看图软件（GPU 纹理上限 16384）会显示为空白。
// 用法：node docs/assets/src/render.mjs [名字 ...]，不写名字就渲染全部。
// 需要本机装有 Edge 或 Chrome，可用 BROWSER 环境变量指定路径；字体用 MiSans，没有时退回系统中文字体。
import { spawn } from "node:child_process";
import { existsSync, mkdtempSync, readdirSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { crc32, deflateSync, inflateSync } from "node:zlib";

const here = dirname(fileURLToPath(import.meta.url));
const out = resolve(here, "..");
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// 把几张同宽的 PNG（8 位、非隔行扫描，即浏览器截图的格式）上下拼成一张。
function stackPngs(buffers) {
  let width, colorType, bpp;
  const rows = [];
  for (const buf of buffers) {
    let pos = 8;
    const idat = [];
    let w, h;
    while (pos < buf.length) {
      const len = buf.readUInt32BE(pos);
      const type = buf.toString("latin1", pos + 4, pos + 8);
      const body = buf.subarray(pos + 8, pos + 8 + len);
      if (type === "IHDR") {
        w = body.readUInt32BE(0);
        h = body.readUInt32BE(4);
        if (body[8] !== 8 || body[12] !== 0) throw new Error("只支持 8 位、非隔行扫描的 PNG");
        colorType = body[9];
        bpp = { 0: 1, 2: 3, 4: 2, 6: 4 }[colorType];
        if (!bpp) throw new Error(`不支持的 PNG 颜色类型 ${colorType}`);
        if (width !== undefined && width !== w) throw new Error("分段宽度不一致");
        width = w;
      } else if (type === "IDAT") {
        idat.push(body);
      }
      pos += 12 + len;
    }
    // 还原每行的滤波，得到原始像素
    const raw = inflateSync(Buffer.concat(idat));
    const stride = w * bpp;
    let prev = Buffer.alloc(stride);
    for (let y = 0; y < h; y++) {
      const filter = raw[y * (stride + 1)];
      const line = Buffer.from(raw.subarray(y * (stride + 1) + 1, (y + 1) * (stride + 1)));
      for (let x = 0; x < stride; x++) {
        const a = x >= bpp ? line[x - bpp] : 0;
        const b = prev[x];
        const c = x >= bpp ? prev[x - bpp] : 0;
        let v = 0;
        if (filter === 1) v = a;
        else if (filter === 2) v = b;
        else if (filter === 3) v = (a + b) >> 1;
        else if (filter === 4) {
          const p = a + b - c;
          const pa = Math.abs(p - a), pb = Math.abs(p - b), pc = Math.abs(p - c);
          v = pa <= pb && pa <= pc ? a : pb <= pc ? b : c;
        }
        line[x] = (line[x] + v) & 0xff;
      }
      rows.push(line);
      prev = line;
    }
  }
  const raw = Buffer.alloc(rows.length * (width * bpp + 1));
  rows.forEach((line, y) => line.copy(raw, y * (width * bpp + 1) + 1));
  const chunk = (type, body) => {
    const head = Buffer.alloc(8);
    head.writeUInt32BE(body.length, 0);
    head.write(type, 4, "latin1");
    const crc = Buffer.alloc(4);
    crc.writeUInt32BE(crc32(Buffer.concat([head.subarray(4), body])), 0);
    return Buffer.concat([head, body, crc]);
  };
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(width, 0);
  ihdr.writeUInt32BE(rows.length, 4);
  ihdr[8] = 8;
  ihdr[9] = colorType;
  return Buffer.concat([
    Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
    chunk("IHDR", ihdr),
    chunk("IDAT", deflateSync(raw)),
    chunk("IEND", Buffer.alloc(0)),
  ]);
}

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
  // 浏览器单次截图的高度有上限（约 16384 设备像素），长图把内容逐段上移后分段截取，再拼成一张。
  const parts = [];
  const seg = Math.min(size.h, 4000);
  const scale = Math.min(2, Math.floor((16000 / size.h) * 100) / 100);
  await send("Emulation.setDeviceMetricsOverride", { width: size.w, height: seg, deviceScaleFactor: scale, mobile: false });
  for (let y = 0; y < size.h; y += seg) {
    const h = Math.min(seg, size.h - y);
    if (y > 0) await evaluate(`document.documentElement.style.overflow = "hidden"; document.body.style.marginTop = "${-y}px"`);
    await sleep(200);
    const shot = await send("Page.captureScreenshot", { format: "png", clip: { x: 0, y: 0, width: size.w, height: h, scale: 1 } });
    parts.push(Buffer.from(shot.result.data, "base64"));
  }
  const png = join(out, `${name}.png`);
  writeFileSync(png, parts.length > 1 ? stackPngs(parts) : parts[0]);
  console.log("saved", png, `${size.w}x${size.h}`, scale === 2 ? "" : `@${scale}x`);
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
