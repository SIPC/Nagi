package recording

import (
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// 录制输入（见 docs/audit.md#终端与命令录制）：浏览器发来的按键记成 asciicast 的 "i" 事件。
// 面板看不到终端的回显状态（容器终端的 pty 在容器里），所以按回显判断，而且判断得很严：宁可把有回显的
// 当成没有回显（只记个数），也不能把密码当成回显录下来。
//   - 回车之前：敲的字符要由紧接着的输出原样开头（去掉颜色等控制序列以后）才算回显。
//     中间有别的输出、或者输出开头对不上，就算没有回显。一个个敲的字符还要求这段输出正好就是它们的回显：
//     开头对上、后面还有别的（例如先敲了密码、提示符才出来），也算没有回显。
//     粘贴的一整段可以只是输出的开头，也可以分几段回显。
//   - 按回车时：还在等回显的字符合成一整串，要由回车之后的输出原样开头、并且紧跟着换行（回车本身的回显）
//     才算回显（粘贴一条命令再回车就是这样）。输密码时回显关着，回车后的输出是程序自己打印的换行、
//     「Sorry, try again」或新的提示符，开头对不上，整串记成未回显。
//   - 超时（回车前 echoWait、回车后 enterWait）还没有输出，也算没有回显。
// 没有回显的只记「未回显的 N 个字符」（一串 •），不留内容。回车、退格、方向键、Ctrl 组合这类控制键照录成名称。

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
	// entered：按过回车的一整串，要在回车后的输出开头、并且紧跟着换行才算回显。
	entered bool
	// needNewline：这一串已经在上一段输出的末尾对上了，还要下一段输出以换行开头。
	needNewline bool
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
				rec.enterLocked(now)
			}
			rec.inputEventAt(now, tok)
			continue
		}
		rec.typed = append(rec.typed, typedRun{text: truncRunes(tok, maxTypedRun), at: now, deadline: now.Add(echoWait)})
	}
}

// enterLocked 在按回车时调用：还在等回显、没按过回车的字符合成一整串（见文件开头）。
func (rec *Recorder) enterLocked(now time.Time) {
	var open []typedRun
	keep := rec.typed[:0]
	for _, t := range rec.typed {
		if t.entered {
			keep = append(keep, t)
		} else {
			open = append(open, t)
		}
	}
	rec.typed = keep
	if len(open) == 0 {
		return
	}
	var b strings.Builder
	for _, t := range open {
		b.WriteString(t.text)
	}
	rec.typed = append(rec.typed, typedRun{text: truncRunes(b.String(), maxTypedRun), at: open[0].at, deadline: now.Add(enterWait), entered: true})
}

// matchEchoLocked 在记下一段输出前调用：按文件开头的规则，对上的照录，对不上的记成未回显。
func (rec *Recorder) matchEchoLocked(out string, now time.Time) {
	if len(rec.typed) == 0 {
		return
	}
	rec.resolveStaleLocked(now)
	rest := ansiRe.ReplaceAllString(out, "")
	if rest == "" {
		// 只有光标移动、颜色这类控制序列：还没有回显，接着等。
		return
	}
	for len(rec.typed) > 0 && rest != "" {
		t := &rec.typed[0]
		switch {
		case t.needNewline:
			if !startsWithNewline(rest) {
				rec.hideLocked(rec.typed[:1])
				rec.typed = rec.typed[1:]
				continue
			}
			rec.inputEventAt(t.at, t.text)
			rec.typed = rec.typed[1:]
		case t.entered:
			after, ok := strings.CutPrefix(rest, t.text)
			switch {
			case !ok:
				rec.hideLocked(rec.typed[:1])
				rec.typed = rec.typed[1:]
			case after == "":
				// 换行可能在下一段输出里。
				t.needNewline = true
				return
			case startsWithNewline(after):
				rec.inputEventAt(t.at, t.text)
				rec.typed = rec.typed[1:]
				rest = after
			default:
				rec.hideLocked(rec.typed[:1])
				rec.typed = rec.typed[1:]
			}
		default:
			// 从 t 起，连续对上的都算进这一段（打字比回显快时，几个字符的回显在同一段输出里）。
			n, after := 0, rest
			single := true
			for n < len(rec.typed) && !rec.typed[n].entered && !rec.typed[n].needNewline && after != "" {
				a, ok := strings.CutPrefix(after, rec.typed[n].text)
				if !ok {
					break
				}
				if utf8.RuneCountInString(rec.typed[n].text) > 1 {
					single = false
				}
				n, after = n+1, a
			}
			if n > 0 {
				// 一个个敲的字符：输出（去掉控制序列以后）必须正好是它们的回显，后面不能还有别的。输密码时回显关着，
				// 这时恰好到达的输出（后来才打印的提示符「Password: 」、后台日志）开头对上了，不能当成回显。
				// 粘贴的一整段不受这个限制。
				if single && after != "" {
					rec.hideLocked(rec.typed[:n])
				} else {
					for _, e := range rec.typed[:n] {
						rec.inputEventAt(e.at, e.text)
					}
				}
				rec.typed = rec.typed[n:]
				rest = after
				continue
			}
			if utf8.RuneCountInString(t.text) > 1 && strings.HasPrefix(t.text, rest) {
				// 回显被拆成了几段输出（粘贴的长命令）：先录对上的部分，剩下的接着等。
				rec.inputEventAt(t.at, rest)
				t.text = t.text[len(rest):]
				return
			}
			// 输出开头对不上：这个和后面还在等的都算没有回显。
			rec.hideLocked(rec.typed)
			rec.typed = nil
			return
		}
	}
}

func startsWithNewline(s string) bool {
	return strings.HasPrefix(s, "\r") || strings.HasPrefix(s, "\n")
}

// resolveStaleLocked 把等回显超时的字符记成未回显。
func (rec *Recorder) resolveStaleLocked(now time.Time) {
	keep := rec.typed[:0]
	var stale []typedRun
	for _, t := range rec.typed {
		if now.After(t.deadline) {
			stale = append(stale, t)
		} else {
			keep = append(keep, t)
		}
	}
	rec.hideLocked(stale)
	rec.typed = keep
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

func truncRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
