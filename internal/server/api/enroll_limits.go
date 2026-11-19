package api

import (
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/SIPC/nagi/internal/pkg/i18n"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// 节点加入的限制（见 docs/control-plane.md#接入码）：节点提交的主机名、接入码的加入次数、加入通知。
// 接入码可能经别的面板（或那边被攻破的节点）转交，拿到的人能反复加入。

const (
	// maxNodeName 是节点名称的长度上限（字符数），和 RenameNode 一样。
	maxNodeName = 64
	// inviteMaxUses 是一个接入码最多能加入的次数。接入码 1 小时内有效，相当于每小时 20 次；
	// 移交节点时一次最多选这么多台（HandoverNodes 检查）。
	inviteMaxUses = 20
	// enrollAlertWindow 是同一个接入码的加入通知合并的时长：第一次加入立即通知，之后这段时间内的合成一条。
	enrollAlertWindow = 10 * time.Minute
	// enrollAlertNames 是合并的通知里最多列出的节点数。
	enrollAlertNames = 10
)

// invisibleRune 判断字符是不是不显示出来、或者会改变显示方式的：控制字符、格式字符（零宽空格、双向控制符）、
// 行和段分隔符、私用区和代理区的字符、变体选择符、其他默认不显示的字符（韩文填充符之类）、盲文空格。
// 节点名称里不能有：带着它们的名字和另一个名字看起来一样，却不算重名。
func invisibleRune(r rune) bool {
	return unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp, unicode.Co, unicode.Cs,
		unicode.Variation_Selector, unicode.Other_Default_Ignorable_Code_Point) || r == '\u2800' || r == utf8.RuneError
}

// plainSpace 把各种空格（不换行空格、全角空格等）换成普通空格，它们看起来一样。
func plainSpace(r rune) rune {
	if unicode.Is(unicode.Zs, r) {
		return ' '
	}
	return r
}

// enrollName 把节点加入时提交的主机名变成节点名称：去掉控制字符和不可见字符，各种空格换成普通空格，
// 最多 64 个字符（和 RenameNode 的规则一样）；是空的用节点 ID。和别的节点重名时加后缀 -2、-3……，不拒绝，免得正常的加入失败。
func enrollName(hostname, nodeID string, taken func(string) bool) string {
	name := strings.TrimSpace(strings.Map(func(r rune) rune {
		if invisibleRune(r) {
			return -1
		}
		return plainSpace(r)
	}, hostname))
	name = truncateRunes(name, maxNodeName)
	if name == "" {
		name = nodeID
	}
	if !taken(name) {
		return name
	}
	for i := 2; ; i++ {
		suffix := "-" + strconv.Itoa(i)
		candidate := strings.TrimSpace(truncateRunes(name, maxNodeName-len(suffix))) + suffix
		if !taken(candidate) {
			return candidate
		}
	}
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// enrollAlerts 合并同一个接入码的加入通知：第一次加入照常立即通知，之后 enrollAlertWindow 内的加入只计数，
// 到时发一条汇总（还有几个节点、它们的名称和来源地址）。审计里每次加入照样各记一条。
type enrollAlerts struct {
	mu     sync.Mutex
	bursts map[string]*enrollBurst
	// window 是合并的时长，测试里改短；为 0 时用 enrollAlertWindow。
	window time.Duration
}

type enrollBurst struct {
	count int
	names []string
}

// add 登记一次用接入码 key 的加入。返回 true 时由调用方立即发这次的通知；返回 false 表示已经合并，
// 窗口结束时如果合并了加入，调用 flush(合并的次数, 列出的节点)。
func (e *enrollAlerts) add(key, node string, flush func(count int, names []string)) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if b, ok := e.bursts[key]; ok {
		b.count++
		if len(b.names) < enrollAlertNames {
			b.names = append(b.names, node)
		}
		return false
	}
	if e.bursts == nil {
		e.bursts = map[string]*enrollBurst{}
	}
	e.bursts[key] = &enrollBurst{}
	window := e.window
	if window == 0 {
		window = enrollAlertWindow
	}
	time.AfterFunc(window, func() {
		e.mu.Lock()
		b := e.bursts[key]
		delete(e.bursts, key)
		e.mu.Unlock()
		if b != nil && b.count > 0 {
			flush(b.count, b.names)
		}
	})
	return true
}

// inviteAlert 是合并后的加入通知：同一个接入码在窗口内又加入了 count 个节点，列出其中前几个的名称和来源地址。
func inviteAlert(tokenHash string, count int, names []string) *nagiv1.Alert {
	list := strings.Join(names, "、")
	if count > len(names) {
		list += "……"
	}
	return &nagiv1.Alert{Key: "security:enroll-invite:" + tokenHash[:min(12, len(tokenHash))],
		Title:   i18n.Errorf("又有 %d 个节点用同一个接入码加入", count).Error(),
		Message: i18n.Errorf("节点：%s", list).Error(), Time: timestamppb.Now()}
}
