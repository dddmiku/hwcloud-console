package task

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/playwright-community/playwright-go"

	"hwcloud/internal/browser"
	"hwcloud/internal/config"
	"hwcloud/internal/huawei"
	"hwcloud/internal/fsutil"
)

// huaweiSession 用当前 cookie 文件构造一个纯协议会话。
func huaweiSession() (*huawei.Session, error) {
	return huawei.New("")
}

// 会话保活：华为云的 SessionID 有效期短（约 1-2 小时）且绑定 IP，
// 但浏览器的持久化 profile 能在过期后用刷新令牌自动续期。
// 因此保活流程是：用 profile 打开页面 → 让页面自己续期 → 导出新 cookie。

// SessionAlive 用 HTTP 快速判断当前 cookie 是否还有效。
func SessionAlive() (bool, error) {
	sess, err := huaweiSession()
	if err != nil {
		return false, err
	}
	info, err := sess.CheckLogin()
	if err != nil {
		return false, err
	}
	return info != nil, nil
}

// EnsureSession 确保会话可用：失效时先用 refresh token 自我恢复，
// 恢复不了再拉起浏览器 profile 续期。
// 这是所有任务命令的前置步骤——用户不需要再手动同步 cookie。
func EnsureSession() error {
	if alive, _ := SessionAlive(); alive {
		return nil
	}

	// 第一层：纯协议自愈。丢掉 WAF 短效 cookie 后重放 refresh token，
	// SSO 会自动下发新的会话 cookie 并写回 cookie jar。
	if err := refreshByToken(); err == nil {
		if alive, _ := SessionAlive(); alive {
			return nil
		}
	}

	// 第二层：浏览器 profile 续期（需要本机有 Chrome 与 profile）
	if _, err := KeepAlive(true, 0); err == nil {
		if alive, _ := SessionAlive(); alive {
			return nil
		}
	}

	return fmt.Errorf("会话已失效且无法自动恢复，需要在本机执行一次 hwcloud login 后同步 state 目录")
}

// KeepAliveResult 描述一次保活的结果。
type KeepAliveResult struct {
	Before   string `json:"before"`
	After    string `json:"after"`
	Refreshed bool  `json:"refreshed"`
	Username string `json:"username,omitempty"`
	Message  string `json:"message,omitempty"`
}

// KeepAlive 刷新会话。
// 优先走纯协议（refreshByToken）：不需要浏览器、不触发验证码，服务器上也能跑。
// 纯协议失败时才回退到浏览器 profile（需要本机有 Chrome 和已登录的 profile）。
// minRemain 为触发刷新的剩余时长阈值（小时）；<=0 表示仅在失效时才刷新。
func KeepAlive(headless bool, minRemain float64) (*KeepAliveResult, error) {
	res := &KeepAliveResult{}

	alive, _ := SessionAlive()
	remain := remainingHours()
	res.Before = "expired"
	if alive {
		res.Before = "alive"
		if minRemain <= 0 || remain > minRemain {
			res.After = "alive"
			res.Message = "会话有效，无需刷新"
			if sess, err := huaweiSession(); err == nil {
				res.Username = sess.Username
			}
			return res, nil
		}
	}

	// 1. 纯协议刷新（首选：无浏览器依赖）
	if err := refreshByToken(); err == nil {
		if alive, _ := SessionAlive(); alive {
			res.After = "alive"
			res.Refreshed = true
			res.Message = "已用 refresh token 重建会话（纯协议）"
			if sess, err := huaweiSession(); err == nil {
				res.Username = sess.Username
			}
			return res, nil
		}
	}

	// 2. 浏览器 profile 兜底
	return keepAliveByBrowser(res, headless)
}

// keepAliveByBrowser 用持久化浏览器 profile 续期。
// 只有在纯协议失败时才走这里——它要求本机装了 Chrome 且 profile 里还有登录态。
func keepAliveByBrowser(res *KeepAliveResult, headless bool) (*KeepAliveResult, error) {
	profileDir := config.HuaweiProfileDir()
	if err := os.MkdirAll(profileDir, 0o755); err != nil {
		return nil, err
	}
	if err := browser.EnsureDriver(); err != nil {
		return nil, err
	}
	pw, err := playwright.Run()
	if err != nil {
		return nil, err
	}
	defer pw.Stop()

	ctx, err := browser.LaunchPersistent(pw, profileDir, headless)
	if err != nil {
		return nil, err
	}
	defer ctx.Close()

	// 关键：profile 里只有持久 cookie，会话 cookie（CASTGC/SessionID）在关窗时被丢弃。
	// 先把上次导出的 cookie 灌回去，才能让 SSO 认出旧会话并续期。
	_ = browser.SeedCookies(ctx, config.CookieFile())

	var page playwright.Page
	if pages := ctx.Pages(); len(pages) > 0 {
		page = pages[0]
	} else {
		page, err = ctx.NewPage()
		if err != nil {
			return nil, err
		}
	}

	// 关键：先走 SSO 登录页（带 service 参数），而不是直接开业务页。
	//
	// 直接开 developer.huaweicloud.com/grow 时，即使 SSO 会话已失效，页面本身
	// 也可能返回 200（静态资源可访问），但页面内部调 devdata 接口会 404，
	// 于是被误判成「profile 也救不回来」。带 service 参数的 auth 登录页会让
	// SSO 主动做 OAuth 跳转：profile 里还有登录态就直接放行并下发新 cookie，
	// 真的失效了才停在登录页。
	ssoURL := "https://auth.huaweicloud.com/authui/login?service=" +
		url.QueryEscape(config.GrowURL) + "&locale=zh-cn"
	if _, err := page.Goto(ssoURL, playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateDomcontentloaded,
		Timeout:   playwright.Float(60000),
	}); err != nil {
		return nil, err
	}
	page.WaitForTimeout(6000)

	// SSO 放行后会跳到业务页；若仍停在 auth 域说明需要人工登录
	if strings.Contains(page.URL(), "auth.huaweicloud.com") {
		res.After = "expired"
		res.Message = "profile 里的 SSO 会话已失效，需要人工登录一次（hwcloud login）"
		return res, nil
	}

	// 再打开成长中心，确保业务 cookie 下发完整
	if _, err := page.Goto(config.GrowURL, playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateDomcontentloaded,
		Timeout:   playwright.Float(60000),
	}); err != nil {
		return nil, err
	}
	page.WaitForTimeout(5000)

	// 3. 在页面里探测登录态
	ok := false
	for i := 0; i < 6; i++ {
		v, err := page.Evaluate(probeLoginJS)
		if err == nil {
			if m, isMap := v.(map[string]any); isMap && len(m) > 0 {
				ok = true
				for _, k := range []string{"memAlias", "memName", "username"} {
					if s, isStr := m[k].(string); isStr && s != "" {
						res.Username = s
						break
					}
				}
				break
			}
		}
		page.WaitForTimeout(3000)
	}

	if !ok {
		res.After = "expired"
		res.Message = "profile 也无法恢复登录态，需要人工登录一次（hwcloud login）"
		return res, nil
	}

	// 4. 导出新 cookie
	if err := browser.SaveContextCookies(ctx, config.CookieFile()); err != nil {
		return nil, err
	}
	meta := map[string]any{}
	if data, err := os.ReadFile(config.MetaFile()); err == nil {
		_ = json.Unmarshal(data, &meta)
	}
	if ua, err := page.Evaluate("navigator.userAgent"); err == nil {
		if s, ok := ua.(string); ok && s != "" {
			meta["user_agent"] = s
		}
	}
	meta["login_at"] = time.Now().Format("2006-01-02 15:04:05")
	meta["refreshed_by"] = "keepalive"
	if data, err := json.MarshalIndent(meta, "", "  "); err == nil {
		_ = fsutil.WriteFileAtomic(config.MetaFile(), data, 0o600)
	}

	res.After = "alive"
	res.Refreshed = true
	res.Message = "已用 profile 刷新会话"
	return res, nil
}

// keepaliveCookies 是不影响登录态、可以安全丢弃/忽略的短效 cookie。
// _fr_ssid 是 WAF 的会话指纹，SessionID 是站点会话 cookie——
// 实测两者都过期后，SSO 仍能用 refresh token 换回新会话，
// 所以它们不能用来判断「登录是否快过期」。
var keepaliveCookies = map[string]bool{
	"_fr_ssid":          true,
	"SessionID":         true,
	"HWWAFSESID":        true,
	"HWWAFSESTIME":      true,
	"cbc-sid":           true,
	"cf":                true,
	"csrf-eco":          true,
	"_frid":             true,
	"vk":                true,
	"ua":                true,
	"HW_viewts_id1_cloud_huawei_com_auth_huaweicloud_com": true,
}

// authExpiry 返回真正决定登录态的那个 cookie 的过期时间。
// 优先用 refresh token（HW_refts_*），它才是会话能否续期的根。
func authExpiry() (time.Time, string, bool) {
	return authExpiryIn(config.CookieFile())
}

// authExpiryIn 同 authExpiry，但显式指定 cookie 文件路径。
// 供不依赖全局 accountID 的调用方使用（见 SessionInfoFor）。
func authExpiryIn(cookieFile string) (time.Time, string, bool) {
	data, err := os.ReadFile(cookieFile)
	if err != nil {
		return time.Time{}, "", false
	}
	var list []struct {
		Name    string  `json:"name"`
		Expires float64 `json:"expires"`
	}
	if json.Unmarshal(data, &list) != nil {
		return time.Time{}, "", false
	}

	// 1. refresh token 优先
	for _, c := range list {
		if strings.HasPrefix(c.Name, "HW_refts_") && c.Expires > 0 {
			return time.Unix(int64(c.Expires), 0), c.Name, true
		}
	}
	// 2. 退回到最晚过期的非 WAF cookie
	var latest float64
	var latestName string
	for _, c := range list {
		if c.Expires <= 0 || keepaliveCookies[c.Name] {
			continue
		}
		if c.Expires > latest {
			latest = c.Expires
			latestName = c.Name
		}
	}
	if latest > 0 {
		return time.Unix(int64(latest), 0), latestName, true
	}
	return time.Time{}, "", false
}

// remainingHours 返回登录凭据的剩余小时数；无法判断时返回 0。
func remainingHours() float64 {
	t, _, ok := authExpiry()
	if !ok {
		return 0
	}
	return time.Until(t).Hours()
}

// refreshByToken 用 SSO 的长期凭据重新换发站点会话。
//
// 真正的短效瓶颈是 ECO_CN_ID（.huaweicloud.com，会话 cookie，
// 值里内嵌过期时间戳 `..._-_<unix>_-_...`，有效期约 1 小时）。
// 它一旦过期，devdata 的所有接口都返回 404（伪装成「路由不存在」）。
//
// 但 ECO_CN_ID 不需要人工介入就能重建：先访问 developer.huaweicloud.com/grow，
// SSO 会用 HW_refts_*（refresh token，约半年）和 hwid_cas_sid（约 1 年）
// 重新签发一个 ECO_CN_ID，有效期直接到当天 08:00。
// 拿到之后再回到 devdata 调接口即可，全程不需要浏览器、不触发验证码。
func refreshByToken() error {
	data, err := os.ReadFile(config.CookieFile())
	if err != nil {
		return err
	}
	var list []struct {
		Name    string  `json:"name"`
		Value   string  `json:"value"`
		Domain  string  `json:"domain"`
		Path    string  `json:"path"`
		Expires float64 `json:"expires"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		return err
	}
	if len(list) == 0 {
		return fmt.Errorf("cookie 文件为空")
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		return err
	}
	byDomain := map[string][]*http.Cookie{}
	for _, c := range list {
		host := strings.TrimPrefix(c.Domain, ".")
		// 必须保留原始 Path：JSESSIONID 在 /UnifiedIDMPortal 下，
		// 改成 "/" 会让 CAS 找不到会话，SSO 续期直接失败。
		p := c.Path
		if p == "" {
			p = "/"
		}
		byDomain[host] = append(byDomain[host], &http.Cookie{
			Name: c.Name, Value: c.Value, Path: p, Domain: c.Domain,
		})
	}
	for host, cs := range byDomain {
		// 按 path 分组写入，否则同一 host 不同 path 的 cookie 会互相覆盖
		byPath := map[string][]*http.Cookie{}
		for _, c := range cs {
			byPath[c.Path] = append(byPath[c.Path], c)
		}
		for p, group := range byPath {
			jar.SetCookies(&url.URL{Scheme: "https", Host: host, Path: p}, group)
		}
	}

	ua := config.UserAgent()
	client := &http.Client{Jar: jar, Timeout: 30 * time.Second}

	// 第 1 步：访问 grow 页，逼 SSO 重新签发 ECO_CN_ID
	req, err := http.NewRequest("GET", config.GrowURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// 第 2 步：确认 devdata 的接口恢复
	req2, err := http.NewRequest("GET", config.DevData+"/api/get-ainfo", nil)
	if err != nil {
		return err
	}
	req2.Header.Set("User-Agent", ua)
	req2.Header.Set("Accept", "application/json, text/plain, */*")
	req2.Header.Set("Referer", config.GrowURL)
	resp2, err := client.Do(req2)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != 200 || resp2.Header.Get("csrf") == "" {
		return fmt.Errorf("SSO 未能重建会话（get-ainfo status=%d）", resp2.StatusCode)
	}

	// 第 3 步：把 jar 里最新的 cookie 写回文件
	return saveJarCookies(jar, list)
}

// saveJarCookies 把 cookie jar 里的最新状态写回 cookie 文件。
//
// 注意：http.CookieJar.Cookies() 返回的 *http.Cookie 不带 Domain/Path
// （Go 的 jar 在读出时会清空这两个字段），直接写文件会得到 domain="" 的废条目，
// 下次加载时这些 cookie 全部失效。所以这里用遍历时的 host 反填 Domain。
func saveJarCookies(jar http.CookieJar, original []struct {
	Name    string  `json:"name"`
	Value   string  `json:"value"`
	Domain  string  `json:"domain"`
	Path    string  `json:"path"`
	Expires float64 `json:"expires"`
}) error {
	var out []map[string]any
	seen := map[string]bool{}
	for _, host := range cookieHosts {
		for _, c := range jar.Cookies(&url.URL{Scheme: "https", Host: host, Path: "/"}) {
			// jar 会清空 Domain/Path，用查询用的 host 反填；
			// .huaweicloud.com 这类父域 cookie 在两个子域都能读到，用带点形式保留共享语义。
			domain := c.Domain
			if domain == "" {
				domain = host
			}
			path := c.Path
			if path == "" {
				path = "/"
			}
			key := c.Name + "|" + domain + "|" + path
			if seen[key] {
				continue
			}
			seen[key] = true
			exp := float64(-1)
			if !c.Expires.IsZero() {
				exp = float64(c.Expires.Unix())
			}
			out = append(out, map[string]any{
				"name": c.Name, "value": c.Value, "domain": domain,
				"path": path, "expires": exp,
			})
		}
	}
	for _, c := range original {
		key := c.Name + "|" + c.Domain + "|" + c.Path
		if seen[key] || c.Expires <= 0 {
			continue
		}
		seen[key] = true
		out = append(out, map[string]any{
			"name": c.Name, "value": c.Value, "domain": c.Domain,
			"path": c.Path, "expires": c.Expires,
		})
	}
	blob, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(config.CookieFile(), blob, 0o600)
}

// cookieHosts 是导出 cookie 时需要遍历的域。
var cookieHosts = []string{
	"devdata.huaweicloud.com",
	"developer.huaweicloud.com",
	"bbs.huaweicloud.com",
	"id1.cloud.huawei.com",
	"auth.huaweicloud.com",
	"huaweicloud.com",
}

// SessionInfo 返回当前会话的剩余信息。
func SessionInfo() (map[string]any, error) {
	sess, err := huaweiSession()
	if err != nil {
		return nil, err
	}
	info, err := sess.CheckLogin()
	if err != nil {
		return nil, err
	}
	out := map[string]any{"alive": info != nil}
	if info == nil {
		return out, nil
	}
	out["username"] = sess.Username

	// 报告真正决定登录态的凭据（refresh token），而不是 WAF 短效 cookie
	if t, name, ok := authExpiry(); ok {
		out["auth_expiry"] = t.Format("2006-01-02 15:04:05")
		out["auth_cookie"] = name
		out["auth_remaining_hours"] = time.Until(t).Hours()
		out["auth_remaining_days"] = time.Until(t).Hours() / 24
	}

	// 顺带给出 WAF 短效 cookie 的信息（仅供排查，过期不影响登录）
	data, err := os.ReadFile(config.CookieFile())
	if err == nil {
		var list []struct {
			Name    string  `json:"name"`
			Expires float64 `json:"expires"`
		}
		if json.Unmarshal(data, &list) == nil {
			var earliest float64
			var earliestName string
			for _, c := range list {
				if c.Expires <= 0 {
					continue
				}
				if earliest == 0 || c.Expires < earliest {
					earliest = c.Expires
					earliestName = c.Name
				}
			}
			if earliest > 0 {
				t := time.Unix(int64(earliest), 0)
				out["shortest_expiry"] = t.Format("2006-01-02 15:04:05")
				out["shortest_cookie"] = earliestName
				out["shortest_remaining_hours"] = time.Until(t).Hours()
			}
		}
	}
	return out, nil
}

// SessionInfoFor 是 SessionInfo 的「不依赖全局 accountID」版本。
//
// 为什么需要：SessionInfo 内部走 huaweiSession() → huawei.New("") →
// config.CookieFile()，而 CookieFile() 从进程级全局变量推导路径。
// 所以调用方必须先拿全局锁把 accountID 切到目标账号 —— 而周常/保活
// 这类长任务会把锁持有 5-10 分钟，期间点「检查」只能干等或报忙。
//
// 这个版本直接接收账号 state 目录，不碰任何全局状态，可以并发调用。
// dir 形如 /opt/hwcloud-demo/accounts/<phone>/state。
func SessionInfoFor(dir string) (map[string]any, error) {
	sess, err := huawei.NewForAccount(dir)
	if err != nil {
		return nil, err
	}
	info, err := sess.CheckLogin()
	if err != nil {
		return nil, err
	}
	out := map[string]any{"alive": info != nil}
	if info == nil {
		return out, nil
	}
	out["username"] = sess.Username

	cookieFile := filepath.Join(dir, "cookies.json")

	// 报告真正决定登录态的凭据（refresh token），而不是 WAF 短效 cookie
	if t, name, ok := authExpiryIn(cookieFile); ok {
		out["auth_expiry"] = t.Format("2006-01-02 15:04:05")
		out["auth_cookie"] = name
		out["auth_remaining_hours"] = time.Until(t).Hours()
		out["auth_remaining_days"] = time.Until(t).Hours() / 24
	}

	// 顺带给出 WAF 短效 cookie 的信息（仅供排查，过期不影响登录）
	if data, err := os.ReadFile(cookieFile); err == nil {
		var list []struct {
			Name    string  `json:"name"`
			Expires float64 `json:"expires"`
		}
		if json.Unmarshal(data, &list) == nil {
			var earliest float64
			var earliestName string
			for _, c := range list {
				if c.Expires <= 0 {
					continue
				}
				if earliest == 0 || c.Expires < earliest {
					earliest = c.Expires
					earliestName = c.Name
				}
			}
			if earliest > 0 {
				t := time.Unix(int64(earliest), 0)
				out["shortest_expiry"] = t.Format("2006-01-02 15:04:05")
				out["shortest_cookie"] = earliestName
				out["shortest_remaining_hours"] = time.Until(t).Hours()
			}
		}
	}
	return out, nil
}

// profileDirExists 检查 profile 是否已初始化。
func profileDirExists() bool {
	st, err := os.Stat(filepath.Join(config.HuaweiProfileDir(), "Default"))
	return err == nil && st.IsDir()
}
