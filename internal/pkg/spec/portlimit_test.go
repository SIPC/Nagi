package spec

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/docker/go-connections/nat"
)

// rangeBomb 是审查报告 2026-11-18-dddb82a#1 里的 compose：20 行 host_ip 不同的全范围端口，展开后 131 万个。
func rangeBomb() string {
	var b strings.Builder
	b.WriteString("services:\n  web:\n    image: nginx\n    ports:\n")
	for i := 1; i <= 20; i++ {
		fmt.Fprintf(&b, "      - \"10.0.0.%d:1-65535:1-65535\"\n", i)
	}
	return b.String()
}

// 超过上限的 compose 在交给解析库之前就被拒绝：保存校验、读取、权限检查、提醒都很快返回，不分配大量内存。
func TestComposePortRangeBomb(t *testing.T) {
	text := rangeBomb()
	cfg := &nagiv1.ContainerConfig{Compose: text}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	start := time.Now()

	if _, err := Validate(context.Background(), cfg, "web", nil, "/check", "check"); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "端口过多") {
		t.Errorf("保存校验应该以端口过多拒绝：%v", err)
	}
	if _, err := Load(context.Background(), cfg, "web", nil, "/check", "check"); err == nil {
		t.Error("已经保存的超限 compose 读取时应该报错")
	}
	if reasons, _ := PrivilegeReasons(cfg, "web"); len(reasons) == 0 {
		t.Error("解析不了的 compose 应该算有特权")
	}
	if _, err := AdviseCompose(text); err == nil {
		t.Error("AdviseCompose 应该报错")
	}

	elapsed := time.Since(start)
	runtime.ReadMemStats(&after)
	// 不检查前要十几秒、几个 GB；阈值放得很宽，免得在慢的 CI 上偶发失败。
	if elapsed > 5*time.Second {
		t.Errorf("用了 %v，应该很快拒绝", elapsed)
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 64<<20 {
		t.Errorf("分配了 %d MB，不应该展开端口", alloc>>20)
	}
	if n := testing.AllocsPerRun(3, func() { _ = CheckComposePorts(text, map[string]string{}) }); n > 5000 {
		t.Errorf("CheckComposePorts 每次分配 %v 次", n)
	}
}

func TestCheckComposePorts(t *testing.T) {
	ports := func(lines ...string) string {
		return "services:\n  web:\n    image: nginx\n    ports:\n      - " + strings.Join(lines, "\n      - ") + "\n"
	}
	cases := []struct {
		name, compose string
		env           map[string]string
		ok            bool
	}{
		{"普通端口", ports(`"8080:80"`, `"443:443/tcp"`, "53/udp", "9000", `"127.0.0.1:5432:5432"`), nil, true},
		{"游戏服务器的 UDP 端口段", ports(`"27015-27030:27015-27030/udp"`, `"27015:27015/tcp"`), nil, true},
		{"FTP 被动端口", ports(`"21:21"`, `"21100-21199:21100-21199"`), nil, true},
		{"正好上限", ports(`"1-1000:1-1000"`), nil, true},
		{"超过上限", ports(`"1-1001:1-1001"`), nil, false},
		{"只写容器端口的范围", ports(`"1-65535"`), nil, false},
		{"IPv6 地址", ports(`"[::1]:1-2000:1-2000"`), nil, false},
		{"对外端口是范围、容器端口只有一个", ports(`"8000-60000:80"`), nil, true},
		{"长写法", "services:\n  web:\n    image: nginx\n    ports:\n      - target: 80\n        published: \"8000-60000\"\n        host_ip: 10.0.0.1\n", nil, true},
		{"多个服务合计", "services:\n  a:\n    image: nginx\n    ports: [\"1-600:1-600\"]\n  b:\n    image: nginx\n    ports: [\"1-600:1-600\"]\n", nil, false},
		{"expose 的范围", "services:\n  web:\n    image: nginx\n    expose: [\"1-65535\"]\n", nil, false},
		{"expose 普通", "services:\n  web:\n    image: nginx\n    expose: [\"3000\", 4000, \"5000-5010/udp\"]\n", nil, true},
		{"锚点引用两次", "x-p: &p [\"10.0.0.1:1-600:1-600\"]\nservices:\n  a:\n    image: nginx\n    ports: *p\n  b:\n    image: nginx\n    ports: *p\n", nil, false},
		{"合并键", "x-base: &b\n  ports: [\"1-1001:1-1001\"]\nservices:\n  web:\n    image: nginx\n    <<: *b\n", nil, false},
		{"merge 标签的普通键", "x-p: &p\n  ports: [\"1-1001:1-1001\"]\nservices:\n  web:\n    image: nginx\n    !!merge foo: *p\n", nil, false},
		{"extends 继承", "services:\n  a:\n    image: nginx\n    ports: [\"1-600:1-600\"]\n  b:\n    extends: a\n", nil, false},
		{"extends 的 map 写法", "services:\n  a:\n    image: nginx\n    ports: [\"1-600:1-600\"]\n  b:\n    extends:\n      service: a\n", nil, false},
		{"多个文档", ports(`"1-600:1-600"`) + "---\n" + ports(`"10.0.0.1:1-600:1-600"`), nil, false},
		{"变量替换后的范围", ports(`"${R}:${R}"`), map[string]string{"R": "1-65535"}, false},
		{"变量的默认值", ports(`"${R:-1-65535}"`), map[string]string{}, false},
		{"变量是一个端口", ports(`"${P}:80"`), map[string]string{"P": "8080"}, true},
		{"不替换变量时按原文", ports(`"${R}:${R}"`), nil, true},
		// 审查报告 v0.35.0#3：解析库认 "+1"，计数也要认。
		{"起点带加号", ports(`"+1-65535:+1-65535"`), nil, false},
		{"终点带加号", ports(`"1-+65535:1-+65535"`), nil, false},
		{"两头都带加号", ports(`"+1-+65535"`), nil, false},
		{"带 host_ip 和加号", ports(`"10.0.0.1:+1-65535:+1-65535"`), nil, false},
		{"IPv6 地址和加号", ports(`"[::1]:+1-2000:+1-2000/udp"`), nil, false},
		{"带加号但没超过上限", ports(`"+8000-+8010:+8000-+8010"`), nil, true},
		{"expose 带加号", "services:\n  web:\n    image: nginx\n    expose: [\"+1-65535\", \"1-+65535/udp\"]\n", nil, false},
		{"变量替换后带加号", ports(`"${R}"`), map[string]string{"R": "+1-65535"}, false},
		{"含横线却读不了的", ports(`"1 - 2"`), nil, false},
		{"expose 含横线却读不了的", "services:\n  web:\n    image: nginx\n    expose: [\"0x1-0xffff\"]\n", nil, false},
		{"读不了但没有横线的", ports(`"abc"`), nil, true},
	}
	for _, c := range cases {
		err := CheckComposePorts(c.compose, c.env)
		if (err == nil) != c.ok {
			t.Errorf("%s：err = %v，应为 ok=%v", c.name, err, c.ok)
		}
		if err != nil && !errors.Is(err, ErrInvalid) {
			t.Errorf("%s：应该是参数错误：%v", c.name, err)
		}
	}
	if err := CheckComposePorts("services: [", nil); err == nil {
		t.Error("读不了的 YAML 应该报错")
	}
}

// 计数和解析库（go-connections 的 nat.ParsePortSpec）真正展开的端口数一致；解析库读不了、含横线的按超过上限算。
func TestPortRangeSizeMatchesCompose(t *testing.T) {
	for _, s := range []string{
		"80", "8080:80", "53/udp", "443:443/tcp", "127.0.0.1:5432:5432", "[::1]:80:80",
		"27015-27030:27015-27030/udp", "8000-60000:80", "1-1000", "80-80",
		"+80", "+1-1000", "1-+1000", "+1-+1000", "10.0.0.1:+1-500:+1-500", "[::1]:+1-500:+1-500/udp",
		"0-999", "00001-00010", "1-65535", "+1-65535", "10.0.0.1:1-+65535:1-+65535/sctp",
		"1-", "-5", "5-1", "1-70000", "+-1", "1 - 2", "0x1-0x10",
	} {
		got := portRangeSize(s)
		maps, err := nat.ParsePortSpec(s)
		switch {
		case err == nil && got != min(len(maps), MaxComposePorts+1):
			t.Errorf("%q：计数 %d，解析库展开 %d 个", s, got, len(maps))
		case err != nil && strings.Contains(s, "-") && got <= MaxComposePorts:
			t.Errorf("%q：解析库读不了（%v），含横线应按超过上限算，得到 %d", s, err, got)
		}
	}
	if got := portRangeSize("${R}"); got != 1 {
		t.Errorf("没替换的变量应该算一个，得到 %d", got)
	}
}

// 用 "+" 写的全范围（审查报告 v0.35.0#3）和不带 "+" 的一样很快被拒绝，不交给解析库展开。
func TestComposePortRangeBombPlus(t *testing.T) {
	var b strings.Builder
	b.WriteString("services:\n  web:\n    image: nginx\n    ports:\n")
	for i := 1; i <= 3; i++ {
		fmt.Fprintf(&b, "      - \"10.0.0.%d:+1-65535:+1-65535\"\n", i)
	}
	b.WriteString("    expose: [\"+1-65535\"]\n")
	text := b.String()
	cfg := &nagiv1.ContainerConfig{Compose: text}
	start := time.Now()
	if _, err := Validate(context.Background(), cfg, "web", nil, "/check", "check"); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "端口过多") {
		t.Errorf("保存校验应该以端口过多拒绝：%v", err)
	}
	if _, err := Load(context.Background(), cfg, "web", nil, "/check", "check"); err == nil {
		t.Error("已经保存的超限 compose 读取时应该报错")
	}
	if reasons, _ := PrivilegeReasons(cfg, "web"); len(reasons) == 0 {
		t.Error("解析不了的 compose 应该算有特权")
	}
	if _, err := AdviseCompose(text); err == nil {
		t.Error("AdviseCompose 应该报错")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("用了 %v，应该很快拒绝", elapsed)
	}
}

// 合理的端口和范围照常保存，导入时一个个列出来。
func TestValidateReasonablePorts(t *testing.T) {
	cfg := &nagiv1.ContainerConfig{Compose: "services:\n  game:\n    image: game:1\n    ports:\n      - \"27015-27030:27015-27030/udp\"\n      - \"8080:80\"\n"}
	cp, err := Validate(context.Background(), cfg, "game", nil, "/check", "check")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(ComposePorts("game", cp)); n != 17 {
		t.Errorf("应该有 17 个端口，得到 %d", n)
	}
	// 变量来自容器的变量：替换以后超过上限的拒绝。
	cfg = &nagiv1.ContainerConfig{Compose: "services:\n  game:\n    image: game:1\n    ports: [\"${R}:${R}/udp\"]\n",
		Env: []*nagiv1.EnvVar{{Key: "R", Value: "10000-20000"}}}
	if _, err := Validate(context.Background(), cfg, "game", nil, "/check", "check"); err == nil || !strings.Contains(err.Error(), "端口过多") {
		t.Errorf("变量替换后超过上限应该拒绝：%v", err)
	}
}
