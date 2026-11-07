package release

import (
	"context"
	"strings"
	"time"

	"github.com/SIPC/nagi/internal/pkg/i18n"
)

// 更新日志（CHANGELOG.md、CHANGELOG.en.md）随发布文件一起上传，面板的更新弹窗从这里取（见 docs/panel.md#更新面板）。

// ChangelogName 返回界面语言对应的更新日志文件名。
func ChangelogName(lang i18n.Lang) string {
	if lang == i18n.En {
		return "CHANGELOG.en.md"
	}
	return "CHANGELOG.md"
}

// FetchChangelog 从 base 下载更新日志。
func FetchChangelog(ctx context.Context, base, name string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	b, err := fetch(ctx, base+"/"+name, 1<<20, nil)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Section 是更新日志里一个版本的说明。
type Section struct {
	Version string
	Body    string
}

// Sections 取出更新日志里比 from 新、不比 to 新的版本（「## <版本>」到下一个「## 」之前），按文件里的顺序，新的在前。
func Sections(md, from, to string) []Section {
	var out []Section
	var cur *Section
	var body []string
	flush := func() {
		if cur != nil {
			cur.Body = strings.TrimSpace(strings.Join(body, "\n"))
			out = append(out, *cur)
		}
		cur, body = nil, nil
	}
	for _, line := range strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n") {
		if rest, ok := strings.CutPrefix(line, "## "); ok {
			flush()
			f := strings.Fields(rest)
			if len(f) > 0 && IsVersion(f[0]) && Older(from, f[0]) && !Older(to, f[0]) {
				cur = &Section{Version: f[0]}
			}
			continue
		}
		if cur != nil {
			body = append(body, line)
		}
	}
	flush()
	return out
}
