package projects

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/nodes"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/protobuf/proto"
)

// 旧版本的节点不认识存放位置：面板不把它发过去，直接提示升级（看节点上次上报的 NodeInfo.replica_dirs）。
func TestCheckReplicaDirs(t *testing.T) {
	ctx := context.Background()
	s := haTestService(t)
	s.Nodes = nodes.NewRegistry(s.Q, slog.Default(), nil, nil)
	info, _ := proto.Marshal(&nagiv1.NodeInfo{ReplicaDirs: true})
	old, _ := proto.Marshal(&nagiv1.NodeInfo{MultiBackup: true})
	if err := s.Q.UpdateNodeSeen(ctx, store.UpdateNodeSeenParams{Info: info, ID: "n2"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Q.UpdateNodeSeen(ctx, store.UpdateNodeSeenParams{Info: old, ID: "n3"}); err != nil {
		t.Fatal(err)
	}
	if !s.SupportsReplicaDirs(ctx, "n2") || s.SupportsReplicaDirs(ctx, "n3") {
		t.Fatal("按节点上报的 replica_dirs 判断")
	}

	got, err := s.checkReplicaDirs(ctx, []string{"n2", "n3"}, map[string]string{"n2": " /mnt/big/ ", "n3": ""})
	if err != nil || len(got) != 1 || got["n2"] != "/mnt/big" {
		t.Fatalf("应该接受新节点上的位置、忽略留空的：%v %v", got, err)
	}
	for desc, c := range map[string]struct {
		added []string
		dirs  map[string]string
		want  string
	}{
		"旧版本的节点":    {[]string{"n3"}, map[string]string{"n3": "/mnt/big"}, "版本过低"},
		"不是新加的备份节点": {[]string{"n3"}, map[string]string{"n2": "/mnt/big"}, "新加的备份节点"},
		"相对路径":      {[]string{"n2"}, map[string]string{"n2": "mnt/big"}, "绝对路径"},
		"包含 ..":     {[]string{"n2"}, map[string]string{"n2": "/mnt/../etc"}, "绝对路径"},
	} {
		if _, err := s.checkReplicaDirs(ctx, c.added, c.dirs); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s：应该拒绝（%s），实际 %v", desc, c.want, err)
		}
	}
}

// 面板记下单独指定的位置，只显示现在的备份节点上的。
func TestBackupReplicaDirsShown(t *testing.T) {
	ctx := context.Background()
	s := haTestService(t)
	if err := s.Q.SetBackupReplicaDir(ctx, "aaaaaaaaaaaaaaaa", "web", "n2", "/mnt/big"); err != nil {
		t.Fatal(err)
	}
	if err := s.Q.SetBackupReplicaDir(ctx, "aaaaaaaaaaaaaaaa", "web", "n3", "/mnt/old"); err != nil {
		t.Fatal(err)
	}
	c, err := s.container(ctx, "aaaaaaaaaaaaaaaa", "web")
	if err != nil {
		t.Fatal(err)
	}
	got := s.backupReplicaDirs(ctx, c, []string{"n2"})
	if len(got) != 1 || got["n2"] != "/mnt/big" {
		t.Fatalf("只显示现在的备份节点：%v", got)
	}
	// 改回默认目录时删掉记录。
	if err := s.Q.SetBackupReplicaDir(ctx, "aaaaaaaaaaaaaaaa", "web", "n2", ""); err != nil {
		t.Fatal(err)
	}
	if got := s.backupReplicaDirs(ctx, c, []string{"n2"}); len(got) != 0 {
		t.Fatalf("改回默认目录后不再显示：%v", got)
	}
	// 删除容器时一起删掉。
	if _, err := s.Q.ListBackupReplicaDirs(ctx, "aaaaaaaaaaaaaaaa", "web"); err != nil {
		t.Fatal(err)
	}
	if err := s.Q.DeleteContainer(ctx, store.DeleteContainerParams{ProjectID: "aaaaaaaaaaaaaaaa", Name: "web"}); err != nil {
		t.Fatal(err)
	}
	if m, err := s.Q.ListBackupReplicaDirs(ctx, "aaaaaaaaaaaaaaaa", "web"); err != nil || len(m) != 0 {
		t.Fatalf("删除容器后记录应该一起删掉：%v %v", m, err)
	}
}

// 指定存放位置时，备份节点必须是这个容器的备份节点。
func TestSetBackupReplicaDirNotBackup(t *testing.T) {
	s := haTestService(t)
	s.Nodes = nodes.NewRegistry(s.Q, slog.Default(), nil, nil)
	err := s.SetBackupReplicaDir(context.Background(), "aaaaaaaaaaaaaaaa", "web", "n2", "/mnt/big", func(string) {})
	if err == nil || !strings.Contains(err.Error(), "不是容器") {
		t.Fatalf("应该拒绝：%v", err)
	}
	if err := s.SetBackupReplicaDir(context.Background(), "aaaaaaaaaaaaaaaa", "web", "n2", "mnt", func(string) {}); err == nil || !strings.Contains(err.Error(), "绝对路径") {
		t.Fatalf("应该拒绝相对路径：%v", err)
	}
}
