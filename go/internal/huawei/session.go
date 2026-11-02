package huawei

import (
	"bytes"
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

	"github.com/mattn/go-ieproxy"

	"hwcloud/internal/config"
	"hwcloud/internal/fsutil"
)

type Session struct {
	client     *http.Client
	jar        http.CookieJar
	csrf       string
	UA         string
	Username   string
	CookieFile string
	rawCookies []cookieJSON
}

type cookieJSON struct {
	Name    string  `json:"name"`
	Value   string  `json:"value"`
	Domain  string  `json:"domain"`
	Path    string  `json:"path"`
	Expires float64 `json:"expires"`
}

type Mission struct {
	Degree   int    `json:"degree"`
	Title    string `json:"title"`
	IsFinish int    `json:"is_finish"`
	Bonus    int    `json:"bonus"`
	URL      string `json:"url"`
}

type missionResponse struct {
	Total int       `json:"total"`
	List  []Mission `json:"QueryMissionDtoList"`
}

// NewForAccount 构造一个**不依赖全局 accountID** 的会话。
//
// 为什么需要这个：config.SetAccount 是进程级全局变量，CookieFile()
// 和 UserAgent() 都从它推导路径。所以任何读写会话的代码都必须先拿
// 全局锁 s.mu，把 accountID 切到目标账号 —— 而全局锁会被周常/保活
// 这类长任务持有 5-10 分钟。
//
// 结果是：点「任务」「积分」这种 1 秒就能返回的纯 HTTP 查询，
// 要排队等 10 分钟，最后还可能撞上前端超时。用户看到的是
// 「有任务正在执行，账号上下文被占用」。
//
// 这个构造函数直接接收账号目录，把 cookie 和 UA 的读取路径固定下来，
// 不碰任何全局状态，因此可以并发调用、完全不需要锁。
//
// dir 是账号的 state 目录（如 /opt/hwcloud-demo/accounts/<phone>/state）。
func NewForAccount(dir string) (*Session, error) {
	cookieFile := filepath.Join(dir, "cookies.json")
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{
		Proxy:             ieproxy.GetProxyFunc(),
		ForceAttemptHTTP2: false,
	}
	s := &Session{
		client:     &http.Client{Jar: jar, Timeout: 20 * time.Second, Transport: transport},
		jar:        jar,
		UA:         userAgentIn(dir),
		CookieFile: cookieFile,
	}
	if err := s.loadCookies(cookieFile); err != nil {
		return nil, err
	}
	return s, nil
}

// userAgentIn 读指定账号目录下 meta.json 里的 UA。
//
// 必须是这个而不是 config.UserAgent()：后者读全局 accountID 指向的目录，
// 并发时会拿到别的账号的 UA。华为云 WAF 按 UA 指纹校验，
// UA 不匹配直接回 404，伪装成「会话失效」—— 排查成本极高。
func userAgentIn(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return config.DefaultUA
	}
	var meta map[string]any
	if json.Unmarshal(data, &meta) != nil {
		return config.DefaultUA
	}
	if ua, ok := meta["user_agent"].(string); ok && ua != "" {
		return ua
	}
	return config.DefaultUA
}

func New(cookieFile string) (*Session, error) {
	if cookieFile == "" {
		cookieFile = config.CookieFile()
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{
		Proxy:             ieproxy.GetProxyFunc(),
		ForceAttemptHTTP2: false,
	}
	s := &Session{
		client:     &http.Client{Jar: jar, Timeout: 20 * time.Second, Transport: transport},
		jar:        jar,
		UA:         config.UserAgent(),
		CookieFile: cookieFile,
	}
	if err := s.loadCookies(cookieFile); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Session) loadCookies(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var list []cookieJSON
	if err := json.Unmarshal(data, &list); err != nil {
		return fmt.Errorf("cookie 文件解析失败: %w", err)
	}
	s.rawCookies = append(s.rawCookies, list...)
	for _, c := range list {
		if c.Name == "" || !strings.Contains(c.Domain, "huaweicloud.com") {
			continue
		}
		host := strings.TrimPrefix(c.Domain, ".")
		if host == "" {
			continue
		}
		u := &url.URL{Scheme: "https", Host: host}
		ck := &http.Cookie{Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path}
		if ck.Path == "" {
			ck.Path = "/"
		}
		// SetCookies 用带 Path 的 URL，否则 jar 会把 cookie 挂到 "/" 上，
		// /UnifiedIDMPortal 下的 JSESSIONID 就取不到了。
		u.Path = ck.Path
		if c.Expires > 0 {
			ck.Expires = time.Unix(int64(c.Expires), 0)
		}
		s.jar.SetCookies(u, []*http.Cookie{ck})
	}
	return nil
}

func (s *Session) ImportCookieString(raw string) int {
	count := 0
	u := &url.URL{Scheme: "https", Host: "huaweicloud.com"}
	for _, part := range strings.Split(raw, ";") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 || kv[0] == "" {
			continue
		}
		c := cookieJSON{Name: kv[0], Value: kv[1], Domain: ".huaweicloud.com", Path: "/"}
		s.jar.SetCookies(u, []*http.Cookie{{Name: c.Name, Value: c.Value, Domain: c.Domain, Path: "/"}})
		s.rawCookies = append(s.rawCookies, c)
		count++
	}
	return count
}

func (s *Session) SaveCookies() error {
	data, err := json.MarshalIndent(s.rawCookies, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(s.CookieFile, data, 0o600)
}

func (s *Session) bootstrapCSRF() error {
	s.csrf = ""
	req, err := http.NewRequest(http.MethodGet, config.DevData+config.API["csrf"], nil)
	if err != nil {
		return err
	}
	s.setCommonHeaders(req)
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode == http.StatusOK {
		if token := resp.Header.Get("csrf"); token != "" {
			s.csrf = token
		}
	}
	return nil
}

// CSRF 返回当前会话的 csrf 令牌，缺失时先拉取一次。
func (s *Session) CSRF() (string, error) {
	if s.csrf == "" {
		if err := s.bootstrapCSRF(); err != nil {
			return "", err
		}
	}
	return s.csrf, nil
}

// UserAgent 返回当前会话使用的 UA（登录时从浏览器读出并存入 meta.json）。
func (s *Session) UserAgent() string {
	return s.UA
}

// HTTPClient 暴露带 cookie jar 与系统代理的客户端，供其他域的接口复用会话。
func (s *Session) HTTPClient() *http.Client {
	return s.client
}

func (s *Session) setCommonHeaders(req *http.Request) {
	req.Header.Set("User-Agent", s.UA)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	req.Header.Set("Referer", config.GrowURL)
	req.Header.Set("Origin", "https://developer.huaweicloud.com")
}

func (s *Session) do(method, key string, body any) ([]byte, int, error) {
	var payload []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		payload = b
	}
	for attempt := 0; attempt < 2; attempt++ {
		if s.csrf == "" {
			_ = s.bootstrapCSRF()
		}
		var reader io.Reader
		if payload != nil {
			reader = bytes.NewReader(payload)
		}
		req, err := http.NewRequest(method, config.DevData+config.API[key], reader)
		if err != nil {
			return nil, 0, err
		}
		s.setCommonHeaders(req)
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if s.csrf != "" {
			req.Header.Set("csrf", s.csrf)
		}
		resp, err := s.client.Do(req)
		if err != nil {
			return nil, 0, err
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusForbidden && strings.Contains(string(data), "csrf") && attempt == 0 {
			s.csrf = ""
			continue
		}
		return data, resp.StatusCode, nil
	}
	return nil, 0, fmt.Errorf("请求失败: %s", key)
}

// CheckLogin 返回个人信息；未登录或会话过期返回 nil。
func (s *Session) CheckLogin() (map[string]any, error) {
	if err := s.bootstrapCSRF(); err != nil {
		return nil, err
	}
	if s.csrf == "" {
		return nil, nil
	}
	data, status, err := s.do(http.MethodGet, "personal", nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, nil
	}
	var info map[string]any
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, err
	}
	if code, _ := info["error_code"].(string); code != "" {
		return nil, nil
	}
	if v, _ := info["memAlias"].(string); v != "" {
		s.Username = v
	} else if v, _ := info["memName"].(string); v != "" {
		s.Username = v
	} else if v, _ := info["username"].(string); v != "" {
		s.Username = v
	}
	return info, nil
}

func (s *Session) RequireLogin() (map[string]any, error) {
	info, err := s.CheckLogin()
	if err != nil {
		return nil, err
	}
	if info == nil {
		return nil, fmt.Errorf("未登录或登录态已过期，请先运行 `hwcloud login`")
	}
	return info, nil
}

func (s *Session) SignStatus() (map[string]any, error) {
	data, status, err := s.do(http.MethodGet, "sign", nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("签到状态查询失败: HTTP %d %s", status, truncate(data))
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	if code, _ := result["error_code"].(string); code != "" {
		return nil, fmt.Errorf("签到状态查询失败: %v", result)
	}
	return result, nil
}

func (s *Session) Sign(account string) (map[string]any, error) {
	if account == "" {
		if _, err := s.RequireLogin(); err != nil {
			return nil, err
		}
		account = s.Username
	}
	data, status, err := s.do(http.MethodPost, "sign", map[string]string{"account_name": account})
	if err != nil {
		return nil, err
	}
	var result map[string]any
	if len(data) > 0 {
		_ = json.Unmarshal(data, &result)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("签到失败: HTTP %d %s", status, truncate(data))
	}
	if code, _ := result["error_code"].(string); code != "" {
		return nil, fmt.Errorf("签到失败: %v", result)
	}
	return result, nil
}

// AutoSign 今日未签到时签到；已签到直接返回。
func (s *Session) AutoSign() (bool, error) {
	if _, err := s.RequireLogin(); err != nil {
		return false, err
	}
	status, err := s.SignStatus()
	if err != nil {
		return false, err
	}
	if asFloat(status["sign_flag"]) == 1 {
		return true, nil
	}
	_, err = s.Sign(s.Username)
	return false, err
}

func (s *Session) Missions(pageNo, pageSize int, category string) ([]Mission, int, error) {
	if pageSize < 1 || pageSize > 100 {
		pageSize = 100
	}
	body := map[string]any{
		"page_no":      pageNo,
		"page_size":    pageSize,
		"category_key": category,
	}
	data, status, err := s.do(http.MethodPost, "missions", body)
	if err != nil {
		return nil, 0, err
	}
	if status != http.StatusOK {
		return nil, 0, fmt.Errorf("任务列表查询失败: HTTP %d %s", status, truncate(data))
	}
	var result missionResponse
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, 0, err
	}
	return result.List, result.Total, nil
}

func (s *Session) AllMissions() ([]Mission, error) {
	var all []Mission
	for page := 1; ; page++ {
		batch, total, err := s.Missions(page, 100, "DAILY_MISSIONS")
		if err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if len(batch) == 0 || len(all) >= total {
			break
		}
	}
	return all, nil
}

func (s *Session) Beans() int {
	n, _ := s.BeansE()
	return n
}

// BeansE 同 Beans，但区分「真的是 0 分」和「请求失败」。
//
// 面板的「积分」按钮要用这个：Beans() 失败时返回 0，用户会以为
// 积分清零了，而实际上只是网络抖了一下或会话过期。
func (s *Session) BeansE() (int, error) {
	data, status, err := s.do(http.MethodGet, "beans", nil)
	if err != nil {
		return 0, err
	}
	if status != http.StatusOK {
		return 0, fmt.Errorf("积分接口返回 HTTP %d", status)
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		return 0, fmt.Errorf("积分响应解析失败: %w", err)
	}
	v, ok := result["beans"]
	if !ok {
		return 0, fmt.Errorf("积分响应里没有 beans 字段: %s", truncate(data))
	}
	return int(asFloat(v)), nil
}

func asFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	default:
		return 0
	}
}

func truncate(data []byte) string {
	if len(data) > 200 {
		return string(data[:200])
	}
	return string(data)
}