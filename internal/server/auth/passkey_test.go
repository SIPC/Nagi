package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/SIPC/nagi/internal/server/store"
	"github.com/fxamacker/cbor/v2"
)

// softKey 是测试用的软件验证器：一把 P-256 密钥。
type softKey struct {
	key   *ecdsa.PrivateKey
	id    []byte
	count uint32
}

func newSoftKey() *softKey {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	id := make([]byte, 16)
	_, _ = rand.Read(id)
	return &softKey{key: k, id: id}
}

func (k *softKey) cose() []byte {
	b, _ := cbor.Marshal(map[int]any{1: 2, 3: -7, -1: 1, -2: k.key.X.FillBytes(make([]byte, 32)), -3: k.key.Y.FillBytes(make([]byte, 32))})
	return b
}

func clientJSON(typ string, challenge []byte, origin string) []byte {
	b, _ := json.Marshal(map[string]string{"type": typ, "challenge": base64.RawURLEncoding.EncodeToString(challenge), "origin": origin})
	return b
}

func (k *softKey) authData(rpID string, flags byte, withCred bool) []byte {
	h := sha256.Sum256([]byte(rpID))
	out := append(h[:], flags)
	out = binary.BigEndian.AppendUint32(out, k.count)
	if withCred {
		out = append(out, make([]byte, 16)...) // AAGUID
		out = binary.BigEndian.AppendUint16(out, uint16(len(k.id)))
		out = append(out, k.id...)
		out = append(out, k.cose()...)
	}
	return out
}

func (k *softKey) create(rpID string, challenge []byte, origin string) (clientData, attestation []byte) {
	ad := k.authData(rpID, flagUP|flagUV|flagAT, true)
	att, _ := cbor.Marshal(map[string]any{"fmt": "none", "attStmt": map[string]any{}, "authData": ad})
	return clientJSON("webauthn.create", challenge, origin), att
}

func (k *softKey) get(rpID, challengeID string, challenge []byte, origin string, userID int64) PasskeyAssertion {
	k.count++
	cd := clientJSON("webauthn.get", challenge, origin)
	ad := k.authData(rpID, flagUP|flagUV, false)
	h := sha256.Sum256(cd)
	digest := sha256.Sum256(append(append([]byte{}, ad...), h[:]...))
	sig, _ := ecdsa.SignASN1(rand.Reader, k.key, digest[:])
	return PasskeyAssertion{ChallengeID: challengeID, CredentialID: k.id, ClientDataJSON: cd, AuthenticatorData: ad, Signature: sig, UserHandle: UserHandle(userID)}
}

func TestPasskey(t *testing.T) {
	h := newSessionHarness(t)
	ctx := context.Background()
	admin, _ := h.q.GetUserByUsername(ctx, "admin")

	if _, err := h.s.BeginPasskeyRegistration(ctx, admin.ID); err == nil {
		t.Fatal("没绑定域名时不能添加")
	}
	_ = h.q.SetSetting(ctx, store.SetSettingParams{Key: settingDomain, Value: "nagi.example.com"})
	const origin = "https://nagi.example.com"

	// 添加。
	key := newSoftKey()
	reg, err := h.s.BeginPasskeyRegistration(ctx, admin.ID)
	if err != nil || reg.RPID != "nagi.example.com" {
		t.Fatalf("开始添加：%v %v", reg, err)
	}
	cd, att := key.create("nagi.example.com", reg.Challenge, "https://evil.example.net")
	if err := h.s.AddPasskey(ctx, admin.ID, reg.ChallengeID, "笔记本", cd, att); err == nil {
		t.Fatal("别的来源不能添加")
	}
	reg, _ = h.s.BeginPasskeyRegistration(ctx, admin.ID)
	cd, att = key.create("nagi.example.com", reg.Challenge, origin)
	if err := h.s.AddPasskey(ctx, admin.ID, reg.ChallengeID, "笔记本", cd, att); err != nil {
		t.Fatal(err)
	}
	if err := h.s.AddPasskey(ctx, admin.ID, reg.ChallengeID, "笔记本", cd, att); err == nil {
		t.Fatal("挑战只能用一次")
	}

	login := func(a PasskeyAssertion) error {
		_, err := h.s.PasskeyLogin(ctx, "198.51.100.1", a)
		return err
	}
	begin := func() (string, []byte) {
		id, ch, _, err := h.s.BeginPasskeyLogin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return id, ch
	}

	// 正常登录。
	id, ch := begin()
	if err := login(key.get("nagi.example.com", id, ch, origin, admin.ID)); err != nil {
		t.Fatalf("登录失败：%v", err)
	}
	// 钓鱼代理：页面在别的域名下，浏览器填的来源是代理的域名。
	id, ch = begin()
	if err := login(key.get("nagi.example.com", id, ch, "https://nagi-example.com", admin.ID)); err == nil {
		t.Fatal("经钓鱼代理转发的来源不能登录")
	}
	// 代理自己的域名下的签名（rpIdHash 不同）。
	id, ch = begin()
	if err := login(key.get("nagi-example.com", id, ch, origin, admin.ID)); err == nil {
		t.Fatal("别的域名的签名不能登录")
	}
	// 签名被改。
	id, ch = begin()
	a := key.get("nagi.example.com", id, ch, origin, admin.ID)
	a.Signature[len(a.Signature)-1] ^= 1
	if err := login(a); err == nil {
		t.Fatal("签名不对不能登录")
	}
	// 重放上一次的签名：挑战已经用掉了。
	id, ch = begin()
	a = key.get("nagi.example.com", id, ch, origin, admin.ID)
	if err := login(a); err != nil {
		t.Fatal(err)
	}
	if err := login(a); err == nil {
		t.Fatal("同一个挑战不能用两次")
	}
	// 签名计数倒退：通行密钥被复制。
	id, ch = begin()
	key.count -= 2
	if err := login(key.get("nagi.example.com", id, ch, origin, admin.ID)); err == nil {
		t.Fatal("签名计数没有增加时不能登录")
	}
	key.count += 5
	// 换了域名以后旧的用不了。
	_ = h.q.SetSetting(ctx, store.SetSettingParams{Key: settingDomain, Value: "panel.example.org"})
	id, ch = begin()
	if err := login(key.get("panel.example.org", id, ch, "https://panel.example.org", admin.ID)); err == nil {
		t.Fatal("换了域名以后旧的通行密钥不能登录")
	}

	// 删除。
	list, _ := h.s.ListPasskeys(ctx, admin.ID)
	if len(list) != 1 || list[0].Name != "笔记本" || list[0].LastUsedAt == 0 {
		t.Fatalf("列表不对：%+v", list)
	}
	op, _ := h.q.GetUserByUsername(ctx, "op")
	if _, err := h.s.DeletePasskey(ctx, op.ID, list[0].ID); err == nil {
		t.Fatal("不能删别人的通行密钥")
	}
	if _, err := h.s.DeletePasskey(ctx, admin.ID, list[0].ID); err != nil {
		t.Fatal(err)
	}
}

func TestOriginAllowed(t *testing.T) {
	for _, c := range []struct {
		origin, rp string
		ok         bool
	}{
		{"https://nagi.example.com", "nagi.example.com", true},
		{"https://nagi.example.com:8443", "nagi.example.com", true},
		{"http://nagi.example.com", "nagi.example.com", false},
		{"https://a.nagi.example.com", "nagi.example.com", false},
		{"http://localhost:5173", "localhost", true},
		{"https://evil.com", "nagi.example.com", false},
	} {
		if got := originAllowed(c.origin, c.rp); got != c.ok {
			t.Errorf("%s / %s：%v", c.origin, c.rp, got)
		}
	}
}

// L8、R6：没登录的人反复要登录挑战，之前能占满挑战池；按网段限数量以后，和攻击者共用出口（CDN 节点、NAT）的人
// 照样开始不了通行密钥登录。现在登录挑战不占名额（无状态，编号带 HMAC），添加的挑战另外计数。
func TestPasskeyChallengePool(t *testing.T) {
	h := newSessionHarness(t)
	ctx := context.Background()
	admin, _ := h.q.GetUserByUsername(ctx, "admin")
	_ = h.q.SetSetting(ctx, store.SetSettingParams{Key: settingDomain, Value: "nagi.example.com"})
	from := WithMeta(ctx, Meta{IP: "198.51.100.7"})
	for i := range 5000 {
		if _, _, _, err := h.s.BeginPasskeyLogin(from); err != nil {
			t.Fatalf("第 %d 次：登录挑战不应有名额限制：%v", i, err)
		}
	}
	// 同一个出口后面的本人照样能开始登录，已登录的成员照样能添加。
	id, ch, rp, err := h.s.BeginPasskeyLogin(from)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.BeginPasskeyRegistration(ctx, admin.ID); err != nil {
		t.Fatalf("添加不应受影响：%v", err)
	}
	// 编号只能用一次，改过的编号、别的域名、过期的都不认。
	now := time.Now()
	c, ok := h.s.verifyLoginChallenge(id, rp, now)
	if !ok || string(c.challenge) != string(ch) {
		t.Fatal("挑战应能取出，内容和发出去的一致")
	}
	if !h.s.markLoginUsed(id, c.expires, now) || h.s.markLoginUsed(id, c.expires, now) {
		t.Fatal("挑战只能用一次")
	}
	id2, _, _, _ := h.s.BeginPasskeyLogin(from)
	bad := []byte(id2)
	bad[0] ^= 1
	if _, ok := h.s.verifyLoginChallenge(string(bad), rp, now); ok {
		t.Fatal("改过的编号不应认")
	}
	if _, ok := h.s.verifyLoginChallenge(id2, "evil.example.net", now); ok {
		t.Fatal("别的域名不应认")
	}
	if _, ok := h.s.verifyLoginChallenge(id2, rp, now.Add(passkeyChallengeTTL+time.Second)); ok {
		t.Fatal("过期的不应认")
	}
}

// S4：之前带一个随便的凭据提交，编号就记进「用过的编号」表；表有 10 万的上限，没登录的人换很多 /64 地址就能填满，
// 之后所有人的通行密钥登录都失败。现在凭据确实存在以后才记，表按过期时间分桶、到期整桶丢弃，没有上限。
func TestPasskeyLoginUnknownCredentialNotRecorded(t *testing.T) {
	h := newSessionHarness(t)
	ctx := context.Background()
	_ = h.q.SetSetting(ctx, store.SetSettingParams{Key: settingDomain, Value: "nagi.example.com"})
	for i := range 300 {
		id, _, _, err := h.s.BeginPasskeyLogin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		ip := fmt.Sprintf("2001:db8:%x::1", i)
		_, _ = h.s.PasskeyLogin(ctx, ip, PasskeyAssertion{ChallengeID: id, CredentialID: []byte(fmt.Sprintf("unknown-%d", i))})
	}
	h.s.mu.Lock()
	n := 0
	for _, b := range h.s.usedLogin {
		n += len(b)
	}
	h.s.mu.Unlock()
	if n != 0 {
		t.Fatalf("不认识的凭据不应占表，实际记了 %d 条", n)
	}
	// 分桶：过期的整桶丢弃。
	now := time.Now()
	exp := now.Add(passkeyChallengeTTL)
	if !h.s.markLoginUsed("a", exp, now) {
		t.Fatal("第一次应能记下")
	}
	h.s.markLoginUsed("b", now.Add(time.Hour), now.Add(passkeyChallengeTTL+2*time.Minute))
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	if _, ok := h.s.usedLogin[exp.Unix()/60]; ok {
		t.Fatal("过期的桶应整桶丢弃")
	}
}
