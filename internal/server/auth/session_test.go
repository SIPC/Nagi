package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/pkg/db"
	"github.com/SIPC/nagi/internal/server/audit"
	"github.com/SIPC/nagi/internal/server/store"
)

const (
	chromeUA  = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	chromeFP  = "t13d1516h2_8daaf6152771_02713d6af862"
	firefoxUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:133.0) Gecko/20100101 Firefox/133.0"
	// Go 的 net/http 客户端。
	goFP = "t13d1312h2_f57a46bbacb6_e7c285222651"
	// curl（OpenSSL）。
	curlFP = "t13d3112h2_e8f1e7e78f70_6bebaf5329ac"
)

type sessionHarness struct {
	t      *testing.T
	s      *Service
	q      *store.Queries
	d      *sql.DB
	log    *audit.Log
	mu     sync.Mutex
	alerts []string
}

func newSessionHarness(t *testing.T) *sessionHarness {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	d, err := db.OpenMigrated(ctx, filepath.Join(dir, "panel.db"), store.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	l, err := audit.Open(ctx, filepath.Join(dir, "audit.db"), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	q := store.New(d)
	h := &sessionHarness{t: t, q: q, d: d, log: l, s: New(q, plainSealer{})}
	h.s.SetAudit(l)
	h.s.Alert = func(_ context.Context, key, title, msg string) {
		h.mu.Lock()
		h.alerts = append(h.alerts, msg)
		h.mu.Unlock()
	}
	if _, err := h.s.CreateUser(ctx, "admin", "correct horse", RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.CreateUser(ctx, "op", "correct horse", RoleOperator); err != nil {
		t.Fatal(err)
	}
	return h
}

// login 登录，返回令牌。
func (h *sessionHarness) login(user string, info RequestInfo) string {
	h.t.Helper()
	ctx := WithMeta(context.Background(), Meta{IP: info.IP, UserAgent: info.UserAgent, Fingerprint: info.Fingerprint})
	c, err := h.s.Login(ctx, info.IP, user, "correct horse", "")
	if err != nil {
		h.t.Fatal(err)
	}
	return c.Value
}

func (h *sessionHarness) check(token string, info RequestInfo) (sessionResult, error) {
	return h.s.authenticate(context.Background(), token, info)
}

// shift 把会话的时间往前拨：轮换时间、挑战时间、登记密钥的截止时间。
func (h *sessionHarness) age(token string, d time.Duration) {
	h.t.Helper()
	st, _, _, err := h.q.SessionByToken(context.Background(), HashToken(token))
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.q.UpdateSession(context.Background(), st.ID, map[string]any{
		"rotated_at": st.RotatedAt - int64(d/time.Second), "challenge_at": st.ChallengeAt - int64(d/time.Second), "key_until": st.KeyUntil - int64(d/time.Second),
	}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *sessionHarness) alerted(substr string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, a := range h.alerts {
		if strings.Contains(a, substr) {
			return true
		}
	}
	return false
}

// browserKey 模拟页面的会话密钥（WebCrypto ECDSA P-256）。
type browserKey struct{ k *ecdsa.PrivateKey }

func newBrowserKey() browserKey {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	return browserKey{k}
}

func (b browserKey) spki() []byte {
	der, _ := x509.MarshalPKIXPublicKey(&b.k.PublicKey)
	return der
}

// sign 按 WebCrypto 的格式签名：r‖s 各 32 字节。
func (b browserKey) sign(msg string) []byte {
	sum := sha256.Sum256([]byte(msg))
	r, s, _ := ecdsa.Sign(rand.Reader, b.k, sum[:])
	out := make([]byte, 64)
	r.FillBytes(out[:32])
	s.FillBytes(out[32:])
	return out
}

var (
	home   = RequestInfo{IP: "203.0.113.10", UserAgent: chromeUA, Fingerprint: chromeFP}
	office = RequestInfo{IP: "198.51.100.20", UserAgent: chromeUA, Fingerprint: chromeFP}
)

// rotate 走一遍轮换：到时间后的请求带回挑战，签名后换发新令牌，新令牌转正。
func (h *sessionHarness) rotate(token string, key browserKey, info RequestInfo) string {
	h.t.Helper()
	h.age(token, RotateInterval)
	res, err := h.check(token, info)
	if err != nil || res.challenge == "" {
		h.t.Fatalf("到时间后没有挑战：%v", err)
	}
	next, err := h.s.RotateSession(context.Background(), token, key.sign(res.challenge), info)
	if err != nil {
		h.t.Fatal(err)
	}
	// 新令牌转正之前旧令牌照常可用。
	if _, err := h.check(token, info); err != nil {
		h.t.Fatalf("新令牌转正之前旧令牌不能用：%v", err)
	}
	if _, err := h.check(next, info); err != nil {
		h.t.Fatalf("新令牌不能用：%v", err)
	}
	return next
}

// 主人和盗用者都在用：主人轮换以后，盗用者手里的令牌过了 60 秒宽限期再用，整个会话退出并告警。
func TestSessionReuseDetected(t *testing.T) {
	h := newSessionHarness(t)
	ctx := context.Background()
	token := h.login("admin", home)
	key := newBrowserKey()
	if err := h.s.RegisterSessionKey(ctx, token, key.spki()); err != nil {
		t.Fatal(err)
	}
	next := h.rotate(token, key, home)
	// 宽限期内的并发请求照常。
	if _, err := h.check(token, home); err != nil {
		t.Fatalf("宽限期内的旧令牌：%v", err)
	}
	// 宽限期过后，盗用者（另一台机器，同一种浏览器）还拿着旧令牌。
	if _, err := h.q.SessionByID(ctx, mustSession(t, h, next).ID); err != nil {
		t.Fatal(err)
	}
	st := mustSession(t, h, next)
	if _, err := h.d.ExecContext(ctx, `UPDATE session_tokens SET retired_at = retired_at - 120 WHERE session_id = ?`, st.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.check(token, office); !errors.Is(err, errLoggedOut) {
		t.Fatalf("重用旧令牌应退出会话：%v", err)
	}
	if _, err := h.check(next, home); err == nil {
		t.Fatal("整个会话应已退出")
	}
	if !h.alerted("可能被盗用") {
		t.Fatalf("没有告警：%v", h.alerts)
	}
}

func mustSession(t *testing.T, h *sessionHarness, token string) store.SessionState {
	t.Helper()
	st, _, _, err := h.q.SessionByToken(context.Background(), HashToken(token))
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// 只复制了 cookie、主人不再使用：到了轮换时间拿不出签名，要求重新验证，并告警。
func TestSessionStolenCookieWithoutKey(t *testing.T) {
	h := newSessionHarness(t)
	ctx := context.Background()
	token := h.login("op", home)
	if err := h.s.RegisterSessionKey(ctx, token, newBrowserKey().spki()); err != nil {
		t.Fatal(err)
	}
	// 盗用者在另一个网络上用同一种浏览器：操作员默认不绑定网络，能用到轮换时间。
	if _, err := h.check(token, office); err != nil {
		t.Fatalf("轮换之前：%v", err)
	}
	h.age(token, RotateInterval)
	res, err := h.check(token, office)
	if err != nil || res.challenge == "" {
		t.Fatalf("应收到挑战：%v", err)
	}
	// 用自己的密钥签名：对不上。
	if _, err := h.s.RotateSession(ctx, token, newBrowserKey().sign(res.challenge), office); err == nil {
		t.Fatal("签名对不上也换发了令牌")
	}
	var rv *ErrReverify
	if _, err := h.check(token, office); !errors.As(err, &rv) {
		t.Fatalf("应要求重新验证：%v", err)
	}
	if !h.alerted("会话密钥") {
		t.Fatalf("没有告警：%v", h.alerts)
	}
	// 也不签名：挑战过期以后同样要重新验证。
	token2 := h.login("op", home)
	_ = h.s.RegisterSessionKey(ctx, token2, newBrowserKey().spki())
	h.age(token2, RotateInterval)
	if res, _ := h.check(token2, office); res.challenge == "" {
		t.Fatal("应收到挑战")
	}
	h.age(token2, challengeGrace+time.Second)
	if _, err := h.check(token2, office); !errors.As(err, &rv) {
		t.Fatalf("挑战过期应要求重新验证：%v", err)
	}
	// 盗用者过不了重新验证：连续 3 次失败退出会话。
	for i := range reverifyLimit {
		err := h.s.Reverify(ctx, token2, "guess", "", office)
		if err == nil {
			t.Fatal("密码不对也通过了")
		}
		if i == reverifyLimit-1 && !strings.Contains(err.Error(), "已退出") {
			t.Fatalf("第 %d 次失败：%v", i+1, err)
		}
	}
	if _, err := h.check(token2, home); err == nil || errors.As(err, &rv) {
		t.Fatalf("会话应已退出：%v", err)
	}
}

// 主人自己的浏览器丢了私钥（清除网站数据以外的情况）：重新验证后可以重新登记，会话继续。
func TestSessionReverifyRestoresKey(t *testing.T) {
	h := newSessionHarness(t)
	ctx := context.Background()
	token := h.login("admin", home)
	h.age(token, RotateInterval+keyWindow)
	var rv *ErrReverify
	if _, err := h.check(token, home); !errors.As(err, &rv) || rv.Reason != ReasonNoKey {
		t.Fatalf("没登记密钥应要求重新验证：%v", err)
	}
	if err := h.s.Reverify(ctx, token, "correct horse", "", home); err != nil {
		t.Fatal(err)
	}
	key := newBrowserKey()
	if err := h.s.RegisterSessionKey(ctx, token, key.spki()); err != nil {
		t.Fatalf("重新验证后应能重新登记：%v", err)
	}
	if err := h.s.RegisterSessionKey(ctx, token, newBrowserKey().spki()); err == nil {
		t.Fatal("登记过以后不能换成别的密钥")
	}
	h.rotate(token, key, home)
}

// 标识是浏览器、TLS 握手却来自脚本工具（用 curl 带上 cookie）：直接退出并告警。
func TestSessionToolFingerprint(t *testing.T) {
	for _, fp := range []string{goFP, curlFP, "t13d1714h1_aaaaaaaaaaaa_bbbbbbbbbbbb"} {
		h := newSessionHarness(t)
		token := h.login("admin", home)
		if _, err := h.check(token, RequestInfo{IP: home.IP, UserAgent: chromeUA, Fingerprint: fp}); !errors.Is(err, errLoggedOut) {
			t.Fatalf("%s：应退出：%v", fp, err)
		}
		if !h.alerted("脚本工具") {
			t.Fatalf("没有告警：%v", h.alerts)
		}
	}
	// 标识没变、密码套件变了（换了一个客户端）：重新验证，不告警。
	h := newSessionHarness(t)
	token := h.login("admin", home)
	var rv *ErrReverify
	if _, err := h.check(token, RequestInfo{IP: home.IP, UserAgent: chromeUA, Fingerprint: "t13d1516h2_99999999999a_02713d6af862"}); !errors.As(err, &rv) || rv.Reason != ReasonTLSChanged {
		t.Fatalf("应要求重新验证：%v", err)
	}
	if len(h.alerts) != 0 {
		t.Fatalf("不应告警：%v", h.alerts)
	}
}

// 同一个浏览器的握手只是扩展不同（会话恢复多出 pre_shared_key、有的握手带 padding）、或者走了 HTTP/3：
// 不要求重新验证。手机网络下重连频繁，以前按完整指纹比较会反复弹出验证。
func TestSessionTLSResumption(t *testing.T) {
	h := newSessionHarness(t)
	token := h.login("admin", home)
	for _, fp := range []string{
		"t13d1517h2_8daaf6152771_cb7bf5808d99", // 会话恢复：多一个扩展，扩展哈希不同
		"t13d1518h2_8daaf6152771_1234567890ab", // 恢复并带 early_data
		chromeFP,
	} {
		if _, err := h.check(token, RequestInfo{IP: home.IP, UserAgent: chromeUA, Fingerprint: fp}); err != nil {
			t.Fatalf("%s：不应要求重新验证：%v", fp, err)
		}
	}
	if looksLikeTool("t13d1516h3_8daaf6152771_02713d6af862") {
		t.Fatal("HTTP/3 的握手不是脚本工具")
	}
}

// 浏览器或操作系统变了：退出；只是版本号变了（自动更新）不受影响。
func TestSessionDeviceChange(t *testing.T) {
	h := newSessionHarness(t)
	token := h.login("admin", home)
	updated := RequestInfo{IP: home.IP, UserAgent: strings.Replace(chromeUA, "131.0.0.0", "132.0.0.0", 1), Fingerprint: "t13d1516h2_8daaf6152771_0271aaaaaaaa"}
	if _, err := h.check(token, updated); err != nil {
		t.Fatalf("浏览器更新后应照常可用：%v", err)
	}
	if _, err := h.check(token, RequestInfo{IP: home.IP, UserAgent: firefoxUA}); !errors.Is(err, errLoggedOut) {
		t.Fatalf("换了浏览器应退出：%v", err)
	}
	if !h.alerted("另一种浏览器") {
		t.Fatalf("没有告警：%v", h.alerts)
	}
}

// 手机在 Wi-Fi 和移动网络之间切换一次不退出；5 分钟内来回切换要重新验证。IPv4 和 IPv6 之间切换不算。
func TestSessionNetworkFlip(t *testing.T) {
	h := newSessionHarness(t)
	wifi := RequestInfo{IP: "203.0.113.10", UserAgent: chromeUA}
	cell := RequestInfo{IP: "198.51.100.77", UserAgent: chromeUA}
	v6 := RequestInfo{IP: "2001:db8:1::5", UserAgent: chromeUA}
	token := h.login("op", wifi)
	if _, err := h.check(token, v6); err != nil {
		t.Fatalf("切到 IPv6：%v", err)
	}
	if _, err := h.check(token, wifi); err != nil {
		t.Fatalf("切回 IPv4：%v", err)
	}
	if _, err := h.check(token, cell); err != nil {
		t.Fatalf("切换一次不应受影响：%v", err)
	}
	var rv *ErrReverify
	if _, err := h.check(token, wifi); !errors.As(err, &rv) || rv.Reason != ReasonNetworkFlip {
		t.Fatalf("来回切换应要求重新验证：%v", err)
	}
	if !h.alerted("来回切换") {
		t.Fatalf("没有告警：%v", h.alerts)
	}
}

// 管理员的网段变了要重新验证（不告警）；同一网段里换 IP 不受影响。高权限确认只对确认时的 IP 有效。
func TestSessionAdminNetwork(t *testing.T) {
	h := newSessionHarness(t)
	ctx := context.Background()
	token := h.login("admin", home)
	if _, err := h.s.Elevate(ctx, home.IP, token, "correct horse", ""); err != nil {
		t.Fatal(err)
	}
	res, err := h.check(token, RequestInfo{IP: "203.0.113.99", UserAgent: chromeUA, Fingerprint: chromeFP})
	if err != nil {
		t.Fatalf("同一网段：%v", err)
	}
	if res.user.Elevated() {
		t.Fatal("IP 变了以后高权限确认应失效")
	}
	var rv *ErrReverify
	if _, err := h.check(token, office); !errors.As(err, &rv) || rv.Reason != ReasonNetworkChanged {
		t.Fatalf("管理员换了网段应要求重新验证：%v", err)
	}
	if len(h.alerts) != 0 {
		t.Fatalf("不应告警：%v", h.alerts)
	}
	if err := h.s.Reverify(ctx, token, "correct horse", "", office); err != nil {
		t.Fatal(err)
	}
	if _, err := h.check(token, office); err != nil {
		t.Fatalf("重新验证以后：%v", err)
	}
}

// 空闲超过 24 小时退出。
func TestSessionIdle(t *testing.T) {
	h := newSessionHarness(t)
	token := h.login("admin", home)
	st := mustSession(t, h, token)
	_ = h.q.UpdateSession(context.Background(), st.ID, map[string]any{"last_seen": time.Now().Add(-25 * time.Hour).Unix()})
	if _, err := h.check(token, home); err == nil || !strings.Contains(err.Error(), "过期") {
		t.Fatalf("空闲超时应退出：%v", err)
	}
}

func TestJA4(t *testing.T) {
	// Chrome 风格：GREASE、SNI、h2，扩展排序后算哈希。
	h := &tls.ClientHelloInfo{
		CipherSuites:      []uint16{0x0a0a, 0x1301, 0x1302, 0x1303, 0xc02b},
		ServerName:        "panel.example.com",
		SupportedProtos:   []string{"h2", "http/1.1"},
		SupportedVersions: []uint16{0x2a2a, tls.VersionTLS13, tls.VersionTLS12},
		Extensions:        []uint16{0x1a1a, 0x0000, 0x0017, 0x0010, 0x000d, 0x002b},
		SignatureSchemes:  []tls.SignatureScheme{0x0403, 0x0804},
	}
	fp := JA4(h)
	if !strings.HasPrefix(fp, "t13d0405h2_") {
		t.Fatalf("JA4 = %s", fp)
	}
	if looksLikeTool(chromeFP) || !looksLikeTool(goFP) || !looksLikeTool(curlFP) {
		t.Fatal("指纹分类不对")
	}
}

func TestParseAgent(t *testing.T) {
	for ua, want := range map[string]string{
		chromeUA:  "Chrome · Windows",
		firefoxUA: "Firefox · Windows",
		"Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1": "Safari · iOS",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36 Edg/131.0.0.0":     "Edge · macOS",
		"curl/8.5.0": "",
	} {
		if got := ParseAgent(ua).String(); got != want {
			t.Errorf("%s → %q，应为 %q", ua, got, want)
		}
	}
}
