import { useEffect, useRef, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { toast } from "sonner";
import { errorMessage, settingsClient } from "@/lib/api";
import { Alert, Button, HelpTip, Notice, QueryError, RowsSkeleton, Section, SettingRow, SettingRows, Spinner } from "@/components/ui";
import { useStandbys } from "@/pages/standby";
import { t } from "@/i18n";
import { confirmAction } from "@/components/confirm";
import type { Node, UpdateState } from "@/gen/nagi/v1/api_pb";

// 「版本更新」区块和右下角的提示（见 docs/panel.md#更新面板）：检查有没有新版本，一键更新本面板。
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
          else navigate({ to: "/settings", search: { tab: "update" } });
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

export function UpdateCard() {
  const queryClient = useQueryClient();
  const q = usePanelUpdate();
  const status = useUpdateStatus();
  const standbys = useStandbys().data;
  const [checking, setChecking] = useState(false);
  const [checked, setChecked] = useState(false);
  const [starting, setStarting] = useState(false);
  const [error, setError] = useState("");

  const check = async () => {
    setChecking(true);
    setChecked(false);
    setError("");
    try {
      queryClient.setQueryData(["panel-update"], await settingsClient.checkPanelUpdate({ force: true }));
      setChecked(true);
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
  const panel = status.data?.panel;
  const active = updateActive(panel);
  const failed = panel?.phase === "failed";
  const primaryHint = !standbys?.isStandby && standbys?.standbys.length ? t.update.primaryHint : "";
  const updateButton = (label: string) =>
    d?.available && (
      <Button readOnlyOk onClick={() => update(d.latest)} disabled={starting}>
        {starting && <Spinner />}
        {label}
      </Button>
    );
  const details = d?.available && (
    <>
      {primaryHint && (
        <>
          {primaryHint} <HelpTip>{t.update.primaryHelp}</HelpTip>{" "}
        </>
      )}
      <a className="text-foreground underline-offset-4 hover:underline" href={d.releaseUrl} target="_blank" rel="noreferrer">
        {t.update.notes}
      </a>
    </>
  );

  return (
    <Section title={t.settings.tabs.update.label} help={t.update.help}>
      {error && <Alert>{error}</Alert>}
      {status.error && !status.data && <QueryError error={errorMessage(status.error)} onRetry={() => status.refetch()} />}
      {panel && failed && (
        <Notice tone="danger" title={t.update.tips.panelFailed(panel.target)} actions={updateButton(t.update.retry)}>
          {panel.error}
          {panel.error && details && " "}
          {details}
        </Notice>
      )}
      {panel &&
        active &&
        (panel.phase === "restarting" ? (
          <Notice tone="success" title={t.update.restartingTitle(panel.target)}>
            {t.update.restartingText}
          </Notice>
        ) : (
          <Notice tone="info" title={t.update.installingTitle(panel.target)}>
            {t.update.installingText}
          </Notice>
        ))}
      {d?.available && !active && !failed && (
        <Notice tone="info" title={t.update.availableTitle(d.latest)} actions={updateButton(t.update.action)}>
          {details}
        </Notice>
      )}
      {checked && d && !d.available && <Notice title={t.update.upToDate} />}
      {standbys?.isStandby && <Notice title={t.update.standbyTitle}>{t.update.standbyHint}</Notice>}
      {q.isPending ? (
        <RowsSkeleton count={2} />
      ) : !d ? (
        <QueryError error={errorMessage(q.error)} onRetry={() => q.refetch()} />
      ) : (
        <SettingRows>
          <SettingRow
            label={t.update.current}
            description={`${d.current || "—"}${d.unsupported ? ` · ${d.unsupported}` : ""}`}
          />
          {!d.unsupported && (
            <SettingRow
              label={t.update.latest}
              description={d.latest || "—"}
            >
              <Button readOnlyOk variant="outline" onClick={check} disabled={checking || starting || active}>
                {checking && <Spinner />}
                {t.update.check}
              </Button>
            </SettingRow>
          )}
        </SettingRows>
      )}
    </Section>
  );
}
