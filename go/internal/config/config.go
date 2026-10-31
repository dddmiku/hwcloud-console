package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	DevData         = "https://devdata.huaweicloud.com"
	GrowURL         = "https://developer.huaweicloud.com/grow"
	EduLabDetail    = "https://edu.huaweicloud.com/lab/experiment-detail_%d"
	BlogEditorURL   = "https://bbs.huaweicloud.com/blogs/article"
	GitCodeRepo     = "agent-best-practices/eduSwarm"
	// GitCode 的组织路径。Star 任务从这个组织下所有仓库里挑一个没 star 过的，
	// 所以只取组织名，不写死具体仓库。
	GitCodeOrg = "agent-best-practices"
	// GitCode 的新建 Issue 入口是 /issues/create（会跳到 /issues/create/choose 模板页）。
	// /issues/new 已被废弃：会 302 回 issues 列表，导致「找不到标题输入框」。
	GitCodeIssuesNew = "https://gitcode.com/" + GitCodeRepo + "/issues/create"
	DefaultUA       = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36"
	DefaultLabID    = 2325
)

var API = map[string]string{
	"csrf":       "/api/get-ainfo",
	"personal":   "/rest/developer/fwdu/rest/developer/user/hdcommunityservice/v1/member/get-personal-info",
	"sign":       "/rest/developer/fwdu/rest/developer/user/hdcommunitysoservice/v1/growth/sign",
	"missions":   "/rest/developer/fwdu/rest/developer/user/hdcommunitysoservice/v1/growth/mission/query-mission-list-by-condition",
	"beans":      "/rest/developer/fwdu/rest/developer/user/hdcommunitysoservice/v1/growth/bonus-beans",
	"devInfo":    "/rest/developer/fwdu/rest/developer/user/hdcommunitysoservice/v1/activity/get-developer-info",
}

var (
	rootOnce sync.Once
	rootPath string

	accountMu sync.RWMutex
	accountID string
)

// SetAccount 切换到指定账号的状态目录。
//
// 多账号场景下每个账号有独立的 state（cookie、fp、sid），
// 避免互相覆盖。传空字符串表示回到全局默认目录。
func SetAccount(id string) {
	accountMu.Lock()
	defer accountMu.Unlock()
	accountID = id
}

// AccountID 返回当前账号 ID。
//
// 优先级：SetAccount 显式设置 > HWCLOUD_ACCOUNT 环境变量。
// 环境变量这条是给命令行工具用的——keepalive 脚本要按账号目录
// 逐个续期，没法在同一个进程里调 SetAccount。
func AccountID() string {
	accountMu.RLock()
	id := accountID
	accountMu.RUnlock()
	if id != "" {
		return id
	}
	return strings.TrimSpace(os.Getenv("HWCLOUD_ACCOUNT"))
}

// Root 依次通过 HWCLOUD_HOME、当前目录向上查找 state/data、可执行文件目录定位项目根目录。
func Root() string {
	rootOnce.Do(func() {
		if v := os.Getenv("HWCLOUD_HOME"); v != "" {
			rootPath = v
			return
		}
		if dir, err := os.Getwd(); err == nil {
			for {
				if exists(filepath.Join(dir, "state", "cookies.json")) || exists(filepath.Join(dir, "data", "blog_post.md")) {
					rootPath = dir
					return
				}
				parent := filepath.Dir(dir)
				if parent == dir {
					break
				}
				dir = parent
			}
		}
		if exe, err := os.Executable(); err == nil {
			dir := filepath.Dir(exe)
			for {
				if exists(filepath.Join(dir, "state", "cookies.json")) || exists(filepath.Join(dir, "data", "blog_post.md")) {
					rootPath = dir
					return
				}
				parent := filepath.Dir(dir)
				if parent == dir {
					break
				}
				dir = parent
			}
			rootPath = filepath.Dir(exe)
			return
		}
		rootPath = "."
	})
	return rootPath
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// StateDir 返回当前账号的状态目录。
// 未指定账号时用 Root()/state；指定后按账号隔离。
func StateDir() string {
	if id := AccountID(); id != "" {
		return filepath.Join(Root(), "accounts", id, "state")
	}
	return filepath.Join(Root(), "state")
}
func DataDir() string           { return filepath.Join(Root(), "data") }
func CookieFile() string        { return filepath.Join(StateDir(), "cookies.json") }
func MetaFile() string          { return filepath.Join(StateDir(), "meta.json") }
func HuaweiProfileDir() string  { return filepath.Join(StateDir(), "browser_profile") }
func GitCodeProfileDir() string { return filepath.Join(StateDir(), "gitcode_profile") }
func GitCodeStateFile() string  { return filepath.Join(StateDir(), "gitcode_state.json") }

// UserAgent 返回登录时记录的浏览器 UA。
// 华为云 WAF 按 UA 指纹校验：UA 与登录时不一致会直接回 404（伪装成「会话失效」），
// 因此所有请求都必须使用 meta.json 里记下的那个 UA。
func UserAgent() string {
	data, err := os.ReadFile(MetaFile())
	if err != nil {
		return DefaultUA
	}
	var meta map[string]any
	if json.Unmarshal(data, &meta) != nil {
		return DefaultUA
	}
	if ua, ok := meta["user_agent"].(string); ok && ua != "" {
		return ua
	}
	return DefaultUA
}