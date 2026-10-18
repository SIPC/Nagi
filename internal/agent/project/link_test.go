package project

import "testing"

func TestCleanLinkPorts(t *testing.T) {
	got, err := cleanLinkPorts([]string{" pg:5432 ", "", "web.app:8080", "pg:5432"})
	if err != nil || len(got) != 2 || got[0] != "pg:5432" || got[1] != "web.app:8080" {
		t.Fatalf("got %v %v", got, err)
	}
	for _, bad := range []string{"pg", "pg:0", "pg:70000", "PG:80", "a.b.c:80", ":80"} {
		if _, err := cleanLinkPorts([]string{bad}); err == nil {
			t.Errorf("%q 应该被拒绝", bad)
		}
	}
	m := linkPortMap([]string{"pg:5432", "pg:5433", "web.app:80"})
	if len(m["pg"]) != 2 || m["web.app"][0] != 80 {
		t.Fatalf("got %v", m)
	}
}
