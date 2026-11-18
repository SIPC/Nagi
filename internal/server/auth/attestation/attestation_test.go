package attestation

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"math/big"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/cryptobyte"
	cryptobyte_asn1 "golang.org/x/crypto/cryptobyte/asn1"
)

// berTrue 是写成 0x01 的「是」（BER 允许，DER 只允许 0xFF）。
var berTrue = asn1.RawValue{Class: asn1.ClassUniversal, Tag: asn1.TagBoolean, Bytes: []byte{0x01}}

// keyUsageCritical 是叶子证书里 KeyUsage 扩展开头的 OID 和 critical（0xFF）。
var keyUsageCritical = []byte{0x06, 0x03, 0x55, 0x1d, 0x0f, 0x01, 0x01, 0xff}

// berChain 生成一条像部分小米机型那样的证明链：叶子证书的 KeyUsage 扩展 critical 写成 0x01，
// 证明里的 deviceLocked 也写成 0x01；签名按改过的字节重新签，和手机发来的一样是有效的。
// patch 可以在重新签名之后再改待签名部分，用来模拟签名后被改过的证书。
func berChain(t *testing.T, patch func(tbs []byte)) [][]byte {
	t.Helper()
	rootKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	rootTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fake root"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	rootDER, err := x509.CreateCertificate(rand.Reader, rootTmpl, rootTmpl, &rootKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	root, _ := x509.ParseCertificate(rootDER)

	rot, err := asn1.Marshal(struct {
		Key    []byte
		Locked asn1.RawValue
		State  asn1.Enumerated
		Hash   []byte
	}{[]byte{1}, berTrue, BootVerified, []byte{2}})
	if err != nil {
		t.Fatal(err)
	}
	hwItem, err := asn1.Marshal(asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 704, IsCompound: true, Bytes: rot})
	if err != nil {
		t.Fatal(err)
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
	}{300, TrustedEnvironment, 300, TrustedEnvironment, []byte("challenge"), []byte{},
		asn1.RawValue{Class: asn1.ClassUniversal, Tag: asn1.TagSequence, IsCompound: true},
		asn1.RawValue{Class: asn1.ClassUniversal, Tag: asn1.TagSequence, IsCompound: true, Bytes: hwItem}})
	if err != nil {
		t.Fatal(err)
	}
	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leafTmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "Android Keystore Key"},
		NotBefore: time.Unix(0, 0), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
		ExtraExtensions: []pkix.Extension{{Id: oidKeyDescription, Value: kd}}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, root, &leafKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}

	// 拆开叶子证书：待签名部分、签名算法、签名。
	s := cryptobyte.String(leafDER)
	var cert, tbs, alg cryptobyte.String
	if !s.ReadASN1(&cert, cryptobyte_asn1.SEQUENCE) || !cert.ReadASN1Element(&tbs, cryptobyte_asn1.SEQUENCE) ||
		!cert.ReadASN1Element(&alg, cryptobyte_asn1.SEQUENCE) {
		t.Fatal("拆不开叶子证书")
	}
	raw := bytes.Clone(tbs)
	at := bytes.Index(raw, keyUsageCritical)
	if at < 0 {
		t.Fatal("叶子证书里没有 critical 的 KeyUsage")
	}
	raw[at+len(keyUsageCritical)-1] = 0x01
	digest := sha256.Sum256(raw)
	sig, err := ecdsa.SignASN1(rand.Reader, rootKey, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	if patch != nil {
		patch(raw)
	}
	var b cryptobyte.Builder
	b.AddASN1(cryptobyte_asn1.SEQUENCE, func(b *cryptobyte.Builder) {
		b.AddBytes(raw)
		b.AddBytes(alg)
		b.AddASN1BitString(sig)
	})
	leaf, err := b.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	return [][]byte{leaf, rootDER}
}

func TestParseBERBooleans(t *testing.T) {
	chain := berChain(t, nil)
	if _, err := x509.ParseCertificate(chain[0]); err == nil || !strings.Contains(err.Error(), "critical") {
		t.Fatalf("Go 自己应该拒绝 0x01 的 critical：%v", err)
	}
	r, err := Parse(chain, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if string(r.Challenge) != "challenge" || r.SecurityLevel != TrustedEnvironment {
		t.Errorf("证明内容不对：%+v", r)
	}
	if !r.BootLocked() {
		t.Error("deviceLocked 写成 0x01 也应该算已锁定")
	}
}

// 签名按原样的字节校验：把 0x01 改成别的非零值，放宽后意思一样，但签名对不上，必须拒绝。
func TestParseBERBooleansSigned(t *testing.T) {
	chain := berChain(t, func(tbs []byte) {
		at := bytes.Index(tbs, append(keyUsageCritical[:len(keyUsageCritical)-1:len(keyUsageCritical)-1], 0x01))
		tbs[at+len(keyUsageCritical)-1] = 0x02
	})
	if _, err := Parse(chain, time.Now()); err == nil || !strings.Contains(err.Error(), "签名") {
		t.Fatalf("签名后改过的证书应该因签名不对被拒绝：%v", err)
	}
}

func TestLenientBooleans(t *testing.T) {
	// 已经是 DER 的原样返回（同一个切片），不复制。
	der := []byte{0x30, 0x03, 0x01, 0x01, 0xff}
	if got := lenientBooleans(der); &got[0] != &der[0] {
		t.Error("没有要改的不该复制")
	}
	ber := []byte{0x30, 0x03, 0x01, 0x01, 0x01}
	if got := lenientBooleans(ber); !bytes.Equal(got, der) || ber[4] != 0x01 {
		t.Errorf("应该返回改过的副本、不动原来的：%x %x", got, ber)
	}
	// 「否」和 OCTET STRING 里的字节不改；结构不对的原样返回。
	for _, in := range [][]byte{
		{0x30, 0x03, 0x01, 0x01, 0x00},
		{0x04, 0x03, 0x01, 0x01, 0x01},
		{0x30, 0x05, 0x01, 0x01, 0x01},
		{0x30, 0x80, 0x01, 0x01, 0x01, 0x00, 0x00},
	} {
		if got := lenientBooleans(in); !bytes.Equal(got, in) {
			t.Errorf("%x 不该改：%x", in, got)
		}
	}
}
