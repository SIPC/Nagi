// 回放终端录制（见 docs/audit.md#终端与命令录制）：只有管理员能看，要先做高权限确认，回放本身也记进审计。
// 用 asciinema-player 播放，支持倍速和拖动；上方的筛选框在输出里找文字，点结果跳到那个时间。
// 下方列出录到的输入（asciicast 的 "i" 事件），按行合并，点一行跳到那个时间。
import { useEffect, useMemo, useRef, useState } from "react";
import * as AsciinemaPlayer from "asciinema-player";
import "asciinema-player/dist/bundle/asciinema-player.css";
import { Dialog, EmptyRow, Input, List, QueryError, RowsSkeleton } from "@/components/ui";
import { errorMessage, settingsClient } from "@/lib/api";
import { requireElevation } from "@/components/elevate";
import { t } from "@/i18n";

type Hit = { time: number; text: string; hidden?: boolean };

// 去掉 ANSI 控制序列，只留文字，用来搜索。
const ansi = /\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07]*(\x07|\x1b\\)|\x1b[@-Z\\-_]/g;

function searchCast(cast: string, q: string): Hit[] {
  const needle = q.trim().toLowerCase();
  if (!needle) return [];
  const hits: Hit[] = [];
  for (const line of cast.split("\n")) {
    if (!line.startsWith("[")) continue;
    let ev: [number, string, string];
    try {
      ev = JSON.parse(line);
    } catch {
      continue;
    }
    if (ev[1] !== "o") continue;
    for (const text of ev[2].replace(ansi, "").split(/\r?\n/)) {
      if (text.toLowerCase().includes(needle)) {
        hits.push({ time: ev[0], text: text.trim() });
        if (hits.length >= 50) return hits;
      }
    }
  }
  return hits;
}

// idleLimit：回放时停顿最多算这么久（秒）。在这里先压缩录制里的时间，而不是交给播放器的 idleTimeLimit：
// 下面的查找结果和输入列表按压缩后的时间跳转，和进度条对得上。
const idleLimit = 2;

function compressIdle(cast: string): string {
  let last = 0;
  let shifted = 0;
  return cast
    .split("\n")
    .map((line) => {
      if (!line.startsWith("[")) return line;
      try {
        const ev = JSON.parse(line) as [number, string, string];
        const gap = ev[0] - last;
        last = ev[0];
        shifted += Math.min(Math.max(gap, 0), idleLimit);
        ev[0] = Math.round(shifted * 1e6) / 1e6;
        return JSON.stringify(ev);
      } catch {
        return line;
      }
    })
    .join("\n");
}

// hiddenMark 是面板记录没有回显的字符时用的占位（见 audit/recording_input.go）。
const hiddenMark = "•";

// inputLines 把输入事件按行合并：可见字符连成一行，回车结束这一行；其他控制键写成 [名称]；没有回显的字符单独一行。
function inputLines(cast: string): Hit[] {
  const lines: Hit[] = [];
  let cur: Hit | undefined;
  const end = () => {
    if (cur && cur.text.trim()) lines.push(cur);
    cur = undefined;
  };
  for (const line of cast.split("\n")) {
    if (!line.startsWith("[")) continue;
    let ev: [number, string, string];
    try {
      ev = JSON.parse(line);
    } catch {
      continue;
    }
    if (ev[1] !== "i") continue;
    const [time, , data] = ev;
    if (data.length > 0 && [...data].every((c) => c === hiddenMark)) {
      end();
      lines.push({ time, text: t.audit.replay.hidden([...data].length), hidden: true });
      continue;
    }
    if (data === "⏎") {
      end();
      continue;
    }
    cur ??= { time, text: "" };
    // 控制键的名称不超过 5 个字符（⌫、Esc、^C、PgUp），可见字符可能很长（粘贴）。
    const isKey = data.length <= 5 && (data === "⌫" || data === "⇥" || data === "Esc" || /^\^.$/.test(data) || /^[↑↓←→]$/.test(data) || ["Home", "End", "Del", "PgUp", "PgDn"].includes(data));
    cur.text += isKey ? `[${data}]` : data;
  }
  end();
  return lines;
}

function clock(s: number) {
  const m = Math.floor(s / 60);
  const sec = Math.floor(s % 60);
  return `${m}:${String(sec).padStart(2, "0")}`;
}

export function RecordingDialog({ id, open, onClose }: { id: string; open: boolean; onClose: () => void }) {
  const [cast, setCast] = useState<string>();
  const [error, setError] = useState<unknown>();
  // attempt：点「重试」时加一，重新下载录制。
  const [attempt, setAttempt] = useState(0);
  const [query, setQuery] = useState("");
  // 验证身份通过后才打开对话框：不让两个对话框叠在一起。
  const [allowed, setAllowed] = useState(false);
  const box = useRef<HTMLDivElement>(null);
  const player = useRef<AsciinemaPlayer.Player>(undefined);
  const m = t.audit.replay;

  useEffect(() => {
    if (!open) return;
    let cancelled = false;
    setCast(undefined);
    setError(undefined);
    setAllowed(false);
    (async () => {
      if (!(await requireElevation(m.elevate, { suggestTotp: false }))) {
        onClose();
        return;
      }
      if (!cancelled) setAllowed(true);
      try {
        const parts: string[] = [];
        const dec = new TextDecoder();
        for await (const c of settingsClient.getRecording({ id })) parts.push(dec.decode(c.data, { stream: true }));
        if (!cancelled) setCast(compressIdle(parts.join("")));
      } catch (err) {
        if (!cancelled) setError(err);
      }
    })();
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, id, attempt]);

  useEffect(() => {
    if (!cast || !box.current) return;
    const p = AsciinemaPlayer.create({ data: cast }, box.current, { fit: "both", speed: 1, controls: true, theme: "asciinema" });
    player.current = p;
    return () => {
      p.dispose();
      player.current = undefined;
    };
  }, [cast]);

  const hits = useMemo(() => (cast ? searchCast(cast, query) : []), [cast, query]);
  const inputs = useMemo(() => (cast ? inputLines(cast) : []), [cast]);
  const seek = (time: number) => player.current?.seek(Math.max(0, time - 0.5)).then(() => player.current?.play());

  return (
    <Dialog open={open && allowed} onClose={onClose} title={m.title} description={m.description} className="max-w-[960px]">
      <div className="grid gap-4">
        {error ? (
          <QueryError error={errorMessage(error)} onRetry={() => setAttempt((n) => n + 1)} />
        ) : !cast ? (
          <RowsSkeleton count={4} />
        ) : (
          <>
            <Input value={query} onChange={(e) => setQuery(e.target.value)} placeholder={m.search} aria-label={m.search} autoComplete="off" />
            {query.trim() !== "" && (
              <div className="-mx-2 max-h-40 overflow-x-hidden overflow-y-auto px-2">
                {hits.length === 0 ? (
                  <EmptyRow>{m.noMatch}</EmptyRow>
                ) : (
                  <List>
                    {hits.map((h, i) => (
                      <button
                        key={i}
                        type="button"
                        // 和列表行同样的尺寸：高 36、左右 8、列之间 16；整行是按钮，点了跳到那一刻。
                        className="flex min-h-9 cursor-pointer items-center gap-4 rounded-md border border-transparent px-2 text-left outline-none transition-colors hover:bg-accent focus-ring"
                        onClick={() => seek(h.time)}
                      >
                        <span className="w-12 shrink-0 font-mono text-[13px] text-muted-foreground tabular-nums">{clock(h.time)}</span>
                        <span className="min-w-0 truncate font-mono text-[13px]">{h.text}</span>
                      </button>
                    ))}
                  </List>
                )}
              </div>
            )}
            {/* 高度固定，播放器按它缩放，进度条和倍速始终在视野里。底色和终端相同，播放器比框窄时两边也是同一块，和上面的查找框同宽。 */}
            <div ref={box} className="nagi-player h-[60vh] max-h-[560px] overflow-hidden rounded-lg bg-log [&_.ap-wrapper]:h-full [&_.ap-player]:[--term-color-background:var(--log)]!" />
            {/* 旧的录制没有输入，不显示这一块。 */}
            {inputs.length > 0 && (
              <section className="grid gap-2" aria-label={m.inputs}>
                <h3 className="text-sm font-medium">{m.inputs}</h3>
                <div className="-mx-2 max-h-48 overflow-x-hidden overflow-y-auto px-2">
                  <List>
                    {inputs.map((h, i) => (
                      <button
                        key={i}
                        type="button"
                        className="flex min-h-9 cursor-pointer items-center gap-4 rounded-md border border-transparent px-2 text-left outline-none transition-colors hover:bg-accent focus-ring"
                        onClick={() => seek(h.time)}
                      >
                        <span className="w-12 shrink-0 font-mono text-[13px] text-muted-foreground tabular-nums">{clock(h.time)}</span>
                        <span className={h.hidden ? "min-w-0 truncate text-sm text-muted-foreground" : "min-w-0 truncate font-mono text-[13px]"}>{h.text}</span>
                      </button>
                    ))}
                  </List>
                </div>
              </section>
            )}
          </>
        )}
      </div>
    </Dialog>
  );
}
