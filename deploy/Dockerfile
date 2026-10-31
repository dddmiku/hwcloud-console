# hwcloud 控制台 —— 单容器镜像（含自动登录）
#
# `docker compose up -d` 一条命令跑起来。用户不需要装 Go、不需要编译、
# 不需要手动下载任何浏览器。
#
# ── 构建前置 ──
# 镜像用**预编译的 Linux 二进制**，不在容器里跑 go build。
# 原因：目标机器可能只有 1 GB 内存，Go 编译需要 ~2 GB，
# 在容器里编译会 OOM 或被 swap 拖到十几分钟。
# 预编译由 deploy/build.sh 完成（它会在有 Go 的机器上编，
# 或者从已部署的服务器上取）。
#
# 需要准备的文件（deploy/assets/）：
#   hwcloud                       Linux amd64 二进制
#   playwright-driver/            Playwright node 驱动 1.61.1
#   browsers/chromium-1228/       Chromium 149（GitCode 用）
#
# ── 两套浏览器，各司其职 ──
#   Chromium (Playwright)  —— GitCode 的 Star / 提 Issue / 活动报名
#   Camoufox (定制 Firefox) —— 华为云自动登录（过易盾滑块）
#
#   华为云必须用 Camoufox：华为 SSO 的 fp 是 SDK 采集真实浏览器环境后
#   加密生成的，纯 HTTP 造不出来，Playwright 的指纹也会被识别。
#
#   GitCode 用 Chromium 更快：它不需要反检测，Camoufox 启动慢得多。
#
# ── Camoufox 浏览器（约 2.4 GB）不打进镜像 ──
#   改为首次启动时自动下载到数据卷：
#     - 镜像保持 ~700 MB，拉取快
#     - 下载一次后持久化，重启容器不重下
#     - 下载失败不影响面板和 GitCode 任务，只是华为云自动登录不可用

FROM debian:bookworm-slim

# 换国内 apt 源。
#
# 构建机在国内，直连 deb.debian.org 时 apt 下载能拖十几分钟甚至超时。
# 用阿里云镜像，同样的包几十秒装完。
# 注：bookworm 的源格式是 deb822（/etc/apt/sources.list.d/debian.sources），
# 不是老的 sources.list，所以要改对文件。
RUN sed -i 's|deb.debian.org|mirrors.aliyun.com|g; s|security.debian.org|mirrors.aliyun.com|g' \
        /etc/apt/sources.list.d/debian.sources 2>/dev/null \
    || sed -i 's|deb.debian.org|mirrors.aliyun.com|g; s|security.debian.org|mirrors.aliyun.com|g' \
        /etc/apt/sources.list

# 系统依赖（精简版）。
#
# 只装真正需要的，每个包都要能说出用途 —— 多装一个就多几分钟构建时间，
# 而这台构建机带宽有限：
#   ca-certificates  访问 https（华为云、GitCode）
#   tzdata           时区（任务按 CST 计算日期）
#   curl             健康检查 + 下载 Camoufox
#   xvfb             虚拟显示 —— 两套浏览器都是有头模式
#   x11vnc           人工过验证码时看画面（默认不启动）
#   fonts-noto-cjk   中文字体，缺了网页全是方块
#   python3 + venv   Camoufox 运行时
#   unzip            解压 Camoufox 浏览器包
#   libnss3/libatk*  Chromium 运行时依赖（缺了报 libnss3.so not found）
#   libgtk-3-0       Camoufox（Firefox）运行时依赖
#
# 刻意不装：xvfb 之外的其他 X 组件、firefox 包（Camoufox 自带）、
# libasound2（无声卡环境用不到，但 Chromium 会警告不影响运行）
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates tzdata curl \
        xvfb x11vnc \
        fonts-noto-cjk \
        python3 python3-venv \
        unzip \
        libnss3 libatk1.0-0 libatk-bridge2.0-0 libcups2 libdrm2 \
        libxkbcommon0 libxcomposite1 libxdamage1 libxfixes3 libxrandr2 \
        libgbm1 libpango-1.0-0 libcairo2 \
        libgtk-3-0 libdbus-glib-1-2 libxt6 \
    && rm -rf /var/lib/apt/lists/*

ENV TZ=Asia/Shanghai \
    HWCLOUD_HOME=/opt/hwcloud-demo \
    PLAYWRIGHT_BROWSERS_PATH=/opt/hwcloud-demo/browsers \
    PLAYWRIGHT_DRIVER_PATH=/opt/hwcloud-demo/playwright-driver \
    CAMOUFOX_HOME=/opt/hwcloud-demo/camoufox \
    DISPLAY=:99 \
    PIP_NO_CACHE_DIR=1 \
    PYTHONUNBUFFERED=1

WORKDIR /opt/hwcloud-demo

# Playwright 驱动 + Chromium。
# 版本必须与 go.mod 里的 playwright-go v0.6100.0 对应
# （driver 1.61.1 / Chromium revision 1228），版本错了浏览器起不来，
# 报的错是 "Executable doesn't exist"，很难看出是版本问题。
COPY assets/playwright-driver/ ./playwright-driver/
COPY assets/browsers/ ./browsers/

# Camoufox：华为云自动登录用的定制 Firefox。
#
# 为什么现在打进镜像（早先版本是让用户自己装）：
#   早先顾虑是「浏览器本体 2.4 GB 会让镜像涨到 3 GB」。
#   但现在构建跑在 GitHub Actions 上，runner 网络通畅、磁盘充足，
#   构建成本不再是问题；而用户自己装要过 GitHub 下载 + 国内网络，
#   失败率很高，装完还得重启容器。镜像自带是更好的体验。
#
# 关键设计：浏览器装到 /opt/hwcloud-demo/camoufox-baked，
# 而运行时 CAMOUFOX_HOME 指向挂载的 /opt/hwcloud-demo/camoufox。
# entrypoint 会检查挂载目录，为空就把 baked 的复制过去。
# 这样：
#   - 镜像升级不会冲掉用户已下载的版本（在挂载卷里）
#   - 首次启动不用联网下载，复制一份即可（秒级）
#
# pip 用清华源（国内构建快）。
# 浏览器下载走 camoufox 自带的 GitHub Releases 拉取逻辑，
# 安装位置由 XDG_CACHE_HOME 控制（见下面那段）。
RUN python3 -m venv /opt/hwcloud-demo/venv \
    && /opt/hwcloud-demo/venv/bin/pip install --no-cache-dir --upgrade pip \
        -i https://pypi.tuna.tsinghua.edu.cn/simple \
    && /opt/hwcloud-demo/venv/bin/pip install --no-cache-dir \
        -i https://pypi.tuna.tsinghua.edu.cn/simple \
        "camoufox[geoip]==0.5.8" "playwright==1.62.0"

# 预下载浏览器到镜像内的 baked 目录。
#
# 坑：camoufox **没有** CAMOUFOX_INSTALL 这个环境变量。
# 它的安装目录写死成 `Path(user_cache_dir("camoufox"))`，
# 也就是 XDG 缓存目录：Linux 上是 $XDG_CACHE_HOME/camoufox
# （默认 ~/.cache/camoufox）。
# 所以要改位置只能设 XDG_CACHE_HOME。
#
# 注意这个 ENV 只在这一步生效，装完就删掉 ——
# 留着的话运行时所有程序（pip、node 等）的缓存都会写到这个目录，
# 而它不该是运行期可写的。
RUN export XDG_CACHE_HOME=/tmp/camoufox-build \
    && /opt/hwcloud-demo/venv/bin/python -m camoufox fetch \
    && mkdir -p /opt/hwcloud-demo/camoufox-baked \
    && cp -a /tmp/camoufox-build/camoufox/. /opt/hwcloud-demo/camoufox-baked/ \
    && rm -rf /tmp/camoufox-build \
    && du -sh /opt/hwcloud-demo/camoufox-baked \
    && ls /opt/hwcloud-demo/camoufox-baked/browsers/

# 应用
COPY assets/hwcloud ./go/bin/hwcloud
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
COPY camoufox_server.py /opt/hwcloud-demo/camoufox_server.py
COPY assets/data/ ./data/

RUN chmod +x ./go/bin/hwcloud /usr/local/bin/entrypoint.sh \
    && mkdir -p logs accounts state camoufox

EXPOSE 8899

# 健康检查：/api/auth/state 未登录也返回 JSON，适合探活。
# start-period 给 60 秒 —— 首次启动要先起 Xvfb 和 Camoufox 服务，
# 慢机器上 30 秒不够，会被误判成 unhealthy。
# 注意 Camoufox 浏览器下载是**后台**进行的，不阻塞面板启动，
# 所以不需要为它延长 start-period。
HEALTHCHECK --interval=30s --timeout=5s --start-period=60s --retries=3 \
    CMD curl -fsS http://127.0.0.1:8899/api/auth/state >/dev/null || exit 1

# entrypoint：拉起 Xvfb → 按需下载 Camoufox → 起 Camoufox 服务 → 主进程
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
CMD ["serve", "--addr", ":8899"]
