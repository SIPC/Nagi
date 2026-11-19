package api

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// 看不见的字符（零宽空格、双向控制符、变体选择符、韩文填充符等）和各种空格不能让名字绕过重名检查：
// 加入时去掉，改名时拒绝。
func TestNodeNameInvisibleRunes(t *testing.T) {
	names := map[string]bool{"web-1": true, "web 1": true}
	taken := func(s string) bool { return names[s] }
	for _, c := range []struct{ in, want string }{
		{"web-1\u200b", "web-1-2"},       // 零宽空格
		{"\ufeffweb-1", "web-1-2"},       // 字节序标记
		{"web\u200d-1", "web-1-2"},       // 零宽连接符
		{"web-1\u202e", "web-1-2"},       // 从右到左覆盖
		{"\u2066web-1\u2069", "web-1-2"}, // 双向隔离
		{"web-1\ufe0f", "web-1-2"},       // 变体选择符
		{"web-1\u3164", "web-1-2"},       // 韩文填充符
		{"web-1\u034f", "web-1-2"},       // 组合用字形连接符
		{"web-1\u2800", "web-1-2"},       // 盲文空格
		{"web-1\u2028", "web-1-2"},       // 行分隔符
		{"web-1\U000e0041", "web-1-2"},   // 标签字符
		{"web-1\ue000", "web-1-2"},       // 私用区
		{"web\u00a01", "web 1-2"},        // 不换行空格
		{"web\u30001", "web 1-2"},        // 全角空格
		{"\u202edb", "db"},
	} {
		if got := enrollName(c.in, "abcd1234", taken); got != c.want {
			t.Errorf("enrollName(%+q) = %+q，想要 %+q", c.in, got, c.want)
		}
	}

	// 改名：带这些字符的名称在查库之前就拒绝。
	svc := &nodeService{&API{}}
	for _, name := range []string{"web-1\u200b", "web-1\u202e", "\ufeffweb-1", "web-1\ufe0f", "web-1\u3164", "we\x1bb"} {
		_, err := svc.RenameNode(context.Background(), connect.NewRequest(&nagiv1.RenameNodeRequest{NodeId: "n", Name: name}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("RenameNode(%+q) 应拒绝：%v", name, err)
		}
	}
}
