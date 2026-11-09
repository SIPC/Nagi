import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { create } from "@bufbuild/protobuf";
import { Plus } from "lucide-react";
import { toast } from "sonner";
import { errorMessage, settingsClient } from "@/lib/api";
import { AlertChannelSchema, AlertConfigSchema, type AlertChannel, type AlertConfig } from "@/gen/nagi/v1/common_pb";
import { confirmAction } from "@/components/confirm";
import { Alert, Button, Dialog, DialogActions, EmptyRow, Field, FormRow, HelpTip, Input, List, Notice, QueryError, Row, RowMenu, RowsSkeleton, Section, Select, Spinner } from "@/components/ui";
import { t } from "@/i18n";

// ---- 告警设置：通知渠道（规则在各节点和项目的设置里） ----

const channelName = (c: AlertChannel) => c.name || c.type;

// ChannelDialog：添加或编辑一个渠道。保存前也可以发测试消息。
function ChannelDialog({ initial, adding, onClose, onSave }: { initial: AlertChannel; adding: boolean; onClose: () => void; onSave: (c: AlertChannel) => Promise<void> }) {
  const [c, setC] = useState(initial);
  const [busy, setBusy] = useState<"save" | "test" | null>(null);
  const [error, setError] = useState("");
  const patch = (p: Partial<AlertChannel>) => setC(create(AlertChannelSchema, { ...c, ...p }));

  const run = async (what: "save" | "test", fn: () => Promise<unknown>) => {
    setBusy(what);
    setError("");
    try {
      await fn();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(null);
    }
  };

  return (
    <Dialog open onClose={onClose} title={adding ? t.alerts.addChannel : t.alerts.editChannel} className="max-w-[520px]">
      <form
        className="grid gap-4"
        onSubmit={(e) => {
          e.preventDefault();
          run("save", () => onSave(c));
        }}
      >
        <FormRow>
          <Field label={t.alerts.name} htmlFor="channel-name">
            <Input id="channel-name" autoFocus value={c.name} onChange={(e) => patch({ name: e.target.value })} />
          </Field>
          <Field label={t.alerts.type} htmlFor="channel-type">
            <Select id="channel-type" value={c.type} onChange={(e) => patch({ type: e.target.value })}>
              <option value="webhook">Webhook</option>
              <option value="telegram">Telegram</option>
            </Select>
          </Field>
        </FormRow>
        {c.type === "telegram" ? (
          <FormRow>
            <Field label={t.alerts.botToken} htmlFor="channel-token">
              <Input id="channel-token" className="font-mono [&:not(:focus)]:[-webkit-text-security:disc]" autoComplete="off" spellCheck={false} data-1p-ignore data-lpignore="true" value={c.telegramToken} onChange={(e) => patch({ telegramToken: e.target.value })} />
            </Field>
            <Field label={t.alerts.chatId} htmlFor="channel-chat">
              <Input id="channel-chat" className="font-mono" autoComplete="off" spellCheck={false} value={c.telegramChatId} onChange={(e) => patch({ telegramChatId: e.target.value })} />
            </Field>
          </FormRow>
        ) : (
          <Field label={t.alerts.webhookUrl} htmlFor="channel-url">
            <Input id="channel-url" type="url" placeholder="https://" autoComplete="off" spellCheck={false} value={c.url} onChange={(e) => patch({ url: e.target.value })} />
          </Field>
        )}
        {error && <Alert>{error}</Alert>}
        <DialogActions>
          <Button
            variant="ghost"
            className="mr-auto"
            disabled={busy !== null}
            onClick={() =>
              run("test", async () => {
                await settingsClient.testAlertChannel({ channel: c });
                toast.success(t.alerts.testSent(channelName(c)));
              })
            }
          >
            {busy === "test" && <Spinner />}
            {t.alerts.test}
          </Button>
          <Button variant="outline" onClick={onClose}>
            {t.common.actions.cancel}
          </Button>
          <Button type="submit" disabled={busy !== null}>
            {busy === "save" && <Spinner />}
            {t.common.actions.save}
          </Button>
        </DialogActions>
      </form>
    </Dialog>
  );
}

// 查询成功才挂载，所以这里拿到的一定是面板上的配置，不会用空配置覆盖。
// 告警规则在各节点和项目的设置里（见 components/alert-rules.tsx），这里只有渠道。
function AlertForm({ config, lost }: { config: AlertConfig | undefined; lost: boolean }) {
  const queryClient = useQueryClient();
  const channels = config?.channels ?? [];
  const [editing, setEditing] = useState<{ index: number | null; channel: AlertChannel } | null>(null);
  const [testing, setTesting] = useState<number | null>(null);
  const [channelError, setChannelError] = useState("");

  // 渠道的增删改在列表上就看得到，不另外提示。
  const push = async (nextChannels: AlertChannel[]) => {
    await settingsClient.updateAlertConfig({ config: create(AlertConfigSchema, { channels: nextChannels }) });
    await queryClient.invalidateQueries({ queryKey: ["alert-config"] });
  };

  const saveChannel = async (c: AlertChannel) => {
    const index = editing?.index ?? null;
    await push(index === null ? [...channels, c] : channels.map((x, i) => (i === index ? c : x)));
    setEditing(null);
  };

  const removeChannel = async (i: number) => {
    if (!(await confirmAction(t.alerts.deleteChannelConfirm(channelName(channels[i])), { action: t.alerts.deleteChannel, danger: true }))) return;
    setChannelError("");
    try {
      await push(channels.filter((_, j) => j !== i));
    } catch (err) {
      setChannelError(errorMessage(err));
    }
  };

  const test = async (i: number) => {
    setChannelError("");
    setTesting(i);
    try {
      await settingsClient.testAlertChannel({ channel: channels[i] });
      toast.success(t.alerts.testSent(channelName(channels[i])));
    } catch (err) {
      setChannelError(errorMessage(err));
    } finally {
      setTesting(null);
    }
  };

  const addButton = (
    <Button variant="outline" onClick={() => setEditing({ index: null, channel: create(AlertChannelSchema, { type: "webhook" }) })}>
      <Plus />
      {t.alerts.addChannel}
    </Button>
  );
  return (
    <>
      <Section title={t.settings.tabs.alerts.label} help={`${t.alerts.channelsHelp}${t.alerts.rulesWhere}`} actions={addButton}>
        {lost && (
          <Notice tone="warning" title={t.alerts.lostTitle}>
            {t.alerts.lostBody} <HelpTip>{t.alerts.lostHelp}</HelpTip>
          </Notice>
        )}
        {channelError && <Alert>{channelError}</Alert>}
        {channels.length === 0 ? (
          <EmptyRow action={addButton}>{t.alerts.noChannels}</EmptyRow>
        ) : (
          <List>
            {channels.map((c, i) => (
              <Row
                key={i}
                title={<span className="truncate">{channelName(c)}</span>}
                sub={<span className="wrap-anywhere">{c.type === "telegram" ? `Telegram · ${c.telegramChatId}` : `Webhook · ${c.url}`}</span>}
                actionsWidth="w-auto"
                actions={
                  <span className="flex items-center">
                    <Button size="row" variant="ghost" disabled={testing !== null} onClick={() => test(i)}>
                      {testing === i && <Spinner />}
                      {t.alerts.test}
                    </Button>
                    <RowMenu
                      items={[
                        { label: t.common.actions.edit, onSelect: () => setEditing({ index: i, channel: c }) },
                        { label: t.common.actions.delete, danger: true, onSelect: () => removeChannel(i) },
                      ]}
                    />
                  </span>
                }
              />
            ))}
          </List>
        )}
      </Section>

      {editing && <ChannelDialog initial={editing.channel} adding={editing.index === null} onClose={() => setEditing(null)} onSave={saveChannel} />}
    </>
  );
}

export function AlertSettings() {
  const cfg = useQuery({
    queryKey: ["alert-config"],
    queryFn: () => settingsClient.getAlertConfig({}),
  });
  if (cfg.isPending)
    return (
      <Section title={t.settings.tabs.alerts.label}>
        <RowsSkeleton />
      </Section>
    );
  // 查询失败时不显示表单：空配置一保存就把面板上的配置覆盖了。
  if (!cfg.data)
    return (
      <Section title={t.settings.tabs.alerts.label}>
        <QueryError error={errorMessage(cfg.error)} onRetry={() => cfg.refetch()} />
      </Section>
    );
  return <AlertForm config={cfg.data.config} lost={cfg.data.lost} />;
}
