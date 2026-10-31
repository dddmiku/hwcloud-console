// Package browser 提供 Playwright 浏览器的启动与生命周期管理。
//
// 这个文件专门处理「进程堆积」问题。
//
// 为什么需要兜底清理：
//
//	每个任务入口都有 defer br.Close() / defer pw.Stop()，正常情况下没问题。
//	但 defer 只在函数正常返回或 panic 时执行，以下几种情况会漏：
//	  1. 进程被 kill -9 —— defer 完全不跑，Chromium 变成孤儿
//	  2. Go 运行时崩溃（OOM、栈溢出）
//	  3. 任务 goroutine 泄漏，函数永远不返回
//	  4. Playwright driver 的 node 进程自己挂了，但 Chromium 还活着
//
//	这些情况下 Chromium 会一直占着内存（每个 200-400 MB），
//	累积几个就把 1.7 GB 的机器拖垮。
//
// 策略：
//   - 每次启动浏览器前，扫一遍残留进程，清理超龄的
//   - 用 profile 目录名做标记，只清自己启动的，不碰别人的浏览器
//   - 超龄阈值给足（默认 30 分钟），避免误杀正在跑的长任务
package browser

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// orphanMaxAge 是判定「孤儿浏览器」的年龄阈值。
//
// 为什么是 30 分钟：GitCode 登录最慢实测 3 分 9 秒，云实验 5 分钟，
// 开发者赛事（报名 + 上传 zip）约 8 分钟。留 30 分钟余量，
// 正常任务绝不会被误杀，而真正卡死的进程半小时内一定会被收走。
const orphanMaxAge = 30 * time.Minute

var (
	reapMu   sync.Mutex
	lastReap time.Time
)

// ReapOrphanBrowsers 清理超龄的 Chromium 进程。
//
// 只在以下条件下动手：
//   - 进程的命令行里含 Playwright 的 user-data-dir（说明是我们起的）
//   - 进程启动时间超过 orphanMaxAge
//
// 返回清理掉的进程数。
//
// 内部做了 5 分钟节流：连续跑任务时不必每次都扫 /proc。
func ReapOrphanBrowsers() int {
	reapMu.Lock()
	if time.Since(lastReap) < 5*time.Minute {
		reapMu.Unlock()
		return 0
	}
	lastReap = time.Now()
	reapMu.Unlock()

	if _, err := os.Stat("/proc"); err != nil {
		return 0 // 非 Linux，跳过
	}

	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}

	now := time.Now()
	killed := 0

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if pid == os.Getpid() {
			continue
		}

		// 命令行里必须同时有 chrome 和我们的 profile 路径，
		// 避免误杀用户自己开的浏览器或别的项目的 Playwright。
		cmdline, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil || len(cmdline) == 0 {
			continue
		}
		cmd := strings.ReplaceAll(string(cmdline), "\x00", " ")
		if !isOurBrowser(cmd) {
			continue
		}

		// 用 /proc/<pid> 的修改时间近似进程启动时间
		st, err := os.Stat(filepath.Join("/proc", e.Name()))
		if err != nil {
			continue
		}
		if now.Sub(st.ModTime()) < orphanMaxAge {
			continue
		}

		if proc, err := os.FindProcess(pid); err == nil {
			// 先 SIGTERM 让它自己收尾（Chromium 会清理临时 profile），
			// 3 秒后还在就 SIGKILL。
			_ = proc.Signal(os.Interrupt)
			go func(p *os.Process) {
				time.Sleep(3 * time.Second)
				_ = p.Kill()
			}(proc)
			killed++
		}
	}

	if killed > 0 {
		// 顺手清掉 Chromium 崩溃后留下的临时 profile 目录
		cleanStaleProfiles()
	}
	return killed
}

// isOurBrowser 判断命令行是不是我们启动的浏览器。
//
// 认这几个特征（任一命中即可）：
//   - playwright 的 .org.chromium.Chromium.* 临时目录
//   - 我们自己的 profile 目录（gitcode_profile / browser_profile）
//   - Playwright driver 的 node 进程
func isOurBrowser(cmd string) bool {
	markers := []string{
		"org.chromium.Chromium.",
		"ms-playwright",
		"gitcode_profile",
		"browser_profile",
		"playwright-driver",
		"playwright/driver",
	}
	for _, m := range markers {
		if strings.Contains(cmd, m) {
			return true
		}
	}
	return false
}

// cleanStaleProfiles 删除 /tmp 下超龄的 Playwright 临时 profile。
//
// Chromium 正常退出会自己删，被 kill -9 时会留下
// /tmp/playwright_chromiumdev_profile-XXXXXX 这样的目录。
// 每个几十 MB，攒多了很占地方。
func cleanStaleProfiles() {
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		return
	}
	now := time.Now()
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "playwright_") &&
			!strings.HasPrefix(name, "playwright-download-") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) < orphanMaxAge {
			continue
		}
		_ = os.RemoveAll(filepath.Join(os.TempDir(), name))
	}
}

// CountBrowsers 返回当前存活的 Chromium 进程数，用于监控和日志。
func CountBrowsers() int {
	out, err := exec.Command("sh", "-c", "ps -eo args 2>/dev/null | grep -c '[c]hrome'").Output()
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return n
}
