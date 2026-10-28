package api

import (
	"context"
	"errors"
	"net/http"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/i18n"
)

// langInterceptor 按请求头 Accept-Language 确定语言：放进 ctx（i18n.T 用），返回的错误换成这个语言的文字。
// 前端总是带上界面当前的语言。要放在拦截器的最外层，登录检查等拦截器返回的错误也要换。
type langInterceptor struct{}

func (langInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		lang := i18n.Parse(req.Header().Get("Accept-Language"))
		resp, err := next(i18n.WithLang(ctx, lang), req)
		return resp, localizeError(lang, err)
	}
}

func (langInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (langInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		lang := i18n.Parse(conn.RequestHeader().Get("Accept-Language"))
		return localizeError(lang, next(i18n.WithLang(ctx, lang), conn))
	}
}

// requestLang 是普通 HTTP 请求（上传、终端、webhook……）的语言：查询参数 lang 优先
// （WebSocket 不能设请求头，前端打开终端时放在这里），其次是 Accept-Language。
func requestLang(r *http.Request) i18n.Lang {
	if l, ok := i18n.Match(r.URL.Query().Get("lang")); ok {
		return l
	}
	return i18n.Parse(r.Header.Get("Accept-Language"))
}

// localizeError 把错误的文字换成 lang，错误码、details 和 metadata 不变。
func localizeError(lang i18n.Lang, err error) error {
	if err == nil {
		return nil
	}
	var ce *connect.Error
	if !errors.As(err, &ce) {
		msg := i18n.Localize(lang, err)
		if msg == err.Error() {
			return err
		}
		return connect.NewError(connect.CodeOf(err), errors.New(msg))
	}
	inner := ce.Unwrap()
	if inner == nil {
		return err
	}
	msg := i18n.Localize(lang, inner)
	if msg == ce.Message() {
		return err
	}
	out := connect.NewError(ce.Code(), errors.New(msg))
	for _, d := range ce.Details() {
		out.AddDetail(d)
	}
	for k, v := range ce.Meta() {
		out.Meta()[k] = v
	}
	return out
}
