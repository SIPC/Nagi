package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/coder/websocket"
	"google.golang.org/grpc/status"
)

// TerminalHandler 处理 GET /ws/exec?node=&project=&service=&cols=&rows=：
// 浏览器通过 WebSocket 打开容器终端，面板桥接到节点的 AgentService.Exec。
//
// 消息格式：二进制帧是终端的输入输出；文本帧是控制消息（JSON），
// 浏览器发 {"type":"resize","cols":80,"rows":24}，面板发 {"type":"exit","code":0} 或 {"type":"error","message":"..."}。
// 需要已登录；WebSocket 库默认只接受同源请求，防止其他网站借用户的 cookie 打开终端。
func (a *API) TerminalHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := a.Auth.Authenticate(r.Context(), auth.SessionToken(r.Header)); err != nil {
			http.Error(w, "未登录", http.StatusUnauthorized)
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
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()

		fail := func(msg string) {
			b, _ := json.Marshal(map[string]any{"type": "error", "message": msg})
			_ = ws.Write(ctx, websocket.MessageText, b)
			ws.Close(websocket.StatusNormalClosure, "")
		}

		conn, err := a.Nodes.Get(q.Get("node"))
		if err != nil {
			fail("节点不在线")
			return
		}
		stream, err := conn.Agent.Exec(ctx)
		if err != nil {
			fail(err.Error())
			return
		}
		if err := stream.Send(&nagiv1.ExecInput{Input: &nagiv1.ExecInput_Start{Start: &nagiv1.ExecStart{
			ProjectId: q.Get("project"),
			Container: q.Get("container"),
			Service:   q.Get("service"),
			Size:      &nagiv1.TerminalSize{Cols: uint32(cols), Rows: uint32(rows)},
		}}}); err != nil {
			fail(err.Error())
			return
		}

		// 浏览器 → 节点
		go func() {
			defer stream.CloseSend()
			for {
				typ, data, err := ws.Read(ctx)
				if err != nil {
					cancel()
					return
				}
				var in *nagiv1.ExecInput
				if typ == websocket.MessageBinary {
					in = &nagiv1.ExecInput{Input: &nagiv1.ExecInput_Stdin{Stdin: data}}
				} else {
					var msg struct {
						Type string `json:"type"`
						Cols uint32 `json:"cols"`
						Rows uint32 `json:"rows"`
					}
					if json.Unmarshal(data, &msg) != nil || msg.Type != "resize" {
						continue
					}
					in = &nagiv1.ExecInput{Input: &nagiv1.ExecInput_Resize{Resize: &nagiv1.TerminalSize{Cols: msg.Cols, Rows: msg.Rows}}}
				}
				if err := stream.Send(in); err != nil {
					return
				}
			}
		}()

		// 节点 → 浏览器
		for {
			out, err := stream.Recv()
			if err != nil {
				if errors.Is(err, io.EOF) || ctx.Err() != nil {
					ws.Close(websocket.StatusNormalClosure, "")
				} else if st, ok := status.FromError(err); ok {
					fail(st.Message())
				} else {
					fail(err.Error())
				}
				return
			}
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
	})
}
