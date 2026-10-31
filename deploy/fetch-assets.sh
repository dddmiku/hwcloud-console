#!/usr/bin/env bash
#
# 准备 Docker 构建所需的 Playwright 资源。
#
# 为什么需要这一步：
#   Playwright 的 node 驱动（131 MB）和 Chromium（384 MB）版本必须与
#   go.mod 里的 playwright-go 严格对应 —— 当前是 v0.6100.0，它要求
#   driver 1.61.1 / Chromium revision 1228。
#   版本对不上时浏览器起不来，报的错是 "Executable doesn't exist"，
#   很难看出是版本问题。所以不用「最新版」，而是从一台**已验证可用**
#   的机器上原样取下来。
#
# 用法：
#   ./fetch-assets.sh local          # 从本机已装好的目录复制
#   ./fetch-assets.sh ssh user@host  # 从远端服务器拉
#   ./fetch-assets.sh download       # 用官方 playwright 下载（需要外网）
#
# 产物放在 deploy/assets/，Dockerfile 直接 COPY 进去。

set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ASSETS="$HERE/assets"
MODE="${1:-local}"

# 期望版本：改 go.mod 里的 playwright-go 时这里也要跟着改
EXPECT_DRIVER="1.61.1"
EXPECT_CHROMIUM="1228"

info() { echo "[fetch-assets] $*"; }
die()  { echo "[fetch-assets] 错误: $*" >&2; exit 1; }

mkdir -p "$ASSETS"

check_version() {
    local dir="$1"
    local pkg="$dir/playwright-driver/package/package.json"
    if [ -f "$pkg" ]; then
        local v
        v=$(grep -o '"version"[^,]*' "$pkg" | head -1 | grep -o '[0-9][0-9.]*')
        if [ "$v" != "$EXPECT_DRIVER" ]; then
            info "警告: 驱动版本 $v 与期望的 $EXPECT_DRIVER 不一致"
        else
            info "驱动版本 $v ✓"
        fi
    fi
    if [ -d "$dir/browsers/chromium-$EXPECT_CHROMIUM" ]; then
        info "Chromium revision $EXPECT_CHROMIUM ✓"
    else
        info "警告: 未找到 chromium-$EXPECT_CHROMIUM"
    fi
}

case "$MODE" in
    local)
        SRC="${2:-/opt/hwcloud-demo}"
        [ -d "$SRC" ] || die "源目录不存在: $SRC"
        info "从 $SRC 复制…"
        rsync -a --delete "$SRC/playwright-driver/" "$ASSETS/playwright-driver/"
        mkdir -p "$ASSETS/browsers"
        rsync -a --delete "$SRC/browsers/chromium-$EXPECT_CHROMIUM/" \
              "$ASSETS/browsers/chromium-$EXPECT_CHROMIUM/"
        # ffmpeg 不是必需的（不录制视频），但 playwright 校验时会找
        [ -d "$SRC/browsers/ffmpeg-1011" ] && \
            rsync -a --delete "$SRC/browsers/ffmpeg-1011/" "$ASSETS/browsers/ffmpeg-1011/"
        ;;

    ssh)
        REMOTE="${2:-}"
        [ -n "$REMOTE" ] || die "用法: fetch-assets.sh ssh user@host"
        info "从 $REMOTE 拉取…"
        mkdir -p "$ASSETS/browsers"
        rsync -az --delete "$REMOTE:/opt/hwcloud-demo/playwright-driver/" \
              "$ASSETS/playwright-driver/"
        rsync -az --delete "$REMOTE:/opt/hwcloud-demo/browsers/chromium-$EXPECT_CHROMIUM/" \
              "$ASSETS/browsers/chromium-$EXPECT_CHROMIUM/"
        rsync -az --delete "$REMOTE:/opt/hwcloud-demo/browsers/ffmpeg-1011/" \
              "$ASSETS/browsers/ffmpeg-1011/" 2>/dev/null || true
        ;;

    download)
        info "用官方 playwright 下载（需要外网，约 500 MB）…"
        command -v npx >/dev/null || die "需要 node/npx"
        cd "$ASSETS"
        PLAYWRIGHT_BROWSERS_PATH="$ASSETS/browsers" \
            npx --yes "playwright@$EXPECT_DRIVER" install chromium
        # driver 本体：从 npm 包解出来
        npx --yes "playwright@$EXPECT_DRIVER" --version >/dev/null
        ;;

    *)
        die "未知模式: $MODE（可选 local / ssh / download）"
        ;;
esac

info "校验产物…"
check_version "$ASSETS"

SIZE=$(du -sh "$ASSETS" | cut -f1)
info "完成，assets 总大小 $SIZE"
info ""
info "接下来："
info "  cd $(dirname "$HERE") && docker compose build"
