// 赛事报名 + 提交作品。
//
// 完整链路（2026-10-31 实测打通）：
//  1. /competition/list 找「立即报名」→ 进入 raceEnrollment/<id>
//  2. 填报名表：职业选「其他」（选「企业员工」会让「公司」变成必填，
//     而公司字段是 readonly 展示字段，账号没填过就永远是空，无法提交）
//  3. 其余必填：班级/学号/指导老师 随便填；省份/学校选第一项
//  4. 提交 → 页面显示「报名成功」
//  5. 到 /competition/information/<id>/submission，选阶段组+阶段，
//     上传 zip（<=200MB，每天 1 次）
//  6. 任务变为已完成
package task

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/playwright-community/playwright-go"

	"hwcloud/internal/browser"
	"hwcloud/internal/config"
)

// EventOptions 赛事任务参数。
type EventOptions struct {
	WorkZip   string // 作品 zip 路径，空则自动生成
	Headless  bool
	DryRun    bool
	Timeout   int
}

// 报名表的固定填法。
// 职业必须选「其他」—— 选「企业员工」会把「公司」变成必填，
// 而公司是 readonly 展示字段（值来自账号资料），空值时无法通过校验。
var (
	eventFillText = map[string]string{
		"请输入请填写您的班级": "1班",
		"请输入学号":       "2026001",
		"请输入指导老师":     "李老师",
	}
	eventOccupation = "其他"
)

// pickOption 点开下拉并选一项（默认第一项）。
func pickOption(page playwright.Page, placeholder, want string) (string, error) {
	inp := page.Locator(fmt.Sprintf(`input[placeholder="%s"]`, placeholder))
	if count(inp) == 0 {
		return "", fmt.Errorf("找不到 %s", placeholder)
	}
	box, err := inp.First().BoundingBox()
	if err != nil || box == nil {
		return "", fmt.Errorf("%s 无位置信息", placeholder)
	}
	if err := inp.First().Click(); err != nil {
		return "", err
	}
	page.WaitForTimeout(1800)

	// 选项弹层可能挂在 body 末尾，用「紧邻输入框下方」筛选，
	// 避免把导航栏文字当成选项（踩过：选到「中国站」）。
	opts, err := page.Evaluate(`(inY) => {
		const out = [];
		document.querySelectorAll('.tiny-option-label').forEach(el => {
			const t = (el.innerText || '').trim();
			if (!t || t.length > 30) return;
			const r = el.getBoundingClientRect();
			if (r.width < 20 || r.height < 10) return;
			if (r.y < inY - 30 || r.y > inY + 500) return;
			out.push({text: t, x: Math.round(r.x + r.width/2),
			          y: Math.round(r.y + r.height/2)});
		});
		return out;
	}`, box.Y)
	if err != nil {
		return "", err
	}
	list, _ := opts.([]any)
	if len(list) == 0 {
		_ = page.Keyboard().Press("Escape")
		return "", fmt.Errorf("%s 没有可选项", placeholder)
	}

	pick := list[0].(map[string]any)
	if want != "" {
		for _, it := range list {
			m := it.(map[string]any)
			if m["text"] == want {
				pick = m
				break
			}
		}
	}
	if err := page.Mouse().Click(toFloat(pick["x"]), toFloat(pick["y"])); err != nil {
		return "", err
	}
	page.WaitForTimeout(1200)
	return fmt.Sprint(pick["text"]), nil
}

func toFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	}
	return 0
}

// formErrors 收集当前表单上的校验错误提示。
func formErrors(page playwright.Page) []string {
	v, err := page.Evaluate(`() => {
		const out = new Set();
		document.querySelectorAll('*').forEach(el => {
			const t = (el.innerText || '').trim();
			if (!/^请(输入|选择)/.test(t) || t.length > 25) return;
			const r = el.getBoundingClientRect();
			if (r.width < 30 || r.height < 10) return;
			out.add(t);
		});
		return Array.from(out);
	}`)
	if err != nil {
		return nil
	}
	raw, _ := v.([]any)
	out := make([]string, 0, len(raw))
	for _, x := range raw {
		out = append(out, fmt.Sprint(x))
	}
	return out
}

// RunEventTask 报名开发者赛事并提交作品。
func RunEventTask(opts EventOptions) (map[string]any, error) {
	if opts.Timeout <= 0 {
		opts.Timeout = 300
	}
	if err := browser.EnsureDriver(); err != nil {
		return nil, err
	}
	pw, err := playwright.Run()
	if err != nil {
		return nil, err
	}
	defer pw.Stop()

	br, ctx, err := browser.NewStorageContextUA(pw, config.CookieFile(), opts.Headless, config.DefaultUA)
	if err != nil {
		return nil, err
	}
	defer br.Close()

	page, err := ctx.NewPage()
	if err != nil {
		return nil, err
	}

	// 1. 赛事列表
	if _, err := page.Goto("https://developer.huaweicloud.com/competition/list",
		playwright.PageGotoOptions{WaitUntil: playwright.WaitUntilStateDomcontentloaded,
			Timeout: playwright.Float(60000)}); err != nil {
		return nil, err
	}
	page.WaitForTimeout(10000)

	// 找第一个「立即报名」
	pos, err := page.Evaluate(`() => {
		const hits = [];
		document.querySelectorAll('*').forEach(el => {
			const t = (el.innerText || '').trim();
			if (t !== '立即报名') return;
			const r = el.getBoundingClientRect();
			if (r.width < 60 || r.height < 25 || r.y < 600) return;
			hits.push({x: Math.round(r.x + r.width/2), y: Math.round(r.y + r.height/2)});
		});
		return hits[0] || null;
	}`)
	if err != nil {
		return nil, err
	}
	m, _ := pos.(map[string]any)
	if m == nil {
		return map[string]any{"ok": true, "msg": "没有可报名的赛事"}, nil
	}
	if err := page.Mouse().Click(toFloat(m["x"]), toFloat(m["y"])); err != nil {
		return nil, err
	}
	page.WaitForTimeout(9000)

	// 点击「立即报名」会**打开新标签页**跳到 raceEnrollment/<id>，
	// 原页面不动。早先的代码继续用原来的 page，于是：
	//   - enrollURL 拿到的是列表页地址
	//   - 后面所有表单操作都在列表页上找，必然报「找不到 请选择职业」
	//
	// 这里把新标签页接过来。找不到新页面就退回原页面
	// （某些赛事可能改成同页弹窗）。
	if pages := ctx.Pages(); len(pages) > 1 {
		enrollPage := pages[len(pages)-1]
		_ = enrollPage.WaitForLoadState(playwright.PageWaitForLoadStateOptions{
			State: playwright.LoadStateDomcontentloaded,
		})
		enrollPage.WaitForTimeout(3000)
		if strings.Contains(enrollPage.URL(), "raceEnrollment") {
			page = enrollPage
		}
	}

	enrollURL := page.URL()
	fmt.Printf("[*] 报名页: %s\n", enrollURL)

	if opts.DryRun {
		return map[string]any{"dry_run": true, "enroll_url": enrollURL}, nil
	}

	// 2. 填报名表
	if _, err := pickOption(page, "请选择职业", eventOccupation); err != nil {
		return nil, fmt.Errorf("选职业失败: %w", err)
	}
	_, _ = pickOption(page, "请选择行业", "")
	page.WaitForTimeout(1200)

	for ph, val := range eventFillText {
		inp := page.Locator(fmt.Sprintf(`input[placeholder="%s"]`, ph))
		if count(inp) == 0 {
			continue
		}
		_ = inp.First().Fill(val)
		page.WaitForTimeout(300)
	}
	_, _ = pickOption(page, "请选择省份", "")
	page.WaitForTimeout(1200)
	_, _ = pickOption(page, "请选择学校", "")
	page.WaitForTimeout(1200)

	// 3. 提交报名
	btn := page.Locator(`button:has-text("报名")`)
	if count(btn) == 0 {
		return nil, fmt.Errorf("找不到报名按钮")
	}
	if err := btn.Last().Click(); err != nil {
		return nil, err
	}
	page.WaitForTimeout(8000)

	if errs := formErrors(page); len(errs) > 0 {
		return nil, fmt.Errorf("报名表单还有必填项未填: %v", errs)
	}

	body, _ := page.Locator("body").InnerText()
	signedUp := strings.Contains(body, "报名成功")

	// 4. 去提交作品页
	subURL := strings.Replace(enrollURL, "/raceEnrollment/", "/information/", 1) + "/submission"
	if _, err := page.Goto(subURL, playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateDomcontentloaded,
		Timeout:   playwright.Float(60000),
	}); err != nil {
		return nil, err
	}
	page.WaitForTimeout(10000)

	_, _ = pickOption(page, "请选择阶段组", "")
	page.WaitForTimeout(1200)
	_, _ = pickOption(page, "请选择阶段", "")
	page.WaitForTimeout(1200)

	// 5. 上传作品 zip
	zipPath := opts.WorkZip
	if zipPath == "" {
		zipPath = filepath.Join(config.StateDir(), "work_upload.zip")
	}
	if _, err := os.Stat(zipPath); err != nil {
		return nil, fmt.Errorf("作品文件不存在: %s", zipPath)
	}

	up := page.GetByText("上传作品", playwright.PageGetByTextOptions{Exact: playwright.Bool(true)})
	if count(up) > 0 {
		_ = up.First().Click()
		page.WaitForTimeout(4000)
	}
	fi := page.Locator(`input[type=file]`)
	if count(fi) == 0 {
		return nil, fmt.Errorf("找不到文件上传框")
	}
	if err := fi.First().SetInputFiles(zipPath); err != nil {
		return nil, err
	}
	page.WaitForTimeout(6000)

	// 确认按钮
	for _, label := range []string{"确定", "提交", "确认上传", "上传"} {
		b := page.GetByText(label, playwright.PageGetByTextOptions{Exact: playwright.Bool(true)})
		if count(b) == 0 {
			continue
		}
		if err := b.First().Click(playwright.LocatorClickOptions{Timeout: playwright.Float(6000)}); err == nil {
			page.WaitForTimeout(6000)
			break
		}
	}
	page.WaitForTimeout(4000)

	final, _ := page.Locator("body").InnerText()
	uploaded := strings.Contains(final, "评分中") || strings.Contains(final, filepath.Base(zipPath))

	return map[string]any{
		"ok":        true,
		"signed_up": signedUp,
		"uploaded":  uploaded,
		"enroll_url": enrollURL,
		"sub_url":   subURL,
		"zip":       zipPath,
		"time":      time.Now().Format("2006-01-02 15:04:05"),
	}, nil
}
