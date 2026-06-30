#!/usr/bin/env bash
# 一键交叉编译 dashboard + agent 到所有常用平台。
# 用法: ./scripts/build-all.sh [版本号]
# 产物输出到 dist/
set -euo pipefail

VERSION="${1:-$(git describe --tags --always 2>/dev/null || echo dev)}"
OUT="dist"

# 平台列表: GOOS/GOARCH
PLATFORMS=(
  "linux/amd64"
  "linux/arm64"
  "linux/arm/7"
  "darwin/amd64"
  "darwin/arm64"
  "windows/amd64"
  "windows/arm64"
)

mkdir -p "$OUT"
echo "==> 编译版本 $VERSION -> $OUT/"

for p in "${PLATFORMS[@]}"; do
  goos="${p%%/*}"
  rest="${p#*/}"
  goarch="${rest%%/*}"
  # 处理 GOARM(如 arm/7)
  goarm=""
  if [[ "$rest" == *"/"* ]]; then
    goarm="${rest##*/}"
  fi

  for bin in dashboard agent; do
    ext=""
    [[ "$goos" == "windows" ]] && ext=".exe"
    outname="$OUT/${bin}-${goos}-${goarch}${goarm:+-${goarm}}-${VERSION}${ext}"
    echo "  -> ${bin} $goos/$goarch${goarm:+/$goarm}"
    export GOOS="$goos" GOARCH="$goarch"
    [[ -n "$goarm" ]] && export GOARM="$goarm" || unset GOARM
    export CGO_ENABLED=0
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" \
      -o "$outname" "./cmd/$bin"
  done
done

# 同时为当前平台生成默认名,方便本机测试
export GOOS GOARCH 2>/dev/null || true
unset GOOS GOARCH GOARM CGO_ENABLED
go build -trimpath -ldflags "-s -w" -o "$OUT/dashboard" ./cmd/dashboard
go build -trimpath -ldflags "-s -w" -o "$OUT/agent" ./cmd/agent

echo
echo "==> 完成。产物:"
ls -lh "$OUT" | awk 'NR>1{print "   "$5"\t"$9}'
echo
echo "Dashboard 部署: ./dashboard -addr :8443 -secret <密钥> -tls-cert cert.pem -tls-key key.pem"
echo "Agent 部署:     ./agent -server <域名>:8443 -token <密钥> -tls -name <主机名>"
