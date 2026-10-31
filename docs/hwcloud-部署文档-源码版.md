# hwcloud 部署文档 · 源码版

> 适合：想改代码、想调试、想理解实现。
>
> 只想用的话看同目录 `hwcloud-部署文档-Docker版.md`——那边不需要 Go，一条命令启动。

---

## 目录

1. [环境准备](#一环境准备)
2. [编译](#二编译)
3. [两种运行方式](#三两种运行方式)
4. [命令行完整用法](#四命令行完整用法)
5. [代码结构](#五代码结构)
6. [改代码要注意的坑](#六改代码要注意的坑)
7. [自己构建 Docker 镜像](#七自己构建-docker-镜像)
8. [systemd 部署（不用 Docker）](#八systemd-部署不用-docker)
9. [调试](#九调试)
10. [排障](#十排障)

---

## 一、环境准备

### 必需

| 项目 | 版本 | 说明 |
| --- | --- | --- |
| Go | **1.23+** | go.mod 里写的 `go 1.23.1` |
| Playwright driver | **1.61.1** | 必须精确匹配，见下文 |
| Chromium | revision **1228** | 和 driver 绑定 |

### 可选

| 项目 | 用途 |
| --- | --- |
| Python 3.9+ | 只有华为云自动登录（Camoufox）需要 |
| Camoufox | 华为云自动登录，2.4 GB |
| Xvfb | Linux 无头服务器跑有头浏览器 |
| VNC | 人工过验证码 |

### 装 Go

**Windows**：下载 https://go.dev/dl/ 的 msi 安装包，装完重启终端。

**Linux**：

```bash
wget https://go.dev/dl/go1.23.1.linux-amd64.tar.gz
rm -rf /usr/local/go && tar -C /usr/local -xzf go1.23.1.linux-amd64.tar.gz
echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc
source ~/.bashrc
```

验证：

```bash
go version   # 要 >= go1.23
```

### Playwright driver 从哪来

**包里有**（如果拿到的是完整分享包）：`deploy/assets/playwright-driver/` 和 `deploy/assets/browsers/`。

**包里没有**（纯源码包）：三个办法

**办法 1：从已部署的服务器取**

```bash
cd deploy
./build.sh --from-server root@<服务器IP>
```

会 `scp`/`rsync` 下来二进制、driver、Chromium。约 500 MB。

**办法 2：让程序自己下**

首次运行会自动尝试下载 driver。但国内经常卡在 GitHub，不推荐。

**办法 3：手动指定已有路径**

```bash
export PLAYWRIGHT_DRIVER_PATH=/path/to/playwright-driver
export PLAYWRIGHT_BROWSERS_PATH=/path/to/browsers
```

### 版本必须对齐（最容易踩的坑）

`go.mod`：

```go
github.com/playwright-community/playwright-go v0.6100.0
```

对应：

- driver **1.61.1**
- Chromium revision **1228**

版本错了浏览器起不来，报的错是：

```
Executable doesn't exist at /path/to/chromium-XXXX/chrome-linux/chrome
```

**这个错误看起来像路径问题，实际是版本问题。** `XXXX` 就是实际 revision，和你期望的对不上。

查 driver 版本：

```bash
cat deploy/assets/playwright-driver/package/package.json | grep version
```

查 Chromium revision：

```bash
ls deploy/assets/browsers/
# 应该看到 chromium-1228
```

---

## 二、编译

### Windows

```bat
cd go
go build -trimpath -o bin\hwcloud.exe ./cmd/hwcloud
```

产物：`go\bin\hwcloud.exe`

### Linux / macOS

```bash
cd go
go build -trimpath -o bin/hwcloud ./cmd/hwcloud
```

产物：`go/bin/hwcloud`

### 交叉编译（给 Linux 服务器）

在 Windows 或 macOS 上编 Linux 二进制：

```bash
cd go
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags="-s -w" -o bin/hwcloud ./cmd/hwcloud
```

`CGO_ENABLED=0` 是必须的——`go-ieproxy` 依赖 CGO，但代码里没用到它需要 CGO 的部分，关掉能编出纯静态二进制。

`-ldflags="-s -w"` 去掉符号表和调试信息，体积从约 18 MB 降到 12 MB。

### 关于 `third_party/playwright-go`

`go.mod` 最后一行：

```go
replace github.com/playwright-community/playwright-go => ./third_party/playwright-go
```

**这个 replace 不能删。**

原因：官方 `v0.6100.0` 在 Go proxy 上声明的模块路径是 `github.com/mxschmitt/playwright-go`，而代码里 import 的是 `github.com/playwright-community/playwright-go`。直接拉官方会报：

```
module declares its path as: github.com/mxschmitt/playwright-go
        but was required as: github.com/playwright-community/playwright-go
```

本地 `third_party/playwright-go` 副本的模块路径是对的，所以随包分发。

### 编译产物验证

```bash
./bin/hwcloud            # 应该打印 usage
./bin/hwcloud status     # 看会话和绑定状态
```

---

## 三、两种运行方式

### 方式 A：网页面板（推荐）

```bash
./bin/hwcloud serve --addr :8899
```

打开 `http://127.0.0.1:8899`，首次会引导创建管理员账号。

面板能做的事比命令行多：

- 多账号管理
- 定时任务（日常 / 周常）
- 会话保活
- 一键过验证码（配 VNC）
- 任务执行进度、日志

**Linux 服务器必须先有 X display**，否则 Star / 提 Issue / 活动报名全部失败：

```bash
Xvfb :99 -screen 0 1440x1000x24 &
export DISPLAY=:99
```

为什么：GitCode 对无头浏览器做了行为判定。`headless=True` 时访问 `/issues/create` 会被 **302 重定向回列表**，表现成「会话失效」，极难排查。所以这些任务必须跑有头浏览器。

### 方式 B：命令行

```bash
./bin/hwcloud check      # 账号、积分、签到、任务
./bin/hwcloud sign       # 签到
./bin/hwcloud lab        # 云实验
```

完整命令见下一节。

---

## 四、命令行完整用法

### 登录

| 命令 | 说明 |
| --- | --- |
| `login` | 华为云交互登录（拉起浏览器，手动登） |
| `login-pw --account <手机号> --password <密码>` | 纯协议密码登录 |
| `login-pw --account <手机号> --sms <验证码>` | 短信登录 |
| `login-pw2` | 纯协议账密登录（设备已信任时免验证码） |
| `pure-login` | 纯协议登录（可带 `--fp` 指纹） |
| `import-cookie <cookie头或文件>` | 从 Cookie 导入会话 |
| `save-sid <sid>` | 保存 `hwid_cas_sid`（信任设备后得到） |
| `login-gitcode` | GitCode 交互登录 |

**`login-pw` vs `login-pw2` 的区别**：

- `login-pw`：通用，可能需要短信验证码
- `login-pw2`：走设备信任链，已信任的设备直接登录，不用验证码

### 日常任务

| 命令 | 说明 |
| --- | --- |
| `check` | 查看登录态、签到、任务状态 |
| `sign` | 自动签到（已签到跳过） |
| `tasks --stars 1,2` | 列出任务 |
| `lab` | 启动云实验 + 提交过程记录 |
| `lab --dry-run` | 只检查页面，不启动 |
| `blog` | 发布 `data/blog_post.md` |
| `blog --dry-run` | 只填充编辑器，不发布 |
| `blogs` | 列出已发布文章 |
| `drafts` | 列出草稿 |
| `delete <id>` | 删除已发布文章 |
| `devspace` | 上报「体验 AI DevSpace」行为 |

### GitCode 相关

| 命令 | 说明 |
| --- | --- |
| `status` | 实践中心账号 + GitCode 绑定状态 |
| `bind --gitcode <用户名>` | 绑定 GitCode 账号 |
| `issue` | 创建 Issue（读 `data/issue_feature.md`） |
| `issue --dry-run` | 只填充不提交 |
| `star` | 给实践中心仓库点 Star（自动挑未 star 的） |
| `eventjoin` | 报名进行中的开发者活动 |

### 会话与面板

| 命令 | 说明 |
| --- | --- |
| `serve --addr :8899` | 启动 Web 面板 |
| `session` | 查看会话剩余有效期 |
| `keepalive` | 用持久化 profile 刷新会话（失效自动续期） |

### 常用组合

**每天跑一次日常**

```bash
./bin/hwcloud check
./bin/hwcloud sign
./bin/hwcloud lab
./bin/hwcloud issue
./bin/hwcloud star
./bin/hwcloud eventjoin
```

**只看状态不动手**

```bash
./bin/hwcloud status
./bin/hwcloud session
./bin/hwcloud check
```

---

## 五、代码结构

```
go/
├── cmd/hwcloud/main.go          入口，只调 cli.Main
├── internal/
│   ├── cli/cli.go               所有命令的分发和实现（~850 行）
│   ├── web/                     网页面板
│   │   ├── server.go            路由、handler、withAccount 账号上下文
│   │   ├── auth.go              管理员账号、令牌、fail-closed
│   │   ├── assets.go            go:embed 静态资源
│   │   └── static/
│   │       ├── index.html
│   │       ├── app.css
│   │       └── app.js           面板全部前端逻辑
│   ├── task/                    任务实现
│   │   ├── star.go              Star 仓库
│   │   ├── event_join.go        开发者活动报名
│   │   ├── hoh.go               GitCode 绑定
│   │   ├── captcha_client.go    打码服务客户端
│   │   └── ...
│   ├── huawei/                  华为云 API 和会话
│   ├── gitcode/                 GitCode API
│   ├── account/store.go         多账号存储
│   ├── config/                  全局配置（含 SetAccount）
│   └── fsutil/fsutil.go         原子写文件
└── third_party/playwright-go/   本地化的 Playwright 客户端
```

### 关键设计

**`go:embed` 打包前端**

`assets.go`：

```go
//go:embed static/index.html static/app.css static/app.js
var staticFS embed.FS
```

改了 `static/` 下的文件**必须重新编译**，否则跑的还是旧前端。

**`config.SetAccount` 是进程级全局**

这是最重要的架构约束：

```go
prev := config.AccountID()
config.SetAccount(id)
defer config.SetAccount(prev)
```

`withAccount` 用这种 save/restore 的方式切换账号。**它不是线程安全的**——两个 goroutine 同时切会串号。

所以 `web/server.go` 里所有涉及账号的操作都必须串行，靠 `s.mu` 保证。

**`sync.Mutex` 不可重入**

`withAccount` 持锁期间，**不能调用任何也会拿锁的函数**，否则死锁。

曾经的 bug：`handleRunTask` 的 keepalive 分支在 `withAccount` 内部调 `autoLogin`，而 `autoLogin` 也要 `withAccount` → 永久卡死。

修复：把 keepalive 提到 `withAccount` 外面。

`go/scripts/check_deadlock.py` 是静态检查脚本，改完代码跑一下：

```bash
python3 scripts/check_deadlock.py
```

退出码 0 = 没有重入。

---

## 六、改代码要注意的坑

### 1. 前端改完必须重编译

```bash
# 改 static/app.js 之后
go build -trimpath -o bin/hwcloud ./cmd/hwcloud
```

`go:embed` 在编译期把文件嵌进去，不重编译等于没改。

验证是否生效：

```bash
curl -s http://127.0.0.1:8899/static/app.js | grep "<你改的字符串>"
```

### 2. 不要在 HTML 里用 inline onclick

HTML 属性里的 `&#39;` **会被解析回 `'`**，所以 `esc()` 防不住注入：

```html
<!-- 危险：esc() 转义成 &#39; 后，浏览器解析属性时又变回 ' -->
<button onclick="doThing('${esc(name)}')">
```

正确做法是 `data-*` + 事件委托：

```html
<button data-act="delete" data-arg="${esc(name)}">
```

```js
document.addEventListener('click', e => {
  const btn = e.target.closest('[data-act]');
  if (!btn) return;
  doThing(btn.dataset.act, btn.dataset.arg);
});
```

面板里 32 个按钮全部改成了这种写法。

### 3. 写文件用 `fsutil.WriteFileAtomic`

直接 `os.WriteFile` 在断电 / kill 时会留下半截文件，账号表就废了。

```go
import "hwcloud/internal/fsutil"

err := fsutil.WriteFileAtomic(path, data, 0o600)
```

它做的是：临时文件 → `fsync` → `rename`（同目录，原子）。

### 4. Playwright 的 `Evaluate` 返回类型

```go
v, _ := page.Evaluate("() => document.querySelectorAll('.card').length")
n := v.(float64)   // ❌ 会 panic 或得到 0
n := v.(int)       // ✅ JS 整数返回 int
```

JS 整数返回 Go 的 `int`，不是 `float64`。`v.(float64)` 类型断言失败会得到零值，静默出错。

### 5. Star 按钮要点两次（JS 点击）

GitCode 的 Star 按钮是 `div.status-btn`，尺寸 0x0。Playwright 的 `click()` 会报：

```
element is not visible
```

必须用 JS 直接点：

```go
el.Evaluate("el => el.click()")
```

### 6. 仓库列表的 `path_with_namespace` 是 null

GitCode 的仓库列表 API 里这个字段是 `null`，要用 `html_url`：

```go
// ❌
repo["path_with_namespace"]

// ✅
repo["html_url"]
```

### 7. 部署后必须重启才生效

改完代码部署到服务器后，**正在运行的进程还是旧代码**。

```bash
# 用 SIGTERM，不要 -9（-9 会让在跑的任务留下脏状态）
pkill -TERM -f 'hwcloud serve'
```

注意只杀 python/go 主进程，别把 bash 包装脚本杀了。

---

## 七、自己构建 Docker 镜像

改了代码想打成镜像：

```bash
cd deploy

# 1. 准备 assets（编译 Linux 二进制 + 拷 driver/浏览器）
./build.sh

# 2. 构建
docker compose build

# 3. 启动
docker compose up -d
```

`build.sh` 做三件事：

1. `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build` → `assets/hwcloud`
2. 从 `HWCLOUD_SRC`（默认 `/opt/hwcloud-demo`）或 `--from-server` 拷 driver + Chromium
3. 拷 `data/` 模板

产物全在 `deploy/assets/`，Dockerfile 直接从那里 COPY。

### 为什么不在容器里编译

Dockerfile 用的是**预编译二进制**：

```dockerfile
COPY assets/hwcloud ./go/bin/hwcloud
```

不是 `RUN go build`。

原因：目标机器可能只有 **1 GB 内存**，Go 编译需要约 2 GB，在容器里编译会 OOM 或被 swap 拖到十几分钟。

所以编译放在 `build.sh`（有 Go 的机器上跑），容器只负责运行。

### 用 `--from-server` 偷懒

本机没有 Go 或者懒得配 driver：

```bash
./build.sh --from-server root@<服务器IP>
```

直接从已部署的机器 `scp` 二进制 + `rsync` 浏览器资源。

### 改了前端之后

```bash
cd go && go build -trimpath -o bin/hwcloud ./cmd/hwcloud
cd ../deploy && ./build.sh      # 会重新拷二进制
docker compose build && docker compose up -d
```

---

## 八、systemd 部署（不用 Docker）

不想用 Docker 的话，`deploy/` 里有现成的 unit 文件。

### 目录布局

```
/opt/hwcloud-demo/
├── go/bin/hwcloud          二进制
├── playwright-driver/
├── browsers/
├── data/
├── accounts/
├── state/
└── logs/
```

### 装服务

```bash
# 1. 放二进制和资源
mkdir -p /opt/hwcloud-demo/{go/bin,accounts,state,logs,data}
cp bin/hwcloud /opt/hwcloud-demo/go/bin/
cp -r assets/playwright-driver /opt/hwcloud-demo/
cp -r assets/browsers /opt/hwcloud-demo/
cp -r ../data/* /opt/hwcloud-demo/data/

# 2. 装 Xvfb 服务（有头浏览器必需）
cp deploy/hwcloud-console.service /etc/systemd/system/

# 3. 启动
systemctl daemon-reload
systemctl enable --now hwcloud-console
```

### unit 文件关键点

`hwcloud-console.service`：

```ini
Environment=DISPLAY=:99
Environment=PLAYWRIGHT_DRIVER_PATH=/opt/hwcloud-demo/playwright-driver
Environment=PLAYWRIGHT_BROWSERS_PATH=/opt/hwcloud-demo/browsers
ExecStart=/opt/hwcloud-demo/go/bin/hwcloud serve --addr :8899
TimeoutStopSec=40
KillSignal=SIGTERM
```

- `DISPLAY=:99` **不能少**。缺了会在 systemd 下报 `Missing X server or $DISPLAY`，Star / 活动报名 / 提 Issue 全挂。
- `TimeoutStopSec=40`：Go 侧收到 SIGTERM 会等在跑任务收尾，给足时间。

### 还需要 Xvfb 服务

`hwcloud-console.service` 的 `After=hwcloud-xvfb.service`，所以两个要一起装：

```bash
cp deploy/hwcloud-console.service deploy/hwcloud-xvfb.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now hwcloud-xvfb
systemctl enable --now hwcloud-console
```

`hwcloud-xvfb.service` 内容（包里已自带）：

```ini
[Unit]
Description=Xvfb virtual display for hwcloud
After=network.target

[Service]
Type=simple
# :99 与 hwcloud-console.service 里的 DISPLAY=:99 对应，
# 改这里必须同步改那个文件，否则浏览器报 Missing X server or $DISPLAY
ExecStart=/usr/bin/Xvfb :99 -screen 0 1440x1000x24 -nolisten tcp
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

Xvfb 没装的话先装：

```bash
apt install -y xvfb
```

### 定时任务

`deploy/` 里有：

| 文件 | 用途 |
| --- | --- |
| `hwcloud-daily.sh` | 日常任务 |
| `hwcloud-weekly.sh` | 周常任务 |
| `hwcloud-keepalive.sh` | 会话保活 |
| `hwcloud-keepalive.timer` | 保活定时器 |

装 timer：

```bash
cp deploy/hwcloud-keepalive.{service,timer} /etc/systemd/system/
systemctl enable --now hwcloud-keepalive.timer
```

日常 / 周常用 cron：

```cron
0 9 * * *  /opt/hwcloud-demo/deploy/hwcloud-daily.sh  >> /opt/hwcloud-demo/logs/daily.log 2>&1
0 9 * * 1  /opt/hwcloud-demo/deploy/hwcloud-weekly.sh >> /opt/hwcloud-demo/logs/weekly.log 2>&1
```

---

## 九、调试

### 看浏览器实际在干什么

```bash
# 有头模式 + VNC，直接看画面
Xvfb :99 -screen 0 1440x1000x24 &
export DISPLAY=:99
x11vnc -display :99 -forever -shared -rfbport 5901 &
./bin/hwcloud serve --addr :8899
```

然后用 VNC 连 `127.0.0.1:5901`。

### 死锁检查

```bash
python3 go/scripts/check_deadlock.py
```

检查 `withAccount` 内部是否调用了会重入的函数。退出码 0 = 干净。

### 原子写检查

```bash
grep -rn "os\.WriteFile(" go/internal/ --include="*.go"
```

**应该没有输出**。所有状态文件（cookie、账号表、令牌、日志裁剪）都走 `fsutil.WriteFileAtomic`。

有输出说明新代码引入了非原子写——写一半被 kill 或断电，文件就是半截 JSON，读者解析失败，表现成「莫名掉线」，且查不到线索。

### 面板接口手测

```bash
# 认证状态
curl -s http://127.0.0.1:8899/api/auth/state

# 任务列表
curl -s -H "Authorization: Bearer <token>" http://127.0.0.1:8899/api/tasks

# 静态资源
curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:8899/static/app.js
```

### 日志在哪

| 位置 | 内容 |
| --- | --- |
| stdout | 主进程日志 |
| `logs/camoufox.log` | Camoufox 登录服务 |
| `logs/xvfb.log` | Xvfb |
| `logs/x11vnc.log` | VNC |

---

## 十、排障

### 编译报 `module declares its path as`

`third_party/playwright-go` 没拷进来，或者 `go.mod` 的 `replace` 被删了。

检查：

```bash
ls go/third_party/playwright-go/
grep replace go/go.mod
```

### 运行报 `Executable doesn't exist`

版本不匹配。见[第一节的版本对齐](#版本必须对齐最容易踩的坑)。

### 报 `Missing X server or $DISPLAY`

Linux 上没有 X display。装 Xvfb：

```bash
apt install -y xvfb
Xvfb :99 -screen 0 1440x1000x24 &
export DISPLAY=:99
```

### GitCode 任务一直说「会话失效」

先确认是不是**真的**失效。有个经典陷阱：

**无头浏览器访问 `/issues/create` 会被 302 回列表**，看起来像会话失效，实际是行为判定。

确认方法：用 `DISPLAY` 有头模式跑一次，如果成功就是这个问题。

### Star 任务报 `element is not visible`

Star 按钮尺寸 0x0，必须用 JS 点击。见[第六节](#六改代码要注意的坑)。

### 面板一直「加载中…」

历史 bug，根因是 `withAccount` 自锁死（`sync.Mutex` 不可重入）。

跑一下死锁检查：

```bash
python3 go/scripts/check_deadlock.py
```

### 改完前端没生效

`go:embed` 需要重新编译：

```bash
go build -trimpath -o bin/hwcloud ./cmd/hwcloud
```

确认：

```bash
curl -s http://127.0.0.1:8899/static/app.js | grep "<你的改动>"
```

### 部署后行为没变

运行中的进程还是旧代码。重启：

```bash
pkill -TERM -f 'hwcloud serve'
./bin/hwcloud serve --addr :8899 &
```

**不要用 `kill -9`**——会打断正在跑的任务，留下脏状态。

---

## 附：常用命令速查

```bash
# 编译
cd go && go build -trimpath -o bin/hwcloud ./cmd/hwcloud

# 交叉编译给 Linux
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o bin/hwcloud ./cmd/hwcloud

# 跑面板
./bin/hwcloud serve --addr :8899

# 日常
./bin/hwcloud check && ./bin/hwcloud sign

# GitCode
./bin/hwcloud status
./bin/hwcloud bind --gitcode <用户名>
./bin/hwcloud star
./bin/hwcloud issue
./bin/hwcloud eventjoin

# 会话
./bin/hwcloud session
./bin/hwcloud keepalive

# 检查
python3 go/scripts/check_deadlock.py
grep -rn "os.WriteFile" go/internal/

# Docker（改完代码）
cd deploy && ./build.sh && docker compose build && docker compose up -d
```
