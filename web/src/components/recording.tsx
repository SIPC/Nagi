// 回放终端录制（见 docs/audit.md#终端与命令录制）：只有管理员能看，要先做高权限确认，回放本身也记进审计。
// 用 asciinema-player 播放，支持倍速和拖动；上方的筛选框在输出里找文字，点结果跳到那个时间。
import { useEffect, useMemo, useRef, useState } from "react";
import * as AsciinemaPlayer from "asciinema-player";
import "asciinema-player/dist/bundle/asciinema-player.css";
import { Dialog, EmptyRow, Input, List, QueryError, RowsSkeleton } from "@/components/ui";
import { errorMessage, settingsClient } from "@/lib/api";
import { requireElevation } from "@/components/elevate";
import { t } from "@/i18n";

type Hit = { time: number; text: string };

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
        if (!cancelled) setCast(parts.join(""));
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
    const p = AsciinemaPlayer.create({ data: cast }, box.current, { fit: "both", idleTimeLimit: 2, speed: 1, controls: true, theme: "asciinema" });
    player.current = p;
    return () => {
      p.dispose();
      player.current = undefined;
    };
  }, [cast]);

  const hits = useMemo(() => (cast ? searchCast(cast, query) : []), [cast, query]);

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
              <div className="max-h-40 overflow-y-auto">
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
                        onClick={() => player.current?.seek(Math.max(0, h.time - 0.5)).then(() => player.current?.play())}
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
            <div ref={box} className="h-[60vh] max-h-[560px] overflow-hidden rounded-lg bg-log [&_.ap-wrapper]:h-full [&_.ap-player]:[--term-color-background:var(--log)]!" />
          </>
        )}
      </div>
    </Dialog>
  );
}
