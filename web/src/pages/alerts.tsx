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
      setMsg("已保存并下发至在线节点");
    } catch (err) {
      setError(errorMessage(err));
    }
  };

  const test = async (c: AlertChannel) => {
    setError("");
    setMsg("");
    try {
      await settingsClient.testAlertChannel({ channel: c });
      setMsg(`已发送测试消息至 ${c.name || c.type}`);
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
      <Section title="通知渠道">
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
            <Notice tone="warning" title="告警配置无法解密">
              面板主密钥已重新生成，原告警配置无法解密，请重新填写并保存。保存前各节点继续按原配置发送告警，但面板自身的节点失联告警无法发送。
            </Notice>
          )}
        </div>
      )}

      <Section
        title="通知渠道"
        flush
        actions={
          <Button size="sm" variant="outline" onClick={addChannel}>
            添加渠道
          </Button>
        }
      >
        {channels.length === 0 ? (
          <EmptyRow>未配置渠道，告警不会发送。</EmptyRow>
        ) : (
          <Table>
            <thead>
              <tr>
                <Th className="w-40">名称</Th>
                <Th className="w-40">类型</Th>
                <Th>目标</Th>
                <Th className="w-px" />
              </tr>
            </thead>
            <tbody>
              {channels.map((c, i) => (
                <tr key={i}>
                  <Td>
                    <Input
                      className="h-8"
                      placeholder="名称"
                      aria-label="名称"
                      value={c.name}
                      onChange={(e) => update(i, { name: e.target.value })}
                    />
                  </Td>
                  <Td>
                    <Select
                      className="h-8"
                      aria-label="类型"
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
                          placeholder="机器人令牌"
                          aria-label="机器人令牌"
                          value={c.telegramToken}
                          onChange={(e) =>
                            update(i, { telegramToken: e.target.value })
                          }
                        />
                        <Input
                          className="h-8 min-w-28 flex-1 font-mono"
                          placeholder="聊天 ID"
                          aria-label="聊天 ID"
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
                        aria-label="Webhook 地址"
                        value={c.url}
                        onChange={(e) => update(i, { url: e.target.value })}
                      />
                    )}
                  </Td>
                  <Td>
                    <div className="flex justify-end gap-1">
                      <Button size="xs" variant="ghost" onClick={() => test(c)}>
                        测试
                      </Button>
                      <Button
                        size="xs"
                        variant="ghost"
                        onClick={() =>
                          setChannels(channels.filter((_, j) => j !== i))
                        }
                      >
                        删除
                      </Button>
                    </div>
                  </Td>
                </tr>
              ))}
            </tbody>
          </Table>
        )}
      </Section>

      <Section title="触发规则" help="留空时使用默认值，即输入框中的灰色数字。">
        <div className="grid grid-cols-2 gap-4 sm:grid-cols-4">
          {ruleField("磁盘使用率（%）", "diskPercent", 90)}
          {ruleField("内存使用率（%）", "memPercent", 90)}
          {ruleField("内存持续时间（分钟）", "memMinutes", 5)}
          {ruleField("重启次数", "restartCount", 3)}
          {ruleField("重启统计窗口（分钟）", "restartMinutes", 10)}
          {ruleField("证书剩余天数", "certDays", 7)}
          {ruleField("节点失联时间（秒）", "offlineSeconds", 60)}
          {ruleField("5xx 错误率（%）", "errorPercent", 5)}
        </div>
      </Section>

      <div className="flex items-center gap-4">
        <Button size="sm" onClick={save}>
          保存
        </Button>
        {msg && <span className="text-[13px] text-success">{msg}</span>}
      </div>
    </div>
  );
}
