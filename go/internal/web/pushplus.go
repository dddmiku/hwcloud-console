package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"hwcloud/internal/config"
)

// PushPlus 通知（https://www.pushplus.plus）。
//
// 用途：长任务跑完后把结果推到微信，不用一直盯着面板。
// 配置存 state/pushplus.json，token 由用户在面板里自己填，
// 不写进代码、不进 git。
//
// 为什么用 PushPlus 而不是企业微信/钉钉：
//   个人开发者场景，PushPlus 只要求一个 token，扫码就能拿，
//   不需要建群机器人或企业应用，接入成本最低。

// pushPlusConfig 是持久化到 state/pushplus.json 的结构。
type pushPlusConfig struct {
	Token string `json:"token"`
	// Enabled 控制是否真的发送。用户可能想保留 token 但临时关掉推送。
	Enabled bool `json:"enabled"`
	// Events 决定哪些事件触发推送。
	Events pushPlusEvents `json:"events"`
	// Topic 是 PushPlus 的群组编码，一对一推送留空即可。
	Topic string `json:"topic,omitempty"`
}

type pushPlusEvents struct {
	// TaskDone 长任务（日常/周常/批量）执行结束时推送。
	TaskDone bool `json:"task_done"`
	// TaskFail 单项任务失败时立即推送（不等整批跑完）。
	TaskFail bool `json:"task_fail"`
	// LoginExpired 账号会话失效且自动登录失败时推送。
	LoginExpired bool `json:"login_expired"`
}

// LF 是换行常量。用常量而不是直接写字面量，
// 避免外部脚本处理转义时把反斜杠吃掉（本项目踩过两次）。
const LF = string(rune(10))

var (
	pushCfgMu sync.RWMutex
	pushCfg   *pushPlusConfig
)

func pushPlusConfigPath() string {
	return filepath.Join(config.Root(), "state", "pushplus.json")
}

// defaultPushPlusConfig 返回默认配置。
//
// 默认 events 全开、enabled 开 —— 用户填了 token 就是想要推送，
// 不需要再点一次开关；想静默可以单独关。
func defaultPushPlusConfig() *pushPlusConfig {
	return &pushPlusConfig{
		Enabled: true,
		Events: pushPlusEvents{
			TaskDone:     true,
			TaskFail:     true,
			LoginExpired: true,
		},
	}
}

// loadPushPlusConfig 从磁盘读配置，读不到就返回默认值（不报错）。
func loadPushPlusConfig() *pushPlusConfig {
	cfg := defaultPushPlusConfig()
	raw, err := os.ReadFile(pushPlusConfigPath())
	if err != nil {
		return cfg
	}
	var onDisk pushPlusConfig
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		return cfg
	}
	// 字段级合并：老版本文件缺 events 时保留默认 true，
	// 而不是因为缺字段就全变 false。
	if onDisk.Token != "" || onDisk.Enabled {
		cfg.Token = onDisk.Token
		cfg.Enabled = onDisk.Enabled
		cfg.Topic = onDisk.Topic
		if onDisk.Events != (pushPlusEvents{}) {
			cfg.Events = onDisk.Events
		}
	}
	return cfg
}

// currentPushConfig 取当前配置（带缓存）。
func currentPushConfig() *pushPlusConfig {
	pushCfgMu.RLock()
	c := pushCfg
	pushCfgMu.RUnlock()
	if c != nil {
		return c
	}
	c = loadPushPlusConfig()
	pushCfgMu.Lock()
	pushCfg = c
	pushCfgMu.Unlock()
	return c
}

// savePushPlusConfig 写盘并刷新缓存。
func savePushPlusConfig(c *pushPlusConfig) error {
	dir := filepath.Join(config.Root(), "state")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	path := pushPlusConfigPath()
	// 单槽备份覆盖写，不按时间戳无限追加
	if old, err := os.ReadFile(path); err == nil {
		_ = os.WriteFile(path+".bak", old, 0o600)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return err
	}
	pushCfgMu.Lock()
	pushCfg = c
	pushCfgMu.Unlock()
	return nil
}

// PushPlusPayload 是 PushPlus 的发送请求体。
type PushPlusPayload struct {
	Token    string `json:"token"`
	Title    string `json:"title"`
	Content  string `json:"content"`
	Template string `json:"template,omitempty"` // html / txt / json / markdown
	Topic    string `json:"topic,omitempty"`
}

// PushPlusResponse 是 PushPlus 的响应。
type PushPlusResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data string `json:"data"`
}

// sendPushPlus 发一条推送。调用方负责判断是否要发。
//
// 设计要点：
//   - 5 秒超时：推送失败绝对不能拖慢任务本身
//   - 失败只记日志、向上返回错误，由调用方决定是否忽略
//   - 不重试：PushPlus 偶发 500 不代表 token 坏了，重试反而可能重复推送
func sendPushPlus(token, title, content, topic string) error {
	if strings.TrimSpace(token) == "" {
		return fmt.Errorf("pushplus token 为空")
	}
	payload := PushPlusPayload{
		Token:    token,
		Title:    title,
		Content:  content,
		Template: "txt",
		Topic:    topic,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost,
		"https://www.pushplus.plus/send", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("请求 pushplus 失败: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var pr PushPlusResponse
	if err := json.Unmarshal(raw, &pr); err != nil {
		return fmt.Errorf("pushplus 响应解析失败（HTTP %d）: %s", resp.StatusCode, clipStr(string(raw), 120))
	}
	if pr.Code != 200 {
		return fmt.Errorf("pushplus 返回错误 code=%d msg=%s", pr.Code, pr.Msg)
	}
	return nil
}

// notify 按事件类型决定是否推送。这是业务侧唯一入口。
//
// event 取值："task_done" / "task_fail" / "login_expired"
//
// 所有错误都只记日志 —— 推送失败绝不能影响任务结果。
func (s *Server) notify(event, title, content string) {
	cfg := currentPushConfig()
	if cfg == nil || !cfg.Enabled || strings.TrimSpace(cfg.Token) == "" {
		return
	}
	on := false
	switch event {
	case "task_done":
		on = cfg.Events.TaskDone
	case "task_fail":
		on = cfg.Events.TaskFail
	case "login_expired":
		on = cfg.Events.LoginExpired
	}
	if !on {
		return
	}
	if err := sendPushPlus(cfg.Token, title, content, cfg.Topic); err != nil {
		svcLog("[pushplus] 推送失败（%s）：%v", event, err)
		return
	}
	svcLog("[pushplus] 已推送（%s）：%s", event, title)
}

// ---- HTTP handlers ----

// handlePushPlusGet 返回当前配置。
//
// token 做掩码：面板本身要显示"已配置"，但没必要把完整 token
// 再吐回前端（浏览器历史、截图、日志都可能泄露）。
func (s *Server) handlePushPlusGet(w http.ResponseWriter, r *http.Request) {
	cfg := currentPushConfig()
	out := map[string]any{
		"enabled": cfg.Enabled,
		"topic":   cfg.Topic,
		"events":  cfg.Events,
		"has_token": strings.TrimSpace(cfg.Token) != "",
	}
	if cfg.Token != "" {
		out["token_masked"] = maskToken(cfg.Token)
	}
	writeJSON(w, out)
}

// handlePushPlusSave 保存配置。
//
// token 传空字符串表示"保持原值不变"——前端只显示掩码，
// 用户不改 token 时提交的自然是空的，不能因此把已存的 token 清掉。
// 要清空必须显式传 clear_token=true。
func (s *Server) handlePushPlusSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		Token      string `json:"token"`
		ClearToken bool   `json:"clear_token"`
		Enabled    *bool  `json:"enabled"`
		Topic      string `json:"topic"`
		Events     *struct {
			TaskDone     bool `json:"task_done"`
			TaskFail     bool `json:"task_fail"`
			LoginExpired bool `json:"login_expired"`
		} `json:"events"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}

	cfg := *currentPushConfig() // 拷贝，避免并发读写
	if req.ClearToken {
		cfg.Token = ""
	} else if t := strings.TrimSpace(req.Token); t != "" {
		cfg.Token = t
	}
	if req.Enabled != nil {
		cfg.Enabled = *req.Enabled
	}
	cfg.Topic = strings.TrimSpace(req.Topic)
	if req.Events != nil {
		cfg.Events = pushPlusEvents{
			TaskDone:     req.Events.TaskDone,
			TaskFail:     req.Events.TaskFail,
			LoginExpired: req.Events.LoginExpired,
		}
	}
	if err := savePushPlusConfig(&cfg); err != nil {
		writeJSON(w, map[string]any{"error": "保存失败: " + err.Error()})
		return
	}
	svcLog("[pushplus] 配置已更新（enabled=%v has_token=%v）", cfg.Enabled, cfg.Token != "")
	writeJSON(w, map[string]any{"ok": true})
}

// handlePushPlusTest 发一条测试推送，验证 token 可用。
func (s *Server) handlePushPlusTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	cfg := currentPushConfig()
	token := strings.TrimSpace(cfg.Token)
	if token == "" {
		writeJSON(w, map[string]any{"error": "还没配置 token"})
		return
	}
	title := "【hwcloud】测试推送"
	content := fmt.Sprintf("这是一条测试消息。\n\n发送时间：%s\n如果你收到这条消息，说明 PushPlus 配置正确。",
		time.Now().Format("2006-01-02 15:04:05"))
	if err := sendPushPlus(token, title, content, cfg.Topic); err != nil {
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	svcLog("[pushplus] 测试推送成功")
	writeJSON(w, map[string]any{"ok": true, "msg": "已发送，请查看微信"})
}

// maskToken 把 token 变成 abc1****ef23 的形状，够认出来但不可用。
func maskToken(t string) string {
	r := []rune(t)
	if len(r) <= 8 {
		return strings.Repeat("*", len(r))
	}
	return string(r[:4]) + strings.Repeat("*", len(r)-8) + string(r[len(r)-4:])
}
