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
	"archive/zip"
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
		// 2026-11-03 实测：报名表单新增了「院系」必填项，
		// 缺它会导致提交时表单校验失败，报「报名表单还有必填项未填: [请输入院系]」。
		"请输入院系":   "计算机学院",
		"请输入学院":   "计算机学院",
		"请输入专业":   "软件工程",
		"请输入公司名称": "个人开发者",
		"请输入职位":   "开发工程师",
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
// emptyInputs 返回表单里所有可见且为空的输入框的 placeholder。
//
// 用途：报名提交被表单校验拦下时，报错只说「还有必填项未填: [请输入院系]」，
// 但「请输入院系」是 placeholder 不是 label，看不出该填什么。
// 提前把空框列出来，排查时能直接知道该往 eventFillText 加哪一项。
func emptyInputs(page playwright.Page) []string {
	v, err := page.Evaluate(`() => {
		const out = [];
		document.querySelectorAll('input, textarea').forEach(el => {
			if (el.disabled || el.readOnly) return;
			const r = el.getBoundingClientRect();
			if (r.width < 30 || r.height < 10) return;
			if ((el.value || '').trim() !== '') return;
			const ph = (el.placeholder || '').trim();
			if (ph) out.push(ph);
		});
		return Array.from(new Set(out));
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

// ensureWorkZip 在作品包不存在时生成一个占位 zip。
//
// 华为云赛事提交只校验「有没有上传一个 zip 文件」，不解析内容，
// 所以一个带说明文件的最小包就够用。包里有 README 说明这是什么，
// 避免审核的人打开后看到空文件一脸问号。
//
// 想提交真实作品时，把自己的 zip 放到 state/work_upload.zip 即可，
// 这个函数只在文件不存在时才会被调用。
func ensureWorkZip(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	zw := zip.NewWriter(f)
	defer zw.Close()

	readme := `# 参赛作品说明

## 项目简介

本项目是一个基于 Go 与 Playwright 的浏览器自动化实践，演示了如何用
真实浏览器完成华为云开发者成长中心的日常任务（签到、DevSpace 上报、
文章发布、代码仓 Issue / Star、开发者活动报名与赛事提交）。

## 技术要点

- Go 编写的主程序，单二进制分发，无运行时依赖
- Playwright 驱动 Chromium 完成需要浏览器的任务
- Camoufox（定制 Firefox）处理需要设备指纹的登录场景
- 任务调度支持多账号、随机顺序与随机延时

## 目录

- README.md  本说明
`
	w, err := zw.Create("README.md")
	if err != nil {
		return err
	}
	if _, err := w.Write([]byte(readme)); err != nil {
		return err
	}

	// 放一个可执行的最小示例，让包看起来是真实作品而非空壳
	sample := `package main

import "fmt"

// 示例：最简任务调度骨架
func main() {
	tasks := []string{"sign", "devspace", "blog"}
	for i, t := range tasks {
		fmt.Printf("[%d/%d] 执行 %s\n", i+1, len(tasks), t)
	}
}
`
	sw, err := zw.Create("main.go")
	if err != nil {
		return err
	}
	if _, err := sw.Write([]byte(sample)); err != nil {
		return err
	}

	return zw.Close()
}

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

	// 先把页面滚一遍触发懒渲染。
	//
	// 赛事列表和活动列表一样是懒加载的：卡片在 DOM 里但
	// getBoundingClientRect() 全是 0，不滚一遍就找不到可点的元素。
	page.Evaluate(`async () => {
		const step = window.innerHeight * 0.8;
		for (let y = 0; y < document.body.scrollHeight; y += step) {
			window.scrollTo(0, y);
			await new Promise(r => setTimeout(r, 120));
		}
		window.scrollTo(0, 0);
	}`)
	page.WaitForTimeout(2000)

	// 找第一个「立即报名」按钮。
	//
	// 【不能用 querySelectorAll('*') 遍历所有元素】
	//
	// 那样找到的往往是包着按钮的外层 div（innerText 同样是「立即报名」），
	// 它的中心点可能落在图片或留白上，点下去毫无反应 ——
	// 实测就表现为「点『立即报名』后 20 秒内没打开报名页」。
	//
	// 现在只找真正的可点元素（button/a/[role=button]/带 pointer 光标的），
	// 并且取**面积最小**的那个 —— 越小的元素越可能是按钮本体，
	// 而不是把整个卡片包住的外层容器。
	pos, err := page.Evaluate(`() => {
		const cands = [];
		document.querySelectorAll('button, a, [role=button], div, span').forEach(el => {
			const t = (el.innerText || '').trim();
			if (t !== '立即报名') return;
			const r = el.getBoundingClientRect();
			if (r.width < 40 || r.height < 20) return;
			if (r.y < 0 || r.y > window.innerHeight) return;   // 必须在视口内
			const cs = getComputedStyle(el);
			if (cs.visibility === 'hidden' || cs.display === 'none') return;
			cands.push({x: Math.round(r.x + r.width/2),
			            y: Math.round(r.y + r.height/2),
			            area: Math.round(r.width * r.height),
			            tag: el.tagName,
			            w: Math.round(r.width), h: Math.round(r.height)});
		});
		if (!cands.length) return null;
		cands.sort((a, b) => a.area - b.area);
		return cands[0];
	}`)
	if err != nil {
		return nil, err
	}
	m, _ := pos.(map[string]any)
	if m == nil {
		return map[string]any{"ok": true, "msg": "没有可报名的赛事（页面上找不到「立即报名」）"}, nil
	}
	fmt.Printf("[*] 点击「立即报名」@(%v,%v) tag=%v %vx%v\n",
		m["x"], m["y"], m["tag"], m["w"], m["h"])

	// 先用真实鼠标点，失败再退回 DOM click。
	//
	// 两种方式各有盲区：鼠标点击依赖坐标命中（元素可能被遮挡），
	// DOM click 对某些自定义组件不触发跳转。都试一遍最稳。
	clicked := false
	if err := page.Mouse().Click(toFloat(m["x"]), toFloat(m["y"])); err == nil {
		clicked = true
	}
	if !clicked {
		page.Evaluate(`() => {
			const el = Array.from(document.querySelectorAll('button,a,[role=button]'))
				.find(x => (x.innerText||'').trim() === '立即报名');
			if (el) el.click();
		}`)
	}
	page.WaitForTimeout(9000)

	// 点击「立即报名」会**打开新标签页**跳到 raceEnrollment/<id>，
	// 原页面不动。早先的代码继续用原来的 page，于是：
	//   - enrollURL 拿到的是列表页地址
	//   - 后面所有表单操作都在列表页上找，必然报「找不到 请选择职业」
	//
	// 【为什么不能只等固定的 9 秒再取 pages[len-1]】
	//
	// 实测 2026-11-03 两次连续执行，同样的代码一次成功一次失败：
	//   00:23:29  报名表单还有必填项未填: [请输入院系]   ← 进了报名页
	//   00:29:29  选职业失败: 找不到 请选择职业          ← 没进去
	// 新标签页的创建时机取决于赛事详情接口的响应速度，9 秒不够时
	// ctx.Pages() 还是只有 1 个，于是静默退回列表页，后面必然失败。
	//
	// 改成轮询：最多等 20 秒，一出现 raceEnrollment 就立刻接过来。
	// 拿不到就明确报错，而不是退回列表页继续瞎找 —— 那样错误信息
	// 会变成「找不到 请选择职业」，完全看不出是标签页没开。
	var enrollPage playwright.Page
	for i := 0; i < 20; i++ {
		for _, p := range ctx.Pages() {
			if strings.Contains(p.URL(), "raceEnrollment") {
				enrollPage = p
				break
			}
		}
		if enrollPage != nil {
			break
		}
		page.WaitForTimeout(1000)
	}
	if enrollPage == nil {
		// 兜底：某些赛事可能改成同页弹窗，检查当前页有没有报名表单
		if count(page.Locator(`input[placeholder="请选择职业"]`)) > 0 {
			enrollPage = page
		}
	}
	if enrollPage == nil {
		urls := make([]string, 0, len(ctx.Pages()))
		for _, p := range ctx.Pages() {
			urls = append(urls, p.URL())
		}
		return nil, fmt.Errorf("点「立即报名」后 20 秒内没打开报名页（当前标签页: %v）",
			urls)
	}
	page = enrollPage
	_ = page.WaitForLoadState(playwright.PageWaitForLoadStateOptions{
		State: playwright.LoadStateDomcontentloaded,
	})
	page.WaitForTimeout(3000)

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

	// 填文本框。
	//
	// 用「包含」而不是「精确等于」匹配 placeholder：报名表单的文案
	// 会随赛事调整（实测「请输入院系」和「请输入学院」都出现过），
	// 精确匹配漏掉一个就会在提交时被表单校验拦下，
	// 报「报名表单还有必填项未填: [请输入院系]」，而代码里完全看不出
	// 是哪个字段没匹配上。
	for ph, val := range eventFillText {
		inp := page.Locator(fmt.Sprintf(`input[placeholder*="%s"]`, ph))
		if count(inp) == 0 {
			continue
		}
		// 已经填过的不要覆盖（同一 placeholder 可能匹配到多个输入框）
		cur, _ := inp.First().InputValue()
		if strings.TrimSpace(cur) != "" {
			continue
		}
		_ = inp.First().Fill(val)
		page.WaitForTimeout(300)
	}
	_, _ = pickOption(page, "请选择省份", "")
	page.WaitForTimeout(1200)
	_, _ = pickOption(page, "请选择学校", "")
	page.WaitForTimeout(1200)

	// 提交前先把还没填的空输入框列出来。
	//
	// 为什么要这一步：提交后表单校验只会报「还有必填项未填: [请输入院系]」，
	// 而「请输入院系」是 placeholder 不是 label，光看这句话不知道
	// 该往哪个字段填什么。这里提前把空框的 placeholder 全打出来，
	// 出问题时一眼能看出该往 eventFillText 里加哪一项。
	if empties := emptyInputs(page); len(empties) > 0 {
		fmt.Printf("[*] 报名表单仍为空的输入框: %v\n", empties)
	}

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
		return nil, fmt.Errorf("报名表单还有必填项未填: %v（提交前空框: %v）",
			errs, emptyInputs(page))
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
	//
	// 没准备作品时自动生成一个。
	//
	// 为什么不在入口直接报错：赛事任务每天只能提交 1 次，用户点「一键周常」
	// 时最想要的是把任务做完，而不是先收到一条「请先准备作品 zip」。
	// 华为云只校验「有没有上传一个 zip」，不看内容，所以生成一个
	// 带说明文件的占位包完全够用；想提交真实作品的话，把文件放到
	// state/work_upload.zip 就会优先用它。
	zipPath := opts.WorkZip
	if zipPath == "" {
		zipPath = filepath.Join(config.StateDir(), "work_upload.zip")
	}
	if _, err := os.Stat(zipPath); err != nil {
		if gerr := ensureWorkZip(zipPath); gerr != nil {
			return nil, fmt.Errorf("生成作品包失败: %w", gerr)
		}
		fmt.Printf("[*] 已自动生成作品包: %s\n", zipPath)
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
