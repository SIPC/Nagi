package audit

import (
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// 录制输入（见 docs/audit.md#终端与命令录制）：浏览器发来的按键记成 asciicast 的 "i" 事件。
// 面板看不到终端的回显状态（容器终端的 pty 在容器里），所以按回显判断：敲的字符紧接着出现在输出里，
// 说明终端在回显，照录；echoWait 内没有回显（输密码时），只记「未回显的 N 个字符」（一串 •），不留内容。
// 回车、退格、方向键、Ctrl 组合这类控制键不含内容，照录成名称。

const (
	echoWait = 2 * time.Second
	// enterWait：按了回车以后，前面还在等回显的字符最多再等这么久（远程登录时回显会晚一点到）。
	// 输密码按回车以后，后面敲的字符不会被当成密码的回显。
	enterWait   = 500 * time.Millisecond
	maxTypedRun = 4096
	// hiddenMark 是未回显字符的占位：回放时显示成「未回显 N 个字符」。
	hiddenMark = "•"
)

type typedRun struct {
	text     string
	at       time.Time
	deadline time.Time
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)|\x1b[@-Z\\-_]`)

// keyNames 是常见控制键的名称。
var keyNames = map[string]string{
	"\r": "⏎", "\n": "⏎", "\t": "⇥", "\x7f": "⌫", "\b": "⌫", "\x1b": "Esc",
	"\x1b[A": "↑", "\x1b[B": "↓", "\x1b[C": "→", "\x1b[D": "←",
	"\x1bOA": "↑", "\x1bOB": "↓", "\x1bOC": "→", "\x1bOD": "←",
	"\x1b[H": "Home", "\x1b[F": "End", "\x1b[3~": "Del", "\x1b[5~": "PgUp", "\x1b[6~": "PgDn",
}

// splitKeys 把一段输入分成控制键和可见字符。括号粘贴的起止标记去掉。
func splitKeys(s string) (tokens []string, printable []bool) {
	s = strings.NewReplacer("\x1b[200~", "", "\x1b[201~", "").Replace(s)
	var run strings.Builder
	flush := func() {
		if run.Len() > 0 {
			tokens, printable = append(tokens, run.String()), append(printable, true)
			run.Reset()
		}
	}
	for len(s) > 0 {
		c := s[0]
		switch {
		case c == 0x1b:
			flush()
			n := escLen(s)
			seq := s[:n]
			name := keyNames[seq]
			if name == "" {
				name = "Esc"
			}
			tokens, printable = append(tokens, name), append(printable, false)
			s = s[n:]
		case c < 0x20 || c == 0x7f:
			flush()
			name := keyNames[string(c)]
			if name == "" {
				name = "^" + string(rune(c+'@'))
			}
			tokens, printable = append(tokens, name), append(printable, false)
			s = s[1:]
		default:
			r, n := utf8.DecodeRuneInString(s)
			if r != utf8.RuneError || n > 1 {
				run.WriteString(s[:n])
			}
			s = s[n:]
		}
	}
	flush()
	return tokens, printable
}

// escLen 返回 s 开头的转义序列的长度（CSI、SS3，或者单独的 Esc 加一个字符）。
func escLen(s string) int {
	if len(s) < 2 {
		return 1
	}
	switch s[1] {
	case '[':
		for i := 2; i < len(s); i++ {
			if s[i] >= 0x40 && s[i] <= 0x7e {
				return i + 1
			}
		}
		return len(s)
	case 'O':
		return min(3, len(s))
	}
	return 1
}

// Input 记下浏览器发来的一段按键。
func (rec *Recorder) Input(b []byte) {
	if rec == nil || len(b) == 0 {
		return
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.closed {
		return
	}
	now := time.Now()
	rec.resolveStaleLocked(now)
	tokens, printable := splitKeys(string(b))
	// 每条消息里的可见字符单独等回显（浏览器一般一个按键一条，粘贴是一整条）：打字比回显快时，
	// 一个个回显也能对上。
	for i, tok := range tokens {
		if !printable[i] {
			if tok == "⏎" {
				for j := range rec.typed {
					rec.typed[j].deadline = minTime(rec.typed[j].deadline, now.Add(enterWait))
				}
			}
			rec.inputEventAt(now, tok)
			continue
		}
		rec.typed = append(rec.typed, typedRun{text: truncRunes(tok, maxTypedRun), at: now, deadline: now.Add(echoWait)})
	}
}

// matchEchoLocked 在记下一段输出前调用：等着回显的字符出现在输出里，就照录。
func (rec *Recorder) matchEchoLocked(out string, now time.Time) {
	if len(rec.typed) == 0 {
		return
	}
	rec.resolveStaleLocked(now)
	plain := ansiRe.ReplaceAllString(out, "")
	// 按顺序找：前面没回显的（正在输密码）不挡住后面的回显。
	pos := 0
	keep := rec.typed[:0]
	for _, t := range rec.typed {
		i := strings.Index(plain[pos:], t.text)
		if i < 0 {
			keep = append(keep, t)
			continue
		}
		pos += i + len(t.text)
		rec.inputEventAt(t.at, t.text)
	}
	rec.typed = keep
}

// resolveStaleLocked 把等回显超时的字符记成未回显。
func (rec *Recorder) resolveStaleLocked(now time.Time) {
	n := 0
	for n < len(rec.typed) && now.After(rec.typed[n].deadline) {
		n++
	}
	rec.hideLocked(rec.typed[:n])
	rec.typed = rec.typed[n:]
}

func (rec *Recorder) hideAllLocked() {
	rec.hideLocked(rec.typed)
	rec.typed = nil
}

// hideLocked 把一串没有回显的输入记成一个事件（一个个敲的密码合成一条「N 个字符」）。
func (rec *Recorder) hideLocked(runs []typedRun) {
	if len(runs) == 0 {
		return
	}
	n := 0
	for _, t := range runs {
		n += utf8.RuneCountInString(t.text)
	}
	rec.inputEventAt(runs[0].at, strings.Repeat(hiddenMark, n))
}

func (rec *Recorder) inputEventAt(at time.Time, data string) {
	rec.eventAt(at, "i", data)
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func truncRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
