package auditsink

// 审计日志外送的传输（见 docs/audit.md#外送）：syslog（TCP + TLS）、HTTP（JSON Lines）、S3（SigV4）。
// 面板外送审计库（server/audit）和节点外送本地审计（agent/auditlog）共用。

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/SIPC/nagi/internal/pkg/i18n"
)

// Config 是一个外送目标。
type Config struct {
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

// PostHTTP 把若干行作为 JSON Lines POST 出去。
func PostHTTP(ctx context.Context, client *http.Client, sk Config, lines [][]byte) error {
	body := append(bytes.Join(lines, []byte("\n")), '\n')
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sk.URL, bytes.NewReader(body))
	if err != nil {
		return i18n.New("接收地址格式不正确")
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	req.Header.Set("User-Agent", "nagi-audit")
	for k, v := range sk.Headers {
		req.Header.Set(k, v)
	}
	res, err := client.Do(req)
	if err != nil {
		return unreachable(req.URL.Host)
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<16))
	if res.StatusCode/100 != 2 {
		return i18n.Errorf("接收端返回 HTTP %d，请检查接收地址和请求头", res.StatusCode)
	}
	return nil
}

// SendSyslog 按 RFC 5424 发出（TCP + TLS，RFC 5425 的长度前缀分帧）。
func SendSyslog(ctx context.Context, sk Config, lines [][]byte) error {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if host, _, err := net.SplitHostPort(sk.Address); err == nil {
		cfg.ServerName = host
	}
	if sk.CA != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(sk.CA)) {
			return i18n.New("CA 证书格式不正确，须为 PEM 格式")
		}
		cfg.RootCAs = pool
	}
	d := tls.Dialer{NetDialer: &net.Dialer{Timeout: 10 * time.Second}, Config: cfg}
	conn, err := d.DialContext(ctx, "tcp", sk.Address)
	if err != nil {
		var cert *tls.CertificateVerificationError
		if errors.As(err, &cert) {
			return i18n.Errorf("%s 的证书无法验证，自签证书须填写 CA 证书", sk.Address)
		}
		return unreachable(sk.Address)
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

// Send 把若干行发到目标。S3 每批一个对象，键见 ObjectKey。scope 是 PanelScope 或 NodeScope(节点 ID)。
func Send(ctx context.Context, client *http.Client, sk Config, scope string, first int64, lines [][]byte) error {
	switch sk.Type {
	case "http":
		return PostHTTP(ctx, client, sk, lines)
	case "syslog":
		return SendSyslog(ctx, sk, lines)
	case "s3":
		return S3Put(ctx, client, sk, ObjectKey(sk.Prefix, scope, first, time.Now()), bytes.Join(lines, []byte("\n")), "application/x-ndjson")
	}
	return fmt.Errorf("unknown sink type %q", sk.Type)
}

// PanelScope 是面板自己的记录在 S3 里的目录。
const PanelScope = "panel"

// NodeScope 是节点本地审计在 S3 里的目录：node/<节点 ID>。面板和各节点的键分开，
// 同一小时、序号相同的批次不会互相覆盖（见 docs/audit.md#外送）。ID 里只留字母、数字、- 和 _，不能借它跳到别的目录。
func NodeScope(nodeID string) string {
	id := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return -1
	}, nodeID)
	if id == "" {
		id = "unknown"
	}
	return "node/" + id
}

// ObjectKey 是一批记录的对象键：<前缀>/<scope>/<年>/<月>/<日>/<时>-<first>.jsonl，first 是批里第一条的序号。
func ObjectKey(prefix, scope string, first int64, now time.Time) string {
	now = now.UTC()
	return ScopedKey(prefix, scope, fmt.Sprintf("%s/%s-%d.jsonl", now.Format("2006/01/02"), now.Format("15"), first))
}

// ScopedKey 拼出 <前缀>/<scope>/<name>。
func ScopedKey(prefix, scope, name string) string {
	key := strings.Trim(prefix, "/")
	if key != "" {
		key += "/"
	}
	return key + scope + "/" + name
}

// S3Put 用 SigV4 签名 PUT 一个对象。
func S3Put(ctx context.Context, client *http.Client, sk Config, key string, body []byte, contentType string) error {
	endpoint := strings.TrimRight(sk.Endpoint, "/")
	if !strings.Contains(endpoint, "://") {
		endpoint = "https://" + endpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return i18n.New("存储地址格式不正确")
	}
	// 统一用路径风格（https://<endpoint>/<bucket>/<key>），兼容 MinIO、R2、各家的 S3 兼容存储。
	u.Path = "/" + sk.Bucket + "/" + key
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.String(), bytes.NewReader(body))
	if err != nil {
		return i18n.New("存储地址格式不正确")
	}
	req.Header.Set("Content-Type", contentType)
	region := sk.Region
	if region == "" {
		region = "us-east-1"
	}
	signV4(req, body, sk.AccessKey, sk.SecretKey, region, time.Now().UTC())
	res, err := client.Do(req)
	if err != nil {
		return unreachable(u.Host)
	}
	defer res.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
	if res.StatusCode/100 != 2 {
		// 只带 S3 的错误码（<Code>），不回显响应体：地址可能指向内网服务，响应体会经测试外送和失败原因被读出来。
		if m := s3CodeRe.FindSubmatch(msg); m != nil {
			return i18n.Errorf("存储返回 HTTP %d（%s），请检查存储桶、区域和密钥", res.StatusCode, string(m[1]))
		}
		return i18n.Errorf("存储返回 HTTP %d，请检查存储桶、区域和密钥", res.StatusCode)
	}
	return nil
}

// s3CodeRe 取 S3 错误响应里的错误码，例如 AccessDenied、NoSuchBucket。
var s3CodeRe = regexp.MustCompile(`<Code>([A-Za-z0-9.]{1,64})</Code>`)

// NewClient 返回外送用的 HTTP 客户端：不跟随重定向。接收端返回 3xx 时，跟随会把审计正文连同自定义请求头
// 发到重定向的地址（跨域时 Go 只去掉 Authorization 和 Cookie）。
func NewClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
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

// unreachable 是连不上目标时的错误。只写主机和端口：Go 的错误里带着完整地址，地址里可能有令牌，
// 不能显示在界面上，也不能写进日志。
func unreachable(host string) error {
	return i18n.Errorf("无法连接 %s，请检查地址和网络", host)
}
