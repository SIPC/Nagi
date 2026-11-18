package release

import (
	"context"
	"time"
)

// Stage 是下载新版本进行到的步骤（见 Progress）。
type Stage string

const (
	// StageDownload：正在从 Progress.Source 下载新版本。
	StageDownload Stage = "download"
	// StageVerify：下载完，按 SHA256SUMS 核对。
	StageVerify Stage = "verify"
	// StageTest：试运行新版本，确认能在本机运行、版本对得上。
	StageTest Stage = "test"
)

// Progress 是 Install、Prepare 的进度，通过 WithProgress 交给调用方。只报二进制本身的下载，不报 SHA256SUMS 这类小文件。
// 换下一个地址时从 0 重新报，Source 是新地址。
type Progress struct {
	Stage Stage
	// Source 是正在用的地址（主机名，同 Source.Name）。
	Source string
	// Downloaded 是已下载的字节数；Total 是文件大小，不知道时为 -1。核对和试运行时是下载完的大小。
	Downloaded, Total int64
}

// progressInterval 是下载时两次进度之间至少相隔的时间。测试里改短。
var progressInterval = 200 * time.Millisecond

type progressKey struct{}

// WithProgress 返回带进度回调的 ctx：用它调用 Install、Prepare 时，fn 收到下载、核对、试运行的进度。
// 下载时最多每 200 毫秒调用一次，下载完再调用一次。fn 在下载的 goroutine 里调用，不能阻塞。
func WithProgress(ctx context.Context, fn func(Progress)) context.Context {
	return context.WithValue(ctx, progressKey{}, fn)
}

func progressFrom(ctx context.Context) func(Progress) {
	fn, _ := ctx.Value(progressKey{}).(func(Progress))
	return fn
}
