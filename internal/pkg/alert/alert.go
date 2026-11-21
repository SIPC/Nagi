// Package alert 把告警发送到渠道（webhook、Telegram），面板和节点共用。
package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/SIPC/nagi/internal/pkg/i18n"
	"github.com/SIPC/nagi/internal/pkg/safehttp"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// RepeatInterval：补发队列里的告警超过这么久就不再补发（已经过时）。规则的重复提醒间隔在规则里设置（见 policy.go）。
const RepeatInterval = 4 * time.Hour

// ErrorWindowDefault：http 映射的默认统计窗口；请求量突增的基线不含最近这么久。
const ErrorWindowDefault = 5 * time.Minute

func or(v, def uint32) uint32 {
	if v == 0 {
		return def
	}
	return v
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

// Validate 检查渠道配置。webhook 地址直接写 IP 的，在这里按地址策略检查（见 safehttp.Check）；
// 写域名的发送时才知道解析到哪里，发送时检查。
func Validate(c *nagiv1.AlertChannel) error {
	switch c.GetType() {
	case "webhook":
		u, err := url.Parse(c.GetUrl())
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Hostname() == "" {
			return i18n.New("webhook 地址须以 http:// 或 https:// 开头")
		}
		host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
		if host == "localhost" || strings.HasSuffix(host, ".localhost") {
			return blockedError(u.Host, &safehttp.BlockedError{})
		}
		if ip, err := netip.ParseAddr(u.Hostname()); err == nil {
			if err := safehttp.Check(ip, c.GetAllowPrivate()); err != nil {
				b, _ := safehttp.Blocked(err)
				return blockedError(u.Host, b)
			}
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

// webhook 的地址是管理员填的，面板和每个节点都会去连接，所以只连接允许的地址（见 safehttp）：
// 默认只连公网，渠道勾了「允许内网地址」时也连内网；不用代理，不跟随跳转。
var (
	publicClient  = safehttp.Client(sendTimeout, false)
	privateClient = safehttp.Client(sendTimeout, true)
	// webhookClient 在测试里替换。
	webhookClient = func(allowPrivate bool) *http.Client {
		if allowPrivate {
			return privateClient
		}
		return publicClient
	}
	// telegramClient 只连 api.telegram.org，地址是固定的，照常用环境变量里的代理（有的节点要经代理才能访问 Telegram）。
	telegramClient = &http.Client{Timeout: sendTimeout, CheckRedirect: safehttp.NoRedirect}
	// telegramAPI 在测试里替换。
	telegramAPI = "https://api.telegram.org"
)

const sendTimeout = 15 * time.Second

// AllowLoopbackForTest 让其他包的测试把 webhook 发到本机的 httptest 服务（正式的客户端连不上本机），
// 测试结束时恢复。只给测试用：参数是 *testing.T。
func AllowLoopbackForTest(t interface{ Cleanup(func()) }) {
	old := webhookClient
	t.Cleanup(func() { webhookClient = old })
	webhookClient = func(bool) *http.Client {
		return &http.Client{Timeout: sendTimeout, CheckRedirect: safehttp.NoRedirect}
	}
}

// blockedError 是 webhook 地址因为地址策略不能使用的说明。host 不带地址里的账号密码。
func blockedError(host string, b *safehttp.BlockedError) error {
	if b != nil && b.Private {
		return i18n.Errorf("webhook 地址 %s 指向内网。要发到内网，请在渠道上勾选「允许内网地址」", host)
	}
	return i18n.Errorf("webhook 地址 %s 指向本机、链路本地或云服务器的元数据地址，不能使用", host)
}

// Text 返回告警的纯文本内容（见 docs/monitoring.md#消息格式）。
func Text(a *nagiv1.Alert) string {
	en := isEn(a)
	pick := func(zh, e string) string {
		if en {
			return e
		}
		return zh
	}
	title, detail := Render(a)
	prefix := "🔴 "
	switch {
	case a.GetStatus() == "resolved":
		prefix = pick("✅ 已恢复：", "✅ Resolved: ")
		if a.GetRule() == RuleOffline || a.GetRule() == RuleHealth { // 文字本身已说明恢复
			prefix = "✅ "
		}
	case a.GetStatus() == "ended":
		prefix = pick("⚪ 已结束：", "⚪ Ended: ")
	case a.GetStatus() == "flapping":
		prefix = "🟡 "
	case a.GetStatus() == "info":
		prefix = "ℹ️ "
	case a.GetReminder():
		prefix = pick("🔁 仍未恢复：", "🔁 Still firing: ")
	}
	var b strings.Builder
	b.WriteString(prefix + title)
	if detail != "" {
		b.WriteString("\n" + detail)
	}
	if a.GetNode() != "" {
		b.WriteString(pick("\n节点：", "\nNode: ") + a.GetNode())
	}
	if a.GetProject() != "" {
		b.WriteString(pick("\n项目：", "\nProject: ") + a.GetProject())
	}
	if a.GetTime() != nil {
		b.WriteString(pick("\n时间：", "\nTime: ") + a.GetTime().AsTime().Local().Format("2006-01-02 15:04:05"))
	}
	return b.String()
}

// Send 把一条告警发到一个渠道。
func Send(ctx context.Context, c *nagiv1.AlertChannel, a *nagiv1.Alert) error {
	switch c.GetType() {
	case "webhook":
		title, message := Render(a)
		m := map[string]any{
			"status":  a.GetStatus(),
			"key":     a.GetKey(),
			"title":   title,
			"message": message,
			"node":    a.GetNode(),
			"project": a.GetProject(),
			"time":    a.GetTime().AsTime().Format(time.RFC3339),
			"text":    Text(a),
		}
		// 规则告警的结构化内容（旧版本节点、事件类告警没有）。
		if a.GetRule() != "" {
			m["rule"], m["duration_seconds"], m["reminder"] = a.GetRule(), a.GetDurationSeconds(), a.GetReminder()
			for k, v := range map[string]*float64{"value": a.Value, "threshold": a.Threshold, "recover_threshold": a.RecoverThreshold, "peak": a.Peak} {
				if v != nil {
					m[k] = *v
				}
			}
			if a.GetSubject() != "" {
				m["subject"] = a.GetSubject()
			}
		}
		if a.GetEndReason() != "" {
			m["end_reason"] = a.GetEndReason()
		}
		body, _ := json.Marshal(m)
		host := "?"
		if u, err := url.Parse(c.GetUrl()); err == nil {
			host = u.Host
		}
		err := post(ctx, webhookClient(c.GetAllowPrivate()), c.GetUrl(), body, i18n.Errorf("无法连接 webhook 地址 %s，请检查地址和面板的网络", host))
		if b, ok := safehttp.Blocked(err); ok {
			return blockedError(host, b)
		}
		return err
	case "telegram":
		body, _ := json.Marshal(map[string]any{"chat_id": c.GetTelegramChatId(), "text": Text(a)})
		err := post(ctx, telegramClient, telegramAPI+"/bot"+c.GetTelegramToken()+"/sendMessage", body,
			i18n.New("无法连接 Telegram，请检查面板能否访问 api.telegram.org"))
		var st statusError
		if errors.As(err, &st) {
			switch st {
			case http.StatusUnauthorized, http.StatusNotFound:
				return i18n.New("Telegram 机器人令牌无效")
			case http.StatusBadRequest, http.StatusForbidden:
				return i18n.Errorf("Telegram 拒绝了这条消息（HTTP %d），请检查聊天 ID，并确认机器人在这个聊天里", int(st))
			}
		}
		return err
	}
	return i18n.New("渠道类型只能是 webhook 或 telegram")
}

// statusError 是渠道返回的非 2xx 状态码。
type statusError int

func (e statusError) Error() string { return e.Localize(i18n.Zh) }

func (e statusError) Localize(l i18n.Lang) string {
	return i18n.Sprintf(l, "渠道返回 HTTP %d", int(e))
}

// post 发送一条告警。
//   - 连不上时返回 unreachable：Go 的错误里带着完整地址（Telegram 的地址里有机器人令牌），不能显示给浏览器，
//     也不能写进日志和活动记录。地址策略不允许时返回 *safehttp.BlockedError，由调用方换成说明。
//   - 非 2xx（包括跳转，不跟随）只报状态码，不带响应内容：地址是别人填的，响应内容可能是内网服务的数据。
func post(ctx context.Context, client *http.Client, url string, body []byte, unreachable error) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return unreachable
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		if b, ok := safehttp.Blocked(err); ok {
			return b
		}
		return unreachable
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 != 2 {
		return statusError(resp.StatusCode)
	}
	return nil
}
