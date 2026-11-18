package directdl

import (
	"bytes"
	"crypto/aes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// 已知答案：NIST SP 800-38A F.5.5（CTR-AES256），第二块起计数器的最后一个字节进位。
func TestStreamKnownAnswer(t *testing.T) {
	var k ContentKey
	copy(k.Key[:], unhex(t, "603deb1015ca71be2b73aef0857d77811f352c073b6108d72d9810a30914dff4"))
	copy(k.IV[:], unhex(t, "f0f1f2f3f4f5f6f7f8f9fafbfcfdfeff"))
	plain := unhex(t, "6bc1bee22e409f96e93d7e117393172aae2d8a571e03ac9c9eb76fac45af8e5130c81c46a35ce411e5fbc1191a0a52eff69f2445df4f9b17ad2b417be66c3710")
	want := unhex(t, "601ec313775789a5b7a7f504bbf3d228f443e3ca4d62b59aca84e990cacaf5c52b0930daa23de94ce87017ba2d84988ddfc9c58db67aada613c2dd08457941a6")
	got := make([]byte, len(plain))
	k.Stream(0).XORKeyStream(got, plain)
	if !bytes.Equal(got, want) {
		t.Fatalf("密文不对：%x", got)
	}
	// 从中间开始（续传、分段请求）：和整段密文的那一部分一样。
	for _, off := range []int64{1, 15, 16, 17, 33, 63} {
		part := make([]byte, len(plain)-int(off))
		k.Stream(off).XORKeyStream(part, plain[off:])
		if !bytes.Equal(part, want[off:]) {
			t.Errorf("从 %d 开始不对", off)
		}
	}
}

// 计数器是完整的 128 位大端整数（openssl 的 CTR 也是）：低 64 位溢出时向高位进位，全 1 时回绕到 0。
// 按定义逐块算出密钥流（AES 加密计数器），和 Stream 从各个位置开始的比较。
func TestStreamCounter(t *testing.T) {
	for _, ivHex := range []string{
		"00000000000000000000000000000000",
		"0000000000000000fffffffffffffffe", // 低 64 位溢出
		"fffffffffffffffffffffffffffffffd", // 128 位回绕
		"0123456789abcdef00000000000000ff",
	} {
		var k ContentKey
		_, _ = rand.Read(k.Key[:])
		copy(k.IV[:], unhex(t, ivHex))
		block, _ := aes.NewCipher(k.Key[:])
		const n = 6 * aes.BlockSize
		ref := make([]byte, n)
		ctr := k.IV
		for i := 0; i < n; i += aes.BlockSize {
			block.Encrypt(ref[i:], ctr[:])
			addCounter(&ctr, 1)
		}
		for off := int64(0); off < n; off++ {
			got := make([]byte, n-off)
			k.Stream(off).XORKeyStream(got, got)
			if !bytes.Equal(got, ref[off:]) {
				t.Fatalf("IV %s 从 %d 开始的密钥流不对", ivHex, off)
			}
		}
	}
	// 大的偏移：计数器加 offset/16。
	var iv [16]byte
	copy(iv[:], unhex(t, "00000000000000000000000000ffffff"))
	addCounter(&iv, 0x0101)
	if hex.EncodeToString(iv[:]) != "00000000000000000000000001000100" {
		t.Errorf("进位不对：%x", iv)
	}
}

func TestKeys(t *testing.T) {
	secret := bytes.Repeat([]byte{7}, 32)
	c := Claims{Node: "n1", Kind: KindVolume, Project: "p", Container: "web", Volume: "data", Path: "a.bin", ID: "1", Expires: 1}
	a := FileKey(secret, c, `"10-20"`)
	// 同一个文件、同一个版本：换了链接（编号、到期时间）也是同一个密钥，可以接着续传。
	c2 := c
	c2.ID, c2.Expires = "2", 2
	if FileKey(secret, c2, `"10-20"`) != a {
		t.Error("同一个文件的密钥应该相同")
	}
	for name, other := range map[string]ContentKey{
		"文件变了": FileKey(secret, c, `"10-21"`),
		"换了文件": FileKey(secret, Claims{Node: "n1", Kind: KindVolume, Project: "p", Container: "web", Volume: "data", Path: "b.bin"}, `"10-20"`),
		"换了密钥": FileKey(bytes.Repeat([]byte{8}, 32), c, `"10-20"`),
		"目录打包": ArchiveKey(secret, c),
		"快照里的": FileKey(secret, Claims{Node: "n1", Kind: KindVolume, Project: "p", Container: "web", Volume: "data", Path: "a.bin", Snapshot: "s"}, `"10-20"`),
		"字段拼接": FileKey(secret, Claims{Node: "n1", Kind: KindVolume, Project: "p", Container: "web", Volume: "dat", Path: "aa.bin"}, `"10-20"`),
	} {
		if other.Key == a.Key || other.IV == a.IV || other.Tag == a.Tag {
			t.Errorf("%s：密钥应该不同", name)
		}
	}
	// 目录打包每个链接一个密钥。
	if ArchiveKey(secret, c) == ArchiveKey(secret, c2) {
		t.Error("目录打包的密钥应该每个链接不同")
	}
	if len(a.Tag) != 8 {
		t.Errorf("标识：%q", a.Tag)
	}
}

// HTTPS 和加密 HTTP 的链接不能互换。
func TestTransportSeparation(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	now := time.Unix(1_800_000_000, 0)
	c := Claims{Node: "n1", Kind: KindNode, Path: "/etc/hosts", Expires: now.Add(TTL).Unix(), ID: "x"}
	h := SignHTTP(key, c)
	s := Sign(key, c)
	if got, err := VerifyHTTP([][]byte{key}, h, now); err != nil || got.Transport != TransportHTTP || got.Path != c.Path {
		t.Fatalf("HTTP 链接应该通过：%+v %v", got, err)
	}
	if _, err := Verify([][]byte{key}, h, now); err != ErrInvalid {
		t.Errorf("HTTP 链接不能当 HTTPS 用：%v", err)
	}
	if _, err := VerifyHTTP([][]byte{key}, s, now); err != ErrInvalid {
		t.Errorf("HTTPS 链接不能当 HTTP 用：%v", err)
	}
	// 用 HTTPS 的签名上下文签一个写着 Transport 的链接，也不能当 HTTP 用；反过来同样。
	forged := sign(key, macHTTPS, Claims{Node: "n1", Kind: KindNode, Path: "/", Expires: c.Expires, Transport: TransportHTTP})
	if _, err := Verify([][]byte{key}, forged, now); err != ErrInvalid {
		t.Errorf("写着 Transport 的 HTTPS 链接不该通过：%v", err)
	}
	if _, err := VerifyHTTP([][]byte{key}, forged, now); err != ErrInvalid {
		t.Errorf("HTTPS 上下文签的链接不该当 HTTP 用：%v", err)
	}
	plain := sign(key, macHTTP, Claims{Node: "n1", Kind: KindNode, Path: "/", Expires: c.Expires})
	if _, err := VerifyHTTP([][]byte{key}, plain, now); err != ErrInvalid {
		t.Errorf("没写 Transport 的不该通过：%v", err)
	}
	if got, err := VerifyHTTP([][]byte{key}, h, now.Add(TTL+time.Second)); err != ErrExpired || got.ID != "x" {
		t.Errorf("过期：%+v %v", got, err)
	}
	if got := HTTPURL("203.0.113.5", 80, "t"); got != "http://203.0.113.5/.nagi/dl/t" {
		t.Error(got)
	}
	if got := HTTPURL("2001:db8::1", 8080, "t"); got != "http://[2001:db8::1]:8080/.nagi/dl/t" {
		t.Error(got)
	}
}

func TestSafeName(t *testing.T) {
	for in, want := range map[string]string{
		"a.txt":          "a.txt",
		"dir/b.txt":      "b.txt",
		`..\c.txt`:       `..\c.txt`,
		`it's \.bin`:     `it's \.bin`,
		"":               "download",
		"..":             "download",
		"a\nb\rc\x00d":   "a_b_c_d",
		"x‮y.txt":        "x_y.txt",
		"it's $(rm) `x`": "it's $(rm) `x`",
		"-rf":            "-rf",
		"\xff\xfe.bin":   "_.bin",
	} {
		if got := SafeName(in); got != want {
			t.Errorf("SafeName(%q) = %q，想要 %q", in, got, want)
		}
	}
	if got := SafeName(strings.Repeat("文", 100)); len(got) > 200 || !strings.HasPrefix(got, "文") || strings.ToValidUTF8(got, "") != got {
		t.Errorf("长名字：%d %q", len(got), got)
	}
}

func TestCommandFormat(t *testing.T) {
	k := FileKey(bytes.Repeat([]byte{1}, 32), Claims{Node: "n"}, "e")
	c := Command{URL: "http://203.0.113.5/.nagi/dl/abc.def", Name: "it's.txt", Key: k, SHA256: strings.Repeat("a", 64), Resume: true}
	s := c.String()
	if strings.ContainsAny(s, "\n\r") {
		t.Fatal("命令应该只有一行")
	}
	for _, want := range []string{"curl -fg -C - -o 'it'\\''s.txt." + k.Tag + ".part' 'http://203.0.113.5/.nagi/dl/abc.def'",
		"openssl enc -d -aes-256-ctr -K " + hex.EncodeToString(k.Key[:]) + " -iv " + hex.EncodeToString(k.IV[:]),
		"-out 'it'\\''s.txt'", "rm -f -- ", "openssl dgst -sha256", "'" + strings.Repeat("a", 64) + "'"} {
		if !strings.Contains(s, want) {
			t.Errorf("命令里没有 %q：%s", want, s)
		}
	}
	c.Resume, c.SHA256 = false, ""
	if s := c.String(); strings.Contains(s, "-C -") || strings.Contains(s, "dgst") {
		t.Errorf("目录打包不续传、不核对：%s", s)
	}
}

// 文件名里的 shell 特殊字符不会被解释：用假的 curl、openssl、rm 记下各自收到的参数，看文件名原样传到。
func TestCommandQuoting(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("需要 sh")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("没有 sh")
	}
	bin := t.TempDir()
	for _, tool := range []string{"curl", "openssl", "rm"} {
		script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done >> \"$LOG\"\necho -- >> \"$LOG\"\n"
		if tool == "openssl" {
			// dgst 要输出哈希给后面比较。
			script += "if [ \"$1\" = dgst ]; then echo '(stdin)= ok'; fi\n"
		}
		if err := os.WriteFile(filepath.Join(bin, tool), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"it's $(touch pwned) `touch pwned2` ; touch pwned3 && x", "-rf", "a\"b\\c", "$HOME*?[a]", "line\nbreak"} {
		dir := t.TempDir()
		log := filepath.Join(dir, "log")
		// 假的 openssl 不写文件，先放一个解密后的文件给最后的核对读。
		if err := os.WriteFile(filepath.Join(dir, SafeName(name)), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		c := Command{URL: "http://203.0.113.5/.nagi/dl/x.y", Name: name, Key: ArchiveKey([]byte("k"), Claims{}), SHA256: "ok", Resume: true}
		cmd := exec.Command(sh, "-c", c.String())
		cmd.Dir = dir
		cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "LOG=" + log, "HOME=/nonexistent"}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%q：%v %s", name, err, out)
		}
		if !strings.Contains(string(out), "SHA-256 OK") {
			t.Errorf("%q：没有核对通过：%s", name, out)
		}
		for _, f := range []string{"pwned", "pwned2", "pwned3"} {
			if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
				t.Fatalf("%q：文件名被 shell 执行了", name)
			}
		}
		b, _ := os.ReadFile(log)
		safe := SafeName(name)
		args := strings.Split(string(b), "\n")
		has := func(v string) bool {
			for _, a := range args {
				if a == v {
					return true
				}
			}
			return false
		}
		if !has(safe) || !has(safe+"."+c.Key.Tag+".part") {
			t.Errorf("%q：参数里没有原样的文件名 %q：%q", name, safe, args)
		}
	}
}

// 端到端：用真的 curl 和 openssl 跑命令，从一个按 Range 发密文的 HTTP 服务下载、解密、核对 SHA-256，
// 包括先下载一半、再运行同一条命令续传。没有 curl 或 openssl 时跳过。
func TestCommandOpenSSL(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("需要 sh")
	}
	for _, tool := range []string{"sh", "curl", "openssl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("没有 %s", tool)
		}
	}
	plain := make([]byte, 3<<20+12345)
	_, _ = rand.Read(plain)
	sum := sha256.Sum256(plain)
	k := FileKey(bytes.Repeat([]byte{9}, 32), Claims{Node: "n", Path: "x"}, `"1-2"`)
	cipherText := make([]byte, len(plain))
	k.Stream(0).XORKeyStream(cipherText, plain)

	var mu sync.Mutex
	var requests []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Header.Get("Range"))
		mu.Unlock()
		// 每一段按它的起点重新加密（和节点一样：从 offset 开始的密钥流），不是从整段密文里切。
		off := int64(0)
		if rg := r.Header.Get("Range"); rg != "" {
			fmt.Sscanf(rg, "bytes=%d-", &off)
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", off, len(plain)-1, len(plain)))
			w.Header().Set("Content-Length", fmt.Sprint(int64(len(plain))-off))
			w.WriteHeader(http.StatusPartialContent)
		}
		out := make([]byte, int64(len(plain))-off)
		k.Stream(off).XORKeyStream(out, plain[off:])
		_, _ = w.Write(out)
	}))
	defer srv.Close()

	name := "data 'v1'.bin"
	c := Command{URL: srv.URL + HTTPPathPrefix + "tok", Name: name, Key: k, SHA256: hex.EncodeToString(sum[:]), Resume: true}
	dir := t.TempDir()
	// 先放一半密文，模拟断开的下载。
	part := filepath.Join(dir, name+"."+k.Tag+".part")
	if err := os.WriteFile(part, cipherText[:1<<20+7], 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", c.String())
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "SHA-256 OK") {
		t.Fatalf("命令失败：%v\n%s\n%s", err, c.String(), out)
	}
	got, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("解密的内容不对：%v", err)
	}
	if _, err := os.Stat(part); err == nil {
		t.Error("密文应该删掉")
	}
	mu.Lock()
	got1 := append([]string(nil), requests...)
	mu.Unlock()
	if len(got1) != 1 || got1[0] != fmt.Sprintf("bytes=%d-", 1<<20+7) {
		t.Errorf("应该从断开的地方续传：%q", got1)
	}
	// 内容不对时提示失败。
	c.SHA256 = strings.Repeat("0", 64)
	cmd = exec.Command("sh", "-c", c.String())
	cmd.Dir = t.TempDir()
	out, _ = cmd.CombinedOutput()
	if !strings.Contains(string(out), "FAILED") {
		t.Errorf("SHA-256 对不上时应该提示：%s", out)
	}
}

// 直接用 openssl 解密 Go 加密的内容（不经 curl），确认两边的 CTR 一致，包括计数器跨过 64 位的情况。
func TestOpenSSLDecrypt(t *testing.T) {
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("没有 openssl")
	}
	for _, ivHex := range []string{"", "0000000000000000ffffffffffffff00", "ffffffffffffffffffffffffffffff00"} {
		k := ArchiveKey([]byte("secret"), Claims{ID: ivHex})
		if ivHex != "" {
			copy(k.IV[:], unhex(t, ivHex))
		}
		plain := make([]byte, 100_003)
		_, _ = rand.Read(plain)
		enc := make([]byte, len(plain))
		k.Stream(0).XORKeyStream(enc, plain)
		cmd := exec.Command("openssl", "enc", "-d", "-aes-256-ctr", "-K", hex.EncodeToString(k.Key[:]), "-iv", hex.EncodeToString(k.IV[:]))
		cmd.Stdin = bytes.NewReader(enc)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		got, err := cmd.Output()
		if err != nil {
			t.Fatalf("openssl：%v %s", err, stderr.String())
		}
		if !bytes.Equal(got, plain) {
			t.Fatalf("IV %q：openssl 解密的内容不对", ivHex)
		}
	}
}
