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

	"github.com/SIPC/nagi/internal/pkg/release"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// downloadURLFile 记着安装时用 --download-url 指定的镜像地址（<节点目录>/identity/download-url），
// 一键升级也从这里下载；没有时从 GitHub Release 下载。
const downloadURLFile = "download-url"

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
	exe, err := os.Executable()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	base := release.DownloadBase(want)
	if b, err := os.ReadFile(filepath.Join(a.id.dir, downloadURLFile)); err == nil && strings.TrimSpace(string(b)) != "" {
		base = strings.TrimRight(strings.TrimSpace(string(b)), "/")
	}
	tmp := exe + ".new"
	if err := download(ctx, base, tmp); err != nil {
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
	if version != want {
		// 镜像地址不分版本，里面放的可能不是面板要求的版本。
		os.Remove(tmp)
		return nil, status.Errorf(codes.FailedPrecondition, "下载到的是 %s，不是面板要求的 %s（检查镜像地址 %s）", version, want, base)
	}
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

// download 从 base 下载 nagi-linux-<架构> 到 dst，并按同一地址下的 SHA256SUMS 核对。
func download(ctx context.Context, base, dst string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	name := "nagi-linux-" + runtime.GOARCH

	sums, err := fetch(ctx, base+"/SHA256SUMS", 64<<10, nil)
	if err != nil {
		return err
	}
	want := release.Checksum(string(sums), name)
	if want == "" {
		return fmt.Errorf("SHA256SUMS 里没有 %s", name)
	}

	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = fetch(ctx, base+"/"+name, 0, io.MultiWriter(f, h))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("下载的文件校验不通过（%s，应为 %s）", got[:12], want[:min(12, len(want))])
	}
	return nil
}

// fetch 下载 url：w 不为空时写进 w，否则读进内存返回（最多 limit 字节）。
func fetch(ctx context.Context, url string, limit int64, w io.Writer) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("下载 %s 失败：%w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("下载 %s 失败：%s", url, resp.Status)
	}
	if w != nil {
		if _, err := io.Copy(w, resp.Body); err != nil {
			return nil, fmt.Errorf("下载 %s 失败：%w", url, err)
		}
		return nil, nil
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, fmt.Errorf("下载 %s 失败：%w", url, err)
	}
	return b, nil
}
