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
  Card,
  CardContent,
  CardHeader,
  CardTitle,
  Input,
  Select,
  Spinner,
} from "@/components/ui";

// ---- 告警设置（临时测试界面）----

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
      setMsg("已保存，并下发给在线节点");
    } catch (err) {
      setError(errorMessage(err));
    }
  };

  const test = async (c: AlertChannel) => {
    setError("");
    setMsg("");
    try {
      await settingsClient.testAlertChannel({ channel: c });
      setMsg(`已发送测试消息到 ${c.name || c.type}`);
    } catch (err) {
      setError(errorMessage(err));
    }
  };

  const ruleField = (
    label: string,
    key: keyof AlertRules,
    placeholder: number,
  ) => (
    <label className="grid gap-1 text-xs text-muted-foreground">
      {label}
      <Input
        type="number"
        className="w-28"
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
    </label>
  );

  if (cfg.isPending) return <Spinner />;
  return (
    <Card className="mb-6">
      <CardHeader>
        <CardTitle>告警</CardTitle>
      </CardHeader>
      <CardContent className="grid gap-4 text-sm">
        {error && <Alert>{error}</Alert>}
        {cfg.data?.lost && (
          <Alert>
            面板主密钥重新生成过，原来的告警配置解不开了，请重新填写并保存。在那之前各节点继续按原来的配置发告警，面板自己的"节点失联"告警发不出去。
          </Alert>
        )}
        {msg && <div className="text-muted-foreground">{msg}</div>}
        <div className="grid gap-2">
          <div className="font-medium">渠道</div>
          {channels.length === 0 && (
            <div className="text-muted-foreground">
              还没有渠道，告警不会发送。
            </div>
          )}
          {channels.map((c, i) => (
            <div key={i} className="flex flex-wrap items-center gap-2">
              <Input
                className="w-28"
                placeholder="名称"
                value={c.name}
                onChange={(e) => update(i, { name: e.target.value })}
              />
              <Select
                className="w-28"
                value={c.type}
                onChange={(e) => update(i, { type: e.target.value })}
              >
                <option value="webhook">webhook</option>
                <option value="telegram">telegram</option>
              </Select>
              {c.type === "telegram" ? (
                <>
                  <Input
                    className="w-64 font-mono"
                    placeholder="机器人令牌"
                    value={c.telegramToken}
                    onChange={(e) =>
                      update(i, { telegramToken: e.target.value })
                    }
                  />
                  <Input
                    className="w-36 font-mono"
                    placeholder="聊天 ID"
                    value={c.telegramChatId}
                    onChange={(e) =>
                      update(i, { telegramChatId: e.target.value })
                    }
                  />
                </>
              ) : (
                <Input
                  className="w-96 font-mono"
                  placeholder="https://..."
                  value={c.url}
                  onChange={(e) => update(i, { url: e.target.value })}
                />
              )}
              <Button size="sm" variant="outline" onClick={() => test(c)}>
                测试
              </Button>
              <Button
                size="sm"
                variant="ghost"
                onClick={() => setChannels(channels.filter((_, j) => j !== i))}
              >
                删除
              </Button>
            </div>
          ))}
          <div>
            <Button
              size="sm"
              variant="outline"
              onClick={() =>
                setChannels([
                  ...channels,
                  create(AlertChannelSchema, { type: "webhook" }),
                ])
              }
            >
              添加渠道
            </Button>
          </div>
        </div>
        <div className="grid gap-2">
          <div className="font-medium">规则（留空用默认值）</div>
          <div className="flex flex-wrap gap-3">
            {ruleField("磁盘 %", "diskPercent", 90)}
            {ruleField("内存 %", "memPercent", 90)}
            {ruleField("内存持续分钟", "memMinutes", 5)}
            {ruleField("重启次数", "restartCount", 3)}
            {ruleField("重启窗口分钟", "restartMinutes", 10)}
            {ruleField("证书剩余天数", "certDays", 7)}
            {ruleField("失联秒数", "offlineSeconds", 60)}
            {ruleField("5xx 比例 %", "errorPercent", 5)}
          </div>
        </div>
        <div>
          <Button size="sm" onClick={save}>
            保存
          </Button>
        </div>
      </CardContent>
    </Card>
  );
}

