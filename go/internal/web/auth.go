package web

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"hwcloud/internal/config"
)

// auth 管理面板的登录认证。
//
// 设计：
//   - 账号密码存在 state/panel_users.json，密码用 SHA-256 + 盐哈希
//   - 登录成功后签发 token，同时写入内存和磁盘（重启不掉线）
//   - token 有效期 30 天
//   - 首次启动若没有任何用户，允许通过 /api/auth/setup 创建
type auth struct {
	mu     sync.RWMutex
	users  map[string]string    // username -> 密码哈希
	tokens map[string]tokenInfo // token -> 信息

	// loadErr 记录账号表/令牌表读取失败的原因。
	//
	// 非 nil 时整个认证系统进入 fail-closed 状态：受保护接口返回 500，
	// /api/auth/setup 拒绝创建管理员。这是为了防止「文件损坏 → 当成
	// 没有用户 → 任何人可注册管理员」这条提权路径。
	loadErr error

	// setupMu 串行化 hasUsers + addUser，防止并发 setup 竞态。
	setupMu sync.Mutex
}

type tokenInfo struct {
	Username string    `json:"username"`
	Expires  time.Time `json:"expires"`
}

const (
	usersFile  = "panel_users.json"
	tokensFile = "panel_tokens.json"
	cookieName = "hwcloud_session"
	tokenTTL   = 30 * 24 * time.Hour // 30 天
)

func newAuth() *auth {
	a := &auth{
		users:  map[string]string{},
		tokens: map[string]tokenInfo{},
	}
	a.loadUsers()
	a.loadTokens()
	go a.gcLoop()
	return a
}

func (a *auth) usersPath() string  { return filepath.Join(config.Root(), "state", usersFile) }
func (a *auth) tokensPath() string { return filepath.Join(config.Root(), "state", tokensFile) }

// loadUsers 读管理员账号表。
//
// 关键：必须区分「文件不存在（首次启动）」和「文件存在但损坏」。
//
// 原来两种情况都让 users 为空，而 hasUsers()==false 会让未鉴权的
// /api/auth/setup 重新开放管理员创建 —— 也就是说一次非原子写导致的
// 文件截断（磁盘满、断电），就把面板变成「任何人可创建管理员」。
// 这个面板以 root 运行，后果是整机沦陷。
//
// 现在损坏时设置 loadErr，requireAuth 与 handleAuthSetup 一律拒绝
// （fail closed），并提示管理员手动修复，而不是静默重开注册。
func (a *auth) loadUsers() {
	data, err := os.ReadFile(a.usersPath())
	if err != nil {
		if os.IsNotExist(err) {
			return // 首次启动，正常
		}
		a.loadErr = fmt.Errorf("读取 %s 失败: %w", usersFile, err)
		return
	}
	var v struct {
		Users map[string]string `json:"users"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		a.loadErr = fmt.Errorf("%s 解析失败（文件可能已损坏）: %w", usersFile, err)
		return
	}
	if v.Users != nil {
		a.users = v.Users
	}
}

// saveUsers 原子写账号表。
//
// 直接 os.WriteFile 在写入过程中崩溃会留下半截 JSON，下次启动就会
// 触发上面的 loadErr 分支。用临时文件 + fsync + rename 保证原子性。
func (a *auth) saveUsers() error {
	a.mu.RLock()
	cp := make(map[string]string, len(a.users))
	for k, v := range a.users {
		cp[k] = v
	}
	a.mu.RUnlock()

	data, err := json.MarshalIndent(map[string]any{"users": cp}, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化账号表失败: %w", err)
	}
	return writeFileAtomic(a.usersPath(), data, 0o600)
}

func (a *auth) loadTokens() {
	data, err := os.ReadFile(a.tokensPath())
	if err != nil {
		return
	}
	var v struct {
		Tokens map[string]tokenInfo `json:"tokens"`
	}
	if json.Unmarshal(data, &v) == nil && v.Tokens != nil {
		now := time.Now()
		for k, t := range v.Tokens {
			if now.Before(t.Expires) {
				a.tokens[k] = t
			}
		}
	}
}

func (a *auth) saveTokens() {
	a.mu.RLock()
	cp := make(map[string]tokenInfo, len(a.tokens))
	for k, v := range a.tokens {
		cp[k] = v
	}
	a.mu.RUnlock()

	data, err := json.MarshalIndent(map[string]any{"tokens": cp}, "", "  ")
	if err != nil {
		return
	}
	// 令牌表损坏不会导致提权（最坏是所有人掉线重登），
	// 但仍然用原子写，避免半截文件让下次启动解析失败。
	_ = writeFileAtomic(a.tokensPath(), data, 0o600)
}

// hasUsers 返回是否已创建任何账号。
func (a *auth) hasUsers() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.users) > 0
}

// broken 返回认证系统是否处于损坏状态（账号表读不出来）。
//
// 损坏时必须 fail closed：既不能让已登录用户继续操作，也不能让任何人
// 通过 /api/auth/setup 重新创建管理员。
func (a *auth) broken() error {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.loadErr
}

// setup 原子地完成「检查是否已有用户 + 创建第一个用户」。
//
// 原来 handleAuthSetup 里 hasUsers() 与 addUser() 是两次独立加锁，
// 两个并发请求能同时通过检查，后写入者覆盖前者；服务先上线后初始化
// 的窗口期内，任何能连上端口的人都能把自己设成管理员。
func (a *auth) setup(username, password string) error {
	a.setupMu.Lock()
	defer a.setupMu.Unlock()

	if err := a.broken(); err != nil {
		return fmt.Errorf("账号表损坏，拒绝初始化: %w", err)
	}
	if a.hasUsers() {
		return fmt.Errorf("已存在管理员账号，不能重复初始化")
	}
	return a.addUser(username, password)
}

// addUser 新增账号。
func (a *auth) addUser(username, password string) error {
	a.mu.Lock()
	a.users[username] = hashPassword(username, password)
	a.mu.Unlock()
	return a.saveUsers()
}

// check 校验账号密码。
func (a *auth) check(username, password string) bool {
	a.mu.RLock()
	want, ok := a.users[username]
	a.mu.RUnlock()
	if !ok {
		return false
	}
	got := hashPassword(username, password)
	return subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1
}

// issue 签发 token。
func (a *auth) issue(username string) string {
	b := make([]byte, 32)
	rand.Read(b)
	tok := hex.EncodeToString(b)

	a.mu.Lock()
	a.tokens[tok] = tokenInfo{Username: username, Expires: time.Now().Add(tokenTTL)}
	a.mu.Unlock()
	a.saveTokens()
	return tok
}

// lookup 校验 token 并返回用户名。
func (a *auth) lookup(tok string) (string, bool) {
	if tok == "" {
		return "", false
	}
	a.mu.RLock()
	info, ok := a.tokens[tok]
	a.mu.RUnlock()
	if !ok || time.Now().After(info.Expires) {
		return "", false
	}
	return info.Username, true
}

func (a *auth) revoke(tok string) {
	a.mu.Lock()
	delete(a.tokens, tok)
	a.mu.Unlock()
	a.saveTokens()
}

// expireInfo 返回 token 的剩余时间。
func (a *auth) expireInfo(tok string) (time.Time, bool) {
	a.mu.RLock()
	info, ok := a.tokens[tok]
	a.mu.RUnlock()
	return info.Expires, ok
}

func (a *auth) gcLoop() {
	t := time.NewTicker(1 * time.Hour)
	defer t.Stop()
	for range t.C {
		now := time.Now()
		a.mu.Lock()
		changed := false
		for k, info := range a.tokens {
			if now.After(info.Expires) {
				delete(a.tokens, k)
				changed = true
			}
		}
		a.mu.Unlock()
		if changed {
			a.saveTokens()
		}
	}
}

// requireAuth 是中间件：未登录返回 401。
func (a *auth) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 账号表损坏 → fail closed。绝不能当成「没有用户」放行 setup。
		if err := a.broken(); err != nil {
			http.Error(w, "面板账号数据损坏，请检查 state/panel_users.json："+err.Error(),
				http.StatusInternalServerError)
			return
		}
		if !a.hasUsers() {
			writeJSONStatus(w, http.StatusForbidden,
				map[string]any{"error": "未初始化", "need_setup": true})
			return
		}
		c, err := r.Cookie(cookieName)
		if err != nil {
			writeJSONStatus(w, http.StatusUnauthorized,
				map[string]any{"error": "未登录", "need_login": true})
			return
		}
		if _, ok := a.lookup(c.Value); !ok {
			writeJSONStatus(w, http.StatusUnauthorized,
				map[string]any{"error": "登录已过期", "need_login": true})
			return
		}
		next(w, r)
	}
}

// hashPassword 用用户名做盐，避免相同密码产生相同哈希。
func hashPassword(username, pw string) string {
	sum := sha256.Sum256([]byte("hwcloud-panel:" + username + ":" + pw))
	return hex.EncodeToString(sum[:])
}

func cookieOpts() (name string, maxAge int, httpOnly, secure bool, path string, sameSite http.SameSite) {
	return cookieName, int(tokenTTL.Seconds()), true, false, "/", http.SameSiteLaxMode
}

func clientIP(r *http.Request) string {
	if xf := r.Header.Get("X-Forwarded-For"); xf != "" {
		if i := strings.Index(xf, ","); i > 0 {
			return strings.TrimSpace(xf[:i])
		}
		return strings.TrimSpace(xf)
	}
	if i := strings.LastIndex(r.RemoteAddr, ":"); i > 0 {
		return r.RemoteAddr[:i]
	}
	return r.RemoteAddr
}

// fmtExpire 格式化剩余时间。
func fmtExpire(t time.Time) string {
	d := time.Until(t)
	if d <= 0 {
		return "已过期"
	}
	days := int(d.Hours() / 24)
	if days >= 1 {
		return fmt.Sprintf("%d 天后过期", days)
	}
	return fmt.Sprintf("%d 小时后过期", int(d.Hours())+1)
}

// writeFileAtomic 原子写文件：临时文件 + fsync + rename。
//
// 为什么整个包都用它：直接 os.WriteFile 在写入过程中被 kill 或断电，
// 会留下半截文件。对 panel_users.json 这种「解析失败 = 没有用户 =
// 开放注册」的文件，截断等于提权漏洞；对 cookies.json 则是会话永久失效。
//
// rename 在同一文件系统内是原子的：读者要么看到旧内容，要么看到新内容，
// 不会看到中间态。fsync 保证 rename 之前数据真的落盘。
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// rename 成功后 Remove 是空操作；失败路径负责清理
	defer os.Remove(tmpName)

	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
