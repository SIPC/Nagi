package api

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/release"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

const panelScript = "../../../scripts/install.sh"

// 面板安装脚本里的共用代码要和 sharedScript 一字不差（节点安装脚本由面板生成，用的是 sharedScript）。
func TestSharedScript(t *testing.T) {
	b, err := os.ReadFile(panelScript)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(string(b), "\r\n", "\n")
	begin := strings.Index(text, "# >>> 共用代码开始")
	end := strings.Index(text, "# <<< 共用代码结束")
	if begin < 0 || end < 0 {
		t.Fatal("scripts/install.sh 里找不到共用代码的起止标记")
	}
	got := text[begin : end+len("# <<< 共用代码结束")]
	if got != sharedScript {
		t.Fatal("scripts/install.sh 里的共用代码和 install.go 的 sharedScript 不一致，请同步修改")
	}
}

// 安装脚本里的加速代理和 Docker Hub 镜像加速地址要和 Go 里的一致：一键升级按 release.GitHubProxies 换代理，
// 界面上「使用默认加速」填 DefaultRegistryMirrors。scripts/install.sh 由 TestSharedScript 保证和 sharedScript 一样。
func TestInstallConstants(t *testing.T) {
	proxies := `GH_PROXIES="` + release.GitHubProxies + `"` + "\n"
	if !strings.Contains(sharedScript, proxies) {
		t.Errorf("sharedScript 里的 GH_PROXIES 和 release.GitHubProxies 不一致，应为 %s", proxies)
	}
	quoted := make([]string, len(DefaultRegistryMirrors))
	for i, m := range DefaultRegistryMirrors {
		quoted[i] = `"` + m + `"`
	}
	mirrors := `REGISTRY_MIRRORS='` + strings.Join(quoted, ", ") + `'` + "\n"
	if !strings.Contains(sharedScript, mirrors) {
		t.Errorf("sharedScript 里的 REGISTRY_MIRRORS 和 DefaultRegistryMirrors 不一致，应为 %s", mirrors)
	}
}

// 生成的节点安装脚本和面板安装脚本都要能通过 sh -n 语法检查。
func TestInstallScriptsSyntax(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("没有 sh")
	}
	var buf bytes.Buffer
	if err := installScript.Execute(&buf, map[string]string{
		"Panel": "203.0.113.10:34567", "Fingerprint": "sha256//abc=", "Version": "v0.1.0",
		"Base": "https://github.com/SIPC/Nagi/releases/download/v0.1.0", "Shared": sharedScript,
	}); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"ensure_docker\n", "link_command\n", "download_nagi\n", `PANEL="203.0.113.10:34567"`,
		`BASE="https://github.com/SIPC/Nagi/releases/download/v0.1.0"`, `PANEL_VERSION="v0.1.0"`} {
		if !strings.Contains(buf.String(), s) {
			t.Errorf("节点安装脚本里没有 %q", s)
		}
	}
	// 二进制不再从面板下载。
	if strings.Contains(buf.String(), "/download/nagi-linux-") && strings.Contains(buf.String(), "$PANEL/download") {
		t.Error("节点安装脚本还在从面板下载 nagi")
	}
	node := filepath.Join(t.TempDir(), "node.sh")
	if err := os.WriteFile(node, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{node, panelScript} {
		if out, err := exec.Command(sh, "-n", f).CombinedOutput(); err != nil {
			t.Errorf("%s 语法错误：%v\n%s", f, err, out)
		}
	}
}

// v0.35.0#10：令牌用环境变量 NAGI_TOKEN 传给安装脚本和 nagi enroll，不出现在进程参数里；旧命令的 --token 仍然认。
// 脚本读完就清掉环境变量，不传给 Docker 安装脚本等子进程。
func TestInstallScriptTokenFromEnv(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("没有 sh")
	}
	// 共用代码换成打印读到的令牌后退出，只跑参数解析这一段。
	var buf bytes.Buffer
	if err := installScript.Execute(&buf, map[string]string{
		"Panel": "203.0.113.10:34567", "Fingerprint": "sha256//abc=", "Version": "v0.1.0",
		"Base": "https://example.invalid", "Shared": `printf 'token=%s env=%s\n' "$TOKEN" "${NAGI_TOKEN-unset}"; exit 0`,
	}); err != nil {
		t.Fatal(err)
	}
	script := buf.String()
	if strings.Contains(script, `--token "$TOKEN"`) || !strings.Contains(script, `NAGI_TOKEN="$TOKEN" "$BIN" enroll `) {
		t.Error("调用 nagi enroll 时应该用环境变量传令牌")
	}
	run := func(env []string, args ...string) (string, error) {
		cmd := exec.Command(sh, append([]string{"-s", "--"}, args...)...)
		cmd.Stdin = strings.NewReader(script)
		for _, kv := range os.Environ() {
			if !strings.HasPrefix(kv, "NAGI_") {
				cmd.Env = append(cmd.Env, kv)
			}
		}
		cmd.Env = append(cmd.Env, env...)
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	if out, err := run([]string{"NAGI_TOKEN=tok-env"}); err != nil || out != "token=tok-env env=unset" {
		t.Errorf("环境变量传令牌：%q %v", out, err)
	}
	if out, err := run([]string{"NAGI_TOKEN=tok-env"}, "--download-url", "https://mirror.example"); err != nil || out != "token=tok-env env=unset" {
		t.Errorf("环境变量传令牌，命令末尾追加选项：%q %v", out, err)
	}
	if out, err := run(nil, "--token", "tok-arg"); err != nil || out != "token=tok-arg env=unset" {
		t.Errorf("旧命令的 --token：%q %v", out, err)
	}
	if out, err := run(nil); err == nil || !strings.Contains(out, "NAGI_TOKEN") {
		t.Errorf("没有令牌时应该报错：%q %v", out, err)
	}
}

// v0.35.0#10：面板生成的安装命令用环境变量传令牌，末尾的 -s -- 让用户能接着追加选项。
func TestEnrollCommandTokenInEnv(t *testing.T) {
	h := newAuditHarness(t)
	h.a.Panel.Fingerprint, h.a.Panel.Port = "sha256//abc=", 34567
	h.a.Panel.NewEnrollToken = func(context.Context) (string, time.Time, error) {
		return "tok123", time.Now().Add(time.Hour), nil
	}
	res, err := (&nodeService{h.a}).CreateEnrollCommand(context.Background(), connect.NewRequest(&nagiv1.CreateEnrollCommandRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	cmd := res.Msg.GetCommand()
	if !strings.HasSuffix(cmd, `" | NAGI_TOKEN=tok123 sh -s --`) || strings.Contains(cmd, "--token") || strings.Contains(cmd, "k=tok123") {
		t.Errorf("安装命令应该用环境变量传令牌：%s", cmd)
	}
}
