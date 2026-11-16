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

// L8：没开安全入口时，未登录的人每 5 分钟发 1000 次 BeginPasskeyLogin，之前所有人的通行密钥登录和添加都失败。
// 现在登录和添加分开计数，同一个网段的登录挑战最多 20 个。
func TestPasskeyChallengePool(t *testing.T) {
	h := newSessionHarness(t)
	ctx := context.Background()
	admin, _ := h.q.GetUserByUsername(ctx, "admin")
	_ = h.q.SetSetting(ctx, store.SetSettingParams{Key: settingDomain, Value: "nagi.example.com"})
	from := func(ip string) context.Context { return WithMeta(ctx, Meta{IP: ip}) }
	// 攻击者在一个网段里刷。
	n := 0
	for range 2000 {
		if _, _, _, err := h.s.BeginPasskeyLogin(from("198.51.100.7")); err == nil {
			n++
		}
	}
	if n != maxLoginChallengesPerNet {
		t.Fatalf("同一个网段同时有效的登录挑战应该最多 %d 个，实际 %d 个", maxLoginChallengesPerNet, n)
	}
	// 本人从别的网段登录、已登录的成员添加，都不受影响。
	if _, _, _, err := h.s.BeginPasskeyLogin(from("203.0.113.10")); err != nil {
		t.Fatalf("别的网段的登录不应受影响：%v", err)
	}
	// 攻击者用很多网段占满登录的池子，添加照样可用。
	for i := range 200 {
		for range maxLoginChallengesPerNet {
			_, _, _, _ = h.s.BeginPasskeyLogin(from(fmt.Sprintf("10.%d.%d.1", i/250, i%250)))
		}
	}
	if _, err := h.s.BeginPasskeyRegistration(ctx, admin.ID); err != nil {
		t.Fatalf("登录的挑战占满以后，添加不应受影响：%v", err)
	}
}
