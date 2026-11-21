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
	// 同一个链接（到期时间不参与）：同一个密钥，链接内可以续传。换了链接（重新获取的命令）就换一个密钥。
	c1 := c
	c1.Expires = 9
	if FileKey(secret, c1, `"10-20"`) != a {
		t.Error("同一个链接的密钥应该相同")
	}
	c2 := c
	c2.ID, c2.Expires = "2", 2
	if k := FileKey(secret, c2, `"10-20"`); k.Key == a.Key || k.IV == a.IV || k.Tag == a.Tag {
		t.Error("不同链接的密钥应该不同")
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
	c := Command{URL: "http://203.0.113.5/.nagi/dl/abc.def", Name: "it's.txt", Key: k, SHA256: strings.Repeat("a", 64), Resume: true, Size: 1000}
	s := c.String()
	for _, want := range []string{"N=1\n", "out='it'\\''s.txt'", "base='it'\\''s.txt." + k.Tag + "'", "url='http://203.0.113.5/.nagi/dl/abc.def'",
		"curl -4 -fgsS -r ", "2> \"$f.log\"", "NAGI_DOWNLOAD_DIR", "Saving to ",
		"openssl enc -d -aes-256-ctr -K " + hex.EncodeToString(k.Key[:]) + " -iv " + hex.EncodeToString(k.IV[:]),
		"openssl dgst -sha256", "'" + strings.Repeat("a", 64) + "'", "within 5 minutes"} {
		if !strings.Contains(s, want) {
			t.Errorf("命令里没有 %q：%s", want, s)
		}
	}
	c.Resume, c.SHA256, c.Size = false, "", 0
	if s := c.String(); strings.Contains(s, "-r \"") || strings.Contains(s, "N=") || strings.Contains(s, "dgst") {
		t.Errorf("目录打包不分段、不续传、不核对：%s", s)
	}
}

// fakeTools 在 bin 里放假的 curl、openssl：参数逐行记进 $LOG。openssl dgst 输出 sum。
func fakeTools(t *testing.T, bin, sum string) {
	t.Helper()
	for _, tool := range []string{"curl", "openssl"} {
		script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done >> \"$LOG\"\necho -- >> \"$LOG\"\n"
		if tool == "openssl" {
			// dgst 要输出哈希给后面比较；解密时把 -out 指向的文件写成 size 字节。
			script += "if [ \"$1\" = dgst ]; then echo '(stdin)= " + sum + "'; exit 0; fi\n" +
				"cat > /dev/null; while [ $# -gt 1 ]; do [ \"$1\" = -out ] && head -c \"$SIZE\" /dev/zero > \"$2\"; shift; done\n"
		} else {
			script += "printf 'x'\n"
		}
		if err := os.WriteFile(filepath.Join(bin, tool), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// 文件名里的 shell 特殊字符不会被解释：用假的 curl、openssl 记下各自收到的参数，看文件名原样传到。
func TestCommandQuoting(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("需要 sh")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("没有 sh")
	}
	bin := t.TempDir()
	// SHA-256 只认 64 位十六进制（见 ValidSHA256），假的 openssl dgst 输出同一个值。
	sum := strings.Repeat("ab", 32)
	fakeTools(t, bin, sum)
	for _, name := range []string{"it's $(touch pwned) `touch pwned2` ; touch pwned3 && x", "-rf", "a\"b\\c", "$HOME*?[a]", "line\nbreak"} {
		dir := t.TempDir()
		log := filepath.Join(t.TempDir(), "log")
		c := Command{URL: "http://203.0.113.5/.nagi/dl/x.y", Name: name, Key: ArchiveKey([]byte("k"), Claims{}), SHA256: sum, Resume: true, Size: 1}
		cmd := exec.Command(sh, "-c", c.String())
		cmd.Dir = dir
		cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "LOG=" + log, "SIZE=1", "HOME=/nonexistent"}
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), "SHA-256 OK") {
			t.Fatalf("%q：%v %s", name, err, out)
		}
		for _, f := range []string{"pwned", "pwned2", "pwned3"} {
			if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
				t.Fatalf("%q：文件名被 shell 执行了", name)
			}
		}
		safe := SafeName(name)
		if _, err := os.Stat(filepath.Join(dir, safe)); err != nil {
			t.Errorf("%q：没有按原样的文件名 %q 保存：%v", name, safe, err)
		}
		b, _ := os.ReadFile(log)
		if !strings.Contains(string(b), "\n-out\n"+safe+"\n") {
			t.Errorf("%q：openssl 的参数里没有原样的文件名：%q", name, b)
		}
	}
}

// 保存的位置：默认当前目录，不可写时 ~/Downloads，NAGI_DOWNLOAD_DIR 指定的目录不可写时报原因、不下载。
func TestCommandSaveDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("需要 sh")
	}
	if os.Getuid() == 0 {
		t.Skip("root 不受目录权限限制")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("没有 sh")
	}
	bin := t.TempDir()
	sum := strings.Repeat("ab", 32)
	fakeTools(t, bin, sum)
	c := Command{URL: "http://203.0.113.5/.nagi/dl/x.y", Name: "a b.bin", Key: ArchiveKey([]byte("k"), Claims{}), SHA256: sum, Resume: true, Size: 1}
	ro := t.TempDir()
	if err := os.Chmod(ro, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(ro, 0o755)
	home := t.TempDir()
	run := func(env ...string) string {
		cmd := exec.Command(sh, "-c", c.String())
		cmd.Dir = ro
		cmd.Env = append([]string{"PATH=" + bin + ":/usr/bin:/bin", "LOG=/dev/null", "SIZE=1", "HOME=" + home}, env...)
		out, _ := cmd.CombinedOutput()
		return string(out)
	}
	if out := run(); !strings.Contains(out, "Saved to "+filepath.Join(home, "Downloads", "a b.bin")) {
		t.Errorf("当前目录不可写时应该存到 ~/Downloads：%s", out)
	}
	if out := run("NAGI_DOWNLOAD_DIR=" + filepath.Join(ro, "x")); !strings.Contains(out, "folder not writable") || strings.Contains(out, "Saving") {
		t.Errorf("指定的目录不可写时应该说明：%s", out)
	}
}

// 端到端：用真的 curl 和 openssl 跑一个连接的命令（小文件也是 1 段），从一个按 Range 发密文的 HTTP 服务下载、解密、
// 核对 SHA-256，包括先下载一半、再运行同一条命令续传。没有 curl 或 openssl 时跳过。
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
	srv := &rangeServer{plain: plain, key: k}
	hs := httptest.NewServer(srv)
	defer hs.Close()

	name := "data 'v1' 中文.bin"
	c := Command{URL: hs.URL + HTTPPathPrefix + "tok", Name: name, Key: k, SHA256: hex.EncodeToString(sum[:]), Resume: true, Size: int64(len(plain))}
	dir := t.TempDir()
	// 先放一部分密文，模拟断开的下载。
	enc := make([]byte, 1<<20+7)
	k.Stream(0).XORKeyStream(enc, plain[:len(enc)])
	part := filepath.Join(dir, c.partBase(name)+".1.part0")
	if err := os.WriteFile(part, enc, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", c.String())
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "SHA-256 OK") {
		t.Fatalf("命令失败：%v\n%s", err, out)
	}
	got, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("解密的内容不对：%v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("只应该留下解密的文件：%v", entries)
	}
	if want := fmt.Sprintf("bytes=%d-%d", len(enc), len(plain)-1); len(srv.requests) != 1 || srv.requests[0] != want {
		t.Errorf("应该从断开的地方续传（%s）：%q", want, srv.requests)
	}
	// 内容不对时提示失败。
	c.SHA256 = strings.Repeat("0", 64)
	cmd = exec.Command("sh", "-c", c.String())
	cmd.Dir = t.TempDir()
	out, _ = cmd.CombinedOutput()
	if !strings.Contains(string(out), "FAILED: SHA-256 mismatch") {
		t.Errorf("SHA-256 对不上时应该提示：%s", out)
	}
	// 链接被节点拒绝（404）时说明原因，不再重试，再运行提示重新生成。
	srv404 := httptest.NewServer(http.NotFoundHandler())
	defer srv404.Close()
	c.URL = srv404.URL + HTTPPathPrefix + "tok"
	cmd = exec.Command("sh", "-c", c.String())
	cmd.Dir = t.TempDir()
	out, _ = cmd.CombinedOutput()
	if !strings.Contains(string(out), "rejected the link") || strings.Count(string(out), "rejected the link") != 1 {
		t.Errorf("404 时应该说明一次原因：%s", out)
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
