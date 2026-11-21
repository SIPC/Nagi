package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/SIPC/nagi/internal/pkg/recording"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/audit"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// sessionRecording preserves legacy nodes while new nodes keep their own body.
// Its embedded nil Recorder makes bridge Input/Output/Resize no-ops remotely.
type sessionRecording struct {
	*audit.Recorder
	remote string
	stream grpc.ClientStream
}

func (a *API) recordingContext(ctx context.Context) context.Context {
	return recording.WithCapture(ctx, a.RecordingEnabled(ctx))
}

func (a *API) startSessionRecording(ctx context.Context, stream grpc.ClientStream, nodeID string, cols, rows int) *sessionRecording {
	r := &sessionRecording{}
	md, _ := metadata.FromOutgoingContext(ctx)
	if values := md.Get(recording.CaptureHeader); len(values) != 1 || values[0] != "1" {
		return r
	}
	header, err := stream.Header()
	if err != nil {
		return r
	}
	if ids := header.Get(recording.IDHeader); len(ids) > 0 {
		if len(ids) == 1 && recording.ValidID(ids[0]) {
			r.remote, r.stream = recording.Reference(nodeID, ids[0]), stream
		}
		return r // recognized node: never silently make a panel body instead
	}
	r.Recorder = a.Recordings.Start(ctx, cols, rows)
	return r
}

func (r *sessionRecording) ID() string {
	if r == nil {
		return ""
	}
	if r.remote != "" {
		return r.remote
	}
	return r.Recorder.ID()
}

func (r *sessionRecording) Close() audit.Summary {
	if r == nil {
		return audit.Summary{}
	}
	if r.remote == "" {
		return r.Recorder.Close()
	}
	// A lost final trailer is not proof of a fully persisted recording.
	result := audit.Summary{ID: r.remote, Truncated: true}
	if values := r.stream.Trailer().Get(recording.SummaryTrailer); len(values) == 1 {
		var summary audit.Summary
		_, id, _ := recording.ParseReference(r.remote)
		if json.Unmarshal([]byte(values[0]), &summary) == nil && summary.ID == id {
			summary.ID = r.remote
			return summary
		}
	}
	return result
}

// WebSocket bridges stop at exit_code, but gRPC exposes trailers only after EOF.
func (r *sessionRecording) receive(recv func() (*nagiv1.ExecOutput, error)) (*nagiv1.ExecOutput, error) {
	msg, err := recv()
	if err == nil && r.remote != "" {
		if _, done := msg.Output.(*nagiv1.ExecOutput_ExitCode); done {
			_, _ = recv()
		}
	}
	return msg, err
}

func (a *API) readNodeRecording(ctx context.Context, nodeID, id string, send func([]byte) error) error {
	cc, err := a.logConnection(nodeID)
	if err != nil {
		return err
	}
	defer cc.Close()
	stream, err := nagiv1.NewLogServiceClient(cc).ReadRecording(ctx, &nagiv1.LocalRecordingQuery{Id: id})
	if err != nil {
		return toConnect(err)
	}
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return toConnect(err)
		}
		if err := send(chunk.GetData()); err != nil {
			return err
		}
	}
}
