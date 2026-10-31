#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""给 AI Agent 开发实践中心下**尚未 star** 的仓库点一个 Star。

为什么这样做：
  GitCode 的「已 star」状态没有可用的只读接口 —— /api/v5/user/starred
  需要 personal access token（会话里只有 JWT，401 token not found）。
  唯一可靠的判据是打开仓库页看按钮文字：Star=未 star，Starred=已 star。

  star 按钮本身是 div.status-btn 且 getBoundingClientRect 宽高为 0
  （被外层容器折叠了），Playwright 的 click 会报 element is not visible。
  用 JS 直接 el.click() 可以触发，前端照样发
  POST /api/v2/projects/<project_id>/star。

用法：
    python3 gc_star.py <account_id> [--dry-run] [--limit 5]
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
ORG = "agent-best-practices"


async def main():
    from playwright.async_api import async_playwright

    args = [a for a in sys.argv[1:] if not a.startswith("--")]
    dry = "--dry-run" in sys.argv
    limit = 5
    if "--limit" in sys.argv:
        limit = int(sys.argv[sys.argv.index("--limit") + 1])
    if not args:
        print("用法: gc_star.py <account_id> [--dry-run] [--limit 5]")
        return 2
    aid = args[0]
    state = os.path.join(ROOT, aid, "state", "gitcode_state.json")
    if not os.path.exists(state):
        print(f"!!! {aid} 没有 gitcode_state.json")
        return 2

    async with async_playwright() as p:
        br = await p.chromium.launch(headless=True, executable_path=CHROME,
                                     args=["--no-sandbox"])
        ctx = await br.new_context(storage_state=state, user_agent=UA,
                                   viewport={"width": 1400, "height": 1000})
        page = await ctx.new_page()

        # 先进首页刷新令牌（GitCode 的 access_token 只有 ~24h）
        await page.goto("https://gitcode.com", wait_until="domcontentloaded",
                        timeout=60000)
        await page.wait_for_timeout(6000)

        # 组织仓库列表
        repos = await page.evaluate("""async (org) => {
            const r = await fetch(`/api/v5/orgs/${org}/repos?page=1&per_page=100`,
                                  {credentials: 'include'});
            if (!r.ok) return {err: r.status};
            const j = await r.json();
            return Array.isArray(j) ? j.map(x => ({
                id: x.id,
                // 注意：path_with_namespace 是 null，必须用 html_url。
                // 早先按 path_with_namespace 拼出 https://gitcode.com/None，
                // 打开的页面自然没有 star 按钮，误判成「仓库不支持 star」。
                url: x.html_url || '',
                name: x.name || '',
                stars: x.stargazers_count || 0,
            })) : {err: 'not array'};
        }""", ORG)
        if isinstance(repos, dict):
            print("!!! 拿不到仓库列表:", repos)
            await ctx.close(); await br.close()
            return 1
        print(f"[1] 组织下 {len(repos)} 个仓库")

        picked = []
        for repo in repos:
            if len(picked) >= limit:
                break
            url = repo["url"]
            if not url:
                continue
            try:
                await page.goto(url, wait_until="domcontentloaded", timeout=45000)
            except Exception as e:
                print(f"    跳过 {repo['name']}: {str(e)[:60]}")
                continue
            # 等 star 按钮真正渲染出来。3.5 秒不够 —— 仓库首页要先拉 README、
            # 目录树、统计信息，实测要 6-8 秒 status-btn 才出现。
            try:
                await page.wait_for_selector(".status-btn", timeout=15000)
            except Exception:
                pass
            await page.wait_for_timeout(1500)

            st = await page.evaluate("""() => {
                const els = Array.from(document.querySelectorAll('.status-btn'));
                const s = els.find(e => /star/i.test(e.innerText || ''));
                if (!s) return {found: false, n: els.length,
                                texts: els.map(e => (e.innerText || '').trim()).slice(0, 6)};
                const t = (s.innerText || '').trim();
                return {found: true, text: t, starred: /starred/i.test(t)};
            }""")
            if not st.get("found"):
                print(f"    {repo['name'][:30]}: 没有 star 按钮"
                      f"（status-btn={st.get('n',0)} 个，文本={st.get('texts')}）")
                continue
            if st["starred"]:
                print(f"    {repo['name'][:30]}: 已 star，跳过")
                continue
            print(f"    {repo['name'][:30]}: 未 star（按钮={st['text']!r}）")
            picked.append(repo)

            if dry:
                continue
            # JS 点击 —— 按钮被折叠成 0x0，Playwright 的 click 会判 not visible
            r = await page.evaluate("""() => {
                const els = Array.from(document.querySelectorAll('.status-btn'));
                const s = els.find(e => /star/i.test(e.innerText || ''));
                if (!s) return {ok: false, err: 'gone'};
                s.click();
                return {ok: true};
            }""")
            await page.wait_for_timeout(3500)
            after = await page.evaluate("""() => {
                const els = Array.from(document.querySelectorAll('.status-btn'));
                const s = els.find(e => /star/i.test(e.innerText || ''));
                return s ? (s.innerText || '').trim() : '';
            }""")
            ok = "starred" in after.lower()
            print(f"      -> 点击{'成功' if ok else '失败'}（现在 {after!r}）")
            if ok:
                break  # 每周只需一个

        print(f"\n[done] 候选 {len(picked)} 个，{'dry-run 未点击' if dry else '已处理'}")
        await ctx.close()
        await br.close()
        return 0


sys.exit(asyncio.run(main()))
