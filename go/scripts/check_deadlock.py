#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""静态检查：找出「在 s.mu 内部又拿 s.mu」的自锁死路径。

为什么需要：
    Go 的 sync.Mutex 不可重入。withAccount() 会 s.mu.Lock()，
    如果它包裹的函数里又调了 withAccount / autoLogin / ensureAlive /
    probeAccount，就会自锁死 —— 进程不崩溃、不打日志、永远卡住，
    排查起来非常费时间（实测卡死 2 分钟以上，Camoufox 那边早就成功了）。

    这类 bug 编译器查不出来，只能靠调用图分析。

检查方法：
    1. 找出所有 withAccount(...) 包裹的函数体
    2. 在函数体里搜索禁止调用的符号
    3. 递归跟进被调用的本地函数（一层就够，实测的坑都在一层内）

用法：
    python3 check_deadlock.py [--verbose]
    退出码 0 = 干净，1 = 发现可疑路径
"""
import os
import re
import sys

SRC = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))),
                   "internal", "web")

# 这些符号一旦在 s.mu 内部被调用就会自锁死
FORBIDDEN = [
    "withAccount(",
    "withAccountTry(",   # TryLock 虽然不会死锁，但语义上仍然不该出现在锁内
    "autoLogin(",
    "ensureAlive(",
    "probeAccount(",
    "probeAccountFull(",
    "probeAccountTry(",
    "handleAccounts(",
    "refreshProbesAsync(",
]

# 已知安全的例外（目前为空）。
#
# 早先把 runDaily/runWeekly 放进来豁免，但那样会掩盖「它们以后又调
# autoLogin」的回归 —— 它们现在是安全的（内部没有任何禁止符号），
# 检查器能自己验证出来，不需要豁免。
SAFE_CALLEES = set()


def read_files():
    out = {}
    for fn in sorted(os.listdir(SRC)):
        if not fn.endswith(".go"):
            continue
        p = os.path.join(SRC, fn)
        with open(p, encoding="utf-8") as f:
            out[fn] = f.read().splitlines()
    return out


def find_func_body(lines, start_idx):
    """从函数声明行往下找函数体范围（大括号配平）。"""
    depth = 0
    started = False
    for i in range(start_idx, len(lines)):
        for ch in lines[i]:
            if ch == "{":
                depth += 1
                started = True
            elif ch == "}":
                depth -= 1
                if started and depth == 0:
                    return start_idx, i
    return start_idx, len(lines) - 1


def func_of(lines, idx):
    """往上找包含 idx 行的函数声明。"""
    for i in range(idx, -1, -1):
        m = re.match(r"^func (?:\([^)]*\)\s*)?(\w+)\(", lines[i])
        if m:
            return m.group(1), i
    return None, None


def build_func_index(files):
    """建立 函数名 -> (文件名, 起止行) 索引，用于跟进间接调用。"""
    idx = {}
    for fn, lines in files.items():
        for i, line in enumerate(lines):
            m = re.match(r"^func (?:\([^)]*\)\s*)?(\w+)\(", line)
            if not m:
                continue
            name = m.group(1)
            _, end = find_func_body(lines, i)
            idx[name] = (fn, i, end)
    return idx


def calls_inside(lines, start, end, names):
    """在 [start,end] 行范围内，找出调用了 names 里哪些函数。"""
    hits = set()
    for i in range(start, min(end + 1, len(lines))):
        line = lines[i]
        stripped = line.strip()
        if stripped.startswith("//"):
            continue
        for n in names:
            # 匹配 n( / s.n( / x.n( 等，但不能是 myrunDaily(
            if re.search(r"(?<![A-Za-z0-9_])(?:\w+\.)?" + re.escape(n) + r"\s*\(", line):
                hits.add(n)
    return hits


def main():
    verbose = "--verbose" in sys.argv
    files = read_files()
    funcs = build_func_index(files)
    local_names = set(funcs.keys())
    problems = []

    for fn, lines in files.items():
        for i, line in enumerate(lines):
            if not re.search(r"s\.withAccount\(", line):
                continue
            if re.match(r"^func .*withAccount", line):
                continue

            # 用大括号配平找闭包体结束。
            #
            # 注意：不能按整行的 { } 计数 —— `s.withAccount(id, func() any {`
            # 这一行里可能同时出现 { 和 }（例如 `. (map[string]any)` 后面的
            # 收尾），按行数会立刻配平，end 直接等于起始行。
            # 必须从左到右逐字符扫描，找到开括号的位置再开始计数。
            open_at = line.find("func() any {")
            if open_at < 0:
                open_at = line.find("{")
            depth = 0
            started = False
            end = len(lines) - 1
            for j in range(i, len(lines)):
                seg = lines[j] if j > i else lines[j][open_at:]
                for ch in seg:
                    if ch == "{":
                        depth += 1
                        started = True
                    elif ch == "}":
                        depth -= 1
                        if started and depth == 0:
                            end = j
                            break
                if started and depth == 0:
                    break

            caller, caller_line = func_of(lines, i)
            body = lines[i + 1:end]

            # 1. 闭包体内的直接违规
            for k, bl in enumerate(body):
                stripped = bl.strip()
                if stripped.startswith("//") or stripped.startswith("*"):
                    continue
                for bad in FORBIDDEN:
                    if bad in bl:
                        problems.append({
                            "file": fn, "line": i + k + 2,
                            "caller": caller, "symbol": bad.rstrip("("),
                            "how": "直接调用",
                            "text": stripped[:90],
                        })

            # 2. 闭包体调用的本地函数里的间接违规（跟进一层）
            called = calls_inside(lines, i + 1, end - 1, local_names)
            for callee in called:
                if callee in SAFE_CALLEES:
                    continue
                cfn, cstart, cend = funcs[callee]
                clines = files[cfn]
                for j in range(cstart, min(cend + 1, len(clines))):
                    stripped = clines[j].strip()
                    if stripped.startswith("//") or stripped.startswith("*"):
                        continue
                    for bad in FORBIDDEN:
                        if bad in clines[j]:
                            problems.append({
                                "file": cfn, "line": j + 1,
                                "caller": callee,
                                "symbol": bad.rstrip("("),
                                "how": f"经 {caller}() 的 withAccount 间接调用",
                                "text": stripped[:90],
                            })

    if verbose:
        print(f"扫描 {len(files)} 个文件, {len(funcs)} 个函数")

    # 去重
    seen = set()
    uniq = []
    for p in problems:
        key = (p["file"], p["line"], p["symbol"])
        if key in seen:
            continue
        seen.add(key)
        uniq.append(p)

    if not uniq:
        print("[OK] 没有发现 s.mu 内的自锁死路径")
        return 0

    print(f"[FAIL] 发现 {len(uniq)} 处可疑调用：\n")
    for p in uniq:
        print(f"  {p['file']}:{p['line']}  调用了 {p['symbol']}  ({p['how']})")
        print(f"     代码: {p['text']}")
        print()
    print("修法：把该调用移到 withAccount 之外，或改用 TryLock 版本。")
    return 1


if __name__ == "__main__":
    sys.exit(main())
