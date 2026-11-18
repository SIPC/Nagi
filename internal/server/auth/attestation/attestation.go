// Package attestation 校验安卓 Keystore 的密钥证明（Key Attestation，见 docs/android.md#密钥）：
// 证书链、根证书、证明扩展里的安全级别、挑战、密钥的使用条件、应用包名和 bootloader 状态。
package attestation

import (
	"bytes"
	"crypto/x509"
	_ "embed"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"fmt"
	"time"
)

// roots.pem 是 Google 的硬件证明根证书（https://android.googleapis.com/attestation/root），随面板版本更新。
//
//go:embed roots.pem
var rootsPEM []byte

// rootKeys 是根证书的公钥（SPKI）。按公钥比较：同一把根密钥重新签发过证书（2016、2022），旧手机的链可能以旧证书结尾。
var rootKeys = func() [][]byte {
	var out [][]byte
	rest := rootsPEM
	for {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			return out
		}
		c, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			panic(err)
		}
		out = append(out, c.RawSubjectPublicKeyInfo)
	}
}()

// 安全级别（attestationSecurityLevel）。
const (
	Software           = 0
	TrustedEnvironment = 1
	StrongBox          = 2
)

// 验证启动状态（verifiedBootState）。
const (
	BootVerified   = 0
	BootSelfSigned = 1
	BootUnverified = 2
	BootFailed     = 3
)

// 用户验证方式（userAuthType 的位）。
const (
	AuthPassword    = 1
	AuthFingerprint = 2
)

// keyOrigin 的取值：在 Keystore 里生成（不是导入的）。
const OriginGenerated = 0

// oidKeyDescription 是证明扩展的 OID。
var oidKeyDescription = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 11129, 2, 1, 17}

// AuthList 是证明里一组授权（softwareEnforced 或 hardwareEnforced）中用到的几项。
type AuthList struct {
	NoAuthRequired         bool
	UserAuthType           int64
	HasUserAuthType        bool
	AuthTimeout            int64
	HasAuthTimeout         bool
	UnlockedDeviceRequired bool
	Origin                 int64
	HasOrigin              bool
	RootOfTrust            *RootOfTrust
	OSPatchLevel           int64
	// AppID 是 attestationApplicationId 的 DER。
	AppID []byte
}

// RootOfTrust 是证明里的 bootloader 状态。
type RootOfTrust struct {
	DeviceLocked      bool
	VerifiedBootState int
}

// Result 是一条证明链的内容。
type Result struct {
	// HardwareRoot：链的最后一张证书是 Google 的硬件证明根证书签发的（或就是它）。
	HardwareRoot  bool
	SecurityLevel int
	Challenge     []byte
	// PublicKey 是被证明的密钥（叶子证书的 SPKI）。
	PublicKey []byte
	// Packages 是证明里的应用包名。
	Packages []string
	SW, HW   AuthList
}

// Either 返回两组授权里任一组有的值（有的条件在部分手机上由 Keystore 软件实现，记在 softwareEnforced）。
func (r *Result) Either(f func(AuthList) bool) bool { return f(r.HW) || f(r.SW) }

// BootLocked 判断 bootloader 已锁定、系统验证通过。
func (r *Result) BootLocked() bool {
	rot := r.HW.RootOfTrust
	if rot == nil {
		rot = r.SW.RootOfTrust
	}
	return rot != nil && rot.DeviceLocked && (rot.VerifiedBootState == BootVerified || rot.VerifiedBootState == BootSelfSigned)
}

// PatchLevel 是系统安全补丁日期（YYYYMM），没有时为 0。
func (r *Result) PatchLevel() int64 {
	if r.HW.OSPatchLevel != 0 {
		return r.HW.OSPatchLevel
	}
	return r.SW.OSPatchLevel
}

const (
	// MaxChain 是证明证书链最多几张。真实的链是叶子、一两张中间证书和根证书（远程配发的密钥多一两张），不超过 5 张。
	MaxChain = 10
	// MaxCertBytes 是每张证书的上限。真实的证书一两 KiB，叶子带证明扩展也不到 4 KiB。
	MaxCertBytes = 16 << 10
)

// Parse 校验证书链（叶子在前）每一张都由下一张签发，并读出证明扩展。不检查根证书是不是 Google 的：结果里的 HardwareRoot 说明。
// now 用来检查中间证书的有效期；叶子证书的有效期由手机随意填写，不检查。
//
// 除叶子外每张证书都必须是 CA，证明扩展只能出现在叶子里：Keystore 生成的密钥（叶子）不是 CA，
// 但它的私钥能对任意数据签名，不检查的话可以用一把真的硬件密钥给伪造的叶子签名，把链延长一张（「链延长」）。
func Parse(chain [][]byte, now time.Time) (*Result, error) {
	if len(chain) < 2 {
		return nil, errors.New("证明证书链不完整")
	}
	if len(chain) > MaxChain {
		return nil, errors.New("证明证书链太长")
	}
	certs := make([]*x509.Certificate, len(chain))
	for i, der := range chain {
		if len(der) > MaxCertBytes {
			return nil, errors.New("证明证书太大")
		}
		c, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, fmt.Errorf("证明证书无法解析：%w", err)
		}
		certs[i] = c
	}
	for i := 0; i+1 < len(certs); i++ {
		child, parent := certs[i], certs[i+1]
		if !parent.BasicConstraintsValid || !parent.IsCA {
			return nil, errors.New("证明证书链不正确：签发证书不是 CA")
		}
		for _, e := range parent.Extensions {
			if e.Id.Equal(oidKeyDescription) {
				return nil, errors.New("证明证书链不正确：密钥证明只能在第一张证书里")
			}
		}
		if err := parent.CheckSignature(child.SignatureAlgorithm, child.RawTBSCertificate, child.Signature); err != nil {
			return nil, fmt.Errorf("证明证书链的签名不正确：%w", err)
		}
		if i+1 < len(certs) && (now.Before(parent.NotBefore) || now.After(parent.NotAfter)) {
			return nil, errors.New("证明证书已过期或尚未生效")
		}
	}
	last := certs[len(certs)-1]
	res := &Result{PublicKey: certs[0].RawSubjectPublicKeyInfo}
	for _, k := range rootKeys {
		if bytes.Equal(last.RawSubjectPublicKeyInfo, k) {
			res.HardwareRoot = true
		}
	}
	var ext []byte
	for _, e := range certs[0].Extensions {
		if e.Id.Equal(oidKeyDescription) {
			ext = e.Value
		}
	}
	if ext == nil {
		return nil, errors.New("证书里没有密钥证明")
	}
	var kd struct {
		Version       int
		SecurityLevel asn1.Enumerated
		KMVersion     int
		KMLevel       asn1.Enumerated
		Challenge     []byte
		UniqueID      []byte
		SW            asn1.RawValue
		HW            asn1.RawValue
	}
	if _, err := asn1.Unmarshal(ext, &kd); err != nil {
		return nil, fmt.Errorf("密钥证明无法解析：%w", err)
	}
	res.SecurityLevel = int(kd.SecurityLevel)
	res.Challenge = kd.Challenge
	var err error
	if res.SW, err = parseAuthList(kd.SW.Bytes); err != nil {
		return nil, err
	}
	if res.HW, err = parseAuthList(kd.HW.Bytes); err != nil {
		return nil, err
	}
	for _, id := range [][]byte{res.SW.AppID, res.HW.AppID} {
		if id == nil {
			continue
		}
		var app struct {
			Packages []struct {
				Name    []byte
				Version int64
			} `asn1:"set"`
			Digests [][]byte `asn1:"set"`
		}
		if _, err := asn1.Unmarshal(id, &app); err != nil {
			return nil, fmt.Errorf("证明里的应用信息无法解析：%w", err)
		}
		for _, p := range app.Packages {
			res.Packages = append(res.Packages, string(p.Name))
		}
	}
	return res, nil
}

// parseAuthList 读出 AuthorizationList（一串 [tag] EXPLICIT 的可选项）里用到的几项，其余的跳过。
func parseAuthList(b []byte) (AuthList, error) {
	var l AuthList
	for len(b) > 0 {
		var item asn1.RawValue
		rest, err := asn1.Unmarshal(b, &item)
		if err != nil {
			return l, fmt.Errorf("密钥证明的授权列表无法解析：%w", err)
		}
		b = rest
		if item.Class != asn1.ClassContextSpecific {
			continue
		}
		inner := item.Bytes
		switch item.Tag {
		case 503:
			l.NoAuthRequired = true
		case 504:
			if _, err := asn1.Unmarshal(inner, &l.UserAuthType); err != nil {
				return l, err
			}
			l.HasUserAuthType = true
		case 505:
			if _, err := asn1.Unmarshal(inner, &l.AuthTimeout); err != nil {
				return l, err
			}
			l.HasAuthTimeout = true
		case 509:
			l.UnlockedDeviceRequired = true
		case 702:
			if _, err := asn1.Unmarshal(inner, &l.Origin); err != nil {
				return l, err
			}
			l.HasOrigin = true
		case 704:
			var rot struct {
				BootKey  []byte
				Locked   bool
				State    asn1.Enumerated
				BootHash []byte `asn1:"optional"`
			}
			if _, err := asn1.Unmarshal(inner, &rot); err != nil {
				return l, fmt.Errorf("证明里的 bootloader 状态无法解析：%w", err)
			}
			l.RootOfTrust = &RootOfTrust{DeviceLocked: rot.Locked, VerifiedBootState: int(rot.State)}
		case 706:
			if _, err := asn1.Unmarshal(inner, &l.OSPatchLevel); err != nil {
				return l, err
			}
		case 709:
			var id []byte
			if _, err := asn1.Unmarshal(inner, &id); err != nil {
				return l, err
			}
			l.AppID = id
		}
	}
	return l, nil
}
