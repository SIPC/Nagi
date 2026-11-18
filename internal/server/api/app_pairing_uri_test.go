package api

import (
	"crypto/sha256"
	"encoding/base64"
	"net/url"
	"testing"
)

func TestAppPairingURI(t *testing.T) {
	sum := sha256.Sum256([]byte("spki"))
	fp := "sha256//" + base64.StdEncoding.EncodeToString(sum[:])
	got, err := appPairingURI("203.0.113.5:8443", "nagi.example.com", fp, "abc_DEF-123")
	if err != nil {
		t.Fatal(err)
	}
	want := "nagi-app://pair?v=2&a=203.0.113.5:8443&d=nagi.example.com&k=" + base64.RawURLEncoding.EncodeToString(sum[:16]) + "&c=abc_DEF-123"
	if got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
	u, err := url.Parse(got)
	if err != nil || u.Query().Get("a") != "203.0.113.5:8443" {
		t.Fatalf("parse: %v %v", u, err)
	}
	if _, err := appPairingURI("[2001:db8::1]:8443", "", "bad", "c"); err == nil {
		t.Fatal("bad fingerprint accepted")
	}
}
