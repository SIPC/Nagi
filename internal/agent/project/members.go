package project

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/SIPC/nagi/internal/agent/store/projectstore"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/protobuf/proto"
)

// 成员（见 docs/agent.md#项目与成员）：项目里独立部署的单位。成员名在项目内唯一，创建后不能改。

func checkMemberName(name string) error {
	if !nameRe.MatchString(name) {
		return invalid("成员名只能用小写字母、数字和 -，不以 - 开头或结尾，不超过 32 个字符")
	}
	return nil
}

// AddMember 往项目里加一个成员：保存配置作为它的版本 1（还没部署）。
func (m *Manager) AddMember(ctx context.Context, id, name string, cfg *nagiv1.MemberConfig) (*nagiv1.Project, error) {
	if err := checkMemberName(name); err != nil {
		return nil, err
	}
	row, pdb, unlock, err := m.projectForUpdate(ctx, id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	err = m.addMember(ctx, pdb, paths{root: row.Dir}, id, name, cfg)
	pdb.Close()
	if err != nil {
		return nil, err
	}
	return m.getProject(ctx, id)
}

func (m *Manager) addMember(ctx context.Context, pdb *projectDB, p paths, id, name string, cfg *nagiv1.MemberConfig) error {
	if cfg == nil {
		return invalid("缺少成员配置")
	}
	cfg = proto.Clone(cfg).(*nagiv1.MemberConfig)
	if _, err := pdb.q.GetMember(ctx, name); err == nil {
		return invalid("项目里已经有成员 %s", name)
	}
	mp := p.member(name)
	if entries, err := os.ReadDir(mp.root); err == nil && len(entries) > 0 {
		return invalid("成员目录 %s 已存在（可能是之前删除成员时保留的），换个名字或先删除这个目录", mp.root)
	}
	settings := pdb.settings(ctx)
	cp, err := validate(ctx, cfg, name, settings.GetSharedEnv(), mp.dataDir(), composeProjectName(id, name))
	if err != nil {
		return err
	}
	if err := checkNames(ctx, pdb, name, cfg, cp.ServiceNames()); err != nil {
		return err
	}
	for _, d := range []string{mp.dataDir(), mp.volumesDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("创建成员目录: %w", err)
		}
	}
	if _, err := ensureDeployKey(mp); err != nil {
		return err
	}
	token, err := randomToken()
	if err != nil {
		return err
	}
	if err := pdb.q.InsertMember(ctx, projectstore.InsertMemberParams{Name: name, CreatedAt: time.Now().Unix(), WebhookToken: token}); err != nil {
		return err
	}
	data, err := proto.Marshal(cfg)
	if err != nil {
		return err
	}
	if err := pdb.q.InsertVersion(ctx, projectstore.InsertVersionParams{
		Member: name, Version: 1, Config: data, SharedEnv: marshalEnv(settings.GetSharedEnv()), CreatedAt: time.Now().Unix(),
	}); err != nil {
		return err
	}
	return writeConfigFiles(mp, cfg, name, settings.GetSharedEnv())
}

// checkNames 检查名字会不会撞：单容器成员在项目内网里的名字就是成员名，compose 成员内部按服务原名互相访问，
// 所以单容器成员的名字不能和项目里任何 compose 成员的服务名相同，否则 compose 成员内部查这个名字时
// 可能查到项目内网里的单容器成员。member 是正在加入或修改的成员，services 是它的服务。
func checkNames(ctx context.Context, pdb *projectDB, member string, cfg *nagiv1.MemberConfig, services []string) error {
	rows, err := pdb.q.ListMembers(ctx)
	if err != nil {
		return err
	}
	settings := pdb.settings(ctx)
	for _, r := range rows {
		if r.Name == member {
			continue
		}
		var cfgs []*nagiv1.MemberConfig
		if c, _, err := pdb.latest(ctx, r.Name); err == nil {
			cfgs = append(cfgs, c)
		}
		if d, err := pdb.draft(ctx, r.Name); err == nil && d != nil {
			cfgs = append(cfgs, d)
		}
		for _, other := range cfgs {
			switch {
			case isSingle(cfg) && !isSingle(other):
				cp, err := loadMember(ctx, other, r.Name, settings.GetSharedEnv(), "/", "check")
				if err != nil {
					continue
				}
				if _, ok := cp.Services[member]; ok {
					return invalid("成员名 %s 和成员 %s 里的服务同名：%s 内部查 %s 时可能查到这个成员，请换个名字", member, r.Name, r.Name, member)
				}
			case !isSingle(cfg) && isSingle(other):
				for _, s := range services {
					if s == r.Name {
						return invalid("服务名 %s 和单容器成员 %s 同名：这份 compose 内部查 %s 时可能查到那个成员，请改服务名", s, r.Name, s)
					}
				}
			}
		}
	}
	return nil
}

// DeleteMember 删除成员：停止并删除它的容器，从项目里移除（版本记录一并删除）。成员目录默认保留，
// deleteDir 时一起删除。对外映射里指向它的规则失效。
func (m *Manager) DeleteMember(ctx context.Context, id, member string, baseRevision int64, deleteDir bool, onLine func(string)) (*nagiv1.Project, error) {
	row, pdb, unlock, err := m.projectForUpdate(ctx, id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	defer pdb.Close()
	mrow, err := pdb.member(ctx, member)
	if err != nil {
		return nil, err
	}
	if err := checkMemberRevision(mrow, baseRevision); err != nil {
		return nil, err
	}
	p := paths{root: row.Dir}
	if err := m.down(ctx, id, p, member, onLine); err != nil {
		return nil, err
	}
	for _, f := range []func(context.Context, string) error{pdb.q.DeleteMemberVersions, pdb.q.DeleteDraft, pdb.q.DeleteMember} {
		if err := f(ctx, member); err != nil {
			return nil, err
		}
	}
	_ = m.nodeQ.DeleteSubnet(ctx, memberOwner(id, member))
	if deleteDir {
		if err := os.RemoveAll(p.member(member).root); err != nil {
			return nil, fmt.Errorf("删除成员目录: %w", err)
		}
	}
	return pdb.project(ctx, id, row.Dir)
}

// SaveDraft 保存成员的草稿。草稿要先通过校验，部署时才会成为新版本。
func (m *Manager) SaveDraft(ctx context.Context, id, member string, baseRevision int64, cfg *nagiv1.MemberConfig) (*nagiv1.Project, error) {
	if cfg == nil {
		return nil, invalid("缺少成员配置")
	}
	cfg = proto.Clone(cfg).(*nagiv1.MemberConfig)
	row, pdb, unlock, err := m.projectForUpdate(ctx, id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	defer pdb.Close()
	mrow, err := pdb.member(ctx, member)
	if err != nil {
		return nil, err
	}
	if err := checkMemberRevision(mrow, baseRevision); err != nil {
		return nil, err
	}
	latest, _, err := pdb.latest(ctx, member)
	if err != nil {
		return nil, err
	}
	if isSingle(latest) != isSingle(cfg) {
		return nil, invalid("成员创建后不能在单容器和 compose 之间切换，请新建一个成员")
	}
	mp := paths{root: row.Dir}.member(member)
	cp, err := validate(ctx, cfg, member, pdb.settings(ctx).GetSharedEnv(), mp.dataDir(), composeProjectName(id, member))
	if err != nil {
		return nil, err
	}
	if err := checkNames(ctx, pdb, member, cfg, cp.ServiceNames()); err != nil {
		return nil, err
	}
	if err := pdb.setDraft(ctx, member, cfg); err != nil {
		return nil, err
	}
	if err := pdb.bumpMember(ctx, mrow); err != nil {
		return nil, err
	}
	return pdb.project(ctx, id, row.Dir)
}

// DiscardDraft 放弃成员的草稿。
func (m *Manager) DiscardDraft(ctx context.Context, id, member string, baseRevision int64) (*nagiv1.Project, error) {
	row, pdb, unlock, err := m.projectForUpdate(ctx, id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	defer pdb.Close()
	mrow, err := pdb.member(ctx, member)
	if err != nil {
		return nil, err
	}
	if err := checkMemberRevision(mrow, baseRevision); err != nil {
		return nil, err
	}
	if err := pdb.q.DeleteDraft(ctx, member); err != nil {
		return nil, err
	}
	if err := pdb.bumpMember(ctx, mrow); err != nil {
		return nil, err
	}
	return pdb.project(ctx, id, row.Dir)
}

// hasMember 检查项目里有这个成员。
func (m *Manager) hasMember(ctx context.Context, dir, member string) error {
	pdb, err := openProject(ctx, dir)
	if err != nil {
		return err
	}
	defer pdb.Close()
	_, err = pdb.member(ctx, member)
	return err
}

func marshalEnv(vars []*nagiv1.EnvVar) []byte {
	b, _ := proto.Marshal(&nagiv1.EnvList{Vars: vars})
	return b
}
