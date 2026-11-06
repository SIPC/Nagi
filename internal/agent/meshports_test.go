package agent

import (
	"testing"

	"github.com/SIPC/nagi/internal/pkg/mesh"
)

// TestMeshPorts：mesh 上各个服务的端口不能撞，撞了后启动的那个监听不上。
func TestMeshPorts(t *testing.T) {
	ports := map[uint16]string{}
	for name, p := range map[string]uint16{"MeshService": mesh.ServicePort, "测速": mesh.SpeedPort, "项目内网": linkPort} {
		if other, ok := ports[p]; ok {
			t.Fatalf("%s 和 %s 都用 mesh 的 %d 端口", name, other, p)
		}
		ports[p] = name
	}
}
