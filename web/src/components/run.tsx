import { useEffect, useRef, useState, type RefObject } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { errorMessage } from "@/lib/api";
import { useRole } from "@/components/role";
import { Alert, Button, LogBox, Notice } from "@/components/ui";
import { t } from "@/i18n";
import { toast } from "sonner";

// 执行一个会输出过程的操作（部署、回滚、快照……），显示输出和结果。

type RunResult = { success: boolean; error: string };
type StreamEvent = { event: { case: string | undefined; value?: unknown } };

export function useRun(projectId: string) {
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const { isAdmin } = useRole();
  const [lines, setLines] = useState<string[]>([]);
  const [result, setResult] = useState<RunResult>();
  const [busy, setBusy] = useState(false);
  const boxRef = useRef<HTMLDivElement>(null);
  const run = async (start: () => AsyncIterable<StreamEvent>, keep = false) => {
    setBusy(true);
    setResult(undefined);
    if (!keep) setLines([]);
    // 结果显示在输出区；输出区不在视野里时（例如在配置页底部点了部署）才另外弹 Toast。
    const finish = (r: RunResult) => {
      setResult(r);
      if (inView(boxRef.current)) return;
      if (r.success) toast.success(t.common.feedback.done);
      // 失败的详情记在活动里（只有管理员能看）。
      else toast.error(r.error || t.common.feedback.failed, isAdmin ? { action: { label: t.common.feedback.viewActivity, onClick: () => navigate({ to: "/activity" }) } } : undefined);
    };
    try {
      for await (const ev of start()) {
        if (ev.event.case === "output") setLines((l) => appendLine(l, ev.event.value as string));
        if (ev.event.case === "result") finish(ev.event.value as RunResult);
      }
    } catch (err) {
      finish({ success: false, error: errorMessage(err) });
    } finally {
      setBusy(false);
      const refresh = () => {
        for (const key of ["project", "deployments", "versions", "snapshots"]) queryClient.invalidateQueries({ queryKey: [key, projectId] });
        queryClient.invalidateQueries({ queryKey: ["projects"] });
      };
      refresh();
      // 节点攒一下事件才上报状态，过一会再取一次，免得显示「已完成」时状态还是旧的。
      setTimeout(refresh, 2000);
    }
  };
  const output = <RunOutput boxRef={boxRef} lines={lines} result={result} />;
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

function inView(el: HTMLElement | null) {
  if (!el) return false;
  const r = el.getBoundingClientRect();
  return r.height > 0 && r.bottom > 0 && r.top < window.innerHeight;
}

function RunOutput({ boxRef, lines, result }: { boxRef: RefObject<HTMLDivElement | null>; lines: string[]; result?: RunResult }) {
  const outRef = useRef<HTMLPreElement>(null);
  const [showNotes, setShowNotes] = useState(false);
  useEffect(() => {
    outRef.current?.scrollTo({ top: outRef.current.scrollHeight });
  }, [lines.length]);
  if (lines.length === 0 && !result) return null;
  const f = t.common.feedback;
  const notes = lines.filter((l) => noteLine.test(l));
  const warn = notes.some((l) => warnLine.test(l));
  // 同一处只放一条提示：失败时只写原因（需要注意的几行在下面的输出里）；
  // 否则结果作标题、需要注意的行作说明，多于一行时合并成「需要注意 N 项」，点开看。
  return (
    <div ref={boxRef} className="grid gap-2">
      {result && !result.success ? (
        <Alert>{result.error || f.failed}</Alert>
      ) : (
        (result || notes.length > 0) && (
          <Notice
            tone={warn ? "warning" : result ? "success" : "neutral"}
            title={result && notes.length > 0 ? f.done : undefined}
            actions={
              notes.length > 1 && (
                <Button variant="outline" readOnlyOk aria-expanded={showNotes} onClick={() => setShowNotes(!showNotes)}>
                  {showNotes ? f.hideNotes : f.showNotes}
                </Button>
              )
            }
          >
            {notes.length === 0 ? f.done : notes.length === 1 ? notes[0] : f.notes(notes.length)}
          </Notice>
        )
      )}
      {showNotes && notes.length > 1 && (
        <ul className="grid gap-1 pl-3.5 text-muted-foreground">
          {notes.map((l, i) => (
            <li key={i}>{l}</li>
          ))}
        </ul>
      )}
      {lines.length > 0 && (
        <LogBox ref={outRef} className="max-h-72">
          {lines.join("\n")}
        </LogBox>
      )}
    </div>
  );
}
