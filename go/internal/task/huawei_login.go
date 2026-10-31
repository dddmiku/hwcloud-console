package task

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/playwright-community/playwright-go"

	"hwcloud/internal/browser"
	"hwcloud/internal/config"
	"hwcloud/internal/fsutil"
)

const probeLoginJS = `async () => {
  try {
    const base = 'https://devdata.huaweicloud.com';
    const r = await fetch(base + '/api/get-ainfo', {credentials: 'include'});
    const csrf = r.headers.get('csrf');
    if (!csrf) return null;
    const p = await fetch(base + '/rest/developer/fwdu/rest/developer/user/hdcommunityservice/v1/member/get-personal-info',
      {credentials: 'include', headers: {csrf}});
    if (!p.ok) return null;
    const d = await p.json();
    return (d && !d.error_code) ? d : null;
  } catch (e) { return null; }
}`

// probeLoginPage 在页面内发起 fetch，走 Chrome 网络栈（包含系统代理）。
func probeLoginPage(page playwright.Page) map[string]any {
	v, err := page.Evaluate(probeLoginJS)
	if err != nil {
		return nil
	}
	if m, ok := v.(map[string]any); ok && len(m) > 0 {
		return m
	}
	return nil
}

// extractHwidSid 从浏览器里取出 hwid_cas_sid。
//
// 它是华为 SDK 的设备标识，存在 localStorage 里，登录成功后才有值。
// 设备被信任后这个值会稳定下来，之后纯协议请求带上它就能免验证码。
//
// 取值的几个来源按优先级：
//  1. localStorage.hwid_cas_sid —— SDK 自己存的位置
//  2. sessionStorage.hwid_cas_sid —— 某些流程只放 session 里
//  3. 页面上的 hwidSDK / hwidConfig 对象
func extractHwidSid(ctx playwright.BrowserContext, page playwright.Page) string {
	// 先看当前页面
	if v := readSidFromPage(page); v != "" {
		return v
	}

	// 当前页面可能不在华为登录域下，再去开一个专门读
	tmp, err := ctx.NewPage()
	if err != nil {
		return ""
	}
	defer tmp.Close()

	for _, u := range []string{
		"https://auth.huaweicloud.com/authui/login.html?service=https%3A%2F%2Fwww.huaweicloud.com%2F&locale=zh-cn",
		"https://id1.cloud.huawei.com/UnifiedIDMPortal/portal/index.html",
	} {
		if _, err := tmp.Goto(u, playwright.PageGotoOptions{
			WaitUntil: playwright.WaitUntilStateDomcontentloaded,
			Timeout:   playwright.Float(20000),
		}); err != nil {
			continue
		}
		if v := readSidFromPage(tmp); v != "" {
			return v
		}
	}
	return ""
}

const readSidJS = `() => {
  const out = {};
  try { out.ls = localStorage.getItem('hwid_cas_sid') || ''; } catch (e) {}
  try { out.ss = sessionStorage.getItem('hwid_cas_sid') || ''; } catch (e) {}
  try {
    if (window.localStorage && localStorage.hwid_cas_sid) out.raw = localStorage.hwid_cas_sid;
  } catch (e) {}
  try {
    if (window.hwidSDK && typeof window.hwidSDK.getSid === 'function') {
      out.sdk = window.hwidSDK.getSid() || '';
    }
  } catch (e) {}
  return out;
}`

func readSidFromPage(page playwright.Page) string {
	v, err := page.Evaluate(readSidJS)
	if err != nil {
		return ""
	}
	m, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	for _, k := range []string{"ls", "ss", "raw", "sdk"} {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// LoginHuawei 打开浏览器手动登录，成功后导出 cookie 与 UA。
func LoginHuawei(timeoutSec int) error {
	if err := browser.EnsureDriver(); err != nil {
		return err
	}
	pw, err := playwright.Run()
	if err != nil {
		return err
	}
	defer pw.Stop()

	if err := os.MkdirAll(config.HuaweiProfileDir(), 0o755); err != nil {
		return err
	}
	ctx, err := browser.LaunchPersistent(pw, config.HuaweiProfileDir(), false)
	if err != nil {
		return err
	}
	defer ctx.Close()
	// 先注入已保存的 cookie，会话仍有效时无需再次手动登录
	_ = browser.SeedCookies(ctx, config.CookieFile())

	var page playwright.Page
	if pages := ctx.Pages(); len(pages) > 0 {
		page = pages[0]
	} else {
		page, err = ctx.NewPage()
		if err != nil {
			return err
		}
	}
	if _, err := page.Goto(config.GrowURL, playwright.PageGotoOptions{WaitUntil: playwright.WaitUntilStateDomcontentloaded}); err != nil {
		return err
	}
	ua := config.DefaultUA
	if v, err := page.Evaluate("navigator.userAgent"); err == nil {
		if s, ok := v.(string); ok && s != "" {
			ua = s
		}
	}

	fmt.Println("[*] 浏览器已打开，请在窗口内完成华为云登录（账号密码 / 短信 / 扫码均可）。")
	fmt.Println("[*] 登录成功后会自动保存会话，无需手动关闭窗口。")

	deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)
	var personal map[string]any
	for time.Now().Before(deadline) {
		if page.IsClosed() {
			return fmt.Errorf("浏览器窗口被关闭，登录流程中止")
		}
		if m := probeLoginPage(page); m != nil {
			personal = m
			break
		}
		time.Sleep(3 * time.Second)
	}
	if personal == nil {
		return fmt.Errorf("超时未检测到登录态")
	}

	if err := browser.SaveContextCookies(ctx, config.CookieFile()); err != nil {
		return err
	}

	// 抓 hwid_cas_sid：这是设备信任的钥匙。
	// SDK 把它存在 localStorage.hwid_cas_sid，之后所有 ajaxHandler 请求都会自动带上。
	// 有了它，纯协议登录就不再触发短信 / 验证码。
	sid := extractHwidSid(ctx, page)
	if sid != "" {
		if err := SaveHwidSid(sid); err != nil {
			fmt.Printf("[!] hwid_cas_sid 保存失败：%v\n", err)
		} else {
			fmt.Printf("[+] hwid_cas_sid 已保存：%s...\n", sid[:min(24, len(sid))])
		}
	} else {
		fmt.Println("[!] 未取到 hwid_cas_sid（页面里没有该值，可能是这次没用账密登录）")
	}
	meta := map[string]any{}
	if data, err := os.ReadFile(config.MetaFile()); err == nil {
		_ = json.Unmarshal(data, &meta)
	}
	meta["user_agent"] = ua
	meta["login_at"] = time.Now().Format("2006-01-02 15:04:05")
	if data, err := json.MarshalIndent(meta, "", "  "); err == nil {
		_ = fsutil.WriteFileAtomic(config.MetaFile(), data, 0o600)
	}

	username := ""
	for _, key := range []string{"memAlias", "memName", "username"} {
		if v, ok := personal[key].(string); ok && v != "" {
			username = v
			break
		}
	}
	if username == "" {
		username = "未知用户"
	}
	fmt.Printf("[+] 登录成功：%s\n", username)
	fmt.Printf("[+] 会话已保存：%s\n", config.CookieFile())
	return nil
}