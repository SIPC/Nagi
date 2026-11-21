#!/bin/sh
# 打印 CHANGELOG.md 中某个版本的说明（「## <版本>」到下一个「## 」之前），供 GitHub Release 的正文使用。
# 末尾附上通用的升级方式。没有这个版本时退出码为 1。预发布版（v0.38.0-pre.1）的升级方式写成安装这个版本的命令，
# 并说明它是预发布版（见 docs/development.md#预发布版本）。
#
#   scripts/release-notes.sh v0.3.0
set -eu

TAG=${1:?用法：release-notes.sh <版本>}
FILE=$(dirname "$0")/../CHANGELOG.md

BODY=$(awk -v tag="$TAG" '
  /^## / { if (found) exit; if ($2 == tag) { found = 1; next } }
  found { print }
' "$FILE")
BODY=$(printf '%s\n' "$BODY" | sed -e '/./,$!d')
[ -n "$BODY" ] || {
  echo "CHANGELOG.md 中没有 $TAG" >&2
  exit 1
}

printf '%s\n\n' "$BODY"
case "$TAG" in
  *-*)
    cat <<EOF
### 安装与升级

这是预发布版，用于在正式发布前试用，可能不稳定，不会推送给使用正式版渠道的面板和节点。在面板服务器上以 root 执行以下命令安装或升级到这个版本：

\`\`\`bash
curl -fsSL https://github.com/SIPC/Nagi/releases/download/$TAG/install.sh | sudo sh -s -- --version $TAG
\`\`\`

也可以在面板的「设置 → 关于」中把更新渠道改为「预发布」后一键更新。节点要在节点设置中把更新渠道改为「预发布」才能升级到这个版本。完整的更新日志见 [CHANGELOG.md](https://github.com/SIPC/Nagi/blob/main/CHANGELOG.md)。
EOF
    exit 0
    ;;
esac
cat <<'EOF'
### 安装与升级

全新安装或升级面板（在面板服务器上以 root 执行）：

```bash
curl -fsSL https://github.com/SIPC/Nagi/releases/latest/download/install.sh | sudo sh
```

v0.6.0 及以后的面板也可以在设置中一键更新。面板升级后，在面板的「节点」页一键升级各节点。完整的更新日志见 [CHANGELOG.md](https://github.com/SIPC/Nagi/blob/main/CHANGELOG.md)。
EOF
