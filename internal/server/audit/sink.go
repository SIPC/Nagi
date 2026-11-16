package audit

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 日志外送（见 docs/audit.md#外送）：面板被攻破、机器被格式化以后，日志在别处还有一份。每个目标一个游标，
// 发送失败时游标不动，恢复后从游标处补发，不另存一份数据。每小时和每 1000 条把最新的哈希作为锚点发出去。

// SinkConfig 是一个外送目标。
type SinkConfig struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"` // syslog、http、s3
	// syslog：RFC 5424，TCP + TLS。
	Address string `json:"address,omitempty"`
	CA      string `json:"ca,omitempty"` // 自签的日志服务器的 CA（PEM），为空时用系统的根证书
	// http：每 2 秒批量 POST 一次 JSON Lines，可以带固定请求头。
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	// s3：每小时一个对象，可选同时上传录制。
	Endpoint   string `json:"endpoint,omitempty"`
	Region     string `json:"region,omitempty"`
	Bucket     string `json:"bucket,omitempty"`
	Prefix     string `json:"prefix,omitempty"`
	AccessKey  string `json:"access_key,omitempty"`
	SecretKey  string `json:"secret_key,omitempty"`
	Recordings bool   `json:"recordings,omitempty"`
}

// SinkStatus 是一个目标的发送状态。
type SinkStatus struct {
	Cursor      int64     `json:"cursor"`
	LastSuccess time.Time `json:"last_success"`
	LastError   string    `json:"last_error,omitempty"`
	FailingFrom time.Time `json:"failing_from"`
}

// Anchor 是发往外送目标的锚点：链上最新的一条记录。
type Anchor struct {
	Time time.Time `json:"time"`
	ID   int64     `json:"id"`
	Hash string    `json:"hash"`
}

const (
	sinkInterval   = 2 * time.Second
	s3Interval     = time.Hour
	anchorInterval = time.Hour
	anchorEvery    = 1000
	sinkBatch      = 500
	// sinkAlertAfter：连续失败这么久告警。
	sinkAlertAfter = 10 * time.Minute
)

// Shipper 把审计记录外送到配置的目标。
type Shipper struct {
	Log        *Log
	Recordings *Recordings
	// OnFailure 在某个目标连续失败超过 10 分钟时调用（failing=true），恢复时再调用一次。
	OnFailure func(ctx context.Context, sink SinkConfig, failing bool, err string)

	mu      sync.Mutex
	sinks   []SinkConfig
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	status  map[string]*SinkStatus
	alerted map[string]bool
	client  *http.Client
}

// Configure 换成新的目标列表，重启发送。
func (s *Shipper) Configure(ctx context.Context, sinks []SinkConfig) {
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()
	s.wg.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status == nil {
		s.status, s.alerted = map[string]*SinkStatus{}, map[string]bool{}
		s.client = &http.Client{Timeout: 30 * time.Second}
	}
	s.sinks = sinks
	ctx, s.cancel = context.WithCancel(context.WithoutCancel(ctx))
	for _, sk := range sinks {
		st := s.status[sk.ID]
		if st == nil {
			st = &SinkStatus{}
			s.status[sk.ID] = st
		}
		if c, err := strconv.ParseInt(s.Log.Meta(ctx, "sink:"+sk.ID+":cursor"), 10, 64); err == nil {
			st.Cursor = c
		} else {
			// 新目标从现在开始发，不补发以前的记录（导出可以拿到全部）。
			st.Cursor = s.lastID(ctx)
			_ = s.Log.SetMeta(ctx, "sink:"+sk.ID+":cursor", strconv.FormatInt(st.Cursor, 10))
		}
		s.wg.Add(1)
		go s.run(ctx, sk)
	}
}

// Stop 停止发送。
func (s *Shipper) Stop() {
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

// Status 返回各目标的状态。
func (s *Shipper) Status() map[string]SinkStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]SinkStatus{}
	for id, st := range s.status {
		out[id] = *st
	}
	return out
}

// Flush 等所有目标把 ID 不大于 id 的记录发出去，最多等 timeout。修改外送配置前用它把这条变更发往旧目标。
func (s *Shipper) Flush(id int64, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		done := true
		s.mu.Lock()
		for _, sk := range s.sinks {
			if st := s.status[sk.ID]; st == nil || st.Cursor < id {
				done = false
			}
		}
		s.mu.Unlock()
		if done {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

func (s *Shipper) lastID(ctx context.Context) int64 {
	var id int64
	_ = s.Log.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM events`).Scan(&id)
	return id
}

func (s *Shipper) run(ctx context.Context, sk SinkConfig) {
	defer s.wg.Done()
	interval := sinkInterval
	if sk.Type == "s3" {
		interval = s3Interval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	// 启动时先发一次锚点，之后每小时、每 1000 条一次。
	var lastAnchor time.Time
	sinceAnchor := 0
	for {
		n, err := s.sendBatch(ctx, sk, &lastAnchor, &sinceAnchor)
		s.mark(ctx, sk, err)
		if err == nil && n == sinkBatch {
			continue // 还有积压，接着发
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Shipper) mark(ctx context.Context, sk SinkConfig, err error) {
	s.mu.Lock()
	st := s.status[sk.ID]
	now := time.Now()
	var notify *bool
	if err != nil {
		if st.FailingFrom.IsZero() {
			st.FailingFrom = now
		}
		st.LastError = err.Error()
		if now.Sub(st.FailingFrom) > sinkAlertAfter && !s.alerted[sk.ID] {
			s.alerted[sk.ID] = true
			v := true
			notify = &v
		}
	} else {
		st.LastSuccess, st.LastError, st.FailingFrom = now, "", time.Time{}
		if s.alerted[sk.ID] {
			s.alerted[sk.ID] = false
			v := false
			notify = &v
		}
	}
	msg := st.LastError
	s.mu.Unlock()
	if notify != nil && s.OnFailure != nil {
		s.OnFailure(ctx, sk, *notify, msg)
	}
}

// sendBatch 发一批游标之后的记录，需要时带上锚点；成功后移动游标。返回发出的记录数。
func (s *Shipper) sendBatch(ctx context.Context, sk SinkConfig, lastAnchor *time.Time, sinceAnchor *int) (int, error) {
	s.mu.Lock()
	cursor := s.status[sk.ID].Cursor
	s.mu.Unlock()
	limit := sinkBatch
	if sk.Type == "s3" {
		limit = 1 << 20 // 每小时一个对象，装下这一个小时的全部记录
	}
	var events []Event
	rows, err := s.Log.db.QueryContext(ctx, `SELECT `+columns+` FROM events WHERE id > ? ORDER BY id LIMIT ?`, cursor, limit)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		e, err := scan(rows)
		if err != nil {
			rows.Close()
			return 0, err
		}
		events = append(events, e)
	}
	rows.Close()
	anchorDue := time.Since(*lastAnchor) >= anchorInterval || *sinceAnchor+len(events) >= anchorEvery
	if len(events) == 0 && !anchorDue {
		return 0, nil
	}
	var lines [][]byte
	for _, e := range events {
		b, _ := json.Marshal(ExportLine(e))
		lines = append(lines, b)
	}
	var anchor *Anchor
	if anchorDue {
		a := Anchor{Time: time.Now()}
		_ = s.Log.db.QueryRowContext(ctx, `SELECT id, hash FROM events ORDER BY id DESC LIMIT 1`).Scan(&a.ID, &a.Hash)
		if a.ID > 0 {
			anchor = &a
			b, _ := json.Marshal(map[string]any{"type": "anchor", "time": a.Time.UTC().Format(time.RFC3339Nano), "id": a.ID, "hash": a.Hash})
			lines = append(lines, b)
		}
	}
	if len(lines) == 0 {
		return 0, nil
	}
	if err := s.deliver(ctx, sk, events, lines); err != nil {
		return 0, err
	}
	if len(events) > 0 {
		last := events[len(events)-1].ID
		s.mu.Lock()
		s.status[sk.ID].Cursor = last
		s.mu.Unlock()
		_ = s.Log.SetMeta(ctx, "sink:"+sk.ID+":cursor", strconv.FormatInt(last, 10))
	}
	*sinceAnchor += len(events)
	if anchor != nil {
		*lastAnchor, *sinceAnchor = time.Now(), 0
		b, _ := json.Marshal(anchor)
		_ = s.Log.SetMeta(ctx, "last_anchor", string(b))
	}
	return len(events), nil
}

// LastAnchor 返回最近一次发出的锚点。
func (l *Log) LastAnchor(ctx context.Context) (Anchor, bool) {
	var a Anchor
	v := l.Meta(ctx, "last_anchor")
	return a, v != "" && json.Unmarshal([]byte(v), &a) == nil
}

// ExportLine 是外送和导出的一行：全部字段，时间用 RFC 3339 毫秒，带上哈希链。
func ExportLine(e Event) map[string]any {
	var m map[string]any
	b, _ := json.Marshal(e)
	_ = json.Unmarshal(b, &m)
	m["time"] = e.Time.UTC().Format("2006-01-02T15:04:05.000Z07:00")
	m["prev_hash"], m["hash"] = e.PrevHash, e.Hash
	return m
}

func (s *Shipper) deliver(ctx context.Context, sk SinkConfig, events []Event, lines [][]byte) error {
	switch sk.Type {
	case "http":
		return s.postHTTP(ctx, sk, lines)
	case "syslog":
		return sendSyslog(ctx, sk, lines)
	case "s3":
		first := int64(0)
		if len(events) > 0 {
			first = events[0].ID
		}
		key := strings.Trim(sk.Prefix, "/")
		if key != "" {
			key += "/"
		}
		now := time.Now().UTC()
		key += fmt.Sprintf("%s/%s-%d.jsonl", now.Format("2006/01/02"), now.Format("15"), first)
		if err := s3Put(ctx, s.client, sk, key, bytes.Join(lines, []byte("\n")), "application/x-ndjson"); err != nil {
			return err
		}
		if sk.Recordings && s.Recordings != nil {
			for _, e := range events {
				if e.RecordingID != "" {
					if err := s.uploadRecording(ctx, sk, e.RecordingID); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}
	return fmt.Errorf("unknown sink type %q", sk.Type)
}

// TestSink 发一条测试消息。
func (s *Shipper) TestSink(ctx context.Context, sk SinkConfig) error {
	line, _ := json.Marshal(map[string]any{"type": "test", "time": time.Now().UTC().Format(time.RFC3339Nano), "message": "Nagi audit sink test"})
	client := &http.Client{Timeout: 15 * time.Second}
	switch sk.Type {
	case "http":
		return postHTTP(ctx, client, sk, [][]byte{line})
	case "syslog":
		return sendSyslog(ctx, sk, [][]byte{line})
	case "s3":
		return s3Put(ctx, client, sk, strings.Trim(sk.Prefix+"/nagi-test.json", "/"), line, "application/json")
	}
	return fmt.Errorf("unknown sink type %q", sk.Type)
}

func (s *Shipper) postHTTP(ctx context.Context, sk SinkConfig, lines [][]byte) error {
	return postHTTP(ctx, s.client, sk, lines)
}

func postHTTP(ctx context.Context, client *http.Client, sk SinkConfig, lines [][]byte) error {
	body := append(bytes.Join(lines, []byte("\n")), '\n')
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sk.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	req.Header.Set("User-Agent", "nagi-audit")
	for k, v := range sk.Headers {
		req.Header.Set(k, v)
	}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<16))
	if res.StatusCode/100 != 2 {
		return fmt.Errorf("HTTP %d", res.StatusCode)
	}
	return nil
}

// sendSyslog 按 RFC 5424 发出（TCP + TLS，RFC 5425 的长度前缀分帧）。
func sendSyslog(ctx context.Context, sk SinkConfig, lines [][]byte) error {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if host, _, err := net.SplitHostPort(sk.Address); err == nil {
		cfg.ServerName = host
	}
	if sk.CA != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(sk.CA)) {
			return errors.New("CA 证书格式不正确")
		}
		cfg.RootCAs = pool
	}
	d := tls.Dialer{NetDialer: &net.Dialer{Timeout: 10 * time.Second}, Config: cfg}
	conn, err := d.DialContext(ctx, "tcp", sk.Address)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	host, _ := os.Hostname()
	var buf bytes.Buffer
	for _, l := range lines {
		// PRI 110 = facility 13（log audit）× 8 + severity 6（informational）。
		msg := fmt.Sprintf("<110>1 %s %s nagi - audit - %s", time.Now().UTC().Format(time.RFC3339Nano), host, l)
		fmt.Fprintf(&buf, "%d %s", len(msg), msg)
	}
	_, err = conn.Write(buf.Bytes())
	return err
}

// ---- S3（SigV4 签名的 PUT） ----

func (s *Shipper) uploadRecording(ctx context.Context, sk SinkConfig, id string) error {
	if !recordingIDRe.MatchString(id) {
		return nil
	}
	dir := filepath.Join(s.Recordings.Dir, id[:6], id)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil // 已经被清理
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		key := strings.Trim(sk.Prefix, "/")
		if key != "" {
			key += "/"
		}
		key += "recordings/" + id + "/" + e.Name()
		if err := s3Put(ctx, s.client, sk, key, b, "application/octet-stream"); err != nil {
			return err
		}
	}
	return nil
}

func s3Put(ctx context.Context, client *http.Client, sk SinkConfig, key string, body []byte, contentType string) error {
	endpoint := strings.TrimRight(sk.Endpoint, "/")
	if !strings.Contains(endpoint, "://") {
		endpoint = "https://" + endpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return err
	}
	// 统一用路径风格（https://<endpoint>/<bucket>/<key>），兼容 MinIO、R2、各家的 S3 兼容存储。
	u.Path = "/" + sk.Bucket + "/" + key
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	region := sk.Region
	if region == "" {
		region = "us-east-1"
	}
	signV4(req, body, sk.AccessKey, sk.SecretKey, region, time.Now().UTC())
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(res.Body, 512))
	if res.StatusCode/100 != 2 {
		return fmt.Errorf("S3 HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

func hmacSHA256(key []byte, s string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(s))
	return h.Sum(nil)
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// signV4 按 AWS Signature Version 4 签名请求。
func signV4(req *http.Request, body []byte, accessKey, secretKey, region string, now time.Time) {
	amzDate := now.Format("20060102T150405Z")
	day := now.Format("20060102")
	payload := sha256Hex(body)
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payload)
	req.Header.Set("Host", req.URL.Host)
	headers := map[string]string{"host": req.URL.Host, "x-amz-content-sha256": payload, "x-amz-date": amzDate, "content-type": req.Header.Get("Content-Type")}
	var names []string
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, k := range names {
		canonHeaders.WriteString(k + ":" + strings.TrimSpace(headers[k]) + "\n")
	}
	signed := strings.Join(names, ";")
	canonical := strings.Join([]string{req.Method, req.URL.EscapedPath(), req.URL.RawQuery, canonHeaders.String(), signed, payload}, "\n")
	scope := day + "/" + region + "/s3/aws4_request"
	toSign := strings.Join([]string{"AWS4-HMAC-SHA256", amzDate, scope, sha256Hex([]byte(canonical))}, "\n")
	k := hmacSHA256([]byte("AWS4"+secretKey), day)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, "s3")
	k = hmacSHA256(k, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(k, toSign))
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s", accessKey, scope, signed, sig))
}
