package release

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// progressLog 收集进度。
type progressLog struct {
	mu   sync.Mutex
	list []Progress
}

func (l *progressLog) add(p Progress) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.list = append(l.list, p)
}

func (l *progressLog) all() []Progress {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Progress(nil), l.list...)
}

// slowServer 分 chunks 块发送 body，每块之间停 pause；sized 为 false 时不发 Content-Length。
func slowServer(t *testing.T, body []byte, chunks int, pause time.Duration, sized bool) *httptest.Server {
	t.Helper()
	name := binaryName()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/SHA256SUMS"):
			w.Write([]byte(sum(string(body)) + "  " + name + "\n"))
		case strings.HasSuffix(r.URL.Path, "/"+name):
			if sized {
				w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			}
			w.WriteHeader(http.StatusOK)
			step := (len(body) + chunks - 1) / chunks
			for i := 0; i < len(body); i += step {
				w.Write(body[i:min(i+step, len(body))])
				w.(http.Flusher).Flush()
				select {
				case <-r.Context().Done():
					return
				case <-time.After(pause):
				}
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func fastProgress(t *testing.T, d time.Duration) {
	t.Helper()
	old := progressInterval
	progressInterval = d
	t.Cleanup(func() { progressInterval = old })
}

func TestProgress(t *testing.T) {
	setup(t)
	fastProgress(t, 50*time.Millisecond)
	quiet := slog.New(slog.DiscardHandler)
	body := bytes.Repeat([]byte("nagi"), 256<<10) // 1 MiB

	t.Run("下载、核对、试运行，下载的字节数递增", func(t *testing.T) {
		srv := slowServer(t, body, 16, 20*time.Millisecond, true)
		src := Source{Base: srv.URL + "/nagi", Mirror: true}
		var log progressLog
		ctx := WithProgress(context.Background(), log.add)
		checked := false
		_, err := fetchFirst(ctx, []Source{src}, filepath.Join(t.TempDir(), "x"), func(Source) error { checked = true; return nil }, quiet)
		if err != nil || !checked {
			t.Fatalf("下载失败：%v", err)
		}
		got := log.all()
		if len(got) < 4 {
			t.Fatalf("进度太少：%+v", got)
		}
		if got[0] != (Progress{Stage: StageDownload, Source: src.Name(), Downloaded: 0, Total: -1}) {
			t.Errorf("开始时应报下载、0、大小未知：%+v", got[0])
		}
		size := int64(len(body))
		var downloads int
		var prev int64
		for i, p := range got {
			if p.Source != src.Name() {
				t.Errorf("第 %d 个进度的地址是 %q", i, p.Source)
			}
			if p.Stage != StageDownload {
				continue
			}
			downloads++
			if p.Downloaded < prev {
				t.Errorf("已下载的字节数变少了：%d → %d", prev, p.Downloaded)
			}
			prev = p.Downloaded
			if i > 0 && p.Total != size {
				t.Errorf("收到响应头后总大小应为 %d：%+v", size, p)
			}
		}
		n := len(got)
		if want := (Progress{Stage: StageDownload, Source: src.Name(), Downloaded: size, Total: size}); got[n-3] != want {
			t.Errorf("下载完应报一次全部：%+v", got[n-3])
		}
		if got[n-2].Stage != StageVerify || got[n-1].Stage != StageTest || got[n-1].Downloaded != size || got[n-1].Total != size {
			t.Errorf("下载完应依次是核对、试运行：%+v", got[n-2:])
		}
		// 16 块、每块之间停 20ms，约 320ms：每 50ms 最多一次，加上开始和结束，远少于读的次数。
		if downloads > 12 {
			t.Errorf("进度报得太勤：%d 次", downloads)
		}
	})

	t.Run("不知道大小时总大小为 -1", func(t *testing.T) {
		srv := slowServer(t, body, 4, 10*time.Millisecond, false)
		var log progressLog
		_, err := fetchFirst(WithProgress(context.Background(), log.add), []Source{{Base: srv.URL + "/nagi", Mirror: true}}, filepath.Join(t.TempDir(), "x"), nil, quiet)
		if err != nil {
			t.Fatal(err)
		}
		got := log.all()
		for _, p := range got {
			if p.Total != -1 {
				t.Errorf("总大小应为 -1：%+v", p)
			}
		}
		if last := got[len(got)-1]; last.Stage != StageVerify || last.Downloaded != int64(len(body)) {
			t.Errorf("没有试运行时最后是核对，已下载全部：%+v", last)
		}
	})

	t.Run("换地址时从 0 重新报", func(t *testing.T) {
		// 「GitHub」的文件被改过（校验不一致），换到加速代理。
		direct, proxy := newFake(t, "corrupt"), newFake(t, "ok")
		sources := sourcesOf(direct, proxy)
		var log progressLog
		src, err := fetchFirst(WithProgress(context.Background(), log.add), sources, filepath.Join(t.TempDir(), "x"), nil, quiet)
		if err != nil || src.Proxy == "" {
			t.Fatalf("得到 %+v、%v", src, err)
		}
		got := log.all()
		first, second := sources[0].Name(), sources[1].Name()
		if first == second {
			t.Fatalf("两个地址的名字一样：%s", first)
		}
		var switched int = -1
		for i, p := range got {
			if p.Source == second {
				switched = i
				break
			}
			if p.Source != first || p.Stage == StageTest {
				t.Errorf("第一个地址只应报下载和核对（核对不通过）：%+v", p)
			}
		}
		if switched < 1 {
			t.Fatalf("没有换地址：%+v", got)
		}
		if p := got[switched-1]; p.Stage != StageVerify || p.Downloaded != int64(len(evilBin)) {
			t.Errorf("第一个地址应下载完、核对时失败：%+v", p)
		}
		if got[switched] != (Progress{Stage: StageDownload, Source: second, Downloaded: 0, Total: -1}) {
			t.Errorf("换地址后应从 0 重新报：%+v", got[switched])
		}
		if last := got[len(got)-1]; last.Source != second || last.Stage != StageVerify || last.Downloaded != int64(len(goodBin)) {
			t.Errorf("最后应是第二个地址核对：%+v", last)
		}
	})

	t.Run("没有回调时照常下载", func(t *testing.T) {
		srv := slowServer(t, body, 2, time.Millisecond, true)
		if _, err := fetchFirst(context.Background(), []Source{{Base: srv.URL + "/nagi", Mirror: true}}, filepath.Join(t.TempDir(), "x"), nil, quiet); err != nil {
			t.Fatal(err)
		}
	})
}
