import { useEffect, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { create } from "@bufbuild/protobuf";
import { errorMessage, settingsClient } from "@/lib/api";
import {
  AlertChannelSchema,
  AlertConfigSchema,
  AlertRulesSchema,
  type AlertChannel,
  type AlertRules,
} from "@/gen/nagi/v1/common_pb";
import {
  Alert,
  Button,
  EmptyRow,
  Field,
  Input,
  Loading,
  Notice,
  Section,
  Select,
  Table,
  Td,
  Th,
} from "@/components/ui";
import { t } from "@/i18n";

// ---- 告警设置：通知渠道和触发规则，一起保存 ----

export function AlertSettings() {
  const cfg = useQuery({
    queryKey: ["alert-config"],
    queryFn: () => settingsClient.getAlertConfig({}),
  });
  const [channels, setChannels] = useState<AlertChannel[]>([]);
  const [rules, setRules] = useState<AlertRules>(create(AlertRulesSchema));
  const [msg, setMsg] = useState("");
  const [error, setError] = useState("");

  useEffect(() => {
    if (cfg.data?.config) {
      setChannels(cfg.data.config.channels);
      setRules(cfg.data.config.rules ?? create(AlertRulesSchema));
    }
  }, [cfg.data]);

  const update = (i: number, patch: Partial<AlertChannel>) =>
    setChannels(
      channels.map((c, j) =>
        j === i ? create(AlertChannelSchema, { ...c, ...patch }) : c,
      ),
    );

  const save = async () => {
    setError("");
    setMsg("");
    try {
      const res = await settingsClient.updateAlertConfig({
        config: create(AlertConfigSchema, { channels, rules }),
      });
      setChannels(res.config?.channels ?? []);
      setMsg(t.alerts.savedAndPushed);
    } catch (err) {
      setError(errorMessage(err));
    }
  };

  const test = async (c: AlertChannel) => {
    setError("");
    setMsg("");
    try {
      await settingsClient.testAlertChannel({ channel: c });
      setMsg(t.alerts.testSent(c.name || c.type));
    } catch (err) {
      setError(errorMessage(err));
    }
  };

  const ruleField = (
    label: string,
    key: keyof AlertRules,
    placeholder: number,
  ) => (
    <Field label={label} htmlFor={`rule-${key}`}>
      <Input
        id={`rule-${key}`}
        type="number"
        placeholder={String(placeholder)}
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

  if (cfg.isPending)
    return (
      <Section title={t.alerts.channels}>
        <Loading />
      </Section>
    );

  const addChannel = () =>
    setChannels([...channels, create(AlertChannelSchema, { type: "webhook" })]);
  return (
    <div className="grid gap-8">
      {(error || cfg.data?.lost) && (
        <div className="grid gap-3">
          {error && <Alert>{error}</Alert>}
          {cfg.data?.lost && (
            <Notice tone="warning" title={t.alerts.lostTitle}>
              {t.alerts.lostBody}
            </Notice>
          )}
        </div>
      )}

      <Section
        title={t.alerts.channels}
        flush
        actions={
          <Button size="sm" variant="outline" onClick={addChannel}>
            {t.alerts.addChannel}
          </Button>
        }
      >
        {channels.length === 0 ? (
          <EmptyRow>{t.alerts.noChannels}</EmptyRow>
        ) : (
          <Table>
            <thead>
              <tr>
                <Th className="w-40">{t.alerts.name}</Th>
                <Th className="w-40">{t.alerts.type}</Th>
                <Th>{t.alerts.target}</Th>
                <Th className="w-px" />
              </tr>
            </thead>
            <tbody>
              {channels.map((c, i) => (
                <tr key={i}>
                  <Td>
                    <Input
                      className="h-8"
                      placeholder={t.alerts.name}
                      aria-label={t.alerts.name}
                      value={c.name}
                      onChange={(e) => update(i, { name: e.target.value })}
                    />
                  </Td>
                  <Td>
                    <Select
                      className="h-8"
                      aria-label={t.alerts.type}
                      value={c.type}
                      onChange={(e) => update(i, { type: e.target.value })}
                    >
                      <option value="webhook">Webhook</option>
                      <option value="telegram">Telegram</option>
                    </Select>
                  </Td>
                  <Td>
                    {c.type === "telegram" ? (
                      <div className="flex gap-2">
                        <Input
                          className="h-8 min-w-48 flex-[2] font-mono"
                          placeholder={t.alerts.botToken}
                          aria-label={t.alerts.botToken}
                          value={c.telegramToken}
                          onChange={(e) =>
                            update(i, { telegramToken: e.target.value })
                          }
                        />
                        <Input
                          className="h-8 min-w-28 flex-1 font-mono"
                          placeholder={t.alerts.chatId}
                          aria-label={t.alerts.chatId}
                          value={c.telegramChatId}
                          onChange={(e) =>
                            update(i, { telegramChatId: e.target.value })
                          }
                        />
                      </div>
                    ) : (
                      <Input
                        className="h-8 min-w-64 font-mono"
                        placeholder="https://..."
                        aria-label={t.alerts.webhookUrl}
                        value={c.url}
                        onChange={(e) => update(i, { url: e.target.value })}
                      />
                    )}
                  </Td>
                  <Td>
                    <div className="flex justify-end gap-1">
                      <Button size="xs" variant="ghost" onClick={() => test(c)}>
                        {t.alerts.test}
                      </Button>
                      <Button
                        size="xs"
                        variant="ghost"
                        onClick={() =>
                          setChannels(channels.filter((_, j) => j !== i))
                        }
                      >
                        {t.common.actions.delete}
                      </Button>
                    </div>
                  </Td>
                </tr>
              ))}
            </tbody>
          </Table>
        )}
      </Section>

      <Section title={t.alerts.rules} help={t.alerts.rulesHelp}>
        <div className="grid grid-cols-2 gap-4 sm:grid-cols-4">
          {ruleField(t.alerts.rule.diskPercent, "diskPercent", 90)}
          {ruleField(t.alerts.rule.memPercent, "memPercent", 90)}
          {ruleField(t.alerts.rule.memMinutes, "memMinutes", 5)}
          {ruleField(t.alerts.rule.restartCount, "restartCount", 3)}
          {ruleField(t.alerts.rule.restartMinutes, "restartMinutes", 10)}
          {ruleField(t.alerts.rule.certDays, "certDays", 7)}
          {ruleField(t.alerts.rule.offlineSeconds, "offlineSeconds", 60)}
          {ruleField(t.alerts.rule.errorPercent, "errorPercent", 5)}
        </div>
      </Section>

      <div className="flex items-center gap-4">
        <Button size="sm" onClick={save}>
          {t.common.actions.save}
        </Button>
        {msg && <span className="text-[13px] text-success">{msg}</span>}
      </div>
    </div>
  );
}
