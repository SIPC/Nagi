package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"errors"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/server/audit"
	"github.com/SIPC/nagi/internal/server/auth/attestation"
	"github.com/SIPC/nagi/internal/server/store"
)

// 安卓 App 的会话（见 docs/android.md）：配对、证明、令牌、网段绑定、解锁、退出。

const appHost = "203.0.113.10:9443"

var appHosts = []string{appHost}

// appKeys 是模拟手机的三把密钥。
type appKeys struct{ device, confirm, push *ecdsa.PrivateKey }

func newAppKeys() appKeys {
	gen := func() *ecdsa.PrivateKey { k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader); return k }
	return appKeys{gen(), gen(), gen()}
}

func signAppToken(k *ecdsa.PrivateKey, host, device, challenge string) []byte {
	sum := sha256.Sum256(AppTokenSigned(host, device, challenge))
	sig, err := ecdsa.SignASN1(rand.Reader, k, sum[:])
	if err != nil {
		panic(err)
	}
	return sig
}

// webUser 登录网页，返回带会话 ID 的成员（生成配对码要网页会话）。
func (h *sessionHarness) webUser(name string) User {
	h.t.Helper()
	tok := h.login(name, RequestInfo{IP: "198.51.100.7", UserAgent: chromeUA})
	u, err := h.s.SessionUser(context.Background(), tok)
	if err != nil {
		h.t.Fatal(err)
	}
	return u
}

// pairSoftware 在开发模式下用软件密钥添加手机，返回设备 ID。
func (h *sessionHarness) pairSoftware(user string, keys appKeys) string {
	h.t.Helper()
	h.s.SetDevAppKeys(true)
	defer h.s.SetDevAppKeys(false)
	code, _, err := h.s.CreateAppPairing(context.Background(), h.webUser(user))
	if err != nil {
		h.t.Fatal(err)
	}
	res, err := h.s.PairApp(context.Background(), "198.51.100.7", code, proofs(keys, nil), AppDeviceInfo{Model: "Pixel 9", AndroidVersion: "16", SDK: 36})
	if err != nil {
		h.t.Fatal(err)
	}
	return res.DeviceID
}

func proofs(k appKeys, chains [][][]byte) [3]AppKeyProof {
	var out [3]AppKeyProof
	for i, key := range []*ecdsa.PrivateKey{k.device, k.confirm, k.push} {
		out[i].PublicKey = appSPKI(&key.PublicKey)
		if chains != nil {
			out[i].Attestation = chains[i]
		}
	}
	return out
}

// token 换取令牌。
func (h *sessionHarness) appToken(id string, key *ecdsa.PrivateKey, kind, ip string) (AppTokenResult, error) {
	h.t.Helper()
	ch, err := h.s.AppChallenge(context.Background(), ip, id)
	if err != nil {
		h.t.Fatal(err)
	}
	return h.s.AppToken(context.Background(), ip, AppTokenRequest{DeviceID: id, Challenge: ch, Key: kind, Host: appHost,
		Signature: signAppToken(key, appHost, id, ch)}, appHosts, false)
}

func connectCode(err error) connect.Code {
	if ce := new(connect.Error); errors.As(err, &ce) {
		return ce.Code()
	}
	return connect.CodeUnknown
}

func TestAppPairing(t *testing.T) {
	h := newSessionHarness(t)
	ctx := context.Background()
	u := h.webUser("op")
	code, exp, err := h.s.CreateAppPairing(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Until(exp); d < 4*time.Minute || d > 5*time.Minute {
		t.Errorf("配对码有效期 %v，想要 5 分钟", d)
	}
	got, err := h.s.GetAppPairing(ctx, "198.51.100.8", code)
	if err != nil || got.Username != "op" {
		t.Fatalf("GetAppPairing = %+v, %v", got, err)
	}
	if _, err := h.s.GetAppPairing(ctx, "198.51.100.8", "nope"); connectCode(err) != connect.CodeNotFound {
		t.Errorf("不认识的配对码：%v，想要 not_found", err)
	}
	// 正式模式：没有证明的软件密钥被拒绝，记审计。
	keys := newAppKeys()
	if _, err := h.s.PairApp(ctx, "198.51.100.8", code, proofs(keys, nil), AppDeviceInfo{Model: "Pixel"}); connectCode(err) != connect.CodeInvalidArgument {
		t.Fatalf("没有证明：%v，想要 invalid_argument", err)
	}
	if n := h.countAudit("app_attestation_failed"); n != 1 {
		t.Errorf("app_attestation_failed 记了 %d 条", n)
	}
	// 开发模式：接受软件密钥。
	h.s.SetDevAppKeys(true)
	res, err := h.s.PairApp(ctx, "198.51.100.8", code, proofs(keys, nil), AppDeviceInfo{Model: "Pixel 9\x07", AndroidVersion: "16"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Name != "Pixel 9" {
		t.Errorf("名称 %q，控制字符应去掉", res.Name)
	}
	d, err := h.q.AppDeviceByID(ctx, res.DeviceID)
	if err != nil || d.SecurityLevel != "software" || !d.DevKeys || d.BootLocked || d.Username != "op" {
		t.Fatalf("设备记录 %+v, %v", d, err)
	}
	if used, name, _ := h.s.AppPairingStatus(ctx, u, code); !used || name != "Pixel 9" {
		t.Errorf("AppPairingStatus = %v %q", used, name)
	}
	// 只能用一次。
	if _, err := h.s.PairApp(ctx, "198.51.100.8", code, proofs(newAppKeys(), nil), AppDeviceInfo{}); connectCode(err) != connect.CodeNotFound {
		t.Errorf("第二次使用：%v，想要 not_found", err)
	}
	if n := h.countAudit("app_paired"); n != 1 {
		t.Errorf("app_paired 记了 %d 条", n)
	}
	if !h.alerted("Pixel 9") {
		t.Error("添加手机应该发通知")
	}
}

func TestAppPairingRules(t *testing.T) {
	h := newSessionHarness(t)
	ctx := context.Background()
	u := h.webUser("op")
	// 重新生成时旧的作废。
	old, _, _ := h.s.CreateAppPairing(ctx, u)
	cur, _, _ := h.s.CreateAppPairing(ctx, u)
	if _, err := h.s.GetAppPairing(ctx, "198.51.100.8", old); connectCode(err) != connect.CodeNotFound {
		t.Errorf("旧配对码：%v", err)
	}
	// 网页会话退出后作废。
	if err := h.q.DeleteSessionByID(ctx, u.SessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.GetAppPairing(ctx, "198.51.100.8", cur); connectCode(err) != connect.CodeNotFound {
		t.Errorf("会话退出后：%v", err)
	}
	// App 和访问令牌不能生成配对码。
	for _, who := range []User{{ID: u.ID, DeviceID: "x", SessionID: "s"}, {ID: u.ID, TokenID: 3, SessionID: "s"}, {ID: u.ID}} {
		if _, _, err := h.s.CreateAppPairing(ctx, who); connectCode(err) != connect.CodePermissionDenied {
			t.Errorf("%+v：%v，想要 permission_denied", who, err)
		}
	}
	// 每个成员最多 10 台。
	for range MaxAppDevices {
		h.pairSoftware("admin", newAppKeys())
	}
	if _, _, err := h.s.CreateAppPairing(ctx, h.webUser("admin")); connectCode(err) != connect.CodeResourceExhausted {
		t.Errorf("第 11 台：%v", err)
	}
}

func TestAppToken(t *testing.T) {
	h := newSessionHarness(t)
	ctx := context.Background()
	keys := newAppKeys()
	id := h.pairSoftware("op", keys)
	res, err := h.appToken(id, keys.device, AppKeyDevice, "198.51.100.20")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.Token, AppTokenPrefix) || len(res.Token) != len(AppTokenPrefix)+43 || res.NextChallenge == "" {
		t.Fatalf("令牌 %q", res.Token)
	}
	if d := time.Until(res.Expires); d > AppTokenTTL || d < AppTokenTTL-time.Minute {
		t.Errorf("有效期 %v", d)
	}
	u, err := h.s.AppTokenUser(ctx, res.Token, "198.51.100.99")
	if err != nil || u.Name != "op" || u.Via != audit.ViaApp || u.DeviceID != id || u.ElevatedUntil != 0 {
		t.Fatalf("AppTokenUser = %+v, %v", u, err)
	}
	// 换了网段：要重新换取。
	var rt *ErrAppRetoken
	if _, err := h.s.AppTokenUser(ctx, res.Token, "198.51.101.20"); !errors.As(err, &rt) || rt.Reason != "network" {
		t.Errorf("换网段：%v", err)
	}
	// 下一次用 Token 给的挑战，不用再要。
	next, err := h.s.AppToken(ctx, "198.51.100.20", AppTokenRequest{DeviceID: id, Challenge: res.NextChallenge, Key: AppKeyDevice, Host: appHost,
		Signature: signAppToken(keys.device, appHost, id, res.NextChallenge)}, appHosts, false)
	if err != nil {
		t.Fatal(err)
	}
	// 挑战只能用一次。
	if _, err := h.s.AppToken(ctx, "198.51.100.20", AppTokenRequest{DeviceID: id, Challenge: res.NextChallenge, Key: AppKeyDevice, Host: appHost,
		Signature: signAppToken(keys.device, appHost, id, res.NextChallenge)}, appHosts, false); connectCode(err) != connect.CodeFailedPrecondition {
		t.Errorf("重用挑战：%v", err)
	}
	// 别的设备的挑战不能用。
	other := h.pairSoftware("op", newAppKeys())
	if _, err := h.s.AppToken(ctx, "198.51.100.20", AppTokenRequest{DeviceID: other, Challenge: next.NextChallenge, Key: AppKeyDevice, Host: appHost,
		Signature: signAppToken(keys.device, appHost, other, next.NextChallenge)}, appHosts, false); connectCode(err) != connect.CodeFailedPrecondition {
		t.Errorf("别的设备的挑战：%v", err)
	}
	// 签的主机不是面板自己的：拒绝（别的服务器骗 App 签的挑战拿到这里用不了）。
	ch, _ := h.s.AppChallenge(ctx, "198.51.100.20", id)
	if _, err := h.s.AppToken(ctx, "198.51.100.20", AppTokenRequest{DeviceID: id, Challenge: ch, Key: AppKeyDevice, Host: "evil.example.com",
		Signature: signAppToken(keys.device, "evil.example.com", id, ch)}, appHosts, false); connectCode(err) != connect.CodePermissionDenied {
		t.Errorf("别的主机：%v", err)
	}
	// 用确认密钥签设备密钥的用途、或者签错：签名不对。
	ch, _ = h.s.AppChallenge(ctx, "198.51.100.20", id)
	if _, err := h.s.AppToken(ctx, "198.51.100.20", AppTokenRequest{DeviceID: id, Challenge: ch, Key: AppKeyDevice, Host: appHost,
		Signature: signAppToken(keys.confirm, appHost, id, ch)}, appHosts, false); connectCode(err) != connect.CodeUnauthenticated {
		t.Errorf("签名不对：%v", err)
	}
	if n := h.countAudit("app_signature_invalid"); n != 1 {
		t.Errorf("app_signature_invalid 记了 %d 条", n)
	}
	// 不认识的设备：404。
	if _, err := h.s.AppChallenge(ctx, "198.51.100.20", "nope"); connectCode(err) != connect.CodeNotFound {
		t.Errorf("不认识的设备：%v", err)
	}
	// 退出后令牌立即作废。
	if ok, err := h.s.RemoveAppDevice(ctx, u.ID, u.Name, id, "test"); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if _, err := h.s.AppTokenUser(ctx, next.Token, "198.51.100.20"); err == nil {
		t.Error("退出后令牌还能用")
	}
}

func TestAppUnlock(t *testing.T) {
	h := newSessionHarness(t)
	ctx := context.Background()
	keys := newAppKeys()
	id := h.pairSoftware("op", keys)
	// 超过解锁时长：设备密钥不行，确认密钥可以，并且把解锁时间延后。
	if _, err := h.d.Exec(`UPDATE app_devices SET last_unlock_at = ? WHERE id = ?`, time.Now().Add(-16*time.Minute).Unix(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := h.appToken(id, keys.device, AppKeyDevice, "198.51.100.20"); connectCode(err) != connect.CodeFailedPrecondition {
		t.Fatalf("超时后设备密钥：%v，想要 failed_precondition", err)
	}
	if n := h.countAudit("app_unlock_required"); n != 1 {
		t.Errorf("app_unlock_required 记了 %d 条", n)
	}
	if _, err := h.appToken(id, keys.confirm, AppKeyConfirm, "198.51.100.20"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.appToken(id, keys.device, AppKeyDevice, "198.51.100.20"); err != nil {
		t.Fatalf("解锁后设备密钥：%v", err)
	}
	// 时长可以改（5–60 分钟），超出范围按默认。
	if err := h.q.SetSetting(ctx, store.SetSettingParams{Key: SettingAppUnlock, Value: "5"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.d.Exec(`UPDATE app_devices SET last_unlock_at = ? WHERE id = ?`, time.Now().Add(-6*time.Minute).Unix(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := h.appToken(id, keys.device, AppKeyDevice, "198.51.100.20"); connectCode(err) != connect.CodeFailedPrecondition {
		t.Errorf("5 分钟：%v", err)
	}
	// 连续 5 次签名不对：要先解锁。
	h.appToken(id, keys.confirm, AppKeyConfirm, "198.51.100.20")
	for range appSigFailLimit {
		h.appToken(id, keys.push, AppKeyDevice, "198.51.100.21")
	}
	if _, err := h.appToken(id, keys.device, AppKeyDevice, "198.51.100.20"); connectCode(err) != connect.CodeFailedPrecondition {
		t.Errorf("签错 5 次后设备密钥：%v", err)
	}
	if _, err := h.appToken(id, keys.confirm, AppKeyConfirm, "198.51.100.20"); err != nil {
		t.Errorf("签错 5 次后确认密钥：%v", err)
	}
}

func TestAppAdminNetwork(t *testing.T) {
	h := newSessionHarness(t)
	ctx := context.Background()
	keys := newAppKeys()
	id := h.pairSoftware("admin", keys)
	res, err := h.appToken(id, keys.device, AppKeyDevice, "198.51.100.20")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.SetAdminNetworks(ctx, []string{"198.51.100.0/24"}, "198.51.100.20"); err != nil {
		t.Fatal(err)
	}
	ch, _ := h.s.AppChallenge(ctx, "192.0.2.5", id)
	if _, err := h.s.AppToken(ctx, "192.0.2.5", AppTokenRequest{DeviceID: id, Challenge: ch, Key: AppKeyDevice, Host: appHost,
		Signature: signAppToken(keys.device, appHost, id, ch)}, appHosts, false); connectCode(err) != connect.CodePermissionDenied {
		t.Errorf("范围外换取：%v", err)
	}
	if _, err := h.s.AppTokenUser(ctx, res.Token, "198.51.100.30"); err != nil {
		t.Errorf("范围内使用：%v", err)
	}
	if h.countAudit("admin_network_denied") != 1 {
		t.Error("应记 admin_network_denied")
	}
}

func TestAppRemovedWithPassword(t *testing.T) {
	h := newSessionHarness(t)
	ctx := context.Background()
	keys := newAppKeys()
	id := h.pairSoftware("op", keys)
	res, err := h.appToken(id, keys.device, AppKeyDevice, "198.51.100.20")
	if err != nil {
		t.Fatal(err)
	}
	u := h.webUser("op")
	if err := h.s.ChangePassword(ctx, u.ID, "", "correct horse", "battery staple"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.q.AppDeviceByID(ctx, id); err == nil {
		t.Error("改密码后手机应该退出")
	}
	if _, err := h.s.AppTokenUser(ctx, res.Token, "198.51.100.20"); err == nil {
		t.Error("改密码后令牌还能用")
	}
}

func TestAppInterceptor(t *testing.T) {
	h := newSessionHarness(t)
	keys := newAppKeys()
	id := h.pairSoftware("op", keys)
	res, err := h.appToken(id, keys.device, AppKeyDevice, "198.51.100.20")
	if err != nil {
		t.Fatal(err)
	}
	i := &interceptor{s: h.s, public: map[string]bool{}}
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+res.Token)
	ctx, _, err := i.check(context.Background(), "/nagi.v1.ProjectService/ListProjects", "198.51.100.20:5000", hdr)
	if err != nil || CurrentUser(ctx).DeviceID != id {
		t.Fatalf("同一网段：%v", err)
	}
	_, _, err = i.check(context.Background(), "/nagi.v1.ProjectService/ListProjects", "203.0.113.50:5000", hdr)
	ce := new(connect.Error)
	if !errors.As(err, &ce) || ce.Code() != connect.CodeUnauthenticated || ce.Meta().Get(RetokenHeader) != "network" {
		t.Errorf("换网段：%v", err)
	}
	// 只认 nagi_app_ 前缀的 Bearer，别的当作没登录。
	hdr.Set("Authorization", "Bearer nagi_xyz")
	if _, _, err := i.check(context.Background(), "/nagi.v1.ProjectService/ListProjects", "198.51.100.20:5000", hdr); connectCode(err) != connect.CodeUnauthenticated {
		t.Errorf("访问令牌当 App 令牌：%v", err)
	}
}

// ---- 证明 ----

// fakeChain 生成一条证明链：自己的根证书（不是 Google 的）签发的叶子证书，带 KeyDescription 扩展。
func fakeChain(t *testing.T, key *ecdsa.PrivateKey, level int, challenge []byte, sw, hw []asn1.RawValue) [][]byte {
	t.Helper()
	rootKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	rootTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fake root"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	rootDER, err := x509.CreateCertificate(rand.Reader, rootTmpl, rootTmpl, &rootKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	root, _ := x509.ParseCertificate(rootDER)
	list := func(items []asn1.RawValue) asn1.RawValue {
		var b []byte
		for _, it := range items {
			d, err := asn1.Marshal(it)
			if err != nil {
				t.Fatal(err)
			}
			b = append(b, d...)
		}
		return asn1.RawValue{Class: asn1.ClassUniversal, Tag: asn1.TagSequence, IsCompound: true, Bytes: b}
	}
	kd, err := asn1.Marshal(struct {
		Version       int
		SecurityLevel asn1.Enumerated
		KMVersion     int
		KMLevel       asn1.Enumerated
		Challenge     []byte
		UniqueID      []byte
		SW            asn1.RawValue
		HW            asn1.RawValue
	}{300, asn1.Enumerated(level), 300, asn1.Enumerated(level), challenge, []byte{}, list(sw), list(hw)})
	if err != nil {
		t.Fatal(err)
	}
	leafTmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "Android Keystore Key"},
		NotBefore: time.Unix(0, 0), NotAfter: time.Now().Add(100 * 365 * 24 * time.Hour),
		ExtraExtensions: []pkix.Extension{{Id: asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 11129, 2, 1, 17}, Value: kd}}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, root, &key.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	return [][]byte{leafDER, rootDER}
}

// tagged 是 AuthorizationList 里的一项：[tag] EXPLICIT inner。
func tagged(t *testing.T, tag int, inner any) asn1.RawValue {
	t.Helper()
	b, err := asn1.Marshal(inner)
	if err != nil {
		t.Fatal(err)
	}
	return asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: tag, IsCompound: true, Bytes: b}
}

func appID(t *testing.T, pkg string) []byte {
	t.Helper()
	b, err := asn1.Marshal(struct {
		Packages []struct {
			Name    []byte
			Version int64
		} `asn1:"set"`
		Digests [][]byte `asn1:"set"`
	}{Packages: []struct {
		Name    []byte
		Version int64
	}{{[]byte(pkg), 1}}, Digests: [][]byte{{1, 2, 3}}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestAttestationParse(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	null := asn1.RawValue{Tag: asn1.TagNull}
	rot := struct {
		Key    []byte
		Locked bool
		State  asn1.Enumerated
		Hash   []byte
	}{[]byte{1}, true, 0, []byte{2}}
	hw := []asn1.RawValue{tagged(t, 504, 3), tagged(t, 702, 0), tagged(t, 704, rot), tagged(t, 706, 202510)}
	sw := []asn1.RawValue{tagged(t, 509, null), tagged(t, 709, appID(t, AppPackage))}
	chain := fakeChain(t, key, attestation.StrongBox, []byte("challenge"), sw, hw)
	r, err := attestation.Parse(chain, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if r.HardwareRoot {
		t.Error("自己的根证书不该算 Google 的")
	}
	if r.SecurityLevel != attestation.StrongBox || string(r.Challenge) != "challenge" || !r.HW.HasUserAuthType || r.HW.UserAuthType != 3 ||
		!r.SW.UnlockedDeviceRequired || !r.BootLocked() || r.PatchLevel() != 202510 || len(r.Packages) != 1 || r.Packages[0] != AppPackage {
		t.Errorf("解析结果 %+v", r)
	}
	// 链断了：根证书换一张。
	other := fakeChain(t, key, 1, nil, nil, nil)
	if _, err := attestation.Parse([][]byte{chain[0], other[1]}, time.Now()); err == nil {
		t.Error("签名对不上的链应该拒绝")
	}
}

// 链延长：用一把「真」的叶子密钥 K1（没有任何条件）给伪造的叶子（软件密钥 K2，证明扩展自己填）签名，
// 得到 [伪造, 真叶子, 根]。真叶子不是 CA，要拒绝；证明扩展出现在第二张、链太长、证书太大也拒绝。
func TestAttestationChainExtension(t *testing.T) {
	k1, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	k2, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	real := fakeChain(t, k1, attestation.Software, []byte("other app"), nil, nil)
	k1Cert, _ := x509.ParseCertificate(real[0])

	// 伪造的证明扩展：StrongBox、挑战、包名、userAuthType=3、bootloader 已锁定。
	null := asn1.RawValue{Tag: asn1.TagNull}
	rot := struct {
		Key    []byte
		Locked bool
		State  asn1.Enumerated
		Hash   []byte
	}{[]byte{1}, true, 0, []byte{2}}
	hw := []asn1.RawValue{tagged(t, 504, 3), tagged(t, 704, rot)}
	sw := []asn1.RawValue{tagged(t, 509, null), tagged(t, 709, appID(t, AppPackage))}
	forgedExt, _ := x509.ParseCertificate(fakeChain(t, k2, attestation.StrongBox, []byte("challenge"), sw, hw)[0])
	var ext []pkix.Extension
	for _, e := range forgedExt.Extensions {
		if e.Id.Equal(asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 11129, 2, 1, 17}) {
			ext = append(ext, e)
		}
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(3), Subject: pkix.Name{CommonName: "Android Keystore Key"},
		NotBefore: time.Unix(0, 0), NotAfter: time.Now().Add(time.Hour), ExtraExtensions: ext}
	forged, err := x509.CreateCertificate(rand.Reader, tmpl, k1Cert, &k2.PublicKey, k1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := attestation.Parse([][]byte{forged, real[0], real[1]}, time.Now()); err == nil || !strings.Contains(err.Error(), "CA") {
		t.Fatalf("链延长（叶子密钥给伪造的叶子签名）应该因为签发证书不是 CA 被拒绝：%v", err)
	}

	// 即使签发证书是 CA，证明扩展也只能在第一张。
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	selfRootKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	rootTmpl := &x509.Certificate{SerialNumber: big.NewInt(10), Subject: pkix.Name{CommonName: "root"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	rootDER, _ := x509.CreateCertificate(rand.Reader, rootTmpl, rootTmpl, &selfRootKey.PublicKey, selfRootKey)
	rootParsed, _ := x509.ParseCertificate(rootDER)
	interTmpl := &x509.Certificate{SerialNumber: big.NewInt(11), Subject: pkix.Name{CommonName: "inter"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, ExtraExtensions: ext}
	interDER, err := x509.CreateCertificate(rand.Reader, interTmpl, rootParsed, &caKey.PublicKey, selfRootKey)
	if err != nil {
		t.Fatal(err)
	}
	interParsed, _ := x509.ParseCertificate(interDER)
	leafTmpl := &x509.Certificate{SerialNumber: big.NewInt(12), Subject: pkix.Name{CommonName: "Android Keystore Key"},
		NotBefore: time.Unix(0, 0), NotAfter: time.Now().Add(time.Hour), ExtraExtensions: ext}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, interParsed, &k2.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := attestation.Parse([][]byte{leafDER, interDER, rootDER}, time.Now()); err == nil {
		t.Error("证明扩展出现在中间证书里应该拒绝")
	}
	// 对照：中间证书不带证明扩展时这条链是好的。
	interTmpl.ExtraExtensions = nil
	interDER, _ = x509.CreateCertificate(rand.Reader, interTmpl, rootParsed, &caKey.PublicKey, selfRootKey)
	interParsed, _ = x509.ParseCertificate(interDER)
	leafDER, _ = x509.CreateCertificate(rand.Reader, leafTmpl, interParsed, &k2.PublicKey, caKey)
	if _, err := attestation.Parse([][]byte{leafDER, interDER, rootDER}, time.Now()); err != nil {
		t.Errorf("叶子、中间证书、根的正常链应该通过：%v", err)
	}

	// 链太长、证书太大。
	long := make([][]byte, attestation.MaxChain+1)
	for i := range long {
		long[i] = real[1]
	}
	if _, err := attestation.Parse(long, time.Now()); err == nil {
		t.Error("太长的链应该拒绝")
	}
	huge := append(append([]byte{}, real[0]...), make([]byte, attestation.MaxCertBytes)...)
	if _, err := attestation.Parse([][]byte{huge, real[1]}, time.Now()); err == nil {
		t.Error("太大的证书应该拒绝")
	}
}

// 模拟器、自己签的证明（软件级别、根证书不是 Google 的）：正式模式拒绝，开发模式接受。
func TestAppAttestationRejectsEmulator(t *testing.T) {
	h := newSessionHarness(t)
	ctx := context.Background()
	keys := newAppKeys()
	code, _, err := h.s.CreateAppPairing(ctx, h.webUser("op"))
	if err != nil {
		t.Fatal(err)
	}
	ch := sha256.Sum256([]byte(code))
	null := asn1.RawValue{Tag: asn1.TagNull}
	sw := []asn1.RawValue{tagged(t, 709, appID(t, AppPackage))}
	chains := [][][]byte{
		fakeChain(t, keys.device, attestation.Software, ch[:], append(sw, tagged(t, 509, null)), nil),
		fakeChain(t, keys.confirm, attestation.Software, ch[:], append(sw, tagged(t, 504, 3)), nil),
		fakeChain(t, keys.push, attestation.Software, ch[:], append(sw, tagged(t, 503, null)), nil),
	}
	_, err = h.s.PairApp(ctx, "198.51.100.8", code, proofs(keys, chains), AppDeviceInfo{Model: "sdk_gphone64"})
	if connectCode(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "Google") {
		t.Fatalf("模拟器的证明：%v，想要因根证书拒绝", err)
	}
	// 挑战不对：开发模式下也拒绝。
	h.s.SetDevAppKeys(true)
	bad := [][][]byte{fakeChain(t, keys.device, 0, []byte("x"), sw, nil), chains[1], chains[2]}
	if _, err := h.s.PairApp(ctx, "198.51.100.8", code, proofs(keys, bad), AppDeviceInfo{}); err == nil || !strings.Contains(err.Error(), "挑战") {
		t.Errorf("挑战不对：%v", err)
	}
	// 包名不对：拒绝。
	wrongPkg := [][][]byte{fakeChain(t, keys.device, 0, ch[:], []asn1.RawValue{tagged(t, 709, appID(t, "com.example.evil"))}, nil), chains[1], chains[2]}
	if _, err := h.s.PairApp(ctx, "198.51.100.8", code, proofs(keys, wrongPkg), AppDeviceInfo{}); err == nil || !strings.Contains(err.Error(), "Nagi") {
		t.Errorf("包名不对：%v", err)
	}
	// 公钥和证明里的不一致：拒绝。
	swapped := [][][]byte{chains[1], chains[1], chains[2]}
	if _, err := h.s.PairApp(ctx, "198.51.100.8", code, proofs(keys, swapped), AppDeviceInfo{}); err == nil || !strings.Contains(err.Error(), "公钥") {
		t.Errorf("公钥不一致：%v", err)
	}
	res, err := h.s.PairApp(ctx, "198.51.100.8", code, proofs(keys, chains), AppDeviceInfo{Model: "sdk_gphone64"})
	if err != nil {
		t.Fatalf("开发模式下模拟器的证明：%v", err)
	}
	if d, _ := h.q.AppDeviceByID(ctx, res.DeviceID); d.SecurityLevel != "software" || !d.DevKeys {
		t.Errorf("设备记录 %+v", d.AppDevice)
	}
}

// 硬件级别的证明，使用条件不符合时拒绝（根证书检查之后才检查使用条件，所以这里直接测规则）。
func TestAppKeyRules(t *testing.T) {
	r := func(hw, sw attestation.AuthList) *attestation.Result { return &attestation.Result{HW: hw, SW: sw} }
	cases := []struct {
		rule int
		res  *attestation.Result
		sdk  int
		ok   bool
	}{
		{0, r(attestation.AuthList{}, attestation.AuthList{UnlockedDeviceRequired: true}), 36, true},
		{0, r(attestation.AuthList{}, attestation.AuthList{}), 36, false},
		{1, r(attestation.AuthList{HasUserAuthType: true, UserAuthType: 3}, attestation.AuthList{}), 36, true},
		{1, r(attestation.AuthList{HasUserAuthType: true, UserAuthType: 2}, attestation.AuthList{}), 28, true},
		{1, r(attestation.AuthList{HasUserAuthType: true, UserAuthType: 3, HasAuthTimeout: true, AuthTimeout: 300}, attestation.AuthList{}), 36, false},
		{1, r(attestation.AuthList{NoAuthRequired: true}, attestation.AuthList{}), 36, false},
		{1, r(attestation.AuthList{}, attestation.AuthList{HasUserAuthType: true, UserAuthType: 3}), 36, false},
		{2, r(attestation.AuthList{NoAuthRequired: true}, attestation.AuthList{}), 36, true},
		{2, r(attestation.AuthList{HasUserAuthType: true, UserAuthType: 3}, attestation.AuthList{}), 36, false},
	}
	for i, c := range cases {
		if got := appKeyRules[c.rule].check(c.res, c.sdk) == ""; got != c.ok {
			t.Errorf("第 %d 条（%s）：通过 = %v，想要 %v", i, appKeyRules[c.rule].name, got, c.ok)
		}
	}
}

func (h *sessionHarness) countAudit(action string) int {
	h.t.Helper()
	evs, err := h.log.List(context.Background(), audit.Filter{Limit: 1000})
	if err != nil {
		h.t.Fatal(err)
	}
	n := 0
	for _, e := range evs {
		if e.Action == action {
			n++
		}
	}
	return n
}
