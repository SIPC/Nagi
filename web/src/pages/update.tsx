import { useEffect, useRef, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { toast } from "sonner";
import { errorMessage, settingsClient } from "@/lib/api";
import { Alert, AutoHeight, Button, dotColor, HelpTip, Skeleton, Spinner, Waves } from "@/components/ui";
import { confirmAction } from "@/components/confirm";
import { useStandbys } from "@/pages/standby";
import { t } from "@/i18n";
import { cn, timeAgo } from "@/lib/utils";
import { timestampDate } from "@bufbuild/protobuf/wkt";
import type { Node, UpdateState } from "@/gen/nagi/v1/api_pb";

// 「关于」顶部的版本信息和右下角的提示（见 docs/panel.md#更新面板）：检查有没有新版本，一键更新本面板。
// 进度记在面板上（GetUpdateStatus），刷新页面、换浏览器以后还在。

// 与 internal/pkg/release.IsVersion 一致。
const versionRe = /^v(\d+)\.(\d+)\.(\d+)([-+][0-9A-Za-z.-]+)?$/;

// isOlder：节点版本 a 是否早于面板版本 b。不是正式版本号（例如本地编译的 dev）时不比较。
function isOlder(a: string, b: string) {
  const x = versionRe.exec(a);
  const y = versionRe.exec(b);
  if (!x || !y) return false;
  for (let i = 1; i <= 3; i++) {
    const p = Number(x[i]);
    const q = Number(y[i]);
    if (p !== q) return p < q;
  }
  // 数字相同时，预发布版（v0.2.0-rc.1）早于正式版（v0.2.0）。
  return a !== b && x[4]?.startsWith("-") === true && !y[4]?.startsWith("-");
}

// upgradeTarget：节点可以一键升级到的版本（即面板的版本），不能升级时返回空串。
// 只升不降；面板自带的节点随面板升级；只支持 Linux 节点（见 internal/agent/upgrade.go）。
export function upgradeTarget(n: Node, panelVersion: string) {
  if (!n.online || n.pending || !n.info || n.info.embedded || n.info.os !== "linux") return "";
  return isOlder(n.info.version, panelVersion) ? panelVersion : "";
}

// canStopDownload：节点能不能停止正在进行的下载（agent 的 CancelUpgrade，v0.20.0 起，预发布版也算）。
// 更早的节点只能取消排队中的升级。不是正式版本号（本地编译的 dev）时当作支持。
export function canStopDownload(version: string) {
  const x = versionRe.exec(version);
  if (!x) return true;
  const since = [0, 20, 0];
  for (let i = 0; i < 3; i++) {
    const p = Number(x[i + 1]);
    if (p !== since[i]) return p > since[i];
  }
  return true;
}

// cancelable：这条升级能不能取消。已经替换、正在重启的不能。
export function cancelable(s: UpdateState | undefined, n: Node | undefined) {
  return s?.phase === "queued" || (s?.phase === "installing" && canStopDownload(n?.info?.version ?? ""));
}

export function updateActive(s: UpdateState | undefined) {
  return !!s && s.phase !== "failed";
}

// usePanelUpdate：最新版本的信息。面板自己缓存 1 小时，查不到时不重试（侧栏只是不显示提示）。
export function usePanelUpdate() {
  return useQuery({
    queryKey: ["panel-update"],
    queryFn: () => settingsClient.checkPanelUpdate({}),
    retry: false,
    staleTime: 60 * 60 * 1000,
  });
}

// useUpdateStatus：面板和节点的更新进度，只有管理员能查。更新时查得勤一些；面板重启时查询会失败，保留上一次的结果接着查。
export function useUpdateStatus(enabled = true) {
  return useQuery({
    queryKey: ["update-status"],
    queryFn: () => settingsClient.getUpdateStatus({}),
    enabled,
    retry: false,
    refetchInterval: (q) => {
      const d = q.state.data;
      return d && (updateActive(d.panel) || Object.values(d.nodes).some(updateActive)) ? 2000 : 15000;
    },
  });
}

// UpdateDot：有新版本时侧栏和二级菜单上的小圆点。
export function UpdateDot() {
  const q = usePanelUpdate();
  if (!q.data?.available) return null;
  return <span role="img" aria-label={t.update.badge} title={t.update.badge} className="ml-auto size-1.5 shrink-0 rounded-full bg-primary" />;
}

// 关掉过的提示记在浏览器里，同一件事不再弹。隐私模式等情况下存不了，只是下次还会提示。
function tipDismissed(key: string) {
  try {
    return localStorage.getItem(`update-tip:${key}`) !== null;
  } catch {
    return false;
  }
}

function dismissTip(key: string) {
  try {
    localStorage.setItem(`update-tip:${key}`, "1");
  } catch {
    // 见上。
  }
}

type Tip = { key: string; kind: "info" | "loading" | "error"; title: string; description?: string; to?: "update" | "nodes" };

function useTip(id: string, tip: Tip | null) {
  const navigate = useNavigate();
  const key = tip?.key;
  const shown = tip && !tipDismissed(tip.key) ? tip : null;
  // 代码换掉或收起提示时 sonner 也会调 onDismiss：只有还在显示的这一条被关掉才算用户关的。
  const current = useRef<string | null>(null);
  useEffect(() => {
    current.current = shown?.key ?? null;
    if (!shown) {
      toast.dismiss(id);
      return;
    }
    const show = shown.kind === "loading" ? toast.loading : shown.kind === "error" ? toast.error : toast;
    show(shown.title, {
      id,
      position: "bottom-right",
      duration: Infinity,
      description: shown.description,
      closeButton: shown.kind !== "loading",
      onDismiss: () => {
        if (current.current === shown.key && shown.kind !== "loading") dismissTip(shown.key);
      },
      action: shown.to && {
        label: t.update.tips.view,
        onClick: () => {
          dismissTip(shown.key);
          if (shown.to === "nodes") navigate({ to: "/nodes" });
          else navigate({ to: "/settings", search: { tab: "about" } });
        },
      },
    });
    // eslint-disable-next-line react-hooks/exhaustive-deps -- 内容由 key 决定。
  }, [id, key, !!shown, shown?.description, navigate]);
  useEffect(
    () => () => {
      current.current = null;
      toast.dismiss(id);
    },
    [id],
  );
}

// UpdateTips：右下角的提示：有新版本、更新进度和失败原因。面板更新好以后刷新页面，换上新的界面。只给管理员看。
export function UpdateTips({ nodes, isStandby }: { nodes: Node[]; isStandby: boolean }) {
  const check = usePanelUpdate();
  const status = useUpdateStatus().data;
  const panel = status?.panel;

  const updating = useRef(false);
  useEffect(() => {
    if (!status) return;
    if (updateActive(panel)) updating.current = true;
    else if (updating.current) {
      updating.current = false;
      if (!panel) window.location.reload();
    }
  }, [status, panel]);

  const latest = check.data?.available ? check.data.latest : "";
  let panelTip: Tip | null = null;
  if (panel?.phase === "failed") {
    panelTip = { key: `panel-failed-${panel.target}-${panel.startedAt?.seconds}`, kind: "error", title: t.update.tips.panelFailed(panel.target), description: panel.error, to: "update" };
  } else if (panel) {
    panelTip = { key: `panel-${panel.phase}`, kind: "loading", title: panel.phase === "restarting" ? t.update.restartingTitle(panel.target) : t.update.installingTitle(panel.target) };
  } else if (latest) {
    panelTip = { key: `panel-new-${latest}`, kind: "info", title: t.update.tips.available(latest), to: "update" };
  }
  useTip("panel-update", panelTip);

  const states = Object.entries(status?.nodes ?? {});
  const running = states.filter(([, s]) => updateActive(s));
  const failed = states.find(([, s]) => s.phase === "failed");
  const name = (id: string) => nodes.find((n) => n.id === id)?.name ?? id;
  const behind = status && !isStandby && !latest && !panel ? nodes.filter((n) => upgradeTarget(n, status.version)).length : 0;
  let nodeTip: Tip | null = null;
  if (running.length > 0) {
    nodeTip = { key: `nodes-running-${running.length}`, kind: "loading", title: t.update.tips.nodesRunning(running.length), to: "nodes" };
  } else if (failed) {
    nodeTip = { key: `node-failed-${failed[0]}-${failed[1].startedAt?.seconds}`, kind: "error", title: t.update.tips.nodeFailed(name(failed[0])), description: failed[1].error, to: "nodes" };
  } else if (behind > 0 && status) {
    nodeTip = { key: `nodes-new-${status.version}`, kind: "info", title: t.update.tips.nodesAvailable(behind, status.version), to: "nodes" };
  }
  useTip("node-upgrade", nodeTip);

  return null;
}

// 更新日志的一段行内文字：只认行内代码和链接，其余照原样显示。
function inline(text: string) {
  const out: React.ReactNode[] = [];
  const re = /`([^`]+)`|\[([^\]]+)\]\((https?:\/\/[^)\s]+)\)/g;
  let last = 0;
  for (const m of text.matchAll(re)) {
    out.push(text.slice(last, m.index));
    if (m[1]) {
      out.push(
        <code key={m.index} className="font-mono text-[0.9em]">
          {m[1]}
        </code>,
      );
    } else {
      out.push(
        <a key={m.index} className="text-foreground underline underline-offset-4" href={m[3]} target="_blank" rel="noreferrer">
          {m[2]}
        </a>,
      );
    }
    last = m.index + m[0].length;
  }
  out.push(text.slice(last));
  return out;
}

// NotesList：更新日志的列表。圆点贴齐左边、和第一行文字居中，文字跟在圆点后面，和列表上下的文字左对齐。
function NotesList({ items }: { items: string[] }) {
  return (
    <ul className="grid gap-1">
      {items.map((it, i) => (
        <li key={i} className="flex gap-2.5">
          <span aria-hidden className="flex h-[1lh] shrink-0 items-center">
            <span className="size-1 rounded-full bg-muted-foreground/70" />
          </span>
          <span className="min-w-0">{inline(it)}</span>
        </li>
      ))}
    </ul>
  );
}

// NotesBody：一个版本的更新日志。只有「### 分组」和列表两种写法（见 docs/development.md#发布），其他行当作段落。
function NotesBody({ body }: { body: string }) {
  const blocks: React.ReactNode[] = [];
  let items: string[] = [];
  const flushList = () => {
    if (items.length === 0) return;
    blocks.push(<NotesList key={blocks.length} items={items} />);
    items = [];
  };
  for (const raw of body.split("\n")) {
    const line = raw.trim();
    if (line.startsWith("- ")) {
      items.push(line.slice(2));
      continue;
    }
    flushList();
    if (!line) continue;
    if (line.startsWith("#")) {
      blocks.push(
        <h4 key={blocks.length} className="pt-1 text-xs leading-5 text-muted-foreground">
          {line.replace(/^#+\s*/, "")}
        </h4>,
      );
    } else blocks.push(<p key={blocks.length}>{inline(line)}</p>);
  }
  flushList();
  return <div className="grid gap-2">{blocks}</div>;
}

// useReleaseNotes：本面板到最新版本之间各版本的更新日志。
function useReleaseNotes(version: string, enabled: boolean) {
  return useQuery({
    queryKey: ["panel-release-notes", version],
    queryFn: () => settingsClient.getPanelReleaseNotes({}),
    enabled: enabled && !!version,
    retry: false,
    staleTime: Infinity,
  });
}

// notesPreview：最新版本日志的第一组，最多三条，放在「关于」的版本信息下面。
function notesPreview(body: string) {
  let group = "";
  const items: string[] = [];
  for (const raw of body.split("\n")) {
    const line = raw.trim();
    if (line.startsWith("#")) {
      if (group || items.length) break;
      group = line.replace(/^#+\s*/, "");
    } else if (line.startsWith("- ")) {
      items.push(line.slice(2));
      if (items.length === 3) break;
    }
  }
  return { group, items };
}

// PanelVersion：「关于」顶部的品牌和版本（见 docs/panel.md#更新面板）。参考 macOS「软件更新」：名称、版本和状态在一起，
// 右边是这时唯一的操作；有新版本时下面列出最新版本的几条更新说明，「更多信息」就地展开全部。只有管理员能检查和更新，其他角色只看版本和状态。
export function PanelVersion({ isAdmin }: { isAdmin: boolean }) {
  const queryClient = useQueryClient();
  const q = usePanelUpdate();
  const status = useUpdateStatus(isAdmin);
  const standbys = useStandbys().data;
  const [checking, setChecking] = useState(false);
  const [starting, setStarting] = useState(false);
  const [error, setError] = useState("");
  const [expanded, setExpanded] = useState(false);
  const notesRef = useRef<HTMLDivElement>(null);
  // 收起时内容变短：在底部点收起，日志的开头已经滚出屏幕时滚回开头，不然收起后的内容和按钮都在视野外。
  const toggleNotes = () => {
    setExpanded(!expanded);
    const el = notesRef.current;
    if (expanded && el && el.getBoundingClientRect().top < 0) {
      el.scrollIntoView({ block: "start", behavior: matchMedia("(prefers-reduced-motion: reduce)").matches ? "auto" : "smooth" });
    }
  };

  const check = async () => {
    setChecking(true);
    setError("");
    try {
      queryClient.setQueryData(["panel-update"], await settingsClient.checkPanelUpdate({ force: true }));
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setChecking(false);
    }
  };

  const update = async (version: string) => {
    if (!(await confirmAction(t.update.confirm(version), { action: t.update.updatePanel }))) return;
    setStarting(true);
    setError("");
    try {
      await settingsClient.updatePanel({});
      await status.refetch();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setStarting(false);
    }
  };

  const d = q.data;
  const panel = isAdmin ? status.data?.panel : undefined;
  const active = updateActive(panel);
  const failed = panel?.phase === "failed";
  const canUpdate = isAdmin && !!d?.available && !active;
  const notes = useReleaseNotes(d?.latest ?? "", canUpdate);
  const releases = notes.data?.releases ?? [];
  const latest = releases[0];
  const preview = latest && notesPreview(latest.body);
  // 主面板有备用面板时提醒它们要单独更新；备用面板上提醒只更新自己。
  const scopeHint = standbys?.isStandby
    ? { text: t.update.standbyHint, help: t.update.standbyHelp }
    : standbys?.standbys.length
      ? { text: t.update.primaryHint, help: t.update.primaryHelp }
      : null;

  // 版本号后面的状态：同一时间只有一种，按进行中、失败、可更新、已是最新的顺序。
  let tone: keyof typeof dotColor | null = null;
  let state = "";
  if (panel && active) {
    state = panel.phase === "restarting" ? t.update.restartingTitle(panel.target) : t.update.installingTitle(panel.target);
  } else if (panel && failed) {
    [tone, state] = ["danger", t.update.tips.panelFailed(panel.target)];
  } else if (d?.available) {
    [tone, state] = ["info", t.update.availableTitle(d.latest)];
  } else if (d && !d.unsupported) {
    [tone, state] = ["success", t.update.upToDate];
  }
  const checkedAt = d?.checkedAt && timestampDate(d.checkedAt);
  const showChecked = !!checkedAt && !!d && !d.available && !d.unsupported && !active && !failed;

  // 右边的按钮：可更新时是更新（失败后是重试），其余是检查更新。更新进行中、不能一键更新时没有按钮。
  let action: React.ReactNode = null;
  if (canUpdate) {
    action = (
      <Button readOnlyOk onClick={() => update(d.latest)} disabled={starting}>
        {starting && <Spinner />}
        {failed ? t.update.retry : t.update.action}
      </Button>
    );
  } else if (isAdmin && !active && !d?.unsupported && !q.isPending) {
    action = (
      <Button readOnlyOk variant="outline" onClick={check} disabled={checking}>
        {checking && <Spinner />}
        {t.update.check}
      </Button>
    );
  }

  // 版本信息下面：进行中的进度、失败原因、可更新时的更新说明，和名称左对齐。
  let below: React.ReactNode = null;
  if (panel && active) {
    below = <p className="text-pretty text-muted-foreground">{panel.phase === "restarting" ? t.update.restartingText : t.update.installingText}</p>;
  } else if (panel && failed && panel.error) {
    below = <p className="text-pretty text-muted-foreground">{panel.error}</p>;
  }
  // 收起时是最新版本的前三条；展开是本面板到最新版本之间的全部，按版本分段，新的在前。高度过渡，下面的内容跟着移动。
  const notesBlock = canUpdate && latest && preview && preview.items.length > 0 && (
    <div ref={notesRef} className="grid scroll-mt-20 gap-2 text-muted-foreground">
      <AutoHeight>
        {expanded ? (
          <div className="grid gap-5">
            {releases.map((r) => (
              <section key={r.version} className="grid gap-1">
                {/* 版本号用等宽字体。 */}
                <h3 className="font-mono text-xs leading-5 text-foreground">{r.version}</h3>
                <NotesBody body={r.body} />
              </section>
            ))}
          </div>
        ) : (
          <div className="grid gap-1">
            <p className="text-xs leading-5">
              <span className="font-mono text-foreground">{latest.version}</span>
              {preview.group && ` · ${preview.group}`}
            </p>
            <NotesList items={preview.items} />
          </div>
        )}
      </AutoHeight>
      <button type="button" aria-expanded={expanded} className="justify-self-start rounded-sm text-foreground outline-none hover:underline focus-ring" onClick={toggleNotes}>
        {expanded ? t.update.lessInfo : t.update.moreInfo}
      </button>
    </div>
  );

  // 间距按 HIG 的 8 点网格：logo 到名称 12，名称到介绍 4，介绍到版本 12，版本到下面的说明 16。
  return (
    <div className="grid">
      <Waves className="h-5 w-9 text-wave" />
      {/* 品牌是第一个区块：标题和区块标题同级（16 / 600），不和页面标题抢层级。 */}
      <h2 className="mt-3 text-base font-semibold">Nagi</h2>
      <p className="mt-1 text-pretty text-muted-foreground">{t.about.tagline}</p>
      {/* 按钮和版本同一行，上下居中。 */}
      <div className="mt-3 flex min-h-9 items-center gap-4">
        <div className="flex min-w-0 flex-1 flex-wrap items-center gap-x-3 gap-y-1 text-[13px] text-muted-foreground">
          {q.isPending ? (
            <Skeleton className="h-3 w-40" />
          ) : (
            <>
              {/* 状态点放在版本号前面，进行中换成转圈；版本号用等宽字体。 */}
              <span className="inline-flex items-center gap-2">
                {panel && active ? <Spinner /> : tone && <span className={cn("size-1.5 rounded-full", dotColor[tone])} />}
                <span className="font-mono text-foreground">{d?.current || status.data?.version || "—"}</span>
              </span>
              {d?.unsupported && <span>{d.unsupported}</span>}
              {!d && q.error && <span>{errorMessage(q.error)}</span>}
              {state && <span>{state}</span>}
              {showChecked && <span>{t.update.checkedAgo(timeAgo(checkedAt))}</span>}
              {canUpdate && scopeHint && (
                <span className="inline-flex items-center gap-1">
                  {scopeHint.text}
                  <HelpTip>{scopeHint.help}</HelpTip>
                </span>
              )}
            </>
          )}
        </div>
        {action && <div className="shrink-0">{action}</div>}
      </div>
      {(error || below || notesBlock) && (
        <div className="mt-4 grid gap-3">
          {error && <Alert>{error}</Alert>}
          {below}
          {notesBlock}
        </div>
      )}
    </div>
  );
}
