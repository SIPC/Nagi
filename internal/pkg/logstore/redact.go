package logstore

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
)

const hidden = "[已隐藏]"

// 支持引号、反斜杠转义和拼接的 shell 单词；只用于保守隐藏，不执行/解释命令。
const textArgument = `(?:"(?:\\.|[^"\\])*\\?(?:"|$)|'[^']*(?:'|$)|\\.|[^\s,;|&()"'<>` + "`" + `\\])+`

var (
	credentials     = regexp.MustCompile(`(?i)(password|passwd|secret|token|authorization|cookie|private[_-]?key|credential|key|command)(?:["']?\s*[:=]\s*|\s+)` + textArgument)
	shortCredential = regexp.MustCompile(`(^|[\s;|&(])(-[pkK])\s*=?\s*` + textArgument)
	userInfo        = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)?[^\s"'<>/@:]+:[^\s"'<>/]+@`)
	bearer          = regexp.MustCompile(`(?i)\b(Bearer|Basic)\s+[A-Za-z0-9+/_.=:-]+`)
	urls            = regexp.MustCompile(`https?://[^\s"'<>]+`)
	privateKey      = regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`)
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
	s = urls.ReplaceAllStringFunc(s, func(raw string) string {
		u, err := url.Parse(raw)
		if err != nil {
			return "[已隐藏的地址]"
		}
		u.User, u.RawQuery, u.Fragment = nil, "", ""
		// 程序日志也可能打印安全入口或部署 URL，不保留 URL 路径中的秘密。
		u.Path, u.RawPath = SafePanelPath(u.Path), ""
		return u.String()
	})
	s = userInfo.ReplaceAllString(s, "${1}"+hidden+"@")
	s = bearer.ReplaceAllString(s, hidden)
	s = credentials.ReplaceAllString(s, "$1="+hidden)
	return shortCredential.ReplaceAllString(s, "${1}${2}"+hidden)
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
	var trailing any
	if err := d.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("日志 JSON 含多余或不完整的内容")
	}
	return json.Marshal(redactValue("", value))
}

func redactValue(key string, value any) any {
	k := strings.ReplaceAll(strings.ToLower(key), "-", "_")
	for _, name := range []string{"password", "passwd", "secret", "authorization", "cookie", "private_key", "privatekey", "credential"} {
		if strings.Contains(k, name) {
			return hidden
		}
	}
	if (strings.Contains(k, "token") && !strings.HasSuffix(k, "_id")) || k == "key" || k == "api_key" || k == "apikey" || k == "command" || k == "detail" {
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
		// 审计 params、手工拼接的 error 等可以是套在 JSON 字符串里的 JSON。
		// 保持调用方的字符串类型，但先按字段递归隐藏，再回退文本模式。
		trimmed := strings.TrimSpace(x)
		if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
			if b, err := sanitizeJSON([]byte(x)); err == nil {
				return string(b)
			}
		}
		return scrubText(x)
	}
	return value
}
