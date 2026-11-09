package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/SIPC/nagi/internal/pkg/i18n"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/coder/websocket"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TerminalHandler 处理 GET /ws/exec?project=&container=&service=&cols=&rows=&lang=：
// 浏览器通过 WebSocket 打开容器终端，面板桥接到节点的 AgentService.Exec。
//
// 消息格式：二进制帧是终端的输入输出；文本帧是控制消息（JSON），
// 浏览器发 {"type":"resize","cols":80,"rows":24}，面板发 {"type":"exit","code":0} 或 {"type":"error","message":"..."}。
// 需要已登录；WebSocket 库默认只接受同源请求，防止其他网站借用户的 cookie 打开终端。
func (a *API) TerminalHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		token := auth.SessionToken(r.Header)
		u, err := a.Auth.SessionUser(r.Context(), token)
		if err != nil {
			http.Error(w, i18n.Sprintf(lang, "请先登录"), http.StatusUnauthorized)
			return
		}
		if !auth.CanOperate(u.Role) {
			http.Error(w, i18n.Sprintf(lang, "当前账号没有权限执行这个操作"), http.StatusForbidden)
			return
		}
		if err := a.projectAllowed(r.Context(), u, r.URL.Query().Get("project")); err != nil {
			http.Error(w, i18n.Localize(lang, err), http.StatusNotFound)
			return
		}
		q := r.URL.Query()
		cols, _ := strconv.Atoi(q.Get("cols"))
		rows, _ := strconv.Atoi(q.Get("rows"))
		if cols <= 0 || rows <= 0 {
			cols, rows = 80, 24
		}

		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		ctx, cancel := context.WithCancel(auth.WithUser(r.Context(), u))
		defer cancel()
		fail := wsFail(ctx, ws)

		start := &nagiv1.ExecStart{
			ProjectId: q.Get("project"),
			Container: q.Get("container"),
			Service:   q.Get("service"),
			Size:      &nagiv1.TerminalSize{Cols: uint32(cols), Rows: uint32(rows)},
		}
		if err := a.checkPrivilegedEntry(ctx, u.Role, start.ProjectId, start.Container); err != nil {
			a.recordOperation(ctx, "/OpenTerminal", start, activityFailure(err))
			fail(i18n.Localize(lang, err))
			return
		}
		nodeID, err := a.Projects.Locate(ctx, q.Get("project"), q.Get("container"))
		if err != nil {
			a.recordOperation(ctx, "/OpenTerminal", start, activityFailure(err))
			fail(i18n.Localize(lang, err))
			return
		}
		conn, err := a.Nodes.Get(nodeID)
		if err != nil {
			a.recordOperation(ctx, "/OpenTerminal", start, activityFailure(err))
			fail(i18n.Localize(lang, err))
			return
		}
		stream, err := conn.Agent.Exec(ctx)
		if err == nil {
			err = stream.Send(&nagiv1.ExecInput{Input: &nagiv1.ExecInput_Start{Start: start}})
		}
		if err != nil {
			a.recordOperation(ctx, "/OpenTerminal", start, activityFailure(err))
			fail(terminalError(lang, err))
			return
		}
		a.recordOperation(ctx, "/OpenTerminal", start, "")

		// 定时重查：踢出登录、降权、移出项目范围之后要断开。管理员降为操作员时，特权容器的终端也要断开。
		go a.watchSession(ctx, token, u, func(cur auth.User) error {
			if err := a.containerAccess(ctx, cur, start.ProjectId, auth.CanOperate); err != nil {
				return err
			}
			if err := a.checkPrivilegedEntry(ctx, cur.Role, start.ProjectId, start.Container); errors.Is(err, errPrivilegedEntry) {
				return err
			}
			return nil
		}, func(err error) {
			fail(i18n.Localize(lang, err))
			cancel()
		})

		bridgeTerminal(ctx, cancel, ws, lang, newActivityClock(), terminalStream{
			stdin: func(b []byte) error {
				return stream.Send(&nagiv1.ExecInput{Input: &nagiv1.ExecInput_Stdin{Stdin: b}})
			},
			resize: func(cols, rows uint32) error {
				return stream.Send(&nagiv1.ExecInput{Input: &nagiv1.ExecInput_Resize{Resize: &nagiv1.TerminalSize{Cols: cols, Rows: rows}}})
			},
			closeSend: stream.CloseSend,
			recv:      stream.Recv,
		})
	})
}

// sessionRecheck 是重查打开着的连接（终端、日志流）的间隔：踢出登录、降权、移出项目范围之后要断开。测试里会改短。
var sessionRecheck = 30 * time.Second

var (
	errSessionExpired = i18n.New("登录已过期，连接已断开")
	errRoleChanged    = i18n.New("权限已变更，连接已断开")
)

// watchSession 每隔 sessionRecheck 重查一次会话，直到 ctx 结束：会话失效或者换了账号，或者 check（拿到重新读出的用户）
// 返回错误时，调用 stop 并返回。
func (a *API) watchSession(ctx context.Context, token string, u auth.User, check func(cur auth.User) error, stop func(error)) {
	t := time.NewTicker(sessionRecheck)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		cur, err := a.Auth.SessionUser(ctx, token)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil || cur.ID != u.ID:
			err = errSessionExpired
		default:
			err = check(cur)
		}
		if err != nil {
			stop(err)
			return
		}
	}
}

// containerAccess 重查用户还能不能进入项目的容器：角色和项目范围。
func (a *API) containerAccess(ctx context.Context, cur auth.User, projectID string, allowed func(role string) bool) error {
	if !allowed(cur.Role) {
		return errRoleChanged
	}
	if err := a.projectAllowed(ctx, cur, projectID); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return errRoleChanged
	}
	return nil
}

// terminalError 返回终端出错时给浏览器看的说明：节点返回的说明按语言翻译，连接层面的错误（EOF、
// "error reading from server" 这类 gRPC 的原始信息）换成「与节点的连接已断开」。
func terminalError(lang i18n.Lang, err error) string {
	if !isStatus(err) {
		return i18n.Sprintf(lang, "与节点的连接已断开")
	}
	switch status.Code(err) {
	case codes.Unimplemented:
		return i18n.Sprintf(lang, "节点版本过低，升级后才能打开终端")
	case codes.Unavailable, codes.Canceled, codes.Unknown, codes.DeadlineExceeded:
		return i18n.Sprintf(lang, "与节点的连接已断开")
	}
	return errorText(lang, err)
}

// wsFail 返回发送错误并关闭 WebSocket 的函数。
func wsFail(ctx context.Context, ws *websocket.Conn) func(string) {
	return func(msg string) {
		b, _ := json.Marshal(map[string]any{"type": "error", "message": msg})
		_ = ws.Write(ctx, websocket.MessageText, b)
		ws.Close(websocket.StatusNormalClosure, "")
	}
}

// terminalStream 是节点上的一个终端：容器终端和节点终端的消息类型不同，桥接的逻辑一样。
type terminalStream struct {
	stdin     func([]byte) error
	resize    func(cols, rows uint32) error
	closeSend func() error
	recv      func() (*nagiv1.ExecOutput, error)
}

// activityClock 记下终端最后一次有输入输出的时间。
type activityClock struct{ at atomic.Int64 }

func newActivityClock() *activityClock {
	c := &activityClock{}
	c.touch()
	return c
}

func (c *activityClock) touch() { c.at.Store(time.Now().UnixNano()) }

func (c *activityClock) since() time.Duration { return time.Since(time.Unix(0, c.at.Load())) }

// bridgeTerminal 把 WebSocket 和节点上的终端接起来，直到一边结束。
func bridgeTerminal(ctx context.Context, cancel context.CancelFunc, ws *websocket.Conn, lang i18n.Lang, active *activityClock, st terminalStream) {
	fail := wsFail(ctx, ws)
	// 浏览器 → 节点
	go func() {
		defer st.closeSend()
		for {
			typ, data, err := ws.Read(ctx)
			if err != nil {
				cancel()
				return
			}
			active.touch()
			if typ == websocket.MessageBinary {
				err = st.stdin(data)
			} else {
				var msg struct {
					Type string `json:"type"`
					Cols uint32 `json:"cols"`
					Rows uint32 `json:"rows"`
				}
				if json.Unmarshal(data, &msg) != nil || msg.Type != "resize" {
					continue
				}
				err = st.resize(msg.Cols, msg.Rows)
			}
			if err != nil {
				return
			}
		}
	}()

	// 节点 → 浏览器
	for {
		out, err := st.recv()
		if err != nil {
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				ws.Close(websocket.StatusNormalClosure, "")
			} else {
				fail(terminalError(lang, err))
			}
			return
		}
		active.touch()
		switch v := out.Output.(type) {
		case *nagiv1.ExecOutput_Stdout:
			if err := ws.Write(ctx, websocket.MessageBinary, v.Stdout); err != nil {
				return
			}
		case *nagiv1.ExecOutput_ExitCode:
			b, _ := json.Marshal(map[string]any{"type": "exit", "code": v.ExitCode})
			_ = ws.Write(ctx, websocket.MessageText, b)
			ws.Close(websocket.StatusNormalClosure, "")
			return
		}
	}
}
