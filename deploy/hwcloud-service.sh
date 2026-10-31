#!/usr/bin/env bash

# 注意这里**故意不加 set -e**：
#   脚本是「逐账号/逐步骤容错」的 —— 单个账号或单个任务失败只记录并跳过，
#   其余继续。加 -e 会让第一个失败就中止整轮，后面全白等。
#   真正的失败靠显式 if / exit code 处理，不用 -e 兜。
# hwcloud 独立服务：在服务器上常驻运行，不依赖任何 PC 或浏览器。
#
# 设计前提（实测结论）：
#   - 华为云登录需要 826 字符的加密浏览器指纹 fp，纯 HTTP 造不出来，
#     所以「自动重新登录」不可能实现，登录只能人工做一次。
#   - 但登录后的会话能活 29 天以上（refresh token 约 6 个月），
#     所以只要维持住会话，服务就能长期无人值守。
#   - 唯一会失效的是 ECO_CN_ID（1 小时），它在有效期内可用，
#     过期后无法纯协议重建——这就是本脚本要盯的指标。
#
# 用法：
#   hwcloud-service.sh daemon    # 常驻，每 10 分钟检查一次
#   hwcloud-service.sh once      # 只检查一次（给 systemd timer 用）
#   hwcloud-service.sh status    # 看当前状态
set -uo pipefail

HWCLOUD_HOME="${HWCLOUD_HOME:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
export HWCLOUD_HOME
export PLAYWRIGHT_DRIVER_PATH="${PLAYWRIGHT_DRIVER_PATH:-$HWCLOUD_HOME/playwright-driver}"
export PLAYWRIGHT_BROWSERS_PATH="${PLAYWRIGHT_BROWSERS_PATH:-$HWCLOUD_HOME/browsers}"

BIN="${HWCLOUD_BIN:-$HWCLOUD_HOME/go/bin/hwcloud}"
LOG="${HWCLOUD_LOG:-$HWCLOUD_HOME/logs/service.log}"
STATE_DIR="$HWCLOUD_HOME/state"
MARK="$STATE_DIR/NEED_MANUAL_LOGIN"
INTERVAL="${HWCLOUD_INTERVAL:-600}"

mkdir -p "$(dirname "$LOG")" "$STATE_DIR"
log() { echo "[$(date '+%Y-%m-%d %H:%M:%S')] $*" | tee -a "$LOG" >&2; }

# check_once：检查会话，失效时写标记。
# 返回 0=正常，1=需要人工登录，2=其他错误
check_once() {
    local out rc
    out=$("$BIN" session 2>&1)
    rc=$?

    if [ $rc -ne 0 ]; then
        log "session 命令失败: $out"
        return 2
    fi

    # 解析 alive 字段
    local alive
    alive=$(echo "$out" | grep -o '"alive": *[a-z]*' | grep -o 'true\|false' | head -1)

    if [ "$alive" = "true" ]; then
        rm -f "$MARK"
        local days hours
        days=$(echo "$out" | grep -o '"auth_remaining_days": *[0-9.]*' | grep -o '[0-9.]*' | head -1)
        hours=$(echo "$out" | grep -o '"shortest_remaining_hours": *[0-9.-]*' | grep -o '[-0-9.]*' | head -1)
        log "OK 会话有效 | 凭据剩余 ${days:-?} 天 | 最短 cookie ${hours:-?} 小时"
        return 0
    fi

    # 会话失效：先试一次 keepalive（纯协议 + 浏览器兜底）
    log "会话失效，尝试 keepalive..."
    out=$("$BIN" keepalive 2>&1)
    rc=$?
    if [ $rc -eq 0 ]; then
        log "keepalive 成功: $out"
        rm -f "$MARK"
        return 0
    fi

    # 救不回来
    log "keepalive 失败: $out"
    {
        echo "$(date '+%Y-%m-%d %H:%M:%S') 会话已失效且无法自动恢复"
        echo "需要在有浏览器的机器上执行一次登录，然后把 state/cookies.json 和 state/meta.json"
        echo "复制到本机的 $STATE_DIR/ 目录。"
    } > "$MARK"
    log "已写入 $MARK"
    return 1
}

# run_daily：跑每日任务。用当天日期做幂等标记，避免重启后重复跑。
run_daily() {
    local today stamp="$STATE_DIR/.daily-$(date '+%Y-%m-%d')"
    today=$(date '+%Y-%m-%d')

    if [ -f "$stamp" ]; then
        return 0
    fi

    log "--- 开始每日任务 ---"

    local out
    out=$("$BIN" sign 2>&1); log "sign: $out"
    out=$("$BIN" devspace 2>&1); log "devspace: $out"

    # 文章只检查不自动发（内容需要人工把关）
    if "$BIN" blogs 2>&1 | grep -q "$today"; then
        log "blog: 今日已发布"
    else
        log "blog: 今日未发布（不自动发，避免重复内容）"
    fi

    touch "$stamp"
    log "--- 每日任务结束 ---"

    # 清理 7 天前的标记
    find "$STATE_DIR" -name '.daily-*' -mtime +7 -delete 2>/dev/null || true
}

case "${1:-once}" in
    once)
        check_once
        exit $?
        ;;

    daemon)
        log "=== 服务启动 (间隔 ${INTERVAL}s) ==="
        trap 'log "收到退出信号"; exit 0' TERM INT
        while true; do
            if check_once; then
                # 会话正常时才跑任务
                run_daily
            fi
            sleep "$INTERVAL"
        done
        ;;

    status)
        "$BIN" session 2>&1
        echo
        if [ -f "$MARK" ]; then
            echo "!!! 需要人工登录 !!!"
            cat "$MARK"
        else
            echo "状态正常"
        fi
        echo
        echo "最近日志："
        tail -20 "$LOG" 2>/dev/null || echo "(无日志)"
        ;;

    *)
        echo "用法: $0 {daemon|once|status}"
        exit 1
        ;;
esac
