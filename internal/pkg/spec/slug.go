package spec

import (
	"strings"
	"unicode"

	"github.com/mozillazg/go-pinyin"
)

// Slug 把名称转成小写字母、数字和 - 组成的短名（不超过 32 个字符），用作网络名和容器目录名的默认值。
// 汉字转成不带声调的拼音，每个字之间用 - 隔开（"我的博客" → "wo-de-bo-ke"）；其他字符变成 -。
// 名称里没有能用的字符时返回空。
func Slug(name string) string {
	args := pinyin.NewArgs()
	var b strings.Builder
	sep := func() {
		if b.Len() > 0 && !strings.HasSuffix(b.String(), "-") {
			b.WriteByte('-')
		}
	}
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
		case unicode.Is(unicode.Han, r):
			if py := pinyin.SinglePinyin(r, args); len(py) > 0 && py[0] != "" {
				sep()
				b.WriteString(py[0])
				b.WriteByte('-')
			}
		default:
			sep()
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 32 {
		out = strings.Trim(out[:32], "-")
	}
	return out
}
