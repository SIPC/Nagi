package directdl

import (
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestSignVerify(t *testing.T) {
	key, other := []byte("0123456789abcdef0123456789abcdef"), []byte("another key another key another!")
	now := time.Unix(1_800_000_000, 0)
	c := Claims{Node: "n1", Kind: KindVolume, Project: "p", Container: "web", Volume: "data", Path: "a/b.txt", Expires: now.Add(TTL).Unix()}
	tok := Sign(key, c)
	if got, err := Verify([][]byte{other, key}, tok, now); err != nil || got != c {
		t.Fatalf("应该通过：%+v %v", got, err)
	}
	if _, err := Verify([][]byte{other}, tok, now); err != ErrInvalid {
		t.Errorf("别的密钥不该通过：%v", err)
	}
	if _, err := Verify([][]byte{key}, tok, now.Add(TTL+time.Second)); err != ErrExpired {
		t.Errorf("过期的不该通过：%v", err)
	}
	// 改了内容（换一个文件）签名就对不上。
	payload, sig, _ := strings.Cut(tok, ".")
	forged := Sign([]byte("x"), Claims{Node: "n1", Kind: KindVolume, Path: "/etc/shadow", Expires: c.Expires})
	fp, _, _ := strings.Cut(forged, ".")
	if _, err := Verify([][]byte{key}, fp+"."+sig, now); err != ErrInvalid {
		t.Errorf("改过内容的不该通过：%v", err)
	}
	for _, bad := range []string{"", "abc", payload, payload + ".", "." + sig, strings.Repeat("a", 5000)} {
		if _, err := Verify([][]byte{key}, bad, now); err != ErrInvalid {
			t.Errorf("%.20q 不该通过：%v", bad, err)
		}
	}
	// 没有密钥（节点还没收到面板的密钥）时什么都不通过。
	if _, err := Verify(nil, tok, now); err != ErrInvalid {
		t.Errorf("没有密钥时不该通过：%v", err)
	}
}

func TestURLAndPublicAddress(t *testing.T) {
	if got := URL("203.0.113.5", "t"); got != "https://203.0.113.5/.nagi/download/t" {
		t.Error(got)
	}
	if got := URL("2001:db8::1", "t"); got != "https://[2001:db8::1]/.nagi/download/t" {
		t.Error(got)
	}
	for addr, want := range map[string]bool{
		"8.8.8.8": true, "2606:4700::1111": true,
		"10.0.0.1": false, "192.168.1.2": false, "172.16.0.1": false, "100.64.1.1": false, "127.0.0.1": false,
		"169.254.1.1": false, "fd00::1": false, "fe80::1": false, "::1": false,
	} {
		if got := PublicAddress(netip.MustParseAddr(addr)); got != want {
			t.Errorf("%s：%v，想要 %v", addr, got, want)
		}
	}
}
