#!/usr/bin/env bash
# hwcloud 每日任务：对每个账号执行 签到 + DevSpace 行为上报。
#
# 设计要点（都是踩过的坑）：
#   1. 多账号：遍历 accounts/*/，通过 HWCLOUD_ACCOUNT 切换 state 目录。
#      早先版本只跑默认账号，第二个账号从来没执行过。
#   2. 会话兜底：sign/devspace 内部会走 ensureSession，但纯协议救不回来时
#      会直接失败。所以先跑一次 keepalive --auto-login 把会话拉起来，
#      失败就跳过该账号并留标记，不要让后面的任务刷一堆 401。
#   3. DISPLAY：DevSpace 之类可能拉起浏览器，服务器有 Xvfb :99。
#   4. 不发文章：避免每天往公开博客刷内容，只报告是否已发。
#
# 注意这里**故意不加 set -e**：
#   脚本是「逐账号容错」的 —— 某个账号离线、某个任务失败，只跳过该账号
#   并记录，其余账号继续跑。加了 -e 会让第一个失败就中止整轮，
#   后面所有账号当天都白等。用 -u（未定义变量报错）+ pipefail（管道
#   错误不吞）就够了，真正的失败靠显式 if/exit code 处理。
set -uo pipefail

export HWCLOUD_HOME=/opt/hwcloud-demo
export PLAYWRIGHT_DRIVER_PATH=/opt/hwcloud-demo/playwright-driver
export PLAYWRIGHT_BROWSERS_PATH=/opt/hwcloud-demo/browsers
export DISPLAY=:99
export PATH=/opt/hwcloud-demo/go/bin:$PATH

BIN=/opt/hwcloud-demo/go/bin/hwcloud
LOG=/opt/hwcloud-demo/logs/daily.log
mkdir -p /opt/hwcloud-demo/logs
log() { echo "[$(date '+%Y-%m-%d %H:%M:%S')] $*" >> "$LOG"; }

if [ ! -x "$BIN" ]; then
    log "FATAL: $BIN 不存在或不可执行"
    exit 2
fi

log "=== 每日任务开始 ==="
overall=0

for d in /opt/hwcloud-demo/accounts/*/; do
    acc=$(basename "$d")
    # 没有 cookies.json 的是未完成登录的残留目录
    if [ ! -f "$d/state/cookies.json" ]; then
        log "[$acc] 跳过（无 cookies.json，未完成登录）"
        continue
    fi

    # 先把会话拉起来（离线会自动账密登录）
    HWCLOUD_ACCOUNT="$acc" "$BIN" keepalive --auto-login >/tmp/ka.$$ 2>&1
    ka_rc=$?
    if [ $ka_rc -ne 0 ]; then
        log "[$acc] 会话不可用（keepalive rc=$ka_rc），跳过: $(tr '\n' ' ' < /tmp/ka.$$ | head -c 300)"
        echo "$(date '+%Y-%m-%d %H:%M:%S') 会话无法自动恢复，需要重新登录" \
            > "$d/state/NEED_MANUAL_LOGIN"
        overall=1
        rm -f /tmp/ka.$$
        continue
    fi
    rm -f /tmp/ka.$$ "$d/state/NEED_MANUAL_LOGIN"

    out=$(HWCLOUD_ACCOUNT="$acc" "$BIN" sign 2>&1); log "[$acc] sign: $(echo "$out" | head -3 | tr '\n' ' ')"
    out=$(HWCLOUD_ACCOUNT="$acc" "$BIN" devspace 2>&1); log "[$acc] devspace: $(echo "$out" | tr '\n' ' ' | head -c 200)"

    # 文章只检查不发布
    today=$(date '+%Y-%m-%d')
    if HWCLOUD_ACCOUNT="$acc" "$BIN" blogs 2>&1 | grep -q "$today"; then
        log "[$acc] blog: 今日已发布"
    else
        log "[$acc] blog: 今日未发布（自动发布已禁用）"
    fi
done

log "=== 每日任务结束 (overall=$overall) ==="
tail -n 500 "$LOG" > "$LOG.tmp" && mv "$LOG.tmp" "$LOG"
exit $overall
