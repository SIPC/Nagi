package nodes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/SIPC/nagi/internal/pkg/logstore"
	"github.com/SIPC/nagi/internal/pkg/mesh"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func (p *panelMesh) logCopyNode(ctx context.Context) (string, error) {
	key, ok := mesh.Caller(ctx, p.M)
	if !ok {
		return "", status.Error(codes.PermissionDenied, "无法确认日志来源")
	}
	return p.r.logCopyNodeKey(ctx, key)
}

func (r *Registry) logCopyNodeKey(ctx context.Context, key mesh.Key) (string, error) {
	rows, err := r.q.ListNodes(ctx)
	if err != nil {
		return "", status.Error(codes.Unavailable, "读取已登记节点失败")
	}
	isolated, _, err := r.q.IsolatedNodes(ctx)
	if err != nil {
		return "", status.Error(codes.Unavailable, "读取节点隔离状态失败")
	}
	for _, n := range rows {
		if bytes.Equal(n.MeshKey, key[:]) && n.Pending == 0 && n.Revoked == 0 {
			for _, id := range isolated {
				if id == n.ID {
					return "", status.Error(codes.PermissionDenied, "节点已隔离")
				}
			}
			return n.ID, nil
		}
	}
	return "", status.Error(codes.PermissionDenied, "日志来源不是已批准且未隔离的节点")
}

func (p *panelMesh) GetLogReceipt(ctx context.Context, q *nagiv1.LogReceiptQuery) (*nagiv1.LogReceipt, error) {
	id, err := p.logCopyNode(ctx)
	if err != nil {
		return nil, err
	}
	r, err := p.r.LogCopies.Receipt(ctx, id, q.GetSource(), q.GetCategory())
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	return &nagiv1.LogReceipt{Source: q.GetSource(), Category: q.GetCategory(), After: r.After}, nil
}

func (p *panelMesh) PushLogs(ctx context.Context, q *nagiv1.LogCopyBatch) (*nagiv1.LogReceipt, error) {
	id, err := p.logCopyNode(ctx)
	if err != nil {
		return nil, err
	}
	if proto.Size(q) > 1<<20 || len(q.GetEntries()) > 256 {
		return nil, status.Error(codes.ResourceExhausted, "日志批次过大")
	}
	done, ok := p.r.CopyIntake.Begin(q.GetCatchup())
	if !ok {
		return nil, status.Error(codes.ResourceExhausted, "面板正忙，稍后再追赶近期日志副本")
	}
	defer done()
	b := logstore.CopyBatch{Source: q.GetSource(), Category: q.GetCategory(), After: q.GetAfter(), Through: q.GetThrough(), WriteErrors: q.GetWriteErrors()}
	for _, e := range q.GetEntries() {
		if e.GetTime() == nil || e.GetTime().CheckValid() != nil || !json.Valid(e.GetDataJson()) {
			return nil, status.Error(codes.InvalidArgument, "无效的日志时间或正文")
		}
		b.Events = append(b.Events, logstore.Event{Seq: e.GetSeq(), Time: e.GetTime().AsTime(), Category: e.GetCategory(), Kind: e.GetKind(), Project: e.GetProject(), Object: e.GetObject(), Data: json.RawMessage(e.GetDataJson())})
	}
	for _, v := range q.GetPruned() {
		b.Pruned = append(b.Pruned, logstore.Pruned{Category: v.GetCategory(), First: v.GetFirst(), Last: v.GetLast(), Count: v.GetCount()})
	}
	r, err := p.r.LogCopies.Receive(ctx, id, b)
	if errors.Is(err, logstore.ErrCopyConflict) {
		p.r.log.Warn("日志副本重发正文冲突，保留已存证据", "node", id, "source", q.GetSource(), "category", q.GetCategory())
		return nil, status.Error(codes.DataLoss, err.Error())
	}
	if errors.Is(err, logstore.ErrCopyCursor) {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	return &nagiv1.LogReceipt{Source: q.GetSource(), Category: q.GetCategory(), After: r.After}, nil
}
