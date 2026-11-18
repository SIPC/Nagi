package directdl

import (
	"encoding/hex"
	"strconv"
	"strings"
)

// Windows 版的加密 HTTP 下载命令（见 docs/agent.md#加密-http-下载）：PowerShell 脚本，Windows 10、11 自带的
// Windows PowerShell 5.1 和 PowerShell 7 都能运行，不用装 OpenSSL。cmd.exe 不支持。
//   - 下载用系统自带的 curl.exe（写明 .exe：Windows PowerShell 5.1 里 curl 是 Invoke-WebRequest 的别名）。
//   - .NET 没有 CTR 模式：用 Add-Type 编译一小段 C#，拿 AES-ECB 加密计数器块得到密钥流，按 1 MiB 一块异或。
//     计数器和 openssl、节点一样是 128 位大端、每 16 字节加一（溢出回绕），IV 相同。
//   - 最后用 Get-FileHash 核对 SHA-256，输出 SHA-256 OK 或 FAILED。
//
// 整段可以一次粘贴进 PowerShell 窗口：包在 & { } 里，出错时 return，不会关掉窗口，变量也不留在会话里。
// 文件名放进单引号字符串（' 和弯引号成对写），先按 Windows 的规则清理（WinSafeName）。

// ctrType 是 C# 类型的名字，改了实现要换一个（同一个 PowerShell 会话里已经加载的类型不能重新定义）。
const ctrType = "NagiCtr1"

// ctrSource 是 C# 的实现。只用 C# 5 的写法（Windows PowerShell 5.1 的 Add-Type 用 .NET Framework 自带的编译器），
// 只引用 mscorlib 里的类型：AES 的加密器（ICryptoTransform）由 PowerShell 创建后传进来。
const ctrSource = `using System;
using System.IO;
using System.Security.Cryptography;
public static class NagiCtr1 {
    public static byte[] Hex(string s) {
        byte[] b = new byte[s.Length / 2];
        for (int i = 0; i < b.Length; i++) b[i] = Convert.ToByte(s.Substring(i * 2, 2), 16);
        return b;
    }
    public static long Length(string path) {
        return File.Exists(path) ? new FileInfo(path).Length : 0;
    }
    // Append adds what one curl run received (tmp) to the segment file, then deletes tmp.
    public static void Append(string tmp, string part) {
        if (!File.Exists(tmp)) return;
        using (FileStream o = new FileStream(part, FileMode.Append, FileAccess.Write))
        using (FileStream i = new FileStream(tmp, FileMode.Open, FileAccess.Read)) { i.CopyTo(o); }
        File.Delete(tmp);
    }
    // Decrypt reads the segments in order and writes the AES-256-CTR plaintext. ecb: AES-ECB encryptor, no padding.
    // The counter is a 128-bit big-endian number starting at the IV, like openssl enc -aes-256-ctr.
    public static void Decrypt(string[] inputs, string output, ICryptoTransform ecb, string ivHex) {
        byte[] ctr = Hex(ivHex);
        const int B = 1 << 20;
        byte[] buf = new byte[B], blocks = new byte[B], ks = new byte[B];
        int next = 0;
        Stream cur = null;
        try {
            using (FileStream o = new FileStream(output, FileMode.Create, FileAccess.Write)) {
                while (true) {
                    int n = 0;
                    while (n < B) {
                        if (cur == null) {
                            if (next >= inputs.Length) break;
                            cur = new FileStream(inputs[next++], FileMode.Open, FileAccess.Read);
                        }
                        int r = cur.Read(buf, n, B - n);
                        if (r == 0) { cur.Dispose(); cur = null; continue; }
                        n += r;
                    }
                    if (n == 0) break;
                    int nb = (n + 15) / 16;
                    for (int j = 0; j < nb; j++) {
                        Buffer.BlockCopy(ctr, 0, blocks, j * 16, 16);
                        for (int k = 15; k >= 0; k--) { ctr[k]++; if (ctr[k] != 0) break; }
                    }
                    ecb.TransformBlock(blocks, 0, nb * 16, ks, 0);
                    for (int j = 0; j < n; j++) buf[j] ^= ks[j];
                    o.Write(buf, 0, n);
                    if (n < B) break;
                }
            }
        } finally {
            if (cur != null) cur.Dispose();
        }
    }
}
`

// CTRSource 返回 C# 的实现（测试用）。
func CTRSource() string { return ctrSource }

// Windows 生成 PowerShell 的命令。分段下载（Segmented）时各段用 Start-Process 同时运行 curl.exe，每段先下到
// 「<段>.tmp」，结束后接到「文件名.<标识>.<N>.part<i>」后面，再从收到的地方接着下载，最多 segmentRetries 轮；
// 一个连接时下载到「文件名.<标识>.part」（普通文件 curl.exe -C - 续传）。同一条命令可以重复运行续传。
func (c Command) Windows() string {
	name := WinSafeName(SafeName(c.Name))
	base := c.partBase(name)
	var b strings.Builder
	w := func(line string) { b.WriteString(line + "\n") }
	w("# PowerShell (Windows 10/11). cmd.exe is not supported.")
	w("& {")
	if c.Segmented() {
		w("$N = " + strconv.Itoa(Threads))
	}
	w("$url = " + psQuote(c.URL))
	w("$out = " + psQuote(name))
	w("$base = " + psQuote(base))
	w("[long]$size = " + strconv.FormatInt(c.Size, 10))
	w("$ErrorActionPreference = 'Stop'")
	w("$dir = (Get-Location -PSProvider FileSystem).ProviderPath")
	w("if (-not ('" + ctrType + "' -as [type])) {")
	w("Add-Type -TypeDefinition @'")
	b.WriteString(ctrSource)
	w("'@")
	w("}")
	flag := ipFlag(c.URL)
	args := "'-fgS'"
	if flag != "" {
		args = "'" + flag + "', " + args
	}
	if c.Segmented() {
		w("$seg = [long][math]::Floor(($size + $N - 1) / $N)")
		w("$count = [int][math]::Ceiling($size / $seg)")
		w("$parts = @(for ($i = 0; $i -lt $count; $i++) { Join-Path $dir ($base + '.' + $N + '.part' + $i) })")
		w("for ($try = 0; $try -lt " + strconv.Itoa(segmentRetries) + "; $try++) {")
		w("  $run = @()")
		w("  for ($i = 0; $i -lt $count; $i++) {")
		w("    $f = $parts[$i]; [" + ctrType + "]::Append($f + '.tmp', $f)")
		w("    $a = $i * $seg; $e = [math]::Min($a + $seg, $size) - 1; $from = $a + [" + ctrType + "]::Length($f)")
		w("    if ($from -gt $e) { continue }")
		w("    $p = Start-Process -FilePath curl.exe -ArgumentList @(" + strings.Replace(args, "-fgS", "-fgsS", 1) +
			", '-r', ('{0}-{1}' -f $from, $e), '-o', ('\"' + $f + '.tmp\"'), $url) -WorkingDirectory $dir -NoNewWindow -PassThru")
		w("    $null = $p.Handle; $run += $p")
		w("  }")
		w("  if ($run.Count -eq 0) { break }")
		w("  foreach ($p in $run) { $p.WaitForExit(); if ($p.ExitCode -ne 0) { Write-Host ('curl.exe exited with ' + $p.ExitCode) } }")
		w("  if ($try -lt " + strconv.Itoa(segmentRetries-1) + ") { Start-Sleep -Seconds 1 }")
		w("}")
		w("$total = 0")
		w("foreach ($f in $parts) { [" + ctrType + "]::Append($f + '.tmp', $f); $total += [" + ctrType + "]::Length($f) }")
		w("if ($total -ne $size) { Write-Host 'FAILED: download incomplete, run the command again to resume'; return }")
	} else {
		w("$parts = @(Join-Path $dir ($base + '.part'))")
		if c.Resume {
			args += ", '-C', '-'"
		}
		w("& curl.exe " + strings.ReplaceAll(args, ", ", " ") + " -o $parts[0] $url")
		w("if ($LASTEXITCODE -ne 0) { Write-Host 'FAILED: download incomplete, run the command again'; return }")
	}
	w("$aes = [Security.Cryptography.Aes]::Create()")
	w("$aes.Mode = 'ECB'; $aes.Padding = 'None'; $aes.Key = [" + ctrType + "]::Hex('" + hex.EncodeToString(c.Key.Key[:]) + "')")
	w("$o = Join-Path $dir $out")
	w("[" + ctrType + "]::Decrypt([string[]]$parts, $o, $aes.CreateEncryptor(), '" + hex.EncodeToString(c.Key.IV[:]) + "')")
	w("foreach ($f in $parts) { Remove-Item -LiteralPath $f }")
	if c.SHA256 != "" {
		w("if ((Get-FileHash -LiteralPath $o -Algorithm SHA256).Hash -eq '" + c.SHA256 + "') { 'SHA-256 OK' } else { Write-Host 'FAILED' }")
	} else {
		w("'Done'")
	}
	b.WriteString("}")
	return b.String()
}

// psQuote 用 PowerShell 的单引号括起来：单引号里只有引号本身需要处理，PowerShell 把弯单引号（‘ ’ ‚ ‛）也当单引号，
// 都写两遍。
func psQuote(s string) string {
	var b strings.Builder
	b.WriteByte('\'')
	for _, r := range s {
		switch r {
		case '\'', '‘', '’', '‚', '‛':
			b.WriteRune(r)
		}
		b.WriteRune(r)
	}
	b.WriteByte('\'')
	return b.String()
}

// WinSafeName 在 SafeName 之后按 Windows 的规则再清理：<>:"/\|?* 换成 _，去掉结尾的点和空格，
// 设备名（CON、NUL、COM1 等，带扩展名也算）前面加 _。
func WinSafeName(name string) string {
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || strings.ContainsRune(`<>:"/\|?*`, r) {
			return '_'
		}
		return r
	}, name)
	name = strings.TrimRight(name, ". ")
	if name == "" {
		return "download"
	}
	stem := strings.ToUpper(strings.TrimRight(strings.SplitN(name, ".", 2)[0], " "))
	switch stem {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return "_" + name
	}
	if len(stem) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) && stem[3] >= '0' && stem[3] <= '9' {
		return "_" + name
	}
	if strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT") {
		switch stem[3:] {
		case "¹", "²", "³":
			return "_" + name
		}
	}
	return name
}
