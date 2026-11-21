package recording

import (
	"strings"
	"testing"

	"google.golang.org/grpc/metadata"
)

func TestCapturePolicyAndRemoteReferences(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		ctx := WithCapture(WithCapture(t.Context(), !enabled), enabled)
		md, _ := metadata.FromOutgoingContext(ctx)
		if CaptureRequested(metadata.NewIncomingContext(t.Context(), md)) != enabled {
			t.Fatal("new policy did not replace existing metadata")
		}
	}
	if CaptureRequested(t.Context()) || CaptureRequested(metadata.NewIncomingContext(t.Context(), metadata.Pairs(CaptureHeader, "1", CaptureHeader, "0"))) {
		t.Fatal("absent or ambiguous policy enabled capture")
	}
	const id = "202611-0123456789abcdef01234567"
	ref := Reference("node-123", id)
	if node, parsed, ok := ParseReference(ref); !ok || node != "node-123" || parsed != id {
		t.Fatalf("lost remote identity: %q %q %v", node, parsed, ok)
	}
	for _, bad := range []string{id, "node::" + id, "node:../other:" + id, "node:n1:../../key", ref + ":extra", strings.Replace(ref, id, strings.ToUpper(id), 1)} {
		if _, _, ok := ParseReference(bad); ok {
			t.Fatalf("accepted unsafe/ambiguous reference %q", bad)
		}
	}
}
