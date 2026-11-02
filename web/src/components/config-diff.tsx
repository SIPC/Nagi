// 草稿和当前运行版本的对比：把配置展开成文本，按行比较。
import type { ContainerConfig } from "@/gen/nagi/v1/common_pb";
import { portsToText } from "@/lib/config";
import { t } from "@/i18n";

function configLines(c: ContainerConfig | undefined): string[] {
  if (!c) return [];
  const out: string[] = [];
  const add = (label: string, v: string | undefined) => {
    if (v) out.push(`${label}: ${v}`);
  };
  const f = c.form;
  if (f) {
    add("image", f.image);
    add("command", f.command);
    add("ports", portsToText(f.ports));
    add("workdir", f.workdir);
    for (const v of f.volumes) add("volume", v);
    for (const e of f.env) add("env", `${e.key}=${e.value}`);
    add("memory", f.memoryMb ? `${f.memoryMb} MB` : "");
    add("cpus", f.cpus ? String(f.cpus) : "");
    add("health", f.healthCommand);
    add("health-interval", f.healthIntervalSeconds ? `${f.healthIntervalSeconds}s` : "");
  } else {
    out.push(...(c.compose ?? "").split("\n").filter((l) => l.trim()));
  }
  for (const e of c.env) add("env", `${e.key}=${e.value}`);
  for (const [svc, list] of Object.entries(c.serviceEnv)) for (const e of list.vars) add(`env ${svc}`, `${e.key}=${e.value}`);
  add("git", c.source?.gitUrl);
  add("ref", c.source?.gitRef);
  for (const p of c.preserve) add("preserve", p);
  add("depends-on", c.dependsOn.join(", "));
  return out;
}

type Op = { kind: "same" | "add" | "del"; text: string };

// 最长公共子序列。配置只有几十行，直接算。
function diffLines(a: string[], b: string[]): Op[] {
  const m = a.length;
  const n = b.length;
  const dp = Array.from({ length: m + 1 }, () => new Array<number>(n + 1).fill(0));
  for (let i = m - 1; i >= 0; i--) for (let j = n - 1; j >= 0; j--) dp[i][j] = a[i] === b[j] ? dp[i + 1][j + 1] + 1 : Math.max(dp[i + 1][j], dp[i][j + 1]);
  const out: Op[] = [];
  let i = 0;
  let j = 0;
  while (i < m || j < n) {
    if (i < m && j < n && a[i] === b[j]) {
      out.push({ kind: "same", text: a[i] });
      i++;
      j++;
    } else if (i < m && (j >= n || dp[i + 1][j] >= dp[i][j + 1])) out.push({ kind: "del", text: a[i++] });
    else out.push({ kind: "add", text: b[j++] });
  }
  return out;
}

// 敏感变量的值不直接显示。
const maskLine = (text: string) => text.replace(/^(env(?: \S+)?: [^=]*(?:pass|pwd|secret|token|key|credential)[^=]*=).+$/i, "$1••••••");

// ConfigDiff：「相对 vN 的改动」区块的内容：只列有改动的行，新增绿色、删除红色。
export function ConfigDiff({ deployed, draft }: { deployed: ContainerConfig | undefined; draft: ContainerConfig | undefined }) {
  const ops = diffLines(configLines(deployed), configLines(draft)).filter((o) => o.kind !== "same");
  if (ops.length === 0) return <p className="text-muted-foreground">{t.projectContainers.config.diffNone}</p>;
  return (
    <div className="overflow-auto rounded-md bg-muted p-3 font-mono text-xs leading-5 whitespace-pre">
      {ops.map((o, i) => (
        <div key={i} className={o.kind === "add" ? "text-success" : "text-destructive"}>
          {`${o.kind === "add" ? "+" : "-"} ${maskLine(o.text)}`}
        </div>
      ))}
    </div>
  );
}
