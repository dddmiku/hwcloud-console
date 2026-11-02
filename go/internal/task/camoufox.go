package task

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"hwcloud/internal/config"
	"hwcloud/internal/fsutil"
)

// 华为云 Camoufox 浏览器登录客户端。
//
// 为什么不再走纯协议：
//   华为 SSO 的 fp 是 SDK 采集浏览器环境（canvas/WebGL/字体/时序）后加密生成的，
//   每次会话都不同。纯协议复用旧 fp/hwmeta 会被判「设备不安全」，
//   表现为反复要滑块验证码，而 type=5 的滑块根本过不了。
//   改用 Camoufox（Firefox 内核级指纹伪装）后实测直接跳过滑块、只发短信。
//
// 部署形态：
//   浏览器跑在服务器的 Xvfb 里（/opt/hwcam，systemd 单元 hwlogin），
//   监听 127.0.0.1:8901，面板通过这个客户端调用它。
//   会话产物（cookies.json / meta.json / fp.txt / hwmeta.txt）落在
//   /opt/hwcam/sessions/<phone>/，这里读回来写进账号 state 目录。

// CamoufoxBase 是 Camoufox 登录服务的地址，可用环境变量覆盖。
func CamoufoxBase() string {
	if v := os.Getenv("HWCLOUD_CAMOUFOX"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "http://127.0.0.1:8901"
}

// CamoufoxSessionDir 是服务端会话产物目录。
func CamoufoxSessionDir() string {
	if v := os.Getenv("HWCLOUD_CAMOUFOX_WORK"); v != "" {
		return v
	}
	return "/opt/hwcam/sessions"
}

// CamoufoxCookie 是 Camoufox 导出的 cookie（Playwright 原生形状）。
type CamoufoxCookie struct {
	Name    string  `json:"name"`
	Value   string  `json:"value"`
	Domain  string  `json:"domain"`
	Path    string  `json:"path"`
	Expires float64 `json:"expires"`
}

// CamoufoxResult 是 /login 的返回。
type CamoufoxResult struct {
	OK       bool              `json:"ok"`
	Stage    string            `json:"stage"`
	Phone    string            `json:"phone"`
	Trusted  bool              `json:"trusted"`
	FinalURL string            `json:"final_url"`
	Cookies  []CamoufoxCookie  `json:"cookies"`
	Meta     map[string]any    `json:"meta"`
	FP       string            `json:"fp"`
	Hwmeta   string            `json:"hwmeta"`
	Error    string            `json:"error"`
}

// camoufoxPost 调 Camoufox 服务的 POST 接口。
func camoufoxPost(path string, payload map[string]any, timeout time.Duration) (*CamoufoxResult, error) {
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, CamoufoxBase()+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("连接 Camoufox 服务失败（%s）: %w", CamoufoxBase(), err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var res CamoufoxResult
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, fmt.Errorf("解析返回失败: %s", clipStr(string(data), 200))
	}
	if res.Error != "" {
		return &res, fmt.Errorf("%s", res.Error)
	}
	return &res, nil
}

// CamoufoxHealth 探测服务是否在线。
func CamoufoxHealth() (bool, string) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(CamoufoxBase() + "/health")
	if err != nil {
		return false, err.Error()
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var h struct {
		OK      bool   `json:"ok"`
		Browser string `json:"browser"`
		Display string `json:"display"`
	}
	json.Unmarshal(data, &h)
	if !h.OK {
		return false, string(data)
	}
	return true, h.Browser
}

// CamoufoxLogin 提交账密；smsCode 为空时只到短信这一步。
func CamoufoxLogin(phone, password, smsCode string) (*CamoufoxResult, error) {
	payload := map[string]any{"phone": phone, "password": password}
	if smsCode != "" {
		payload["sms_code"] = smsCode
	}
	return camoufoxPost("/login", payload, 280*time.Second)
}

// CamoufoxLogout 让服务端丢弃该账号的会话并关闭浏览器。
//
// 为什么需要：Camoufox 是单实例服务，一个账号开着的浏览器会一直占着
// 全局 LOCK 和 1-2 GB 内存。用户关了登录弹窗（典型场景：等短信等到一半
// 不想等了），浏览器不会自己退出 —— 后续所有账号的登录都会卡在 LOCK 上，
// 最终报「连接 Camoufox 服务失败」，而没人知道是自己没填验证码导致的。
//
// 超时给 30 秒：关闭浏览器要等 profile 里的 SQLite WAL 落盘，
// 正常 2-3 秒，给足余量但不至于卡住 UI。
func CamoufoxLogout(phone string) error {
	body, _ := json.Marshal(map[string]any{"phone": phone})
	req, err := http.NewRequest(http.MethodPost, CamoufoxBase()+"/logout", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("通知 Camoufox 关闭会话失败: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("关闭会话返回 HTTP %d", resp.StatusCode)
	}
	return nil
}

// SaveCamoufoxArtifacts 把 Camoufox 会话产物写进当前账号的 state 目录。
//
// cookies 用 Playwright 原始 domain/path，比纯协议里手工拼的 pathFor 更准，
// 因为浏览器看到的就是服务端真正下发的那份。
func SaveCamoufoxArtifacts(res *CamoufoxResult) error {
	if res == nil {
		return fmt.Errorf("空结果")
	}
	if len(res.Cookies) == 0 {
		return fmt.Errorf("没有 cookie")
	}

	// cookies.json
	out := make([]map[string]any, 0, len(res.Cookies))
	for _, c := range res.Cookies {
		exp := c.Expires
		if exp < 0 {
			exp = -1
		}
		out = append(out, map[string]any{
			"name":    c.Name,
			"value":   c.Value,
			"domain":  c.Domain,
			"path":    c.Path,
			"expires": exp,
		})
	}
	blob, _ := json.MarshalIndent(out, "", "  ")
	if err := fsutil.WriteFileAtomic(config.CookieFile(), blob, 0o600); err != nil {
		return fmt.Errorf("写 cookies.json 失败: %w", err)
	}

	// meta.json：UA 必须和浏览器一致，否则 WAF 按 UA 指纹直接回 404
	meta := res.Meta
	if meta == nil {
		meta = map[string]any{}
	}
	if _, ok := meta["login_at"]; !ok {
		meta["login_at"] = time.Now().Format("2006-01-02 15:04:05")
	}
	meta["login_via"] = "camoufox"
	if blob, err := json.MarshalIndent(meta, "", "  "); err == nil {
		_ = fsutil.WriteFileAtomic(config.MetaFile(), blob, 0o600)
	}

	// fp / hwmeta 缓存：后续纯协议续期时能复用
	if res.FP != "" {
		_ = SaveFp(res.FP)
	}
	if res.Hwmeta != "" {
		_ = SaveHwmeta(res.Hwmeta)
	}

	// hwid_cas_sid：设备信任认的是 cookie 里的那个
	for _, c := range res.Cookies {
		if c.Name == "hwid_cas_sid" && c.Value != "" {
			_ = SaveHwidSid(c.Value)
			break
		}
	}
	return nil
}

// CamoufoxCookieNames 返回 cookie 名集合，用于判断登录是否真的成功。
func CamoufoxCookieNames(res *CamoufoxResult) map[string]bool {
	m := map[string]bool{}
	if res == nil {
		return m
	}
	for _, c := range res.Cookies {
		m[c.Name] = true
	}
	return m
}
