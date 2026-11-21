package standby

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"maps"
	"net"
	"time"

	"github.com/SIPC/nagi/internal/pkg/tlsutil"
	"github.com/SIPC/nagi/internal/server/audit"
)

// 审计日志的增量同步（见 docs/audit.md#备用面板）：主面板的数据库快照不带审计库，审计记录另走一条连接
// （ALPN nagi-standby-audit），按 id 增量同步，断线后从备用面板已有的最大 id 续传。录制的块随引用它的记录一起发。

// AuditALPN 是审计同步连接的 TLS 应用层协议名。
const AuditALPN = "nagi-standby-audit/1"

// auditBatchSize 是每帧最多带的记录数。
const auditBatchSize = 200

type auditHello struct {
	After int64 `json:"after"`
}

// ServeAudit 处理备用面板的审计同步连接：读到备用面板已有的最大 id，之后持续发新的记录。
func (p *Primary) ServeAudit(ctx context.Context, conn *tls.Conn, log *audit.Log, rec *audit.Recordings) {
	defer conn.Close()
	state := conn.ConnectionState()
	if len(state.VerifiedChains) == 0 || len(state.PeerCertificates) == 0 {
		return
	}
	if _, err := p.Q.GetStandbyPanelByServerFP(ctx, tlsutil.Fingerprint(state.PeerCertificates[0])); err != nil {
		return
	}
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	b, err := readFrame(conn)
	if err != nil {
		return
	}
	var hello auditHello
	if json.Unmarshal(b, &hello) != nil {
		return
	}
	cursor := hello.After
	lastSent := time.Now()
	var sentCursors map[string]int64
	for ctx.Err() == nil {
		batch, frame, err := log.ReadBatch(ctx, cursor, auditBatchSize, rec)
		if err != nil {
			return
		}
		if len(batch.Events) == 0 && len(batch.Tombstones) == 0 {
			// 追上了：隔一会儿再看。外送游标变了就发过去，否则按时发心跳。
			if c := log.SinkCursors(ctx); len(c) > 0 && !maps.Equal(c, sentCursors) {
				_ = conn.SetWriteDeadline(time.Now().Add(time.Minute))
				if writeFrame(conn, audit.CursorFrame(c)) != nil {
					return
				}
				sentCursors, lastSent = c, time.Now()
			} else if time.Since(lastSent) >= heartbeat {
				_ = conn.SetWriteDeadline(time.Now().Add(time.Minute))
				if writeFrame(conn, nil) != nil {
					return
				}
				lastSent = time.Now()
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}
		_ = conn.SetWriteDeadline(time.Now().Add(time.Minute))
		if writeFrame(conn, frame) != nil {
			return
		}
		lastSent = time.Now()
		if n := len(batch.Events); n > 0 {
			cursor = batch.Events[n-1].ID
		}
		for _, t := range batch.Tombstones {
			cursor = max(cursor, t.ID)
		}
	}
}

// RunAudit 从主面板增量同步审计日志，直到 ctx 取消。
func (c *Client) RunAudit(ctx context.Context, log *audit.Log, rec *audit.Recordings) {
	backoff := time.Second
	for ctx.Err() == nil {
		err := c.syncAuditOnce(ctx, log, rec)
		if err != nil && ctx.Err() == nil {
			c.Log.Warn("审计日志同步断开，稍后重连", "err", err, "retry_in", backoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (c *Client) syncAuditOnce(ctx context.Context, log *audit.Log, rec *audit.Recordings) error {
	cert, err := clientCert(c.DataDir)
	if err != nil {
		return err
	}
	cfg := tlsutil.PinnedConfig(c.Info.PrimaryFingerprint)
	cfg.Certificates = []tls.Certificate{cert}
	cfg.NextProtos = []string{AuditALPN}
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	conn, err := tls.DialWithDialer(d, "tcp", c.PrimaryAddress(), cfg)
	if err != nil {
		return err
	}
	defer conn.Close()
	go func() {
		<-ctx.Done()
		conn.Close()
	}()
	hello, _ := json.Marshal(auditHello{After: log.MaxID(ctx)})
	if err := writeFrame(conn, hello); err != nil {
		return err
	}
	for {
		_ = conn.SetReadDeadline(time.Now().Add(3 * heartbeat))
		frame, err := readFrame(conn)
		if err != nil {
			return err
		}
		if len(frame) == 0 {
			continue
		}
		if _, err := log.ApplyFrame(ctx, frame, rec); err != nil {
			return err
		}
	}
}
