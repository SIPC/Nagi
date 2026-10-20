package spec

import "testing"

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{
		"Blog":         "blog",
		"我的博客":         "wo-de-bo-ke",
		"Shop 商城 v2":   "shop-shang-cheng-v2",
		"echo-server":  "echo-server",
		"!!!":          "",
		"测试/Test_App": "ce-shi-test-app",
	} {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q，应该是 %q", in, got, want)
		}
	}
}
