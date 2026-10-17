package nodes

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/SIPC/nagi/internal/pkg/alert"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// settingAlertConfig 是告警配置在 settings 表里的键。配置里有 Telegram 令牌，用主密钥加密后再存。
const settingAlertConfig = "alert_config"

var alertConfigContext = []byte("alert_config")

// AlertConfig 读取告警配置，没有配置时返回空配置。
func (r *Registry) AlertConfig(ctx context.Context) (*nagiv1.AlertConfig, error) {
	cfg := &nagiv1.AlertConfig{}
	v, err := r.q.GetSetting(ctx, settingAlertConfig)
	if err != nil || v == "" {
		return cfg, nil
	}
	sealed, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return nil, err
	}
	plain, err := r.sealer.Open(sealed, alertConfigContext)
	if err != nil {
		return nil, fmt.Errorf("解密告警配置: %w", err)
	}
	return cfg, proto.Unmarshal(plain, cfg)
}

// SetAlertConfig 保存告警配置，并下发给所有在线节点。
func (r *Registry) SetAlertConfig(ctx context.Context, cfg *nagiv1.AlertConfig) error {
	plain, err := proto.Marshal(cfg)
	if err != nil {
		return err
	}
	sealed, err := r.sealer.Seal(plain, alertConfigContext)
	if err != nil {
		return err
	}
	if err := r.q.SetSetting(ctx, store.SetSettingParams{
		Key: settingAlertConfig, Value: base64.StdEncoding.EncodeToString(sealed),
	}); err != nil {
		return err
	}
	r.mu.RLock()
	conns := make([]*Conn, 0, len(r.conns))
	for _, c := range r.conns {
		conns = append(conns, c)
	}
	r.mu.RUnlock()
	for _, c := range conns {
		go r.pushAlertConfig(context.Background(), c, cfg)
	}
	return nil
}

// pushAlertConfig 把告警配置下发给一个节点。cfg 为 nil 时读取当前配置。
func (r *Registry) pushAlertConfig(ctx context.Context, c *Conn, cfg *nagiv1.AlertConfig) {
	if cfg == nil {
		var err error
		if cfg, err = r.AlertConfig(ctx); err != nil {
			r.log.Warn("读取告警配置失败", "err", err)
			return
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if _, err := c.Agent.SetAlertConfig(ctx, &nagiv1.SetAlertConfigRequest{Config: cfg}); err != nil {
		r.log.Warn("下发告警配置失败", "node", c.NodeID, "err", err)
	}
}

// SendAlert 把告警发到面板配置的渠道。channelIDs 为空时发到所有渠道。
func (r *Registry) SendAlert(ctx context.Context, a *nagiv1.Alert, channelIDs []string) error {
	cfg, err := r.AlertConfig(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, ch := range cfg.GetChannels() {
		if len(channelIDs) > 0 && !slices.Contains(channelIDs, ch.GetId()) {
			continue
		}
		if err := alert.Send(ctx, ch, a); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", ch.GetName(), err))
		}
	}
	return errors.Join(errs...)
}

// RelayAlert 替节点发送它直接发不出去的告警。
func (p *panelService) RelayAlert(ctx context.Context, req *nagiv1.RelayAlertRequest) (*nagiv1.RelayAlertResponse, error) {
	a := req.GetAlert()
	if node, err := p.r.q.GetNode(ctx, p.conn.NodeID); err == nil && node.Name != "" {
		a.Node = node.Name
	}
	if err := p.r.SendAlert(ctx, a, req.GetChannelIds()); err != nil {
		return nil, err
	}
	return &nagiv1.RelayAlertResponse{}, nil
}

// RunOfflineAlerts 检查节点失联：节点离线超过阈值时发告警，重新连上时发恢复通知。
// 失联只能由面板判断，节点自己发不出来。
func (r *Registry) RunOfflineAlerts(ctx context.Context) {
	start := time.Now()
	type offline struct {
		notified time.Time
		name     string
	}
	firing := map[string]*offline{}
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		cfg, err := r.AlertConfig(ctx)
		if err != nil || len(cfg.GetChannels()) == 0 {
			continue
		}
		rules := alert.EffectiveRules(cfg.GetRules())
		list, err := r.q.ListNodes(ctx)
		if err != nil {
			continue
		}
		now := time.Now()
		seen := map[string]bool{}
		for _, n := range list {
			seen[n.ID] = true
			name := n.Name
			if name == "" {
				name = n.ID
			}
			st := firing[n.ID]
			if r.Online(n.ID) {
				if st != nil {
					delete(firing, n.ID)
					r.sendLogged(ctx, &nagiv1.Alert{Key: "offline", Status: "resolved", Title: "节点已重新连上面板",
						Node: name, Time: timestamppb.New(now)})
				}
				continue
			}
			// 面板刚启动时节点需要时间重连，离线时间从面板启动开始算。
			since := time.Unix(n.LastSeen, 0)
			if since.Before(start) {
				since = start
			}
			down := now.Sub(since)
			if down < time.Duration(rules.OfflineSeconds)*time.Second {
				continue
			}
			if st != nil && now.Sub(st.notified) < alert.RepeatInterval {
				continue
			}
			firing[n.ID] = &offline{notified: now, name: name}
			r.sendLogged(ctx, &nagiv1.Alert{Key: "offline", Status: "firing",
				Title:   fmt.Sprintf("节点失联超过 %d 秒", int(down.Seconds())),
				Message: "最后在线：" + time.Unix(n.LastSeen, 0).Local().Format("2006-01-02 15:04:05"),
				Node:    name, Time: timestamppb.New(now)})
		}
		// 已被移除的节点不再跟踪。
		for id := range firing {
			if !seen[id] {
				delete(firing, id)
			}
		}
	}
}

func (r *Registry) sendLogged(ctx context.Context, a *nagiv1.Alert) {
	r.log.Info("告警", "status", a.GetStatus(), "title", a.GetTitle(), "node", a.GetNode())
	if err := r.SendAlert(ctx, a, nil); err != nil {
		r.log.Warn("发送告警失败", "err", err)
	}
}
