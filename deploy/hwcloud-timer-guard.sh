#!/usr/bin/env bash
# hwcloud-timer-guard.sh —— 定时器自愈。
#
# 背景（2026-11-11 实测）：hwcloud-daily.timer 在 11-09 停摆了两天没人发现。
# 原因是我为了清理卡死的 daily 任务执行了 `systemctl stop hwcloud-daily.service`，
# 而 service 上配了 `Conflicts=`，systemd 把关联的 timer 一起停掉了，
# 之后忘记重启。表现是「定时任务悄无声息地不再触发」，日志里连失败都没有
# —— 因为根本没启动。
#
# 这个脚本每 10 分钟检查一次两个业务 timer 是否 active，不 active 就拉起来，
# 并记一行日志。它只负责「timer 是否在跑」，不关心 service 执行结果
# （service 失败有各自的日志和面板）。
#
# 为什么不用 systemd 的 Restart=：
#   Restart= 作用于 service，而这里失效的是 **timer**。
#   timer 被 stop 之后 systemd 不会自动拉起，只能靠外部守护。
#
# 为什么不直接用 `systemctl start` 不加判断：
#   每秒都 start 会刷 systemd journal；先判 is-active 只在真异常时动作。
set -uo pipefail

TIMERS="hwcloud-daily.timer hwcloud-weekly.timer"
LOG=/opt/hwcloud-demo/logs/timer-guard.log
mkdir -p "$(dirname "$LOG")"

# 日志只保留最近 500 行，避免无限增长
if [ -f "$LOG" ] && [ "$(wc -l < "$LOG")" -gt 600 ]; then
    tail -n 500 "$LOG" > "$LOG.tmp" && mv "$LOG.tmp" "$LOG"
fi

log() { echo "[$(date '+%Y-%m-%d %H:%M:%S')] $*" >> "$LOG"; }

fixed=0
for t in $TIMERS; do
    # is-enabled 只看开机自启，is-active 才看当前是否在跑。
    # 两个都要：被 disable 的 timer 重启机器后也不会起来。
    enabled=$(systemctl is-enabled "$t" 2>/dev/null || echo unknown)
    active=$(systemctl is-active "$t" 2>/dev/null || echo unknown)

    if [ "$enabled" != "enabled" ]; then
        if systemctl enable "$t" >/dev/null 2>&1; then
            log "$t 未启用（$enabled），已 enable"
            fixed=$((fixed+1))
        else
            log "$t enable 失败（当前 $enabled）"
        fi
    fi

    if [ "$active" != "active" ]; then
        if systemctl start "$t" >/dev/null 2>&1; then
            log "$t 未运行（$active），已 start"
            fixed=$((fixed+1))
        else
            log "$t start 失败（当前 $active）"
        fi
    fi
done

# 没有异常就不写日志，避免每 10 分钟刷一条
if [ "$fixed" -gt 0 ]; then
    log "本轮修复 $fixed 项，当前状态："
    for t in $TIMERS; do
        log "  $t: $(systemctl is-enabled "$t" 2>/dev/null)/$(systemctl is-active "$t" 2>/dev/null)"
    done
fi

exit 0
