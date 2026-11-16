import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { errorMessage, settingsClient } from "@/lib/api";
import { confirmAction } from "@/components/confirm";
import { Alert, Button, CodeBlock, Dialog, DialogActions, Input, QueryError, RowsSkeleton, Section, SettingRow, SettingRows, Skeleton, Spinner } from "@/components/ui";
import { t } from "@/i18n";

// 设置行里的复制按钮：有框，写「复制」，各设置页统一用它。复制不改东西，备用面板上也能用。
export function CopyTextButton({ text }: { text: string }) {
  return (
    <Button
      readOnlyOk
      variant="outline"
      onClick={async () => {
        try {
          await navigator.clipboard.writeText(text);
          toast.success(t.common.feedback.copied);
        } catch {
          toast.error(t.common.feedback.copyFailed);
        }
      }}
    >
      {t.common.actions.copy}
    </Button>
  );
}

type Editing = "address" | "domain";

// 「面板访问」：节点和浏览器怎么找到本面板。修改时在那一行下面展开输入框。
export function PanelAccessCard() {
  const queryClient = useQueryClient();
  const q = useQuery({
    queryKey: ["settings"],
    queryFn: () => settingsClient.getSettings({}),
  });
  const [editing, setEditing] = useState<Editing | null>(null);
  const [value, setValue] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [unbinding, setUnbinding] = useState(false);

  const heading = t.settings.tabs.access.label;
  const help = t.settings.tabs.access.description;
  if (q.isPending)
    return (
      <Section title={heading} help={help}>
        <RowsSkeleton />
      </Section>
    );
  if (!q.data)
    return (
      <Section title={heading} help={help}>
        <QueryError error={errorMessage(q.error)} onRetry={() => q.refetch()} />
      </Section>
    );
  const s = q.data.settings;
  if (!s)
    return (
      <Section title={heading} help={help}>
        <QueryError error={t.common.feedback.failed} onRetry={() => q.refetch()} />
      </Section>
    );

  const open = (what: Editing) => {
    setEditing(what);
    setValue(what === "address" ? s.publicAddress : s.domain);
    setError("");
  };

  const save = async (what: Editing, raw: string) => {
    const next = raw.trim();
    if (next === (what === "address" ? s.publicAddress : s.domain)) {
      setEditing(null);
      return;
    }
    if (what === "domain" && !next && !(await confirmAction(t.access.domain.unbindConfirm(s.domain), { action: t.access.domain.unbind, danger: true }))) return;
    setBusy(true);
    setError("");
    try {
      if (what === "address") await settingsClient.updateSettings({ publicAddress: next });
      else await settingsClient.setPanelDomain({ domain: next });
      setEditing(null);
      await queryClient.invalidateQueries({ queryKey: ["settings"] });
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const editRow = (what: Editing) => (
    <form
      className="grid gap-2 px-2 pb-2"
      onSubmit={(e) => {
        e.preventDefault();
        save(what, value);
      }}
    >
      <div className="flex flex-wrap items-center gap-2">
        <Input
          autoFocus
          className="min-w-48 flex-1"
          aria-label={what === "address" ? t.access.address.label : t.access.domain.label}
          aria-invalid={!!error}
          placeholder={what === "domain" ? "panel.example.com" : undefined}
          required={what === "domain" && !s.domain}
          value={value}
          onChange={(e) => setValue(e.target.value)}
        />
        {what === "address" && <span className="font-mono text-[13px] text-muted-foreground">:{s.port}</span>}
        <Button type="submit" disabled={busy}>
          {busy && <Spinner />}
          {what === "address" ? t.common.actions.save : value.trim() || !s.domain ? t.access.domain.bind : t.access.domain.unbind}
        </Button>
        <Button variant="ghost" onClick={() => setEditing(null)} disabled={busy}>
          {t.common.actions.cancel}
        </Button>
      </div>
      {/* 和 Field 的错误同一种写法：14 号红字，在输入框下面。 */}
      {error && (
        <p role="alert" className="leading-5 text-destructive">
          {error}
        </p>
      )}
    </form>
  );

  return (
    <Section title={heading} help={help}>
      {/* 解除绑定不经过输入框，错误显示在上面；修改时显示在输入框下面。 */}
      {error && !editing && <Alert>{error}</Alert>}
      <SettingRows>
        <SettingRow
          label={t.access.address.label}
          help={t.access.address.help}
          description={`${s.publicAddress || t.access.address.unset} · ${t.access.address.usage}`}
        >
          {editing !== "address" && (
            <Button variant="outline" onClick={() => open("address")}>
              {t.common.actions.edit}
            </Button>
          )}
        </SettingRow>
        {editing === "address" && editRow("address")}
        <SettingRow label={t.access.domain.label} help={t.access.domain.help} description={s.domain ? t.access.domain.bound(s.domain) : t.access.domain.unbound}>
          {editing !== "domain" &&
            (s.domain ? (
              <span className="flex items-center gap-2">
                <Button variant="outline" onClick={() => open("domain")} disabled={busy}>
                  {t.common.actions.edit}
                </Button>
                <Button
                  variant="outline"
                  onClick={async () => {
                    // 转圈只跟着「解除绑定」，「编辑」只置灰。
                    setUnbinding(true);
                    await save("domain", "");
                    setUnbinding(false);
                  }}
                  disabled={busy}
                >
                  {unbinding && <Spinner />}
                  {t.access.domain.unbind}
                </Button>
              </span>
            ) : (
              <Button variant="outline" onClick={() => open("domain")}>
                {t.access.domain.bind}
              </Button>
            ))}
        </SettingRow>
        {editing === "domain" && editRow("domain")}
        <SettingRow label={t.access.fingerprint.label} help={t.access.fingerprint.help} description={<span className="break-all">{s.fingerprint}</span>}>
          <CopyTextButton text={s.fingerprint} />
        </SettingRow>
        {/* 主密钥和公钥指纹放在一起：都是面板的身份，换机器恢复时要用到。 */}
        {s.masterKeyPath && (
          <SettingRow label={t.security.masterKey.label} help={t.security.masterKey.help} description={<span className="break-all">{`${s.masterKeyPath} · ${t.security.masterKey.backup}`}</span>}>
            <CopyTextButton text={s.masterKeyPath} />
          </SettingRow>
        )}
        <SecureEntryRow />
      </SettingRows>
    </Section>
  );
}

// SecureEntryRow：安全入口（见 docs/panel.md）。开启后只有带入口路径的地址能打开登录页：开启后马上显示地址，提示保存；关闭前确认。
function SecureEntryRow() {
  const sec = useQuery({ queryKey: ["security"], queryFn: () => settingsClient.getSecurity({}) });
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [shown, setShown] = useState(false);
  const m = t.security.entrance;
  const s = sec.data;
  const url = s?.entrance ? `${window.location.origin}/${s.entrance}` : "";
  const set = async (enabled: boolean) => {
    if (!enabled && !(await confirmAction(m.disableConfirm, { action: m.disable, danger: true }))) return;
    setBusy(true);
    setError("");
    try {
      await settingsClient.setEntrance({ enabled });
      await sec.refetch();
      if (enabled) setShown(true);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };
  const description = sec.isPending ? (
    <Skeleton className="my-1 h-3 w-40" />
  ) : !s ? (
    <span className="text-destructive">{errorMessage(sec.error)}</span>
  ) : error ? (
    <span className="text-destructive">{error}</span>
  ) : (
    <span className="break-all">{s.entrance ? `${url} · ${m.onStatus}` : m.offStatus}</span>
  );
  return (
    <>
      <SettingRow label={m.label} help={m.help} description={description}>
        {!s ? (
          !sec.isPending && (
            <Button variant="outline" readOnlyOk onClick={() => sec.refetch()}>
              {t.common.actions.retry}
            </Button>
          )
        ) : (
          <span className="flex items-center gap-2">
            {s.entrance && <CopyTextButton text={url} />}
            <Button variant="outline" onClick={() => set(!s.entrance)} disabled={busy}>
              {busy && <Spinner />}
              {s.entrance ? m.actionOff : m.actionOn}
            </Button>
          </span>
        )}
      </SettingRow>
      {shown && url && (
        <Dialog open dismissible={false} onClose={() => setShown(false)} title={m.enabledTitle} description={m.enabledText}>
          <CodeBlock copy>{url}</CodeBlock>
          <DialogActions>
            <Button onClick={() => setShown(false)}>{t.common.actions.done}</Button>
          </DialogActions>
        </Dialog>
      )}
    </>
  );
}
