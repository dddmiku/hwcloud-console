#!/usr/bin/env bash
#
# 安装 Camoufox 浏览器（华为云自动登录用）—— 可选步骤。
#
# 什么时候需要：
#   华为云会话失效时，面板需要「自动账密登录」把会话拉起来。
#   这个功能要过网易易盾的滑块，必须用 Camoufox（定制 Firefox）。
#   不装的话面板照常可用，只是会话失效时需要手动登录一次。
#
# 为什么单独装：
#   浏览器本体约 2.4 GB。塞进镜像会让镜像从 700 MB 涨到 3 GB，
#   而多数人根本用不上这个功能。所以做成按需安装。
#
# 用法（在宿主机上执行，需要容器已经跑起来）：
#   ./install-camoufox.sh
#
# 装完之后重启容器，entrypoint 会自动识别并拉起登录服务：
#   docker compose restart

set -euo pipefail

CONTAINER="${CONTAINER:-hwcloud}"
PYPI="${PYPI:-https://pypi.tuna.tsinghua.edu.cn/simple}"

# 版本锁定：与已验证可用的环境一致。
# 改动前请确认 Camoufox 和 Playwright 的兼容性。
CAMOUFOX_VER="0.5.8"
PLAYWRIGHT_VER="1.62.0"

info() { echo -e "\033[36m[install-camoufox]\033[0m $*"; }
ok()   { echo -e "\033[32m[install-camoufox]\033[0m $*"; }
die()  { echo -e "\033[31m[install-camoufox] 错误:\033[0m $*" >&2; exit 1; }

docker ps --filter "name=^${CONTAINER}$" --format '{{.Names}}' | grep -q . \
    || die "容器 $CONTAINER 没在运行。先 docker compose up -d"

info "在容器内安装 Python 包（camoufox $CAMOUFOX_VER / playwright $PLAYWRIGHT_VER）…"
docker exec "$CONTAINER" /opt/hwcloud-demo/venv/bin/pip install --no-cache-dir \
    -i "$PYPI" \
    "camoufox[geoip]==${CAMOUFOX_VER}" "playwright==${PLAYWRIGHT_VER}"

info "下载 Camoufox 浏览器（约 2.4 GB，走镜像加速）…"
info "  下载到容器的 /opt/hwcloud-demo/camoufox/，该目录映射到宿主机"
info "  data-runtime/camoufox/，镜像升级不会冲掉。"

# camoufox 自带下载器直连 github.com，国内常卡在 302。
# 优先用 gh-proxy 镜像自己下，失败再回退官方源。
CB_VER="156.0.1-beta.36"
CB_ZIP="camoufox-${CB_VER}-lin.x86_64.zip"
CB_URL="https://github.com/daijro/camoufox/releases/download/v${CB_VER}/${CB_ZIP}"

docker exec "$CONTAINER" bash -c "
set -e
DEST=/opt/hwcloud-demo/camoufox
DL=\$DEST/dl
mkdir -p \"\$DL\"

try() {
    echo \"  尝试: \$1\"
    curl -fL --retry 2 --connect-timeout 20 -m 3600 -o \"\$DL/$CB_ZIP\" \"\$1\" && \
        [ -s \"\$DL/$CB_ZIP\" ]
}

ok_dl=0
for u in \
    'https://gh-proxy.com/$CB_URL' \
    'https://ghfast.top/$CB_URL' \
    '$CB_URL'
do
    if try \"\$u\"; then ok_dl=1; break; fi
done

if [ \"\$ok_dl\" = '0' ]; then
    echo '所有下载源都失败，回退到 camoufox 自带下载器…'
    /opt/hwcloud-demo/venv/bin/python -m camoufox fetch || exit 1
else
    echo \"  下载完成 (\$(du -h \"\$DL/$CB_ZIP\" | cut -f1))，解压…\"
    TARGET=\"\$DEST/browsers/official/${CB_VER}-72637885\"
    mkdir -p \"\$TARGET\"
    unzip -q -o \"\$DL/$CB_ZIP\" -d \"\$TARGET\"
    printf '{\"version\":\"156.0.1\",\"build\":\"beta.36\"}\n' > \"\$TARGET/version.json\"
    printf '{\"active_version\":\"browsers/official/${CB_VER}-72637885\"}\n' > \"\$DEST/config.json\"
    rm -f \"\$DL/$CB_ZIP\"
fi
"

ok ""
ok "安装完成。重启容器让 entrypoint 拉起登录服务："
ok "  docker compose restart"
ok ""
ok "验证："
ok "  docker exec $CONTAINER curl -fsS http://127.0.0.1:8901/health"
