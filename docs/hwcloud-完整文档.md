# hwcloud 完整部署与使用文档

> 一份文档覆盖：安装、配置、日常使用、所有踩过的坑。
>
> 版本：2026-11-01 · 对应镜像 `ghcr.io/dddmiku/hwcloud-console:latest`

---

## 目录

**第一部分：部署**
1. [两种部署方式](#一两种部署方式)
2. [Docker 部署（推荐）](#二docker-部署推荐)
3. [systemd 部署](#三systemd-部署)
4. [数据在哪、怎么备份](#四数据在哪怎么备份)

**第二部分：使用**
5. [第一次使用](#五第一次使用)
6. [账号管理](#六账号管理)
7. [GitCode 绑定](#七gitcode-绑定)
8. [任务说明](#八任务说明)
9. [定时任务](#九定时任务)

**第三部分：疑难**
10. [VNC 与验证码](#十vnc-与验证码)
11. [常见故障排查](#十一常见故障排查)
12. [开发与改代码](#十二开发与改代码)

---

# 第一部分：部署

## 一、两种部署方式

| 方式 | 适合 | 优点 | 缺点 |
| --- | --- | --- | --- |
| **Docker** | 绝大多数人 | 一条命令，自带浏览器 | 镜像 4.4 GB |
| **systemd** | 想改代码 / 资源紧张 | 轻量、可控 | 要自己装依赖 |

**选 Docker**：只想用，不想折腾。
**选 systemd**：要改代码，或者机器内存 < 2 GB。

---

## 二、Docker 部署（推荐）

### 前置：装 Docker

**Linux：**

```bash
curl -fsSL https://get.docker.com | sh
systemctl enable --now docker
```

国内网络慢就用阿里云镜像：

```bash
curl -fsSL https://get.docker.com | sh -s -- --mirror Aliyun
```

**Windows / macOS**：装 [Docker Desktop](https://www.docker.com/products/docker-desktop/)。

验证：

```bash
docker --version
docker compose version
```

`docker compose` 中间是**空格**，不是 `docker-compose`。

### 三步启动

```bash
# 1. 解压
unzip hwcloud-share-*.zip
cd hwcloud-share/deploy

# 2. 启动
docker compose up -d

# 3. 打开浏览器
# http://<服务器IP>:8899
```

首次会拉镜像约 **1.99 GB（压缩）/ 4.4 GB（解压）**，视网络几分钟到半小时。

### 镜像里有什么

| 组件 | 大小 | 用途 |
| --- | --- | --- |
| Camoufox（定制 Firefox） | 2.4 GB | 华为云自动登录，过易盾滑块 |
| Chromium | 379 MB | GitCode 任务（Star / Issue / 活动） |
| Playwright driver | 118 MB | 浏览器控制 |
| Go 二进制 | 12 MB | 主程序 |

**Camoufox 已内置，不用再跑 `install-camoufox.sh`。**

### 打码服务（可选）

云实验、GitCode 登录遇到滑块时需要打码服务算缺口位置。**不配也能用** ——
签到、发文章、Star、提 Issue、活动报名这些纯 HTTP 任务完全不依赖它。

服务地址和凭据在源码里不硬编码，全部从环境变量读。写在 `deploy/.env` 里：

```bash
cd deploy
cp .env.example .env
vim .env
```

```bash
HWCLOUD_CAPTCHA_BASE=https://你的打码服务地址
HWCLOUD_CAPTCHA_KEY=你的-API-Key
HWCLOUD_CAPTCHA_USER=登录用户名
HWCLOUD_CAPTCHA_PASS=登录密码
```

`docker compose up -d` 生效。`docker-compose.yml` 已用 `${VAR:-}` 引用，
没配时传空字符串等于不启用，不会报错。

验证：

```bash
docker exec hwcloud env | grep CAPTCHA
```

### 端口

默认只监听本机：

```yaml
ports:
  - "127.0.0.1:8899:8899"
```

**三种访问方式：**

**A. 本机用** —— 浏览器开 `http://127.0.0.1:8899`，不用改。

**B. 从别的机器访问** —— 改成 `"8899:8899"`：

```yaml
ports:
  - "8899:8899"
```

> ⚠️ 这样面板直接暴露公网，**必须先设强密码**，最好再套 HTTPS。

**C. SSH 隧道（最安全，推荐）** —— 不改 compose：

```bash
ssh -L 8899:127.0.0.1:8899 root@<服务器IP>
# 本地打开 http://127.0.0.1:8899
```

### 升级

```bash
cd hwcloud-share/deploy
docker compose pull
docker compose up -d
```

**只下载变化的层，通常 3-15 MB，不是 4.4 GB。**

镜像分层设计（大件在前，小件在后）：

```
[0-7]  debian + 依赖 + Chromium + Camoufox   ← 永不变，跳过
[8]    Go 二进制 3.4 MB                      ← 每次更新变
[9-12] 配置 / data                           ← 每次更新变
```

`data-runtime/` 原样保留，账号会话不丢。

---

## 三、systemd 部署

### 装依赖

```bash
apt install -y xvfb x11vnc fonts-noto-cjk unzip
```

### 目录布局

```
/opt/hwcloud-demo/
├── go/bin/hwcloud        二进制
├── playwright-driver/
├── browsers/
├── data/
├── accounts/
├── state/
├── logs/
└── deploy/               脚本
```

### 装服务

```bash
cp deploy/hwcloud-console.service deploy/hwcloud-xvfb.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now hwcloud-xvfb
systemctl enable --now hwcloud-console
```

**`hwcloud-xvfb.service` 不能少** —— 面板里 Star / 提 Issue / 活动报名都要有头浏览器，缺 DISPLAY 会全部失败。

### 定时任务

```bash
cp deploy/hwcloud-daily.sh deploy/hwcloud-weekly.sh deploy/hwcloud-lib.sh /usr/local/bin/
chmod +x /usr/local/bin/hwcloud-{daily,weekly,lib}.sh

cp deploy/hwcloud-daily.{service,timer} /etc/systemd/system/
cp deploy/hwcloud-weekly.{service,timer} /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now hwcloud-daily.timer hwcloud-weekly.timer
```

> **坑**：systemd 执行的是 `/usr/local/bin/hwcloud-daily.sh`，而你改的可能是 `/opt/hwcloud-demo/deploy/` 下的副本。**两个是独立文件，不是软链接。** 改完必须同步过去。

---

## 四、数据在哪、怎么备份

### Docker

```
deploy/data-runtime/
├── accounts/    账号、会话 cookie、面板令牌
├── state/       华为云 / GitCode 登录态
├── logs/
└── camoufox/    浏览器（首次启动从镜像复制过来，约 2.4 GB）
```

### systemd

```
/opt/hwcloud-demo/{accounts,state,logs}/
```

### 备份

```bash
tar czf ~/hwcloud-backup-$(date +%F).tgz data-runtime/accounts data-runtime/state
```

只备份 `accounts` 和 `state` 就够。`camoufox` 是浏览器，重装即可；`logs` 不用备。

### 恢复

```bash
docker compose down
tar xzf ~/hwcloud-backup-2026-11-01.tgz
docker compose up -d
```

---

# 第二部分：使用

## 五、第一次使用

### 1. 创建管理员账号

首次打开面板会让你设用户名密码。**只用于登录面板**，不是华为云账号。

### 2. 添加华为云账号

点「添加账号」，填手机号和密码。

> **粘贴时带空格也没关系** —— 系统会自动清洗。支持去掉：
> 空格、横线、`+86` 前缀、NBSP、零宽空格、BOM、全角空格。

### 3. 给账号加备注

账号多了容易混。点账号卡片左上角的「+ 备注」：

```
┌──────────────────────────────────────────────────┐
│ [主号] 138****8000  yd_xxxxxxxx       ● 在线     │
│ ┌──────────────────┐ [保存] [取消]               │
│ │ 主号             │                             │
│ └──────────────────┘                             │
└──────────────────────────────────────────────────┘
```

- 回车保存，Esc 取消
- 最多 40 字

---

## 六、账号管理

### 卡片信息

| 字段 | 含义 |
| --- | --- |
| 备注 | 你自定义的标签 |
| 手机号 | 华为云账号 |
| 用户名 | 华为云 IAM 用户名 |
| 状态 | 在线 / 离线 |
| 设备信任 | 已信任 = 登录免验证码 |
| 积分 | 当前成长中心积分 |
| 凭据剩余 | 会话还有几天过期（通常 400 天） |

### 按钮

| 按钮 | 作用 |
| --- | --- |
| 一键日常 | 签到 + DevSpace + 发文章 |
| 一键周常 | 云实验 + 提 Issue + Star + 活动 + 赛事 |
| 签到 | 单独签到 |
| DevSpace | 上报「体验 AI DevSpace」 |
| 实验 | 启动云实验并提交记录 |
| 发文章 | 发布 `data/blog_post.md` |
| 提 Issue | 在 GitCode 提交 `data/issue_feature.md` |
| Star | 给仓库点 Star（自动挑没 star 的） |
| 活动报名 | 报名进行中的开发者活动 |
| GitCode | 设置 GitCode 账密 + 绑定状态 |
| 保活 | 刷新会话 |
| 检查 | 检查会话有效性 |
| 任务 | 查看成长中心任务列表 |

---

## 七、GitCode 绑定

### 为什么必须绑定

华为云判定「每周提 Issue」「每周 Star 仓库」是否完成，靠的是 **GitCode 账号 → 华为云账号的绑定关系**。

**不绑定的话，Issue 发得再多，成长中心也一直显示未完成。**

### 怎么绑定

1. 点账号卡片上的「GitCode」按钮
2. 填 GitCode **用户名**和密码
3. 点「保存并登录」

系统会自动：
- 登录 GitCode
- **自动获取真实用户名**（从 `gitcode.com/setting/account` 读取）
- 绑定到华为云
- 显示 `✓ GitCode: 用户名`

> **可以填手机号吗？**
> 可以。GitCode 支持手机号登录，系统会自动从账号设置页抓取真实用户名再绑定。
> 不用你手动去复制。

### 绑定状态

| 显示 | 含义 |
| --- | --- |
| `✓ GitCode: xxx` 绿色 | 已绑定，任务会计分 |
| `⚠ GitCode 未绑定` 黄色 | 没绑定，**Issue / Star 不计分** |
| `⚠ GitCode 账号不一致` | 华为云绑的是 A，本地存的是 B |
| 灰色 | 查询失败（会话失效） |

---

## 八、任务说明

### 每日（签到 + DevSpace + 文章）

| 任务 | 积分 | 说明 |
| --- | --- | --- |
| 签到 | +10 | 每天一次，已签到自动跳过 |
| DevSpace | +10 | 上报「体验 AI DevSpace」 |
| 发文章 | +50 | 每周限 3 篇，自动发布 `data/blog_post.md` |

### 每周（云实验 + Issue + Star + 活动 + 赛事）

| 任务 | 积分 | 说明 |
| --- | --- | --- |
| 云实验 | +50 | 启动实验 → 记录过程 → 结束 |
| 提 Issue | +60 | 需要 GitCode 绑定 |
| Star 仓库 | +50 | 需要 GitCode 绑定 |
| 开发者活动 | +10 | 报名进行中的活动 |
| 开发者赛事 | +100 | 报名 + 提交作品 zip |

**前置条件**：GitCode 账号必须绑定到华为云账号，否则 Issue / Star 不计分。

---

## 九、定时任务

### 三层随机化

| 层级 | 机制 | 范围 |
| --- | --- | --- |
| 整轮启动 | systemd `RandomizedDelaySec` | 09:30 后 0-5 分钟 |
| 账号顺序 | `shuf` 打乱 | 每轮不同 |
| 账号间隔 | `random_delay` | 日常 0-180s / 周常 0-240s |

**为什么**：固定时刻、固定顺序、同一秒齐发，都是明显的机器特征。

### 手动运行不加延时

| 触发方式 | 延时 |
| --- | --- |
| systemd timer | ✅ 有 |
| 面板点按钮 | ❌ 无（走 Go API） |
| 手动跑脚本加 `HWCLOUD_MANUAL=1` | ❌ 无 |

### 查看状态

```bash
systemctl list-timers --all | grep hwcloud
systemctl status hwcloud-daily.timer
tail -30 /opt/hwcloud-demo/logs/daily.log
```

---

# 第三部分：疑难

## 十、VNC 与验证码

### 什么时候需要

GitCode 登录偶尔要拖滑块，或者华为云账号开了双重验证要输短信码。服务器没显示器，用 VNC 看画面。

### 方式 A：浏览器直接打开（推荐）

```
http://<服务器IP>:8899/vnc/
```

**不用装客户端、不用开隧道，密码自动填好。**

面板里 GitCode 登录失败时，会自动展开 VNC 面板，点「在浏览器中打开画面」即可。

### 方式 B：VNC 客户端

```bash
ssh -L 5901:127.0.0.1:5901 root@<服务器IP>
```

VNC 客户端连 `127.0.0.1:5901`，密码见面板。

客户端：TightVNC / RealVNC（Windows）、屏幕共享（macOS）、`vncviewer`（Linux）。

### 安全说明

VNC 只监听 `127.0.0.1`，外网必须经 SSH 隧道或面板代理。**不要改成 `0.0.0.0`。**

---

## 十一、常见故障排查

### 面板打不开

```bash
# 1. 服务在跑吗
systemctl is-active hwcloud-console
# 或
docker ps | grep hwcloud

# 2. 端口通吗
curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:8899/api/auth/state

# 3. 容器内部
docker exec hwcloud curl -s http://127.0.0.1:8899/api/auth/state
```

**Docker 特有**：默认只绑 `127.0.0.1`，从别的机器访问要改端口映射。

### 任务一直不跑 / 面板卡住

**根因通常是账号锁被占死。** 检查：

```bash
ps aux | grep -c chrome    # 浏览器残留？
tail -20 /opt/hwcloud-demo/logs/service.log
```

**修复**：

```bash
systemctl stop hwcloud-console
pkill -9 -f chrome
systemctl start hwcloud-console
```

代码里已加：
- 浏览器兜底回收（30 分钟超龄自动清理）
- `JoinEvent` 4 分钟硬超时

### 提 Issue 报「找不到提交按钮」

GitCode 按浏览器语言渲染。真实文案是 **`Submit your issue`**（英文站）。

代码已支持 7 种文案 + 结构兜底。如果还失败，报错里会**列出页面上所有按钮名**，直接看真实文案。

### 开发者赛事报「选职业失败」

**已修复**。原因是点「立即报名」会打开**新标签页**，旧代码继续在列表页操作。

现在会检测 `ctx.Pages()` 并切换到 `raceEnrollment` 页面。

### GitCode 登录成功但绑定失败

**三种情况：**

| 报错 | 原因 | 解决 |
| --- | --- | --- |
| `gitcode user not found` | 用户名填成手机号 | **已自动处理**，会抓真实用户名 |
| `HTTP 404` | GitCode 账号不存在 | 检查用户名拼写 |
| `permission_denied` | 华为云会话失效 | 重新登录华为云 |

### 登录报「账号或密码错误」

**这是真的密码错了**，不是程序 bug。

之前代码会把「密码错误」误判成「免验证登录」，然后去 CAS 换票被弹回，最终报含糊的「缺少 SSOTGC/CASTGC」。

现在会直接显示「**账号或密码错误，请在面板里更正后重试**」。

### 华为云账号开了双重验证

会弹出「身份验证」框要短信码。

代码已能识别（`身份验证` / `双重验证` / `验证码输入框` / `获取验证码` 按钮），会走到短信流程，面板提示你输入验证码。

### 容器 unhealthy

```bash
docker logs hwcloud
docker exec hwcloud ps aux | grep hwcloud
```

正常日志：

```
[entrypoint] Xvfb 就绪 (DISPLAY=:99, 1440x1000x24)
[entrypoint] Camoufox 登录服务就绪 (:8901)
[entrypoint] 启动主进程: hwcloud serve --addr :8899
```

卡在 Xvfb → `shm_size` 太小，不要低于 `1gb`。

### 浏览器报 `Executable doesn't exist`

版本不匹配。driver 必须 **1.61.1**，Chromium 必须 **revision 1228**（对应 `go.mod` 的 `playwright-go v0.6100.0`）。

### 内存不够

每个 Chromium 200-400 MB，Camoufox 约 1 GB。

1 GB 内存的机器必加 swap：

```bash
fallocate -l 2G /swapfile && chmod 600 /swapfile
mkswap /swapfile && swapon /swapfile
echo '/swapfile none swap sw 0 0' >> /etc/fstab
```

### 服务器拉不到 Docker 镜像

```bash
# 配镜像加速
cat > /etc/docker/daemon.json << 'EOF'
{
  "registry-mirrors": [
    "https://docker.1ms.run",
    "https://docker.xuanyuan.me"
  ]
}
EOF
systemctl restart docker
```

> 加速器只加速**拉取**，不加速推送。

---

## 十二、开发与改代码

### 编译

```bash
cd go
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags="-s -w" -o bin/hwcloud ./cmd/hwcloud
```

`CGO_ENABLED=0` 必须 —— 编出纯静态二进制，不依赖目标机 glibc。

### 前端改了必须重编译

前端用 `go:embed` 嵌入二进制：

```go
//go:embed static/index.html static/app.css static/app.js
```

**改完 `static/` 下的文件不重编译 = 没改。**

验证：

```bash
curl -s http://127.0.0.1:8899/static/app.js | grep "<你改的字符串>"
```

### 七个必须知道的坑

**1. 不要在 HTML 里用 inline onclick**

```html
<!-- 危险：&#39; 在属性解析时会解码回 '，esc() 挡不住 -->
<button onclick="doThing('${esc(name)}')">
```

正确做法：`data-*` + 事件委托。

**2. 事件委托别限定元素类型**

```js
// 错误：备注徽标是 span，button 限定会让它点不动
ev.target.closest('button[data-act]')

// 正确
ev.target.closest('[data-act]')
```

**3. Playwright 的 Evaluate 返回 int 不是 float64**

```go
v, _ := page.Evaluate("() => count")
n := v.(int)       // ✅
n := v.(float64)   // ❌ 静默得 0
```

**4. 写文件用 `fsutil.WriteFileAtomic`**

直接 `os.WriteFile` 在断电/kill 时留下半截 JSON，cookies.json 坏了就是永久掉线。

**5. Go 源码里写不可见字符必须用 `\u` 转义**

```go
// 错误：直接粘贴会让源码混进真正的 BOM，编译器报
// "invalid BOM in the middle of the file"
const invisible = "　"     // ← 真正的全角空格

// 正确
const invisible = "　"
```

**6. systemd 执行的文件可能不是你改的那个**

```
systemd:  /usr/local/bin/hwcloud-daily.sh
你改的:   /opt/hwcloud-demo/deploy/hwcloud-daily.sh
```

两个是独立文件。改完要 `cp` 过去，用 `md5sum` 确认一致。

**7. 部署后必须重启**

```bash
systemctl restart hwcloud-console
# 或
docker compose restart
```

**不要用 `kill -9`** —— 会打断在跑的任务，留下脏状态。用 `SIGTERM`。

### 检查清单

```bash
# 死锁检查
python3 go/scripts/check_deadlock.py

# 非原子写检查（应该没有输出）
grep -rn "os\.WriteFile(" go/internal/ --include="*.go"
```

### 自己构建镜像

改完代码推到 GitHub，Actions 自动构建（约 5 分钟，含 Camoufox 下载）。

手动触发：

```bash
gh workflow run build-image.yml
```

或访问 **https://github.com/dddmiku/hwcloud-console/actions**

---

## 附：仓库与镜像

| 项 | 地址 |
| --- | --- |
| 源码 | https://github.com/dddmiku/hwcloud-console |
| 镜像 | `ghcr.io/dddmiku/hwcloud-console:latest` |
| Actions | https://github.com/dddmiku/hwcloud-console/actions |

镜像标签：

- `latest` —— 最新构建
- `<commit>` —— 具体版本，想锁定用这个
- `YYYYMMDD` —— 每周一自动重建

---

## 附：常用命令速查

```bash
# === 服务 ===
systemctl status hwcloud-console
systemctl restart hwcloud-console
docker compose up -d / down / restart / logs -f

# === 检查 ===
curl -s http://127.0.0.1:8899/api/auth/state
ps aux | grep -c chrome
systemctl list-timers --all | grep hwcloud

# === 日志 ===
tail -f /opt/hwcloud-demo/logs/service.log
tail -f /opt/hwcloud-demo/logs/daily.log
docker logs -f hwcloud

# === 清理 ===
pkill -9 -f chrome                    # 清理浏览器残留
docker builder prune -af              # 清构建缓存
rm -rf /tmp/playwright_*              # 清临时 profile

# === 备份 ===
tar czf ~/hwcloud-backup-$(date +%F).tgz data-runtime/accounts data-runtime/state
```
