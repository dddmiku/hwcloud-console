#!/usr/bin/env bash
#
# 一键准备 Docker 构建资源。
#
# 做三件事：
#   1. 编译 Linux 二进制（或用 --from-server 从已部署的机器取）
#   2. 取 Playwright 驱动 + Chromium（版本必须与 go.mod 对应）
#   3. 拷 data/ 内容（任务模板）
#
# 产物全部落在 deploy/assets/，Dockerfile 直接从那里 COPY。
#
# 用法：
#   ./build.sh                      # 本机编译（需要 Go）
#   ./build.sh --from-server        # 从已部署的服务器取二进制和浏览器
#   ./build.sh --from-server root@1.2.3.4
#
# 之后：
#   cd deploy && docker compose build

set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(dirname "$HERE")"
ASSETS="$HERE/assets"

# 期望版本：改 go.mod 里的 playwright-go 时这里也要跟着改
EXPECT_DRIVER="1.61.1"
EXPECT_CHROMIUM="1228"

info() { echo -e "\033[36m[build]\033[0m $*"; }
ok()   { echo -e "\033[32m[build]\033[0m $*"; }
die()  { echo -e "\033[31m[build] 错误:\033[0m $*" >&2; exit 1; }

MODE="local"
REMOTE=""
for arg in "$@"; do
    case "$arg" in
        --from-server) MODE="server" ;;
        root@*|*@*) REMOTE="$arg" ;;
        *) die "未知参数: $arg" ;;
    esac
done

mkdir -p "$ASSETS"

# ---------- 1. 二进制 ----------
info "准备 hwcloud 二进制…"
if [ "$MODE" = "server" ]; then
    [ -n "$REMOTE" ] || die "用法: build.sh --from-server root@<ip>"
    info "从 $REMOTE 取二进制…"
    scp "$REMOTE:/opt/hwcloud-demo/go/bin/hwcloud" "$ASSETS/hwcloud"
else
    command -v go >/dev/null || die "本机没有 Go。改用: build.sh --from-server root@<ip>"
    info "本地交叉编译 linux/amd64…"
    ( cd "$ROOT/go" && \
      CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
      go build -trimpath -ldflags="-s -w" -o "$ASSETS/hwcloud" ./cmd/hwcloud )
fi
chmod +x "$ASSETS/hwcloud"
ok "二进制就绪: $(du -h "$ASSETS/hwcloud" | cut -f1)"

# ---------- 2. Playwright 资源 ----------
info "准备 Playwright 驱动和 Chromium…"
if [ -d "$ASSETS/playwright-driver/package" ] && \
   [ -d "$ASSETS/browsers/chromium-$EXPECT_CHROMIUM" ]; then
    ok "已存在，跳过（删掉 deploy/assets 可强制重取）"
else
    if [ "$MODE" = "server" ]; then
        [ -n "$REMOTE" ] || die "需要指定服务器地址"
        info "从 $REMOTE 拉取（约 500 MB）…"
        mkdir -p "$ASSETS/browsers"
        rsync -az --delete "$REMOTE:/opt/hwcloud-demo/playwright-driver/" \
              "$ASSETS/playwright-driver/"
        rsync -az --delete "$REMOTE:/opt/hwcloud-demo/browsers/chromium-$EXPECT_CHROMIUM/" \
              "$ASSETS/browsers/chromium-$EXPECT_CHROMIUM/"
        rsync -az --delete "$REMOTE:/opt/hwcloud-demo/browsers/ffmpeg-1011/" \
              "$ASSETS/browsers/ffmpeg-1011/" 2>/dev/null || true
    else
        SRC="${HWCLOUD_SRC:-/opt/hwcloud-demo}"
        [ -d "$SRC/playwright-driver" ] || \
            die "找不到 $SRC/playwright-driver。用 --from-server 或设置 HWCLOUD_SRC"
        info "从 $SRC 复制…"
        mkdir -p "$ASSETS/browsers"
        rsync -a --delete "$SRC/playwright-driver/" "$ASSETS/playwright-driver/"
        rsync -a --delete "$SRC/browsers/chromium-$EXPECT_CHROMIUM/" \
              "$ASSETS/browsers/chromium-$EXPECT_CHROMIUM/"
        [ -d "$SRC/browsers/ffmpeg-1011" ] && \
            rsync -a --delete "$SRC/browsers/ffmpeg-1011/" "$ASSETS/browsers/ffmpeg-1011/"
    fi
    ok "Playwright 资源就绪"
fi

# ---------- 3. 任务数据 ----------
info "拷贝 data/ 模板…"
rsync -a --delete "$ROOT/data/" "$ASSETS/data/"

# ---------- 4. 校验 ----------
PKG="$ASSETS/playwright-driver/package/package.json"
if [ -f "$PKG" ]; then
    V=$(grep -o '"version"[^,]*' "$PKG" | head -1 | grep -o '[0-9][0-9.]*')
    if [ "$V" = "$EXPECT_DRIVER" ]; then
        ok "驱动版本 $V ✓"
    else
        echo -e "\033[33m[build] 警告:\033[0m 驱动版本 $V ≠ 期望 $EXPECT_DRIVER"
        echo "  浏览器可能起不来，报错是 Executable doesn't exist"
    fi
fi
[ -d "$ASSETS/browsers/chromium-$EXPECT_CHROMIUM" ] \
    && ok "Chromium revision $EXPECT_CHROMIUM ✓" \
    || echo -e "\033[33m[build] 警告:\033[0m 缺少 chromium-$EXPECT_CHROMIUM"

info "总大小: $(du -sh "$ASSETS" | cut -f1)"
ok ""
ok "资源就绪。接下来："
ok "  cd $HERE && docker compose build"
ok "  docker compose up -d"
