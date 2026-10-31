# Docker 部署

一条命令跑起来，不需要装 Go、不需要编译、不需要手动下载浏览器。

```bash
docker compose up -d
```

然后浏览器打开 `http://<服务器IP>:8899`，首次访问会引导你创建管理员账号。

---

## 目录里有什么

| 文件 | 用途 |
| --- | --- |
| `docker-compose.yml` | 编排文件，`docker compose up -d` 直接用它 |
| `Dockerfile` | 镜像构建文件（一般用不到，只有自己改代码才需要） |
| `entrypoint.sh` | 容器入口：拉起 Xvfb → 按需拉起登录服务 → 主进程 |
| `camoufox_server.py` | 华为云自动登录的 HTTP 服务（可选组件） |
| `install-camoufox.sh` | 安装 Camoufox 浏览器（可选，见下文） |
| `build.sh` | 自己构建镜像时准备 `assets/` 用 |

---

## 端口与访问

默认只监听本机：

```yaml
ports:
  - "127.0.0.1:8899:8899"
```

这样外网访问不到面板。要从别的机器访问，改成 `"8899:8899"`，**并且务必先设一个强密码**——面板能直接操作你的华为云账号。

用 SSH 隧道访问（不改 compose，最安全）：

```bash
ssh -L 8899:127.0.0.1:8899 root@<服务器IP>
# 然后本地打开 http://127.0.0.1:8899
```

---

## 数据在哪

所有可变数据都映射到宿主机的 `./data-runtime/`，容器只是运行载体：

```
data-runtime/
├── accounts/    账号列表、会话 cookie、令牌
├── state/       华为云 / GitCode 登录态
├── logs/        运行日志
└── camoufox/    Camoufox 浏览器（可选，自己装的）
```

升级镜像不会动这些目录：

```bash
docker compose pull      # 拉新镜像
docker compose up -d     # 重建容器（数据卷原样保留）
```

删容器、换镜像、重建都不影响 `data-runtime/` 里的内容。**只有手动 `rm -rf data-runtime` 才会真正丢数据。**

建议定期备份：

```bash
tar czf hwcloud-backup-$(date +%F).tgz data-runtime/accounts data-runtime/state
```

---

## 华为云自动登录（可选）

会话能活 29 天，手动登一次就够用很久。会话失效后，面板需要「自动账密登录」把会话拉起来——这个功能要过网易易盾的滑块，**必须用 Camoufox**（定制 Firefox），普通 Chromium 的指纹过不去。

Camoufox 浏览器本体约 2.4 GB，**不打进镜像**：

- 塞进去会让镜像从 1.6 GB 涨到 4 GB，拉取时间翻几倍
- 多数人用不上（手动登一次能撑一个月）
- 下载依赖 GitHub，国内经常卡住

### 装法

```bash
./install-camoufox.sh
```

它会在容器内 `pip install camoufox`，然后把浏览器下载到 `data-runtime/camoufox/`（映射进容器），走 gh-proxy 镜像加速，失败自动回退官方源。

装完重启容器让 entrypoint 识别：

```bash
docker compose restart
```

验证：

```bash
docker exec hwcloud curl -fsS http://127.0.0.1:8901/health
```

### 没装会怎样

面板和 GitCode 任务（Star / 提 Issue / 活动报名）**全部正常**。只是华为云会话失效时，自动登录按钮会报「未安装 Camoufox」，需要你手动登一次。

entrypoint 日志会明确写出来：

```
[entrypoint] 未检测到 Camoufox 浏览器 —— 华为云自动登录不可用
[entrypoint]   面板和 GitCode 任务（Star / 提 Issue / 活动报名）正常
```

---

## 人工过验证码（VNC）

GitCode 登录偶尔要拖滑块，服务器上没有显示器，用 VNC 看画面。

在 `docker-compose.yml` 里设密码：

```yaml
environment:
  VNC_PASSWORD: "换成你的密码"
```

并把端口映射出来（或者用 SSH 隧道）：

```yaml
ports:
  - "127.0.0.1:8899:8899"
  - "127.0.0.1:5901:5901"
```

重启后：

```bash
ssh -L 5901:127.0.0.1:5901 root@<服务器IP>
```

用任意 VNC 客户端连 `127.0.0.1:5901`，就能看到容器内的浏览器画面并操作。

详细步骤见 [VNC 与验证码](VNC与验证码.md)。

---

## 资源要求

| 项目 | 最低 | 建议 |
| --- | --- | --- |
| 内存 | 2 GB | 4 GB |
| 磁盘 | 5 GB | 10 GB |

每个 Chromium 实例约 200–400 MB。compose 里默认限制 4 GB：

```yaml
deploy:
  resources:
    limits:
      memory: 4g
```

1 GB 的机器建议先加 swap，否则浏览器任务会 OOM。`shm_size: 1gb` 也不要调小，Chromium 崩溃多半是这个。

---

## 排障

**容器一直 unhealthy**

```bash
docker logs hwcloud
docker exec hwcloud ps aux | grep hwcloud
```

正常启动日志长这样：

```
[entrypoint] Xvfb 就绪 (DISPLAY=:99, 1440x1000x24)
[entrypoint] 启动主进程: hwcloud serve --addr :8899
[2026-01-01 00:00:00] 服务启动，监听 :8899
```

如果卡在 Xvfb，多半是 `shm_size` 太小。

**面板打得开但任务不跑**

```bash
docker exec hwcloud ls -la /opt/hwcloud-demo/state
docker exec hwcloud cat /opt/hwcloud-demo/logs/*.log | tail -50
```

多半是会话过期，去面板点「自动登录」或重新登录一次。

**浏览器启动失败，报 `Executable doesn't exist`**

镜像里的 Playwright driver 和 Chromium 版本是绑定的（driver 1.61.1 / Chromium revision 1228）。自己改 Dockerfile 换版本时两者必须一起换，否则浏览器起不来。

**apt 下载慢**

Dockerfile 已经换成阿里云镜像源。自己改的话保留这行：

```dockerfile
RUN sed -i 's|deb.debian.org|mirrors.aliyun.com|g' /etc/apt/sources.list.d/debian.sources
```

---

## 自己构建镜像

一般不需要——直接用 `ghcr.io/dddmiku/hwcloud-console:latest` 就行。

改代码后要重新构建：

```bash
# 1. 准备 assets（编译 Linux 二进制 + 拷 driver/浏览器）
./build.sh

# 2. 构建
docker compose build
docker compose up -d
```

`build.sh` 会：

- 交叉编译 Go 二进制到 `assets/hwcloud`（Linux amd64）
- 从已有部署里拷 `playwright-driver/` 和 `browsers/`
- 准备 `assets/data/`

如果本机没有 Go，用 `--from-server` 从已部署的服务器上取现成的二进制。

**不要**在容器里跑 `go build`：目标机器可能只有 1 GB 内存，Go 编译需要约 2 GB，会 OOM 或被 swap 拖到十几分钟。所以 Dockerfile 用的是预编译二进制。
