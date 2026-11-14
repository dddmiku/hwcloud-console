#!/usr/bin/env bash
# hwcloud 每日任务：对每个账号执行完整的一键日常。
#
# 设计要点（都是踩过的坑）：
#   1. 多账号：遍历 accounts/*/，通过 HWCLOUD_ACCOUNT 切换 state 目录。
#      早先版本只跑默认账号，第二个账号从来没执行过。
#   2. 会话兜底：先跑一次 keepalive --auto-login 把会话拉起来，
#      失败就跳过该账号并留标记，不要让后面的任务刷一堆 401。
#   3. DISPLAY：DevSpace 之类会拉起浏览器，服务器有 Xvfb :99。
#   4. 用 `hwcloud daily` 而不是逐个命令拼。
#
# 第 4 点是最重要的修正：
#   早先这里是 `sign` + `devspace` + 查文章状态 三条命令拼起来的，
#   后来在 runDaily 里加了「鸿蒙浏览 / 鸿蒙 Star」两个任务，
#   但脚本没跟着改 —— 结果定时任务天天「成功」，面板上的每日任务
#   却一直没做（实测某个账号的鸿蒙浏览、鸿蒙Star 长期未完成）。
#   现在统一调 `hwcloud daily`，和面板上那个「一键日常」走同一份逻辑，
#   以后加任务只需要改 runDaily 一处。
#
#   顺带说明「自动发布已禁用」：那是旧脚本里写死的
#   `blog: 今日未发布（自动发布已禁用）` 提示 —— 旧版本故意不自动发文章，
#   避免每天往公开博客刷内容。现在改用 runDaily，会正常发文章。
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

# 账号顺序随机化 + 每账号随机延时。
# 原因见 hwcloud-lib.sh：固定顺序是明显的机器特征，
# 同一秒四个账号齐发也是。手动运行（HWCLOUD_MANUAL=1）不加延时。
#
# 找 lib 的顺序：脚本同目录 → /usr/local/bin → deploy 目录。
# 为什么不止同目录：systemd 执行的是 /usr/local/bin/hwcloud-daily.sh，
# 而开发时改的是 /opt/hwcloud-demo/deploy/ 下的副本，两边都要能找到。
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
    random_delay "$acc"

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

    # 一键日常：签到 + DevSpace + 发文章 + 鸿蒙浏览 + 鸿蒙 Star
    #
    # 用 `daily` 子命令而不是逐个 `sign` / `devspace` 拼：
    # 拼命令会和面板的 runDaily 逐渐脱节（鸿蒙那两个任务就是这么漏掉的）。
    #
    # 注意这里是 `2>/dev/null` 不是 `2>&1`：
    # svcLog 写的是 stderr（进度日志），stdout 只有结果 JSON。
    # 合并的话 JSON 会被日志行冲散，下面 json.load 必然失败 ——
    # 实测日志里全是「(输出非 JSON)」。
    # 单账号限时 20 分钟。
    #
    # 为什么必须限时：2026-11-14 实测某账号的鸿蒙 Star 卡了 47 分钟，
    # 把整个 daily 窗口（systemd TimeoutStartSec=3600）耗尽，systemd 触发
    # "start operation timed out" 直接 TERM 掉主进程，后面两个账号根本没跑。
    # 一个账号卡死不该让整批失败 —— 单独限时，超时就把这个号标记失败继续下一个。
    #
    # 正常单号 2-8 分钟，20 分钟足够；超时说明真的卡了，标记后由下次补跑。
    out=$(HWCLOUD_ACCOUNT="$acc" timeout 1200 "$BIN" daily 2>/dev/null)
    rc=$?
    if [ $rc -eq 124 ]; then
        log "[$acc] daily 超时（20 分钟），跳过该账号继续下一个"
        overall=1
        continue
    fi
    # 结果是一大坨 JSON，只取 msg 和失败项，避免日志被撑爆
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
    log "[$acc] daily(rc=$rc): $(echo "$summary" | tr '\n' ' ' | head -c 400)"
    if [ $rc -ne 0 ]; then
        overall=1
    fi
done

log "=== 每日任务结束 (overall=$overall) ==="
tail -n 500 "$LOG" > "$LOG.tmp" && mv "$LOG.tmp" "$LOG"
exit $overall
