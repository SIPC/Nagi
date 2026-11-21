#!/bin/sh
# 核对要发布的标签（.github/workflows/release.yml 的 tag 任务用），通过时打印 prerelease=true 或 prerelease=false。
#
#   scripts/release-tag.sh <标签> [已发布的同名 Release]
#
# - 标签必须是 v<主>.<次>.<修订>（正式版）或 v<主>.<次>.<修订>-pre.<序号>（预发布版，序号 1～98，没有前导 0）。
#   其他写法（rc、beta、构建信息、前导 0）不发布：面板按语义化版本号比较，写法统一才不会出现意外的先后。
# - 第二个参数是 GitHub 上已有的同名 Release（没有时为空）。同一版本号不重新发布：面板按版本号判断要不要更新，
#   同号的新包装了也认不出（见 docs/development.md#预发布版本）。要修正就发下一个版本号。
set -eu

TAG=${1:?用法：release-tag.sh <标签> [已发布的同名 Release]}
RELEASED=${2:-}

num='(0|[1-9][0-9]*)'
if printf '%s\n' "$TAG" | grep -Eqx "v$num\\.$num\\.$num"; then
  PRE=false
elif printf '%s\n' "$TAG" | grep -Eqx "v$num\\.$num\\.$num-pre\\.[1-9][0-9]?" && [ "${TAG##*.}" -le 98 ]; then
  # 序号最大 98：安卓 App 的 versionCode 给每个修订号留 100 个位置，99 是正式版（见 android/app/build.gradle.kts）。
  PRE=true
else
  echo "标签 $TAG 不是 vX.Y.Z 或 vX.Y.Z-pre.N，不发布" >&2
  exit 1
fi

if [ -n "$RELEASED" ]; then
  echo "$TAG 已经发布过，同一版本号不重新发布：面板认不出同号的新包。请发下一个版本号" >&2
  exit 1
fi

echo "prerelease=$PRE"
