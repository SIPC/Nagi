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
)

// 一键更新面板（见 docs/panel.md#更新面板）：和节点的一键升级一样，从 GitHub Release（安装时指定了镜像就从镜像）
// 下载，核对后替换可执行文件，然后原地重启。内嵌的节点在同一个进程里，一起更新。进度见 upgrades.go。

// updateCheckTTL 是最新版本号的缓存时间：每次打开页面都会查，不能每次都去问 GitHub。
const updateCheckTTL = time.Hour

type panelUpdates struct {
	mu      sync.Mutex
	latest  string
	checked time.Time
	// running：正在更新。成功以后进程被替换，不会再回到 false。
	running atomic.Bool

	// 更新状态（见 upgrades.go）。
	stateMu sync.Mutex
	state   updateRecords
	loaded  bool
	saved   string
}

// get 返回最新发布的版本号，force 时不用缓存。
func (u *panelUpdates) get(ctx context.Context, force bool) (string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if !force && u.latest != "" && time.Since(u.checked) < updateCheckTTL {
		return u.latest, nil
	}
	v, err := release.Latest(ctx)
	if err != nil {
		return "", i18n.Errorf("检查更新失败：%w", err)
	}
	u.latest, u.checked = v, time.Now()
	return v, nil
}

// updateUnsupported 返回本面板不能一键更新的原因，能更新时返回空。发布的文件只有 Linux 版，本地编译的版本没有对应的发布文件。
func (p *PanelInfo) updateUnsupported() error {
	if !release.IsVersion(p.Version) {
		return i18n.New("这是本地编译的版本，不能一键更新")
	}
	if runtime.GOOS != "linux" {
		return i18n.New("只有 Linux 上的面板能一键更新")
	}
	return nil
}

func (s *settingsService) CheckPanelUpdate(ctx context.Context, req *connect.Request[nagiv1.CheckPanelUpdateRequest]) (*connect.Response[nagiv1.CheckPanelUpdateResponse], error) {
	out := &nagiv1.CheckPanelUpdateResponse{Current: s.Panel.Version}
	if err := s.Panel.updateUnsupported(); err != nil {
		out.Unsupported = i18n.Localize(i18n.FromContext(ctx), err)
		return connect.NewResponse(out), nil
	}
	latest, err := s.updates.get(ctx, req.Msg.GetForce())
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	out.Latest, out.ReleaseUrl = latest, release.PageURL(latest)
	out.Available = release.Older(s.Panel.Version, latest)
	return connect.NewResponse(out), nil
}

func (s *settingsService) UpdatePanel(ctx context.Context, _ *connect.Request[nagiv1.UpdatePanelRequest]) (*connect.Response[nagiv1.UpdatePanelResponse], error) {
	if err := s.Panel.updateUnsupported(); err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	if !s.updates.running.CompareAndSwap(false, true) {
		return nil, connect.NewError(connect.CodeAborted, i18n.New("面板正在更新，请稍候"))
	}
	// 版本由面板自己决定，不听请求的：只升不降。
	// 请求断开（例如刚点完就刷新页面）不影响更新。问 GitHub 要一两秒，先用缓存的版本号记下状态，免得界面这时还能再点一次。
	ctx = context.WithoutCancel(ctx)
	s.updates.mu.Lock()
	cached := s.updates.latest
	s.updates.mu.Unlock()
	if release.Older(s.Panel.Version, cached) {
		s.withUpdates(ctx, func(r *updateRecords) { r.Panel = newUpdateState(cached, s.Panel.Version, updateInstalling) })
	}
	abort := func(code connect.Code, err error) (*connect.Response[nagiv1.UpdatePanelResponse], error) {
		s.updates.running.Store(false)
		s.withUpdates(ctx, func(r *updateRecords) { r.Panel = nil })
		return nil, connect.NewError(code, err)
	}
	latest, err := s.updates.get(ctx, true)
	if err != nil {
		return abort(connect.CodeUnavailable, err)
	}
	if !release.Older(s.Panel.Version, latest) {
		return abort(connect.CodeFailedPrecondition, i18n.Errorf("面板已经是最新版本 %s", s.Panel.Version))
	}
	s.withUpdates(ctx, func(r *updateRecords) { r.Panel = newUpdateState(latest, s.Panel.Version, updateInstalling) })
	go s.runPanelUpdate(ctx, latest)
	return connect.NewResponse(&nagiv1.UpdatePanelResponse{Version: latest}), nil
}
