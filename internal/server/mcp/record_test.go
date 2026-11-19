package mcp

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/SIPC/nagi/internal/pkg/i18n"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/audit"
	"github.com/SIPC/nagi/internal/server/auth"
)

// 工具参数里用来指定节点（名称或 ID）的字段，键名要含 node（或者是 peer）：记录时按键名把名称换成节点 ID，
// 起了别的名字（target、to、host 这类）的话，名称会原样留在记录里，给看不到这台节点的成员看到。
func TestNodeArgKeys(t *testing.T) {
	// 说明里提到节点和名称、其实不是指定节点的字段。confirm 这类重复节点名称的字段记录时整个隐去（audit.JSONParams）。
	notNode := map[string]bool{"confirm": true, "confirm_name": true}
	var check func(tool string, typ reflect.Type)
	check = func(tool string, typ reflect.Type) {
		for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice {
			typ = typ.Elem()
		}
		if typ.Kind() != reflect.Struct {
			return
		}
		for i := range typ.NumField() {
			f := typ.Field(i)
			key, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if f.Anonymous || f.Type.Kind() == reflect.Struct || f.Type.Kind() == reflect.Slice && f.Type.Elem().Kind() == reflect.Struct {
				check(tool, f.Type)
				continue
			}
			kind := f.Type.Kind()
			if kind == reflect.Slice {
				kind = f.Type.Elem().Kind()
			}
			desc := strings.ToLower(f.Tag.Get("jsonschema"))
			if kind != reflect.String || key == "" || notNode[key] || !strings.Contains(desc, "node") {
				continue
			}
			// 说明里写着按名称或 ID 指定节点的：node name、name or ID、names or IDs。
			byName := strings.Contains(desc, "node name") || strings.Contains(desc, "name or id") || strings.Contains(desc, "names or ids")
			if byName && !nodeArg(key) {
				t.Errorf("工具 %s 的参数 %s 像是用来指定节点的（%q），键名要含 node；不是的话登记在 notNode 里", tool, key, f.Tag.Get("jsonschema"))
			}
		}
	}
	for _, tl := range allTools {
		if tl.in == nil {
			t.Errorf("工具 %s 没有记下参数类型", tl.name)
			continue
		}
		check(tl.name, tl.in)
	}
	for key, want := range map[string]bool{"node": true, "backup_node": true, "entry_nodes": true, "peer": true, "container": false, "project": false} {
		if nodeArg(key) != want {
			t.Errorf("nodeArg(%q) = %v", key, !want)
		}
	}
}

// 记录里的节点参数写成 ID：按调用者看得到的节点对名称（不分大小写），重名的写各台的 ID，对不上的原样留着；
// 别的参数不动，大整数不走样。
func TestNodeArgIDs(t *testing.T) {
	c, fn, _ := nodeAdminCall(t)
	in := `{"project":"shop","node":"Alpha","backup_node":"n2","entry_nodes":["beta","nowhere"],"peer":"panel","expected_revision":12345678901234567,"name":"alpha"}`
	out, found := c.h.nodeArgIDs(c.ctx, []byte(in))
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["node"] != "n1" || got["backup_node"] != "n2" || got["peer"] != "panel" || got["name"] != "alpha" {
		t.Errorf("节点参数：%s", out)
	}
	if l, _ := got["entry_nodes"].([]any); len(l) != 2 || l[0] != "n2" || l[1] != "nowhere" {
		t.Errorf("列表里的节点：%s", out)
	}
	if !strings.Contains(string(out), "12345678901234567") {
		t.Errorf("大整数不该走样：%s", out)
	}
	if found["n1"] != "alpha" || found["n2"] != "beta" {
		t.Errorf("换到的节点：%v", found)
	}
	// 重名的写成各台 ID 的数组：查看记录的成员看不到其中一台时，面板按 ID 去掉的是数组里的那一项，不会剩下「n1,」这样半截的值。
	fn.nodes = []*nagiv1.Node{{Id: "n1", Name: "same"}, {Id: "n2", Name: "Same"}, {Id: "n3", Name: "other"}}
	out, found = c.h.nodeArgIDs(c.ctx, []byte(`{"node":"same","entry_nodes":["same","other"]}`))
	if !strings.Contains(string(out), `"node":["n1","n2"]`) || !strings.Contains(string(out), `"entry_nodes":[["n1","n2"],"n3"]`) || strings.Contains(string(out), "same") {
		t.Errorf("重名的节点：%s", out)
	}
	if found["n1"] != "same" || found["n2"] != "Same" {
		t.Errorf("重名时换到的节点：%v", found)
	}
	// 记录的对象：节点参数不是一个 ID 时不当成节点。
	var rec audit.Event
	c.h.o.Audit = func(_ context.Context, _ auth.User, e audit.Event, _ string, _ *audit.Collector) { rec = e }
	c.h.record(c.ctx, c.u, tool{name: "get_node"}, []byte(`{"node":"same"}`), audit.ResultFailed, "x", 0, 0, nil)
	if rec.TargetType != "tool" || strings.Contains(rec.Params, "n1,") {
		t.Errorf("重名时的记录：%s %s %s", rec.TargetType, rec.TargetID, rec.Params)
	}
	// 拿不到节点列表时不留原文。
	if out, _ := (&handler{}).nodeArgIDs(c.ctx, []byte(`{"project":"shop","node":"alpha"}`)); strings.Contains(string(out), "alpha") || !strings.Contains(string(out), "shop") {
		t.Errorf("拿不到节点列表：%s", out)
	}
	// 没有节点参数时原样返回。
	if out, _ := c.h.nodeArgIDs(c.ctx, []byte(`{"project":"shop"}`)); string(out) != `{"project":"shop"}` {
		t.Errorf("没有节点参数：%s", out)
	}
}

// 调用记录：节点参数是 ID，对象是节点时记 ID 和名称，参数里记着工具要的权限；参数太长时权限仍然认得出。
func TestRecordNodeAsID(t *testing.T) {
	c, _, _ := nodeAdminCall(t)
	var got audit.Event
	project := ""
	c.h.o.Audit = func(_ context.Context, _ auth.User, e audit.Event, p string, _ *audit.Collector) { got, project = e, p }
	byName := map[string]tool{}
	for _, tl := range allTools {
		byName[tl.name] = tl
	}
	perms := func(params string) []string {
		var p struct {
			Perms     *[]string      `json:"perms"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal([]byte(params), &p); err != nil || p.Perms == nil {
			t.Fatalf("参数里应该记着权限：%s", params)
		}
		return *p.Perms
	}

	c.h.record(c.ctx, c.u, byName["migrate_container"], []byte(`{"project":"shop","container":"","node":"beta"}`), audit.ResultFailed, "x", 0, 0, nil)
	if got.TargetType != "node" || got.TargetID != "n2" || got.TargetName != "beta" || project != "shop" {
		t.Errorf("对象：%s %s %s 项目 %s", got.TargetType, got.TargetID, got.TargetName, project)
	}
	if strings.Contains(got.Params, "beta") || !strings.Contains(got.Params, `"node":"n2"`) {
		t.Errorf("参数里的节点应该是 ID：%s", got.Params)
	}
	if p := perms(got.Params); !slices.Contains(p, auth.PermMigrate.String()) {
		t.Errorf("迁移要的权限：%v", p)
	}

	c.h.record(c.ctx, c.u, byName["exec"], []byte(`{"project":"shop","container":"web","command":["id"]}`), audit.ResultOK, "", 3, 0, nil)
	if p := perms(got.Params); !slices.Contains(p, auth.PermTerminal.String()) {
		t.Errorf("执行命令要的权限：%v", p)
	}
	if got.TargetType != "container" || got.TargetID != "web" {
		t.Errorf("对象：%s %s", got.TargetType, got.TargetID)
	}

	long := `{"project":"shop","container":"web","command":["` + strings.Repeat("x", 2*audit.MaxParams) + `"]}`
	c.h.record(c.ctx, c.u, byName["exec"], []byte(long), audit.ResultOK, "", 0, 0, nil)
	if len(got.Params) > audit.MaxParams || !strings.Contains(got.Params, `"truncated":true`) {
		t.Errorf("太长的参数：%d 字节", len(got.Params))
	}
	perms(got.Params)
}

// 工具自己的报错提到节点时带引用：返回给客户端的是名称，记进审计时可以按节点 ID 换。
func TestNodeErrorRef(t *testing.T) {
	err := nodeErrorf("node %s is not a backup node of container %s", nodeRef(&nagiv1.Node{Id: "n2", Name: "beta"}), "web")
	if err.Error() != "node beta is not a backup node of container web" {
		t.Errorf("文字：%q", err.Error())
	}
	var seen i18n.Ref
	out, changed := i18n.Redact(err, func(r i18n.Ref) (string, bool) {
		seen = r
		return "other node", true
	})
	if !changed || seen.Kind != "node" || seen.ID != "n2" || out.Error() != "node other node is not a backup node of container web" {
		t.Errorf("换过以后：%v %+v", out, seen)
	}
	if err.Error() != "node beta is not a backup node of container web" {
		t.Error("原来的错误不该被改")
	}
}

// 备份节点、mesh 成员按名称找：对上不止一个时报错，不取第一个。
func TestAmbiguousNames(t *testing.T) {
	c, fn, _ := nodeAdminCall(t)
	m := &nagiv1.Container{Name: "web", BackupNodeIds: []string{"n1", "n2"}}
	if id, err := c.backupNodeID(m, "beta"); err != nil || id != "n2" {
		t.Errorf("按名称：%q %v", id, err)
	}
	if id, err := c.backupNodeID(m, "n1"); err != nil || id != "n1" {
		t.Errorf("按 ID：%q %v", id, err)
	}
	if _, err := c.backupNodeID(m, "gamma"); err == nil {
		t.Error("不是备份节点应该报错")
	}
	fn.nodes = []*nagiv1.Node{{Id: "n1", Name: "same"}, {Id: "n2", Name: "same"}, {Id: "n3", Name: "same"}}
	if _, err := c.backupNodeID(m, "same"); err == nil || !strings.Contains(err.Error(), "n1, n2") {
		t.Errorf("两台备份节点同名应该报错并列出 ID：%v", err)
	}
	if id, err := c.backupNodeID(&nagiv1.Container{Name: "web", BackupNodeIds: []string{"n3"}}, "same"); err != nil || id != "n3" {
		t.Errorf("只有一台备份节点叫这个名称：%q %v", id, err)
	}
	st := &nagiv1.MeshStatus{Peers: []*nagiv1.MeshPeerStatus{
		{Name: "dup", PublicKey: []byte{1}}, {Name: "dup", PublicKey: []byte{2}}, {Name: "one", PublicKey: []byte{3}},
	}}
	if p, err := c.findPeer(st, "one"); err != nil || p.GetPublicKey()[0] != 3 {
		t.Errorf("唯一的名称：%v %v", p, err)
	}
	if _, err := c.findPeer(st, "dup"); err == nil || !strings.Contains(err.Error(), "01") || !strings.Contains(err.Error(), "02") {
		t.Errorf("重名应该报错并列出公钥：%v", err)
	}
	if p, err := c.findPeer(st, "02"); err != nil || p.GetPublicKey()[0] != 2 {
		t.Errorf("按公钥：%v %v", p, err)
	}
}
