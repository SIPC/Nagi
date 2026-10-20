import { useEffect, useRef, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { errorMessage } from "@/lib/api";
import { Alert, LogBox, Notice } from "@/components/ui";

// 执行一个会输出过程的操作（部署、回滚、快照……），显示输出和结果。

type RunResult = { success: boolean; error: string };
type StreamEvent = { event: { case: string | undefined; value?: unknown } };

export function useRun(nodeId: string, projectId: string) {
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
        if (ev.event.case === "output") setLines((l) => [...l, ev.event.value as string]);
        if (ev.event.case === "result") setResult(ev.event.value as RunResult);
      }
    } catch (err) {
      setResult({ success: false, error: errorMessage(err) });
    } finally {
      setBusy(false);
      for (const key of ["project", "deployments", "versions", "snapshots"]) queryClient.invalidateQueries({ queryKey: [key, nodeId, projectId] });
      queryClient.invalidateQueries({ queryKey: ["projects"] });
    }
  };
  const output = <RunOutput lines={lines} result={result} />;
  return { run, busy, output, setLines };
}

function RunOutput({ lines, result }: { lines: string[]; result?: RunResult }) {
  const outRef = useRef<HTMLPreElement>(null);
  useEffect(() => {
    outRef.current?.scrollTo({ top: outRef.current.scrollHeight });
  }, [lines.length]);
  if (lines.length === 0 && !result) return null;
  return (
    <div className="grid gap-2">
      {result && (result.success ? <Notice tone="success">已完成</Notice> : <Alert>{result.error || "操作失败"}</Alert>)}
      {lines.length > 0 && (
        <LogBox ref={outRef} className="max-h-72">
          {lines.join("\n")}
        </LogBox>
      )}
    </div>
  );
}
