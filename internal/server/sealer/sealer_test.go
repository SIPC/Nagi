package sealer

import (
	"path/filepath"
	"testing"
)

func TestSealOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.key")
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	ct, err := s.Seal([]byte("secret"), []byte("n1/p1"))
	if err != nil {
		t.Fatal(err)
	}
	// 重新加载同一个密钥文件也能解密。
	s2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	pt, err := s2.Open(ct, []byte("n1/p1"))
	if err != nil || string(pt) != "secret" {
		t.Fatalf("open: %q %v", pt, err)
	}
	if _, err := s2.Open(ct, []byte("n1/p2")); err == nil {
		t.Fatal("expected failure with different context")
	}
}
