package api

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/logstore"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/audit"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/modelreport"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type modelReportService struct{ *API }

const reportConfigKey = "model_report_config"

func (s *modelReportService) config(ctx context.Context) (modelreport.Config, error) {
	var cfg modelreport.Config
	v, err := s.Q.GetSetting(ctx, reportConfigKey)
	if errors.Is(err, sql.ErrNoRows) || err == nil && v == "" {
		return cfg, nil
	}
	if err != nil || s.Sealer == nil {
		return cfg, errors.New("config_unavailable")
	}
	b, err := base64.StdEncoding.DecodeString(v)
	if err == nil {
		b, err = s.Sealer.Open(b, []byte(reportConfigKey))
	}
	if err != nil || json.Unmarshal(b, &cfg) != nil {
		return modelreport.Config{}, errors.New("config_unavailable")
	}
	return cfg, nil
}

func reportConfigView(c modelreport.Config) *nagiv1.ReportConfig {
	return &nagiv1.ReportConfig{Enabled: c.Enabled, Endpoint: c.Endpoint, Model: c.Model, HasApiKey: c.APIKey != "", AllowPrivate: c.AllowPrivate}
}

func reportError(err error) error {
	// Only our stable error codes may reach the client, never database/provider errors.
	return connect.NewError(connect.CodeFailedPrecondition, err)
}

func (s *modelReportService) GetReportConfig(ctx context.Context, _ *connect.Request[nagiv1.GetReportConfigRequest]) (*connect.Response[nagiv1.ReportConfig], error) {
	c, err := s.config(ctx)
	if err != nil {
		return nil, reportError(err)
	}
	return connect.NewResponse(reportConfigView(c)), nil
}

func (s *modelReportService) UpdateReportConfig(ctx context.Context, req *connect.Request[nagiv1.UpdateReportConfigRequest]) (*connect.Response[nagiv1.ReportConfig], error) {
	s.reportConfigMu.Lock()
	defer s.reportConfigMu.Unlock()
	c, err := s.config(ctx)
	if err != nil && !req.Msg.ClearApiKey {
		return nil, reportError(err)
	}
	if req.Msg.ClearApiKey {
		c.APIKey = ""
	}
	if req.Msg.ApiKey != "" {
		c.APIKey = req.Msg.ApiKey
	}
	c.Enabled, c.Endpoint, c.Model, c.AllowPrivate = req.Msg.Enabled, strings.TrimSpace(req.Msg.Endpoint), strings.TrimSpace(req.Msg.Model), req.Msg.AllowPrivate
	if c.Enabled || c.Endpoint != "" || c.Model != "" || c.APIKey != "" {
		// Disabled configurations can remove the key. Enabling needs a real key.
		check := c
		if !c.Enabled && check.APIKey == "" {
			check.APIKey = "unused"
		}
		if err := check.Validate(); err != nil {
			return nil, reportError(err)
		}
	}
	b, _ := json.Marshal(c)
	if s.Sealer == nil {
		return nil, reportError(errors.New("config_unavailable"))
	}
	b, err = s.Sealer.Seal(b, []byte(reportConfigKey))
	if err != nil {
		return nil, reportError(errors.New("config_unavailable"))
	}
	if err := s.Q.SetSetting(ctx, store.SetSettingParams{Key: reportConfigKey, Value: base64.StdEncoding.EncodeToString(b)}); err != nil {
		return nil, reportError(errors.New("config_unavailable"))
	}
	return connect.NewResponse(reportConfigView(c)), nil
}

func (s *modelReportService) GenerateReport(ctx context.Context, req *connect.Request[nagiv1.GenerateReportRequest]) (*connect.Response[nagiv1.ModelReport], error) {
	c, err := s.config(ctx)
	if err != nil {
		return nil, reportError(err)
	}
	if !c.Enabled {
		return nil, reportError(errors.New("disabled"))
	}
	if err := c.Validate(); err != nil {
		return nil, reportError(err)
	}
	lang := req.Msg.Language
	if lang != "en" && lang != "zh" {
		lang = "zh"
	}
	in, err := s.reportInput(ctx, lang)
	if err != nil {
		return nil, reportError(errors.New("collection_failed"))
	}
	r, err := s.ModelReports.Generate(ctx, c, auth.CurrentUser(ctx).ID, in)
	if err != nil {
		return nil, reportError(err)
	}
	return connect.NewResponse(reportView(r)), nil
}

func (s *modelReportService) ListReports(ctx context.Context, _ *connect.Request[nagiv1.ListReportsRequest]) (*connect.Response[nagiv1.ListReportsResponse], error) {
	rs, err := s.ModelReports.List(ctx)
	if err != nil {
		return nil, reportError(errors.New("storage_unavailable"))
	}
	out := &nagiv1.ListReportsResponse{}
	for _, r := range rs {
		out.Reports = append(out.Reports, reportView(r))
	}
	return connect.NewResponse(out), nil
}

func (s *modelReportService) GetReport(ctx context.Context, req *connect.Request[nagiv1.GetReportRequest]) (*connect.Response[nagiv1.ModelReport], error) {
	rs, err := s.ModelReports.List(ctx)
	if err != nil {
		return nil, reportError(errors.New("storage_unavailable"))
	}
	for _, r := range rs {
		if r.ID == req.Msg.Id {
			return connect.NewResponse(reportView(r)), nil
		}
	}
	return nil, connect.NewError(connect.CodeNotFound, errors.New("report_not_found"))
}

func reportView(r *modelreport.Report) *nagiv1.ModelReport {
	out := &nagiv1.ModelReport{Id: r.ID, Status: r.Status, Started: timestamppb.New(r.Started), Model: r.Model,
		TemplateVersion: r.Template, MaterialJson: string(r.Material), Digest: r.Digest, Content: r.Content, Failure: r.Failure, Actor: r.Actor, Trigger: r.Trigger}
	if !r.Finished.IsZero() {
		out.Finished = timestamppb.New(r.Finished)
	}
	return out
}

// Rule keys often contain project names/domains. Emit only a known family.
func reportRule(key string) string {
	for _, family := range []string{"cpu", "mem", "disk", "offline", "cert", "restart", "errors", "health", "snapshot", "backup", "container_cpu", "traffic", "4xx", "egress"} {
		if key == family || strings.HasPrefix(key, family+":") {
			return family
		}
	}
	return "other"
}

func reportEnum(v string, allowed ...string) string {
	for _, a := range allowed {
		if v == a {
			return v
		}
	}
	return "unknown"
}

func (s *modelReportService) reportInput(ctx context.Context, lang string) (modelreport.Input, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	now := time.Now().UTC()
	in := modelreport.Input{From: now.Add(-30 * time.Minute), To: now, Language: lang,
		Missing: []string{"raw_logs_intentionally_excluded", "route_attack_signals_not_collected", "history_may_be_pruned_or_incomplete"}}
	aliases := map[string]string{}
	var nodeIDs []string
	alias := func(id string) string {
		if id == "" {
			return "panel"
		}
		if aliases[id] == "" {
			aliases[id] = fmt.Sprintf("N%d", len(aliases)+1)
			nodeIDs = append(nodeIDs, id)
		}
		return aliases[id]
	}
	add := func(id, key, status, validity string, at time.Time) {
		in.Alerts = append(in.Alerts, modelreport.Alert{Ref: fmt.Sprintf("A%d", len(in.Alerts)+1), Node: alias(id), Rule: reportRule(key), Status: reportEnum(status, "firing", "resolved", "ended"), Validity: reportEnum(validity, "valid", "stale", "unknown"), At: at})
	}
	if s.Nodes != nil {
		states, err := s.Nodes.AlertStates(ctx, "", true)
		if err != nil {
			return in, err
		}
		seen := map[string]bool{}
		for _, st := range states {
			a := st.GetAlert()
			if a == nil {
				continue
			}
			if a.GetStatus() != "firing" && a.GetTime().AsTime().Before(in.From) {
				continue
			}
			add(st.GetNodeId(), a.Key, a.Status, st.GetValidity(), a.GetTime().AsTime())
			seen[st.GetNodeId()+"\x00"+a.Key] = true
		}
		for _, a := range s.Nodes.ActiveAlerts() {
			if !seen[a.NodeID+"\x00"+a.Key] {
				add(a.NodeID, a.Key, "firing", "unknown", a.Since)
			}
		}
	} else {
		in.Missing = append(in.Missing, "node_alerts_unavailable")
	}
	if s.Logs != nil {
		events, err := s.Logs.Query(ctx, logstore.Filter{Category: logstore.Alert, From: in.From, To: in.To, Limit: 1000})
		if err != nil {
			return in, err
		}
		if len(events) == 1000 {
			in.Missing = append(in.Missing, "panel_alert_history_limit_reached")
		}
		for _, ev := range events {
			// Only panel-origin lifecycle summaries; never forward any log body.
			if ev.Kind != "panel.firing" && ev.Kind != "panel.resolved" {
				continue
			}
			b, _ := json.Marshal(ev.Data)
			var e audit.Event
			if json.Unmarshal(b, &e) != nil || e.ActorID != "panel" {
				continue
			}
			var p struct {
				Key string `json:"key"`
			}
			if json.Unmarshal([]byte(e.Params), &p) != nil {
				continue
			}
			add(e.TargetID, p.Key, e.Action, "unknown", ev.Time)
		}
	} else {
		in.Missing = append(in.Missing, "panel_alert_history_unavailable")
	}
	// Reject excess evidence rather than silently analyzing a favorable subset.
	if len(in.Alerts) > 60 || len(nodeIDs) > 8 {
		return in, errors.New("material_too_large")
	}
	for _, id := range nodeIDs {
		res, err := s.queryNodeMetrics(ctx, id, &nagiv1.QueryMetricsRequest{Prefix: "node:", From: timestamppb.New(in.From), To: timestamppb.New(in.To), Step: 60})
		if err != nil {
			in.Missing = append(in.Missing, aliases[id]+":metrics_unavailable")
			continue
		}
		before := len(in.Metrics)
		for _, series := range res.Series {
			switch series.Name {
			case "node:cpu", "node:mem", "node:disk", "node:net_rx", "node:net_tx":
			default:
				continue
			}
			m := modelreport.Metric{Ref: fmt.Sprintf("M%d", len(in.Metrics)+1), Node: aliases[id], Name: series.Name}
			for i, v := range series.Values {
				if i >= len(series.Times) {
					break
				}
				at := time.Unix(series.Times[i], 0).UTC()
				if at.Before(in.From) || at.After(in.To) || math.IsNaN(v) || math.IsInf(v, 0) {
					continue
				}
				if m.Count == 0 {
					m.Min, m.Max, m.First, m.Last = v, v, at, at
				}
				m.Min, m.Max = math.Min(m.Min, v), math.Max(m.Max, v)
				if at.Before(m.First) {
					m.First = at
				}
				if at.After(m.Last) {
					m.Last = at
				}
				m.Count++
				m.Mean += (v - m.Mean) / float64(m.Count)
			}
			if m.Count > 0 {
				in.Metrics = append(in.Metrics, m)
			}
		}
		if len(in.Metrics) == before {
			in.Missing = append(in.Missing, aliases[id]+":no_allowlisted_samples")
		}
	}
	return in, nil
}
