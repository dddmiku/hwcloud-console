package task

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"
)

// 打码服务客户端（AntiCAP）。
//
// 鉴权：先 X-API-Key 换 JWT，再用 Bearer 调识别接口。
// JWT 有效期很长（exp 约 10 年），缓存在内存里复用。
//
// 服务地址和凭据全部从环境变量读，不写死在源码里 ——
// 本仓库是公开的，硬编码等于把打码服务送给所有人用。
//
//	HWCLOUD_CAPTCHA_BASE  https://你的打码服务
//	HWCLOUD_CAPTCHA_KEY   X-API-Key
//	HWCLOUD_CAPTCHA_USER  登录用户名
//	HWCLOUD_CAPTCHA_PASS  登录密码
//
// 不配置时滑块/OCR 相关功能直接返回「未配置」，不影响签到、文章等纯 HTTP 任务。
var (
	captchaMu    sync.Mutex
	captchaToken string
	captchaExp   time.Time
)

func captchaAPIBase() string { return os.Getenv("HWCLOUD_CAPTCHA_BASE") }
func captchaAPIKey() string { return os.Getenv("HWCLOUD_CAPTCHA_KEY") }
func captchaUser() string   { return os.Getenv("HWCLOUD_CAPTCHA_USER") }
func captchaPass() string   { return os.Getenv("HWCLOUD_CAPTCHA_PASS") }

// captchaConfigured 判断是否配了打码服务，没配就别发请求了。
func captchaConfigured() error {
	if captchaAPIBase() == "" || captchaAPIKey() == "" {
		return fmt.Errorf("未配置打码服务：请设置 HWCLOUD_CAPTCHA_BASE / HWCLOUD_CAPTCHA_KEY")
	}
	return nil
}

// captchaHTTP 是打码服务专用的 HTTP 客户端。
var captchaHTTP = &http.Client{Timeout: 30 * time.Second}

// captchaLogin 换 JWT，结果缓存。
func captchaLogin() (string, error) {
	captchaMu.Lock()
	defer captchaMu.Unlock()

	if err := captchaConfigured(); err != nil {
		return "", err
	}

	if captchaToken != "" && time.Now().Before(captchaExp) {
		return captchaToken, nil
	}

	form := url.Values{}
	form.Set("username", captchaUser())
	form.Set("password", captchaPass())

	req, err := http.NewRequest(http.MethodPost, captchaAPIBase()+"/api/login", bytes.NewBufferString(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("X-API-Key", captchaAPIKey())
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := captchaHTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("打码服务登录失败 %d: %s", resp.StatusCode, clipStr(string(body), 150))
	}

	var r struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &r); err != nil || r.AccessToken == "" {
		return "", fmt.Errorf("打码服务登录响应异常: %s", clipStr(string(body), 150))
	}

	captchaToken = r.AccessToken
	// JWT exp 很长，保守缓存 1 小时
	captchaExp = time.Now().Add(time.Hour)
	return captchaToken, nil
}

// SliderMatch 调用打码服务识别滑块缺口位置。
//
// 参数是两张图的 base64：target（滑块小图）和 background（带缺口的背景图）。
// 返回缺口在背景图上的 x 坐标（像素）。
func SliderMatch(targetB64, backgroundB64 string) (int, error) {
	token, err := captchaLogin()
	if err != nil {
		return 0, err
	}

	payload := map[string]string{
		"target_base64":     targetB64,
		"background_base64": backgroundB64,
	}
	data, _ := json.Marshal(payload)

	req, err := http.NewRequest(http.MethodPost, captchaAPIBase()+"/api/slider/match", bytes.NewReader(data))
	if err != nil {
		return 0, err
	}
	req.Header.Set("X-API-Key", captchaAPIKey())
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := captchaHTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("滑块识别失败 %d: %s", resp.StatusCode, clipStr(string(body), 150))
	}

	// 响应形如 {"result":{"target":[x1,y1,x2,y2]}}
	var r struct {
		Result struct {
			Target []int `json:"target"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return 0, fmt.Errorf("滑块识别响应解析失败: %s", clipStr(string(body), 200))
	}
	if len(r.Result.Target) < 3 {
		return 0, fmt.Errorf("滑块识别未返回坐标: %s", clipStr(string(body), 200))
	}
	// target[0] 是左边界 x，target[2] 是右边界 x，取中心
	x := (r.Result.Target[0] + r.Result.Target[2]) / 2
	return x, nil
}

// OCRBase64 识别图片里的文字（用于普通图片验证码）。
func OCRBase64(imgB64 string) (string, error) {
	token, err := captchaLogin()
	if err != nil {
		return "", err
	}

	payload := map[string]string{"image": imgB64}
	data, _ := json.Marshal(payload)

	req, err := http.NewRequest(http.MethodPost, captchaAPIBase()+"/api/ocr", bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("X-API-Key", captchaAPIKey())
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := captchaHTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("OCR 失败 %d: %s", resp.StatusCode, clipStr(string(body), 150))
	}

	var r struct {
		Result string `json:"result"`
		Text   string `json:"text"`
	}
	json.Unmarshal(body, &r)
	if r.Result != "" {
		return r.Result, nil
	}
	return r.Text, nil
}

// b64NoPrefix 去掉 data:image/...;base64, 前缀，只保留纯 base64。
func b64NoPrefix(s string) string {
	if i := bytes.Index([]byte(s), []byte("base64,")); i >= 0 {
		return s[i+7:]
	}
	return s
}

// b64Of 把原始字节编码成 base64。
func b64Of(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}
