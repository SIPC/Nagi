package auth

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math/big"
	"net/url"
	"strings"

	"github.com/fxamacker/cbor/v2"
)

// WebAuthn 的校验（见 docs/panel.md#通行密钥）：只做通行密钥登录要用的部分。不校验证明（attestation），
// 添加时请求的就是 none：面板不限制验证器的品牌，只要它能证明持有私钥。

// COSE 算法：ES256、EdDSA、RS256。浏览器按顺序挑第一个验证器支持的。
var coseAlgorithms = []int64{-7, -8, -257}

const (
	flagUP = 0x01 // 用户在场
	flagUV = 0x04 // 用户已验证（指纹、面容、PIN）
	flagAT = 0x40 // 带着凭据数据（只在添加时）
)

var errWebAuthn = errors.New("webauthn")

type clientData struct {
	Type      string `json:"type"`
	Challenge string `json:"challenge"`
	Origin    string `json:"origin"`
}

// checkClientData 核对浏览器给的 clientDataJSON：类型、挑战、来源。来源由浏览器填写，
// 钓鱼代理的页面在别的域名下，来源对不上；它也拿不到这个域名的通行密钥。
func checkClientData(raw []byte, typ string, challenge []byte, rpID string) error {
	var c clientData
	if err := json.Unmarshal(raw, &c); err != nil {
		return errWebAuthn
	}
	got, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(c.Challenge, "="))
	if err != nil || c.Type != typ || subtle.ConstantTimeCompare(got, challenge) != 1 {
		return errWebAuthn
	}
	if !originAllowed(c.Origin, rpID) {
		return errWebAuthn
	}
	return nil
}

// originAllowed：来源的主机名就是绑定的域名，用 https；本机的 localhost 浏览器也当作安全来源，允许 http。
// 端口不限：同一个主机名上的不同端口都在域名持有人手里。
func originAllowed(origin, rpID string) bool {
	u, err := url.Parse(origin)
	if err != nil || !strings.EqualFold(u.Hostname(), rpID) {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	return u.Scheme == "http" && (rpID == "localhost" || strings.HasSuffix(rpID, ".localhost"))
}

type authData struct {
	rpIDHash     []byte
	flags        byte
	signCount    uint32
	credentialID []byte
	publicKey    []byte // COSE
}

// parseAuthData 解析验证器数据。withCredential 时（添加）必须带凭据 ID 和公钥。
func parseAuthData(raw []byte, rpID string, withCredential bool) (authData, error) {
	if len(raw) < 37 {
		return authData{}, errWebAuthn
	}
	a := authData{rpIDHash: raw[:32], flags: raw[32], signCount: binary.BigEndian.Uint32(raw[33:37])}
	want := sha256.Sum256([]byte(rpID))
	if !bytes.Equal(a.rpIDHash, want[:]) || a.flags&flagUP == 0 || a.flags&flagUV == 0 {
		return authData{}, errWebAuthn
	}
	if !withCredential {
		return a, nil
	}
	rest := raw[37:]
	if a.flags&flagAT == 0 || len(rest) < 18 {
		return authData{}, errWebAuthn
	}
	n := int(binary.BigEndian.Uint16(rest[16:18]))
	rest = rest[18:]
	if n == 0 || n > 1023 || len(rest) < n {
		return authData{}, errWebAuthn
	}
	a.credentialID = rest[:n]
	var key cbor.RawMessage
	if _, err := cbor.UnmarshalFirst(rest[n:], &key); err != nil {
		return authData{}, errWebAuthn
	}
	if _, err := parseCOSEKey(key); err != nil {
		return authData{}, err
	}
	a.publicKey = key
	return a, nil
}

// parseAttestation 从添加时的 attestationObject 里取出验证器数据。
func parseAttestation(raw []byte, rpID string) (authData, error) {
	var obj struct {
		AuthData []byte `cbor:"authData"`
	}
	if err := cbor.Unmarshal(raw, &obj); err != nil {
		return authData{}, errWebAuthn
	}
	return parseAuthData(obj.AuthData, rpID, true)
}

type coseKey struct {
	alg int64
	pub crypto.PublicKey
}

// parseCOSEKey 解析 COSE 公钥，只接受 coseAlgorithms 里的算法。
func parseCOSEKey(raw []byte) (coseKey, error) {
	var m map[int64]cbor.RawMessage
	if err := cbor.Unmarshal(raw, &m); err != nil {
		return coseKey{}, errWebAuthn
	}
	var kty, alg int64
	if cbor.Unmarshal(m[1], &kty) != nil || cbor.Unmarshal(m[3], &alg) != nil {
		return coseKey{}, errWebAuthn
	}
	bytesAt := func(k int64) []byte {
		var b []byte
		_ = cbor.Unmarshal(m[k], &b)
		return b
	}
	switch {
	case kty == 2 && alg == -7: // EC2，P-256
		var crv int64
		x, y := bytesAt(-2), bytesAt(-3)
		if cbor.Unmarshal(m[-1], &crv) != nil || crv != 1 || len(x) != 32 || len(y) != 32 {
			return coseKey{}, errWebAuthn
		}
		pub := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
		if !pub.Curve.IsOnCurve(pub.X, pub.Y) { //nolint:staticcheck // 只用来检查点在曲线上
			return coseKey{}, errWebAuthn
		}
		return coseKey{alg, pub}, nil
	case kty == 1 && alg == -8: // OKP，Ed25519
		var crv int64
		x := bytesAt(-2)
		if cbor.Unmarshal(m[-1], &crv) != nil || crv != 6 || len(x) != ed25519.PublicKeySize {
			return coseKey{}, errWebAuthn
		}
		return coseKey{alg, ed25519.PublicKey(x)}, nil
	case kty == 3 && alg == -257: // RSA
		n, e := bytesAt(-1), bytesAt(-2)
		if len(n) < 256 || len(e) == 0 || len(e) > 4 {
			return coseKey{}, errWebAuthn
		}
		var exp int
		for _, b := range e {
			exp = exp<<8 | int(b)
		}
		return coseKey{alg, &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: exp}}, nil
	}
	return coseKey{}, errWebAuthn
}

// verifyAssertion 校验登录时的签名：签的是 authenticatorData ‖ SHA-256(clientDataJSON)。
func verifyAssertion(publicKey, authenticatorData, clientDataJSON, signature []byte) error {
	key, err := parseCOSEKey(publicKey)
	if err != nil {
		return err
	}
	ch := sha256.Sum256(clientDataJSON)
	msg := append(append([]byte{}, authenticatorData...), ch[:]...)
	switch pub := key.pub.(type) {
	case *ecdsa.PublicKey:
		h := sha256.Sum256(msg)
		if ecdsa.VerifyASN1(pub, h[:], signature) {
			return nil
		}
	case ed25519.PublicKey:
		if ed25519.Verify(pub, msg, signature) {
			return nil
		}
	case *rsa.PublicKey:
		h := sha256.Sum256(msg)
		if rsa.VerifyPKCS1v15(pub, crypto.SHA256, h[:], signature) == nil {
			return nil
		}
	}
	return errWebAuthn
}
