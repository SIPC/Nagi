#!/bin/sh
# Nagi 面板一键安装脚本。
#
#   curl -fsSL https://github.com/SIPC/Nagi/releases/latest/download/install.sh | sh
#   curl -fsSL https://github.com/SIPC/Nagi/releases/latest/download/install.sh | sh -s -- --port 28443
#
# 下载 nagi、校验 SHA-256、安装到 /usr/local/bin/nagi，并以 systemd 服务 nagi-server 运行面板
# （默认带一个内嵌节点）。首次安装后打印初始化链接。已经安装过时再执行一次即为升级。
# 节点不用这个脚本：在面板的节点页复制安装命令。
set -eu

REPO="SIPC/Nagi"
BIN=/usr/local/bin/nagi
SERVICE=nagi-server
UNIT=/etc/systemd/system/$SERVICE.service

VERSION=latest
DATA_DIR=""
PORT=""
NO_AGENT=""
STANDBY=""
DOWNLOAD_URL="${NAGI_DOWNLOAD_URL:-}"

usage() {
  cat <<'EOF'
用法：install.sh [选项]

  --version <版本>        安装指定版本，例如 v0.1.0（默认最新版本）
  --port <端口>           面板端口（默认首次启动时随机选择）
  --no-agent              只运行面板，不带内嵌节点（面板单独放在一台不运行项目的机器上）
  --standby <配对码>      作为备用面板安装（配对码在主面板「设置 → 备用面板」中生成）
  --data-dir <目录>       数据目录（默认 /var/lib/nagi）
  --download-url <地址>   从该地址下载 nagi-linux-<架构> 和 SHA256SUMS（镜像或内网），
                          也可用环境变量 NAGI_DOWNLOAD_URL 指定
  -h, --help              显示帮助

已安装时再次执行即为升级：只更换 nagi，服务配置（端口、数据目录、是否带内嵌节点）保持不变。
EOF
}

say() { printf '%s\n' "$*"; }
fail() {
  printf '错误：%s\n' "$*" >&2
  exit 1
}
need_value() { [ $# -ge 2 ] && [ -n "$2" ] || fail "$1 需要一个值"; }

while [ $# -gt 0 ]; do
  case "$1" in
    --version) need_value "$@"; VERSION="$2"; shift 2 ;;
    --port) need_value "$@"; PORT="$2"; shift 2 ;;
    --no-agent) NO_AGENT=1; shift ;;
    --standby) need_value "$@"; STANDBY="$2"; shift 2 ;;
    --data-dir) need_value "$@"; DATA_DIR="$2"; shift 2 ;;
    --download-url) need_value "$@"; DOWNLOAD_URL="$2"; shift 2 ;;
    -h | --help) usage; exit 0 ;;
    *) fail "未知参数 $1（--help 查看用法）" ;;
  esac
done

if [ -n "$PORT" ]; then
  case "$PORT" in *[!0-9]*) fail "端口须为数字" ;; esac
  { [ "$PORT" -ge 1 ] && [ "$PORT" -le 65535 ]; } || fail "端口须在 1～65535 之间"
  { [ "$PORT" -ne 80 ] && [ "$PORT" -ne 443 ]; } || fail "80 和 443 端口保留给项目域名，请使用其他面板端口"
fi
case "$DATA_DIR" in "" | /*) ;; *) fail "数据目录须为绝对路径" ;; esac

# ---- 环境检查 ----

[ "$(uname -s)" = "Linux" ] || fail "Nagi 仅支持 Linux"
[ "$(id -u)" = "0" ] || fail "请使用 root 运行（例如 curl ... | sudo sh）"
case "$(uname -m)" in
  x86_64 | amd64) ARCH=amd64 ;;
  aarch64 | arm64) ARCH=arm64 ;;
  *) fail "不支持的 CPU 架构 $(uname -m)，仅支持 amd64 和 arm64" ;;
esac
command -v curl >/dev/null 2>&1 || fail "需要 curl"
if command -v sha256sum >/dev/null 2>&1; then
  SHA="sha256sum"
elif command -v shasum >/dev/null 2>&1; then
  SHA="shasum -a 256"
else
  fail "需要 sha256sum 或 shasum 以校验下载的文件"
fi

HAS_SYSTEMD=""
if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
  HAS_SYSTEMD=1
fi
UPGRADE=""
if [ -n "$HAS_SYSTEMD" ] && [ -f "$UNIT" ]; then
  UPGRADE=1
  # 升级只更换 nagi，沿用原有的服务配置。不在这里修改：已加入的节点按原端口连接面板，
  # 换数据目录等于另起一个面板，去掉或加上内嵌节点会让本机的容器脱离管理或多出一个节点。
  OLD_DATA_DIR=/var/lib/nagi
  OLD_PORT=""
  OLD_NO_AGENT=""
  # shellcheck disable=SC2046
  set -- $(sed -n 's/^ExecStart=//p' "$UNIT")
  while [ $# -gt 0 ]; do
    case "$1" in
      --data-dir) OLD_DATA_DIR="${2:-}"; shift ;;
      --port) OLD_PORT="${2:-}"; shift ;;
      --no-agent) OLD_NO_AGENT=1 ;;
    esac
    shift
  done
  CHANGED=""
  { [ -z "$DATA_DIR" ] || [ "$DATA_DIR" = "$OLD_DATA_DIR" ]; } || CHANGED="--data-dir"
  { [ -z "$PORT" ] || [ "$PORT" = "$OLD_PORT" ]; } || CHANGED="--port"
  { [ -z "$NO_AGENT" ] || [ -n "$OLD_NO_AGENT" ]; } || CHANGED="--no-agent"
  [ -z "$CHANGED" ] || fail "本机已安装面板，升级时不能修改 $CHANGED（现有配置见 $UNIT）。不带该参数重新执行即可升级"
  DATA_DIR=$OLD_DATA_DIR
  PORT=$OLD_PORT
  NO_AGENT=$OLD_NO_AGENT
fi
[ -n "$DATA_DIR" ] || DATA_DIR=/var/lib/nagi

if [ -n "$STANDBY" ]; then
  [ -z "$UPGRADE" ] || fail "本机已安装面板，--standby 仅用于全新安装"
  [ -z "$(ls -A "$DATA_DIR/panel" 2>/dev/null)" ] || fail "$DATA_DIR/panel 不为空。备用面板须使用空的数据目录，可用 --data-dir 指定其他目录"
  NO_AGENT=1 # 备用面板不带内嵌节点（nagi server 对配对来的面板也这样处理），无需 Docker
fi

# 面板自带节点，同一台机器不能再运行一个节点：数据目录和 80/443 端口会冲突。
if [ -z "$UPGRADE" ] && [ -f /etc/systemd/system/nagi-agent.service ]; then
  [ -n "$NO_AGENT" ] || fail "本机已安装 Nagi 节点（nagi-agent 服务）。面板自带节点，同一台机器不能再运行一个节点。仅需面板时请加 --no-agent --data-dir /var/lib/nagi-panel"
  [ "$DATA_DIR" != /var/lib/nagi ] || fail "本机的 Nagi 节点使用 /var/lib/nagi，请用 --data-dir 为面板指定其他目录，例如 /var/lib/nagi-panel"
fi

if [ -z "$NO_AGENT" ]; then
  command -v docker >/dev/null 2>&1 || fail "未找到 Docker。面板自带节点，需要先安装 Docker：https://docs.docker.com/engine/install/ （仅运行面板请加 --no-agent）"
  CV=$(docker compose version --short 2>/dev/null | sed 's/^v//') || CV=""
  [ -n "$CV" ] || fail "未找到 docker compose 插件，请先安装：https://docs.docker.com/compose/install/linux/"
  MAJOR=${CV%%.*}
  REST=${CV#*.}
  MINOR=${REST%%.*}
  if [ "$MAJOR" -lt 2 ] || { [ "$MAJOR" -eq 2 ] && [ "$MINOR" -lt 24 ]; }; then
    fail "docker compose 版本 $CV 过旧，需要 2.24 或更新版本"
  fi
fi

# ---- 下载并校验 ----

if [ -n "$DOWNLOAD_URL" ]; then
  BASE=${DOWNLOAD_URL%/}
elif [ "$VERSION" = latest ]; then
  BASE="https://github.com/$REPO/releases/latest/download"
else
  BASE="https://github.com/$REPO/releases/download/$VERSION"
fi

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

say "下载 nagi（linux/$ARCH）：$BASE"
curl -fL --retry 3 --progress-bar -o "$TMP/nagi" "$BASE/nagi-linux-$ARCH" ||
  fail "下载失败：$BASE/nagi-linux-$ARCH（网络不通时可用 --download-url 指定镜像）"
curl -fsSL --retry 3 -o "$TMP/SHA256SUMS" "$BASE/SHA256SUMS" || fail "下载校验文件失败：$BASE/SHA256SUMS"

WANT=$(awk -v f="nagi-linux-$ARCH" '$2 == f || $2 == "*" f { print $1 }' "$TMP/SHA256SUMS")
[ -n "$WANT" ] || fail "SHA256SUMS 中没有 nagi-linux-$ARCH"
GOT=$($SHA "$TMP/nagi" | awk '{ print $1 }')
[ "$WANT" = "$GOT" ] || fail "SHA-256 校验失败：应为 $WANT，实际为 $GOT"
chmod 0755 "$TMP/nagi"
NEW_VERSION=$("$TMP/nagi" version 2>/dev/null) || fail "下载的 nagi 无法在本机运行"

OLD_VERSION=""
[ -x "$BIN" ] && OLD_VERSION=$("$BIN" version 2>/dev/null || true)
install -m 0755 "$TMP/nagi" "$BIN"
if [ -n "$OLD_VERSION" ] && [ "$OLD_VERSION" != "$NEW_VERSION" ]; then
  say "已安装 nagi $NEW_VERSION（原版本 $OLD_VERSION）：$BIN"
else
  say "已安装 nagi $NEW_VERSION：$BIN"
fi

# ---- 服务 ----

server_args() {
  ARGS="server --data-dir $DATA_DIR"
  [ -z "$PORT" ] || ARGS="$ARGS --port $PORT"
  [ -z "$NO_AGENT" ] || ARGS="$ARGS --no-agent"
  [ -z "${1:-}" ] || ARGS="$ARGS --standby $1"
  printf '%s' "$ARGS"
}

if [ -z "$HAS_SYSTEMD" ]; then
  say ""
  say "未检测到 systemd，请以服务方式自行运行："
  say "  $BIN $(server_args "$STANDBY")"
  exit 0
fi

write_unit() {
  WANTS="network-online.target"
  [ -n "$NO_AGENT" ] || WANTS="$WANTS docker.service"
  cat >"$UNIT" <<EOF
[Unit]
Description=Nagi panel
After=$WANTS
Wants=$WANTS

[Service]
ExecStart=$BIN $(server_args "${1:-}")
Restart=always
RestartSec=5
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
}

SINCE=$(date '+%Y-%m-%d %H:%M:%S')
if [ -z "$UPGRADE" ]; then
  write_unit "$STANDBY"
  systemctl enable "$SERVICE" >/dev/null 2>&1
fi
systemctl restart "$SERVICE"

# 等面板启动，从日志中取出地址、公钥指纹和初始化链接。
say "启动面板……"
LOG=""
i=0
while [ $i -lt 60 ]; do
  LOG=$(journalctl -u "$SERVICE" --since "$SINCE" -o cat --no-pager 2>/dev/null || true)
  case "$LOG" in *"msg=面板已启动"*) break ;; esac
  sleep 1
  i=$((i + 1))
done
case "$LOG" in
  *"msg=面板已启动"*) ;;
  *)
    say "面板在 60 秒内未完成启动，最近的日志："
    journalctl -u "$SERVICE" --since "$SINCE" -n 30 -o cat --no-pager 2>/dev/null || true
    fail "请检查上面的日志，或执行 journalctl -u $SERVICE -f 查看"
    ;;
esac

# 备用面板配对成功后，从服务配置中去掉一次性的配对码（之后的启动不再需要）。
if [ -n "$STANDBY" ] && [ -f "$DATA_DIR/panel/paired" ]; then
  write_unit ""
fi

URL=$(printf '%s\n' "$LOG" | sed -n 's/.*msg=面板已启动 .*url=\([^ ]*\).*/\1/p' | tail -n 1)
FINGERPRINT=$(printf '%s\n' "$LOG" | sed -n 's/.*msg=面板已启动 .*fingerprint="\{0,1\}\([^" ]*\).*/\1/p' | tail -n 1)
SETUP=$(printf '%s\n' "$LOG" | grep -o 'https://[^ "]*/setup?token=[^ "]*' | tail -n 1 || true)
ENTRANCE=$(printf '%s\n' "$LOG" | sed -n 's/.*msg=安全入口已开启.*url=\([^ ]*\).*/\1/p' | tail -n 1)
PANEL_PORT=${URL##*:}

say ""
if [ -n "$UPGRADE" ]; then
  say "面板已升级并重新启动。"
elif [ -n "$STANDBY" ]; then
  say "备用面板已与主面板配对并启动，在主面板「设置 → 备用面板」中可以看到它。"
else
  say "面板已安装并启动。"
fi
say ""
say "  地址：$URL"
[ -z "$ENTRANCE" ] || say "  安全入口：$ENTRANCE"
say "  公钥指纹：$FINGERPRINT"
if [ -n "$SETUP" ]; then
  say ""
  say "  请在浏览器中打开下面的链接设置管理员账号（24 小时内有效）："
  say ""
  say "    $SETUP"
  say ""
  say "  面板使用自签证书，浏览器会提示不安全，确认公钥指纹后继续即可。"
fi
say ""
say "  防火墙需放行面板端口 $PANEL_PORT 的 TCP 和 UDP（UDP 用于节点之间的加密网络）。"
[ -n "$NO_AGENT" ] || say "  项目通过域名对外访问时，还需放行 80 和 443 端口。"
say "  查看日志：journalctl -u $SERVICE -f"
say "  升级：再次执行本脚本；节点在面板的节点页一键升级。"
