// Package fsutil 提供原子文件写入。
//
// 为什么需要单独的包：面板和 CLI 会在多个 goroutine / 进程里写同一批
// 状态文件（cookies.json、meta.json、hwmeta、fp、hwid_cas_sid）。
// 直接 os.WriteFile 在写入过程中被 kill、断电或磁盘满，会留下半截
// JSON —— 对 cookies.json 意味着会话永久失效（解析失败 → 未登录），
// 而用户只看到「又掉线了」，查不到任何线索。
//
// 实现：临时文件 + fsync + rename。rename 在同一文件系统内是原子的，
// 读者要么看到旧内容要么看到新内容，不存在中间态。
package fsutil

import (
	"os"
	"path/filepath"
)

// WriteFileAtomic 原子写文件。
//
// 步骤：
//  1. 在目标同目录建临时文件（同目录保证 rename 不跨文件系统）
//  2. 写入 + Chmod + Sync（Sync 保证 rename 前数据真的落盘）
//  3. rename 覆盖目标
//
// 任何一步失败都会清理临时文件并返回错误，目标文件保持原样。
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// rename 成功后 Remove 是空操作；失败路径靠它清理
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
