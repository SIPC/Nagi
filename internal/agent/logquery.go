package agent

import (
	"context"
	"encoding/json"
	"time"

	"github.com/SIPC/nagi/internal/pkg/logstore"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func (s *meshService) QueryMetrics(ctx context.Context, q *nagiv1.QueryMetricsRequest) (*nagiv1.QueryMetricsResponse, error) {
	if err := s.checkMeshPanel(ctx, nagiv1.LogService_QueryMetrics_FullMethodName); err != nil {
		return nil, err
	}
	return queryMetrics(ctx, s.a.metrics, q)
}

func (s *meshService) QueryLogs(ctx context.Context, q *nagiv1.LogQuery) (*nagiv1.LogPage, error) {
	if err := s.checkMeshPanel(ctx, nagiv1.LogService_QueryLogs_FullMethodName); err != nil {
		return nil, err
	}
	if s.a.cfg.Logs == nil {
		return nil, status.Error(codes.Unavailable, "本机日志库没有启动")
	}
	if q.GetAfter() < 0 || q.GetLimit() < 0 || q.GetLimit() > 1000 || q.GetAfter() > 0 && q.GetSource() == "" {
		return nil, status.Error(codes.InvalidArgument, "无效的日志游标或分页大小")
	}
	var migrations []*nagiv1.LogMigration
	for _, m := range s.a.cfg.Logs.Migrations() {
		migrations = append(migrations, &nagiv1.LogMigration{Id: m.ID, Phase: m.Phase, Completed: m.Completed, Total: m.Total, Error: m.Error})
	}
	if q.GetMigrationsOnly() {
		return &nagiv1.LogPage{Migrations: migrations}, nil
	}
	f := logstore.Filter{After: q.GetAfter(), Limit: int(q.GetLimit()), Category: q.GetCategory(), Project: q.GetProject(), Object: q.GetObject(), Search: q.GetSearch(), KindPrefix: q.GetKindPrefix()}
	for _, v := range []struct {
		in  *timestamppb.Timestamp
		out *time.Time
	}{{q.GetFrom(), &f.From}, {q.GetTo(), &f.To}} {
		if v.in != nil {
			if err := v.in.CheckValid(); err != nil {
				return nil, status.Error(codes.InvalidArgument, "无效的日志时间")
			}
			t := v.in.AsTime()
			if !time.Unix(0, t.UnixNano()).Equal(t) {
				return nil, status.Error(codes.InvalidArgument, "日志时间超出支持范围")
			}
			*v.out = t
		}
	}
	if !f.From.IsZero() && !f.To.IsZero() && !f.From.Before(f.To) {
		return nil, status.Error(codes.InvalidArgument, "日志开始时间必须早于结束时间")
	}
	page, err := s.a.cfg.Logs.ReadPage(ctx, f)
	if q.GetSource() != "" && page.State.Source != "" && page.State.Source != q.GetSource() {
		return nil, status.Error(codes.FailedPrecondition, "日志来源已变化，请从头查询；不能沿用旧游标")
	}
	if err != nil {
		if page.State.Source != "" && f.After > page.State.Last {
			return nil, status.Error(codes.InvalidArgument, "日志游标超过当前来源末尾")
		}
		return nil, toStatus(err)
	}
	out := &nagiv1.LogPage{Source: page.State.Source, LatestSeq: page.State.Last, NextAfter: page.Next, More: page.More,
		WriteErrors: page.State.WriteErrors, DiskBytes: page.State.DiskBytes, BudgetBytes: page.State.BudgetBytes, Migrations: migrations}
	for _, e := range page.Events {
		data, err := json.Marshal(e.Data)
		if err != nil {
			return nil, status.Error(codes.Internal, "日志正文损坏")
		}
		out.Entries = append(out.Entries, &nagiv1.LogEntry{Seq: e.Seq, Time: timestamppb.New(e.Time), Received: timestamppb.New(e.Received),
			Category: e.Category, Kind: e.Kind, Project: e.Project, Object: e.Object, DataJson: data})
	}
	for _, p := range page.State.Pruned {
		out.Pruned = append(out.Pruned, &nagiv1.LogPruned{Category: p.Category, First: p.First, Last: p.Last, Count: p.Count})
	}
	return out, nil
}
