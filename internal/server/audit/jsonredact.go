package audit

import (
	"encoding/json"
	"sort"
	"strings"
)

// MCP 工具的参数是 JSON，不是 protobuf 消息，按字段名脱敏（见 docs/audit.md#参数脱敏）：
//   - 密码、令牌、确认码这类字段替换为 Hidden；
//   - 环境变量、共享变量只记名字；
//   - compose 和文件内容只记 SHA-256 和长度。

var (
	jsonHidden  = []string{"password", "secret_value", "token", "confirm", "code"}
	jsonEnv     = map[string]bool{"env": true, "set": true, "variables": true, "service_env": true, "shared_variables": true}
	jsonContent = map[string]bool{"content": true, "compose": true, "files": true, "data": true}
)

// JSONParams 把工具参数（JSON）脱敏后返回。
func JSONParams(raw []byte) string {
	var v any
	if len(raw) == 0 || json.Unmarshal(raw, &v) != nil {
		return ""
	}
	b, _ := json.Marshal(redactJSON("", v))
	if string(b) == "{}" || string(b) == "null" {
		return ""
	}
	return TruncateParams(string(b))
}

func redactJSON(key string, v any) any {
	k := strings.ToLower(key)
	for _, h := range jsonHidden {
		if strings.Contains(k, h) {
			return Hidden
		}
	}
	if jsonContent[k] {
		b, _ := json.Marshal(v)
		if s, ok := v.(string); ok {
			b = []byte(s)
		}
		return digest(b)
	}
	if jsonEnv[k] {
		return envNames(v)
	}
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for kk, vv := range x {
			out[kk] = redactJSON(kk, vv)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, vv := range x {
			out[i] = redactJSON(key, vv)
		}
		return out
	}
	return v
}

// envNames 只留变量名：{"A": "1"} 和 ["A=1"] 都变成 ["A"]；按服务分的变量保留服务名。
func envNames(v any) any {
	switch x := v.(type) {
	case map[string]any:
		var names []string
		nested := map[string]any{}
		for k, vv := range x {
			if _, ok := vv.(map[string]any); ok {
				nested[k] = envNames(vv)
				continue
			}
			names = append(names, k)
		}
		sort.Strings(names)
		if len(nested) > 0 {
			if len(names) > 0 {
				nested["_"] = names
			}
			return nested
		}
		return names
	case []any:
		var names []string
		for _, vv := range x {
			if s, ok := vv.(string); ok {
				name, _, _ := strings.Cut(s, "=")
				names = append(names, name)
			}
		}
		return names
	}
	return Hidden
}
