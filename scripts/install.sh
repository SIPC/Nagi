#!/bin/sh
# Nagi 面板一键安装脚本。
#
#   curl -fsSL https://github.com/SIPC/Nagi/releases/latest/download/install.sh | sh
#   curl -fsSL https://github.com/SIPC/Nagi/releases/latest/download/install.sh | sh -s -- --port 28443
#   curl -fsSL https://github.com/SIPC/Nagi/releases/latest/download/install.sh | NAGI_STANDBY=<配对码> sh   # 备用面板
#
# 没有 Docker 时自动安装（面板带内嵌节点时需要）；下载 nagi、校验 SHA-256、安装到 /usr/local/bin/nagi
# 并注册为 nagi 命令，以 systemd 服务 nagi-server 运行面板。首次安装后打印初始化链接。
# 已经安装过时再执行一次即为升级。
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
# 配对码用环境变量 NAGI_STANDBY 传，不出现在进程参数里。读完就清掉，不传给 Docker 安装脚本等子进程。
STANDBY="${NAGI_STANDBY:-}"
unset NAGI_STANDBY
# STANDBY_ARG：配对码是用 --standby 参数给的（旧写法，安装期间本机其他用户能从进程参数里读到）。
STANDBY_ARG=""
# STANDBY_WRITTEN：已经写了配对码文件（见 write_standby_env），出错退出时由 on_exit 清掉。
STANDBY_WRITTEN=""
DOWNLOAD_URL="${NAGI_DOWNLOAD_URL:-}"
DOCKER_MIRROR=""

usage() {
  cat <<'EOF'
用法：install.sh [选项]

  --version <版本>        安装指定版本，例如 v0.1.0（默认最新版本）
  --port <端口>           面板端口（默认首次启动时随机选择）
  --no-agent              只运行面板，不带内嵌节点（面板单独放在一台不运行项目的机器上）
  --standby <配对码>      作为备用面板安装（配对码在主面板「设置 → 备用面板」中生成）。
                          建议改用环境变量 NAGI_STANDBY 指定，配对码不出现在进程参数里：
                          curl … | NAGI_STANDBY=<配对码> sh（以 root 执行，不要在 sh 前加 sudo）
  --data-dir <目录>       数据目录（默认 /var/lib/nagi）
  --download-url <地址>   从该地址下载 nagi-linux-<架构> 和 SHA256SUMS（镜像或内网），
                          也可用环境变量 NAGI_DOWNLOAD_URL 指定
  --docker-mirror <名称>  自动安装 Docker 时使用的软件源镜像：Aliyun 或 AzureChinaCloud
                          （检测到中国大陆网络时默认用 Aliyun，并经加速代理下载 GitHub 上的文件）
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
    --standby) need_value "$@"; STANDBY="$2"; STANDBY_ARG=1; shift 2 ;;
    --data-dir) need_value "$@"; DATA_DIR="$2"; shift 2 ;;
    --download-url) need_value "$@"; DOWNLOAD_URL="$2"; shift 2 ;;
    --docker-mirror) need_value "$@"; DOCKER_MIRROR="$2"; shift 2 ;;
    -h | --help) usage; exit 0 ;;
    *) fail "未知参数 $1（--help 查看用法）" ;;
  esac
done

if [ -n "$STANDBY_ARG" ]; then
  printf '%s
' "注意：配对码写在命令参数里，安装期间本机其他用户可以看到。建议改用环境变量：先执行 sudo -i 切换到 root，再执行 curl … | NAGI_STANDBY=<配对码> sh" >&2
fi

if [ -n "$PORT" ]; then
  case "$PORT" in *[!0-9]*) fail "端口须为数字" ;; esac
  { [ "$PORT" -ge 1 ] && [ "$PORT" -le 65535 ]; } || fail "端口须在 1～65535 之间"
  { [ "$PORT" -ne 80 ] && [ "$PORT" -ne 443 ]; } || fail "80 和 443 端口保留给项目域名，请使用其他面板端口"
fi
case "$DATA_DIR" in "" | /*) ;; *) fail "数据目录须为绝对路径" ;; esac

# >>> 共用代码开始：scripts/install.sh 与 internal/server/api/install.go 中的这一段须保持一致（有测试检查）。
# 用到调用方定义的 say、fail、BIN、ARCH、TMP（临时目录）、DOCKER_MIRROR、BASE（下载地址）。

# 中国大陆网络（见 docs/guide/install.md#中国大陆服务器）：detect_cn 检测到以后 CN=1，GitHub 上的文件经加速代理下载，
# Docker 用阿里云的软件源，并配置 Docker Hub 镜像加速。代理和加速地址都是第三方服务，按顺序试，失效了换下一个。
# 两个列表在 Go 里也有一份（release.GitHubProxies：一键升级时换代理用；api.DefaultRegistryMirrors：界面上的默认加速），有测试检查一致。
GH_PROXIES="https://ghfast.top/ https://gh-proxy.com/ https://ghproxy.net/"
REGISTRY_MIRRORS='"https://docker.m.daocloud.io", "https://docker.1ms.run"'
CN=""
# GH_PROXY 是下载 nagi 时实际用上的代理（没用代理时为空），记下来给面板上的一键升级用。
GH_PROXY=""

# detect_cn 判断本机是否在中国大陆网络：按 Cloudflare 看到的所在地；查不到时看能不能连上 GitHub。
detect_cn() {
  LOC=$(curl -fsS --max-time 5 https://www.cloudflare.com/cdn-cgi/trace 2>/dev/null | sed -n 's/^loc=//p' || true)
  if [ -n "$LOC" ]; then
    [ "$LOC" != CN ] || CN=1
  elif ! curl -fsS --max-time 8 -o /dev/null https://github.com 2>/dev/null; then
    CN=1
  fi
  [ -z "$CN" ] || say "检测到中国大陆网络，GitHub 上的文件经加速代理下载，Docker 使用国内镜像"
}

# gh_urls 列出下载 GitHub 上一个文件可以用的地址，每行一个：中国大陆网络时先列加速代理，最后是原地址。
gh_urls() {
  if [ -n "$CN" ]; then
    for P in $GH_PROXIES; do printf '%s%s\n' "$P" "$1"; done
  fi
  printf '%s\n' "$1"
}

# gh_fetch 把 GitHub 上的文件 $1 下载到 $2，按 gh_urls 的顺序试。
gh_fetch() {
  for U in $(gh_urls "$1"); do
    curl -fsSL --retry 2 --connect-timeout 10 --speed-limit 1024 --speed-time 30 -o "$2" "$U" && return 0
  done
  return 1
}

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
  COMPOSE_URL="https://github.com/docker/compose/releases/download/v2.40.3/docker-compose-linux-$CARCH"
  curl -fsSL --retry 2 --connect-timeout 10 --max-time 60 "$COMPOSE_URL.sha256" -o "$TMP/compose.sha256" || return 1
  gh_fetch "$COMPOSE_URL" "$TMP/docker-compose" || return 1
  if command -v sha256sum >/dev/null 2>&1; then CSHA=sha256sum; else CSHA="shasum -a 256"; fi
  CWANT=$(awk '{print $1}' "$TMP/compose.sha256")
  CGOT=$($CSHA "$TMP/docker-compose" | awk '{print $1}') || return 1
  [ -n "$CWANT" ] && [ "$CWANT" = "$CGOT" ] || return 1
  install -m 0755 "$TMP/docker-compose" /usr/local/lib/docker/cli-plugins/docker-compose
}

# registry_mirrors 在中国大陆网络时给 Docker 配置 Docker Hub 镜像加速，部署时拉取镜像不会卡住。
# 只在还没有 /etc/docker/daemon.json 时写，已有的配置不动；写完让 Docker 重新加载配置，不会重启容器。
registry_mirrors() {
  [ -n "$CN" ] && [ ! -e /etc/docker/daemon.json ] || return 0
  mkdir -p /etc/docker
  printf '{\n  "registry-mirrors": [%s]\n}\n' "$REGISTRY_MIRRORS" >/etc/docker/daemon.json
  say "已配置 Docker Hub 镜像加速：/etc/docker/daemon.json"
  MIRRORS_ADDED=1
}

# ensure_docker 确保 Docker 和 compose 插件（2.24 或更新）可用：没有 Docker 时用官方脚本 get.docker.com 安装
# （DOCKER_MIRROR 非空时作为它的 --mirror 参数；中国大陆网络时默认用 Aliyun），并启动 Docker；compose 插件缺失或过旧时单独安装。
ensure_docker() {
  if ! command -v docker >/dev/null 2>&1; then
    say "未找到 Docker，使用官方脚本 get.docker.com 安装……"
    [ -z "$CN" ] || [ -n "$DOCKER_MIRROR" ] || DOCKER_MIRROR=Aliyun
    curl -fsSL --retry 3 --max-time 60 -o "$TMP/get-docker.sh" https://get.docker.com ||
      fail "下载 Docker 安装脚本失败，请先手动安装 Docker：https://docs.docker.com/engine/install/"
    if [ -n "$DOCKER_MIRROR" ]; then
      sh "$TMP/get-docker.sh" --mirror "$DOCKER_MIRROR"
    else
      sh "$TMP/get-docker.sh"
    fi || fail "安装 Docker 失败，请先手动安装 Docker：https://docs.docker.com/engine/install/"
  fi
  MIRRORS_ADDED=""
  registry_mirrors
  if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
    systemctl enable --now docker >/dev/null 2>&1 || true
    [ -z "$MIRRORS_ADDED" ] || systemctl reload docker >/dev/null 2>&1 || true
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

# fetch_nagi 从 $1 下载二进制，但只信任 BASE 的校验值（自动加速代理不能提供信任依据）。
fetch_nagi() {
  curl -fL --retry 2 --connect-timeout 10 --speed-limit 1024 --speed-time 30 --progress-bar -o "$TMP/nagi" "$1/nagi-linux-$ARCH" || return 1
  curl -fsSL --retry 2 --connect-timeout 10 --max-time 60 -o "$TMP/SHA256SUMS" "$BASE/SHA256SUMS" || { say "无法从可信来源取得校验值：$BASE/SHA256SUMS"; return 1; }
  WANT=$(awk -v f="nagi-linux-$ARCH" '$2 == f || $2 == "*" f { print $1 }' "$TMP/SHA256SUMS")
  [ -n "$WANT" ] || { say "SHA256SUMS 中没有 nagi-linux-$ARCH：$1"; return 1; }
  GOT=$($SHA "$TMP/nagi" | awk '{ print $1 }')
  [ "$WANT" = "$GOT" ] || { say "SHA-256 校验失败：应为 $WANT，实际为 $GOT（$1）"; return 1; }
}

# download_nagi 下载 nagi 到 $TMP/nagi，校验值只从 BASE 取得，不经自动代理，
# 试运行确认能在本机运行，版本号放在 NEW_VERSION。BASE 是 GitHub Release 或 --download-url 指定的镜像；
# 是 GitHub 时按 gh_urls 的顺序试，用上的代理放在 GH_PROXY。
download_nagi() {
  if command -v sha256sum >/dev/null 2>&1; then
    SHA="sha256sum"
  elif command -v shasum >/dev/null 2>&1; then
    SHA="shasum -a 256"
  else
    fail "需要 sha256sum 或 shasum 以校验下载的文件"
  fi
  case "$BASE" in
    https://github.com/*) URLS=$(gh_urls "$BASE") ;;
    *) URLS=$BASE ;;
  esac
  OK=""
  for U in $URLS; do
    say "下载 nagi（linux/$ARCH）：$U"
    if fetch_nagi "$U"; then
      OK=1
      [ "$U" = "$BASE" ] || GH_PROXY=${U%"$BASE"}
      break
    fi
  done
  [ -n "$OK" ] || fail "下载失败：$BASE/nagi-linux-$ARCH（网络不通时可用 --download-url 指定镜像）"
  chmod 0755 "$TMP/nagi"
  NEW_VERSION=$("$TMP/nagi" version 2>/dev/null) || fail "下载的 nagi 无法在本机运行"
}

# <<< 共用代码结束

# ---- 环境检查 ----

[ "$(uname -s)" = "Linux" ] || fail "Nagi 仅支持 Linux"
if [ "$(id -u)" != "0" ]; then
  # sudo 不传递环境变量，NAGI_STANDBY 要在 root 的 shell 里设置。
  [ -z "$STANDBY" ] || [ -n "$STANDBY_ARG" ] || fail "请先执行 sudo -i 切换到 root，再执行安装命令（sudo 不传递环境变量 NAGI_STANDBY）"
  fail "请使用 root 运行（例如 curl ... | sudo sh）"
fi
case "$(uname -m)" in
  x86_64 | amd64) ARCH=amd64 ;;
  aarch64 | arm64) ARCH=arm64 ;;
  *) fail "不支持的 CPU 架构 $(uname -m)，仅支持 amd64 和 arm64" ;;
esac
command -v curl >/dev/null 2>&1 || fail "需要 curl"

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

# on_exit 在脚本退出时执行，$1 是退出码：删掉临时目录。备用面板没装成（出错或被中断）时不把配对码留在磁盘上：
# 还没配上对时先停下面板（免得它带着配对码反复重试，或者在脚本退出以后才配上对、之后因为缺少配对码文件起不来），
# 再删掉配对码文件；已经配上对时和成功时一样，从服务配置中去掉配对码文件。
on_exit() {
  rm -rf "$TMP"
  [ "$1" -ne 0 ] && [ -n "$STANDBY_WRITTEN" ] || return 0
  [ -f "$DATA_DIR/panel/paired" ] || systemctl stop "$SERVICE" >/dev/null 2>&1 || true
  if [ -f "$DATA_DIR/panel/paired" ]; then
    write_unit "" || true
  else
    rm -f "$STANDBY_ENV"
    say "备用面板未完成配对，已停止面板并删除配对码文件 $STANDBY_ENV。" >&2
  fi
}

# standby_version_check 在要装的 nagi（$1，版本号）太旧时停下：v0.35.0 之前的 nagi 不读环境变量 NAGI_STANDBY 里的配对码，
# 照常安装会在空的数据目录里初始化成一个新的主面板。版本的匹配和节点安装脚本的 enrollVersionCheck 一样。
standby_version_check() {
  case "$1" in
    v0.[0-9].* | v0.[12][0-9].* | v0.3[0-4].*)
      fail "要安装的 nagi 是 $1，低于 v0.35.0，不能用这个脚本安装备用面板。请改用该版本的安装脚本 https://github.com/$REPO/releases/download/$1/install.sh，或更新 --download-url 指向的镜像后重试" ;;
  esac
}

TMP=$(mktemp -d)
trap 'on_exit $?' EXIT
trap 'exit 130' INT TERM HUP

detect_cn

# 面板自带节点，节点需要 Docker；只运行面板（含备用面板）时不需要。
[ -n "$NO_AGENT" ] || ensure_docker

# ---- 下载并校验 ----

OLD_VERSION=""
[ -x "$BIN" ] && OLD_VERSION=$("$BIN" version 2>/dev/null || true)

# 下载前先说清楚这次装哪个版本。latest 跟随 GitHub 的重定向得到版本号，并按它下载，免得显示的和下载的不一致；
# 查不到时仍从 latest 下载。镜像不分版本，要下载后才知道。
TARGET=""
if [ -n "$DOWNLOAD_URL" ]; then
  BASE=${DOWNLOAD_URL%/}
elif [ "$VERSION" = latest ]; then
  BASE="https://github.com/$REPO/releases/latest/download"
  for U in $(gh_urls "https://github.com/$REPO/releases/latest"); do
    TARGET=$(curl -sI --retry 2 --max-time 10 -o /dev/null -w '%{redirect_url}' "$U" || true)
    TARGET=${TARGET##*/}
    case "$TARGET" in
      v[0-9]*)
        BASE="https://github.com/$REPO/releases/download/$TARGET"
        break
        ;;
      *) TARGET="" ;;
    esac
  done
else
  BASE="https://github.com/$REPO/releases/download/$VERSION"
  TARGET=$VERSION
fi

if [ -z "$TARGET" ]; then
  [ -z "$OLD_VERSION" ] || say "当前版本：$OLD_VERSION"
elif [ -z "$OLD_VERSION" ]; then
  say "本次安装 nagi $TARGET"
elif [ "$OLD_VERSION" = "$TARGET" ]; then
  say "当前已是 $TARGET，重新安装一遍"
else
  say "当前版本 $OLD_VERSION，本次更新到 $TARGET"
fi

# 备用面板：--version 指定了旧版本时下载前就停下，镜像里的版本要下载后才知道。
[ -z "$STANDBY" ] || [ -z "$TARGET" ] || standby_version_check "$TARGET"
download_nagi
[ -z "$STANDBY" ] || standby_version_check "$NEW_VERSION"

install -m 0755 "$TMP/nagi" "$BIN"
link_command
# 用了镜像或加速代理时记下来，面板上一键更新时也从这里下载（见 internal/pkg/release/install.go）。
if [ -n "$DOWNLOAD_URL" ]; then
  mkdir -p "$DATA_DIR/panel"
  printf '%s\n' "$DOWNLOAD_URL" >"$DATA_DIR/panel/download-url"
fi
if [ -n "$GH_PROXY" ]; then
  mkdir -p "$DATA_DIR/panel"
  printf '%s\n' "$GH_PROXY" >"$DATA_DIR/panel/github-proxy"
else
  rm -f "$DATA_DIR/panel/github-proxy"
fi
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
  printf '%s' "$ARGS"
}

if [ -z "$HAS_SYSTEMD" ]; then
  say ""
  say "未检测到 systemd，请以服务方式自行运行："
  if [ -n "$STANDBY" ]; then
    say "  NAGI_STANDBY=<配对码> $BIN $(server_args)"
  else
    say "  $BIN $(server_args)"
  fi
  exit 0
fi

# 配对码不写进启动参数和服务配置（进程参数和 $UNIT 谁都能读），放在只有 root 能读的文件里，由 systemd 作为环境变量
# NAGI_STANDBY 传给面板；配对成功后删掉。
STANDBY_ENV="$DATA_DIR/standby.env"

# write_standby_env 把配对码 $1 写进 $STANDBY_ENV，权限 0600：先删掉原有的文件（权限可能更宽，也可能是指向别处的链接），
# 再新建（set -C：删掉以后又出现同名文件时不写，报错退出）。
write_standby_env() {
  mkdir -p "$DATA_DIR"
  rm -f "$STANDBY_ENV"
  STANDBY_WRITTEN=1
  (umask 077 && set -C && printf 'NAGI_STANDBY=%s\n' "$1" >"$STANDBY_ENV") || fail "无法写入 $STANDBY_ENV"
}

# write_unit 写服务配置；$1 不为空时是配对码。
write_unit() {
  WANTS="network-online.target"
  [ -n "$NO_AGENT" ] || WANTS="$WANTS docker.service"
  ENV_LINE=""
  if [ -n "${1:-}" ]; then
    write_standby_env "$1"
    ENV_LINE="EnvironmentFile=$STANDBY_ENV"
  else
    rm -f "$STANDBY_ENV"
  fi
  cat >"$UNIT" <<EOF
[Unit]
Description=Nagi panel
After=$WANTS
Wants=$WANTS

[Service]
$ENV_LINE
ExecStart=$BIN $(server_args)
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
say "  重置管理员密码：nagi reset password --data-dir $DATA_DIR（更多命令：nagi --help）"
say "  升级：在面板的「设置 → 关于」一键更新，或再次执行本脚本；节点在面板的节点页一键升级。"
