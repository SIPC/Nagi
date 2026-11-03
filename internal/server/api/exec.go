package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/SIPC/nagi/internal/pkg/i18n"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/store"
)

// ExecResult 是在容器里执行一条命令的结果。
type ExecResult struct {
	Output    string
	ExitCode  int32
	TimedOut  bool
	Truncated bool
}

// execOutputMax 是保留的输出上限：超出时保留开头和结尾各一半。
const execOutputMax = 64 << 10

// RunCommand 在容器里执行一条命令并等它结束，给 MCP 的 exec 工具用（见 docs/mcp.md#日常操作）。
// 权限和终端一样：能操作容器的角色、访问范围内的项目，令牌还要允许执行命令。每次执行都记进活动记录，连同命令。
func (a *API) RunCommand(ctx context.Context, projectID, container, service string, command []string, timeout time.Duration) (ExecResult, error) {
	u := auth.CurrentUser(ctx)
	start := &nagiv1.ExecStart{
		ProjectId: projectID, Container: container, Service: service, Command: command,
		Size: &nagiv1.TerminalSize{Cols: 200, Rows: 50},
	}
	// 命令本身记在详情里：活动记录要能看出 agent 在容器里做过什么。
	detail := strings.Join(command, " ")
	fail := func(err error) (ExecResult, error) {
		a.recordExec(ctx, start, detail, activityFailure(err))
		return ExecResult{}, err
	}
	switch {
	case len(command) == 0:
		return ExecResult{}, i18n.New("命令不能为空")
	case !auth.CanOperate(u.Role) || u.TokenID != 0 && !u.AllowExec:
		return fail(i18n.New("当前账号没有权限执行这个操作"))
	}
	if err := a.projectAllowed(ctx, u, projectID); err != nil {
		return fail(err)
	}
	if a.Panel.Standby != nil {
		return fail(a.standbyError())
	}
	nodeID, err := a.Projects.Locate(ctx, projectID, container)
	if err != nil {
		return fail(err)
	}
	conn, err := a.Nodes.Get(nodeID)
	if err != nil {
		return fail(i18n.New("容器所在的节点不在线"))
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	stream, err := conn.Agent.Exec(ctx)
	if err != nil {
		return fail(err)
	}
	if err := stream.Send(&nagiv1.ExecInput{Input: &nagiv1.ExecInput_Start{Start: start}}); err != nil {
		return fail(err)
	}
	var out headTail
	res := ExecResult{ExitCode: -1}
	for {
		msg, err := stream.Recv()
		if err != nil {
			if ctx.Err() != nil {
				res.TimedOut = true
			} else if !errors.Is(err, io.EOF) {
				return fail(err)
			}
			break
		}
		switch v := msg.Output.(type) {
		case *nagiv1.ExecOutput_Stdout:
			out.Write(v.Stdout)
		case *nagiv1.ExecOutput_ExitCode:
			res.ExitCode = v.ExitCode
		}
	}
	res.Output, res.Truncated = out.String()
	failure := ""
	if res.TimedOut {
		failure = "timeout"
	}
	// 命令自己失败（退出码不为 0）不算这次执行失败，但记下退出码。
	if !res.TimedOut && res.ExitCode != 0 {
		detail += " → exit " + strconv.Itoa(int(res.ExitCode))
	}
	a.recordExec(ctx, start, detail, failure)
	return res, nil
}

// recordExec 记一条执行命令的活动：详情是命令，失败时后面接原因。
func (a *API) recordExec(ctx context.Context, start *nagiv1.ExecStart, command, failure string) {
	detail := command
	if failure != "" {
		detail += ": " + failure
	}
	err := a.Q.InsertActivity(context.WithoutCancel(ctx), store.InsertActivityParams{
		CreatedAt: time.Now().Unix(), Kind: "operation", Actor: clipActivity(auth.Username(ctx), 64), Action: "ExecCommand",
		ProjectID: start.GetProjectId(), Target: clipActivity(start.GetContainer(), 200), Detail: clipActivity(detail, 500),
		Failed: boolInt(failure != ""), Via: clipActivity(auth.CurrentUser(ctx).Via, 80),
	})
	if err != nil {
		a.Log.Warn("写入活动记录失败", "err", err)
	}
}

// headTail 只留输出的开头和结尾，中间的丢掉。终端的输出里换行是 \r\n，去掉 \r。
type headTail struct {
	head, tail []byte
	dropped    bool
}

func (h *headTail) Write(p []byte) {
	p = bytes.ReplaceAll(p, []byte("\r"), nil)
	half := execOutputMax / 2
	if room := half - len(h.head); room > 0 {
		n := min(room, len(p))
		h.head = append(h.head, p[:n]...)
		p = p[n:]
	}
	h.tail = append(h.tail, p...)
	if len(h.tail) > half {
		h.tail = h.tail[len(h.tail)-half:]
		h.dropped = true
	}
}

func (h *headTail) String() (string, bool) {
	if !h.dropped {
		return string(h.head) + string(h.tail), false
	}
	return string(h.head) + "\n…（中间的输出已省略）…\n" + string(h.tail), true
}
