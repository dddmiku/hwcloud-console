#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""在 Xvfb :99 上打开 GitCode 登录页并填好账密，等人通过 VNC 手动过验证码。

验证码（网易易盾「拖白球避障」）无法自动破解 —— 它的轨迹由 canvas 记录并
与背景障碍做碰撞校验，模板匹配只能猜起点，猜错就锁。所以这里只做机械部分：
打开页面、切到密码登录、填账密、勾协议、点登录，剩下的滑块交给人在 VNC 里拖。

用法：
    python3 gc_manual_login.py <account_id> [--timeout 900]

成功判据：localStorage.userInfo 出现。成功后自动把会话导出成
state/gitcode_state.json，后续任务就能直接复用（不必再登录）。
"""
import asyncio
import json
import os
import sys

sys.stdout.reconfigure(encoding="utf-8", errors="replace")

CHROME = "/opt/hwcloud-demo/browsers/chromium-1228/chrome-linux64/chrome"
UA = ("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
      "(KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36")
ROOT = "/opt/hwcloud-demo/accounts"


def load_account(aid):
    with open(os.path.join(ROOT, aid, "account.json"), encoding="utf-8") as f:
        return json.load(f)


async def main():
    from playwright.async_api import async_playwright

    aid = sys.argv[1] if len(sys.argv) > 1 else ""
    if not aid:
        print("用法: gc_manual_login.py <account_id> [--timeout 900]")
        return 2
    timeout = 900
    if "--timeout" in sys.argv:
        timeout = int(sys.argv[sys.argv.index("--timeout") + 1])

    acc = load_account(aid)
    user, pwd = acc.get("gitcode_user", ""), acc.get("gitcode_pass", "")
    if not user or not pwd:
        print(f"!!! 账号 {aid} 没有 gitcode_user / gitcode_pass")
        return 2

    profile = os.path.join(ROOT, aid, "state", "gitcode_profile")
    state_out = os.path.join(ROOT, aid, "state", "gitcode_state.json")
    os.makedirs(profile, exist_ok=True)

    async with async_playwright() as p:
        ctx = await p.chromium.launch_persistent_context(
            profile, headless=False, executable_path=CHROME, user_agent=UA,
            viewport={"width": 1280, "height": 900},
            locale="zh-CN", timezone_id="Asia/Shanghai",
            args=["--no-sandbox", "--disable-blink-features=AutomationControlled",
                  "--lang=zh-CN", "--start-maximized"])
        page = ctx.pages[0] if ctx.pages else await ctx.new_page()

        print(f"[1] 打开登录页（账号 {user}）")
        await page.goto("https://gitcode.com/login",
                        wait_until="domcontentloaded", timeout=60000)
        await page.wait_for_timeout(5000)

        # 切到密码登录：页面在服务器上是英文，两种文案都试
        box = await page.evaluate("""() => {
            const tabs = Array.from(document.querySelectorAll('.login-tab'));
            const t = tabs.find(el => /password|密码/i.test(el.innerText||''))
                   || tabs[tabs.length - 1];
            if (!t) return null;
            const r = t.getBoundingClientRect();
            return {x: r.x + r.width/2, y: r.y + r.height/2, text: (t.innerText||'').trim()};
        }""")
        if box:
            await page.mouse.click(box["x"], box["y"])
            await page.wait_for_timeout(2500)
            print(f"[2] 已切到密码登录（tab={box['text']!r}）")
        else:
            print("[2] !!! 没找到登录 tab")

        await page.locator('input.devui-input__inner[type="text"]').first.fill(user)
        await page.locator('input.devui-input__inner[type="password"]').first.fill(pwd)
        await page.wait_for_timeout(500)
        print("[3] 已填账密")

        # 两个协议复选框的真实 input 是 0x0 + opacity:0，只能点 label
        await page.evaluate("""() => {
            document.querySelectorAll('label, .devui-checkbox').forEach(el => {
                const r = el.getBoundingClientRect();
                if (r.width < 50 || r.height < 10) return;
                if (!/agree|同意/i.test(el.innerText||'')) return;
                const inp = el.querySelector('input[type=checkbox]');
                if (inp && inp.checked) return;
                el.click();
            });
        }""")
        await page.wait_for_timeout(600)
        print("[4] 已勾协议")

        btn = await page.evaluate("""() => {
            const c = [];
            document.querySelectorAll('button').forEach(el => {
                const t = (el.innerText||'').trim().toLowerCase();
                const r = el.getBoundingClientRect();
                if (r.width < 40 || r.height < 20) return;
                if (!(t === 'log in' || t === 'login' || t === '登 录' || t === '登录')) return;
                c.push({x: r.x + r.width/2, y: r.y + r.height/2, area: r.width*r.height});
            });
            c.sort((a,b) => b.area - a.area);
            return c[0] || null;
        }""")
        if btn:
            await page.mouse.click(btn["x"], btn["y"])
            print("[5] 已点登录 —— 现在请用 VNC 连上来看画面")
        else:
            print("[5] !!! 没找到登录按钮，请在 VNC 里手动点")

        print()
        print("=" * 62)
        print("  接下来在 VNC 里操作：把白球拖到目标图标，避开障碍物")
        print("  验证码过了之后脚本会自动检测到登录态并导出会话")
        print(f"  最长等待 {timeout} 秒，Ctrl+C 可中断（不会导出）")
        print("=" * 62)
        print()

        ok = False
        step = max(1, timeout // 30)
        for i in range(0, timeout, step):
            await page.wait_for_timeout(step * 1000)
            st = await page.evaluate("""() => ({
                url: location.href,
                user: (localStorage.getItem('userInfo')||'').slice(0,80),
                yidun: !!document.querySelector('.yidun_popup'),
                tip: ((document.querySelector('.yidun_tips__text')||{}).innerText||'').trim().slice(0,40),
            })""")
            left = timeout - (i + step)
            tag = "验证码中" if st["yidun"] else "无验证码"
            print(f"    [{i+step}s] {tag} url={st['url'][:48]} 剩余 {left}s")
            if st["tip"]:
                print(f"           tip={st['tip']!r}")
            if st["user"]:
                ok = True
                print(f"    >>> 登录成功: {st['user'][:60]}")
                break

        if ok:
            await page.wait_for_timeout(3000)
            await ctx.storage_state(path=state_out)
            os.chmod(state_out, 0o600)
            size = os.path.getsize(state_out)
            print(f"\n[OK] 会话已导出 -> {state_out} ({size} 字节)")
        else:
            print(f"\n[FAIL] {timeout}s 内未检测到登录态，未导出会话")

        await ctx.close()
        return 0 if ok else 1


sys.exit(asyncio.run(main()))
