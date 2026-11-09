import { useState } from "react";
import { create } from "@bufbuild/protobuf";
import { AlertRulesSchema, type AlertRules } from "@/gen/nagi/v1/common_pb";
import { errorMessage } from "@/lib/api";
import { t } from "@/i18n";
import { Alert, Button, Check, Field, FormActions, FormBody, FormRow, Input, Spinner } from "@/components/ui";
import { useUnsaved } from "@/components/unsaved";

// 告警规则（见 docs/monitoring.md#告警）：节点的规则在节点设置里，容器的规则在项目设置里、容器可以单独设置。
// 每条规则一个开关，阈值留空用默认值（容器单独设置时，默认值是项目的）。

type Threshold = Exclude<keyof AlertRules, "enabled" | "$typeName" | "$unknown">;

type RuleDef = {
  key: string;
  title: string;
  fields: { key: Threshold; label: string; fallback: number; help?: string }[];
};

const r = t.alerts.rule;

export const nodeRules: RuleDef[] = [
  {
    key: "mem",
    title: r.mem,
    fields: [
      { key: "memPercent", label: r.memPercent, fallback: 90 },
      { key: "memMinutes", label: r.memMinutes, fallback: 5 },
    ],
  },
  { key: "disk", title: r.disk, fields: [{ key: "diskPercent", label: r.diskPercent, fallback: 90 }] },
  { key: "offline", title: r.offline, fields: [{ key: "offlineSeconds", label: r.offlineSeconds, fallback: 60 }] },
  { key: "cert", title: r.cert, fields: [{ key: "certDays", label: r.certDays, fallback: 7 }] },
];

export const containerRules: RuleDef[] = [
  {
    key: "restart",
    title: r.restart,
    fields: [
      { key: "restartCount", label: r.restartCount, fallback: 3 },
      { key: "restartMinutes", label: r.restartMinutes, fallback: 10 },
    ],
  },
  { key: "errors", title: r.errors, fields: [{ key: "errorPercent", label: r.errorPercent, fallback: 5, help: r.errorPercentHelp }] },
  { key: "health", title: r.health, fields: [] },
  { key: "snapshot", title: r.snapshot, fields: [] },
  { key: "backup", title: r.backup, fields: [] },
];

export const ruleOn = (rules: AlertRules | undefined, key: string) => rules?.enabled[key] !== false;

// rulesSummary：设置行里的一句概括。
export function rulesSummary(defs: RuleDef[], rules: AlertRules | undefined): string {
  const off = defs.filter((d) => !ruleOn(rules, d.key)).length;
  const custom = defs.some((d) => d.fields.some((f) => Number(rules?.[f.key] ?? 0) > 0));
  if (off === defs.length) return t.alerts.summary.allOff;
  if (off > 0) return t.alerts.summary.someOff(off);
  return custom ? t.alerts.summary.custom : t.alerts.summary.defaults;
}

// AlertRulesFields：一组规则的开关和阈值。inherited 是留空时用的值（容器沿用的项目规则），没有时用默认值。
export function AlertRulesFields({ id, defs, value, onChange, inherited, disabled }: { id: string; defs: RuleDef[]; value: AlertRules; onChange: (v: AlertRules) => void; inherited?: AlertRules; disabled?: boolean }) {
  const set = (patch: Partial<AlertRules>) => onChange(create(AlertRulesSchema, { ...value, ...patch }));
  return (
    <div className="grid gap-6">
      {defs.map((d) => {
        const on = ruleOn(value, d.key);
        return (
          <div key={d.key} className="grid gap-3">
            <Check checked={on} disabled={disabled} onChange={(v) => set({ enabled: { ...value.enabled, [d.key]: v } })}>
              {d.title}
            </Check>
            {d.fields.length > 0 && (
              <FormRow>
                {d.fields.map((f) => (
                  <Field key={f.key} label={f.label} htmlFor={`${id}-${f.key}`} help={f.help}>
                    <Input
                      id={`${id}-${f.key}`}
                      type="number"
                      min={0}
                      inputMode="numeric"
                      disabled={disabled || !on}
                      placeholder={t.alerts.defaultValue(Number(inherited?.[f.key] || 0) || f.fallback)}
                      value={Number(value[f.key] ?? 0) || ""}
                      onChange={(e) => set({ [f.key]: Number(e.target.value) || 0 })}
                    />
                  </Field>
                ))}
              </FormRow>
            )}
          </div>
        );
      })}
    </div>
  );
}

// AlertRulesForm：规则加保存按钮。onSave 抛出的错误显示在按钮上方。
export function AlertRulesForm({ id, defs, initial, inherited, onSave, onCancel, disabled }: { id: string; defs: RuleDef[]; initial: AlertRules | undefined; inherited?: AlertRules; onSave: (v: AlertRules) => Promise<void>; onCancel?: () => void; disabled?: boolean }) {
  const [value, setValue] = useState<AlertRules>(initial ?? create(AlertRulesSchema));
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const dirty = useUnsaved(value);
  const save = async () => {
    setBusy(true);
    setError("");
    try {
      await onSave(value);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };
  return (
    <form
      onSubmit={(e) => {
        e.preventDefault();
        save();
      }}
    >
      <FormBody className="gap-6">
        <AlertRulesFields id={id} defs={defs} value={value} onChange={setValue} inherited={inherited} disabled={disabled} />
        {error && <Alert>{error}</Alert>}
        {!disabled && (
          <FormActions>
            {onCancel && (
              <Button variant="outline" readOnlyOk onClick={onCancel}>
                {t.common.actions.cancel}
              </Button>
            )}
            <Button type="submit" disabled={busy || (!dirty && !onCancel)}>
              {busy && <Spinner />}
              {t.common.actions.save}
            </Button>
          </FormActions>
        )}
      </FormBody>
    </form>
  );
}
