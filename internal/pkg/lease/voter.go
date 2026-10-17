// Package lease 实现顶替的租约投票（见 docs/failover.md#租约与三方多数票）。
//
// 每个设置了顶替的项目有三个投票者：A、B 和见证人。只有拿到 3 票中 2 票的一方才能运行项目。
// 投票就是租约：投票者把票投给一方，持有者定期续约；超过租约时长没有续约，这张票作废，可以投给别人。
// 每次换人运行，轮次（epoch）加一，旧持有者看到更高的轮次就知道自己已经被顶替。
//
// 投给谁、第几轮写在磁盘上。投票者重启后不知道上次续约是什么时候，所以按"刚刚续过约"处理：
// 重启后的一个租约时长内只给原来的持有者续约，不投给别人。这样投票者重启不会造成两边同时运行。
package lease

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

var idRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

type vote struct {
	For     []byte    `json:"for"`
	Epoch   uint64    `json:"epoch"`
	expires time.Time // 只在内存里，用本机的单调时钟
}

// Voter 是一个投票者，管理它参与的所有项目的票。
type Voter struct {
	dir string
	mu  sync.Mutex
	v   map[string]*vote
}

// NewVoter 创建投票者，票存在 dir 下。
func NewVoter(dir string) *Voter {
	return &Voter{dir: dir, v: map[string]*vote{}}
}

func (v *Voter) file(project string) string { return filepath.Join(v.dir, project+".vote.json") }

// load 读取项目的票；第一次读取时按"刚刚续过约"处理（见包注释）。调用方持有 v.mu。
func (v *Voter) load(project string, lease time.Duration) *vote {
	if x := v.v[project]; x != nil {
		return x
	}
	x := &vote{}
	if b, err := os.ReadFile(v.file(project)); err == nil && json.Unmarshal(b, x) == nil && len(x.For) > 0 {
		x.expires = time.Now().Add(lease)
	}
	v.v[project] = x
	return x
}

func (v *Voter) save(project string, x *vote) error {
	if err := os.MkdirAll(v.dir, 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(x)
	tmp := v.file(project) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, v.file(project))
}

// Vote 处理 candidate 对第 epoch 轮租约的请求。members 是允许运行项目的两方（A、B）。
// 返回是否同意，以及投票之后这张票投给谁、第几轮。
//   - 同一方、同一轮：续约。
//   - 更高的轮次：只有当前的票已经作废（超过租约时长没续约）或者还没投过时才同意。
func (v *Voter) Vote(project string, members [][]byte, candidate []byte, epoch uint64, lease time.Duration) (bool, []byte, uint64) {
	if !idRe.MatchString(project) {
		return false, nil, 0
	}
	allowed := false
	for _, m := range members {
		if bytes.Equal(m, candidate) {
			allowed = true
		}
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	x := v.load(project, lease)
	if !allowed {
		return false, x.For, x.Epoch
	}
	now := time.Now()
	switch {
	case bytes.Equal(x.For, candidate) && epoch == x.Epoch:
		x.expires = now.Add(lease)
		return true, x.For, x.Epoch
	case epoch > x.Epoch && (len(x.For) == 0 || now.After(x.expires)):
		nx := &vote{For: append([]byte(nil), candidate...), Epoch: epoch, expires: now.Add(lease)}
		if err := v.save(project, nx); err != nil {
			return false, x.For, x.Epoch
		}
		v.v[project] = nx
		return true, nx.For, nx.Epoch
	}
	return false, x.For, x.Epoch
}

// Set 直接设置票（设置顶替时，初始租约归 A；或者知道了更新的持有者）。
func (v *Voter) Set(project string, holder []byte, epoch uint64, lease time.Duration) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	x := &vote{For: append([]byte(nil), holder...), Epoch: epoch, expires: time.Now().Add(lease)}
	if err := v.save(project, x); err != nil {
		return err
	}
	v.v[project] = x
	return nil
}

// State 返回当前的票：投给谁、第几轮、是否已经作废。
func (v *Voter) State(project string, lease time.Duration) (holder []byte, epoch uint64, expired bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	x := v.load(project, lease)
	return x.For, x.Epoch, len(x.For) == 0 || time.Now().After(x.expires)
}

// Forget 删除项目的票（取消顶替时）。
func (v *Voter) Forget(project string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.v, project)
	os.Remove(v.file(project))
}
