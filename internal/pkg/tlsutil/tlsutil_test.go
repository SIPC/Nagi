package tlsutil

import (
	"crypto/tls"
	"testing"
	"time"
)

func TestPinnedHandshakeAndClientCert(t *testing.T) {
	ca, err := NewCA("test")
	if err != nil {
		t.Fatal(err)
	}
	serverKey, _ := GenerateKey()
	serverCert, err := ca.IssueServer(serverKey, []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}

	nodeKey, _ := GenerateKey()
	csr, err := CreateCSR(nodeKey, "node")
	if err != nil {
		t.Fatal(err)
	}
	nodeCert, err := ca.SignCSR(csr, "node-123", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if nodeCert.Subject.CommonName != "node-123" {
		t.Fatalf("CommonName = %q", nodeCert.Subject.CommonName)
	}

	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{serverCert.Raw}, PrivateKey: serverKey}},
		ClientAuth:   tls.VerifyClientCertIfGiven,
		ClientCAs:    ca.Pool(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	gotCN := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		tc := c.(*tls.Conn)
		if err := tc.Handshake(); err != nil {
			gotCN <- "handshake error: " + err.Error()
			return
		}
		state := tc.ConnectionState()
		if len(state.PeerCertificates) > 0 {
			gotCN <- state.PeerCertificates[0].Subject.CommonName
		} else {
			gotCN <- ""
		}
		tc.Close()
	}()

	cfg := PinnedConfig(Fingerprint(serverCert))
	cfg.Certificates = []tls.Certificate{{Certificate: [][]byte{nodeCert.Raw}, PrivateKey: nodeKey}}
	conn, err := tls.Dial("tcp", ln.Addr().String(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if cn := <-gotCN; cn != "node-123" {
		t.Fatalf("server saw client CN %q", cn)
	}
}

func TestPinnedRejectsWrongKey(t *testing.T) {
	ca, _ := NewCA("test")
	serverKey, _ := GenerateKey()
	serverCert, _ := ca.IssueServer(serverKey, []string{"127.0.0.1"})
	otherKey, _ := GenerateKey()
	otherCert, _ := ca.IssueServer(otherKey, []string{"127.0.0.1"})

	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{serverCert.Raw}, PrivateKey: serverKey}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			_ = c.(*tls.Conn).Handshake()
			c.Close()
		}
	}()

	_, err = tls.Dial("tcp", ln.Addr().String(), PinnedConfig(Fingerprint(otherCert)))
	if err == nil {
		t.Fatal("expected fingerprint mismatch")
	}
}
