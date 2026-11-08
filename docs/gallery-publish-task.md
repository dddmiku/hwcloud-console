# 「在开发者作品展览馆中发布作品」任务调研（2026-11-08）

## 任务定义（从成长中心原始 JSON 拿到，Observed）

```json
{
  "title": "在开发者作品展览馆中发布作品",
  "bonus": 200,
  "degree": 3,
  "type": "PROJECT_MAN_OTHER",
  "url": "https://gallery.developer.huaweicloud.com/gallery/submit-guide",
  "guide": "1、登录华为云账号；\n2、点击【去完成】，打开开发者作品展览馆发布作品介绍页，根据页面中的操作指导完成作品发布，即可完成任务；\n3、任务周期：每月一次。",
  "rule_list": [{
    "name": "发布作品至高校运营平台",
    "behavior_code": "c24fa6f58de248fbaf61909a4287eca9",
    "mission_cycle_type": "MONTHLY",
    "mission_cycle_num": 1,
    "bonus": 200
  }],
  "start_time": "2025-12-31 16:00:00",
  "end_time": "2026-12-31 15:59:59"
}
```

**周期是 MONTHLY（每月一次）**，不是每周。当前 `is_finish: 0`。

## 官方发布流程（展览馆 submit-guide 页原文，Observed）

展览馆自己不给 Web 表单，页面明确写「只需 3 步」且第 2 步是：

> **在 AI 对话中安装 Skill 并发出发布口令，按提示完成提交。**
>
> ```
> npx skills add https://gitcode.com/Lingxi-HandsOn/gallery.git \
>   --skill huawei-cloud-publish-work-to-gallery -y
> 请将当前项目发布至开发者作品展览馆。
> ```

即：官方路径是「在 AI DevSpace 里跑 Skill」，不是填网页表单。

## Skill 的底层实现（读源码得出，Observed）

仓库 `https://gitcode.com/Lingxi-HandsOn/gallery.git` 的
`skills/huawei-cloud-publish-work-to-gallery/`：

- `SKILL.md` 定义了 8 步流水线：公告/版本检查 → 起服务/隧道 →
  Git 仓库校验 → 封面截图 → 简介 → 详情图 → 活动选择 → 发布
- `scripts/publish-work.mjs` → `POST /open-api-public/v1/gallery/works`
  （multipart：workName / introduction / image / detail(zip) / gitUrl /
  gitBranch / envUrl / trainingCampId / 可选 replaceWorkId）
- **鉴权：`X-Tmp-Ak` / `X-Tmp-Sk` / `X-Security-Token` 三个头，必须是
  STS 最小权限临时凭证**，由 `hcloud STS AssumeAgency`（自委托
  `SELF_VERIFY`）生成，需永久 AK/SK
- 成功后服务端**即时代为领取成长积分**，结果在响应 `data.reward` 返回
- **当月重复领取仍返回 `is_success: true` 但不发分** —— 无法从响应判断

## 官方推荐路径：在 AI DevSpace 里让 AI 干活（2026-11-08 第四轮）

用户确认：**正确做法是在 AI 对话里发布任务，AI 自己装 Skill 干活**，
最后让用户授权 GitCode、选活动。这条路不需要自己搞 AK/SK ——
DevSpace 环境会把临时凭据同步进去。

### 已探明的入口链路

```
hands-on-agent.developer.huaweicloud.com/#tasks
  → 点「前往AI DevSpace实践」
  → devstation.developer.huaweicloud.com/aidevspace?dc=<签名>
```

进入后要连过 4 道弹窗（都验证过结构）：

| 顺序 | 弹窗 | 真实 DOM | 处理方式 |
|---|---|---|---|
| 1 | 华为云开发者服务协议 | `input.por-native`(隐藏) + `<label for="protocolSign">` + `a#protocol-submit-btn` | 点 **label[for]**（点 span/外层 div 无效），两个都勾后才 enable |
| 2 | 临时凭据自动配置 | `.tiny-dialog-box` + `.tiny-button--primary`（文字「同意」） | 直接点 |
| 3 | CodeArts 代码智能体开通 | `label` + 「立即开通」按钮 | 勾同意后点「立即开通」 |
| 4 | 进入对话界面 | — | — |

**坑：**
- 协议弹窗的两个 checkbox 是 `input.por-native`（`opacity:0` 隐藏），
  **必须 `label[for=<id>].click()`**。点 span / 点外层 div / 直接 `inp.click()`
  都不会让 Vue 更新状态，「进入」按钮一直 disabled。
- 弹窗顺序不固定（实测先出「临时凭据」，再出「CodeArts 开通」），
  必须**每轮重新检测当前弹窗类型**，不能按固定顺序写。
- 页面上可能同时存在多个 `.tiny-button--primary`（例如别处的「同意」），
  必须**限定在弹窗容器内**再点，否则会点到无关按钮。

### 官方发布流程（submit-guide 页原文）

```
步骤 1  生成并验收作品（在 AI 对话里描述需求，AI 生成代码并运行）
步骤 2  在 AI 对话中安装 Skill 并发出发布口令，按提示完成提交
步骤 3  发布朋友圈（作品详情页点「分享作品」）
```

Skill 安装命令：

```bash
npx skills add https://gitcode.com/Lingxi-HandsOn/gallery.git \
  --skill huawei-cloud-publish-work-to-gallery -y
```

发布口令：

```
请将当前项目发布至开发者作品展览馆。
```

之后 AI 会自动：识别作品目录 → 授权身份 → 检查 Git 仓库 →
生成封面/简介/详情图 → 列出可投稿活动 → 提交 → 返回作品链接。

**用户需要做的**：在 Skill 提示时登录/授权 GitCode，以及**选活动**
（用户说选第一个即可）。

### Skill 底层调用的接口（已读源码确认）

```
POST /open-api-public/v1/gallery/works   (multipart)
Headers: X-Tmp-Ak / X-Tmp-Sk / X-Security-Token  ← STS 临时凭证
```

STS 凭证由 DevSpace 环境提供（就是「临时凭据自动配置」那个弹窗同步进去的
AK/SK），所以走这条路**不需要用户手填 AK/SK**。

---

## 实测结论（2026-11-08，第三轮，全部 Observed）


**用户提示「原本就是华为云跳转的」是对的 —— 本轮把展览馆登录 + uniPortal 绑定
全部打通了。** 之前判断「uniPortal 是独立账号体系」是错的。

### ✅ 完整链路（已验证走通）

```
1. 打开展览馆，页面自动 POST /open-web-guest/v1/gallery/auth/eui-login
   → 200 {"success":true}
   → 展览馆有了 eui 会话（eui-me 返回 nickName）

2. 带 x-csrf-token POST /open-web-user/v1/gallery/auth/oauth/bind/start
   {"providerId":"003d668803421000","consent":true}
   → 200 拿到 authorizeUrl（uniportal saaslogin1/oauth2/authorize?...）

3. 打开 authorizeUrl → 落到 uniportal 密码登录页
   关键：改成访问 /uniportal1/oauth2/hwid?redirect=<原 redirect>
   → 自动走华为云 OAuth：
     oauth-login.cloud.huawei.com/oauth2/v3/authorize
     oauth-login1.cloud.huawei.com/oauth2/v3/loginCallback
   → 落到 uniportal/firstCompleteUserInfo.html（首次自动创建 uniPortal 账号）

4. 该页两个协议勾选框是 Element UI 的 <label class="el-checkbox">，
   Vue 把点击绑在 label 上（点外层 div / 点 input 都无效，按钮会一直 disabled）：
     - 用 l.click() 点 label 本身
     - 验证 input.checked === true 后再继续
   → 点「Agree and Continue」
   → uniportal commonRedirectionPage → gallery/profile

5. 验证绑定成功：
   GET /open-web-user/v1/gallery/auth/oauth/links
   → {"data":[{"providerId":"003d668803421000","providerName":"uniPortal",
               "providerUserName":"hw...","tokenValid":true}]}
```

### 🔑 关键配置（从 uniportal app-policy 接口读到的）

```
GET https://uniportal.huawei.com/uniportal1/rest/hwidcenter/app-policy

{
  "enablehwidLogin": 1,
  "hwLoginAddress": "/oauth2/hwid",        ← 华为云账号登录入口
  "hwLoginAddressMobile": "/jssdklogin",
  "loginType": ["account","sms"],
  "accountLoginMode": "accountName,mobile,email,w3Account",
  "thirdPartyAllow": true,
  "tenantId": 5
}
```

`enablehwidLogin:1` + `hwLoginAddress:/oauth2/hwid` 就是「华为云账号直接登录
uniPortal」的开关。**uniPortal 账号会在首次登录时自动创建**，
不需要单独注册，也不需要 W3 账号。

### 🔑 接口名坑：my-works 是旧接口

| 接口 | 结果 |
|---|---|
| `GET /open-web-user/v1/gallery/my-works` | ❌ 401 `not_authenticated`（旧接口，已废弃） |
| `GET /open-web-user/v1/gallery/me/works` | ✅ 200 `{"items":[],"total":0}` |

**必须用 `/me/works`**。前端 bundle 里两个都有，但只有后者有效。

其它已验证可用：

```
GET  /open-web-user/v1/gallery/auth/eui-me          → 200
GET  /open-web-user/v1/gallery/auth/me              → 200 {"accountId":...,"user":null}
GET  /open-web-user/v1/gallery/auth/oauth/links     → 200 已绑定 uniPortal
GET  /open-web-user/v1/gallery/me/works             → 200 作品列表
GET  /open-web-user/v1/gallery/me/signature-status  → 200
```

### 现在只剩两个门槛

| 环节 | 状态 |
|---|---|
| 展览馆 eui 登录 | ✅ 已打通 |
| uniPortal（华为云账号）绑定 | ✅ 已打通，账号已自动创建 |
| `me/works` 查作品 | ✅ 已通，当前 0 个作品 |
| **发布作品（open-api-public/works）** | ❌ 仍需 STS 临时凭证 |
| **可发布的真实项目** | ❌ 没有 |

发布接口的鉴权与上面这套 cookie/eui 体系**完全无关**，它只认
`X-Tmp-Ak / X-Tmp-Sk / X-Security-Token`（STS 最小权限临时凭证），
必须用永久 AK/SK + IAM 自委托生成。

---

## 第二轮结论（已修正，保留作对照）

第二轮结论被本轮推翻的部分：*「uniPortal 是独立账号体系，用华为云账号
登录报 Incorrect account name or password」* —— 那是因为走的是
**密码登录页**（`login-pc.html#/passwordLogin`），那里确实不认华为云账号。
正确入口是 `/uniportal1/oauth2/hwid`（华为云 OAuth 跳转）。

---

## 第二轮实测记录（历史）

### ✅ 突破 1：展览馆登录可以成功

之前 429 是因为**直接 fetch `eui-login` 太频繁**。让页面自己触发就成功：

```
[REQ]  POST /open-web-guest/v1/gallery/auth/eui-login
[RESP 200] {"data":{},"message":"成功","success":true}
[RESP 200] /open-web-user/v1/gallery/auth/eui-me
    {"data":{"id":"007fdca97272a000","nickName":"hw...","domainIdMasked":"06***c0",
             "isTeacher":false,"isStudent":false}}
```

**关键**：`eui-login` 是「用华为云会话静默换取展览馆登录态」，
成功后只在**当前浏览器上下文**里有效（换 context 就没了），
所以必须**同一个浏览器会话内完成登录 → 发布**，不能分两次跑。

限流是按出口 IP 计（服务器固定 IP `124.71.113.188`），
冷却窗口约 **60~90 秒**（实测等 90 秒后成功）。

### ✅ 突破 2：`bind/start` 能拿到授权 URL（之前被 CSRF 挡住了）

之前直接 POST 报 `403 CSRF 校验失败`。前端代码里写得很清楚：

```js
// api.js 的非 GET 请求统一加：
const csrf = cookie('__Host-bp_csrf') || cookie('bp_csrf');
headers['x-csrf-token'] = csrf;
```

补上这个头后成功：

```json
{"success":true,"data":{
  "authorizeUrl":"https://uniportal.huawei.com/saaslogin1/oauth2/authorize?client_id=f7358fb0-...",
  "state":"5ced0be2-..."}}
```

### ❌ 硬阻塞：uniPortal 是独立账号体系，不是华为云账号

授权页跳到了 `uniportal.huawei.com/uniportal1/login-pc.html`，表单是：
`#username` / `#password` / `#login-btn`，标题「Sign In with Uniportal ID」。

用华为云手机号 + 密码试了一次：

```
Incorrect account name or password, you can try 4 times
```

**uniPortal 是华为内部的 W3 账号体系，和华为云账号不通用。**
页面上的「Use another account to log in」只是文本，没有可点的替代登录入口。

### 结论

| 环节 | 状态 |
|---|---|
| 展览馆 eui 登录 | ✅ 可自动化（同会话内，等限流冷却） |
| `bind/start` 拿授权 URL | ✅ 可自动化（补 `x-csrf-token`） |
| uniPortal 授权 | ❌ **需要 W3 账号**，不是华为云账号 |
| `open-api-public/works` | ❌ 需要 STS（AK/SK + 自委托），服务器没有 |
| 可发布的真实项目 | ❌ 没有 |

**卡点从「展览馆登录」转移到了「uniPortal 账号」**。即便拿到 uniPortal 账号完成
GitCode 绑定，发布仍需 STS 临时凭证。

## 一个关键侧面发现

`my-works` 带 CSRF 后仍返回：

```json
{"success":false,"code":"not_authenticated","reason":"请先登录"}
```

而同一个会话里 `eui-me` 是 200。说明展览馆有**两套会话**：
- `eui-*`（华为云 EIAM 通道）→ `eui-me` 能过
- `open-web-user` 的业务会话（`my-works` / `oauth/links` 用）→ 需要 uniPortal 授权后才有

所以 `oauth/links` 返回 `{"success":true,"data":[]}`（空数组）不是错误，
就是「还没绑定任何 OAuth 账号」。

## 原探测记录（第一轮）

| 探测项 | 结果 |
|---|---|
| Web 端是否有发布表单 | **没有**。前端 bundle 里只有读接口 + 点赞/分享/隐藏/改简介，无 create-work |
| `open-web-guest/v1/gallery/camps` | ✅ 200，79 个活动，**不需要任何凭证** |
| `open-api-guest/v1/gallery/announcements/current` | ✅ 200 |
| `open-api-public/v1/gallery/camps` | ❌ 400 `GALLERY.PARAM.MISSING 缺少最小权限 STS 临时凭证` |
| `open-api-public/v1/gallery/works` (POST) | ❌ 400 同上（同源带 cookie 也一样） |
| `open-web-user/v1/gallery/auth/eui-me`（同源带 cookie） | ❌ 401 `未认证或鉴权失败` |
| `open-web-user/v1/gallery/auth/eui-login` | ❌ 429 `登录尝试过于频繁`（探测次数过多被限流） |
| `open-web-user/v1/gallery/auth/oauth/bind/start` | ❌ 401（未登录） |
| 展览馆 OAuth provider | 只有 `uniPortal`（`id=003d668803421000`），即华为云统一登录 |

**关键点：展览馆有独立的登录态（`eui-me` 那套），华为云 cookie 不等于展览馆登录态。**
导航栏里显示的 `hw00000001` 只是华为云公共头，不代表展览馆已登录。

## 三个硬依赖（缺一不可）

1. **展览馆登录态**：`POST /open-web-guest/v1/gallery/auth/eui-login` 用华为云
   会话静默换取。当前被 429 限流（服务端按 IP/账号计数，窗口较长）。
2. **STS 临时凭证**：发布接口只认 `X-Tmp-Ak/Sk/Security-Token`，需要
   - 永久 AK/SK（`HUAWEICLOUD_SDK_AK/SK`）
   - IAM 自委托 `SELF_VERIFY`（`hcloud IAM CreateAgency`，账号 ID = Domain ID）
   - `hcloud STS AssumeAgency` 生成 900s 临时凭证
   - 本服务器**没有 hcloud，也没有存任何 AK/SK**
3. **一个可发布的真实项目**：要 `gitUrl`（GitCode 公仓）+ `gitBranch` +
   可访问的本地服务截图 + 详情 zip。目前没有现成作品项目。

## 可选落地路径

### 路径 A：补齐 AK/SK + hcloud（正统做法，最稳）

1. 在华为云控制台「我的凭证 → 访问密钥」创建永久 AK/SK
   （`console.huaweicloud.com/iam/?region=cn-north-4#/iam/myCredential`，
   实测该 hash 路由会跳回 overview，需从 UI 菜单进）
2. 服务器装 KooCLI（`hcloud`）
3. 创建自委托：`hcloud IAM CreateAgency --agency.name=SELF_VERIFY ...`
4. 跑官方 Skill：`npx skills add ... --skill huawei-cloud-publish-work-to-gallery`
5. 需要一个真实项目仓库

代价：要用户提供 AK/SK（长期凭据，风险高）；要准备一个作品项目；
首次跑 Skill 还要装字体 + 下载 Chromium（5~10 分钟）。

### 路径 B：只做「作品发布」的最小复刻（不跑完整 Skill）

绕开 Skill 的截图/封面/图表流水线，直接用 Go 复刻 `publish-work.mjs`：

- 需要：STS 三件套（同路径 A 的 1~3）
- 需要：手工准备 `image`（封面 png）+ `detail`（含 README.md 的 zip）
- 需要：一个 `gitUrl` + `gitBranch`
- 直接 `POST /open-api-public/v1/gallery/works` multipart

比路径 A 少装一堆依赖，但同样要 AK/SK 和一个真实项目。

### 路径 C：纯浏览器（不可行）

展览馆没有 Web 发布表单，`eui-login` 之后也没有 create-work 接口。
浏览器只能完成「授权/登录」，发布本身必须走 OpenAPI。**这条路不存在。**

## 当前阻塞

- 展览馆 `eui-login` 被限流（429），今天无法继续验证登录态
- 服务器无 AK/SK、无 hcloud
- 没有可发布的真实作品项目

## 需要用户提供的东西

1. **华为云 AK/SK**（如果愿意；或者用户自己在控制台创建后填进环境变量）
2. **一个可发布的 GitCode 公仓**（项目代码 + 分支），以及它能跑起来的说明
3. 或者明确指示：**放弃这个任务**（它是 3 星、+200 分、每月一次，
   投入产出比低，且需要长期凭据）

## 相关代码位置

本次未改动任何生产代码。可能的落点：

- 新增 `go/internal/task/gallery.go`：STS 获取 + multipart 发布
- 面板加「发布作品」按钮
- 定时任务：每月检查 `is_finish`，未完成则跑一次

## 附：任务清单里同级别的其它任务

- 每周通过一项微认证（+200）：要真去考试，不可自动化
- 建议被实现（+200）：取决于官方采纳
- 每周参与开发者赛事（+100）：已完成
