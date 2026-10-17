package nodes

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/pkg/tlsutil"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestRenewCertificateOnlyForOwnKey(t *testing.T) {
	ca, err := tlsutil.NewCA("test")
	if err != nil {
		t.Fatal(err)
	}
	own, _ := tlsutil.GenerateKey()
	other, _ := tlsutil.GenerateKey()
	ownCSR, _ := tlsutil.CreateCSR(own, "n")
	otherCSR, _ := tlsutil.CreateCSR(other, "n")

	ownCert, err := ca.SignCSR(ownCSR, "x", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	nodeID := NodeID(ownCert.RawSubjectPublicKeyInfo)

	r := &Registry{ca: ca, log: slog.Default()}
	svc := &panelService{r: r, conn: &Conn{NodeID: nodeID}}

	resp, err := svc.RenewCertificate(context.Background(), &nagiv1.RenewCertificateRequest{Csr: ownCSR})
	if err != nil {
		t.Fatalf("own key: %v", err)
	}
	if len(resp.GetCertificate()) == 0 {
		t.Fatal("empty certificate")
	}
	_, err = svc.RenewCertificate(context.Background(), &nagiv1.RenewCertificateRequest{Csr: otherCSR})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("other key: got %v, want PermissionDenied", err)
	}
}
