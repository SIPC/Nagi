#!/bin/sh
# Nagi 一键卸载脚本，面板和节点通用。
#
#   curl -fsSL https://github.com/SIPC/Nagi/releases/latest/download/uninstall.sh | sudo sh
#   curl -fsSL https://github.com/SIPC/Nagi/releases/latest/download/uninstall.sh | sudo sh -s -- --purge
#
# 默认只卸载程序：停止并删除 nagi-server、nagi-agent 服务和 nagi 命令，删除项目内网的代理容器；
# 项目容器、Nagi 数据目录和项目文件都保留。
# --purge 另外删除 Nagi 管理的所有容器、网络，以及 Nagi 数据目录（面板数据库、密钥、节点身份和缓存）。
# 项目文件（容器目录，默认在 /srv/nagi 下，含运行数据和快照）任何情况下都不删除，只列出位置。
# Docker 本身不卸载。
set -eu

BIN=/usr/local/bin/nagi
UNIT_DIR=/etc/systemd/system
LINK_IMAGE=nagi-link:empty
PURGE=""
YES=""
EXTRA_DIRS=""

usage() {
  cat <<'EOF'
用法：uninstall.sh [选项]

  --purge             同时删除 Nagi 管理的容器、网络和 Nagi 数据目录（不可恢复）
  --data-dir <目录>   额外要删除的数据目录，配合 --purge 使用（未以 systemd 服务运行时需要指定）
  -y, --yes           不询问，直接执行
  -h, --help          显示帮助

项目文件（容器目录，默认在 /srv/nagi 下）不会被删除，确认不再需要后请手动删除。
本机是节点时，建议先在面板上把容器迁移走或删除，再在「节点」页移除这个节点。
EOF
}

say() { printf '%s\n' "$*"; }
fail() {
  printf '错误：%s\n' "$*" >&2
  exit 1
}

while [ $# -gt 0 ]; do
  case "$1" in
    --purge) PURGE=1; shift ;;
    --data-dir)
      [ $# -ge 2 ] && [ -n "$2" ] || fail "--data-dir 需要一个值"
      case "$2" in /*) ;; *) fail "数据目录须为绝对路径" ;; esac
      EXTRA_DIRS="$EXTRA_DIRS $2"
      shift 2
      ;;
    -y | --yes) YES=1; shift ;;
    -h | --help) usage; exit 0 ;;
    *) fail "未知参数 $1（--help 查看用法）" ;;
  esac
done

[ "$(uname -s)" = "Linux" ] || fail "Nagi 仅支持 Linux"
[ "$(id -u)" = "0" ] || fail "请使用 root 运行（例如 curl ... | sudo sh）"

HAS_SYSTEMD=""
if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
  HAS_SYSTEMD=1
fi

# ---- 收集要处理的内容 ----

# unit_data_dir 打印服务配置里的 --data-dir（没写时为默认的 /var/lib/nagi）。
unit_data_dir() {
  DIR=/var/lib/nagi
  # shellcheck disable=SC2046
  set -- $(sed -n 's/^ExecStart=//p' "$1")
  while [ $# -gt 0 ]; do
    [ "$1" != "--data-dir" ] || DIR="${2:-$DIR}"
    shift
  done
  printf '%s' "$DIR"
}

SERVICES=""
DATA_DIRS=""
for s in nagi-server nagi-agent; do
  if [ -f "$UNIT_DIR/$s.service" ]; then
    SERVICES="$SERVICES $s"
    DATA_DIRS="$DATA_DIRS $(unit_data_dir "$UNIT_DIR/$s.service")"
  fi
done
[ -n "$DATA_DIRS" ] || [ ! -d /var/lib/nagi ] || DATA_DIRS=/var/lib/nagi
# shellcheck disable=SC2086
DATA_DIRS=$(printf '%s\n' $DATA_DIRS $EXTRA_DIRS | grep . | sort -u | tr '\n' ' ')

HAS_DOCKER=""
if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
  HAS_DOCKER=1
fi

count() { printf '%s\n' "$1" | grep -c . || true; }

LINK_IDS=""
PROJECT_IDS=""
COMPOSE_PROJECTS=""
PROJECT_DIRS=""
if [ -n "$HAS_DOCKER" ]; then
  LINK_IDS=$(docker ps -aq --filter label=nagi.link)
  PROJECT_IDS=$(docker ps -aq --filter label=nagi.project)
  COMPOSE_PROJECTS=$(docker ps -a --filter label=nagi.project --format '{{.Label "com.docker.compose.project"}}' | grep . | sort -u || true)
  # 容器目录：compose 的项目目录是 <容器目录>/data。
  PROJECT_DIRS=$(docker ps -a --filter label=nagi.project --format '{{.Label "com.docker.compose.project.working_dir"}}' |
    grep . | sed 's#/data/*$##' | sort -u || true)
fi
# 节点设置里的项目根目录（面板内嵌节点的数据在 <面板数据目录>/agent 下）。
ROOTS=""
for d in $DATA_DIRS; do
  for f in "$d/identity/projects-root" "$d/agent/identity/projects-root"; do
    [ ! -f "$f" ] || ROOTS="$ROOTS $(head -n 1 "$f")"
  done
done
[ -n "$ROOTS" ] || [ ! -d /srv/nagi ] || ROOTS=/srv/nagi
# 要保留的项目文件：去掉已经包含在其他目录里的（排序后上级目录在前）。
KEEP_DIRS=""
# shellcheck disable=SC2086
for d in $(printf '%s\n' $ROOTS $PROJECT_DIRS | grep . | sort -u || true); do
  covered=""
  for k in $KEEP_DIRS; do
    case "$d/" in "$k"/*) covered=1 ;; esac
  done
  [ -n "$covered" ] || KEEP_DIRS="$KEEP_DIRS $d"
done

if [ -z "$SERVICES" ] && [ ! -e "$BIN" ] && [ -z "$LINK_IDS" ] && [ -z "$PROJECT_IDS" ] && [ -z "$DATA_DIRS" ]; then
  say "本机没有发现 Nagi。"
  exit 0
fi

# ---- 确认 ----

say "将卸载："
for s in $SERVICES; do
  case "$s" in
    nagi-server) say "  - 服务 nagi-server（面板）" ;;
    nagi-agent) say "  - 服务 nagi-agent（节点）" ;;
  esac
done
[ ! -e "$BIN" ] || say "  - 程序 $BIN 及 nagi 命令"
[ -z "$LINK_IDS" ] || say "  - 项目内网的代理容器 $(count "$LINK_IDS") 个"
if [ -n "$PURGE" ]; then
  [ -z "$PROJECT_IDS" ] || say "  - Nagi 管理的容器 $(count "$PROJECT_IDS") 个（$(count "$COMPOSE_PROJECTS") 组）及其网络"
  for d in $DATA_DIRS; do say "  - 数据目录 $d"; done
fi
say ""
say "将保留："
if [ -z "$PURGE" ]; then
  [ -z "$PROJECT_IDS" ] || say "  - 项目容器 $(count "$PROJECT_IDS") 个（继续运行，但不再经 Caddy 对外提供访问）"
  for d in $DATA_DIRS; do say "  - 数据目录 $d"; done
fi
for d in $KEEP_DIRS; do say "  - 项目文件 $d"; done
[ -z "$HAS_DOCKER" ] || say "  - Docker"
say ""
case " $SERVICES " in
  *" nagi-server "*)
    say "提示：卸载面板后，已加入的节点将失去管理（容器照常运行）。配置了备用面板时，"
    say "      可先在备用面板上将其提升为主面板；也可以先把节点移交给其他面板。"
    say ""
    ;;
  *" nagi-agent "*)
    say "提示：建议先在面板上迁移或删除本节点的容器，再在「节点」页移除这个节点。"
    say ""
    ;;
esac

if [ -z "$YES" ]; then
  ( : </dev/tty ) 2>/dev/null || fail "无法交互确认，确认无误后加 -y 重新执行"
  if [ -n "$PURGE" ]; then
    printf '数据删除后无法恢复。输入 yes 确认卸载：' >/dev/tty
  else
    printf '输入 yes 确认卸载：' >/dev/tty
  fi
  read -r ANSWER </dev/tty || ANSWER=""
  [ "$ANSWER" = "yes" ] || fail "已取消"
fi

# ---- 执行 ----

for s in $SERVICES; do
  say "停止服务 $s……"
  if [ -n "$HAS_SYSTEMD" ]; then
    systemctl disable --now "$s" >/dev/null 2>&1 || true
  fi
  rm -f "$UNIT_DIR/$s.service"
done
[ -z "$SERVICES" ] || [ -z "$HAS_SYSTEMD" ] || systemctl daemon-reload
# 没有以 systemd 服务运行时，结束手动启动的面板和节点进程。
if command -v pkill >/dev/null 2>&1; then
  pkill -f "^$BIN (server|agent)" 2>/dev/null || true
fi

if [ -n "$HAS_DOCKER" ]; then
  if [ -n "$LINK_IDS" ]; then
    say "删除代理容器……"
    # shellcheck disable=SC2086
    docker rm -f $LINK_IDS >/dev/null 2>&1 || true
  fi
  docker image rm "$LINK_IMAGE" >/dev/null 2>&1 || true

  if [ -n "$PURGE" ]; then
    for p in $COMPOSE_PROJECTS; do
      say "删除容器组 $p……"
      docker compose -p "$p" down --remove-orphans >/dev/null 2>&1 || true
      IDS=$(docker ps -aq --filter "label=com.docker.compose.project=$p")
      # shellcheck disable=SC2086
      [ -z "$IDS" ] || docker rm -f $IDS >/dev/null 2>&1 || true
      NETS=$(docker network ls -q --filter "label=com.docker.compose.project=$p")
      # shellcheck disable=SC2086
      [ -z "$NETS" ] || docker network rm $NETS >/dev/null 2>&1 || true
      # 命名 volume 绑定在容器目录的 volumes/ 下，删除 volume 对象不会删除其中的数据。
      VOLS=$(docker volume ls -q --filter "label=com.docker.compose.project=$p")
      # shellcheck disable=SC2086
      [ -z "$VOLS" ] || docker volume rm $VOLS >/dev/null 2>&1 || true
    done
    NETS=$(docker network ls -q --filter label=nagi.project)
    if [ -n "$NETS" ]; then
      say "删除项目内网……"
      # shellcheck disable=SC2086
      docker network rm $NETS >/dev/null 2>&1 || true
    fi
  fi
fi

say "删除 nagi 命令……"
if [ -L /usr/bin/nagi ] && [ "$(readlink /usr/bin/nagi)" = "$BIN" ]; then
  rm -f /usr/bin/nagi
fi
rm -f "$BIN" /usr/share/bash-completion/completions/nagi

if [ -n "$PURGE" ]; then
  for d in $DATA_DIRS; do
    # 只删除至少两级的绝对路径，避免误删 / 或 /var 这样的目录。
    case "$d" in
      /?*/?*)
        say "删除数据目录 $d……"
        rm -rf "$d"
        ;;
      *) say "跳过可疑的数据目录 $d，请手动确认后删除" ;;
    esac
  done
fi

say ""
say "Nagi 已卸载。"
if [ -z "$PURGE" ] && [ -n "$DATA_DIRS" ]; then
  say ""
  say "  数据目录已保留（含面板数据库、密钥和节点身份），不再需要时可加 --purge 重新执行本脚本删除。"
fi
if [ -n "$KEEP_DIRS" ]; then
  say ""
  say "  项目文件已保留（含容器的运行数据和快照），确认不再需要后可手动删除："
  for d in $KEEP_DIRS; do say "    rm -rf $d"; done
fi
if [ -f /etc/docker/daemon.json ] && grep -q registry-mirrors /etc/docker/daemon.json 2>/dev/null; then
  say ""
  say "  /etc/docker/daemon.json 中的镜像加速（registry-mirrors）未改动，如曾在面板上配置过，可按需删除。"
fi
