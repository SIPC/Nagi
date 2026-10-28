// Package release 是 nagi 发布文件的下载地址（GitHub Release，见 .github/workflows/release.yml）。
// 节点安装和一键升级都从这里下载 nagi，面板不提供二进制。
package release

import (
	"regexp"
	"strings"
)

// Repo 是发布 nagi 的 GitHub 仓库。
const Repo = "SIPC/Nagi"

var versionRe = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?$`)

// IsVersion 判断是不是正式发布的版本号（例如 v0.1.0）。本地编译的版本是 dev，没有对应的发布文件。
func IsVersion(v string) bool { return versionRe.MatchString(v) }

// DownloadBase 返回某个版本的发布文件所在的地址，其下有 nagi-linux-<架构> 和 SHA256SUMS。
// 不是正式版本时用最新发布的版本。
func DownloadBase(version string) string {
	if IsVersion(version) {
		return "https://github.com/" + Repo + "/releases/download/" + version
	}
	return "https://github.com/" + Repo + "/releases/latest/download"
}

// Checksum 从 SHA256SUMS 的内容里找出文件的 SHA-256（十六进制），没有时返回空。
func Checksum(sums, name string) string {
	for _, line := range strings.Split(sums, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && (f[1] == name || f[1] == "*"+name) {
			return strings.ToLower(f[0])
		}
	}
	return ""
}
