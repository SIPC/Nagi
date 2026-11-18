#!/bin/sh
# 把 Google 最新的证明证书吊销列表放进面板（见 docs/android.md#吊销列表），发布时在编译之前运行（release.yml）。
# 取不到、格式不对、面板读不了时保留仓库里的那份，不让发布失败。
set -eu
dir=internal/server/auth/attestation
tmp=$(mktemp)
hdr=$(mktemp)
old=$(mktemp -d)
trap 'rm -rf "$tmp" "$hdr" "$old"' EXIT
cp "$dir/status.json" "$dir/status-date.txt" "$old/"
if curl -fsS --max-time 60 -D "$hdr" -o "$tmp" https://android.googleapis.com/attestation/status &&
  jq -e '.entries | type == "object" and length > 0' "$tmp" > /dev/null; then
  lm=$(grep -i '^last-modified:' "$hdr" | cut -d' ' -f2- | tr -d '\r' || true)
  now=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  # 日期取 Last-Modified，解析不了或晚于现在时取现在（面板运行时也这样）。RFC 3339 的 UTC 时间可以按字符串比较。
  date=$(date -u -d "${lm:-now}" +%Y-%m-%dT%H:%M:%SZ 2> /dev/null) || date=$now
  if [ "$date" \> "$now" ]; then date=$now; fi
  # CDN 可能给出旧的副本：不比仓库里的新就不换。
  if [ ! "$date" \> "$(cat "$old/status-date.txt")" ]; then
    echo "吊销列表没有比仓库里的新（$date），用仓库里的"
    exit 0
  fi
  cp "$tmp" "$dir/status.json"
  echo "$date" > "$dir/status-date.txt"
  if go test -count=1 -run 'TestEmbeddedRevocations' "./$dir/" > /dev/null; then
    echo "吊销列表：$(jq '.entries | length' "$dir/status.json") 项，$date"
    exit 0
  fi
  cp "$old/status.json" "$old/status-date.txt" "$dir/"
fi
echo "::warning::取不到可用的吊销列表，用仓库里的（$(cat "$dir/status-date.txt")）"
