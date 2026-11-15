package spec

import (
	"fmt"
	"slices"
	"strings"
	"time"
	// 节点上不一定有时区数据（例如精简的容器镜像），按固定时刻快照要用。
	_ "time/tzdata"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/protobuf/proto"
)

// 项目设置里快照间隔和份数上限留空（0）时的默认值，见 docs/snapshot.md。
const (
	DefaultSnapshotIntervalMinutes = 720
	DefaultSnapshotLimit           = 10
	// MaxSnapshotIntervalMinutes 是定时快照间隔的上限（7 天）。
	MaxSnapshotIntervalMinutes = 7 * 24 * 60
	// MaxSnapshotTimes 是每天固定时刻的个数上限。
	MaxSnapshotTimes = 24
)

// SnapshotIntervalMinutes 返回生效的定时快照间隔（分钟）。
func SnapshotIntervalMinutes(n uint32) uint32 {
	if n == 0 {
		return DefaultSnapshotIntervalMinutes
	}
	return n
}

// SnapshotLimit 返回生效的快照份数上限。
func SnapshotLimit(n uint32) uint32 {
	if n == 0 {
		return DefaultSnapshotLimit
	}
	return n
}

// CleanSnapshotSchedule 校验并整理定时快照计划：时刻统一写成 HH:MM、排序去重，时区要认得。
func CleanSnapshotSchedule(in *nagiv1.SnapshotSchedule) (*nagiv1.SnapshotSchedule, error) {
	out := &nagiv1.SnapshotSchedule{Disabled: in.GetDisabled(), IntervalMinutes: in.GetIntervalMinutes(), TimeZone: strings.TrimSpace(in.GetTimeZone())}
	if out.IntervalMinutes > MaxSnapshotIntervalMinutes {
		return nil, Invalid("定时快照间隔不能超过 7 天")
	}
	for _, t := range in.GetTimes() {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		h, m, ok := parseClock(t)
		if !ok {
			return nil, Invalid("快照时刻 %s 须为 24 小时制的 HH:MM，例如 03:00", t)
		}
		out.Times = append(out.Times, fmt.Sprintf("%02d:%02d", h, m))
	}
	slices.Sort(out.Times)
	out.Times = slices.Compact(out.Times)
	if len(out.Times) > MaxSnapshotTimes {
		return nil, Invalid("每天最多 %d 个快照时刻", MaxSnapshotTimes)
	}
	if out.TimeZone != "" {
		if _, err := time.LoadLocation(out.TimeZone); err != nil {
			return nil, Invalid("不认识的时区 %s", out.TimeZone)
		}
	}
	return out, nil
}

// parseClock 解析 HH:MM（小时可以是一位）。
func parseClock(s string) (h, m int, ok bool) {
	hs, ms, found := strings.Cut(s, ":")
	if !found || len(hs) < 1 || len(hs) > 2 || len(ms) != 2 {
		return 0, 0, false
	}
	if _, err := fmt.Sscanf(hs+" "+ms, "%d %d", &h, &m); err != nil {
		return 0, 0, false
	}
	for _, c := range hs + ms {
		if c < '0' || c > '9' {
			return 0, 0, false
		}
	}
	return h, m, h < 24 && m < 60
}

// SnapshotScheduleOf 返回容器配置里生效的定时快照计划。旧版本的面板没有下发计划，按间隔。
func SnapshotScheduleOf(s *nagiv1.ContainerSpec) *nagiv1.SnapshotSchedule {
	if sch := s.GetSnapshotSchedule(); sch != nil {
		return sch
	}
	return &nagiv1.SnapshotSchedule{IntervalMinutes: s.GetSnapshotIntervalMinutes()}
}

// ProjectSnapshotSchedule 返回项目设置里的定时快照计划，没有时按旧的间隔设置。
func ProjectSnapshotSchedule(s *nagiv1.ProjectSettings) *nagiv1.SnapshotSchedule {
	if sch := s.GetSnapshotSchedule(); sch != nil {
		return proto.CloneOf(sch)
	}
	return &nagiv1.SnapshotSchedule{IntervalMinutes: s.GetSnapshotIntervalMinutes()}
}

// LegacySnapshotInterval 把计划换算成旧版本节点认得的间隔（分钟）：每天固定时刻按平均间隔，关闭时用最长的间隔。
func LegacySnapshotInterval(sch *nagiv1.SnapshotSchedule) uint32 {
	switch {
	case sch.GetDisabled():
		return MaxSnapshotIntervalMinutes
	case len(sch.GetTimes()) > 0:
		return uint32(24 * 60 / len(sch.GetTimes()))
	}
	return sch.GetIntervalMinutes()
}

// SnapshotGap 是计划里两份定时快照之间大致的间隔（失败重试的上限按它）。fast 不为 0 时（设置了顶替）不超过 fast。
func SnapshotGap(sch *nagiv1.SnapshotSchedule, fast time.Duration) time.Duration {
	var gap time.Duration
	switch {
	case sch.GetDisabled():
		gap = 24 * time.Hour
	case len(sch.GetTimes()) > 0:
		gap = 24 * time.Hour / time.Duration(len(sch.GetTimes()))
	default:
		gap = time.Duration(SnapshotIntervalMinutes(sch.GetIntervalMinutes())) * time.Minute
	}
	if fast > 0 && (gap > fast || sch.GetDisabled()) {
		gap = fast
	}
	return gap
}

// SnapshotDue 判断上一份定时快照在 last、现在是 now 时，是否该做下一份：
//   - 按间隔：距上一份满了间隔；
//   - 每天固定时刻：上一份之后又过了一个时刻（节点离线错过的时刻，上线后补一份）；
//   - 关闭：不做。
//
// fast 不为 0 时（设置了顶替）另外每隔 fast 一份，关闭了也照做。
func SnapshotDue(sch *nagiv1.SnapshotSchedule, last, now time.Time, fast time.Duration) bool {
	if fast > 0 && now.Sub(last) >= fast {
		return true
	}
	if sch.GetDisabled() {
		return false
	}
	if len(sch.GetTimes()) > 0 {
		p := LastSnapshotTime(sch, now)
		return !p.IsZero() && p.After(last)
	}
	return now.Sub(last) >= time.Duration(SnapshotIntervalMinutes(sch.GetIntervalMinutes()))*time.Minute
}

// LastSnapshotTime 返回 now 及以前最近的一个固定时刻，没有设置时刻时为零值。
func LastSnapshotTime(sch *nagiv1.SnapshotSchedule, now time.Time) time.Time {
	loc := time.UTC
	if l, err := time.LoadLocation(sch.GetTimeZone()); err == nil && sch.GetTimeZone() != "" {
		loc = l
	}
	local := now.In(loc)
	var best time.Time
	for day := 0; day >= -1; day-- {
		y, mo, d := local.AddDate(0, 0, day).Date()
		for _, t := range sch.GetTimes() {
			h, m, ok := parseClock(t)
			if !ok {
				continue
			}
			p := time.Date(y, mo, d, h, m, 0, 0, loc)
			if !p.After(now) && p.After(best) {
				best = p
			}
		}
		if !best.IsZero() {
			return best
		}
	}
	return best
}
