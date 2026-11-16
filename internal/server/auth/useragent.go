package auth

import "strings"

// Agent 是从 User-Agent 里认出的浏览器和系统。只比较类型和系统，不比较版本号：浏览器自动更新不受影响。
type Agent struct {
	Browser string // Chrome、Edge、Firefox、Safari、Opera、空（不是浏览器）
	OS      string // Windows、macOS、iOS、Android、Linux、ChromeOS
}

// IsBrowser 判断标识说自己是浏览器。
func (a Agent) IsBrowser() bool { return a.Browser != "" }

// String 是给人看的「浏览器 · 系统」。
func (a Agent) String() string {
	switch {
	case a.Browser != "" && a.OS != "":
		return a.Browser + " · " + a.OS
	case a.Browser != "":
		return a.Browser
	}
	return a.OS
}

// ParseAgent 从 User-Agent 认出浏览器类型和操作系统。
func ParseAgent(ua string) Agent {
	var a Agent
	if strings.HasPrefix(ua, "Mozilla/5.0") {
		switch {
		case strings.Contains(ua, "Edg/") || strings.Contains(ua, "EdgA/") || strings.Contains(ua, "EdgiOS/"):
			a.Browser = "Edge"
		case strings.Contains(ua, "OPR/"):
			a.Browser = "Opera"
		case strings.Contains(ua, "Firefox/") || strings.Contains(ua, "FxiOS/"):
			a.Browser = "Firefox"
		case strings.Contains(ua, "Chrome/") || strings.Contains(ua, "CriOS/") || strings.Contains(ua, "Chromium/"):
			a.Browser = "Chrome"
		case strings.Contains(ua, "Safari/") && strings.Contains(ua, "Version/"):
			a.Browser = "Safari"
		}
	}
	switch {
	case strings.Contains(ua, "Android"):
		a.OS = "Android"
	case strings.Contains(ua, "iPhone") || strings.Contains(ua, "iPad") || strings.Contains(ua, "iPod"):
		a.OS = "iOS"
	case strings.Contains(ua, "Windows"):
		a.OS = "Windows"
	case strings.Contains(ua, "CrOS"):
		a.OS = "ChromeOS"
	case strings.Contains(ua, "Macintosh") || strings.Contains(ua, "Mac OS X"):
		a.OS = "macOS"
	case strings.Contains(ua, "Linux"):
		a.OS = "Linux"
	}
	return a
}
