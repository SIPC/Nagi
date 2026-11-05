package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"time"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/i18n"
	"github.com/SIPC/nagi/internal/pkg/mesh"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/proto/nagi/v1/nagiv1connect"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/store"
	"github.com/coder/websocket"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// 节点访问：节点的终端和文件（见 docs/node-access.md）。只有管理员，要先做高权限确认。

const (
	// nodeShellIdle 是没有输入输出时断开节点终端的时长。
	nodeShellIdle = time.Hour
	// nodeShellMaxAge 是节点终端最长的使用时间，节点上也有同样的限制。
	nodeShellMaxAge = 12 * time.Hour
	// nodeShellRecheck 是重查会话和角色的间隔：踢出登录、降权之后要断开。
	nodeShellRecheck = 30 * time.Second
	// maxNodeUpload 是上传到节点的单个文件的上限。
	maxNodeUpload = 8 << 30
)

// elevationHeader 让界面分清"还没有做高权限确认"和别的拒绝原因。
const elevationHeader = "X-Nagi-Elevation"

// elevationRequired 是要先做高权限确认的接口。
var elevationRequired = map[string]bool{
	nagiv1connect.NodeServiceListNodeDirProcedure:           true,
	nagiv1connect.NodeServiceReadNodeFileProcedure:          true,
	nagiv1connect.NodeServiceWriteNodeFileProcedure:         true,
	nagiv1connect.NodeServiceDeleteNodeFileProcedure:        true,
	nagiv1connect.NodeServiceCreateNodeDirProcedure:         true,
	nagiv1connect.NodeServiceListNodeFileBackupsProcedure:   true,
	nagiv1connect.NodeServiceRestoreNodeFileBackupProcedure: true,
	// 导入现有目录：读主机上的文件（见 docs/agent.md#现有目录）。
	nagiv1connect.NodeServiceInspectComposeDirProcedure:   true,
	nagiv1connect.ProjectServiceImportComposeDirProcedure: true,
	// 访问令牌是长期有效的凭据（见 docs/mcp.md#个人访问令牌）。
	nagiv1connect.SettingsServiceCreateAccessTokenProcedure: true,
}

func elevationError() error {
	err := connect.NewError(connect.CodePermissionDenied, i18n.New("需要身份验证"))
	err.Meta().Set(elevationHeader, "required")
	return err
}

// elevationCheck 拦下没有做高权限确认的请求。放在角色检查之后。
type elevationCheck struct{}

func (elevationCheck) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if elevationRequired[req.Spec().Procedure] && !auth.CurrentUser(ctx).Elevated() {
			return nil, elevationError()
		}
		return next(ctx, req)
	}
}

func (elevationCheck) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (elevationCheck) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

func (s *authService) Elevate(ctx context.Context, req *connect.Request[nagiv1.ElevateRequest]) (*connect.Response[nagiv1.ElevateResponse], error) {
	until, err := s.Auth.Elevate(ctx, auth.RequestIP(req.Peer().Addr, req.Header()), auth.SessionToken(req.Header()), req.Msg.GetPassword(), req.Msg.GetTotpCode())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&nagiv1.ElevateResponse{ElevatedUntil: timestamppb.New(until)}), nil
}

// ---- 节点文件 ----

func (s *nodeService) ListNodeDir(ctx context.Context, req *connect.Request[nagiv1.NodeFileRef]) (*connect.Response[nagiv1.ListNodeDirResponse], error) {
	a, err := s.agent(req.Msg.GetNodeId())
	if err != nil {
		return nil, err
	}
	out, err := a.ListNodeDir(ctx, req.Msg)
	if err != nil {
		return nil, nodeAccessError(err)
	}
	return connect.NewResponse(out), nil
}

func (s *nodeService) ReadNodeFile(ctx context.Context, req *connect.Request[nagiv1.NodeFileRef]) (*connect.Response[nagiv1.ReadNodeFileResponse], error) {
	a, err := s.agent(req.Msg.GetNodeId())
	if err != nil {
		return nil, err
	}
	out, err := a.ReadNodeFile(ctx, req.Msg)
	if err != nil {
		return nil, nodeAccessError(err)
	}
	return connect.NewResponse(out), nil
}

func (s *nodeService) WriteNodeFile(ctx context.Context, req *connect.Request[nagiv1.WriteNodeFileRequest]) (*connect.Response[nagiv1.WriteNodeFileResponse], error) {
	a, err := s.agent(req.Msg.GetNodeId())
	if err != nil {
		return nil, err
	}
	out, err := a.WriteNodeFile(ctx, req.Msg)
	if err != nil {
		return nil, nodeAccessError(err)
	}
	return connect.NewResponse(out), nil
}

func (s *nodeService) DeleteNodeFile(ctx context.Context, req *connect.Request[nagiv1.DeleteNodeFileRequest]) (*connect.Response[nagiv1.NodeFileOpResponse], error) {
	a, err := s.agent(req.Msg.GetNodeId())
	if err != nil {
		return nil, err
	}
	out, err := a.DeleteNodeFile(ctx, req.Msg)
	if err != nil {
		return nil, nodeAccessError(err)
	}
	return connect.NewResponse(out), nil
}

func (s *nodeService) CreateNodeDir(ctx context.Context, req *connect.Request[nagiv1.NodeFileRef]) (*connect.Response[nagiv1.NodeFileOpResponse], error) {
	a, err := s.agent(req.Msg.GetNodeId())
	if err != nil {
		return nil, err
	}
	out, err := a.CreateNodeDir(ctx, req.Msg)
	if err != nil {
		return nil, nodeAccessError(err)
	}
	return connect.NewResponse(out), nil
}

func (s *nodeService) ListNodeFileBackups(ctx context.Context, req *connect.Request[nagiv1.NodeFileRef]) (*connect.Response[nagiv1.ListNodeFileBackupsResponse], error) {
	a, err := s.agent(req.Msg.GetNodeId())
	if err != nil {
		return nil, err
	}
	out, err := a.ListNodeFileBackups(ctx, req.Msg)
	if err != nil {
		return nil, nodeAccessError(err)
	}
	return connect.NewResponse(out), nil
}

func (s *nodeService) RestoreNodeFileBackup(ctx context.Context, req *connect.Request[nagiv1.RestoreNodeFileBackupRequest]) (*connect.Response[nagiv1.WriteNodeFileResponse], error) {
	a, err := s.agent(req.Msg.GetNodeId())
	if err != nil {
		return nil, err
	}
	out, err := a.RestoreNodeFileBackup(ctx, req.Msg)
	if err != nil {
		return nil, nodeAccessError(err)
	}
	return connect.NewResponse(out), nil
}

// nodeAccessError 转换节点返回的错误。旧版本的节点没有这些接口。
func nodeAccessError(err error) error {
	if st, ok := status.FromError(err); ok && st.Code() == codes.Unimplemented {
		return connect.NewError(connect.CodeFailedPrecondition, i18n.New("节点版本过低，请升级节点"))
	}
	return fromGRPC(err)
}

// ---- 活动记录 ----

// nodeName 返回节点在面板上显示的名称。
func (a *API) nodeName(ctx context.Context, nodeID string) string {
	if n, err := a.Q.GetNode(ctx, nodeID); err == nil && n.Name != "" {
		return n.Name
	}
	return nodeID
}

// recordNodeAccess 记下一次节点访问。不走 Connect 的入口（终端、上传下载）用它。
func (a *API) recordNodeAccess(ctx context.Context, action, nodeID, detail string, failed bool) {
	ctx = context.WithoutCancel(ctx)
	err := a.Q.InsertActivity(ctx, store.InsertActivityParams{
		CreatedAt: time.Now().Unix(),
		Kind:      "operation",
		Actor:     clipActivity(auth.Username(ctx), 64),
		Action:    action,
		Target:    clipActivity(a.nodeName(ctx, nodeID), 200),
		Detail:    clipActivity(detail, 500),
		Failed:    boolInt(failed),
	})
	if err != nil {
		a.Log.Warn("写入活动记录失败", "err", err)
		return
	}
	_ = a.Q.PruneActivity(ctx, activityKeep)
}

// ---- 节点终端 ----

// NodeShellHandler 处理 GET /ws/node-shell?node=&cols=&rows=&cwd=&lang=：浏览器打开节点的终端，面板桥接到节点的 AgentService.NodeShell。
// 消息格式和容器终端一样。检查都在升级 WebSocket 之前，所以被拒绝时是普通的 HTTP 错误；界面先确认过身份，一般走不到。
func (a *API) NodeShellHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		token := auth.SessionToken(r.Header)
		u, err := a.Auth.SessionUser(r.Context(), token)
		if err != nil {
			http.Error(w, i18n.Sprintf(lang, "未登录"), http.StatusUnauthorized)
			return
		}
		q := r.URL.Query()
		nodeID := q.Get("node")
		ctx, cancel := context.WithCancel(auth.WithUser(r.Context(), u))
		defer cancel()
		ip := auth.RequestIP(r.RemoteAddr, r.Header)
		if u.Role != auth.RoleAdmin {
			a.recordNodeAccess(ctx, "OpenNodeShell", nodeID, "当前账号没有权限执行这个操作", true)
			http.Error(w, i18n.Sprintf(lang, "当前账号没有权限执行这个操作"), http.StatusForbidden)
			return
		}
		if !u.Elevated() {
			a.recordNodeAccess(ctx, "OpenNodeShell", nodeID, "需要身份验证", true)
			w.Header().Set(elevationHeader, "required")
			http.Error(w, i18n.Sprintf(lang, "需要身份验证"), http.StatusForbidden)
			return
		}
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
		fail := wsFail(ctx, ws)

		conn, err := a.Nodes.Get(nodeID)
		if err != nil {
			a.recordNodeAccess(ctx, "OpenNodeShell", nodeID, "节点不在线", true)
			fail(i18n.Sprintf(lang, "节点不在线"))
			return
		}
		stream, err := conn.Agent.NodeShell(ctx)
		if err == nil {
			err = stream.Send(&nagiv1.NodeShellInput{Input: &nagiv1.NodeShellInput_Start{Start: &nagiv1.NodeShellStart{
				Size: &nagiv1.TerminalSize{Cols: uint32(cols), Rows: uint32(rows)}, Cwd: q.Get("cwd"), Actor: u.Name,
			}}})
		}
		if err != nil {
			a.recordNodeAccess(ctx, "OpenNodeShell", nodeID, activityFailure(err), true)
			fail(i18n.Localize(lang, nodeAccessError(err)))
			return
		}

		began := time.Now()
		a.recordNodeAccess(ctx, "OpenNodeShell", nodeID, "IP "+ip, false)
		a.alertNodeShell(ctx, nodeID, u.Name, ip)
		defer func() {
			a.recordNodeAccess(ctx, "CloseNodeShell", nodeID, fmt.Sprintf("%s，%s", time.Since(began).Round(time.Second), ip), false)
		}()

		// 定时重查：踢出登录、降权之后要断开；太久没有动静、用了太久也断开。
		lastActive := newActivityClock()
		go func() {
			t := time.NewTicker(nodeShellRecheck)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				}
				msg := ""
				cur, err := a.Auth.SessionUser(ctx, token)
				switch {
				case ctx.Err() != nil:
					return
				case err != nil || cur.ID != u.ID:
					msg = i18n.Sprintf(lang, "登录已失效，连接已断开")
				case cur.Role != auth.RoleAdmin:
					msg = i18n.Sprintf(lang, "权限已变更，连接已断开")
				case time.Since(began) > nodeShellMaxAge:
					msg = i18n.Sprintf(lang, "已达到最长使用时间（12 小时），连接已断开")
				case lastActive.since() > nodeShellIdle:
					msg = i18n.Sprintf(lang, "空闲超过 1 小时，连接已断开")
				}
				if msg != "" {
					fail(msg)
					cancel()
					return
				}
			}
		}()

		bridgeTerminal(ctx, cancel, ws, lang, lastActive, terminalStream{
			stdin: func(b []byte) error {
				return stream.Send(&nagiv1.NodeShellInput{Input: &nagiv1.NodeShellInput_Stdin{Stdin: b}})
			},
			resize: func(cols, rows uint32) error {
				return stream.Send(&nagiv1.NodeShellInput{Input: &nagiv1.NodeShellInput_Resize{Resize: &nagiv1.TerminalSize{Cols: cols, Rows: rows}}})
			},
			closeSend: stream.CloseSend,
			recv:      stream.Recv,
		})
	})
}

// alertNodeShell 打开节点终端时发一条通知，账号被盗用时用户能马上知道。发送可能很慢，不挡打开终端。
func (a *API) alertNodeShell(ctx context.Context, nodeID, user, ip string) {
	ctx = context.WithoutCancel(ctx)
	name := a.nodeName(ctx, nodeID)
	go func() {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		a.Nodes.SendPanelAlert(ctx, &nagiv1.Alert{
			Key: "node-shell:" + nodeID, Status: "firing", Title: "节点终端已打开",
			Message: fmt.Sprintf("用户：%s；来源：%s；节点：%s", user, ip, name),
			Node:    name, Time: timestamppb.Now(),
		})
	}()
}

// ---- 上传下载 ----

// NodeFileHandler 下载和上传节点上的文件：GET、POST /api/node-file?node=&path=&confirmed=。
// 不走 Connect，因为浏览器要直接下载、流式上传。内容经 mesh 传输，节点只接受主面板。
func (a *API) NodeFileHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		fail := func(code int, err error) {
			msg := i18n.Localize(lang, err)
			if st, ok := status.FromError(err); ok && err != nil {
				msg = st.Message()
				switch st.Code() {
				case codes.Unavailable, codes.DeadlineExceeded:
					msg = i18n.Localize(lang, i18n.New("无法连接节点"))
				case codes.Unimplemented:
					msg = i18n.Localize(lang, i18n.New("节点版本过低，请升级节点"))
				}
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": msg})
		}
		u, err := a.Auth.SessionUser(r.Context(), auth.SessionToken(r.Header))
		if err != nil {
			fail(http.StatusUnauthorized, i18n.New("未登录"))
			return
		}
		q := r.URL.Query()
		nodeID, path := q.Get("node"), q.Get("path")
		ctx := auth.WithUser(r.Context(), u)
		action := "DownloadNodeFile"
		if r.Method != http.MethodGet {
			action = "UploadNodeFile"
		}
		record := func(failure string) {
			detail := path
			if failure != "" {
				detail += ": " + failure
			}
			a.recordNodeAccess(ctx, action, nodeID, detail, failure != "")
		}
		if u.Role != auth.RoleAdmin {
			record("当前账号没有权限执行这个操作")
			fail(http.StatusForbidden, i18n.New("当前账号没有权限执行这个操作"))
			return
		}
		if !u.Elevated() {
			record("需要身份验证")
			w.Header().Set(elevationHeader, "required")
			fail(http.StatusForbidden, i18n.New("需要身份验证"))
			return
		}
		m := a.Nodes.Mesh()
		if m == nil {
			fail(http.StatusServiceUnavailable, i18n.New("面板的 mesh 没有启动，无法传输文件"))
			return
		}
		key, err := a.Nodes.NodeMeshKey(nodeID)
		if err != nil {
			fail(http.StatusServiceUnavailable, err)
			return
		}
		cc, err := mesh.Client(m, key)
		if err != nil {
			fail(http.StatusServiceUnavailable, err)
			return
		}
		defer cc.Close()
		client := nagiv1.NewMeshServiceClient(cc)
		ref := &nagiv1.NodeFileRef{NodeId: nodeID, Path: path}

		if r.Method == http.MethodGet {
			stream, err := client.NodeFileDownload(ctx, ref)
			if err != nil {
				record(activityFailure(err))
				fail(http.StatusBadGateway, err)
				return
			}
			head, err := stream.Recv()
			if err != nil {
				record(activityFailure(err))
				fail(http.StatusBadRequest, err)
				return
			}
			// check=1 只确认能下载，界面先问一次，出错时留在页面上提示。真正下载时才记录。
			if q.Get("check") == "1" {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			record("")
			// 一律作为附件下载，并禁止猜类型：不能在面板的域名下把节点上的文件当网页打开。
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": head.GetName()}))
			if !head.GetArchive() {
				w.Header().Set("Content-Length", strconv.FormatInt(head.GetSize(), 10))
			}
			for {
				c, err := stream.Recv()
				if errors.Is(err, io.EOF) {
					return
				}
				if err != nil {
					// 已经开始发内容了，只能断开连接，让浏览器知道下载没有完成。
					panic(http.ErrAbortHandler)
				}
				if _, err := w.Write(c.GetData()); err != nil {
					return
				}
			}
		}

		if r.Header.Get("X-Nagi-Upload") != "1" {
			fail(http.StatusBadRequest, i18n.New("缺少 X-Nagi-Upload 请求头"))
			return
		}
		if r.ContentLength > maxNodeUpload {
			record("file too large")
			fail(http.StatusRequestEntityTooLarge, i18n.New("单个文件不能超过 8 GB"))
			return
		}
		stream, err := client.NodeFileUpload(ctx)
		if err == nil {
			err = stream.Send(&nagiv1.NodeFileUploadChunk{Ref: ref, Confirmed: q.Get("confirmed") == "1"})
		}
		if err != nil {
			record(activityFailure(err))
			fail(http.StatusBadGateway, err)
			return
		}
		body := http.MaxBytesReader(w, r.Body, maxNodeUpload)
		buf := make([]byte, 256<<10)
		for {
			n, rerr := body.Read(buf)
			if n > 0 {
				if err := stream.Send(&nagiv1.NodeFileUploadChunk{Data: buf[:n]}); err != nil {
					break // 节点提前结束（例如拒绝），真正的原因在 CloseAndRecv 里。
				}
			}
			if errors.Is(rerr, io.EOF) {
				break
			}
			if rerr != nil {
				var tooBig *http.MaxBytesError
				if errors.As(rerr, &tooBig) {
					record("file too large")
					fail(http.StatusRequestEntityTooLarge, i18n.New("单个文件不能超过 8 GB"))
					return
				}
				record(activityFailure(rerr))
				fail(http.StatusBadRequest, rerr)
				return
			}
		}
		res, err := stream.CloseAndRecv()
		if err != nil {
			record(activityFailure(err))
			fail(http.StatusBadRequest, err)
			return
		}
		record("")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]int64{"size": res.GetSize()})
	})
}
