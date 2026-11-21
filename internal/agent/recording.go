package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/pkg/logstore"
	"github.com/SIPC/nagi/internal/pkg/recording"
	"github.com/SIPC/nagi/internal/pkg/sealer"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func (a *agent) openRecordings(ctx context.Context) func() {
	if a.cfg.Logs == nil {
		return func() {}
	}
	path := filepath.Join(a.cfg.DataDir, "logs", "recordings.db")
	keyPath := filepath.Join(a.cfg.DataDir, "identity", "recording.key")
	// Never replace a lost key with a new one over an existing encrypted history.
	_, keyErr := os.Stat(keyPath)
	_, historyErr := os.Stat(path)
	if errors.Is(keyErr, os.ErrNotExist) && !errors.Is(historyErr, os.ErrNotExist) {
		a.log.Error("录制密钥缺失，保留原正文，停止新增录制")
		return func() {}
	}
	key, err := sealer.Load(keyPath)
	if err == nil {
		r := &recording.Recordings{Sealer: key, FlushInterval: time.Duration(a.cfg.LogConfig.RecordingFlushSeconds) * time.Second}
		err = r.UseDatabase(ctx, path, a.cfg.Logs)
		if err == nil {
			a.recordings = r
			return func() { r.Close() }
		}
	}
	a.log.Error("无法打开本机加密录制，终端仍可用，新增录制未入库", "err", err)
	return func() {}
}

// The authenticated control stream supplies policy and receives only an ID and
// bounded summary. Bodies stay on the execution node and are read through mesh.
func (s *agentService) startRecording(stream grpc.ServerStream, cols, rows int, project, object, kind string) (*recording.Recorder, func()) {
	requested := recording.CaptureRequested(stream.Context())
	var rec *recording.Recorder
	if requested {
		rec = s.node.recordings.Start(stream.Context(), cols, rows)
	}
	_ = stream.SendHeader(metadata.Pairs(recording.IDHeader, rec.ID()))
	if requested && rec == nil {
		s.node.cfg.Logs.Report(logstore.Event{Category: logstore.System, Kind: "terminal.recording_unavailable", Project: project, Object: object,
			Data: map[string]any{"panel": s.panel, "operation": kind}})
	}
	var once sync.Once
	return rec, func() {
		once.Do(func() {
			if rec == nil {
				return
			}
			summary := rec.Close()
			s.node.cfg.Logs.Report(logstore.Event{Category: logstore.Audit, Kind: "terminal.recording", Project: project, Object: object,
				Data: map[string]any{"panel": s.panel, "operation": kind, "recording": summary}})
			// A full chunk manifest can exceed gRPC's metadata limit. Its durable
			// copy stays in node audit history, not in a control-stream trailer.
			summary.Chunks = nil
			body, _ := json.Marshal(summary)
			stream.SetTrailer(metadata.Pairs(recording.SummaryTrailer, string(body)))
		})
	}
}

func (s *meshService) ReadRecording(q *nagiv1.LocalRecordingQuery, stream nagiv1.LogService_ReadRecordingServer) error {
	ctx := stream.Context()
	if err := s.checkMeshPanel(ctx, nagiv1.LogService_ReadRecording_FullMethodName); err != nil {
		return err
	}
	if !recording.ValidID(q.GetId()) {
		return status.Error(codes.InvalidArgument, "无效的录制编号")
	}
	if s.a.recordings == nil {
		return status.Error(codes.Unavailable, "节点录制库没有启动")
	}
	err := s.a.recordings.Read(q.GetId(), func(body []byte) error {
		for len(body) > 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := s.checkMeshPanel(ctx, nagiv1.LogService_ReadRecording_FullMethodName); err != nil {
				return err
			}
			n := min(len(body), 64<<10)
			if err := stream.Send(&nagiv1.LocalRecordingChunk{Data: body[:n]}); err != nil {
				return err
			}
			body = body[n:]
		}
		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return status.FromContextError(ctx.Err()).Err()
		}
		if _, ok := status.FromError(err); ok {
			return err
		}
		return status.Error(codes.NotFound, "录制不存在、已清理或无法读取")
	}
	return nil
}
