package release

import "testing"

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
