package mcp

import (
	"fmt"
	"slices"

	"github.com/SIPC/nagi/internal/pkg/i18n"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// 工具自己的报错里提到节点时用 nodeErrorf：返回给客户端的是节点名称，记进审计的是带节点 ID 的引用
// （见 def 里的 notes.Mark），查看记录的成员看不到这台节点时显示「其他节点」。直接把名称拼进 fmt.Errorf 的话，
// 记录里就只有名称，认不出是哪台。

// nodeRef 是报错里对一个节点的引用。
func nodeRef(n *nagiv1.Node) i18n.Ref { return i18n.NodeRef(n.GetId(), n.GetName()) }

// nodeErrorf 和 fmt.Errorf 一样，参数里的节点传 nodeRef。
func nodeErrorf(format string, args ...any) error { return &refError{format: format, args: args} }

type refError struct {
	format string
	args   []any
}

func (e *refError) Error() string { return fmt.Sprintf(e.format, e.args...) }

// Redact 实现 i18n.Redactor：把参数里的引用换成 hide 给的文字。
func (e *refError) Redact(hide func(i18n.Ref) (string, bool)) (error, bool) {
	args := slices.Clone(e.args)
	changed := false
	for i, a := range args {
		if r, ok := a.(i18n.Ref); ok {
			if s, ok := hide(r); ok {
				args[i], changed = s, true
			}
		}
	}
	if !changed {
		return e, false
	}
	return &refError{format: e.format, args: args}, true
}
