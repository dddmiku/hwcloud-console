#!/usr/bin/env bash

# 注意这里**故意不加 set -e**：
#   脚本是「逐账号/逐步骤容错」的 —— 单个账号或单个任务失败只记录并跳过，
#   其余继续。加 -e 会让第一个失败就中止整轮，后面全白等。
#   真正的失败靠显式 if / exit code 处理，不用 -e 兜。
# 会话保活（**已默认停用**，仅在明确需要时手动启用）
#
# ⚠️ 2026-11-08 起 hwcloud-keepalive.timer 默认 **disabled**。
#
# 为什么停用：这个定时器每 20 分钟跑一次 keepalive，以前 CLI 的
# --auto-login 默认是 true，于是会话一失效就会拉 Camoufox 有头浏览器
# 登录 —— 一天几十次，出口 IP 很快被华为云/GitCode 限流
# （实测 2026-11-08 展览馆 eui-login 直接 429「登录尝试过于频繁」）。
#
# 现在的原则：**按需登录**。
#   - 用户点任务 → 面板 ensureAlive 检查，离线才登录一次
#   - 用户点「保活」按钮 → 才跑 keepalive
#   - 后台不再有定时保活
#
# 纯协议续期（不拉浏览器）其实很便宜，需要恢复的话：
#   systemctl enable --now hwcloud-keepalive.timer
# 注意此时请确认 CLI 用 `keepalive`（不带 --auto-login），不要自动登录。
set -uo pipefail

export HWCLOUD_HOME=/opt/hwcloud-demo
export PLAYWRIGHT_DRIVER_PATH=/opt/hwcloud-demo/playwright-driver
export PLAYWRIGHT_BROWSERS_PATH=/opt/hwcloud-demo/browsers

LOG=/opt/hwcloud-demo/logs/keepalive.log
MARK=/opt/hwcloud-demo/state/NEED_MANUAL_LOGIN
mkdir -p /opt/hwcloud-demo/logs
log() { echo "[$(date '+%Y-%m-%d %H:%M:%S')] $*" >> "$LOG"; }

out=$(hwcloud keepalive 2>&1)
rc=$?
log "rc=$rc $out"

if [ $rc -eq 0 ]; then
    rm -f "$MARK"
else
    # 纯协议救不回来，需要人工在本机登录一次再同步 state
    echo "$(date '+%Y-%m-%d %H:%M:%S') 会话无法自动恢复，需要在本机执行 hwcloud login 后同步 state 目录" > "$MARK"
    log "FAILED -> 已写入 $MARK"
fi

# 日志只留最近 2000 行
if [ -f "$LOG" ]; then
    tail -n 2000 "$LOG" > "$LOG.tmp" && mv "$LOG.tmp" "$LOG"
fi

exit $rc
