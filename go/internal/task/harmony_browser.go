package task

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/playwright-community/playwright-go"

	"hwcloud/internal/browser"
	"hwcloud/internal/huawei"
)

// 鸿蒙实践中心的每日任务走真实浏览器页面。
//
// 为什么必须走浏览器：成长中心的判定依据是各业务站上报的 behavior_code
// 事件，纯协议调用（包括心跳接口）在鸿蒙侧能记 done，但成长中心不认账。
// 2026-11-07 实测：
//   - 浏览器打开案例详情页 30 秒 → 成长中心「每日浏览一个鸿蒙案例」is_finish=1
//   - 浏览器打开 GitCode 案例仓点 Star → 「每日Star一个鸿蒙案例代码仓」completed=true
//   - 纯协议版本此前连续多天全部显示未完成。

// harmonyCase 是 /api/cases 返回的案例条目（只取用到的字段）。
type harmonyCase struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	PublicRepo string `json:"publicRepo"`
}

// listHarmonyCases 分页取全部案例。
//
// 这个接口的 page_size 参数会 422，只能传 page（每页固定 6 条）。
// limit 传再大也只返回 6 条，所以必须按 total 翻页。
func listHarmonyCases(c *harmonyClient) ([]harmonyCase, error) {
	var out []harmonyCase
	total := -1
	for page := 1; page <= 30; page++ {
		st, raw, err := c.call(fmt.Sprintf("/api/cases?limit=200&page=%d", page), http.MethodGet, nil)
		if err != nil {
			return nil, err
		}
		if st != http.StatusOK {
			return nil, fmt.Errorf("案例列表 HTTP %d: %s", st, harmonyClip(string(raw)))
		}
		var resp struct {
			Cases []harmonyCase `json:"cases"`
			Total int           `json:"total"`
		}
		if err := json.Unmarshal(raw, &resp); err != nil {
			return nil, fmt.Errorf("解析案例列表失败: %v", err)
		}
		total = resp.Total
		if len(resp.Cases) == 0 {
			break
		}
		out = append(out, resp.Cases...)
		if total > 0 && len(out) >= total {
			break
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("案例列表为空")
	}
	return out, nil
}

// repoPathFromPublicRepo 把 publicRepo 转成 GitCode 的 owner/repo 形式。
//
// 返回的值可能带 .git 后缀，也可能只有 owner/repo 一段，这里统一清洗。
func repoPathFromPublicRepo(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "\t", "")
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	s = strings.TrimSuffix(s, ".git")
	if i := strings.Index(s, "gitcode.com/"); i >= 0 {
		s = s[i+len("gitcode.com/"):]
	}
	s = strings.TrimPrefix(s, "/")
	return s
}

// checkGitcodeStar 触发一次鸿蒙侧的 Star 核验。
//
// external_check 类任务不会自动轮询，必须显式提交 action=check。
// 返回 (completed, 原始结果)。
func checkGitcodeStar(sess *huawei.Session, caseID string) (bool, map[string]any, error) {
	c, err := newHarmonyClient(sess)
	if err != nil {
		return false, nil, err
	}
	st, raw, err := c.call("/api/growth/tasks/gitcode-star/submit", http.MethodPost,
		map[string]any{"case_id": caseID, "payload": map[string]any{"action": "check"}})
	if err != nil {
		return false, nil, err
	}
	out := map[string]any{}
	if json.Unmarshal(raw, &out) != nil {
		return false, map[string]any{"http_status": st, "raw": harmonyClip(string(raw))}, nil
	}
	out["http_status"] = st
	completed, _ := out["completed"].(bool)
	return completed, out, nil
}

// RunHarmonyDwellBrowser 完成「每日浏览一个鸿蒙案例」（+10）。
//
// 打开真实案例详情页并保持在前台。页面自己会开计时会话、每 5 秒心跳，
// 30 秒后由鸿蒙侧结算并上报成长中心。
//
// 关键约束：document.hidden 为 true 时前端不发心跳，所以必须 headed，
// 且服务器要有 Xvfb（systemd 里已配 DISPLAY=:99）。
func RunHarmonyDwellBrowser(sess *huawei.Session, headless bool, maxSeconds int) (map[string]any, error) {
	c, err := newHarmonyClient(sess)
	if err != nil {
		return nil, err
	}
	if maxSeconds <= 0 {
		maxSeconds = 60
	}

	// 已完成就不重复跑
	if st, raw, err := c.call("/api/growth/tasks/"+harmonyTaskKey, http.MethodGet, nil); err == nil && st == http.StatusOK {
		var resp struct {
			Task harmonyTaskBrief `json:"task"`
		}
		if json.Unmarshal(raw, &resp) == nil {
			switch resp.Task.TodayStatus {
			case "done", "awarding", "award_failed", "failed_terminal":
				return map[string]any{"task_key": harmonyTaskKey, "already": true, "status": resp.Task.TodayStatus}, nil
			}
		}
	}

	cases, err := listHarmonyCases(c)
	if err != nil {
		return nil, err
	}
	caseID := cases[0].ID

	if err := browser.EnsureDriver(); err != nil {
		return nil, err
	}
	pw, err := playwright.Run()
	if err != nil {
		return nil, err
	}
	defer pw.Stop()
	br, ctx, err := browser.NewHuaweiContext(pw, headless)
	if err != nil {
		return nil, err
	}
	defer br.Close()
	defer ctx.Close()

	page, err := ctx.NewPage()
	if err != nil {
		return nil, err
	}
	pageURL := harmonyBase + "/cases/" + caseID
	if _, err := page.Goto(pageURL, playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateDomcontentloaded,
		Timeout:   playwright.Float(90000),
	}); err != nil {
		return nil, fmt.Errorf("打开案例页失败: %w", err)
	}
	page.WaitForTimeout(8000)
	_ = page.BringToFront()

	// 等够 30 秒。前端每 5 秒心跳一次，40 秒留足余量。
	dwell := 40
	if maxSeconds < dwell {
		dwell = maxSeconds
	}
	deadline := time.Now().Add(time.Duration(dwell) * time.Second)
	for time.Now().Before(deadline) {
		page.WaitForTimeout(5000)
	}

	// 用任务状态确认（成长中心侧以 is_finish 为准）
	status := ""
	if st, raw, err := c.call("/api/growth/tasks/"+harmonyTaskKey, http.MethodGet, nil); err == nil && st == http.StatusOK {
		var resp struct {
			Task harmonyTaskBrief `json:"task"`
		}
		if json.Unmarshal(raw, &resp) == nil {
			status = resp.Task.TodayStatus
		}
	}
	done := status == "done" || status == "awarding" || status == "award_failed" || status == "failed_terminal"
	return map[string]any{
		"task_key":  harmonyTaskKey,
		"case_id":   caseID,
		"page":      pageURL,
		"dwell_sec": dwell,
		"status":    status,
		"done":      done,
	}, nil
}

// starButtonState 读取 GitCode 仓库页顶部的 Star 按钮状态。
func starButtonState(page playwright.Page) (text string, starred bool) {
	v, err := page.Evaluate(`() => {
		const els=[...document.querySelectorAll('div.status-btn')]
			.filter(e=>e.getBoundingClientRect().width>0);
		if (!els.length) return null;
		const e=els[els.length-1];
		return {text:(e.innerText||'').replace(/\s+/g,' ').trim(),
			cls:(e.className||'').toString()};
	}`)
	if err != nil || v == nil {
		return "", false
	}
	m, ok := v.(map[string]any)
	if !ok {
		return "", false
	}
	text, _ = m["text"].(string)
	cls, _ := m["cls"].(string)
	if strings.Contains(cls, "starred") || strings.HasPrefix(strings.TrimSpace(text), "Starred") {
		return text, true
	}
	return text, false
}

// dismissGitcodeGuide 关掉 GitCode 首页的新手引导蒙层。
//
// 不关的话它会盖住整个页面（role=dialog 的 spotlight-guide），
// Playwright 的点击会被 mask 拦截，报 "subtree intercepts pointer events"。
func dismissGitcodeGuide(page playwright.Page) {
	for i := 0; i < 4; i++ {
		loc := page.Locator(".spotlight-guide-close")
		if n, _ := loc.Count(); n == 0 {
			break
		}
		if err := loc.First().Click(playwright.LocatorClickOptions{
			Force: playwright.Bool(true), Timeout: playwright.Float(3000)}); err != nil {
			break
		}
		page.WaitForTimeout(700)
	}
	if n, _ := page.Locator(".spotlight-guide").Count(); n > 0 {
		_, _ = page.Evaluate(`() => document.querySelectorAll('.spotlight-guide').forEach(e=>e.remove())`)
		page.WaitForTimeout(300)
	}
}

// clickGitcodeStar 点击仓库页的 Star 按钮。返回点击前后的按钮文案。
func clickGitcodeStar(page playwright.Page) (before, after string, err error) {
	before, _ = starButtonState(page)
	loc := page.Locator(`xpath=(//div[contains(@class,'status-btn') and .//span[normalize-space(text())='Star']])[last()]`)
	n, _ := loc.Count()
	if n == 0 {
		return before, "", fmt.Errorf("未找到 Star 按钮（当前 %q）", before)
	}
	if err := loc.First().Click(playwright.LocatorClickOptions{Timeout: playwright.Float(8000)}); err != nil {
		box, _ := loc.First().BoundingBox()
		if box == nil {
			return before, "", err
		}
		if err2 := page.Mouse().Click(box.X+box.Width/2, box.Y+box.Height/2); err2 != nil {
			return before, "", err
		}
	}
	page.WaitForTimeout(4500)
	after, _ = starButtonState(page)
	return before, after, nil
}

// gitcodePageLoggedIn 判断当前 GitCode 页面是否处于已登录状态。
//
// 判据（任一命中即算已登录，全部不命中算未登录）：
//   - cookie 里有 GITCODE_ACCESS_TOKEN（最可靠，但页面上下文读不到 httpOnly
//     时可能为空，所以不能只靠它）
//   - 页面右上角没有 Login / Register 按钮
//   - 有用户头像/用户名菜单
//
// 反过来只要明确看到 Login 按钮且有 Star 区，就能判定未登录 —— 这个信号
// 比「读不到 Star 按钮」可靠得多，后者在页面还没渲染完时也会成立。
func gitcodePageLoggedIn(page playwright.Page) (bool, string) {
	v, err := page.Evaluate(`() => {
		const txt = document.body ? (document.body.innerText || '') : '';
		const btns = [...document.querySelectorAll('a, button, div[role=button]')]
			.map(e => (e.innerText || '').trim().toLowerCase())
			.filter(Boolean);
		const hasLogin = btns.some(t => t === 'login' || t === '登录' || t === 'sign in');
		const hasRegister = btns.some(t => t === 'register' || t === '注册' || t === 'sign up');
		const hasStarArea = /star/i.test(txt);
		const hasUserMenu = !!document.querySelector(
			'[class*=avatar], [class*=user-menu], [class*=userinfo], [class*=user-info]');
		return {hasLogin, hasRegister, hasStarArea, hasUserMenu};
	}`)
	if err != nil {
		return true, "页面状态读取失败（按已登录继续，失败会走原有判据）"
	}
	m, ok := v.(map[string]any)
	if !ok {
		return true, "页面状态格式异常（按已登录继续）"
	}
	hasLogin := asBool(m["hasLogin"])
	hasRegister := asBool(m["hasRegister"])
	hasStarArea := asBool(m["hasStarArea"])
	hasUserMenu := asBool(m["hasUserMenu"])
	if hasUserMenu {
		return true, ""
	}
	if (hasLogin || hasRegister) && hasStarArea {
		return false, "页面右上角出现 Login/Register 且无用户菜单"
	}
	return true, ""
}

// gitcodeLoginProbeURL 返回用于登录态预检的仓库 URL。
func gitcodeLoginProbeURL(repoPath string) string {
	return "https://gitcode.com/" + repoPath
}

// gitcodeSessionExpired 判断账号的 GitCode 会话是否已过期。
//
// 为什么要主动判断、不能靠页面报错：
//   GitCode 的仓库页是公开可读的，会话过期后页面照样打开、
//   Star 按钮照样能点 —— 点击只是前端乐观更新（按钮变 Starred），
//   服务端以匿名身份收下，刷新后立刻变回「Star」。
//   等到核验失败再重登就白点一次，而且会误判成「同步延迟」。
//
// GITCODE_ACCESS_TOKEN 是 exp-iat=24h 的 JWT，直接解 payload 里的 exp。
func gitcodeSessionExpired(stateDir string) (bool, string) {
	raw, err := os.ReadFile(gitcodeStateFileIn(stateDir))
	if err != nil {
		return true, "读取 GitCode 会话失败: " + err.Error()
	}
	var st struct {
		Cookies []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"cookies"`
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return true, "解析 GitCode 会话失败: " + err.Error()
	}
	token := ""
	for _, c := range st.Cookies {
		if c.Name == "GITCODE_ACCESS_TOKEN" {
			token = c.Value
			break
		}
	}
	if token == "" {
		return true, "会话里没有 GITCODE_ACCESS_TOKEN"
	}
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return true, "GITCODE_ACCESS_TOKEN 格式异常"
	}
	payload := parts[1]
	// JWT 的 base64url 不带 padding，补齐后才能解
	if pad := len(payload) % 4; pad != 0 {
		payload += strings.Repeat("=", 4-pad)
	}
	dec, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		// 有些实现用 RawURLEncoding，换成无 padding 再试一次
		dec, err = base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			return true, "解码 token payload 失败: " + err.Error()
		}
	}
	var claims struct {
		Exp int64 `json:"exp"`
		Iat int64 `json:"iat"`
	}
	if err := json.Unmarshal(dec, &claims); err != nil {
		return true, "解析 token claims 失败: " + err.Error()
	}
	if claims.Exp == 0 {
		return true, "token 没有 exp 字段"
	}
	exp := time.Unix(claims.Exp, 0)
	if time.Now().After(exp) {
		return true, fmt.Sprintf("GITCODE_ACCESS_TOKEN 已于 %s 过期", exp.Local().Format("2006-01-02 15:04"))
	}
	return false, ""
}

// RunHarmonyStarBrowser 完成「每日Star一个鸿蒙案例代码仓」（+30）。
//
// 任务要求：每天 Star 一个**不同**的案例仓，且不能先 Star 再取消。
// 所以流程是「挑一个当前未 Star 的案例仓 → 点 Star」，已 Star 的必须跳过
// （点它等于取消 Star，会把任务做废）。
//
// 先查一次核验：如果今天已经有了新 Star（比如面板重复点），直接返回。
func RunHarmonyStarBrowser(sess *huawei.Session, stateDir string, headless bool) (map[string]any, error) {
	c, err := newHarmonyClient(sess)
	if err != nil {
		return nil, err
	}

	// 绑定状态
	if st, raw, err := c.call("/api/growth/tasks/gitcode-star", http.MethodGet, nil); err == nil && st == http.StatusOK {
		var resp struct {
			Task struct {
				TodayStatus string `json:"today_status"`
				GitBound    bool   `json:"git_bound"`
			} `json:"task"`
		}
		if json.Unmarshal(raw, &resp) == nil {
			switch resp.Task.TodayStatus {
			case "done", "awarding", "award_failed", "failed_terminal":
				return map[string]any{"task_key": "gitcode-star", "already": true,
					"status": resp.Task.TodayStatus}, nil
			}
			if !resp.Task.GitBound {
				return map[string]any{"task_key": "gitcode-star", "need_binding": true,
					"msg": "GitCode 未绑定到鸿蒙实践中心，请先运行 harmony-bind"}, nil
			}
		}
	}

	// 今天是否已经有新 Star：check 一次，completed=true 就不用再点
	if cases, err := listHarmonyCases(c); err == nil && len(cases) > 0 {
		if done, _, err := checkGitcodeStar(sess, cases[0].ID); err == nil && done {
			return map[string]any{"task_key": "gitcode-star", "already": true,
				"msg": "今日已有新 Star，无需重复"}, nil
		}
	}

	// GitCode 会话过期要先重登：过期状态下点 Star 只是前端乐观更新，
	// 服务端按匿名处理，刷新即还原（2026-11-07 在 13800138004 上实测）。
	if expired, why := gitcodeSessionExpired(stateDir); expired {
		return nil, fmt.Errorf("%s%s", errGitCodeExpired, why)
	}

	cases, err := listHarmonyCases(c)
	if err != nil {
		return nil, err
	}

	// 只保留有 publicRepo 的案例
	type candidate struct {
		CaseID   string
		Title    string
		RepoPath string
	}
	var cands []candidate
	for _, cs := range cases {
		p := repoPathFromPublicRepo(cs.PublicRepo)
		if p == "" || strings.HasSuffix(p, "/org-desc") || p == "org-desc" {
			continue
		}
		cands = append(cands, candidate{CaseID: cs.ID, Title: cs.Title, RepoPath: p})
	}
	if len(cands) == 0 {
		return nil, fmt.Errorf("案例列表里没有可用代码仓")
	}

	if err := browser.EnsureDriver(); err != nil {
		return nil, err
	}
	pw, err := playwright.Run()
	if err != nil {
		return nil, err
	}
	defer pw.Stop()

	br, ctx, err := browser.NewStorageContext(pw, gitcodeStateFileIn(stateDir), headless)
	if err != nil {
		return nil, err
	}
	defer br.Close()
	defer ctx.Close()

	page, err := ctx.NewPage()
	if err != nil {
		return nil, err
	}

	// 起手先做一次登录态预检。
	//
	// 为什么必须在循环外做：仓库页是公开可读的，未登录时页面照样能打开、
	// 只是没有 Star 按钮。原来只在「读不到 Star 按钮」时判断一次，但那个
	// 判据依赖页面文案，漏判就在**每个候选仓库**上重跑一遍 6 秒等待 ——
	// 40 多个案例仓就是 4 分钟以上纯空转。2026-11-09 实测热情号就是这样
	// 卡了 13 分钟被 systemd 杀掉：cookie 域是 .atomgit.com，打开
	// gitcode.com 时是未登录态（右上角 Login/Register）。
	//
	// 预检只看一个可靠信号：页面上有 Login/Register（或「登录」）按钮，
	// 且有 Star 区（说明页面渲染完了）。命中就打过期前缀让上层重登。
	if _, err := page.Goto(gitcodeLoginProbeURL(cands[0].RepoPath), playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateDomcontentloaded,
		Timeout:   playwright.Float(60000),
	}); err == nil {
		page.WaitForTimeout(5000)
		if logged, why := gitcodePageLoggedIn(page); !logged {
			return nil, fmt.Errorf("%sGitCode 未登录（%s）", errGitCodeExpired, why)
		}
	}

	// ---------- 遍历候选仓库找未 Star 的 ----------
	//
	// 必须设总时长上限。
	//
	// 2026-11-14 实测：某账号在 09:44 进入这段循环，10:31 才出来，
	// 卡了 47 分钟 —— 把整个 daily 窗口（systemd TimeoutStartSec=3600）
	// 耗尽，后面两个账号直接没跑。
	//
	// 原因：候选仓库 40+ 个，每个 goto 超时 60s + 等待 6s，最坏 44 分钟。
	// 网络稍慢或某些仓库一直超时，就会一路试到底。
	//
	// 现在加 12 分钟硬上限并缩短单步超时：正常几个仓库就能找到未 Star 的，
	// 超时就按「待同步」返回（下次再试），不拖垮其它账号。
	starDeadline := time.Now().Add(12 * time.Minute)
	attempts := []map[string]any{}
	for _, cand := range cands {
		if time.Now().After(starDeadline) {
			// 用 Println 而非 Printf：这个文件曾被脚本改坏过一次，
			// 反斜杠 n 被吞成了真实换行，导致语法错误。Println 天然带换行。
			fmt.Println("[star] 遍历候选仓库超时，放弃本次 Star，已试个数:", len(attempts))
			return map[string]any{
				"task_key":      "gitcode-star",
				"completed":     false,
				"pending_check": true,
				"tried":         attempts,
				"msg": fmt.Sprintf("遍历 %d 个候选仓库仍未找到可 Star 的（超时 12 分钟），下次再试", len(attempts)),
			}, nil
		}
		repoURL := "https://gitcode.com/" + cand.RepoPath
		if _, err := page.Goto(repoURL, playwright.PageGotoOptions{
			WaitUntil: playwright.WaitUntilStateDomcontentloaded,
			Timeout:   playwright.Float(30000),
		}); err != nil {
			attempts = append(attempts, map[string]any{"repo": cand.RepoPath, "skip": "goto: " + err.Error()})
			continue
		}
		page.WaitForTimeout(4000)
		dismissGitcodeGuide(page)

		text, starred := starButtonState(page)
		if starred {
			// 已 Star 的绝不能再点 —— 那是取消 Star
			attempts = append(attempts, map[string]any{"repo": cand.RepoPath, "skip": "已 Starred"})
			continue
		}
		if text == "" {
			// 读不到 Star 按钮多半是 GitCode 未登录（页面显示登录按钮）。
			// 打上过期前缀，让 web 层触发一次自动重登。
			needLogin, _ := page.Evaluate(`() => {
				const t = document.body.innerText || '';
				return /扫码登录|登录后即可|请先登录|Sign in to continue/i.test(t);
			}`)
			if b, _ := needLogin.(bool); b {
				return nil, fmt.Errorf("%sGitCode 页面要求登录（%s）", errGitCodeExpired, cand.RepoPath)
			}
			attempts = append(attempts, map[string]any{"repo": cand.RepoPath, "skip": "未读到 Star 按钮"})
			continue
		}

		before, after, err := clickGitcodeStar(page)
		if err != nil {
			attempts = append(attempts, map[string]any{"repo": cand.RepoPath, "error": err.Error()})
			continue
		}
		ok := strings.HasPrefix(strings.TrimSpace(after), "Starred")
		rec := map[string]any{"repo": cand.RepoPath, "case_id": cand.CaseID,
			"before": before, "after": after, "star_ok": ok}
		attempts = append(attempts, rec)
		if !ok {
			continue
		}

		// 点完立刻核验；华为云侧要拉一次 GitCode 状态，给几轮退避。
		//
		// 实测同步延迟不稳定：同一个仓库，账号 13800138001 秒过，
		// 13800138002 要几分钟，13800138003 更久。早期退避总共只有
		// 24 秒，经常在同步完成前就判定失败。这里放宽到约 2 分钟。
		var last map[string]any
		backoff := []time.Duration{0, 5 * time.Second, 10 * time.Second,
			15 * time.Second, 20 * time.Second, 20 * time.Second, 30 * time.Second}
		for i, wait := range backoff {
			if wait > 0 {
				time.Sleep(wait)
			}
			done, detail, err := checkGitcodeStar(sess, cand.CaseID)
			if err != nil {
				continue
			}
			last = detail
			if done {
				return map[string]any{
					"task_key":       "gitcode-star",
					"completed":      true,
					"repo":           cand.RepoPath,
					"case_id":        cand.CaseID,
					"case_title":     cand.Title,
					"before":         before,
					"after":          after,
					"check_attempts": i + 1,
					"check_detail":   detail,
					"tried":          attempts,
				}, nil
			}
		}
		return map[string]any{
			"task_key":      "gitcode-star",
			"completed":     false,
			"pending_check": true,
			"repo":          cand.RepoPath,
			"case_id":       cand.CaseID,
			"before":        before,
			"after":         after,
			"check_detail":  last,
			"tried":         attempts,
			// Star 已经真的点上了，只是华为云侧还没同步完。
			// 这种情况**不要重复点**（再点就是取消 Star），
			// 过几分钟重跑一次即可 —— 开头那次 check 会直接判定完成。
			"msg": "已点 Star，但华为云核验尚未同步；Star 已生效，勿重复点击，稍后重跑即可",
		}, nil
	}

	return map[string]any{
		"task_key":  "gitcode-star",
		"completed": false,
		"tried":     attempts,
		"msg":       "所有案例仓都已 Starred（或无法点击），今日没有可新增的 Star",
	}, nil
}

// RunDevSpaceBrowser 完成「每日体验 AI DevSpace」（+30）。
//
// 纯协议的 /api/track/visit + /api/track/aishell_click 虽然返回 success，
// 但成长中心不认。必须真实打开实践中心页面并从「前往AI DevSpace实践」
// 进入 AI 开发者空间（该动作会触发 aishell_click 与后续页面行为）。
func RunDevSpaceBrowser(sess *huawei.Session, headless bool) (map[string]any, error) {
	if err := browser.EnsureDriver(); err != nil {
		return nil, err
	}
	pw, err := playwright.Run()
	if err != nil {
		return nil, err
	}
	defer pw.Stop()
	br, ctx, err := browser.NewHuaweiContext(pw, headless)
	if err != nil {
		return nil, err
	}
	defer br.Close()
	defer ctx.Close()

	page, err := ctx.NewPage()
	if err != nil {
		return nil, err
	}

	var popup playwright.Page
	ctx.On("page", func(p playwright.Page) { popup = p })

	if _, err := page.Goto("https://hands-on-agent.developer.huaweicloud.com/#tasks", playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateDomcontentloaded,
		Timeout:   playwright.Float(90000),
	}); err != nil {
		return nil, fmt.Errorf("打开实践中心失败: %w", err)
	}
	page.WaitForTimeout(8000)

	loc := page.Locator(`a:has-text("前往AI DevSpace实践")`).First()
	clicked := false
	if n, _ := loc.Count(); n > 0 {
		if err := loc.Click(playwright.LocatorClickOptions{Timeout: playwright.Float(8000)}); err == nil {
			clicked = true
		} else if box, _ := loc.BoundingBox(); box != nil {
			if err2 := page.Mouse().Click(box.X+box.Width/2, box.Y+box.Height/2); err2 == nil {
				clicked = true
			}
		}
	}
	page.WaitForTimeout(9000)

	target := page
	if popup != nil {
		_ = popup.WaitForLoadState(playwright.PageWaitForLoadStateOptions{State: playwright.LoadStateDomcontentloaded})
		target = popup
	}
	// AI 开发者空间首次进入会有协议弹窗，点「进入」；没有就跳过
	for _, sel := range []string{`button:has-text("进入")`, `button:has-text("同意并继续")`} {
		b := target.Locator(sel).First()
		if n, _ := b.Count(); n > 0 {
			_ = b.Click(playwright.LocatorClickOptions{Timeout: playwright.Float(4000)})
			target.WaitForTimeout(2000)
			break
		}
	}
	target.WaitForTimeout(8000)

	// 用鸿蒙任务状态确认（成长中心是最终判定方，这里只做记录）
	status := ""
	if c, err := newHarmonyClient(sess); err == nil {
		if _, _, err := c.call("/api/growth/tasks", http.MethodGet, nil); err == nil {
			status = "visited"
		}
	}
	return map[string]any{
		"clicked":  clicked,
		"final_url": target.URL(),
		"status":   status,
	}, nil
}

