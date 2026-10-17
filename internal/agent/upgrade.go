package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/SIPC/nagi/internal/pkg/tlsutil"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Upgrade 从发起请求的面板下载 nagi（钉住面板公钥），核对 SHA-256，试运行确认能用，
// 替换自己的可执行文件，返回后原地重启（同一个进程号，systemd 和容器里都适用）。
// 项目容器由 Docker 运行，不受影响；内嵌的 Caddy 会中断一两秒。
func (s *agentService) Upgrade(ctx context.Context, req *nagiv1.UpgradeRequest) (*nagiv1.UpgradeResponse, error) {
	a := s.node
	if a.cfg.Embedded {
		return nil, status.Error(codes.FailedPrecondition, "面板自带的节点随面板一起升级，不能单独升级")
	}
	p, ok := a.id.panel(s.panel)
	if !ok {
		return nil, status.Error(codes.FailedPrecondition, "找不到这个面板")
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	tmp := exe + ".new"
	if err := download(ctx, p, tmp, req.GetSha256()); err != nil {
		os.Remove(tmp)
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	vctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	out, err := exec.CommandContext(vctx, tmp, "version").Output()
	cancel()
	if err != nil {
		os.Remove(tmp)
		return nil, status.Errorf(codes.FailedPrecondition, "新版本在这台机器上运行不了：%v", err)
	}
	version := strings.TrimSpace(string(out))
	if err := os.Rename(tmp, exe); err != nil {
		os.Remove(tmp)
		return nil, status.Error(codes.Internal, err.Error())
	}
	a.log.Info("已替换为新版本，重启", "from", a.cfg.Version, "to", version)
	go func() {
		time.Sleep(time.Second) // 先把响应发回去
		if err := reexec(exe); err != nil {
			a.log.Error("重启失败，请手动重启 nagi", "err", err)
		}
	}()
	return &nagiv1.UpgradeResponse{Version: version}, nil
}

func download(ctx context.Context, p trustedPanel, dst, want string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: tlsutil.PinnedConfig(p.Fingerprint)}}
	url := fmt.Sprintf("https://%s/download/nagi-linux-%s", p.Address, runtime.GOARCH)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("下载失败：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("下载失败：%s", strings.TrimSpace(string(msg)))
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("下载失败：%w", err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); want != "" && got != want {
		return fmt.Errorf("下载的文件校验不通过（%s，应为 %s）", got[:12], want[:min(12, len(want))])
	}
	return nil
}
