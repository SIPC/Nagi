package nodes

import (
	"context"
	"encoding/json"

	"github.com/SIPC/nagi/internal/pkg/logstore"
	"github.com/SIPC/nagi/internal/pkg/mesh"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func (p *panelMesh) authorizeLogPanel(ctx context.Context) error {
	k, ok := mesh.Caller(ctx, p.M)
	if !ok || p.r.AuthorizeLogPanel == nil {
		return status.Error(codes.PermissionDenied, "无法确认主备日志成员")
	}
	if err := p.r.AuthorizeLogPanel(ctx, k); err != nil {
		return status.Error(codes.PermissionDenied, "主备日志成员已撤权或无法核验")
	}
	if p.r.LogCopies == nil {
		return status.Error(codes.Unavailable, "近期日志副本没有启动")
	}
	return nil
}

func (p *panelMesh) ListLogCopies(ctx context.Context, q *nagiv1.PanelLogPosition) (*nagiv1.PanelLogInventory, error) {
	if err := p.authorizeLogPanel(ctx); err != nil {
		return nil, err
	}
	pos, entries, err := p.r.LogCopies.Inventory(ctx, logstore.CopyPosition{Source: q.GetSource(), After: q.GetAfter()})
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	out := &nagiv1.PanelLogInventory{Position: &nagiv1.PanelLogPosition{Source: pos.Source, After: pos.After}}
	for _, e := range entries {
		out.Entries = append(out.Entries, &nagiv1.PanelLogIndex{Cursor: e.Cursor, Node: e.Node, Source: e.Source, Category: e.Category, Seq: e.Seq, Received: timestamppb.New(e.Received), Hash: e.Hash})
	}
	return out, nil
}

func (p *panelMesh) ReadLogCopies(ctx context.Context, q *nagiv1.PanelLogRead) (*nagiv1.PanelLogBodies, error) {
	if err := p.authorizeLogPanel(ctx); err != nil {
		return nil, err
	}
	if len(q.GetCursors()) > 8 {
		return nil, status.Error(codes.ResourceExhausted, "日志补缺请求过大")
	}
	items, err := p.r.LogCopies.Fetch(ctx, q.GetSource(), q.GetCursors())
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	out := &nagiv1.PanelLogBodies{}
	for _, v := range items {
		e := v.Event
		data, err := json.Marshal(e.Data)
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		out.Entries = append(out.Entries, &nagiv1.PanelLogBody{Cursor: v.Cursor, Node: v.Node, Source: e.Source, Entry: &nagiv1.LogEntry{Seq: e.Seq, Time: timestamppb.New(e.Time), Received: timestamppb.New(e.Received), Category: e.Category, Kind: e.Kind, Project: e.Project, Object: e.Object, DataJson: data}})
	}
	return out, nil
}
