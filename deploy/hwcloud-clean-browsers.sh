#!/usr/bin/env bash
# 清理残留的 Chromium / Playwright 进程。
#
# 为什么需要：
#   Playwright 启动的浏览器是独立进程树（chromium + zygote + renderer + gpu）。
#   Go 里虽然每个任务都有 defer br.Close()，但下列情况仍会残留：
#     - 面板进程被 systemctl restart（SIGTERM 后 10 秒 SIGKILL，defer 来不及跑完）
#     - 任务超时后被上层直接杀掉
#     - 浏览器自身卡死（如卡在验证码页），Close 请求挂住
#   实测一次 GitCode 登录卡住后残留了 12 个 chromium 进程。
#
# 判断依据：
#   1. 进程的 --user-data-dir 指向我们的 profile 目录
#   2. 该 profile 没有任何「活着的父级」在用（父进程不是当前面板）
#   3. 进程存在时间超过阈值（避免误杀正在启动的）
#
# 用法：hwcloud-clean-browsers.sh [--dry-run] [最小存活秒数]
set -uo pipefail

LOG=/opt/hwcloud-demo/logs/service.log
mkdir -p "$(dirname "$LOG")" 2>/dev/null
log() { echo "[$(date '+%Y-%m-%d %H:%M:%S')] [clean-browsers] $*" >> "$LOG"; }

DRY=0
MIN_AGE=600          # 默认只清理存活超过 10 分钟的
for arg in "$@"; do
    case "$arg" in
        --dry-run) DRY=1 ;;
        *) [[ "$arg" =~ ^[0-9]+$ ]] && MIN_AGE="$arg" ;;
    esac
done

PANEL_PID=$(systemctl show -p MainPID --value hwcloud-console 2>/dev/null || echo 0)
[ -z "$PANEL_PID" ] && PANEL_PID=0

now=$(date +%s)
killed=0
scanned=0

# 找出所有 chromium 主进程（带 --user-data-dir 的那个）
for pid in $(pgrep -f 'chrome-linux64/chrome' 2>/dev/null); do
    cmdline=$(tr '\0' ' ' < "/proc/$pid/cmdline" 2>/dev/null || echo "")
    [ -z "$cmdline" ] && continue
    scanned=$((scanned + 1))

    # 只处理我们自己的 profile
    case "$cmdline" in
        *"/opt/hwcloud-demo/accounts/"*"/state/gitcode_profile"*) ;;
        *"/opt/hwcloud-demo/accounts/"*"/state/browser_profile"*) ;;
        *"/opt/hwcloud-demo/state/"*) ;;
        *) continue ;;
    esac

    # 只处理主进程（子进程没有 --user-data-dir，会自动跟着退出）
    case "$cmdline" in
        *"--user-data-dir"*) ;;
        *) continue ;;
    esac

    # 存活时长
    start=$(stat -c %Y "/proc/$pid" 2>/dev/null || echo "$now")
    age=$((now - start))
    if [ "$age" -lt "$MIN_AGE" ]; then
        continue
    fi

    # 父进程是当前面板？那是正常任务，跳过
    ppid=$(awk '{print $4}' "/proc/$pid/stat" 2>/dev/null || echo 0)
    if [ "$ppid" = "$PANEL_PID" ]; then
        continue
    fi
    # 父进程还活着且是 node（playwright driver）→ 任务在跑，跳过
    if [ -n "$ppid" ] && [ "$ppid" != "0" ] && [ "$ppid" != "1" ] && kill -0 "$ppid" 2>/dev/null; then
        pcmd=$(tr '\0' ' ' < "/proc/$ppid/cmdline" 2>/dev/null || echo "")
        case "$pcmd" in
            *node*|*playwright*) continue ;;
        esac
    fi

    if [ "$DRY" = "1" ]; then
        log "[dry-run] 将清理 pid=$pid age=${age}s profile=$(echo "$cmdline" | grep -o 'user-data-dir=[^ ]*' | head -1)"
    else
        kill -TERM "$pid" 2>/dev/null
        sleep 1
        kill -KILL "$pid" 2>/dev/null
        log "已清理 pid=$pid age=${age}s"
    fi
    killed=$((killed + 1))
done

if [ "$killed" -gt 0 ]; then
    log "本轮清理 $killed 个残留浏览器（扫描 $scanned 个 chrome 进程）"
fi

exit 0
