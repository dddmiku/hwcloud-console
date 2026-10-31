# 华为云成长中心任务自动化 Demo

自动完成华为云开发者成长中心的日常任务：签到、云实验、提 Issue、Star 仓库、开发者活动报名、开发者赛事。

带一个网页面板，多账号管理、定时任务、会话保活都在里面。

---

## 快速开始（Docker）

不需要装 Go，不需要编译，不需要手动下载浏览器：

```bash
cd deploy
docker compose up -d
```

打开 `http://<服务器IP>:8899`，首次访问会引导你创建管理员账号。

> 默认只监听 `127.0.0.1`，外网访问不到。要从别的机器访问，改 `deploy/docker-compose.yml` 里的端口映射，**并且务必先设强密码**——面板能直接操作你的华为云账号。

升级（在 `deploy/` 目录下）：

```bash
docker compose pull && docker compose up -d
```

数据在 `deploy/data-runtime/`，升级不丢。

详细说明见 [Docker 部署](docs/Docker部署.md)（端口、数据卷、资源要求、排障）。

---

## 能做什么

| 任务 | 说明 |
| --- | --- |
| 每日签到 | 自动签到，已签到跳过 |
| 云实验 | 启动实验 → 记录过程 → 结束，拿积分 |
| 提 Issue | 在 GitCode 上提交 `data/issue_feature.md` |
| Star 仓库 | 自动挑没 Star 过的仓库 |
| 开发者活动 | 报名 + 填表 |
| 开发者赛事 | 报名 + 提交作品（每天 1 次） |
| 会话保活 | 定时探活，失效自动账密登录（需 Camoufox） |

**前置条件**：GitCode 账号必须绑定到华为云账号，否则 Issue / Star 不计入任务。面板里有「绑定 GitCode」按钮。

---

## 华为云自动登录（可选）

会话能活 29 天，手动登一次够用很久。要让它自动续，需要装 Camoufox 浏览器（约 2.4 GB，**不打进镜像**）：

```bash
./install-camoufox.sh
docker compose restart
```

不装也能用——面板和所有 GitCode 任务正常，只是会话失效时要手动登一次。

---

## 从源码构建

只有改代码才需要。

**Docker 镜像**：

```bash
cd deploy
./build.sh              # 准备 assets（编译 Linux 二进制 + 拷 driver/浏览器）
docker compose build
docker compose up -d
```

**本地 Windows 运行**：

```bat
cd go
go build -trimpath -o bin\hwcloud.exe ./cmd/hwcloud
bin\hwcloud.exe serve --addr :8899
```

需要 Go 1.23+ 和 Playwright driver（首次运行会尝试自动下载）。

---

## 包含内容

| 目录 | 内容 |
| --- | --- |
| `go/` | Go 实现源码 |
| `go/third_party/playwright-go/` | 本地化的 playwright-go 1.61 客户端模块（源码） |
| `data/` | 示例内容：博客正文、Issue 内容、实验记录 |
| `state/` | 空运行目录，登录后生成会话 |
| `deploy/` | Docker 部署文件、systemd 服务、Camoufox 安装脚本 || `docs/` | 使用指南、Docker 部署、VNC 与验证码、纯协议登录逆向 |

---

## 文档

- [Docker 部署](docs/Docker部署.md) — 一条命令启动、端口、数据卷、排障
- [使用指南](docs/使用指南.md) — driver 安装、命令行用法、排障
- [VNC 与验证码](docs/VNC与验证码.md) — 人工过滑块、GitCode 绑定
- [纯协议登录逆向](docs/纯协议登录逆向.md) — 华为云登录协议分析
- [Go 实现说明](go/README.md)

---

## 安全提示

`data-runtime/`（或本地运行的 `state/`）保存登录会话和浏览器 profile，**不要分享或提交到仓库**。

面板默认只监听本机，这是有意的。要暴露到公网，至少做到：

1. 设一个强密码（面板首次启动会让你创建）
2. 走 HTTPS 反向代理，不要裸 HTTP
3. 定期备份 `data-runtime/accounts` 和 `data-runtime/state`
