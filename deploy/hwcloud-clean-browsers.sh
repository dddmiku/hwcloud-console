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
        # Playwright 每次任务会新建临时 profile（/tmp/playwright_chromiumdev_profile-XXXX）。
        # 2026-11-11 实测：batch daily 结束后留下一个这样的 Chrome + node driver，
        # 存活 7 小时没被清 —— 因为上面的路径白名单不含 /tmp。
        # 这类 profile 只可能是本项目启动的（服务器上没有其它 Playwright 使用者）。
        *"/tmp/playwright_chromiumdev_profile-"*) ;;
        *) continue ;;
    esac

    # 只处理主进程（子进程没有 --user-data-dir，会自动跟着退出）
    case "$cmdline" in
        *"--user-data-dir"*) ;;
        *) continue ;;
    esac

    # 存活时长。
    #
    # ⚠ 不能用 `stat -c %Y /proc/$pid`：那是目录 inode 的 mtime，
    # 进程启动后只要有子进程 fork / 文件被访问就会刷新。
    # 2026-11-11 实测一个跑了 7 小时的 Chrome 被算成 age=20s，
    # 于是永远达不到 MIN_AGE 阈值，残留清理不掉。
    #
    # 正确做法：/proc/<pid>/stat 第 22 字段 starttime（开机后的 clock ticks）
    # 加上 /proc/uptime 换算成秒。
    start_ticks=$(awk '{print $22}' "/proc/$pid/stat" 2>/dev/null || echo "")
    up_secs=$(awk '{printf "%d", $1}' /proc/uptime 2>/dev/null || echo 0)
    clk_tck=$(getconf CLK_TCK 2>/dev/null || echo 100)
    if [ -n "$start_ticks" ] && [ "$clk_tck" -gt 0 ]; then
        start_secs=$(( start_ticks / clk_tck ))
        age=$(( up_secs - start_secs ))
    else
        # 兜底：退回旧方法（可能不准，但比不判强）
        start=$(stat -c %Y "/proc/$pid" 2>/dev/null || echo "$now")
        age=$((now - start))
    fi
    [ "$age" -lt 0 ] && age=0
    if [ "$age" -lt "$MIN_AGE" ]; then
        continue
    fi

    # 父进程是当前面板？那是正常任务，跳过
    ppid=$(awk '{print $4}' "/proc/$pid/stat" 2>/dev/null || echo 0)
    if [ "$ppid" = "$PANEL_PID" ]; then
        continue
    fi

    # 父进程还活着且是 node（playwright driver）→ 通常是任务在跑，跳过。
    #
    # ⚠ 但对 /tmp/playwright_chromiumdev_profile-* 不能这样判：
    # 2026-11-11 实测残留了一个 node driver + Chrome 组合，存活 7 小时。
    # driver 进程一直在（Playwright 不会自动退出 driver），所以「父进程活着」
    # 这条永远成立，残留永远不会被清。这类临时 profile 是每次任务新建的，
    # 单账号任务最长也就 10 分钟出头；超过 30 分钟还在 = 一定是残留。
    is_tmp_profile=0
    case "$cmdline" in
        *"/tmp/playwright_chromiumdev_profile-"*) is_tmp_profile=1 ;;
    esac
    if [ "$is_tmp_profile" = "1" ]; then
        if [ "$age" -lt 1800 ]; then
            continue   # 30 分钟内的临时 profile 一律不动
        fi
        # 超过 30 分钟：直接进入清理流程，不再看父进程
    elif [ -n "$ppid" ] && [ "$ppid" != "0" ] && [ "$ppid" != "1" ] && kill -0 "$ppid" 2>/dev/null; then
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
