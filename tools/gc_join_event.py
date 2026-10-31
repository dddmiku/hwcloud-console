#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""完成「每周参加开发者活动」(+10)：报名一个进行中的活动。

链路（实测）：
  1. https://developer.huaweicloud.com/events
  2. 点「报名进行中」的卡片 → 新标签打开 https://edu.huaweicloud.com/signup/<id>
  3. 详情页先显示「立即报名」按钮，**点了才展开表单**（不是一开始就渲染）
  4. 表单字段：
       #inputName    姓名，已从实名信息预填成掩码「张**」
       #inputPhone   手机，同上
       #inputMail    邮箱，必填，要自己填
       学校/公司      必填，自定义 input，没有 id，靠 placeholder 定位
       您的岗位       必填，是 div.select-box 自定义下拉，不是 <select>
  5. 点「报名」提交

坑：
  - 「立即报名」里含「报名」二字，用 /已报名/ 判断会误判成已报名
  - 岗位下拉必须点 .select-box 展开，再点弹出的选项
  - 姓名/手机是掩码显示，不要试图改（会破坏实名关联）

用法：
    python3 gc_join_event.py [account_id] [--dry-run]
"""
import asyncio
import json
import sys

sys.stdout.reconfigure(encoding="utf-8", errors="replace")

CHROME = "/opt/hwcloud-demo/browsers/chromium-1228/chrome-linux64/chrome"
UA = ("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
      "(KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36")
COOKIES = "/opt/hwcloud-demo/accounts/{aid}/state/cookies.json"

FILL_EMAIL = "dev@example.com"
FILL_COMPANY = "独立开发者"
PREFER_ROLE = ("其他", "学生", "开发者", "工程师", "研发")


async def fill_form(pg):
    """填邮箱、学校/公司、岗位。返回是否全部填好。"""
    ok = True

    try:
        await pg.locator("#inputMail").first.fill(FILL_EMAIL, timeout=8000)
        print("    邮箱 OK")
    except Exception as e:
        print(f"    邮箱失败: {str(e)[:60]}")
        ok = False

    try:
        await pg.locator('input[placeholder*="学校"], input[placeholder*="公司"]'
                         ).first.fill(FILL_COMPANY, timeout=8000)
        print("    学校/公司 OK")
    except Exception as e:
        print(f"    学校/公司失败: {str(e)[:60]}")
        ok = False

    # 岗位：自定义下拉。点 .select-box 展开，再点选项。
    try:
        await pg.locator(".select-box").first.click(timeout=8000)
        await pg.wait_for_timeout(1500)
        opts = await pg.evaluate("""() => {
            return Array.from(document.querySelectorAll(
                '.select-box li, .select-list li, .option, [class*=option]'))
                .map(e => (e.innerText||'').trim())
                .filter(t => t && t.length < 15);
        }""")
        print(f"    岗位选项: {opts[:12]}")
        picked = next((w for w in PREFER_ROLE if w in opts), opts[0] if opts else None)
        if picked:
            await pg.get_by_text(picked, exact=True).first.click(timeout=8000)
            print(f"    岗位 OK: {picked}")
            await pg.wait_for_timeout(800)
        else:
            print("    岗位没有可选项")
            ok = False
    except Exception as e:
        print(f"    岗位失败: {str(e)[:60]}")
        ok = False

    return ok


async def main():
    from playwright.async_api import async_playwright

    args = [a for a in sys.argv[1:] if not a.startswith("--")]
    dry = "--dry-run" in sys.argv
    aid = args[0] if args else "<ACCOUNT_B>"

    raw = json.load(open(COOKIES.format(aid=aid), encoding="utf-8"))
    cks = [{"name": c["name"], "value": c.get("value", ""),
            "domain": c.get("domain"), "path": c.get("path", "/"),
            "secure": bool(c.get("secure", True))}
           for c in raw if "huaweicloud.com" in c.get("domain", "")]

    async with async_playwright() as p:
        br = await p.chromium.launch(headless=True, executable_path=CHROME,
                                     args=["--no-sandbox"])
        ctx = await br.new_context(user_agent=UA,
                                   viewport={"width": 1400, "height": 1200})
        await ctx.add_cookies(cks)
        pg = await ctx.new_page()

        print("[1] 打开活动列表")
        await pg.goto("https://developer.huaweicloud.com/events",
                      wait_until="domcontentloaded", timeout=60000)
        await pg.wait_for_timeout(9000)

        cards = await pg.evaluate("""() => {
            return Array.from(document.querySelectorAll('.salesPro-box')).map((c, i) => {
                const t = (c.innerText || '');
                const r = c.getBoundingClientRect();
                return {i, x: r.x + r.width/2, y: r.y + r.height/2,
                        signup: /报名进行中/.test(t),
                        text: t.replace(/\\n/g, ' | ').slice(0, 70)};
            });
        }""")
        live = [c for c in cards if c["signup"]]
        print(f"    报名中 {len(live)} 个")
        if not live:
            print("!!! 没有报名中的活动")
            await ctx.close(); await br.close()
            return 1

        for card in live:
            print(f"\n[2] {card['text'][:50]}")
            before = len(ctx.pages)
            await pg.mouse.click(card["x"], card["y"])
            await pg.wait_for_timeout(7000)
            detail = ctx.pages[-1] if len(ctx.pages) > before else pg
            await detail.wait_for_load_state("domcontentloaded")
            await detail.wait_for_timeout(4000)
            act_id = detail.url.rstrip("/").split("/")[-1]
            print(f"    {detail.url}")

            state = await detail.evaluate("""() => {
                const btns = Array.from(document.querySelectorAll('button,a'))
                    .map(b => (b.innerText || '').trim());
                return {
                    already: btns.includes('已报名'),
                    canSignup: btns.includes('立即报名'),
                    hasForm: !!document.getElementById('inputName'),
                };
            }""")
            print(f"    already={state['already']} canSignup={state['canSignup']} form={state['hasForm']}")

            if state["already"]:
                print("    已报名，下一个")
                if detail is not pg:
                    await detail.close()
                continue

            if dry:
                print("[dry-run] 不提交")
                await ctx.close(); await br.close()
                return 0

            # 展开表单
            if not state["hasForm"]:
                print("[3] 点「立即报名」展开表单")
                await detail.evaluate("""() => {
                    const b = Array.from(document.querySelectorAll('button,a,span'))
                        .find(x => (x.innerText||'').trim() === '立即报名'
                                && x.getBoundingClientRect().width > 30);
                    if (b) b.click();
                }""")
                await detail.wait_for_timeout(5000)

            print("[4] 填表")
            if not await fill_form(detail):
                print("    填表不完整，下一个")
                if detail is not pg:
                    await detail.close()
                continue

            print("[5] 提交")
            await detail.evaluate("""() => {
                const b = Array.from(document.querySelectorAll('button,a'))
                    .find(x => (x.innerText||'').trim() === '报名'
                            && x.getBoundingClientRect().width > 30);
                if (b) b.click();
            }""")
            await detail.wait_for_timeout(8000)

            after = await detail.evaluate("""() => {
                const t = document.body.innerText || '';
                const btns = Array.from(document.querySelectorAll('button,a'))
                    .map(b => (b.innerText || '').trim());
                const err = Array.from(document.querySelectorAll('*'))
                    .filter(x => /请(输入|选择)/.test(x.innerText||'')
                            && (x.innerText||'').length < 40);
                return {
                    ok: btns.includes('已报名') || /报名成功|您已报名/.test(t),
                    err: err.length ? err[0].innerText.trim() : '',
                };
            }""")
            print(f"[6] ok={after['ok']} err={after['err']!r}")

            if after["ok"]:
                print(f"\n[OK] 已报名 {act_id}")
                await ctx.close(); await br.close()
                return 0

            if detail is not pg:
                await detail.close()

        print("\n[FAIL] 没有报成功的活动")
        await ctx.close()
        await br.close()
        return 1


sys.exit(asyncio.run(main()))
