package store_test

import (
	"context"
	"slices"
	"testing"

	"github.com/SIPC/nagi/internal/server/store"
)

// 每个成员各自一份；整份覆盖；删除成员时一起删除。
func TestProjectOrder(t *testing.T) {
	ctx := context.Background()
	q := open(t)
	a, err := q.CreateUser(ctx, store.CreateUserParams{Username: "a", PasswordHash: "h", CreatedAt: 1, Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := q.CreateUser(ctx, store.CreateUserParams{Username: "b", PasswordHash: "h", CreatedAt: 2, Role: "viewer"})
	if err != nil {
		t.Fatal(err)
	}
	if o, err := q.GetProjectOrder(ctx, a); err != nil || o.Sort != "" || len(o.Groups) != 0 || len(o.Projects) != 0 {
		t.Fatalf("没保存过应为空：%+v %v", o, err)
	}
	if err := q.SetProjectOrder(ctx, a, store.ProjectOrder{Sort: "custom", Groups: []string{"生产", "测试"}, Projects: []string{"p2", "p1"}}); err != nil {
		t.Fatal(err)
	}
	if err := q.SetProjectOrder(ctx, b, store.ProjectOrder{Sort: "name"}); err != nil {
		t.Fatal(err)
	}
	o, err := q.GetProjectOrder(ctx, a)
	if err != nil || o.Sort != "custom" || !slices.Equal(o.Groups, []string{"生产", "测试"}) || !slices.Equal(o.Projects, []string{"p2", "p1"}) {
		t.Fatalf("a 的排序：%+v %v", o, err)
	}
	if err := q.SetProjectOrder(ctx, a, store.ProjectOrder{Sort: "", Projects: []string{"p1"}}); err != nil {
		t.Fatal(err)
	}
	if o, _ := q.GetProjectOrder(ctx, a); o.Sort != "" || len(o.Groups) != 0 || !slices.Equal(o.Projects, []string{"p1"}) {
		t.Fatalf("应整份覆盖：%+v", o)
	}
	if o, _ := q.GetProjectOrder(ctx, b); o.Sort != "name" {
		t.Fatalf("b 的排序不受影响：%+v", o)
	}
	if err := q.DeleteUser(ctx, b); err != nil {
		t.Fatal(err)
	}
	if o, _ := q.GetProjectOrder(ctx, b); o.Sort != "" {
		t.Fatalf("删除成员后应一起删除：%+v", o)
	}
}

// 节点页的排序和项目页的分开存。
func TestNodeOrder(t *testing.T) {
	ctx := context.Background()
	q := open(t)
	a, err := q.CreateUser(ctx, store.CreateUserParams{Username: "a", PasswordHash: "h", CreatedAt: 1, Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.SetProjectOrder(ctx, a, store.ProjectOrder{Sort: "name", Groups: []string{"项目组"}, Projects: []string{"p1"}}); err != nil {
		t.Fatal(err)
	}
	if o, err := q.GetNodeOrder(ctx, a); err != nil || o.Sort != "" || len(o.Groups) != 0 || len(o.Nodes) != 0 {
		t.Fatalf("没保存过应为空：%+v %v", o, err)
	}
	if err := q.SetNodeOrder(ctx, a, store.NodeOrder{Sort: "custom", Groups: []string{"香港", "美国"}, Nodes: []string{"n2", "n1"}}); err != nil {
		t.Fatal(err)
	}
	if o, _ := q.GetNodeOrder(ctx, a); o.Sort != "custom" || !slices.Equal(o.Groups, []string{"香港", "美国"}) || !slices.Equal(o.Nodes, []string{"n2", "n1"}) {
		t.Fatalf("节点页的排序：%+v", o)
	}
	if o, _ := q.GetProjectOrder(ctx, a); o.Sort != "name" || !slices.Equal(o.Groups, []string{"项目组"}) || !slices.Equal(o.Projects, []string{"p1"}) {
		t.Fatalf("项目页的排序不受影响：%+v", o)
	}
}
