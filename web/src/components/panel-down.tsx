import { useEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { Button, buttonVariants, CopyButton, HelpTip, Waves } from "@/components/ui";
import { knownStandbys, standbyURL } from "@/lib/offline";
import { t } from "@/i18n";

// 开着的页面连不上面板时盖住页面的提示（见 docs/standby.md#面板不可用时），列出浏览器里记下的备用面板。
// 新打开、刷新也连不上时由 Service Worker 显示 public/offline.html，内容一样。

// 每 5 秒试一次，连续 20 秒连不上才算：面板更新、重启只要几秒，不应该弹。
const probeMs = 5000;
const downAfterMs = 20000;
// 前面有代理（本机 Caddy、CDN）时，面板不在返回这些状态码，和 sw.js 一致。
const unavailable = new Set([502, 503, 504, 520, 521, 522, 523, 524]);

// usePanelDown：面板是不是已经连不上一段时间了。本机断网时不算：那不是面板的问题，提示转到备用面板也没用。
function usePanelDown() {
  const [down, setDown] = useState(false);
  useEffect(() => {
    let since = 0;
    let stopped = false;
    const probe = async () => {
      let ok = false;
      try {
        // 不经过离线缓存的公开接口；任何不是代理报错的响应都说明面板在。
        const r = await fetch("/api/standby/status", { cache: "no-store", signal: AbortSignal.timeout(probeMs) });
        ok = !unavailable.has(r.status);
      } catch {
        ok = false;
      }
      if (stopped) return;
      if (ok || !navigator.onLine) {
        since = 0;
        setDown(false);
        return;
      }
      since ||= Date.now();
      if (Date.now() - since >= downAfterMs) setDown(true);
    };
    const id = setInterval(probe, probeMs);
    return () => {
      stopped = true;
      clearInterval(id);
    };
  }, []);
  return down;
}

export function PanelDown() {
  const down = usePanelDown();
  const ref = useRef<HTMLDivElement>(null);
  // 盖住整页时，下面的页面不能再用 Tab 或读屏碰到：其余顶层元素都设 inert，焦点移到提示上。
  useEffect(() => {
    const box = ref.current;
    if (!down || !box) return;
    const others = [...document.body.children].filter((el): el is HTMLElement => el instanceof HTMLElement && el !== box && !el.inert);
    for (const el of others) el.inert = true;
    box.focus();
    return () => {
      for (const el of others) el.inert = false;
    };
  }, [down]);
  if (!down) return null;
  const k = t.standby.down;
  const standbys = knownStandbys()?.standbys ?? [];
  return createPortal(
    <div ref={ref} tabIndex={-1} role="alertdialog" aria-modal aria-labelledby="panel-down-title" aria-describedby="panel-down-text" className="fixed inset-0 z-[100] flex animate-fade-in items-center justify-center overflow-y-auto bg-background px-4 py-6 outline-none">
      <div className="w-full max-w-[480px]">
        <Waves className="h-4 w-7 text-wave" />
        <h1 id="panel-down-title" className="mt-4 text-2xl leading-8 font-semibold tracking-tight">
          {k.title}
        </h1>
        <p id="panel-down-text" className="mt-1 text-muted-foreground">
          {standbys.length > 0 ? k.withStandby : k.noStandby}
        </p>
        {standbys.length > 0 && (
          <>
            {/* 列表行之间不加线；只有第一个备用面板的「打开」是主按钮。 */}
            <ul className="-mx-2 mt-6 grid">
              {standbys.map((s, i) => {
                const url = standbyURL(s.address);
                return (
                  <li key={s.address} className="flex min-h-13 items-center gap-2 rounded-md px-2 py-1.5 transition-colors hover:bg-accent">
                    <div className="min-w-0 flex-1">
                      <div className="truncate">{s.name}</div>
                      <div className="font-mono text-[13px] break-all text-muted-foreground">{url}</div>
                    </div>
                    <CopyButton text={url} />
                    <a href={url} target="_blank" rel="noopener noreferrer" className={buttonVariants({ variant: i === 0 ? "default" : "outline" })}>
                      {k.open}
                    </a>
                  </li>
                );
              })}
            </ul>
            <p className="mt-4 text-muted-foreground">
              {k.note} <HelpTip>{k.noteHelp}</HelpTip>
            </p>
          </>
        )}
        <div className="mt-6">
          <Button variant="outline" readOnlyOk onClick={() => window.location.reload()}>
            {k.retry}
          </Button>
        </div>
      </div>
    </div>,
    document.body,
  );
}
