package directdl

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/netip"
	"net/url"
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

// FileKey 派生普通文件的内容密钥：每个链接一个（链接编号参与派生），同一个链接内续传、分段请求用同一段密钥流；
// 不同链接（重新获取的命令）的密钥流不同，即使文件在两次下载之间变了，也不会用同一段密钥流加密两份不同的内容。
// 文件和 ETag 也参与派生。重新获取的命令不能接着之前下载到一半的部分续传（文件名里的标识跟着变，不会拼错）。
func FileKey(secret []byte, c Claims, etag string) ContentKey {
	return derive(secret, "file", c.Node, c.ID, c.Kind, c.Project, c.Container, c.Volume, c.Snapshot, c.Path, etag)
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

// Command 是加密 HTTP 下载的命令。String 是 POSIX sh 的版本（Linux、macOS，要有 curl 和 openssl），
// Windows 是 PowerShell 的版本（Windows 10、11 自带的 curl.exe 和 .NET，见 windows.go）。
type Command struct {
	URL  string
	Name string // 文件名（来自节点，这里清理后再用）
	Key  ContentKey
	// SHA256 是文件内容的 SHA-256（64 位十六进制），为空或格式不对时不核对（见 ValidSHA256）。
	SHA256 string
	// Resume：普通文件，可以续传、分段下载；目录打包是现生成的流，都不能。
	Resume bool
	// Size 是文件大小（目录打包为 0）。不小于 MultiThreshold 的普通文件分 Threads 段同时下载。
	Size int64
}

// MultiThreshold：不小于这么大的普通文件分段同时下载；更小的、目录打包用一个连接。
const MultiThreshold = 32 << 20

// Threads 是分段下载默认的连接数，写在命令开头（N=4、$N = 4），可以改。
const Threads = 4

// Segmented：这条命令分段同时下载。
func (c Command) Segmented() bool { return c.Resume && c.Size >= MultiThreshold }

// ipFlag 是链接地址的协议族（-4 或 -6）：链接只认第一次下载的来源地址，几个连接要从同一个地址来。
// 链接的主机本来就是 IP，写明只是保险。
func ipFlag(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	a, err := netip.ParseAddr(u.Hostname())
	if err != nil {
		return ""
	}
	if a.Is4() || a.Is4In6() {
		return "-4"
	}
	return "-6"
}

// partBase 是下载到一半的密文的文件名前缀：「文件名.<标识>」。
func (c Command) partBase(name string) string { return name + "." + c.Key.Tag }

// segmentRetries：每段最多试这么多次（每次从已经收到的地方接着下载）。链接发出的字节有上限（文件大小加 4 MiB），
// 各段不重叠，重试只重发断开时在路上、没收到的部分，所以次数要有限。都失败时可以再运行一次命令接着下载。
const segmentRetries = 3

// posixPrelude 选保存的目录并确认能写：$NAGI_DOWNLOAD_DIR，没有时用当前目录，当前目录不可写时用 ~/Downloads；
// 进入这个目录，写明保存的位置。why 把 curl 的退出码（和它写的日志）说成原因：节点对无效的链接一律回 404。
const posixPrelude = `nagi_w() { mkdir -p -- "$1" 2>/dev/null && nagi_t="$1/.nagi-write-test.$$" && (: > "$nagi_t") 2>/dev/null && rm -f -- "$nagi_t"; }
if [ -n "${NAGI_DOWNLOAD_DIR:-}" ]; then dir=$NAGI_DOWNLOAD_DIR; elif nagi_w .; then dir=.; else dir="$HOME/Downloads"; fi
if ! nagi_w "$dir" || ! cd -- "$dir"; then
  echo "FAILED: cannot write to $dir (folder not writable). Set NAGI_DOWNLOAD_DIR to a writable folder and run the command again." >&2; exit 1
fi
echo "Saving to $(pwd)/$out"
why() {
  m=$(tail -n 1 "$2" 2>/dev/null)
  case "$1" in
    22) case "$m" in *409*) echo 'the file changed on the node: generate a new command in the panel' ;;
        *) echo 'the node rejected the link (expired, used from another network address, or used up): generate a new command in the panel' ;; esac ;;
    23) echo 'cannot write the file (folder not writable or disk full)' ;;
    5|6|7) echo 'cannot connect to the node (port blocked or node offline)' ;;
    18|28|52|55|56) echo 'the connection was interrupted: run the command again to resume' ;;
    *) echo "curl exited with $1: $m" ;;
  esac
}
`

// String 生成 POSIX sh 的命令（Linux、macOS，要有 curl 和 openssl），整段在子 shell ( … ) 里运行，粘贴进终端出错时
// 不会关掉终端，也可以由面板作为 .sh 提供（见 ShortPOSIX）。普通文件按 HTTP Range 分成 N 段（不到 MultiThreshold 的
// 文件 1 段），各自用后台的 curl 追加到「文件名.<标识>.<N>.part<i>」，错误写进各自的 .log（几个 curl 的输出不混在
// 一起），断开后从已经收到的地方接着下载（同一条命令可以重复运行）；所有段完成（检查每段的大小，总大小要等于文件大小）
// 后按顺序 cat 起来交给 openssl 解密：CTR 的密钥流只和位置有关，拼起来的密文和一个连接下载的一样。解密后核对大小，
// 有 SHA-256 时核对。目录打包是现生成的流：一个连接，不续传。
// 每个值都用单引号括起来，文件名里的控制字符换掉，不会被 shell 解释。
func (c Command) String() string {
	name := SafeName(c.Name)
	var b strings.Builder
	flag := ipFlag(c.URL)
	if flag != "" {
		flag += " "
	}
	w := func(line string) { b.WriteString(line + "\n") }
	dec := "openssl enc -d -aes-256-ctr -K " + hex.EncodeToString(c.Key.Key[:]) + " -iv " + hex.EncodeToString(c.Key.IV[:])
	again := "run the same command again within " + ttlText() + " to resume; after that, generate a new command in the panel"
	w("(")
	if c.Segmented() {
		w("N=" + strconv.Itoa(Threads))
	} else if c.Resume {
		w("N=1")
	}
	w("url=" + shellQuote(c.URL))
	w("out=" + shellQuote(name))
	w("base=" + shellQuote(c.partBase(name)))
	w("size=" + strconv.FormatInt(c.Size, 10))
	b.WriteString(posixPrelude)
	if !c.Resume {
		w("rm -f -- \"$base.part\"")
		w("curl " + flag + "-fgS -o \"$base.part\" \"$url\" || { rc=$?; echo \"FAILED: $(why \"$rc\" '')\" >&2; exit 1; }")
		w(dec + " -in \"$base.part\" -out \"$out\" || { echo \"FAILED: cannot write $(pwd)/$out\" >&2; exit 1; }")
		w("rm -f -- \"$base.part\"")
		w("echo 'Done'")
		w("echo \"Saved to $(pwd)/$out\"")
		b.WriteString(")")
		return b.String()
	}
	w("seg=$(( (size + N - 1) / N )); [ \"$seg\" -gt 0 ] || seg=1; count=$(( (size + seg - 1) / seg ))")
	w("have=0; i=0")
	w("while [ \"$i\" -lt \"$count\" ]; do f=\"$base.$N.part$i\"; rm -f -- \"$f.rc\" \"$f.log\"; [ -f \"$f\" ] && have=$((have + $(wc -c < \"$f\"))); i=$((i + 1)); done")
	w("free=$(df -Pk . 2>/dev/null | awk 'NR==2 {print $4}')")
	w("if [ -n \"$free\" ] && [ $((free * 1024)) -lt $((2 * size - have)) ]; then")
	w("  echo \"FAILED: not enough disk space in $(pwd): $(( (2 * size - have) / 1048576 + 1 )) MiB needed, $((free / 1024)) MiB free\" >&2; exit 1")
	w("fi")
	w("i=0; pids=")
	w("while [ \"$i\" -lt \"$count\" ]; do")
	w("  (")
	w("    a=$((i * seg)); b=$((a + seg - 1)); [ \"$b\" -lt \"$size\" ] || b=$((size - 1))")
	w("    f=\"$base.$N.part$i\"; tries=0")
	w("    while :; do")
	w("      have=0; [ -f \"$f\" ] && have=$(($(wc -c < \"$f\")))")
	w("      [ $((a + have)) -le \"$b\" ] || exit 0")
	w("      [ \"$tries\" -lt " + strconv.Itoa(segmentRetries) + " ] || exit 1")
	w("      tries=$((tries + 1))")
	w("      curl " + flag + "-fgsS -r \"$((a + have))-$b\" \"$url\" >> \"$f\" 2> \"$f.log\" && continue")
	w("      rc=$?; echo \"$rc\" > \"$f.rc\"")
	w("      case \"$rc\" in 22|23) exit 1 ;; esac")
	w("      sleep 1")
	w("    done")
	w("  ) &")
	w("  pids=\"$pids $!\"; i=$((i + 1))")
	w("done")
	w("echo \"Downloading $count segment(s) of $size bytes\"")
	w("for p in $pids; do wait \"$p\"; done")
	w("set --; total=0; i=0")
	w("while [ \"$i\" -lt \"$count\" ]; do")
	w("  f=\"$base.$N.part$i\"; set -- \"$@\" \"$f\"")
	w("  [ -f \"$f\" ] && total=$((total + $(wc -c < \"$f\"))); i=$((i + 1))")
	w("done")
	w("if [ \"$total\" -ne \"$size\" ]; then")
	w("  for f in \"$@\"; do [ -f \"$f.rc\" ] && echo \"FAILED: $(why \"$(cat \"$f.rc\")\" \"$f.log\")\"; done | sort -u >&2")
	w("  echo \"FAILED: download incomplete ($total of $size bytes), " + again + "\" >&2; exit 1")
	w("fi")
	w("cat -- \"$@\" < /dev/null | " + dec + " -out \"$out\" || { echo \"FAILED: cannot write $(pwd)/$out\" >&2; exit 1; }")
	w("[ \"$(($(wc -c < \"$out\")))\" -eq \"$size\" ] || { echo 'FAILED: the decrypted file has the wrong size' >&2; exit 1; }")
	w("for f in \"$@\"; do rm -f -- \"$f\" \"$f.rc\" \"$f.log\"; done")
	if ValidSHA256(c.SHA256) {
		w("if [ \"$(openssl dgst -sha256 < \"$out\" | awk '{print $NF}')\" = " + shellQuote(c.SHA256) + " ]; then echo 'SHA-256 OK'; else echo 'FAILED: SHA-256 mismatch' >&2; exit 1; fi")
	} else {
		w("echo 'Done'")
	}
	w("echo \"Saved to $(pwd)/$out\"")
	b.WriteString(")")
	return b.String()
}

// SafeName 把文件名清理成能放进命令的样子：去掉路径（/ 之前的部分；Linux 的文件名里可以有 \，照原样保留），
// 控制字符和不可见的格式字符换成 _，空的、只有点的换成 download。结果仍然要经 shellQuote 括起来。
func SafeName(name string) string {
	if i := strings.LastIndex(name, "/"); i >= 0 {
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

// ValidSHA256 判断 s 是不是 64 位十六进制（大小写都行）。拼进命令之前一律先检查：值来自节点。
func ValidSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}
