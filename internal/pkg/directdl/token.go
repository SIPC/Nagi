// Package directdl 是直连下载的签名链接（见 docs/agent.md#直连下载）：面板检查完权限后签一个短期有效的链接，
// 浏览器拿着它直接从节点下载，节点用面板给的密钥核对签名和有效期。
package directdl

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/netip"
	"strings"
	"time"

	"github.com/SIPC/nagi/internal/pkg/i18n"
)

// PathPrefix 是节点上直连下载的路径前缀，后面跟着签名链接。
const PathPrefix = "/.nagi/download/"

// TTL 是链接的有效期：期间可以开始下载、续传（多线程下载工具会用同一个链接发很多次分段请求）。
// 已经开始的下载过了有效期照常传完。链接本身就是凭据，不绑定用户和来源 IP，所以有效期短：
// 撤销成员的权限后，面板还会给各节点换密钥，让没过期的链接也立即失效（见 docs/agent.md#直连下载）。
const TTL = 5 * time.Minute

// 下载的种类。
const (
	KindVolume = "volume" // 容器卷里的文件（包括快照里的、目录打包）
	KindNode   = "node"   // 节点上的文件（管理员）
)

// Claims 是链接里写明的下载内容。
type Claims struct {
	Node      string `json:"n"`
	Kind      string `json:"k"`
	Project   string `json:"p,omitempty"`
	Container string `json:"c,omitempty"`
	Volume    string `json:"v,omitempty"`
	Snapshot  string `json:"s,omitempty"`
	Path      string `json:"f"`
	Expires   int64  `json:"e"`
	// ID 是面板给这个链接的随机编号，面板的活动记录和节点的本地审计都记它，用来对上同一个链接。旧版本面板签的没有。
	ID string `json:"i,omitempty"`
	// User 是签发时的成员（面板说的），节点的本地审计记它。
	User string `json:"u,omitempty"`
	// Transport 是加密 HTTP 下载的链接写的 TransportHTTP（见 http.go），HTTPS 的链接为空。
	Transport string `json:"t,omitempty"`
}

// NewID 返回一个新的链接编号（16 位十六进制）。
func NewID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

var (
	ErrInvalid = i18n.New("下载链接无效")
	ErrExpired = i18n.New("下载链接已过期，请重新下载")
)

// 签名的上下文：HTTPS 和加密 HTTP 的链接分开，一种链接不能当另一种用。
const (
	macHTTPS = "nagi-direct-download\x00"
	macHTTP  = "nagi-direct-download-http\x00"
)

func mac(key []byte, context, payload string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(context))
	h.Write([]byte(payload))
	return h.Sum(nil)
}

// Sign 签一个链接：base64url(内容).base64url(HMAC-SHA256)。
func Sign(key []byte, c Claims) string {
	c.Transport = ""
	return sign(key, macHTTPS, c)
}

func sign(key []byte, context string, c Claims) string {
	b, _ := json.Marshal(c)
	payload := base64.RawURLEncoding.EncodeToString(b)
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac(key, context, payload))
}

// Verify 核对签名（任一密钥对得上即可）和有效期，返回链接里的内容。签名对得上、只是过期时返回 ErrExpired，
// 同时返回链接里的内容（节点记进本地审计）。只认 HTTPS 的链接（加密 HTTP 的见 VerifyHTTP）。
func Verify(keys [][]byte, token string, now time.Time) (Claims, error) {
	c, err := verify(keys, macHTTPS, token, now)
	if c.Transport != "" && (err == nil || errors.Is(err, ErrExpired)) {
		return Claims{}, ErrInvalid
	}
	return c, err
}

func verify(keys [][]byte, context, token string, now time.Time) (Claims, error) {
	payload, sig, ok := strings.Cut(token, ".")
	if !ok || len(token) > 4096 {
		return Claims{}, ErrInvalid
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return Claims{}, ErrInvalid
	}
	valid := false
	for _, k := range keys {
		if len(k) > 0 && hmac.Equal(got, mac(k, context, payload)) {
			valid = true
		}
	}
	if !valid {
		return Claims{}, ErrInvalid
	}
	b, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return Claims{}, ErrInvalid
	}
	var c Claims
	if json.Unmarshal(b, &c) != nil || c.Node == "" || (c.Kind != KindVolume && c.Kind != KindNode) {
		return Claims{}, ErrInvalid
	}
	if now.Unix() > c.Expires {
		return c, ErrExpired
	}
	return c, nil
}

// URL 返回节点上的直连下载地址。address 是节点的公网 IP。
func URL(address, token string) string {
	host := address
	if a, err := netip.ParseAddr(address); err == nil && a.Is6() {
		host = "[" + a.String() + "]"
	}
	return "https://" + host + PathPrefix + token
}

// PublicAddress 判断地址能不能用来申请 IP 证书、让浏览器直连：公网单播地址（不是内网、回环、链路本地）。
func PublicAddress(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsValid() && a.IsGlobalUnicast() && !a.IsPrivate() && !a.IsLoopback() && !a.IsLinkLocalUnicast() &&
		!netip.MustParsePrefix("100.64.0.0/10").Contains(a)
}
