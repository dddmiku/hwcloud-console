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

# 账号顺序随机化 + 每账号随机延时，见 hwcloud-lib.sh。
# 周常比日常更重（要开浏览器报名赛事），延时的上限也放宽一些。
for _lib in "$(dirname "$0")/hwcloud-lib.sh" \
            /usr/local/bin/hwcloud-lib.sh \
            /opt/hwcloud-demo/deploy/hwcloud-lib.sh; do
    [ -f "$_lib" ] && { . "$_lib"; break; }
done
if ! command -v random_accounts >/dev/null 2>&1; then
    log "WARN: 找不到 hwcloud-lib.sh，退回固定顺序（无随机延时）"
    random_accounts() { for d in /opt/hwcloud-demo/accounts/*/; do
        [ -f "$d/state/cookies.json" ] && basename "$d"; done; }
    random_delay() { :; }
fi

for acc in $(random_accounts); do
    d="/opt/hwcloud-demo/accounts/$acc"
    random_delay "$acc" 240

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

    # 一键周常：云实验 + 提Issue + Star仓库 + 开发者活动 + 开发者赛事 + 鸿蒙云端实践
    #
    # 用 `weekly` 子命令而不是逐个 `lab` / `issue` 拼：
    # 旧脚本只跑这两步，后来在 runWeekly 里加的 Star仓库 / 活动 / 赛事 /
    # 鸿蒙云端实践全都没执行 —— 面板上看着「周常跑了」，实际大部分任务没做。
    # 统一调 `weekly` 后，加任务只需要改 runWeekly 一处。
    #
    # `2>/dev/null` 而不是 `2>&1`：svcLog 写 stderr，stdout 只有结果 JSON。
    out=$(HWCLOUD_ACCOUNT="$acc" "$BIN" weekly 2>/dev/null)
    rc=$?
    summary=$(echo "$out" | python3 -c '
import json,sys
try:
    d=json.load(sys.stdin)
except Exception:
    print("(输出非 JSON)"); raise SystemExit
print(d.get("msg",""))
for s in d.get("steps",[]):
    if not s.get("ok"):
        print("  ✗ %s: %s" % (s.get("name"), s.get("msg","")[:80]))
' 2>/dev/null)
    log "[$acc] weekly(rc=$rc): $(echo "$summary" | tr '\n' ' ' | head -c 500)"
    if [ $rc -ne 0 ]; then
        overall=1
    fi
done

log "=== 每周任务结束 (overall=$overall) ==="
tail -n 500 "$LOG" > "$LOG.tmp" && mv "$LOG.tmp" "$LOG"
exit $overall
