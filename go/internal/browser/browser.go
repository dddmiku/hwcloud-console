package browser

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/playwright-community/playwright-go"

	"hwcloud/internal/config"
	"hwcloud/internal/fsutil"
)

func EnsureDriver() error {
	if os.Getenv("PLAYWRIGHT_DRIVER_PATH") != "" {
		return nil
	}
	// 优先使用项目内置的 driver（兼容 hwcloud-go/、go/ 与根目录布局）
	candidates := []string{
		filepath.Join(config.Root(), "hwcloud-go", "third_party", "playwright-driver"),
		filepath.Join(config.Root(), "go", "third_party", "playwright-driver"),
		filepath.Join(config.Root(), "third_party", "playwright-driver"),
	}
	for _, local := range candidates {
		if _, err := os.Stat(filepath.Join(local, "package", "cli.js")); err == nil {
			_ = os.Setenv("PLAYWRIGHT_DRIVER_PATH", local)
			return nil
		}
	}
	return playwright.Install(&playwright.RunOptions{SkipInstallBrowsers: true, Verbose: false})
}

func Launch(pw *playwright.Playwright, headless bool) (playwright.Browser, error) {
	ReapOrphanBrowsers() // 见 reap.go
	opts := playwright.BrowserTypeLaunchOptions{
		Headless: playwright.Bool(headless),
		Args:     []string{"--disable-blink-features=AutomationControlled"},
	}
	// headless shell 是独立下载项，只装了完整 Chromium 的环境（如服务器）会因缺少它
	// 直接启动失败；指定 channel=chromium 走完整浏览器的新无头实现。
	if headless {
		opts.Channel = playwright.String("chromium")
	}
	return pw.Chromium.Launch(opts)
}

func chromeExists() bool {
	for _, base := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), os.Getenv("LOCALAPPDATA")} {
		if base == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(base, "Google", "Chrome", "Application", "chrome.exe")); err == nil {
			return true
		}
	}
	return false
}

// LaunchPersistent 优先使用系统 Chrome（可复用旧 profile），失败时回退到内置 Chromium。
func LaunchPersistent(pw *playwright.Playwright, dir string, headless bool) (playwright.BrowserContext, error) {
	return LaunchPersistentUA(pw, dir, headless, config.UserAgent())
}

// LaunchPersistentUA 同 LaunchPersistent，但由调用方指定 UA。
//
// 为什么需要它：config.UserAgent() 读的是华为云登录时记下的 UA（Firefox），
// 而这里启动的是 Chromium。UA 说自己是 Firefox、实际却是 Chromium 的
// TLS/JA3 和 JS 指纹，GitCode 的 WAF 会直接判为伪造请求并返回 418
// （"Sorry, your request has been intercepted..."）。GitCode 必须用
// 与浏览器一致的 Chrome UA。
func LaunchPersistentUA(pw *playwright.Playwright, dir string, headless bool, ua string) (playwright.BrowserContext, error) {
	// 启动前先收一遍超龄的孤儿浏览器。
	// 详见 reap.go：defer 防不住 kill -9 / OOM，残留的 Chromium
	// 每个占 200-400 MB，攒几个就把小机器拖垮。
	ReapOrphanBrowsers()

	opts := playwright.BrowserTypeLaunchPersistentContextOptions{
		Headless: playwright.Bool(headless),
		Viewport: &playwright.Size{Width: 1280, Height: 900},
		Args:     []string{"--disable-blink-features=AutomationControlled"},
		// 华为云 WAF 按 UA 指纹判定：无头 UA 带 "HeadlessChrome"，会被直接回 404，
		// 表现为「get-ainfo 拿不到 csrf」从而误判成会话失效。这里统一成本地登录时的 UA。
		UserAgent: playwright.String(ua),
	}
	if headless {
		opts.Channel = playwright.String("chromium")
	}
	if chromeExists() {
		opts.Channel = playwright.String("chrome")
		if ctx, err := pw.Chromium.LaunchPersistentContext(dir, opts); err == nil {
			return ctx, nil
		}
	}
	// 回退到内置 Chromium。注意 headless 时必须保留 Channel="chromium"：
	// 置空会退到 chrome-headless-shell，而它默认不随 chromium 一起安装。
	if headless {
		opts.Channel = playwright.String("chromium")
	} else {
		opts.Channel = nil
	}
	return pw.Chromium.LaunchPersistentContext(dir, opts)
}

func NewHuaweiContext(pw *playwright.Playwright, headless bool) (playwright.Browser, playwright.BrowserContext, error) {
	br, err := Launch(pw, headless)
	if err != nil {
		return nil, nil, err
	}
	ctx, err := br.NewContext(playwright.BrowserNewContextOptions{
		Viewport:  &playwright.Size{Width: 1600, Height: 1000},
		UserAgent: playwright.String(config.UserAgent()),
	})
	if err != nil {
		br.Close()
		return nil, nil, err
	}
	cookies, err := loadCookies(config.CookieFile())
	if err != nil {
		br.Close()
		return nil, nil, err
	}
	if err := ctx.AddCookies(cookies); err != nil {
		br.Close()
		return nil, nil, err
	}
	return br, ctx, nil
}

func SeedCookies(ctx playwright.BrowserContext, path string) error {
	cookies, err := loadCookies(path)
	if err != nil || len(cookies) == 0 {
		return err
	}
	return ctx.AddCookies(cookies)
}

func NewStorageContext(pw *playwright.Playwright, stateFile string, headless bool) (playwright.Browser, playwright.BrowserContext, error) {
	return NewStorageContextUA(pw, stateFile, headless, config.DefaultUA)
}

// NewStorageContextUA 同 NewStorageContext，但由调用方指定 UA。
// 用途见 LaunchPersistentUA：Chromium 必须配 Chrome UA。
func NewStorageContextUA(pw *playwright.Playwright, stateFile string, headless bool, ua string) (playwright.Browser, playwright.BrowserContext, error) {
	br, err := Launch(pw, headless)
	if err != nil {
		return nil, nil, err
	}

	// StorageStatePath 要求 Playwright 标准格式：
	//   {"cookies":[...], "origins":[...]}
	//
	// 但项目里有两类文件都叫 "state"：
	//   gitcode_state.json  —— Playwright 导出的，本来就是标准格式
	//   cookies.json        —— 我们自己存的，是**裸数组** [{name,value,...}]
	//
	// 直接把裸数组传给 StorageStatePath 会报：
	//   could not parse storage state file:
	//   json: cannot unmarshal array into Go value of type playwright.OptionalStorageState
	//
	// 所以这里先探一下文件头：是 '[' 就转成标准格式再传给 Playwright。
	opts := playwright.BrowserNewContextOptions{
		Viewport:  &playwright.Size{Width: 1440, Height: 1100},
		UserAgent: playwright.String(ua),
	}

	if st, ok := normalizeStorageState(stateFile); ok {
		opts.StorageState = st
	} else {
		opts.StorageStatePath = playwright.String(stateFile)
	}

	ctx, err := br.NewContext(opts)
	if err != nil {
		br.Close()
		return nil, nil, err
	}
	return br, ctx, nil
}

// normalizeStorageState 把裸 cookie 数组转成 Playwright 的 storage state 结构。
//
// 返回 (state, true) 表示转换成功；
// 返回 (nil, false) 表示文件已是标准格式或读取失败，调用方应走 StorageStatePath。
func normalizeStorageState(path string) (*playwright.OptionalStorageState, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, false // 已是对象格式，交给 Playwright 自己读
	}

	var cookies []cookieFile
	if err := json.Unmarshal(trimmed, &cookies); err != nil {
		return nil, false
	}

	state := &playwright.OptionalStorageState{}
	for _, c := range cookies {
		if c.Domain == "" {
			continue // 没 domain 的 cookie 无法注入
		}
		p := c.Path
		if p == "" {
			p = "/"
		}
		oc := playwright.OptionalCookie{
			Name:   c.Name,
			Value:  c.Value,
			Domain: playwright.String(c.Domain),
			Path:   playwright.String(p),
		}
		if c.Expires > 0 {
			oc.Expires = playwright.Float(c.Expires)
		}
		state.Cookies = append(state.Cookies, oc)
	}
	return state, true
}

type cookieFile struct {
	Name    string  `json:"name"`
	Value   string  `json:"value"`
	Domain  string  `json:"domain"`
	Path    string  `json:"path"`
	Expires float64 `json:"expires"`
}

func loadCookies(path string) ([]playwright.OptionalCookie, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var list []cookieFile
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, err
	}
	out := make([]playwright.OptionalCookie, 0, len(list))
	for _, c := range list {
		if c.Name == "" {
			continue
		}
		item := playwright.OptionalCookie{Name: c.Name, Value: c.Value}
		if c.Domain != "" {
			item.Domain = playwright.String(c.Domain)
		}
		if c.Path != "" {
			item.Path = playwright.String(c.Path)
		}
		if c.Expires > 0 {
			item.Expires = playwright.Float(c.Expires)
		}
		out = append(out, item)
	}
	return out, nil
}

type savedCookie struct {
	Name    string  `json:"name"`
	Value   string  `json:"value"`
	Domain  string  `json:"domain"`
	Path    string  `json:"path"`
	Expires float64 `json:"expires"`
}

func SaveContextCookies(ctx playwright.BrowserContext, path string) error {
	cookies, err := ctx.Cookies()
	if err != nil {
		return err
	}
	out := make([]savedCookie, 0, len(cookies))
	for _, c := range cookies {
		out = append(out, savedCookie{
			Name:    c.Name,
			Value:   c.Value,
			Domain:  c.Domain,
			Path:    c.Path,
			Expires: c.Expires,
		})
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(path, data, 0o600)
}