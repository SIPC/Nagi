import { useEffect, useRef, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { errorMessage } from "@/lib/api";
import { Alert, LogBox, Notice } from "@/components/ui";
import { t } from "@/i18n";

// 执行一个会输出过程的操作（部署、回滚、快照……），显示输出和结果。

type RunResult = { success: boolean; error: string };
type StreamEvent = { event: { case: string | undefined; value?: unknown } };

export function useRun(projectId: string) {
  const queryClient = useQueryClient();
  const [lines, setLines] = useState<string[]>([]);
  const [result, setResult] = useState<RunResult>();
  const [busy, setBusy] = useState(false);
  const run = async (start: () => AsyncIterable<StreamEvent>, keep = false) => {
    setBusy(true);
    setResult(undefined);
    if (!keep) setLines([]);
    try {
      for await (const ev of start()) {
        if (ev.event.case === "output") setLines((l) => appendLine(l, ev.event.value as string));
        if (ev.event.case === "result") setResult(ev.event.value as RunResult);
      }
    } catch (err) {
      setResult({ success: false, error: errorMessage(err) });
    } finally {
      setBusy(false);
      for (const key of ["project", "deployments", "versions", "snapshots"]) queryClient.invalidateQueries({ queryKey: [key, projectId] });
      queryClient.invalidateQueries({ queryKey: ["projects"] });
    }
  };
  const output = <RunOutput lines={lines} result={result} />;
  // hasOutput：有没有要显示的输出，调用方据此决定要不要给输出留位置。
  const hasOutput = lines.length > 0 || result !== undefined;
  return { run, busy, output, hasOutput, setLines };
}

// 拉取镜像时每一层的进度（"1fdd0614a47e Downloading 11.5MB"）原地更新，不是每次都加一行。
const layerLine = /^\s*([0-9a-f]{12}) (Waiting|Pulling fs layer|Downloading|Download complete|Verifying Checksum|Extracting|Pull complete|Already exists)\b/;

function appendLine(lines: string[], text: string): string[] {
  // 一次输出可能有好几行。
  const parts = text.split(/\r?\n/);
  if (parts.length > 1) return parts.filter((l) => l !== "").reduce(appendLine, lines);
  const m = layerLine.exec(text);
  if (m) {
    for (let i = lines.length - 1; i >= 0 && i >= lines.length - 50; i--) {
      if (layerLine.exec(lines[i])?.[1] === m[1]) {
        const next = lines.slice();
        next[i] = text;
        return next;
      }
    }
  }
  return [...lines, text];
}

// 需要用户注意的输出（端口没开放、自动加了映射）：单独列在结果上面，免得淹没在日志里。
// 匹配的是面板和节点输出的文字，两种语言都要认得（节点的输出还没有翻译，始终是中文）。
// 改了面板上这几条的译文（internal/pkg/i18n/en.go）时要跟着改。
const noteLine = /未开放|已按配置里的端口添加映射|匿名卷|not exposed|Added a port mapping from the config|anonymous volume/i; // i18n-ignore
const warnLine = /未开放|匿名卷|not exposed|anonymous volume/i; // i18n-ignore

function RunOutput({ lines, result }: { lines: string[]; result?: RunResult }) {
  const outRef = useRef<HTMLPreElement>(null);
  useEffect(() => {
    outRef.current?.scrollTo({ top: outRef.current.scrollHeight });
  }, [lines.length]);
  if (lines.length === 0 && !result) return null;
  return (
    <div className="grid gap-2">
      {result && (result.success ? <Notice tone="success">{t.common.feedback.done}</Notice> : <Alert>{result.error || t.common.feedback.failed}</Alert>)}
      {lines
        .filter((l) => noteLine.test(l))
        .map((l, i) => (
          <Notice key={i} tone={warnLine.test(l) ? "warning" : undefined}>
            {l}
          </Notice>
        ))}
      {lines.length > 0 && (
        <LogBox ref={outRef} className="max-h-72">
          {lines.join("\n")}
        </LogBox>
      )}
    </div>
  );
}
