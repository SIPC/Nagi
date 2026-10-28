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

	"github.com/SIPC/nagi/internal/pkg/i18n"
)

// installScript 是节点安装脚本，由面板填入自己的地址和公钥指纹后提供下载。
// 下载二进制时同样用 --pinnedpubkey 校验面板身份。令牌只用于加入，不会写进服务配置。
// 没有 Docker 时自动安装，并把 nagi 注册为命令（见 sharedScript）。
var installScript = template.Must(template.New("install").Parse(`#!/bin/sh
# Nagi 节点安装脚本，由面板生成。
set -eu

PANEL="{{.Panel}}"
FINGERPRINT="{{.Fingerprint}}"
BIN=/usr/local/bin/nagi
TOKEN=""
DOCKER_MIRROR=""

say() { printf '%s\n' "$*"; }
fail() {
  printf '错误：%s\n' "$*" >&2
  exit 1
}

while [ $# -gt 0 ]; do
  case "$1" in
    --token) TOKEN="${2:-}"; shift 2 ;;
    --docker-mirror) DOCKER_MIRROR="${2:-}"; shift 2 ;;
    *) fail "未知参数 $1" ;;
  esac
done
[ -n "$TOKEN" ] || fail "缺少 --token"

{{.Shared}}

[ "$(uname -s)" = "Linux" ] || fail "Nagi 节点仅支持 Linux"
[ "$(id -u)" = "0" ] || fail "请使用 root 运行"
case "$(uname -m)" in
  x86_64 | amd64) ARCH=amd64 ;;
  aarch64 | arm64) ARCH=arm64 ;;
  *) fail "不支持的 CPU 架构 $(uname -m)" ;;
esac
command -v curl >/dev/null 2>&1 || fail "需要 curl"

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

ensure_docker

say "从面板下载 nagi（linux/$ARCH）……"
curl -fsS --pinnedpubkey "$FINGERPRINT" -k -o "$TMP/nagi" "https://$PANEL/download/nagi-linux-$ARCH" \
  || fail "下载失败。如果面板和节点的 CPU 架构不同，请把对应架构的 nagi 放到面板数据目录的 binaries/ 下"
install -m 0755 "$TMP/nagi" "$BIN"
link_command

"$BIN" enroll --server "$PANEL" --fingerprint "$FINGERPRINT" --token "$TOKEN"

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
  say "节点已加入并启动。查看日志：journalctl -u nagi-agent -f"
else
  say "节点已加入。未检测到 systemd，请以服务方式自行运行：nagi agent --data-dir /var/lib/nagi"
fi
say "已注册 nagi 命令，例如 nagi panel list 查看信任的面板（更多命令：nagi --help）。"
`))

// sharedScript 是节点安装脚本和面板安装脚本（scripts/install.sh）共用的函数：自动安装 Docker 和 compose 插件、
// 把 nagi 注册为命令。面板安装脚本在 GitHub 上单独提供，只能复制一份；TestSharedScript 检查两边一致。
const sharedScript = `# >>> 共用代码开始：scripts/install.sh 与 internal/server/api/install.go 中的这一段须保持一致（有测试检查）。
# 用到调用方定义的 say、fail、BIN、ARCH、TMP（临时目录）、DOCKER_MIRROR。

# compose_version 打印 docker compose 插件的版本号（不带 v），没有插件时失败。
compose_version() {
  docker compose version --short 2>/dev/null | sed 's/^v//'
}

# compose_ok：compose 插件存在且不低于 2.24。
compose_ok() {
  CV=$(compose_version) || return 1
  case "$CV" in [0-9]*.[0-9]*) ;; *) return 1 ;; esac
  MAJOR=${CV%%.*}
  REST=${CV#*.}
  MINOR=${REST%%[!0-9]*}
  [ "$MAJOR" -gt 2 ] || { [ "$MAJOR" -eq 2 ] && [ "$MINOR" -ge 24 ]; }
}

# install_compose 安装或升级 compose 插件：先用系统的包管理器（Docker 官方软件源里有 docker-compose-plugin），
# 不行再从 GitHub 下载到 /usr/local/lib/docker/cli-plugins（优先于系统目录里的旧版本）。
install_compose() {
  if command -v apt-get >/dev/null 2>&1; then
    apt-get install -y -q docker-compose-plugin >/dev/null 2>&1 && compose_ok && return 0
  elif command -v dnf >/dev/null 2>&1; then
    dnf install -y -q docker-compose-plugin >/dev/null 2>&1 && compose_ok && return 0
  elif command -v yum >/dev/null 2>&1; then
    yum install -y -q docker-compose-plugin >/dev/null 2>&1 && compose_ok && return 0
  fi
  case "$ARCH" in amd64) CARCH=x86_64 ;; *) CARCH=aarch64 ;; esac
  mkdir -p /usr/local/lib/docker/cli-plugins
  curl -fL --retry 3 --progress-bar -o "$TMP/docker-compose" \
    "https://github.com/docker/compose/releases/latest/download/docker-compose-linux-$CARCH" &&
    install -m 0755 "$TMP/docker-compose" /usr/local/lib/docker/cli-plugins/docker-compose
}

# ensure_docker 确保 Docker 和 compose 插件（2.24 或更新）可用：没有 Docker 时用官方脚本 get.docker.com 安装
# （DOCKER_MIRROR 非空时作为它的 --mirror 参数），并启动 Docker；compose 插件缺失或过旧时单独安装。
ensure_docker() {
  if ! command -v docker >/dev/null 2>&1; then
    say "未找到 Docker，使用官方脚本 get.docker.com 安装……"
    curl -fsSL --retry 3 -o "$TMP/get-docker.sh" https://get.docker.com ||
      fail "下载 Docker 安装脚本失败，请先手动安装 Docker：https://docs.docker.com/engine/install/"
    if [ -n "$DOCKER_MIRROR" ]; then
      sh "$TMP/get-docker.sh" --mirror "$DOCKER_MIRROR"
    else
      sh "$TMP/get-docker.sh"
    fi || fail "安装 Docker 失败，请先手动安装 Docker：https://docs.docker.com/engine/install/"
  fi
  if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
    systemctl enable --now docker >/dev/null 2>&1 || true
  fi
  docker info >/dev/null 2>&1 || fail "Docker 未运行，请检查后重试：systemctl status docker"
  if ! compose_ok; then
    say "安装 docker compose 插件……"
    install_compose || true
    compose_ok || fail "docker compose 插件缺失或版本低于 2.24，请手动安装：https://docs.docker.com/compose/install/linux/"
  fi
  say "Docker：$(docker --version | sed 's/^Docker version //; s/,.*//')，compose：$(compose_version)"
}

# link_command 把 nagi 注册为命令：部分环境的 PATH 里没有 /usr/local/bin（例如 RHEL 系 sudo 的 secure_path），
# 在 /usr/bin 下建一个链接（已有同名的文件时不动）；并安装 bash 命令补全。
link_command() {
  if [ ! -e /usr/bin/nagi ] || [ -L /usr/bin/nagi ]; then
    ln -sf "$BIN" /usr/bin/nagi
  fi
  if [ -d /usr/share/bash-completion/completions ]; then
    "$BIN" completion bash >/usr/share/bash-completion/completions/nagi 2>/dev/null || true
  fi
}

# <<< 共用代码结束`

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
			"Shared":      sharedScript,
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
			http.Error(w, i18n.Localize(requestLang(r), err), http.StatusNotFound)
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
		return "", i18n.Errorf("面板没有 linux/%s 的 nagi，请把它放到面板数据目录的 binaries/%s", arch, name)
	}
	return os.Executable()
}
