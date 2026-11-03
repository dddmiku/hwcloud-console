package task

import (
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

// 鸿蒙实践中心的 GitCode 绑定（GitCode Star 任务的前置条件）。
//
// 为什么必须走浏览器：
//   GitCode 的 /oauth/authorize 页面是纯 SPA（<div id="app"></div> 里
//   由 JS 渲染授权按钮），用 curl 拿到的 HTML 里没有任何表单或 code。
//   所以只能开真浏览器，载入 GitCode 会话，点「授权」，
//   让它跳到 harmonybp 的 /oauth/gitcode/callback?code=...&state=...
//
// 两个会话缺一不可：
//   - harmonybp 会话（华为云 Cookie）→ 拿 authorize_url、最后提交绑定
//   - gitcode 会话（gitcode_state.json）→ 在 GitCode 侧确认授权
//
// 提交绑定的接口是 POST /api/growth/bindings/gitcode，body 是
// {"code": "...", "state": "..."}（见 OAuthCallbackView 的 y("gitcode", {code, state})）。
// 直接传 username 会 400 git_login_required —— 绑定认的是 OAuth code，
// 不是用户名。

// HarmonyBindResult 绑定结果
type HarmonyBindResult struct {
	OK      bool   `json:"ok"`
	Code    string `json:"code,omitempty"`
	State   string `json:"state,omitempty"`
	Status  string `json:"status"`
	Err     string `json:"error,omitempty"`
	GitName string `json:"git_name,omitempty"`
}

// HarmonyBindGitCode 完成 GitCode → 鸿蒙实践中心的 OAuth 绑定。
//
// 步骤：
//  1. 用华为云会话取 authorize_url
//  2. 开浏览器载入 GitCode 会话，访问 authorize_url，点授权
//  3. 拦截跳到 harmonybp/oauth/gitcode/callback 的请求，取出 code/state
//  4. 用华为云会话 POST /api/growth/bindings/gitcode {code, state}
func HarmonyBindGitCode(sess *huawei.Session, stateDir string, headless bool) (map[string]any, error) {
	c, err := newHarmonyClient(sess)
	if err != nil {
		return nil, err
	}

	// 已经绑过就不重复
	if st, raw, err := c.call("/api/growth/bindings", http.MethodGet, nil); err == nil && st == http.StatusOK {
		var b struct {
			Bindings []map[string]any `json:"bindings"`
		}
		if json.Unmarshal(raw, &b) == nil && len(b.Bindings) > 0 {
			return map[string]any{
				"ok":       true,
				"already":  true,
				"bindings": b.Bindings,
				"msg":      "GitCode 已绑定，无需重复",
			}, nil
		}
	}

	// 1. authorize_url
	st, raw, err := c.call("/api/growth/bindings/gitcode/oauth/url", http.MethodGet, nil)
	if err != nil {
		return nil, err
	}
	if st != http.StatusOK {
		return nil, fmt.Errorf("取授权地址 HTTP %d: %s", st, harmonyClip(string(raw)))
	}
	var urlResp struct {
		Configured   bool   `json:"configured"`
		AuthorizeURL string `json:"authorize_url"`
	}
	if err := json.Unmarshal(raw, &urlResp); err != nil {
		return nil, fmt.Errorf("解析授权地址失败: %v", err)
	}
	if urlResp.AuthorizeURL == "" {
		return nil, fmt.Errorf("服务端没给 authorize_url（configured=%v）", urlResp.Configured)
	}

	// 2. 浏览器授权
	gcState := gitcodeStateFileIn(stateDir)
	if _, err := os.Stat(gcState); err != nil {
		return nil, fmt.Errorf("未找到 GitCode 会话 %s，请先运行 GitCode 登录", gcState)
	}

	pw, err := playwright.Run()
	if err != nil {
		return nil, err
	}
	defer pw.Stop()

	// GitCode 对无头浏览器有行为判定，必须 headed
	br, ctx, err := browser.NewStorageContext(pw, gcState, headless)
	if err != nil {
		return nil, err
	}
	defer br.Close()
	defer ctx.Close()

	page, err := ctx.NewPage()
	if err != nil {
		return nil, err
	}

	// 拦截 callback：GitCode 授权后会 302 到
	// https://harmonybp.developer.huaweicloud.com/oauth/gitcode/callback?code=..&state=..
	var gotCode, gotState string
	page.On("request", func(r playwright.Request) {
		u := r.URL()
		if !strings.Contains(u, "/oauth/gitcode/callback") {
			return
		}
		if i := strings.Index(u, "code="); i >= 0 {
			v := u[i+5:]
			if j := strings.IndexAny(v, "&"); j >= 0 {
				v = v[:j]
			}
			gotCode = v
		}
		if i := strings.Index(u, "state="); i >= 0 {
			v := u[i+6:]
			if j := strings.IndexAny(v, "&"); j >= 0 {
				v = v[:j]
			}
			gotState = v
		}
	})

	if _, err := page.Goto(urlResp.AuthorizeURL, playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateDomcontentloaded,
		Timeout:   playwright.Float(60000),
	}); err != nil {
		return nil, fmt.Errorf("打开授权页失败: %v", err)
	}
	page.WaitForTimeout(6000)

	// 3. 点授权。GitCode 的授权页按钮文案实测是「授权」/「同意授权」/「Authorize」。
	//
	// 用 button:has-text 而不是 GetByRole：playwright-go v0.6100 的
	// GetByRole 在 Name 传 *string 时会 panic（escapeForAttributeSelector
	// 里做 interface{} → string 断言，收到的是 *string）。这个版本有 bug，
	// 选择器写法更稳。
	clicked := false
	for _, label := range []string{"同意授权", "确认授权", "授权", "Authorize", "允许"} {
		for _, sel := range []string{
			`button:has-text("%s")`,
			`.btn:has-text("%s")`,
			`a:has-text("%s")`,
		} {
			loc := page.Locator(fmt.Sprintf(sel, label))
			if n, _ := loc.Count(); n == 0 {
				continue
			}
			if vis, _ := loc.First().IsVisible(); !vis {
				continue
			}
			if err := loc.First().Click(playwright.LocatorClickOptions{
				Timeout: playwright.Float(8000),
			}); err == nil {
				clicked = true
				fmt.Printf("[*] 点击授权：%s（%s）\n", label, sel)
				break
			}
		}
		if clicked {
			break
		}
	}

	// 等 callback（最多 30 秒）
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && gotCode == "" {
		page.WaitForTimeout(1000)
		// 页面本身也可能已经跳到 callback
		if strings.Contains(page.URL(), "/oauth/gitcode/callback") {
			u := page.URL()
			if i := strings.Index(u, "code="); i >= 0 {
				v := u[i+5:]
				if j := strings.IndexAny(v, "&"); j >= 0 {
					v = v[:j]
				}
				gotCode = v
			}
			if i := strings.Index(u, "state="); i >= 0 {
				v := u[i+6:]
				if j := strings.IndexAny(v, "&"); j >= 0 {
					v = v[:j]
				}
				gotState = v
			}
		}
	}

	if gotCode == "" {
		body, _ := page.Locator("body").InnerText()
		return map[string]any{
			"ok":      false,
			"clicked": clicked,
			"url":     page.URL(),
			"page":    harmonyClip(body),
			"msg":     "没拿到 OAuth code：授权按钮可能没点到，或 GitCode 会话已失效",
		}, nil
	}
	if gotState == "" {
		gotState = strings.TrimPrefix(urlResp.AuthorizeURL[strings.Index(urlResp.AuthorizeURL, "state=")+6:], "")
		if i := strings.Index(gotState, "&"); i >= 0 {
			gotState = gotState[:i]
		}
	}

	// 4. 提交绑定
	st, raw, err = c.call("/api/growth/bindings/gitcode", http.MethodPost,
		map[string]any{"code": gotCode, "state": gotState})
	if err != nil {
		return nil, err
	}
	if st != http.StatusOK {
		return map[string]any{
			"ok":     false,
			"status": fmt.Sprintf("HTTP %d", st),
			"detail": harmonyClip(string(raw)),
			"code":   gotCode,
		}, nil
	}

	var bindResp map[string]any
	_ = json.Unmarshal(raw, &bindResp)
	out := map[string]any{"ok": true, "code": gotCode, "bind": bindResp}

	// 顺手复查一次 Star 任务状态
	if st2, raw2, err2 := c.call("/api/growth/tasks/gitcode-star", http.MethodGet, nil); err2 == nil && st2 == http.StatusOK {
		var t2 struct {
			Task struct {
				TodayStatus string `json:"today_status"`
				GitBound    bool   `json:"git_bound"`
				GitName     string `json:"git_name"`
			} `json:"task"`
		}
		if json.Unmarshal(raw2, &t2) == nil {
			out["git_bound"] = t2.Task.GitBound
			out["git_name"] = t2.Task.GitName
			out["task_status"] = t2.Task.TodayStatus
		}
	}
	return out, nil
}

// gitcodeStateFileIn 返回指定账号 state 目录下的 GitCode 会话文件。
func gitcodeStateFileIn(stateDir string) string {
	return stateDir + "/gitcode_state.json"
}
