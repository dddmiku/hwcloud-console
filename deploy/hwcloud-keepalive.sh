#!/usr/bin/env bash

# 注意这里**故意不加 set -e**：
#   脚本是「逐账号/逐步骤容错」的 —— 单个账号或单个任务失败只记录并跳过，
#   其余继续。加 -e 会让第一个失败就中止整轮，后面全白等。
#   真正的失败靠显式 if / exit code 处理，不用 -e 兜。
# 会话保活：每小时跑一次，让 SSO 用 refresh token 重建 ECO_CN_ID。
#
# 背景：ECO_CN_ID（.huaweicloud.com 的会话 cookie）只活 1 小时，过期后
# devdata 的所有接口都返回 404。但它可以纯协议重建——访问 grow 页就会
# 被 SSO 重新签发，不需要浏览器、不触发验证码。
#
# 失败时写一个显式标记文件，方便一眼看出「需要人工在本机重新登录」。
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
