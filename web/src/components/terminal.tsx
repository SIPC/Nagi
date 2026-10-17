import { useEffect, useRef, useState } from "react";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";
import { Alert, Button, Card, CardContent, CardHeader, CardTitle, Select } from "@/components/ui";

// 容器终端：通过 WebSocket 连到面板，面板转发到节点上的 docker exec。
export function TerminalCard({ nodeId, projectId, member, services }: { nodeId: string; projectId: string; member: string; services: string[] }) {
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
      node: nodeId,
      project: projectId,
      member,
      service: selected,
      cols: String(term.cols),
      rows: String(term.rows),
    });
    const ws = new WebSocket(`${location.protocol === "https:" ? "wss" : "ws"}://${location.host}/ws/exec?${params}`);
    ws.binaryType = "arraybuffer";
    const enc = new TextEncoder();
    setStatus("连接中…");
    ws.onopen = () => {
      setStatus("");
      term.focus();
    };
    ws.onmessage = (e) => {
      if (typeof e.data === "string") {
        const msg = JSON.parse(e.data);
        if (msg.type === "exit") setStatus(`进程已退出（退出码 ${msg.code}）`);
        if (msg.type === "error") setStatus(msg.message);
        return;
      }
      term.write(new Uint8Array(e.data));
    };
    ws.onclose = () => setStatus((s) => s || "连接已关闭");
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
  }, [session, nodeId, projectId, member, selected]);

  return (
    <Card>
      <CardHeader className="flex-row items-center justify-between gap-4">
        <CardTitle>终端</CardTitle>
        <div className="flex gap-2">
          <Select value={selected} onChange={(e) => setService(e.target.value)} className="w-40">
            {services.map((s) => (
              <option key={s} value={s}>
                {s}
              </option>
            ))}
          </Select>
          <Button size="sm" onClick={() => setSession((n) => n + 1)} disabled={!selected}>
            {session ? "重新连接" : "打开终端"}
          </Button>
        </div>
      </CardHeader>
      {session > 0 && (
        <CardContent className="grid gap-2">
          {status && <Alert className="border-border bg-muted text-muted-foreground">{status}</Alert>}
          <div ref={boxRef} className="h-96 overflow-hidden rounded-md bg-black p-2" />
        </CardContent>
      )}
    </Card>
  );
}