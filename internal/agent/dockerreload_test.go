package agent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// fakeHost 是假的本机：记下执行的命令和发 SIGHUP 的进程。
type fakeHost struct {
	systemd    bool
	active     bool // systemctl is-active docker
	reloadFail bool // systemctl reload docker 失败
	pidFile    int  // 0 表示没有 pid 文件
	procs      map[int]string
	hupFail    bool
	ran        []string
	hups       []int
}

func (f *fakeHost) reloader() dockerReloader {
	return dockerReloader{
		systemd: func() bool { return f.systemd },
		run: func(_ context.Context, name string, args ...string) error {
			cmd := strings.Join(append([]string{name}, args...), " ")
			f.ran = append(f.ran, cmd)
			switch {
			case strings.Contains(cmd, "is-active") && !f.active:
				return errors.New("inactive")
			case strings.Contains(cmd, "reload") && f.reloadFail:
				return errors.New("failed")
			}
			return nil
		},
		pidFile: func() (int, bool) { return f.pidFile, f.pidFile != 0 },
		comm:    func(pid int) string { return f.procs[pid] },
		pids: func() []int {
			var out []int
			for pid := range f.procs {
				out = append(out, pid)
			}
			slices.Sort(out)
			return out
		},
		hup: func(pid int) error {
			if f.hupFail {
				return errors.New("operation not permitted")
			}
			f.hups = append(f.hups, pid)
			return nil
		},
	}
}

// TestDockerReload：按什么办法让 dockerd 重新加载配置。不重启 Docker。
func TestDockerReload(t *testing.T) {
	cases := []struct {
		desc    string
		host    fakeHost
		reload  bool  // 应执行 systemctl reload docker
		hups    []int // 应发 SIGHUP 的进程
		wantErr string
	}{
		{desc: "systemd 管理的 docker", host: fakeHost{systemd: true, active: true, pidFile: 100, procs: map[int]string{100: "dockerd"}}, reload: true},
		{desc: "systemctl reload 失败时按 pid 文件发 SIGHUP", host: fakeHost{systemd: true, active: true, reloadFail: true, pidFile: 100, procs: map[int]string{100: "dockerd"}}, reload: true, hups: []int{100}},
		{desc: "docker 不归 systemd 管", host: fakeHost{systemd: true, active: false, pidFile: 100, procs: map[int]string{100: "dockerd"}}, hups: []int{100}},
		{desc: "没有 systemd", host: fakeHost{pidFile: 100, procs: map[int]string{1: "init", 100: "dockerd"}}, hups: []int{100}},
		{desc: "pid 文件是旧的，找进程", host: fakeHost{pidFile: 100, procs: map[int]string{100: "sshd", 200: "containerd", 300: "dockerd"}}, hups: []int{300}},
		{desc: "没有 pid 文件，找进程", host: fakeHost{procs: map[int]string{1: "init", 300: "dockerd"}}, hups: []int{300}},
		{desc: "找不到 dockerd", host: fakeHost{procs: map[int]string{1: "init", 200: "containerd"}}, wantErr: "找不到 dockerd"},
		{desc: "多个 dockerd", host: fakeHost{procs: map[int]string{300: "dockerd", 400: "dockerd"}}, wantErr: "多个 dockerd"},
		{desc: "发不了信号", host: fakeHost{pidFile: 100, procs: map[int]string{100: "dockerd"}, hupFail: true}, wantErr: "无法让 Docker 重新加载配置"},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			h := c.host
			_, err := h.reloader().reload(context.Background())
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("应报错 %q，实际 %v", c.wantErr, err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if got := slices.Contains(h.ran, "systemctl reload docker"); got != c.reload {
				t.Errorf("执行的命令：%v", h.ran)
			}
			for _, cmd := range h.ran {
				if strings.Contains(cmd, "restart") || strings.Contains(cmd, "stop") {
					t.Errorf("不应重启 Docker：%s", cmd)
				}
			}
			if !slices.Equal(h.hups, c.hups) {
				t.Errorf("SIGHUP 发给了 %v，应为 %v", h.hups, c.hups)
			}
		})
	}
}

func TestSameMirrors(t *testing.T) {
	cases := []struct {
		a, b []string
		want bool
	}{
		{nil, nil, true},
		{[]string{"https://a.example"}, []string{"https://a.example/"}, true},
		{[]string{"https://a.example", "https://b.example"}, []string{"https://b.example/", "https://a.example/"}, false},
		{[]string{"https://a.example"}, nil, false},
	}
	for _, c := range cases {
		if got := sameMirrors(c.a, c.b); got != c.want {
			t.Errorf("sameMirrors(%v, %v) = %v", c.a, c.b, got)
		}
	}
}
