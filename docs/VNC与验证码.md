# VNC + GitCode 验证码操作手册

GitCode 的**账密登录**会弹网易易盾的「拖动白色排球，躲避障碍击中」验证码。
这个验证码**无法自动破解** —— 它的轨迹由 canvas 记录并与背景障碍做碰撞校验，
模板匹配只能猜起点，猜错几次就会锁（"失败过多，点此重试"）。

所以流程是：脚本负责机械部分（打开页面、填账密、勾协议、点登录），
**滑块由人在 VNC 里手动拖一次**。过了之后会话会被导出，之后几个月都能复用。

---

## 一、什么时候需要做这一步

只有**首次绑定某个 GitCode 账号**，或 **refresh_token 过期**（约几个月）时才需要。

判断方法：

```bash
hwcloud status          # 看 bound 是否为 true
ls state/gitcode_state.json   # 文件存在 = 已有会话
```

如果 `gitcode_state.json` 存在且没报「会话失效」，就不用管这一步。

---

## 二、服务器端准备（一次性）

### 1. 确认 Xvfb 在跑

浏览器必须是**有头模式** —— GitCode 对无头浏览器做了行为判定，
访问 `/issues/create` 会被 302 回列表，表现成「会话失效」，很难排查。

```bash
ps aux | grep -c '[X]vfb'     # 应该是 1 或 2
# 没有的话：
Xvfb :99 -screen 0 1400x1000x24 &
```

### 2. 启动 x11vnc

```bash
# 设密码（只做一次）
x11vnc -storepasswd <你的VNC密码> /etc/x11vnc.pass

# 启动。注意 -localhost：只监听 127.0.0.1，
# 外部必须经 SSH 隧道进来。不要改成 0.0.0.0 —— 那等于把桌面暴露到公网。
x11vnc -display :99 -localhost -rfbauth /etc/x11vnc.pass \
       -forever -shared -rfbport 5901
```

做成 systemd 常驻（推荐，见 `deploy/hwcloud-vnc.service`）：

```bash
cp deploy/hwcloud-vnc.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now hwcloud-vnc
systemctl is-active hwcloud-vnc      # active
ss -tlnp | grep 5901                 # 127.0.0.1:5901
```

---

## 三、从本地连上 VNC

### 方式 A：SSH 隧道 + VNC 客户端（推荐）

**第一步**，本地开一个终端，建隧道并保持不关：

```bash
ssh -L 5901:127.0.0.1:5901 root@<SERVER_IP>
```

Windows 上如果每次都要手输密码很烦，可以用脚本自动建（见 `tools/vnc_tunnel.py`，
改掉里面的 HOST/PASSWORD 后 `python vnc_tunnel.py`）。

**第二步**，另开一个终端/窗口，用 VNC 客户端连本地端口：

| 客户端 | 命令 |
|---|---|
| TigerVNC | `vncviewer 127.0.0.1:5901` |
| RealVNC | `vncviewer.exe 127.0.0.1:5901` |
| TightVNC | `tvnviewer.exe 127.0.0.1:5901` |
| macOS | `open vnc://127.0.0.1:5901` |

地址填 `127.0.0.1:5901`，密码是上面 `-storepasswd` 设的那个。

> **注意**：Windows 上 RealVNC 的默认安装路径是
> `C:\Program Files\RealVNC\VNC Viewer\vncviewer.exe`。
> 如果你装的是 TigerVNC，路径是 `C:\Program Files\TigerVNC\vncviewer.exe`。
> 不确定就先搜一下：
> ```powershell
> Get-ChildItem "C:\Program Files","C:\Program Files (x86)" -Filter "*vnc*.exe" -Recurse
> ```

### 方式 B：直接暴露端口（不推荐）

把 x11vnc 的 `-localhost` 去掉，然后在云服务器安全组放行 5901。
**风险**：VNC 协议本身加密很弱，等于把 root 桌面挂到公网。
只在内网或临时排障时用，用完立刻关掉。

---

## 四、手动过验证码

### 1. 在服务器上跑登录脚本

```bash
cd /opt/hwcloud-demo
HWCLOUD_HOME=/opt/hwcloud-demo \
PLAYWRIGHT_DRIVER_PATH=/opt/hwcloud-demo/playwright-driver \
PLAYWRIGHT_BROWSERS_PATH=/opt/hwcloud-demo/browsers \
HWCLOUD_ACCOUNT=<账号ID> \
DISPLAY=:99 \
python3 tools/gc_manual_login.py <账号ID>
```

脚本会依次做：

```
[1] 打开登录页（账号 xxx）
[2] 已切到密码登录（tab='密码登录'）
[3] 已填账密
[4] 已勾协议
[5] 已点登录 —— 现在请用 VNC 连上来看画面
```

然后进入最长 840 秒的等待循环，每 28 秒打印一次状态。

### 2. 在 VNC 里拖滑块

连上 VNC 后你应该能看到浏览器窗口，中间是验证码弹窗：

- 画面顶部 240px 是游戏区，底部 120px 是图例
- 左下角有个白色排球（约 25×25）
- 用鼠标**按住白球**，横向拖到目标图标处
- 中途不能碰到障碍物

> **拖的时候手要稳，一次过。** 失败 3-5 次会锁死，只能等几分钟或换 IP。

### 3. 等脚本自动收尾

过了验证码后，脚本每 28 秒检查一次 `localStorage.userInfo`，
检测到就立刻导出会话：

```
    [56s] 无验证码 url=https://gitcode.com/ 剩余 784s
    >>> 登录成功: {"id":"...","object_id":"..."
[OK] 会话已导出 -> /opt/hwcloud-demo/accounts/<账号ID>/state/gitcode_state.json (16203 字节)
```

看到 `[OK]` 就成了，VNC 可以关掉。

---

## 五、绑定 GitCode 到华为云账号（关键！）

**光登录 GitCode 是不够的。** 华为云判定「每周给代码仓提 Issue」「每周 Star 代码仓」
是否完成，靠的是 GitCode 账号 ↔ 华为云账号的**绑定关系**。

不绑定的话，Issue 发得再多、Star 点得再多，任务页永远显示「未完成」。
（实测：Issue #983 已成功发出，任务仍是未完成，绑定后立刻变已完成。）

```bash
hwcloud bind --gitcode <GitCode用户名>
```

验证：

```bash
hwcloud status
# 期望输出：
# {
#   "bound": true,
#   "binding": {
#     "gitcode_username": "...",
#     "hw_account_name": "...",
#     ...
#   }
# }
```

绑定接口不需要 GitCode 登录态，只要华为云会话 + GitCode 用户名：

```
POST https://hands-on-agent.developer.huaweicloud.com/api/binding/create
{"gitcode_username":"...","hw_account_name":"...","hw_domain_id":"...",
 "hw_user_id":"...","hw_project_id":"..."}
```

---

## 六、注意事项

### 环境变量必须齐全

CLI 直接跑和面板跑的环境不一样。手工跑 CLI 时必须带上：

```bash
HWCLOUD_HOME=/opt/hwcloud-demo
PLAYWRIGHT_DRIVER_PATH=/opt/hwcloud-demo/playwright-driver
PLAYWRIGHT_BROWSERS_PATH=/opt/hwcloud-demo/browsers
HWCLOUD_ACCOUNT=<账号ID>
DISPLAY=:99
```

漏掉 `PLAYWRIGHT_BROWSERS_PATH` 会报：
`Executable doesn't exist at /root/.cache/ms-playwright/chromium-1228/...`
漏掉 `DISPLAY` 会报 `Missing X server or $DISPLAY`。

### 浏览器 UA 必须是 Chrome

华为云记录的 UA 是 Firefox，但 GitCode 的 WAF 会检测
「UA 说自己是 Firefox、TLS/JA3 和 JS 指纹却是 Chromium」→ 直接返回 **418**
（"The access is blocked"）。

所以 GitCode 相关的浏览器必须用 `config.DefaultUA`（Chrome），
不能用 `config.UserAgent()`（华为云那条 Firefox）。代码里已经处理，
自己写脚本时注意别用错。

### 协议勾选框要勾两个

登录页有**两个**协议勾选框，它们的真实 `<input>` 都是 `0x0` + `opacity:0`，
直接 `fill()` / `check()` 都不行，必须点 `label` 或 `.devui-checkbox`。
少勾一个，点登录没有任何反应（也不报错）。

### 无头模式不行

GitCode 对无头浏览器做行为判定，`headless=True` 时访问 `/issues/create`
会被 302 回 issues 列表。必须 `headless=False` + `DISPLAY=:99`。

### refresh_token 有效期

- `GITCODE_ACCESS_TOKEN`（JWT）约 **24 小时**
- `GITCODE_REFRESH_TOKEN` 约 **几个月**

页面加载时前端会自动用 refresh_token 换新的 access_token，
所以**不要**每次都主动重登 —— 主动重登反而会触发验证码。
只有在真正提交失败时才重登一次。

### Issue 发出去就删不掉

GitCode 规则：Issue 一旦有参与者（评论、被指派、被 `grape_bot` 自动回复），
创建者就**无法删除**：

```json
{"error_code":400,"error_message":"There are other participants"}
```

页面上「删除 issue」按钮会变成 `disabled`。误发的 Issue 只能用
`PUT {state_event:"close"}` 关掉，不能删。

所以周常里**必须先查任务是否已完成**（`weeklyMissionDone`），
本周已达标就别再发，否则会往仓库里刷重复内容。

### Star 要挑没点过的

同一个仓库重复 star 不会重复计分。周常的实现是遍历组织下所有仓库，
读按钮文字判断（`Star` = 未 star，`Starred` = 已 star），
挑第一个未 star 的点。这样每周自然选到新的。

两个坑：
1. 仓库列表接口里 `path_with_namespace` 是 `null`，必须用 `html_url`
   拼 URL。用前者会得到 `https://gitcode.com/None`，打开当然没有按钮。
2. star 按钮被折叠成 `0x0`，Playwright 的 `click()` 会报
   `element is not visible` 并无限重试。必须用 JS 的 `el.click()`。

### 参加开发者活动要填表

这个任务不是调接口，是真的填报名表单：

```
活动列表 → 点「报名进行中」卡片 → 新标签打开详情页
  → 点「立即报名」展开表单（不是一开始就渲染）
  → 填 邮箱 / 学校公司 / 岗位
  → 点「报名」
```

- 姓名、手机是实名信息，页面显示成 `张**` / `157****9941`，**不要改**
  （改了会破坏实名关联）
- 岗位是 `div.select-box` 自定义下拉，不是 `<select>`，要先点开再点选项
- 「立即报名」里含「报名」二字，用 `/已报名/` 判断会误判成已报名
- 活动卡片是**懒渲染**的：刚加载时 `.salesPro-box` 在 DOM 里但
  `getBoundingClientRect()` 全是 0，必须先滚一遍页面触发渲染
- 卡片坐标要用 DOM 点击（`el.click()`），不要用鼠标坐标点击 ——
  卡片可能在视口外，坐标点击会落空

### 面板卡在「加载中…」

现象：打开面板，账号数显示 0，一直转圈。

根因：`/api/accounts` 会**同步**探测每个账号的会话和积分，而探测要过
`withAccount`（全局锁）。只要有长任务在跑（保活 3 分钟、周常、云实验
几分钟），列表请求就排队干等。

修法：
- 列表接口读缓存秒回，探测放后台异步刷新
- 探测用 `TryLock`，拿不到锁就退回缓存，不阻塞
- 后台刷新加单飞标志，防止协程堆积

### playwright-go 的数字类型陷阱

`Evaluate()` 返回的 JS 数字**不一定**是 `float64` —— 整数会被解成 `int`。
直接写 `v.(float64)` 会静默得到 `0`，不报错。

实测踩坑：活动卡片的宽度用 `c["w"].(float64)` 取值，全部变成 0，
过滤后一个卡片都不剩，表现成「没有可点击的活动卡片」，排查了很久。

统一用类型转换函数处理：

```go
func toF(v any) float64 {
	switch n := v.(type) {
	case float64:    return n
	case float32:    return float64(n)
	case int:        return float64(n)
	case int64:      return float64(n)
	case json.Number: f, _ := n.Float64(); return f
	default:         return 0
	}
}
```

### 进程泄漏

Playwright 启动的浏览器是独立进程树（chromium + zygote + renderer + gpu）。
Go 里虽然有 `defer br.Close()`，但下列情况仍会残留：

- 面板被 `systemctl restart`（SIGTERM 后 10 秒 SIGKILL，defer 来不及跑完）
- 任务超时后被上层直接杀掉
- 浏览器自身卡死（如卡在验证码页），Close 请求挂住

实测一次登录卡住后残留了 12 个 chromium 进程（约 200-400 MB）。

兜底：`deploy/hwcloud-clean-browsers.sh` + 每 30 分钟的 timer，
只清理「属于我们的 profile 目录 + 存活超过 15 分钟 + 没有活跃父进程」的进程。

```bash
hwcloud-clean-browsers.sh --dry-run    # 先看会清理什么
hwcloud-clean-browsers.sh 900          # 真清理（900 秒阈值）
```

### VNC 安全

- `x11vnc` 必须加 `-localhost`，只监听 `127.0.0.1`
- 外部一律经 SSH 隧道进来
- VNC 密码单独设，别用服务器 root 密码
- 用完可以 `systemctl stop hwcloud-vnc`，下次需要再启
