// 检查界面代码里有没有写死的中文：文案要放在 src/i18n 里。注释不算；确实需要中文的行（例如匹配节点输出的中文）
// 在行尾加 // i18n-ignore。用法：pnpm check:i18n
import { readdirSync, readFileSync, statSync } from "node:fs";
import { join, relative } from "node:path";
import { fileURLToPath } from "node:url";

const root = fileURLToPath(new URL("../src", import.meta.url));
const skip = new Set([join(root, "i18n"), join(root, "gen")]);
const han = /[\u3400-\u9fff\uff01-\uff5e\u3001-\u303f]/;

function* files(dir) {
  for (const name of readdirSync(dir)) {
    const p = join(dir, name);
    if (skip.has(p)) continue;
    if (statSync(p).isDirectory()) yield* files(p);
    else if (/\.(tsx?|jsx?)$/.test(name)) yield p;
  }
}

// 去掉注释（保留换行，行号不变）。字符串里的 // 也会被当成注释去掉，只会漏报，不会误报。
function stripComments(src) {
  return src
    .replace(/\/\*[\s\S]*?\*\//g, (m) => m.replace(/[^\n]/g, ""))
    .replace(/(^|[^:"'`\\])\/\/.*$/gm, "$1");
}

let found = 0;
for (const file of files(root)) {
  const raw = readFileSync(file, "utf8").split("\n");
  const lines = stripComments(raw.join("\n")).split("\n");
  lines.forEach((line, i) => {
    if (han.test(line) && !raw[i].includes("i18n-ignore")) {
      found++;
      console.log(`${relative(root, file)}:${i + 1}: ${raw[i].trim()}`);
    }
  });
}
if (found) {
  console.error(`\n${found} 行有写死的中文，请移到 src/i18n。`);
  process.exit(1);
}
console.log("没有写死的中文。");
