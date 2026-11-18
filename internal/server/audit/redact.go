package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

// 参数脱敏（见 docs/audit.md#参数脱敏）：
//   - 字段带 debug_redact = true 的替换为 Hidden；
//   - 环境变量（EnvVar）只记名字；
//   - compose、文件内容这类字段（ContentFields）只记 SHA-256 和长度；
//   - bytes 字段一律只记 SHA-256 和长度。
// 测试（api 包的 TestAuditRedaction）遍历所有请求消息，名字像密钥的字段既没标记、又不在这几类里的，测试失败。

// Hidden 是被隐藏的值。
const Hidden = "[已隐藏]"

// ContentFields 是内容字段：只记 SHA-256 和长度，内容本来就在版本记录或文件里。
var ContentFields = map[protoreflect.FullName]bool{
	"nagi.v1.ContainerConfig.compose":      true,
	"nagi.v1.WriteNodeFileRequest.content": true,
}

// SafeFields 是名字像密钥、其实不是的字段（例如 public_key、exit_code），值照常记录。
var SafeFields = map[protoreflect.FullName]string{
	"nagi.v1.OAuthRequest.code_challenge":        "PKCE 的挑战值是验证码的哈希，本来就公开传递",
	"nagi.v1.OAuthRequest.code_challenge_method": "PKCE 的算法名",
	"nagi.v1.EnvVar.key":                         "环境变量名，值不记",
	"nagi.v1.AppTokenRequest.key":                "用的是哪把密钥（device、confirm），不是密钥本身",
}

// envVar 是环境变量的消息名，值不记。
const envVar protoreflect.FullName = "nagi.v1.EnvVar"

// Params 把请求消息转成脱敏后的 JSON。
func Params(m proto.Message) string {
	return TruncateParams(ParamsRaw(m))
}

// ParamsRaw 和 Params 一样脱敏，但不截断：调用方合并别的内容、缩短过长的列表以后自己截断（见 api.withExtraParams）。
func ParamsRaw(m proto.Message) string {
	if m == nil {
		return ""
	}
	v := redactMessage(m.ProtoReflect(), 0)
	if obj, ok := v.(map[string]any); ok && len(obj) == 0 {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// Redacted 判断字段带 debug_redact = true。
func Redacted(fd protoreflect.FieldDescriptor) bool {
	opts, ok := fd.Options().(*descriptorpb.FieldOptions)
	return ok && opts.GetDebugRedact()
}

const maxDepth = 12

func redactMessage(m protoreflect.Message, depth int) any {
	if depth > maxDepth {
		return "…"
	}
	if m.Descriptor().FullName() == envVar {
		// 只记名字（以及是不是密钥），值不记。
		out := map[string]any{"key": m.Get(m.Descriptor().Fields().ByName("key")).String()}
		return out
	}
	out := map[string]any{}
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		out[string(fd.Name())] = redactField(fd, v, depth)
		return true
	})
	return out
}

func redactField(fd protoreflect.FieldDescriptor, v protoreflect.Value, depth int) any {
	if Redacted(fd) {
		return Hidden
	}
	if ContentFields[fd.FullName()] {
		return contentDigest(fd, v)
	}
	switch {
	case fd.IsList():
		l := v.List()
		items := make([]any, 0, l.Len())
		for i := range l.Len() {
			items = append(items, redactScalar(fd, l.Get(i), depth))
		}
		return items
	case fd.IsMap():
		obj := map[string]any{}
		v.Map().Range(func(k protoreflect.MapKey, mv protoreflect.Value) bool {
			obj[k.String()] = redactScalar(fd.MapValue(), mv, depth)
			return true
		})
		return obj
	}
	return redactScalar(fd, v, depth)
}

func redactScalar(fd protoreflect.FieldDescriptor, v protoreflect.Value, depth int) any {
	switch fd.Kind() {
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return redactMessage(v.Message(), depth+1)
	case protoreflect.BytesKind:
		return digest(v.Bytes())
	case protoreflect.StringKind:
		return v.String()
	case protoreflect.BoolKind:
		return v.Bool()
	case protoreflect.EnumKind:
		if ev := fd.Enum().Values().ByNumber(v.Enum()); ev != nil {
			return string(ev.Name())
		}
		return int32(v.Enum())
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return v.Int()
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return v.Uint()
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		return v.Int()
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return v.Uint()
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		return v.Float()
	}
	return nil
}

// contentDigest 是内容字段的摘要：字符串、bytes 记 SHA-256 和长度；map、列表、消息里的内容分别摘要。
func contentDigest(fd protoreflect.FieldDescriptor, v protoreflect.Value) any {
	switch {
	case fd.IsMap():
		obj := map[string]any{}
		v.Map().Range(func(k protoreflect.MapKey, mv protoreflect.Value) bool {
			obj[k.String()] = scalarDigest(fd.MapValue(), mv)
			return true
		})
		return obj
	case fd.IsList():
		l := v.List()
		items := make([]any, 0, l.Len())
		for i := range l.Len() {
			items = append(items, scalarDigest(fd, l.Get(i)))
		}
		return items
	}
	return scalarDigest(fd, v)
}

func scalarDigest(fd protoreflect.FieldDescriptor, v protoreflect.Value) any {
	switch fd.Kind() {
	case protoreflect.StringKind:
		return digest([]byte(v.String()))
	case protoreflect.BytesKind:
		return digest(v.Bytes())
	case protoreflect.MessageKind:
		// 消息里只记字段名和每个字段的摘要。
		m := v.Message()
		obj := map[string]any{}
		m.Range(func(f protoreflect.FieldDescriptor, fv protoreflect.Value) bool {
			obj[string(f.Name())] = contentDigest(f, fv)
			return true
		})
		return obj
	}
	return Hidden
}

func digest(b []byte) map[string]any {
	sum := sha256.Sum256(b)
	return map[string]any{"sha256": hex.EncodeToString(sum[:]), "bytes": len(b)}
}

// EnvChanges 比较两组环境变量，返回增加、修改、删除的名字（不含值）。
func EnvChanges(before, after map[string]string) map[string][]string {
	var added, changed, removed []string
	for k, v := range after {
		if old, ok := before[k]; !ok {
			added = append(added, k)
		} else if old != v {
			changed = append(changed, k)
		}
	}
	for k := range before {
		if _, ok := after[k]; !ok {
			removed = append(removed, k)
		}
	}
	sort.Strings(added)
	sort.Strings(changed)
	sort.Strings(removed)
	out := map[string][]string{}
	if len(added) > 0 {
		out["added"] = added
	}
	if len(changed) > 0 {
		out["changed"] = changed
	}
	if len(removed) > 0 {
		out["removed"] = removed
	}
	return out
}
