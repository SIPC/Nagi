import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { errorMessage, settingsClient } from "@/lib/api";
import { confirmAction } from "@/components/confirm";
import { Alert, Button, Input, QueryError, RowsSkeleton, Section, SettingRow, SettingRows, Spinner } from "@/components/ui";
import { t } from "@/i18n";

// 带文字的复制按钮。复制不改东西，备用面板上也能用。
export function CopyTextButton({ text, label }: { text: string; label: string }) {
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
      {label}
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
  if (!s) return null;

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
      {error && (
        <p role="alert" className="text-[13px] leading-5 text-destructive">
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
                <Button variant="outline" onClick={() => save("domain", "")} disabled={busy}>
                  {busy && <Spinner />}
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
          <CopyTextButton text={s.fingerprint} label={t.common.actions.copy} />
        </SettingRow>
      </SettingRows>
    </Section>
  );
}
