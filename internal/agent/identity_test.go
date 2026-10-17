package agent

import (
	"errors"
	"testing"
)

func TestTrustList(t *testing.T) {
	dir := t.TempDir()
	id, err := loadIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = id.addPanel(trustedPanel{Address: "a:1", Fingerprint: "fa"})

	// 另一个进程（本地命令）直接改了文件：先加一个，节点进程随后的修改不能把它覆盖掉。
	cli, _ := loadIdentity(dir)
	_ = cli.addPanel(trustedPanel{Address: "b:1", Fingerprint: "fb"})
	_ = id.addPanel(trustedPanel{Address: "a:2", Fingerprint: "fa"})
	if got := len(id.Panels()); got != 2 {
		t.Fatalf("应该有 2 个面板，实际 %d", got)
	}
	if p, _ := id.panel("fa"); p.Address != "a:2" {
		t.Fatalf("地址没更新：%+v", p)
	}

	if _, err := id.removePanel("b:1", false); err != nil {
		t.Fatal(err)
	}
	if _, err := id.removePanel("fa", false); !errors.Is(err, errLastPanel) {
		t.Fatalf("不应该能移除最后一个面板：%v", err)
	}
	if _, err := id.removePanel("nope", true); err == nil {
		t.Fatal("移除不存在的面板应该报错")
	}
	if _, err := id.removePanel("fa", true); err != nil || len(id.Panels()) != 0 {
		t.Fatalf("force 应该能移除最后一个：%v", err)
	}
	if err := cli.reload(); err != nil || len(cli.Panels()) != 0 {
		t.Fatalf("reload 后应该看到空列表：%v %d", err, len(cli.Panels()))
	}
}
