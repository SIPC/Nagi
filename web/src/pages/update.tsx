import { useEffect, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { errorMessage, settingsClient } from "@/lib/api";
import { Alert, Button, Notice, SettingRow, SettingRows, Spinner } from "@/components/ui";
import { useStandbys } from "@/pages/standby";
import { t } from "@/i18n";
import { confirmAction } from "@/components/confirm";

// 「版本更新」区块（见 docs/panel.md#更新面板）：检查有没有新版本，一键更新本面板。

// 面板重启后等多久还没恢复就提示去服务器上看。
const restartTimeoutMs = 3 * 60 * 1000;

// usePanelUpdate：最新版本的信息。面板自己缓存 1 小时，查不到时不重试（侧栏只是不显示提示）。
export function usePanelUpdate() {
  return useQuery({
    queryKey: ["panel-update"],
    queryFn: () => settingsClient.checkPanelUpdate({}),
    retry: false,
    staleTime: 60 * 60 * 1000,
  });
}

// UpdateDot：有新版本时侧栏和二级菜单上的小圆点。
export function UpdateDot() {
  const q = usePanelUpdate();
  if (!q.data?.available) return null;
  return <span role="img" aria-label={t.update.badge} title={t.update.badge} className="ml-auto size-1.5 shrink-0 rounded-full bg-primary" />;
}

export function UpdateCard() {
  const queryClient = useQueryClient();
  const q = usePanelUpdate();
  const standbys = useStandbys().data;
  const [checking, setChecking] = useState(false);
  const [checked, setChecked] = useState(false);
  const [updating, setUpdating] = useState(false);
  const [error, setError] = useState("");
  // 已经替换好、正在重启：等面板回来报新版本，然后刷新页面换上新的界面。
  const [target, setTarget] = useState("");
  const [timedOut, setTimedOut] = useState(false);

  useEffect(() => {
    if (!target) return;
    const deadline = Date.now() + restartTimeoutMs;
    const timer = setInterval(async () => {
      try {
        const res = await settingsClient.getSettings({});
        if (res.settings?.version === target) {
          clearInterval(timer);
          window.location.reload();
          return;
        }
      } catch {
        // 面板还在重启。
      }
      if (Date.now() > deadline) {
        clearInterval(timer);
        setTimedOut(true);
      }
    }, 2000);
    return () => clearInterval(timer);
  }, [target]);

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
    if (!(await confirmAction(t.update.confirm(version), { action: t.update.action }))) return;
    setUpdating(true);
    setError("");
    try {
      const res = await settingsClient.updatePanel({});
      setTarget(res.version);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setUpdating(false);
    }
  };

  if (q.isPending) return null;
  const d = q.data;
  const shownError = error || (q.error ? errorMessage(q.error) : "");
  const standbyHint = standbys?.isStandby ? t.update.standbyHint : standbys?.standbys.length ? t.update.primaryHint : "";

  return (
    <SettingRows>
      {shownError && (
        <div className="pb-5">
          <Alert>{shownError}</Alert>
        </div>
      )}
      {target && (
        <div className="pb-5">
          {timedOut ? (
            <Alert>{t.update.timeout(target)}</Alert>
          ) : (
            <Notice tone="success" title={t.update.restartingTitle(target)}>
              {t.update.restartingText}
            </Notice>
          )}
        </div>
      )}
      {d?.available && !target && (
        <div className="pb-5">
          <Notice
            title={t.update.availableTitle(d.latest)}
            actions={
              <Button readOnlyOk size="sm" onClick={() => update(d.latest)} disabled={updating}>
                {updating && <Spinner />}
                {updating ? t.update.running : t.update.action}
              </Button>
            }
          >
            <p>
              {t.update.text}{" "}
              <a className="text-foreground underline underline-offset-2" href={d.releaseUrl} target="_blank" rel="noreferrer">
                {t.update.notes}
              </a>
            </p>
            {standbyHint && <p className="mt-1">{standbyHint}</p>}
          </Notice>
        </div>
      )}
      <SettingRow label={t.update.current} description={d?.unsupported}>
        <code className="font-mono text-[13px]">{d?.current || "—"}</code>
      </SettingRow>
      {!d?.unsupported && (
        <SettingRow label={t.update.latest} description={checked && d && !d.available ? t.update.upToDate : undefined}>
          <div className="flex flex-wrap items-center gap-3">
            <code className="font-mono text-[13px]">{d?.latest || "—"}</code>
            <Button readOnlyOk size="sm" variant="outline" onClick={check} disabled={checking || updating || !!target}>
              {checking && <Spinner />}
              {t.update.check}
            </Button>
          </div>
        </SettingRow>
      )}
    </SettingRows>
  );
}
