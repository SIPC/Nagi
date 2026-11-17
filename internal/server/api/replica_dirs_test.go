package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/proto/nagi/v1/nagiv1connect"
	"github.com/SIPC/nagi/internal/server/auth"
)

// 指定副本的存放位置只有管理员能做，而且要先做高权限确认（见 docs/snapshot.md#副本的存放位置）。
// 操作员可以设置备份节点，但只能用节点的默认目录。
func TestSetBackupNodesReplicaDirAdminOnly(t *testing.T) {
	s := &projectService{API: &API{}}
	req := func(dirs map[string]string) *connect.Request[nagiv1.SetBackupNodesRequest] {
		return connect.NewRequest(&nagiv1.SetBackupNodesRequest{ProjectId: "p1", Container: "web", BackupNodeIds: []string{"n2"}, ReplicaDirs: dirs})
	}
	code := func(err error) connect.Code {
		var ce *connect.Error
		if errors.As(err, &ce) {
			return ce.Code()
		}
		return connect.CodeUnknown
	}

	operator := auth.WithUser(context.Background(), auth.User{ID: 2, Role: auth.RoleOperator, ElevatedUntil: time.Now().Add(time.Minute).Unix()})
	if _, err := s.SetBackupNodes(operator, req(map[string]string{"n2": "/mnt/big"})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("操作员指定存放位置应该被拒绝：%v", err)
	}

	admin := auth.WithUser(context.Background(), auth.User{ID: 1, Role: auth.RoleAdmin})
	_, err := s.SetBackupNodes(admin, req(map[string]string{"n2": "/mnt/big"}))
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Meta().Get(elevationHeader) != "required" {
		t.Fatalf("管理员没有做高权限确认时应该要求确认：%v", err)
	}

	// 留空的存放位置等于没有指定，不拦（后面会因为测试里没有项目服务而出错，这里只看没有被权限拦下）。
	func() {
		defer func() { _ = recover() }()
		_, err := s.SetBackupNodes(operator, req(map[string]string{"n2": "  "}))
		if code(err) == connect.CodePermissionDenied {
			t.Fatalf("没有指定存放位置时操作员不应该被拦：%v", err)
		}
	}()
}

func TestReplicaDirNeedsElevation(t *testing.T) {
	for _, p := range []string{nagiv1connect.NodeServiceSetNodeReplicaDirProcedure, nagiv1connect.ProjectServiceSetBackupReplicaDirProcedure} {
		if !elevationRequired[p] {
			t.Errorf("%s 要先做高权限确认", p)
		}
		if !activityRecorded(p) {
			t.Errorf("%s 要记进活动记录", p)
		}
	}
	for _, role := range []string{auth.RoleOperator, auth.RoleViewer} {
		for _, p := range []string{nagiv1connect.NodeServiceSetNodeReplicaDirProcedure, nagiv1connect.ProjectServiceSetBackupReplicaDirProcedure} {
			if auth.Allowed(role, p) {
				t.Errorf("%s 不应该能调用 %s", role, p)
			}
		}
	}
}
