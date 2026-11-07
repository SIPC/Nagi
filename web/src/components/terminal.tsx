import { useEffect, useRef, useState } from "react";
import type * as React from "react";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";
import { Button, Notice, Select, Spinner, Status } from "@/components/ui";
import { locale, t } from "@/i18n";
import { cn } from "@/lib/utils";

type Conn = { kind: "idle" | "connecting" | "open" | "ended" | "failed"; message?: string };

// 手机上打不出来的按键。
const mobileKeys: { label: string; seq: string }[] = [
  { label: "Esc", seq: "\x1b" },
  { label: "Tab", seq: "\t" },
  { label: "←", seq: "\x1b[D" },
  { label: "↑", seq: "\x1b[A" },
  { label: "↓", seq: "\x1b[B" },
  { label: "→", seq: "\x1b[C" },
];

// ctrlOf 把字符换成 Ctrl 组合键：a → \x01。不是字母时原样返回。
function ctrlOf(d: string) {
  const code = d.toUpperCase().charCodeAt(0);
  return d.length === 1 && code >= 64 && code <= 95 ? String.fromCharCode(code - 64) : d;
}

type SessionProps = {
  // url 返回 WebSocket 的路径和参数（不含 cols、rows、lang）。
  url: () => string;
  // deps 变了就重新连接（例如换了服务）。
  deps?: unknown[];
  // beforeConnect 在点连接时调用，返回 false 就不连了。
  beforeConnect?: () => Promise<boolean>;
  intro: React.ReactNode;
  canConnect?: boolean;
  picker?: React.ReactNode;
  // label 一直显示在连接状态旁边，防止敲错机器。
  label?: React.ReactNode;
  // onSession 在每次连接时调用。
  onSession?: () => void;
};

// TerminalSession：通过 WebSocket 连到面板，面板转发到节点。消息格式见 internal/server/api/terminal.go。
// 顶部左边是连接状态，右边是连接和重新连接；连接前不显示黑框。
export function TerminalSession({ url, deps = [], beforeConnect, intro, canConnect = true, picker, label, onSession }: SessionProps) {
  const [session, setSession] = useState(0);
  const [conn, setConn] = useState<Conn>({ kind: "idle" });
  const [ctrl, setCtrl] = useState(false);
  const ctrlRef = useRef(false);
  const boxRef = useRef<HTMLDivElement>(null);
  const termRef = useRef<Terminal | null>(null);
  const sendRef = useRef<(d: string) => void>(() => {});

  const setCtrlOn = (on: boolean) => {
    ctrlRef.current = on;
    setCtrl(on);
  };

  useEffect(() => {
    if (!session || !boxRef.current) return;
    const styles = getComputedStyle(document.documentElement);
    const term = new Terminal({
      fontSize: 13,
      lineHeight: 1.5,
      fontFamily: 'ui-monospace, "SFMono-Regular", Menlo, Consolas, monospace',
      cursorBlink: true,
      theme: { background: styles.getPropertyValue("--log").trim(), foreground: styles.getPropertyValue("--log-foreground").trim() },
    });
    termRef.current = term;
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.open(boxRef.current);
    fit.fit();
    const target = new URL(url(), location.origin);
    target.protocol = location.protocol === "https:" ? "wss:" : "ws:";
    target.searchParams.set("cols", String(term.cols));
    target.searchParams.set("rows", String(term.rows));
    // WebSocket 不能设置请求头，界面语言放在参数里，面板按它返回错误信息。
    target.searchParams.set("lang", locale);
    const ws = new WebSocket(target);
    ws.binaryType = "arraybuffer";
    const enc = new TextEncoder();
    const send = (d: string) => {
      if (ws.readyState === WebSocket.OPEN) ws.send(enc.encode(d));
    };
    sendRef.current = send;
    setConn({ kind: "connecting" });
    onSession?.();
    ws.onopen = () => {
      setConn({ kind: "open" });
      term.focus();
    };
    ws.onmessage = (e) => {
      if (typeof e.data === "string") {
        const msg = JSON.parse(e.data);
        if (msg.type === "exit") setConn({ kind: "ended", message: t.app.terminal.exited(msg.code) });
        if (msg.type === "error") setConn({ kind: "failed", message: msg.message });
        return;
      }
      term.write(new Uint8Array(e.data));
    };
    ws.onclose = () => setConn((c) => (c.kind === "ended" || c.kind === "failed" ? c : { kind: "failed", message: t.app.terminal.closed }));
    const input = term.onData((d) => {
      if (ctrlRef.current) {
        setCtrlOn(false);
        send(ctrlOf(d));
        return;
      }
      send(d);
    });
    // 窗口大小变了、从隐藏的标签切回来时重新计算。隐藏时宽度是 0，不用算。
    const onResize = () => {
      if (!boxRef.current?.clientWidth) return;
      fit.fit();
      if (ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type: "resize", cols: term.cols, rows: term.rows }));
    };
    const observer = new ResizeObserver(onResize);
    observer.observe(boxRef.current);
    return () => {
      observer.disconnect();
      input.dispose();
      ws.onclose = null;
      ws.close();
      term.dispose();
      termRef.current = null;
      sendRef.current = () => {};
    };
    // url 依赖的值都在 deps 里。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [session, ...deps]);

  const connect = async () => {
    if (beforeConnect && !(await beforeConnect())) return;
    setSession((n) => n + 1);
  };

  return (
    <section className="grid gap-4">
      {conn.kind === "ended" || conn.kind === "failed" ? (
        <Notice
          tone={conn.kind === "failed" ? "danger" : "neutral"}
          actions={
            <>
              {picker}
              <Button variant="outline" onClick={connect} disabled={!canConnect}>
                {t.app.terminal.reconnect}
              </Button>
            </>
          }
        >
          <span className="font-medium">{conn.message}</span>
        </Notice>
      ) : (
        <header className="flex min-h-8 flex-wrap items-center justify-between gap-x-4 gap-y-2">
          <div className="flex min-w-0 items-center gap-3">
            {conn.kind === "open" ? (
              <Status tone="success">{t.projectContainers.terminal.connected}</Status>
            ) : conn.kind === "connecting" ? (
              <Status tone="neutral">{t.app.terminal.connecting}</Status>
            ) : (
              <span className="text-muted-foreground">{canConnect ? intro : t.app.terminal.noRunning}</span>
            )}
            {label && conn.kind !== "idle" && <span className="truncate font-mono text-[13px] text-muted-foreground">{label}</span>}
          </div>
          <div className="flex items-center gap-2">
            {picker}
            {conn.kind === "idle" ? (
              <Button variant={canConnect ? "default" : "outline"} onClick={connect} disabled={!canConnect}>
                {t.app.terminal.connect}
              </Button>
            ) : (
              <Button variant="ghost" className="-mr-3" onClick={connect} disabled={!canConnect || conn.kind === "connecting"}>
                {conn.kind === "connecting" && <Spinner />}
                {t.app.terminal.reconnect}
              </Button>
            )}
          </div>
        </header>
      )}
      {session > 0 && (
        <>
          {conn.kind === "open" && (
            <div className="grid grid-cols-7 gap-2 md:hidden">
              {mobileKeys.map((k) => (
                <Button
                  key={k.label}
                  variant="outline"
                  readOnlyOk
                  className="min-w-0 px-0"
                  // 按下时不抢走终端的焦点，否则手机键盘会收起来。
                  onPointerDown={(e) => e.preventDefault()}
                  onClick={() => {
                    sendRef.current(k.seq);
                    termRef.current?.focus();
                  }}
                >
                  {k.label}
                </Button>
              ))}
              <Button
                variant="outline"
                readOnlyOk
                aria-pressed={ctrl}
                className={cn("min-w-0 px-0", ctrl && "bg-accent")}
                onPointerDown={(e) => e.preventDefault()}
                onClick={() => {
                  setCtrlOn(!ctrl);
                  termRef.current?.focus();
                }}
              >
                Ctrl
              </Button>
            </div>
          )}
          <div className="h-[640px] overflow-hidden rounded-lg bg-log px-4 py-3 max-md:-mb-12 max-md:h-[max(16rem,calc(100dvh-26rem))]">
            <div ref={boxRef} className="h-full" />
          </div>
        </>
      )}
    </section>
  );
}

// 容器终端：面板转发到节点上的 docker exec。
export function TerminalView({ projectId, container, services }: { projectId: string; container: string; services: string[] }) {
  const [service, setService] = useState("");
  const selected = service || services[0] || "";
  const picker = services.length > 1 && (
    <Select value={selected} onChange={(e) => setService(e.target.value)} className="w-40" aria-label={t.projectContainers.logs.service}>
      {services.map((s) => (
        <option key={s} value={s}>
          {s}
        </option>
      ))}
    </Select>
  );
  return (
    <TerminalSession
      url={() => `/ws/exec?${new URLSearchParams({ project: projectId, container, service: selected })}`}
      deps={[projectId, container, selected]}
      intro={t.app.terminal.intro}
      canConnect={!!selected}
      picker={picker}
    />
  );
}
