// 定时快照的计划（见 docs/snapshot.md#定时快照）：开关，按间隔或每天定时。项目设置和容器的「存储」标签共用。
import type { MessageInitShape } from "@bufbuild/protobuf";
import type { ProjectSettings, SnapshotSchedule, SnapshotScheduleSchema } from "@/gen/nagi/v1/common_pb";
import { Check, Collapse, Field, FormRow, Input, Select } from "@/components/ui";
import { defaultSnapshotIntervalMinutes } from "@/lib/config";
import { t } from "@/i18n";

export type ScheduleForm = { enabled: boolean; mode: "interval" | "daily"; interval: string; times: string };
type ScheduleLike = Pick<SnapshotSchedule, "disabled" | "intervalMinutes" | "times" | "timeZone">;

// projectSchedule：项目设置里的计划。旧版本面板保存的项目只有间隔。
export function projectSchedule(s?: ProjectSettings): ScheduleLike {
  return s?.snapshotSchedule ?? { disabled: false, intervalMinutes: s?.snapshotIntervalMinutes ?? 0, times: [], timeZone: "" };
}

// browserTimeZone：浏览器所在的时区，保存每天定时的计划时记下它，节点按它计算。
export function browserTimeZone() {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
  } catch {
    return "UTC";
  }
}

export function scheduleForm(sch?: ScheduleLike): ScheduleForm {
  return {
    enabled: !sch?.disabled,
    mode: sch?.times.length ? "daily" : "interval",
    interval: sch?.intervalMinutes ? String(sch.intervalMinutes) : "",
    times: sch?.times.join(", ") ?? "",
  };
}

// 时刻之间可以用空白、英文逗号、全角逗号（U+FF0C）或顿号（U+3001）分隔。
const splitTimes = (v: string) => v.split(/[\s,\uFF0C\u3001]+/).filter(Boolean);
const clockRe = /^([01]?\d|2[0-3]):[0-5]\d$/;

// scheduleErrors：表单里填错的地方。关闭时不检查。
export function scheduleErrors(f: ScheduleForm): { interval?: string; times?: string } {
  if (!f.enabled) return {};
  const m = t.projectData.schedule;
  if (f.mode === "interval") {
    const v = f.interval.trim();
    return v && !/^[1-9]\d*$/.test(v) ? { interval: t.projectSettings.general.positiveInteger } : {};
  }
  const times = splitTimes(f.times);
  if (times.length === 0) return { times: m.timesRequired };
  const bad = times.find((x) => !clockRe.test(x));
  if (bad) return { times: m.timesInvalid(bad) };
  if (new Set(times).size > 24) return { times: m.tooManyTimes };
  return {};
}

// toSchedule：表单转成要提交的计划。另一种方式下填的值也带上（关掉后再打开、换回来时还在），每天定时的按浏览器时区。
export function toSchedule(f: ScheduleForm, prev?: ScheduleLike): MessageInitShape<typeof SnapshotScheduleSchema> {
  const daily = f.mode === "daily";
  return {
    disabled: !f.enabled,
    intervalMinutes: Number(f.interval.trim() || 0),
    times: daily ? splitTimes(f.times) : [],
    timeZone: daily ? browserTimeZone() : (prev?.timeZone ?? ""),
  };
}

// sameForm：两份表单是否相同（判断有没有未保存的修改）。
export function sameForm(a: ScheduleForm, b: ScheduleForm) {
  return a.enabled === b.enabled && a.mode === b.mode && a.interval.trim() === b.interval.trim() && splitTimes(a.times).join() === splitTimes(b.times).join();
}

// interval：间隔，整小时的写成小时。
const intervalText = (min: number) => (min % 60 === 0 ? t.common.time.hours(min / 60) : t.common.time.minutes(min));

// scheduleSummary：一句话说明计划，例如「每 12 小时自动快照」「每天 03:00、15:00 自动快照」。时区和浏览器的不同时注明。
export function scheduleSummary(sch?: ScheduleLike) {
  const m = t.projectData.schedule;
  if (sch?.disabled) return m.off;
  if (sch?.times.length) {
    const tz = sch.timeZone || "UTC";
    return m.dailyAt(t.common.list(sch.times)) + (tz === browserTimeZone() ? "" : m.zone(tz));
  }
  return m.every(intervalText(sch?.intervalMinutes || defaultSnapshotIntervalMinutes));
}

// ScheduleFields：开关、方式和间隔或时刻。note 是关闭时的说明（顶替、备份节点）。
export function ScheduleFields({
  id,
  value,
  onChange,
  errors,
  disabled,
  timeZone,
  note,
}: {
  id: string;
  value: ScheduleForm;
  onChange: (f: ScheduleForm) => void;
  errors: { interval?: string; times?: string };
  disabled?: boolean;
  // timeZone：显示每天定时按哪个时区算。编辑时是浏览器的（保存时记下它）；只读显示时是已保存的。
  timeZone?: string;
  note?: React.ReactNode;
}) {
  const m = t.projectData.schedule;
  // 开关切换时字段和说明各自用 Collapse 过渡，不直接换掉整行。间距放在里面，收起后不留空。
  return (
    <div className="grid">
      <Check className="justify-self-start" checked={value.enabled} disabled={disabled} onChange={(enabled) => onChange({ ...value, enabled })}>
        {m.enabled}
      </Check>
      <Collapse open={value.enabled}>
        <FormRow className="pt-4">
          <Field label={m.mode} htmlFor={`${id}-mode`}>
            <Select id={`${id}-mode`} value={value.mode} disabled={disabled} onChange={(e) => onChange({ ...value, mode: e.target.value as ScheduleForm["mode"] })}>
              <option value="interval">{m.interval}</option>
              <option value="daily">{m.daily}</option>
            </Select>
          </Field>
          {value.mode === "interval" ? (
            <Field label={m.intervalMinutes} htmlFor={`${id}-interval`} hint={m.intervalHint} error={errors.interval}>
              <Input
                id={`${id}-interval`}
                value={value.interval}
                disabled={disabled}
                onChange={(e) => onChange({ ...value, interval: e.target.value })}
                inputMode="numeric"
                autoComplete="off"
                placeholder={String(defaultSnapshotIntervalMinutes)}
              />
            </Field>
          ) : (
            <Field label={m.times} htmlFor={`${id}-times`} hint={m.timesHint(timeZone ?? browserTimeZone())} error={errors.times}>
              <Input
                id={`${id}-times`}
                value={value.times}
                disabled={disabled}
                onChange={(e) => onChange({ ...value, times: e.target.value })}
                autoComplete="off"
                spellCheck={false}
                placeholder="03:00, 15:00"
              />
            </Field>
          )}
        </FormRow>
      </Collapse>
      {note && (
        <Collapse open={!value.enabled}>
          <p className="pt-4 leading-5 text-muted-foreground">{note}</p>
        </Collapse>
      )}
    </div>
  );
}
