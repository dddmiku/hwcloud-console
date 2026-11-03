package task

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/playwright-community/playwright-go"

	"hwcloud/internal/browser"
	"hwcloud/internal/config"
)

const clipboardJS = `async (b64) => {
  try {
    const bin = atob(b64);
    const arr = new Uint8Array(bin.length);
    for (let i = 0; i < bin.length; i++) arr[i] = bin.charCodeAt(i);
    const blob = new Blob([arr], {type: 'image/png'});
    await navigator.clipboard.write([new ClipboardItem({'image/png': blob})]);
    return true;
  } catch (e) { return false; }
}`

type LabOptions struct {
	LabID      int
	RecordText string
	Headless   bool
	End        bool
	Timeout    int
	DryRun     bool
}

func count(l playwright.Locator) int {
	n, _ := l.Count()
	return n
}

func visible(l playwright.Locator) bool {
	v, _ := l.IsVisible()
	return v
}

// findConsole 等实验控制台出现。
//
// 实测华为云实验环境的 URL 有几种形态：
//   .../lab/consoleExperiment_<id>_...      （最常见）
//   .../lab/experimentalStudy_<id>          （旧版）
//   .../lab/experiment-detail_<id>?stage=.. （点「开始实验」后原地跳转，同一标签页）
// 最后一种容易被误判成「详情页」而继续等，白等到超时。所以这里同时
// 用页面内容判定：出现「剩余时间」或「实验准备完成」就算控制台就绪。
func findConsole(ctx playwright.BrowserContext, main playwright.Page, timeoutSec int) playwright.Page {
	return findConsoleWith(ctx, main, timeoutSec, nil)
}

// findConsoleWith 同 findConsole，但在等待期间每一轮都调用 poke
// （用于反复关掉「上次实验剩余时间」这类延迟弹出的对话框）。
func findConsoleWith(ctx playwright.BrowserContext, main playwright.Page, timeoutSec int, poke func() bool) playwright.Page {
	isConsoleURL := func(u string) bool {
		return strings.Contains(u, "consoleExperiment") ||
			strings.Contains(u, "experimentalStudy") ||
			strings.Contains(u, "console")
	}
	// 内容判定：控制台一定有倒计时、「实验准备完成」或「实验控制台」标题。
	// 注意「实验准备中」不算就绪 —— 环境还在拉起来，此时提交记录会失败。
	looksLikeConsole := func(p playwright.Page) bool {
		body, err := p.Locator("body").InnerText()
		if err != nil {
			return false
		}
		if strings.Contains(body, "实验准备中") {
			return false
		}
		return strings.Contains(body, "剩余时间") ||
			strings.Contains(body, "实验准备完成") ||
			strings.Contains(body, "实验控制台")
	}

	deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)
	for time.Now().Before(deadline) {
		// 先清掉可能弹出的对话框，否则它会挡住控制台的渲染判定
		if poke != nil {
			poke()
		}
		for _, p := range ctx.Pages() {
			if isConsoleURL(p.URL()) || looksLikeConsole(p) {
				return p
			}
		}
		if isConsoleURL(main.URL()) || looksLikeConsole(main) {
			return main
		}
		main.WaitForTimeout(3000)
	}
	return nil
}

func submitRecord(page playwright.Page, imagePath, text string) bool {
	tab := page.GetByText("过程记录", playwright.PageGetByTextOptions{Exact: playwright.Bool(true)})
	if count(tab) == 0 {
		return false
	}
	if err := tab.First().Click(); err != nil {
		return false
	}
	page.WaitForTimeout(2500)

	var textarea playwright.Locator
	if items, err := page.Locator("textarea").All(); err == nil {
		for _, item := range items {
			if visible(item) {
				textarea = item
				break
			}
		}
	}
	if textarea == nil {
		return false
	}
	if err := textarea.Fill(text); err != nil {
		return false
	}

	img, err := os.ReadFile(imagePath)
	if err != nil {
		return false
	}
	result, err := page.Evaluate(clipboardJS, base64.StdEncoding.EncodeToString(img))
	if err != nil {
		return false
	}
	if ok, _ := result.(bool); !ok {
		return false
	}

	dropzone := page.Locator(".image-upload-dropzone").First()
	if err := dropzone.Click(); err != nil {
		return false
	}
	page.WaitForTimeout(500)
	if err := page.Keyboard().Press("Control+V"); err != nil {
		return false
	}
	page.WaitForTimeout(5000)

	dialog := page.Locator("#process-record-dialog")
	if count(dialog.Locator("img")) == 0 {
		return false
	}

	var last playwright.Locator
	if confirms, err := page.GetByText("确认", playwright.PageGetByTextOptions{Exact: playwright.Bool(true)}).All(); err == nil {
		for _, c := range confirms {
			if visible(c) {
				last = c
			}
		}
	}
	if last == nil {
		return false
	}
	if err := last.Click(); err != nil {
		return false
	}
	page.WaitForTimeout(6000)
	return !visible(dialog)
}

// endLab 结束实验、释放环境。
//
// 为什么要重试 + 兜底跳转：
//   submitRecord 提交后页面可能已经跳到记录页，这时原页面上没有
//   「结束实验」按钮，直接找会返回 false —— 环境就一直开着占配额
//   （实测第二次跑周常就出现 ended:false）。
//   所以先原地找，找不到就回实验详情页再找一次。
func endLab(page playwright.Page, labID int) bool {
	tryEnd := func() bool {
		button := page.GetByText("结束实验", playwright.PageGetByTextOptions{Exact: playwright.Bool(true)})
		if count(button) == 0 {
			return false
		}
		if err := button.First().Click(); err != nil {
			return false
		}
		page.WaitForTimeout(2500)
		for _, label := range []string{"确定", "确认", "结束"} {
			items, err := page.GetByText(label, playwright.PageGetByTextOptions{Exact: playwright.Bool(true)}).All()
			if err != nil {
				continue
			}
			for _, item := range items {
				if visible(item) {
					if err := item.Click(); err == nil {
						page.WaitForTimeout(4000)
						return true
					}
				}
			}
		}
		return false
	}

	if tryEnd() {
		return true
	}

	// 原地找不到（可能已被 submitRecord 带离控制台），
	// 回到实验详情页再试一次。
	if _, err := page.Goto(
		fmt.Sprintf("https://edu.huaweicloud.com/lab/experiment-detail_%d", labID),
		playwright.PageGotoOptions{WaitUntil: playwright.WaitUntilStateDomcontentloaded}); err == nil {
		page.WaitForTimeout(3000)
	}

	return tryEnd()
}

// RunLabTask 启动云实验、提交过程记录，默认结束后释放环境。
func RunLabTask(opts LabOptions) (map[string]any, error) {
	if opts.Timeout <= 0 {
		opts.Timeout = 180
	}
	if err := browser.EnsureDriver(); err != nil {
		return nil, err
	}
	pw, err := playwright.Run()
	if err != nil {
		return nil, err
	}
	defer pw.Stop()

	br, ctx, err := browser.NewHuaweiContext(pw, opts.Headless)
	if err != nil {
		return nil, err
	}
	defer br.Close()
	_ = ctx.GrantPermissions([]string{"clipboard-read", "clipboard-write"})

	page, err := ctx.NewPage()
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf(config.EduLabDetail, opts.LabID)
	if _, err := page.Goto(url, playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateDomcontentloaded,
		Timeout:   playwright.Float(60000),
	}); err != nil {
		return nil, err
	}
	page.WaitForTimeout(8000)

	start := page.GetByText("开始实验", playwright.PageGetByTextOptions{Exact: playwright.Bool(true)})
	if opts.DryRun {
		manual := page.GetByText("查看手册", playwright.PageGetByTextOptions{Exact: playwright.Bool(true)})
		return map[string]any{
			"dry_run":          true,
			"lab_id":           opts.LabID,
			"has_start_button": count(start) > 0,
			"has_manual_button": count(manual) > 0,
		}, nil
	}
	if count(start) == 0 {
		manual := page.GetByText("查看手册", playwright.PageGetByTextOptions{Exact: playwright.Bool(true)})
		if count(manual) == 0 {
			return nil, fmt.Errorf("页面上找不到「开始实验」或「查看手册」按钮")
		}
		if err := manual.First().Click(); err != nil {
			return nil, err
		}
		page.WaitForTimeout(5000)
		return map[string]any{"mode": "manual", "lab_id": opts.LabID, "started": true}, nil
	}
	if err := start.First().Click(); err != nil {
		return nil, err
	}
	page.WaitForTimeout(4000)

	// 上次实验没结束时，华为云会弹「温馨提示」对话框：
	//   「您已获得今日免费名额，上次实验剩余时间 58 分钟，本次可选择：
	//     使用剩余时间 / 重新计时」
	// 不处理这个弹窗，后面的控制台就一直停在「实验准备中 17%」，
	// 过程记录提交不了、结束按钮也点不到，整轮白跑。
	//
	// 坑：弹窗不是立刻出现的 —— 点「开始实验」后约 4~8 秒才渲染出来。
	// 只在固定 4 秒后查一次会漏掉，必须在 findConsole 的等待循环里反复查。
	//
	// 选「使用剩余时间」：直接复用已开好的环境，最快也最省配额。
	dismissHint := func() bool {
		// 用 has-text 而不是 GetByText(Exact)。
		//
		// 实测 2026-11-03 截图：弹窗按钮确实是「使用剩余时间」四个字，
		// 但 GetByText + Exact 匹配不到 —— 按钮里的文字可能被包在
		// <span> 里或带前后空白，精确匹配就落空。
		//
		// 顺序有讲究：「使用剩余时间」优先 —— 直接复用已开好的环境，
		// 最快也最省配额。「重新计时」会把上次的剩余时间清零。
		//
		// 选择器用 tiny-button / button 这些真实的按钮类名 ——
		// 华为云这套 UI 用的是 tiny 组件库，弹窗按钮都是
		// <button class="tiny-button ...">。用 *:has-text 会匹配到
		// 外层容器，点下去没反应。
		// 选择器顺序有讲究，按「精确度」从高到低排。
		//
		// 实测 2026-11-03：这个弹窗是 **Element UI**，不是 tiny 组件库。
		// 真实结构：
		//   <div class="el-message-box__btns">
		//     <button class="el-button"><span>使用剩余时间</span></button>
		//     <button class="el-button el-button--primary"><span>重新计时</span></button>
		//   </div>
		// 弹窗正文里还有 <strong>使用剩余时间</strong> 这样的说明文字 ——
		// 用 `*:has-text` 或裸 `span:has-text` 会先命中那段说明，
		// 点下去什么也不发生，弹窗一直挂着，环境卡在「实验准备中 51%」。
		//
		// 所以先限定在 .el-message-box__btns 里的 button，再退到通用 button。
		selectors := []string{
			`.el-message-box__btns button:has-text("%s")`,
			`.el-dialog__footer button:has-text("%s")`,
			`.el-message-box button:has-text("%s")`,
			`.tiny-dialog__box button:has-text("%s")`,
			`.tiny-dialog button:has-text("%s")`,
			`button:has-text("%s")`,
			`.tiny-button:has-text("%s")`,
		}
		for _, label := range []string{"使用剩余时间", "重新计时", "确定", "我知道了", "继续"} {
			for _, sel := range selectors {
				btn := page.Locator(fmt.Sprintf(sel, label))
				n, _ := btn.Count()
				if n == 0 {
					continue
				}
				// 只点可见的：隐藏的兜底元素点不到，会白等 5 秒
				if vis, _ := btn.First().IsVisible(); !vis {
					continue
				}
				if err := btn.First().Click(playwright.LocatorClickOptions{
					Timeout: playwright.Float(5000),
				}); err == nil {
					fmt.Printf("[*] 处理实验提示弹窗：%s（%s）\n", label, sel)
					page.WaitForTimeout(3000)
					return true
				}
			}
		}
		return false
	}
	if dismissHint() {
		// 已处理
	}

	console := findConsoleWith(ctx, page, opts.Timeout, dismissHint)
	if console == nil {
		return nil, fmt.Errorf("实验环境页面未打开（可能是资源排队或登录态失效）")
	}
	console.WaitForTimeout(8000)

	// 如果控制台开在新标签页，dismissHint 里的 page 就不是控制台 ——
	// 它点的弹窗在另一个页面上，永远点不到。这里把 page 指向 console，
	// 保证后续所有 dismissHint 都作用在正确的页面。
	if console != page {
		fmt.Println("[*] 实验控制台在新标签页，切换操作目标")
		page = console
	}

	// 再处理一次弹窗。
	//
	// 「温馨提示」不是一开始就出现：实测点「开始实验」后 4~8 秒才渲染，
	// 而且环境准备到某个阶段时会**再弹一次**。findConsoleWith 的循环
	// 在判定到控制台后立刻返回，可能刚好错过了这一波。
	// 截图和提交记录都在后面，弹窗盖着的话两者都会失败
	// （实测 record_saved=false、ended=false，而任务列表一直未完成）。
	for i := 0; i < 3; i++ {
		if !dismissHint() {
			break
		}
	}

	// 等环境真正就绪。
	//
	// 实测：刚进控制台时页面写「实验准备中，大约需要 1 分钟。40%」，
	// 此时左侧「实验手册目录」显示「暂无数据」—— 手册要等环境好了才加载。
	// 不等就去找目录，永远拿到 0 个条目，walkManual 什么也点不到。
	waitReady := func(maxSec int) bool {
		deadline := time.Now().Add(time.Duration(maxSec) * time.Second)
		for time.Now().Before(deadline) {
			dismissHint()
			body, err := console.Locator("body").InnerText()
			if err != nil {
				console.WaitForTimeout(3000)
				continue
			}
			if strings.Contains(body, "实验准备中") {
				console.WaitForTimeout(4000)
				continue
			}
			if strings.Contains(body, "实验准备完成") || strings.Contains(body, "剩余时间") {
				return true
			}
			console.WaitForTimeout(4000)
		}
		return false
	}
	ready := waitReady(180)
	fmt.Printf("[*] 实验环境就绪: %v\n", ready)

	// 在控制台里真实操作一段，推进「已完成」进度。
	//
	// 为什么必须做这一步：
	//   成长中心的「每周实践一个实验」判定依据不是「开过环境」，
	//   而是实验平台的进度。实测只点「开始实验」→ 截图 → 结束实验，
	//   控制台一直停在「已完成 0%」，任务列表里 is_finish 永远是 0。
	//   必须在手册侧栏点开各章节、滚动阅读，让进度推进。
	//
	// 做法：依次点手册目录里的步骤项，每次点完滚一遍正文。
	// 不追求把实验真做完（那要操作云控制台创建 ECS 等资源，代价太大），
	// 只求进度离开 0%。
	progressed := walkManual(console)
	progressed["ready"] = ready

	if err := os.MkdirAll(config.StateDir(), 0o755); err != nil {
		return nil, err
	}
	shot := filepath.Join(config.StateDir(), fmt.Sprintf("lab_%d_console.png", opts.LabID))
	if _, err := console.Screenshot(playwright.PageScreenshotOptions{Path: playwright.String(shot)}); err != nil {
		return nil, err
	}

	recordSaved := submitRecord(console, shot, opts.RecordText)
	ended := false
	if opts.End {
		ended = endLab(console, opts.LabID)
	}
	return map[string]any{
		"mode":         "env",
		"lab_id":       opts.LabID,
		"console_url":  console.URL(),
		"screenshot":   shot,
		"record_saved": recordSaved,
		"ended":        ended,
		"progressed":   progressed,
	}, nil
}

// walkManual 在实验控制台里点开手册目录的各个步骤并滚动正文，
// 让「已完成 N%」离开 0%。
//
// 只做安全的只读动作（点目录项、滚动），不碰云控制台里的创建/删除操作 ——
// 那些会真的产生计费资源，且失败后还要善后。
func walkManual(page playwright.Page) map[string]any {
	out := map[string]any{}

	// 等目录加载。刚进控制台时「实验手册目录」显示「暂无数据」，
	// 环境准备好之后才会填充。
	var steps []string
	for attempt := 0; attempt < 15; attempt++ {
		steps = collectManualSteps(page)
		if len(steps) > 0 {
			break
		}
		page.WaitForTimeout(2000)
	}
	out["steps_found"] = len(steps)
	if len(steps) == 0 {
		if body, err := page.Locator("body").InnerText(); err == nil {
			out["body_snippet"] = harmonyClip(body)
		}
		return out
	}

	clicked := 0
	for i, name := range steps {
		if i >= 12 { // 最多点 12 个，控制耗时
			break
		}
		loc := page.GetByText(name, playwright.PageGetByTextOptions{Exact: playwright.Bool(true)})
		n, _ := loc.Count()
		if n == 0 {
			continue
		}
		if err := loc.First().Click(playwright.LocatorClickOptions{
			Timeout: playwright.Float(6000),
		}); err != nil {
			continue
		}
		clicked++
		// 滚一遍正文，模拟阅读
		page.Evaluate(`async () => {
			const el = document.querySelector('.manual-content, .cp-richtext, main') || document.body;
			const h = el.scrollHeight || 0;
			for (let y = 0; y < Math.min(h, 3000); y += 400) {
				el.scrollTop = y;
				await new Promise(r => setTimeout(r, 120));
			}
			el.scrollTop = 0;
		}`)
		page.WaitForTimeout(1200)
	}
	out["steps_clicked"] = clicked

	// 读一下当前进度文案（「已完成 N%」）
	if body, err := page.Locator("body").InnerText(); err == nil {
		if i := strings.Index(body, "已完成"); i >= 0 {
			s := body[i:]
			if len(s) > 20 {
				s = s[:20]
			}
			out["progress_text"] = strings.TrimSpace(s)
		}
	}
	return out
}

// collectManualSteps 从控制台左侧手册目录里取出步骤标题。
//
// 实测结构（tiny 组件库）：
//   <div class="tiny-card--logo__title">实验手册目录</div>
//   <div class="tiny-tree-node">...1. 准备环境...</div>
// 目录项文本形如「1. 准备环境」「2. 创建弹性云服务器ECS」。
func collectManualSteps(page playwright.Page) []string {
	raw, err := page.Evaluate(`() => {
		const out = [];
		const seen = new Set();
		// 1) 优先找目录容器里的条目
		const boxes = document.querySelectorAll(
			'.tiny-tree, .tiny-tree-node, [class*="catalog"], [class*="manual"], [class*="outline"]');
		const push = (t) => {
			t = (t || '').trim().replace(/\s+/g, ' ');
			if (!t || t.length > 50) return;
			if (seen.has(t)) return;
			seen.add(t);
			out.push(t);
		};
		boxes.forEach(box => {
			box.querySelectorAll('li, a, .tiny-tree-node__label, [class*="item"]').forEach(el => {
				const t = (el.innerText || '').trim();
				if (t && t.length < 50) push(t);
			});
		});
		// 2) 兜底：全页找「数字. 文字」形态的短文本
		if (out.length === 0) {
			document.querySelectorAll('div,span,li,a').forEach(el => {
				const t = (el.innerText || '').trim();
				if (/^[1-9][0-9]?[\.、]\s*\S/.test(t) && t.length < 40) push(t);
			});
		}
		return out.slice(0, 20);
	}`)
	if err != nil {
		return nil
	}
	items, _ := raw.([]any)
	out := make([]string, 0, len(items))
	for _, it := range items {
		if s, ok := it.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}