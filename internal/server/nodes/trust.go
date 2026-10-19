package nodes

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/store"
)

// settingReplicaTrusted 是允许在备份节点上使用特权配置的项目 ID 列表（JSON），在 settings 表里。
const settingReplicaTrusted = "replica_trusted"

// TrustedProjects 返回管理员允许在备份节点上使用特权配置的项目。
func (r *Registry) TrustedProjects(ctx context.Context) []string {
	var ids []string
	if v, err := r.q.GetSetting(ctx, settingReplicaTrusted); err == nil {
		_ = json.Unmarshal([]byte(v), &ids)
	}
	return ids
}

// SetProjectTrusted 允许或禁止项目在备份节点上使用特权配置，并下发给所有在线节点。
// 项目 ID 跟着项目走（顶替、迁移后不变），所以不区分节点。
func (r *Registry) SetProjectTrusted(ctx context.Context, id string, trusted bool) error {
	ids := slices.DeleteFunc(r.TrustedProjects(ctx), func(x string) bool { return x == id })
	if trusted {
		ids = append(ids, id)
	}
	b, _ := json.Marshal(ids)
	if err := r.q.SetSetting(ctx, store.SetSettingParams{Key: settingReplicaTrusted, Value: string(b)}); err != nil {
		return err
	}
	r.mu.RLock()
	conns := make([]*Conn, 0, len(r.conns))
	for _, c := range r.conns {
		conns = append(conns, c)
	}
	r.mu.RUnlock()
	for _, c := range conns {
		if !c.Pending() {
			go r.pushTrusted(context.Background(), c)
		}
	}
	return nil
}

// pushTrusted 把允许使用特权配置的项目下发给一个节点。
func (r *Registry) pushTrusted(ctx context.Context, c *Conn) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if _, err := c.Agent.SetTrustedProjects(ctx, &nagiv1.SetTrustedProjectsRequest{ProjectIds: r.TrustedProjects(ctx)}); err != nil {
		r.log.Warn("下发允许特权配置的项目失败", "node", c.NodeID, "err", err)
	}
}
