package release

import (
	"strings"
	"testing"
)

func TestSections(t *testing.T) {
	md := "# 更新日志\n\n说明\n\n## v0.3.0\n\n### 修复\n\n- 三。\n\n## v0.2.1\n\n- 二点一。\n\n## v0.2.0\n\n- 二。\n\n## v0.1.0\n\n- 一。\n"
	got := Sections(md, "v0.2.0", "v0.3.0")
	if len(got) != 2 || got[0].Version != "v0.3.0" || got[1].Version != "v0.2.1" {
		t.Fatalf("应取 v0.3.0 和 v0.2.1：%+v", got)
	}
	if got[0].Body != "### 修复\n\n- 三。" {
		t.Errorf("正文：%q", got[0].Body)
	}
	if got := Sections(md, "v0.3.0", "v0.3.0"); len(got) != 0 {
		t.Errorf("已是最新时没有：%+v", got)
	}
	// 镜像里放的可能是更新的版本：只取到 to 为止。
	if got := Sections(md, "v0.1.0", "v0.2.0"); len(got) != 1 || got[0].Version != "v0.2.0" {
		t.Errorf("只取到 v0.2.0：%+v", got)
	}
}

// 更新到预发布版时列出各预发布版的小节；更新到正式版时只列正式版，正式版的条目已经汇总了各预发布版。
func TestSectionsPrerelease(t *testing.T) {
	md := "## v0.38.0\n\n- 正式。\n\n## v0.38.0-pre.2\n\n- 二。\n\n## v0.38.0-pre.1\n\n- 一。\n\n## v0.37.0\n\n- 旧。\n"
	versions := func(s []Section) (out []string) {
		for _, x := range s {
			out = append(out, x.Version)
		}
		return out
	}
	if got := versions(Sections(md, "v0.37.0", "v0.38.0-pre.2")); strings.Join(got, ",") != "v0.38.0-pre.2,v0.38.0-pre.1" {
		t.Errorf("到 pre.2：%v", got)
	}
	if got := versions(Sections(md, "v0.38.0-pre.1", "v0.38.0-pre.2")); strings.Join(got, ",") != "v0.38.0-pre.2" {
		t.Errorf("pre.1 到 pre.2：%v", got)
	}
	if got := versions(Sections(md, "v0.38.0-pre.2", "v0.38.0")); strings.Join(got, ",") != "v0.38.0" {
		t.Errorf("pre.2 到正式版：%v", got)
	}
	if got := versions(Sections(md, "v0.37.0", "v0.38.0")); strings.Join(got, ",") != "v0.38.0" {
		t.Errorf("上一个正式版到正式版：%v", got)
	}
}
