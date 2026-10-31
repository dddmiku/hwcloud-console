#!/usr/bin/env bash

# 注意这里**故意不加 set -e**：
#   脚本是「逐账号/逐步骤容错」的 —— 单个账号或单个任务失败只记录并跳过，
#   其余继续。加 -e 会让第一个失败就中止整轮，后面全白等。
#   真正的失败靠显式 if / exit code 处理，不用 -e 兜。
# hwcloud 每周任务：实践一个实验 + 给代码仓提 Issue。
#
# 设计要点：
#   1. 多账号遍历（同 daily）。
#   2. 每步先查任务是否已完成 —— 云实验每次开始都消耗一个「今日免费名额」，
#      重复跑纯属浪费；提 Issue 是公开动作，已达标就不该再发。
#   3. 会话兜底：先 keepalive --auto-login，失败就跳过该账号。
#   4. DISPLAY：GitCode 提 Issue 需要浏览器（无头会被 302 回列表）。
set -uo pipefail

export HWCLOUD_HOME=/opt/hwcloud-demo
export PLAYWRIGHT_DRIVER_PATH=/opt/hwcloud-demo/playwright-driver
export PLAYWRIGHT_BROWSERS_PATH=/opt/hwcloud-demo/browsers
export DISPLAY=:99
export PATH=/opt/hwcloud-demo/go/bin:$PATH

BIN=/opt/hwcloud-demo/go/bin/hwcloud
LOG=/opt/hwcloud-demo/logs/weekly.log
mkdir -p /opt/hwcloud-demo/logs
log() { echo "[$(date '+%Y-%m-%d %H:%M:%S')] $*" >> "$LOG"; }

if [ ! -x "$BIN" ]; then
    log "FATAL: $BIN 不存在或不可执行"
    exit 2
fi

log "=== 每周任务开始 ==="
overall=0

for d in /opt/hwcloud-demo/accounts/*/; do
    acc=$(basename "$d")
    if [ ! -f "$d/state/cookies.json" ]; then
        log "[$acc] 跳过（无 cookies.json，未完成登录）"
        continue
    fi

    HWCLOUD_ACCOUNT="$acc" "$BIN" keepalive --auto-login >/tmp/kaw.$$ 2>&1
    if [ $? -ne 0 ]; then
        log "[$acc] 会话不可用，跳过: $(tr '\n' ' ' < /tmp/kaw.$$ | head -c 300)"
        echo "$(date '+%Y-%m-%d %H:%M:%S') 会话无法自动恢复，需要重新登录" \
            > "$d/state/NEED_MANUAL_LOGIN"
        overall=1
        rm -f /tmp/kaw.$$
        continue
    fi
    rm -f /tmp/kaw.$$ "$d/state/NEED_MANUAL_LOGIN"

    # 1. 云实验（已完成则跳过，省一次免费名额）
    tasks_out=$(HWCLOUD_ACCOUNT="$acc" "$BIN" tasks --stars 1,2,3,4,5 2>&1)
    if echo "$tasks_out" | grep "实验" | grep -q "已完成"; then
        log "[$acc] lab: 本周已完成，跳过"
    else
        log "[$acc] lab: 开始执行云实验"
        out=$(HWCLOUD_ACCOUNT="$acc" "$BIN" lab --headless 2>&1)
        log "[$acc] lab: $(echo "$out" | tr '\n' ' ' | head -c 400)"
    fi

    # 2. 提 Issue（已完成则跳过，避免刷重复内容）
    if echo "$tasks_out" | grep "Issue" | grep -q "已完成"; then
        log "[$acc] issue: 本周已完成，跳过"
    elif [ ! -f "$d/state/gitcode_state.json" ]; then
        log "[$acc] issue: 无 GitCode 会话，跳过"
    else
        out=$(HWCLOUD_ACCOUNT="$acc" "$BIN" issue 2>&1)
        log "[$acc] issue: $(echo "$out" | tr '\n' ' ' | head -c 400)"
    fi
done

log "=== 每周任务结束 (overall=$overall) ==="
tail -n 500 "$LOG" > "$LOG.tmp" && mv "$LOG.tmp" "$LOG"
exit $overall
