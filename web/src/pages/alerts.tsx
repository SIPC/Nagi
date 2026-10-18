import { useEffect, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { errorMessage, nodeClient, settingsClient } from "@/lib/api";
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

// ---- 节点监控（临时测试界面）----

export function Sparkline({ values }: { values: number[] }) {
  if (values.length < 2)
    return <span className="text-muted-foreground">—</span>;
  const max = Math.max(...values, 1);
  const pts = values
    .map((v, i) => `${(i / (values.length - 1)) * 120},${24 - (v / max) * 22}`)
    .join(" ");
  return (
    <svg width="120" height="24" className="inline-block align-middle">
      <polyline
        points={pts}
        fill="none"
        stroke="currentColor"
        strokeWidth="1.5"
      />
    </svg>
  );
}

export function fmtBytes(v: number) {
  const units = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(1)} ${units[i]}`;
}

export function NodeMetrics({ nodeId }: { nodeId: string }) {
  const q = useQuery({
    queryKey: ["node-metrics", nodeId],
    queryFn: () =>
      nodeClient.queryNodeMetrics({
        nodeId,
        prefix: "node:",
        from: timestampFromDate(new Date(Date.now() - 3600_000)),
      }),
    refetchInterval: 10_000,
  });
  if (q.isPending) return <Spinner />;
  if (q.error) return <Alert>{errorMessage(q.error)}</Alert>;
  const series = Object.fromEntries(
    q.data.series.map((s) => [s.name, s.values]),
  );
  const last = (n: string) => series[n]?.[series[n].length - 1];
  const row = (label: string, name: string, fmt: (v: number) => string) => (
    <div className="flex items-center gap-3">
      <span className="w-20 text-muted-foreground">{label}</span>
      <span className="w-24 font-mono">
        {last(name) === undefined ? "—" : fmt(last(name)!)}
      </span>
      <Sparkline values={series[name] ?? []} />
    </div>
  );
  return (
    <div className="grid gap-1 py-2 text-xs">
      {row("CPU", "node:cpu", (v) => `${v.toFixed(1)}%`)}
      {row("内存", "node:mem", (v) => `${v.toFixed(1)}%`)}
      {row("磁盘", "node:disk", (v) => `${v.toFixed(1)}%`)}
      {row("下行", "node:net_rx", (v) => `${fmtBytes(v)}/s`)}
      {row("上行", "node:net_tx", (v) => `${fmtBytes(v)}/s`)}
    </div>
  );
}

// 项目的容器和对外访问规则的监控（最近 1 小时）。
export function ProjectMetrics({
  nodeId,
  projectId,
}: {
  nodeId: string;
  projectId: string;
}) {
  const from = () => timestampFromDate(new Date(Date.now() - 3600_000));
  const q = useQuery({
    queryKey: ["project-metrics", nodeId, projectId],
    queryFn: async () => {
      const [c, r] = await Promise.all([
        nodeClient.queryNodeMetrics({
          nodeId,
          prefix: `c:${projectId}/`,
          from: from(),
        }),
        nodeClient.queryNodeMetrics({
          nodeId,
          prefix: `r:${projectId}/`,
          from: from(),
        }),
      ]);
      return [...c.series, ...r.series];
    },
    refetchInterval: 10_000,
  });
  if (q.isPending) return <Spinner />;
  if (q.error) return <Alert>{errorMessage(q.error)}</Alert>;
  // 序列名形如 c:<项目>/<服务>:cpu、r:<项目>/<规则>:req，按服务/规则分组。
  const groups = new Map<string, Map<string, number[]>>();
  for (const s of q.data) {
    const rest = s.name.slice(s.name.indexOf("/") + 1);
    const i = rest.lastIndexOf(":");
    const key = `${s.name[0] === "c" ? "容器" : "规则"} ${rest.slice(0, i)}`;
    if (!groups.has(key)) groups.set(key, new Map());
    groups.get(key)!.set(rest.slice(i + 1), s.values);
  }
  if (groups.size === 0)
    return <div className="text-sm text-muted-foreground">还没有数据</div>;
  const fmt: Record<string, [string, (v: number) => string]> = {
    cpu: ["CPU", (v) => `${v.toFixed(1)}%`],
    mem: ["内存", fmtBytes],
    net_rx: ["下行", (v) => `${fmtBytes(v)}/s`],
    net_tx: ["上行", (v) => `${fmtBytes(v)}/s`],
    restarts: ["重启", (v) => String(v)],
    req: ["请求", (v) => `${v.toFixed(2)}/s`],
    "5xx": ["5xx", (v) => `${v.toFixed(2)}/s`],
    lat: ["耗时", (v) => `${v.toFixed(0)} ms`],
  };
  return (
    <div className="grid gap-3 text-xs">
      {[...groups.entries()].map(([name, series]) => (
        <div key={name}>
          <div className="mb-1 font-medium">{name}</div>
          {[...series.entries()].map(([kind, values]) => (
            <div key={kind} className="flex items-center gap-3">
              <span className="w-12 text-muted-foreground">
                {fmt[kind]?.[0] ?? kind}
              </span>
              <span className="w-24 font-mono">
                {(fmt[kind]?.[1] ?? String)(values[values.length - 1])}
              </span>
              <Sparkline values={values} />
            </div>
          ))}
        </div>
      ))}
    </div>
  );
}
