package release

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// scripts/release-tag.sh：只发 vX.Y.Z 和 vX.Y.Z-pre.N，同一版本号不重新发布（见 docs/development.md#预发布版本）。
// 接受的标签都要被 IsVersion 认得，面板才能比较。
func TestReleaseTagScript(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("没有 sh")
	}
	run := func(args ...string) (string, error) {
		out, err := exec.Command(sh, append([]string{"../../../scripts/release-tag.sh"}, args...)...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	for tag, want := range map[string]string{
		"v0.37.1":        "prerelease=false",
		"v1.0.0":         "prerelease=false",
		"v0.37.1-pre.1":  "prerelease=true",
		"v0.37.1-pre.12": "prerelease=true",
		"v0.37.1-pre.98": "prerelease=true",
	} {
		out, err := run(tag)
		if err != nil || out != want {
			t.Errorf("%s：%q %v，应为 %q", tag, out, err, want)
		}
		if !IsVersion(tag) {
			t.Errorf("%s 能发布，面板却认不出", tag)
		}
	}
	for _, tag := range []string{"v0.37", "0.37.1", "v0.37.1-rc.1", "v0.37.1-pre", "v0.37.1-pre.0", "v0.37.1-pre.01", "v0.37.1-pre.99", "v0.37.1-pre.100", "v01.37.1", "v0.37.1+b1", "v0.37.1-pre.1.2", "dev"} {
		if out, err := run(tag); err == nil {
			t.Errorf("%s 不应发布：%q", tag, out)
		}
	}
	// 已经发布过（GitHub 上有同名 Release）时不重新发布，正式版和预发布版都一样。
	for _, tag := range []string{"v0.37.0", "v0.37.1-pre.1"} {
		if out, err := run(tag, tag); err == nil || !strings.Contains(out, "已经发布过") {
			t.Errorf("%s 已发布过时应停下：%q %v", tag, out, err)
		}
	}
}

// scripts/release-notes.sh：预发布版的正文给出安装这个版本的命令，不给 releases/latest 的命令（那会装上正式版）。
func TestReleaseNotesPrerelease(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("没有 sh")
	}
	dir := t.TempDir()
	os.Mkdir(dir+"/scripts", 0o755)
	script, err := os.ReadFile("../../../scripts/release-notes.sh")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(dir+"/scripts/release-notes.sh", script, 0o755)
	os.WriteFile(dir+"/CHANGELOG.md", []byte("# 更新日志\n\n## v0.38.0-pre.1\n\n### 修复\n\n- 一。\n\n## v0.37.0\n\n- 旧。\n"), 0o644)
	notes := func(tag string) string {
		out, err := exec.Command(sh, dir+"/scripts/release-notes.sh", tag).CombinedOutput()
		if err != nil {
			t.Fatalf("%s：%v %s", tag, err, out)
		}
		return string(out)
	}
	pre := notes("v0.38.0-pre.1")
	if !strings.HasPrefix(pre, "### 修复\n\n- 一。") || !strings.Contains(pre, "releases/download/v0.38.0-pre.1/install.sh | sudo sh -s -- --version v0.38.0-pre.1") || strings.Contains(pre, "releases/latest") {
		t.Errorf("预发布版的正文：\n%s", pre)
	}
	if stable := notes("v0.37.0"); !strings.Contains(stable, "releases/latest/download/install.sh | sudo sh") || strings.Contains(stable, "--version") {
		t.Errorf("正式版的正文：\n%s", stable)
	}
}

// release.yml：每个任务都等标签核对通过；预发布版标成 prerelease、不设为 latest，正式版设为 latest。
// 不真推标签，只核对流程的结构和表达式。
func TestReleaseWorkflow(t *testing.T) {
	b, err := os.ReadFile("../../../.github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		Jobs map[string]struct {
			Needs   any               `yaml:"needs"`
			If      string            `yaml:"if"`
			Outputs map[string]string `yaml:"outputs"`
			Steps   []struct {
				Uses string            `yaml:"uses"`
				Run  string            `yaml:"run"`
				With map[string]string `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(b, &wf); err != nil {
		t.Fatal(err)
	}
	needs := func(job string) map[string]bool {
		out := map[string]bool{}
		switch v := wf.Jobs[job].Needs.(type) {
		case string:
			out[v] = true
		case []any:
			for _, x := range v {
				out[x.(string)] = true
			}
		}
		return out
	}
	tag, ok := wf.Jobs["tag"]
	if !ok || !strings.Contains(tag.Outputs["prerelease"], "steps.check.outputs.prerelease") {
		t.Fatalf("缺少输出 prerelease 的 tag 任务：%+v", tag)
	}
	if !strings.Contains(tag.Steps[len(tag.Steps)-1].Run, "scripts/release-tag.sh") {
		t.Error("tag 任务应调用 scripts/release-tag.sh")
	}
	// 只有 test 直接等 tag，其余的经 test（vuln、build、android 并行，release 等全部）；release 也直接等 tag 才拿得到输出。
	if !needs("test")["tag"] || !needs("release")["tag"] {
		t.Errorf("test、release 应等 tag：test=%v release=%v", needs("test"), needs("release"))
	}
	release := wf.Jobs["release"]
	if !strings.Contains(release.If, "needs.tag.result == 'success'") {
		t.Errorf("release 的条件应要求 tag 通过：%s", release.If)
	}
	var with map[string]string
	for _, s := range release.Steps {
		if strings.HasPrefix(s.Uses, "softprops/action-gh-release@") {
			with = s.With
		}
	}
	if with == nil {
		t.Fatal("没有找到发布步骤")
	}
	if with["prerelease"] != "${{ needs.tag.outputs.prerelease }}" {
		t.Errorf("prerelease：%q", with["prerelease"])
	}
	// GitHub 表达式里 A && B || C 在 B 为真值时等于三元运算：预发布版得 'false'，正式版得 'true'。
	if with["make_latest"] != "${{ needs.tag.outputs.prerelease == 'true' && 'false' || 'true' }}" {
		t.Errorf("make_latest：%q", with["make_latest"])
	}
}
