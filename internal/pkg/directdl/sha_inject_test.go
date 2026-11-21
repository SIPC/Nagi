package directdl

import (
	"strings"
	"testing"
)

// H1：SHA-256 的值来自节点，被攻破的节点可能给出带引号的值。两种命令只在它是 64 位十六进制时才写进去，并加引号。
func TestCommandRejectsBadSHA(t *testing.T) {
	evil := "x'); Invoke-Pwned calc; ('"
	c := Command{URL: "http://203.0.113.5/.nagi/dl/a.b", Name: "f.bin", SHA256: evil, Size: 10, Resume: true}
	for name, out := range map[string]string{"posix": c.String(), "windows": c.Windows()} {
		if strings.Contains(out, "Pwned") || strings.Contains(out, "calc") {
			t.Errorf("%s 命令里出现了节点给的值：%s", name, out)
		}
	}
	good := strings.Repeat("ab", 32)
	c.SHA256 = good
	if !strings.Contains(c.Windows(), "'"+good+"'") || !strings.Contains(c.String(), good) {
		t.Fatal("合法的 SHA-256 应写进命令")
	}
	for _, s := range []string{"", good[:63], good + "0", strings.Repeat("g", 64), strings.Repeat("A", 64)} {
		if want := len(s) == 64 && s != strings.Repeat("g", 64); ValidSHA256(s) != want {
			t.Errorf("ValidSHA256(%q) = %v", s, !want)
		}
	}
}
