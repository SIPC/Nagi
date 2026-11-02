import { useEffect, useRef, useState } from "react";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";
import { Button, Notice, Select, Spinner, Status } from "@/components/ui";
import { locale, t } from "@/i18n";

type Conn = { kind: "idle" | "connecting" | "open" | "ended" | "failed"; message?: string };

// 容器终端：通过 WebSocket 连到面板，面板转发到节点上的 docker exec。
// 顶部左边是连接状态，右边是连接和重新连接；连接前不显示黑框。
export function TerminalView({ projectId, container, services }: { projectId: string; container: string; services: string[] }) {
  const [service, setService] = useState("");
  const [session, setSession] = useState(0);
  const [conn, setConn] = useState<Conn>({ kind: "idle" });
  const boxRef = useRef<HTMLDivElement>(null);
  const selected = service || services[0] || "";

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
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.open(boxRef.current);
    fit.fit();
    const params = new URLSearchParams({
      project: projectId,
      container,
      service: selected,
      cols: String(term.cols),
      rows: String(term.rows),
      // WebSocket 不能设置请求头，界面语言放在参数里，面板按它返回错误信息。
      lang: locale,
    });
    const ws = new WebSocket(`${location.protocol === "https:" ? "wss" : "ws"}://${location.host}/ws/exec?${params}`);
    ws.binaryType = "arraybuffer";
    const enc = new TextEncoder();
    setConn({ kind: "connecting" });
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
    const input = term.onData((d) => ws.readyState === WebSocket.OPEN && ws.send(enc.encode(d)));
    const onResize = () => {
      fit.fit();
      if (ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type: "resize", cols: term.cols, rows: term.rows }));
    };
    window.addEventListener("resize", onResize);
    return () => {
      window.removeEventListener("resize", onResize);
      input.dispose();
      ws.onclose = null;
      ws.close();
      term.dispose();
    };
  }, [session, projectId, container, selected]);

  const reconnect = () => setSession((n) => n + 1);
  const picker = services.length > 1 && (
    <Select value={selected} onChange={(e) => setService(e.target.value)} className="w-40" aria-label={t.projectContainers.logs.allContainers}>
      {services.map((s) => (
        <option key={s} value={s}>
          {s}
        </option>
      ))}
    </Select>
  );

  return (
    <section className="grid gap-4">
      {conn.kind === "ended" || conn.kind === "failed" ? (
        <Notice
          tone={conn.kind === "failed" ? "danger" : "neutral"}
          actions={
            <>
              {picker}
              <Button variant="outline" onClick={reconnect} disabled={!selected}>
                {t.app.terminal.reconnect}
              </Button>
            </>
          }
        >
          <span className="font-medium">{conn.message}</span>
        </Notice>
      ) : (
        <header className="flex min-h-8 flex-wrap items-center justify-between gap-x-4 gap-y-2">
          {conn.kind === "open" ? (
            <Status tone="success">{t.projectContainers.terminal.connected}</Status>
          ) : conn.kind === "connecting" ? (
            <Status tone="neutral">{t.app.terminal.connecting}</Status>
          ) : (
            <span className="text-muted-foreground">{services.length ? t.app.terminal.intro : t.app.terminal.noRunning}</span>
          )}
          <div className="flex items-center gap-2">
            {picker}
            {conn.kind === "idle" ? (
              <Button variant={selected ? "default" : "outline"} onClick={reconnect} disabled={!selected}>
                {t.app.terminal.connect}
              </Button>
            ) : (
              <Button variant="ghost" className="-mr-3" onClick={reconnect} disabled={!selected || conn.kind === "connecting"}>
                {conn.kind === "connecting" && <Spinner />}
                {t.app.terminal.reconnect}
              </Button>
            )}
          </div>
        </header>
      )}
      {session > 0 && (
        <div className="h-[640px] overflow-hidden rounded-lg bg-log px-4 py-3 max-md:-mb-12 max-md:h-[calc(100dvh-19rem)]">
          <div ref={boxRef} className="h-full" />
        </div>
      )}
    </section>
  );
}
