package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/i18n"
	"github.com/SIPC/nagi/internal/pkg/mesh"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/proto/nagi/v1/nagiv1connect"
	"github.com/SIPC/nagi/internal/server/audit"
	"github.com/SIPC/nagi/internal/server/auth"
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
	// maxNodeUpload 是上传到节点的单个文件的上限。
	maxNodeUpload = 8 << 30
)

// elevationHeader 让界面分清"还没有做高权限确认"和别的拒绝原因。
const elevationHeader = "X-Nagi-Elevation"

// elevationRequired 是要先做高权限确认的接口。
var elevationRequired = map[string]bool{
	// 测试外送会把保存的凭据发到填写的地址（见 docs/audit.md#外送）。
	nagiv1connect.SettingsServiceTestAuditSinkProcedure:     true,
	nagiv1connect.NodeServiceListNodeDirProcedure:           true,
	nagiv1connect.NodeServiceReadNodeFileProcedure:          true,
	nagiv1connect.NodeServiceWriteNodeFileProcedure:         true,
	nagiv1connect.NodeServiceDeleteNodeFileProcedure:        true,
	nagiv1connect.NodeServiceCreateNodeDirProcedure:         true,
	nagiv1connect.NodeServiceListNodeFileBackupsProcedure:   true,
	nagiv1connect.NodeServiceRestoreNodeFileBackupProcedure: true,
	nagiv1connect.NodeServiceRenameNodeFileProcedure:        true,
	nagiv1connect.NodeServiceMoveNodeFilesProcedure:         true,
	nagiv1connect.NodeServiceCopyNodeFilesProcedure:         true,
	nagiv1connect.NodeServiceCompressNodeFilesProcedure:     true,
	nagiv1connect.NodeServiceExtractNodeFileProcedure:       true,
	nagiv1connect.NodeServiceListNodeDirSizesProcedure:      true,
	// 导入现有目录：读主机上的文件（见 docs/agent.md#现有目录）。
	nagiv1connect.NodeServiceInspectComposeDirProcedure:   true,
	nagiv1connect.ProjectServiceImportComposeDirProcedure: true,
	// 访问令牌是长期有效的凭据（见 docs/mcp.md#个人访问令牌）。
	nagiv1connect.SettingsServiceCreateAccessTokenProcedure: true,
	// 开启两步验证：否则拿到会话的人能换上自己的密钥，再用它通过高权限确认，账号主人也登不上了。
	// 已开启时确认要用当前的验证码，相当于换密钥前先验证旧的。EnableTOTP 只认这里发出的密钥，不用再拦。
	nagiv1connect.SettingsServiceBeginTOTPProcedure: true,
	// 添加通行密钥：和两步验证一样，拿到会话的人不能借它给自己留一个登录凭据。
	nagiv1connect.SettingsServiceBeginPasskeyRegistrationProcedure: true,
	// 管理员登录地址：拿到会话的人不能借它把账号主人锁在外面（见 docs/panel.md#管理员登录地址）。
	nagiv1connect.SettingsServiceSetAdminNetworksProcedure: true,
	// 恢复从节点缓存拼回的顶替和备份设置：会让别的节点接收并运行这个容器的配置（见 docs/panel.md#项目数据）。
	nagiv1connect.NodeServiceConfirmPendingHAProcedure: true,
	// 回放终端录制：输出里可能有密钥（见 docs/audit.md#终端与命令录制）。
	nagiv1connect.SettingsServiceGetRecordingProcedure: true,
	// 隔离和解除隔离节点（见 docs/mesh.md#隔离）。
	nagiv1connect.NodeServiceIsolateNodeProcedure:   true,
	nagiv1connect.NodeServiceUnisolateNodeProcedure: true,
	// 删除、清理节点上的镜像（见 docs/agent.md#节点镜像）。
	nagiv1connect.NodeServiceDeleteNodeImagesProcedure: true,
	nagiv1connect.NodeServicePruneNodeImagesProcedure:  true,
	// 副本的存放位置：在节点上任意位置建目录、写数据（见 docs/snapshot.md#副本的存放位置）。
	// 设置备份节点时指定存放位置也要确认，在 SetBackupNodes 里检查。
	nagiv1connect.NodeServiceSetNodeReplicaDirProcedure:      true,
	nagiv1connect.ProjectServiceSetBackupReplicaDirProcedure: true,
}

// needsElevation 判断这次调用要不要先做高权限确认。访问令牌按 auth.User.ElevatedFor 的规则（见 docs/mcp.md#高权限确认）。
func needsElevation(u auth.User, procedure string) bool {
	return elevationRequired[procedure] && !u.ElevatedFor(procedure)
}

func elevationError() error {
	err := connect.NewError(connect.CodePermissionDenied, i18n.New("请先验证身份"))
	err.Meta().Set(elevationHeader, "required")
	return err
}

// elevationCheck 拦下没有做高权限确认的请求。放在角色检查之后。
type elevationCheck struct{}

func (elevationCheck) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if needsElevation(auth.CurrentUser(ctx), req.Spec().Procedure) {
			return nil, elevationError()
		}
		return next(ctx, req)
	}
}

func (elevationCheck) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (elevationCheck) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		if needsElevation(auth.CurrentUser(ctx), conn.Spec().Procedure) {
			return elevationError()
		}
		return next(ctx, conn)
	}
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
		return connect.NewError(connect.CodeFailedPrecondition, i18n.New("节点版本过低，升级后才能使用此功能"))
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
	e := audit.Event{Action: action, TargetType: "node", TargetID: nodeID, TargetName: a.nodeName(ctx, nodeID)}
	if failed {
		e.Result, e.Error = audit.ResultFailed, detail
	} else if detail != "" {
		b, _ := json.Marshal(map[string]string{"detail": detail})
		e.Params = string(b)
	}
	a.Record(ctx, e)
}

// recordNodeShellOpen 记一条打开节点终端的记录，带上录制 ID。
func (a *API) recordNodeShellOpen(ctx context.Context, nodeID, ip, recordingID string) {
	ctx = context.WithoutCancel(ctx)
	b, _ := json.Marshal(map[string]string{"detail": "IP " + ip})
	a.Record(ctx, audit.Event{Action: "OpenNodeShell", TargetType: "node", TargetID: nodeID, TargetName: a.nodeName(ctx, nodeID),
		Params: string(b), RecordingID: recordingID})
}

// ---- 节点终端 ----

// NodeShellHandler 处理 GET /ws/node-shell?node=&cols=&rows=&cwd=&lang=：浏览器打开节点的终端，面板桥接到节点的 AgentService.NodeShell。
// 消息格式和容器终端一样。检查都在升级 WebSocket 之前，所以被拒绝时是普通的 HTTP 错误；界面先确认过身份，一般走不到。
func (a *API) NodeShellHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		token := auth.SessionToken(r.Header)
		u, err := a.Auth.RequestUser(r)
		if err != nil {
			http.Error(w, i18n.Sprintf(lang, "请先登录"), http.StatusUnauthorized)
			return
		}
		q := r.URL.Query()
		nodeID := q.Get("node")
		ctx, cancel := context.WithCancel(auth.WithRequest(auth.WithUser(r.Context(), u), r))
		defer cancel()
		ip := auth.RequestIP(r.RemoteAddr, r.Header)
		if u.Role != auth.RoleAdmin {
			a.recordNodeAccess(ctx, "OpenNodeShell", nodeID, "当前账号没有权限执行这个操作", true)
			http.Error(w, i18n.Sprintf(lang, "当前账号没有权限执行这个操作"), http.StatusForbidden)
			return
		}
		if !u.Elevated() {
			a.recordNodeAccess(ctx, "OpenNodeShell", nodeID, "请先验证身份", true)
			w.Header().Set(elevationHeader, "required")
			http.Error(w, i18n.Sprintf(lang, "请先验证身份"), http.StatusForbidden)
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
			a.recordNodeAccess(ctx, "OpenNodeShell", nodeID, err.Error(), true)
			fail(i18n.Localize(lang, err))
			return
		}
		stream, err := conn.Agent.NodeShell(ctx)
		if err == nil {
			err = stream.Send(&nagiv1.NodeShellInput{Input: &nagiv1.NodeShellInput_Start{Start: &nagiv1.NodeShellStart{
				Size: &nagiv1.TerminalSize{Cols: uint32(cols), Rows: uint32(rows)}, Cwd: q.Get("cwd"), Actor: u.Name, Ip: ip,
			}}})
		}
		if err != nil {
			a.recordNodeAccess(ctx, "OpenNodeShell", nodeID, activityFailure(err), true)
			fail(terminalError(lang, err))
			return
		}

		began := time.Now()
		// 录制先开始，打开终端的记录带上它的 ID：终端还开着时也能回放已经录下的部分。
		rec := a.Recordings.Start(ctx, cols, rows)
		a.recordNodeShellOpen(ctx, nodeID, ip, rec.ID())
		a.alertNodeShell(ctx, nodeID, u.Name, ip)
		defer func() { a.recordSessionEnd(ctx, "CloseNodeShell", "node", nodeID, "", rec.Close()) }()

		// 定时重查：踢出登录、降权之后要断开；太久没有动静、用了太久也断开。
		lastActive := newActivityClock()
		go a.watchSession(ctx, token, u, func(cur auth.User) error {
			switch {
			case cur.Role != auth.RoleAdmin:
				return errRoleChanged
			case time.Since(began) > nodeShellMaxAge:
				return i18n.New("已达到最长使用时间（12 小时），连接已断开")
			case lastActive.since() > nodeShellIdle:
				return i18n.New("空闲超过 1 小时，连接已断开")
			}
			return nil
		}, func(err error) {
			fail(i18n.Localize(lang, err))
			cancel()
		})

		bridgeTerminal(ctx, cancel, ws, lang, lastActive, terminalStream{
			rec: rec,
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
			Message: fmt.Sprintf("成员：%s；来源：%s；节点：%s", user, ip, name),
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
			msg := errorText(lang, err)
			if isStatus(err) {
				switch status.Code(err) {
				case codes.Unavailable, codes.DeadlineExceeded:
					msg = i18n.Localize(lang, i18n.New("无法连接节点，请稍后重试"))
				case codes.Unimplemented:
					msg = i18n.Localize(lang, i18n.New("节点版本过低，升级后才能使用此功能"))
				}
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": msg})
		}
		u, err := a.Auth.RequestUser(r)
		if err != nil {
			fail(http.StatusUnauthorized, i18n.New("请先登录"))
			return
		}
		q := r.URL.Query()
		nodeID, path := q.Get("node"), q.Get("path")
		ctx := auth.WithRequest(auth.WithUser(r.Context(), u), r)
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
			fail(http.StatusForbidden, i18n.New("请先验证身份"))
			return
		}
		m := a.Nodes.Mesh()
		if m == nil {
			fail(http.StatusServiceUnavailable, i18n.New("面板的节点网络没有启动，无法传输文件"))
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
			// check=1 只确认能下载，界面先问一次，出错时留在页面上提示。真正下载时才记录。
			fileDownload{
				open: func(ctx context.Context, offset, length int64) (chunkStream, error) {
					return client.NodeFileDownload(ctx, &nagiv1.NodeFileRef{NodeId: nodeID, Path: path, Offset: offset, Length: length})
				},
				record: record,
				fail:   fail,
				check:  q.Get("check") == "1",
				key:    fmt.Sprintf("%s|%s|%s", auth.Username(ctx), nodeID, path),
			}.serve(w, r)
			return
		}

		if r.Header.Get("X-Nagi-Upload") != "1" {
			fail(http.StatusBadRequest, i18n.New("缺少 X-Nagi-Upload 请求头"))
			return
		}
		if op := q.Get("op"); op != "" {
			nodeChunks(client, ref, q.Get("confirmed") == "1", record).serve(w, r, op)
			return
		}
		if r.Method != http.MethodPost {
			fail(http.StatusMethodNotAllowed, i18n.New("上传参数无效"))
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
