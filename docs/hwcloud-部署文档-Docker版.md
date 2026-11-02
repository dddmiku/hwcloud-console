# hwcloud 部署文档 · Docker 版

> 适合：只想用，不想改代码。全程不需要 Go、不需要编译。
>
> 源码版见同目录 `hwcloud-部署文档-源码版.md`。

---

## 目录

1. [三步跑起来](#一三步跑起来)
2. [端口怎么改](#二端口怎么改)
3. [数据在哪、怎么备份](#三数据在哪怎么备份)
4. [升级](#四升级)
5. [华为云自动登录（已内置）](#五华为云自动登录已内置)
6. [打码服务（可选，过滑块用）](#五之二打码服务可选过滑块用)
7. [人工过验证码（VNC）](#六人工过验证码vnc)
8. [第一次用要做什么](#七第一次用要做什么)
9. [排障](#八排障)
10. [卸载](#九卸载)
11. [资源与安全](#十资源与安全)

---

## 一、三步跑起来

### 前置：装 Docker

**Linux（一键脚本）**

```bash
curl -fsSL https://get.docker.com | sh
systemctl enable --now docker
```

国内网络如果卡住，用阿里云镜像：

```bash
curl -fsSL https://get.docker.com | sh -s -- --mirror Aliyun
systemctl enable --now docker
```

**Windows / macOS**：装 [Docker Desktop](https://www.docker.com/products/docker-desktop/)，装完启动它，任务栏出现鲸鱼图标即可。

验证：

```bash
docker --version
docker compose version
```

两个命令都要有输出。`docker compose` 是 Docker 自带的子命令（中间是空格，不是 `docker-compose`）；如果报 `unknown command`，说明 Docker 版本太老，装新版。

### 第一步：解压

```bash
unzip hwcloud-share-20261031.zip
cd hwcloud-share/deploy
```

**后面所有命令都在 `deploy/` 目录里执行**——`docker-compose.yml` 就在这。

### 第二步：启动

```bash
docker compose up -d
```

第一次会拉镜像，约 1.6 GB，视网络需要几分钟。拉完自动启动。

### 第三步：打开面板

浏览器访问：

```
http://<服务器IP>:8899
```

本机跑的话就是 `http://127.0.0.1:8899`。

首次打开会引导你**创建管理员账号**——这个账号只用于登录面板，不是华为云账号。密码设强一点，面板能直接操作你的华为云账号。

> **注意**：`docker-compose.yml` 默认把端口绑在 `127.0.0.1`，也就是只有本机能访问。从别的机器打不开是正常的，见下一节。

---

## 二、端口怎么改

### 情况 A：本机用（默认，不用改）

浏览器开 `http://127.0.0.1:8899` 就行。

### 情况 B：从别的机器访问

编辑 `deploy/docker-compose.yml`，找到这一行：

```yaml
ports:
  - "127.0.0.1:8899:8899"
```

改成：

```yaml
ports:
  - "8899:8899"
```

重启：

```bash
docker compose up -d
```

现在 `http://<服务器IP>:8899` 能访问了。

> **安全警告**：这样面板直接暴露在公网。**必须先设强密码**，最好再套一层 HTTPS 反向代理（Nginx / Caddy），不要裸 HTTP 传密码。

### 情况 C：不想暴露端口，用 SSH 隧道（最安全）

保持默认的 `127.0.0.1:8899` 不动，在你自己的电脑上执行：

```bash
ssh -L 8899:127.0.0.1:8899 root@<服务器IP>
```

保持这个 SSH 窗口开着，然后本地浏览器打开 `http://127.0.0.1:8899`。

流量全程走 SSH 加密，公网看不到 8899 端口。推荐这种。

### 换一个端口号

比如想用 9000：

```yaml
ports:
  - "127.0.0.1:9000:8899"
```

冒号**右边**的 `8899` 是容器内部端口，不要改；只改左边的。

---

## 三、数据在哪、怎么备份

所有可变数据都在 `deploy/data-runtime/`：

```
deploy/data-runtime/
├── accounts/    账号列表、会话 cookie、面板令牌
├── state/       华为云 / GitCode 登录态、浏览器 profile
├── logs/        运行日志
└── camoufox/    Camoufox 浏览器（可选，自己装的）
```

这些目录映射进容器，**容器删了重建数据也在**。

### 备份

```bash
cd hwcloud-share/deploy
tar czf ~/hwcloud-backup-$(date +%F).tgz data-runtime/accounts data-runtime/state
```

只备份 `accounts` 和 `state` 就够了（`logs` 不用备，`camoufox` 是 2.4GB 的浏览器，重装即可）。

### 恢复

```bash
cd hwcloud-share/deploy
docker compose down
tar xzf ~/hwcloud-backup-2026-10-31.tgz
docker compose up -d
```

### 什么时候必须备份

- **升级镜像前**（虽然升级不会动数据，但备份是保险）
- **换服务器前**
- **重装系统前**

---

## 四、升级

```bash
cd hwcloud-share/deploy
docker compose pull
docker compose up -d
```

`data-runtime/` 原样保留，账号和会话都不受影响。

确认版本：

```bash
docker compose images
```

想看更新了什么，看镜像的发布时间：

```bash
docker image inspect ghcr.io/dddmiku/hwcloud-console:latest --format '{{.Created}}'
```

---

## 五、华为云自动登录（已内置）

### 结论：不用装

Camoufox 浏览器（定制 Firefox，约 2.4 GB）**已经打进镜像**。首次启动时
entrypoint 会自动把它从镜像复制到 `data-runtime/camoufox/`，之后容器重启
直接复用，镜像升级也不会冲掉。

为什么必须用它：华为云登录要过**网易易盾滑块**。普通 Chromium 的指纹过不去，
Playwright 也会被识别，所以只能用 Camoufox 这种内核级指纹伪装的浏览器。

### 验证是否就绪

```bash
docker logs hwcloud | head -8
```

看到这两行就对了：

```
[entrypoint] Camoufox 登录服务就绪 (:8901)
[entrypoint] 启动主进程: hwcloud serve --addr :8899
```

也可以直接探活：

```bash
docker exec hwcloud curl -fsS http://127.0.0.1:8901/health
```

返回 `{"ok": true, "service": "hwlogin-camoufox", ...}` 即正常。

### 起不来怎么办

```bash
docker logs hwcloud | grep -i camoufox
docker exec hwcloud cat /opt/hwcloud-demo/logs/camoufox.log
```

| 现象 | 原因 / 解决 |
| --- | --- |
| `未检测到 Camoufox 浏览器` | `data-runtime/camoufox/` 被清空了，删掉目录重启容器会重新复制 |
| 复制很慢 | 2.4 GB 从镜像层复制，首次启动要 1-3 分钟，属正常 |
| 服务起来了但登录失败 | 易盾改版了，需要更新镜像 |

### 内存要求

Camoufox 常驻约 1 GB。1 GB 内存的机器必须加 swap：

```bash
fallocate -l 2G /swapfile && chmod 600 /swapfile
mkswap /swapfile && swapon /swapfile
echo '/swapfile none swap sw 0 0' >> /etc/fstab
```

---

## 五之二、打码服务（可选，过滑块用）

### 什么时候需要

云实验、GitCode 登录遇到滑块时，需要把背景图和拼图块交给打码服务，
算出缺口 x 坐标再模拟拖动。

**不配也能用**：签到、发文章、Star、提 Issue、活动报名这些纯 HTTP 任务
完全不依赖打码服务。只有滑块相关任务会报「未配置打码服务」。

### 怎么配

打码服务的地址和凭据在源码里**不再硬编码**，全部从环境变量读。
推荐写在 `deploy/.env` 里，这样密码不会进 git：

```bash
cd deploy
cp .env.example .env
vim .env
```

填这四项：

```bash
HWCLOUD_CAPTCHA_BASE=https://你的打码服务地址
HWCLOUD_CAPTCHA_KEY=你的-API-Key
HWCLOUD_CAPTCHA_USER=登录用户名
HWCLOUD_CAPTCHA_PASS=登录密码
```

然后重启容器让它生效：

```bash
docker compose up -d
```

`docker-compose.yml` 里已经用 `${HWCLOUD_CAPTCHA_BASE:-}` 的形式引用了这四项，
`.env` 没配时传空字符串，等于不启用，不会报错。

### 验证

```bash
docker exec hwcloud env | grep CAPTCHA
```

能看到四项就说明传进去了。

### 自己搭打码服务

源码 `deploy/camoufox_server.py` 里有一份 AntiCAP 客户端的参考实现，
接口是：

- `POST /api/login` — 用户名密码换 JWT
- `POST /api/slider/match` — 传 `target_base64` + `background_base64`，返回缺口 x
- `POST /api/ocr` — 传 `image`，返回识别文字

---
## 六、人工过验证码（VNC）

GitCode 登录偶尔要拖滑块验证码。服务器上没有显示器，用 VNC 看容器里的浏览器画面。

### 开启

编辑 `deploy/docker-compose.yml`，找到 `environment:` 那段，取消注释并改密码：

```yaml
environment:
  TZ: Asia/Shanghai
  VNC_PASSWORD: "换成你的密码"
```

再把 5901 端口映射出来（同一文件 `ports:` 段）：

```yaml
ports:
  - "127.0.0.1:8899:8899"
  - "127.0.0.1:5901:5901"
```

重启：

```bash
docker compose up -d
```

### 连接

**推荐 SSH 隧道**（5901 不暴露公网）：

```bash
ssh -L 5901:127.0.0.1:5901 root@<服务器IP>
```

然后用 VNC 客户端连 `127.0.0.1:5901`：

- Windows：[TightVNC Viewer](https://www.tightvnc.com/download.php) 或 RealVNC
- macOS：系统自带「屏幕共享」，输入 `vnc://127.0.0.1:5901`
- Linux：`vncviewer 127.0.0.1:5901`

输入你设的 `VNC_PASSWORD`。

### 用法

连上后能看到一个虚拟桌面。面板里点「登录 GitCode」时，浏览器会在这个桌面里打开，你直接用鼠标拖滑块即可。

验证码过了之后可以关掉 VNC，不影响任务。

详细步骤（含 GitCode 绑定流程）见包里的 `docs/VNC与验证码.md`。

---

## 七、第一次用要做什么

### 1. 创建管理员账号

首次打开面板会让你设用户名和密码。**只用于登录面板**。

### 2. 添加华为云账号

面板里「添加账号」，填：

- 华为云账号（手机号）
- 密码

然后点「登录」，会拉起浏览器走一次登录流程。

### 3. 绑定 GitCode（关键，别跳过）

**GitCode 账号必须绑定到华为云账号**，否则提 Issue、Star 仓库这些任务**不计入成长中心**，白做。

面板里有「绑定 GitCode」按钮，点了会走绑定流程。

绑定时要填：

- GitCode 用户名
- 华为云账号名
- 华为云 domain_id / user_id / project_id

这些 ID 从华为云控制台能查到。绑定成功后，Issue 和 Star 才会算分。

### 4. 跑一次日常

面板上「日常」按钮，或命令行：

```bash
docker exec hwcloud /opt/hwcloud-demo/go/bin/hwcloud check
```

看输出确认账号、积分、签到状态。

### 5. 设置定时（可选）

面板里有「定时任务」开关，打开后会按计划自动跑日常/周常。

---

## 八、排障

### 容器一直 unhealthy

```bash
docker ps -a | grep hwcloud
docker logs hwcloud
docker exec hwcloud ps aux | grep hwcloud
```

正常启动日志：

```
[entrypoint] Xvfb 就绪 (DISPLAY=:99, 1440x1000x24)
[entrypoint] 未检测到 Camoufox 浏览器 —— 华为云自动登录不可用
[entrypoint]   面板和 GitCode 任务（Star / 提 Issue / 活动报名）正常
[entrypoint] 启动主进程: hwcloud serve --addr :8899
[2026-01-01 00:00:00] 服务启动，监听 :8899
```

如果停在 Xvfb 那行不动，多半是 `shm_size` 太小。检查 compose 里：

```yaml
shm_size: 1gb
```

不要调小。

### 面板打不开

```bash
# 1. 容器在跑吗
docker ps | grep hwcloud

# 2. 端口绑对了吗
docker port hwcloud

# 3. 容器内部通吗
docker exec hwcloud curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:8899/
```

- 容器内部返回 200，外面打不开 → 端口绑定问题（见[第二节](#二端口怎么改)）或防火墙
- 容器内部也打不开 → 看 `docker logs hwcloud`

防火墙放行（Ubuntu）：

```bash
ufw allow 8899/tcp
```

### 面板能开但任务不跑

```bash
docker exec hwcloud ls -la /opt/hwcloud-demo/state
docker exec hwcloud cat /opt/hwcloud-demo/logs/*.log | tail -50
```

多半是会话过期。去面板点「自动登录」（需要 Camoufox），或者手动重新登录一次。

### 报 `Executable doesn't exist`

浏览器版本和 Playwright driver 对不上。镜像里是绑定好的（driver 1.61.1 / Chromium revision 1228），正常不会出现。

如果你自己改过 Dockerfile，两者必须一起换：

```dockerfile
COPY assets/playwright-driver/ ./playwright-driver/
COPY assets/browsers/ ./browsers/
```

### 拉镜像很慢 / 卡住

```bash
docker pull ghcr.io/dddmiku/hwcloud-console:latest
```

单独跑这个看进度。国内网络慢的话，配个加速器（`/etc/docker/daemon.json`）：

```json
{
  "registry-mirrors": [
    "https://docker.1ms.run",
    "https://docker.xuanyuan.me"
  ]
}
```

改完重启 Docker：

```bash
systemctl restart docker
```

> 注意：加速器只加速**拉取**，不影响推送。

### 内存不够，浏览器被 OOM

```bash
dmesg | grep -i "killed process" | tail -5
free -m
```

每个 Chromium 约 200–400 MB。1 GB 内存的机器必加 swap：

```bash
fallocate -l 2G /swapfile
chmod 600 /swapfile
mkswap /swapfile
swapon /swapfile
echo '/swapfile none swap sw 0 0' >> /etc/fstab
```

### 时间不对（任务按日期算）

```bash
docker exec hwcloud date
```

应该是 CST（东八区）。compose 里已经设了 `TZ: Asia/Shanghai`，如果不对检查宿主机时间：

```bash
timedatectl set-timezone Asia/Shanghai
```

### 想彻底重来

```bash
cd hwcloud-share/deploy
docker compose down
rm -rf data-runtime          # ⚠️ 这会删掉所有账号和会话
docker compose up -d
```

删之前先备份（见[第三节](#三数据在哪怎么备份)）。

---

## 九、卸载

```bash
cd hwcloud-share/deploy
docker compose down

# 删镜像（约 1.6 GB）
docker rmi ghcr.io/dddmiku/hwcloud-console:latest

# 删数据（确认不要了再执行）
rm -rf data-runtime
```

---

## 十、资源与安全

### 资源要求

| 项目 | 最低 | 建议 |
| --- | --- | --- |
| 内存 | 2 GB | 4 GB |
| 磁盘 | 5 GB | 10 GB |
| CPU | 1 核 | 2 核 |

装了 Camoufox 再加 **2.4 GB 磁盘**。

compose 里默认限制 4 GB 内存：

```yaml
deploy:
  resources:
    limits:
      memory: 4g
```

1 GB 的机器建议加 swap（见排障），或者把上限调低到 `2g`。

### 安全清单

部署到公网前，逐条确认：

- [ ] **面板密码足够强**（首次启动设的那个）
- [ ] **走 HTTPS**，不要裸 HTTP。用 Caddy 最省事：

  ```caddy
  panel.example.com {
      reverse_proxy 127.0.0.1:8899
  }
  ```

- [ ] **`data-runtime/` 权限收紧**（里面有登录 cookie）：

  ```bash
  chmod 700 data-runtime/accounts data-runtime/state
  ```

- [ ] **定期备份** `accounts` 和 `state`
- [ ] **不要把 `data-runtime/` 分享给别人** —— 里面有可直接复用的登录态

### 面板能做什么

面板可以：

- 用你的华为云账号执行任务（签到、实验、提 Issue 等）
- 读写 `data-runtime/` 里的所有数据
- 启动浏览器访问外部网站

所以**密码就是账号本身**，不要设弱密码，不要分享。

---

## 附：常用命令速查

```bash
# 启动 / 停止 / 重启
docker compose up -d
docker compose down
docker compose restart

# 看日志
docker logs hwcloud
docker logs -f hwcloud              # 实时跟踪
docker logs --tail 100 hwcloud

# 进容器
docker exec -it hwcloud bash

# 手动跑任务
docker exec hwcloud /opt/hwcloud-demo/go/bin/hwcloud check
docker exec hwcloud /opt/hwcloud-demo/go/bin/hwcloud sign

# 健康检查
curl -s http://127.0.0.1:8899/api/auth/state

# 升级
docker compose pull && docker compose up -d

# 备份
tar czf ~/hwcloud-backup-$(date +%F).tgz data-runtime/accounts data-runtime/state
```
