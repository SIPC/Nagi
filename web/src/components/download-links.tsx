// 文件的「下载」和「复制下载链接」（见 docs/agent.md#直连下载、#加密-http-下载），卷文件和节点文件共用。
// 「下载」按节点的直连下载状态选方式：
//   - 关了直连下载（或旧版本的节点）：经面板下载，用面板签发的签名链接（见 lib/direct-download.ts 的 relayLink）；
//   - HTTPS 能用：探测能连上就直接从节点下载，否则悄悄改为经面板下载；
//   - 只有加密 HTTP：打开下载命令的对话框（curl 经 HTTP 下载密文，在本地解密）。浏览器拦截 HTTPS 页面上的 HTTP 下载，
//     所以命令只能复制；对话框里也可以改为经面板下载。
// 「复制下载链接」只在 HTTPS 能用时出现，给下载工具或别的设备用。
import { useRef, useState } from "react";
import { toast } from "sonner";
import { Alert, Button, CodeBlock, Dialog, DialogActions, Segmented, Spinner, textLink } from "@/components/ui";
import type { DirectDownload } from "@/gen/nagi/v1/common_pb";
import { t } from "@/i18n";
import { errorMessage } from "@/lib/api";
import { type DirectCommand, directActions, directEndpoint, directLink, fetchDirectCommand, fetchDirectLink, relayLink, saveFrom } from "@/lib/direct-download";

type Options = {
  // direct：节点上报的直连下载状态；旧版本的节点没有，一律经面板下载。
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
type OS = "posix" | "windows";

// visitorOS：命令默认给哪个系统。
function visitorOS(): OS {
  const nav = navigator as Navigator & { userAgentData?: { platform?: string } };
  const platform = nav.userAgentData?.platform || nav.userAgent;
  return /windows/i.test(platform) ? "windows" : "posix";
}

export function useDownloads({ direct, url, check, fail, onError }: Options) {
  const m = t.fileManager.link;
  const can = directActions(direct);
  const [link, setLink] = useState<string | null>(null);
  const [cmd, setCmd] = useState<CommandState | null>(null);
  const [relaying, setRelaying] = useState(false);
  const abort = useRef<AbortController | null>(null);

  const relay = async (path: string) => saveFrom(await relayLink(url(path), fail));

  // download 返回时下载已经交给浏览器（或者打开了命令的对话框）；出错时抛错，由页面显示。
  const download = async (path: string) => {
    if (can.command) {
      void openCommand(path);
      return;
    }
    if (can.link) {
      await check(path);
      const direct = await directLink(directEndpoint(url(path)));
      if (direct) {
        saveFrom(direct);
        return;
      }
    }
    await relay(path);
  };

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
  const relayFromDialog = async () => {
    if (!cmd) return;
    setRelaying(true);
    try {
      await relay(cmd.path);
      closeCommand();
    } catch (err) {
      setCmd({ ...cmd, error: errorMessage(err), data: undefined });
    } finally {
      setRelaying(false);
    }
  };

  const items = (path: string) => (can.link ? [{ label: m.copyLink, onSelect: () => void copyLink(path) }] : []);

  const dialogs = (
    <>
      <Dialog open={link !== null} onClose={() => setLink(null)} title={m.linkTitle} description={m.linkHelp}>
        <CodeBlock copy>{link ?? ""}</CodeBlock>
        <DialogActions>
          <Button onClick={() => setLink(null)}>{t.common.actions.done}</Button>
        </DialogActions>
      </Dialog>
      <Dialog open={cmd !== null} onClose={closeCommand} title={m.commandTitle} description={m.commandHelp}>
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
          <Button variant="outline" onClick={() => void relayFromDialog()} disabled={relaying}>
            {relaying && <Spinner />}
            {m.relay}
          </Button>
          <Button onClick={closeCommand}>{t.common.actions.done}</Button>
        </DialogActions>
      </Dialog>
    </>
  );

  return { download, items, dialogs, available: can.link };
}

// CommandBody：Linux / macOS、Windows 两种命令和几条说明（加密、密钥留在历史里、需要的工具、保存位置、分段、续传和校验）。
// 默认显示一行短命令（从面板取脚本运行），「显示完整命令」换成完整的脚本：不能用 iex、curl | sh 时直接粘贴。
function CommandBody({ data }: { data: DirectCommand }) {
  const m = t.fileManager.link;
  const [os, setOS] = useState<OS>(() => (data.command_windows ? visitorOS() : "posix"));
  const [full, setFull] = useState(false);
  const win = os === "windows" && !!data.command_windows;
  const short = win ? data.short_windows : data.short;
  const showShort = !!short && !full;
  const notes = [m.encrypted, win ? m.windowsTools : m.posixTools, win ? m.windowsSaved : m.posixSaved];
  if (showShort) notes.push(m.shortTrust);
  if (data.archive) notes.push(m.archive);
  else {
    if (data.segmented) notes.push(showShort ? m.segmentedShort : m.segmented);
    notes.push(showShort ? m.resumeShort : m.resume, m.changing);
    if (data.sha256) notes.push(m.verified);
    else if (data.hash_skipped === "too_large") notes.push(m.tooLarge);
    else if (data.hash_skipped === "timeout") notes.push(m.timeout);
  }
  const text = showShort ? short : win ? data.command_windows! : data.command;
  return (
    <>
      {data.command_windows && (
        <Segmented
          value={os}
          onChange={setOS}
          label={m.commandTitle}
          items={[
            { value: "posix", label: m.posix },
            { value: "windows", label: m.windows },
          ]}
        />
      )}
      {/* 短命令放不下时折行，不截断：要复制前看清楚取脚本的地址是不是自己的面板。 */}
      <CodeBlock copy className="[&>code]:max-h-72 [&>code]:overflow-auto">
        {text}
      </CodeBlock>
      <ul className="grid gap-1 leading-5 text-muted-foreground">
        {notes.map((n) => (
          <li key={n}>{n}</li>
        ))}
      </ul>
      {short && (
        <p className="leading-5 text-muted-foreground">
          <button type="button" className={textLink} onClick={() => setFull(!full)}>
            {full ? m.showShort : m.showFull}
          </button>
          {!full && ` ${m.fullHelp}`}
        </p>
      )}
    </>
  );
}
