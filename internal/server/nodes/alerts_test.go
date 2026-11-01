package nodes

import "testing"

func TestAlertContainer(t *testing.T) {
	for key, want := range map[string][2]string{
		"health:p1/web/nginx": {"p1", "web"},
		"lease:p1-web":        {"p1", "web"},
		"disk":                {"", ""},
		"cert:example.com":    {"", ""},
		"errors:p1/web":       {"p1", "web"},
	} {
		p, c := alertContainer(key)
		if p != want[0] || c != want[1] {
			t.Errorf("%s = %s/%s，应为 %v", key, p, c, want)
		}
	}
}
