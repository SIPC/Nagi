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

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/i18n"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/audit"
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
		u, err := a.Auth.RequestUser(r)
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
		ctx, cancel := context.WithCancel(auth.WithRequest(auth.WithUser(r.Context(), u), r))
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
		// 录制先开始，打开终端的记录带上它的 ID：终端还开着时也能回放已经录下的部分。
		rec := a.Recordings.Start(ctx, cols, rows)
		a.recordOperation(withRecording(ctx, rec.ID()), "/OpenTerminal", start, "")

		// 定时重查：踢出登录、退出手机、降权、移出项目范围之后要断开。管理员降为操作员时，特权容器的终端也要断开。
		go a.watchSession(ctx, u, r.Header, appByDevice, func(cur auth.User) error {
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

		defer func() {
			a.recordSessionEnd(ctx, "CloseTerminal", "container", start.Container, start.ProjectId, rec.Close())
		}()
		bridgeTerminal(ctx, cancel, ws, lang, newActivityClock(), terminalStream{
			rec: rec,
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
	errAppLocked      = i18n.New("App 已超过解锁时长，连接已断开")
)

// appRecheck 是 App 打开的连接怎样重查调用者。
type appRecheck int

const (
	// appByToken：按打开时带的 App 令牌重查（auth.StreamUser）：令牌还在、没过期、网段没变，再加上设备记录。
	// 令牌过期（10 分钟）时结束，错误带 Nagi-App-Retoken，App 换新令牌重新打开。日志流用它：重新打开不丢东西。
	appByToken appRecheck = iota
	// appByDevice：按设备记录重查（auth.UnlockedDeviceUser），不看令牌是否过期。终端用它：令牌每 10 分钟就要换，
	// 按令牌查会把终端连同里面的进程一起断开；手机被退出、成员被删除或改密码时设备记录就没了。
	// 另外要求手机还在解锁时长内、签名失败次数没到上限（App 退到后台、锁屏以后不再换取令牌，过了解锁时长断开），
	// 连接从打开起最长用到会话设置里的「最长登录时间」，和网页会话的绝对上限一致（v0.35.0-fix#2）。
	appByDevice
)

// watchSession 每隔 sessionRecheck 重查一次调用者，直到 ctx 结束：调用者不再有效，或者 check（拿到重新读出的用户）
// 返回错误时，调用 stop 并返回。
//
//   - 网页会话：按会话 ID 重查：令牌每 15 分钟轮换一次，按令牌查会在轮换以后断开（见 docs/panel.md#会话）。
//   - App（u.DeviceID 不为空）：按 app 的方式重查（见 appRecheck），h 是打开连接时的请求头，带着 App 令牌。
//     管理员还要从允许的地址来（按 ctx 里这次连接的来源）。
//
// stop 拿到的错误：调用者不再有效时是 errSessionExpired；App 令牌要重新换取时是带 Nagi-App-Retoken 的 *connect.Error；
// 其余是 check 返回的错误。
func (a *API) watchSession(ctx context.Context, u auth.User, h http.Header, app appRecheck, check func(cur auth.User) error, stop func(error)) {
	opened := time.Now()
	t := time.NewTicker(sessionRecheck)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		cur, err := a.recheckCaller(ctx, u, h, app, opened)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
		default:
			err = check(cur)
		}
		if err != nil {
			stop(err)
			return
		}
	}
}

// recheckCaller 重新读出打开着的连接的调用者（见 watchSession）。opened 是连接打开的时间。
func (a *API) recheckCaller(ctx context.Context, u auth.User, h http.Header, app appRecheck, opened time.Time) (auth.User, error) {
	if u.DeviceID == "" {
		cur, err := a.Auth.SessionByID(ctx, u.SessionID)
		if err != nil || cur.ID != u.ID {
			return auth.User{}, errSessionExpired
		}
		return cur, nil
	}
	var cur auth.User
	var err error
	if app == appByToken {
		cur, err = a.Auth.StreamUser(ctx, u, h)
		if ce := new(connect.Error); errors.As(err, &ce) && ce.Meta().Get(auth.RetokenHeader) != "" {
			return auth.User{}, ce
		}
	} else {
		// 网页的会话从登录起最长 MaxAge（会话设置里的「最长登录时间」），按设备重查的连接从打开起同样最长 MaxAge。
		if time.Since(opened) > a.Auth.Policy(ctx).MaxAge {
			return auth.User{}, errSessionExpired
		}
		cur, err = a.Auth.UnlockedDeviceUser(ctx, u.DeviceID, auth.RequestMeta(ctx).IP)
		if errors.Is(err, auth.ErrAppLocked) {
			return auth.User{}, errAppLocked
		}
	}
	if err != nil || cur.ID != u.ID || cur.DeviceID != u.DeviceID {
		return auth.User{}, errSessionExpired
	}
	return cur, nil
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
	// rec 录下输出、窗口大小和输入（没开录制时为 nil）。没有回显的输入（密码）只记个数（见 audit/recording_input.go）。
	rec *audit.Recorder
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
				st.rec.Input(data)
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
				st.rec.Resize(msg.Cols, msg.Rows)
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
			st.rec.Output(v.Stdout)
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
