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
	"net/http"
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
	if _, err := h.s.CreateUser(ctx, "admin", "correct horse", AdminRoleID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.CreateUser(ctx, "op", "correct horse", OperatorRoleID); err != nil {
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
	next, err := h.s.RotateSession(context.Background(), token, res.challenge, key.sign(res.challenge), info)
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
	if _, err := h.s.RotateSession(ctx, token, res.challenge, newBrowserKey().sign(res.challenge), office); err == nil {
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
	// 第一次过期：可能是手机把标签页放到了后台，再发一次挑战。
	h.age(token2, challengeGrace+time.Second)
	res2, err := h.check(token2, office)
	if err != nil || !strings.HasPrefix(res2.challenge, reissuedPrefix) {
		t.Fatalf("第一次过期应再发一次挑战：%q %v", res2.challenge, err)
	}
	// 再发的也签不出来：要求重新验证。
	h.age(token2, challengeGrace+time.Second)
	if _, err := h.check(token2, office); !errors.As(err, &rv) {
		t.Fatalf("再发的挑战也过期应要求重新验证：%v", err)
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
	for _, fp := range []string{
		goFP, curlFP, "t13d1712h1_aaaaaaaaaaaa_bbbbbbbbbbbb", "t13d181700_aaaaaaaaaaaa_bbbbbbbbbbbb",
		// 合成的低扩展数指纹：声明 h3 或 q 不能成为绕过工具检测的凭据。
		"t13d0311h3_55b375c5d22e_aaaaaaaaaaaa", "q13d0311h3_55b375c5d22e_aaaaaaaaaaaa",
	} {
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

// 同一个浏览器的握手只是扩展不同（会话恢复多出 pre_shared_key、有的握手带 padding）、或者声明了不同的 ALPN：
// 不要求重新验证。手机网络下重连频繁，以前按完整指纹比较会反复弹出验证。
func TestSessionTLSResumption(t *testing.T) {
	h := newSessionHarness(t)
	token := h.login("admin", home)
	for _, fp := range []string{
		"t13d1517h2_8daaf6152771_cb7bf5808d99", // 会话恢复：多一个扩展，扩展哈希不同
		"t13d1518h2_8daaf6152771_1234567890ab", // 恢复并带 early_data
		"t13d1517h1_8daaf6152771_c4a234726a38", // WebSocket（终端）另开的连接只声明 http/1.1
		"t13d1516h3_8daaf6152771_02713d6af862", // 仅改变 ALPN 的合成样本，不是真实 QUIC 握手
		chromeFP,
	} {
		if _, err := h.check(token, RequestInfo{IP: home.IP, UserAgent: chromeUA, Fingerprint: fp}); err != nil {
			t.Fatalf("%s：不应要求重新验证：%v", fp, err)
		}
	}
	for _, fp := range []string{"t13d1516h3_8daaf6152771_02713d6af862", "t13d1517h1_8daaf6152771_c4a234726a38"} {
		if looksLikeTool(fp) {
			t.Fatalf("%s：只改变 ALPN 不应被判为脚本工具", fp)
		}
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

// 手机在 Wi-Fi 和移动网络之间来回切换：只记审计，不要求重新验证、不告警。IPv4 和 IPv6 之间切换不算。
// N2：两处同时用一个登录（请求交替从两个网段来），5 分钟内来回切换达到 6 次时告警，仍然不要求重新验证。
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
	if _, err := h.check(token, wifi); err != nil {
		t.Fatalf("来回切换不应要求重新验证：%v", err)
	}
	if len(h.alerts) != 0 {
		t.Fatalf("不应告警：%v", h.alerts)
	}
	// 本人在 wifi，盗用者在 cell，同时使用：每个请求都切换一次。
	for i := range 10 {
		next := cell
		if i%2 == 1 {
			next = wifi
		}
		if _, err := h.check(token, next); err != nil {
			t.Fatalf("同时使用时也不要求重新验证：%v", err)
		}
	}
	if len(h.alerts) != 1 || !h.alerted("来回切换了 6 次") {
		t.Fatalf("5 分钟内来回切换 6 次应告警一次：%v", h.alerts)
	}
}

// 手机浏览器切换「桌面版网站」：Android 报成 Linux、iPhone 报成 macOS，不算换了设备。
func TestSessionDesktopMode(t *testing.T) {
	android := "Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Mobile Safari/537.36"
	desktop := "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	iphone := "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1"
	mac := "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Safari/605.1.15"
	for _, pair := range [][2]string{{android, desktop}, {iphone, mac}} {
		h := newSessionHarness(t)
		token := h.login("op", RequestInfo{IP: home.IP, UserAgent: pair[0]})
		if _, err := h.check(token, RequestInfo{IP: home.IP, UserAgent: pair[1]}); err != nil {
			t.Fatalf("切换桌面版网站不应退出：%v", err)
		}
		if _, err := h.check(token, RequestInfo{IP: home.IP, UserAgent: pair[0]}); err != nil {
			t.Fatalf("切回手机版不应退出：%v", err)
		}
	}
	// 换成 Windows 仍然算换了设备。
	h := newSessionHarness(t)
	token := h.login("op", RequestInfo{IP: home.IP, UserAgent: android})
	if _, err := h.check(token, RequestInfo{IP: home.IP, UserAgent: chromeUA}); !errors.Is(err, errLoggedOut) {
		t.Fatalf("换成另一种系统应退出：%v", err)
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
	// 切回验证过的网段不用再验证（记住最近 5 个）。
	if _, err := h.check(token, home); err != nil {
		t.Fatalf("切回验证过的网段：%v", err)
	}
	if _, err := h.check(token, office); err != nil {
		t.Fatalf("再切回去：%v", err)
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

// L5：偷到 cookie 的人用脚本工具调用，带上 Cf-Ray 这类 CDN 请求头。之前直接跳过指纹检查；现在只不判断「像脚本工具」，
// 指纹和本人的不同照样要重新验证。真的经过 CDN 的本人：CDN 回源的指纹固定时照常可用。
func TestSessionCDNHeaderStillChecksFingerprint(t *testing.T) {
	h := newSessionHarness(t)
	token := h.login("admin", home)
	hdr := http.Header{"Cf-Ray": {"x"}, "User-Agent": {chromeUA}}
	ctx := WithFingerprint(context.Background(), curlFP)
	info := RequestInfo{IP: home.IP, UserAgent: chromeUA, Fingerprint: RequestFingerprint(ctx, home.IP+":5000", hdr), CDN: ViaCDN(hdr)}
	var rv *ErrReverify
	if _, err := h.check(token, info); !errors.As(err, &rv) || rv.Reason != ReasonTLSChanged {
		t.Fatalf("带 CDN 请求头的脚本工具应要求重新验证：%v", err)
	}

	// 本人经 CDN 访问：第一次记下 CDN 回源的指纹，之后照常可用，不会因为它像脚本工具被退出。
	h2 := newSessionHarness(t)
	viaCDN := RequestInfo{IP: home.IP, UserAgent: chromeUA, Fingerprint: curlFP, CDN: true}
	token2 := h2.login("admin", RequestInfo{IP: home.IP, UserAgent: chromeUA})
	for range 3 {
		if _, err := h2.check(token2, viaCDN); err != nil {
			t.Fatalf("经 CDN 访问应照常可用：%v", err)
		}
	}
}

// L6：管理员平时只用 IPv4，偷到 cookie 的人从任意 IPv6 网段来。之前第一次出现的 IPv6 网段直接记为已验证；
// 现在会话已经验证过 IPv4 时，IPv6 第一次出现也要重新验证。没有绑定网络的成员不受影响。
func TestSessionNewFamilyNeedsReverify(t *testing.T) {
	h := newSessionHarness(t)
	token := h.login("admin", home)
	if _, err := h.check(token, home); err != nil {
		t.Fatal(err)
	}
	v6 := RequestInfo{IP: "2001:db8:dead::1", UserAgent: chromeUA, Fingerprint: chromeFP}
	var rv *ErrReverify
	if _, err := h.check(token, v6); !errors.As(err, &rv) || rv.Reason != ReasonNetworkChanged {
		t.Fatalf("已验证过 IPv4 的管理员会话，IPv6 第一次出现应要求重新验证：%v", err)
	}
	op := h.login("op", home)
	if _, err := h.check(op, v6); err != nil {
		t.Fatalf("没有绑定网络的成员不受影响：%v", err)
	}
}

// L7：两个标签页同时轮换，之前两边都成功、先发的令牌被后一个删掉，浏览器拿着它就被要求重新登录。
// 现在同一个挑战只能换一次令牌，发出去的令牌都能用。
func TestSessionConcurrentRotate(t *testing.T) {
	h := newSessionHarness(t)
	ctx := context.Background()
	token := h.login("admin", home)
	key := newBrowserKey()
	if err := h.s.RegisterSessionKey(ctx, token, key.spki()); err != nil {
		t.Fatal(err)
	}
	for round := range 30 {
		h.age(token, RotateInterval)
		res, err := h.check(token, home)
		if err != nil || res.challenge == "" {
			t.Fatalf("第 %d 轮没有挑战：%v", round, err)
		}
		sig := key.sign(res.challenge)
		var wg sync.WaitGroup
		issued := make([]string, 2)
		for i := range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				issued[i], _ = h.s.RotateSession(ctx, token, res.challenge, sig, home)
			}()
		}
		wg.Wait()
		n := 0
		for _, next := range issued {
			if next == "" {
				continue
			}
			n++
			if _, err := h.check(next, home); err != nil {
				t.Fatalf("第 %d 轮发出去的令牌不能用：%v", round, err)
			}
			token = next
		}
		if n != 1 {
			t.Fatalf("第 %d 轮同一个挑战换出了 %d 个令牌，应该只有 1 个", round, n)
		}
	}
}

// L9：blog.example.com 上的人给 .example.com 种一个 nagi_session=<攻击者自己的会话>，受害者没有 __Host- cookie 时
// 打开面板就进了攻击者的账号（登录 CSRF）。旧名字的 cookie 不再被接受。
func TestLegacyCookieIgnored(t *testing.T) {
	h := newSessionHarness(t)
	attacker := h.login("op", home)
	hdr := http.Header{"Cookie": {legacyCookieName + "=" + attacker}}
	if got := SessionToken(hdr); got != "" {
		t.Fatal("旧名字的 cookie 不应被当成会话")
	}
	if _, err := h.s.RequestUser(&http.Request{Header: hdr, RemoteAddr: home.IP + ":5000"}); err == nil {
		t.Fatal("只带旧名字 cookie 的请求应要求登录")
	}
	hdr.Add("Cookie", CookieName+"=real")
	if got := SessionToken(hdr); got != "real" {
		t.Fatalf("新名字的 cookie 照常使用：%q", got)
	}
}

// R5：L5 修好以后，带 CDN 请求头、再改一下 UA 的版本号，指纹就被当成浏览器更新直接采用，脚本工具照样能用。
// 现在带 CDN 请求头时 UA 和指纹一起变，要求重新验证并记 session_tls_changed；本人经 CDN、浏览器更新时指纹不变，不受影响。
func TestSessionCDNHeaderUAChange(t *testing.T) {
	h := newSessionHarness(t)
	token := h.login("admin", home)
	if _, err := h.check(token, home); err != nil {
		t.Fatal(err)
	}
	newer := strings.Replace(chromeUA, "131.0.0.0", "132.0.0.0", 1)
	var rv *ErrReverify
	if _, err := h.check(token, RequestInfo{IP: home.IP, UserAgent: newer, Fingerprint: curlFP, CDN: true}); !errors.As(err, &rv) || rv.Reason != ReasonTLSChanged {
		t.Fatalf("带 CDN 请求头、改了 UA 的脚本工具应要求重新验证：%v", err)
	}
	// 本人经 CDN 访问：指纹是 CDN 回源的，浏览器更新后不变。
	h2 := newSessionHarness(t)
	cdn := RequestInfo{IP: home.IP, UserAgent: chromeUA, Fingerprint: goFP, CDN: true}
	token2 := h2.login("admin", cdn)
	if _, err := h2.check(token2, cdn); err != nil {
		t.Fatal(err)
	}
	cdn.UserAgent = newer
	if _, err := h2.check(token2, cdn); err != nil {
		t.Fatalf("经 CDN 访问的本人浏览器更新不应受影响：%v", err)
	}
}
