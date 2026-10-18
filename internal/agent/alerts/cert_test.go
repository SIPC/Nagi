package alerts

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCertExpiries 检查只看还在用的域名、跳过本地 CA 签的证书。
func TestCertExpiries(t *testing.T) {
	dir := t.TempDir()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	write := func(issuer, domain string) {
		tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: domain},
			NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{domain}}
		der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		d := filepath.Join(dir, "certificates", issuer, domain)
		os.MkdirAll(d, 0o755)
		os.WriteFile(filepath.Join(d, domain+".crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
	}
	write("acme-v02.api.letsencrypt.org-directory", "a.com")
	write("acme-v02.api.letsencrypt.org-directory", "old.com")
	write("local", "web.localhost")
	e := &Engine{certDir: dir, Domains: func() map[string]bool { return map[string]bool{"a.com": true, "web.localhost": true} }}
	got := e.certExpiries()
	if len(got) != 1 || got["a.com"].IsZero() {
		t.Fatalf("got %v", got)
	}
}
