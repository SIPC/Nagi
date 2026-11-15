package spec

import (
	"slices"
	"testing"
	"time"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

func TestCleanSnapshotSchedule(t *testing.T) {
	got, err := CleanSnapshotSchedule(&nagiv1.SnapshotSchedule{Times: []string{"15:00", " 3:05", "", "15:00"}, TimeZone: "Asia/Shanghai"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.GetTimes(), []string{"03:05", "15:00"}) {
		t.Errorf("时刻没有整理好：%v", got.GetTimes())
	}
	for _, bad := range []*nagiv1.SnapshotSchedule{
		{Times: []string{"24:00"}},
		{Times: []string{"3:5"}},
		{Times: []string{"ab:cd"}},
		{Times: []string{"03:00"}, TimeZone: "Mars/Base"},
		{IntervalMinutes: MaxSnapshotIntervalMinutes + 1},
	} {
		if _, err := CleanSnapshotSchedule(bad); err == nil {
			t.Errorf("%v 应该报错", bad)
		}
	}
}

func TestSnapshotDue(t *testing.T) {
	sh, _ := time.LoadLocation("Asia/Shanghai")
	at := func(s string) time.Time {
		v, err := time.ParseInLocation("2006-01-02 15:04", s, sh)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	daily := &nagiv1.SnapshotSchedule{Times: []string{"03:00", "15:00"}, TimeZone: "Asia/Shanghai"}
	cases := []struct {
		desc      string
		sch       *nagiv1.SnapshotSchedule
		last, now string
		fast      time.Duration
		want      bool
	}{
		{"按间隔：没到", &nagiv1.SnapshotSchedule{IntervalMinutes: 60}, "2026-11-15 10:00", "2026-11-15 10:59", 0, false},
		{"按间隔：到了", &nagiv1.SnapshotSchedule{IntervalMinutes: 60}, "2026-11-15 10:00", "2026-11-15 11:00", 0, true},
		{"默认间隔 720 分钟", &nagiv1.SnapshotSchedule{}, "2026-11-15 00:00", "2026-11-15 11:59", 0, false},
		{"每天定时：还没到下一个时刻", daily, "2026-11-15 03:00", "2026-11-15 14:59", 0, false},
		{"每天定时：到了 15:00", daily, "2026-11-15 03:00", "2026-11-15 15:00", 0, true},
		{"每天定时：手动快照在时刻之前，时刻到了照做", daily, "2026-11-15 14:58", "2026-11-15 15:01", 0, true},
		{"每天定时：跨天", daily, "2026-11-15 15:00", "2026-11-16 02:59", 0, false},
		{"每天定时：离线错过了，上线补一份", daily, "2026-11-14 15:00", "2026-11-15 10:00", 0, true},
		{"关闭", &nagiv1.SnapshotSchedule{Disabled: true}, "2026-11-01 00:00", "2026-11-15 00:00", 0, false},
		{"关闭但设置了顶替", &nagiv1.SnapshotSchedule{Disabled: true}, "2026-11-15 10:00", "2026-11-15 10:15", 15 * time.Minute, true},
		{"每天定时加顶替：15 分钟一份", daily, "2026-11-15 10:00", "2026-11-15 10:15", 15 * time.Minute, true},
	}
	for _, c := range cases {
		if got := SnapshotDue(c.sch, at(c.last), at(c.now), c.fast); got != c.want {
			t.Errorf("%s：得到 %v", c.desc, got)
		}
	}
	// 时刻按计划里的时区算，和节点的时区无关：上海 03:00 是 UTC 前一天 19:00。
	if got := LastSnapshotTime(daily, time.Date(2026, 11, 14, 19, 30, 0, 0, time.UTC)); !got.Equal(time.Date(2026, 11, 14, 19, 0, 0, 0, time.UTC)) {
		t.Errorf("LastSnapshotTime = %v", got)
	}
	// 没有时区时按 UTC。
	if got := LastSnapshotTime(&nagiv1.SnapshotSchedule{Times: []string{"03:00"}}, time.Date(2026, 11, 15, 2, 0, 0, 0, time.UTC)); !got.Equal(time.Date(2026, 11, 14, 3, 0, 0, 0, time.UTC)) {
		t.Errorf("UTC LastSnapshotTime = %v", got)
	}
}

func TestLegacySnapshotInterval(t *testing.T) {
	cases := []struct {
		sch  *nagiv1.SnapshotSchedule
		want uint32
	}{
		{&nagiv1.SnapshotSchedule{IntervalMinutes: 90}, 90},
		{&nagiv1.SnapshotSchedule{Times: []string{"03:00", "15:00"}}, 720},
		{&nagiv1.SnapshotSchedule{Disabled: true, IntervalMinutes: 60}, MaxSnapshotIntervalMinutes},
	}
	for _, c := range cases {
		if got := LegacySnapshotInterval(c.sch); got != c.want {
			t.Errorf("%v：得到 %d，应为 %d", c.sch, got, c.want)
		}
	}
	if got := SnapshotGap(&nagiv1.SnapshotSchedule{Disabled: true}, 15*time.Minute); got != 15*time.Minute {
		t.Errorf("关闭但设置了顶替的间隔 = %v", got)
	}
}
