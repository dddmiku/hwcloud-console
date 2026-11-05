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

	"hwcloud/internal/huawei"
)

// 鸿蒙开发实践中心的成长任务（harmonybp.developer.huaweicloud.com）。
//
// 这一组任务是纯协议可解的 —— 前端是个 Vue SPA，但它调的接口都在
// 同一个域下的 /api/growth/*，鉴权就是华为云会话 Cookie（和 edu 那套
// 是同一个 domainid 体系），所以不需要开浏览器。
//
// 接口清单（从 assets/LayoutComponent.vue_*.js 里还原，前端用的就是这些）：
//
//	GET  /api/growth/tasks                      任务列表（含 evaluator / need_seconds）
//	GET  /api/growth/tasks/<key>                任务详情
//	GET  /api/growth/bindings                   GitCode 绑定状态
//	POST /api/growth/tasks/<key>/sessions       开计时会话 → {session_id}
//	POST /api/growth/sessions/<sid>/heartbeat   心跳，返回新 session_id
//	POST /api/growth/tasks/<key>/submit         外部核验类任务提交
//
// 最容易踩的坑：**每次心跳服务端都会签发一个新的 session_id**，
// 前端拿到后会覆盖旧的（见 CaseDetailView 的 M() 函数）。
// 如果一直用最初那个 token 发心跳，服务端判为无效会话，
// accumulated_ms 会被清零 —— 表现是计时跑到 15 秒后突然归零，
// 永远到不了 30 秒。必须每一轮都用上一轮返回的 session_id。

const (
	harmonyBase    = "https://harmonybp.developer.huaweicloud.com"
	harmonyTaskKey = "case-view-daily"
	// 心跳间隔。前端是 5 秒（mr=5e3），服务端 max_gap_ms 是 15000，
	// 留足余量取 5 秒。
	harmonyBeatInterval = 5 * time.Second

	// errGitCodeExpired 是 GitCode 会话过期的错误前缀。
	//
	// GITCODE_ACCESS_TOKEN 是 exp - iat = 24h 的 JWT，每天定时跑必然过期。
	// 用固定前缀让调用方能用 strings.Contains 判定，而不是去匹配
	// 「HTTP 401」「NOT_LOGIN_ERROR」「420」这些会随上游文案变化的字符串。
	// 触发重登的代码在 web 层的 runDaily（那里持有账号锁，
	// 调 LoginGitCodeAuto 才会写到正确的账号目录）。
	errGitCodeExpired = "GITCODE_EXPIRED: "
)

// HarmonyTask HarmonyOS 实践中心任务执行结果
type HarmonyTask struct {
	TaskKey   string `json:"task_key"`
	Name      string `json:"name"`
	Points    int    `json:"points"`
	Done      bool   `json:"done"`
	ElapsedMs int    `json:"elapsed_ms"`
	Status    string `json:"status"`
	Err       string `json:"error,omitempty"`
}

// harmonyClient 用账号的 Cookie 访问鸿蒙实践中心。
type harmonyClient struct {
	cookie string
	ua     string
	http   *http.Client
}

func newHarmonyClient(sess *huawei.Session) (*harmonyClient, error) {
	if sess == nil {
		return nil, fmt.Errorf("会话为空")
	}
	ck := sess.CookieHeader()
	if strings.TrimSpace(ck) == "" {
		return nil, fmt.Errorf("账号 Cookie 为空，请先登录")
	}
	ua := sess.UA
	if ua == "" {
		ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
			"(KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36"
	}
	return &harmonyClient{
		cookie: ck,
		ua:     ua,
		http:   &http.Client{Timeout: 25 * time.Second},
	}, nil
}

func (c *harmonyClient) call(path, method string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, harmonyBase+path, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("User-Agent", c.ua)
	req.Header.Set("Cookie", c.cookie)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Referer", harmonyBase+"/")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	return resp.StatusCode, raw, err
}

// harmonyClip 截断响应体，避免把整页 HTML 塞进错误信息。
func harmonyClip(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}

type harmonyTaskBrief struct {
	TaskKey     string `json:"task_key"`
	Name        string `json:"name"`
	Evaluator   string `json:"evaluator"`
	Period      string `json:"period"`
	PointsHint  int    `json:"points_hint"`
	TodayStatus string `json:"today_status"`
	NeedSeconds int    `json:"need_seconds"`
}

// HarmonyTasks 列出鸿蒙实践中心的成长任务。
func HarmonyTasks(sess *huawei.Session) ([]HarmonyTask, error) {
	c, err := newHarmonyClient(sess)
	if err != nil {
		return nil, err
	}
	status, raw, err := c.call("/api/growth/tasks", http.MethodGet, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("任务列表 HTTP %d: %s", status, harmonyClip(string(raw)))
	}
	var resp struct {
		Tasks []harmonyTaskBrief `json:"tasks"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("解析任务列表失败: %v", err)
	}
	out := make([]HarmonyTask, 0, len(resp.Tasks))
	for _, t := range resp.Tasks {
		out = append(out, HarmonyTask{
			TaskKey: t.TaskKey,
			Name:    t.Name,
			Points:  t.PointsHint,
			Status:  t.TodayStatus,
			Done:    t.TodayStatus == "done",
		})
	}
	return out, nil
}

// harmonyCaseID 取任意一个已发布的案例 ID。
//
// dwell 类任务要求 case_id 是真实案例 —— 传 null 服务端不报错，
// 但会把它当成「没有具体案例」的会话，计时结果不计数。
func (c *harmonyClient) harmonyCaseID() (string, error) {
	status, raw, err := c.call("/api/cases?page_size=1", http.MethodGet, nil)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("案例列表 HTTP %d", status)
	}
	var resp struct {
		Cases []struct {
			ID string `json:"id"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", err
	}
	if len(resp.Cases) == 0 || resp.Cases[0].ID == "" {
		return "", fmt.Errorf("案例列表为空")
	}
	return resp.Cases[0].ID, nil
}

// RunHarmonyDwell 完成「每日浏览一个案例」。
//
// 流程：开 session → 每 5 秒心跳 → 累积到 need_seconds 服务端自己结算。
// 关键：每轮心跳返回的新 session_id 必须接上（见文件头说明）。
func RunHarmonyDwell(sess *huawei.Session, maxSeconds int) (map[string]any, error) {
	c, err := newHarmonyClient(sess)
	if err != nil {
		return nil, err
	}
	if maxSeconds <= 0 {
		maxSeconds = 120
	}

	// 已完成就直接返回，不用再跑一遍
	status, raw, err := c.call("/api/growth/tasks/"+harmonyTaskKey, http.MethodGet, nil)
	if err == nil && status == http.StatusOK {
		var t harmonyTaskBrief
		if json.Unmarshal(raw, &t) == nil {
			switch t.TodayStatus {
			case "done", "awarding", "award_failed", "failed_terminal":
				return map[string]any{
					"task_key": harmonyTaskKey,
					"already":  true,
					"status":   t.TodayStatus,
					"msg":      "今日已完成，无需重复",
				}, nil
			}
		}
	}

	caseID, err := c.harmonyCaseID()
	if err != nil {
		return nil, fmt.Errorf("取案例 ID 失败: %v", err)
	}

	status, raw, err = c.call("/api/growth/tasks/"+harmonyTaskKey+"/sessions",
		http.MethodPost, map[string]any{"case_id": caseID})
	if err != nil {
		return nil, err
	}
	if status == http.StatusConflict {
		return map[string]any{"task_key": harmonyTaskKey, "already": true, "msg": "本周期已完成"}, nil
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("开计时会话 HTTP %d: %s", status, harmonyClip(string(raw)))
	}
	var started struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(raw, &started); err != nil || started.SessionID == "" {
		return nil, fmt.Errorf("开计时会话返回异常: %s", harmonyClip(string(raw)))
	}

	sid := started.SessionID
	deadline := time.Now().Add(time.Duration(maxSeconds) * time.Second)
	var lastAcc int
	beats := 0

	for time.Now().Before(deadline) {
		status, raw, err = c.call("/api/growth/sessions/"+sid+"/heartbeat",
			http.MethodPost, map[string]any{"session_id": sid})
		if err != nil {
			return nil, err
		}
		if status == http.StatusUnauthorized {
			return nil, fmt.Errorf("登录态失效（401），请重新登录华为云")
		}
		if status == http.StatusNotFound || status == http.StatusConflict {
			return map[string]any{
				"task_key": harmonyTaskKey,
				"already":  true,
				"msg":      fmt.Sprintf("会话已结束（HTTP %d），可能已完成", status),
			}, nil
		}
		if status != http.StatusOK {
			return nil, fmt.Errorf("心跳 HTTP %d: %s", status, harmonyClip(string(raw)))
		}

		var beat struct {
			SessionID     string `json:"session_id"`
			AccumulatedMs int    `json:"accumulated_ms"`
			Completed     bool   `json:"completed"`
			AwardStatus   string `json:"award_status"`
			Points        int    `json:"points"`
		}
		if err := json.Unmarshal(raw, &beat); err != nil {
			return nil, fmt.Errorf("解析心跳失败: %v", err)
		}
		beats++
		lastAcc = beat.AccumulatedMs

		// 关键：跟着服务端签发的新 token 走，否则累积时间会被清零
		if beat.SessionID != "" {
			sid = beat.SessionID
		}

		if beat.Completed {
			return map[string]any{
				"task_key":     harmonyTaskKey,
				"done":         true,
				"elapsed_ms":   beat.AccumulatedMs,
				"award_status": beat.AwardStatus,
				"points":       beat.Points,
				"beats":        beats,
			}, nil
		}
		time.Sleep(harmonyBeatInterval)
	}

	return map[string]any{
		"task_key":   harmonyTaskKey,
		"done":       false,
		"elapsed_ms": lastAcc,
		"beats":      beats,
		"msg":        fmt.Sprintf("超过 %d 秒仍未完成（累积 %d ms）", maxSeconds, lastAcc),
	}, nil
}

// HarmonyStarStatus 查 GitCode Star 任务的状态（纯协议，只读）。
func HarmonyStarStatus(sess *huawei.Session) (map[string]any, error) {
	c, err := newHarmonyClient(sess)
	if err != nil {
		return nil, err
	}
	status, raw, err := c.call("/api/growth/tasks/gitcode-star", http.MethodGet, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", status, harmonyClip(string(raw)))
	}
	var resp struct {
		Task struct {
			TaskKey     string   `json:"task_key"`
			Name        string   `json:"name"`
			TodayStatus string   `json:"today_status"`
			GitBound    bool     `json:"git_bound"`
			GitLogin    string   `json:"git_login"`
			GitName     string   `json:"git_name"`
			Orgs        []string `json:"orgs"`
			Match       string   `json:"match"`
		} `json:"task"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	t := resp.Task
	return map[string]any{
		"task_key":     t.TaskKey,
		"name":         t.Name,
		"status":       t.TodayStatus,
		"git_bound":    t.GitBound,
		"git_login":    t.GitLogin,
		"git_name":     t.GitName,
		"orgs":         t.Orgs,
		"match":        t.Match,
		"need_binding": !t.GitBound,
	}, nil
}

// ---------- GitCode Star 任务（每日 +30）----------

// GitCode 的 star 接口是纯协议的：
//
//	POST https://gitcode.com/api/v2/projects/<id>/star
//
// 实测 200 + {"star_count": N}，星数立刻 +1，不需要点页面上的按钮。
// 这个接口只认 Cookie（GITCODE_ACCESS_TOKEN 那个 JWT 已过期时也能用，
// 会话 cookie 还有效就行）。
//
// 找仓库用：GET /api/v5/orgs/<org>/repos?page=1&per_page=100

const (
	gitcodeAPI   = "https://gitcode.com"
	gitcodeUA    = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/156.0.0.0 Safari/537.36"
	harmonyOrg = "harmony-practice-center"
)

// gitcodeRepo 组织仓库的简要信息
type gitcodeRepo struct {
	ID      int    `json:"id"`
	Name    string `json:"name"`
	HTMLURL string `json:"html_url"`
	Stars   int    `json:"stargazers_count"`
}

// gitcodeCookie 从 gitcode_state.json 里拼 Cookie 头。
func gitcodeCookie(stateDir string) (string, error) {
	raw, err := os.ReadFile(gitcodeStateFileIn(stateDir))
	if err != nil {
		return "", fmt.Errorf("未找到 GitCode 会话：%v", err)
	}
	var st struct {
		Cookies []struct {
			Name   string `json:"name"`
			Value  string `json:"value"`
			Domain string `json:"domain"`
		} `json:"cookies"`
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return "", fmt.Errorf("解析 GitCode 会话失败：%v", err)
	}
	var b strings.Builder
	for _, c := range st.Cookies {
		if !strings.Contains(c.Domain, "gitcode") {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("; ")
		}
		b.WriteString(c.Name)
		b.WriteByte('=')
		b.WriteString(c.Value)
	}
	if b.Len() == 0 {
		return "", fmt.Errorf("GitCode 会话里没有可用 Cookie")
	}
	return b.String(), nil
}

func gitcodeCall(cookie, path, method string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, gitcodeAPI+path, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("User-Agent", gitcodeUA)
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Referer", gitcodeAPI+"/")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	cli := &http.Client{Timeout: 25 * time.Second}
	resp, err := cli.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	return resp.StatusCode, raw, err
}

// listGitcodeRepos 列组织的仓库。
func listGitcodeRepos(cookie, org string) ([]gitcodeRepo, error) {
	st, raw, err := gitcodeCall(cookie,
		"/api/v5/orgs/"+org+"/repos?page=1&per_page=100", http.MethodGet, nil)
	if err != nil {
		return nil, err
	}
	if st != http.StatusOK {
		return nil, fmt.Errorf("仓库列表 HTTP %d: %s", st, harmonyClip(string(raw)))
	}
	var repos []gitcodeRepo
	if err := json.Unmarshal(raw, &repos); err != nil {
		return nil, fmt.Errorf("解析仓库列表失败: %v", err)
	}
	return repos, nil
}

// RunHarmonyStar 完成「Star 鸿蒙开发实践中心案例仓」(+30)。
//
// 三步，全部纯协议：
//  1. 从组织的仓库列表里挑一个还没 star 的（star 数最少的优先 ——
//     已 star 的仓库不会再让接口返回错误，只能靠计数变化判断，
//     所以直接挑星数最少的，通常就是没 star 过的）
//  2. POST /api/v2/projects/<id>/star
//  3. POST /api/growth/tasks/gitcode-star/submit {"payload":{"action":"check"}}
//     让华为云去核验 —— 只 star 不 check，任务状态一直是 todo。
//
// 第 3 步是最容易漏的：external_check 类任务不会自动轮询，
// 必须显式触发一次 check。action 传别的值都报 400 unknown_action。
func RunHarmonyStar(sess *huawei.Session, stateDir string) (map[string]any, error) {
	c, err := newHarmonyClient(sess)
	if err != nil {
		return nil, err
	}

	// 先看绑定状态，没绑定就别白忙
	if st, raw, err := c.call("/api/growth/tasks/gitcode-star", http.MethodGet, nil); err == nil && st == http.StatusOK {
		var resp struct {
			Task struct {
				TodayStatus string `json:"today_status"`
				GitBound    bool   `json:"git_bound"`
			} `json:"task"`
		}
		if json.Unmarshal(raw, &resp) == nil {
			switch resp.Task.TodayStatus {
			case "done", "awarding", "award_failed", "failed_terminal":
				return map[string]any{
					"task_key": "gitcode-star",
					"already":  true,
					"status":   resp.Task.TodayStatus,
					"msg":      "今日已完成，无需重复",
				}, nil
			}
			if !resp.Task.GitBound {
				return map[string]any{
					"task_key":     "gitcode-star",
					"need_binding": true,
					"msg":          "GitCode 未绑定到鸿蒙实践中心，请先运行 harmony-bind",
				}, nil
			}
		}
	}

	cookie, err := gitcodeCookie(stateDir)
	if err != nil {
		return nil, err
	}

	repos, err := listGitcodeRepos(cookie, harmonyOrg)
	if err != nil {
		// 401 / NOT_LOGIN_ERROR 说明 GitCode 会话过期。
		//
		// 这个 token 的有效期只有 24 小时左右（GITCODE_ACCESS_TOKEN 是
		// 一个 exp - iat = 24h 的 JWT），所以每天定时跑必然撞上。
		// 打成特定前缀，让调用方（runDaily）能识别并触发一次自动重登。
		//
		// 为什么不在这个函数里直接重登：
		//   LoginGitCodeAuto 走 config.GitCodeProfileDir()，那是进程级全局
		//   （见 hwcloud-account-context-leak）。RunHarmonyStar 是从无锁分支
		//   调进来的，全局账号没被设置，直接重登会写错账号的 profile。
		msg := err.Error()
		if strings.Contains(msg, "HTTP 401") || strings.Contains(msg, "NOT_LOGIN_ERROR") ||
			strings.Contains(msg, "420") {
			return nil, fmt.Errorf("%s%s", errGitCodeExpired, msg)
		}
		return nil, err
	}
	if len(repos) == 0 {
		return nil, fmt.Errorf("组织 %s 下没有仓库", harmonyOrg)
	}

	// 星数最少的最可能是没 star 过的
	target := repos[0]
	for _, r := range repos[1:] {
		if r.Stars < target.Stars {
			target = r
		}
	}

	before := target.Stars
	st, raw, err := gitcodeCall(cookie,
		fmt.Sprintf("/api/v2/projects/%d/star", target.ID), http.MethodPost, nil)
	if err != nil {
		return nil, err
	}
	if st != http.StatusOK && st != http.StatusCreated {
		// 401 也带过期前缀 —— 实测 token 过期时，仓库列表可能还能返回
		// （走的是未登录可见的公开数据），但 POST /star 会 401。
		// 只在 listGitcodeRepos 加判断会漏掉这种情况。
		if st == http.StatusUnauthorized {
			return nil, fmt.Errorf("%sstar 接口 HTTP %d: %s",
				errGitCodeExpired, st, harmonyClip(string(raw)))
		}
		return nil, fmt.Errorf("star 接口 HTTP %d: %s", st, harmonyClip(string(raw)))
	}
	var starResp struct {
		StarCount int `json:"star_count"`
	}
	_ = json.Unmarshal(raw, &starResp)

	out := map[string]any{
		"task_key":     "gitcode-star",
		"repo":         target.Name,
		"url":          target.HTMLURL,
		"stars_before": before,
		"stars_after":  starResp.StarCount,
		"star_ok":      starResp.StarCount > before,
	}

	// 关键一步：触发华为云核验。
	//
	// 为什么要重试：华为云侧要先去 GitCode 拉一次 star 状态才能判定。
	// 刚 star 完立刻 check，它拉到的是旧数据，返回
	// missing:["org:harmony-practice-center"] —— 实测 daily 里连着跑
	// 就撞上过（日志显示「核验未通过」，但隔一会儿手工重跑就 success）。
	// 所以按退避重试几次，直到 completed 或明确失败。
	//
	// 注意：check 是幂等的（只是触发一次核验），重复调用不会重复发分。
	var chk struct {
		Completed      bool   `json:"completed"`
		AlreadyAwarded bool   `json:"already_awarded_today"`
		AwardStatus    string `json:"award_status"`
		Points         any    `json:"points"`
		Progress       struct {
			Matched []string `json:"matched"`
			Missing []string `json:"missing"`
			Failed  []string `json:"failed"`
		} `json:"progress"`
	}

	// 退避序列：等华为云把 GitCode 的状态同步过来，总共约 45 秒。
	// 再长就没意义了 —— daily 是串行跑的，不能为一个任务卡太久。
	backoff := []time.Duration{0, 5 * time.Second, 10 * time.Second, 15 * time.Second, 15 * time.Second}
	attempts := 0

	for i, wait := range backoff {
		if wait > 0 {
			time.Sleep(wait)
		}
		attempts = i + 1

		st, raw, err = c.call("/api/growth/tasks/gitcode-star/submit", http.MethodPost,
			map[string]any{"case_id": nil, "payload": map[string]any{"action": "check"}})
		if err != nil {
			out["check_error"] = err.Error()
			continue
		}
		if st != http.StatusOK {
			out["check_status"] = fmt.Sprintf("HTTP %d", st)
			out["check_detail"] = harmonyClip(string(raw))
			continue
		}

		chk = struct {
			Completed      bool   `json:"completed"`
			AlreadyAwarded bool   `json:"already_awarded_today"`
			AwardStatus    string `json:"award_status"`
			Points         any    `json:"points"`
			Progress       struct {
				Matched []string `json:"matched"`
				Missing []string `json:"missing"`
				Failed  []string `json:"failed"`
			} `json:"progress"`
		}{}
		if json.Unmarshal(raw, &chk) != nil {
			out["check_raw"] = harmonyClip(string(raw))
			continue
		}
		out["completed"] = chk.Completed
		out["award_status"] = chk.AwardStatus
		out["points"] = chk.Points
		out["already_awarded"] = chk.AlreadyAwarded
		out["matched"] = chk.Progress.Matched
		out["missing"] = chk.Progress.Missing
		out["failed"] = chk.Progress.Failed
		out["check_attempts"] = attempts

		if chk.Completed {
			return out, nil
		}
		// 有 failed 说明是真的失败（不是同步延迟），重试也没用
		if len(chk.Progress.Failed) > 0 {
			return out, nil
		}
	}

	out["check_attempts"] = attempts
	return out, nil
}

// IsGitCodeExpired 判断错误是否为 GitCode 会话过期。
//
// 调用方（web 层的 runDaily）据此决定要不要触发一次自动重登。
// 用前缀判定而不是匹配上游文案：401 / NOT_LOGIN_ERROR / 420 都可能变，
// 但我们自己加的前缀不会变。
func IsGitCodeExpired(err error) bool {
	return err != nil && strings.Contains(err.Error(), errGitCodeExpired)
}
