package spec

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"strings"

	"github.com/compose-spec/compose-go/v2/template"
	"github.com/docker/go-connections/nat"
	"gopkg.in/yaml.v3"
)

// MaxComposePorts 是一份 compose（一个容器）里 ports 和 expose 展开范围后最多的端口数。
// compose 的每个端口部署时都导入成一个端口映射（Caddy 上一个监听），跨节点的代理也按声明的端口逐个监听，
// 所以几十个端口的游戏服务器、FTP 被动端口段都够用；上万个端口的媒体端口段（RTP、TURN）在 Nagi 里本来就用不了，
// 这类服务用 network_mode: host（见 docs/agent.md#compose-的限制）。
const MaxComposePorts = 1000

// CheckComposePorts 在交给 compose 解析库之前，按原文估算 ports 和 expose 展开范围后的端口数，超过 MaxComposePorts 时拒绝。
// 解析库会把 "1-65535:1-65535" 这样的范围逐个展开，host_ip 不同的范围不会去重，几百字节的 compose 就能展开出上百万个端口，
// 耗尽面板的 CPU 和内存。所以 LoadCompose、composeModel（PrivilegeReasons、AdviseCompose）都先过这一步。
//
// 只读不改：用 yaml.v3 读出每个文档，任何位置的 ports、expose 键下的列表都算（锚点、合并键、!!merge 写法引用到的
// 每一处都算一次），同一个文件里 extends 继承来的端口也加上。估算只会偏多：compose 会去掉重复的端口。
// env 不为 nil 时先按它替换变量（和 LoadCompose 一样），为 nil 时按原文（composeModel 不替换变量）。
// 原文 YAML 读不了时返回错误：compose 解析库读得了、这里读不了的写法不能绕过检查。
func CheckComposePorts(text string, env map[string]string) error {
	c := portCounter{env: env, memo: map[*yaml.Node]int{}, lists: map[*yaml.Node]int{}, busy: map[*yaml.Node]bool{}}
	services := map[string]*serviceCount{}
	dec := yaml.NewDecoder(bytes.NewReader([]byte(text)))
	total := 0
	for {
		var doc yaml.Node
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		total = capPorts(total + c.count(&doc))
		c.collectServices(&doc, services)
	}
	for name := range services {
		total = capPorts(total + inherited(name, services, map[string]bool{}))
	}
	if total > MaxComposePorts {
		return Invalid("compose 的端口过多：ports 和 expose 展开范围后最多 %d 个", MaxComposePorts)
	}
	return nil
}

// capPorts 让计数停在上限之外一点，锚点层层引用时数字也不会溢出。
func capPorts(n int) int { return min(n, MaxComposePorts+1) }

type portCounter struct {
	env map[string]string
	// memo、lists 按节点记下算过的结果：同一个锚点被引用多少次都只算一遍。busy 防止锚点引用自己。
	memo, lists map[*yaml.Node]int
	busy        map[*yaml.Node]bool
}

// count 返回节点下面所有 ports、expose 列表里的端口数。
func (c *portCounter) count(n *yaml.Node) int {
	if n == nil {
		return 0
	}
	if v, ok := c.memo[n]; ok {
		return v
	}
	if c.busy[n] {
		return 0
	}
	c.busy[n] = true
	defer delete(c.busy, n)
	total := 0
	switch n.Kind {
	case yaml.AliasNode:
		total = c.count(n.Alias)
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			switch keyName(n.Content[i]) {
			case "ports", "expose":
				total = capPorts(total + c.list(n.Content[i+1]))
			default:
				total = capPorts(total + c.count(n.Content[i+1]))
			}
		}
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, ch := range n.Content {
			total = capPorts(total + c.count(ch))
		}
	}
	c.memo[n] = total
	return total
}

// list 返回 ports、expose 的值里的端口数：列表里每一项按 entry 算，不是列表的按普通节点往下找。
func (c *portCounter) list(n *yaml.Node) int {
	n = resolveAlias(n)
	if n == nil {
		return 0
	}
	if n.Kind != yaml.SequenceNode {
		return c.count(n)
	}
	if v, ok := c.lists[n]; ok {
		return v
	}
	total := 0
	for _, e := range n.Content {
		total = capPorts(total + c.entry(e))
	}
	c.lists[n] = total
	return total
}

// entry 返回 ports、expose 里一项的端口数。短写法按容器端口的范围算（compose 按它展开，对外端口是范围、
// 容器端口只有一个时不展开）；长写法（map）的 target 只能是一个端口。
func (c *portCounter) entry(n *yaml.Node) int {
	n = resolveAlias(n)
	if n == nil || n.Kind != yaml.ScalarNode {
		return 1
	}
	s := n.Value
	if c.env != nil && strings.Contains(s, "$") {
		if v, err := template.Substitute(s, func(k string) (string, bool) {
			v, ok := c.env[k]
			return v, ok
		}); err == nil {
			s = v
		}
	}
	return portRangeSize(s)
}

// portRangeSize 返回短写法 [IP:][对外端口:]容器端口[/协议] 里容器端口的个数。
// 和 compose 解析库展开时用同一套规则（go-connections 的 nat.ParsePortSpec）：按冒号分开取最后一段，
// 去掉第一个斜杠后的协议，再用 nat.ParsePortRange 读范围（它认 "+1" 这样的写法），所以算出的数和真正展开的一样。
// 读不了的：容器端口里有没替换的变量时算一个（解析库也读不了，不会展开），变量带默认值（"${PORT:-8080}"）时
// 最后一个冒号在变量里面，取到的是 "-8080}"，同样算一个；没有变量却含 "-" 的按超过上限算，
// 免得解析库和这里的规则有出入时被绕过；其余的（比如 "abc"）解析库也会报错，算一个。
func portRangeSize(s string) int {
	parts := strings.Split(s, ":")
	port, _, _ := strings.Cut(parts[len(parts)-1], "/")
	lo, hi, err := nat.ParsePortRange(port)
	switch {
	case err == nil:
		return capPorts(int(hi-lo) + 1)
	case strings.Contains(port, "$"), insideVariable(s):
		return 1
	case strings.Contains(port, "-"):
		return MaxComposePorts + 1
	default:
		return 1
	}
}

// insideVariable 判断 s 的最后一个冒号是不是在没替换的变量里面（"${PORT:-8080}"、"8080:${PORT:-80}"）：
// 冒号之前的 "${" 比 "}" 多。
func insideVariable(s string) bool {
	i := strings.LastIndex(s, ":")
	return i >= 0 && strings.Count(s[:i], "${") > strings.Count(s[:i], "}")
}

// serviceCount 是一个服务自己写的端口数和它 extends 的同一个文件里的服务。
type serviceCount struct {
	own    int
	parent string
}

// collectServices 记下文档里每个服务的端口数和 extends。多个文档里的同名服务会被 compose 合并，端口数相加。
func (c *portCounter) collectServices(doc *yaml.Node, out map[string]*serviceCount) {
	root := resolveAlias(doc)
	if root != nil && root.Kind == yaml.DocumentNode && len(root.Content) == 1 {
		root = resolveAlias(root.Content[0])
	}
	services := mapValue(root, "services")
	if services == nil || services.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(services.Content); i += 2 {
		name := keyName(services.Content[i])
		svc := resolveAlias(services.Content[i+1])
		sc := out[name]
		if sc == nil {
			sc = &serviceCount{}
			out[name] = sc
		}
		sc.own = capPorts(sc.own + c.count(svc))
		if p := extendsParent(mapValue(svc, "extends")); p != "" {
			sc.parent = p
		}
	}
}

// extendsParent 返回 extends 指向的同一个文件里的服务（extends: web 或 extends: {service: web}）。
// 指向别的文件的读不到：面板上没有这些文件，节点上只有管理员能保存 extends。
func extendsParent(n *yaml.Node) string {
	n = resolveAlias(n)
	switch {
	case n == nil:
		return ""
	case n.Kind == yaml.ScalarNode:
		return n.Value
	case n.Kind == yaml.MappingNode && mapValue(n, "file") == nil:
		if s := resolveAlias(mapValue(n, "service")); s != nil && s.Kind == yaml.ScalarNode {
			return s.Value
		}
	}
	return ""
}

// inherited 返回服务经 extends 从上级继承来的端口数（上级自己的加上上级继承的）。
func inherited(name string, services map[string]*serviceCount, seen map[string]bool) int {
	sc := services[name]
	if sc == nil || sc.parent == "" || seen[name] {
		return 0
	}
	seen[name] = true
	p := services[sc.parent]
	if p == nil {
		return 0
	}
	return capPorts(p.own + inherited(sc.parent, services, seen))
}

func resolveAlias(n *yaml.Node) *yaml.Node {
	for i := 0; n != nil && n.Kind == yaml.AliasNode && i < 100; i++ {
		n = n.Alias
	}
	if n != nil && n.Kind == yaml.AliasNode {
		return nil
	}
	return n
}

// mapValue 返回 map 节点里某个键的值，没有时为 nil。
func mapValue(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if keyName(n.Content[i]) == key {
			return n.Content[i+1]
		}
	}
	return nil
}

// keyName 是键的文字；写成 !!binary 的键按解码后的内容算。
func keyName(n *yaml.Node) string {
	n = resolveAlias(n)
	if n == nil || n.Kind != yaml.ScalarNode {
		return ""
	}
	if n.ShortTag() == "!!binary" {
		if b, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(n.Value), "")); err == nil {
			return string(b)
		}
	}
	return n.Value
}
