package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/SIPC/nagi/internal/agent/metrics"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
)

// hintDirs 返回出错时要检查剩余空间的目录：节点目录和所有容器目录。节点启动时设置。
var hintDirs = func() []string { return nil }

// diskFullFree 是判断"磁盘满了"的剩余空间：SQLite、kopia 在空间耗尽前就会失败。
const diskFullFree = 16 << 20

// explain 把常见的底层错误改写成用户能看懂的说明，原始错误附在后面；看不出原因的原样返回。
func explain(msg string) string {
	if hint := diskHint(msg); hint != "" {
		return hint + "（原始错误：" + msg + "）"
	}
	if hint := dockerHint(msg); hint != "" {
		return hint + "（原始错误：" + msg + "）"
	}
	return msg
}

// explainStatus 改写 gRPC 错误的信息，保留错误码。
func explainStatus(err error) error {
	if err == nil {
		return nil
	}
	st, _ := status.FromError(err) // 不是 gRPC 状态的普通错误得到 Unknown，gRPC 也是这样转换的
	if msg := explain(st.Message()); msg != st.Message() {
		return status.Error(st.Code(), msg)
	}
	return err
}

func explainUnary(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	resp, err := handler(ctx, req)
	return resp, explainStatus(err)
}

func explainStream(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	return explainStatus(handler(srv, ss))
}

// diskHint 识别磁盘满导致的错误，说明是哪块盘。SQLite 在磁盘满时可能只报"disk I/O error"，
// 所以这类错误要实际看一下剩余空间才下结论。
func diskHint(msg string) string {
	lower := strings.ToLower(msg)
	explicit := strings.Contains(lower, "no space left on device") || strings.Contains(lower, "database or disk is full")
	// 数据库因为磁盘满而只读打开以后，写入时报的是"readonly database"。
	if !explicit && !strings.Contains(lower, "disk i/o error") && !strings.Contains(lower, "readonly database") {
		return ""
	}
	seen := map[string]bool{}
	var full []string
	for _, dir := range hintDirs() {
		mount, free, ok := metrics.DiskFree(dir)
		if !ok || seen[mount] {
			continue
		}
		seen[mount] = true
		if free < diskFullFree {
			full = append(full, fmt.Sprintf("%s 只剩 %s", mount, humanBytes(int64(free))))
		}
	}
	if len(full) > 0 {
		return "节点磁盘满了：" + strings.Join(full, "，") + "。腾出空间后重试"
	}
	if explicit {
		return "节点磁盘空间不足，腾出空间后重试"
	}
	return ""
}

// dockerHints 是拉取镜像时常见的错误。只用于 docker compose 的输出，避免误判别的错误。
var dockerHints = []struct {
	match []string
	hint  string
}{
	{[]string{"pull access denied", "repository does not exist"}, "镜像不存在，或者是私有镜像、需要先登录镜像仓库。检查镜像名有没有写错"},
	{[]string{"manifest unknown"}, "镜像没有这个标签（版本）。检查镜像名冒号后面的标签"},
	{[]string{"toomanyrequests"}, "Docker Hub 限制了拉取次数。稍后再试，或者登录 Docker Hub 账号、给节点配置镜像加速"},
	{[]string{"unauthorized", "authentication required"}, "镜像仓库要求登录，或者登录信息不对"},
	{[]string{"i/o timeout", "tls handshake timeout", "no such host", "connection refused", "connection reset", "context deadline exceeded"},
		"连不上镜像仓库，节点的网络不通。国内节点可以给节点配置镜像加速"},
	{[]string{"no matching manifest"}, "这个镜像没有适合节点 CPU 架构的版本"},
}

func dockerHint(msg string) string {
	if !strings.Contains(msg, "docker compose 失败") && !strings.Contains(msg, "拉取镜像") {
		return ""
	}
	lower := strings.ToLower(msg)
	for _, h := range dockerHints {
		for _, m := range h.match {
			if strings.Contains(lower, m) {
				return h.hint
			}
		}
	}
	return ""
}

// humanBytes 把字节数写成 KB、MB、GB。
func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
