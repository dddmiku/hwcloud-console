package task

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"hwcloud/internal/config"
	"hwcloud/internal/fsutil"
)

// logCsc 打印 csc 调试日志，方便排查验证码问题。
func logCsc(format string, args ...any) {
	log.Printf("[csc] "+format, args...)
}

// 华为云登录验证码（csc 行为验证 / 滑块拼图）。
//
// 为什么不是图片验证码：
//   华为有两条验证码通道，由 localInfo.displayCaptchaType 决定：
//     0 → 图片验证码（authCodeImage / authCodeValidate）
//     1 → csc 行为验证（滑块拼图，contentcenter 的 captcha.js）
//   被风控标记的账号会走 type=5 的 csc，图片验证码根本不被接受
//   （实测：正确输入图片码仍返回 10000706 need picture authcode risk）。
//
// 完整链路（从 sdk_union.js 逆向）：
//   1. POST cscPreprocess {captchaType:1, clientType:"cas", lang, sceneId}
//        → {appId, businessId, sceneId, type, challenge, hcg, hct, serviceDomain, ...}
//   2. 前端加载 csCaptchaUrl 指向的 captcha.js，用上面的参数初始化滑块
//   3. 用户拖完，SDK 回调 {resultCode:0, validate:"..."}
//   4. authcode = JSON.stringify({type, sceneId, lang, challenge, hcg, hct, validate})
//   5. 把 authcode + session_code_key 塞进 unionLoginByPwd
//
// 第 2-3 步必须在真实浏览器里做（滑块轨迹要真实），所以放到我们自己的前端，
// 用户手动过一次，后端只负责协议部分。

// cscStore 保存进行中的 csc 验证会话。
//
// 关键：cscPreprocess 拿到的 challenge 与登录会话绑定，
// 所以这里必须把 pureSession 一起存下来，后续登录复用同一个会话。
type cscStore struct {
	mu   sync.Mutex
	byID map[string]*cscSession
}

type cscSession struct {
	sess      *pureSession
	account   string
	password  string
	pre       *CscPreprocess
	validate  string
	created   time.Time
}

var cscs = &cscStore{byID: map[string]*cscSession{}}

// cscTTL 是 csc 会话的存活时间。challenge 由 hct 时间戳决定，
// 实测约 2 分钟内有效。
const cscTTL = 110 * time.Second

func (cs *cscStore) get(id string) *cscSession {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	s, ok := cs.byID[id]
	if !ok {
		return nil
	}
	if time.Since(s.created) > cscTTL {
		delete(cs.byID, id)
		return nil
	}
	return s
}

func (cs *cscStore) put(id string, s *cscSession) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for k, v := range cs.byID {
		if time.Since(v.created) > cscTTL {
			delete(cs.byID, k)
		}
	}
	cs.byID[id] = s
}

func (cs *cscStore) drop(id string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	delete(cs.byID, id)
}

// CscPreprocess 是 cscPreprocess 接口的响应。
type CscPreprocess struct {
	AppID         string   `json:"appId"`
	BusinessID    string   `json:"businessId"`
	SceneID       string   `json:"sceneId"`
	Type          int      `json:"type"`
	Challenge     string   `json:"challenge"`
	Hcg           string   `json:"hcg"`
	Hct           int64    `json:"hct"`
	ServiceDomain []string `json:"serviceDomain"`
	CdnDomain     string   `json:"cdnDomain"`
	IsSuccess     int      `json:"isSuccess"`
	ErrorCode     string   `json:"errorCode"`
	ErrorDesc     string   `json:"errorDesc"`
}

// CscChallenge 是给前端的响应：包含初始化滑块所需的一切。
type CscChallenge struct {
	Stage       string   `json:"stage"`
	AppID       string   `json:"app_id,omitempty"`
	BusinessID  string   `json:"business_id,omitempty"`
	SceneID     string   `json:"scene_id,omitempty"`
	Type        int      `json:"type,omitempty"`
	Challenge   string   `json:"challenge,omitempty"`
	Hcg         string   `json:"hcg,omitempty"`
	Hct         int64    `json:"hct,omitempty"`
	ServiceHost []string `json:"service_domain,omitempty"`
	SDKURL      string   `json:"sdk_url,omitempty"`
	Lang        string   `json:"lang,omitempty"`
	Message     string   `json:"message,omitempty"`
	ErrorCode   string   `json:"error_code,omitempty"`
}

// 华为验证码 SDK 地址（从 localInfo.csCaptchaUrl 取，带多个 CDN 备选）。
var cscSDKURLs = []string{
	"https://contentcenter-drcn.dbankcdn.com/pub_1/cscaptcha_captcha_900_9/b/v3/netease/captcha.js",
	"https://contentcenter-tx-drcn.dbankcdn.com/pub_1/cscaptcha_captcha_900_9/b/v3/netease/captcha.js",
	"https://contentcenter-hw-drcn.dbankcdn.com/pub_1/cscaptcha_captcha_900_9/b/v3/netease/captcha.js",
}

// FetchCsc 建立登录会话并调用 cscPreprocess 拿滑块参数。
//
// 顺序很重要：必须先提交一次账密触发 10000706 风控，再调 cscPreprocess。
// 浏览器抓包证实：跳过这一步直接调 cscPreprocess，服务端会按「高风险」返回 type=5，
// 那种滑块无论怎么拖都过不了；先触发风控才会返回正常的 type=2。
func FetchCsc(accountID, account, password string) (*CscChallenge, error) {
	res := &CscChallenge{Stage: "cscPreprocess", Lang: "zh-cn"}

	sess, err := newPureSession()
	if err != nil {
		return nil, err
	}
	if err := sess.bootstrap(); err != nil {
		res.Message = "建立会话失败: " + err.Error()
		return res, nil
	}

	// 1. 先提交一次账密，触发风控（预期返回 10000706）
	pwForm := url.Values{}
	pwForm.Set("clientID", "103493351")
	pwForm.Set("hwid_cas_sid", sess.sid)
	pwForm.Set("lang", "zh-cn")
	pwForm.Set("languageCode", "zh-cn")
	pwForm.Set("localStorageID", sess.cfg.LocalStorageID)
	pwForm.Set("loginChannel", "88000000")
	pwForm.Set("pageToken", sess.info.PageToken)
	pwForm.Set("pageTokenKey", sess.info.PageTokenKey)
	pwForm.Set("password", password)
	pwForm.Set("reqClientType", "88")
	pwForm.Set("state", sess.info.State)
	pwForm.Set("userAccount", account)

	pwBody, err := sess.post("login/getLoginIdsByPwd", pwForm)
	if err != nil {
		res.Message = "密码校验失败: " + err.Error()
		return res, nil
	}
	var pwResp struct {
		AccountInfoList []struct {
			AnonymousAccount string `json:"anonymousAccount"`
		} `json:"accountInfoList"`
		ErrorCode string `json:"errorCode"`
		ErrorDesc string `json:"errorDesc"`
	}
	json.Unmarshal([]byte(pwBody), &pwResp)
	if pwResp.ErrorCode != "" {
		res.ErrorCode = pwResp.ErrorCode
		res.Message = "密码校验未通过: " + pwResp.ErrorDesc
		return res, nil
	}
	anonAcc := ""
	if len(pwResp.AccountInfoList) > 0 {
		anonAcc = pwResp.AccountInfoList[0].AnonymousAccount
	}

	// 1.5 提交 unionLoginByPwd 触发风控（不带 authcode，预期 10000706）
	hwmeta, _ := LoadHwmeta()
	uForm := sess.baseForm()
	uForm.Set("userAccount", account)
	uForm.Set("password", password)
	uForm.Set("opType", "0")
	uForm.Set("serial", "0")
	uForm.Set("registerCountry", "cn")
	uForm.Set("anonymousLoginID", anonAcc)
	uForm.Set("hwid_cas_sid", sess.sid)
	if hwmeta != "" {
		uForm.Set("hwmeta", hwmeta)
	}
	uForm.Set("service", authBase+"/authui/casLogin?service="+url.QueryEscape(loginService))
	uForm.Set("bsAcctService", authBase+"/authui/casLoginAPP?service="+url.QueryEscape(loginService))
	uForm.Set("scope", "https://www.huawei.com/auth/account/unified.profile+https://www.huawei.com/auth/account/risk.idstate+LoginState")
	uForm.Set("access_type", "offline")

	uBody, _ := sess.post("login/unionLoginByPwd", uForm)
	logCsc("触发风控 unionLoginByPwd: %s", clipStr(uBody, 200))

	// 2. 现在调 cscPreprocess，这次服务端会按真实风险等级返回 type
	form := sess.baseForm()
	form.Set("captchaType", "1")
	form.Set("clientType", "cas")
	form.Set("lang", "zh-cn")
	form.Set("sceneId", "login")
	form.Set("hwid_cas_sid", sess.sid)

	body, err := sess.post("cscPreprocess", form)
	if err != nil {
		res.Message = err.Error()
		return res, nil
	}

	var pre CscPreprocess
	if err := json.Unmarshal([]byte(body), &pre); err != nil {
		res.Message = "解析 cscPreprocess 失败: " + clipStr(body, 200)
		return res, nil
	}
	if pre.IsSuccess != 1 || pre.Challenge == "" {
		res.ErrorCode = pre.ErrorCode
		res.Message = "获取滑块参数失败: " + clipStr(body, 200)
		return res, nil
	}

	cscs.put(accountID, &cscSession{
		sess:     sess,
		account:  account,
		password: password,
		pre:      &pre,
		created:  time.Now(),
	})

	res.Stage = "csc"
	res.AppID = pre.AppID
	res.BusinessID = pre.BusinessID
	res.SceneID = pre.SceneID
	res.Type = pre.Type
	res.Challenge = pre.Challenge
	res.Hcg = pre.Hcg
	res.Hct = pre.Hct
	res.ServiceHost = pre.ServiceDomain
	res.SDKURL = cscSDKURLs[0]
	res.Message = "请拖动滑块完成验证"
	return res, nil
}

// CscSDK 代理验证码 SDK 脚本，避免前端跨域加载失败。
func CscSDK() ([]byte, string, error) {
	var lastErr error
	for _, u := range cscSDKURLs {
		req, err := http.NewRequest(http.MethodGet, u, nil)
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("User-Agent", config.UserAgent())
		req.Header.Set("Accept", "*/*")
		req.Header.Set("Referer", "https://auth.huaweicloud.com/")

		client := &http.Client{Timeout: 20 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || len(data) == 0 {
			lastErr = fmt.Errorf("空响应")
			continue
		}
		return data, "application/javascript; charset=utf-8", nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("所有 CDN 都不可用")
	}
	return nil, "", lastErr
}

// CscSubmit 用滑块验证结果完成登录。
//
// validate 参数可以是：
//   - 纯 validate 字符串
//   - SDK onVerify 回调的完整 JSON（含 challenge/hcg/hct/validate），
//     这时以回调里的值为准，因为服务端可能要求原样回传。
func CscSubmit(accountID, validate string) (*PureLoginResult, error) {
	res := &PureLoginResult{}

	cs := cscs.get(accountID)
	if cs == nil {
		res.Stage = "csc"
		res.Message = "滑块验证已过期，请刷新"
		res.NeedManual = true
		return res, nil
	}
	if validate == "" {
		res.Stage = "csc"
		res.Message = "缺少 validate"
		res.NeedManual = true
		return res, nil
	}
	cs.validate = validate

	// 如果传进来的是完整回调 JSON，解析出各字段
	cbType := cs.pre.Type
	cbScene := cs.pre.SceneID
	cbChallenge := cs.pre.Challenge
	cbHcg := cs.pre.Hcg
	cbHct := cs.pre.Hct
	validateVal := validate

	if strings.HasPrefix(strings.TrimSpace(validate), "{") {
		var m map[string]any
		if json.Unmarshal([]byte(validate), &m) == nil {
			if v, ok := m["type"].(float64); ok {
				cbType = int(v)
			}
			if v, ok := m["sceneId"].(string); ok && v != "" {
				cbScene = v
			}
			if v, ok := m["challenge"].(string); ok && v != "" {
				cbChallenge = v
			}
			if v, ok := m["hcg"].(string); ok && v != "" {
				cbHcg = v
			}
			if v, ok := m["hct"].(float64); ok && v != 0 {
				cbHct = int64(v)
			}
			if v, ok := m["validate"].(string); ok && v != "" {
				validateVal = v
			}
		}
	}

	sess := cs.sess
	res.Sid = sess.sid

	// 密码校验
	pwForm := url.Values{}
	pwForm.Set("clientID", "103493351")
	pwForm.Set("hwid_cas_sid", sess.sid)
	pwForm.Set("lang", "zh-cn")
	pwForm.Set("languageCode", "zh-cn")
	pwForm.Set("localStorageID", sess.cfg.LocalStorageID)
	pwForm.Set("loginChannel", "88000000")
	pwForm.Set("pageToken", sess.info.PageToken)
	pwForm.Set("pageTokenKey", sess.info.PageTokenKey)
	pwForm.Set("password", cs.password)
	pwForm.Set("reqClientType", "88")
	pwForm.Set("state", sess.info.State)
	pwForm.Set("userAccount", cs.account)

	pwBody, err := sess.post("login/getLoginIdsByPwd", pwForm)
	if err != nil {
		res.Stage = "getLoginIdsByPwd"
		res.Message = err.Error()
		return res, nil
	}
	var pwResp struct {
		AccountInfoList []struct {
			AnonymousAccount string `json:"anonymousAccount"`
		} `json:"accountInfoList"`
		ErrorCode string `json:"errorCode"`
		ErrorDesc string `json:"errorDesc"`
	}
	json.Unmarshal([]byte(pwBody), &pwResp)
	if pwResp.ErrorCode != "" {
		res.Stage = "getLoginIdsByPwd"
		res.ErrorCode = pwResp.ErrorCode
		res.ErrorDesc = pwResp.ErrorDesc
		res.Message = "密码校验未通过"
		return res, nil
	}
	anonAcc := ""
	if len(pwResp.AccountInfoList) > 0 {
		anonAcc = pwResp.AccountInfoList[0].AnonymousAccount
	}
	res.Username = anonAcc

	hwmeta, _ := LoadHwmeta()

	// 拼 authcode —— SDK 的 getRandomCode 就是这个结构
	// type 用字符串：SDK 里 parseInt 后仍以原始形态进 JSON
	authcodeObj := map[string]any{
		"type":      cbType,
		"sceneId":   cbScene,
		"lang":      "zh-CN",
		"challenge": cbChallenge,
		"hcg":       cbHcg,
		"hct":       cbHct,
		"validate":  validateVal,
	}
	authcodeBytes, _ := json.Marshal(authcodeObj)
	authcode := string(authcodeBytes)
	logCsc("authcode: %s", authcode)

	// 同时准备一份「字符串 type」的变体，失败时自动重试
	altObj := map[string]any{
		"type":      fmt.Sprintf("%d", cbType),
		"sceneId":   cbScene,
		"lang":      "zh-CN",
		"challenge": cbChallenge,
		"hcg":       cbHcg,
		"hct":       cbHct,
		"validate":  validateVal,
	}
	altBytes, _ := json.Marshal(altObj)
	altAuthcode := string(altBytes)

	uForm := sess.baseForm()
	uForm.Set("userAccount", cs.account)
	uForm.Set("password", cs.password)
	uForm.Set("opType", "0")
	uForm.Set("serial", "0")
	uForm.Set("registerCountry", "cn")
	uForm.Set("anonymousLoginID", anonAcc)
	uForm.Set("hwid_cas_sid", sess.sid)
	if hwmeta != "" {
		uForm.Set("hwmeta", hwmeta)
	}
	uForm.Set("session_code_key", "login_session_ramdom_code_key")
	uForm.Set("service", authBase+"/authui/casLogin?service="+url.QueryEscape(loginService))
	uForm.Set("bsAcctService", authBase+"/authui/casLoginAPP?service="+url.QueryEscape(loginService))
	uForm.Set("scope", "https://www.huawei.com/auth/account/unified.profile+https://www.huawei.com/auth/account/risk.idstate+LoginState")
	uForm.Set("access_type", "offline")

	// 依次尝试 authcode 的两种形态，任一成功即通过。
	// 华为对 type 的类型（数字/字符串）在不同版本表现不一致，实测对比最省事。
	var uResp struct {
		ErrorCode   string `json:"errorCode"`
		ErrorDesc   string `json:"errorDesc"`
		CallbackURL string `json:"callbackURL"`
	}
	for i, ac := range []string{authcode, altAuthcode} {
		uForm.Set("authcode", ac)
		body, err := sess.post("login/unionLoginByPwd", uForm)
		if err != nil {
			res.Stage = "unionLoginByPwd"
			res.Message = err.Error()
			return res, nil
		}
		logCsc("尝试 %d authcode: %s", i+1, clipStr(ac, 300))
		logCsc("尝试 %d 响应: %s", i+1, clipStr(body, 400))

		var r struct {
			ErrorCode   string `json:"errorCode"`
			ErrorDesc   string `json:"errorDesc"`
			CallbackURL string `json:"callbackURL"`
		}
		json.Unmarshal([]byte(body), &r)

		// 10000706 = 仍要求验证码，换下一个变体再试
		if r.ErrorCode == "10000706" {
			uResp.ErrorCode = r.ErrorCode
			uResp.ErrorDesc = r.ErrorDesc
			continue
		}
		// 其它结果（成功 / 需要短信 / 明确错误）直接采用
		uResp.ErrorCode = r.ErrorCode
		uResp.ErrorDesc = r.ErrorDesc
		uResp.CallbackURL = r.CallbackURL
		break
	}

	if uResp.ErrorCode == "10002080" {
		res.Stage = "need_sms"
		res.NeedSMS = true
		res.ErrorCode = uResp.ErrorCode
		res.Message = "滑块已通过，还需要短信验证码"
		return res, nil
	}
	if uResp.ErrorCode != "" {
		res.Stage = "unionLoginByPwd"
		res.ErrorCode = uResp.ErrorCode
		res.ErrorDesc = uResp.ErrorDesc
		res.Message = "登录失败"
		return res, nil
	}

	// 跟随 callbackURL 完成 OAuth
	if uResp.CallbackURL != "" {
		logCsc("callbackURL: %s", clipStr(uResp.CallbackURL, 400))
		followOAuth(sess, uResp.CallbackURL)
	} else {
		logCsc("callbackURL 为空（登录响应里没有）")
	}

	// 关键：走完 devdata 的 SSO 循环，把 devdata 域的会话 cookie 种下来。
	// 浏览器抓包证实：casLogin 之后还要经过
	//   devdata/index/islogin → authui/login → devdata/index/islogin?ticket=ST-...
	// 才会拿到 devdata 的会话；少了这一步，签到等接口全是 403 token not found。
	completeSSO(sess)

	// 信任设备
	rdForm := url.Values{}
	rdForm.Set("devID", sess.sid)
	rdForm.Set("hwMeta", hwmeta)
	_, _ = sess.postRaw(authBase+"/authui/riskData", rdForm)

	tbForm := sess.baseForm()
	tbForm.Set("userAccount", cs.account)
	tbForm.Set("operType", "1")
	tbForm.Set("trustBrowser", "1")
	tbForm.Set("anonymousLoginID", anonAcc)
	tbForm.Set("hwid_cas_sid", sess.sid)
	tbBody, err := sess.post("login/updateTrustBrowser", tbForm)
	if err != nil {
		res.Stage = "updateTrustBrowser"
		res.Message = err.Error()
		return res, nil
	}
	logCsc("updateTrustBrowser 响应: %s", clipStr(tbBody, 300))
	var tbResp struct {
		IsSuccess int    `json:"isSuccess"`
		ErrorCode string `json:"errorCode"`
	}
	json.Unmarshal([]byte(tbBody), &tbResp)
	if tbResp.ErrorCode != "" {
		res.Stage = "updateTrustBrowser"
		res.ErrorCode = tbResp.ErrorCode
		res.Message = "信任设备失败"
		return res, nil
	}
	logCsc("设备信任成功, sid=%s", clipStr(sess.cookieSid(), 40))

	_ = SaveHwidSid(sess.cookieSid())
	savePureCookies(sess.jar, config.CookieFile())

	meta := map[string]any{}
	if data, err := os.ReadFile(config.MetaFile()); err == nil {
		json.Unmarshal(data, &meta)
	}
	meta["user_agent"] = sess.ua
	meta["login_at"] = time.Now().Format("2006-01-02 15:04:05")
	meta["login_via"] = "csc-slider"
	if data, err := json.MarshalIndent(meta, "", "  "); err == nil {
		_ = fsutil.WriteFileAtomic(config.MetaFile(), data, 0o600)
	}

	cscs.drop(accountID)

	res.Stage = "done"
	res.Message = "登录成功，设备已信任"
	res.SavedTo = config.CookieFile()
	return res, nil
}

// CscSendSMS 在滑块通过后发送短信验证码（用于需要二次验证的账号）。
func CscSendSMS(accountID string) (*PureLoginResult, error) {
	res := &PureLoginResult{Stage: "getSMSCodeV3"}

	cs := cscs.get(accountID)
	if cs == nil {
		res.Message = "滑块验证已过期，请刷新"
		res.NeedManual = true
		return res, nil
	}

	form := cs.sess.baseForm()
	form.Set("userAccount", cs.account)
	form.Set("accountType", "2")
	form.Set("mobilePhone", maskPhone(cs.account))
	form.Set("operType", "8")
	form.Set("smsReqType", "6")
	form.Set("hwid_cas_sid", cs.sess.sid)

	body, err := cs.sess.post("login/getSMSCodeV3", form)
	if err != nil {
		res.Message = err.Error()
		return res, nil
	}

	var r struct {
		IsSuccess        int    `json:"isSuccess"`
		PreviousSentCode string `json:"previousSentCode"`
		ErrorCode        string `json:"errorCode"`
		ErrorDesc        string `json:"errorDesc"`
	}
	json.Unmarshal([]byte(body), &r)

	res.ErrorCode = r.ErrorCode
	res.ErrorDesc = r.ErrorDesc
	if r.IsSuccess == 1 {
		res.NeedSMS = true
		res.Message = "短信已发送"
	} else {
		res.Message = "短信发送失败: " + clipStr(body, 150)
	}
	return res, nil
}

// CscSubmitSMS 用短信码完成二次验证。
func CscSubmitSMS(accountID, smsCode string) (*PureLoginResult, error) {
	cs := cscs.get(accountID)
	if cs == nil {
		return &PureLoginResult{Stage: "csc", Message: "滑块验证已过期，请刷新", NeedManual: true}, nil
	}

	sess := cs.sess
	res := &PureLoginResult{Sid: sess.sid}

	// 重新做一次密码校验拿 anonAcc
	pwForm := url.Values{}
	pwForm.Set("clientID", "103493351")
	pwForm.Set("hwid_cas_sid", sess.sid)
	pwForm.Set("lang", "zh-cn")
	pwForm.Set("languageCode", "zh-cn")
	pwForm.Set("localStorageID", sess.cfg.LocalStorageID)
	pwForm.Set("loginChannel", "88000000")
	pwForm.Set("pageToken", sess.info.PageToken)
	pwForm.Set("pageTokenKey", sess.info.PageTokenKey)
	pwForm.Set("password", cs.password)
	pwForm.Set("reqClientType", "88")
	pwForm.Set("state", sess.info.State)
	pwForm.Set("userAccount", cs.account)
	pwBody, _ := sess.post("login/getLoginIdsByPwd", pwForm)
	var pwResp struct {
		AccountInfoList []struct {
			AnonymousAccount string `json:"anonymousAccount"`
		} `json:"accountInfoList"`
	}
	json.Unmarshal([]byte(pwBody), &pwResp)
	anonAcc := ""
	if len(pwResp.AccountInfoList) > 0 {
		anonAcc = pwResp.AccountInfoList[0].AnonymousAccount
	}

	hwmeta, _ := LoadHwmeta()
	authcodeObj := map[string]any{
		"type":      cs.pre.Type,
		"sceneId":   cs.pre.SceneID,
		"lang":      "zh-cn",
		"challenge": cs.pre.Challenge,
		"hcg":       cs.pre.Hcg,
		"hct":       cs.pre.Hct,
		"validate":  cs.validate,
	}
	authcodeBytes, _ := json.Marshal(authcodeObj)

	vForm := sess.baseForm()
	vForm.Set("userAccount", cs.account)
	vForm.Set("password", cs.password)
	vForm.Set("opType", "1")
	vForm.Set("serial", "0")
	vForm.Set("registerCountry", "cn")
	vForm.Set("anonymousLoginID", anonAcc)
	vForm.Set("hwid_cas_sid", sess.sid)
	if hwmeta != "" {
		vForm.Set("hwmeta", hwmeta)
	}
	vForm.Set("verifyUserAccount", maskPhone(cs.account))
	vForm.Set("verifyAccountType", "2")
	vForm.Set("twoStepVerifyCode", smsCode)
	vForm.Set("authcode", string(authcodeBytes))
	vForm.Set("session_code_key", "login_session_ramdom_code_key")
	vForm.Set("service", authBase+"/authui/casLogin?service="+url.QueryEscape(loginService))
	vForm.Set("bsAcctService", authBase+"/authui/casLoginAPP?service="+url.QueryEscape(loginService))
	vForm.Set("scope", "https://www.huawei.com/auth/account/unified.profile+https://www.huawei.com/auth/account/risk.idstate+LoginState")
	vForm.Set("access_type", "offline")

	vBody, err := sess.post("login/unionLoginByPwd", vForm)
	if err != nil {
		res.Stage = "unionLoginByPwd"
		res.Message = err.Error()
		return res, nil
	}
	var vResp struct {
		ErrorCode   string `json:"errorCode"`
		ErrorDesc   string `json:"errorDesc"`
		CallbackURL string `json:"callbackURL"`
	}
	json.Unmarshal([]byte(vBody), &vResp)
	if vResp.ErrorCode != "" {
		res.Stage = "unionLoginByPwd"
		res.ErrorCode = vResp.ErrorCode
		res.ErrorDesc = vResp.ErrorDesc
		res.Message = "短信验证失败"
		return res, nil
	}

	if vResp.CallbackURL != "" {
		logCsc("SMS callbackURL: %s", clipStr(vResp.CallbackURL, 400))
		// 手动逐步跟随，确保走完 OAuth 全链：
		//   oauth-login1/authorize → loginCallback → CAS/remoteLogin
		//   → authui/casLogin(Set-Cookie x12) → authui/login → grow?ticket=ST-...
		followOAuth(sess, vResp.CallbackURL)
	} else {
		logCsc("SMS callbackURL 为空")
	}

	// 走完 devdata 的 SSO 循环，种下 devdata 域会话 cookie
	completeSSO(sess)

	// 信任设备
	rdForm := url.Values{}
	rdForm.Set("devID", sess.sid)
	rdForm.Set("hwMeta", hwmeta)
	_, _ = sess.postRaw(authBase+"/authui/riskData", rdForm)

	tbForm := sess.baseForm()
	tbForm.Set("userAccount", cs.account)
	tbForm.Set("operType", "1")
	tbForm.Set("trustBrowser", "1")
	tbForm.Set("anonymousLoginID", anonAcc)
	tbForm.Set("hwid_cas_sid", sess.sid)
	tbBody2, err := sess.post("login/updateTrustBrowser", tbForm)
	if err != nil {
		logCsc("updateTrustBrowser 请求失败: %v", err)
	} else {
		logCsc("updateTrustBrowser 响应: %s", clipStr(tbBody2, 300))
		var tb struct {
			IsSuccess int    `json:"isSuccess"`
			ErrorCode string `json:"errorCode"`
		}
		json.Unmarshal([]byte(tbBody2), &tb)
		if tb.ErrorCode != "" {
			logCsc("信任设备失败: %s", tb.ErrorCode)
		} else {
			logCsc("设备信任成功, sid=%s", clipStr(sess.cookieSid(), 40))
		}
	}

	_ = SaveHwidSid(sess.cookieSid())
	savePureCookies(sess.jar, config.CookieFile())

	meta := map[string]any{}
	if data, err := os.ReadFile(config.MetaFile()); err == nil {
		json.Unmarshal(data, &meta)
	}
	meta["user_agent"] = sess.ua
	meta["login_at"] = time.Now().Format("2006-01-02 15:04:05")
	meta["login_via"] = "csc-slider+sms"
	if data, err := json.MarshalIndent(meta, "", "  "); err == nil {
		_ = fsutil.WriteFileAtomic(config.MetaFile(), data, 0o600)
	}

	cscs.drop(accountID)

	res.Stage = "done"
	res.Username = anonAcc
	res.Message = "登录成功，设备已信任"
	res.SavedTo = config.CookieFile()
	return res, nil
}

// completeSSO 走完 devdata 的 SSO 循环，把 devdata 域的会话 cookie 种下来。
//
// 浏览器抓包证实的完整链路：
//
//	devdata.huaweicloud.com/index/islogin
//	  -> 302 auth.huaweicloud.com/authui/login?service=https://devdata.../index/islogin
//	  -> 302 devdata.huaweicloud.com/index/islogin?ticket=ST-xxxx-sso
//	  -> 200 + Set-Cookie（devdata 的会话 token）
//
// 少走这一步，devdata 下只有 WAF cookie，签到 / 任务接口全部 403 token not found。
func completeSSO(sess *pureSession) {
	const devdataIsLogin = "https://devdata.huaweicloud.com/index/islogin"

	// devdata 对 Referer 敏感：没有浏览器同款 Referer 会直接 403
	doGet := func(target, referer string) (string, int, error) {
		req, err := http.NewRequest(http.MethodGet, target, nil)
		if err != nil {
			return "", 0, err
		}
		req.Header.Set("User-Agent", sess.ua)
		req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
		req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
		req.Header.Set("Upgrade-Insecure-Requests", "1")
		if referer != "" {
			req.Header.Set("Referer", referer)
		}
		resp, err := sess.client.Do(req)
		if err != nil {
			return "", 0, err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b), resp.StatusCode, nil
	}

	// 从开发者中心页进入，带上它的 Referer
	referer := "https://developer.huaweicloud.com/grow"
	for i := 0; i < 4; i++ {
		body, code, err := doGet(devdataIsLogin, referer)
		if err != nil {
			logCsc("SSO 第 %d 次 islogin 失败: %v", i+1, err)
			return
		}
		logCsc("SSO 第 %d 次 islogin: status=%d len=%d", i+1, code, len(body))
		if code == http.StatusOK {
			break
		}
	}

	// 验证：访问 get-ainfo 看能否拿到 csrf
	if csrf := fetchDevdataCSRF(sess); csrf != "" {
		logCsc("SSO 完成，已拿到 devdata csrf")
	} else {
		logCsc("SSO 完成但未拿到 csrf（devdata 会话可能仍未建立）")
	}
}

// fetchDevdataCSRF 请求 devdata 的 get-ainfo，返回 csrf 头（空表示会话无效）。
func fetchDevdataCSRF(sess *pureSession) string {
	req, err := http.NewRequest(http.MethodGet, "https://devdata.huaweicloud.com/api/get-ainfo", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", sess.ua)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Referer", "https://developer.huaweicloud.com/")

	resp, err := sess.client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	return resp.Header.Get("csrf")
}

// followOAuth 手动逐步跟随 OAuth 重定向链。
//
// 为什么不用 http.Client 的自动重定向：
//   - 中间会跨多个域（oauth-login1 / id1 / auth / developer / devdata）
//   - 每一步都可能 Set-Cookie，需要确保 cookie 落到正确的域和 path
//   - 自动跟随遇到 3xx 链过长会中断，而这里恰好需要走完 6-8 跳
func followOAuth(sess *pureSession, startURL string) {
	client := &http.Client{
		Jar:     sess.jar,
		Timeout: 30 * time.Second,
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// 1. 跟随 authorize → CAS/remoteLogin → loginCallback
	cur := startURL
	visited := map[string]bool{}
	var callbackURL string
	for hop := 0; hop < 8; hop++ {
		if cur == "" || visited[cur] {
			break
		}
		visited[cur] = true

		req, err := http.NewRequest(http.MethodGet, cur, nil)
		if err != nil {
			break
		}
		req.Header.Set("User-Agent", sess.ua)
		req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
		req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
		req.Header.Set("Referer", authBase+"/authui/login.html")

		resp, err := client.Do(req)
		if err != nil {
			logCsc("OAuth hop %d 失败: %v", hop+1, err)
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		loc := resp.Header.Get("Location")
		host := ""
		if u, e := url.Parse(cur); e == nil {
			host = u.Host
		}
		logCsc("OAuth hop %d: %s → %d %s", hop+1, host, resp.StatusCode, clipStr(loc, 100))

		if strings.Contains(cur, "loginCallback") {
			callbackURL = cur
			break
		}

		// authorize 返回 200 的 SPA 页面，跳转由页面 JS 发起：
		// 它会去请求 id1 的 CAS/remoteLogin，再 302 回 loginCallback。
		// 这里手动补上这一步，否则链路卡在 200 就断了。
		if resp.StatusCode == 200 && strings.Contains(host, "oauth-login1") && strings.Contains(cur, "/authorize") {
			// authorize 返回 SPA 页面，跳转由页面 JS 发起。
			// 真实流程是：页面把 authorize 的全部参数搬到 loginCallback，
			// 然后请求 CAS/remoteLogin?service=<loginCallback URL>。
			// 这里照做，否则 remoteLogin 会把我们甩回登录页。
			uu, e := url.Parse(cur)
			if e != nil {
				break
			}
			cbURL := "https://oauth-login1.cloud.huawei.com/oauth2/v3/loginCallback?" + uu.RawQuery
			remoteURL := "https://id1.cloud.huawei.com/CAS/remoteLogin?service=" +
				url.QueryEscape(cbURL)
			logCsc("OAuth: authorize 返回 SPA，主动请求 CAS/remoteLogin")
			cur = remoteURL
			continue
		}

		if resp.StatusCode < 300 || resp.StatusCode >= 400 || loc == "" {
			break
		}
		if strings.HasPrefix(loc, "/") {
			if u, e := url.Parse(cur); e == nil {
				loc = u.Scheme + "://" + u.Host + loc
			}
		}
		cur = loc
	}

	if callbackURL == "" {
		logCsc("OAuth: 没走到 loginCallback，无法继续")
		return
	}

	// 2. POST /oauth2/ajax/login 换 code
	u, err := url.Parse(callbackURL)
	if err != nil {
		return
	}
	q := u.Query()
	form := url.Values{}
	for _, k := range []string{
		"access_type", "client_id", "code_challenge_method", "display", "flowID",
		"h", "include_granted_scopes", "lang", "nonce", "prompt", "redirect_uri",
		"response_type", "risks", "scope", "state", "themeName", "ticket", "v",
		"siteID", "countryCode",
	} {
		if val := q.Get(k); val != "" {
			form.Set(k, val)
		}
	}

	loginURL := "https://oauth-login1.cloud.huawei.com/oauth2/ajax/login?reflushCode=" +
		fmt.Sprintf("%.16f", float64(time.Now().UnixNano())/1e9) + "&display=page"
	req, err := http.NewRequest(http.MethodPost, loginURL, strings.NewReader(form.Encode()))
	if err != nil {
		return
	}
	req.Header.Set("User-Agent", sess.ua)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "zh-CN")
	req.Header.Set("Origin", "https://oauth-login1.cloud.huawei.com")
	req.Header.Set("Referer", callbackURL)
	// 必填：缺了会返回 1101 "ajax request interface version wrong in head"
	req.Header.Set("interfaceversion", "v3")
	req.Header.Set("fromloginauth", "false")

	resp, err := client.Do(req)
	if err != nil {
		logCsc("oauth2/ajax/login 失败: %v", err)
		return
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	logCsc("oauth2/ajax/login: %d len=%d", resp.StatusCode, len(body))

	// 3. 从响应里取 code，或直接取跳转地址
	code := extractOAuthCode(string(body))
	if code == "" {
		if loc := resp.Header.Get("Location"); loc != "" {
			if lu, e := url.Parse(loc); e == nil {
				code = lu.Query().Get("code")
			}
		}
	}
	if code == "" {
		logCsc("oauth2/ajax/login 未返回 code，响应: %s", clipStr(string(body), 300))
		return
	}
	logCsc("拿到 OAuth code: %s", clipStr(code, 40))

	// 4. 用 code 请求 casLogin
	redirectURI := q.Get("redirect_uri")
	if redirectURI == "" {
		return
	}
	casURL := redirectURI + "&code=" + url.QueryEscape(code)
	body2, code2, loc2 := doGetNoRedirect(client, casURL, sess.ua, "https://oauth-login1.cloud.huawei.com/")
	logCsc("casLogin: %d len=%d %s", code2, len(body2), clipStr(loc2, 120))

	// 5. 继续跟随 casLogin 的重定向
	cur = loc2
	if strings.HasPrefix(cur, "/") {
		cur = authBase + cur
	}
	for hop := 0; hop < 6 && cur != ""; hop++ {
		body3, code3, loc3 := doGetNoRedirect(client, cur, sess.ua, authBase+"/authui/login.html")
		host := ""
		if uu, e := url.Parse(cur); e == nil {
			host = uu.Host
		}
		logCsc("cas 后续 hop %d: %s → %d len=%d %s", hop+1, host, code3, len(body3), clipStr(loc3, 100))
		if code3 < 300 || code3 >= 400 || loc3 == "" {
			break
		}
		cur = loc3
		if strings.HasPrefix(cur, "/") {
			if uu, e := url.Parse(cur); e == nil {
				cur = uu.Scheme + "://" + uu.Host + cur
			}
		}
	}
}

// extractOAuthCode 从 loginCallback 的 HTML/JS 里提取 OAuth code。
func extractOAuthCode(body string) string {
	// 常见形态：code=XXXX / "code":"XXXX" / code: 'XXXX'
	pats := []*regexp.Regexp{
		regexp.MustCompile(`[?&]code=([A-Za-z0-9%+/=_-]{30,})`),
		regexp.MustCompile(`"code"\s*:\s*"([A-Za-z0-9%+/=_-]{30,})"`),
		regexp.MustCompile(`code\s*:\s*["']([A-Za-z0-9%+/=_-]{30,})["']`),
	}
	for _, re := range pats {
		if m := re.FindStringSubmatch(body); len(m) > 1 {
			if v, err := url.QueryUnescape(m[1]); err == nil {
				return v
			}
			return m[1]
		}
	}
	return ""
}

// doGetNoRedirect 发一个不跟随重定向的 GET。
func doGetNoRedirect(client *http.Client, target, ua, referer string) (string, int, string) {
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		return "", 0, ""
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode, resp.Header.Get("Location")
}
