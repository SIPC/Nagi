package directdl

import (
	"encoding/base64"
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
// 也可以由面板作为 .ps1 提供（见 ShortWindows），所以脚本只用 ASCII：Windows PowerShell 5.1 按控制台的代码页
// 解码 curl.exe 的输出，非 ASCII 的文件名用 Base64 写（psString）。
// 保存到下载文件夹（$env:NAGI_DOWNLOAD_DIR 可以改），不用当前目录：在 C:\Windows\System32 这样不可写的目录里
// 运行时 curl.exe 写不进去（exit 23），换个目录运行也接不上之前下载的部分。

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

// winPrelude 选保存的目录并确认能写：$env:NAGI_DOWNLOAD_DIR，没有时用当前用户的下载文件夹（注册表里的位置，
// 被移到别处时也对），再没有时用 <用户目录>\Downloads。相对路径按 PowerShell 的当前目录算。
const winPrelude = `$dir = $env:NAGI_DOWNLOAD_DIR
if (-not $dir) { try { $dir = [Environment]::ExpandEnvironmentVariables((Get-ItemProperty -LiteralPath 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Explorer\User Shell Folders').'{374DE290-123F-4565-9164-39C4925E467B}') } catch { } }
if (-not $dir) { $dir = [IO.Path]::Combine([Environment]::GetFolderPath('UserProfile'), 'Downloads') }
$dir = $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($dir)
try {
  $null = [IO.Directory]::CreateDirectory($dir)
  $t = [IO.Path]::Combine($dir, '.nagi-write-test.' + [Guid]::NewGuid().ToString('N'))
  $probe = [IO.File]::Open($t, [IO.FileMode]::CreateNew, [IO.FileAccess]::Write, [IO.FileShare]::None)
  try { } finally { $probe.Dispose(); [IO.File]::Delete($t) }
}
catch { Write-Host ('FAILED: cannot write to ' + $dir + ' (folder not writable). Set $env:NAGI_DOWNLOAD_DIR to a writable folder and run the command again.'); return }
$o = [IO.Path]::Combine($dir, $out)
Write-Host ('Saving to ' + $o)
`

// winWhy 把 curl.exe 的退出码（和 --stderr 的日志）说成原因。节点对无效的链接一律回 404（见 docs/agent.md）。
const winWhy = `function NagiWhy([int]$code, [string]$log) {
  $msg = ''; if ($log -and [IO.File]::Exists($log)) { $msg = [IO.File]::ReadAllText($log).Trim() }
  $http = ''; if ($msg -match 'error: (\d{3})') { $http = $Matches[1] }
  if ($code -eq 22 -and $http -eq '409') { return 'the file changed on the node: generate a new command in the panel' }
  if ($code -eq 22) { return 'the node rejected the link (expired, used from another network address, or used up): generate a new command in the panel' }
  if ($code -eq 23) { return 'cannot write the file (folder not writable or disk full)' }
  if ($code -in 5, 6, 7) { return 'cannot connect to the node (port blocked or node offline)' }
  if ($code -in 18, 28, 52, 55, 56) { return 'the connection was interrupted: run the command again to resume' }
  $r = 'curl.exe exited with ' + $code; if ($msg) { $r += ': ' + ($msg -split "` + "`" + `n")[-1].Trim() }; return $r
}
function NagiLen([string]$p) { try { $s = [IO.File]::Open($p, 'Open', 'Read', 'ReadWrite, Delete'); try { $s.Length } finally { $s.Dispose() } } catch { 0 } }
`

// Windows 生成 PowerShell 的命令。普通文件分成 $N 段（不到 MultiThreshold 的文件 1 段），各段用 Start-Process 同时运行
// curl.exe（-s，错误写进各自的 .log，输出不会混在一起），每段先下到「<段>.tmp」，结束后接到「文件名.<标识>.<N>.part<i>」
// 后面，再从收到的地方接着下载，最多 segmentRetries 轮；同一条命令可以重复运行续传。目录打包用一个连接、不续传。
func (c Command) Windows() string {
	name := WinSafeName(SafeName(c.Name))
	base := c.partBase(name)
	var b strings.Builder
	w := func(line string) { b.WriteString(line + "\n") }
	w("# PowerShell (Windows 10/11). cmd.exe is not supported.")
	w("& {")
	w("$ErrorActionPreference = 'Stop'")
	if c.Segmented() {
		w("$N = " + strconv.Itoa(Threads))
	} else if c.Resume {
		w("$N = 1")
	}
	w("$url = " + psQuote(c.URL))
	w("$out = " + psString(name))
	w("$base = " + psString(base))
	w("[long]$size = " + strconv.FormatInt(c.Size, 10))
	b.WriteString(winPrelude)
	w("if (-not ('" + ctrType + "' -as [type])) {")
	w("Add-Type -TypeDefinition @'")
	b.WriteString(ctrSource)
	w("'@")
	w("}")
	b.WriteString(winWhy)
	flag := ipFlag(c.URL)
	args := "'-fgS'"
	if flag != "" {
		args = "'" + flag + "', " + args
	}
	again := "Run the same command again within " + ttlText() + " to resume; after that, generate a new command in the panel."
	if c.Resume {
		w("$seg = [long][math]::Max(1, [math]::Floor(($size + $N - 1) / $N))")
		w("$count = [int][math]::Ceiling($size / $seg)")
		w("$parts = @(for ($i = 0; $i -lt $count; $i++) { [IO.Path]::Combine($dir, $base + '.' + $N + '.part' + $i) })")
		w("$have = 0; foreach ($f in $parts) { [" + ctrType + "]::Append($f + '.tmp', $f); $have += [" + ctrType + "]::Length($f) }")
		w("$free = -1; try { $free = (New-Object IO.DriveInfo ([IO.Path]::GetPathRoot($o))).AvailableFreeSpace } catch { }")
		w("if ($free -ge 0 -and $free -lt 2 * $size - $have) { Write-Host ('FAILED: not enough disk space for ' + $o + ': ' + [math]::Ceiling((2 * $size - $have) / 1MB) + ' MiB needed, ' + [math]::Floor($free / 1MB) + ' MiB free'); return }")
		w("$errs = @()")
		w("for ($try = 0; $try -lt " + strconv.Itoa(segmentRetries) + "; $try++) {")
		w("  $run = @(); $errs = @(); $fatal = $false")
		w("  for ($i = 0; $i -lt $count; $i++) {")
		w("    $f = $parts[$i]; [" + ctrType + "]::Append($f + '.tmp', $f)")
		w("    $a = $i * $seg; $e = [math]::Min($a + $seg, $size) - 1; $from = $a + [" + ctrType + "]::Length($f)")
		w("    if ($from -gt $e) { continue }")
		w("    $p = Start-Process -FilePath curl.exe -ArgumentList @(" + strings.Replace(args, "-fgS", "-fgsS", 1) +
			", '-r', ('{0}-{1}' -f $from, $e), '-o', ('\"' + $f + '.tmp\"'), '--stderr', ('\"' + $f + '.log\"'), $url) -NoNewWindow -PassThru")
		w("    $null = $p.Handle; $run += @{ p = $p; i = $i }")
		w("  }")
		w("  if ($run.Count -eq 0) { break }")
		w("  while (@($run | Where-Object { -not $_.p.HasExited }).Count -gt 0) {")
		w("    $got = 0; foreach ($f in $parts) { $got += (NagiLen $f) + (NagiLen ($f + '.tmp')) }")
		w("    Write-Host -NoNewline (\"`r{0:N1} / {1:N1} MiB \" -f ($got / 1MB), ($size / 1MB)); Start-Sleep -Milliseconds 500")
		w("  }")
		w("  Write-Host ''")
		w("  foreach ($r in $run) {")
		w("    $r.p.WaitForExit(); $code = $r.p.ExitCode")
		w("    if ($code -ne 0) { $errs += NagiWhy $code ($parts[$r.i] + '.log'); if ($code -in 22, 23) { $fatal = $true } }")
		w("  }")
		w("  if ($fatal) { break }")
		w("  if ($try -lt " + strconv.Itoa(segmentRetries-1) + ") { Start-Sleep -Seconds 1 }")
		w("}")
		w("$total = 0")
		w("foreach ($f in $parts) { [" + ctrType + "]::Append($f + '.tmp', $f); $total += [" + ctrType + "]::Length($f) }")
		w("if ($total -ne $size) {")
		w("  foreach ($m in @($errs | Select-Object -Unique)) { Write-Host ('FAILED: ' + $m) }")
		w("  Write-Host ('FAILED: download incomplete (' + $total + ' of ' + $size + ' bytes). " + again + "'); return")
		w("}")
	} else {
		w("$parts = @([IO.Path]::Combine($dir, $base + '.part'))")
		w("& curl.exe " + strings.ReplaceAll(args, ", ", " ") + " -o $parts[0] $url")
		w("if ($LASTEXITCODE -ne 0) { Write-Host ('FAILED: ' + (NagiWhy $LASTEXITCODE '')); return }")
	}
	w("$aes = [Security.Cryptography.Aes]::Create()")
	w("$aes.Mode = 'ECB'; $aes.Padding = 'None'; $aes.Key = [" + ctrType + "]::Hex('" + hex.EncodeToString(c.Key.Key[:]) + "')")
	w("try { [" + ctrType + "]::Decrypt([string[]]$parts, $o, $aes.CreateEncryptor(), '" + hex.EncodeToString(c.Key.IV[:]) + "') }")
	w("catch { Write-Host ('FAILED: cannot write ' + $o + ': ' + $_.Exception.GetBaseException().Message); return }")
	w("foreach ($f in $parts) { Remove-Item -LiteralPath $f; if ([IO.File]::Exists($f + '.log')) { [IO.File]::Delete($f + '.log') } }")
	if c.Resume {
		w("if ([" + ctrType + "]::Length($o) -ne $size) { Write-Host 'FAILED: the decrypted file has the wrong size'; return }")
	}
	if ValidSHA256(c.SHA256) {
		w("if ((Get-FileHash -LiteralPath $o -Algorithm SHA256).Hash -eq " + psQuote(c.SHA256) + ") { 'SHA-256 OK' } else { Write-Host 'FAILED: SHA-256 mismatch'; return }")
	} else {
		w("'Done'")
	}
	w("Write-Host ('Saved to ' + $o)")
	b.WriteString("}")
	return b.String()
}

// ttlText 是链接的有效期，写进提示（「5 minutes」）。
func ttlText() string { return strconv.Itoa(int(TTL.Minutes())) + " minutes" }

// psString 写一个 PowerShell 的字符串：可打印的 ASCII 用单引号括起来（psQuote），否则写成 UTF-8 的 Base64 再解码，
// 脚本整体保持 ASCII（见文件开头）。
func psString(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return "[Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('" + base64.StdEncoding.EncodeToString([]byte(s)) + "'))"
		}
	}
	return psQuote(s)
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
