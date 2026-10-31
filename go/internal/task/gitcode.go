package task

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/playwright-community/playwright-go"

	"hwcloud/internal/browser"
	"hwcloud/internal/config"
)

var sectionKeys = []string{"背景与问题", "现状核实", "建议方案", "预期收益"}

// LoginGitCode 被动轮询 localStorage，不做页面跳转或刷新。
func LoginGitCode(timeoutSec int) (map[string]any, error) {
	if err := browser.EnsureDriver(); err != nil {
		return nil, err
	}
	pw, err := playwright.Run()
	if err != nil {
		return nil, err
	}
	defer pw.Stop()

	if err := os.MkdirAll(config.GitCodeProfileDir(), 0o755); err != nil {
		return nil, err
	}
	// UA 必须是 Chrome 的（见 LoginGitCodeAuto 的说明），否则 WAF 418。
	ctx, err := browser.LaunchPersistentUA(pw, config.GitCodeProfileDir(), false, config.DefaultUA)
	if err != nil {
		return nil, err
	}
	defer ctx.Close()

	var page playwright.Page
	if pages := ctx.Pages(); len(pages) > 0 {
		page = pages[0]
	} else {
		page, err = ctx.NewPage()
		if err != nil {
			return nil, err
		}
	}
	if _, err := page.Goto("https://gitcode.com/login", playwright.PageGotoOptions{WaitUntil: playwright.WaitUntilStateDomcontentloaded}); err != nil {
		return nil, err
	}
	fmt.Println("[*] 请登录你已绑定华为云账号的 GitCode 账号（首次使用需先在实践中心完成绑定）。")
	fmt.Println("[*] 本窗口不会自动刷新或跳转，登录完成后自动保存会话。")

	deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)
	var rawUser any
	for time.Now().Before(deadline) {
		if page.IsClosed() {
			return nil, fmt.Errorf("GitCode 登录窗口被关闭")
		}
		if v, err := page.Evaluate("() => localStorage.getItem('userInfo')"); err == nil && v != nil {
			if s, ok := v.(string); ok && s != "" {
				rawUser = s
				break
			}
		}
		if v, err := page.Evaluate("() => localStorage.getItem('refresh_token')"); err == nil && v != nil {
			if s, ok := v.(string); ok && s != "" {
				break
			}
		}
		time.Sleep(2 * time.Second)
	}
	if rawUser == nil {
		return nil, fmt.Errorf("超时未检测到 GitCode 登录态")
	}

	time.Sleep(2 * time.Second)
	if _, err := ctx.StorageState(playwright.BrowserContextStorageStateOptions{
		Path: playwright.String(config.GitCodeStateFile()),
	}); err != nil {
		return nil, err
	}
	user := map[string]any{}
	if s, ok := rawUser.(string); ok {
		_ = json.Unmarshal([]byte(s), &user)
	}
	username := ""
	if v, err := page.Evaluate("() => localStorage.getItem('avatar-visited-username') || ''"); err == nil {
		if s, ok := v.(string); ok {
			username = s
		}
	}
	result := map[string]any{"id": user["id"], "username": username}
	fmt.Printf("[+] GitCode 登录成功：%v\n", username)
	fmt.Printf("[+] 会话已保存：%s\n", config.GitCodeStateFile())
	return result, nil
}

func ParseIssueSections(text string) map[string]string {
	sections := map[string]string{}
	current := ""
	var buf []string
	flush := func() {
		if current != "" {
			sections[current] = strings.TrimSpace(strings.Join(buf, "\n"))
		}
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "## ") {
			flush()
			current = strings.TrimSpace(strings.TrimPrefix(line, "## "))
			buf = nil
			continue
		}
		buf = append(buf, line)
	}
	flush()
	if len(sections) == 0 {
		sections["背景与问题"] = strings.TrimSpace(text)
	}
	return sections
}

func toInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	default:
		return 0
	}
}

// CreateIssue 在 eduSwarm 仓库创建 Issue。
//
// headless 参数保留但**不再用于无头模式**：实测 GitCode 对无头浏览器
// 做了行为判定——访问 /issues/create 会被 302 回 issues 列表，
// 表现成「未进入 Issue 编辑页，会话可能失效」，误导排查。
// 服务器有 Xvfb（DISPLAY=:99），统一走有头模式。
func CreateIssue(title, body, template string, headless, dryRun bool) (map[string]any, error) {
	_ = headless
	headless = false
	state := config.GitCodeStateFile()
	if _, err := os.Stat(state); err != nil {
		return nil, fmt.Errorf("未找到 GitCode 会话，请先运行 `hwcloud login-gitcode`")
	}
	if err := browser.EnsureDriver(); err != nil {
		return nil, err
	}
	pw, err := playwright.Run()
	if err != nil {
		return nil, err
	}
	defer pw.Stop()

	br, ctx, err := browser.NewStorageContextUA(pw, state, headless, config.DefaultUA)
	if err != nil {
		return nil, err
	}
	defer br.Close()

	page, err := ctx.NewPage()
	if err != nil {
		return nil, err
	}

	// 先访问首页，让前端用 refresh_token 换新 access_token。
	//
	// 为什么必须这一步：
	//   保存的 storage_state 里 access_token 的有效期只有 ~24 小时，
	//   过期后直接打开 /issues/create 会被 302 回 issues 列表，
	//   前端看起来像「会话失效」，实际 refresh_token 还有效（能用到几个月后）。
	//   GitCode 前端在页面加载时会自动调
	//   GET /uc/api/v1/user/oauth/token 刷新，所以先落地首页即可。
	//   实测：不访问首页直接进新建页 100% 失败；访问首页后立刻成功。
	if _, err := page.Goto("https://gitcode.com", playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateDomcontentloaded,
		Timeout:   playwright.Float(60000),
	}); err != nil {
		return nil, err
	}
	page.WaitForTimeout(6000)

	if _, err := page.Goto(config.GitCodeIssuesNew, playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateDomcontentloaded,
		Timeout:   playwright.Float(60000),
	}); err != nil {
		return nil, err
	}
	page.WaitForTimeout(6000)

	// 等跳转完成：/issues/create → /issues/create/choose（模板选择页）。
	//
	// 坑：这一步是前端路由跳转，NetworkIdle 之后还要再等 JS 渲染。
	// 实测 6 秒常常不够，页面还停在 /issues 列表，于是误报「会话失效」。
	// 所以这里轮询等，直到 URL 变成 choose 或出现标题输入框。
	for i := 0; i < 30; i++ {
		u := page.URL()
		if strings.Contains(u, "choose") || strings.Contains(u, "issues/create") {
			break
		}
		if count(page.Locator(`input[placeholder*="标题"], input[placeholder*="title" i]`)) > 0 {
			break
		}
		page.WaitForTimeout(1000)
	}
	fmt.Printf("[*] Issue 页面: %s\n", page.URL())

	// 模板选择页：页面列出仓库里配置的 Issue 模板，必须点「使用该模板」才能进编辑器。
	// 实测按钮顺序：0=Bug 报告，1=功能建议，后面是「打开查看」类文档链接。
	//
	// 必须用 button:has-text() 定位，不能用 GetByText(...Exact)。页面上
	// 「使用该模板」这串文字同时出现在外层 DIV 和内层 STRONG 上，GetByText
	// 会命中 DIV —— 点它不触发任何事件，页面原地不动，最后报「未进入
	// Issue 编辑页」，看起来像会话失效，实际是点错了元素。
	if strings.Contains(page.URL(), "choose") {
		index := 1
		if template == "bug" {
			index = 0
		}
		buttons := page.Locator(`button:has-text("使用该模板")`)
		n := count(buttons)
		if n == 0 {
			// 语言/改版兜底
			buttons = page.Locator(`button:has-text("Use this template")`)
			n = count(buttons)
		}
		if n == 0 {
			return nil, fmt.Errorf("模板选择页没有「使用该模板」按钮（页面可能改版）")
		}
		if index >= n {
			index = 0
		}
		if err := buttons.Nth(index).Click(); err != nil {
			return nil, err
		}
		// 点完还要等编辑器渲染
		for i := 0; i < 20; i++ {
			if count(page.Locator(`input[placeholder*="标题"], input[placeholder*="title" i]`)) > 0 {
				break
			}
			page.WaitForTimeout(1000)
		}
	}

	// 标题输入框的 placeholder 中英文都见过，按语义兜底。
	titleInput := page.Locator(`input[placeholder="请填写 Issue 标题"], input[placeholder="Please fill in the Issue title"], input[placeholder*="标题"], input[placeholder*="title" i]`)
	if count(titleInput) == 0 {
		return nil, fmt.Errorf("未进入 Issue 编辑页（当前 %s），GitCode 会话可能已失效或入口改版", page.URL())
	}
	if err := titleInput.Fill(title); err != nil {
		return nil, err
	}
	page.WaitForTimeout(800)

	sections := ParseIssueSections(body)
	editors, err := page.Locator(".cm-content").All()
	if err != nil {
		return nil, err
	}
	for i, key := range sectionKeys {
		text := sections[key]
		if text == "" || i >= len(editors) {
			continue
		}
		editor := editors[i]
		if err := editor.Click(); err != nil {
			continue
		}
		page.WaitForTimeout(200)
		_ = page.Keyboard().Press("Control+A")
		if err := page.Keyboard().InsertText(text); err != nil {
			return nil, err
		}
		page.WaitForTimeout(300)
	}

	if dryRun {
		return map[string]any{"dry_run": true, "title": title, "editors": len(editors), "template": template}, nil
	}

	// 注意：playwright-go 的 ExpectResponse 只接受 string / *regexp.Regexp，
	// 传 func(Response) bool 会在 newURLMatcher 里 panic（invalid urlOrPredicate）。
	//
	// 真实提交端点是
	//   POST https://web-api.gitcode.com/issuepr/api/v1/issue/<owner>%2F<repo>/issues
	// （注意末尾的 /issues，且仓库名是 URL 编码过的）。
	// 早先按 `**/issuepr/api/v1/issue**` 匹配会先命中列表 GET 接口
	// （.../issue/list → {all,closed,issues,opened}），拿到的是空列表。
	//
	// 另：Issue 一旦有参与者（评论、被指派、被 @）就**无法删除**，DELETE 返回
	// 400 {"error_message":"There are other participants"}，页面上「删除 issue」
	// 按钮也会变成 disabled。误发的 Issue 只能 PUT {state_event:"close"} 关掉。
	// 这也是为什么要靠 weeklyMissionDone 先查重 —— 发出去就收不回来了。
	resp, err := page.ExpectResponse(regexp.MustCompile(`/issuepr/api/v1/issue/[^/]+/issues(\?|$)`), func() error {
		return clickIssueSubmit(page)
	}, playwright.PageExpectResponseOptions{Timeout: playwright.Float(90000)})
	if err != nil {
		return nil, err
	}
	var data map[string]any
	if err := resp.JSON(&data); err != nil {
		return nil, err
	}
	iid := toInt(data["iid"])
	// 提交成功后前端会跳到 /issues/<iid>。响应体结构不稳定（有时包一层 data），
	// iid 拿不到就从 URL 里捞，避免「提交成功却报 iid=0」。
	if iid == 0 {
		if m := regexp.MustCompile(`/issues/(\d+)`).FindStringSubmatch(page.URL()); len(m) == 2 {
			iid = toInt(m[1])
		}
	}
	if iid == 0 {
		if inner, ok := data["data"].(map[string]any); ok {
			iid = toInt(inner["iid"])
		}
	}
	result := map[string]any{"iid": iid, "response": data}
	if iid > 0 {
		result["url"] = fmt.Sprintf("https://gitcode.com/%s/issues/%d", config.GitCodeRepo, iid)
	}
	return result, nil
}
// clickIssueSubmit 点击 Issue 编辑页的「提交」按钮。
//
// 为什么不能写死一个文案：GitCode 按浏览器语言渲染界面，
// 服务器上拿到的是**英文**（实测 2026-11：按钮是 "Create issue"），
// 而早先的代码只匹配中文「提交 Issue」，于是必然超时 ——
// 报错是 `waiting for getByText('提交 Issue')`，看着像页面没加载，
// 实际是文案对不上。
//
// 按优先级依次尝试：
//  1. 精确文案（中/英，常见几种写法）
//  2. 提交按钮的常见 class / type 特征
//  3. 编辑器区域里最后一个主按钮（提交通常在最右/最下）
func clickIssueSubmit(page playwright.Page) error {
	// 1. 文案匹配。顺序很重要：**精确的排前面，短词排最后**。
	//
	// 实测过的真实文案（2026-11，GitCode 按浏览器语言渲染）：
	//   英文模板页： "Submit your issue"   ← 实际用的这个
	//   英文列表页： "New issue" / "Create issue"
	//   中文站：     "提交 Issue" / "创建 Issue"
	//
	// 注意 "Submit your issue" 不是 "Submit issue"，多了 "your"，
	// 所以不能只写 "Submit issue"。
	//
	// 短词（"Submit" / "提交"）放最后：has-text 是子串匹配，
	// 短词会误中 "Submit your issue" 之外的按钮，只在前面全失败时才用。
	texts := []string{
		"Submit your issue",
		"Create issue", "Create Issue", "New issue",
		"Submit issue",
		"提交 Issue", "提交Issue", "创建 Issue", "创建Issue",
		"Submit", "提交",
	}
	for _, t := range texts {
		btn := page.Locator(`button:has-text("` + t + `")`).Last()
		if n, err := btn.Count(); err == nil && n > 0 {
			if err := btn.Click(playwright.LocatorClickOptions{Timeout: playwright.Float(8000)}); err == nil {
				return nil
			}
		}
	}

	// 2. 按结构特征找：提交按钮一般是表单里的 primary 按钮。
	//    devui 是 GitCode 用的组件库，主按钮带 devui-button--solid。
	selectors := []string{
		`button.devui-button--solid:not([disabled])`,
		`form button[type="submit"]:not([disabled])`,
		`.issue-editor button.btn-primary:not([disabled])`,
	}
	for _, sel := range selectors {
		btn := page.Locator(sel).Last()
		if n, err := btn.Count(); err == nil && n > 0 {
			if err := btn.Click(playwright.LocatorClickOptions{Timeout: playwright.Float(5000)}); err == nil {
				return nil
			}
		}
	}

	// 3. 最后兜底：把页面上的候选按钮列出来，方便下次排障时看真实文案
	dump := ""
	if all, err := page.Locator(`button`).All(); err == nil {
		var sb strings.Builder
		for i, b := range all {
			if i >= 25 {
				break
			}
			if t, err := b.InnerText(); err == nil {
				t = strings.TrimSpace(t)
				if t != "" && len([]rune(t)) < 30 {
					sb.WriteString("[" + t + "] ")
				}
			}
		}
		dump = sb.String()
	}
	return fmt.Errorf("找不到 Issue 提交按钮（页面 %s）。页面上的按钮：%s", page.URL(), dump)
}


//
// 为什么需要它：
//   LoginGitCode 只是打开登录页然后被动等用户手动操作，适合首次交互。
//   但 GitCode 的 access token 会不定期被服务端作废（cookie 的 expires
//   还写着 2027 年，实际 401 了），这时就需要重新登录。
//   每次都人工扫码太麻烦，这里改成自动填账密。
//
// 登录页结构（2026-10 实测）：
//   需要先点「密码登录」tab，才会出现账密表单：
//     input[placeholder="请填写用户名/邮箱"]
//     input[placeholder="请填写密码"]
//     button "登 录"
// FetchGitCodeUsername 从已保存的 GitCode 会话里读真实用户名。
//
// 为什么需要：GitCode **允许用手机号/邮箱登录**，所以用户在面板里
// 填手机号也能登录成功。但华为云的绑定接口要的是**用户名**
// （gitcode.com/<用户名> 里那段），传手机号会 404：
//
//	{"error":"gitcode user not found","username":"13005999962"}
//
// 用户名在好几个地方都有，最稳的是账号设置页：
//
//	https://gitcode.com/setting/account
//
// 页面上显示 "@wqa12130" 这样的文本，侧栏也有裸用户名。
// 这样用户只需填手机号+密码就能登录，用户名自动取，
// 不用他再去个人主页复制一遍。
//
// 返回的用户名**不带 @ 前缀** —— 华为云绑定接口要的是裸用户名。
func FetchGitCodeUsername() (string, error) {
	if err := browser.EnsureDriver(); err != nil {
		return "", err
	}
	pw, err := playwright.Run()
	if err != nil {
		return "", err
	}
	defer pw.Stop()

	state := config.GitCodeStateFile()
	if _, err := os.Stat(state); err != nil {
		return "", fmt.Errorf("未找到 GitCode 会话，请先登录")
	}

	br, ctx, err := browser.NewStorageContextUA(pw, state, true, config.DefaultUA)
	if err != nil {
		return "", err
	}
	defer br.Close()
	defer ctx.Close()

	page, err := ctx.NewPage()
	if err != nil {
		return "", err
	}

	if _, err := page.Goto("https://gitcode.com/setting/account", playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateDomcontentloaded,
		Timeout:   playwright.Float(45000),
	}); err != nil {
		return "", err
	}
	// 页面是 Vue 应用，@用户名要等 JS 渲染
	for i := 0; i < 20; i++ {
		page.WaitForTimeout(700)
		if n, err := page.Locator(`text=/^@[a-zA-Z0-9_-]+$/`).Count(); err == nil && n > 0 {
			break
		}
	}

	raw, err := page.Evaluate(`() => {
		const hits = [];
		document.querySelectorAll('*').forEach(el => {
			if (el.children.length > 2) return;          // 只看叶子节点
			const t = (el.innerText || '').trim();
			const m = t.match(/^@([a-zA-Z0-9_-]{2,40})$/);
			if (m) hits.push(m[1]);                       // 只取 @ 后面的部分
		});
		if (hits.length) return hits[0];

		// 兜底：账号设置页侧栏直接显示裸用户名。
		// 限定在 setting/account 页面，避免在别的页面误抓导航文字。
		if (!location.pathname.includes('/setting/account')) return '';
		const cands = [];
		document.querySelectorAll('div,span,a').forEach(el => {
			if (el.children.length > 0) return;
			const t = (el.innerText || '').trim();
			if (/^[a-zA-Z0-9_-]{3,40}$/.test(t)) cands.push(t);
		});
		return cands.length ? cands[0] : '';
	}`)
	if err != nil {
		return "", err
	}
	name, _ := raw.(string)
	// 兜底再剥一次 @：万一页面结构变化让 @ 混进来
	name = strings.TrimPrefix(strings.TrimSpace(name), "@")
	if name == "" {
		dump, _ := page.Locator("body").InnerText()
		return "", fmt.Errorf("没能从账号设置页读到用户名（URL: %s）。页面开头：%s",
			page.URL(), firstRunes(dump, 120))
	}
	return name, nil
}

// firstRunes 取字符串前 n 个 rune（避免按字节截断中文）。
func firstRunes(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return string(rs)
	}
	return string(rs[:n])
}

func LoginGitCodeAuto(account, password string, timeoutSec int) (map[string]any, error) {
	if err := browser.EnsureDriver(); err != nil {
		return nil, err
	}
	pw, err := playwright.Run()
	if err != nil {
		return nil, err
	}
	defer pw.Stop()

	if err := os.MkdirAll(config.GitCodeProfileDir(), 0o755); err != nil {
		return nil, err
	}
	// headless=false：GitCode 对无头浏览器风控更严，有头更容易过。
	//
	// UA 必须显式给 Chrome 的：config.UserAgent() 返回的是华为云登录时
	// 记下的 Firefox UA，套在 Chromium 上会 UA 与指纹不一致，
	// GitCode WAF 直接返回 418 "request has been intercepted"。
	ctx, err := browser.LaunchPersistentUA(pw, config.GitCodeProfileDir(), false, config.DefaultUA)
	if err != nil {
		return nil, err
	}
	defer ctx.Close()

	var page playwright.Page
	if pages := ctx.Pages(); len(pages) > 0 {
		page = pages[0]
	} else {
		if page, err = ctx.NewPage(); err != nil {
			return nil, err
		}
	}

	if _, err := page.Goto("https://gitcode.com/login",
		playwright.PageGotoOptions{WaitUntil: playwright.WaitUntilStateDomcontentloaded}); err != nil {
		return nil, err
	}
	// 页面是 Vue 应用，DOM 里 login-tab 要等 JS 渲染完才出现，
	// 实测 3 秒不够（偶发拿不到 tab），这里等到元素真正出现为止。
	for i := 0; i < 20; i++ {
		if n, err := page.Locator(".login-tab").Count(); err == nil && n > 0 {
			break
		}
		page.WaitForTimeout(500)
	}
	page.WaitForTimeout(1000)
	fmt.Printf("[*] GitCode 登录页：%s\n", page.URL())

	// 1. 切到「密码登录」tab
	//
	// 坑一：Playwright 的 text= / :has-text 在这个页面上点不动——
	// 这些 tab 是 <div class="login-tab">，没有 role、没有 href，
	// 而且外层有同名容器，选择器命中的不是可点元素。只能坐标点击。
	//
	// 坑二：GitCode 页面会按浏览器语言渲染，实测服务器上拿到的是
	// 英文界面（Miniprogram / SMS Login / Password login），
	// 只匹配中文「密码登录」会永远失败。所以这里两种语言都认，
	// 再不行就按 DOM 顺序取最后一个 tab（密码登录固定排第三）。
	switched := false
	if pos, err := page.Evaluate(`() => {
		const want = ['密码登录', 'password login', 'login with password'];
		const tabs = Array.from(document.querySelectorAll('.login-tab'))
			.filter(el => {
				const r = el.getBoundingClientRect();
				return r.width > 20 && r.height > 10;
			});
		let pick = null;
		for (const el of tabs) {
			const t = (el.innerText || '').trim().toLowerCase();
			if (want.some(w => t === w || t.includes(w))) { pick = el; break; }
		}
		if (!pick && tabs.length) pick = tabs[tabs.length - 1];
		if (!pick) return null;
		const r = pick.getBoundingClientRect();
		return {x: Math.round(r.x), y: Math.round(r.y),
		        w: Math.round(r.width), h: Math.round(r.height),
		        text: (pick.innerText || '').trim()};
	}`); err == nil {
		if m, ok := pos.(map[string]any); ok {
			x := toInt(m["x"]) + toInt(m["w"])/2
			y := toInt(m["y"]) + toInt(m["h"])/2
			if err := page.Mouse().Click(float64(x), float64(y)); err == nil {
				switched = true
				fmt.Printf("[*] 已点击登录方式：%v\n", m["text"])
			}
		}
	}
	if !switched {
		for _, sel := range []string{
			"div.login-tab:has-text(\"Password login\")",
			"div.login-tab:has-text(\"密码登录\")",
		} {
			if err := page.Locator(sel).First().Click(
				playwright.LocatorClickOptions{Timeout: playwright.Float(5000)}); err == nil {
				switched = true
				break
			}
		}
	}
	if !switched {
		// 打印现场，方便判断是改版还是没渲染完
		if dump, derr := page.Evaluate(`() => {
			const tabs = Array.from(document.querySelectorAll('.login-tab'))
				.map(el => (el.innerText || '').trim());
			return {url: location.href, title: document.title,
			        tabs: tabs, bodyLen: (document.body.innerText || '').length,
			        bodyHead: (document.body.innerText || '').slice(0, 300)};
		}`); derr == nil {
			fmt.Printf("[!] 登录页现场: %v\n", dump)
		}
		return nil, fmt.Errorf("找不到「密码登录」入口，GitCode 登录页可能改版")
	}
	page.WaitForTimeout(2500)

	// 2. 填账密。placeholder 也是双语的，按 input 类型兜底更稳。
	userSel := `input[placeholder="请填写用户名/邮箱"], input[placeholder="Please fill in Username/Email"], input.devui-input__inner[type="text"]`
	passSel := `input[placeholder="请填写密码"], input[placeholder="Please fill in Password"], input.devui-input__inner[type="password"]`
	if err := page.Locator(userSel).First().Fill(account); err != nil {
		return nil, fmt.Errorf("填用户名失败: %w", err)
	}
	if err := page.Locator(passSel).First().Fill(password); err != nil {
		return nil, fmt.Errorf("填密码失败: %w", err)
	}
	page.WaitForTimeout(600)

	// 3. 勾选两个协议复选框。
	//
	// 页面上有两个 devui-checkbox：
	//   y≈552  "By logging in or registering, you agree to User Agreement..."
	//   y≈596  "I agree to share my account, organization, repository information..."
	// 不勾选时点 Log In 什么都不会发生（前端直接 return，连网络请求都不发）。
	// 真正的 <input> 是 0x0 + opacity:0，点不到；要点 label/外层 div。
	// 用 el.click() 直接触发，比坐标更稳（不受滚动影响）。
	checked, cerr := page.Evaluate(`() => {
		const n = [];
		document.querySelectorAll('label, .devui-checkbox').forEach(el => {
			const r = el.getBoundingClientRect();
			if (r.width < 50 || r.height < 10) return;
			const t = (el.innerText || '').toLowerCase();
			if (!t.includes('agree')) return;
			const inp = el.querySelector('input[type=checkbox]');
			if (inp && inp.checked) return;
			el.click();
			n.push((el.innerText || '').trim().slice(0, 40));
		});
		return n;
	}`)
	if cerr != nil {
		fmt.Printf("[!] 勾选协议失败: %v\n", cerr)
	} else {
		fmt.Printf("[*] 已勾选协议: %v\n", checked)
	}
	page.WaitForTimeout(800)

	// 4. 点登录按钮。
	//
	// 页面上有三个候选：顶栏「登录」(64x32)、顶栏「注册」、表单里的
	// 「Log In」(398x40)。按文字匹配 + 取面积最大的那个，能稳定命中表单按钮。
	clicked := false
	if pos, err := page.Evaluate(`() => {
		const want = ['登录', '登 录', 'log in', 'login', 'sign in'];
		const cands = [];
		document.querySelectorAll('button').forEach(el => {
			const t = (el.innerText || '').trim().toLowerCase();
			const r = el.getBoundingClientRect();
			if (r.width < 40 || r.height < 20) return;
			if (!want.some(w => t === w || t.includes(w))) return;
			cands.push({x: Math.round(r.x), y: Math.round(r.y),
			            w: Math.round(r.width), h: Math.round(r.height),
			            area: r.width * r.height, text: (el.innerText||'').trim()});
		});
		if (!cands.length) return null;
		cands.sort((a, b) => b.area - a.area);
		return cands[0];
	}`); err == nil {
		if m, ok := pos.(map[string]any); ok {
			x := toInt(m["x"]) + toInt(m["w"])/2
			y := toInt(m["y"]) + toInt(m["h"])/2
			if err := page.Mouse().Click(float64(x), float64(y)); err == nil {
				clicked = true
				fmt.Printf("[*] 已点击登录按钮：%v\n", m["text"])
			}
		}
	}
	if !clicked {
		for _, sel := range []string{`button:has-text("Log In")`, `button:has-text("登")`} {
			if err := page.Locator(sel).Last().Click(
				playwright.LocatorClickOptions{Timeout: playwright.Float(5000)}); err == nil {
				clicked = true
				break
			}
		}
	}
	if !clicked {
		return nil, fmt.Errorf("找不到登录按钮")
	}

	// 4. 等登录完成。GitCode 登录后会把 userInfo 写进 localStorage。
	//    也可能弹滑块验证 —— 那种情况只能等人工，这里给出明确提示。
	deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)
	var rawUser any
	needCaptcha := false
	for time.Now().Before(deadline) {
		if page.IsClosed() {
			return nil, fmt.Errorf("登录窗口被关闭")
		}
		if v, err := page.Evaluate("() => localStorage.getItem('userInfo')"); err == nil && v != nil {
			if s, ok := v.(string); ok && s != "" {
				rawUser = s
				break
			}
		}
		// 检测滑块/验证码提示（页面可能中英文）
		if !needCaptcha {
			if n, err := page.Locator("text=验证, text=captcha, text=Verify, .captcha, [class*=captcha]").Count(); err == nil && n > 0 {
				needCaptcha = true
				fmt.Println("[!] 检测到验证码，请人工在 VNC 里完成（登录成功后会自动保存）")
			}
		}
		time.Sleep(2 * time.Second)
	}
	if rawUser == nil {
		if needCaptcha {
			return nil, fmt.Errorf("需要人工完成验证码，请用 VNC 连接 :99 手动完成")
		}
		return nil, fmt.Errorf("超时未检测到 GitCode 登录态（账密错误或被风控）")
	}

	time.Sleep(2 * time.Second)
	if _, err := ctx.StorageState(playwright.BrowserContextStorageStateOptions{
		Path: playwright.String(config.GitCodeStateFile()),
	}); err != nil {
		return nil, err
	}

	user := map[string]any{}
	if s, ok := rawUser.(string); ok {
		_ = json.Unmarshal([]byte(s), &user)
	}
	username := ""
	if v, err := page.Evaluate("() => localStorage.getItem('avatar-visited-username') || ''"); err == nil {
		if s, ok := v.(string); ok {
			username = s
		}
	}
	if username == "" {
		if s, ok := user["username"].(string); ok {
			username = s
		}
		if s, ok := user["login"].(string); ok && username == "" {
			username = s
		}
	}
	fmt.Printf("[+] GitCode 自动登录成功：%v\n", username)
	fmt.Printf("[+] 会话已保存：%s\n", config.GitCodeStateFile())
	return map[string]any{"id": user["id"], "username": username, "auto": true}, nil
}
