package api

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/pkg/i18n"
	"github.com/SIPC/nagi/internal/pkg/release"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestLiveProgressSpeed(t *testing.T) {
	start := time.Unix(1000, 0)
	at := func(sec float64) time.Time { return start.Add(time.Duration(sec * float64(time.Second))) }
	dl := func(src string, got int64) release.Progress {
		return release.Progress{Stage: release.StageDownload, Source: src, Downloaded: got, Total: 100 << 20}
	}
	var l liveProgress
	l.update(dl("ghfast.top", 0), at(0))
	if s := l.speed(); s != 0 {
		t.Errorf("只有一个点时速度应为 0：%d", s)
	}
	// 每秒 1 MiB。
	for i := 1; i <= 3; i++ {
		l.update(dl("ghfast.top", int64(i)<<20), at(float64(i)))
	}
	if s := l.speed(); s != 1<<20 {
		t.Errorf("速度应为 1 MiB/s：%d", s)
	}
	// 之后变成每秒 3 MiB：按最近 5 秒算，前面的慢速逐渐移出窗口。
	got := int64(3 << 20)
	for i := 4; i <= 12; i++ {
		got += 3 << 20
		l.update(dl("ghfast.top", got), at(float64(i)))
	}
	if s := l.speed(); s != 3<<20 {
		t.Errorf("窗口外的点应移出，速度应为 3 MiB/s：%d", s)
	}
	if len(l.samples) > int(speedWindow/time.Second)+2 {
		t.Errorf("留的点太多：%d", len(l.samples))
	}

	// 换地址：速度从头算。
	l.update(dl("github.com", 0), at(13))
	if s := l.speed(); s != 0 || l.Source != "github.com" || l.Downloaded != 0 {
		t.Errorf("换地址后应从头算：%+v，速度 %d", l.Progress, s)
	}
	l.update(dl("github.com", 2<<20), at(14))
	if s := l.speed(); s != 2<<20 {
		t.Errorf("换地址后的速度：%d", s)
	}
	// 核对、试运行时没有速度。
	l.update(release.Progress{Stage: release.StageVerify, Source: "github.com", Downloaded: 2 << 20, Total: 2 << 20}, at(14.1))
	if s := l.speed(); s != 0 {
		t.Errorf("核对时速度应为 0：%d", s)
	}

	// 两次进度隔得比窗口还久：仍按这两次算。
	var slow liveProgress
	slow.update(dl("a", 0), at(0))
	slow.update(dl("a", 10<<20), at(10))
	if s := slow.speed(); s != 1<<20 {
		t.Errorf("隔 10 秒下载 10 MiB，速度应为 1 MiB/s：%d", s)
	}
}

func TestFillProgress(t *testing.T) {
	var u panelUpdates
	installing := &updateState{Phase: updateInstalling}
	restarting := &updateState{Phase: updateRestarting, Source: "ghfast.top"}
	other := &updateState{Phase: updateInstalling}
	now := time.Unix(1000, 0)
	p := release.Progress{Stage: release.StageDownload, Source: "ghfast.top", Downloaded: 3 << 20, Total: 70 << 20}
	u.setProgress(installing, release.Progress{Stage: release.StageDownload, Source: "ghfast.top", Total: 70 << 20}, now)
	u.setProgress(installing, p, now.Add(time.Second))
	u.setProgress(restarting, p, now)

	out := updateStateProto(i18n.Zh, installing)
	u.fillProgress(installing, out)
	if out.GetStage() != "download" || out.GetSource() != "ghfast.top" || out.GetDownloaded() != 3<<20 || out.GetTotal() != 70<<20 || out.GetBytesPerSecond() != 3<<20 {
		t.Errorf("下载中的进度没有合进去：%v", out)
	}
	// 只有 installing 时合进去；没有进度的（旧版本的节点）不动。
	out = updateStateProto(i18n.Zh, restarting)
	u.fillProgress(restarting, out)
	if out.GetStage() != "" || out.GetSource() != "ghfast.top" {
		t.Errorf("重启中不应该带进度：%v", out)
	}
	out = updateStateProto(i18n.Zh, other)
	u.fillProgress(other, out)
	if out.GetStage() != "" || out.GetTotal() != 0 {
		t.Errorf("没有进度时不应该有进度字段：%v", out)
	}
	u.clearProgress(installing)
	out = updateStateProto(i18n.Zh, installing)
	u.fillProgress(installing, out)
	if out.GetStage() != "" {
		t.Errorf("清掉以后不应该有进度：%v", out)
	}
}

// fakeProgressAgent 只实现 UpgradeProgress。
type fakeProgressAgent struct {
	nagiv1.AgentServiceClient
	calls atomic.Int32
	err   error
	resp  *nagiv1.UpgradeProgressResponse
}

func (f *fakeProgressAgent) UpgradeProgress(context.Context, *nagiv1.UpgradeProgressRequest, ...grpc.CallOption) (*nagiv1.UpgradeProgressResponse, error) {
	f.calls.Add(1)
	return f.resp, f.err
}

func TestPollUpgradeProgress(t *testing.T) {
	if testing.Short() {
		t.Skip("要等几秒")
	}
	t.Run("记下节点报的进度", func(t *testing.T) {
		a := &API{}
		rec := &updateState{Phase: updateInstalling}
		f := &fakeProgressAgent{resp: &nagiv1.UpgradeProgressResponse{Stage: "download", Source: "github.com", Downloaded: 5, Total: 10}}
		ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
		defer cancel()
		a.pollUpgradeProgress(ctx, f, rec)
		out := &nagiv1.UpdateState{}
		a.updates.fillProgress(rec, out)
		if f.calls.Load() < 2 || out.GetStage() != "download" || out.GetDownloaded() != 5 || out.GetTotal() != 10 || out.GetSource() != "github.com" {
			t.Errorf("问了 %d 次，进度 %v", f.calls.Load(), out)
		}
	})
	t.Run("旧版本的节点不再问", func(t *testing.T) {
		a := &API{}
		rec := &updateState{Phase: updateInstalling}
		f := &fakeProgressAgent{err: status.Error(codes.Unimplemented, "unknown method")}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		start := time.Now()
		a.pollUpgradeProgress(ctx, f, rec)
		if f.calls.Load() != 1 || time.Since(start) > 3*time.Second {
			t.Errorf("Unimplemented 后应该马上停：问了 %d 次，用了 %v", f.calls.Load(), time.Since(start))
		}
		out := &nagiv1.UpdateState{}
		a.updates.fillProgress(rec, out)
		if out.GetStage() != "" {
			t.Errorf("旧版本的节点没有进度：%v", out)
		}
	})
	t.Run("出错和没有进度时接着问", func(t *testing.T) {
		a := &API{}
		rec := &updateState{Phase: updateInstalling}
		f := &fakeProgressAgent{err: status.Error(codes.Unavailable, "down")}
		ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
		defer cancel()
		a.pollUpgradeProgress(ctx, f, rec)
		if f.calls.Load() < 2 {
			t.Errorf("出错后应该接着问：%d 次", f.calls.Load())
		}
	})
}
