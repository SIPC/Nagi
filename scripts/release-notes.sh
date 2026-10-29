#!/bin/sh
# 打印 CHANGELOG.md 中某个版本的说明（「## <版本>」到下一个「## 」之前），供 GitHub Release 的正文使用。
# 末尾附上通用的升级方式。没有这个版本时退出码为 1。
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
cat <<'EOF'
### 安装与升级

全新安装或升级面板（在面板服务器上以 root 执行）：

```bash
curl -fsSL https://github.com/SIPC/Nagi/releases/latest/download/install.sh | sudo sh
```

面板升级后，在面板的「节点」页一键升级各节点。完整的更新日志见 [CHANGELOG.md](https://github.com/SIPC/Nagi/blob/main/CHANGELOG.md)。
EOF
