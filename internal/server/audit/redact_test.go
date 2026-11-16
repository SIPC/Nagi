package audit

import (
	"sort"
	"strings"
	"testing"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"
)

// sensitiveWords 是像密钥的字段名：带这些词的字符串、bytes 字段必须标 debug_redact、登记为内容字段或安全字段。
var sensitiveWords = []string{"password", "token", "secret", "key", "code", "env", "value", "content", "data"}

func looksSensitive(name string) bool {
	for _, w := range sensitiveWords {
		if strings.Contains(name, w) {
			return true
		}
	}
	return false
}

// requestMessages 返回浏览器接口（api.proto 的各个服务）的所有请求消息。
func requestMessages(t *testing.T) []protoreflect.MessageDescriptor {
	t.Helper()
	fd, err := protoregistry.GlobalFiles.FindFileByPath("nagi/v1/api.proto")
	if err != nil {
		t.Fatal(err)
	}
	_ = nagiv1.File_nagi_v1_api_proto
	seen := map[protoreflect.FullName]bool{}
	var out []protoreflect.MessageDescriptor
	for i := range fd.Services().Len() {
		svc := fd.Services().Get(i)
		for j := range svc.Methods().Len() {
			in := svc.Methods().Get(j).Input()
			if !seen[in.FullName()] {
				seen[in.FullName()] = true
				out = append(out, in)
			}
		}
	}
	return out
}

// walk 遍历消息里的所有字段（包括嵌套的消息），每个字段只访问一次。
func walk(md protoreflect.MessageDescriptor, seen map[protoreflect.FullName]bool, fn func(protoreflect.FieldDescriptor) bool) {
	if seen[md.FullName()] || md.FullName() == envVar {
		return
	}
	seen[md.FullName()] = true
	for i := range md.Fields().Len() {
		f := md.Fields().Get(i)
		if !fn(f) {
			continue
		}
		if f.IsMap() {
			if m := f.MapValue().Message(); m != nil {
				walk(m, seen, fn)
			}
		} else if m := f.Message(); m != nil {
			walk(m, seen, fn)
		}
	}
}

func TestAuditRedactionCoversSensitiveFields(t *testing.T) {
	var missing []string
	seen := map[protoreflect.FullName]bool{}
	for _, md := range requestMessages(t) {
		walk(md, seen, func(f protoreflect.FieldDescriptor) bool {
			if Redacted(f) || ContentFields[f.FullName()] {
				return false // 整个字段都不记，不用再看里面
			}
			if _, ok := SafeFields[f.FullName()]; ok {
				return true
			}
			kind := f.Kind()
			if f.IsMap() {
				kind = f.MapValue().Kind()
			}
			if kind != protoreflect.StringKind {
				return true // bytes 只记摘要，数字、布尔不是密钥
			}
			if looksSensitive(string(f.Name())) {
				missing = append(missing, string(f.FullName()))
			}
			return true
		})
	}
	sort.Strings(missing)
	for _, m := range missing {
		t.Errorf("字段 %s 像密钥：标记 debug_redact，或登记到 audit.ContentFields / audit.SafeFields", m)
	}
}

// fill 把消息里所有字符串和 bytes 字段（包括嵌套的、列表和 map 里的）填上 secret。
func fill(m protoreflect.Message, secret string, depth int) {
	if depth > 6 {
		return
	}
	md := m.Descriptor()
	for i := range md.Fields().Len() {
		f := md.Fields().Get(i)
		if f.ContainingOneof() != nil && m.WhichOneof(f.ContainingOneof()) != nil {
			continue
		}
		switch {
		case f.IsMap():
			mp := m.Mutable(f).Map()
			k := protoreflect.ValueOfString(secret).MapKey()
			if f.MapKey().Kind() != protoreflect.StringKind {
				continue
			}
			switch f.MapValue().Kind() {
			case protoreflect.StringKind:
				mp.Set(k, protoreflect.ValueOfString(secret))
			case protoreflect.MessageKind:
				v := mp.NewValue()
				fill(v.Message(), secret, depth+1)
				mp.Set(k, v)
			}
		case f.IsList():
			l := m.Mutable(f).List()
			switch f.Kind() {
			case protoreflect.StringKind:
				l.Append(protoreflect.ValueOfString(secret))
			case protoreflect.BytesKind:
				l.Append(protoreflect.ValueOfBytes([]byte(secret)))
			case protoreflect.MessageKind:
				v := l.NewElement()
				fill(v.Message(), secret, depth+1)
				l.Append(v)
			}
		case f.Kind() == protoreflect.StringKind:
			m.Set(f, protoreflect.ValueOfString(secret))
		case f.Kind() == protoreflect.BytesKind:
			m.Set(f, protoreflect.ValueOfBytes([]byte(secret)))
		case f.Kind() == protoreflect.MessageKind:
			fill(m.Mutable(f).Message(), secret, depth+1)
		}
	}
}

// 所有请求里像密钥的字段填上同一个值，记下来的参数里不能出现这个值。字段名本身（map 的键、环境变量名）照常记录，
// 所以只在值上放密钥：先整体填一个普通值，再把像密钥的字段换成密钥。
func TestAuditParamsHideSecrets(t *testing.T) {
	const plain, secret = "plain-value", "S3CRET-c0ffee"
	for _, md := range requestMessages(t) {
		m := dynamicpb.NewMessage(md)
		fill(m, plain, 0)
		setSecrets(m, secret, 0)
		// 转成生成的类型，和运行时一样。
		typ, err := protoregistry.GlobalTypes.FindMessageByName(md.FullName())
		if err != nil {
			t.Fatal(err)
		}
		real := typ.New().Interface()
		b, _ := proto.Marshal(m)
		if err := proto.Unmarshal(b, real); err != nil {
			t.Fatal(err)
		}
		if p := Params(real); strings.Contains(p, secret) {
			t.Errorf("%s 的参数里出现了密钥：%s", md.FullName(), p)
		}
	}
}

// setSecrets 把像密钥的字符串字段和环境变量的值换成 secret。
func setSecrets(m protoreflect.Message, secret string, depth int) {
	if depth > 6 {
		return
	}
	md := m.Descriptor()
	for i := range md.Fields().Len() {
		f := md.Fields().Get(i)
		if !m.Has(f) {
			continue
		}
		_, safe := SafeFields[f.FullName()]
		sensitive := !safe && (looksSensitive(string(f.Name())) || md.FullName() == envVar && f.Name() == "value")
		switch {
		case f.IsMap():
			mp := m.Mutable(f).Map()
			mp.Range(func(k protoreflect.MapKey, v protoreflect.Value) bool {
				if f.MapValue().Kind() == protoreflect.StringKind && sensitive {
					mp.Set(k, protoreflect.ValueOfString(secret))
				} else if f.MapValue().Kind() == protoreflect.MessageKind {
					setSecrets(v.Message(), secret, depth+1)
				}
				return true
			})
		case f.IsList():
			l := m.Mutable(f).List()
			for j := range l.Len() {
				switch f.Kind() {
				case protoreflect.StringKind:
					if sensitive {
						l.Set(j, protoreflect.ValueOfString(secret))
					}
				case protoreflect.BytesKind:
					l.Set(j, protoreflect.ValueOfBytes([]byte(secret)))
				case protoreflect.MessageKind:
					setSecrets(l.Get(j).Message(), secret, depth+1)
				}
			}
		case f.Kind() == protoreflect.StringKind:
			if sensitive {
				m.Set(f, protoreflect.ValueOfString(secret))
			}
		case f.Kind() == protoreflect.BytesKind:
			m.Set(f, protoreflect.ValueOfBytes([]byte(secret)))
		case f.Kind() == protoreflect.MessageKind:
			setSecrets(m.Mutable(f).Message(), secret, depth+1)
		}
	}
}

func TestEnvChanges(t *testing.T) {
	got := EnvChanges(map[string]string{"A": "1", "B": "2", "C": "3"}, map[string]string{"A": "1", "B": "x", "D": "4"})
	if strings.Join(got["added"], ",") != "D" || strings.Join(got["changed"], ",") != "B" || strings.Join(got["removed"], ",") != "C" {
		t.Fatalf("got %v", got)
	}
}
