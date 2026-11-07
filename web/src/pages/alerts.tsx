import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { create } from "@bufbuild/protobuf";
import { Plus } from "lucide-react";
import { toast } from "sonner";
import { errorMessage, settingsClient } from "@/lib/api";
import { AlertChannelSchema, AlertConfigSchema, AlertRulesSchema, type AlertChannel, type AlertConfig, type AlertRules } from "@/gen/nagi/v1/common_pb";
import { confirmAction } from "@/components/confirm";
import { Alert, Button, Dialog, DialogActions, EmptyRow, Field, FormActions, FormBody, FormRow, HelpTip, Input, List, Notice, QueryError, Row, RowMenu, RowsSkeleton, Section, Select, Spinner } from "@/components/ui";
import { useUnsaved } from "@/components/unsaved";
import { t } from "@/i18n";

// ---- 告警设置：通知渠道和触发规则 ----

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
              <Input id="channel-token" className="font-mono" autoComplete="off" value={c.telegramToken} onChange={(e) => patch({ telegramToken: e.target.value })} />
            </Field>
            <Field label={t.alerts.chatId} htmlFor="channel-chat">
              <Input id="channel-chat" className="font-mono" autoComplete="off" value={c.telegramChatId} onChange={(e) => patch({ telegramChatId: e.target.value })} />
            </Field>
          </FormRow>
        ) : (
          <Field label={t.alerts.webhookUrl} htmlFor="channel-url">
            <Input id="channel-url" placeholder="https://" autoComplete="off" value={c.url} onChange={(e) => patch({ url: e.target.value })} />
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
// 面板上的规则变了（包括刚保存）时由上层换 key 重新挂载，正在填写的规则和「未保存」的起点一起更新。
function AlertForm({ config, lost }: { config: AlertConfig | undefined; lost: boolean }) {
  const queryClient = useQueryClient();
  const channels = config?.channels ?? [];
  const savedRules = config?.rules ?? create(AlertRulesSchema);
  const [rules, setRules] = useState<AlertRules>(savedRules);
  const [editing, setEditing] = useState<{ index: number | null; channel: AlertChannel } | null>(null);
  const [testing, setTesting] = useState<number | null>(null);
  const [saving, setSaving] = useState(false);
  const [channelError, setChannelError] = useState("");
  const [rulesError, setRulesError] = useState("");

  useUnsaved(rules);

  // 渠道和规则存在同一份配置里。各自保存时另一半用面板上已保存的值，不带上还没点保存的修改。
  const push = async (nextChannels: AlertChannel[], nextRules: AlertRules) => {
    await settingsClient.updateAlertConfig({ config: create(AlertConfigSchema, { channels: nextChannels, rules: nextRules }) });
    await queryClient.invalidateQueries({ queryKey: ["alert-config"] });
    toast.success(t.alerts.savedAndPushed);
  };

  const saveChannel = async (c: AlertChannel) => {
    const index = editing?.index ?? null;
    await push(index === null ? [...channels, c] : channels.map((x, i) => (i === index ? c : x)), savedRules);
    setEditing(null);
  };

  const removeChannel = async (i: number) => {
    if (!(await confirmAction(t.alerts.deleteChannelConfirm(channelName(channels[i])), { action: t.alerts.deleteChannel, danger: true }))) return;
    setChannelError("");
    try {
      await push(
        channels.filter((_, j) => j !== i),
        savedRules,
      );
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

  const saveRules = async () => {
    setSaving(true);
    setRulesError("");
    try {
      await push(channels, rules);
    } catch (err) {
      setRulesError(errorMessage(err));
    } finally {
      setSaving(false);
    }
  };

  const ruleField = (label: string, key: keyof AlertRules, placeholder: number, help?: string) => (
    <Field label={label} htmlFor={`rule-${key}`} help={help}>
      <Input
        id={`rule-${key}`}
        type="number"
        placeholder={t.alerts.defaultValue(placeholder)}
        value={(rules[key] as number) || ""}
        onChange={(e) =>
          setRules(
            create(AlertRulesSchema, {
              ...rules,
              [key]: Number(e.target.value) || 0,
            }),
          )
        }
      />
    </Field>
  );

  const addButton = (
    <Button variant="outline" onClick={() => setEditing({ index: null, channel: create(AlertChannelSchema, { type: "webhook" }) })}>
      <Plus />
      {t.alerts.addChannel}
    </Button>
  );
  // 一组规则：小标题加字段。
  const group = (title: string, children: React.ReactNode) => (
    <fieldset className="grid gap-4">
      <legend className="mb-4 font-medium">{title}</legend>
      {children}
    </fieldset>
  );

  return (
    <>
      <Section title={t.alerts.channels} help={t.alerts.channelsHelp} actions={addButton}>
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

      <Section title={t.alerts.rules} help={t.alerts.rulesHelp}>
        <form
          onSubmit={(e) => {
            e.preventDefault();
            saveRules();
          }}
        >
          <FormBody className="gap-6">
            {group(
              t.alerts.ruleGroups.resources,
              <>
                <FormRow>
                  {ruleField(t.alerts.rule.memPercent, "memPercent", 90)}
                  {ruleField(t.alerts.rule.memMinutes, "memMinutes", 5)}
                </FormRow>
                <FormRow>{ruleField(t.alerts.rule.diskPercent, "diskPercent", 90)}</FormRow>
              </>,
            )}
            {group(
              t.alerts.ruleGroups.containers,
              <>
                <FormRow>
                  {ruleField(t.alerts.rule.restartCount, "restartCount", 3)}
                  {ruleField(t.alerts.rule.restartMinutes, "restartMinutes", 10)}
                </FormRow>
                <FormRow>{ruleField(t.alerts.rule.errorPercent, "errorPercent", 5, t.alerts.rule.errorPercentHelp)}</FormRow>
              </>,
            )}
            {group(
              t.alerts.ruleGroups.availability,
              <FormRow>
                {ruleField(t.alerts.rule.offlineSeconds, "offlineSeconds", 60)}
                {ruleField(t.alerts.rule.certDays, "certDays", 7)}
              </FormRow>,
            )}
            {rulesError && <Alert>{rulesError}</Alert>}
            <FormActions>
              <Button type="submit" variant="outline" disabled={saving}>
                {saving && <Spinner />}
                {t.common.actions.save}
              </Button>
            </FormActions>
          </FormBody>
        </form>
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
      <Section title={t.alerts.channels}>
        <RowsSkeleton />
      </Section>
    );
  // 查询失败时不显示表单：空配置一保存就把面板上的配置覆盖了。
  if (!cfg.data)
    return (
      <Section title={t.alerts.channels}>
        <QueryError error={errorMessage(cfg.error)} onRetry={() => cfg.refetch()} />
      </Section>
    );
  return <AlertForm key={JSON.stringify(cfg.data.config?.rules ?? {})} config={cfg.data.config} lost={cfg.data.lost} />;
}
