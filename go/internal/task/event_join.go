package task

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/playwright-community/playwright-go"

	"hwcloud/internal/browser"
	"hwcloud/internal/config"
)

// 报名表单的填充值。
//
// 姓名/手机是账号实名信息，页面显示成掩码（张** / 157****9941）且不需要填；
// 只有邮箱、学校/公司、岗位三项要自己提供。
const (
	eventFillEmail   = "dev@example.com"
	eventFillCompany = "独立开发者"
)

// eventPreferRole 岗位下拉的优先选择顺序。
// 实测选项里没有「其他」，只有「开发者」「工程/技术人员」这类；
// 优先选「开发者」，它最贴近这个账号的实际身份。
var eventPreferRole = []string{"开发者", "工程/技术人员", "架构师", "IT运维"}

// toF 把 Evaluate 返回的任意数字类型转成 float64。
// playwright-go 把 JS number 解成 float64，但整数有时是 int / json.Number，
// 直接类型断言会静默得到 0，导致坐标过滤把所有卡片都丢掉。
func toF(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	default:
		return 0
	}
}

// JoinEvent 完成「每周参加开发者活动」(+10)。
//
// 链路（实测）：
//  1. https://developer.huaweicloud.com/events 列出活动
//  2. 点「报名进行中」的卡片 → 新标签打开
//     https://edu.huaweicloud.com/signup/<activity_id>
//  3. 详情页先显示「立即报名」，**点了才展开表单**
//  4. 表单：#inputName（掩码，只读）/ #inputPhone（掩码）
//     / #inputMail / 学校公司（无 id，靠 placeholder）
//     / 岗位（div.select-box 自定义下拉，不是 <select>）
//  5. 点「报名」提交
//
// 坑：
//   - 「立即报名」含「报名」二字，用 /已报名/ 判断会误判成已报名
//   - 岗位下拉必须点 .select-box 展开，再点弹出的选项
//   - 姓名/手机是掩码，不要改（会破坏实名关联）
func JoinEvent(dryRun bool) (map[string]any, error) {
	// 整体看门狗：这个函数内部全是固定 sleep + 页面等待，
	// 累计可能远超预期，而且没有天然的超时点。
	// 一旦卡住，调用方（周常）会一直占着账号锁，面板所有操作跟着排队 ——
	// 实测卡过 5 分钟以上，用户以为整个面板死了。
	//
	// 这里给一个硬上限：到点直接返回错误，让调用方继续下一步。
	type result struct {
		out map[string]any
		err error
	}
	done := make(chan result, 1)
	go func() {
		o, e := joinEventInner(dryRun)
		done <- result{o, e}
	}()
	select {
	case r := <-done:
		return r.out, r.err
	case <-time.After(4 * time.Minute):
		return nil, fmt.Errorf("开发者活动报名超时（4 分钟），已放弃本次尝试；下次周常会重试")
	}
}

func joinEventInner(dryRun bool) (map[string]any, error) {
	state := config.GitCodeStateFile()
	if _, err := os.Stat(state); err != nil {
		return nil, fmt.Errorf("未找到 GitCode 会话，请先运行 `hwcloud login-gitcode`")
	}

	pw, err := playwright.Run()
	if err != nil {
		return nil, err
	}
	defer pw.Stop()

	b, ctx, err := browser.NewStorageContext(pw, state, true)
	if err != nil {
		return nil, err
	}
	defer b.Close()
	defer ctx.Close()

	page, err := ctx.NewPage()
	if err != nil {
		return nil, err
	}

	out := map[string]any{"dry_run": dryRun}

	if _, err := page.Goto("https://developer.huaweicloud.com/events", playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateDomcontentloaded,
		Timeout:   playwright.Float(60000),
	}); err != nil {
		return nil, err
	}
	page.WaitForTimeout(9000)

	// 收集「报名进行中」的卡片。
	//
	// 坑：卡片是**懒渲染**的 —— 页面刚加载时 .salesPro-box 存在于 DOM
	// 但 getBoundingClientRect() 全是 0（还没排版）。必须滚一遍触发渲染，
	// 否则过滤后一个都不剩，表现为「没有可点击的卡片」。
	page.Evaluate(`async () => {
		const step = window.innerHeight * 0.8;
		for (let y = 0; y < document.body.scrollHeight; y += step) {
			window.scrollTo(0, y);
			await new Promise(r => setTimeout(r, 120));
		}
		window.scrollTo(0, 0);
	}`)
	page.WaitForTimeout(2500)

	raw, err := page.Evaluate(`() => {
		const out = [];
		document.querySelectorAll('.salesPro-box').forEach((c, i) => {
			const t = (c.innerText || '');
			if (!/报名进行中/.test(t)) return;
			const r = c.getBoundingClientRect();
			out.push({i, x: r.x + r.width/2, y: r.y + r.height/2,
			          w: Math.round(r.width), h: Math.round(r.height),
			          text: t.replace(/\n/g, ' | ').slice(0, 70)});
		});
		return out;
	}`)
	if err != nil {
		return nil, err
	}
	allCards, _ := raw.([]any)
	out["cards"] = len(allCards)
	if len(allCards) > 0 {
		if first, ok := allCards[0].(map[string]any); ok {
			out["dbg"] = fmt.Sprintf("w=%v h=%v x=%v y=%v",
				first["w"], first["h"], first["x"], first["y"])
		}
	}

	// 过滤掉坐标无效的（宽度 0 = 被折叠/未渲染）。
	//
	// 注意类型：playwright-go 的 Evaluate 会把 JS 数字解成 float64，
	// 但也见过 int（JSON 里是整数时）。统一用 toF() 转，别直接断言。
	cards := make([]any, 0, len(allCards))
	for _, item := range allCards {
		c, _ := item.(map[string]any)
		if c == nil {
			continue
		}
		if toF(c["w"]) < 50 || toF(c["x"]) <= 0 {
			continue
		}
		cards = append(cards, c)
	}
	out["clickable"] = len(cards)
	if len(cards) == 0 {
		out["result"] = "没有可点击的活动卡片"
		return out, nil
	}

	for _, item := range cards {
		c, _ := item.(map[string]any)
		if c == nil {
			continue
		}
		x := toF(c["x"])
		y := toF(c["y"])
		if x <= 0 || y <= 0 {
			continue
		}
		out["click_x"] = x
		out["click_y"] = y
		out["card_text"], _ = c["text"].(string)

		pagesBefore := len(ctx.Pages())
		// 用 DOM 点击而不是鼠标坐标：卡片可能在视口外，坐标点击会落空。
		idx := int(toF(c["i"]))
		clicked, err := page.Evaluate(`(idx) => {
			const cards = document.querySelectorAll('.salesPro-box');
			const el = cards[idx];
			if (!el) return false;
			el.scrollIntoView({block: 'center'});
			el.click();
			return true;
		}`, int(idx))
		if err != nil {
			continue
		}
		if ok, _ := clicked.(bool); !ok {
			continue
		}

		// 点卡片会在新标签打开详情页。ctx.Pages() 不是立刻更新，
		// 要轮询等一会儿；否则会拿到列表页，然后误判成「没有报名入口」。
		detail := page
		for i := 0; i < 14; i++ {
			page.WaitForTimeout(500)
			if pages := ctx.Pages(); len(pages) > pagesBefore {
				detail = pages[len(pages)-1]
				break
			}
		}
		if detail == page {
			// 没开新标签：可能是当前页跳转了
			page.WaitForTimeout(3000)
		}
		detail.WaitForTimeout(4000)

		actID := detail.URL()
		if i := strings.LastIndex(actID, "/"); i >= 0 {
			actID = actID[i+1:]
		}
		out["activity"] = actID

		st, err := detail.Evaluate(`() => {
			const btns = Array.from(document.querySelectorAll('button,a'))
				.map(b => (b.innerText || '').trim());
			return {already: btns.includes('已报名'),
			        canSignup: btns.includes('立即报名'),
			        hasForm: !!document.getElementById('inputName'),
			        url: location.href,
			        title: document.title.slice(0, 40)};
		}`)
		if err != nil {
			continue
		}
		sm, _ := st.(map[string]any)
		if sm == nil {
			continue
		}
		out["detail_url"], _ = sm["url"].(string)
		out["detail_title"], _ = sm["title"].(string)
		if sm["already"] == true {
			continue
		}
		if sm["canSignup"] != true && sm["hasForm"] != true {
			continue
		}
		if dryRun {
			out["result"] = "dry-run（未提交）"
			return out, nil
		}

		// 展开表单
		if sm["hasForm"] != true {
			detail.Evaluate(`() => {
				const b = Array.from(document.querySelectorAll('button,a,span'))
					.find(x => (x.innerText||'').trim() === '立即报名'
					        && x.getBoundingClientRect().width > 30);
				if (b) b.click();
			}`)
			detail.WaitForTimeout(5000)
		}

		// 填表
		if err := detail.Locator("#inputMail").First().Fill(eventFillEmail); err != nil {
			continue
		}
		if err := detail.Locator(`input[placeholder*="学校"], input[placeholder*="公司"]`).
			First().Fill(eventFillCompany); err != nil {
			continue
		}
		// 岗位：自定义下拉
		if err := detail.Locator(".select-box").First().Click(); err == nil {
			detail.WaitForTimeout(1500)
			optsRaw, _ := detail.Evaluate(`() => {
				return Array.from(document.querySelectorAll(
					'.select-box li, .select-list li, .option, [class*=option]'))
					.map(e => (e.innerText||'').trim())
					.filter(t => t && t.length < 15);
			}`)
			opts, _ := optsRaw.([]any)
			picked := ""
			for _, want := range eventPreferRole {
				for _, o := range opts {
					if s, _ := o.(string); s == want {
						picked = want
						break
					}
				}
				if picked != "" {
					break
				}
			}
			if picked == "" && len(opts) > 0 {
				picked, _ = opts[0].(string)
			}
			if picked != "" {
				detail.GetByText(picked, playwright.PageGetByTextOptions{
					Exact: playwright.Bool(true),
				}).First().Click()
				detail.WaitForTimeout(800)
				out["role"] = picked
			}
		}

		// 提交
		detail.Evaluate(`() => {
			const b = Array.from(document.querySelectorAll('button,a'))
				.find(x => (x.innerText||'').trim() === '报名'
				        && x.getBoundingClientRect().width > 30);
			if (b) b.click();
		}`)
		detail.WaitForTimeout(8000)

		after, _ := detail.Evaluate(`() => {
			const btns = Array.from(document.querySelectorAll('button,a'))
				.map(b => (b.innerText || '').trim());
			const errs = Array.from(document.querySelectorAll('*'))
				.filter(x => /请(输入|选择)/.test(x.innerText||'')
				        && (x.innerText||'').length < 40);
			return {ok: btns.includes('已报名'), err: errs.length ? errs[0].innerText.trim() : ''};
		}`)
		am, _ := after.(map[string]any)
		if am != nil && am["ok"] == true {
			out["ok"] = true
			out["result"] = "已报名"
			return out, nil
		}
		if am != nil {
			out["last_error"], _ = am["err"].(string)
		}

		if detail != page {
			detail.Close()
		}
	}

	out["result"] = "没有可报名的活动（可能都已报名）"
	return out, nil
}
