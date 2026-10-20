package standby

import (
	"context"
	"os"
	"strconv"
	"strings"
	"time"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/auth"
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

// Promote 把备用面板提升为主面板（见 docs/standby.md#提升）：任期加一、记下自己是主面板；
// 把原主面板登记为备用面板（它回来后发现任期落后，自动降为备用、从本面板同步）；删掉 standby.json。
// 调用前要先停止同步，免得之后再收到的快照把这些改动覆盖掉。
func Promote(ctx context.Context, q *store.Queries, dataDir, self string, info *Info) (uint64, error) {
	term, _ := Term(ctx, q)
	term++
	if err := q.SetSetting(ctx, store.SetSettingParams{Key: SettingTerm, Value: strconv.FormatUint(term, 10)}); err != nil {
		return 0, err
	}
	if err := q.SetSetting(ctx, store.SetSettingParams{Key: SettingPrimary, Value: self}); err != nil {
		return 0, err
	}
	if err := q.DeleteStandbyPanelByServerFP(ctx, self); err != nil {
		return 0, err
	}
	if info.PrimaryFingerprint != "" {
		if _, err := q.GetStandbyPanelByServerFP(ctx, info.PrimaryFingerprint); err != nil {
			id, err := auth.RandomToken()
			if err != nil {
				return 0, err
			}
			if err := q.InsertStandbyPanel(ctx, store.InsertStandbyPanelParams{
				ID: strings.ToLower(id[:12]), Name: "原主面板", Address: info.PrimaryAddress, ServerFp: info.PrimaryFingerprint, CreatedAt: time.Now().Unix(),
			}); err != nil {
				return 0, err
			}
		}
	}
	if err := os.Remove(infoPath(dataDir)); err != nil && !os.IsNotExist(err) {
		return 0, err
	}
	return term, nil
}
