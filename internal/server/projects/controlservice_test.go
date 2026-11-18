package projects

import (
	"context"
	"strings"
	"testing"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/store"
)

// 单独控制一个服务和控制整个容器的规则一样：没部署的不能控制，设了顶替的不能停止（顶替按整个容器算），
// 这些在找节点之前就拒绝。
func TestControlServiceRules(t *testing.T) {
	ctx := context.Background()
	s := haTestService(t)
	stop, restart := nagiv1.ContainerAction_CONTAINER_ACTION_STOP, nagiv1.ContainerAction_CONTAINER_ACTION_RESTART
	discard := Output(func(string) {})

	if err := s.ControlService(ctx, "aaaaaaaaaaaaaaaa", "web", "app", restart, discard); err == nil || !strings.Contains(err.Error(), "还没有部署") {
		t.Errorf("未部署：%v", err)
	}
	if err := s.ControlService(ctx, "aaaaaaaaaaaaaaaa", "nope", "app", restart, discard); err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Errorf("容器不存在：%v", err)
	}
	if err := s.Q.SetContainerDeployed(ctx, store.SetContainerDeployedParams{DeployedVersion: 1, ProjectID: "aaaaaaaaaaaaaaaa", Name: "web"}); err != nil {
		t.Fatal(err)
	}
	if err := s.ControlService(ctx, "aaaaaaaaaaaaaaaa", "web", "", restart, discard); err == nil {
		t.Error("服务名为空应拒绝")
	}
	if err := s.Q.SetContainerFailover(ctx, store.SetContainerFailoverParams{Failover: []byte{1}, FailoverEpoch: 1, ProjectID: "aaaaaaaaaaaaaaaa", Name: "web"}); err != nil {
		t.Fatal(err)
	}
	if err := s.ControlService(ctx, "aaaaaaaaaaaaaaaa", "web", "app", stop, discard); err == nil || !strings.Contains(err.Error(), "顶替") {
		t.Errorf("设了顶替时停止服务：%v", err)
	}
}
