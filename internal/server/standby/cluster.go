package standby

import (
	"context"
	"strconv"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/store"
)

// 任期和主面板存在 settings 表里，随同步传给备用面板（见 docs/standby.md#谁是主面板任期）。
const (
	SettingTerm    = "panel_term"
	SettingPrimary = "panel_primary"
)

// Term 返回当前的任期和主面板的公钥指纹。
func Term(ctx context.Context, q *store.Queries) (uint64, string) {
	v, _ := q.GetSetting(ctx, SettingTerm)
	term, _ := strconv.ParseUint(v, 10, 64)
	primary, _ := q.GetSetting(ctx, SettingPrimary)
	return max(term, 1), primary
}

// Init 在主面板启动时记下自己是主面板（第一次启动时任期为 1）。
func Init(ctx context.Context, q *store.Queries, fingerprint string) error {
	if v, err := q.GetSetting(ctx, SettingPrimary); err == nil && v != "" {
		return nil
	}
	if err := q.SetSetting(ctx, store.SetSettingParams{Key: SettingTerm, Value: "1"}); err != nil {
		return err
	}
	return q.SetSetting(ctx, store.SetSettingParams{Key: SettingPrimary, Value: fingerprint})
}

// Cluster 返回告诉节点的主备信息：任期、主面板，以及主面板和所有备用面板的地址和公钥指纹。
// primaryAddress 是主面板的地址（host:port）。
func Cluster(ctx context.Context, q *store.Queries, primaryAddress string) *nagiv1.PanelCluster {
	term, primary := Term(ctx, q)
	if primary == "" {
		return nil
	}
	c := &nagiv1.PanelCluster{Term: term, Primary: primary,
		Panels: []*nagiv1.ClusterPanel{{Address: primaryAddress, Fingerprint: primary}}}
	rows, err := q.ListStandbyPanels(ctx)
	if err != nil {
		return nil
	}
	for _, r := range rows {
		c.Panels = append(c.Panels, &nagiv1.ClusterPanel{Address: r.Address, Fingerprint: r.ServerFp})
	}
	return c
}
