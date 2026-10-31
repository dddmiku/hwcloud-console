#!/usr/bin/env bash
# hwcloud 脚本公共函数：账号列表随机化 + 随机延时。
#
# 为什么需要（都是实测踩过的）：
#
# 1. **随机顺序**
#    原来用 `for d in accounts/*/` 遍历，顺序由 shell glob 决定 ——
#    字母序、每天完全一样。结果：
#      - 同一个账号永远第一个跑，永远占用最空闲的时段
#      - 同一个账号永远最后跑，永远吃到前面账号留下的限流
#      - 华为云的风控如果按「账号访问顺序」建模，固定顺序是明显特征
#    改成每轮随机打乱，各账号长期看是公平的。
#
# 2. **随机延时（仅定时任务）**
#    定时任务在固定时刻启动（cron / systemd timer），
#    四个账号在同一秒开始访问华为云，是极其明显的机器特征。
#    每个账号前加随机等待，把访问时刻打散。
#
#    **手动运行不加延时** —— 用户点了按钮就期望立刻开始，
#    让他等 3 分钟是不可接受的。
#
# 用法：
#   source "$(dirname "$0")/hwcloud-lib.sh"
#   for acc in $(random_accounts); do
#       random_delay "$acc"       # 定时任务调用；手动运行用 HWCLOUD_MANUAL=1 跳过
#       ...
#   done

# HWCLOUD_ACCOUNT_ROOT 账号根目录，可由调用方覆盖
HWCLOUD_ACCOUNT_ROOT="${HWCLOUD_ACCOUNT_ROOT:-/opt/hwcloud-demo/accounts}"

# random_accounts 返回打乱顺序的账号 ID 列表，一行一个。
#
# 只返回有 cookies.json 的账号 —— 没有的是未完成登录的残留目录，
# 跑起来只会刷一堆 401。
random_accounts() {
    local list=()
    local d acc
    for d in "$HWCLOUD_ACCOUNT_ROOT"/*/; do
        [ -d "$d" ] || continue
        acc=$(basename "$d")
        [ -f "$d/state/cookies.json" ] || continue
        list+=("$acc")
    done

    if [ ${#list[@]} -eq 0 ]; then
        return 0
    fi

    # shuf 是 GNU coreutils 自带；没有就退回 awk 随机排序
    if command -v shuf >/dev/null 2>&1; then
        printf '%s\n' "${list[@]}" | shuf
    else
        printf '%s\n' "${list[@]}" | awk 'BEGIN{srand()} {print rand(), $0}' \
            | sort -n | cut -d' ' -f2-
    fi
}

# random_delay 给单个账号加随机等待。
#
# 参数：
#   $1  账号 ID（只用于日志）
#   $2  最大秒数，默认 180（3 分钟）
#
# 环境变量：
#   HWCLOUD_MANUAL=1   跳过延时（手动运行用）
#   HWCLOUD_NO_DELAY=1 同上，兼容另一种叫法
#
# 为什么默认 3 分钟：四个账号最多多等 12 分钟，
# 日常任务本身要跑 5-10 分钟，这个量级不会让整轮拖太久，
# 但足以把「同一秒四个请求」打散成「几分钟内陆续到达」。
random_delay() {
    local acc="$1"
    local max="${2:-180}"

    if [ "${HWCLOUD_MANUAL:-0}" = "1" ] || [ "${HWCLOUD_NO_DELAY:-0}" = "1" ]; then
        return 0
    fi

    # $RANDOM 是 bash 内置，0-32767
    local secs=$(( RANDOM % max ))
    if [ "$secs" -gt 0 ]; then
        printf '[%s] [%s] 随机延时 %d 秒…\n' "$(date '+%Y-%m-%d %H:%M:%S')" "$acc" "$secs"
        sleep "$secs"
    fi
}

# hwcloud_log 统一日志格式，追加到指定文件。
hwcloud_log() {
    local file="$1"; shift
    printf '[%s] %s\n' "$(date '+%Y-%m-%d %H:%M:%S')" "$*" >> "$file"
}
