import { useEffect, useRef, useState } from "react";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";
import { SquareTerminal } from "lucide-react";
import { Button, Notice, Select } from "@/components/ui";
import { locale, t } from "@/i18n";

// 容器终端：通过 WebSocket 连到面板，面板转发到节点上的 docker exec。
export function TerminalView({ projectId, container, services }: { projectId: string; container: string; services: string[] }) {
  const [service, setService] = useState("");
  const [session, setSession] = useState(0);
  const [status, setStatus] = useState("");
  const boxRef = useRef<HTMLDivElement>(null);
  const selected = service || services[0] || "";

  useEffect(() => {
    if (!session || !boxRef.current) return;
    const term = new Terminal({ fontSize: 13, fontFamily: "JetBrains Mono, Menlo, Consolas, monospace", cursorBlink: true });
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
    setStatus(t.app.terminal.connecting);
    ws.onopen = () => {
      setStatus("");
      term.focus();
    };
    ws.onmessage = (e) => {
      if (typeof e.data === "string") {
        const msg = JSON.parse(e.data);
        if (msg.type === "exit") setStatus(t.app.terminal.exited(msg.code));
        if (msg.type === "error") setStatus(msg.message);
        return;
      }
      term.write(new Uint8Array(e.data));
    };
    ws.onclose = () => setStatus((s) => s || t.app.terminal.closed);
    const input = term.onData((d) => ws.readyState === WebSocket.OPEN && ws.send(enc.encode(d)));
    const onResize = () => {
      fit.fit();
      if (ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type: "resize", cols: term.cols, rows: term.rows }));
    };
    window.addEventListener("resize", onResize);
    return () => {
      window.removeEventListener("resize", onResize);
      input.dispose();
      ws.close();
      term.dispose();
    };
  }, [session, projectId, container, selected]);

  return (
    <div className="grid gap-3">
      <div className="flex items-center justify-between gap-3">
        <span className="text-[13px] text-muted-foreground">{services.length ? t.app.terminal.intro : t.app.terminal.noRunning}</span>
        <div className="flex gap-2">
          {services.length > 1 && (
            <Select value={selected} onChange={(e) => setService(e.target.value)} className="h-8 w-40 text-[13px]">
              {services.map((s) => (
                <option key={s} value={s}>
                  {s}
                </option>
              ))}
            </Select>
          )}
          <Button size="sm" variant="outline" onClick={() => setSession((n) => n + 1)} disabled={!selected}>
            <SquareTerminal />
            {session ? t.app.terminal.reconnect : t.app.terminal.connect}
          </Button>
        </div>
      </div>
      {session > 0 && (
        <>
          {status && <Notice>{status}</Notice>}
          <div ref={boxRef} className="h-96 overflow-hidden rounded-md bg-black p-2" />
        </>
      )}
    </div>
  );
}
