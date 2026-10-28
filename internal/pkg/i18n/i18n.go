// Package i18n 让面板返回给浏览器的文案按请求的语言显示（中文、英文）。
//
// 中文是源语言：代码里照常写中文的格式串，英文译文登记在 en.go 里，以中文格式串为键。
// 错误用 Errorf / New 创建，保留格式串和参数，到接口边界再用 Localize 按请求的语言拼出文字；
// Error() 始终是中文，日志不受影响。没有译文的格式串原样显示中文。
package i18n

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
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
	if s, ok := catalogs[l][err.Error()]; ok {
		return s
	}
	return err.Error()
}
