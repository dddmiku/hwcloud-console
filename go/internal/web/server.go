package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"hwcloud/internal/account"
	"hwcloud/internal/config"
	"hwcloud/internal/fsutil"
	"hwcloud/internal/huawei"
	"hwcloud/internal/task"
	"hwcloud/internal/textutil"
)

// Server 是管理面板的 HTTP 服务。
type Server struct {
	mu      sync.Mutex
	store   *account.Store
	auth    *auth
	addr    string
	pending map[string]pendingAccount // 已验密码但还没登录成功的账号

	// 自动账密登录的并发控制。
	// loginMu 串行化整次登录：Camoufox 是单实例服务，同账号并发登录会互相踢掉。
	// loginFailAt 记录每个账号上次失败时间，用于 3 分钟冷却，避免批量操作把
	// 登录服务打爆（每次登录要开浏览器过滑块，约 30-60 秒）。
	loginMu     sync.Mutex
	loginFailMu sync.Mutex
	loginFailAt map[string]time.Time

	// 积分缓存：前端每 45 秒轮询账号列表，积分变得很慢，
	// 每次都查远程接口会把列表页拖到超时。缓存 60 秒。
	beansMu    sync.Mutex
	beansCache map[string]beanEntry

	// 账号探测结果缓存 + 后台刷新单飞标志。
	//
	// 为什么需要：探测要过 withAccount（全局 s.mu），长任务持锁时
	// 列表接口会被拖死（前端停在「加载中…」）。改成读缓存秒回、
	// 后台异步刷新，面板就不再受长任务影响。
	probeMu      sync.Mutex
	probeCache   map[string]probeSnapshot
	probeRunning bool

	// 长任务（日常/周常/实验）的异步执行状态。
	//
	// 为什么要异步：这些任务要开浏览器、跑几分钟，同步 HTTP 请求会
	// 超过浏览器/网关超时，用户看到的是「点了没反应」。改成请求立即
	// 返回 job id，后台 goroutine 跑，前端轮询 /api/jobs 拿进度。
	jobMu sync.Mutex
	jobs  map[string]*jobState
}

// jobState 是一次长任务执行的状态。
type jobState struct {
	ID      string         `json:"id"`
	Account string         `json:"account"`
	Name    string         `json:"name"`
	Label   string         `json:"label"`
	Running bool           `json:"running"`
	Step    string         `json:"step"`      // 当前在做什么
	Result  map[string]any `json:"result"`    // 最终结果
	Error   string         `json:"error"`
	Started time.Time      `json:"started"`
	Ended   time.Time      `json:"ended"`
}

// beanEntry 是积分缓存项。
type beanEntry struct {
	val int
	at  time.Time
}

// pendingAccount 是「待定账号」：密码已验证通过，但登录流程还没走完。
// 只有登录成功后才真正写进 store，失败就丢掉，列表里不会出现。
type pendingAccount struct {
	Phone    string
	Password string
	Username string
	Created  time.Time
}

// NewServer 创建面板服务。
func NewServer(addr string) (*Server, error) {
	store, err := account.NewStore()
	if err != nil {
		return nil, err
	}
	return &Server{
		store:       store,
		auth:        newAuth(),
		addr:        addr,
		pending:     map[string]pendingAccount{},
		loginFailAt: map[string]time.Time{},
		beansCache:  map[string]beanEntry{},
		probeCache:  map[string]probeSnapshot{},
		jobs:        map[string]*jobState{},
	}, nil
}

// pendingGet 取待定账号。
func (s *Server) pendingGet(id string) (pendingAccount, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pending[id]
	if !ok {
		return p, false
	}
	if time.Since(p.Created) > 30*time.Minute {
		delete(s.pending, id)
		return p, false
	}
	return p, true
}

// pendingSet 记录待定账号。
func (s *Server) pendingSet(id string, p pendingAccount) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending[id] = p
}

// pendingDrop 丢弃待定账号。
func (s *Server) pendingDrop(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pending, id)
}

// Start 启动 HTTP 服务。
// svcLog 往 logs/service.log 追加一行，同时打到 stdout。
//
// 为什么需要它：面板的「service」日志 tab 读的是这个文件，但早先只有
// 外部脚本往里写，面板进程自己从不写 —— 文件一直停在旧时间戳，
// 用户点开日志看到的是几小时前的内容，会以为服务没在工作。
// 这里把启动、登录、任务执行这些关键事件都记下来。
func svcLog(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	line := fmt.Sprintf("[%s] %s\n", time.Now().Format("2006-01-02 15:04:05"), msg)
	fmt.Print(line)

	dir := filepath.Join(config.Root(), "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	p := filepath.Join(dir, "service.log")
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(line)

	// 只留最近 2000 行，避免无限增长
	if st, err := f.Stat(); err == nil && st.Size() > 512*1024 {
		if data, err := os.ReadFile(p); err == nil {
			lines := strings.Split(string(data), "\n")
			if len(lines) > 2000 {
				trimmed := strings.Join(lines[len(lines)-2000:], "\n")
				_ = fsutil.WriteFileAtomic(p, []byte(trimmed), 0o644)
			}
		}
	}
}

func (s *Server) Start() error {
	mux := http.NewServeMux()

	// 公开接口
	//
	// 根路径用 "/{$}" 而不是 "/"：Go 的 ServeMux 里 "/" 会匹配**所有**
	// 未命中更具体模式的路由，导致 GET /api/typo 返回面板 HTML + 200，
	// 前端拼错路径不会报错，监控也区分不出 404。
	mux.HandleFunc("/{$}", s.handleIndex)
	// 静态资源（CSS / JS）从 go:embed 提供，不需要鉴权（不含敏感数据）
	mux.Handle("/static/", staticHandler())
	mux.HandleFunc("/api/auth/state", s.handleAuthState)
	mux.HandleFunc("/api/auth/setup", s.handleAuthSetup)
	mux.HandleFunc("/api/auth/login", s.handleAuthLogin)
	mux.HandleFunc("/api/auth/logout", s.handleAuthLogout)
	// 验证码 SDK 是公开静态资源，前端 <script src> 无法携带鉴权头
	mux.HandleFunc("/api/accounts/csc/sdk.js", s.handleCscSDK)

	// 受保护接口
	guard := s.auth.requireAuth
	mux.HandleFunc("/api/auth/users", guard(s.handleAuthUsers))
	mux.HandleFunc("/api/accounts", guard(s.handleAccounts))
	mux.HandleFunc("/api/accounts/add", guard(s.handleAddAccount))
	mux.HandleFunc("/api/accounts/remove", guard(s.handleRemoveAccount))
	mux.HandleFunc("/api/accounts/remark", guard(s.handleAccountRemark))
	mux.HandleFunc("/api/accounts/check", guard(s.handleCheckAccount))
	mux.HandleFunc("/api/accounts/login", guard(s.handleAccountLogin))
	mux.HandleFunc("/api/accounts/captcha", guard(s.handleCaptcha))
	mux.HandleFunc("/api/accounts/captcha/image", guard(s.handleCaptchaImage))
	mux.HandleFunc("/api/accounts/captcha/submit", guard(s.handleCaptchaSubmit))
	mux.HandleFunc("/api/accounts/captcha/auto", guard(s.handleCaptchaAuto))
	mux.HandleFunc("/api/accounts/csc", guard(s.handleCsc))
	mux.HandleFunc("/api/accounts/csc/submit", guard(s.handleCscSubmit))
	mux.HandleFunc("/api/accounts/csc/sms", guard(s.handleCscSMS))
	mux.HandleFunc("/api/accounts/camoufox/health", guard(s.handleCamoufoxHealth))
	mux.HandleFunc("/api/accounts/camoufox/login", guard(s.handleCamoufoxLogin))
	mux.HandleFunc("/api/accounts/camoufox/logout", guard(s.handleCamoufoxLogout))
	mux.HandleFunc("/api/accounts/gitcode", guard(s.handleGitCodeCreds))
	mux.HandleFunc("/api/accounts/gitcode/bindstate", guard(s.handleGitCodeBindState))
	mux.HandleFunc("/api/accounts/run", guard(s.handleRunTask))
	mux.HandleFunc("/api/vnc", guard(s.handleVNCInfo))
	// noVNC 反向代理：浏览器直接打开 /vnc/ 就能操作服务器桌面。
	//
	// 为什么走面板代理而不是直接暴露 6080：
	//   websockify 只监听 127.0.0.1，外网够不着；而且它自己不带鉴权。
	//   挂在面板下就复用了面板的登录态 —— 没登录的人打不开 VNC。
	//
	// 注意：WebSocket 升级请求也要过这层，所以用 httputil.ReverseProxy，
	// 它原生支持 Upgrade。
	mux.Handle("/vnc/", guard(s.handleNoVNC))
	// noVNC 的 WebSocket 端点写死是根路径 /websockify（见 ui.js:
	// initSetting('path','websockify')，它是相对 **站点根** 解析的，
	// 不是相对当前页面）。页面挂在 /vnc/ 下时，它仍然去连
	// ws://<host>/websockify，所以根路径必须也代理一份。
	mux.Handle("/websockify", guard(s.handleNoVNC))
	mux.HandleFunc("/api/jobs", guard(s.handleJobs))
	mux.HandleFunc("/api/timers", guard(s.handleTimers))
	mux.HandleFunc("/api/logs", guard(s.handleLogs))
	mux.HandleFunc("/api/system", guard(s.handleSystem))

	// API 前缀兜底：未注册的 /api/* 一律 404 JSON，而不是落到根路由返回 HTML。
	// 放在所有具体路由之后注册，ServeMux 会优先匹配更具体的模式。
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeJSONStatus(w, http.StatusNotFound, map[string]any{
			"error": "接口不存在: " + r.URL.Path,
		})
	})

	svcLog("服务启动，监听 %s", s.addr)

	// 用 http.Server 而不是裸 ListenAndServe：裸调用没有任何超时，
	// 慢连接（Slowloris）可以无限占用 goroutine，而这里每个请求背后
	// 都可能是一次开浏览器的重任务。
	//
	// WriteTimeout 设 0（不限）：/api/accounts/run 的同步任务要 30-90 秒，
	// 设死会直接掐断正在等待的任务响应。慢客户端风险由 ReadHeaderTimeout
	// 和 IdleTimeout 兜住。
	srv := &http.Server{
		Addr:              s.addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      0,
		IdleTimeout:       120 * time.Second,
	}

	// 优雅关闭：收到 SIGTERM 后先停止接新请求，给在跑的任务 30 秒收尾。
	// 没有这段的话，systemd restart 会在任务写状态文件的中途把进程砍掉，
	// 留下半截 JSON（现在虽然都改成原子写了，但任务本身会白跑）。
	ctx, stop := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		svcLog("收到退出信号，开始优雅关闭…")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			svcLog("优雅关闭超时，强制退出: %v", err)
			return err
		}
		svcLog("已关闭")
		return nil
	}
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// 首页随二进制发布，每次都要拿最新的（改完重启即生效）
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(indexHTMLBytes)
}

// sameOrigin 校验请求来自同源页面。
//
// 用于所有会改变状态的接口，作为 SameSite=Lax 之外的第二道防线：
// Lax 只拦跨站子资源请求，挡不住顶级导航（点链接、window.open）。
//
// 规则：
//   - 有 Origin 头：必须与 Host 同源
//   - 没有 Origin（老浏览器、curl）：看 Referer，有则校验
//   - 两者都没有：放行（本地 curl / 脚本调试，无 CSRF 风险）
func sameOrigin(r *http.Request) bool {
	check := func(u string) bool {
		if u == "" {
			return true
		}
		pu, err := url.Parse(u)
		if err != nil {
			return false
		}
		return pu.Host == r.Host
	}
	if o := r.Header.Get("Origin"); o != "" && o != "null" {
		return check(o)
	}
	if ref := r.Header.Get("Referer"); ref != "" {
		return check(ref)
	}
	return true
}

// writeJSON 输出 JSON 响应。
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}

// writeJSONStatus 带状态码的 JSON 响应。
//
// 面板长期用 200 + {error:...} 表达一切错误，导致 401/403/404 语义全丢，
// 前端只能靠 body 猜、监控也无法区分成功与失败。新代码用这个函数，
// 让认证失败能真正返回 401 —— 前端 api() 才能统一拦截并弹登录框。
func writeJSONStatus(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// withAccount 在指定账号的上下文中执行 fn。
func (s *Server) withAccount(id string, fn func() any) any {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 保存并恢复上一个账号上下文，而不是无条件清空。
	//
	// 原来 defer 里写死 SetAccount("")：如果调用方本来就在某个账号的
	// 上下文里（嵌套调用），退出时会把外层上下文也清掉，后续代码就
	// 拿不到账号了。用 save/restore 语义更安全。
	prev := config.AccountID()
	config.SetAccount(id)
	defer config.SetAccount(prev)

	return fn()
}

// withAccountTry 尝试拿账号锁，拿不到就立刻返回 false。
//
// 用途：只读探测（会话状态、积分）不应该为了等一把被长任务持有的锁
// 而无限期阻塞 —— 那会让面板列表页一直转圈。拿不到锁时调用方应该
// 退回缓存值。
//
// 注意不能用「带超时的 Lock」替代：Go 的 sync.Mutex 没有 TryLockContext，
// 而 TryLock 在长任务持锁期间会一直失败，正好符合「读缓存」的语义。
func (s *Server) withAccountTry(id string, fn func() any) bool {
	if !s.mu.TryLock() {
		return false
	}
	defer s.mu.Unlock()

	prev := config.AccountID()
	config.SetAccount(id)
	defer config.SetAccount(prev)

	fn()
	return true
}

// handleAccounts 返回所有账号及其状态。
func (s *Server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	list := s.store.List()

	// 先用缓存/静态数据秒回，探测放后台。
	//
	// 原来这里是同步探测的：对每个账号调 probeAccountFull，而它内部走
	// withAccount（全局 s.mu）。问题是有长任务在跑时（保活、周常、云实验
	// 都要持锁几十秒到几分钟），列表请求会一直排队，前端永远停在
	// 「加载中…」，账号数还是 0 —— 看起来像面板挂了。
	//
	// 现在改成：
	//   - 立刻用上次探测结果（缓存）组装响应
	//   - 同时起一个后台 goroutine 去刷新，前端下次轮询就能看到新值
	//   - 同一时刻只允许一个刷新在跑，避免任务堆积
	beansOf := make(map[string]int, len(list))
	for i := range list {
		a := &list[i]
		if snap, ok := s.cachedProbe(a.ID); ok {
			a.Alive = snap.Alive
			if snap.Username != "" {
				a.Username = snap.Username
			}
			a.RemainDays = snap.Remain
			a.LastCheck = snap.At.Format("2006-01-02 15:04:05")
			beansOf[a.ID] = snap.Beans
		} else {
			// 没有缓存时用磁盘上的旧值，别让字段是空的
			beansOf[a.ID] = -1
		}
	}
	s.refreshProbesAsync()

	// 返回完整手机号，方便直接核对与复制
	out := make([]map[string]any, 0, len(list))
	for _, a := range list {
		out = append(out, map[string]any{
			"id":            a.ID,
			"phone":         a.Phone,
			"remark":        a.Remark,
			"trusted":       a.Trusted,
			"trusted_at":    a.TrustedAt,
			"alive":         a.Alive,
			"username":      a.Username,
			"remain_days":   a.RemainDays,
			"last_check":    a.LastCheck,
			"last_error":    a.LastError,
			"last_sign":     a.LastSign,
			"last_devspace": a.LastDevSpace,
			"last_blog":     a.LastBlog,
			// 只回用户名，密码不回前端
			"gitcode_user": a.GitCodeUser,
			// 有没有存密码 —— 前端据此决定「重新登录」能不能点。
			// 只回布尔值，绝不回密码本身。
			"password_saved": a.Password != "",
			// 绑定状态：bound / unbound / error / mismatch / ""（未查询）
			// 前端必须把 unbound 显眼地标出来 —— 没绑定时 Issue 白提
			"gitcode_bind_state": a.GitCodeBindState,
			"gitcode_bind_msg":   a.GitCodeBindMsg,
			"gitcode_bind_at":    a.GitCodeBindAt,
			// 积分：-1 表示取不到（离线或接口失败），前端显示 —
			"beans": beansOf[a.ID],
		})
	}
	writeJSON(w, map[string]any{"accounts": out})
}

// probeSnapshot 是一次探测的结果快照。
type probeSnapshot struct {
	Alive    bool
	Username string
	Remain   float64
	Beans    int
	At       time.Time
}

// cachedProbe 读上次探测结果。
//
// 超过 5 分钟的快照视为不可信，不再返回 —— 否则后台刷新因为长任务占锁
// 一直失败时，界面会拿着几小时前的「在线」当真，用户以为账号好好的。
// 返回 false 让调用方显示「未知」，比显示一个错的「在线」好。
func (s *Server) cachedProbe(id string) (probeSnapshot, bool) {
	s.probeMu.Lock()
	defer s.probeMu.Unlock()
	snap, ok := s.probeCache[id]
	if !ok {
		return snap, false
	}
	if time.Since(snap.At) > 5*time.Minute {
		return snap, false
	}
	return snap, true
}

// refreshProbesAsync 起一个后台 goroutine 刷新所有账号的探测结果。
//
// 用 probeRunning 做单飞：面板每 3-45 秒轮询一次，如果每次都起新
// goroutine，长任务期间会堆积几十个等待 withAccount 的协程。
func (s *Server) refreshProbesAsync() {
	s.probeMu.Lock()
	if s.probeRunning {
		s.probeMu.Unlock()
		return
	}
	s.probeRunning = true
	s.probeMu.Unlock()

	go func() {
		defer func() {
			s.probeMu.Lock()
			s.probeRunning = false
			s.probeMu.Unlock()
		}()
		for _, a := range s.store.List() {
			alive, username, remain, beans := s.probeAccountFull(a.ID)
			s.probeMu.Lock()
			s.probeCache[a.ID] = probeSnapshot{
				Alive: alive, Username: username, Remain: remain,
				Beans: beans, At: time.Now(),
			}
			s.probeMu.Unlock()
		}
	}()
}

// probeAccount 检查某账号的会话状态。
func (s *Server) probeAccount(id string) (bool, string, float64) {
	alive, username, remain, _ := s.probeAccountFull(id)
	return alive, username, remain
}

// probeAccountTry 同 probeAccount，但拿不到锁时直接返回 false，不阻塞。
//
// 专供可能已在 s.mu 内部执行的调用方使用（如 handleRunTask 的 keepalive
// 分支 → autoLogin）。Go 的 sync.Mutex 不可重入，用 withAccount 会自锁死。
func (s *Server) probeAccountTry(id string) bool {
	var alive bool
	ok := s.withAccountTry(id, func() any {
		info, err := task.SessionInfo()
		if err != nil {
			return nil
		}
		if v, ok := info["alive"].(bool); ok {
			alive = v
		}
		return nil
	})
	if !ok {
		// 拿不到锁说明有任务在跑，那就认为「不是我们该管的」，
		// 让调用方继续走登录流程（登录本身不需要这把锁）。
		return false
	}
	return alive
}

// probeAccountFull 同 probeAccount，额外返回积分（beans）。
//
// 积分单独取：它在 /growth/bonus-beans，和会话检查不是同一个接口。
// 账号离线时接口会失败，这里返回 -1 表示「取不到」，前端显示成 —，
// 不要显示成 0（0 会被误读成「积分清零了」）。
//
// 积分有 60 秒缓存：handleAccounts 会被前端每 45 秒轮询一次，
// 每次都查一遍远程接口会让列表页明显变慢，而积分本来就变得很慢。
func (s *Server) probeAccountFull(id string) (bool, string, float64, int) {
	var alive bool
	var username string
	var remain float64
	beans := -1

	// 积分缓存：命中就不用再去远程查。
	//
	// 注意这里必须在拿账号锁**之前**读缓存，且只在同一把锁内使用缓存值 ——
	// 见下面「为什么必须合并成一次 withAccountTry」的说明。
	beansCached, beansHit := 0, false
	s.beansMu.Lock()
	if e, ok := s.beansCache[id]; ok && time.Since(e.at) < 60*time.Second {
		beansCached, beansHit = e.val, true
	}
	s.beansMu.Unlock()

	// 用 TryLock：长任务（保活 / 周常 / 云实验）持锁期间拿不到锁，
	// 直接退回上次缓存，而不是把请求排到队列里干等。
	//
	// 【必须一次拿锁做完会话检查 + 取积分】
	//
	// 这里原来写的是两次独立的 withAccountTry：
	//   第一次：SessionInfo()（读 cookies.json 判会话）
	//   第二次：huawei.New("").Beans()（读同一份 cookies 取积分）
	// 两次之间锁会释放，而 config.SetAccount 是**进程级全局变量**。
	// 只要中间有别的 goroutine 拿了锁并把 accountID 改成 B，
	// 第二次就会读 B 的 cookies，把 B 的积分记到 A 头上。
	//
	// 实测症状（2026-11-02）：面板上 4 个账号里有两个显示同一个
	// 用户名 yd_260581878、同一份积分 430 —— 那就是串号。
	// 用户一眼就看出「直接混乱了都」。
	//
	// 会话检查和积分都依赖 config.CookieFile()（= 全局 accountID），
	// 所以它们必须在**同一次**持锁期间完成，不能拆开。
	ok := s.withAccountTry(id, func() any {
		info, err := task.SessionInfo()
		if err != nil {
			return nil
		}
		if v, ok := info["alive"].(bool); ok {
			alive = v
		}
		if v, ok := info["username"].(string); ok {
			username = v
		}
		if v, ok := info["auth_remaining_days"].(float64); ok {
			remain = v
		}
		if !alive {
			return nil
		}
		if beansHit {
			beans = beansCached
			return nil
		}
		if sess, err := huawei.New(""); err == nil {
			beans = sess.Beans()
		}
		return nil
	})
	if !ok {
		// 锁被占：返回缓存快照，别阻塞调用方
		if snap, hit := s.cachedProbe(id); hit {
			return snap.Alive, snap.Username, snap.Remain, snap.Beans
		}
		return false, "", 0, -1
	}

	if beans >= 0 && !beansHit {
		s.beansMu.Lock()
		s.beansCache[id] = beanEntry{val: beans, at: time.Now()}
		s.beansMu.Unlock()
	}
	return alive, username, remain, beans
}

type addReq struct {
	Phone    string `json:"phone"`
	Password string `json:"password"`
}

// handleAddAccount 校验账号密码，通过后记为「待定账号」。
//
// 注意：这里不写 store，账号不会出现在列表里。
// 只有随后的登录流程（滑块/短信）成功，才会真正入库。
func (s *Server) handleAddAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req addReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	// 清洗输入：从网页/微信/密码管理器复制时常带进不可见字符
	// （NBSP、零宽空格、BOM、全角空格），在输入框里看不见，
	// 但会让「密码正确」变成「账号或密码错误」。
	req.Phone = textutil.CleanPhone(req.Phone)
	req.Password = textutil.CleanPassword(req.Password)
	if req.Phone == "" || req.Password == "" {
		writeJSON(w, map[string]any{"error": "手机号和密码都不能为空"})
		return
	}

	// 只建目录（登录过程要往 accounts/<phone>/state 写文件），不入库
	id, err := s.store.PrepareDir(req.Phone)
	if err != nil {
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}

	// 验密码
	verify := s.withAccount(id, func() any {
		res, err := task.VerifyPassword(req.Phone, req.Password)
		if err != nil {
			return map[string]any{"ok": false, "message": err.Error()}
		}
		if res.ErrorCode != "" {
			return map[string]any{"ok": false, "message": res.Message,
				"error_code": res.ErrorCode, "error_desc": res.ErrorDesc}
		}
		return map[string]any{"ok": true, "message": "密码正确", "username": res.Username}
	})

	vm, _ := verify.(map[string]any)
	if ok, _ := vm["ok"].(bool); !ok {
		msg := "密码校验未通过"
		if m, ok := vm["message"].(string); ok && m != "" {
			msg = m
		}
		writeJSON(w, map[string]any{
			"ok":     false,
			"error":  msg,
			"verify": verify,
		})
		return
	}

	username, _ := vm["username"].(string)

	// 记为待定，等登录成功再入库
	s.pendingSet(id, pendingAccount{
		Phone:    req.Phone,
		Password: req.Password,
		Username: username,
		Created:  time.Now(),
	})

	writeJSON(w, map[string]any{
		"ok": true,
		"pending": true,
		"account": map[string]any{
			"id": id, "phone": req.Phone, "trusted": false,
		},
		"verify": verify,
	})
}

type idReq struct {
	ID string `json:"id"`
}

// handleRemoveAccount 删除账号。
func (s *Server) handleRemoveAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req idReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if err := s.store.Remove(req.ID); err != nil {
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// isPhoneLike 判断一个字符串看起来是不是手机号。
//
// 用途：GitCode 允许用手机号登录，用户很容易把手机号当成用户名填进来。
// 绑定接口要的是用户名，传手机号会 404。
// 这里提前拦下，给出明确指引，而不是让用户对着
// 「gitcode user not found, username: 130xxxxxxxx」猜。
func isPhoneLike(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) != 11 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	// 中国大陆手机号以 1 开头
	return s[0] == '1'
}

// handleAccountRemark 保存账号备注。
//
// 为什么单独一个接口而不是塞进现有的编辑流程：
// 备注是高频小操作（用户随时想改个标签），不该为了改一行字
// 触发会话校验、目录操作这些重逻辑。
//
// 请求：{"id":"账号ID","remark":"主号 / 测试 / 项目A"}
// 备注允许为空（等于清除）。
func (s *Server) handleAccountRemark(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		ID     string `json:"id"`
		Remark string `json:"remark"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if req.ID == "" {
		writeJSON(w, map[string]any{"error": "缺少 id"})
		return
	}
	if _, ok := s.store.Get(req.ID); !ok {
		writeJSON(w, map[string]any{"error": "账号不存在"})
		return
	}

	// 清洗 + 限长。备注是展示用的，太长会把卡片撑坏。
	remark := textutil.CleanUsername(req.Remark)
	const maxLen = 40
	if rs := []rune(remark); len(rs) > maxLen {
		remark = string(rs[:maxLen])
	}

	if err := s.store.Update(req.ID, func(a *account.Account) {
		a.Remark = remark
	}); err != nil {
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "remark": remark})
}

// handleCheckAccount 检查单个账号。
func (s *Server) handleCheckAccount(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "missing id", 400)
		return
	}

	// 用 TryLock 版本，拿不到锁就返回缓存并标记 stale。
	//
	// 原来用阻塞版 probeAccount：有长任务持锁时（保活 3 分钟、周常、
	// 云实验），「检查」按钮会一直转圈，用户以为没反应。更糟的是
	// 探测拿不到锁时会回退成 alive=false，界面显示「离线」——
	// 而账号其实好好的，只是锁被占用。这是把「锁竞争」误报成「账号离线」。
	var alive bool
	var username string
	var remain float64
	locked := s.withAccountTry(id, func() any {
		info, err := task.SessionInfo()
		if err != nil {
			return nil
		}
		if v, ok := info["alive"].(bool); ok {
			alive = v
		}
		if v, ok := info["username"].(string); ok {
			username = v
		}
		if v, ok := info["auth_remaining_days"].(float64); ok {
			remain = v
		}
		return nil
	})

	out := map[string]any{"id": id}
	if locked {
		out["alive"] = alive
		out["username"] = username
		out["remain_days"] = remain
		out["stale"] = false
	} else {
		// 锁被占：返回上次缓存，明确告诉前端「这是旧数据」
		if snap, ok := s.cachedProbe(id); ok {
			out["alive"] = snap.Alive
			out["username"] = snap.Username
			out["remain_days"] = snap.Remain
			out["stale"] = true
			out["stale_reason"] = "有任务正在执行，显示的是上次检查结果"
		} else {
			out["alive"] = nil
			out["stale"] = true
			out["stale_reason"] = "有任务正在执行，暂时无法检查"
		}
	}
	writeJSON(w, out)
}

type loginReq struct {
	ID       string `json:"id"`
	Phone    string `json:"phone"`
	Password string `json:"password"`
	Code     string `json:"code"`
	Action   string `json:"action"`
}

// handleAccountLogin 处理登录相关动作。
func (s *Server) handleAccountLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req loginReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}

	// 拿账号信息
	acc, ok := s.store.Get(req.ID)
	if !ok {
		writeJSON(w, map[string]any{"error": "账号不存在"})
		return
	}
	// 前端可能传来脱敏号码（如 157******41），检测到就用账号记录里的真实号码
	phone := req.Phone
	if phone == "" || strings.Contains(phone, "*") {
		phone = acc.Phone
	}
	pwd := req.Password
	if pwd == "" {
		pwd = acc.Password
	}

	var result any
	switch req.Action {
	case "verify":
		result = s.withAccount(req.ID, func() any {
			res, err := task.VerifyPassword(phone, pwd)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return res
		})

	case "send-sms":
		result = s.withAccount(req.ID, func() any {
			res, err := task.SendSMSCode(phone, pwd)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return res
		})

	case "trust":
		result = s.withAccount(req.ID, func() any {
			res, err := task.TrustDeviceLogin(phone, pwd, req.Code)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			// 成功后记录
			if res.Stage == "done" {
				s.store.MarkChecked(req.ID, true, res.Username, 0, "")
				_ = s.store.Update(req.ID, func(a *account.Account) {
					a.Trusted = true
					a.TrustedAt = time.Now().Format("2006-01-02 15:04:05")
					a.HwidCasSid = res.Sid
				})
			}
			return res
		})

	case "pure-login":
		result = s.withAccount(req.ID, func() any {
			res, err := task.LoginByPassword(phone, pwd)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			if res.Stage == "done" {
				s.store.MarkChecked(req.ID, true, res.Username, 0, "")
			} else {
				s.store.MarkChecked(req.ID, false, "", 0, res.Message)
			}
			return res
		})

	default:
		http.Error(w, "unknown action", 400)
		return
	}

	writeJSON(w, result)
}

// taskLabel 把任务名转成中文，用于日志和错误提示。
// 与前端 index.go 里的 TASK_LABELS 保持一致。
func taskLabel(name string) string {
	switch name {
	case "sign":
		return "签到"
	case "devspace":
		return "DevSpace"
	case "lab":
		return "实验"
	case "blog":
		return "发文章"
	case "issue":
		return "提Issue"
	case "star":
		return "Star"
	case "eventjoin":
		return "活动报名"
	case "gitcode":
		return "GitCode"
	case "bindgitcode":
		return "绑定GitCode"
	case "event":
		return "赛事"
	case "keepalive":
		return "保活"
	case "session":
		return "检查"
	case "tasks":
		return "任务"
	case "beans":
		return "积分"
	case "daily":
		return "一键日常"
	case "weekly":
		return "一键周常"
	}
	return name
}

// startJob 启动一个后台长任务（日常/周常），立即返回 job 状态。
//
// 同一账号同一任务不允许并发：重复点击会返回「已在执行中」，
// 避免用户连点两下同时开两个浏览器把登录态互相踢掉。
func (s *Server) startJob(id, name string) (*jobState, error) {
	s.jobMu.Lock()
	for _, j := range s.jobs {
		if j.Account == id && j.Name == name && j.Running {
			cp := *j
			s.jobMu.Unlock()
			return &cp, fmt.Errorf("「%s」已在执行中（已运行 %s）",
				taskLabel(name), time.Since(j.Started).Round(time.Second))
		}
	}

	jobID := fmt.Sprintf("%s-%d", name, time.Now().UnixNano())
	j := &jobState{
		ID:      jobID,
		Account: id,
		Name:    name,
		Label:   taskLabel(name),
		Running: true,
		Step:    "准备中",
		Started: time.Now(),
	}
	s.jobs[jobID] = j
	// 只保留最近 20 条，避免无限增长
	if len(s.jobs) > 20 {
		var oldest string
		var oldestAt time.Time
		for k, v := range s.jobs {
			if v.Running {
				continue
			}
			if oldest == "" || v.Started.Before(oldestAt) {
				oldest, oldestAt = k, v.Started
			}
		}
		if oldest != "" {
			delete(s.jobs, oldest)
		}
	}
	cp := *j
	s.jobMu.Unlock()

	go func() {
		svcLog("[job] %s 启动「%s」（账号 %s）", jobID, taskLabel(name), id)
		s.setJobStep(jobID, "检查会话")

		var res map[string]any
		func() {
			defer func() {
				if r := recover(); r != nil {
					svcLog("[job] %s panic: %v", jobID, r)
					s.finishJob(jobID, nil, fmt.Sprintf("内部错误: %v", r))
				}
			}()

			// 会话检查必须在 withAccount 之外做！
			//
			// withAccount 会 s.mu.Lock()，而 autoLogin 内部也要调 withAccount
			// 来保存会话。Go 的 sync.Mutex 不可重入 —— 在 withAccount 里调
			// ensureAlive/autoLogin 会直接自锁死，表现为任务永远「执行中」
			// （实测卡了 8 分钟，Camoufox 那边早就登录成功了）。
			if ok, msg := s.ensureAlive(id, "执行「"+taskLabel(name)+"」时发现离线"); !ok {
				svcLog("[job] %s 会话不可用: %s", jobID, msg)
				s.finishJob(jobID, map[string]any{
					"label": taskLabel(name),
					"ok":    false,
					"msg":   "账号离线，自动登录失败：" + msg,
					"steps": []map[string]any{{"name": "自动登录", "ok": false, "msg": msg}},
				}, "账号离线："+msg)
				return
			}

			s.setJobStep(jobID, "执行中")
			res = s.withAccount(id, func() any {
				if name == "daily" {
					return s.runDaily()
				}
				return s.runWeekly()
			}).(map[string]any)
		}()

		if res == nil {
			return
		}
		errMsg := ""
		if e, ok := res["error"].(string); ok {
			errMsg = e
		}
		s.finishJob(jobID, res, errMsg)
	}()

	return &cp, nil
}

// setJobStep 更新任务的当前步骤文字。
func (s *Server) setJobStep(jobID, step string) {
	s.jobMu.Lock()
	defer s.jobMu.Unlock()
	if j, ok := s.jobs[jobID]; ok {
		j.Step = step
	}
}

// finishJob 标记任务结束并写入结果。
func (s *Server) finishJob(jobID string, res map[string]any, errMsg string) {
	s.jobMu.Lock()
	defer s.jobMu.Unlock()
	j, ok := s.jobs[jobID]
	if !ok {
		return
	}
	j.Running = false
	j.Result = res
	j.Error = errMsg
	j.Ended = time.Now()
	j.Step = "已完成"
	if errMsg != "" {
		j.Step = "失败"
	}
	svcLog("[job] %s 结束（%s）", jobID, time.Since(j.Started).Round(time.Second))
}

// handleJobs 返回最近的任务状态，供前端轮询。
//
// 可选 ?id=<jobID> 只查单个。
func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")

	// 先在锁内把需要的快照拷出来，解锁后再写响应。
	//
	// 原来是把 writeJSON 放在 defer Unlock 的作用域里 —— 客户端慢（手机
	// 网络、页面切后台）时，一次写响应会阻塞几百毫秒到几秒，而这段时间
	// jobMu 一直被持有，后台任务的状态机（setJobStep/finishJob）全部卡住。
	// 锁内只做内存拷贝，不做 I/O。
	var single *jobState
	var list []*jobState

	s.jobMu.Lock()
	if id != "" {
		if j, ok := s.jobs[id]; ok {
			cp := *j
			single = &cp
		}
	} else {
		list = make([]*jobState, 0, len(s.jobs))
		for _, j := range s.jobs {
			cp := *j
			list = append(list, &cp)
		}
	}
	s.jobMu.Unlock()

	if id != "" {
		if single != nil {
			writeJSON(w, map[string]any{"job": single})
			return
		}
		writeJSON(w, map[string]any{"error": "任务不存在"})
		return
	}

	// 新的在前
	for i := 0; i < len(list); i++ {
		for k := i + 1; k < len(list); k++ {
			if list[k].Started.After(list[i].Started) {
				list[i], list[k] = list[k], list[i]
			}
		}
	}
	if len(list) > 10 {
		list = list[:10]
	}
	writeJSON(w, map[string]any{"jobs": list})
}

// handleRunTask 在指定账号下执行任务。
func (s *Server) handleRunTask(w http.ResponseWriter, r *http.Request) {
	// 只接受 POST。
	//
	// 原来这里不限方法，前端用 GET 调用，于是 /api/accounts/run 可以被
	// 一个 <img src="http://面板/api/accounts/run?id=...&name=weekly">
	// 触发 —— cookie 是 SameSite=Lax，挡不住顶级导航，恶意链接或浏览器
	// 预取就能让面板跑任务（开浏览器、发文章、提 Issue）。
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONStatus(w, http.StatusMethodNotAllowed,
			map[string]any{"error": "该接口只接受 POST"})
		return
	}
	if !sameOrigin(r) {
		writeJSONStatus(w, http.StatusForbidden,
			map[string]any{"error": "跨站请求被拒绝"})
		return
	}

	id := r.URL.Query().Get("id")
	name := r.URL.Query().Get("name")
	if id == "" || name == "" {
		http.Error(w, "missing id or name", 400)
		return
	}

	acc, ok := s.store.Get(id)
	if !ok {
		writeJSON(w, map[string]any{"error": "账号不存在"})
		return
	}

	startAt := time.Now()
	svcLog("[run] %s 请求「%s」", id, taskLabel(name))

	// 长任务（日常/周常）走异步：同步跑要几分钟，HTTP 会超时，
	// 前端表现就是「点了没反应」。这里立即返回 job id，后台执行，
	// 前端轮询 /api/jobs 看进度。
	if name == "daily" || name == "weekly" {
		job, err := s.startJob(id, name)
		if err != nil {
			writeJSON(w, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{
			"async": true,
			"job":   job,
			"msg":   taskLabel(name) + " 已在后台执行，可在日志区查看进度",
		})
		return
	}

	// 除「保活/检查/任务列表」外，其他任务都要用华为云会话。
	// 账号离线时先自动账密登录一次，避免用户点完只看到一堆 401。
	// 保活本身就会尝试续期，不必在这里抢跑（否则会重复开浏览器）。
	// GitCode 登录不需要华为云会话 —— 它用的是 GitCode 自己的账密，
	// 走独立的 gitcode_profile。之前没排除它，导致点「绑定GitCode」会先
	// 触发一次华为云自动登录（30-60 秒），用户以为卡死了。
	if name != "keepalive" && name != "tasks" && name != "gitcode" {
		alive, _, _ := s.probeAccount(id)
		svcLog("[run] %s 「%s」会话检查: alive=%v（耗时 %s）",
			id, taskLabel(name), alive, time.Since(startAt).Round(time.Millisecond))
		if !alive {
			svcLog("[run] %s 「%s」账号离线，开始自动登录（可能需要 30-60 秒）",
				id, taskLabel(name))
			ok, msg := s.autoLogin(id, "点击「"+taskLabel(name)+"」时发现离线")
			svcLog("[run] %s 「%s」自动登录结果: ok=%v msg=%s",
				id, taskLabel(name), ok, msg)
			if !ok {
				writeJSON(w, map[string]any{
					"error": "账号离线，自动登录失败：" + msg,
					"auto_login_failed": true,
				})
				return
			}
		}
	}

	svcLog("[run] %s 「%s」开始执行任务体", id, taskLabel(name))

	// keepalive 必须**在 s.mu 之外**执行。
	//
	// 它内部会调 autoLogin，而 autoLogin 要用 withAccount 保存会话。
	// Go 的 sync.Mutex 不可重入 —— 包在 withAccount 里会自锁死，
	// 表现为日志停在「触发账密登录」之后没有任何输出，而 Camoufox
	// 那边其实已经登录成功了（实测卡死 2 分钟以上，会话产物是新的
	// 但 cookies.json 一直没更新）。
	if name == "keepalive" {
		// 先看会话还在不在；在就直接返回，不必开浏览器。
		//
		// 【这次会话检查必须在账号上下文里做】
		//
		// KeepAlive → huaweiSession() → huawei.New("") → config.CookieFile()，
		// 而 CookieFile() 读的是**进程级全局** config.accountID。
		// 早先这里在 s.mu 之外直接调 task.KeepAlive，全局 accountID 还是
		// 上一个任务留下的值 —— 于是「保活账号 A」实际检查的是
		// 别的账号的 cookie，并把那个账号的会话当成自己的。
		//
		// 实测症状（2026-11-02 10:19）：三个账号的保活并发进来，
		// 账号 A 登录成功后，它的 cookies.json 被写进了
		// 账号 B 的 state 目录，两个号在面板上显示成同一个人
		// （同用户名、同积分）。
		//
		// 用 TryLock 而不是阻塞版：拿不到锁说明有别的账号的长任务在跑，
		// 此时不能瞎猜，直接回「忙，稍后再试」，绝不读别人的 cookie。
		var res *task.KeepAliveResult
		var lockErr error
		if !s.withAccountTry(id, func() any {
			r, err := task.KeepAlive(true, 0)
			if err != nil {
				r = &task.KeepAliveResult{After: "expired", Message: err.Error()}
			}
			res, lockErr = r, err
			return nil
		}) {
			// 锁被占：不要退回读全局状态的路径，明确告诉用户稍后再试。
			// 报「忙」比报一个错的「已在线」安全得多。
			writeJSON(w, map[string]any{
				"before":    "",
				"after":     "busy",
				"refreshed": false,
				"message":   "有任务正在执行，账号上下文被占用，请几秒后重试",
			})
			return
		}
		_ = lockErr
		if res.After != "alive" {
			// 登录本身不需要账号上下文（Camoufox 按手机号开独立会话，
			// 写回时 autoLogin 内部会用 withAccountTry 拿锁），所以放锁外。
			ok, msg := s.autoLogin(id, "保活检查发现会话失效")
			if ok {
				res.After = "alive"
				res.Refreshed = true
				res.Message = "会话失效，已自动账密登录"
			} else {
				res.Message = "会话失效，自动登录失败: " + msg
			}
		}
		writeJSON(w, map[string]any{
			"before":    res.Before,
			"after":     res.After,
			"refreshed": res.Refreshed,
			"username":  res.Username,
			"message":   res.Message,
		})
		return
	}

	// 积分查询：纯 HTTP，同样不该排长任务的队。
	//
	// 和 tasks 一样走 withAccountTry —— 拿不到锁就报忙，不阻塞。
	// 用 BeansE 而不是 Beans：后者失败时返回 0，用户会以为积分清零了。
	if name == "beans" {
		var out map[string]any
		if !s.withAccountTry(id, func() any {
			sess, err := huawei.New("")
			if err != nil {
				out = map[string]any{"error": err.Error()}
				return nil
			}
			n, err := sess.BeansE()
			if err != nil {
				out = map[string]any{"error": err.Error()}
				return nil
			}
			out = map[string]any{"beans": n, "username": sess.Username}
			return nil
		}) {
			writeJSON(w, map[string]any{
				"error": "有任务正在执行，账号上下文被占用，请稍后再查积分",
				"busy":  true,
			})
			return
		}
		// 顺手更新缓存，面板卡片下次刷新就能看到新积分
		if n, ok := out["beans"].(int); ok {
			s.beansMu.Lock()
			s.beansCache[id] = beanEntry{val: n, at: time.Now()}
			s.beansMu.Unlock()
		}
		writeJSON(w, out)
		return
	}

	// 任务列表是只读的纯 HTTP 请求，不该排长任务的队。
	//
	// 为什么单独拎出来：所有 case 都在 s.withAccount 里执行，而那是
	// **全局锁**（不是按账号分的）—— config.SetAccount 是进程级全局变量，
	// huawei.New("") 靠它定位 cookie 文件，所以同一时刻只能有一个账号
	// 在跑。周常持锁 10 分钟，点「任务」就得干等 10 分钟。
	//
	// 用户的原话：「任务列表拉不下拉，这个不是纯协议吗，为啥也要等」。
	// 纯协议确实很快（~1 秒），等的是锁，不是网络。
	//
	// 这里用 withAccountTry：拿不到锁立刻返回「忙」，让前端提示用户
	// 稍后再点，而不是让请求在队列里挂到 HTTP 超时（前端 4 分钟）。
	if name == "tasks" {
		var out map[string]any
		if !s.withAccountTry(id, func() any {
			sess, err := huawei.New("")
			if err != nil {
				out = map[string]any{"error": err.Error()}
				return nil
			}
			missions, err := huawei.TasksByStar(sess, 1, 2, 3, 4, 5)
			if err != nil {
				out = map[string]any{"error": err.Error()}
				return nil
			}
			list := make([]map[string]any, 0, len(missions))
			for _, m := range missions {
				list = append(list, map[string]any{
					"title":  m.Title,
					"degree": m.Degree,
					"bonus":  m.Bonus,
					"done":   m.IsFinish == 1,
				})
			}
			out = map[string]any{"tasks": list}
			return nil
		}) {
			writeJSON(w, map[string]any{
				"error": "有任务正在执行，账号上下文被占用，请稍后再点「任务」",
				"busy":  true,
			})
			return
		}
		writeJSON(w, out)
		return
	}

	result := s.withAccount(id, func() any {
		switch name {
		case "sign":
			sess, err := huawei.New("")
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			already, err := sess.AutoSign()
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return map[string]any{"already_signed": already}

		case "devspace":
			res, err := task.TrackDevSpace()
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return res

		case "lab":
			// 跑云实验：读 data/lab_record.txt 作为过程记录。
			// 实验要开真实环境、跑几分钟，这里给足超时。
			rec, err := os.ReadFile(filepath.Join(config.DataDir(), "lab_record.txt"))
			if err != nil {
				return map[string]any{"error": "缺少实验过程记录 data/lab_record.txt"}
			}
			res, err := task.RunLabTask(task.LabOptions{
				LabID:      config.DefaultLabID,
				RecordText: strings.TrimSpace(string(rec)),
				Headless:   true,
				End:        true,
				Timeout:    600,
			})
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return res

		case "blog":
			// 发布文章：读 data/blog_post.md，标题从首个 # 标题推导。
			raw, err := os.ReadFile(filepath.Join(config.DataDir(), "blog_post.md"))
			if err != nil {
				return map[string]any{"error": "缺少文章 data/blog_post.md"}
			}
			title, content := parseBlogMarkdown(string(raw))
			if title == "" {
				return map[string]any{"error": "文章缺少 # 标题"}
			}
			res, err := task.PublishBlogAPI(title, summarize(content, 100), content, false, nil)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return res

		case "gitcode":
			// 手动登录 GitCode（前端按钮）。
			// 用账号里存的 GitCode 账密自动登录；没存过就报错让用户先设置。
			if acc.GitCodeUser == "" || acc.GitCodePass == "" {
				return map[string]any{"error": "未设置 GitCode 账密，请先填写"}
			}
			res, err := task.LoginGitCodeAuto(acc.GitCodeUser, acc.GitCodePass, 180)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			// 登录成功后把会话同步到账号 state 目录（LoginGitCodeAuto 存的是当前账号目录）
			//
			// 顺手抓一次真实用户名并写回账号。
			//
			// 为什么必须自动抓：GitCode 允许用手机号/邮箱登录，
			// 用户填手机号也能登录成功，但绑定接口要的是用户名，
			// 传手机号会 404「gitcode user not found」。
			// 让用户自己去个人主页复制用户名是多余的成本 ——
			// 账号设置页上明明写着。
			//
			// 抓取失败不影响登录结果，只记在返回里让前端提示。
			if u, uerr := task.FetchGitCodeUsername(); uerr == nil && u != "" {
				res["gitcode_username"] = u
				if u != acc.GitCodeUser {
					_ = s.store.Update(id, func(a *account.Account) {
						a.GitCodeUser = u
						// 用户名变了，旧绑定状态作废
						a.GitCodeBindState = ""
						a.GitCodeBindMsg = ""
					})
					res["username_updated"] = true
					res["previous_username"] = acc.GitCodeUser
				}
			} else if uerr != nil {
				res["username_error"] = uerr.Error()
			}
			return res

		case "bindgitcode":
			// 把 GitCode 账号绑定到华为云。
			//
			// 为什么要单独一步：**GitCode 登录成功 ≠ 绑定成功**。
			// 登录只是拿到 GitCode 的会话（能访问 gitcode.com），
			// 绑定是华为云侧的一条记录（GitCode 用户名 ↔ 华为云账号）。
			// 不绑定的话，Issue 发得再多成长中心也一直显示未完成。
			//
			// 之前的 bug：「保存并登录」只做了登录，没做绑定，
			// 弹窗却说「GitCode 登录成功」，用户以为万事大吉，
			// 实际绑定状态一直是 unbound。
			if acc.GitCodeUser == "" {
				return map[string]any{"error": "未设置 GitCode 用户名，请先填写"}
			}
			// 用户名是手机号时，先从 GitCode 会话里自动抓真实用户名。
			// 用户填手机号能登录（GitCode 支持），但绑定要的是用户名。
			// 这里直接帮他换掉，不用他手动改。
			bindUser := acc.GitCodeUser
			if isPhoneLike(bindUser) {
				if u, uerr := task.FetchGitCodeUsername(); uerr == nil && u != "" {
					bindUser = u
					_ = s.store.Update(id, func(a *account.Account) {
						a.GitCodeUser = u
					})
				} else {
					return map[string]any{
						"error": fmt.Sprintf(
							"GitCode 用户名填的是手机号（%s），且自动获取用户名失败：%v。"+
								"请到 GitCode 账号设置页复制用户名后重新填写。",
							bindUser, uerr),
						"need_username": true,
					}
				}
			}
			res, err := task.HohBind(bindUser)
			if err != nil {
				// 把后端的 404「gitcode user not found」翻译成用户能懂的话
				if strings.Contains(err.Error(), "gitcode user not found") {
					return map[string]any{
						"error": fmt.Sprintf(
							"GitCode 上找不到用户「%s」。请确认：\n"+
								"1. 用户名拼写正确（不是手机号、不是邮箱）\n"+
								"2. 该 GitCode 账号已存在\n"+
								"用户名在个人主页地址里：gitcode.com/<用户名>",
							acc.GitCodeUser),
						"need_username": true,
					}
				}
				return map[string]any{"error": "绑定失败: " + err.Error()}
			}
			// HohBind 内部已经复核过一次；这里再根据结果给出明确结论，
			// 不能只看接口有没有报错 —— 接口返回 200 但没写进去是有的。
			bound, _ := res["bound"].(bool)
			if !bound {
				msg := "绑定接口调用成功，但华为云侧没有查到绑定记录"
				if b, ok := res["binding"]; ok && b != nil {
					msg = fmt.Sprintf("绑定记录异常：%v", b)
				}
				return map[string]any{
					"error":   msg,
					"bound":   false,
					"binding": res["binding"],
					"raw":     res["response"],
				}
			}
			// 绑定成功：把状态写回账号，前端不用再查一次。
			// 早先只返回给前端，账号里的 gitcode_bind_state 还是旧值，
			// 导致绑定成功后列表仍显示「未绑定」。
			now := time.Now().Format("2006-01-02 15:04:05")
			_ = s.store.Update(id, func(a *account.Account) {
				a.GitCodeUser = bindUser
				a.GitCodeBindState = "bound"
				a.GitCodeBindMsg = ""
				a.GitCodeBindAt = now
			})
			return res

		case "event":
			// 开发者赛事：报名 + 提交作品。
			// 走完整链路（列表 → 报名表 → 作品提交页 → 上传 zip），
			// 详见 internal/task/event.go 的说明。
			zipPath := filepath.Join(config.DataDir(), "work_upload.zip")
			res, err := task.RunEventTask(task.EventOptions{
				WorkZip:  zipPath,
				Headless: true,
				Timeout:  300,
			})
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return res

		case "issue":
			// 提 Issue：读 data/issue_feature.md，标题取首个 # 行。
			// GitCode 会话失效时自动用存的账密重登一次再试。
			raw, err := os.ReadFile(filepath.Join(config.DataDir(), "issue_feature.md"))
			if err != nil {
				return map[string]any{"error": "缺少 Issue 内容 data/issue_feature.md"}
			}
			title, body := parseBlogMarkdown(string(raw))
			if title == "" {
				title = "功能建议"
			}
			res, err := task.CreateIssue(title, body, "", true, false)
			if err != nil && strings.Contains(err.Error(), "会话") {
				// 会话失效 → 自动重登
				if acc.GitCodeUser == "" || acc.GitCodePass == "" {
					return map[string]any{
						"error": "GitCode 会话失效，且未保存账密无法自动重登",
					}
				}
				if _, lerr := task.LoginGitCodeAuto(acc.GitCodeUser, acc.GitCodePass, 180); lerr != nil {
					return map[string]any{"error": "GitCode 自动重登失败: " + lerr.Error()}
				}
				// 重登后重试一次
				res, err = task.CreateIssue(title, body, "", true, false)
			}
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return res

		case "star":
			// 给组织下第一个还没 star 过的仓库点 Star。
			// 会话失效时同样用存的账密重登一次再试。
			res, err := task.StarRepo(false)
			if err != nil && strings.Contains(err.Error(), "会话") {
				if acc.GitCodeUser == "" || acc.GitCodePass == "" {
					return map[string]any{
						"error": "GitCode 会话失效，且未保存账密无法自动重登",
					}
				}
				if _, lerr := task.LoginGitCodeAuto(acc.GitCodeUser, acc.GitCodePass, 180); lerr != nil {
					return map[string]any{"error": "GitCode 自动重登失败: " + lerr.Error()}
				}
				res, err = task.StarRepo(false)
			}
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return res

		case "eventjoin":
			// 报名一个进行中的开发者活动
			res, err := task.JoinEvent(false)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return res

		case "daily":
			return s.runDaily()

		case "weekly":
			return s.runWeekly()

		case "session":
			info, err := task.SessionInfo()
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return info

		default:
			return map[string]any{"error": "unknown task"}
		}
	})

	// 记录执行时间
	if m, ok := result.(map[string]any); ok {
		if _, hasErr := m["error"]; !hasErr {
			s.store.MarkTask(id, name)
		}
	}
	// 记一条执行日志：任务名 + 结果摘要，方便从 service 日志回溯
	if m, ok := result.(map[string]any); ok {
		elapsed := time.Since(startAt).Round(time.Second)
		if e, hasErr := m["error"]; hasErr {
			svcLog("[task] %s 执行「%s」失败（%s）: %v", id, taskLabel(name), elapsed, e)
		} else if steps, hasSteps := m["steps"].([]map[string]any); hasSteps {
			// 多步任务（日常/周常）：逐步打印，否则只知道「完成」不知道哪步做了什么
			svcLog("[task] %s 执行「%s」完成（%s）", id, taskLabel(name), elapsed)
			for _, st := range steps {
				ok, _ := st["ok"].(bool)
				mark := "✓"
				if !ok {
					mark = "✗"
				}
				svcLog("  %s %v: %v", mark, st["name"], st["msg"])
			}
		} else {
			summary := ""
			if msg, ok := m["msg"].(string); ok {
				summary = msg
			}
			svcLog("[task] %s 执行「%s」完成（%s）%s", id, taskLabel(name), elapsed, func() string {
				if summary == "" {
					return ""
				}
				return "：" + summary
			}())
		}
	}

	// 任务后刷新状态
	alive, username, remain := s.probeAccount(id)
	s.store.MarkChecked(id, alive, username, remain, "")

	_ = acc
	writeJSON(w, result)
}

// handleNoVNC 把 /vnc/ 反向代理到本机的 noVNC（websockify :6080）。
//
// 为什么需要它：用户要在**浏览器里直接**操作服务器上的浏览器画面
// （过滑块验证码）。原生 VNC 要装客户端 + 开 SSH 隧道，对普通用户太重。
//
// 路径映射（关键：/websockify 必须原样保留）：
//
//	/vnc/            -> http://127.0.0.1:6080/vnc.html
//	/vnc/app/...     -> http://127.0.0.1:6080/app/...
//	/vnc/websockify  -> http://127.0.0.1:6080/websockify   ← WebSocket 升级点
//
// 踩过的坑：一开始把 /vnc 前缀整个剥掉，于是 /vnc/websockify 变成 /，
// websockify 认不出这个路径，直接 404，noVNC 报「无法连接到服务器」。
// 它的 WebSocket 端点固定是 /websockify，不能改也不能剥。
func (s *Server) handleNoVNC(w http.ResponseWriter, r *http.Request) {
	target := &url.URL{Scheme: "http", Host: "127.0.0.1:6080"}

	path := strings.TrimPrefix(r.URL.Path, "/vnc")
	// /websockify 挂在站点根上，没有 /vnc 前缀，TrimPrefix 不会动它
	if path == "" || path == "/" {
		path = "/vnc.html"
	}
	target.Path = path
	target.RawQuery = r.URL.RawQuery
	// 首页默认自动连接 + 自适应缩放，省得用户再点一次「连接」
	if path == "/vnc.html" && target.RawQuery == "" {
		target.RawQuery = "autoconnect=1&resize=scale&reconnect=1&reconnect_delay=2000"
	}

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL = target
			req.Host = target.Host
			// WebSocket 升级依赖这两个头原样透传。
			// ReverseProxy 默认会处理 Upgrade，但 Director 里重建 URL 后
			// 必须保证 Connection/Upgrade 没被清掉。
			if req.Header.Get("Upgrade") != "" {
				req.Header.Set("Connection", "Upgrade")
			}
		},
		FlushInterval: -1, // WebSocket / 流式画面必须立即刷，不能缓冲
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			writeJSONStatus(w, http.StatusBadGateway, map[string]any{
				"error": "noVNC 未运行：" + err.Error(),
				"hint":  "在服务器执行 systemctl start hwcloud-novnc",
			})
		},
	}
	proxy.ServeHTTP(w, r)
}


//
// 为什么需要这个接口：GitCode 登录要过滑块，而浏览器跑在服务器的 Xvfb 上。
// 用户看不到画面就无从操作，也不知道端口和密码。以前这些信息只写在
// docs/VNC与验证码.md 里，用户卡住时根本想不到去翻文档。
//
// 返回字段：
//   available   VNC 是否可用（x11vnc 在监听）
//   port        端口（默认 5901）
//   password    密码明文。x11vnc 的 -rfbauth 文件是 DES 加密的，
//               无法反解，所以这里读的是部署时写在环境变量或文件里的明文副本
//   password_source  密码从哪来（方便排障）
//   tunnel_cmd  SSH 隧道命令，前端直接展示给用户复制
//   display     对应的 X display
//
// 安全：这个接口在 requireAuth 后面，只有登录面板的管理员能读。
// VNC 本身只监听 127.0.0.1，外部必须经 SSH 隧道，所以密码即使泄露
// 也需要先有服务器 SSH 权限才能用。
func (s *Server) handleVNCInfo(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{
		"available": false,
		"port":      5901,
		"display":   ":99",
	}

	port := 5901
	if v := os.Getenv("HWLOGIN_VNC_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			port = n
		}
	}
	out["port"] = port

	// 探测是否真的有进程在监听
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	conn, err := net.DialTimeout("tcp", addr, 800*time.Millisecond)
	if err != nil {
		out["hint"] = "VNC 未运行。在服务器上执行：systemctl start hwcloud-vnc"
		writeJSON(w, out)
		return
	}
	// 读 RFB 版本串确认是 VNC 而不是别的服务占了端口
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 12)
	n, _ := conn.Read(buf)
	_ = conn.Close()
	if n >= 4 && string(buf[:4]) != "RFB " {
		out["hint"] = fmt.Sprintf("端口 %d 被非 VNC 服务占用", port)
		writeJSON(w, out)
		return
	}
	out["available"] = true

	// 密码：优先环境变量，其次明文副本文件。
	// x11vnc.pass 是 DES 加密的，不能反解，所以部署脚本会另存一份明文。
	pass := os.Getenv("VNC_PASSWORD")
	src := "env:VNC_PASSWORD"
	if pass == "" {
		for _, p := range []string{
			"/etc/x11vnc.pass.txt",
			filepath.Join(config.Root(), "state", "vnc_password.txt"),
		} {
			if b, err := os.ReadFile(p); err == nil {
				pass = strings.TrimSpace(string(b))
				src = "file:" + p
				break
			}
		}
	}
	if pass != "" {
		out["password"] = pass
		out["password_source"] = src
	} else {
		out["password_source"] = "unknown"
		out["hint"] = "找不到 VNC 明文密码。可在服务器执行 " +
			"`x11vnc -storepasswd 新密码 /etc/x11vnc.pass` 重设，" +
			"并把明文写入 /etc/x11vnc.pass.txt 供面板读取"
	}

	// SSH 隧道命令。用 r.Host 的主机名部分，避免写死 IP。
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	out["tunnel_cmd"] = fmt.Sprintf("ssh -L %d:127.0.0.1:%d root@%s", port, port, host)
	out["connect_to"] = fmt.Sprintf("127.0.0.1:%d", port)
	// 浏览器直连地址：不用装客户端、不用开隧道
	out["web_url"] = "/vnc/"
	// 探测 noVNC 是否在跑
	if c2, err := net.DialTimeout("tcp", "127.0.0.1:6080", 500*time.Millisecond); err == nil {
		_ = c2.Close()
		out["web_available"] = true
	} else {
		out["web_available"] = false
		out["web_hint"] = "网页版 VNC 未运行：在服务器执行 systemctl start hwcloud-novnc"
	}

	writeJSON(w, out)
}

// handleCaptcha 取一张新的图片验证码。
func (s *Server) handleCaptcha(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "missing id", 400)
		return
	}
	acc, ok := s.store.Get(id)
	if !ok {
		writeJSON(w, map[string]any{"error": "账号不存在"})
		return
	}
	// 必须包在 withAccount 里：FetchCaptcha 内部会写 cookies/meta/fp，
	// 而写入路径走 config.StateDir()，读的是进程级全局的当前账号 ID。
	// 不包的话 cookie 会落到全局 state/ 或别的账号目录，覆盖别人的会话。
	var res any
	var fetchErr error
	s.withAccount(id, func() any {
		res, fetchErr = task.FetchCaptcha(id, acc.Phone, acc.Password)
		return nil
	})
	if fetchErr != nil {
		writeJSON(w, map[string]any{"error": fetchErr.Error()})
		return
	}
	writeJSON(w, res)
}

// handleCaptchaImage 代理验证码图片，避免前端直连华为域名。
func (s *Server) handleCaptchaImage(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "missing id", 400)
		return
	}
	data, ct, err := task.CaptchaImage(id)
	if err != nil {
		http.Error(w, err.Error(), 404)
		return
	}
	if ct == "" {
		ct = "image/png"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "no-store")
	w.Write(data)
}

// handleCaptchaSubmit 校验图片验证码，通过后完成登录（含信任设备）。
// handleCaptchaAuto 用打码服务自动识别图片验证码。
func (s *Server) handleCaptchaAuto(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "missing id", 400)
		return
	}
	var res *task.PureLoginResult
	raw := s.withAccount(id, func() any {
		rr, err := task.AutoSolveCaptcha(id)
		if err != nil {
			return map[string]any{"error": err.Error()}
		}
		res = rr
		return nil
	})
	if m, ok := raw.(map[string]any); ok {
		if e, ok := m["error"]; ok {
			writeJSON(w, map[string]any{"error": e})
			return
		}
	}
	if res == nil {
		writeJSON(w, map[string]any{"error": "自动识别失败"})
		return
	}
	writeJSON(w, res)
}

func (s *Server) handleCaptchaSubmit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		ID      string `json:"id"`
		Code    string `json:"code"`
		SMSCode string `json:"sms_code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if req.ID == "" {
		writeJSON(w, map[string]any{"error": "missing id"})
		return
	}

	// 下面所有 task 调用都必须包在 withAccount 里。
	// CaptchaSubmit / LoginWithCaptcha 内部会写 cookies/meta/hwid_cas_sid，
	// 写入路径读的是进程级全局的当前账号 ID —— 不包就会串号。

	// 第一次：只校验图片码
	if req.SMSCode == "" {
		var res any
		var err1 error
		s.withAccount(req.ID, func() any {
			res, err1 = task.CaptchaSubmit(req.ID, req.Code)
			return nil
		})
		if err1 != nil {
			writeJSON(w, map[string]any{"error": err1.Error()})
			return
		}
		if r, ok := res.(*task.PureLoginResult); ok && r.Stage != "captcha_ok" {
			writeJSON(w, res)
			return
		}
		// 图片码过了，接着提交账密（可能返回 need_sms）
		var login any
		var err2 error
		s.withAccount(req.ID, func() any {
			login, err2 = task.LoginWithCaptcha(req.ID, "")
			return nil
		})
		if err2 != nil {
			writeJSON(w, map[string]any{"error": err2.Error()})
			return
		}
		writeJSON(w, login)
		return
	}

	// 第二次：带短信码完成登录
	var login *task.PureLoginResult
	var err3 error
	s.withAccount(req.ID, func() any {
		login, err3 = task.LoginWithCaptcha(req.ID, req.SMSCode)
		return nil
	})
	if err3 != nil {
		writeJSON(w, map[string]any{"error": err3.Error()})
		return
	}
	if login.Stage == "done" {
		s.store.MarkChecked(req.ID, true, login.Username, 0, "")
		_ = s.store.Update(req.ID, func(a *account.Account) {
			a.Trusted = true
			a.TrustedAt = time.Now().Format("2006-01-02 15:04:05")
			a.HwidCasSid = login.Sid
		})
	}
	writeJSON(w, login)
}

// ===== csc 滑块验证 =====

// handleCsc 取滑块验证参数。
func (s *Server) handleCsc(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "missing id", 400)
		return
	}
	// 优先用待定账号，其次用已入库账号
	var phone, password string
	if p, ok := s.pendingGet(id); ok {
		phone, password = p.Phone, p.Password
	} else if acc, ok := s.store.Get(id); ok {
		phone, password = acc.Phone, acc.Password
	} else {
		writeJSON(w, map[string]any{"error": "账号不存在"})
		return
	}
	// 在账号上下文里执行，保证指纹/cookie 落到 accounts/<id>/state/
	var res *task.CscChallenge
	raw := s.withAccount(id, func() any {
		r, err := task.FetchCsc(id, phone, password)
		if err != nil {
			return map[string]any{"error": err.Error()}
		}
		res = r
		return nil
	})
	if m, ok := raw.(map[string]any); ok {
		if e, ok := m["error"]; ok {
			writeJSON(w, map[string]any{"error": e})
			return
		}
	}
	if res == nil {
		writeJSON(w, map[string]any{"error": "获取验证参数失败"})
		return
	}
	writeJSON(w, res)
}

// handleCscSDK 代理华为验证码 SDK，避免前端跨域。
func (s *Server) handleCscSDK(w http.ResponseWriter, r *http.Request) {
	data, ct, err := task.CscSDK()
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Write(data)
}

// handleCscSubmit 用滑块 validate 完成登录。
func (s *Server) handleCscSubmit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		ID       string `json:"id"`
		Validate string `json:"validate"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if req.ID == "" || req.Validate == "" {
		writeJSON(w, map[string]any{"error": "缺少 id 或 validate"})
		return
	}

	// 在账号上下文里执行，cookie / sid 才会写到 accounts/<id>/state/
	var res *task.PureLoginResult
	raw := s.withAccount(req.ID, func() any {
		r, err := task.CscSubmit(req.ID, req.Validate)
		if err != nil {
			return map[string]any{"error": err.Error()}
		}
		res = r
		return nil
	})
	if m, ok := raw.(map[string]any); ok {
		if e, ok := m["error"]; ok {
			writeJSON(w, map[string]any{"error": e})
			return
		}
	}
	if res == nil {
		writeJSON(w, map[string]any{"error": "登录未返回结果"})
		return
	}
	if res.Stage == "done" {
		// 登录成功：如果这是待定账号，现在才真正入库
		if p, ok := s.pendingGet(req.ID); ok {
			if a, err := s.store.Add(p.Phone, p.Password); err == nil {
				_ = a
			}
			s.pendingDrop(req.ID)
		}
		s.store.MarkChecked(req.ID, true, res.Username, 0, "")
		_ = s.store.Update(req.ID, func(a *account.Account) {
			a.Trusted = true
			a.TrustedAt = time.Now().Format("2006-01-02 15:04:05")
			a.HwidCasSid = res.Sid
		})
		writeJSON(w, res)
		return
	}

	// 滑块已过、需要短信：直接自动发码，用户不用再点「发送短信」
	if res.Stage == "need_sms" {
		var smsRes *task.PureLoginResult
		s.withAccount(req.ID, func() any {
			r, err := task.CscSendSMS(req.ID)
			if err == nil {
				smsRes = r
			}
			return nil
		})
		msg := "验证通过，短信已发送"
		sent := false
		if smsRes != nil {
			sent = smsRes.NeedSMS
			if !sent {
				msg = "验证通过，但短信发送失败：" + smsRes.Message
			}
		}
		writeJSON(w, map[string]any{
			"stage":    "need_sms",
			"need_sms": true,
			"sms_sent": sent,
			"message":  msg,
		})
		return
	}

	writeJSON(w, res)
}

// handleCscSMS 滑块通过后发送短信。
func (s *Server) handleCscSMS(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	smsCode := r.URL.Query().Get("sms_code")
	if id == "" {
		http.Error(w, "missing id", 400)
		return
	}

	if smsCode != "" {
		// 在账号上下文里执行，cookie / sid 才会写到 accounts/<id>/state/
		var res *task.PureLoginResult
		raw := s.withAccount(id, func() any {
			r, err := task.CscSubmitSMS(id, smsCode)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			res = r
			return nil
		})
		if m, ok := raw.(map[string]any); ok {
			if e, ok := m["error"]; ok {
				writeJSON(w, map[string]any{"error": e})
				return
			}
		}
		if res == nil {
			writeJSON(w, map[string]any{"error": "登录未返回结果"})
			return
		}
		if res.Stage == "done" {
			s.store.MarkChecked(id, true, res.Username, 0, "")
			_ = s.store.Update(id, func(a *account.Account) {
				a.Trusted = true
				a.TrustedAt = time.Now().Format("2006-01-02 15:04:05")
				a.HwidCasSid = res.Sid
			})
		}
		writeJSON(w, res)
		return
	}

	var res2 *task.PureLoginResult
	raw2 := s.withAccount(id, func() any {
		r, err := task.CscSendSMS(id)
		if err != nil {
			return map[string]any{"error": err.Error()}
		}
		res2 = r
		return nil
	})
	if m, ok := raw2.(map[string]any); ok {
		if e, ok := m["error"]; ok {
			writeJSON(w, map[string]any{"error": e})
			return
		}
	}
	if res2 == nil {
		writeJSON(w, map[string]any{"error": "发送短信失败"})
		return
	}
	writeJSON(w, res2)
}

// handleSystem 返回系统信息。
func (s *Server) handleSystem(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{
		"time":     time.Now().Format("2006-01-02 15:04:05"),
		"platform": runtime.GOOS + "/" + runtime.GOARCH,
		"root":     config.Root(),
	}
	if st, err := os.Stat(config.CookieFile()); err == nil {
		out["cookies_mtime"] = st.ModTime().Format("2006-01-02 15:04:05")
	}
	writeJSON(w, out)
}

// handleLogs 返回日志内容。
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	id := r.URL.Query().Get("id")
	if name == "" {
		name = "service.log"
	}
	if strings.Contains(name, "..") || strings.ContainsAny(name, "/\\") {
		http.Error(w, "invalid name", 400)
		return
	}

	var base string
	if id != "" {
		base = filepath.Join(s.store.AccountDir(id), "logs")
	} else {
		base = filepath.Join(config.Root(), "logs")
	}
	p := filepath.Join(base, name)

	data, err := os.ReadFile(p)
	if err != nil {
		writeJSON(w, map[string]any{"lines": []string{}, "error": "日志文件不存在: " + name})
		return
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > 300 {
		lines = lines[len(lines)-300:]
	}
	writeJSON(w, map[string]any{"lines": lines, "file": name})
}

// handleTimers 返回定时任务信息。
func (s *Server) handleTimers(w http.ResponseWriter, r *http.Request) {
	if runtime.GOOS != "linux" {
		writeJSON(w, map[string]any{
			"platform": "windows",
			"note":     "Windows 下由面板进程调度，无 systemd timer",
			"timers":   []any{},
		})
		return
	}

	txt, err := exec.Command("systemctl", "list-timers", "hwcloud*", "--no-pager", "--all").Output()
	if err != nil {
		writeJSON(w, map[string]any{"platform": "linux", "error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"platform": "linux", "raw": string(txt)})
}


// ===== 认证接口 =====

// handleAuthState 返回认证状态。
func (s *Server) handleAuthState(w http.ResponseWriter, r *http.Request) {
	// 账号表损坏时如实报告，前端显示修复提示而不是「请创建管理员」。
	// 如果这里回 need_setup=true，用户会以为可以重新初始化，
	// 而 setup 接口其实会拒绝 —— 前后端行为不一致。
	if err := s.auth.broken(); err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]any{
			"error":  "面板账号数据损坏：" + err.Error(),
			"broken": true,
		})
		return
	}

	setup := s.auth.hasUsers()
	username := ""
	expire := ""
	if c, err := r.Cookie(cookieName); err == nil {
		if u, ok := s.auth.lookup(c.Value); ok {
			username = u
			if t, ok := s.auth.expireInfo(c.Value); ok {
				expire = t.Format("2006-01-02 15:04:05")
			}
		}
	}
	writeJSON(w, map[string]any{
		"need_setup": !setup,
		"logged_in":  username != "",
		"username":   username,
		"expire_at":  expire,
	})
}

// handleAuthSetup 首次创建管理员账号。
func (s *Server) handleAuthSetup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	// 账号表损坏时拒绝初始化（fail closed），不能把损坏当成「没有用户」
	if err := s.auth.broken(); err != nil {
		http.Error(w, "面板账号数据损坏，拒绝初始化："+err.Error(),
			http.StatusInternalServerError)
		return
	}

	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if len(req.Username) < 3 {
		writeJSONStatus(w, http.StatusBadRequest,
			map[string]any{"error": "用户名至少 3 位"})
		return
	}
	if len(req.Password) < 6 {
		writeJSONStatus(w, http.StatusBadRequest,
			map[string]any{"error": "密码至少 6 位"})
		return
	}

	// setup 内部用 setupMu 串行化「检查 + 创建」，消除 TOCTOU：
	// 原来 hasUsers() 与 addUser() 是两次独立加锁，两个并发请求都能通过检查。
	if err := s.auth.setup(req.Username, req.Password); err != nil {
		writeJSONStatus(w, http.StatusConflict, map[string]any{"error": err.Error()})
		return
	}

	tok := s.auth.issue(req.Username)
	setCookie(w, tok)
	svcLog("[auth] 管理员账号已创建: %s（来自 %s）", req.Username, clientIP(r))
	writeJSON(w, map[string]any{"ok": true, "username": req.Username})
}

// handleAuthLogin 登录。
func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	if !s.auth.hasUsers() {
		writeJSON(w, map[string]any{"error": "未初始化", "need_setup": true})
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if !s.auth.check(req.Username, req.Password) {
		fmt.Printf("[auth] 登录失败: %s（来自 %s）\n", req.Username, clientIP(r))
		writeJSON(w, map[string]any{"error": "账号或密码错误"})
		return
	}
	tok := s.auth.issue(req.Username)
	setCookie(w, tok)
	fmt.Printf("[auth] 登录成功: %s（来自 %s）\n", req.Username, clientIP(r))
	writeJSON(w, map[string]any{"ok": true, "username": req.Username})
}

// handleAuthLogout 退出登录。
func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		s.auth.revoke(c.Value)
	}
	name, _, _, _, path, _ := cookieOpts()
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: "", Path: path, MaxAge: -1, HttpOnly: true,
	})
	writeJSON(w, map[string]any{"ok": true})
}

// handleAuthUsers 管理面板用户（仅登录后可用）。
func (s *Server) handleAuthUsers(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s.auth.mu.RLock()
		names := make([]string, 0, len(s.auth.users))
		for k := range s.auth.users {
			names = append(names, k)
		}
		s.auth.mu.RUnlock()
		writeJSON(w, map[string]any{"users": names})
		return
	}

	// POST：新增用户
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if len(req.Username) < 3 || len(req.Password) < 6 {
		writeJSON(w, map[string]any{"error": "用户名至少 3 位，密码至少 6 位"})
		return
	}
	if err := s.auth.addUser(req.Username, req.Password); err != nil {
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func setCookie(w http.ResponseWriter, tok string) {
	name, maxAge, httpOnly, secure, path, sameSite := cookieOpts()
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    tok,
		Path:     path,
		MaxAge:   maxAge,
		HttpOnly: httpOnly,
		Secure:   secure,
		SameSite: sameSite,
	})
}

// ---------- Camoufox 浏览器登录 ----------
//
// 纯协议那条路（handleCsc）在真实风控下过不了：fp 是每次会话动态生成的，
// 复用旧值会被判「设备不安全」。这里改成调服务器上的 Camoufox 服务，
// 用真实 Firefox 生成指纹，实测能跳过滑块、直接走短信。

// handleCamoufoxHealth 探测 Camoufox 服务是否可用。
func (s *Server) handleCamoufoxHealth(w http.ResponseWriter, r *http.Request) {
	ok, info := task.CamoufoxHealth()
	writeJSON(w, map[string]any{
		"ok":     ok,
		"info":   info,
		"target": task.CamoufoxBase(),
	})
}

// handleCamoufoxLogout 关闭某个账号在 Camoufox 里开着的浏览器。
//
// 触发场景：用户关掉登录弹窗。典型是「等短信验证码等到一半不想等了」，
// 或者点错了账号。不关的话浏览器会一直占着 LOCK 和内存，
// 后续所有登录请求都排队，最终报「连接 Camoufox 服务失败」。
//
// 这是个清理动作，失败也不该让用户看到错误 —— 前端本来就不等它。
// 但日志要留痕，否则下次排查「为什么浏览器没退」又得猜。
func (s *Server) handleCamoufoxLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		ID    string `json:"id"`
		Phone string `json:"phone"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}

	// 手机号优先从账号库里取，避免前端传错或传空
	phone := req.Phone
	if phone == "" && req.ID != "" {
		if acc, ok := s.store.Get(req.ID); ok {
			phone = acc.Phone
		}
	}
	if phone == "" {
		phone = req.ID
	}
	if phone == "" {
		writeJSON(w, map[string]any{"ok": false, "error": "缺少账号"})
		return
	}

	if err := task.CamoufoxLogout(phone); err != nil {
		svcLog("[logout] %s 关闭 Camoufox 会话失败: %v", phone, err)
		// 仍然返回 ok：前端是 fire-and-forget，报错只会干扰用户。
		writeJSON(w, map[string]any{"ok": true, "warning": err.Error()})
		return
	}
	svcLog("[logout] %s 已关闭 Camoufox 会话", phone)
	writeJSON(w, map[string]any{"ok": true})
}

// handleCamoufoxLogin 用 Camoufox 提交账密 / 短信码。
//
// 请求：{"id":"账号ID","password":"可选，覆盖","sms_code":"可选"}
// 返回：{"stage":"need_sms"|"done"|"failed", ...}
func (s *Server) handleCamoufoxLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		ID       string `json:"id"`
		Phone    string `json:"phone"`
		Password string `json:"password"`
		SMSCode  string `json:"sms_code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}

	// 账号来源：待定账号优先，其次已入库账号，最后用请求里直接给的
	var phone, password string
	if p, ok := s.pendingGet(req.ID); ok {
		phone, password = p.Phone, p.Password
	} else if acc, ok := s.store.Get(req.ID); ok {
		phone, password = acc.Phone, acc.Password
	}
	if req.Phone != "" {
		phone = req.Phone
	}
	if req.Password != "" {
		password = req.Password
	}
	if phone == "" {
		writeJSON(w, map[string]any{"error": "缺少手机号"})
		return
	}
	if password == "" && req.SMSCode == "" {
		// 走到这里说明账号在库里但没有存密码（早期加的账号可能为空）。
		// 前端「重新登录」按钮会先检查 password_saved，正常不会到这。
		writeJSON(w, map[string]any{"error": "该账号没有保存密码，请用「添加账号」重新添加"})
		return
	}

	res, err := task.CamoufoxLogin(phone, password, req.SMSCode)
	if err != nil && (res == nil || res.Stage == "") {
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	if res == nil {
		writeJSON(w, map[string]any{"error": "登录服务未返回结果"})
		return
	}

	// 需要短信：这一轮先不落盘，等用户填码
	if res.Stage == "need_sms" {
		writeJSON(w, map[string]any{
			"stage":    "need_sms",
			"need_sms": true,
			"message":  "验证码已发送到手机",
		})
		return
	}

	// 到了短信环节但没观察到发码请求：不能骗用户说「已发送」，
	// 否则用户会一直等一条不存在的短信（实测踩过：单渠道账号
	// 没有「选择其他验证方式」链接，代码跳过切通道后直接报已发送）。
	if res.Stage == "sms_not_sent" {
		writeJSON(w, map[string]any{
			"stage":   "sms_not_sent",
			"error":   "短信未发出：页面没触发发码请求，请重试",
			"retry":   true,
		})
		return
	}

	// 滑块没通过
	if res.Stage == "slider_failed" {
		writeJSON(w, map[string]any{
			"stage": "slider_failed",
			"error": "滑块验证未通过，请重试",
			"retry": true,
		})
		return
	}

	names := task.CamoufoxCookieNames(res)
	done := names["SSOTGC"] || names["CASTGC"]
	if !done {
		writeJSON(w, map[string]any{
			"stage":   "failed",
			"error":   "登录未完成（缺少 SSOTGC/CASTGC）",
			"missing": missingCookies(names),
		})
		return
	}

	// 成功：产物写进账号 state。
	//
	// 这里绝不能吞错。原来写的是 `if err != nil { _ = err }` —— 磁盘满或
	// 权限错误时用户看到「登录成功」，下一次任务却因为 state 里没有 cookie
	// 全部 401，而 service.log 里没有任何痕迹可查。
	// 登录成功但存不下来 = 登录失败，必须让用户看见。
	var saveArtifactErr error
	s.withAccount(req.ID, func() any {
		saveArtifactErr = task.SaveCamoufoxArtifacts(res)
		return nil
	})
	if saveArtifactErr != nil {
		svcLog("[login] %s 登录成功但保存会话失败: %v", req.ID, saveArtifactErr)
		writeJSONStatus(w, http.StatusInternalServerError, map[string]any{
			"stage": "failed",
			"error": "登录成功但保存会话失败: " + saveArtifactErr.Error(),
		})
		return
	}

	// 待定账号这时才真正入库
	if p, ok := s.pendingGet(req.ID); ok {
		if _, err := s.store.Add(p.Phone, p.Password); err == nil {
			s.pendingDrop(req.ID)
		}
	}
	// 用户名必须从会话里取，不能写手机号。
	//
	// 早先这里写的是 phone，于是 account.json 里 username 变成
	// 「13800138000」这种手机号，而面板卡片和「检查」按钮读的是
	// cookie 里的真实身份（yd_xxxxxxxxx）—— 两处对不上，用户看到
	// 一个号显示两种用户名，以为是串号。
	//
	// res 里没有 username，所以这里取一次 SessionInfo；取不到就留空，
	// 让下一次「检查」去填，不要拿手机号冒充。
	if uname := s.currentUsername(req.ID); uname != "" {
		s.store.MarkChecked(req.ID, true, uname, 0, "")
	} else {
		s.store.MarkChecked(req.ID, true, "", 0, "")
	}
	// Trusted 按 updateTrustBrowser 的真实结果写，不能无条件置 true：
	// 设备没被真正信任的话，下次登录还会弹验证码，界面却显示「已信任」，
	// 排查时会严重误导。
	_ = s.store.Update(req.ID, func(a *account.Account) {
		if res.Trusted {
			a.Trusted = true
			a.TrustedAt = time.Now().Format("2006-01-02 15:04:05")
		}
	})

	writeJSON(w, map[string]any{
		"stage":   "done",
		"message": "登录成功",
		"cookies": len(res.Cookies),
		"trusted": res.Trusted,
	})
}

// missingCookies 返回缺少的关键 cookie，便于前端提示。
func missingCookies(have map[string]bool) []string {
	var out []string
	for _, n := range []string{"SSOTGC", "CASTGC", "JSESSIONID", "hwid_cas_sid"} {
		if !have[n] {
			out = append(out, n)
		}
	}
	return out
}

// currentUsername 在账号上下文里读一次真实用户名。
//
// 必须走 withAccountTry：SessionInfo → huawei.New("") → config.CookieFile()
// 读的是进程级全局 accountID，不切上下文就会读到别的账号。
// 拿不到锁返回空串，调用方应把用户名留空，绝不用手机号顶替。
func (s *Server) currentUsername(id string) string {
	var uname string
	s.withAccountTry(id, func() any {
		if info, err := task.SessionInfo(); err == nil {
			if v, ok := info["username"].(string); ok {
				uname = v
			}
		}
		return nil
	})
	return uname
}

// autoLogin 用账号里存的手机号+密码自动登录，成功后把会话写回该账号 state。
//
// 触发时机（用户要求）：
//   - 前端点任意按钮时发现账号离线
//   - 定时任务（keepalive）检查到会话失效
//
// 为什么放在这里而不是每个 handler 里各写一遍：
//   登录要串行（Camoufox 是单实例服务，且同一账号并发登录会互相踢掉），
//   所以用 s.loginMu 串行化，并且同一个账号短时间内失败过就不再重试，
//   避免一次批量操作把登录服务打爆（每次登录要开浏览器、过滑块，约 30-60 秒）。
//
// 返回值 ok 表示「调用后账号是否可用」，reason 用于日志与前端提示。
func (s *Server) autoLogin(id, reason string) (bool, string) {
	acc, ok := s.store.Get(id)
	if !ok {
		return false, "账号不存在"
	}
	if acc.Password == "" {
		return false, "未保存密码，无法自动登录"
	}

	// 串行化：同一时刻只允许一个自动登录在跑
	s.loginMu.Lock()
	defer s.loginMu.Unlock()

	// 排队期间可能已经被别的请求登录成功了，先复查一次。
	//
	// 必须用 TryLock 版本：autoLogin 可能被 from withAccount 内部调用
	// （handleRunTask 的 keepalive 分支就是这样），此时 s.mu 已被当前
	// goroutine 持有，再走 withAccount 会自锁死 —— 表现为日志停在
	// 「触发账密登录」之后没有任何输出，Camoufox 那边其实早就成功了。
	if s.probeAccountTry(id) {
		return true, "已被其他请求登录成功"
	}

	// 失败冷却：同一账号 3 分钟内只尝试一次
	s.loginFailMu.Lock()
	if t, ok := s.loginFailAt[id]; ok && time.Since(t) < 3*time.Minute {
		s.loginFailMu.Unlock()
		return false, "自动登录刚失败过，3 分钟后再试"
	}
	s.loginFailMu.Unlock()

	fmt.Printf("[autoLogin] %s 触发账密登录（原因：%s）\n", id, reason)
	svcLog("[autoLogin] %s 触发账密登录（原因：%s）", id, reason)
	start := time.Now()
	res, err := task.CamoufoxLogin(acc.Phone, acc.Password, "")
	elapsed := time.Since(start).Round(time.Second)

	// 失败一律记冷却，避免连环重试
	markFail := func(msg string) (bool, string) {
		s.loginFailMu.Lock()
		s.loginFailAt[id] = time.Now()
		s.loginFailMu.Unlock()
		_ = s.store.Update(id, func(a *account.Account) {
			a.LastError = "自动登录失败: " + msg
		})
		fmt.Printf("[autoLogin] %s 失败（%s）：%s\n", id, elapsed, msg)
		svcLog("[autoLogin] %s 失败（%s）：%s", id, elapsed, msg)
		return false, msg
	}

	if err != nil && (res == nil || res.Stage == "") {
		return markFail(err.Error())
	}
	if res == nil {
		return markFail("登录服务未返回结果")
	}
	// 需要短信码 / 滑块没过：自动登录走不通，必须人工介入
	switch res.Stage {
	case "need_sms":
		return markFail("需要短信验证码，自动登录无法完成，请手动登录")
	case "sms_not_sent":
		return markFail("短信未发出，请手动登录")
	case "slider_failed":
		return markFail("滑块验证未通过，请手动登录")
	}

	names := task.CamoufoxCookieNames(res)
	if !names["SSOTGC"] && !names["CASTGC"] {
		return markFail("登录未完成（缺少 SSOTGC/CASTGC）")
	}

	// 保存会话产物。
	//
	// 必须通过 withAccount 拿到账号上下文再写 —— 因为 config.StateDir()
	// 读的是**进程级全局**的当前账号 ID。
	//
	// 早先这里有个「拿不到锁就直接写」的兜底，那是错的：
	// 拿不到锁说明另一个 goroutine 正持锁并设置了**别的**账号，此时直写
	// 会把 A 的 SSOTGC/CASTGC 落进 B 的 cookies.json，两个账号互相顶掉
	// 会话，而日志上只显示「又掉线了」，极难排查。
	//
	// 现在宁可失败也不写错地方：写不进去就返回错误，用户能看到，
	// 下一次点任务会重试登录。
	var saveErr error
	saved := false
	for i := 0; i < 40; i++ {
		if s.withAccountTry(id, func() any {
			saveErr = task.SaveCamoufoxArtifacts(res)
			return nil
		}) {
			saved = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !saved {
		svcLog("[autoLogin] %s 保存会话时 20 秒内未拿到账号上下文，放弃写入", id)
		return markFail("保存会话失败：账号上下文被其他任务长时间占用")
	}
	if saveErr != nil {
		return markFail("保存会话失败: " + saveErr.Error())
	}

	// 用户名从会话里读，不要写手机号（见 currentUsername 的注释）
	s.store.MarkChecked(id, true, s.currentUsername(id), 0, "")
	_ = s.store.Update(id, func(a *account.Account) {
		if res.Trusted {
			a.Trusted = true
			a.TrustedAt = time.Now().Format("2006-01-02 15:04:05")
		}
	})

	// 成功清掉冷却
	s.loginFailMu.Lock()
	delete(s.loginFailAt, id)
	s.loginFailMu.Unlock()

	fmt.Printf("[autoLogin] %s 成功（%s）\n", id, elapsed)
	svcLog("[autoLogin] %s 成功（%s）", id, elapsed)
	return true, "自动登录成功"
}

// ensureAlive 在执行任务前确保账号在线；离线就自动账密登录一次。
// 返回 false 时调用方应直接报错，不要继续跑任务（否则会得到一堆 401）。
func (s *Server) ensureAlive(id, reason string) (bool, string) {
	t0 := time.Now()
	alive, _, _ := s.probeAccount(id)
	svcLog("[alive] %s 会话检查: alive=%v（%s）", id, alive, time.Since(t0).Round(time.Millisecond))
	if alive {
		return true, ""
	}
	svcLog("[alive] %s 离线，开始自动账密登录（可能需要 30-90 秒，含滑块/短信）", id)
	ok, msg := s.autoLogin(id, reason)
	svcLog("[alive] %s 自动登录结果: ok=%v msg=%s（%s）",
		id, ok, msg, time.Since(t0).Round(time.Second))
	return ok, msg
}

// parseBlogMarkdown 从 Markdown 里拆出标题和正文。
//
// 约定：第一个 `# xxx` 是标题，其余全部算正文（标题行本身不进正文）。
// 这和 CLI 里 deriveTitle/stripLeadingTitle 的行为保持一致。
func parseBlogMarkdown(raw string) (title, body string) {
	lines := strings.Split(raw, "\n")
	var out []string
	found := false
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if !found && strings.HasPrefix(t, "# ") {
			title = strings.TrimSpace(strings.TrimPrefix(t, "# "))
			found = true
			continue
		}
		out = append(out, ln)
	}
	return title, strings.TrimSpace(strings.Join(out, "\n"))
}

// summarize 取正文前 n 个字符做摘要（去掉 Markdown 标记和换行）。
func summarize(content string, n int) string {
	r := strings.NewReplacer(
		"#", " ", "*", " ", "`", " ", ">", " ",
		"[", " ", "]", " ", "(", " ", ")", " ",
		"\n", " ", "\r", " ", "\t", " ",
	)
	s := strings.Join(strings.Fields(r.Replace(content)), " ")
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// runDaily 一键做所有日常任务。
//
// 日常 = 每天重置的任务：
//   - 签到（每日签到）
//   - 每日体验 AI DevSpace (+30)
//   - 每日发布文章 (+60)
//
// 顺序有讲究：先签到再 DevSpace 再发文章。签到是纯 API 调用最快，
// 发文最慢（要起浏览器），放最后免得拖慢整体反馈。
func (s *Server) runDaily() map[string]any {
	steps := []map[string]any{}
	dStart := time.Now()

	svcLog("[daily] 开始（账号 %s）", config.AccountID())
	// 注意：不要在这里调 ensureAlive/autoLogin。
	// 本函数是在 s.withAccount 内部执行的（持有 s.mu），而 autoLogin 也要
	// 用 withAccount 保存会话 —— sync.Mutex 不可重入，会直接自锁死。
	// 会话检查已由 startJob 在进入 withAccount 之前完成。

	// 1. 签到
	svcLog("[daily] 步骤1/3 签到…")
	if sess, err := huawei.New(""); err != nil {
		svcLog("[daily] 签到失败（建会话）: %v", err)
		steps = append(steps, map[string]any{"name": "签到", "ok": false, "msg": err.Error()})
	} else if already, err := sess.AutoSign(); err != nil {
		svcLog("[daily] 签到失败: %v", err)
		steps = append(steps, map[string]any{"name": "签到", "ok": false, "msg": err.Error()})
	} else if already {
		svcLog("[daily] 签到：今日已签到")
		steps = append(steps, map[string]any{"name": "签到", "ok": true, "msg": "今日已签到"})
	} else {
		svcLog("[daily] 签到成功")
		steps = append(steps, map[string]any{"name": "签到", "ok": true, "msg": "签到成功"})
	}

	// 2. DevSpace
	svcLog("[daily] 步骤2/3 DevSpace…")
	if res, err := task.TrackDevSpace(); err != nil {
		svcLog("[daily] DevSpace 失败: %v", err)
		steps = append(steps, map[string]any{"name": "DevSpace", "ok": false, "msg": err.Error()})
	} else {
		svcLog("[daily] DevSpace 已上报")
		steps = append(steps, map[string]any{"name": "DevSpace", "ok": true, "msg": "已上报", "detail": res})
	}

	// 3. 发文章
	svcLog("[daily] 步骤3/3 发文章…")
	if raw, err := os.ReadFile(filepath.Join(config.DataDir(), "blog_post.md")); err != nil {
		svcLog("[daily] 发文章缺少 blog_post.md")
		steps = append(steps, map[string]any{"name": "发文章", "ok": false, "msg": "缺少 data/blog_post.md"})
	} else if title, content := parseBlogMarkdown(string(raw)); title == "" {
		steps = append(steps, map[string]any{"name": "发文章", "ok": false, "msg": "文章缺少 # 标题"})
	} else if res, err := task.PublishBlogAPI(title, summarize(content, 100), content, false, nil); err != nil {
		svcLog("[daily] 发文章失败: %v", err)
		steps = append(steps, map[string]any{"name": "发文章", "ok": false, "msg": err.Error()})
	} else {
		svcLog("[daily] 发文章成功: %v", res["url"])
		steps = append(steps, map[string]any{"name": "发文章", "ok": true, "msg": "已发布", "detail": res})
	}

	svcLog("[daily] 结束，总耗时 %s", time.Since(dStart).Round(time.Second))
	return summarizeSteps("日常", steps)
}

// runWeekly 一键做所有周常任务。
//
// 周常 = 每周重置、且能自动化的任务：
//   - 每周实践一个实验 (+50)
//   - 每周给代码仓提 Issue (+60)
//
// 不做的（需要人工参与，无法自动化）：
//   - 每周通过一项微认证 (+200)：要真的去学堂考试
//   - 在开发者作品展览馆发布作品 (+200)：要提交作品审核
//   - 每周参与开发者赛事 (+100) / 开发者赛事晋级 (+200)：要报名比赛
//   - 建议被采纳/被实现 (+60/+200)：取决于官方是否采纳
//   - 每周Star一个代码仓 (+50)：接口未实现（已手动完成过）
func (s *Server) runWeekly() map[string]any {
	steps := []map[string]any{}
	wStart := time.Now()

	// 注意：不要在这里调 ensureAlive/autoLogin（会在 s.mu 内自锁死）。
	// 会话检查已由 startJob 在进入 withAccount 之前完成。
	svcLog("[weekly] 开始（账号 %s）", config.AccountID())

	// 1. 云实验
	//
	// 先查任务是否已完成 —— 华为云实验每次开始都会消耗「今日免费名额」，
	// 重复跑纯属浪费（实测连续跑会把剩余名额从 114 掉到 113）。
	// 而且上次实验没正常结束时，再点「开始实验」会因为剩余时长状态卡住，
	// 页面拿完 quota 数据就停住不跳转，白等 180 秒还占一次名额。
	svcLog("[weekly] 步骤1/5 云实验：查询完成状态…")
	if sess, err := huawei.New(""); err == nil {
		if ms, err := huawei.TasksByStar(sess, 1); err == nil {
			for _, m := range ms {
				if strings.Contains(m.Title, "实验") && m.IsFinish == 1 {
					svcLog("[weekly] 云实验本周已完成，跳过（省一次免费名额）")
					steps = append(steps, map[string]any{
						"name": "云实验", "ok": true, "msg": "本周已完成，跳过（省一次免费名额）",
					})
					goto issueStep
				}
			}
			svcLog("[weekly] 云实验未完成，开始执行（可能要几分钟）")
		} else {
			svcLog("[weekly] 查询任务列表失败: %v（仍继续尝试执行）", err)
		}
	} else {
		svcLog("[weekly] 建立会话失败: %v（仍继续尝试执行）", err)
	}
	if raw, err := os.ReadFile(filepath.Join(config.DataDir(), "lab_record.txt")); err != nil {
		svcLog("[weekly] 云实验缺少 lab_record.txt")
		steps = append(steps, map[string]any{"name": "云实验", "ok": false, "msg": "缺少 data/lab_record.txt"})
	} else if res, err := task.RunLabTask(task.LabOptions{
		LabID:      config.DefaultLabID,
		RecordText: strings.TrimSpace(string(raw)),
		Headless:   true,
		End:        true,
		Timeout:    600,
	}); err != nil {
		svcLog("[weekly] 云实验失败: %v", err)
		steps = append(steps, map[string]any{"name": "云实验", "ok": false, "msg": err.Error()})
	} else {
		svcLog("[weekly] 云实验完成: %v", res)
		steps = append(steps, map[string]any{"name": "云实验", "ok": true, "msg": "已完成", "detail": res})
	}

issueStep:

	// 2. 提 Issue
	//
	// 同样先查任务状态。GitCode 提 Issue 是公开动作，本周已达标就不必再发，
	// 免得往仓库里刷重复内容。
	//
	// 注意不要在这里预先重登 GitCode：实测 refresh_token 有效期有几个月，
	// 页面加载时会自己换新 access_token，主动重登反而会触发网易易盾
	// 「拖白球避障」验证码（很难自动过），拖慢甚至搞挂整个周常。
	// 只有真正提交失败时才重登一次。
	svcLog("[weekly] 步骤2/5 提Issue：查询完成状态…")
	if s.weeklyMissionDone("Issue") {
		svcLog("[weekly] 提Issue 本周已完成，跳过")
		steps = append(steps, map[string]any{"name": "提Issue", "ok": true, "msg": "本周已完成，跳过"})
	} else if raw, err := os.ReadFile(filepath.Join(config.DataDir(), "issue_feature.md")); err != nil {
		svcLog("[weekly] 提Issue 缺少 issue_feature.md")
		steps = append(steps, map[string]any{"name": "提Issue", "ok": false, "msg": "缺少 data/issue_feature.md"})
	} else if title, body := parseBlogMarkdown(string(raw)); title == "" {
		steps = append(steps, map[string]any{"name": "提Issue", "ok": false, "msg": "内容缺少 # 标题"})
	} else if res, err := task.CreateIssue(title, body, "feature", true, false); err == nil {
		svcLog("[weekly] 提Issue 成功: %v", res["url"])
		steps = append(steps, map[string]any{"name": "提Issue", "ok": true, "msg": "已提交", "detail": res})
	} else if acc, ok := s.store.Get(config.AccountID()); ok && acc.GitCodeUser != "" && acc.GitCodePass != "" {
		svcLog("[weekly] 提Issue 首次失败(%v)，尝试 GitCode 重登", err)
		// 提交失败且存了账密 → 重登一次再试
		if _, lerr := task.LoginGitCodeAuto(acc.GitCodeUser, acc.GitCodePass, 180); lerr != nil {
			svcLog("[weekly] GitCode 重登失败: %v", lerr)
			steps = append(steps, map[string]any{
				"name": "提Issue", "ok": false,
				"msg": "提交失败(" + err.Error() + ")，GitCode 自动重登也失败: " + lerr.Error(),
			})
		} else if res, err2 := task.CreateIssue(title, body, "feature", true, false); err2 != nil {
			svcLog("[weekly] 重登后仍失败: %v", err2)
			steps = append(steps, map[string]any{
				"name": "提Issue", "ok": false,
				"msg": "重登后仍失败: " + err2.Error(),
			})
		} else {
			svcLog("[weekly] 重登后提Issue 成功: %v", res["url"])
			steps = append(steps, map[string]any{"name": "提Issue", "ok": true, "msg": "已提交（重登后）", "detail": res})
		}
	} else {
		svcLog("[weekly] 提Issue 失败: %v", err)
		steps = append(steps, map[string]any{"name": "提Issue", "ok": false, "msg": err.Error()})
	}

	// 4. Star 仓库
	//
	// 每次挑组织下第一个**还没 star 过**的仓库 —— 这样每周跑都会选到
	// 一个新的，不会重复 star 同一个。判据只能靠打开仓库页读按钮文字
	// （Star / Starred），接口侧拿不到已 star 状态，原因见 task.StarRepo 注释。
	svcLog("[weekly] 步骤3/5 Star仓库：查询完成状态…")
	if s.weeklyMissionDone("Star") {
		svcLog("[weekly] Star仓库本周已完成，跳过")
		steps = append(steps, map[string]any{"name": "Star仓库", "ok": true, "msg": "本周已完成，跳过"})
	} else if res, err := task.StarRepo(false); err != nil {
		svcLog("[weekly] Star仓库失败: %v", err)
		steps = append(steps, map[string]any{"name": "Star仓库", "ok": false, "msg": err.Error()})
	} else if ok, _ := res["ok"].(bool); ok {
		svcLog("[weekly] Star仓库成功: %v", res["repo"])
		steps = append(steps, map[string]any{"name": "Star仓库", "ok": true, "msg": "已 Star " + fmt.Sprint(res["repo"]), "detail": res})
	} else {
		svcLog("[weekly] Star仓库未完成: %v", res["result"])
		steps = append(steps, map[string]any{"name": "Star仓库", "ok": false, "msg": fmt.Sprint(res["result"])})
	}

	// 5. 开发者活动：报名一个进行中的活动
	svcLog("[weekly] 步骤4/5 开发者活动：查询完成状态…")
	if s.weeklyMissionDone("活动") {
		svcLog("[weekly] 开发者活动本周已完成，跳过")
		steps = append(steps, map[string]any{"name": "开发者活动", "ok": true, "msg": "本周已完成，跳过"})
	} else if res, err := task.JoinEvent(false); err != nil {
		svcLog("[weekly] 开发者活动失败: %v", err)
		steps = append(steps, map[string]any{"name": "开发者活动", "ok": false, "msg": err.Error()})
	} else if ok, _ := res["ok"].(bool); ok {
		svcLog("[weekly] 开发者活动成功: %v", res["activity"])
		steps = append(steps, map[string]any{"name": "开发者活动", "ok": true, "msg": "已报名", "detail": res})
	} else {
		svcLog("[weekly] 开发者活动未完成: %v", res["result"])
		steps = append(steps, map[string]any{"name": "开发者活动", "ok": false, "msg": fmt.Sprint(res["result"])})
	}

	// 6. 开发者赛事：报名 + 提交作品
	//
	// 链路较长（列表 → 报名表 → 作品提交页 → 上传 zip），
	// 实测要点：
	//   - 报名表职业必须选「其他」，选「企业员工」会让 readonly 的
	//     「公司」字段变必填，而该字段值来自账号资料，空值填不进去
	//   - 作品必须是 zip，每天最多上传 1 次
	svcLog("[weekly] 步骤5/5 开发者赛事：查询完成状态…")
	if s.weeklyMissionDone("赛事") {
		svcLog("[weekly] 开发者赛事本周已完成，跳过")
		steps = append(steps, map[string]any{"name": "开发者赛事", "ok": true, "msg": "本周已完成，跳过"})
	} else {
		svcLog("[weekly] 开发者赛事未完成，开始报名 + 提交作品（可能要几分钟）")
		zipPath := filepath.Join(config.DataDir(), "work_upload.zip")
		if res, err := task.RunEventTask(task.EventOptions{
			WorkZip:  zipPath,
			Headless: true,
			Timeout:  300,
		}); err != nil {
			svcLog("[weekly] 开发者赛事失败: %v", err)
			steps = append(steps, map[string]any{"name": "开发者赛事", "ok": false, "msg": err.Error()})
		} else {
			svcLog("[weekly] 开发者赛事完成: 报名=%v 作品=%v", res["signed_up"], res["uploaded"])
			steps = append(steps, map[string]any{"name": "开发者赛事", "ok": true, "msg": "已报名并提交作品", "detail": res})
		}
	}

	svcLog("[weekly] 结束，总耗时 %s", time.Since(wStart).Round(time.Second))
	return summarizeSteps("周常", steps)
}

// weeklyMissionDone 判断标题含 kw 的任务是否已完成。
//
// 用于周常前置检查：已经达标的步骤直接跳过，避免重复消耗配额
// （云实验每次开始都会掉一个免费名额）或往仓库刷重复 Issue。
//
// 坑：任务分散在不同星级里 —— 「每周实践一个实验」是 1 星，
// 「每周给…代码仓提 Issue」是 2 星。只查 1 星会漏掉提 Issue，
// 导致周常每次都多发一条重复 Issue。这里查全部星级。
//
// 查询失败时返回 false —— 宁可多跑一次，也不要因为接口抖动误判成已完成。
func (s *Server) weeklyMissionDone(kw string) bool {
	sess, err := huawei.New("")
	if err != nil {
		return false
	}
	ms, err := huawei.TasksByStar(sess, 1, 2, 3, 4, 5)
	if err != nil {
		return false
	}
	for _, m := range ms {
		if strings.Contains(m.Title, kw) && m.IsFinish == 1 {
			return true
		}
	}
	return false
}

// summarizeSteps 把多步执行结果汇总成一句话 + 明细。
func summarizeSteps(label string, steps []map[string]any) map[string]any {
	okN, failN := 0, 0
	for _, st := range steps {
		if b, _ := st["ok"].(bool); b {
			okN++
		} else {
			failN++
		}
	}
	msg := fmt.Sprintf("%s：成功 %d，失败 %d", label, okN, failN)
	return map[string]any{
		"ok":    failN == 0,
		"label": label,
		"msg":   msg,
		"steps": steps,
	}
}

// handleGitCodeCreds 设置账号的 GitCode 账密。
//
// 为什么存账密而不是只存 token：
//   GitCode 的 access token 会被服务端不定期作废（cookie 里 expires
//   写着 2027 年，实际已经 401）。只存 token 的话失效就得人工重登；
//   存账密才能在失效时自动重登，提 Issue 这种周常任务才不会断。
func (s *Server) handleGitCodeCreds(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		ID       string `json:"id"`
		User     string `json:"user"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	// 同样清洗：GitCode 用户名从别处复制时也会带不可见字符，
	// 导致「登录成功」但「绑定失败」（用户名对不上）。
	req.User = textutil.CleanUsername(req.User)
	req.Password = textutil.CleanPassword(req.Password)
	if req.ID == "" || req.User == "" || req.Password == "" {
		writeJSON(w, map[string]any{"error": "缺少 id / 用户名 / 密码"})
		return
	}
	if _, ok := s.store.Get(req.ID); !ok {
		writeJSON(w, map[string]any{"error": "账号不存在"})
		return
	}
	_ = s.store.Update(req.ID, func(a *account.Account) {
		a.GitCodeUser = req.User
		a.GitCodePass = req.Password
		// 换了用户名，之前的绑定状态就不作数了，清掉重新查
		a.GitCodeBindState = ""
		a.GitCodeBindMsg = ""
	})
	writeJSON(w, map[string]any{"ok": true})
}

// handleGitCodeBindState 查询某个账号的 GitCode → 华为云绑定状态。
//
// 为什么单独做这个接口：面板上原来只显示「GitCode: fewfeng」，
// 那只代表存了用户名，**不代表华为云认了这个绑定**。
// 没绑定的话 Issue 发得再多成长中心也显示未完成，
// 用户完全看不出来问题在哪。
//
// 返回：
//
//	state   bound | unbound | error | unknown
//	msg     失败原因或说明
//	at      查询时间
//
// 结果会写回账号，下次 /api/accounts 直接带出来，不用每次都查。
func (s *Server) handleGitCodeBindState(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		writeJSON(w, map[string]any{"error": "缺少 id"})
		return
	}
	acc, ok := s.store.Get(id)
	if !ok {
		writeJSON(w, map[string]any{"error": "账号不存在"})
		return
	}
	if acc.GitCodeUser == "" {
		writeJSON(w, map[string]any{
			"state": "unknown",
			"msg":   "还没有设置 GitCode 用户名",
		})
		return
	}

	// HohStatus 要读华为云会话，必须包在 withAccount 里切到对应账号目录
	var res map[string]any
	var err error
	s.withAccount(id, func() any {
		res, err = task.HohStatus()
		return nil
	})

	now := time.Now().Format("2006-01-02 15:04:05")

	if err != nil {
		// 会话失效是最常见的原因，直接说清楚，别让用户猜
		msg := err.Error()
		if strings.Contains(msg, "会话") || strings.Contains(msg, "登录") ||
			strings.Contains(msg, "cookie") || strings.Contains(msg, "401") {
			msg = "华为云会话失效，无法查询绑定状态：" + msg
		}
		_ = s.store.Update(id, func(a *account.Account) {
			a.GitCodeBindState = "error"
			a.GitCodeBindMsg = msg
			a.GitCodeBindAt = now
		})
		writeJSON(w, map[string]any{"state": "error", "msg": msg, "at": now})
		return
	}

	bound, _ := res["bound"].(bool)
	state := "unbound"
	msg := "GitCode 账号未绑定到该华为云账号 —— Issue / Star 不会计入成长中心任务"
	if bound {
		state = "bound"
		msg = ""
	}

	// 顺带核对一下绑定的用户名和本地存的是否一致。
	// 不一致说明账号里存的是另一个 GitCode 号，提 Issue 会用错身份。
	if b, ok := res["binding"].(map[string]any); ok {
		if gu, _ := b["gitcode_username"].(string); gu != "" && gu != acc.GitCodeUser {
			msg = fmt.Sprintf("华为云绑定的 GitCode 是 %s，但本账号存的是 %s，提 Issue 会用错身份", gu, acc.GitCodeUser)
			state = "mismatch"
		}
	}

	_ = s.store.Update(id, func(a *account.Account) {
		a.GitCodeBindState = state
		a.GitCodeBindMsg = msg
		a.GitCodeBindAt = now
	})
	writeJSON(w, map[string]any{
		"state":    state,
		"msg":      msg,
		"at":       now,
		"username": res["username"],
		"binding":  res["binding"],
	})
}
