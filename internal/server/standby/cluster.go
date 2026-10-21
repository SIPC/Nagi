package standby

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/SIPC/nagi/internal/pkg/tlsutil"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/store"
)

// 任期和主面板存在 settings 表里，随同步传给备用面板（见 docs/standby.md#谁是主面板任期）。
const (
	SettingTerm    = "panel_term"
	SettingPrimary = "panel_primary"
	// SettingMembers 是这组面板里出现过的所有面板的公钥指纹（JSON 列表，只增不减）：
	// 它们写进节点缓存的配置都算本组写的，移除备用面板以后也一样（见 docs/standby.md）。
	SettingMembers = "panel_members"
)

// SettingSeenTerm 是本面板向别的面板核实过的最大任期。是本面板自己的设置，同步时不覆盖。
const SettingSeenTerm = "seen_term"

func seenTerm(ctx context.Context, q *store.Queries) uint64 {
	v, _ := q.GetSetting(ctx, SettingSeenTerm)
	n, _ := strconv.ParseUint(v, 10, 64)
	return n
}

// SeeTerm 记下向别的面板核实过的任期。
func SeeTerm(ctx context.Context, q *store.Queries, term uint64) error {
	if term <= seenTerm(ctx, q) {
		return nil
	}
	return q.SetSetting(ctx, store.SetSettingParams{Key: SettingSeenTerm, Value: strconv.FormatUint(term, 10)})
}

// Members 返回这组面板里出现过的所有面板：记下的成员、当前主面板和登记的备用面板。
func Members(ctx context.Context, q *store.Queries) []string {
	var out []string
	if v, err := q.GetSetting(ctx, SettingMembers); err == nil {
		_ = json.Unmarshal([]byte(v), &out)
	}
	if v, err := q.GetSetting(ctx, SettingPrimary); err == nil && v != "" {
		out = append(out, v)
	}
	if rows, err := q.ListStandbyPanels(ctx); err == nil {
		for _, r := range rows {
			out = append(out, r.ServerFp)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// AddMember 把面板记进成员列表。
func AddMember(ctx context.Context, q *store.Queries, fingerprint string) error {
	var list []string
	if v, err := q.GetSetting(ctx, SettingMembers); err == nil {
		_ = json.Unmarshal([]byte(v), &list)
	}
	if slices.Contains(list, fingerprint) {
		return nil
	}
	b, _ := json.Marshal(append(list, fingerprint))
	return q.SetSetting(ctx, store.SetSettingParams{Key: SettingMembers, Value: string(b)})
}

// Term 返回当前的任期和主面板的公钥指纹。
func Term(ctx context.Context, q *store.Queries) (uint64, string) {
	v, _ := q.GetSetting(ctx, SettingTerm)
	term, _ := strconv.ParseUint(v, 10, 64)
	primary, _ := q.GetSetting(ctx, SettingPrimary)
	return max(term, 1), primary
}

// Init 在主面板启动时记下自己是主面板（第一次启动时任期为 1）。
func Init(ctx context.Context, q *store.Queries, fingerprint string) error {
	if err := AddMember(ctx, q, fingerprint); err != nil {
		return err
	}
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

// Promote 把备用面板提升为主面板（见 docs/standby.md#提升）：任期加一（也比核实过的别的面板的任期大）、
// 记下自己是主面板；把原主面板登记为备用面板（它回来后核实本面板是主面板，自动降为备用、从本面板同步）；删掉 standby.json。
// 数据库的改动在一个事务里。调用前要先停止同步，免得之后再收到的快照把这些改动覆盖掉。
func Promote(ctx context.Context, d *sql.DB, q *store.Queries, dataDir, self string, info *Info) (uint64, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	qt := q.WithTx(tx)
	term, _ := Term(ctx, qt)
	term = max(term, seenTerm(ctx, qt)) + 1
	if err := qt.SetSetting(ctx, store.SetSettingParams{Key: SettingTerm, Value: strconv.FormatUint(term, 10)}); err != nil {
		return 0, err
	}
	if err := qt.SetSetting(ctx, store.SetSettingParams{Key: SettingPrimary, Value: self}); err != nil {
		return 0, err
	}
	if err := AddMember(ctx, qt, self); err != nil {
		return 0, err
	}
	if err := qt.DeleteStandbyPanelByServerFP(ctx, self); err != nil {
		return 0, err
	}
	if info.PrimaryFingerprint != "" {
		if _, err := qt.GetStandbyPanelByServerFP(ctx, info.PrimaryFingerprint); err != nil {
			id, err := auth.RandomToken()
			if err != nil {
				return 0, err
			}
			if err := qt.InsertStandbyPanel(ctx, store.InsertStandbyPanelParams{
				ID: strings.ToLower(id[:12]), Name: "原主面板", Address: info.PrimaryAddress, ServerFp: info.PrimaryFingerprint, CreatedAt: time.Now().Unix(),
			}); err != nil {
				return 0, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if err := os.Remove(infoPath(dataDir)); err != nil && !os.IsNotExist(err) {
		return 0, err
	}
	return term, nil
}

// PanelStatus 是 GET /api/standby/status 的回复：面板的任期、主面板和自己是不是主面板。公开信息，不需要登录。
type PanelStatus struct {
	Term      uint64 `json:"term"`
	Primary   string `json:"primary"`
	IsPrimary bool   `json:"is_primary"`
}

// FetchStatus 向 address 上的面板要它的状态，钉住 fingerprint：回复确实来自那个面板。
func FetchStatus(ctx context.Context, address, fingerprint string) (PanelStatus, error) {
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsutil.PinnedConfig(fingerprint)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+address+"/api/standby/status", nil)
	if err != nil {
		return PanelStatus{}, err
	}
	res, err := client.Do(req)
	if err != nil {
		return PanelStatus{}, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return PanelStatus{}, fmt.Errorf("面板返回 %s", res.Status)
	}
	var st PanelStatus
	err = json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&st)
	return st, err
}
