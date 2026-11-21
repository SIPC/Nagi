package alerts

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// TestCertExpiries 检查只看还在用的域名、跳过本地 CA 签的证书。
func TestCertExpiries(t *testing.T) {
	dir := t.TempDir()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	write := func(issuer, domain string, validFor time.Duration) {
		tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: domain},
			NotBefore: time.Now(), NotAfter: time.Now().Add(validFor), DNSNames: []string{domain}}
		der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		d := filepath.Join(dir, "certificates", issuer, domain)
		os.MkdirAll(d, 0o755)
		os.WriteFile(filepath.Join(d, domain+".crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
	}
	write("acme-v02.api.letsencrypt.org-directory", "a.com", time.Hour)
	write("acme-v02.api.letsencrypt.org-directory", "old.com", time.Hour)
	write("local", "web.localhost", time.Hour)
	e := &Engine{certDir: dir, Domains: func() map[string]bool { return map[string]bool{"a.com": true, "web.localhost": true} }}
	got := e.CertExpiries()
	if len(got) != 1 || got["a.com"].IsZero() {
		t.Fatalf("got %v", got)
	}
	rcv := &receiver{}
	srv := httptest.NewServer(rcv)
	defer srv.Close()
	engine := newEngine(t, nil)
	engine.certDir, engine.Domains = dir, e.Domains
	o := &observations{}
	engine.metrics, engine.docker = o, o
	engine.SetConfig("p", &nagiv1.AlertConfig{Channels: []*nagiv1.AlertChannel{{Id: "c", Type: "webhook", Url: srv.URL}}})
	engine.evaluate(context.Background())
	if rcv.count() != 1 {
		t.Fatalf("expected expiring certificate: %v", rcv.got)
	}
	if err := os.Remove(filepath.Join(dir, "certificates", "acme-v02.api.letsencrypt.org-directory", "a.com", "a.com.crt")); err != nil {
		t.Fatal(err)
	}
	engine.evaluate(context.Background())
	if rcv.count() != 1 {
		t.Fatal("a missing certificate was reported as recovered")
	}
	write("acme-v02.api.letsencrypt.org-directory", "a.com", 30*24*time.Hour)
	engine.evaluate(context.Background())
	if rcv.count() != 2 || rcv.got[1]["status"] != "resolved" {
		t.Fatalf("renewed certificate was not reported as recovered: %v", rcv.got)
	}
}
