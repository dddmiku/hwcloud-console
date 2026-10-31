package web

import (
	"embed"
	"io/fs"
	"net/http"
)

// 面板前端资源。
//
// 原来整页 HTML+CSS+JS 塞在一个 Go raw string 常量里（50 KB / 1322 行），
// 代价是：
//   - 编辑器不按 .js/.css 检查语法，没有 lint / format / 格式化
//   - raw string 用反引号定界，正文里不能出现反引号 —— 连 ES6 模板字符串
//     都写不了，1026 行 JS 只能全程 `+` 拼接
//   - 改一行 JS 会让整段行号位移，diff 全是噪音
//
// 现在拆成 static/ 下的三个文件，用 go:embed 嵌进二进制。
// 构建仍是 `go build ./cmd/hwcloud`，产物仍是单文件，部署方式不变。
//
//go:embed static/index.html static/app.css static/app.js
var staticFS embed.FS

// staticSub 是去掉 static/ 前缀后的文件树，供 FileServer 使用。
var staticSub = mustSub(staticFS, "static")

func mustSub(f embed.FS, dir string) fs.FS {
	sub, err := fs.Sub(f, dir)
	if err != nil {
		panic("web: 静态资源嵌入失败: " + err.Error())
	}
	return sub
}

// staticHandler 提供 /static/* 下的 CSS / JS。
//
// 加 Cache-Control: no-cache：这些文件随二进制发布，改了就要立刻生效。
// 用 no-cache（而不是 no-store）让浏览器仍可条件请求，命中 304 省流量。
func staticHandler() http.Handler {
	f := http.FileServer(http.FS(staticSub))
	return http.StripPrefix("/static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		f.ServeHTTP(w, r)
	}))
}

// indexHTMLBytes 是嵌入的首页内容，handleIndex 直接写出。
var indexHTMLBytes = mustRead("index.html")

func mustRead(name string) []byte {
	b, err := fs.ReadFile(staticSub, name)
	if err != nil {
		panic("web: 读取嵌入资源失败: " + name + ": " + err.Error())
	}
	return b
}
