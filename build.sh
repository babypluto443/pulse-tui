#!/usr/bin/env bash
# build.sh —— 交叉编译四平台产物到 dist/。
# 用法: ./build.sh [版本号]   (默认 v0.1.0)
# 产物: dist/pulse-tui_<版本>_<GOOS>_<GOARCH>.<tar.gz|zip>
set -euo pipefail

VERSION="${1:-v0.1.0}"
ROOT="$(cd "$(dirname "$0")" && pwd)"
DIST="$ROOT/dist"
PKG="github.com/babypluto/pulse-tui/cmd/pulse-tui"

rm -rf "$DIST"
mkdir -p "$DIST"

for platform in \
    linux/amd64  linux/arm64 \
    darwin/amd64 darwin/arm64; do
    GOOS="${platform%/*}"
    GOARCH="${platform#*/}"
    name="pulse-tui_${VERSION}_${GOOS}_${GOARCH}"

    echo "==> building $name"
    (cd "$ROOT" && GOOS="$GOOS" GOARCH="$GOARCH" CGO_ENABLED=0 \
        go build -trimpath -ldflags "-s -w" -o "$DIST/$name" "$PKG")

    if [ "$GOOS" = "windows" ]; then  # 预留 windows 扩展位：当前矩阵未含
        :
    elif [ "$GOOS" = "darwin" ] || [ "$GOOS" = "linux" ]; then
        tar -czf "$DIST/$name.tar.gz" -C "$DIST" "$name"
        rm "$DIST/$name"
    fi
done

echo
ls -lh "$DIST"
