package task

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/playwright-community/playwright-go"

	"hwcloud/internal/browser"
)

// 云实验列表（KooLabs）。
//
// 为什么需要它：
//   原来的 RunLabTask 固定跑 2325 号实验。但每个实验每天有独立免费名额
//   （free_num），2325 名额用完后接口不报错、点「开始实验」也照样能点，
//   只是会扣实验点，或者干脆卡在「实验准备中」直到超时。
//   实测 372 个实验里有 290 个当天还有名额 —— 没必要死磕一个。
//
// 鉴权链路的坑：
//   列表接口 data.edu.huaweicloud.com/.../experiment-list-for-edu 需要
//   `auth-token` 请求头，而这个 JWT **不在 cookies.json 里** ——
//   它是前端脚本写进 sessionStorage 的（key = "setToken"）。
//   所以纯协议拿不到，必须先开一次浏览器把它读出来，
//   之后的翻页就可以走纯 HTTP 了。

const (
	labListURL = "https://data.edu.huaweicloud.com/rest/developer/user/hdsandboxexpservice/v1/warefree/experiment-list-for-edu"
	labListRef = "https://edu.huaweicloud.com/"
)

// LabInfo 实验条目
type LabInfo struct {
	ID      int    `json:"id"`
	Title   string `json:"title"`
	FreeNum int    `json:"free_num"`
	Price   string `json:"price"`
	Level   string `json:"difficult"`
	Times   int    `json:"times"`
}

// labAuthToken 开一次浏览器，从 sessionStorage 读 auth-token。
//
// 两个必须注意的点：
//  1. 必须 headed。无头模式下前端那段写 setToken 的脚本不跑，
//     sessionStorage 是空的 —— 实测直接返回「页面没给出 auth-token」。
//  2. 只等 domcontentloaded 不够，token 是后续 XHR 回来后由 JS 写进去的，
//     要轮询等到它出现。
//
// 会话来源：cookieFile 指向账号 state 目录下的 cookies.json。
// 不能依赖 config.CookieFile() —— 那是进程级全局，而 labs 走的是
// 无锁分支（NewForAccount），全局账号没被设置，会读到别的账号甚至空会话，
// 页面表现成「未登录」。
func labAuthToken(headless bool, cookieFile string) (token, cookie string, err error) {
	pw, err := playwright.Run()
	if err != nil {
		return "", "", err
	}
	defer pw.Stop()

	br, err := browser.Launch(pw, headless)
	if err != nil {
		return "", "", err
	}
	defer br.Close()

	ctx, err := br.NewContext(playwright.BrowserNewContextOptions{
		Viewport:  &playwright.Size{Width: 1600, Height: 1000},
		UserAgent: playwright.String("Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:156.0) Gecko/20100101 Firefox/156.0"),
	})
	if err != nil {
		return "", "", err
	}
	defer ctx.Close()

	if err := browser.SeedCookies(ctx, cookieFile); err != nil {
		return "", "", fmt.Errorf("注入 Cookie 失败: %v", err)
	}

	page, err := ctx.NewPage()
	if err != nil {
		return "", "", err
	}
	if _, err := page.Goto(labListRef+"lab/", playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateDomcontentloaded,
		Timeout:   playwright.Float(60000),
	}); err != nil {
		return "", "", err
	}

	// 轮询 30 秒等 token 落地
	for i := 0; i < 30; i++ {
		page.WaitForTimeout(1000)
		raw, e := page.Evaluate(`() => sessionStorage.getItem('setToken') || ''`)
		if e != nil {
			continue
		}
		if s, _ := raw.(string); s != "" {
			token = s
			break
		}
	}
	if token == "" {
		body, _ := page.Locator("body").InnerText()
		return "", "", fmt.Errorf("页面没给出 auth-token（sessionStorage.setToken 为空，页面=%s）",
			harmonyClip(body))
	}

	cookies, err := ctx.Cookies()
	if err != nil {
		return token, "", nil
	}
	var b strings.Builder
	for _, c := range cookies {
		if !strings.Contains(c.Domain, "huaweicloud.com") {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("; ")
		}
		b.WriteString(c.Name)
		b.WriteByte('=')
		b.WriteString(c.Value)
	}
	return token, b.String(), nil
}

// ListLabs 列出实验。cookieFile 为账号 state 目录下的 cookies.json。
func ListLabs(headless bool, cookieFile string, maxPages int) ([]LabInfo, error) {
	token, cookie, err := labAuthToken(headless, cookieFile)
	if err != nil {
		return nil, err
	}
	return listLabsWith(token, cookie, maxPages)
}

func listLabsWith(token, cookie string, maxPages int) ([]LabInfo, error) {
	if maxPages <= 0 {
		maxPages = 12
	}
	cli := &http.Client{Timeout: 25 * time.Second}
	out := make([]LabInfo, 0, 256)

	for pageNo := 1; pageNo <= maxPages; pageNo++ {
		body, _ := json.Marshal(map[string]any{
			"course_id": 0, "page_size": 50, "hots": "", "level": "",
			"news": "", "page_index": pageNo, "title": "",
		})
		req, err := http.NewRequest(http.MethodPost, labListURL, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:156.0) Gecko/20100101 Firefox/156.0")
		req.Header.Set("Cookie", cookie)
		req.Header.Set("auth-token", token)
		req.Header.Set("Content-Type", "application/json;charset=UTF-8")
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		req.Header.Set("Origin", "https://edu.huaweicloud.com")
		req.Header.Set("Referer", labListRef)
		req.Header.Set("Accept", "application/json, text/plain, */*")

		resp, err := cli.Do(req)
		if err != nil {
			return nil, err
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("实验列表 HTTP %d: %s", resp.StatusCode, harmonyClip(string(data)))
		}
		var j struct {
			Result struct {
				List []LabInfo `json:"experiment_list"`
			} `json:"result"`
		}
		if err := json.Unmarshal(data, &j); err != nil {
			return nil, fmt.Errorf("解析实验列表失败: %v", err)
		}
		if len(j.Result.List) == 0 {
			break
		}
		out = append(out, j.Result.List...)
		if len(j.Result.List) < 50 {
			break
		}
	}
	return out, nil
}

// PickFreeLab 挑一个当天还有免费名额、且时长最短的实验。
//
// 优先短实验（times 小），因为要等环境起来 + 跑够时长，
// 60 分钟的比 2 小时的省一半时间，收益一样。
func PickFreeLab(headless bool, cookieFile string, preferID int) (LabInfo, error) {
	labs, err := ListLabs(headless, cookieFile, 12)
	if err != nil {
		return LabInfo{}, err
	}

	// 指定的实验如果还有名额，优先用
	if preferID > 0 {
		for _, l := range labs {
			if l.ID == preferID && l.FreeNum > 0 {
				return l, nil
			}
		}
	}

	best := LabInfo{}
	found := false
	for _, l := range labs {
		if l.FreeNum <= 0 {
			continue
		}
		if !found || l.Times < best.Times || (l.Times == best.Times && l.FreeNum > best.FreeNum) {
			best = l
			found = true
		}
	}
	if !found {
		return LabInfo{}, fmt.Errorf("当前没有剩余免费名额的实验")
	}
	return best, nil
}
