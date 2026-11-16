package tlsutil

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// TLS 指纹：面板判断会话是不是真的浏览器在用（见 docs/panel.md#会话），节点的 Caddy 也按同样的规则算好转给面板。

// isGREASE 判断是不是 GREASE 值（RFC 8701），算指纹时去掉。
func isGREASE(v uint16) bool { return v&0x0f0f == 0x0a0a && v>>8 == v&0xff }

// JA4 按 JA4 的规则算 ClientHello 的指纹（TCP 上的 TLS，前缀 t）。
func JA4(h *tls.ClientHelloInfo) string {
	version := "00"
	var best uint16
	for _, v := range h.SupportedVersions {
		if !isGREASE(v) && v > best {
			best = v
		}
	}
	switch best {
	case tls.VersionTLS13:
		version = "13"
	case tls.VersionTLS12:
		version = "12"
	case tls.VersionTLS11:
		version = "11"
	case tls.VersionTLS10:
		version = "10"
	}
	sni := "i"
	if h.ServerName != "" {
		sni = "d"
	}
	var ciphers []string
	for _, c := range h.CipherSuites {
		if !isGREASE(c) {
			ciphers = append(ciphers, fmt.Sprintf("%04x", c))
		}
	}
	var exts []string
	extCount := 0
	for _, e := range h.Extensions {
		if isGREASE(e) {
			continue
		}
		extCount++
		// SNI 和 ALPN 只计数，不进哈希。
		if e == 0x0000 || e == 0x0010 {
			continue
		}
		exts = append(exts, fmt.Sprintf("%04x", e))
	}
	alpn := "00"
	if len(h.SupportedProtos) > 0 && h.SupportedProtos[0] != "" {
		p := h.SupportedProtos[0]
		alpn = string(p[0]) + string(p[len(p)-1])
	}
	var sigs []string
	for _, s := range h.SignatureSchemes {
		if !isGREASE(uint16(s)) {
			sigs = append(sigs, fmt.Sprintf("%04x", uint16(s)))
		}
	}
	sort.Strings(ciphers)
	sort.Strings(exts)
	a := fmt.Sprintf("t%s%s%02d%02d%s", version, sni, min(len(ciphers), 99), min(extCount, 99), alpn)
	c := strings.Join(exts, ",")
	if len(sigs) > 0 {
		c += "_" + strings.Join(sigs, ",")
	}
	return a + "_" + trunc12(strings.Join(ciphers, ",")) + "_" + trunc12(c)
}

func trunc12(s string) string {
	if s == "" {
		return "000000000000"
	}
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:12]
}
