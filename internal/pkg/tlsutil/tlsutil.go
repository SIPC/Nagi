// Package tlsutil 生成和使用面板的自签 CA、证书，以及公钥指纹校验。
//
// 指纹格式与 curl --pinnedpubkey 一致：对证书的 SubjectPublicKeyInfo
// 做 SHA-256，再用标准 Base64 编码，前面加 "sha256//"。
package tlsutil

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

const fingerprintPrefix = "sha256//"

// GenerateKey 生成一把 ECDSA P-256 私钥。
func GenerateKey() (*ecdsa.PrivateKey, error) {
	return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
}

// CA 是面板的自签证书颁发机构。
type CA struct {
	Cert *x509.Certificate
	Key  crypto.Signer
}

// NewCA 生成一个 10 年有效的自签 CA。
func NewCA(commonName string) (*CA, error) {
	key, err := GenerateKey()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          randomSerial(),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &CA{Cert: cert, Key: key}, nil
}

// IssueServer 用 CA 给面板签发服务端证书。key 由调用方提供并长期保存，
// 这样重新签发证书（例如增加 IP）时公钥指纹保持不变。
func (ca *CA) IssueServer(key *ecdsa.PrivateKey, hosts []string) (*x509.Certificate, error) {
	tmpl := &x509.Certificate{
		SerialNumber: randomSerial(),
		Subject:      pkix.Name{CommonName: "nagi-panel"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else if h != "" {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	return ca.sign(tmpl, &key.PublicKey)
}

// SignCSR 用 CA 给节点签发客户端证书，证书的 CommonName 就是节点 ID。
func (ca *CA) SignCSR(csrDER []byte, nodeID string, validity time.Duration) (*x509.Certificate, error) {
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		return nil, fmt.Errorf("解析证书签名请求: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("证书签名请求的签名无效: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: randomSerial(),
		Subject:      pkix.Name{CommonName: nodeID},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(validity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	return ca.sign(tmpl, csr.PublicKey)
}

func (ca *CA) sign(tmpl *x509.Certificate, pub any) (*x509.Certificate, error) {
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, pub, ca.Key)
	if err != nil {
		return nil, err
	}
	return x509.ParseCertificate(der)
}

// Pool 返回只包含这个 CA 的证书池。
func (ca *CA) Pool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(ca.Cert)
	return pool
}

// CreateCSR 生成节点的证书签名请求（DER）。
func CreateCSR(key crypto.Signer, commonName string) ([]byte, error) {
	return x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: commonName},
	}, key)
}

// Fingerprint 计算证书公钥的指纹，格式为 "sha256//<base64>"。
func Fingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return fingerprintPrefix + base64.StdEncoding.EncodeToString(sum[:])
}

// KeyFingerprint 返回公钥的指纹，和用这个公钥签发的证书的 Fingerprint 一样。
func KeyFingerprint(pub crypto.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(der)
	return fingerprintPrefix + base64.StdEncoding.EncodeToString(sum[:]), nil
}

// PinnedConfig 返回一个只信任指定公钥指纹的 TLS 客户端配置。
// 面板使用自签证书，所以跳过常规的 CA 校验，改为核对指纹。
func PinnedConfig(fingerprint string) *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, // 由下面的指纹校验代替
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return errors.New("对方没有提供证书")
			}
			cert, err := x509.ParseCertificate(rawCerts[0])
			if err != nil {
				return err
			}
			if got := Fingerprint(cert); got != fingerprint {
				return fmt.Errorf("面板公钥指纹不匹配：期望 %s，实际 %s", fingerprint, got)
			}
			return nil
		},
	}
}

func randomSerial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		panic(err)
	}
	return n
}

// ---- PEM 读写 ----

// WriteCert 把证书以 PEM 格式写入文件。
func WriteCert(path string, cert *x509.Certificate) error {
	return writePEM(path, "CERTIFICATE", cert.Raw, 0o644)
}

// WriteKey 把私钥以 PEM 格式写入文件，权限 0600。
func WriteKey(path string, key crypto.Signer) error {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	return writePEM(path, "PRIVATE KEY", der, 0o600)
}

// ReadCert 从 PEM 文件读取证书。
func ReadCert(path string) (*x509.Certificate, error) {
	der, err := readPEM(path)
	if err != nil {
		return nil, err
	}
	return x509.ParseCertificate(der)
}

// ReadKey 从 PEM 文件读取 ECDSA 私钥。
func ReadKey(path string) (*ecdsa.PrivateKey, error) {
	der, err := readPEM(path)
	if err != nil {
		return nil, err
	}
	k, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, err
	}
	ek, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s 不是 ECDSA 私钥", path)
	}
	return ek, nil
}

func writePEM(path, typ string, der []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data := pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readPEM(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("%s 不是有效的 PEM 文件", path)
	}
	return block.Bytes, nil
}
