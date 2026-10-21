package nodes

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/pkg/lease"
	"github.com/SIPC/nagi/internal/pkg/mesh"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
)

// 面板作为顶替的见证人：只投票，不运行容器。设置和票按"项目 ID-容器名"存在 <面板数据目录>/witness 下，面板重启后照常投票。

var witnessIDRe = regexp.MustCompile(`^[a-f0-9]{16}-[a-z0-9-]{1,32}$`)

func witnessID(projectID, container string) string { return projectID + "-" + container }

type witness struct {
	dir   string
	voter *lease.Voter
	mu    sync.Mutex
	cfgs  map[string]*nagiv1.FailoverConfig
}

// SetWitnessDir 设置见证人数据目录并读取已有的设置。
func (r *Registry) SetWitnessDir(dir string) {
	w := &witness{dir: dir, voter: lease.NewVoter(filepath.Join(dir, "votes")), cfgs: map[string]*nagiv1.FailoverConfig{}}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !witnessIDRe.MatchString(id) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		cfg := &nagiv1.FailoverConfig{}
		if err == nil && protojson.Unmarshal(b, cfg) == nil {
			w.cfgs[id] = cfg
		}
	}
	r.mu.Lock()
	r.witness = w
	r.mu.Unlock()
}

func leaseDuration(cfg *nagiv1.FailoverConfig) time.Duration {
	if s := cfg.GetLeaseSeconds(); s >= 9 {
		return time.Duration(s) * time.Second
	}
	return 30 * time.Second
}

// SetWitness 保存本面板担任见证人的顶替设置。初始租约归 A，第 1 轮；已有设置时保留当前的票。
func (r *Registry) SetWitness(cfg *nagiv1.FailoverConfig) error {
	return r.SetWitnessFrom(cfg, cfg.GetA().GetKey(), 1)
}

// SetWitnessFrom 同 SetWitness，但新设置从 holder 的第 epoch 轮开始投票（更换见证人时用当前的持有者和轮次）。
func (r *Registry) SetWitnessFrom(cfg *nagiv1.FailoverConfig, holder []byte, epoch uint64) error {
	w := r.witness
	id := witnessID(cfg.GetProjectId(), cfg.GetContainer())
	if w == nil || !witnessIDRe.MatchString(id) {
		return status.Error(codes.InvalidArgument, "顶替设置无效")
	}
	b, err := protojson.Marshal(cfg)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(w.dir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(w.dir, id+".json"), b, 0o600); err != nil {
		return err
	}
	w.mu.Lock()
	_, existed := w.cfgs[id]
	w.cfgs[id] = cfg
	w.mu.Unlock()
	// 已有的票（例如本面板以前当过这个容器的见证人留下的）比给的持有者和轮次旧时，换成给的：
	// 否则面板会按旧的票"刚续过约"处理，一个租约时长内不投给真正的持有者。
	cur, curEpoch, _ := w.voter.State(id, leaseDuration(cfg))
	if !existed || epoch > curEpoch || (epoch == curEpoch && !bytes.Equal(cur, holder)) {
		return w.voter.Set(id, holder, epoch, leaseDuration(cfg))
	}
	return nil
}

// WitnessProjects 返回本面板担任见证人、而且 nodeID 是 A 或 B 的容器（"项目名/容器名"）。移除面板或忘记节点前检查用。
func (r *Registry) WitnessProjects(nodeID string) []string {
	w := r.witness
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	for _, cfg := range w.cfgs {
		if nodeID == "" || cfg.GetA().GetNodeId() == nodeID || cfg.GetB().GetNodeId() == nodeID {
			out = append(out, cfg.GetProjectName()+"/"+cfg.GetContainer())
		}
	}
	return out
}

// ClearWitness 删除本面板担任见证人的顶替设置。
func (r *Registry) ClearWitness(projectID, container string) {
	id := witnessID(projectID, container)
	w := r.witness
	if w == nil || !witnessIDRe.MatchString(id) {
		return
	}
	w.mu.Lock()
	delete(w.cfgs, id)
	w.mu.Unlock()
	os.Remove(filepath.Join(w.dir, id+".json"))
	w.voter.Forget(id)
}

// panelMesh 是面板在 mesh 上的服务：Ping 和见证人投票。
type panelMesh struct {
	mesh.Service
	r *Registry
}

func (p *panelMesh) LeaseVotes(ctx context.Context, req *nagiv1.LeaseVotesRequest) (*nagiv1.LeaseVotesResponse, error) {
	caller, ok := mesh.Caller(ctx, p.M)
	if !ok {
		return nil, status.Error(codes.PermissionDenied, "无法确认调用方")
	}
	if len(req.GetVotes()) > 1024 {
		return nil, status.Error(codes.InvalidArgument, "一次投票请求太多")
	}
	out := &nagiv1.LeaseVotesResponse{}
	for _, v := range req.GetVotes() {
		out.Votes = append(out.Votes, p.r.witnessVote(caller, v))
	}
	return out, nil
}

// WitnessRecord 返回面板作为见证人记下的这个容器的持有者和轮次；面板不是它的见证人时 ok 为 false。
func (r *Registry) WitnessRecord(projectID, container string) (holder []byte, epoch uint64, ok bool) {
	w := r.witness
	if w == nil {
		return nil, 0, false
	}
	id := witnessID(projectID, container)
	w.mu.Lock()
	cfg := w.cfgs[id]
	w.mu.Unlock()
	if cfg == nil {
		return nil, 0, false
	}
	holder, epoch, _ = w.voter.State(id, leaseDuration(cfg))
	return holder, epoch, true
}

// witnessVote 处理一个容器的投票请求。面板不是这个容器的见证人时不同意。
func (r *Registry) witnessVote(caller mesh.Key, req *nagiv1.LeaseVoteRequest) *nagiv1.LeaseVoteResponse {
	w := r.witness
	if w == nil {
		return &nagiv1.LeaseVoteResponse{}
	}
	id := witnessID(req.GetProjectId(), req.GetContainer())
	w.mu.Lock()
	cfg := w.cfgs[id]
	w.mu.Unlock()
	if cfg == nil {
		return &nagiv1.LeaseVoteResponse{}
	}
	parties := [][]byte{cfg.GetA().GetKey(), cfg.GetB().GetKey()}
	if to := req.GetHandoverTo(); len(to) > 0 {
		// 持有者把租约转让给另一方（自动切回原节点）。
		granted, holder, epoch := w.voter.Handover(id, parties, caller[:], to, req.GetEpoch(), leaseDuration(cfg))
		return &nagiv1.LeaseVoteResponse{Granted: granted, Holder: holder, Epoch: epoch}
	}
	granted, holder, epoch := w.voter.Vote(id, parties, caller[:], req.GetEpoch(), leaseDuration(cfg))
	return &nagiv1.LeaseVoteResponse{Granted: granted, Holder: holder, Epoch: epoch}
}
