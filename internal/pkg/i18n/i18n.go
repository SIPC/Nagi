// Package i18n 让面板返回给浏览器的文案按请求的语言显示（中文、英文）。
//
// 中文是源语言：代码里照常写中文的格式串，英文译文登记在 en.go 里，以中文格式串为键。
// 错误用 Errorf / New 创建，保留格式串和参数，到接口边界再用 Localize 按请求的语言拼出文字；
// Error() 始终是中文，日志不受影响。没有译文的格式串原样显示中文。
package i18n

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Lang 是界面语言，取值和前端一致。
type Lang string

const (
	Zh Lang = "zh-CN"
	En Lang = "en"
)

// catalogs 是各语言的译文：中文格式串 → 译文格式串。译文里参数的顺序可以用 %[2]s 这样的写法调整。
var catalogs = map[Lang]map[string]string{En: en}

// Match 把一个语言标签（"zh-CN"、"en-US"……）对应到支持的语言。
func Match(tag string) (Lang, bool) {
	base, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(tag)), "-")
	base, _, _ = strings.Cut(base, "_")
	switch base {
	case "zh":
		return Zh, true
	case "en":
		return En, true
	}
	return "", false
}

// Parse 从 Accept-Language 里按 q 值从高到低选出第一个支持的语言；都不支持或为空时是中文。
func Parse(header string) Lang {
	type candidate struct {
		tag string
		q   float64
	}
	var cs []candidate
	for _, part := range strings.Split(header, ",") {
		tag, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		q := 1.0
		if v, ok := strings.CutPrefix(strings.TrimSpace(params), "q="); ok {
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				q = f
			}
		}
		if tag != "" && q > 0 {
			cs = append(cs, candidate{tag, q})
		}
	}
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].q > cs[j].q })
	for _, c := range cs {
		if l, ok := Match(c.tag); ok {
			return l
		}
	}
	return Zh
}

type ctxKey struct{}

// WithLang 把请求的语言放进 ctx。
func WithLang(ctx context.Context, l Lang) context.Context {
	return context.WithValue(ctx, ctxKey{}, l)
}

// FromContext 返回 ctx 里的语言，没有时是中文。
func FromContext(ctx context.Context) Lang {
	if l, ok := ctx.Value(ctxKey{}).(Lang); ok {
		return l
	}
	return Zh
}

func translate(l Lang, format string) string {
	if s, ok := catalogs[l][format]; ok {
		return s
	}
	return format
}

// List 是一组名字，作为 Sprintf、T、Errorf 的参数时按语言连起来（中文用顿号，英文用逗号）。
type List []string

func (s List) String() string { return strings.Join(s, "、") }

// Localize 按语言连起来。
func (s List) Localize(l Lang) string { return strings.Join(s, translate(l, "、")) }

// Sprintf 按语言格式化：format 换成译文，参数里的错误（和 List 这样有 Localize 方法的值）也按语言拼出文字。
// format 里可以有 %w。
func Sprintf(l Lang, format string, args ...any) string {
	out := make([]any, len(args))
	for i, a := range args {
		if e, ok := a.(error); ok {
			out[i] = Localize(l, e)
		} else if lz, ok := a.(interface{ Localize(Lang) string }); ok {
			out[i] = lz.Localize(l)
		} else {
			out[i] = a
		}
	}
	return fmt.Sprintf(strings.ReplaceAll(translate(l, format), "%w", "%v"), out...)
}

// T 按 ctx 里的语言格式化，用于直接返回给浏览器的文字（例如部署过程中输出的说明）。
func T(ctx context.Context, format string, args ...any) string {
	return Sprintf(FromContext(ctx), format, args...)
}

// Error 是可以按语言显示的错误：保留格式串和参数，Error() 是中文。
type Error struct {
	format  string
	args    []any
	literal bool  // New 创建的：format 是整段文字，不按格式串解释
	std     error // 中文的错误，%w 包着的错误从这里取
}

// Errorf 和 fmt.Errorf 一样，包括用 %w 包装错误（errors.Is / errors.As 照常可用）。
func Errorf(format string, args ...any) error {
	return &Error{format: format, args: args, std: fmt.Errorf(format, args...)}
}

// New 和 errors.New 一样。
func New(text string) error {
	return &Error{format: text, literal: true, std: errors.New(text)}
}

func (e *Error) Error() string { return e.std.Error() }

func (e *Error) Unwrap() []error {
	switch u := e.std.(type) {
	case interface{ Unwrap() error }:
		if w := u.Unwrap(); w != nil {
			return []error{w}
		}
	case interface{ Unwrap() []error }:
		return u.Unwrap()
	}
	return nil
}

// Detail 返回给人看的错误 err，底层原因 cause 只留给日志：Error() 在后面附上 cause 的原始文字，
// 按语言显示时只有 err。errors.Is / errors.As 两个都能找到。
func Detail(err, cause error) error {
	if cause == nil {
		return err
	}
	return detailed{err, cause}
}

type detailed struct{ err, cause error }

func (d detailed) Error() string          { return d.err.Error() + "（" + d.cause.Error() + "）" }
func (d detailed) Localize(l Lang) string { return Localize(l, d.err) }
func (d detailed) Unwrap() []error        { return []error{d.err, d.cause} }

// Localize 按语言拼出错误的文字。
func (e *Error) Localize(l Lang) string {
	if e.literal {
		return translate(l, e.format)
	}
	return Sprintf(l, e.format, e.args...)
}

// Localize 按语言拼出 err 的文字：err 本身，或者它包着的、文字完全相同的错误（只是加了类型的包装，
// 例如 spec 的参数错误）是 *Error 时用它的译文；否则整条文字有译文时用译文；都没有时就是 err.Error()。
func Localize(l Lang, err error) string {
	if err == nil {
		return ""
	}
	for e := err; e != nil; {
		if le, ok := e.(interface{ Localize(Lang) string }); ok {
			return le.Localize(l)
		}
		u, ok := e.(interface{ Unwrap() error })
		if !ok {
			break
		}
		next := u.Unwrap()
		if next == nil || next.Error() != e.Error() {
			break
		}
		e = next
	}
	return Text(l, err.Error())
}

// Text 按语言翻译一段已经拼好的文字，用于节点返回的消息这类只有文字、没有格式串的场合：
// 整条文字有译文时用译文；否则按格式串反推参数（例如「节点 n2 离线」对上「节点 %s 离线」），
// 参数本身也再翻译一次；都对不上时原样返回。
func Text(l Lang, s string) string {
	if out, ok := text(l, s, 0); ok {
		return out
	}
	return s
}

func text(l Lang, s string, depth int) (string, bool) {
	cat := catalogs[l]
	if cat == nil {
		return s, false
	}
	if tr, ok := cat[s]; ok {
		return tr, true
	}
	if depth > 1 || s == "" {
		return s, false
	}
	for _, p := range patternsFor(l, cat) {
		m := p.re.FindStringSubmatch(s)
		if m == nil {
			continue
		}
		args := make([]any, len(m)-1)
		for i, a := range m[1:] {
			if out, ok := text(l, a, depth+1); ok {
				a = out
			}
			args[i] = a
		}
		return fmt.Sprintf(p.tr, args...), true
	}
	return s, false
}

// pattern 是由格式串生成的正则：参数位置换成捕获组，译文里的动词都换成 %s（参数是捕获到的文字）。
type pattern struct {
	re      *regexp.Regexp
	tr      string
	literal int    // 格式串里固定文字的长度，越长越具体，先试
	key     string // 格式串本身，长度相同时按它排序，结果不随 map 的顺序变
}

var (
	verbRe    = regexp.MustCompile(`%(\[\d+\])?[-+# 0]*\d*(\.\d+)?[a-zA-Z]`)
	patternMu sync.Mutex
	patternOf = map[Lang]struct {
		cat  map[string]string
		list []pattern
	}{}
)

func patternsFor(l Lang, cat map[string]string) []pattern {
	patternMu.Lock()
	defer patternMu.Unlock()
	if c, ok := patternOf[l]; ok && reflect.ValueOf(c.cat).Pointer() == reflect.ValueOf(cat).Pointer() && len(c.cat) == len(cat) {
		return c.list
	}
	var list []pattern
	for zh, tr := range cat {
		f := strings.ReplaceAll(zh, "%%", "\x00") // %% 是百分号本身，不是动词
		locs := verbRe.FindAllStringIndex(f, -1)
		if len(locs) == 0 {
			continue
		}
		var b strings.Builder
		b.WriteString("(?s)^")
		literal, last := 0, 0
		addLiteral := func(s string) {
			s = strings.ReplaceAll(s, "\x00", "%")
			b.WriteString(regexp.QuoteMeta(s))
			literal += len(s)
		}
		for _, loc := range locs {
			addLiteral(f[last:loc[0]])
			if strings.HasSuffix(f[loc[0]:loc[1]], "d") {
				b.WriteString(`(-?\d+)`)
			} else {
				b.WriteString(`(.+?)`)
			}
			last = loc[1]
		}
		addLiteral(f[last:])
		b.WriteString("$")
		if literal < 4 {
			continue // 只有参数和标点的格式串什么都能对上
		}
		re, err := regexp.Compile(b.String())
		if err != nil {
			continue
		}
		tr = verbRe.ReplaceAllStringFunc(strings.ReplaceAll(tr, "%%", "\x00"), func(v string) string {
			if m := verbRe.FindStringSubmatch(v); m[1] != "" {
				return "%" + m[1] + "s"
			}
			return "%s"
		})
		tr = strings.ReplaceAll(tr, "\x00", "%%")
		list = append(list, pattern{re: re, tr: tr, literal: literal, key: zh})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].literal != list[j].literal {
			return list[i].literal > list[j].literal
		}
		return list[i].key < list[j].key
	})
	patternOf[l] = struct {
		cat  map[string]string
		list []pattern
	}{cat, list}
	return list
}

// Ref 是错误文字里提到的一个对象（现在只有节点）：格式化时就是它的名称，另外带着 ID。
// 面板生成报错时把节点作为 Ref 传给 Errorf，之后可以按查看的人换掉名称（见 Redact），不用在整段文字里找名称。
type Ref struct{ Kind, ID, Name string }

func (r Ref) String() string { return r.Name }

// NodeRef 是对一个节点的引用。
func NodeRef(id, name string) Ref { return Ref{Kind: "node", ID: id, Name: name} }

// Redactor 是能把自己提到的对象换个名称的错误（例如节点离线的错误）。没有要换的返回 false。
type Redactor interface {
	Redact(hide func(Ref) (string, bool)) (error, bool)
}

// Redact 把 err 里提到的对象换掉名称：hide 返回 (新的名称, true) 的换。返回换过的错误，以及有没有换。
// 只认结构上的引用：Errorf 的参数里的 Ref、参数里的错误、Detail、实现了 Redactor 的错误、只是加了类型的包装
// （文字和里面的错误完全相同）和 errors.Join 连起来的错误。fmt.Errorf 包着的不认：带引用的错误用 Errorf 包。不在文字里找名称：请求的人自己写进报错的文字怎么写都不会被换，也就不能借
// 「换没换」探出有没有某个名称的对象。
func Redact(err error, hide func(Ref) (string, bool)) (error, bool) {
	switch e := err.(type) {
	case nil:
		return nil, false
	case Redactor:
		return e.Redact(hide)
	case *Error:
		if e.literal {
			return err, false
		}
		args := make([]any, len(e.args))
		changed := false
		for i, a := range e.args {
			args[i] = a
			switch v := a.(type) {
			case Ref:
				if name, ok := hide(v); ok {
					args[i], changed = name, true
				}
			case error:
				if r, ok := Redact(v, hide); ok {
					args[i], changed = r, true
				}
			}
		}
		if !changed {
			return err, false
		}
		return Errorf(e.format, args...), true
	case detailed:
		// cause 只留给日志，文字里会附在后面：err 换过时一起换，换不了的就不带了。
		r, changed := Redact(e.err, hide)
		c, cc := Redact(e.cause, hide)
		if !changed && !cc {
			return err, false
		}
		return detailed{r, c}, true
	}
	// 只是加了类型的包装：换里面的。要保留自己类型的包装实现 Redactor（例如 spec 的参数错误）。
	if u, ok := err.(interface{ Unwrap() error }); ok {
		if inner := u.Unwrap(); inner != nil && inner.Error() == err.Error() {
			return Redact(inner, hide)
		}
	}
	// errors.Join 连起来的几个错误：逐个换，再连回去。
	if u, ok := err.(interface{ Unwrap() []error }); ok {
		inner := u.Unwrap()
		if joined := errors.Join(inner...); joined != nil && joined.Error() == err.Error() {
			out := make([]error, len(inner))
			changed := false
			for i, e := range inner {
				r, ok := Redact(e, hide)
				out[i], changed = r, changed || ok
			}
			if changed {
				return errors.Join(out...), true
			}
		}
	}
	return err, false
}

// 引用写进文字时的标记：markOpen 类型:ID markSep 名称 markClose。用控制字符，名称和 ID 里不会有。
const (
	markOpen  = "\x02"
	markSep   = "\x1f"
	markClose = "\x03"
)

var markStrip = strings.NewReplacer(markOpen, "", markSep, "", markClose, "")

// Marked 把一个错误拼成文字，其中的引用写成标记：文字要存下来、以后给不同的人看时用（审计的失败详情），
// 显示时用 RenderRefs 按查看的人换成名称。render 拿到 hide 以后自己调 Redact 并取出文字。
// 文字里原有的标记字符去掉：只有真正的引用才会成为标记，请求的人自己写不出来。
func Marked(render func(hide func(Ref) (string, bool)) string) string {
	var nonce [8]byte
	_, _ = rand.Read(nonce[:])
	tag := "\x00" + hex.EncodeToString(nonce[:]) + ":"
	var refs []Ref
	s := render(func(r Ref) (string, bool) {
		refs = append(refs, r)
		return tag + strconv.Itoa(len(refs)-1) + "\x00", true
	})
	s = markStrip.Replace(s)
	for i, r := range refs {
		mark := markOpen + markStrip.Replace(r.Kind) + ":" + markStrip.Replace(r.ID) + markSep + markStrip.Replace(r.Name) + markClose
		s = strings.ReplaceAll(s, tag+strconv.Itoa(i)+"\x00", mark)
	}
	return s
}

// RenderRefs 把 Marked 写的文字里的标记换成 name 返回的文字。没有标记的文字原样返回；
// 被截断的标记（存的时候有长度上限）连同后面的内容去掉。
func RenderRefs(s string, name func(Ref) string) string {
	if !strings.Contains(s, markOpen) {
		return s
	}
	var b strings.Builder
	for {
		i := strings.Index(s, markOpen)
		if i < 0 {
			b.WriteString(s)
			break
		}
		b.WriteString(s[:i])
		rest := s[i+len(markOpen):]
		j := strings.Index(rest, markClose)
		if j < 0 {
			break
		}
		head, label, _ := strings.Cut(rest[:j], markSep)
		kind, id, _ := strings.Cut(head, ":")
		b.WriteString(name(Ref{Kind: kind, ID: id, Name: label}))
		s = rest[j+len(markClose):]
	}
	return b.String()
}

// PlainRefs 把标记换成记下的名称。
func PlainRefs(s string) string {
	return RenderRefs(s, func(r Ref) string { return r.Name })
}
