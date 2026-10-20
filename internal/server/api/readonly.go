package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/proto/nagi/v1/nagiv1connect"
)

// 备用面板只读（见 docs/standby.md）：接口只放行查询，修改返回"这是备用面板"。

// readOnlyPrefixes 是备用面板上放行的接口：方法名以这些开头的都是查询。
var readOnlyPrefixes = []string{"Get", "List", "Query", "Stream", "Check", "Ping"}

// readOnlyAllowed 是另外放行的接口：登录、退出只改本面板自己的会话；提升为主面板。
var readOnlyAllowed = map[string]bool{
	nagiv1connect.AuthServiceLoginProcedure:              true,
	nagiv1connect.AuthServiceLogoutProcedure:             true,
	nagiv1connect.AuthServiceMeProcedure:                 true,
	nagiv1connect.SettingsServicePromoteStandbyProcedure: true,
}

func readOnlyProcedure(procedure string) bool {
	if readOnlyAllowed[procedure] {
		return true
	}
	i := strings.LastIndexByte(procedure, '/')
	method := procedure[i+1:]
	for _, p := range readOnlyPrefixes {
		if strings.HasPrefix(method, p) {
			return true
		}
	}
	return false
}

// standbyError 是备用面板拒绝修改时的说明。
func (a *API) standbyError() string {
	return "这是备用面板，只能查看；修改请到主面板 " + a.Panel.Standby.Info.PrimaryAddress
}

// readOnly 拒绝备用面板上的修改。
type readOnly struct{ a *API }

func (r readOnly) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if !readOnlyProcedure(req.Spec().Procedure) {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New(r.a.standbyError()))
		}
		return next(ctx, req)
	}
}

func (r readOnly) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (r readOnly) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		if !readOnlyProcedure(conn.Spec().Procedure) {
			return connect.NewError(connect.CodeFailedPrecondition, errors.New(r.a.standbyError()))
		}
		return next(ctx, conn)
	}
}

// standbyBlock 在备用面板上拒绝会修改数据的 HTTP 接口（webhook、上传、终端）。
func (a *API) standbyBlock(h http.Handler) http.Handler {
	if a.Panel.Standby == nil {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, a.standbyError(), http.StatusServiceUnavailable)
	})
}
