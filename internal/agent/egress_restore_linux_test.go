package agent

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// L11：节点重启后 dockerd 先拉起容器，nagi 列完容器之前没有出站规则。现在启动时最先加载上次应用的规则。
// 用一个假的 nft 记下收到的规则。
func TestRestoreEgress(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	_ = os.MkdirAll(bin, 0o755)
	got := filepath.Join(dir, "got.nft")
	script := "#!/bin/sh\ncat > " + got + "\n"
	if err := os.WriteFile(filepath.Join(bin, "nft"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	log := slog.New(slog.DiscardHandler)

	// 没有缓存：什么也不做。
	restoreEgress(dir, log)
	if _, err := os.Stat(got); !os.IsNotExist(err) {
		t.Fatal("没有缓存时不应调用 nft")
	}
	rules := "table inet nagi\ndelete table inet nagi\ntable inet nagi {\n}\n"
	if err := writeFileAtomic(egressCacheFile(dir), []byte(rules), 0o600); err != nil {
		t.Fatal(err)
	}
	restoreEgress(dir, log)
	b, err := os.ReadFile(got)
	if err != nil || string(b) != rules {
		t.Fatalf("启动时应加载上次的规则：%q %v", b, err)
	}
	if st, _ := os.Stat(egressCacheFile(dir)); st.Mode().Perm() != 0o600 {
		t.Fatalf("规则文件权限 %v", st.Mode().Perm())
	}
}
