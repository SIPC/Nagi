package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/SIPC/nagi/internal/pkg/i18n"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/audit"
	"github.com/SIPC/nagi/internal/server/auth"
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
	var rec *sessionRecording
	start := &nagiv1.ExecStart{
		ProjectId: projectID, Container: container, Service: service, Command: command,
		Size: &nagiv1.TerminalSize{Cols: 200, Rows: 50},
	}
	// 命令本身记在详情里：活动记录要能看出 agent 在容器里做过什么。
	detail := strings.Join(command, " ")
	fail := func(err error) (ExecResult, error) {
		a.recordExec(ctx, start, detail, auditFailure(err), rec.Close())
		// 这里不经过 scopeCheck：返回给成员前同样把他看不到的节点换掉（容器可能在范围外的节点上）。
		return ExecResult{}, a.scrubFor(ctx, u, i18n.FromContext(ctx), err)
	}
	switch {
	case len(command) == 0:
		return ExecResult{}, i18n.New("命令不能为空")
	case u.TokenID != 0 && !u.AllowExec:
		return fail(errNoPermission)
	}
	if err := a.projectAllowed(ctx, u, projectID, auth.PermTerminal); err != nil {
		return fail(err)
	}
	if err := a.checkPrivilegedEntry(ctx, u, projectID, container); err != nil {
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
		return fail(err)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ctx = a.recordingContext(ctx)
	stream, err := conn.Agent.Exec(ctx)
	if err != nil {
		return fail(err)
	}
	if err := stream.Send(&nagiv1.ExecInput{Input: &nagiv1.ExecInput_Start{Start: start}}); err != nil {
		return fail(err)
	}
	var out headTail
	res := ExecResult{ExitCode: -1}
	// 录制的是完整的输出流（开头 8 MB、最后 56 MB），返回给 agent 的只有开头和结尾；开头先写上命令。
	rec = a.startSessionRecording(ctx, stream, nodeID, 200, 50)
	rec.Output([]byte(commandLine("", detail)))
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
			rec.Output(crlf(v.Stdout))
		case *nagiv1.ExecOutput_ExitCode:
			res.ExitCode = v.ExitCode
		}
	}
	rec.Output([]byte(exitLine(res.ExitCode, res.TimedOut)))
	res.Output, res.Truncated = out.String()
	failure := ""
	if res.TimedOut {
		failure = "timeout"
	}
	// 命令自己失败（退出码不为 0）不算这次执行失败，但记下退出码。
	if !res.TimedOut && res.ExitCode != 0 {
		detail += " → exit " + strconv.Itoa(int(res.ExitCode))
	}
	a.recordExec(ctx, start, detail, failure, rec.Close())
	return res, nil
}

// commandLine 是录制开头的命令行（灰色的提示符），cwd 不为空时先写一行目录。
func commandLine(cwd, command string) string {
	s := ""
	if cwd != "" {
		s = "\x1b[2m# " + cwd + "\x1b[0m\r\n"
	}
	return s + "\x1b[2m$\x1b[0m " + strings.ReplaceAll(command, "\n", "\r\n") + "\r\n"
}

// exitLine 是录制结尾的退出码。
func exitLine(code int32, timedOut bool) string {
	if timedOut {
		return "\r\n\x1b[2m[超时，已停止]\x1b[0m\r\n"
	}
	return fmt.Sprintf("\r\n\x1b[2m[退出码 %d]\x1b[0m\r\n", code)
}

// crlf 把单独的换行换成回车换行：不在终端里执行的命令输出只有 \n，回放时才不会错位。
func crlf(b []byte) []byte {
	return bytes.ReplaceAll(bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n")), []byte("\n"), []byte("\r\n"))
}

// RecordMCPDenied 记一条被拒绝的 MCP 工具调用（见 docs/mcp.md#安全）：令牌没有这个工具的权限。
func (a *API) RecordMCPDenied(ctx context.Context, u auth.User, tool, reason string) {
	a.Record(auth.WithUser(ctx, u), audit.Event{
		Category: audit.MCP, Action: "mcp:" + tool, TargetType: "tool", TargetID: tool, TargetName: tool,
		Result: audit.ResultDenied, Error: reason,
	})
}

// RecordMCPCall 记一次 MCP 工具调用，再把它触发的接口调用挂在下面（见 docs/audit.md#mcp-调用）。
// project 是工具参数里的项目，可能是名称也可能是 ID。只认调用者访问范围内的项目：参数是调用者自己写的，
// 按全部项目找的话，写一个范围外的项目名称，这条记录就会挂到那个项目上，出现在它的活动记录里。
// 和 MCP 找项目的办法一样：ID、网络名优先，再按名称（不分大小写）；范围内有几个项目同名时不猜，不挂到项目上。
func (a *API) RecordMCPCall(ctx context.Context, u auth.User, e audit.Event, project string, children *audit.Collector) {
	ctx = auth.WithUser(context.WithoutCancel(ctx), u)
	if project != "" {
		e.ProjectID = a.mcpProject(ctx, u, project)
	}
	auditActor(ctx, &e)
	a.Audit.RecordWithChildren(ctx, e, children)
}

// mcpProject 找出 MCP 调用参数里的项目（名称或 ID）在调用者范围内对应的项目 ID，找不到时为空。
func (a *API) mcpProject(ctx context.Context, u auth.User, ref string) string {
	x, err := a.loadAccess(ctx, u)
	if err != nil {
		return ""
	}
	rows, err := a.Q.ListProjects(ctx)
	if err != nil {
		return ""
	}
	byName := ""
	named := 0
	for _, p := range rows {
		if !x.project(p.ID) {
			continue
		}
		if p.ID == ref || p.NetworkName == ref {
			return p.ID
		}
		if strings.EqualFold(p.Name, ref) {
			byName = p.ID
			named++
		}
	}
	if named != 1 {
		return ""
	}
	return byName
}

// recordSessionEnd 记下终端会话的结束：时长、录制和各块的哈希（见 docs/audit.md#终端与命令录制）。
func (a *API) recordSessionEnd(ctx context.Context, action, targetType, targetID, projectID string, rec audit.Summary) {
	ctx = context.WithoutCancel(ctx)
	e := audit.Event{Category: audit.Session, Action: action, ProjectID: projectID, TargetType: targetType, TargetID: targetID,
		TargetName: targetID, RecordingID: rec.ID}
	if targetType == "node" {
		e.TargetName = a.nodeName(ctx, targetID)
	}
	if rec.ID != "" {
		b, _ := json.Marshal(map[string]any{"recording": rec})
		e.Params = string(b)
		e.DurationMS = rec.Seconds * 1000
	}
	a.Record(ctx, e)
}

// recordExec 记一条执行命令的活动：详情是命令，失败时后面接原因。
func (a *API) recordExec(ctx context.Context, start *nagiv1.ExecStart, command, failure string, rec audit.Summary) {
	result := audit.ResultOK
	if failure != "" {
		result = audit.ResultFailed
	}
	p := map[string]any{"service": start.GetService(), "command": command}
	// 录制（见 docs/audit.md#终端与命令录制）的摘要进入哈希链。
	if rec.ID != "" {
		p["recording"] = rec
	}
	params, _ := json.Marshal(p)
	a.Record(ctx, audit.Event{
		Category: audit.Session, Action: "ExecCommand", ProjectID: start.GetProjectId(),
		TargetType: "container", TargetID: start.GetContainer(), TargetName: start.GetContainer(),
		Params: string(params), Result: result, Error: failure, RecordingID: rec.ID,
	})
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
