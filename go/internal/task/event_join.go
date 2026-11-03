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
	// 【必须用华为云会话，不是 GitCode 会话】
	//
	// 早先这里读的是 config.GitCodeStateFile() —— 那是 gitcode.com 的
	// 登录态，拿去访问 developer.huaweicloud.com 完全是未登录状态。
	// 表现：点「立即报名」后跳到 auth.huaweicloud.com 登录页，
	// 页面上根本没有报名表单，于是报「找不到邮箱输入框」，
	// 五张卡片全跳过，最终报「没有可报名的活动」。
	//
	// 实测 2026-11-03 用探测脚本确认：详情页上出现的是
	// userAccount / password 输入框（登录页），不是报名表单。
	cookieFile := config.CookieFile()
	if _, err := os.Stat(cookieFile); err != nil {
		return nil, fmt.Errorf("未找到华为云会话（%s），请先登录", cookieFile)
	}

	pw, err := playwright.Run()
	if err != nil {
		return nil, err
	}
	defer pw.Stop()

	// NewHuaweiContext 会把 cookies.json 灌进浏览器上下文，
	// 并且 UA 与登录时一致（华为云 WAF 按 UA 校验）。
	b, ctx, err := browser.NewHuaweiContext(pw, true)
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
	// traces 记录每张卡片的处理结局（成功/跳过原因）。
	// 出问题时直接看这个数组就知道卡在哪一步，不用靠猜。
	traces := make([]map[string]any, 0, len(cards))
	out["traces"] = traces
	out["clickable"] = len(cards)
	if len(cards) == 0 {
		out["result"] = "没有可点击的活动卡片"
		return out, nil
	}

	// 只试前几张就够了。
	//
	// 任务要求是「每周参加开发者活动」，**报一个就完成** ——
	// 没必要把五张卡片全试一遍。全试的代价是每张都要等页面加载
	// （新标签页 + 详情页渲染 + 表单展开），五张加起来轻松超过
	// 4 分钟看门狗，结果是明明能报成功却被超时打断。
	//
	// 取 3 张是留点余量：万一前两张都已报名，第三张还能兜住。
	const maxTries = 3
	if len(cards) > maxTries {
		out["tried"] = maxTries
		cards = cards[:maxTries]
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
		// 逐张卡片记录处理结果。
		//
		// 原来只有 out["click_x"] 这类单值字段，每轮覆盖，
		// 最后只留下最后一张卡片的状态 —— 前 4 张为什么被跳过
		// 完全看不出来，只能靠猜。现在把每张的结局都攒进 trace。
		trace := map[string]any{"idx": int(toF(c["i"])), "text": c["text"]}
		traces = append(traces, trace)
		out["traces"] = traces

		pagesBefore := len(ctx.Pages())
		idx := int(toF(c["i"]))

		// 先滚到卡片位置，再用**真实鼠标点击**。
		//
		// 【为什么不能只用 el.click()】
		//
		// 卡片的跳转可能绑在子元素的原生事件或 <a> 上，
		// 对容器元素调 el.click() 不一定能触发 —— 实测 5 张卡片
		// 只有最后一张打开了新标签页，前 4 张都没反应。
		// 真实鼠标点击会走完整的命中测试（hit testing），
		// 点在哪个元素上就由那个元素处理，更接近人手操作。
		page.Evaluate(`(idx) => {
			const el = document.querySelectorAll('.salesPro-box')[idx];
			if (el) el.scrollIntoView({block: 'center'});
		}`, int(idx))
		page.WaitForTimeout(600)

		// 重新取坐标（滚动后位置变了）
		posRaw, _ := page.Evaluate(`(idx) => {
			const el = document.querySelectorAll('.salesPro-box')[idx];
			if (!el) return null;
			const r = el.getBoundingClientRect();
			return {x: Math.round(r.x + r.width/2), y: Math.round(r.y + r.height/2),
			        w: Math.round(r.width), h: Math.round(r.height)};
		}`, int(idx))
		pm, _ := posRaw.(map[string]any)
		if pm == nil || toF(pm["w"]) < 50 {
			trace["skip"] = "滚动后坐标无效"
			continue
		}
		if err := page.Mouse().Click(toF(pm["x"]), toF(pm["y"])); err != nil {
			trace["skip"] = "鼠标点击失败: " + err.Error()
			continue
		}
		trace["clicked_at"] = fmt.Sprintf("%v,%v", pm["x"], pm["y"])

		// 点卡片会在新标签打开详情页。ctx.Pages() 不是立刻更新，
		// 要轮询等一会儿；否则会拿到列表页，然后误判成「没有报名入口」。
		detail := page
		for i := 0; i < 12; i++ {
			page.WaitForTimeout(500)
			if pages := ctx.Pages(); len(pages) > pagesBefore {
				detail = pages[len(pages)-1]
				break
			}
		}
		if detail == page {
			// 没开新标签页。
			//
			// 【不能退回列表页继续找按钮】
			//
			// 早先这里是 `page.WaitForTimeout(3000)` 然后继续用 page ——
			// 但 page 是**活动列表页**，列表卡片上本来就有「立即报名」文字，
			// 于是 canSignup 判为 true，代码继续往下填表单，
			// 在列表页上找不到 #inputMail 就 continue，循环结束时报
			// 「没有可报名的活动」—— 完全掩盖了「标签页没打开」这个真因。
			//
			// 现在明确记下来并跳过这张卡片，最后把原因带进 result。
			trace["skip"] = "没打开详情页标签"
			out["tab_failed"] = fmt.Sprintf("卡片 %v 点击后没打开详情页", c["text"])
			continue
		}
		detail.WaitForTimeout(3000)

		actID := detail.URL()
		if i := strings.LastIndex(actID, "/"); i >= 0 {
			actID = actID[i+1:]
		}
		out["activity"] = actID

		// 轮询等详情页渲染出报名按钮。
		//
		// 【为什么不能只等固定的 4 秒】
		//
		// 实测 2026-11-03：详情页（edu.huaweicloud.com/signup/<id>）是
		// 异步渲染的，4 秒时 DOM 里既没有「立即报名」按钮也没有表单，
		// 于是 canSignup 和 hasForm 都是 false，卡片被 continue 跳过。
		// 五张卡片全跳过之后，最终报「没有可报名的活动（可能都已报名）」
		// —— 而实际上每个活动都能报，用户看到这句话完全摸不着头脑。
		//
		// 单独用探测脚本量过：同一页面等 9 秒就能稳定拿到
		// 「立即报名」（class="btn btn-red"，宽 120）。
		// 所以改成轮询：最多等 20 秒，一出现按钮就立刻往下走，
		// 正常情况不会比原来的固定等待更慢。
		var sm map[string]any
		for i := 0; i < 10; i++ {
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
				break
			}
			sm, _ = st.(map[string]any)
			if sm == nil {
				break
			}
			// 已经能判断了就别再等
			if sm["already"] == true || sm["canSignup"] == true || sm["hasForm"] == true {
				break
			}
			detail.WaitForTimeout(1000)
		}
		if sm == nil {
			trace["skip"] = "详情页状态探测失败"
			continue
		}
		out["detail_url"], _ = sm["url"].(string)
		out["detail_title"], _ = sm["title"].(string)
		if sm["already"] == true {
			trace["skip"] = "已报名"
			out["skipped_already"] = actID
			continue
		}
		if sm["canSignup"] != true && sm["hasForm"] != true {
			// 记下跳过的活动 id 和当时的 URL，方便排查是哪类页面没被识别
			trace["skip"] = "详情页没有报名按钮"
			out["skipped_nobutton"] = actID
			out["skipped_url"], _ = sm["url"].(string)
			continue
		}
		if dryRun {
			out["result"] = "dry-run（未提交）"
			return out, nil
		}

		// 展开表单。
		//
		// 点「立即报名」是**整页跳转**，不是弹窗 ——
		// URL 从 /signup/<id> 变成 /signupInfo/<id>/activity。
		// 所以点完要等导航完成，再轮询等表单元素渲染出来，
		// 而不是固定 sleep 5 秒就往下走（表单是异步渲染的，
		// 5 秒时可能还没有任何输入框）。
		if sm["hasForm"] != true {
			detail.Evaluate(`() => {
				const b = Array.from(document.querySelectorAll('button,a,span'))
					.find(x => (x.innerText||'').trim() === '立即报名'
					        && x.getBoundingClientRect().width > 30);
				if (b) b.click();
			}`)

			// 等整页跳转到 /signupInfo/<id>/activity。
			//
			// 点「立即报名」不是弹窗，是**导航**。导航期间
			// Locator().Count() 会返回 0（旧文档已卸载、新文档还没就绪），
			// 所以必须先用 WaitForLoadState 等导航结束，再等表单元素。
			_ = detail.WaitForLoadState(playwright.PageWaitForLoadStateOptions{
				State: playwright.LoadStateDomcontentloaded,
			})
			detail.WaitForTimeout(3000)

			// 再轮询等表单元素渲染出来（最多 12 秒）
			for i := 0; i < 12; i++ {
				cnt, _ := detail.Locator(
					`input[placeholder*="学校"], input[placeholder*="公司"], .select-box`).Count()
				if cnt > 0 {
					break
				}
				detail.WaitForTimeout(1000)
			}
			detail.WaitForTimeout(1200)
			trace["after_expand"] = detail.URL()
		}

		// 填表。
		//
		// 【每个字段都必须可选】
		//
		// 实测 2026-11-03 探测到的真实表单结构（点「立即报名」后
		// URL 变成 /signupInfo/<id>/activity）：
		//   - 输入框只有「请输入学校/公司」一个（外加页头搜索框）
		//   - 下拉只有「请选择职业」
		//   - 按钮是「报名」
		//   **没有 #inputMail**
		//
		// 早先代码把邮箱当必填，填不上就 continue，于是五张卡片
		// 全部被跳过，最终报「没有可报名的活动」——
		// 而实际上每张卡片的表单都不需要邮箱。
		//
		// 现在每个字段都是「有就填，没有就跳过」，只在最后提交时
		// 用页面自身的校验结果判断成败。
		if n, _ := detail.Locator("#inputMail").Count(); n > 0 {
			_ = detail.Locator("#inputMail").First().Fill(eventFillEmail)
			detail.WaitForTimeout(300)
		}
		if n, _ := detail.Locator(`input[placeholder*="学校"], input[placeholder*="公司"]`).Count(); n > 0 {
			_ = detail.Locator(`input[placeholder*="学校"], input[placeholder*="公司"]`).
				First().Fill(eventFillCompany)
			detail.WaitForTimeout(300)
		}
		// 岗位：自定义下拉。
		//
		// 页面上可能有多个 .select-box（实测两个：一个是还没选的
		// 「请选择职业」，一个是已选好的「其他」）。点错那个等于
		// 把已经选好的值又打开一次，后面的选项点击可能落到别处。
		// 这里只点文字仍是「请选择…」占位符的那个。
		opened := false
		if n, _ := detail.Locator(".select-box").Count(); n > 0 {
			for i := 0; i < n; i++ {
				box := detail.Locator(".select-box").Nth(i)
				txt, _ := box.InnerText()
				if !strings.Contains(txt, "请选择") {
					continue
				}
				if err := box.Click(); err == nil {
					opened = true
					detail.WaitForTimeout(1500)
					break
				}
			}
		}
		if opened {
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
			// 「其他」是最安全的选择：选「企业员工」会让公司变成必填，
			// 而公司字段对多数账号是空的。
			if picked == "" {
				for _, o := range opts {
					if s, _ := o.(string); s == "其他" {
						picked = "其他"
						break
					}
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
				trace["role"] = picked
			}
		}

		// 提交
		subClicked, _ := detail.Evaluate(`() => {
			const b = Array.from(document.querySelectorAll('button,a'))
				.find(x => (x.innerText||'').trim() === '报名'
				        && x.getBoundingClientRect().width > 30);
			if (b) { b.click(); return true; }
			return false;
		}`)
		if ok, _ := subClicked.(bool); !ok {
			trace["skip"] = "找不到「报名」提交按钮"
			if detail != page {
				detail.Close()
			}
			continue
		}
		detail.WaitForTimeout(8000)

		// 判断提交结果。
		//
		// 三个信号任意一个成立就算成功：
		//   1. 出现「已报名」按钮（精确匹配）
		//   2. 页面正文出现「报名成功」
		//   3. 「报名」按钮消失了（说明状态变了）
		// 只看第 1 个太脆弱 —— 不同活动的按钮文案不统一，
		// 有的成功后直接整块隐藏。
		after, evalErr := detail.Evaluate(`() => {
			const btns = Array.from(document.querySelectorAll('button,a'))
				.map(b => (b.innerText || '').trim());
			const body = document.body.innerText || '';
			const errs = Array.from(document.querySelectorAll('*'))
				.filter(x => /请(输入|选择)/.test(x.innerText||'')
				        && (x.innerText||'').length < 40)
				.map(x => x.innerText.trim());
			return {
				already: btns.includes('已报名'),
				success: body.includes('报名成功'),
				noSignupBtn: !btns.includes('报名') && !btns.includes('立即报名'),
				errs: errs.slice(0, 3),
				url: location.href
			};
		}`)
		if evalErr != nil {
			trace["skip"] = "提交后读取页面失败: " + evalErr.Error()
			if detail != page {
				detail.Close()
			}
			continue
		}
		am, _ := after.(map[string]any)
		if am != nil {
			trace["after_submit"] = am
			if am["already"] == true || am["success"] == true {
				out["ok"] = true
				out["result"] = "已报名"
				out["activity"] = actID
				return out, nil
			}
			// 「报名」按钮消失也算成功（部分活动成功后整块隐藏）
			if am["noSignupBtn"] == true {
				out["ok"] = true
				out["result"] = "已报名（按钮已消失）"
				out["activity"] = actID
				return out, nil
			}
			if es, ok := am["errs"].([]any); ok && len(es) > 0 {
				var parts []string
				for _, e := range es {
					parts = append(parts, fmt.Sprint(e))
				}
				out["last_error"] = strings.Join(parts, "; ")
				trace["skip"] = "表单校验未过: " + strings.Join(parts, "; ")
			} else {
				trace["skip"] = "提交后状态未变化"
			}
		}

		if detail != page {
			detail.Close()
		}
	}

	out["result"] = "没有可报名的活动"
	// 把跳过的原因一起带出去 —— 只报「没有可报名的活动」时，
	// 用户和排查者都看不出到底是「真的都报过」还是「页面没识别出来」。
	if v, ok := out["skipped_already"]; ok {
		out["result"] = "所有活动都已报名（最后一个: " + fmt.Sprint(v) + "）"
	} else if v, ok := out["skipped_nobutton"]; ok {
		out["result"] = "活动详情页里找不到报名按钮（最后尝试: " + fmt.Sprint(v) + "）"
	}
	return out, nil
}
