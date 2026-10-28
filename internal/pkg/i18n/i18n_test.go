package i18n

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestParse(t *testing.T) {
	cases := map[string]Lang{
		"":                        Zh,
		"en":                      En,
		"en-US,en;q=0.9":          En,
		"zh-CN,zh;q=0.9,en;q=0.8": Zh,
		"fr-FR,en;q=0.5,zh;q=0.4": En,
		"zh;q=0.3,en-GB;q=0.7":    En,
		"de, ja":                  Zh,
		"zh-Hant-TW":              Zh,
		"en;q=0, zh-TW;q=0.5":     Zh,
		" EN-us ; q=1 ":           En,
		"*":                       Zh,
	}
	for in, want := range cases {
		if got := Parse(in); got != want {
			t.Errorf("Parse(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestContext(t *testing.T) {
	if FromContext(context.Background()) != Zh {
		t.Fatal("默认应该是中文")
	}
	if FromContext(WithLang(context.Background(), En)) != En {
		t.Fatal("没有取到 ctx 里的语言")
	}
}

func withCatalog(t *testing.T, entries map[string]string) {
	t.Helper()
	old := catalogs[En]
	m := map[string]string{}
	for k, v := range old {
		m[k] = v
	}
	for k, v := range entries {
		m[k] = v
	}
	catalogs[En] = m
	t.Cleanup(func() { catalogs[En] = old })
}

type wrapper struct{ err error }

func (w wrapper) Error() string { return w.err.Error() }
func (w wrapper) Unwrap() error { return w.err }

func TestLocalize(t *testing.T) {
	withCatalog(t, map[string]string{
		"节点 %s 不在线":       "Node %s is offline",
		"部署失败：%w":         "Deploy failed: %w",
		"未登录":             "Not signed in",
		"%[1]s 有 %[2]d 个": "%[2]d in %[1]s",
	})
	base := errors.New("底层错误")
	offline := Errorf("节点 %s 不在线", "n1")
	wrapped := Errorf("部署失败：%w", offline)

	if got := offline.Error(); got != "节点 n1 不在线" {
		t.Errorf("Error() = %q", got)
	}
	if got := Localize(En, offline); got != "Node n1 is offline" {
		t.Errorf("Localize = %q", got)
	}
	if got := Localize(Zh, offline); got != "节点 n1 不在线" {
		t.Errorf("Localize(Zh) = %q", got)
	}
	// 被包着的错误也换成译文，errors.Is 照常可用。
	if got := Localize(En, wrapped); got != "Deploy failed: Node n1 is offline" {
		t.Errorf("嵌套 = %q", got)
	}
	if got := wrapped.Error(); got != "部署失败：节点 n1 不在线" {
		t.Errorf("嵌套 Error() = %q", got)
	}
	if !errors.Is(wrapped, offline) {
		t.Error("errors.Is 应该能找到被包着的错误")
	}
	inner := Errorf("部署失败：%w", base)
	if !errors.Is(inner, base) {
		t.Error("errors.Is 应该能找到 %w 包着的普通错误")
	}
	if got := Localize(En, inner); got != "Deploy failed: 底层错误" {
		t.Errorf("没有译文的参数应原样显示，got %q", got)
	}
	// 文字相同的包装（例如参数错误类型）穿透；文字不同的包装不穿透。
	if got := Localize(En, wrapper{offline}); got != "Node n1 is offline" {
		t.Errorf("透明包装 = %q", got)
	}
	if got := Localize(En, fmt.Errorf("外层：%w", offline)); got != "外层：节点 n1 不在线" {
		t.Errorf("普通包装 = %q", got)
	}
	// New 不按格式串解释；普通错误整条文字有译文时也换。
	if got := Localize(En, New("未登录")); got != "Not signed in" {
		t.Errorf("New = %q", got)
	}
	if got := Localize(En, New("100% 完成")); got != "100% 完成" {
		t.Errorf("New 带 %% = %q", got)
	}
	if got := Localize(En, errors.New("未登录")); got != "Not signed in" {
		t.Errorf("整条匹配 = %q", got)
	}
	if got := Sprintf(En, "%[1]s 有 %[2]d 个", "a", 3); got != "3 in a" {
		t.Errorf("调整顺序 = %q", got)
	}
	if got := T(WithLang(context.Background(), En), "节点 %s 不在线", "n2"); got != "Node n2 is offline" {
		t.Errorf("T = %q", got)
	}
	if Localize(En, nil) != "" {
		t.Error("nil 应返回空")
	}
}
