package api

import (
	"bytes"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"text/template"
	"time"
)

// installScript 是节点安装脚本，由面板填入自己的地址和公钥指纹后提供下载。
// 下载二进制时同样用 --pinnedpubkey 校验面板身份。令牌只用于加入，不会写进服务配置。
var installScript = template.Must(template.New("install").Parse(`#!/bin/sh
# Nagi 节点安装脚本，由面板生成。
set -eu

PANEL="{{.Panel}}"
FINGERPRINT="{{.Fingerprint}}"
TOKEN=""

fail() { echo "错误：$*" >&2; exit 1; }

while [ $# -gt 0 ]; do
  case "$1" in
    --token) TOKEN="${2:-}"; shift 2 ;;
    *) fail "未知参数 $1" ;;
  esac
done
[ -n "$TOKEN" ] || fail "缺少 --token"

[ "$(uname -s)" = "Linux" ] || fail "Nagi 节点只支持 Linux"
[ "$(id -u)" = "0" ] || fail "请用 root 运行"
case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) fail "不支持的 CPU 架构 $(uname -m)" ;;
esac

command -v curl >/dev/null 2>&1 || fail "需要 curl"
command -v docker >/dev/null 2>&1 || fail "没有找到 Docker。请先安装 Docker：https://docs.docker.com/engine/install/"
CV=$(docker compose version --short 2>/dev/null | sed 's/^v//') || fail "没有找到 docker compose 插件"
[ -n "$CV" ] || fail "没有找到 docker compose 插件"
MAJOR=${CV%%.*}; REST=${CV#*.}; MINOR=${REST%%.*}
if [ "$MAJOR" -lt 2 ] || { [ "$MAJOR" -eq 2 ] && [ "$MINOR" -lt 24 ]; }; then
  fail "docker compose 版本 $CV 太旧，需要 2.24 或更新"
fi

echo "从面板下载 nagi（linux/$ARCH）……"
TMP=$(mktemp)
trap 'rm -f "$TMP"' EXIT
curl -fsS --pinnedpubkey "$FINGERPRINT" -k -o "$TMP" "https://$PANEL/download/nagi-linux-$ARCH" \
  || fail "下载失败。如果面板和节点的 CPU 架构不同，请把对应架构的 nagi 放到面板数据目录的 binaries/ 下"
install -m 0755 "$TMP" /usr/local/bin/nagi

/usr/local/bin/nagi enroll --server "$PANEL" --fingerprint "$FINGERPRINT" --token "$TOKEN"

if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
  cat > /etc/systemd/system/nagi-agent.service <<'UNIT'
[Unit]
Description=Nagi agent
After=network-online.target docker.service
Wants=network-online.target docker.service

[Service]
ExecStart=/usr/local/bin/nagi agent --data-dir /var/lib/nagi
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
UNIT
  systemctl daemon-reload
  systemctl enable --now nagi-agent
  echo "节点已加入并启动。查看日志：journalctl -u nagi-agent -f"
else
  echo "节点已加入。没有检测到 systemd，请自行以服务方式运行：/usr/local/bin/nagi agent --data-dir /var/lib/nagi"
fi
`))

// InstallHandler 提供 GET /install.sh。
func (a *API) InstallHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 只在有可用的加入令牌（刚生成了安装命令或接入码）时提供。安装脚本里有面板地址和指纹，
		// 一直公开的话，开了安全入口也能被扫描器认出这是 Nagi 面板。
		if n, err := a.Q.CountActiveEnrollTokens(r.Context(), time.Now().Unix()); err != nil || n == 0 {
			http.NotFound(w, r)
			return
		}
		var buf bytes.Buffer
		if err := installScript.Execute(&buf, map[string]string{
			"Panel":       a.Panel.PublicHostPort(r.Context()),
			"Fingerprint": a.Panel.Fingerprint,
		}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
		w.Write(buf.Bytes())
	})
}

// DownloadHandler 提供 GET /download/nagi-linux-<架构>。优先用面板数据目录 binaries/ 下的文件；
// 没有时，如果面板自己就是这个架构的 Linux 版本，直接提供面板自身的二进制。
func (a *API) DownloadHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/download/")
		arch, ok := strings.CutPrefix(name, "nagi-linux-")
		if !ok || (arch != "amd64" && arch != "arm64") {
			http.NotFound(w, r)
			return
		}
		path, err := a.binaryPath(arch)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeFile(w, r, path)
	})
}

// binaryPath 返回面板提供给节点的 linux/<arch> 的 nagi：优先用数据目录 binaries/ 下的文件，
// 没有时，如果面板自己就是这个架构的 Linux 版本，用面板自身。
func (a *API) binaryPath(arch string) (string, error) {
	name := "nagi-linux-" + arch
	path := filepath.Join(a.Panel.DataDir, "binaries", name)
	if _, err := os.Stat(path); err == nil {
		return path, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if runtime.GOOS != "linux" || runtime.GOARCH != arch {
		return "", errors.New("面板没有 linux/" + arch + " 的 nagi，请把它放到面板数据目录的 binaries/" + name)
	}
	return os.Executable()
}
