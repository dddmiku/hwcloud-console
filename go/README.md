# hwcloud-go（Demo）

华为云成长中心任务自动化的 Go 实现。`data/` 与 `state/` 位于上一级目录（包根）。

## 构建

```bat
go build -trimpath -o bin\hwcloud.exe ./cmd/hwcloud
```

## 运行

```bat
bin\hwcloud.exe check
bin\hwcloud.exe sign
bin\hwcloud.exe lab --dry-run
bin\hwcloud.exe blog --dry-run
bin\hwcloud.exe issue --dry-run
```

## 目录

```
cmd/hwcloud/          入口
internal/config/      路径、站点、接口常量
internal/huawei/      cookie jar、CSRF、签到、任务、积分
internal/browser/     Playwright 启动、cookie 注入、driver 定位
internal/task/        华为登录、云实验、博客、GitCode
internal/cli/         子命令
third_party/          playwright-go 1.61 客户端（源码）
```

## Playwright driver

本包不包含 driver。首次运行浏览器命令时，Go 客户端会尝试自动下载 driver。若下载失败：

1. 使用包内本地化客户端安装（会下载 driver，可选安装 Chromium）：

```bat
cd third_party\playwright-go
go run ./cmd/playwright install chromium
```

2. 或复用已有 Playwright 1.61 driver，设置环境变量：

```bat
set PLAYWRIGHT_DRIVER_PATH=C:\path\to\playwright\driver
```

该目录应包含 `node.exe` 和 `package\cli.js`。版本要求为 1.61.x。

## 说明

- 项目根目录定位：`HWCLOUD_HOME` → 从当前目录/可执行文件目录向上查找 `data/blog_post.md` 或 `state/cookies.json`。
- HTTP 使用 Windows 系统代理（go-ieproxy）。
- 浏览器优先使用系统 Chrome；未安装时回退到 Playwright Chromium。
- 浏览器命令支持 `--dry-run`，只校验页面和填充，不提交。
- 集成测试（需要有效会话与 driver）：

```bat
go test -tags integration -run TestProbeLogin -v ./internal/task
go test -tags integration -run TestSmokeLaunch -v ./internal/browser
```
