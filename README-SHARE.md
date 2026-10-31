# hwcloud 分享包（已脱敏）

## 这个包是什么

华为云开发者成长中心的自动化工具：Go 实现的 CLI + Web 管理面板，
支持多账号签到 / DevSpace / 云实验 / 发文章 / GitCode 提 Issue，
带定时任务与账号离线自动账密登录。

## 已脱敏的内容

| 占位符 | 原内容 |
|---|---|
| `<ACCOUNT_A>` / `<ACCOUNT_B>` | 真实手机号 |
| `<ACCOUNT_PASSWORD>` | 账号密码 |
| `<SERVER_PASSWORD>` | 部署服务器密码 |
| `<GITCODE_USER>` / `<GITCODE_PASSWORD>` | GitCode 账号 |
| `<PANEL_TOKEN>` | 面板访问 token |
| `<CAPTCHA_API_KEY>` / `<CAPTCHA_PASSWORD>` | 打码服务凭据 |
| `<SERVER_IP>` / `<VPS_IP>` | 服务器地址 |

**未包含**：`accounts/`、`state/`、`cookies`、浏览器 profile、日志、二进制。

## 快速开始

```bash
cd go
go build -o bin/hwcloud ./cmd/hwcloud
./bin/hwcloud serve --addr :8899
```

首次打开面板会让你设置管理员账号密码。

## 目录

- `go/internal/` — 核心代码
  - `huawei/` 华为云接口（签到、任务、积分）
  - `task/` 具体任务（云实验、发文章、GitCode、Camoufox 登录、保活）
  - `web/` 管理面板
  - `account/` 多账号存储
  - `browser/` Playwright 封装
- `deploy/` — systemd 单元与定时脚本
- `data/` — 任务用的内容模板（实验记录、文章、Issue）
- `docs/` — 逆向与排查记录

## 需要自己准备

1. 华为云账号（手机号 + 密码）
2. Playwright 浏览器：`PLAYWRIGHT_BROWSERS_PATH` 指向安装目录
3. 云实验要能过滑块：项目里用 Camoufox（见 `deploy/`），需自行部署
4. 提 Issue / Star 要 GitCode 会话：`hwcloud login-gitcode`
5. **GitCode 验证码要手动过一次** —— 见 `docs/VNC与验证码.md`

## GitCode 首次登录（必读）

GitCode 的账密登录会弹网易易盾「拖动白色排球，躲避障碍击中」验证码，
**无法自动破解**。流程是：脚本打开页面并填好账密 → 你通过 VNC 手动拖一次
→ 脚本自动导出会话，之后几个月都能复用。

完整步骤（含 x11vnc 配置、SSH 隧道、VNC 客户端连法、常见坑）见
**`docs/VNC与验证码.md`**。

关键两点：

- **登录完还要绑定**，否则华为云不认 Issue / Star 任务：
  `hwcloud bind --gitcode <GitCode用户名>`
- 绑定后 `hwcloud status` 里 `bound` 应为 `true`

## 注意

- 华为云的设备信任绑定「设备指纹 + hwid_cas_sid」，换机器要重新信任一次
- 云实验每次开始消耗「今日免费名额」，周常已做前置检查避免浪费
- 面板日志每 5 秒自动刷新
- GitCode Issue 有参与者后**无法删除**，只能关闭 —— 周常已做去重检查
- Playwright 浏览器可能残留进程，`deploy/hwcloud-clean-browsers.sh` + timer 兜底
