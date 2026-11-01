package agent

import (
	"context"
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
		return nil, status.Error(codes.FailedPrecondition, "面板自带的节点随面板一起升级，不能单独升级")
	}
	want := req.GetVersion()
	if !release.IsVersion(want) {
		return nil, status.Errorf(codes.InvalidArgument, "版本号 %q 不对", want)
	}
	exe, err := release.Install(ctx, release.BaseFor(a.id.dir, want), want)
	if err != nil {
		code := codes.Unavailable
		if release.IsUnusable(err) {
			code = codes.FailedPrecondition
		}
		return nil, status.Error(code, err.Error())
	}
	a.log.Info("已替换为新版本，重启", "from", a.cfg.Version, "to", want)
	go func() {
		time.Sleep(time.Second) // 先把响应发回去
		if err := release.Reexec(exe); err != nil {
			a.log.Error("重启失败，请手动重启 nagi", "err", err)
		}
	}()
	return &nagiv1.UpgradeResponse{Version: want}, nil
}
