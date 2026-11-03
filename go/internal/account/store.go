package account

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"hwcloud/internal/config"
	"hwcloud/internal/textutil"
	"hwcloud/internal/fsutil"
)

// Account 描述一个华为云账号的配置与状态。
type Account struct {
	ID       string `json:"id"`        // 目录名，通常是手机号
	Phone    string `json:"phone"`
	Password string `json:"password,omitempty"` // 明文存储，仅本机使用

	// Remark 是用户自定义的备注，用来区分多个账号。
	//
	// 为什么需要：账号多了以后，光看手机号根本分不清哪个是主号、
	// 哪个是测试号、哪个属于哪个项目。备注让用户自己打标签。
	// 纯展示用，不参与任何业务逻辑。
	Remark string `json:"remark,omitempty"`

	// AutoKeepAlive 控制是否允许自动账密登录（保活）。
	//
	// 为什么需要这个开关：
	//   开了双重验证（MFA）的账号，自动登录走到发短信那一步就卡住了 ——
	//   服务端确实把短信发出去了，但没人填验证码，会话永远建不起来。
	//   于是每次保活都重新触发一次发短信，用户手机上被反复轰炸，
	//   而且他自己完全不知道是后台在发（面板上只显示「自动登录失败」）。
	//
	// 关掉之后，保活只做纯协议续期（refresh token），
	// 不会再拉起浏览器、不会发短信。会话真过期了就老老实实标记
	// 「需要人工登录」，等用户自己点「重新登录」。
	//
	// 用指针是为了区分「没设置过」和「显式关掉」：
	// 老账号的 account.json 里没有这个字段，读出来是 nil，
	// 按默认值 true 处理（保持原有行为不变）。
	AutoKeepAlive *bool `json:"auto_keep_alive,omitempty"`

	// 设备信任相关
	HwidCasSid string `json:"hwid_cas_sid,omitempty"`
	Trusted    bool   `json:"trusted"`
	TrustedAt  string `json:"trusted_at,omitempty"`

	// GitCode 凭据。
	// 提 Issue 需要 GitCode 会话，而 GitCode 的 access token 会被服务端
	// 不定期作废（cookie 的 expires 还写着 2027 年，实际已经 401）。
	// 存一份账密，失效时能自动重登，不用人工扫码。
	GitCodeUser string `json:"gitcode_user,omitempty"`
	GitCodePass string `json:"gitcode_pass,omitempty"`

	// GitCode 绑定到华为云的状态。
	//
	// 为什么不能只看 GitCodeUser 有没有值：那只代表「存了用户名」，
	// 和「华为云那边认了这个绑定」是两件事。不绑定的话，Issue 发得再多
	// 成长中心也一直显示未完成（3022 实测过）。
	//
	// 三态，前端要分别显示：
	//   ""         还没查过（刚加账号）
	//   "bound"    已绑定
	//   "unbound"  未绑定
	//   "error"    查询失败（通常是华为云会话失效）
	GitCodeBindState string `json:"gitcode_bind_state,omitempty"`
	// GitCodeBindAt 是最后一次查询/绑定的时间
	GitCodeBindAt string `json:"gitcode_bind_at,omitempty"`
	// GitCodeBindMsg 存失败原因，方便前端直接展示
	GitCodeBindMsg string `json:"gitcode_bind_msg,omitempty"`

	// 状态（运行时更新）
	Alive     bool    `json:"alive"`
	Username  string  `json:"username,omitempty"`
	RemainDays float64 `json:"remain_days,omitempty"`
	LastCheck string  `json:"last_check,omitempty"`
	LastError string  `json:"last_error,omitempty"`

	// 任务执行记录
	LastSign     string `json:"last_sign,omitempty"`
	LastDevSpace string `json:"last_devspace,omitempty"`
	LastBlog     string `json:"last_blog,omitempty"`
}

// Store 管理多账号。
type Store struct {
	mu   sync.RWMutex
	dir  string
	list []*Account
}

// NewStore 创建账号存储。
func NewStore() (*Store, error) {
	dir := filepath.Join(config.Root(), "accounts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{dir: dir}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// Dir 返回账号根目录。
func (s *Store) Dir() string { return s.dir }

// AccountDir 返回某个账号的数据目录。
func (s *Store) AccountDir(id string) string {
	return filepath.Join(s.dir, sanitize(id))
}

func sanitize(id string) string {
	r := strings.NewReplacer("/", "_", "\\", "_", "..", "_", ":", "_")
	return r.Replace(id)
}

// load 从磁盘读取所有账号。
func (s *Store) load() error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(s.dir, e.Name(), "account.json")
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var a Account
		if err := json.Unmarshal(data, &a); err != nil {
			// 损坏的文件不能静默跳过。
			//
			// 原来这里是 `continue` —— 损坏账号直接从列表消失，用户以为
			// 账号被删了；而重新添加同一手机号会走到 persistLocked，
			// 直接把损坏文件覆盖掉，原始数据永久丢失。
			//
			// 现在把损坏文件改名备份（不删），并记录到 LastError，
			// 用户能在界面上看到「某个账号数据损坏」，也能手工修复。
			bad := p + ".corrupt-" + time.Now().Format("20060102-150405")
			if rerr := os.Rename(p, bad); rerr != nil {
				bad = p // 改名失败就原地保留，至少不覆盖
			}
			a = Account{
				ID:        e.Name(),
				LastError: "account.json 损坏，已备份到 " + filepath.Base(bad),
			}
			s.list = append(s.list, &a)
			continue
		}
		if a.ID == "" {
			a.ID = e.Name()
		}
		s.list = append(s.list, &a)
	}
	sort.Slice(s.list, func(i, j int) bool { return s.list[i].ID < s.list[j].ID })
	return nil
}

// List 返回所有账号的副本。
func (s *Store) List() []Account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Account, 0, len(s.list))
	for _, a := range s.list {
		out = append(out, *a)
	}
	return out
}

// Get 按 ID 查找账号。
func (s *Store) Get(id string) (*Account, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, a := range s.list {
		if a.ID == id {
			cp := *a
			return &cp, true
		}
	}
	return nil, false
}

// Add 新增账号。
func (s *Store) Add(phone, password string) (*Account, error) {
	// 兜底清洗：即使调用方漏了，也别把带不可见字符的凭据存进来。
	// 存进去之后每次登录都会失败，而界面上看不出任何异常。
	phone = textutil.CleanPhone(phone)
	password = textutil.CleanPassword(password)
	if phone == "" {
		return nil, fmt.Errorf("手机号不能为空")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, a := range s.list {
		if a.Phone == phone {
			// 已存在则更新密码
			a.Password = password
			return a, s.persistLocked(a)
		}
	}

	a := &Account{
		ID:       sanitize(phone),
		Phone:    phone,
		Password: password,
	}
	s.list = append(s.list, a)
	sort.Slice(s.list, func(i, j int) bool { return s.list[i].ID < s.list[j].ID })

	// 初始化账号目录，并把全局的浏览器指纹复制过去。
	// fp / hwmeta 是浏览器级标识，所有账号可以共用同一份。
	if err := s.initAccountDir(a.ID); err != nil {
		return a, err
	}
	return a, s.persistLocked(a)
}

// initAccountDir 创建账号目录结构，并复制共享的浏览器指纹。
func (s *Store) initAccountDir(id string) error {
	dir := filepath.Join(s.dir, id)
	stateDir := filepath.Join(dir, "state")
	for _, d := range []string{dir, stateDir, filepath.Join(dir, "logs")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}

	// 从全局 state 复制指纹（如果账号目录里还没有）
	globalState := filepath.Join(config.Root(), "state")
	for _, name := range []string{"fp_cache.txt", "hwmeta_cache.txt"} {
		dst := filepath.Join(stateDir, name)
		if _, err := os.Stat(dst); err == nil {
			continue // 已存在
		}
		src := filepath.Join(globalState, name)
		if data, err := os.ReadFile(src); err == nil {
			_ = fsutil.WriteFileAtomic(dst, data, 0o600)
		}
	}
	return nil
}

// PrepareDir 只为某个手机号创建目录并复制共享指纹，但不加入账号列表。
//
// 用途：添加账号时先跑登录流程，此时账号还不该出现在列表里，
// 但登录过程要往 accounts/<phone>/state 写 cookie，所以目录必须先存在。
func (s *Store) PrepareDir(phone string) (string, error) {
	id := sanitize(strings.TrimSpace(phone))
	if id == "" {
		return "", fmt.Errorf("手机号不能为空")
	}
	if err := s.initAccountDir(id); err != nil {
		return "", err
	}
	return id, nil
}

// Update 更新账号字段。
func (s *Store) Update(id string, fn func(*Account)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.list {
		if a.ID == id {
			fn(a)
			return s.persistLocked(a)
		}
	}
	return fmt.Errorf("账号不存在: %s", id)
}

// Remove 删除账号及其数据。
func (s *Store) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, a := range s.list {
		if a.ID == id {
			s.list = append(s.list[:i], s.list[i+1:]...)
			return os.RemoveAll(filepath.Join(s.dir, id))
		}
	}
	return fmt.Errorf("账号不存在: %s", id)
}

// persistLocked 写入 account.json（调用方需持有锁）。
//
// 用「临时文件 + rename」而不是直接 os.WriteFile：
// 后者在写入过程中断电/被 kill 会留下半截 JSON，下次启动解析失败，
// 那个账号的所有配置（密码、GitCode 凭据、积分缓存）就全丢了。
// rename 在同一文件系统内是原子的，要么是旧内容要么是新内容。
//
// 目录权限用 0700：account.json 是 0600，但目录若是 0755，
// 同机其他用户仍能 ls 出账号 ID（手机号）。文件内容读不到，
// 但账号列表本身也是敏感信息。
func (s *Store) persistLocked(a *Account) error {
	dir := filepath.Join(s.dir, a.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// 已存在的目录不会被 MkdirAll 改权限，显式收紧一次
	_ = os.Chmod(dir, 0o700)

	data, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}

	target := filepath.Join(dir, "account.json")
	tmp, err := os.CreateTemp(dir, ".account-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // rename 成功后这里是空操作

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	// fsync 再 rename：只 rename 不 fsync 时，断电可能留下「文件已改名
	// 但内容还在页缓存」的状态，rename 的原子性保证不到内容落盘。
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, target)
}

// Persist 保存指定账号（带锁版本，供外部调用）。
func (s *Store) Persist(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.list {
		if a.ID == id {
			return s.persistLocked(a)
		}
	}
	return fmt.Errorf("账号不存在: %s", id)
}

// MarkChecked 更新检查状态。
func (s *Store) MarkChecked(id string, alive bool, username string, remainDays float64, errMsg string) {
	_ = s.Update(id, func(a *Account) {
		a.Alive = alive
		if username != "" {
			a.Username = username
		}
		a.RemainDays = remainDays
		a.LastCheck = time.Now().Format("2006-01-02 15:04:05")
		a.LastError = errMsg
	})
}

// MaskPhone 返回脱敏手机号：157******41。
func MaskPhone(p string) string {
	if len(p) != 11 {
		if len(p) > 4 {
			return p[:2] + strings.Repeat("*", len(p)-4) + p[len(p)-2:]
		}
		return p
	}
	return p[:3] + "******" + p[9:]
}

// MaskedPhone 返回账号的脱敏手机号。
// AutoKeepAliveOn 返回是否允许自动账密登录，默认 true。
//
// 字段是指针：老账号的 account.json 里没有这一项，读出来是 nil，
// 这里按 true 处理，行为和加开关之前完全一致。
func (a *Account) AutoKeepAliveOn() bool {
	if a.AutoKeepAlive == nil {
		return true
	}
	return *a.AutoKeepAlive
}

func (a *Account) MaskedPhone() string {
	return MaskPhone(a.Phone)
}

// MarkTask 记录任务执行时间。
func (s *Store) MarkTask(id, task string) {
	_ = s.Update(id, func(a *Account) {
		ts := time.Now().Format("2006-01-02 15:04:05")
		switch task {
		case "sign":
			a.LastSign = ts
		case "devspace":
			a.LastDevSpace = ts
		case "blog":
			a.LastBlog = ts
		}
	})
}
