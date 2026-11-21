package logstore

import (
	"bytes"
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
)

const hidden = "[已隐藏]"

var (
	credentials = regexp.MustCompile(`(?i)(password|passwd|secret|token|authorization|cookie|api[_-]?key)([\s"']*[:=][\s"']*|\s+)([^\s,;"'<>]+)`)
	bearer      = regexp.MustCompile(`(?i)\b(Bearer|Basic)\s+[A-Za-z0-9+/_.=:-]+`)
	urls        = regexp.MustCompile(`https?://[^\s"'<>]+`)
	privateKey  = regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`)
)

func clipText(s string, limit int) string {
	s = scrubText(s)
	if len(s) > limit {
		return strings.ToValidUTF8(s[:limit], "") + "…[已截断]"
	}
	return s
}

func scrubText(s string) string {
	s = privateKey.ReplaceAllString(s, hidden)
	s = bearer.ReplaceAllString(s, hidden)
	s = credentials.ReplaceAllString(s, "$1="+hidden)
	return urls.ReplaceAllStringFunc(s, func(raw string) string {
		u, err := url.Parse(raw)
		if err != nil {
			return "[已隐藏的地址]"
		}
		u.User, u.RawQuery, u.Fragment = nil, "", ""
		// 程序日志也可能打印安全入口或部署 URL，不保留 URL 路径中的秘密。
		u.Path, u.RawPath = SafePanelPath(u.Path), ""
		return u.String()
	})
}

// SafePanelPath 不记录查询串；未知/动态入口只留占位，避免安全入口、配对码和签名下载地址进库。
// 业务代理的路径另由调用方按规则脱敏，不套用面板接口白名单。
func SafePanelPath(path string) string {
	if strings.HasPrefix(path, "/nagi.v1.") {
		parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
		if len(parts) == 2 {
			return "/" + parts[0] + "/" + parts[1]
		}
	}
	if path == "/" || path == "/favicon.ico" || path == "/index.html" {
		return path
	}
	if strings.HasPrefix(path, "/assets/") {
		return "/assets/[文件]"
	}
	return "/[已隐藏的路径]"
}

func sanitizeJSON(b []byte) ([]byte, error) {
	var value any
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber() // 纳秒时间和 64 位字节计数不能经过 float64 丢精度。
	if err := d.Decode(&value); err != nil {
		return nil, err
	}
	return json.Marshal(redactValue("", value))
}

func redactValue(key string, value any) any {
	k := strings.ToLower(key)
	for _, name := range []string{"password", "passwd", "secret", "authorization", "cookie", "private_key", "credential"} {
		if strings.Contains(k, name) {
			return hidden
		}
	}
	if (strings.Contains(k, "token") && !strings.HasSuffix(k, "_id")) || k == "key" || k == "api_key" || k == "command" || k == "detail" {
		// 命令/节点旧审计的 detail 是自由文本，可能内嵌凭据；新库宁可隐藏，旧库暂保留兼容。
		return hidden
	}
	switch x := value.(type) {
	case map[string]any:
		for kk, vv := range x {
			x[kk] = redactValue(kk, vv)
		}
		return x
	case []any:
		for i, vv := range x {
			x[i] = redactValue(key, vv)
		}
		return x
	case string:
		return scrubText(x)
	}
	return value
}
