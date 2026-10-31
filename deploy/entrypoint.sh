#!/usr/bin/env bash
#
# 容器入口：Xvfb → （可选）Camoufox 服务 → 主进程
#
# 设计原则：**启动必须快，不能被可选组件拖住**。
# 面板本身只依赖 Xvfb 和 Go 二进制，两者都在镜像里，秒级就绪。
# Camoufox 是可选组件（只有华为云自动登录需要），它没装也不该影响面板。
#
# 为什么需要 Xvfb：
#   GitCode 对无头浏览器做了行为判定 —— headless=True 时访问
#   /issues/create 会被 302 回列表，表现成「会话失效」，很难排查。
#   所以 Star / 提 Issue / 活动报名都必须跑有头浏览器。
#
# 关于 Camoufox：
#   它是华为云自动登录用的定制 Firefox，浏览器包约 2.4 GB。
#   **不打进镜像，也不在启动时自动下载** —— 由用户按需自行安装
#   （见 README「华为云自动登录」一节）。原因：
#     - 2.4 GB 会让镜像从 700 MB 涨到 3 GB，拉取时间翻几倍
#     - 多数人不需要自动登录（会话能活 29 天，手动登一次就够）
#     - 自动下载依赖 GitHub，国内网络经常卡住
#   如果用户已经装了（挂载到 CAMOUFOX_HOME），这里会自动拉起服务。

set -uo pipefail

DISPLAY_NUM="${DISPLAY_NUM:-99}"
SCREEN="${XVFB_SCREEN:-1440x1000x24}"
CAMOUFOX_HOME="${CAMOUFOX_HOME:-/opt/hwcloud-demo/camoufox}"
CAMOUFOX_PORT="${HWLOGIN_PORT:-8901}"
LOG_DIR="${HWCLOUD_HOME:-/opt/hwcloud-demo}/logs"
mkdir -p "$LOG_DIR"

log() { echo "[entrypoint] $*"; }

# ---------- 1. 虚拟显示 ----------
rm -f "/tmp/.X${DISPLAY_NUM}-lock" "/tmp/.X11-unix/X${DISPLAY_NUM}" 2>/dev/null
Xvfb ":${DISPLAY_NUM}" -screen 0 "${SCREEN}" -nolisten tcp >"$LOG_DIR/xvfb.log" 2>&1 &
XVFB_PID=$!

for _ in $(seq 1 50); do
    [ -e "/tmp/.X11-unix/X${DISPLAY_NUM}" ] && break
    sleep 0.1
done

if ! kill -0 "$XVFB_PID" 2>/dev/null; then
    echo "[entrypoint] Xvfb 启动失败：" >&2
    cat "$LOG_DIR/xvfb.log" >&2
    exit 1
fi
export DISPLAY=":${DISPLAY_NUM}"
log "Xvfb 就绪 (DISPLAY=:${DISPLAY_NUM}, ${SCREEN})"

# ---------- 2. Camoufox 登录服务 ----------
#
# 浏览器现在**打进镜像**了，装在 /opt/hwcloud-demo/camoufox-baked。
# 运行时用的是挂载卷 $CAMOUFOX_HOME（默认 /opt/hwcloud-demo/camoufox），
# 这样用户下载/更新浏览器不会因为换镜像而丢失。
#
# 首次启动：挂载卷是空的 -> 从 baked 复制一份过去（秒级，不用联网）。
# 之后启动：挂载卷已有 -> 直接用，镜像升级也不影响。
CAMOUFOX_PID=""
CAMOUFOX_BAKED="/opt/hwcloud-demo/camoufox-baked"

seed_camoufox() {
    # 已经装过就不动
    [ -d "$CAMOUFOX_HOME/browsers" ] && return 0
    # 镜像里没预装（老镜像）就跳过，用户可手动装
    [ -d "$CAMOUFOX_BAKED/browsers" ] || return 1

    log "首次启动：把镜像内置的 Camoufox 复制到数据卷（约 2.4 GB，稍等）…"
    mkdir -p "$CAMOUFOX_HOME"
    # cp -a 保留权限和符号链接；目标在挂载卷上，跨设备复制会慢一点但只需一次
    if cp -a "$CAMOUFOX_BAKED/." "$CAMOUFOX_HOME/" 2>/dev/null; then
        log "Camoufox 已就位: $(du -sh "$CAMOUFOX_HOME" 2>/dev/null | cut -f1)"
        return 0
    fi
    log "复制失败，回退到镜像内置路径"
    return 1
}

camoufox_installed() {
    # camoufox 把浏览器放在 $CAMOUFOX_HOME/browsers/<channel>/<version>/
    [ -d "$CAMOUFOX_HOME/browsers" ] || return 1
    # 目录非空且能找到可执行文件才算装好
    find "$CAMOUFOX_HOME/browsers" -maxdepth 4 -name "camoufox" -type f 2>/dev/null | head -1 | grep -q .
}

# 先尝试播种到数据卷；失败就直接用镜像内的路径
if ! seed_camoufox; then
    if [ -d "$CAMOUFOX_BAKED/browsers" ]; then
        export CAMOUFOX_HOME="$CAMOUFOX_BAKED"
        log "使用镜像内置的 Camoufox: $CAMOUFOX_HOME"
    fi
fi

if camoufox_installed; then
    export HWLOGIN_WORK="${HWLOGIN_WORK:-$CAMOUFOX_HOME/sessions}"
    export HWLOGIN_PORT="$CAMOUFOX_PORT"
    mkdir -p "$HWLOGIN_WORK"

    /opt/hwcloud-demo/venv/bin/python /opt/hwcloud-demo/camoufox_server.py \
        >"$LOG_DIR/camoufox.log" 2>&1 &
    CAMOUFOX_PID=$!

    # 最多等 30 秒；超时不阻断主进程启动
    ready=0
    for _ in $(seq 1 60); do
        if curl -fsS "http://127.0.0.1:${CAMOUFOX_PORT}/health" >/dev/null 2>&1; then
            ready=1
            break
        fi
        kill -0 "$CAMOUFOX_PID" 2>/dev/null || break
        sleep 0.5
    done

    if [ "$ready" = "1" ]; then
        log "Camoufox 登录服务就绪 (:$CAMOUFOX_PORT)"
    else
        log "Camoufox 登录服务未就绪，详见 logs/camoufox.log"
        log "  不影响面板和 GitCode 任务；华为云自动登录暂不可用"
        kill -TERM "$CAMOUFOX_PID" 2>/dev/null
        CAMOUFOX_PID=""
    fi
else
    log "未检测到 Camoufox 浏览器 —— 华为云自动登录不可用"
    log "  面板和 GitCode 任务（Star / 提 Issue / 活动报名）正常"
    log "  需要自动登录时见 README「华为云自动登录」一节"
fi

# ---------- 3. 可选 VNC ----------
if [ -n "${VNC_PASSWORD:-}" ]; then
    x11vnc -storepasswd "$VNC_PASSWORD" /tmp/.vncpass 2>/dev/null
    x11vnc -display ":${DISPLAY_NUM}" -rfbauth /tmp/.vncpass \
           -forever -shared -rfbport 5901 -noxdamage >"$LOG_DIR/x11vnc.log" 2>&1 &
    log "VNC 已启动在 :5901（人工过验证码用）"
fi

# ---------- 4. 主进程 ----------
#
# 不用 exec：exec 会用 Go 进程替换 bash，我们就收不到退出信号，
# Camoufox 和 Xvfb 会变成孤儿。改成前台跑 + 转发信号。
cleanup() {
    [ -n "$CAMOUFOX_PID" ] && kill -TERM "$CAMOUFOX_PID" 2>/dev/null
    kill -TERM "$XVFB_PID" 2>/dev/null
}

log "启动主进程: hwcloud $*"
/opt/hwcloud-demo/go/bin/hwcloud "$@" &
MAIN_PID=$!

trap 'kill -TERM "$MAIN_PID" 2>/dev/null' TERM INT

wait "$MAIN_PID"
RC=$?
cleanup
exit $RC
