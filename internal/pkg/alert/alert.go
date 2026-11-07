// Package alert 把告警发送到渠道（webhook、Telegram），面板和节点共用。
package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/SIPC/nagi/internal/pkg/i18n"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// 规则的默认阈值。
const (
	DefaultDiskPercent    = 90
	DefaultMemPercent     = 90
	DefaultMemMinutes     = 5
	DefaultRestartCount   = 3
	DefaultRestartMinutes = 10
	DefaultCertDays       = 7
	DefaultOfflineSeconds = 60
	DefaultErrorPercent   = 5
	// 5xx 比例按最近 5 分钟计算，请求数太少时不判断。
	ErrorWindow      = 5 * time.Minute
	ErrorMinRequests = 20
	// RepeatInterval：问题一直没恢复时，每隔这么久再提醒一次。
	RepeatInterval = 4 * time.Hour
)

func or(v, def uint32) uint32 {
	if v == 0 {
		return def
	}
	return v
}

// Rules 是填好默认值的规则。
type Rules struct {
	DiskPercent    float64
	MemPercent     float64
	MemFor         time.Duration
	RestartCount   int
	RestartWindow  time.Duration
	CertDays       int
	OfflineSeconds int
	ErrorPercent   float64
}

// EffectiveRules 把 0 替换成默认值。
func EffectiveRules(r *nagiv1.AlertRules) Rules {
	return Rules{
		DiskPercent:    float64(or(r.GetDiskPercent(), DefaultDiskPercent)),
		MemPercent:     float64(or(r.GetMemPercent(), DefaultMemPercent)),
		MemFor:         time.Duration(or(r.GetMemMinutes(), DefaultMemMinutes)) * time.Minute,
		RestartCount:   int(or(r.GetRestartCount(), DefaultRestartCount)),
		RestartWindow:  time.Duration(or(r.GetRestartMinutes(), DefaultRestartMinutes)) * time.Minute,
		CertDays:       int(or(r.GetCertDays(), DefaultCertDays)),
		OfflineSeconds: int(or(r.GetOfflineSeconds(), DefaultOfflineSeconds)),
		ErrorPercent:   float64(or(r.GetErrorPercent(), DefaultErrorPercent)),
	}
}

// ChannelKey 用来判断两个渠道是不是同一个目的地（多个面板配置了同一个渠道时只发一次）。
func ChannelKey(c *nagiv1.AlertChannel) string {
	switch c.GetType() {
	case "telegram":
		return "telegram:" + c.GetTelegramToken() + ":" + c.GetTelegramChatId()
	default:
		return "webhook:" + c.GetUrl()
	}
}

// Validate 检查渠道配置。
func Validate(c *nagiv1.AlertChannel) error {
	switch c.GetType() {
	case "webhook":
		u, err := url.Parse(c.GetUrl())
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return i18n.New("webhook 地址须以 http:// 或 https:// 开头")
		}
	case "telegram":
		if strings.TrimSpace(c.GetTelegramToken()) == "" || strings.TrimSpace(c.GetTelegramChatId()) == "" {
			return i18n.New("请填写 Telegram 机器人令牌和聊天 ID")
		}
	default:
		return i18n.New("渠道类型只能是 webhook 或 telegram")
	}
	return nil
}

var httpClient = &http.Client{Timeout: 15 * time.Second}

// Text 返回告警的纯文本内容。
func Text(a *nagiv1.Alert) string {
	prefix := "🔴 "
	switch a.GetStatus() {
	case "resolved":
		prefix = "✅ 已恢复："
	case "info":
		prefix = "ℹ️ "
	}
	var b strings.Builder
	b.WriteString(prefix + a.GetTitle())
	if a.GetMessage() != "" {
		b.WriteString("\n" + a.GetMessage())
	}
	if a.GetNode() != "" {
		b.WriteString("\n节点：" + a.GetNode())
	}
	if a.GetProject() != "" {
		b.WriteString("\n项目：" + a.GetProject())
	}
	if a.GetTime() != nil {
		b.WriteString("\n时间：" + a.GetTime().AsTime().Local().Format("2006-01-02 15:04:05"))
	}
	return b.String()
}

// Send 把一条告警发到一个渠道。
func Send(ctx context.Context, c *nagiv1.AlertChannel, a *nagiv1.Alert) error {
	switch c.GetType() {
	case "webhook":
		body, _ := json.Marshal(map[string]any{
			"status":  a.GetStatus(),
			"key":     a.GetKey(),
			"title":   a.GetTitle(),
			"message": a.GetMessage(),
			"node":    a.GetNode(),
			"project": a.GetProject(),
			"time":    a.GetTime().AsTime().Format(time.RFC3339),
			"text":    Text(a),
		})
		host := c.GetUrl()
		if u, err := url.Parse(c.GetUrl()); err == nil {
			host = u.Host
		}
		return post(ctx, c.GetUrl(), body, i18n.Errorf("无法连接 webhook 地址 %s，请检查地址和面板的网络", host))
	case "telegram":
		body, _ := json.Marshal(map[string]any{"chat_id": c.GetTelegramChatId(), "text": Text(a)})
		return post(ctx, "https://api.telegram.org/bot"+c.GetTelegramToken()+"/sendMessage", body,
			i18n.New("无法连接 Telegram，请检查面板能否访问 api.telegram.org"))
	}
	return i18n.New("渠道类型只能是 webhook 或 telegram")
}

// post 发送一条告警。连不上时返回 unreachable：Go 的错误里带着完整地址（Telegram 的地址里有机器人令牌），
// 不能显示给浏览器，也不能写进日志和活动记录。
func post(ctx context.Context, url string, body []byte, unreachable error) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return unreachable
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return unreachable
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return i18n.Errorf("渠道返回 HTTP %d：%s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}
