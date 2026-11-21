package agent

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"time"

	"github.com/SIPC/nagi/internal/agent/fileops"
	"github.com/SIPC/nagi/internal/pkg/logstore"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 文件的改名、移动、复制、压缩、解压、目录大小和分块上传（见 docs/agent.md#存储、docs/node-access.md#文件）。
// 卷里的写操作和上传一样：有顶替的容器只在持有租约时能写，放进卷之前再查一次。节点文件先查节点访问的开关。

// fileOpProgressEvery 是长操作发进度的间隔。连接上一直有数据，中间的代理不会因为太久没有动静断开它。
const fileOpProgressEvery = 500 * time.Millisecond

// fileOpStream 运行 run，进行中每隔一会儿发一条进度，最后一条带结果。对方断开时 ctx 被取消，run 停下并清理。
func (s *agentService) fileOpStream(ctx context.Context, action, project, object string, send func(*nagiv1.FileOpEvent) error, run func(*fileops.Progress) (*nagiv1.FileOpResult, error)) error {
	prog := &fileops.Progress{}
	type result struct {
		res *nagiv1.FileOpResult
		err error
	}
	id, started := rand.Text(), time.Now()
	record := func(phase string, r result) {
		data := map[string]any{"task_id": id, "action": action, "phase": phase, "bytes": prog.Bytes.Load(), "files": prog.Files.Load()}
		if phase == "finished" {
			data["duration_ms"], data["result"] = time.Since(started).Milliseconds(), "ok"
			if r.err != nil {
				data["result"], data["error"] = "failed", r.err.Error()
				if errors.Is(r.err, context.Canceled) || status.Code(r.err) == codes.Canceled {
					data["result"] = "canceled"
				}
			}
			if r.res != nil {
				data["conflicts"], data["completed"], data["skipped"], data["skipped_links"] = len(r.res.Conflicts), len(r.res.Paths), r.res.Skipped, r.res.SkippedLinks
				if len(r.res.Conflicts) > 0 && r.err == nil {
					data["result"] = "conflict_no_change"
				}
			}
		}
		s.node.cfg.Logs.Report(logstore.Event{Category: logstore.Task, Kind: "files.task", Project: project, Object: object, Data: data})
	}
	record("started", result{})
	done := make(chan result, 1)
	go func() {
		res, err := run(prog)
		done <- result{res, err}
	}()
	tick := time.NewTicker(fileOpProgressEvery)
	defer tick.Stop()
	interval := time.Duration(s.node.cfg.LogConfig.FileProgressSeconds) * time.Second
	if interval <= 0 {
		interval = time.Second
	}
	last := started
	for {
		select {
		case r := <-done:
			record("finished", r)
			if r.err != nil {
				return toStatus(r.err)
			}
			return send(&nagiv1.FileOpEvent{Bytes: prog.Bytes.Load(), Files: prog.Files.Load(), Result: r.res})
		case <-tick.C:
			if time.Since(last) >= interval {
				record("progress", result{})
				last = time.Now()
			}
			if err := send(&nagiv1.FileOpEvent{Bytes: prog.Bytes.Load(), Files: prog.Files.Load()}); err != nil {
				r := <-done // 对方走了：ctx 已经取消，等 run 清理完
				record("finished", r)
				return err
			}
		case <-ctx.Done():
			r := <-done
			record("finished", r)
			return status.Error(codes.Canceled, "操作已取消")
		}
	}
}

func (s *agentService) TransferVolumeFiles(req *nagiv1.VolumeTransferRequest, stream nagiv1.AgentService_TransferVolumeFilesServer) error {
	check := func() error { return s.node.checkFileWrite(req.GetProjectId(), req.GetContainer()) }
	if err := check(); err != nil {
		return toStatus(err)
	}
	return s.fileOpStream(stream.Context(), "volume.transfer", req.GetProjectId(), req.GetContainer(), stream.Send, func(p *fileops.Progress) (*nagiv1.FileOpResult, error) {
		return s.projects.TransferVolumeFiles(stream.Context(), req, p, check)
	})
}

func (s *agentService) CompressVolumeFiles(req *nagiv1.VolumeCompressRequest, stream nagiv1.AgentService_CompressVolumeFilesServer) error {
	check := func() error { return s.node.checkFileWrite(req.GetProjectId(), req.GetContainer()) }
	if err := check(); err != nil {
		return toStatus(err)
	}
	return s.fileOpStream(stream.Context(), "volume.compress", req.GetProjectId(), req.GetContainer(), stream.Send, func(p *fileops.Progress) (*nagiv1.FileOpResult, error) {
		return s.projects.CompressVolumeFiles(stream.Context(), req, p, check)
	})
}

func (s *agentService) ExtractVolumeFile(req *nagiv1.VolumeExtractRequest, stream nagiv1.AgentService_ExtractVolumeFileServer) error {
	check := func() error { return s.node.checkFileWrite(req.GetProjectId(), req.GetContainer()) }
	if err := check(); err != nil {
		return toStatus(err)
	}
	return s.fileOpStream(stream.Context(), "volume.extract", req.GetProjectId(), req.GetContainer(), stream.Send, func(p *fileops.Progress) (*nagiv1.FileOpResult, error) {
		return s.projects.ExtractVolumeFile(stream.Context(), req, p, check)
	})
}

func (s *agentService) ListVolumeDirSizes(ctx context.Context, req *nagiv1.VolumeDirSizesRequest) (*nagiv1.DirSizesResponse, error) {
	out, err := s.projects.ListVolumeDirSizes(ctx, req)
	if err != nil {
		return nil, toStatus(err)
	}
	return out, nil
}

func (s *agentService) TransferNodeFiles(req *nagiv1.NodeTransferRequest, stream nagiv1.AgentService_TransferNodeFilesServer) error {
	if err := s.node.checkNodeAccess(); err != nil {
		return err
	}
	return s.fileOpStream(stream.Context(), "node.transfer", "", "", stream.Send, func(p *fileops.Progress) (*nagiv1.FileOpResult, error) {
		return s.node.files.Transfer(stream.Context(), req, p)
	})
}

func (s *agentService) CompressNodeFiles(req *nagiv1.NodeCompressRequest, stream nagiv1.AgentService_CompressNodeFilesServer) error {
	if err := s.node.checkNodeAccess(); err != nil {
		return err
	}
	return s.fileOpStream(stream.Context(), "node.compress", "", "", stream.Send, func(p *fileops.Progress) (*nagiv1.FileOpResult, error) {
		return s.node.files.Compress(stream.Context(), req, p)
	})
}

func (s *agentService) ExtractNodeFile(req *nagiv1.NodeExtractRequest, stream nagiv1.AgentService_ExtractNodeFileServer) error {
	if err := s.node.checkNodeAccess(); err != nil {
		return err
	}
	return s.fileOpStream(stream.Context(), "node.extract", "", "", stream.Send, func(p *fileops.Progress) (*nagiv1.FileOpResult, error) {
		return s.node.files.Extract(stream.Context(), req, p)
	})
}

func (s *agentService) ListNodeDirSizes(ctx context.Context, req *nagiv1.NodeDirSizesRequest) (*nagiv1.DirSizesResponse, error) {
	if err := s.node.checkNodeAccess(); err != nil {
		return nil, err
	}
	return s.node.files.DirSizes(ctx, req)
}

// ---- 分块上传（mesh） ----

var errNoTarget = status.Error(codes.InvalidArgument, "缺少上传的目标")

// uploadHistory records the local operation, not delivery of its RPC response.
// Do not copy file contents, the resumable fingerprint or full request parameters.
func (s *meshService) uploadHistory(action, project, object, id string, data map[string]any, err error) {
	data["action"], data["upload_id"], data["result"] = action, id, "ok"
	if err != nil {
		data["result"], data["error"] = "failed", err.Error()
		delete(data, "stored_offset")
	}
	s.a.cfg.Logs.Report(logstore.Event{Category: logstore.Task, Kind: "files.upload", Project: project, Object: object, Data: data})
}

// partReader 把 UploadPart 的消息拼成一个流：先是第一条里的内容，再是后面各条的。
func partReader(first *nagiv1.UploadPartChunk, recv func() (*nagiv1.UploadPartChunk, error)) *io.PipeReader {
	pr, pw := io.Pipe()
	go func() {
		if _, err := pw.Write(first.GetData()); err != nil {
			return
		}
		for {
			c, err := recv()
			if err != nil {
				if errors.Is(err, io.EOF) {
					err = nil
				}
				pw.CloseWithError(err)
				return
			}
			if _, err := pw.Write(c.GetData()); err != nil {
				return
			}
		}
	}()
	return pr
}

func (s *meshService) VolumeUploadBegin(ctx context.Context, req *nagiv1.UploadBeginRequest) (*nagiv1.UploadSession, error) {
	if err := s.checkMeshPanel(ctx, nagiv1.MeshService_VolumeUploadBegin_FullMethodName); err != nil {
		return nil, err
	}
	ref := req.GetVolume()
	if ref == nil {
		return nil, errNoTarget
	}
	if err := s.a.checkFileWrite(ref.GetProjectId(), ref.GetContainer()); err != nil {
		return nil, toStatus(err)
	}
	info, err := s.a.projects.BeginVolumeUpload(ctx, ref, req.GetSize(), req.GetKey())
	s.uploadHistory("volume.begin", ref.GetProjectId(), ref.GetContainer(), info.ID, map[string]any{"size": req.GetSize(), "stored_offset": info.Offset}, err)
	if err != nil {
		return nil, toStatus(err)
	}
	return &nagiv1.UploadSession{Id: info.ID, Offset: info.Offset, Size: info.Size}, nil
}

func (s *meshService) VolumeUploadPart(stream nagiv1.MeshService_VolumeUploadPartServer) error {
	if err := s.checkMeshPanel(stream.Context(), nagiv1.MeshService_VolumeUploadPart_FullMethodName); err != nil {
		return err
	}
	// 每一块都整块读进内存核对哈希，和节点文件的块一样占上传名额，限制同时占用的内存。
	release, err := acquireUpload()
	if err != nil {
		return err
	}
	defer release()
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	ref := first.GetVolume()
	if ref == nil {
		return errNoTarget
	}
	pr := partReader(first, stream.Recv)
	n, err := s.a.projects.AppendVolumeUpload(stream.Context(), ref, first.GetId(), first.GetOffset(), first.GetSha256(), pr)
	pr.Close()
	data := map[string]any{"requested_offset": first.GetOffset()}
	if err == nil {
		data["stored_offset"] = n
	}
	s.uploadHistory("volume.part", ref.GetProjectId(), ref.GetContainer(), first.GetId(), data, err)
	if err != nil {
		return toStatus(err)
	}
	return stream.SendAndClose(&nagiv1.UploadSession{Id: first.GetId(), Offset: n})
}

func (s *meshService) VolumeUploadCommit(ctx context.Context, req *nagiv1.UploadCommitRequest) (*nagiv1.VolumeUploadResponse, error) {
	if err := s.checkMeshPanel(ctx, nagiv1.MeshService_VolumeUploadCommit_FullMethodName); err != nil {
		return nil, err
	}
	ref := req.GetVolume()
	if ref == nil {
		return nil, errNoTarget
	}
	check := func() error { return s.a.checkFileWrite(ref.GetProjectId(), ref.GetContainer()) }
	if err := check(); err != nil {
		return nil, toStatus(err)
	}
	n, err := s.a.projects.CommitVolumeUpload(ctx, ref, req.GetId(), req.GetSha256(), check)
	s.uploadHistory("volume.commit", ref.GetProjectId(), ref.GetContainer(), req.GetId(), map[string]any{"size": n}, err)
	if err != nil {
		return nil, toStatus(err)
	}
	return &nagiv1.VolumeUploadResponse{Size: n}, nil
}

func (s *meshService) VolumeUploadAbort(ctx context.Context, req *nagiv1.UploadAbortRequest) (*nagiv1.UploadSession, error) {
	if err := s.checkMeshPanel(ctx, nagiv1.MeshService_VolumeUploadAbort_FullMethodName); err != nil {
		return nil, err
	}
	if req.GetVolume() == nil {
		return nil, errNoTarget
	}
	err := s.a.projects.AbortVolumeUpload(ctx, req.GetVolume(), req.GetId())
	s.uploadHistory("volume.abort", req.GetVolume().GetProjectId(), req.GetVolume().GetContainer(), req.GetId(), map[string]any{}, err)
	if err != nil {
		return nil, toStatus(err)
	}
	return &nagiv1.UploadSession{Id: req.GetId()}, nil
}

func (s *meshService) NodeFileUploadBegin(ctx context.Context, req *nagiv1.UploadBeginRequest) (*nagiv1.UploadSession, error) {
	if err := s.checkMeshNodeAccess(ctx, nagiv1.MeshService_NodeFileUploadBegin_FullMethodName); err != nil {
		return nil, err
	}
	if req.GetNode() == nil {
		return nil, errNoTarget
	}
	info, err := s.a.files.BeginUpload(req.GetNode().GetPath(), req.GetConfirmed(), req.GetSize(), req.GetKey())
	s.uploadHistory("node.begin", "", "", info.ID, map[string]any{"size": req.GetSize(), "stored_offset": info.Offset}, err)
	if err != nil {
		return nil, err
	}
	return &nagiv1.UploadSession{Id: info.ID, Offset: info.Offset, Size: info.Size}, nil
}

func (s *meshService) NodeFileUploadPart(stream nagiv1.MeshService_NodeFileUploadPartServer) error {
	if err := s.checkMeshNodeAccess(stream.Context(), nagiv1.MeshService_NodeFileUploadPart_FullMethodName); err != nil {
		return err
	}
	release, err := acquireUpload()
	if err != nil {
		return err
	}
	defer release()
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	if first.GetNode() == nil {
		return errNoTarget
	}
	pr := partReader(first, stream.Recv)
	n, err := s.a.files.AppendUpload(first.GetNode().GetPath(), first.GetId(), first.GetOffset(), first.GetSha256(), pr)
	pr.Close()
	data := map[string]any{"requested_offset": first.GetOffset()}
	if err == nil {
		data["stored_offset"] = n
	}
	s.uploadHistory("node.part", "", "", first.GetId(), data, err)
	if err != nil {
		return err
	}
	return stream.SendAndClose(&nagiv1.UploadSession{Id: first.GetId(), Offset: n})
}

func (s *meshService) NodeFileUploadCommit(ctx context.Context, req *nagiv1.UploadCommitRequest) (*nagiv1.VolumeUploadResponse, error) {
	if err := s.checkMeshNodeAccess(ctx, nagiv1.MeshService_NodeFileUploadCommit_FullMethodName); err != nil {
		return nil, err
	}
	if req.GetNode() == nil {
		return nil, errNoTarget
	}
	n, err := s.a.files.CommitUpload(req.GetNode().GetPath(), req.GetConfirmed(), req.GetId(), req.GetSha256())
	s.uploadHistory("node.commit", "", "", req.GetId(), map[string]any{"size": n}, err)
	if err != nil {
		return nil, err
	}
	return &nagiv1.VolumeUploadResponse{Size: n}, nil
}

func (s *meshService) NodeFileUploadAbort(ctx context.Context, req *nagiv1.UploadAbortRequest) (*nagiv1.UploadSession, error) {
	if err := s.checkMeshNodeAccess(ctx, nagiv1.MeshService_NodeFileUploadAbort_FullMethodName); err != nil {
		return nil, err
	}
	if req.GetNode() == nil {
		return nil, errNoTarget
	}
	err := s.a.files.AbortUpload(req.GetNode().GetPath(), req.GetId())
	s.uploadHistory("node.abort", "", "", req.GetId(), map[string]any{}, err)
	if err != nil {
		return nil, err
	}
	return &nagiv1.UploadSession{Id: req.GetId()}, nil
}
