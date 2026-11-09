package task

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/playwright-community/playwright-go"

	"hwcloud/internal/config"
)

// 作品展览馆发布任务（每月一次，+200 积分）。
//
// 全流程走 AI DevSpace 的 AI Shell 对话，不直调接口：
//   1. 发创作口令 → AI 用 CodeArts 生成项目并启动服务
//   2. 发 npx 装 skill 命令 → AI 装 huawei-cloud-publish-work-to-gallery
//   3. AI 返回 GitCode OAuth 链接 → 本模块自动完成登录授权 → 回复「登录完成」
//   4. AI 列出可投稿活动 → 回复序号 1
//
// 关键设计：浏览器必须常驻。
// 早期实现每次 go run 都新建浏览器再关闭，AI Shell 正在执行的
// 「Monitor CodeArts progress」会被打成 tool execution cancelled，
// 整个任务随之中断。所以这里用独立 profile + --remote-debugging-port
// 起一个常驻 Chromium，后续只 ConnectOverCDP 接入，退出只断连接。

const (
	// 注意：端口和 profile 都按账号隔离。
	//
	// 之前 profile 是写死的 /opt/hwcloud-demo/_cdp_profile，导致不管点哪个
	// 账号的任务，浏览器里都是「第一个跑过的账号」的登录态 —— 实测用
	// A 号跑，实际打开的是 B 号的 AI 对话空间，作品会发到错误的账号上。
	galleryCDPPortDefault = "9222"
	galleryStateFile      = "/opt/hwcloud-demo/_probe/gallery_state.json"
)

// galleryProfileFor 按账号隔离浏览器 profile/端口，避免串号。
func galleryProfileFor(acct string) (profile string, port string) {
	profile = filepath.Join("/opt/hwcloud-demo/_cdp_profiles", acct)
	// 端口按账号稳定映射到 9222..9299，同一账号复用同一端口
	h := 0
	for _, r := range acct {
		h = h*31 + int(r)
	}
	if h < 0 {
		h = -h
	}
	port = fmt.Sprintf("%d", 9222+h%78)
	return
}

// GalleryOptions 控制发布流程的行为。
type GalleryOptions struct {
	// Prompt 是第 1 步发给 AI 的创作口令。为空时用默认的校园学习助手。
	Prompt string
	// ActivityIndex 是第 4 步选择的活动序号，默认 1。
	ActivityIndex int
	// Headless 仅用于判断是否要起 Xvfb，实际浏览器始终 headed（AI Shell 依赖真实渲染）。
	Headless bool
	// Timeout 是整个流程的总超时。
	Timeout time.Duration
}

// GalleryResult 是发布结果。
type GalleryResult struct {
	WorkID     string `json:"work_id"`
	WorkName   string `json:"work_name"`
	WorkURL    string `json:"work_url"`
	Activity   string `json:"activity"`
	Reward     string `json:"reward"`
	GitCodeURL string `json:"gitcode_url"`
	PreviewURL string `json:"preview_url"`
}

// RunGalleryPublish 执行完整的作品发布流程。
func RunGalleryPublish(opts GalleryOptions) (*GalleryResult, error) {
	// 不指定主题，让 AI 自己随机创作。
	//
	// 为什么要随机：作品展览馆有重复内容检测，同一个作品反复发布
	// 会被打回。把选题权交给 AI，每次生成的主题天然不同。
	if opts.Prompt == "" {
		// 关键：必须明确「别纠结、立刻开工」。
		//
		// 实测纯自由发挥会让 AI 陷入选题死循环：Particle Life →
		// Aurora → Liquid Identity → Pixel Civilization → Gravity Garden
		// 连换 5 个想法，几十分钟都没开始写代码，最后还纠结要不要用
		// CodeArts skill，整轮跑废。
		//
		// 还必须强制「工程化项目」而不是单个 HTML 文件：
		// 发布 skill 是按「项目目录」工作的（读 package.json / git 仓库）。
		// 实测让 AI 自由发挥时它写了个 particle-life/index.html 单文件，
		// 后续发「请将当前项目发布至开发者作品展览馆」时它找不到「项目」，
		// 就把这句话当成新需求，重新开始选题——流水线整个跑偏。
		//
		// 2026-11-09 压缩：DevSpace 容器只有 30 分钟，实测上一版（要求
		// package.json + src/ 多文件 + npm）光创作就花掉 8 分钟，留给发布
		// 流程只剩 22 分钟，来不及。现在改成「单 HTML + 可选 package.json」，
		// 目标 2-3 分钟出作品。发布 skill 只要能 git init + push 就行，
		// 不要求 npm 工程。
		opts.Prompt = "随机创作一个单文件 Web 作品，题材你定，要有新意。" +
			"【重要】不要权衡、不要列举备选、不要反复讨论，立刻选第一个点子开工。" +
			"【必须】在 /root/workspace 下新建项目目录，写 index.html（内联 CSS/JS），" +
			"再写一个 README.md，然后 git init 并提交一次。" +
			"不要用 npm、不要搭构建链、不要写多个源文件。" +
			"写完后用 python3 -m http.server 8000 启动并把访问地址告诉我。"
	}
	if opts.ActivityIndex <= 0 {
		opts.ActivityIndex = 1
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 90 * time.Minute
	}

	pw, err := playwright.Run()
	if err != nil {
		return nil, fmt.Errorf("启动 playwright 失败: %v", err)
	}
	defer pw.Stop()

	acct := config.AccountID()
	profile, port := galleryProfileFor(acct)
	logStep("使用账号 %s 的独立浏览器 profile（端口 %s）", acct, port)

	// 无论成功失败，函数返回前都关掉浏览器。
	// 常驻浏览器会累积标签吃光内存（实测堆到 1.7GB 后 CDP 直接超时），
	// 必须每轮结束都收干净。
	defer closeGalleryBrowser(port)

	if err := ensureGalleryBrowser(profile, port); err != nil {
		return nil, err
	}

	br, err := pw.Chromium.ConnectOverCDP("http://127.0.0.1:" + port)
	if err != nil {
		return nil, fmt.Errorf("CDP 连接失败: %v", err)
	}
	defer br.Close() // 只断连接，不关浏览器

	ctx := br.Contexts()[0]

	// 关键：先清掉别的账号的 GitCode 登录痕迹，再注入本账号的华为云 cookie。
	//
	// 两步顺序不能反，也不能省：
	//   - 不清 GitCode cookie → OAuth 会以旧账号身份直接通过，串号
	//   - 不注入华为云 cookie → 新 profile 打不开 DevSpace，落到登录页
	clearGitCodeCookies(ctx)
	if err := seedHuaweiCookies(ctx); err != nil {
		return nil, err
	}

	page, shell, err := openAIShell(ctx)
	if err != nil {
		return nil, err
	}

	deadline := time.Now().Add(opts.Timeout)

	// ---------- 步骤 1：发创作口令 ----------
	// 不需要手动新建会话：欢迎页的输入框直接用，AI Shell 会自动建会话。
	if err := ensureChatReady(page, shell, deadline); err != nil {
		return nil, fmt.Errorf("等待输入框失败: %v", err)
	}
	logStep("1/4 发送创作口令")
	if err := sendToAIShell(page, shell, opts.Prompt); err != nil {
		return nil, fmt.Errorf("发送创作口令失败: %v", err)
	}
	if err := waitForSignal(page, shell, []string{
		"预览地址", "预览链接", "服务已启动", "启动成功",
		"localhost:", "127.0.0.1:", "访问地址", "运行在",
	}, deadline); err != nil {
		return nil, fmt.Errorf("等待作品生成失败: %v", err)
	}
	logStep("1/4 完成：作品已生成并启动")

	// ---------- 步骤 2：装 skill + 发发布指令（必须同一条消息）----------
	//
	// 这两句必须合并在一条消息里发。分成两条会进排队队列
	// （用户实测截图看到「2 条排队消息」），而且 AI 收到纯命令时
	// 不一定知道要接着做什么。
	// 正确形态：
	//   npx skills add ... -y 2>&1;
	//   请将当前项目发布至开发者作品展览馆。
	logStep("2/4 装 skill 并发发布指令")
	// 合并成一条消息发送（用户明确要求这两句必须在同一句里）。
	//
	// 但只写两句话 AI 会跑偏：实测它装完 skill 又回头去写新作品，
	// 完全没进入发布流程。所以后面追加一段硬约束，明确告诉它
	// 装完就地执行发布、不要再写代码。
	installCmd := "npx skills add https://gitcode.com/Lingxi-HandsOn/gallery.git " +
		"--skill huawei-cloud-publish-work-to-gallery -y 2>&1;\n" +
		"请将当前项目发布至开发者作品展览馆。\n" +
		"（注意：作品已经做好了，不要再写代码、不要重新生成项目。" +
		"装完这个 skill 后，立刻按它的说明执行发布流程，" +
		"遇到需要登录 GitCode 就输出登录链接后停下来等我授权。）"
	if err := waitForInputIdle(page, shell, deadline); err != nil {
		return nil, fmt.Errorf("等待输入框就绪失败: %v", err)
	}
	if err := sendToAIShell(page, shell, installCmd); err != nil {
		return nil, fmt.Errorf("发送 skill+发布指令失败: %v", err)
	}
	logStep("2/4 已发送（命令 + 发布指令合并为一条）")

	// ---------- 步骤 3：等 AI 发起 GitCode OAuth，本机完成授权 ----------
	//
	// 2026-11-09 重写。这里不再「拿链接 → 代登录 → 回一句登录完成」就完，
	// 因为 skill 的 OAuth 是后端轮询模式（见 autoGitCodeLogin 的注释）：
	//   - AI 跑 gitcode-oauth.ensure.mjs --login，从 ACS 拿到 login_url
	//   - 我在本机浏览器打开 login_url，账密登录 + **点 Authorize**
	//   - 授权码送到 acs.atomgit.com/callback，容器内 client 轮询
	//     /auth/check 就能拿到 valid=true 和 token
	// 所以本机必须真的把「登录 + 授权」两步都做完，只做登录不做授权
	// 是旧版卡死的直接原因。
	loginURL, err := waitForOAuthURL(page, shell, deadline)
	if err != nil {
		return nil, fmt.Errorf("等待 GitCode 登录链接失败: %v", err)
	}
	logStep("3/4 拿到登录链接: %s", loginURL)

	if err := autoGitCodeLogin(ctx, loginURL); err != nil {
		return nil, fmt.Errorf("GitCode 授权失败: %v", err)
	}
	logStep("3/4 GitCode 授权完成")

	// cookie 仍写回账号目录，供 Star/Issue 等页面类任务复用。
	_ = saveGitCodeCookies(ctx)

	// 回一句让 AI 继续。措辞带「授权 / 完成」，因为 skill 在等这两类词。
	if err := waitForInputIdle(page, shell, deadline); err != nil {
		return nil, fmt.Errorf("等待输入框就绪失败: %v", err)
	}
	if err := sendToAIShell(page, shell, "登录完成，GitCode 授权已完成，请继续发布流程"); err != nil {
		return nil, fmt.Errorf("发送登录完成失败: %v", err)
	}

	// 等 AI 打出活动列表
	if err := waitForSignal(page, shell, []string{
		"请回复序号", "选择你要投稿的活动", "请选择活动", "活动编号",
	}, deadline); err != nil {
		return nil, fmt.Errorf("等待活动列表失败: %v", err)
	}
	logStep("3/4 完成：活动列表已返回")

	// ---------- 步骤 4：选择活动 ----------
	logStep("4/4 选择活动 %d", opts.ActivityIndex)
	if err := waitForInputIdle(page, shell, deadline); err != nil {
		return nil, fmt.Errorf("等待输入框就绪失败: %v", err)
	}
	if err := sendToAIShell(page, shell, fmt.Sprintf("%d", opts.ActivityIndex)); err != nil {
		return nil, fmt.Errorf("发送活动序号失败: %v", err)
	}

	if err := waitForSignal(page, shell, []string{
		"发布成功", "workId=", "积分领取请求已提交", "reward=success",
	}, deadline); err != nil {
		return nil, fmt.Errorf("等待发布结果失败: %v", err)
	}
	logStep("4/4 完成：作品已发布")

	res := parseGalleryResult(frameTextOf(shell))
	if res.WorkID == "" {
		return nil, fmt.Errorf("发布流程结束但未解析到 workId")
	}
	// 成功后立刻关掉浏览器，释放内存。
	//
	// 为什么要主动关：常驻浏览器每个标签 100-300MB，任务跑几轮后
	// 会堆到 1.7GB，把 1.7GB 总量的小机器压到只剩 70MB 可用，
	// 接下来 CDP 连接直接超时、整个任务失败（实测踩过）。
	closeGalleryBrowser(port)
	if b, err := json.MarshalIndent(res, "", "  "); err == nil {
		_ = os.WriteFile(galleryStateFile, b, 0o644)
	}
	return res, nil
}

// closeGalleryBrowser 关掉指定端口的浏览器，释放内存。
func closeGalleryBrowser(port string) {
	out, _ := runLocal("bash", "-lc",
		`pkill -f "remote-debugging-port=`+port+`" 2>/dev/null; sleep 1; echo done`)
	_ = out
	logStep("  已关闭浏览器（端口 %s）", port)
}

// ensureGalleryBrowser 确保常驻 Chromium 在跑。
func ensureGalleryBrowser(profile, port string) error {
	if out, _ := runLocal("curl", "-s", "-o", "/dev/null", "-w", "%{http_code}",
		"--max-time", "3", "http://127.0.0.1:"+port+"/json/version"); strings.TrimSpace(out) == "200" {
		return nil
	}
	chrome := findGalleryChrome()
	if chrome == "" {
		return fmt.Errorf("找不到 Chromium 可执行文件")
	}
	_ = os.MkdirAll(profile, 0o755)
	_ = os.MkdirAll("/opt/hwcloud-demo/_probe", 0o755)

	// setsid + nohup：脱离当前进程组，AI 任务不会随调用方退出而被杀
	cmd := fmt.Sprintf(
		`setsid nohup %q --remote-debugging-port=%s --remote-allow-origins='*' `+
			`--no-first-run --no-default-browser-check --no-sandbox --disable-dev-shm-usage `+
			`--user-data-dir=%s --window-size=1600,1000 about:blank `+
			`> /opt/hwcloud-demo/_probe/chrome.log 2>&1 < /dev/null &`,
		chrome, port, profile)
	if _, err := runLocal("bash", "-lc", "export DISPLAY=:99; "+cmd); err != nil {
		return fmt.Errorf("启动浏览器失败: %v", err)
	}

	for i := 0; i < 25; i++ {
		time.Sleep(time.Second)
		if out, _ := runLocal("curl", "-s", "-o", "/dev/null", "-w", "%{http_code}",
			"--max-time", "2", "http://127.0.0.1:"+port+"/json/version"); strings.TrimSpace(out) == "200" {
			return nil
		}
	}
	return fmt.Errorf("浏览器 CDP 端口未就绪")
}

func findGalleryChrome() string {
	for _, p := range []string{
		"/opt/hwcloud-demo/browsers/chromium-1228/chrome-linux64/chrome",
		"/opt/hwcloud-demo/browsers/chromium-1228/chrome-linux/chrome",
		"/usr/bin/google-chrome",
		"/usr/bin/google-chrome-stable",
		"/opt/google/chrome/chrome",
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func runLocal(name string, args ...string) (string, error) {
	c := exec.Command(name, args...)
	out, err := c.CombinedOutput()
	return string(out), err
}

// saveGitCodeCookies 把浏览器里的 GitCode/AtomGit cookie 存回账号目录。
//
// 用途：后续任务（Star 仓库、提 Issue）可以直接复用这次的登录态，
// 不用每次重走 OAuth。存到 state/gitcode_state.json，格式与
// browser.NewStorageContext 读取的 storage state 一致。
//
// ⚠ 2026-11-09 实践记录（这是个真踩过的坑）：
//   展览馆 OAuth 登录走的是 acs.atomgit.com → atomgit.com，登录成功后
//   ctx 里全是 .atomgit.com 域的 cookie。早期版本直接整份覆盖
//   state/gitcode_state.json，把原来 .gitcode.com 域的会话冲掉了。
//   结果：第二天 daily 的「鸿蒙 Star」步骤打开 gitcode.com 时是未登录
//   （页面右上角 Login/Register），在 40 多个候选仓库上空转 13 分钟，
//   最后由 systemd 超时杀掉 —— 表现为「daily 卡死」。
//
//   修法两件：
//     1. 域归一化：atomgit 与 gitcode 是同一套后端，cookie 名一致，
//        把 atomgit 域的 cookie 复制一份到 gitcode 域再存；
//     2. 合并而不是覆盖：读已有 state 文件，按 (name,domain,path) 去重
//        后并入，保留原有 gitcode 域会话。
func saveGitCodeCookies(ctx playwright.BrowserContext) error {
	cookies, err := ctx.Cookies()
	if err != nil {
		return err
	}
	type saved struct {
		Name     string  `json:"name"`
		Value    string  `json:"value"`
		Domain   string  `json:"domain"`
		Path     string  `json:"path"`
		Expires  float64 `json:"expires"`
		HttpOnly bool    `json:"httpOnly"`
		Secure   bool    `json:"secure"`
	}

	// atomgit 域 → gitcode 域映射。
	// 只在登录态 cookie 上做映射：登录态在 .atomgit.com 与 .gitcode.com
	// 之间是同一份 token，复制过去即可被 gitcode.com 页面识别。
	mapDomain := func(d string) string {
		switch {
		case d == ".atomgit.com":
			return ".gitcode.com"
		case d == "atomgit.com":
			return "gitcode.com"
		case d == "web-api.atomgit.com":
			return "web-api.gitcode.com"
		}
		return ""
	}

	seen := map[string]bool{}
	var out []saved
	add := func(s saved) {
		if s.Path == "" {
			s.Path = "/"
		}
		k := s.Name + "\x00" + s.Domain + "\x00" + s.Path
		if seen[k] {
			return
		}
		seen[k] = true
		out = append(out, s)
	}

	for _, c := range cookies {
		d := c.Domain
		if !strings.Contains(d, "gitcode") && !strings.Contains(d, "atomgit") {
			continue
		}
		add(saved{Name: c.Name, Value: c.Value, Domain: d,
			Path: c.Path, Expires: c.Expires, HttpOnly: c.HttpOnly, Secure: c.Secure})
		if nd := mapDomain(d); nd != "" {
			add(saved{Name: c.Name, Value: c.Value, Domain: nd,
				Path: c.Path, Expires: c.Expires, HttpOnly: c.HttpOnly, Secure: c.Secure})
		}
	}
	if len(out) == 0 {
		return nil
	}

	stateDir := filepath.Join(config.Root(), "accounts", config.AccountID(), "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(stateDir, "gitcode_state.json")

	// 合并已有 state：按 (name,domain,path) 去重，新 cookie 覆盖旧值。
	// 这样即使本次只登录了 atomgit 一侧，原有 gitcode 域会话也不会丢。
	if oldRaw, err := os.ReadFile(path); err == nil {
		var oldState struct {
			Cookies []saved `json:"cookies"`
		}
		if json.Unmarshal(oldRaw, &oldState) == nil {
			for _, c := range oldState.Cookies {
				add(c)
			}
		}
	}

	payload := map[string]any{"cookies": out, "origins": []any{}}
	b, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	// 单槽备份覆盖写，不按时间戳无限追加
	if old, err := os.ReadFile(path); err == nil {
		_ = os.WriteFile(path+".bak", old, 0o600)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return err
	}
	logStep("  已保存 %d 条 GitCode cookie 到账号目录（含 atomgit→gitcode 域映射）", len(out))
	return nil
}

// seedHuaweiCookies 把当前账号的华为云 cookie 注入浏览器。
//
// 为什么必须做：CDP profile 是按账号隔离的全新目录，里面没有任何
// 登录态，直接打开 devstation 会被弹到 auth.huaweicloud.com。
// 这里把 accounts/<id>/state/cookies.json 注进去，复用已登录的会话。
func seedHuaweiCookies(ctx playwright.BrowserContext) error {
	path := config.CookieFile()
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("账号没有 cookies.json，请先执行一次登录")
		}
		return err
	}
	var list []struct {
		Name    string  `json:"name"`
		Value   string  `json:"value"`
		Domain  string  `json:"domain"`
		Path    string  `json:"path"`
		Expires float64 `json:"expires"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return fmt.Errorf("解析 cookies.json 失败: %v", err)
	}
	var out []playwright.OptionalCookie
	for _, c := range list {
		if c.Name == "" || c.Value == "" {
			continue
		}
		oc := playwright.OptionalCookie{Name: c.Name, Value: c.Value}
		if c.Domain != "" {
			oc.Domain = playwright.String(c.Domain)
		}
		p := c.Path
		if p == "" {
			p = "/"
		}
		oc.Path = playwright.String(p)
		if c.Expires > 0 {
			oc.Expires = playwright.Float(c.Expires)
		}
		out = append(out, oc)
	}
	if len(out) == 0 {
		return fmt.Errorf("cookies.json 里没有有效 cookie")
	}
	if err := ctx.AddCookies(out); err != nil {
		return fmt.Errorf("注入 cookie 失败: %v", err)
	}
	logStep("  已注入 %d 条华为云 cookie", len(out))
	return nil
}

// clearGitCodeCookies 清掉浏览器里所有 GitCode/AtomGit 的登录痕迹。
//
// 为什么要清：同一个 CDP profile 可能被别的账号用过，或者上一次
// 会话残留了 atomgit 的登录 cookie。不清的话 OAuth 会以旧账号身份
// 直接通过，作品就发布到错误的账号上了（实测串号就是这么发生的）。
func clearGitCodeCookies(ctx playwright.BrowserContext) {
	cookies, err := ctx.Cookies()
	if err != nil {
		return
	}
	n := 0
	for _, c := range cookies {
		d := c.Domain
		if strings.Contains(d, "gitcode") || strings.Contains(d, "atomgit") {
			opts := playwright.BrowserContextClearCookiesOptions{
				Name:   playwright.String(c.Name),
				Domain: playwright.String(d),
			}
			if c.Path != "" {
				opts.Path = playwright.String(c.Path)
			}
			_ = ctx.ClearCookies(opts)
			n++
		}
	}
	if n > 0 {
		logStep("  已清理 %d 条旧 GitCode/AtomGit cookie", n)
	}
}

// openAIShell 打开 AI DevSpace 并连上 aishell iframe。
func openAIShell(ctx playwright.BrowserContext) (playwright.Page, playwright.Frame, error) {
	var page playwright.Page
	for _, p := range ctx.Pages() {
		if strings.Contains(p.URL(), "devstation") {
			page = p
			break
		}
	}
	if page == nil {
		pages := ctx.Pages()
		if len(pages) > 0 {
			page = pages[0]
		} else {
			var err error
			page, err = ctx.NewPage()
			if err != nil {
				return nil, nil, fmt.Errorf("新建页面失败: %v", err)
			}
		}
	}
	page.SetDefaultTimeout(180000)
	_ = page.BringToFront()

	// 诊断：打印所有标签的 URL，确认走不走成长中心
	{
		var us []string
		for _, p := range ctx.Pages() {
			us = append(us, clip(p.URL(), 60))
		}
		logStep("  当前标签 %d 个: %v", len(us), us)
		logStep("  选定页面: %s", clip(page.URL(), 80))
	}

	// 强制走成长中心：不管当前是什么页面，先清空所有标签再从成长中心开始。
	//
	// 为什么要清：实测浏览器 profile 会残留上次任务的标签（甚至多个
	// about:blank + 一个已经打开的 devstation 页面）。如果直接复用，
	// 代码会选中那个 devstation 标签并跳过成长中心，导致成长中心
	// 完全不记录这次发布行为，任务白跑。
	// 只关多余标签，**务必保留最后一个**。
	//
	// Chrome 关掉全部标签后会自我退出（CDP 连接断开），
	// 之后再 NewPage 必然报 "Target page, context or browser has been closed"。
	// 实测就是踩这个坑导致任务 10 秒退。
	pages := ctx.Pages()
	for i := len(pages) - 1; i >= 1; i-- {
		_ = pages[i].Close()
	}
	if len(pages) > 0 {
		page = pages[0]
		_, _ = page.Goto("about:blank", playwright.PageGotoOptions{
			WaitUntil: playwright.WaitUntilStateDomcontentloaded,
			Timeout:   playwright.Float(15000),
		})
	}
	page.SetDefaultTimeout(180000)

	if err := navigateFromGrowthCenter(ctx, page); err != nil {
		return nil, nil, err
	}
	if p2 := findDevstationPage(ctx); p2 != nil {
		page = p2
		page.SetDefaultTimeout(180000)
	}
	page.WaitForTimeout(6000)

	dismissGalleryDialogs(page)

	var shell playwright.Frame
	for i := 0; i < 20; i++ {
		for _, f := range page.Frames() {
			u := f.URL()
			if strings.Contains(u, "workspace.developer.huaweicloud.com") &&
				strings.Contains(u, "/aishell") {
				shell = f
				break
			}
		}
		if shell != nil {
			break
		}
		page.WaitForTimeout(4000)
	}
	if shell == nil {
		return nil, nil, fmt.Errorf("未找到 aishell iframe")
	}
	page.WaitForTimeout(3000)

	return page, shell, nil
}


// asBool 把 JS 返回值宽松转 bool。
//
// Playwright 的 Evaluate 在某些版本会把 true/false 序列化成字符串，
// 直接 v.(bool) 断言会失败 —— 表现就是「按钮明明点了，代码却认为没点」。
// 这里统一兼容 bool / 字符串 / 数字三种形态。
func asBool(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return x == "true" || x == "1"
	case float64:
		return x != 0
	}
	return false
}

// findDevstationPage 在所有标签里找 devstation 页面。
func findDevstationPage(ctx playwright.BrowserContext) playwright.Page {
	for _, p := range ctx.Pages() {
		if strings.Contains(p.URL(), "devstation") {
			return p
		}
	}
	return nil
}

// navigateFromGrowthCenter 走真实链路打开 AI DevSpace：
//
//	成长中心 → 找到「在开发者作品展览馆中发布作品」卡片 → 点「去完成」
//	→ 展览馆 submit-guide → 点「立即生成作品」→ AI DevSpace
//
// 每一步都校验落点，任一步失败就返回错误，由调用方决定是否回退。
func navigateFromGrowthCenter(ctx playwright.BrowserContext, page playwright.Page) error {
	logStep("  从成长中心进入（真实跳转链路）")

	if _, err := page.Goto("https://developer.huaweicloud.com/grow",
		playwright.PageGotoOptions{
			WaitUntil: playwright.WaitUntilStateDomcontentloaded,
			Timeout:   playwright.Float(90000),
		}); err != nil {
		return fmt.Errorf("打开成长中心失败: %v", err)
	}
	page.WaitForTimeout(10000)

	// 1) 找到展览馆任务卡片，点它的「去完成」
	clicked := false
	for round := 0; round < 3 && !clicked; round++ {
		res, _ := page.Evaluate(`() => {
			// 在所有元素里找「包含任务标题 + 去完成 按钮」的最小卡片
			let best = null, bestArea = 1e18;
			for (const e of document.querySelectorAll('*')) {
				const t = e.innerText || '';
				if (t.indexOf('在开发者作品展览馆中发布作品') < 0) continue;
				const r = e.getBoundingClientRect();
				if (r.width < 150 || r.width > 1200 || r.height < 40) continue;
				const area = r.width * r.height;
				if (area < bestArea) { bestArea = area; best = e; }
			}
			if (!best) return {ok:false, reason:'card-not-found'};
			for (const b of best.querySelectorAll('button,a,div,span')) {
				if ((b.innerText||'').trim() !== '去完成') continue;
				const r = b.getBoundingClientRect();
				if (r.width < 20 || r.height < 12) continue;
				b.scrollIntoView({block:'center'});
				b.click();
				return {ok:true, x:Math.round(r.x), y:Math.round(r.y)};
			}
			return {ok:false, reason:'button-not-found'};
		}`)
		if m, ok := res.(map[string]any); ok {
			if asBool(m["ok"]) {
				clicked = true
				logStep("  已点任务「去完成」")
				break
			}
			logStep("  未找到入口: %v", m["reason"])
		}
		page.WaitForTimeout(4000)
	}
	if !clicked {
		// 区分两种原因：会话失效 vs 本月已完成
		//
		// 会话失效时成长中心是空页面（跳登录或被拦），任务卡片自然找不到。
		// 这种情况必须明确报错让上层重新登录，而不是静默回退去直接开
		// DevSpace —— 那样会绕过成长中心的真实跳转链路，任务不被计数。
		body, _ := page.Locator("body").InnerText()
		u := page.URL()
		if strings.Contains(u, "auth.huaweicloud.com") || strings.Contains(u, "login") ||
			len(strings.TrimSpace(body)) < 200 {
			return fmt.Errorf("华为云会话已失效（成长中心页面为空/跳登录），需要重新登录")
		}
		if strings.Contains(body, "已完成") && !strings.Contains(body, "去完成") {
			return fmt.Errorf("本月「发布作品」任务已完成，无需重复执行")
		}
		return fmt.Errorf("成长中心未找到「在开发者作品展览馆中发布作品」的入口（页面 %d 字）", len(body))
	}
	page.WaitForTimeout(10000)

	// 2) 确认落在展览馆 submit-guide；没跳成就直接开
	target := page
	if !strings.Contains(target.URL(), "gallery.developer.huaweicloud.com") {
		// 可能开了新标签
		for _, p := range ctx.Pages() {
			if strings.Contains(p.URL(), "gallery") {
				target = p
				break
			}
		}
	}
	if !strings.Contains(target.URL(), "gallery") {
		logStep("  未跳到展览馆，直接打开 submit-guide")
		if _, err := target.Goto("https://gallery.developer.huaweicloud.com/gallery/submit-guide",
			playwright.PageGotoOptions{
				WaitUntil: playwright.WaitUntilStateDomcontentloaded,
				Timeout:   playwright.Float(90000),
			}); err != nil {
			return fmt.Errorf("打开 submit-guide 失败: %v", err)
		}
		target.WaitForTimeout(8000)
	}
	logStep("  当前页面: %s", clip(target.URL(), 90))

	// 3) 点「立即生成作品」，会跳到 AI DevSpace
	//
	// 注意：这个按钮在页面底部（实测 y≈995，视口 1000），
	// 直接 Click 会因「不在可视区域/被遮挡」失败。必须先滚到它再点。
	gen := false
	for round := 0; round < 5 && !gen; round++ {
		// 先用 JS 找到按钮并滚动到视口中央
		if v, err := target.Evaluate(`() => {
			const want = ['立即生成作品'];
			for (const e of document.querySelectorAll('a, button, div[role=button]')) {
				const t = (e.innerText||'').trim();
				if (!want.includes(t)) continue;
				e.scrollIntoView({block:'center', behavior:'instant'});
				return {found:true, y: Math.round(e.getBoundingClientRect().y)};
			}
			return {found:false};
		}`); err == nil {
			if m, ok := v.(map[string]any); ok && asBool(m["found"]) {
				logStep("  已滚动到「立即生成作品」（y=%v）", m["y"])
			}
		}
		target.WaitForTimeout(1500)

		// 再用 JS 直接点（绕过可见性判断）
		if v, err := target.Evaluate(`() => {
			const want = ['立即生成作品'];
			for (const e of document.querySelectorAll('a, button, div[role=button]')) {
				const t = (e.innerText||'').trim();
				if (!want.includes(t)) continue;
				for (const ev of ['pointerdown','mousedown','pointerup','mouseup','click']) {
					e.dispatchEvent(new MouseEvent(ev, {bubbles:true, cancelable:true, view:window}));
				}
				return true;
			}
			return false;
		}`); err == nil && asBool(v) {
			gen = true
			logStep("  已点「立即生成作品」")
			break
		}

		// 兜底：Playwright Click（滚动后再试）
		for _, sel := range []string{
			`a:has-text("立即生成作品")`,
			`text=立即生成作品`,
		} {
			l := target.Locator(sel).First()
			if c, _ := l.Count(); c == 0 {
				continue
			}
			_ = l.ScrollIntoViewIfNeeded()
			if err := l.Click(playwright.LocatorClickOptions{Timeout: playwright.Float(8000)}); err == nil {
				gen = true
				logStep("  已点「立即生成作品」（Playwright）")
				break
			}
		}
		if !gen {
			target.WaitForTimeout(4000)
		}
	}
	if !gen {
		return fmt.Errorf("submit-guide 未找到「立即生成作品」")
	}

	// 4) 等 devstation 页面出现（可能是新标签）
	for i := 0; i < 20; i++ {
		target.WaitForTimeout(3000)
		if findDevstationPage(ctx) != nil {
			logStep("  已进入 AI DevSpace")
			return nil
		}
	}
	return fmt.Errorf("点击后未出现 devstation 页面")
}

// dismissGalleryDialogs 处理 DevSpace 的四道开通/协议弹窗。
func dismissGalleryDialogs(page playwright.Page) {
	for round := 1; round <= 5; round++ {
		b, _ := page.Locator("body").InnerText()
		if !(strings.Contains(b, "立即开通") || strings.Contains(b, "临时凭据自动配置") ||
			(strings.Contains(b, "华为云开发者服务协议") && strings.Contains(b, "我已阅读并同意"))) {
			return
		}
		_, _ = page.Evaluate(`() => {
			document.querySelectorAll('label.tiny-checkbox').forEach(lb => {
				const i = lb.querySelector('input[type=checkbox]');
				if (i && !i.checked) lb.click();
			});
			document.querySelectorAll('input[type=checkbox]').forEach(i => {
				if (i.checked || i.closest('label.tiny-checkbox')) return;
				const lb = document.querySelector('label[for="' + i.id + '"]');
				if (lb) lb.click();
			});
		}`)
		page.WaitForTimeout(1000)
		_, _ = page.Evaluate(`() => {
			const w = ['同意','进入','立即开通'];
			for (const d of document.querySelectorAll('.tiny-dialog-box,.eco-protocol-content,[role=dialog]')) {
				if (d.getBoundingClientRect().width < 50) continue;
				for (const b of d.querySelectorAll('button,a')) {
					if (b.disabled || (b.className||'').toString().includes('is-disabled')) continue;
					if (w.includes((b.innerText||'').trim())) { b.click(); return; }
				}
			}
		}`)
		page.WaitForTimeout(10000)
	}
}

// dismissCWDialog 关掉可能挡住输入框的「新建会话 / 工作目录 (CWD)」弹窗。
//
// 什么时候会出现：AI Shell 没有活动会话时，直接往输入框发消息，
// 它会先弹「工作目录 (CWD)」让你确认，此时输入框被遮罩挡住，
// waitForInputIdle 会一直判定「不可发送」而死等。
//
// 处理方式：点「确认」接受默认目录 /root/workspace，让会话正常创建；
// 没有弹窗时立即返回。
func dismissCWDialog(shell playwright.Frame) bool {
	closed := false
	for i := 0; i < 3; i++ {
		v, err := shell.Evaluate(`() => {
			// 仅在「工作目录 / CWD」文案存在时才操作，避免误点别的确认框
			const bodyText = document.body.innerText || '';
			if (bodyText.indexOf('工作目录') < 0 && bodyText.indexOf('CWD') < 0) {
				return false;
			}
			for (const b of document.querySelectorAll('button')) {
				if ((b.innerText||'').trim() !== '确认') continue;
				const r = b.getBoundingClientRect();
				if (r.width < 20 || r.height < 10) continue;
				let p = b.parentElement, found = false;
				for (let j = 0; j < 6 && p; j++) {
					const pt = (p.innerText||'');
					if (pt.indexOf('工作目录') >= 0 || pt.indexOf('CWD') >= 0) { found = true; break; }
					p = p.parentElement;
				}
				if (!found) continue;
				b.click();
				return true;
			}
			return false;
		}`)
		if err == nil && asBool(v) {
			closed = true
			time.Sleep(3 * time.Second)
			continue
		}
		break
	}
	return closed
}

// ensureChatReady 只需要确认输入框可用，不需要手动新建会话。
//
// 用户实测：AI Shell 的欢迎页本身就有输入框，直接发消息会自动创建
// 会话。之前写「点新建会话 → 确认 CWD 弹窗」是多余的，而且这条路径
// 在 iframe 里极不稳定（按钮伪类匹配不到、弹层高度为 0、布尔返回值
// 被序列化成字符串），白白卡住整个流程。
//
// 现在只做一件事：等输入框出现。有历史会话就沿用，没有就让 AI Shell
// 自己建。
func ensureChatReady(page playwright.Page, shell playwright.Frame, deadline time.Time) error {
	for time.Now().Before(deadline) {
		var editor playwright.Locator
		for _, sel := range []string{
			`div[contenteditable=true]`,
			`[class*="min-h-11"][class*="overflow-y-auto"]`,
			`textarea`,
		} {
			locs := shell.Locator(sel)
			n, _ := locs.Count()
			for i := 0; i < n; i++ {
				el := locs.Nth(i)
				if vis, _ := el.IsVisible(); !vis {
					continue
				}
				box, _ := el.BoundingBox()
				if box == nil || box.Width < 200 {
					continue
				}
				editor = el
				break
			}
			if editor != nil {
				break
			}
		}
		if editor != nil {
			return nil
		}
		page.WaitForTimeout(3000)
	}
	return fmt.Errorf("输入框一直没出现")
}

// sendToAIShell 在 AI Shell 输入框里发消息。
func sendToAIShell(page playwright.Page, shell playwright.Frame, msg string) error {
	var editor playwright.Locator
	for _, sel := range []string{
		`div[contenteditable=true]`,
		`[class*="min-h-11"][class*="overflow-y-auto"]`,
		`textarea`,
	} {
		locs := shell.Locator(sel)
		n, _ := locs.Count()
		for i := 0; i < n; i++ {
			el := locs.Nth(i)
			if vis, _ := el.IsVisible(); !vis {
				continue
			}
			box, _ := el.BoundingBox()
			if box == nil || box.Width < 200 {
				continue
			}
			editor = el
			break
		}
		if editor != nil {
			break
		}
	}
	if editor == nil {
		return fmt.Errorf("找不到输入框")
	}
	if err := editor.Click(playwright.LocatorClickOptions{Timeout: playwright.Float(8000)}); err != nil {
		return err
	}
	page.WaitForTimeout(500)
	_ = editor.Fill("")
	if err := editor.Fill(msg); err != nil {
		if err2 := editor.PressSequentially(msg,
			playwright.LocatorPressSequentiallyOptions{Delay: playwright.Float(5)}); err2 != nil {
			return err2
		}
	}
	page.WaitForTimeout(1500)
	if val, _ := editor.InnerText(); strings.TrimSpace(val) == "" {
		return fmt.Errorf("输入框内容为空")
	}

	for _, bs := range []string{
		`button[class*=send]`, `[class*=send-btn]`,
		`button[aria-label*=发送]`, `button[type=submit]`,
	} {
		locs := shell.Locator(bs)
		n, _ := locs.Count()
		for i := 0; i < n; i++ {
			b := locs.Nth(i)
			if vis, _ := b.IsVisible(); !vis {
				continue
			}
			if en, _ := b.IsEnabled(); !en {
				continue
			}
			if err := b.Click(playwright.LocatorClickOptions{Timeout: playwright.Float(6000)}); err == nil {
				return nil
			}
		}
	}
	page.Keyboard().Press("Enter")
	return nil
}

// aiIsBusy 判断 AI 是否正在执行。
//
// 判据只能看 DOM 的发送/停止按钮状态，不能看文本里有没有「执行中」——
// 对话历史里会一直留着这些词，导致永远判忙或永远判闲。
// AI 执行中时输入框右侧是红色方形「停止」按钮；空闲时是纸飞机「发送」按钮。
func aiIsBusy(shell playwright.Frame) bool {
	// 主判据：存在停止按钮（红色方形，带 Square 图标或 aria-label=停止）
	if v, err := shell.Evaluate(`() => {
		// 停止按钮特征：aria-label/title 含「停止」，或 class 含 stop
		for (const b of document.querySelectorAll('button')) {
			const r = b.getBoundingClientRect();
			if (r.width < 10 || r.height < 10) continue;
			const aria = ((b.getAttribute('aria-label')||'') + ' ' + (b.title||'')).toLowerCase();
			const cls = ((b.className||'').toString()).toLowerCase();
			if (aria.indexOf('stop') >= 0 || aria.indexOf('停止') >= 0) return true;
			if (cls.indexOf('stop') >= 0) return true;
			// 红色背景的方形按钮（停止按钮），且内部有 rect/square
			const bg = getComputedStyle(b).backgroundColor || '';
			const isRed = /rgb\(\s*2[0-5]\d\s*,/.test(bg) && /,\s*[0-9]{1,2}\s*,/.test(bg);
			if (isRed && Math.abs(r.width - r.height) < 6) return true;
		}
		return false;
	}`); err == nil {
		if asBool(v) {
			return true
		}
	}
	return false
}

// waitForInputIdle 等 AI 上一轮真的结束、输入框可以键入。
//
// 修复过的错误：早先这里要求「存在可见且未 disabled 的按钮」，
// 但发送按钮在输入框为空时本来就是 disabled（灰色纸飞机），
// 于是这个条件永远不成立，流程在发第二条消息前死等 90 分钟。
//
// 正确判据：
//   1) 没有「停止」按钮（AI 不在执行）
//   2) 输入框 contenteditable 且可编辑（不是 disabled/readonly）
func waitForInputIdle(page playwright.Page, shell playwright.Frame, deadline time.Time) error {
	for time.Now().Before(deadline) {
		// 弹窗会挡住输入框，每次轮询都顺手关一下
		if dismissCWDialog(shell) {
			logStep("  已关闭工作目录弹窗")
		}
		if aiIsBusy(shell) {
			page.WaitForTimeout(3000)
			continue
		}
		// AI 不忙了，确认输入框真的能输入
		if v, err := shell.Evaluate(`() => {
			for (const e of document.querySelectorAll('div[contenteditable=true], textarea')) {
				const r = e.getBoundingClientRect();
				if (r.width < 200 || r.height < 20) continue;
				if (e.disabled) continue;
				if (e.getAttribute('contenteditable') === 'false') continue;
				if (e.getAttribute('readonly') !== null) continue;
				// 检查是否被遮罩挡住：用元素中心点做命中测试
				const cx = r.x + r.width/2, cy = r.y + r.height/2;
				const hit = document.elementFromPoint(cx, cy);
				if (!hit) continue;
				if (hit === e || e.contains(hit)) return true;
			}
			return false;
		}`); err == nil && asBool(v) {
			return nil
		}
		page.WaitForTimeout(3000)
	}
	return fmt.Errorf("等待输入框空闲超时")
}

// waitForSignal 等「本轮新增内容」里出现目标信号，并且 AI 完全停止。
//
// 关键教训：只看「信号出现 + 当前不忙」不够。
// 实测 AI 打出预览地址后会继续输出（比如追问「要不要提交到 GitCode」），
// 而那一刻 busy 可能短暂为 false，于是流程误判「本轮结束」，
// 立刻把下一条消息塞进去 —— AI 就把它当成新需求，重新创作去了。
//
// 现在要求：命中信号后，还要连续稳定 N 轮（约 1 分钟）才认定结束。
func waitForSignal(page playwright.Page, shell playwright.Frame, signals []string, deadline time.Time) error {
	baseline := len(frameTextOf(shell))
	lastLog := time.Now()
	hit := false
	stable := 0
	prev := ""
	for time.Now().Before(deadline) {
		page.WaitForTimeout(8000)
		dismissCWDialog(shell)
		txt := frameTextOf(shell)
		var fresh string
		if len(txt) > baseline {
			fresh = txt[baseline:]
		}
		busy := aiIsBusy(shell)

		if !hit && !busy {
			for _, s := range signals {
				if strings.Contains(fresh, s) {
					hit = true
					logStep("  命中信号 %q，等内容稳定…", s)
					break
				}
			}
		}

		if hit {
			if txt == prev && !busy {
				stable++
				if stable >= 8 { // 8 × 8s ≈ 64 秒无变化且不忙
					return nil
				}
			} else {
				stable = 0
				prev = txt
			}
		}

		if time.Since(lastLog) > 2*time.Minute {
			logStep("  …等待中（新增 %d 字符, busy=%v, 已命中=%v）", len(fresh), busy, hit)
			lastLog = time.Now()
		}
	}
	if hit {
		return nil
	}
	return fmt.Errorf("等待超时（未在新增内容里看到目标信号）")
}

// waitForOAuthURL 等 AI 打出 GitCode 登录短链。
func waitForOAuthURL(page playwright.Page, shell playwright.Frame, deadline time.Time) (string, error) {
	re := regexp.MustCompile(`https://acs\.atomgit\.com/s/[0-9a-f]+`)
	var lastURL string
	lastLog := time.Now()
	for time.Now().Before(deadline) {
		page.WaitForTimeout(8000)

		// 弹窗会挡住页面，每次轮询都清一次
		dismissCWDialog(shell)

		txt := frameTextOf(shell)
		busy := aiIsBusy(shell)
		all := re.FindAllString(txt, -1)
		if len(all) > 0 && !busy {
			lastURL = all[len(all)-1]
			if code, _ := runLocal("curl", "-s", "-o", "/dev/null", "-w", "%{http_code}",
				"--max-time", "10", lastURL); strings.TrimSpace(code) != "410" {
				return lastURL, nil
			}
			logStep("  登录链接已过期，等 AI 重新生成")
		}
		if time.Since(lastLog) > 90*time.Second {
			logStep("  …等 GitCode 登录链接（busy=%v, URL数=%d）", busy, len(all))
			lastLog = time.Now()
		}
	}
	if lastURL == "" {
		return "", fmt.Errorf("未找到 GitCode 登录链接")
	}
	return "", fmt.Errorf("登录链接一直过期: %s", lastURL)
}

// autoGitCodeLogin 在宿主机浏览器里完成 AtomGit 登录 + OAuth 授权。
//
// ⚠ 2026-11-09 重写。之前的实现有三个根本性错误，导致整个发布流程卡死：
//
//	错误 1：以为 OAuth 回调打本地 127.0.0.1:7654。
//	  实际从 gitcode-oauth 源码（zhoucungen/gitcode-oauth）确认：
//	    defaultBaseURL = https://acs.atomgit.com
//	    /auth/login 返回的 login_url 里
//	    redirect_uri = https://acs.atomgit.com/callback
//	  授权码送到 ACS 服务端，**不是本地端口**。容器内的 client 轮询
//	  GET https://acs.atomgit.com/auth/check?state=<state> 即可拿到授权结果。
//	  所以宿主机浏览器授权 → 容器内 AI 就能收到 token，天然跨网络。
//
//	错误 2：登录完没点 Authorize。
//	  force_login=true 会强制重新登录，账密登录成功后**会停在
//	  atomgit.com/oauth/authorize 授权确认页**。旧代码登录完就进
//	  「等 auth/success」循环，从不点授权按钮，永远等不到。
//
//	错误 3：拿 cookie 里的 GITCODE_ACCESS_TOKEN 去验开放 API。
//	  那是会话 JWT（295 字符），不是 PAT。开放 API 要的是 ACS 换出来的
//	  access_token，两者完全不同，401 是必然的。
//
// 现在的流程：
//  1. 打开 login_url（会 307 到 atomgit.com/oauth/authorize 或先到 /login）
//  2. 若落到登录页 → 账密登录（切密码 tab、勾两个协议框、点 Log In）
//  3. 登录成功后**必须点 Authorize 授权**
//  4. 轮询 ACS /auth/check 直到 valid=true
//  5. 取 /auth/token 拿到真正的 PAT，写入 ~/.gitcode/auth.toml 供 skill 用
//
// 三个 UI 坑（实测）：
//  1. 两个 DevUI 协议框默认不勾，不勾登录无效
//  2. 按 Enter 不提交，必须点 Log In 按钮
//  3. 勾选框是 .devui-checkbox__material，点别的无效
func autoGitCodeLogin(ctx playwright.BrowserContext, oauthURL string) error {
	acctFile := filepath.Join(config.Root(), "accounts", config.AccountID(), "account.json")
	raw, err := os.ReadFile(acctFile)
	if err != nil {
		return fmt.Errorf("读账号失败: %v", err)
	}
	var acc map[string]any
	_ = json.Unmarshal(raw, &acc)
	user, _ := acc["gitcode_user"].(string)
	pass, _ := acc["gitcode_pass"].(string)
	if user == "" || pass == "" {
		return fmt.Errorf("账号缺少 GitCode 凭据")
	}

	// 从 login_url 里解出 state，用于轮询 ACS。
	// login_url 形如 https://acs.atomgit.com/s/xxxx，打开后会 307 到
	// atomgit.com/oauth/authorize?...&state=atomcode_xxx。
	state := acsStateFromURL(oauthURL)
	if state == "" {
		// 短链里没有 state，先跟一次 307 拿最终 URL
		if final, err := resolveRedirect(oauthURL); err == nil {
			state = acsStateFromURL(final)
		}
	}
	logStep("  ACS state: %s", clip(state, 60))

	page, err := ctx.NewPage()
	if err != nil {
		return fmt.Errorf("新开标签失败: %v", err)
	}
	page.SetDefaultTimeout(60000)
	_ = page.BringToFront()

	if _, err := page.Goto(oauthURL, playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateDomcontentloaded,
		Timeout:   playwright.Float(60000),
	}); err != nil {
		return fmt.Errorf("打开 OAuth 链接失败: %v", err)
	}
	page.WaitForTimeout(6000)

	// 步骤 A：若落到登录页，先完成账密登录
	if strings.Contains(page.URL(), "/login") || strings.Contains(page.URL(), "authui") {
		logStep("  落到登录页，执行账密登录")
		if err := atomGitPasswordLogin(page, user, pass); err != nil {
			return err
		}
		page.WaitForTimeout(8000)
	}

	// 步骤 B：登录后应停在授权确认页，必须点 Authorize
	logStep("  当前页面: %s", clip(page.URL(), 110))
	if !strings.Contains(page.URL(), "/oauth/authorize") {
		// 可能已是成功页（之前授权过），直接往下走
		if !isOAuthSuccess(page) {
			logStep("  警告：未停在授权页（%s），尝试继续", clip(page.URL(), 80))
		}
	}

	if strings.Contains(page.URL(), "/oauth/authorize") {
		logStep("  点 Authorize 完成授权")
		clicked, label := clickAuthorize(page)
		if !clicked {
			return fmt.Errorf("授权页找不到 Authorize 按钮")
		}
		logStep("  已点授权按钮: %s", label)
		page.WaitForTimeout(10000)
		logStep("  授权后页面: %s", clip(page.URL(), 110))
	}

	// 步骤 C：轮询 ACS 确认授权状态
	//
	// 为什么不用页面文字判断：页面显示 "Authorization Successful" 只说明
	// 跳转到了 ACS 的成功页，但 skill 侧要的是 ACS 里 state 对应的记录。
	// 必须用 /auth/check 拿到 valid=true 才算真的成功。
	if state != "" {
		ok := false
		for i := 0; i < 12; i++ {
			body := acsGet("https://acs.atomgit.com/auth/check?state=" + state)
			if strings.Contains(body, `"valid":true`) {
				ok = true
				logStep("  ACS 已确认授权（第 %d 次轮询）", i+1)
				break
			}
			page.WaitForTimeout(3000)
		}
		if !ok {
			logStep("  警告：ACS 未确认授权，但页面已跳成功页，继续（skill 侧会自己重试）")
		}

		// 顺手把 PAT 落盘，方便 skill 直接用 ~/.gitcode/auth.toml，
		// 不必再跑一遍 OAuth。注意 GITCODE_HOME 默认 ~/.gitcode。
		if tok := acsGet("https://acs.atomgit.com/auth/token?state=" + state); strings.Contains(tok, "access_token") {
			if err := writeGitCodeAuthToml(tok); err != nil {
				logStep("  写 auth.toml 失败（不影响，skill 可自己取）: %v", err)
			} else {
				logStep("  已写入 ~/.gitcode/auth.toml（PAT 可直接复用）")
			}
		}
	} else {
		logStep("  警告：没解析到 ACS state，跳过轮询（依赖页面跳转结果）")
	}

	_ = page.Close()
	return nil
}

// atomGitPasswordLogin 在 AtomGit 登录页完成账密登录。
func atomGitPasswordLogin(page playwright.Page, user, pass string) error {
	// 切到密码登录
	switched := false
	for i := 0; i < 10; i++ {
		for _, sel := range []string{`text=Password login`, `text=密码登录`} {
			l := page.Locator(sel).First()
			if c, _ := l.Count(); c == 0 {
				continue
			}
			if vis, _ := l.IsVisible(); !vis {
				continue
			}
			if err := l.Click(playwright.LocatorClickOptions{Timeout: playwright.Float(6000)}); err == nil {
				switched = true
				break
			}
		}
		if switched {
			break
		}
		page.WaitForTimeout(700)
	}
	if !switched {
		return fmt.Errorf("登录页找不到「密码登录」入口")
	}
	page.WaitForTimeout(2500)

	// 填账密
	filled := false
	for _, sel := range []string{
		`input[placeholder*="Username"]`, `input[placeholder*="username"]`,
		`input[placeholder*="邮箱"]`, `input.devui-input__inner[type="text"]`,
		`input[type="text"]`,
	} {
		l := page.Locator(sel).First()
		if c, _ := l.Count(); c == 0 {
			continue
		}
		if vis, _ := l.IsVisible(); !vis {
			continue
		}
		if err := l.Fill(user); err == nil {
			filled = true
			break
		}
	}
	if !filled {
		return fmt.Errorf("登录页找不到用户名输入框")
	}
	if err := page.Locator(`input[type="password"]`).First().Fill(pass); err != nil {
		return fmt.Errorf("填密码失败: %w", err)
	}
	page.WaitForTimeout(600)

	// 两个协议框都必须勾（点 .devui-checkbox__material，点 input/label 无效）
	if _, err := page.Evaluate(`() => {
		let n = 0;
		for (const b of document.querySelectorAll('.devui-checkbox__material')) {
			const inp = b.parentElement ? b.parentElement.querySelector('input[type=checkbox]') : null;
			if (inp && inp.checked) continue;
			b.click(); n++;
		}
		return n;
	}`); err != nil {
		logStep("  勾选协议失败: %v", err)
	}
	page.WaitForTimeout(1200)

	// 必须点按钮，按 Enter 不提交
	clicked, _ := page.Evaluate(`() => {
		for (const b of document.querySelectorAll('button')) {
			const t = (b.innerText || '').trim();
			if (!/^(log in|login|登录|登 录)$/i.test(t)) continue;
			const r = b.getBoundingClientRect();
			if (r.width < 20) continue;
			if (b.disabled) continue;
			b.click();
			return true;
		}
		return false;
	}`)
	if !asBool(clicked) {
		return fmt.Errorf("登录页找不到 Log In 按钮")
	}

	// 等跳转离开登录页
	for i := 0; i < 30; i++ {
		page.WaitForTimeout(1000)
		u := page.URL()
		if !strings.Contains(u, "/login") && !strings.Contains(u, "authui") {
			logStep("  登录成功，已跳转: %s", clip(u, 100))
			return nil
		}
	}
	return fmt.Errorf("账密登录后仍停在登录页（可能密码错误或被风控）")
}

// clickAuthorize 在 OAuth 授权确认页点授权按钮。
func clickAuthorize(page playwright.Page) (bool, string) {
	v, err := page.Evaluate(`() => {
		const exact = ['Authorize', '授权', '同意授权', '确认授权', 'Allow', '允许'];
		const loose = ['authorize', '授权', '同意', 'allow'];
		const btns = [...document.querySelectorAll('button, a, input[type=submit], div[role=button]')];
		// 先精确匹配
		for (const b of btns) {
			const t = (b.innerText || b.value || '').trim();
			const r = b.getBoundingClientRect();
			if (r.width < 20 || r.height < 10) continue;
			if (b.disabled) continue;
			if (!exact.includes(t)) continue;
			b.click();
			return t;
		}
		// 再宽松匹配（排除 Switch account 之类）
		for (const b of btns) {
			const t = (b.innerText || b.value || '').trim().toLowerCase();
			const r = b.getBoundingClientRect();
			if (r.width < 20 || r.height < 10) continue;
			if (b.disabled) continue;
			if (!loose.some(w => t.includes(w))) continue;
			if (t.includes('switch') || t.includes('切换')) continue;
			b.click();
			return (b.innerText || '').trim().slice(0, 40);
		}
		return '';
	}`)
	if err != nil {
		return false, ""
	}
	if s, ok := v.(string); ok && s != "" {
		return true, s
	}
	return false, ""
}

// isOAuthSuccess 判断当前页是否是授权成功页。
func isOAuthSuccess(page playwright.Page) bool {
	u := page.URL()
	if strings.Contains(u, "auth/success") || strings.Contains(u, "callback") {
		return true
	}
	if body, err := page.Locator("body").InnerText(); err == nil {
		if strings.Contains(body, "Authorization Successful") || strings.Contains(body, "授权成功") {
			return true
		}
	}
	return false
}

// acsStateFromURL 从 URL 里解出 OAuth state。
func acsStateFromURL(u string) string {
	parsed, err := url.Parse(u)
	if err != nil {
		return ""
	}
	return parsed.Query().Get("state")
}

// resolveRedirect 跟随一次 307，返回最终的 Location URL。
func resolveRedirect(u string) (string, error) {
	out, err := runLocal("curl", "-s", "-o", "/dev/null",
		"-w", "%{redirect_url}", "--max-time", "15", u)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// acsGet 带 atomcode UA 请求 ACS 接口。
//
// 必须带这个 UA：ACS 用它识别 atomcode 客户端；普通 curl UA 也能通，
// 但带上更接近真实客户端行为。
func acsGet(u string) string {
	out, _ := runLocal("curl", "-s", "--max-time", "20",
		"-H", "User-Agent: atomcode-server/1.0.0", u)
	return out
}

// writeGitCodeAuthToml 把 ACS 返回的 token 写成 skill 认的 auth.toml。
//
// 格式来自 gitcode-oauth 源码 writeAuthToml()：
//
//	access_token  = "..."
//	refresh_token = "..."
//	token_type    = "Bearer"
//	expires_in    = 604799
//	created_at    = 1234567890
//
//	[user]
//	id = ...
//	login = ...
func writeGitCodeAuthToml(tokenJSON string) error {
	var t struct {
		AccessToken  string `json:"access_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int64  `json:"expires_in"`
		RefreshToken string `json:"refresh_token"`
		User         struct {
			ID       string `json:"id"`
			Username string `json:"username"`
			Name     string `json:"name"`
			Email    string `json:"email"`
		} `json:"user"`
	}
	if err := json.Unmarshal([]byte(tokenJSON), &t); err != nil {
		return err
	}
	if t.AccessToken == "" {
		return fmt.Errorf("ACS 未返回 access_token")
	}

	home := os.Getenv("GITCODE_HOME")
	if home == "" {
		home = "/root/.gitcode"
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	content := fmt.Sprintf(`access_token  = %q
refresh_token = %q
token_type    = %q
expires_in    = %d
created_at    = %d

[user]
id    = %q
login = %q
name  = %q
email = %q
`, t.AccessToken, t.RefreshToken, t.TokenType, t.ExpiresIn, time.Now().Unix(),
		t.User.ID, t.User.Username, t.User.Name, t.User.Email)
	path := filepath.Join(home, "auth.toml")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}


// parseGalleryResult 从对话文本里抽取发布结果。
func parseGalleryResult(txt string) *GalleryResult {
	r := &GalleryResult{}
	pick := func(re *regexp.Regexp) string {
		if m := re.FindStringSubmatch(txt); len(m) > 1 {
			return strings.TrimSpace(m[1])
		}
		return ""
	}
	r.WorkID = pick(regexp.MustCompile(`workId[=:：]\s*([0-9a-f]{8,})`))
	r.WorkName = pick(regexp.MustCompile(`workName[=:：]\s*(\S+)`))
	r.WorkURL = pick(regexp.MustCompile(`(https://gallery\.developer\.huaweicloud\.com/gallery/[0-9a-f]+)`))
	if r.WorkURL == "" && r.WorkID != "" {
		r.WorkURL = "https://gallery.developer.huaweicloud.com/gallery/" + r.WorkID
	}
	r.GitCodeURL = pick(regexp.MustCompile(`(https://gitcode\.com/[^\s]+/campus-learning-assistant[^\s]*)`))
	r.PreviewURL = pick(regexp.MustCompile(`(https://[a-z0-9-]+\.devbridge-s2\.hwtunnel\.com)`))
	if strings.Contains(txt, "reward=success") || strings.Contains(txt, "积分领取请求已提交") {
		r.Reward = "200"
	}
	// 活动名：发布结果表格里的「投稿活动」行
	if m := regexp.MustCompile(`投稿活动[\s\S]{0,40}?([^\s|]+)`).FindStringSubmatch(txt); len(m) > 1 {
		r.Activity = strings.TrimSpace(m[1])
	}
	return r
}

func frameTextOf(f playwright.Frame) string {
	b, _ := f.Locator("body").InnerText()
	return b
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "..."
	}
	return s
}

func logStep(format string, args ...any) {
	fmt.Printf("[gallery] "+format+"\n", args...)
}
