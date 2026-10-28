package i18n

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// stub 无论用什么动词格式化都输出 <序号>，用来检查译文的参数和中文一致。
type stub int

func (s stub) Format(f fmt.State, _ rune) { fmt.Fprintf(f, "<%d>", int(s)) }

// verbCount 是格式串里参数的个数（不算 %%）。
func verbCount(format string) int {
	n := 0
	for i := 0; i < len(format); i++ {
		if format[i] != '%' {
			continue
		}
		if i+1 < len(format) && format[i+1] == '%' {
			i++
			continue
		}
		n++
	}
	return n
}

// 格式串和 New 的整段文字放在同一个表里，New 的文字里不会有格式动词。
func TestCatalogArgs(t *testing.T) {
	for zh, tr := range en {
		if strings.TrimSpace(tr) == "" {
			t.Errorf("%q 的译文是空的", zh)
			continue
		}
		if regexp.MustCompile(`\p{Han}`).MatchString(tr) {
			t.Errorf("%q 的译文里还有中文：%q", zh, tr)
		}
		n := verbCount(zh)
		args := make([]any, n)
		for i := range args {
			args[i] = stub(i)
		}
		out := fmt.Sprintf(strings.ReplaceAll(tr, "%w", "%v"), args...)
		if strings.Contains(out, "%!") {
			t.Errorf("%q 的译文参数和中文不一致：%q → %q", zh, tr, out)
			continue
		}
		for i := 0; i < n; i++ {
			if !strings.Contains(out, fmt.Sprintf("<%d>", i)) {
				t.Errorf("%q 的译文没有用到第 %d 个参数：%q", zh, i+1, tr)
			}
		}
	}
}

// scanned 是要求文案都有译文的目录（相对仓库根目录）。
var scanned = []string{"internal/server", "internal/pkg"}

// textArg 是会显示给用户的函数及其文案参数的位置。
var textArg = map[string]int{
	"i18n.Errorf":  0,
	"i18n.New":     0,
	"i18n.T":       1,
	"i18n.Sprintf": 1,
	"spec.Invalid": 0,
	"Invalid":      0, // spec 包里
	"invalid":      0, // projects 包里，等同 spec.Invalid
}

var han = regexp.MustCompile(`\p{Han}`)

// TestCatalogCoverage 检查代码里所有中文文案都有英文译文。
func TestCatalogCoverage(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	fset := token.NewFileSet()
	for _, dir := range scanned {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				idx, ok := textArg[funcName(call.Fun)]
				if !ok || idx >= len(call.Args) {
					return true
				}
				s, ok := stringConst(call.Args[idx])
				if !ok || !han.MatchString(s) {
					return true
				}
				if _, ok := en[s]; !ok {
					t.Errorf("%s：缺少英文译文：%q", fset.Position(call.Pos()), s)
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func funcName(e ast.Expr) string {
	switch f := e.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		if x, ok := f.X.(*ast.Ident); ok {
			return x.Name + "." + f.Sel.Name
		}
	}
	return ""
}

// stringConst 取字符串字面量（包括用 + 连起来的几段）。
func stringConst(e ast.Expr) (string, bool) {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(v.Value)
		return s, err == nil
	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			return "", false
		}
		a, ok1 := stringConst(v.X)
		b, ok2 := stringConst(v.Y)
		return a + b, ok1 && ok2
	case *ast.ParenExpr:
		return stringConst(v.X)
	}
	return "", false
}
