// 草稿和当前运行版本的对比：把配置展开成文本，按行比较。
import { Fragment, useState } from "react";
import { ChevronRight } from "lucide-react";
import type { ContainerConfig } from "@/gen/nagi/v1/common_pb";
import { portsToText } from "@/lib/config";
import { cn } from "@/lib/utils";
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
    } else if (j < n && (i >= m || dp[i][j + 1] >= dp[i + 1][j])) out.push({ kind: "add", text: b[j++] });
    else out.push({ kind: "del", text: a[i++] });
  }
  return out;
}

// 敏感变量的值不直接显示。
const maskLine = (text: string) => text.replace(/^(env(?: \S+)?: [^=]*(?:pass|pwd|secret|token|key|credential)[^=]*=).+$/i, "$1••••••");

export function ConfigDiff({ deployed, draft }: { deployed: ContainerConfig | undefined; draft: ContainerConfig | undefined }) {
  const [open, setOpen] = useState(true);
  const ops = diffLines(configLines(deployed), configLines(draft));
  const changed = ops.filter((o) => o.kind !== "same").length;
  return (
    <div className="rounded-md border">
      <button type="button" className="flex w-full cursor-pointer items-center gap-1.5 px-3.5 py-2.5 text-left text-[13px] font-medium" aria-expanded={open} onClick={() => setOpen(!open)}>
        <ChevronRight className={cn("size-3.5 text-muted-foreground transition-transform", open && "rotate-90")} />
        {t.projectContainers.config.diffTitle}
        <span className="font-normal text-muted-foreground">{t.projectContainers.config.diffCount(changed)}</span>
      </button>
      {open && (
        <div className="overflow-x-auto border-t py-2 font-mono text-xs leading-relaxed">
          {changed === 0 ? (
            <p className="px-3.5 font-sans text-[13px] text-muted-foreground">{t.projectContainers.config.diffNone}</p>
          ) : (
            ops.map((o, i) => (
              <Fragment key={i}>
                {o.kind !== "same" && (
                  <div className={cn("px-3.5 whitespace-pre", o.kind === "add" ? "bg-success/10" : "bg-destructive/10")}>
                    <span className="mr-2 select-none text-muted-foreground">{o.kind === "add" ? "+" : "-"}</span>
                    {maskLine(o.text)}
                  </div>
                )}
              </Fragment>
            ))
          )}
        </div>
      )}
    </div>
  );
}
