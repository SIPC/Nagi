package directdl

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/netip"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// 加密 HTTP 下载（见 docs/agent.md#加密-http-下载）：节点申请不到 IP 证书（80/443 从公网进不来、Let's Encrypt 限额等）
// 时的备用方式。面板给出一条命令，用 curl 经明文 HTTP 下载，再用 openssl 在本地解密。内容用 AES-256-CTR 加密，
// 密钥由面板和节点各自从这条连接的直连下载密钥派生（HKDF-SHA256），不经网络传，也不出现在链接里；
// 链接照样是 5 分钟有效的签名链接，签名的上下文和 HTTPS 链接不同，两种链接不能互换。

// HTTPPathPrefix 是节点上加密 HTTP 下载的路径前缀，后面跟着签名链接。
const HTTPPathPrefix = "/.nagi/dl/"

// TransportHTTP 是加密 HTTP 下载的链接在 Claims.Transport 里写的值。
const TransportHTTP = "http"

// HTTPHashLimit：不超过这么大的文件，签发命令时节点算出 SHA-256，命令下载完核对；更大的不算（太慢）。
const HTTPHashLimit = 4 << 30

// HTTPHashTimeout：签发命令时算 SHA-256 最多这么久，算不完就不带校验。
const HTTPHashTimeout = 30 * time.Second

// 签发命令时没有算 SHA-256 的原因（PrepareDirectHttpResponse.hash_skipped）。
const (
	HashSkippedTooLarge = "too_large"
	HashSkippedTimeout  = "timeout"
	HashSkippedArchive  = "archive"
)

// SignHTTP 签一个加密 HTTP 下载的链接：和 Sign 一样，但签名的上下文不同，并且写明 Transport，
// HTTPS 的链接不能拿来走 HTTP，反过来也不行（旧版本的节点也核对不过）。
func SignHTTP(key []byte, c Claims) string {
	c.Transport = TransportHTTP
	return sign(key, macHTTP, c)
}

// VerifyHTTP 核对加密 HTTP 下载的链接，规则和 Verify 一样。
func VerifyHTTP(keys [][]byte, token string, now time.Time) (Claims, error) {
	c, err := verify(keys, macHTTP, token, now)
	if c.Transport != TransportHTTP && (err == nil || errors.Is(err, ErrExpired)) {
		return Claims{}, ErrInvalid
	}
	return c, err
}

// HTTPURL 返回节点上的加密 HTTP 下载地址。port 为 80 时不写端口。
func HTTPURL(address string, port int, token string) string {
	host := address
	if a, err := netip.ParseAddr(address); err == nil && a.Is6() {
		host = "[" + a.String() + "]"
	}
	if port != 80 {
		host += ":" + strconv.Itoa(port)
	}
	return "http://" + host + HTTPPathPrefix + token
}

// ContentKey 是一次加密 HTTP 下载的内容密钥：AES-256 的密钥和 CTR 的初始计数器（IV），
// Tag 是由同一次派生得到的短标识，用来给下载到一半的文件起名（换了密钥就换一个文件，不会把两种密文拼在一起）。
type ContentKey struct {
	Key [32]byte
	IV  [16]byte
	Tag string
}

// FileKey 派生普通文件的内容密钥：同一个面板密钥下，同一个文件（同一个 ETag）总是同一个密钥，
// 重新签发的命令可以接着之前下载到一半的部分续传；文件变了（ETag 变了）就换一个，不会用同一段密钥流加密不同的内容。
func FileKey(secret []byte, c Claims, etag string) ContentKey {
	return derive(secret, "file", c.Node, c.Kind, c.Project, c.Container, c.Volume, c.Snapshot, c.Path, etag)
}

// ArchiveKey 派生目录打包的内容密钥：每个链接一个（打包的内容每次可能不同，链接也只能用一次）。
func ArchiveKey(secret []byte, c Claims) ContentKey {
	return derive(secret, "archive", c.Node, c.ID)
}

func derive(secret []byte, parts ...string) ContentKey {
	info := "nagi-http-dl\x00" + strings.Join(parts, "\x00")
	out, err := hkdf.Key(sha256.New, secret, nil, info, 32+16+4)
	if err != nil {
		panic(err) // 长度固定，不会出错
	}
	var k ContentKey
	copy(k.Key[:], out[:32])
	copy(k.IV[:], out[32:48])
	k.Tag = hex.EncodeToString(out[48:])
	return k
}

// Stream 返回从第 offset 个字节开始的密钥流：和 openssl enc -aes-256-ctr 一样，IV 是 128 位大端的计数器，
// 每 16 字节加一（溢出时回绕）。Go 的 cipher.NewCTR 也是这样，所以从中间开始时把计数器加上 offset/16，
// 再丢掉块内的 offset%16 个字节。
func (k ContentKey) Stream(offset int64) cipher.Stream {
	block, err := aes.NewCipher(k.Key[:])
	if err != nil {
		panic(err)
	}
	iv := k.IV
	addCounter(&iv, uint64(offset/aes.BlockSize))
	s := cipher.NewCTR(block, iv[:])
	if skip := offset % aes.BlockSize; skip > 0 {
		buf := make([]byte, skip)
		s.XORKeyStream(buf, buf)
	}
	return s
}

// addCounter 把 128 位大端的计数器加上 n。
func addCounter(iv *[16]byte, n uint64) {
	carry := n
	for i := 15; i >= 0 && carry > 0; i-- {
		sum := uint64(iv[i]) + carry&0xff
		iv[i] = byte(sum)
		carry = carry>>8 + sum>>8
	}
}

// Command 是加密 HTTP 下载的命令：一行 POSIX sh，Linux、macOS 都能用（要有 curl 和 openssl）。
type Command struct {
	URL  string
	Name string // 文件名（来自节点，这里清理后再用）
	Key  ContentKey
	// SHA256 是文件内容的 SHA-256（十六进制），为空时不核对。
	SHA256 string
	// Resume：普通文件，curl 带 -C - 续传；目录打包不能续传。
	Resume bool
}

// String 生成命令：先把密文下载到「文件名.<标识>.part」（同一条命令可以重复运行续传），完整后用 openssl 解密成文件名，
// 删掉密文；有 SHA-256 时最后核对。每个参数都用单引号括起来，文件名里的控制字符换掉，不会被 shell 解释。
func (c Command) String() string {
	name := SafeName(c.Name)
	part := name + "." + c.Key.Tag + ".part"
	curl := "curl -fg "
	if c.Resume {
		curl += "-C - "
	}
	s := curl + "-o " + shellQuote(part) + " " + shellQuote(c.URL) +
		" && openssl enc -d -aes-256-ctr -K " + hex.EncodeToString(c.Key.Key[:]) + " -iv " + hex.EncodeToString(c.Key.IV[:]) +
		" -in " + shellQuote(part) + " -out " + shellQuote(name) +
		" && rm -f -- " + shellQuote(part)
	if c.SHA256 != "" {
		// openssl dgst 各版本输出的格式不同（OpenSSL 1.1、3、LibreSSL），哈希总在最后一个字段。
		s += ` && [ "$(openssl dgst -sha256 < ` + shellQuote(name) + ` | awk '{print $NF}')" = ` + shellQuote(c.SHA256) + ` ]` +
			` && echo 'SHA-256 OK' || echo 'FAILED' >&2`
	}
	return s
}

// SafeName 把文件名清理成能放进命令的样子：去掉路径，控制字符和不可见的格式字符换成 _，
// 空的、只有点的换成 download。结果仍然要经 shellQuote 括起来。
func SafeName(name string) string {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	if !utf8.ValidString(name) {
		name = strings.ToValidUTF8(name, "_")
	}
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == utf8.RuneError {
			return '_'
		}
		return r
	}, name)
	if strings.Trim(name, ".") == "" {
		return "download"
	}
	if len(name) > 200 {
		// 留出 .<标识>.part 的位置（文件名一般最长 255 字节），按字符截断。
		cut := 200
		for cut > 0 && !utf8.RuneStart(name[cut]) {
			cut--
		}
		name = name[:cut]
	}
	return name
}

// shellQuote 用单引号括起来：单引号里只有 ' 本身需要处理（结束引号、转义的 '、再开始引号）。
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
