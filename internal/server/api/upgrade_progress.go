package api

import (
	"context"
	"time"

	"github.com/SIPC/nagi/internal/pkg/release"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 下载进度（见 docs/panel.md#更新状态）：面板自己的更新由 release.WithProgress 回调，节点的升级由面板每秒问一次节点
// （agent 的 UpgradeProgress）。进度每秒都在变，只放在内存里，不写 settings 表；面板重启后进行中的更新本来就算失败了。

const (
	// progressPollInterval 是面板问节点升级进度的间隔。
	progressPollInterval = time.Second
	// speedWindow：速度按最近这么久的下载量算。
	speedWindow = 5 * time.Second
)

// liveProgress 是一次更新最新的进度和最近几秒的下载量。
type liveProgress struct {
	release.Progress
	samples []progressSample
}

type progressSample struct {
	at  time.Time
	got int64
}

// update 记下新的进度。换了地址、阶段或者已下载的变少了（重新下载）时，速度从头算。
func (l *liveProgress) update(p release.Progress, now time.Time) {
	if p.Source != l.Source || p.Stage != l.Stage || p.Downloaded < l.Downloaded {
		l.samples = l.samples[:0]
	}
	l.Progress = p
	if p.Stage != release.StageDownload {
		return
	}
	l.samples = append(l.samples, progressSample{now, p.Downloaded})
	// 只留窗口内的，再加窗口外最近的一个，免得两次进度隔得比窗口还久时算不出速度。
	i := 0
	for i < len(l.samples)-2 && now.Sub(l.samples[i+1].at) >= speedWindow {
		i++
	}
	l.samples = l.samples[i:]
}

// speed 是最近几秒的下载速度（字节/秒），算不出时为 0。
func (l *liveProgress) speed() int64 {
	if l.Stage != release.StageDownload || len(l.samples) < 2 {
		return 0
	}
	first, last := l.samples[0], l.samples[len(l.samples)-1]
	d := last.at.Sub(first.at)
	if d < 500*time.Millisecond {
		return 0
	}
	return int64(float64(last.got-first.got) / d.Seconds())
}

// fill 把进度合进给界面的状态。
func (l *liveProgress) fill(out *nagiv1.UpdateState) {
	out.Stage, out.Source = string(l.Stage), l.Source
	out.Downloaded, out.Total, out.BytesPerSecond = l.Downloaded, l.Total, l.speed()
}

// setProgress 记下一次更新（按记录找）的最新进度。
func (u *panelUpdates) setProgress(rec *updateState, p release.Progress, now time.Time) {
	u.progressMu.Lock()
	defer u.progressMu.Unlock()
	if u.progress == nil {
		u.progress = map[*updateState]*liveProgress{}
	}
	l := u.progress[rec]
	if l == nil {
		l = &liveProgress{}
		u.progress[rec] = l
	}
	l.update(p, now)
}

func (u *panelUpdates) clearProgress(rec *updateState) {
	u.progressMu.Lock()
	defer u.progressMu.Unlock()
	delete(u.progress, rec)
}

// fillProgress 在 installing 时把记录的进度合进 out，没有进度时不动。
func (u *panelUpdates) fillProgress(rec *updateState, out *nagiv1.UpdateState) {
	if rec.Phase != updateInstalling {
		return
	}
	u.progressMu.Lock()
	defer u.progressMu.Unlock()
	if l := u.progress[rec]; l != nil {
		l.fill(out)
	}
}

// pollUpgradeProgress 在节点升级（等 Upgrade 返回）期间每秒问一次进度，记在 rec 名下，ctx 结束时返回。
// 旧版本的节点没有 UpgradeProgress（Unimplemented），不再问；其他错误下一秒再试。
func (a *API) pollUpgradeProgress(ctx context.Context, agent nagiv1.AgentServiceClient, rec *updateState) {
	t := time.NewTicker(progressPollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		p, err := agent.UpgradeProgress(qctx, &nagiv1.UpgradeProgressRequest{})
		cancel()
		switch {
		case status.Code(err) == codes.Unimplemented:
			return
		case err != nil:
			continue
		}
		if np, ok := nodeProgress(p); ok {
			a.updates.setProgress(rec, np, time.Now())
		}
	}
}

const (
	// maxNodeSourceBytes 是节点报的下载地址（主机名）的上限，Upgrade 和 UpgradeProgress 都按它截。
	maxNodeSourceBytes = 255
	// maxNodeProgressBytes 是节点报的已下载、总大小的上限。Nagi 的二进制只有几十 MiB，
	// 超过的按上限算（总大小按未知），速度和百分比不会溢出。
	maxNodeProgressBytes = 1 << 30
)

// nodeSource 截短节点报的下载地址：值来自节点，原样显示给界面和 MCP，还会存进 settings 表。
func nodeSource(s string) string { return clipBytes(s, maxNodeSourceBytes) }

// nodeProgress 把节点报的进度整理成能显示的：只认已知的阶段，地址限长，
// 字节数不能是负的，也不能超过 maxNodeProgressBytes（总大小未知时为 -1）。不认识的阶段返回 false。
func nodeProgress(p *nagiv1.UpgradeProgressResponse) (release.Progress, bool) {
	stage := release.Stage(p.GetStage())
	if stage != release.StageDownload && stage != release.StageVerify && stage != release.StageTest {
		return release.Progress{}, false
	}
	total := max(p.GetTotal(), -1)
	if total == 0 || total > maxNodeProgressBytes {
		total = -1
	}
	return release.Progress{Stage: stage, Source: nodeSource(p.GetSource()),
		Downloaded: min(max(p.GetDownloaded(), 0), maxNodeProgressBytes), Total: total}, true
}
