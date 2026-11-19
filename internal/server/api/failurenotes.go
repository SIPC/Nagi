package api

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/SIPC/nagi/internal/pkg/i18n"
)

// MCP 调用的失败详情（见 docs/audit.md#mcp-调用）。工具在进程内经 Connect 客户端调接口，拿到的报错只剩文字：
// 节点的引用（i18n.NodeRef）过不了这一层，成员调用时文字里的节点还已经换成了「其他节点」。记录里要的是带引用标记的
// 那一份（显示时按查看的人换成名称或「其他节点」，见 auditFailure），所以接口这边把每次失败「返回的文字 → 带标记的
// 文字」记在 ctx 里的 FailureNotes 上（进程内调用时 ctx 是同一个），工具失败后按返回的文字找回带标记的。
// 只按整段报错对：成员自己写进报错里的词不会被当成节点。

// FailureNotes 收集一次工具调用期间接口返回的失败。
type FailureNotes struct {
	mu    sync.Mutex
	notes []failureNote
}

type failureNote struct {
	// text 是返回给调用者的文字（按他的语言，可能是任一种，都记下）。
	text string
	// marked 是带引用标记的文字。
	marked string
}

type failureNotesKey struct{}

// WithFailureNotes 返回带收集器的 ctx。
func WithFailureNotes(ctx context.Context) (context.Context, *FailureNotes) {
	n := &FailureNotes{}
	return context.WithValue(ctx, failureNotesKey{}, n), n
}

// noteFailure 记下一次失败：returned 是返回给调用者的报错（成员的已经换过节点名），raw 是原来的。
// 报错里没有引用时不用记。
func noteFailure(ctx context.Context, returned, raw error) {
	n, ok := ctx.Value(failureNotesKey{}).(*FailureNotes)
	if !ok || returned == nil || raw == nil {
		return
	}
	marked := auditFailure(raw)
	if marked == i18n.PlainRefs(marked) {
		return
	}
	for _, lang := range []i18n.Lang{i18n.Zh, i18n.En} {
		n.add(errorText(lang, returned), marked)
	}
}

// noteFailureText 同 noteFailure，返回给调用者的已经是文字（流式操作在结果里返回的失败）。
func noteFailureText(ctx context.Context, text string, raw error) {
	n, ok := ctx.Value(failureNotesKey{}).(*FailureNotes)
	if !ok || text == "" || raw == nil {
		return
	}
	if marked := auditFailure(raw); marked != i18n.PlainRefs(marked) {
		n.add(text, marked)
	}
}

func (n *FailureNotes) add(text, marked string) {
	// 和记录时一样先去掉标记字符（见 Mark），太短的不记：免得别的文字里碰巧有同样的几个字。
	text = i18n.Marked(func(func(i18n.Ref) (string, bool)) string { return text })
	if len(text) < 4 {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, x := range n.notes {
		if x.text == text {
			return
		}
	}
	n.notes = append(n.notes, failureNote{text: text, marked: marked})
}

// Mark 把工具的报错拼成记进审计的失败详情：报错自己带着的引用写成标记，其中接口返回过的报错换成带标记的那一份。
func (n *FailureNotes) Mark(err error) string {
	if err == nil {
		return ""
	}
	marked := auditFailure(err)
	// 先按文字找接口返回过的报错：直接调面板入口的工具（执行命令、卷文件）拿到的报错自己也带引用，
	// 但成员调用时引用里的名称已经换成「其他节点」，接口记下的那一份才是原来的。
	if out, ok := n.apply(i18n.PlainRefs(marked)); ok {
		return out
	}
	return marked
}

// MarkText 同 Mark，失败写在工具的结果里（部署这类工具的 error 字段），只有文字。
func (n *FailureNotes) MarkText(text string) string {
	out, _ := n.apply(i18n.Marked(func(func(i18n.Ref) (string, bool)) string { return text }))
	return out
}

// apply 把 s（没有标记的文字）里接口返回过的报错换成带标记的，返回换过的文字和有没有换。
func (n *FailureNotes) apply(s string) (string, bool) {
	if n == nil || s == "" {
		return s, false
	}
	n.mu.Lock()
	notes := append([]failureNote(nil), n.notes...)
	n.mu.Unlock()
	// 长的先换：短的可能是长的里面的一段。
	sort.SliceStable(notes, func(i, j int) bool { return len(notes[i].text) > len(notes[j].text) })
	// 换上去的内容用占位符顶着，全部换完再填回，后面的不会在已经换上的文字里再找。
	s = strings.ReplaceAll(s, "\x00", "")
	hold := func(i int) string { return "\x00" + strconv.Itoa(i) + "\x00" }
	var done []string
	for _, x := range notes {
		if strings.Contains(s, x.text) {
			s = strings.ReplaceAll(s, x.text, hold(len(done)))
			done = append(done, x.marked)
		}
	}
	for i, marked := range done {
		s = strings.ReplaceAll(s, hold(i), marked)
	}
	return s, len(done) > 0
}
