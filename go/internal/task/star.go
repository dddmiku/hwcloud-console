package task

import (
	"fmt"
	"os"
	"strings"

	"github.com/playwright-community/playwright-go"

	"hwcloud/internal/browser"
	"hwcloud/internal/config"
)

// StarRepo 给 AI Agent 开发实践中心下**尚未 star** 的仓库点一个 Star。
//
// 为什么不用 API：
//   - /api/v5/user/starred 需要 personal access token，而会话里只有 JWT
//     （access_token 是 eyJ... 的 JWT），拿它当 private-token 会 401
//     "token not found"。组织仓库列表 /api/v5/orgs/<org>/repos 的返回体
//     里也没有 starred 字段（是 null），所以没法从接口判断「已 star」。
//   - 唯一可靠的判据是打开仓库页读按钮文字：Star=未 star，Starred=已 star。
//
// 两个必须注意的实现细节：
//  1. 仓库对象里 path_with_namespace 是 null，必须用 html_url。按前者拼 URL
//     会得到 https://gitcode.com/None，打开的页面自然没有 star 按钮，
//     看起来像「该仓库不支持 star」。
//  2. star 按钮是 div.status-btn，被外层容器折叠成 0x0，Playwright 的
//     click 会报 element is not visible 并一直重试。必须用 JS 的
//     el.click() 触发，前端照样发 POST /api/v2/projects/<id>/star。
//
// 每次挑第一个未 star 的仓库 —— 下周再跑自然会选到下一个，不会重复。
func StarRepo(dryRun bool) (map[string]any, error) {
	state := config.GitCodeStateFile()
	if _, err := os.Stat(state); err != nil {
		return nil, fmt.Errorf("未找到 GitCode 会话，请先运行 `hwcloud login-gitcode`")
	}

	pw, err := playwright.Run()
	if err != nil {
		return nil, err
	}
	defer pw.Stop()

	// 必须 headed：GitCode 对无头浏览器做了行为判定。
	b, br, err := browser.NewStorageContext(pw, state, false)
	if err != nil {
		return nil, err
	}
	defer b.Close()
	defer br.Close()

	page, err := br.NewPage()
	if err != nil {
		return nil, err
	}

	// 先访问首页，让前端用 refresh_token 换新 access_token
	if _, err := page.Goto("https://gitcode.com", playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateDomcontentloaded,
		Timeout:   playwright.Float(60000),
	}); err != nil {
		return nil, err
	}
	page.WaitForTimeout(6000)

	// 组织仓库列表。用 Evaluate 直接 fetch，省得解析页面。
	raw, err := page.Evaluate(`async (org) => {
		const r = await fetch('/api/v5/orgs/' + org + '/repos?page=1&per_page=100',
		                      {credentials: 'include'});
		if (!r.ok) return null;
		const j = await r.json();
		if (!Array.isArray(j)) return null;
		return j.map(x => ({id: x.id, url: x.html_url || '', name: x.name || ''}));
	}`, config.GitCodeOrg)
	if err != nil {
		return nil, err
	}
	repos, ok := raw.([]any)
	if !ok || len(repos) == 0 {
		return nil, fmt.Errorf("拿不到组织 %s 的仓库列表", config.GitCodeOrg)
	}

	out := map[string]any{"total": len(repos), "dry_run": dryRun}
	for _, item := range repos {
		m, _ := item.(map[string]any)
		if m == nil {
			continue
		}
		url, _ := m["url"].(string)
		name, _ := m["name"].(string)
		if url == "" {
			continue
		}

		if _, err := page.Goto(url, playwright.PageGotoOptions{
			WaitUntil: playwright.WaitUntilStateDomcontentloaded,
			Timeout:   playwright.Float(45000),
		}); err != nil {
			continue
		}
		// 仓库首页要先拉 README / 目录树 / 统计，实测 6-8 秒 status-btn 才出现
		page.WaitForSelector(".status-btn", playwright.PageWaitForSelectorOptions{
			Timeout: playwright.Float(15000),
		})
		page.WaitForTimeout(1500)

		st, err := page.Evaluate(`() => {
			const els = Array.from(document.querySelectorAll('.status-btn'));
			const s = els.find(e => /star/i.test(e.innerText || ''));
			if (!s) return {found: false};
			const t = (s.innerText || '').trim();
			return {found: true, text: t, starred: /starred/i.test(t)};
		}`)
		if err != nil {
			continue
		}
		sm, _ := st.(map[string]any)
		if sm == nil || sm["found"] != true {
			continue
		}
		if starred, _ := sm["starred"].(bool); starred {
			continue // 已 star，看下一个
		}

		out["repo"] = name
		out["url"] = url
		out["button_before"] = sm["text"]
		if dryRun {
			out["result"] = "dry-run（未点击）"
			return out, nil
		}

		// JS 点击 —— 按钮宽高为 0，Playwright 的 click 会卡在 not visible
		if _, err := page.Evaluate(`() => {
			const els = Array.from(document.querySelectorAll('.status-btn'));
			const s = els.find(e => /star/i.test(e.innerText || ''));
			if (s) s.click();
		}`); err != nil {
			return out, err
		}
		page.WaitForTimeout(3500)

		after, _ := page.Evaluate(`() => {
			const els = Array.from(document.querySelectorAll('.status-btn'));
			const s = els.find(e => /star/i.test(e.innerText || ''));
			return s ? (s.innerText || '').trim() : '';
		}`)
		afterStr, _ := after.(string)
		out["button_after"] = afterStr
		out["ok"] = strings.Contains(strings.ToLower(afterStr), "starred")
		return out, nil
	}

	out["result"] = "所有仓库都已 star"
	return out, nil
}
