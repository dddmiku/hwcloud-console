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

	"hwcloud/internal/config"
	"hwcloud/internal/fsutil"
)

// 华为云纯协议登录（完整链路，全部经抓包验证）
//
// 流程：
//  1. GET  /authui/login.html                 → 建立基础 cookie
//  2. GET  /authui/getSDKBaseInfo             → 拿 pageToken / pageTokenKey / state / hwidConfig
//  3. POST common/getAgreementContent         → 建立 JSESSIONID（Path=/UnifiedIDMPortal）
//  4. POST common/dev  (带 fp)                → 返回 sid，即 hwid_cas_sid
//  5. POST login/unionLoginByPwd              → 提交账密，触发二次验证
//  6. POST login/getSMSCodeV3                 → 发送短信验证码
//  7. POST login/unionLoginByPwd (带 smsCode) → 完成登录
//
// 关键点：
//   - fp 是浏览器指纹，实测可复用（不绑定会话、不校验时效）。抓一次存 fp_cache.txt 即可。
//   - hwid_cas_sid 就是 common/dev 返回的 sid，不是浏览器自己生成的。
//   - JSESSIONID 的 Path 是 /UnifiedIDMPortal，查询 cookie 时要带对路径。

const idpAjaxPath = "/UnifiedIDMPortal/ajaxHandler/"

// FpFile 返回缓存的浏览器指纹文件路径。
func FpFile() string {
	return filepath.Join(config.StateDir(), "fp_cache.txt")
}

// HwmetaFile 返回缓存的浏览器环境指纹（hwmeta）文件路径。
// hwmeta 是 1356 字符的 base64，由 SDK 采集浏览器环境生成，
// 实测和 fp 一样可以复用。
func HwmetaFile() string {
	return filepath.Join(config.StateDir(), "hwmeta_cache.txt")
}

// LoadHwmeta 读取缓存的 hwmeta。
func LoadHwmeta() (string, error) {
	data, err := os.ReadFile(HwmetaFile())
	if err != nil {
		return "", fmt.Errorf("缺少 hwmeta 缓存（%s）", HwmetaFile())
	}
	v := strings.TrimSpace(string(data))
	if v == "" {
		return "", fmt.Errorf("hwmeta 缓存为空")
	}
	return v, nil
}

// SaveHwmeta 保存 hwmeta 到缓存。
func SaveHwmeta(v string) error {
	if err := os.MkdirAll(config.StateDir(), 0o755); err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(HwmetaFile(), []byte(strings.TrimSpace(v)), 0o600)
}

// LoadFp 读取缓存的 fp。
func LoadFp() (string, error) {
	data, err := os.ReadFile(FpFile())
	if err != nil {
		return "", fmt.Errorf("缺少浏览器指纹缓存（%s）", FpFile())
	}
	fp := strings.TrimSpace(string(data))
	if fp == "" {
		return "", fmt.Errorf("指纹缓存为空")
	}
	return fp, nil
}

// SaveFp 保存 fp 到缓存。
func SaveFp(fp string) error {
	if err := os.MkdirAll(config.StateDir(), 0o755); err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(FpFile(), []byte(strings.TrimSpace(fp)), 0o600)
}

// sdkInfo 是 getSDKBaseInfo 的响应。
type sdkInfo struct {
	PageToken    string `json:"pageToken"`
	PageTokenKey string `json:"pageTokenKey"`
	State        string `json:"state"`
	HwidConfig   string `json:"hwidConfig"`
}

// hwidCfg 是 hwidConfig 解码后的关键字段。
type hwidCfg struct {
	LocalStorageID string
	CurrentSiteID  string
	CookieVersion  string
}

// pureSession 是纯协议登录用的 HTTP 会话。
type pureSession struct {
	client *http.Client
	ua     string
	jar    http.CookieJar
	info   *sdkInfo
	cfg    *hwidCfg
	sid    string
}

// cookieSid 返回会话里真正的 hwid_cas_sid。
//
// sess.sid 是 common/dev 返回的值，但登录成功后服务端会通过 Set-Cookie
// 下发一个「已信任」的 hwid_cas_sid，两者可能不同。
// 设备信任认的是 cookie 里的那个，所以持久化必须用它，否则下次登录
// 拿新 sid 去比，永远判定「设备信任失效」，陷入反复验证。
func (s *pureSession) cookieSid() string {
	for _, host := range []string{"id1.cloud.huawei.com", "auth.huaweicloud.com"} {
		for _, p := range []string{"/", "/UnifiedIDMPortal"} {
			for _, c := range s.jar.Cookies(&url.URL{Scheme: "https", Host: host, Path: p}) {
				if c.Name == "hwid_cas_sid" && c.Value != "" {
					return c.Value
				}
			}
		}
	}
	return s.sid
}

func newPureSession() (*pureSession, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	return &pureSession{
		client: &http.Client{
			Jar:     jar,
			Timeout: 30 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) > 20 {
					return fmt.Errorf("重定向次数过多")
				}
				return nil
			},
		},
		ua:  config.UserAgent(),
		jar: jar,
	}, nil
}

func (s *pureSession) get(target string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", s.ua)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), nil
}

func (s *pureSession) post(path string, form url.Values) (string, error) {
	u := idpBase + idpAjaxPath + path + "?reflushCode=" +
		fmt.Sprintf("%.16f", float64(time.Now().UnixNano())/1e9) +
		"&cVersion=UP_CAS_6.26.3.100_green"
	req, err := http.NewRequest(http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", s.ua)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	req.Header.Set("Origin", authBase)
	req.Header.Set("Referer", authBase+"/authui/login.html")
	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), nil
}

// bootstrap 完成 1-4 步，拿到 sid（hwid_cas_sid）。
func (s *pureSession) bootstrap() error {
	// 1. 登录页
	if _, err := s.get(authBase + "/authui/login.html?service=" +
		url.QueryEscape(loginService) + "&locale=zh-cn"); err != nil {
		return err
	}

	// 2. getSDKBaseInfo
	body, err := s.get(authBase + "/authui/getSDKBaseInfo?flowType=unionLogin&service=" +
		url.QueryEscape(loginService) + "&site=mainland")
	if err != nil {
		return err
	}
	var info sdkInfo
	if err := json.Unmarshal([]byte(body), &info); err != nil {
		return fmt.Errorf("解析 getSDKBaseInfo: %w", err)
	}
	if info.PageToken == "" {
		return fmt.Errorf("响应里没有 pageToken")
	}
	s.info = &info

	// 2.5 访问 isLogin，让服务端把 pageToken 与当前会话关联起来。
	// 抓包显示浏览器在 getSDKBaseInfo 之后会先调这个，再调 getAgreementContent；
	// 跳过它会导致后续 unionLoginByPwd 报 10000600（页面过期）。
	_, _ = s.get(authBase + "/authui/isLogin")

	cfg := &hwidCfg{CurrentSiteID: "1"}
	if info.HwidConfig != "" {
		dec, _ := url.QueryUnescape(info.HwidConfig)
		var h map[string]any
		if json.Unmarshal([]byte(dec), &h) == nil {
			getStr := func(k string) string {
				if v, ok := h[k].(string); ok {
					return unescapeHTMLEntities(v)
				}
				return ""
			}
			if v := getStr("localStorageID"); v != "" {
				cfg.LocalStorageID = v
			}
			if v := getStr("currentSiteID"); v != "" {
				cfg.CurrentSiteID = v
			}
			cfg.CookieVersion = getStr("cookieVersion")
		}
	}
	s.cfg = cfg

	// 3. getAgreementContent（建立 JSESSIONID）
	ag := s.baseForm()
	ag.Set("operType", "0")
	ag.Set("regionCode", "cn")
	if _, err := s.post("common/getAgreementContent", ag); err != nil {
		return err
	}

	// 4. common/dev（拿 sid）
	fp, err := LoadFp()
	if err != nil {
		return err
	}
	dev := s.baseForm()
	dev.Set("fp", fp)
	devBody, err := s.post("common/dev", dev)
	if err != nil {
		return err
	}
	var devResp struct {
		Sid       string `json:"sid"`
		IsSuccess int    `json:"isSuccess"`
	}
	if err := json.Unmarshal([]byte(devBody), &devResp); err != nil {
		return fmt.Errorf("解析 common/dev: %w", err)
	}
	if devResp.Sid == "" {
		return fmt.Errorf("common/dev 未返回 sid: %s", clipStr(devBody, 200))
	}
	s.sid = devResp.Sid
	return nil
}

func (s *pureSession) baseForm() url.Values {
	v := url.Values{}
	v.Set("pageToken", s.info.PageToken)
	v.Set("pageTokenKey", s.info.PageTokenKey)
	v.Set("reqClientType", "88")
	v.Set("loginChannel", "88000000")
	v.Set("clientID", "103493351")
	v.Set("lang", "zh-cn")
	v.Set("languageCode", "zh-cn")
	v.Set("state", s.info.State)
	v.Set("localStorageID", s.cfg.LocalStorageID)
	return v
}

// HwidSidFile 返回缓存的 hwid_cas_sid 文件路径。
//
// 这是整套机制的钥匙：一旦设备被信任，common/dev 会对同一个 fp 返回稳定的 sid，
// unionLoginByPwd 就不再触发二次验证（10002080），纯账密即可登录。
// 所以必须持久化这个值，并在每次请求时带上。
func HwidSidFile() string {
	return filepath.Join(config.StateDir(), "hwid_cas_sid.txt")
}

// LoadHwidSid 读取缓存的 hwid_cas_sid。
func LoadHwidSid() (string, error) {
	data, err := os.ReadFile(HwidSidFile())
	if err != nil {
		return "", fmt.Errorf("缺少 hwid_cas_sid（需要先完成一次带验证码的登录并信任设备）")
	}
	v := strings.TrimSpace(string(data))
	if v == "" {
		return "", fmt.Errorf("hwid_cas_sid 缓存为空")
	}
	return v, nil
}

// SaveHwidSid 保存 hwid_cas_sid。
func SaveHwidSid(v string) error {
	if err := os.MkdirAll(config.StateDir(), 0o755); err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(HwidSidFile(), []byte(strings.TrimSpace(v)), 0o600)
}

// TrustDeviceLogin 完成「带验证码登录 + 信任设备」的完整流程。
//
// 这是首次登录服务时用的：走一次短信验证，然后把设备标记为受信任。
// 成功后会保存 hwid_cas_sid，之后 LoginByPassword 就不再需要验证码。
func TrustDeviceLogin(account, password, smsCode string) (*PureLoginResult, error) {
	res := &PureLoginResult{}

	sess, err := newPureSession()
	if err != nil {
		return nil, err
	}

	if err := sess.bootstrap(); err != nil {
		res.Stage = "bootstrap"
		res.Message = err.Error()
		return res, nil
	}
	res.Stage = "bootstrap"
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
	pwForm.Set("password", password)
	pwForm.Set("reqClientType", "88")
	pwForm.Set("state", sess.info.State)
	pwForm.Set("userAccount", account)
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

	// 触发二次验证
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

		// 带验证码重新提交（opType=1, verifyAccountType, twoStepVerifyCode）
		vForm := sess.baseForm()
		vForm.Set("userAccount", account)
		vForm.Set("password", password)
		vForm.Set("opType", "1")
		vForm.Set("serial", "0")
		vForm.Set("registerCountry", "cn")
		vForm.Set("anonymousLoginID", anonAcc)
		vForm.Set("hwid_cas_sid", sess.sid)
		if hwmeta != "" {
			vForm.Set("hwmeta", hwmeta)
		}
		vForm.Set("verifyUserAccount", maskPhone(account))
		vForm.Set("verifyAccountType", "2")
		vForm.Set("twoStepVerifyCode", smsCode)
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
		return res, nil
	}

	// 跟随 callbackURL 完成 OAuth
	if uResp.CallbackURL != "" {
		_, _ = sess.get(uResp.CallbackURL)
	}

	// riskData
	rdForm := url.Values{}
	rdForm.Set("devID", sess.sid)
	rdForm.Set("hwMeta", hwmeta)
	_, _ = sess.postRaw(authBase+"/authui/riskData", rdForm)

	// 信任设备
	tbForm := sess.baseForm()
	tbForm.Set("userAccount", account)
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

	// 保存 sid 和 cookie
	_ = SaveHwidSid(sess.sid)
	savePureCookies(sess.jar, config.CookieFile())

	meta := map[string]any{}
	if data, err := os.ReadFile(config.MetaFile()); err == nil {
		json.Unmarshal(data, &meta)
	}
	meta["user_agent"] = sess.ua
	meta["login_at"] = time.Now().Format("2006-01-02 15:04:05")
	meta["login_via"] = "trust-device"
	if data, err := json.MarshalIndent(meta, "", "  "); err == nil {
		_ = fsutil.WriteFileAtomic(config.MetaFile(), data, 0o600)
	}

	res.Stage = "done"
	res.Message = "登录成功，设备已信任（以后纯账密即可登录）"
	res.SavedTo = config.CookieFile()
	return res, nil
}

// postRaw 用完整 URL 发 POST（不走 ajaxHandler 前缀）。
func (s *pureSession) postRaw(target string, form url.Values) (string, error) {
	req, err := http.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", s.ua)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", authBase)
	req.Header.Set("Referer", authBase+"/authui/login.html")
	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), nil
}

// LoginByPassword 纯协议账密登录（设备已信任时无需验证码）。
//
// 前提：hwid_cas_sid 已缓存且设备已信任（通过 TrustDevice 完成一次）。
// 成功后把 cookie 写入 config.CookieFile()。
func LoginByPassword(account, password string) (*PureLoginResult, error) {
	res := &PureLoginResult{}

	savedSid, err := LoadHwidSid()
	if err != nil {
		res.Stage = "precheck"
		res.Message = err.Error()
		res.NeedManual = true
		return res, nil
	}

	sess, err := newPureSession()
	if err != nil {
		return nil, err
	}

	// 1-3. bootstrap 只到拿 pageToken / JSESSIONID，不用它生成的 sid
	if err := sess.bootstrapNoDev(); err != nil {
		res.Stage = "bootstrap"
		res.Message = err.Error()
		return res, nil
	}

	// 4. common/dev —— 用同一个 fp 请求，已信任时会返回与缓存一致的 sid
	fp, err := LoadFp()
	if err != nil {
		res.Stage = "common/dev"
		res.Message = err.Error()
		return res, nil
	}
	dev := sess.baseForm()
	dev.Set("fp", fp)
	devBody, err := sess.post("common/dev", dev)
	if err != nil {
		res.Stage = "common/dev"
		res.Message = err.Error()
		return res, nil
	}
	var devResp struct {
		Sid string `json:"sid"`
	}
	json.Unmarshal([]byte(devBody), &devResp)
	if devResp.Sid == "" {
		res.Stage = "common/dev"
		res.Message = "common/dev 未返回 sid"
		return res, nil
	}
	if devResp.Sid != savedSid {
		// sid 变了说明设备信任已失效，需要重新走一次验证
		res.Stage = "common/dev"
		res.Sid = devResp.Sid
		res.NeedManual = true
		res.Message = "设备信任已失效（sid 与缓存不一致），需要重新验证一次"
		_ = SaveHwidSid(devResp.Sid)
		return res, nil
	}
	res.Sid = devResp.Sid

	// 5. 密码校验，拿 anonymousLoginID
	pwForm := url.Values{}
	pwForm.Set("clientID", "103493351")
	pwForm.Set("hwid_cas_sid", savedSid)
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

	// 6. unionLoginByPwd —— 设备已信任时直接成功
	hwmeta, _ := LoadHwmeta()
	uForm := sess.baseForm()
	uForm.Set("userAccount", account)
	uForm.Set("password", password)
	uForm.Set("opType", "0")
	uForm.Set("serial", "0")
	uForm.Set("registerCountry", "cn")
	uForm.Set("anonymousLoginID", anonAcc)
	uForm.Set("hwid_cas_sid", savedSid)
	if hwmeta != "" {
		uForm.Set("hwmeta", hwmeta)
	}
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
		res.Stage = "unionLoginByPwd"
		res.ErrorCode = uResp.ErrorCode
		res.NeedManual = true
		res.Message = "设备信任未生效，需要走一次短信验证"
		return res, nil
	}
	if uResp.ErrorCode != "" {
		res.Stage = "unionLoginByPwd"
		res.ErrorCode = uResp.ErrorCode
		res.ErrorDesc = uResp.ErrorDesc
		res.Message = "登录失败"
		return res, nil
	}

	// 7. 跟随 callbackURL 完成 OAuth
	if uResp.CallbackURL != "" {
		_, _ = sess.get(uResp.CallbackURL)
	}

	// 8. 写盘
	savePureCookies(sess.jar, config.CookieFile())
	meta := map[string]any{}
	if data, err := os.ReadFile(config.MetaFile()); err == nil {
		json.Unmarshal(data, &meta)
	}
	meta["user_agent"] = sess.ua
	meta["login_at"] = time.Now().Format("2006-01-02 15:04:05")
	meta["login_via"] = "pure-protocol-password"
	if data, err := json.MarshalIndent(meta, "", "  "); err == nil {
		_ = fsutil.WriteFileAtomic(config.MetaFile(), data, 0o600)
	}

	res.Stage = "done"
	res.Message = "登录成功（无需验证码）"
	res.SavedTo = config.CookieFile()
	return res, nil
}

// bootstrapNoDev 完成 1-3 步（不含 common/dev）。
func (s *pureSession) bootstrapNoDev() error {
	if _, err := s.get(authBase + "/authui/login.html?service=" +
		url.QueryEscape(loginService) + "&locale=zh-cn"); err != nil {
		return err
	}

	body, err := s.get(authBase + "/authui/getSDKBaseInfo?flowType=unionLogin&service=" +
		url.QueryEscape(loginService) + "&site=mainland")
	if err != nil {
		return err
	}
	var info sdkInfo
	if err := json.Unmarshal([]byte(body), &info); err != nil {
		return fmt.Errorf("解析 getSDKBaseInfo: %w", err)
	}
	if info.PageToken == "" {
		return fmt.Errorf("响应里没有 pageToken")
	}
	s.info = &info

	cfg := &hwidCfg{CurrentSiteID: "1"}
	if info.HwidConfig != "" {
		dec, _ := url.QueryUnescape(info.HwidConfig)
		var h map[string]any
		if json.Unmarshal([]byte(dec), &h) == nil {
			getStr := func(k string) string {
				if v, ok := h[k].(string); ok {
					return unescapeHTMLEntities(v)
				}
				return ""
			}
			if v := getStr("localStorageID"); v != "" {
				cfg.LocalStorageID = v
			}
			if v := getStr("currentSiteID"); v != "" {
				cfg.CurrentSiteID = v
			}
			cfg.CookieVersion = getStr("cookieVersion")
		}
	}
	s.cfg = cfg

	ag := s.baseForm()
	ag.Set("operType", "0")
	ag.Set("regionCode", "cn")
	_, err = s.post("common/getAgreementContent", ag)
	return err
}

// VerifyPassword 纯协议验证账号密码是否正确（只读，不产生登录动作）。
//
// 走的是 login/getLoginIdsByPwd，返回账号信息说明密码正确。
// 这个接口不会触发登录风控，可以安全调用。
func VerifyPassword(account, password string) (*PureLoginResult, error) {
	sess, err := newPureSession()
	if err != nil {
		return nil, err
	}
	res := &PureLoginResult{}

	if err := sess.bootstrap(); err != nil {
		res.Stage = "bootstrap"
		res.Message = err.Error()
		return res, nil
	}
	res.Stage = "bootstrap"
	res.Sid = sess.sid

	form := url.Values{}
	form.Set("clientID", "103493351")
	form.Set("hwid_cas_sid", sess.sid)
	form.Set("lang", "zh-cn")
	form.Set("languageCode", "zh-cn")
	form.Set("localStorageID", sess.cfg.LocalStorageID)
	form.Set("loginChannel", "88000000")
	form.Set("pageToken", sess.info.PageToken)
	form.Set("pageTokenKey", sess.info.PageTokenKey)
	form.Set("password", password)
	form.Set("reqClientType", "88")
	form.Set("state", sess.info.State)
	form.Set("userAccount", account)

	body, err := sess.post("login/getLoginIdsByPwd", form)
	if err != nil {
		res.Stage = "getLoginIdsByPwd"
		res.Message = err.Error()
		return res, nil
	}

	var r struct {
		AccountInfoList []struct {
			AnonymousAccount    string `json:"anonymousAccount"`
			AnonymousEncryption string `json:"anonymousEncryption"`
		} `json:"accountInfoList"`
		ErrorCode string `json:"errorCode"`
		ErrorDesc string `json:"errorDesc"`
	}
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		res.Stage = "getLoginIdsByPwd"
		res.Message = "解析失败: " + err.Error()
		return res, nil
	}

	res.Stage = "getLoginIdsByPwd"
	if r.ErrorCode != "" {
		res.ErrorCode = r.ErrorCode
		res.ErrorDesc = r.ErrorDesc
		res.Message = "密码校验未通过"
		return res, nil
	}
	if len(r.AccountInfoList) > 0 {
		res.Username = r.AccountInfoList[0].AnonymousAccount
		res.Message = "密码正确"
	} else {
		res.Message = "密码校验通过，但未返回账号信息"
	}
	return res, nil
}

// SendSMSCode 纯协议发送短信验证码。
// 前提是密码已通过（服务端会校验账号存在）。
func SendSMSCode(account, password string) (*PureLoginResult, error) {
	sess, err := newPureSession()
	if err != nil {
		return nil, err
	}
	res := &PureLoginResult{}

	if err := sess.bootstrap(); err != nil {
		res.Stage = "bootstrap"
		res.Message = err.Error()
		return res, nil
	}
	res.Stage = "bootstrap"

	// 先过密码校验，服务端才会建立登录会话（否则 getSMSCodeV3 报 session is invalid）
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
		res.Stage = "getLoginIdsByPwd"
		res.Message = err.Error()
		return res, nil
	}
	var pwResp struct {
		ErrorCode string `json:"errorCode"`
		ErrorDesc string `json:"errorDesc"`
	}
	json.Unmarshal([]byte(pwBody), &pwResp)
	if pwResp.ErrorCode != "" {
		res.Stage = "getLoginIdsByPwd"
		res.ErrorCode = pwResp.ErrorCode
		res.ErrorDesc = pwResp.ErrorDesc
		res.Message = "密码校验未通过，无法发送短信"
		return res, nil
	}

	form := sess.baseForm()
	form.Set("userAccount", account)
	form.Set("accountType", "2")
	form.Set("mobilePhone", maskPhone(account))
	form.Set("operType", "8")
	form.Set("smsReqType", "6")
	form.Set("hwid_cas_sid", sess.sid)

	body, err := sess.post("login/getSMSCodeV3", form)
	if err != nil {
		res.Stage = "getSMSCodeV3"
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

	res.Stage = "getSMSCodeV3"
	res.ErrorCode = r.ErrorCode
	res.ErrorDesc = r.ErrorDesc
	if r.IsSuccess == 1 {
		res.NeedSMS = true
		res.Message = "短信已发送"
		if r.PreviousSentCode != "" {
			res.Message += "（上次发送码 " + r.PreviousSentCode + "）"
		}
	} else {
		res.Message = "短信发送失败"
	}
	return res, nil
}

// PureLoginResult 描述纯协议登录的结果。
type PureLoginResult struct {
	Stage      string `json:"stage"`
	Sid        string `json:"sid,omitempty"`
	NeedSMS    bool   `json:"need_sms"`
	NeedManual bool   `json:"need_manual,omitempty"`
	Message    string `json:"message,omitempty"`
	ErrorCode  string `json:"error_code,omitempty"`
	ErrorDesc  string `json:"error_desc,omitempty"`
	Username   string `json:"username,omitempty"`
	SavedTo    string `json:"saved_to,omitempty"`
}

// PureLoginStep 执行到「提交账密」这一步。
// 返回 NeedSMS=true 表示已发送短信，需要再调用 PureLoginWithSMS。
func PureLoginStep(account, password string) (*PureLoginResult, error) {
	sess, err := newPureSession()
	if err != nil {
		return nil, err
	}
	res := &PureLoginResult{}

	if err := sess.bootstrap(); err != nil {
		res.Stage = "bootstrap"
		res.Message = err.Error()
		return res, nil
	}
	res.Stage = "bootstrap"
	res.Sid = sess.sid

	// 5. 提交账密
	form := sess.baseForm()
	form.Set("userAccount", account)
	form.Set("password", password)
	form.Set("opType", "0")
	form.Set("serial", "0")
	form.Set("registerCountry", "cn")
	form.Set("anonymousLoginID", maskAccount(account))
	form.Set("hwid_cas_sid", sess.sid)
	form.Set("service", authBase+"/authui/casLogin?service="+url.QueryEscape(loginService))
	form.Set("bsAcctService", authBase+"/authui/casLoginAPP?service="+url.QueryEscape(loginService))
	form.Set("scope", "https://www.huawei.com/auth/account/unified.profile+https://www.huawei.com/auth/account/risk.idstate+LoginState")
	form.Set("access_type", "offline")
	if hwmeta, err := LoadHwmeta(); err == nil {
		form.Set("hwmeta", hwmeta)
	}

	body, err := sess.post("login/unionLoginByPwd", form)
	if err != nil {
		res.Stage = "unionLoginByPwd"
		res.Message = err.Error()
		return res, nil
	}

	var r struct {
		ErrorCode string `json:"errorCode"`
		ErrorDesc string `json:"errorDesc"`
		IsSuccess int    `json:"isSuccess"`
		NeedAuth  any    `json:"needAuthCode"`
	}
	json.Unmarshal([]byte(body), &r)

	if r.ErrorCode != "" {
		res.Stage = "unionLoginByPwd"
		res.ErrorCode = r.ErrorCode
		res.ErrorDesc = r.ErrorDesc
		res.Message = "账号密码校验未通过"
		return res, nil
	}

	// 6. 发短信
	smsForm := sess.baseForm()
	smsForm.Set("userAccount", account)
	smsForm.Set("accountType", "2")
	smsForm.Set("mobilePhone", maskPhone(account))
	smsForm.Set("operType", "8")
	smsForm.Set("smsReqType", "6")
	smsForm.Set("hwid_cas_sid", sess.sid)

	smsBody, err := sess.post("login/getSMSCodeV3", smsForm)
	if err != nil {
		res.Stage = "getSMSCodeV3"
		res.Message = err.Error()
		return res, nil
	}
	json.Unmarshal([]byte(smsBody), &r)

	res.Stage = "getSMSCodeV3"
	res.NeedSMS = true
	res.Message = "账号密码通过，短信已发送"
	res.ErrorCode = r.ErrorCode
	res.ErrorDesc = r.ErrorDesc
	res.SavedTo = config.CookieFile() + ".pure"
	savePureCookies(sess.jar, res.SavedTo)
	return res, nil
}

// PureLoginWithSMS 带短信验证码完成登录。
func PureLoginWithSMS(account, password, smsCode string) (*PureLoginResult, error) {
	sess, err := newPureSession()
	if err != nil {
		return nil, err
	}
	res := &PureLoginResult{}

	if err := sess.bootstrap(); err != nil {
		res.Stage = "bootstrap"
		res.Message = err.Error()
		return res, nil
	}

	// 重新提交账密 + 验证码
	form := sess.baseForm()
	form.Set("userAccount", account)
	form.Set("password", password)
	form.Set("opType", "0")
	form.Set("serial", "0")
	form.Set("registerCountry", "cn")
	form.Set("anonymousLoginID", maskAccount(account))
	form.Set("hwid_cas_sid", sess.sid)
	form.Set("service", authBase+"/authui/casLogin?service="+url.QueryEscape(loginService))
	form.Set("scope", "https://www.huawei.com/auth/account/unified.profile+https://www.huawei.com/auth/account/risk.idstate+LoginState")
	form.Set("access_type", "offline")
	form.Set("smsAuthCode", smsCode)

	body, err := sess.post("login/unionLoginByPwd", form)
	if err != nil {
		res.Stage = "unionLoginByPwd"
		res.Message = err.Error()
		return res, nil
	}

	var r struct {
		ErrorCode   string `json:"errorCode"`
		ErrorDesc   string `json:"errorDesc"`
		CallbackURL string `json:"callbackURL"`
	}
	json.Unmarshal([]byte(body), &r)

	if r.ErrorCode != "" {
		res.Stage = "unionLoginByPwd"
		res.ErrorCode = r.ErrorCode
		res.ErrorDesc = r.ErrorDesc
		return res, nil
	}

	// 跟随 callbackURL 完成 OAuth
	if r.CallbackURL != "" {
		if _, err := sess.get(r.CallbackURL); err != nil {
			res.Message = "跟随 callbackURL 失败: " + err.Error()
		}
	}

	// 导出 cookie
	path := config.CookieFile()
	savePureCookies(sess.jar, path)
	res.Stage = "done"
	res.SavedTo = path
	res.Message = "登录成功"

	if data, err := os.ReadFile(config.MetaFile()); err == nil {
		var meta map[string]any
		if json.Unmarshal(data, &meta) == nil {
			meta["user_agent"] = sess.ua
			meta["login_at"] = time.Now().Format("2006-01-02 15:04:05")
			meta["login_via"] = "pure-protocol"
			if b, err := json.MarshalIndent(meta, "", "  "); err == nil {
				_ = fsutil.WriteFileAtomic(config.MetaFile(), b, 0o600)
			}
		}
	}
	return res, nil
}

// savePureCookies 把 jar 里的 cookie 写盘。
func savePureCookies(jar http.CookieJar, path string) {
	hosts := []string{
		"auth.huaweicloud.com", "id1.cloud.huawei.com", "developer.huaweicloud.com",
		"devdata.huaweicloud.com", "bbs.huaweicloud.com", "huaweicloud.com",
		"console.huaweicloud.com", "portal.huaweicloud.com", "uba.huaweicloud.com",
	}
	var out []map[string]any
	seen := map[string]bool{}
	// /authui/ 必须有：SSOTGC（devdata 会话的钥匙）就挂在这个路径下，
	// 漏了它登录后 session 永远是 expired。
	paths := []string{"/", "/UnifiedIDMPortal", "/authui/"}

	// pathFor 决定 cookie 应该落在哪个 path 上。
	// JSESSIONID 在 /UnifiedIDMPortal，SSOTGC/SSOJTC 在 /authui/，
	// 其余（尤其 CASTGC）都必须是 /，否则 CAS 换票时找不到会话。
	pathFor := func(host, name string) string {
		if name == "JSESSIONID" && strings.Contains(host, "id1.cloud.huawei.com") {
			return "/UnifiedIDMPortal"
		}
		if name == "SSOTGC" || name == "SSOJTC" {
			return "/authui/"
		}
		return "/"
	}

	for _, h := range hosts {
		for _, p := range paths {
			for _, c := range jar.Cookies(&url.URL{Scheme: "https", Host: h, Path: p}) {
				key := c.Name + "|" + c.Domain
				if seen[key] {
					continue
				}
				seen[key] = true
				dom := c.Domain
				if dom == "" {
					dom = h
				}
				exp := float64(-1)
				if !c.Expires.IsZero() {
					exp = float64(c.Expires.Unix())
				}
				out = append(out, map[string]any{
					"name": c.Name, "value": c.Value, "domain": dom,
					"path": pathFor(h, c.Name), "expires": exp,
				})
			}
		}
	}
	if data, err := json.MarshalIndent(out, "", "  "); err == nil {
		_ = os.MkdirAll(filepath.Dir(path), 0o755)
		_ = fsutil.WriteFileAtomic(path, data, 0o600)
	}
}

// maskPhone 把 13800138000 变成 138******00。
func maskPhone(p string) string {
	if len(p) != 11 {
		return p
	}
	return p[:3] + "******" + p[9:]
}

// maskAccount 把 13800138000 变成 1******00 这种匿名 ID。
func maskAccount(p string) string {
	if len(p) < 3 {
		return p
	}
	return p[:1] + strings.Repeat("*", len(p)-3) + p[len(p)-2:]
}

func clipStr(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// unescapeHTMLEntities 还原 &#x3a; / &#x2f; 这类 HTML 实体。
func unescapeHTMLEntities(s string) string {
	r := strings.NewReplacer(
		"&#x3a;", ":", "&#x2f;", "/", "&#x3d;", "=", "&#x3f;", "?",
		"&#x26;", "&", "&amp;", "&", "&#x23;", "#", "&#x25;", "%",
		"&#x2b;", "+", "&quot;", "\"", "&#x2c;", ",",
	)
	return r.Replace(s)
}
