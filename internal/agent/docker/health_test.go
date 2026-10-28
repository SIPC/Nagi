package docker

import (
	"testing"

	"github.com/moby/moby/api/types/container"
)

func TestHealth(t *testing.T) {
	cases := []struct {
		name string
		in   *container.HealthSummary
		want string
	}{
		{"没有健康检查", nil, ""},
		{"none", &container.HealthSummary{Status: container.NoHealthcheck}, ""},
		{"启动中", &container.HealthSummary{Status: container.Starting}, "starting"},
		{"健康", &container.HealthSummary{Status: container.Healthy}, "healthy"},
		{"真的失败", &container.HealthSummary{Status: container.Unhealthy, FailingStreak: 3}, "unhealthy"},
		// 暂停又恢复后，Docker 在下一次检查前报 unhealthy，但没有失败过。
		{"暂停后还没重新检查", &container.HealthSummary{Status: container.Unhealthy}, "healthy"},
	}
	for _, c := range cases {
		if got := health(c.in); got != c.want {
			t.Errorf("%s: health() = %q，应为 %q", c.name, got, c.want)
		}
	}
}
