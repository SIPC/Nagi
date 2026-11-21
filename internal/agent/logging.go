package agent

import (
	"strings"

	"github.com/SIPC/nagi/internal/agent/proxy"
	"github.com/SIPC/nagi/internal/pkg/logstore"
)

func (a *agent) logProxyAccess(e proxy.AccessRecord) {
	project, _, _ := strings.Cut(e.Route, "/")
	a.cfg.Logs.Report(logstore.Event{Time: e.Time, Category: logstore.Access, Kind: "proxy.http", Project: project, Object: e.Route, Data: e})
}

func (a *agent) logProxyConnection(e proxy.ConnectionRecord) {
	project, _, _ := strings.Cut(e.Route, "/")
	a.cfg.Logs.Report(logstore.Event{Time: e.Time, Category: logstore.Access, Kind: "proxy.l4." + e.Phase, Project: project, Object: e.Route, Data: e})
}
