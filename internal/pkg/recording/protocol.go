package recording

import (
	"context"
	"regexp"
	"strings"

	"google.golang.org/grpc/metadata"
)

// Control requests convey policy, not recording bodies. Missing policy means an
// older panel: do not silently start a second capture behind its privacy switch.
const (
	CaptureHeader  = "nagi-recording-capture"
	IDHeader       = "nagi-recording-id"
	SummaryTrailer = "nagi-recording-summary"
)

func WithCapture(ctx context.Context, enabled bool) context.Context {
	value := "0"
	if enabled {
		value = "1"
	}
	md, _ := metadata.FromOutgoingContext(ctx)
	md = md.Copy()
	md.Set(CaptureHeader, value)
	return metadata.NewOutgoingContext(ctx, md)
}

func CaptureRequested(ctx context.Context) bool {
	md, _ := metadata.FromIncomingContext(ctx)
	values := md.Get(CaptureHeader)
	return len(values) == 1 && values[0] == "1"
}

var nodeIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// Reference keeps existing playback APIs and audit links working without
// introducing a second, panel-centralized index of node recording locations.
func Reference(node, id string) string {
	if !nodeIDRe.MatchString(node) || !ValidID(id) {
		return ""
	}
	return "node:" + node + ":" + id
}

func ParseReference(ref string) (node, id string, ok bool) {
	parts := strings.Split(ref, ":")
	if len(parts) != 3 || parts[0] != "node" || Reference(parts[1], parts[2]) != ref {
		return "", "", false
	}
	return parts[1], parts[2], true
}
