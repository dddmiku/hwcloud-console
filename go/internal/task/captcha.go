package task

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"hwcloud/internal/config"
	"hwcloud/internal/fsutil"
)
// 华为云登录图片验证码。
//
// 链路（从 SDK 逆向得到，见 docs/纯协议登录逆向.md）：
//  1. POST authCodeImage     {randomCodeID, session_code_key}      → {randomCodeID, imgCodeUrl}
//  2. 前端把 imgCodeUrl 显示出来，用户看图输入
//  3. unionLoginByPwd 时把 randomCodeObj 展开进表单：
//     authcode={"randomCodeID":"..","randomCodeValue":".."} + session_code_key
//
// 关键点：
//   - randomCodeID 由服务端下发，同一会话内必须保持；换 ID 会让旧图失效。
//   - 校验通过后 ID 仍然有效，可以直接用于随后的 unionLoginByPwd。
//   - 整个流程必须跑在同一个 cookie 会话里，所以会话要跨 HTTP 请求保活。

// captchaStore 保存进行中的验证码会话。
//
// 面板是多用户的，但同一账号的验证码只能有一个在途（华为服务端也是这么认的），
// 所以按账号 ID 索引。会话里带着 pureSession，后续登录直接复用它，
// 避免「换了 JSESSIONID 导致 authcode 对不上」。
type captchaStore struct {
	mu   sync.Mutex
	byID map[string]*captchaSession
}

type captchaSession struct {
	sess         *pureSession
	account      string
	password     string
	randomCodeID string
	imgURL       string
	code         string
	verified     bool
	created      time.Time
}

var captchas = &captchaStore{byID: map[string]*captchaSession{}}

// captchaTTL 是验证码会话的存活时间。华为图片本身约 2 分钟过期，
// 这里留一点余量，超时就要求重新获取。
const captchaTTL = 110 * time.Second

func (cs *captchaStore) get(id string) *captchaSession {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	s, ok := cs.byID[id]
	if !ok {
		return nil
	}
	if time.Since(s.created) > captchaTTL {
		delete(cs.byID, id)
		return nil
	}
	return s
}

func (cs *captchaStore) put(id string, s *captchaSession) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	// 顺手清掉过期项，避免长期运行堆积
	for k, v := range cs.byID {
		if time.Since(v.created) > captchaTTL {
			delete(cs.byID, k)
		}
	}
	cs.byID[id] = s
}

func (cs *captchaStore) drop(id string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	delete(cs.byID, id)
}

// CaptchaChallenge 是获取验证码后的返回体。
type CaptchaChallenge struct {
	Stage       string `json:"stage"`
	ImageURL    string `json:"image_url,omitempty"`
	RandomCodeID string `json:"random_code_id,omitempty"`
	SessionKey  string `json:"session_key,omitempty"`
	Message     string `json:"message,omitempty"`
	ErrorCode   string `json:"error_code,omitempty"`
	ErrorDesc   string `json:"error_desc,omitempty"`
}

// sessionCodeKey 是密码登录用的固定 key（SDK 里 pwdLogin 组件的默认值）。
const sessionCodeKey = "login_session_ramdom_code_key"

// FetchCaptcha 建立一个新的登录会话并取一张验证码图片。
//
// 会话被保存下来，用户填完码后 CaptchaSubmit 会复用它。
func FetchCaptcha(accountID, account, password string) (*CaptchaChallenge, error) {
	res := &CaptchaChallenge{Stage: "authCodeImage"}

	sess, err := newPureSession()
	if err != nil {
		return nil, err
	}
	if err := sess.bootstrap(); err != nil {
		res.Message = "建立会话失败: " + err.Error()
		return res, nil
	}

	form := url.Values{}
	form.Set("randomCodeID", "")
	form.Set("session_code_key", sessionCodeKey)
	form.Set("languageCode", "zh-cn")

	body, err := sess.post("authCodeImage", form)
	if err != nil {
		res.Message = err.Error()
		return res, nil
	}

	var r struct {
		RandomCodeID string `json:"randomCodeID"`
		ImgCodeURL   string `json:"imgCodeUrl"`
		ErrorCode    string `json:"errorCode"`
		ErrorDesc    string `json:"errorDesc"`
	}
	json.Unmarshal([]byte(body), &r)
	if r.RandomCodeID == "" || r.ImgCodeURL == "" {
		res.ErrorCode = r.ErrorCode
		res.ErrorDesc = r.ErrorDesc
		res.Message = "获取验证码失败: " + clipStr(body, 200)
		return res, nil
	}

	captchas.put(accountID, &captchaSession{
		sess:         sess,
		account:      account,
		password:     password,
		randomCodeID: r.RandomCodeID,
		imgURL:       r.ImgCodeURL,
		created:      time.Now(),
	})

	res.Stage = "captcha"
	res.ImageURL = r.ImgCodeURL
	res.RandomCodeID = r.RandomCodeID
	res.SessionKey = sessionCodeKey
	res.Message = "请输入图片中的字符"
	return res, nil
}

// CaptchaImage 返回验证码图片的原始字节。
//
// 华为返回的 imgCodeUrl 通常是 data:image/png;base64,... 内联图，
// 这种情况前端直接用它即可，不需要走这个接口。只有返回 http(s) URL 时才需要代理。
func CaptchaImage(accountID string) ([]byte, string, error) {
	cs := captchas.get(accountID)
	if cs == nil {
		return nil, "", fmt.Errorf("验证码已过期，请刷新")
	}
	if strings.HasPrefix(cs.imgURL, "data:") {
		return nil, "", fmt.Errorf("验证码是内联图片，直接用 image_url 字段")
	}

	req, err := http.NewRequest(http.MethodGet, cs.imgURL, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", cs.sess.ua)
	req.Header.Set("Referer", authBase+"/authui/login.html")
	req.Header.Set("Accept", "image/avif,image/webp,image/apng,image/*,*/*;q=0.8")

	client := &http.Client{Jar: cs.sess.jar, Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	ct := resp.Header.Get("Content-Type")
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, ct, err
	}
	if !strings.HasPrefix(ct, "image/") {
		return nil, ct, fmt.Errorf("返回的不是图片（Content-Type: %s）", ct)
	}
	return data, ct, nil
}

// CaptchaSubmit 校验用户填的验证码。
//
// SDK 的实际做法（见 sdk_union.js 的 ajaxAuthCodeValidate / ajaxLoginByPwd）：
//   - authCodeValidate 只是输入框的实时校验，服务端返回 10000201 表示「图片验证码已过期」，
//     不是参数错误；它随时可能失效，不能当作登录的前置条件。
//   - 真正登录时，randomCodeObj = {authcode, session_code_key} 会被展开进
//     unionLoginByPwd 表单，由登录接口自己校验验证码。
//
// 所以这里只做「记录用户输入」，不调用 authCodeValidate，避免多消耗一次有效期。
func CaptchaSubmit(accountID, code string) (*PureLoginResult, error) {
	res := &PureLoginResult{Stage: "captcha_ok"}

	cs := captchas.get(accountID)
	if cs == nil {
		res.Stage = "captcha"
		res.Message = "验证码已过期，请刷新"
		res.NeedManual = true
		return res, nil
	}

	cs.code = strings.TrimSpace(code)
	cs.verified = true
	res.Message = "已记录验证码"
	return res, nil
}

// AutoSolveCaptcha 用打码服务自动识别图片验证码。
//
// 只适用于 displayCaptchaType=0 的普通图片验证码；
// 滑块拼图（type=2/5）需要拖动轨迹，走另一条路。
func AutoSolveCaptcha(accountID string) (*PureLoginResult, error) {
	res := &PureLoginResult{Stage: "auto_captcha"}

	cs := captchas.get(accountID)
	if cs == nil {
		res.Stage = "captcha"
		res.Message = "验证码已过期，请刷新"
		res.NeedManual = true
		return res, nil
	}

	// 从缓存里取图片（FetchCaptcha 时拿到的 data URI）
	b64 := b64NoPrefix(cs.imgURL)
	if b64 == "" || !strings.HasPrefix(cs.imgURL, "data:") {
		res.Message = "当前验证码不是内联图片，无法自动识别"
		res.NeedManual = true
		return res, nil
	}

	text, err := OCRBase64(b64)
	if err != nil {
		res.Message = "自动识别失败: " + err.Error()
		res.NeedManual = true
		return res, nil
	}
	text = strings.TrimSpace(text)
	if text == "" {
		res.Message = "自动识别结果为空"
		res.NeedManual = true
		return res, nil
	}

	cs.code = text
	cs.verified = true
	res.Stage = "captcha_ok"
	res.Message = "自动识别为 " + text
	return res, nil
}

// LoginWithCaptcha 用已验证的验证码会话提交账密。
//
// smsCode 为空时只提交账密；若服务端要求二次验证，会返回 NeedSMS。
// smsCode 非空时走 opType=1 的二次验证提交。
func LoginWithCaptcha(accountID, smsCode string) (*PureLoginResult, error) {
	res := &PureLoginResult{}

	cs := captchas.get(accountID)
	if cs == nil {
		res.Stage = "captcha"
		res.Message = "验证码已过期，请刷新"
		res.NeedManual = true
		return res, nil
	}
	if !cs.verified {
		res.Stage = "captcha"
		res.Message = "请先完成图片验证码"
		res.NeedManual = true
		return res, nil
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
	// randomCodeObj 展开进表单：authcode 是 JSON 串，session_code_key 单独一个字段。
	// 这是 SDK 在 isNeedImageCode 时的做法（ajaxLoginByPwd 里的 ot.extend(s, r.randomCodeObj)）。
	authcode := fmt.Sprintf(`{"randomCodeID":%q,"randomCodeValue":%q}`, cs.randomCodeID, cs.code)

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
	uForm.Set("authcode", authcode)
	uForm.Set("session_code_key", sessionCodeKey)
	uForm.Set("service", authBase+"/authui/casLogin?service="+url.QueryEscape(loginService))
	uForm.Set("bsAcctService", authBase+"/authui/casLoginAPP?service="+url.QueryEscape(loginService))
	uForm.Set("scope", "https://www.huawei.com/auth/account/unified.profile+https://www.huawei.com/auth/account/risk.idstate+LoginState")
	uForm.Set("access_type", "offline")

	uBody, err := sess.post("login/unionLoginByPwd", uForm)
	if err != nil {
		res.Stage = "unionLoginByPwd"
		res.Message = err.Error()
		return res, nil
	}
	var uResp struct {
		ErrorCode   string `json:"errorCode"`
		ErrorDesc   string `json:"errorDesc"`
		CallbackURL string `json:"callbackURL"`
	}
	json.Unmarshal([]byte(uBody), &uResp)

	if uResp.ErrorCode == "10002080" {
		if smsCode == "" {
			res.Stage = "need_sms"
			res.NeedSMS = true
			res.Message = "需要短信验证码"
			return res, nil
		}
		// 带短信码重新提交
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
		vForm.Set("authcode", authcode)
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
			res.Message = "验证码校验失败"
			return res, nil
		}
		uResp.CallbackURL = vResp.CallbackURL
	} else if uResp.ErrorCode != "" {
		res.Stage = "unionLoginByPwd"
		res.ErrorCode = uResp.ErrorCode
		res.ErrorDesc = uResp.ErrorDesc
		res.Message = "登录失败"
		return res, nil
	}

	if uResp.CallbackURL != "" {
		_, _ = sess.get(uResp.CallbackURL)
	}

	// 信任设备（把这次设备标记为可信，以后免验证码）
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

	_ = SaveHwidSid(sess.sid)
	savePureCookies(sess.jar, config.CookieFile())

	meta := map[string]any{}
	if data, err := os.ReadFile(config.MetaFile()); err == nil {
		json.Unmarshal(data, &meta)
	}
	meta["user_agent"] = sess.ua
	meta["login_at"] = time.Now().Format("2006-01-02 15:04:05")
	meta["login_via"] = "captcha"
	if data, err := json.MarshalIndent(meta, "", "  "); err == nil {
		_ = fsutil.WriteFileAtomic(config.MetaFile(), data, 0o600)
	}

	captchas.drop(accountID)

	res.Stage = "done"
	res.Message = "登录成功，设备已信任（以后纯账密即可登录）"
	res.SavedTo = config.CookieFile()
	return res, nil
}

// HasCaptcha 返回某账号是否有进行中的验证码会话。
func HasCaptcha(accountID string) bool {
	return captchas.get(accountID) != nil
}

