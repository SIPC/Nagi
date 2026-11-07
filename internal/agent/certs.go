package agent

import (
	"context"
	"slices"
	"sync"
	"time"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// 域名的证书状态，上报给面板显示（见 docs/exposure.md#域名检查）。

// certErrorMax 是上报的证书申请失败原因的最大长度：ACME 的错误信息可能很长，每次上报都要带。
const certErrorMax = 500

// certRetryEvery 是手动重试申请证书的最短间隔。重试会让本节点所有还没拿到证书的域名都重新申请，
// 而 Let's Encrypt 对同一个域名每小时只允许 5 次失败的验证，连续点几下就用完了。
const certRetryEvery = time.Minute

// certFailures 记着各域名最近一次申请证书失败的原因，申请成功时清掉。只在内存里：重启后 Caddy 会重新申请。
type certFailures struct {
	mu      sync.Mutex
	m       map[string]string
	retried time.Time // 上次手动重试的时间
}

// beginRetry 开始一次手动重试：清掉这些域名的失败记录（节点下次上报时显示申请中）。
// 离上次重试太近时不做，返回还要等多久。
func (f *certFailures) beginRetry(domains []string, now time.Time) time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	if wait := certRetryEvery - now.Sub(f.retried); wait > 0 {
		return wait
	}
	f.retried = now
	for _, d := range domains {
		delete(f.m, d)
	}
	return 0
}

func (f *certFailures) set(domain string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil {
		delete(f.m, domain)
		return
	}
	if f.m == nil {
		f.m = map[string]string{}
	}
	msg := err.Error()
	if len(msg) > certErrorMax {
		msg = msg[:certErrorMax]
	}
	f.m[domain] = msg
}

func (f *certFailures) get(domain string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.m[domain]
}

func (s *agentService) RetryCerts(_ context.Context, req *nagiv1.RetryCertsRequest) (*nagiv1.RetryCertsResponse, error) {
	if s.node.proxy == nil {
		return nil, status.Error(codes.FailedPrecondition, "节点没有启动 Caddy，无法申请证书")
	}
	if wait := s.node.certFails.beginRetry(req.GetDomains(), time.Now()); wait > 0 {
		if n := int(wait.Seconds()) + 1; n > 1 {
			return nil, status.Errorf(codes.ResourceExhausted, "刚重试过，请 %d 秒后重试", n)
		}
		return nil, status.Error(codes.ResourceExhausted, "刚重试过，请 1 秒后重试")
	}
	if err := s.node.proxy.Reload(); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	s.changed()
	return &nagiv1.RetryCertsResponse{}, nil
}

// certStatuses 返回本节点开放的域名（只开 HTTP 的除外）的证书状态。没有启动 Caddy 时返回空。
func (a *agent) certStatuses(ctx context.Context) []*nagiv1.DomainCert {
	if a.proxy == nil {
		return nil
	}
	routes, err := a.allRoutes(ctx)
	if err != nil {
		return nil
	}
	var domains []string
	for _, r := range routes {
		if !r.HTTPOnly {
			domains = append(domains, r.Domains...)
		}
	}
	slices.Sort(domains)
	domains = slices.Compact(domains)
	expiries := a.alerts.CertExpiries()
	out := make([]*nagiv1.DomainCert, 0, len(domains))
	for _, d := range domains {
		c := &nagiv1.DomainCert{Domain: d, Error: a.certFails.get(d)}
		switch {
		case a.proxy.HasCert(d):
			c.State = "obtained"
			if t, ok := expiries[d]; ok {
				c.NotAfter = timestamppb.New(t)
			}
		case c.Error != "":
			c.State = "failed"
		default:
			c.State = "pending"
		}
		out = append(out, c)
	}
	return out
}
