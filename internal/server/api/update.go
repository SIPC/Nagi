package api

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/i18n"
	"github.com/SIPC/nagi/internal/pkg/release"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/auth"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// 一键更新面板（见 docs/panel.md#更新面板）：和节点的一键升级一样，从 GitHub Release（安装时指定了镜像就从镜像）
// 下载，核对后替换可执行文件，然后原地重启。内嵌的节点在同一个进程里，一起更新。进度见 upgrades.go。

// updateCheckTTL 是最新版本号的缓存时间：每次打开页面都会查，不能每次都去问 GitHub。
const updateCheckTTL = time.Hour

type panelUpdates struct {
	mu      sync.Mutex
	latest  string
	checked time.Time
	// channel：latest 是哪个更新渠道的最新版本（见 channels.go）。渠道换了就不用缓存。
	channel string
	// via：latest 是经这个加速代理查到的（主机名），直接从 GitHub 查到时为空（见 release.Latest）。
	via string
	// running：正在更新。成功以后进程被替换，不会再回到 false。
	running atomic.Bool

	// 更新状态（见 upgrades.go）。
	stateMu sync.Mutex
	state   updateRecords
	loaded  bool
	saved   string
	// cancels：正在下载的节点升级的取消函数，按记录找。在 stateMu 里读写。
	cancels map[*updateState]context.CancelFunc
	// progress：进行中的更新的下载进度，按记录找，只在内存里（见 upgrade_progress.go）。在 progressMu 里读写。
	progressMu sync.Mutex
	progress   map[*updateState]*liveProgress

	// notes：各语言最新版本的更新日志，版本变了再下载。
	notesMu sync.Mutex
	notes   map[i18n.Lang]changelog
}

type changelog struct{ version, text string }

// changelog 返回 version 的更新日志，同一版本只下载一次。
func (u *panelUpdates) changelog(ctx context.Context, base, version string, lang i18n.Lang) (string, error) {
	u.notesMu.Lock()
	defer u.notesMu.Unlock()
	if c, ok := u.notes[lang]; ok && c.version == version {
		return c.text, nil
	}
	text, err := release.FetchChangelog(ctx, base, release.ChangelogName(lang))
	if err != nil {
		return "", err
	}
	if u.notes == nil {
		u.notes = map[i18n.Lang]changelog{}
	}
	u.notes[lang] = changelog{version, text}
	return text, nil
}

// get 返回渠道 channel 最新发布的版本号，force 时不用缓存。连不上 GitHub 时经 proxy（安装时用上的加速代理，可以为空）查。
func (u *panelUpdates) get(ctx context.Context, force bool, channel, proxy string) (string, error) {
	c, err := u.getChecked(ctx, force, channel, proxy)
	return c.version, err
}

// forget 清掉缓存的最新版本（换了渠道）。
func (u *panelUpdates) forget() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.latest, u.via, u.channel = "", "", ""
}

// latestCheck 是一次最新版本的查询结果。
type latestCheck struct {
	version string
	// via：经这个加速代理查到的（主机名），直接从 GitHub 查到时为空。
	via     string
	checked time.Time
}

// getChecked 和 get 一样，另外返回查询的时间和经哪个加速代理查到。
func (u *panelUpdates) getChecked(ctx context.Context, force bool, channel, proxy string) (latestCheck, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if !force && u.latest != "" && u.channel == channel && time.Since(u.checked) < updateCheckTTL {
		return latestCheck{u.latest, u.via, u.checked}, nil
	}
	v, via, err := latestFunc(ctx, channel, proxy)
	if err != nil {
		return latestCheck{}, err
	}
	// 渠道认不出预发布版时（正式版渠道、经代理查到的结果被篡改）不当作可以更新的版本。
	if !release.Allowed(channel, v) {
		return latestCheck{}, i18n.Errorf("查到的版本 %s 不属于当前更新渠道，请稍后重试", v)
	}
	u.latest, u.via, u.checked, u.channel = v, via, time.Now(), channel
	return latestCheck{v, via, u.checked}, nil
}

// latestFunc 是 release.LatestIn，测试里换掉。
var latestFunc = release.LatestIn

// updateUnsupported 返回本面板不能一键更新的原因，能更新时返回空。发布的文件只有 Linux 版，本地编译的版本没有对应的发布文件。
func (p *PanelInfo) updateUnsupported() error {
	if !release.IsVersion(p.Version) {
		return i18n.New("这是本地编译的版本，无法一键更新")
	}
	if runtime.GOOS != "linux" {
		return i18n.New("只有运行在 Linux 上的面板能一键更新")
	}
	return nil
}

func (s *settingsService) CheckPanelUpdate(ctx context.Context, req *connect.Request[nagiv1.CheckPanelUpdateRequest]) (*connect.Response[nagiv1.CheckPanelUpdateResponse], error) {
	out := &nagiv1.CheckPanelUpdateResponse{Current: s.Panel.Version}
	if err := s.Panel.updateUnsupported(); err != nil {
		out.Unsupported = i18n.Localize(i18n.FromContext(ctx), err)
		return connect.NewResponse(out), nil
	}
	// 让面板马上联网检查（force）只给管理员（C2）；其他成员拿到的是缓存的结果。
	force := req.Msg.GetForce() && auth.CurrentUser(ctx).IsAdmin()
	out.Channel = s.Panel.UpdateChannel(ctx)
	c, err := s.updates.getChecked(ctx, force, out.Channel, release.ProxyFor(s.Panel.DataDir))
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	out.Latest, out.ReleaseUrl, out.CheckedAt = c.version, release.PageURL(c.version), timestamppb.New(c.checked)
	out.Available = release.Older(s.Panel.Version, c.version)
	out.LatestSource = c.via
	return connect.NewResponse(out), nil
}

func (s *settingsService) GetPanelReleaseNotes(ctx context.Context, _ *connect.Request[nagiv1.GetPanelReleaseNotesRequest]) (*connect.Response[nagiv1.GetPanelReleaseNotesResponse], error) {
	if err := s.Panel.updateUnsupported(); err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	latest, err := s.updates.get(ctx, false, s.Panel.UpdateChannel(ctx), release.ProxyFor(s.Panel.DataDir))
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	out := &nagiv1.GetPanelReleaseNotesResponse{}
	if !release.Older(s.Panel.Version, latest) {
		return connect.NewResponse(out), nil
	}
	// 和更新时下载新版本走同一个地址：安装时指定了镜像就从镜像取。
	md, err := s.updates.changelog(ctx, release.BaseFor(s.Panel.DataDir, latest), latest, i18n.FromContext(ctx))
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	for _, sec := range release.Sections(md, s.Panel.Version, latest) {
		out.Releases = append(out.Releases, &nagiv1.ReleaseNotes{Version: sec.Version, Body: sec.Body})
	}
	return connect.NewResponse(out), nil
}

func (s *settingsService) UpdatePanel(ctx context.Context, _ *connect.Request[nagiv1.UpdatePanelRequest]) (*connect.Response[nagiv1.UpdatePanelResponse], error) {
	if err := s.Panel.updateUnsupported(); err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	if !s.updates.running.CompareAndSwap(false, true) {
		return nil, connect.NewError(connect.CodeAborted, i18n.New("面板正在更新，请在完成后重试"))
	}
	// 版本由面板自己决定，不听请求的：只升不降。
	// 请求断开（例如刚点完就刷新页面）不影响更新。问 GitHub 要一两秒，先用缓存的版本号记下状态，免得界面这时还能再点一次。
	ctx = context.WithoutCancel(ctx)
	channel := s.Panel.UpdateChannel(ctx)
	s.updates.mu.Lock()
	cached := s.updates.latest
	if s.updates.channel != channel {
		cached = ""
	}
	s.updates.mu.Unlock()
	if release.Older(s.Panel.Version, cached) {
		s.withUpdates(ctx, func(r *updateRecords) { r.Panel = newUpdateState(cached, s.Panel.Version, updateInstalling) })
	}
	abort := func(code connect.Code, err error) (*connect.Response[nagiv1.UpdatePanelResponse], error) {
		s.updates.running.Store(false)
		s.withUpdates(ctx, func(r *updateRecords) { r.Panel = nil })
		return nil, connect.NewError(code, err)
	}
	latest, err := s.updates.get(ctx, true, channel, release.ProxyFor(s.Panel.DataDir))
	if err != nil {
		return abort(connect.CodeUnavailable, err)
	}
	if !release.Older(s.Panel.Version, latest) {
		return abort(connect.CodeFailedPrecondition, i18n.Errorf("面板已是最新版本 %s", s.Panel.Version))
	}
	s.withUpdates(ctx, func(r *updateRecords) { r.Panel = newUpdateState(latest, s.Panel.Version, updateInstalling) })
	go s.runPanelUpdate(ctx, latest)
	return connect.NewResponse(&nagiv1.UpdatePanelResponse{Version: latest}), nil
}
