package task

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"hwcloud/internal/huawei"
)

// AI 办公实践中心的行为上报走它自己的 JSONP 登录接口和埋点接口，
// 与 devdata 那套成长中心接口是两个不同的后端。
const (
	hohBase        = "https://hands-on-agent.developer.huaweicloud.com"
	hohLoginPath   = "/api/islogin?callback=checklogin"
	hohClickPath   = "/api/track/aishell_click"
	hohVisitPath   = "/api/track/visit"
	hohBindingPath = "/api/binding/status?hw_account_name="
)

type hohUser struct {
	Username string `json:"username"`
	DomainID string `json:"domainId"`
	UserID   string `json:"userId"`
}

// hohRequest 用会话 cookie 打实践中心接口。
func hohRequest(session *huawei.Session, method, path string, payload any) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, hohBase+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", session.UserAgent())
	req.Header.Set("Referer", hohBase+"/")
	req.Header.Set("Origin", hohBase)
	req.Header.Set("Accept", "*/*")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := session.HTTPClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("实践中心接口返回 HTTP %d: %s", resp.StatusCode, clipBody(data))
	}
	return data, nil
}

// hohLogin 读 JSONP 里的登录信息。
func hohLogin(session *huawei.Session) (*hohUser, error) {
	data, err := hohRequest(session, http.MethodGet, hohLoginPath, nil)
	if err != nil {
		return nil, err
	}
	text := string(data)
	start := strings.Index(text, "(")
	end := strings.LastIndex(text, ")")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("登录态解析失败: %s", clipBody(data))
	}
	var u hohUser
	if err := json.Unmarshal([]byte(text[start+1:end]), &u); err != nil {
		return nil, fmt.Errorf("登录态 JSON 解析失败: %w", err)
	}
	if u.Username == "" {
		return nil, fmt.Errorf("实践中心未登录，请确认 cookie 含 .huaweicloud.com 域")
	}
	return &u, nil
}

// HohStatus 返回实践中心的账号与 GitCode 绑定状态。
func HohStatus() (map[string]any, error) {
	session, err := huawei.New("")
	if err != nil {
		return nil, err
	}
	u, err := hohLogin(session)
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"username":  u.Username,
		"domain_id": u.DomainID,
		"bound":     false,
	}
	data, err := hohRequest(session, http.MethodGet, hohBindingPath+u.Username, nil)
	if err == nil {
		var bind map[string]any
		if json.Unmarshal(data, &bind) == nil {
			if b, ok := bind["bound"].(bool); ok {
				out["bound"] = b
			}
			out["binding"] = bind["binding"]
		}
	}
	return out, nil
}

// HohBind 把 GitCode 账号绑定到当前华为云账号。
//
// 为什么必须绑定：华为云判定「每周给代码仓提 Issue」是否完成，靠的是
// GitCode 账号 → 华为云账号的绑定关系。不绑定的话，Issue 发得再多，
// 任务页也一直显示「未完成」（3022 实测：Issue #983 已发出，任务仍为未完成）。
//
// 绑定接口只需要华为云会话 + GitCode 用户名，不需要 GitCode 登录态：
//
//	POST /api/binding/create
//	{"gitcode_username":"...","hw_account_name":"...","hw_domain_id":"...",
//	 "hw_user_id":"...","hw_project_id":"..."}
func HohBind(gitcodeUser string) (map[string]any, error) {
	session, err := huawei.New("")
	if err != nil {
		return nil, err
	}
	u, err := hohLogin(session)
	if err != nil {
		return nil, err
	}
	payload := map[string]string{
		"gitcode_username": gitcodeUser,
		"hw_account_name":  u.Username,
		"hw_domain_id":     u.DomainID,
		"hw_user_id":       u.UserID,
		"hw_project_id":    "",
	}
	data, err := hohRequest(session, http.MethodPost, "/api/binding/create", payload)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"gitcode_username": gitcodeUser, "hw_account_name": u.Username}
	var resp map[string]any
	if json.Unmarshal(data, &resp) == nil {
		out["response"] = resp
	}
	// 复核：绑定接口返回成功不代表真的写进去了，再查一次状态
	if st, err := HohStatus(); err == nil {
		out["bound"] = st["bound"]
		out["binding"] = st["binding"]
	}
	return out, nil
}

// TrackDevSpace 上报「体验 AI DevSpace」行为。
func TrackDevSpace() (map[string]any, error) {
	session, err := huawei.New("")
	if err != nil {
		return nil, err
	}
	u, err := hohLogin(session)
	if err != nil {
		return nil, err
	}
	payload := map[string]string{
		"hw_account_name": u.Username,
		"hw_domain_id":    u.DomainID,
	}
	out := map[string]any{"account": u.Username}
	for name, path := range map[string]string{"click": hohClickPath, "visit": hohVisitPath} {
		data, err := hohRequest(session, http.MethodPost, path, payload)
		if err != nil {
			out[name] = "err: " + err.Error()
			continue
		}
		out[name] = strings.TrimSpace(string(data))
	}
	return out, nil
}
