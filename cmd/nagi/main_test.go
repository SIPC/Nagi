package main

import (
	"bytes"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// 加入令牌优先用 --token（旧的安装脚本），没给时读环境变量 NAGI_TOKEN（新的安装脚本）。
func TestTokenFromEnv(t *testing.T) {
	t.Setenv(tokenEnv, " tok-env\n")
	if got := tokenFromEnv(""); got != "tok-env" {
		t.Errorf("没给 --token 时应该读环境变量，得到 %q", got)
	}
	if got := tokenFromEnv("tok-flag"); got != "tok-flag" {
		t.Errorf("给了 --token 时用它，得到 %q", got)
	}
	t.Setenv(tokenEnv, "")
	if got := tokenFromEnv(""); got != "" {
		t.Errorf("都没有时应该为空，得到 %q", got)
	}
}

// 备用面板的配对码也一样：没给 --standby 时读环境变量 NAGI_STANDBY，不必写在进程参数里。
func TestStandbyCodeFromEnv(t *testing.T) {
	t.Setenv(standbyEnv, " nagi-standby:abc\n")
	if got := flagOrEnv("", standbyEnv); got != "nagi-standby:abc" {
		t.Errorf("没给 --standby 时应该读环境变量，得到 %q", got)
	}
	if got := flagOrEnv("nagi-standby:flag", standbyEnv); got != "nagi-standby:flag" {
		t.Errorf("给了 --standby 时用它，得到 %q", got)
	}
}

// nagi server --standby <码> 仍然接受，但提示改用环境变量；用环境变量时不提示。读完都清掉环境变量。
func TestStandbyCodeFlagWarns(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	t.Setenv(standbyEnv, "nagi-standby:env")
	if got := standbyCodeFrom("nagi-standby:flag", log); got != "nagi-standby:flag" {
		t.Errorf("给了 --standby 时用它，得到 %q", got)
	}
	if !strings.Contains(buf.String(), standbyEnv) {
		t.Errorf("用 --standby 时应该提示改用环境变量：%s", buf.String())
	}
	if _, ok := os.LookupEnv(standbyEnv); ok {
		t.Error("读完应该清掉环境变量")
	}
	buf.Reset()
	t.Setenv(standbyEnv, "nagi-standby:env")
	if got := standbyCodeFrom("", log); got != "nagi-standby:env" {
		t.Errorf("没给 --standby 时应该读环境变量，得到 %q", got)
	}
	if buf.Len() != 0 {
		t.Errorf("用环境变量时不应该提示：%s", buf.String())
	}
}

// 安装脚本遇到 --standby <码> 时照常接受，但提示改用环境变量；用环境变量时不提示。
// 用一个不合法的端口让脚本在参数检查时停下，不会真的安装。
func TestInstallScriptStandbyArgWarns(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("没有 sh")
	}
	run := func(env []string, args ...string) string {
		cmd := exec.Command(sh, append([]string{"../../scripts/install.sh"}, args...)...)
		cmd.Env = append([]string{"PATH=" + os.Getenv("PATH")}, env...)
		out, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "端口须为数字") {
			t.Fatalf("应该在端口检查时停下：%v %s", err, out)
		}
		return string(out)
	}
	if out := run(nil, "--standby", "nagi-standby:abc", "--port", "x"); !strings.Contains(out, "NAGI_STANDBY=<配对码>") {
		t.Errorf("用 --standby 时应该提示改用环境变量：%s", out)
	}
	if out := run([]string{standbyEnv + "=nagi-standby:abc"}, "--port", "x"); strings.Contains(out, "注意") {
		t.Errorf("用环境变量时不应该提示：%s", out)
	}
}

// 面板安装脚本不把配对码写进服务的启动参数和服务配置（谁都能读），而是放在只有 root 能读的环境变量文件里。
func TestInstallScriptStandbyCodeNotInArgs(t *testing.T) {
	b, err := os.ReadFile("../../scripts/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(b)
	if strings.Contains(script, `ARGS --standby`) || strings.Contains(script, "server --standby") {
		t.Error("安装脚本不应该把配对码放进 nagi server 的参数")
	}
	for _, want := range []string{`EnvironmentFile=`, standbyEnv + `=`, `umask 077`} {
		if !strings.Contains(script, want) {
			t.Errorf("安装脚本应该用只有 root 能读的环境变量文件传配对码，缺少 %q", want)
		}
	}
}

// installScriptFunc 从面板安装脚本里取出函数 name 的定义（从 `name() {` 到行首的 `}`），给测试单独执行。
func installScriptFunc(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("../../scripts/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	i := strings.Index(s, "\n"+name+"() {\n")
	if i < 0 {
		t.Fatalf("安装脚本里找不到函数 %s", name)
	}
	j := strings.Index(s[i:], "\n}\n")
	if j < 0 {
		t.Fatalf("安装脚本里函数 %s 没有结尾", name)
	}
	return s[i+1 : i+j+3]
}

// 配对码文件先删再以 0600 新建：原有的文件权限更宽、是指向别处的链接时都不沿用。
// 安装出错退出时，还没配上对就停下面板、删掉配对码文件；已经配上对时从服务配置中去掉它。
func TestInstallScriptStandbyEnvFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("要用 Unix 的文件权限和符号链接")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("没有 sh")
	}
	funcs := `say() { printf '%s\n' "$*"; }
fail() { printf '错误：%s\n' "$*" >&2; exit 1; }
systemctl() { echo "systemctl $*" >>"$DATA_DIR/calls"; }
write_unit() { echo "write_unit $*" >>"$DATA_DIR/calls"; }
` + installScriptFunc(t, "write_standby_env") + installScriptFunc(t, "on_exit")
	run := func(dir, body string) string {
		cmd := exec.Command(sh, "-euc", funcs+body)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "DATA_DIR=" + dir, "STANDBY_ENV=" + filepath.Join(dir, "standby.env"),
			"SERVICE=nagi-server", "TMP=" + filepath.Join(dir, "tmp"), "STANDBY_WRITTEN="}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v %s", err, out)
		}
		b, _ := os.ReadFile(filepath.Join(dir, "calls"))
		return string(out) + string(b)
	}

	// 原有的文件权限更宽：换成 0600 的新文件。
	dir := t.TempDir()
	env := filepath.Join(dir, "standby.env")
	if err := os.WriteFile(env, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(dir, `write_standby_env nagi-standby:abc`)
	if fi, err := os.Lstat(env); err != nil || fi.Mode() != 0o600 {
		t.Fatalf("配对码文件应该是 0600 的普通文件：%v %v", fi.Mode(), err)
	}
	if b, _ := os.ReadFile(env); string(b) != "NAGI_STANDBY=nagi-standby:abc\n" {
		t.Errorf("配对码文件内容不对：%q", b)
	}

	// 原有的是指向别处的链接：不写到链接指向的地方。
	dir = t.TempDir()
	env = filepath.Join(dir, "standby.env")
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, env); err != nil {
		t.Fatal(err)
	}
	run(dir, `write_standby_env nagi-standby:abc`)
	if b, _ := os.ReadFile(target); string(b) != "keep" {
		t.Errorf("不应该写到链接指向的文件：%q", b)
	}
	if fi, err := os.Lstat(env); err != nil || !fi.Mode().IsRegular() {
		t.Errorf("配对码文件应该是新建的普通文件：%v %v", fi, err)
	}

	// 出错退出、还没配上对：停下面板，删掉配对码文件。
	dir = t.TempDir()
	env = filepath.Join(dir, "standby.env")
	out := run(dir, `write_standby_env nagi-standby:abc; on_exit 1`)
	if _, err := os.Stat(env); !os.IsNotExist(err) {
		t.Errorf("出错退出时应该删掉配对码文件：%v", err)
	}
	if !strings.Contains(out, "systemctl stop nagi-server") {
		t.Errorf("还没配上对时应该停下面板：%s", out)
	}

	// 出错退出、已经配上对：从服务配置中去掉配对码文件，不停面板。
	dir = t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "panel"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "panel", "paired"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	out = run(dir, `write_standby_env nagi-standby:abc; on_exit 1`)
	if !strings.Contains(out, "write_unit ") || strings.Contains(out, "systemctl stop") {
		t.Errorf("已经配上对时应该从服务配置中去掉配对码文件、不停面板：%s", out)
	}

	// 正常退出、没写过配对码文件：都不动。
	dir = t.TempDir()
	env = filepath.Join(dir, "standby.env")
	if out := run(dir, `write_standby_env nagi-standby:abc; on_exit 0`); strings.Contains(out, "systemctl") || strings.Contains(out, "write_unit") {
		t.Errorf("正常退出时不应该清理：%s", out)
	}
	if _, err := os.Stat(env); err != nil {
		t.Errorf("正常退出时配对码文件由后面的步骤处理：%v", err)
	}
	if out := run(t.TempDir(), `on_exit 1`); strings.Contains(out, "systemctl") || strings.Contains(out, "write_unit") {
		t.Errorf("没写过配对码文件时不应该清理：%s", out)
	}
}

// 网页、App 给出的命令和使用文档默认用环境变量传配对码，不写成 --standby <码>。
func TestStandbyCommandsUseEnv(t *testing.T) {
	for _, f := range []string{
		"../../web/src/pages/standby.tsx",
		"../../android/feature/settings/src/main/kotlin/ink/sipc/nagi/settings/StandbyInviteScreens.kt",
		"../../docs/guide/standby.md",
		"../../docs/guide/standby.en.md",
	} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if s := string(b); strings.Contains(s, "--standby") || !strings.Contains(s, standbyEnv+"=") {
			t.Errorf("%s 应该用环境变量 %s 传配对码，不用 --standby", f, standbyEnv)
		}
	}
}
