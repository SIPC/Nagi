package agent

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/pkg/release"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Upgrade 下载面板指定版本的 nagi（GitHub Release 或安装时指定的镜像），按 SHA256SUMS 核对，
// 试运行确认能用、版本对得上，替换自己的可执行文件，返回后原地重启（同一个进程号，systemd 和容器里都适用）。
// 项目容器由 Docker 运行，不受影响；内嵌的 Caddy 会中断一两秒。
func (s *agentService) Upgrade(ctx context.Context, req *nagiv1.UpgradeRequest) (*nagiv1.UpgradeResponse, error) {
	a := s.node
	if a.cfg.Embedded {
		return nil, status.Error(codes.FailedPrecondition, "面板自带的节点随面板一起升级，无法单独升级")
	}
	want := req.GetVersion()
	if !release.IsVersion(want) {
		return nil, status.Errorf(codes.InvalidArgument, "版本号 %s 格式不正确", want)
	}
	u := &a.upgrade
	ctx, cancel, err := u.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()
	p, err := release.Prepare(ctx, release.BaseFor(a.id.dir, want), want)
	if err == nil && !u.commit() {
		p.Discard()
		err = context.Canceled
	}
	if err != nil {
		u.end()
		code := codes.Unavailable
		switch {
		case errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled):
			return nil, status.Error(codes.Canceled, "升级已取消")
		case release.IsUnusable(err):
			code = codes.FailedPrecondition
		}
		return nil, status.Error(code, err.Error())
	}
	exe, err := p.Commit()
	if err != nil {
		u.end()
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	a.log.Info("已替换为新版本，重启", "from", a.cfg.Version, "to", want)
	go func() {
		time.Sleep(time.Second) // 先把响应发回去
		if err := release.Reexec(exe); err != nil {
			a.log.Error("重启失败，请手动重启 nagi", "err", err)
			u.end()
		}
	}()
	return &nagiv1.UpgradeResponse{Version: want}, nil
}

// CancelUpgrade 取消正在下载、校验的升级。已经开始替换的不能取消。
func (s *agentService) CancelUpgrade(context.Context, *nagiv1.CancelUpgradeRequest) (*nagiv1.CancelUpgradeResponse, error) {
	if err := s.node.upgrade.abort(); err != nil {
		return nil, err
	}
	return &nagiv1.CancelUpgradeResponse{}, nil
}

// upgradeState 是正在进行的升级，同一时间只有一个。面板发来的请求不跟着调用方取消（见 detach.go），
// 所以取消要单独发 CancelUpgrade。取消后要等这次升级收尾（删掉下载了一半的文件）才能再升级，两次会用同一个临时文件。
type upgradeState struct {
	mu        sync.Mutex
	active    bool
	canceled  bool
	replacing bool // 已经开始替换（直到重启），不能取消
	cancel    context.CancelFunc
}

func (u *upgradeState) begin(ctx context.Context) (context.Context, context.CancelFunc, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.active {
		return nil, nil, status.Error(codes.Aborted, "节点正在升级，请在完成后重试")
	}
	ctx, cancel := context.WithCancel(ctx)
	u.active, u.canceled, u.replacing, u.cancel = true, false, false, cancel
	return ctx, cancel, nil
}

// commit 在替换之前调用：已经取消时返回 false。
func (u *upgradeState) commit() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.canceled {
		return false
	}
	u.replacing = true
	return true
}

// end 在升级失败（包括取消）时调用，之后可以再升级。
func (u *upgradeState) end() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.active, u.replacing, u.cancel = false, false, nil
}

func (u *upgradeState) abort() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	switch {
	case !u.active:
		return status.Error(codes.NotFound, "节点没有在升级")
	case u.replacing:
		return status.Error(codes.FailedPrecondition, "节点已替换为新版本，正在重启，无法取消")
	}
	u.canceled = true
	u.cancel()
	return nil
}
