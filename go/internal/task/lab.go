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
		for _, label := range []string{"使用剩余时间", "重新计时", "确定", "我知道了"} {
			btn := page.GetByText(label, playwright.PageGetByTextOptions{Exact: playwright.Bool(true)})
			if count(btn) == 0 {
				continue
			}
			if err := btn.First().Click(); err == nil {
				fmt.Printf("[*] 处理实验提示弹窗：%s\n", label)
				page.WaitForTimeout(2500)
				return true
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
	if body, err := console.Locator("body").InnerText(); err == nil {
		if !strings.Contains(body, "剩余时间") && !strings.Contains(body, "实验准备完成") {
			console.WaitForTimeout(10000)
		}
	}

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
	}, nil
}