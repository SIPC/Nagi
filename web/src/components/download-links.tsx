// 复制下载链接、下载命令（见 docs/agent.md#直连下载、#加密-http-下载）：文件菜单里的两项和它们的对话框，卷文件和节点文件共用。
// 下载链接是 HTTPS 的直连链接，给下载工具或别的设备用；HTTPS 不能用（节点拿不到 IP 证书等）时，改用加密 HTTP 下载，给一条命令：
// curl 经 HTTP 下载密文，openssl 在本地解密。浏览器拦截 HTTPS 页面上的 HTTP 下载，所以命令只能复制。
import { useRef, useState } from "react";
import { toast } from "sonner";
import { Alert, Button, CodeBlock, Dialog, DialogActions, Spinner } from "@/components/ui";
import type { DirectDownload } from "@/gen/nagi/v1/common_pb";
import { t } from "@/i18n";
import { errorMessage } from "@/lib/api";
import { type DirectCommand, directActions, directEndpoint, fetchDirectCommand, fetchDirectLink } from "@/lib/direct-download";

type Options = {
  // direct：节点上报的直连下载状态；旧版本的节点没有，两项都不出现。
  direct: DirectDownload | undefined;
  // url：经面板下载的地址（/api/volume-file?…、/api/node-file?…）。
  url: (path: string) => string;
  // check：先确认能下载（和下载一样），不能时抛错。
  check: (path: string) => Promise<void>;
  // fail：把面板的错误响应变成错误（节点文件要处理身份验证过期）。
  fail?: (res: Response) => Promise<Error>;
  onError: (message: string) => void;
};

type CommandState = { path: string; data?: DirectCommand; error?: string };

export function useDownloadLinks({ direct, url, check, fail, onError }: Options) {
  const m = t.fileManager.link;
  const can = directActions(direct);
  const [link, setLink] = useState<string | null>(null);
  const [cmd, setCmd] = useState<CommandState | null>(null);
  const abort = useRef<AbortController | null>(null);

  const copyLink = async (path: string) => {
    let value: string;
    try {
      await check(path);
      value = await fetchDirectLink(directEndpoint(url(path)), fail);
    } catch (err) {
      onError(errorMessage(err));
      return;
    }
    try {
      await navigator.clipboard.writeText(value);
      toast.success(m.copied);
    } catch {
      // 等面板签发后浏览器可能不让写剪贴板（Safari）：显示出来，手动复制。
      setLink(value);
    }
  };

  const openCommand = async (path: string) => {
    abort.current?.abort();
    const ctl = new AbortController();
    abort.current = ctl;
    setCmd({ path });
    try {
      await check(path);
      const data = await fetchDirectCommand(directEndpoint(url(path)), fail, ctl.signal);
      if (!ctl.signal.aborted) setCmd({ path, data });
    } catch (err) {
      if (!ctl.signal.aborted) setCmd({ path, error: errorMessage(err) });
    }
  };
  const closeCommand = () => {
    abort.current?.abort();
    setCmd(null);
  };

  const items = (path: string) => [
    ...(can.link ? [{ label: m.copyLink, onSelect: () => void copyLink(path) }] : []),
    ...(can.command ? [{ label: m.copyCommand, onSelect: () => void openCommand(path) }] : []),
  ];

  const dialogs = (
    <>
      <Dialog open={link !== null} onClose={() => setLink(null)} title={m.linkTitle} description={m.linkHelp}>
        <CodeBlock copy>{link ?? ""}</CodeBlock>
        <DialogActions>
          <Button onClick={() => setLink(null)}>{t.common.actions.done}</Button>
        </DialogActions>
      </Dialog>
      <Dialog open={cmd !== null} onClose={closeCommand} title={m.commandTitle}>
        {cmd?.data ? (
          <CommandBody data={cmd.data} />
        ) : cmd?.error ? (
          <Alert>{cmd.error}</Alert>
        ) : (
          <p className="flex items-center gap-2 text-muted-foreground">
            <Spinner />
            {m.loading}
          </p>
        )}
        <DialogActions>
          <Button variant={cmd?.data ? "default" : "outline"} onClick={closeCommand}>
            {cmd?.data ? t.common.actions.done : t.common.actions.cancel}
          </Button>
        </DialogActions>
      </Dialog>
    </>
  );

  return { items, dialogs, available: can.link || can.command };
}

// CommandBody：命令和几条说明（加密、密钥留在历史里、需要的工具、续传和校验）。
function CommandBody({ data }: { data: DirectCommand }) {
  const m = t.fileManager.link;
  const notes = [m.encrypted, m.tools];
  if (data.archive) notes.push(m.archive);
  else {
    notes.push(m.resume, m.changing);
    if (data.sha256) notes.push(m.verified);
    else if (data.hash_skipped === "too_large") notes.push(m.tooLarge);
    else if (data.hash_skipped === "timeout") notes.push(m.timeout);
  }
  return (
    <>
      <CodeBlock copy>{data.command}</CodeBlock>
      <ul className="grid gap-1 leading-5 text-muted-foreground">
        {notes.map((n) => (
          <li key={n}>{n}</li>
        ))}
      </ul>
    </>
  );
}
