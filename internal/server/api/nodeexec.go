package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/SIPC/nagi/internal/pkg/i18n"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// RunNodeCommand 在节点上执行一条命令并等它结束，给 MCP 的 node_exec 工具用（见 docs/mcp.md#节点命令）。
// 这等于节点的 root，所以比节点终端还多一道：只有管理员的访问令牌、创建时单独勾选了「节点终端」才能用；
// 网页的会话不走这里。每次执行都记进活动记录（连同命令），并发一条告警。节点上关闭了节点访问时节点会拒绝。
func (a *API) RunNodeCommand(ctx context.Context, nodeID, command, cwd string, timeout time.Duration) (ExecResult, error) {
	u := auth.CurrentUser(ctx)
	fail := func(err error) (ExecResult, error) {
		a.recordNodeExec(ctx, nodeID, command, activityFailure(err))
		return ExecResult{}, err
	}
	switch {
	case command == "":
		return ExecResult{}, i18n.New("命令不能为空")
	case u.Role != auth.RoleAdmin || u.TokenID == 0 || !u.AllowNodeExec:
		return fail(i18n.New("当前账号没有权限执行这个操作"))
	case a.Panel.Standby != nil:
		return fail(a.standbyError())
	}
	conn, err := a.Nodes.Get(nodeID)
	if err != nil {
		return fail(err)
	}
	// 节点那边按同一个超时结束命令；这里多等一会儿，好收到它的退出码。
	ctx2, cancel := context.WithTimeout(ctx, timeout+10*time.Second)
	defer cancel()
	stream, err := conn.Agent.NodeExec(ctx2, &nagiv1.NodeExecRequest{
		Command: command, Cwd: cwd, TimeoutSeconds: uint32(timeout / time.Second), Actor: u.Name + " (" + u.Via + ")",
	})
	if err != nil {
		return fail(nodeExecError(err))
	}
	var out headTail
	res := ExecResult{ExitCode: -1}
	began, started := time.Now(), false
	for {
		msg, err := stream.Recv()
		if err != nil {
			if ctx2.Err() != nil {
				res.TimedOut = true
			} else if !errors.Is(err, io.EOF) {
				return fail(nodeExecError(err))
			}
			break
		}
		if !started {
			started = true
			a.alertNodeExec(ctx, nodeID, u)
		}
		switch v := msg.Output.(type) {
		case *nagiv1.ExecOutput_Stdout:
			out.Write(v.Stdout)
		case *nagiv1.ExecOutput_ExitCode:
			res.ExitCode = v.ExitCode
		}
	}
	res.Output, res.Truncated = out.String()
	// 节点那边超时时退出码是 -1。
	if res.ExitCode == -1 && time.Since(began) >= timeout {
		res.TimedOut = true
	}
	detail, failure := command, ""
	switch {
	case res.TimedOut:
		failure = "timeout"
	case res.ExitCode != 0:
		detail += " → exit " + strconv.Itoa(int(res.ExitCode))
	}
	a.recordNodeExec(ctx, nodeID, detail, failure)
	return res, nil
}

// nodeExecError 翻译节点返回的错误：旧版本节点没有这个接口。
func nodeExecError(err error) error {
	if st, ok := status.FromError(err); ok && st.Code() == codes.Unimplemented {
		return i18n.New("节点版本过低，升级后才能执行节点命令")
	}
	return fromGRPC(err)
}

// recordNodeExec 记一条节点命令的活动：详情是命令，失败时后面接原因。
func (a *API) recordNodeExec(ctx context.Context, nodeID, command, failure string) {
	ctx = context.WithoutCancel(ctx)
	detail := command
	if failure != "" {
		detail += ": " + failure
	}
	err := a.Q.InsertActivity(ctx, store.InsertActivityParams{
		CreatedAt: time.Now().Unix(), Kind: "operation", Actor: clipActivity(auth.Username(ctx), 64), Action: "NodeExec",
		Target: clipActivity(a.nodeName(ctx, nodeID), 200), Detail: clipActivity(detail, 500),
		Failed: boolInt(failure != ""), Via: clipActivity(auth.CurrentUser(ctx).Via, 80),
	})
	if err != nil {
		a.Log.Warn("写入活动记录失败", "err", err)
		return
	}
	_ = a.Q.PruneActivity(ctx, activityKeep)
}

// alertNodeExec 在节点上执行命令时发一条通知，和打开节点终端一样，令牌被盗用时用户能马上知道。命令本身不发出去（可能带密钥）。
func (a *API) alertNodeExec(ctx context.Context, nodeID string, u auth.User) {
	ctx = context.WithoutCancel(ctx)
	name := a.nodeName(ctx, nodeID)
	go func() {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		a.Nodes.SendPanelAlert(ctx, &nagiv1.Alert{
			Key: "node-exec:" + nodeID, Status: "firing", Title: "已在节点上执行命令",
			Message: fmt.Sprintf("用户：%s；来源：%s；节点：%s。命令见日志", u.Name, u.Via, name),
			Node:    name, Time: timestamppb.Now(),
		})
	}()
}
