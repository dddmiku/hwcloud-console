package task

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"hwcloud/internal/config"
	"hwcloud/internal/huawei"
	"hwcloud/internal/fsutil"
)

// 华为云账号密码登录的协议（2026-10 实测）：
//
//  1. GET  auth.huaweicloud.com/authui/login.html  取 SessionID 等基础 cookie
//  2. GET  auth.huaweicloud.com/authui/getSDKBaseInfo  取 flowType / site
//  3. POST id1.cloud.huawei.com/UnifiedIDMPortal/ajaxHandler/login/jsRemoteLogin
//     拿到 pageToken / pageTokenKey / state 三元组
//  4. POST .../login/unionLoginByPwd  提交明文账号密码
//  5. 若返回二次验证，需 POST 短信验证码；通过后写 SSO cookie
//
// 第 5 步依赖手机验证码，无法全自动；但第 1-4 步可以，且第 4 步之后
// 会发一条短信到绑定手机。

const (
	authBase   = "https://auth.huaweicloud.com"
	idpBase    = "https://id1.cloud.huawei.com"
	unifiedPth = "/UnifiedIDMPortal/ajaxHandler"
	// 华为云开发者中心的回调地址，登录成功后跳回这里
	loginService = "https://developer.huaweicloud.com/grow"
)

// loginTokens 是 UnifiedIDMPortal 每次会话都要带的三元组。
type loginTokens struct {
	PageToken      string
	PageTokenKey   string
	State          string
	LocalStorageID string
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return strings.Repeat("0", n*2)
	}
	return hex.EncodeToString(b)
}

func randomAlnum(n int) string {
	const chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return strings.Repeat("A", n)
	}
	for i := range b {
		b[i] = chars[int(b[i])%len(chars)]
	}
	return string(b)
}

// PasswordLoginOptions 描述一次密码登录。
type PasswordLoginOptions struct {
	Account  string
	Password string
	// SMSCode 为空时只做「账号密码校验」这一步，返回 NeedSMS 提示；
	// 填了验证码才完成整条链路。
	SMSCode string
	Timeout int
}

// PasswordLoginResult 是登录结果。
type PasswordLoginResult struct {
	NeedSMS     bool   `json:"need_sms"`
	Message     string `json:"message,omitempty"`
	Username    string `json:"username,omitempty"`
	CookiesSaved string `json:"cookies_saved,omitempty"`
}

// PasswordLogin 走纯协议登录华为云。
func PasswordLogin(opts PasswordLoginOptions) (*PasswordLoginResult, error) {
	if opts.Account == "" || opts.Password == "" {
		return nil, fmt.Errorf("需要账号和密码")
	}
	sess, err := newAuthSession()
	if err != nil {
		return nil, err
	}

	// 1. 打开登录页，拿基础 cookie
	if err := sess.bootstrap(); err != nil {
		return nil, err
	}

	// 2. 取三元组
	tokens, err := sess.remoteLoginTokens()
	if err != nil {
		return nil, err
	}

	// 3. 提交账号密码
	body, err := sess.unionLoginByPwd(tokens, opts.Account, opts.Password)
	if err != nil {
		return nil, err
	}
	if msg := detectLoginError(body); msg != "" {
		return &PasswordLoginResult{NeedSMS: false, Message: msg}, nil
	}

	// 4. 判断是否要求二次验证
	if needsSMS(body) {
		if opts.SMSCode == "" {
			return &PasswordLoginResult{
				NeedSMS: true,
				Message: "账号密码校验通过，需要短信验证码（已发送到绑定手机）。带 --sms 再执行一次",
			}, nil
		}
		if err := sess.submitSMS(tokens, opts.SMSCode); err != nil {
			return nil, err
		}
	}

	// 5. 落盘 cookie
	if err := sess.saveCookies(); err != nil {
		return nil, err
	}
	return &PasswordLoginResult{
		NeedSMS:      false,
		Username:     opts.Account,
		CookiesSaved: config.CookieFile(),
	}, nil
}

// authSession 是登录期用的 HTTP 会话（独立于业务 Session）。
type authSession struct {
	client *http.Client
	ua     string
	jar    map[string]string
}

func newAuthSession() (*authSession, error) {
	return &authSession{
		client: &http.Client{Timeout: 30 * time.Second},
		ua:     config.DefaultUA,
		jar:    map[string]string{},
	}, nil
}

func (a *authSession) do(req *http.Request) ([]byte, int, error) {
	req.Header.Set("User-Agent", a.ua)
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	if len(a.jar) > 0 {
		var parts []string
		for k, v := range a.jar {
			parts = append(parts, k+"="+v)
		}
		req.Header.Set("Cookie", strings.Join(parts, "; "))
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	for _, ck := range resp.Cookies() {
		a.jar[ck.Name] = ck.Value
	}
	data, _ := io.ReadAll(resp.Body)
	return data, resp.StatusCode, nil
}

// bootstrap 访问登录页与 SDK 信息接口，建立基础会话。
// 华为云的前端会在 localStorage 里生成 cbc-sid / hwid_cas_sid 两个会话 ID，
// 并作为 cookie 回传；缺少它们时 id1 会拒绝 jsRemoteLogin（10006003）。
func (a *authSession) bootstrap() error {
	u := authBase + "/authui/login.html?service=" +
		url.QueryEscape(loginService) + "&locale=zh-cn"
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	if _, _, err := a.do(req); err != nil {
		return err
	}

	// 前端在 localStorage 生成、并同步成 cookie 的两个会话 ID
	if a.jar["cbc-sid"] == "" {
		a.jar["cbc-sid"] = "204937" + randomHex(35)
	}
	if a.jar["hwid_cas_sid"] == "" {
		a.jar["hwid_cas_sid"] = "204937" + randomHex(35)
	}

	u2 := authBase + "/authui/getSDKBaseInfo?flowType=unionLogin&service=" +
		url.QueryEscape(loginService) + "&site=mainland"
	req2, _ := http.NewRequest(http.MethodGet, u2, nil)
	req2.Header.Set("Referer", authBase+"/authui/login.html")
	_, _, err := a.do(req2)
	return err
}

// remoteLoginTokens 拿 pageToken / pageTokenKey。
// 注意：state 与 localStorageID 由前端生成后作为**输入**传给 jsRemoteLogin，
// 不是响应里取的；实测浏览器用 32 位 hex 的 state。
func (a *authSession) remoteLoginTokens() (*loginTokens, error) {
	state := randomHex(16)
	localStorageID := randomAlnum(51)

	form := url.Values{}
	form.Set("reqClientType", "88")
	form.Set("loginChannel", "88000000")
	form.Set("clientID", "103493351")
	form.Set("lang", "zh-cn")
	form.Set("languageCode", "zh-cn")
	form.Set("state", state)
	form.Set("loginUrl", authBase+"/authui/login.html#/hwIDLogin")
	form.Set("service", authBase+"/authui/casLogin?service="+url.QueryEscape(loginService))
	form.Set("themeName", "lightred")
	form.Set("jsSiteID", "1")
	form.Set("scope", "https://www.huawei.com/auth/account/unified.profile+https://www.huawei.com/auth/account/risk.idstate+LoginState")
	form.Set("client_id", "103493351")
	form.Set("access_type", "offline")
	form.Set("regionCode", "cn")
	form.Set("localStorageID", localStorageID)

	u := idpBase + unifiedPth + "/login/jsRemoteLogin?reflushCode=" +
		fmt.Sprintf("%.16f", float64(time.Now().UnixNano())/1e9) +
		"&cVersion=UP_CAS_6.26.3.100_green"
	req, _ := http.NewRequest(http.MethodPost, u, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", authBase+"/authui/login.html")

	data, status, err := a.do(req)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("jsRemoteLogin 失败 HTTP %d: %s", status, clipBody(data))
	}
	t, err := parseTokens(data)
	if err != nil {
		return nil, err
	}
	// state 是输入值，响应里通常没有，补回去
	t.State = state
	t.LocalStorageID = localStorageID
	return t, nil
}

// parseTokens 从响应里提取三元组；接口可能返回 JSON 也可能返回带变量的 JS。
func parseTokens(data []byte) (*loginTokens, error) {
	s := string(data)
	get := func(keys ...string) string {
		for _, k := range keys {
			for _, pat := range []string{
				`"` + k + `":"`, `"` + k + `": "`, k + `=`,
			} {
				if i := strings.Index(s, pat); i >= 0 {
					rest := s[i+len(pat):]
					if len(pat) > 0 && pat[len(pat)-1] == '"' {
						if j := strings.Index(rest, `"`); j > 0 {
							return rest[:j]
						}
					} else {
						end := strings.IndexAny(rest, "&;,\n\"")
						if end > 0 {
							return rest[:end]
						}
					}
				}
			}
		}
		return ""
	}
	t := &loginTokens{
		PageToken:    get("pageToken"),
		PageTokenKey: get("pageTokenKey"),
		State:        get("state"),
	}
	if t.PageToken == "" || t.State == "" {
		return nil, fmt.Errorf("未能解析 pageToken/state，响应片段: %s", clipBody(data))
	}
	return t, nil
}

// unionLoginByPwd 提交账号密码。密码是明文，这是接口本身的设计。
func (a *authSession) unionLoginByPwd(t *loginTokens, account, password string) ([]byte, error) {
	form := url.Values{}
	form.Set("pageToken", t.PageToken)
	form.Set("pageTokenKey", t.PageTokenKey)
	form.Set("reqClientType", "88")
	form.Set("loginChannel", "88000000")
	form.Set("clientID", "103493351")
	form.Set("lang", "zh-cn")
	form.Set("languageCode", "zh-cn")
	form.Set("state", t.State)
	form.Set("localStorageID", t.LocalStorageID)
	form.Set("userAccount", account)
	form.Set("password", password)
	form.Set("service", authBase+"/authui/casLogin?service="+url.QueryEscape(loginService))
	form.Set("bsAcctService", authBase+"/authui/casLoginAPP?service="+url.QueryEscape(loginService))

	u := idpBase + unifiedPth + "/login/unionLoginByPwd?reflushCode=" +
		fmt.Sprintf("%.16f", float64(time.Now().UnixNano())/1e9) +
		"&cVersion=UP_CAS_6.26.3.100_green"
	req, _ := http.NewRequest(http.MethodPost, u, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", authBase+"/authui/login.html")

	data, status, err := a.do(req)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("unionLoginByPwd 失败 HTTP %d: %s", status, clipBody(data))
	}
	return data, nil
}

// submitSMS 提交短信验证码。
func (a *authSession) submitSMS(t *loginTokens, code string) error {
	form := url.Values{}
	form.Set("pageToken", t.PageToken)
	form.Set("pageTokenKey", t.PageTokenKey)
	form.Set("reqClientType", "88")
	form.Set("loginChannel", "88000000")
	form.Set("clientID", "103493351")
	form.Set("lang", "zh-cn")
	form.Set("languageCode", "zh-cn")
	form.Set("state", t.State)
	form.Set("localStorageID", t.LocalStorageID)
	form.Set("verifyCode", code)

	u := idpBase + unifiedPth + "/login/verifySMSCode?reflushCode=" +
		fmt.Sprintf("%.16f", float64(time.Now().UnixNano())/1e9) +
		"&cVersion=UP_CAS_6.26.3.100_green"
	req, _ := http.NewRequest(http.MethodPost, u, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", authBase+"/authui/login.html")

	data, status, err := a.do(req)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("短信验证失败 HTTP %d: %s", status, clipBody(data))
	}
	if msg := detectLoginError(data); msg != "" {
		return fmt.Errorf("%s", msg)
	}
	return nil
}

// needsSMS 判断响应是否要求二次验证。
func needsSMS(data []byte) bool {
	s := string(data)
	return strings.Contains(s, "verifyCode") ||
		strings.Contains(s, "needVerify") ||
		strings.Contains(s, "二次验证") ||
		strings.Contains(s, "SMS")
}

// detectLoginError 从响应里提取错误信息；没有错误返回空串。
func detectLoginError(data []byte) string {
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return ""
	}
	for _, k := range []string{"errorMsg", "error_msg", "message", "msg"} {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// saveCookies 把登录得到的 cookie 写成 hwcloud 认识的格式。
func (a *authSession) saveCookies() error {
	if err := os.MkdirAll(config.StateDir(), 0o755); err != nil {
		return err
	}
	var out []map[string]any
	for name, value := range a.jar {
		domain := ".huaweicloud.com"
		if strings.HasPrefix(name, "JSESSIONID") {
			domain = "id1.cloud.huawei.com"
		}
		out = append(out, map[string]any{
			"name":   name,
			"value":  value,
			"domain": domain,
			"path":   "/",
		})
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	if err := fsutil.WriteFileAtomic(config.CookieFile(), data, 0o600); err != nil {
		return err
	}
	meta := map[string]any{
		"user_agent": a.ua,
		"login_at":   time.Now().Format("2006-01-02 15:04:05"),
		"login_via":  "password-protocol",
	}
	metaData, _ := json.MarshalIndent(meta, "", "  ")
	return fsutil.WriteFileAtomic(config.MetaFile(), metaData, 0o600)
}

// VerifyProtocolLogin 用已保存的 cookie 验证登录态是否可用。
func VerifyProtocolLogin() (*huawei.Session, error) {
	sess, err := huawei.New("")
	if err != nil {
		return nil, err
	}
	if _, err := sess.CheckLogin(); err != nil {
		return nil, err
	}
	return sess, nil
}
