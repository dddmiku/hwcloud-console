# 每日任务：浏览器链路改造依据（2026-11-06 立项，2026-11-07 验证通过）

## 结论（2026-11-07 已验证）

四个每日任务**全部改走真实浏览器页面后，成长中心全部认账**，积分已到账：

```
+10  每日浏览一个鸿蒙案例
+30  每日Star一个鸿蒙案例代码仓
+60  每日发布文章
+30  每日体验AI DevSpace
（+10 签到第7天）
```

纯协议能「做完」任务，但**成长中心判定不通过**。原因是成长中心靠各业务站的
`behavior_code` 事件判定，纯协议调用不产生这个事件。

## 零、验证结果（2026-11-07，账号 13800138001）

| 任务 | 浏览器链路 | 结果 |
|---|---|---|
| 每日浏览一个鸿蒙案例 | 打开 `/cases/<id>`，保持前台 40 秒 | `is_finish=1` ✅ |
| 每日Star一个鸿蒙案例代码仓 | 打开 GitCode 案例仓 → 关引导 → 点 Star | `completed=true`, `completed_at` 已有 ✅ |
| 每日发布文章 | `bbs.huaweicloud.com/blogs/article` 编辑器填标题/正文/发布 | `is_finish=1` ✅ |
| 每日体验AI DevSpace | `hands-on-agent...#tasks` → 点「前往AI DevSpace实践」→ 进 AI 开发者空间 | `is_finish=1` ✅ |

`hwcloud daily` 端到端跑通：5 步成功、0 失败、总耗时 56 秒。

## 一、已确认的事实（Observed）

### 1. 任务确实曾经完成过（积分明细 bonus-flow）

接口：`POST https://devdata.huaweicloud.com/rest/developer/fwdu/rest/developer/user/hdcommunitysoservice/v1/growth/bonus-flow`
body: `{"page_no":1,"page_size":50}`，返回 `commit_time` 是 **UTC**（+8 小时 = 北京时间）。

账号 13800138001（YD_000000001 / hw00000001）：

```
2026-10-31 17:39:48 +60 每日发布文章
2026-10-31 17:44:28 +30 每日体验AI DevSpace
2026-11-02 02:34:02 +60 每日发布文章
2026-11-02 02:36:05 +30 每日体验AI DevSpace
2026-11-03 03:37:39 +10 每日浏览一个鸿蒙案例
2026-11-03 03:56:24 +30 每日Star一个鸿蒙案例代码仓
2026-11-04 03:00:00 +60 每日发布文章
2026-11-04 03:00:00 +10 每日浏览一个鸿蒙案例
2026-11-04 03:00:00 +30 每日Star一个鸿蒙案例代码仓
2026-11-05 02:47:23 +30 每日体验AI DevSpace
2026-11-05 02:47:23 +60 每日发布文章
2026-11-05 02:47:23 +10 每日浏览一个鸿蒙案例
2026-11-05 02:54:45 +30 每日Star一个鸿蒙案例代码仓
2026-11-06 01:39:08 +10 签到第6天      <-- 今天只有签到，4 个每日任务都没到账
```

注意 11-04 / 11-05 有「同一秒批量入账」（03:00:00、02:47:23），说明成长中心
有延迟批处理；但 11-06 只有签到，4 个任务全天都没入账。

### 2. 任务指南原文（成长中心「任务指南」弹窗抓到的）

| 任务 | 指南原文 |
|---|---|
| 每日浏览一个鸿蒙案例 | 在鸿蒙开发实践中心浏览任意一个案例详情页超过30秒 |
| 每日Star一个鸿蒙案例代码仓 | 在鸿蒙开发实践中心，打开任一案例详情页，前往GitCode代码仓，点击Star按钮。每日需Star不同项目且不可取消Star |
| 每日体验AI DevSpace | 访问AI办公开发实践中心，并体验AI DevSpace |
| 每日发布文章 | 在华为云开发者社区中发布博客文章 |
| 建议被采纳 | 在云声提交的建议被采纳 |

### 3. 两个系统没打通

鸿蒙实践中心 `GET /api/growth/tasks` 返回：

```json
{
  "task_key": "case-view-daily",
  "today_status": "done",       <-- 鸿蒙自己认为完成了
  "record_status": "done",
  "completed_at": 1793929179890
}
```

但成长中心 `DAILY_MISSIONS` 里同名任务 `is_finish: 0`。
=> 鸿蒙的 dwell 判定和成长中心的 behavior_code 上报是两套独立判定。

### 4. GitCode Star 的核验缺口（直接证据）

鸿蒙 `POST /api/growth/tasks/gitcode-star/submit {"case_id":null,"payload":{"action":"check"}}`
返回：

```json
{ "completed": false,
  "progress": { "matched": [], "missing": ["org:harmony-practice-center"], "failed": [] } }
```

说明华为云侧核验要去 GitCode 查「当前绑定账号是否 star 了该 org 下的仓」，
查不到。我们之前是直接 `POST https://gitcode.com/api/v2/projects/<id>/star`，
绕过了「案例详情页 → 前往代码仓」这个来源链路。

**另一个坑**：`GET /api/v5/orgs/harmony-practice-center/repos` 返回 80 个仓库，
第 0 个是 `org-desc`（组织介绍页，id=11252840），不是案例仓。
旧代码「挑星数最少的」很可能选中它。

### 4b. 案例 ↔ GitCode 仓库映射（这是明日方案的钥匙）

`GET https://harmonybp.developer.huaweicloud.com/api/cases?limit=200`（注意：
`page_size=100` 会 422，只能用 `limit`；默认只返回 6 条，共 72 个案例）

返回的每个案例都有 `publicRepo` 字段，就是该案例绑定的 GitCode 仓库：

```
单词打卡学习     id=8064f6e996cd42d3b20bb88f8a96ade5  repo=harmony-practice-center/word-check-in.git
个人记账本       id=09441243b98f4e12bf14cc7c2d1aebec  repo=harmony-practice-center/account_book.git
数独小游戏       id=c0043e1e9d0240d19c23fa2556d5ecfc  repo=harmony-practice-center/Harmony-APP-sudoku.git
公交出行应用模板 id=c5f78fbd5a5e48d98857520b3c8f6565  repo=harmony-practice-center/HarmonyOS-APPTMPL-bustravel
计算器应用模板   id=ee08ba32e1f14631b37a4596f8011d29  repo=harmony-practice-center/HarmonyOS-APPTMPL-calculator
万年历应用模板   id=3314b7fde63748f686db00773b02034e  repo=harmony-practice-center/HarmonyOS-APPTMPL-Calendar
```

**Star 任务必须 star 案例关联的仓库，不是 org 下随便一个仓。**
旧代码从 `GET /api/v5/orgs/harmony-practice-center/repos` 里「挑星数最少的」
会命中 `org-desc`（组织介绍，id=11252840），完全无效。

### 4c. check 返回的真实语义（关键）

用案例仓触发 `POST /api/growth/tasks/gitcode-star/submit {"case_id":"<案例id>","payload":{"action":"check"}}`：

```json
{
  "completed": false,
  "period_key": "2026-11-06",
  "progress": {
    "git_login": "demo_user",
    "match": "any",
    "matched": [],
    "missing": ["org:harmony-practice-center"],
    "need_daily_star": true,
    "last_star_at": "2026-11-05T09:42:52.485+08:00",
    "today": "2026-11-06"
  }
}
```

**`last_star_at` 是 11-05，说明它看得到我们昨天的 star。**
`missing:["org:harmony-practice-center"]` 的真实含义是
**「今天在这个 org 下还没有新的 star」**，不是「从来没 star 过」。

配合 `need_daily_star: true` 和任务指南「每日需Star不同项目且不可取消Star」，
结论：**每天必须 star 一个不同仓库，且最好落在当天**。
同一天重复 star 同一个仓库无效；star 后取消也无效。

### 5. 成长中心「去完成」的真实跳转（拦截 window.open 抓到）

```
每日浏览一个鸿蒙案例 / 每日Star一个鸿蒙案例代码仓
  -> https://harmonybp.developer.huaweicloud.com/#practice-tasks
每日体验AI DevSpace
  -> https://hands-on-agent.developer.huaweicloud.com/#tasks
每日发布文章
  -> https://bbs.huaweicloud.com/blogs?utm_source=grow
每周通过一项微认证
  -> https://edu.huaweicloud.com/certification/micro/list?grow
在开发者作品展览馆中发布作品
  -> https://gallery.developer.huaweicloud.com/gallery/submit-guide
建议被采纳
  -> https://developer.huaweicloud.com/space/support/my-suggestion?source=grow
```

### 6. 鸿蒙前端的计时协议（从 CaseDetailView-CiBCbm7T.js 还原）

```js
const mr = 5e3;                              // 心跳间隔 5 秒
const wr = "growth_session:";
// sessionStorage key = wr + taskKey + ":" + caseId

// 开计时：
POST /api/growth/tasks/case-view-daily/sessions
  -> { session_id, need_seconds }

// 心跳（每 5 秒，且 document.hidden 时跳过）：
POST /api/growth/sessions/<session_id>/heartbeat
  -> { accumulated_ms, need_ms, completed, session_id, already_awarded_today, points, award_status }
  // 每次返回新 session_id，必须覆盖旧的（前端 S = E.session_id）

// 页面离开时用 sendBeacon 保底发一次：
navigator.sendBeacon(<heartbeat-url>, new Blob([JSON.stringify({session_id})]))

// 已完成条件：heartbeat 返回 completed=true
```

关键：**`document.hidden` 为 true 时不发心跳**。所以无头模式 / 后台标签页
不会累积时间——必须在真实可见的页面上跑 30 秒。

## 二、落地实现（已合并进代码）

新增 `go/internal/task/harmony_browser.go`：

- `RunHarmonyDwellBrowser` —— 打开真实案例详情页停留 40 秒（headed + Xvfb）
- `RunHarmonyStarBrowser` —— 遍历案例仓，跳过已 Starred，点未 Star 的那个，再触发 check
- `RunDevSpaceBrowser` —— 实践中心 → 「前往AI DevSpace实践」→ AI 开发者空间
- `listHarmonyCases` —— `/api/cases` 分页（`page_size` 会 422，只能 `page`，每页固定 6 条）
- `checkGitcodeStar` / `starButtonState` / `dismissGitcodeGuide` / `clickGitcodeStar` 辅助函数

`internal/web/server.go` 的 `runDaily` 四个步骤全部改为浏览器版；
新增 `runHarmonyStarBrowserWithRelogin`（GitCode 会话失效时自动重登）。

### 关键坑（都已修）

1. **GitCode 新手引导蒙层**：`.spotlight-guide` 是 `aria-modal=true` 的遮罩，
   不关掉时 Playwright 点击被拦（`subtree intercepts pointer events`）。
   关法：点 `.spotlight-guide-close`，兜底直接 `remove()`。
2. **Star 按钮选择器**：`(//div[contains(@class,'status-btn') and .//span[normalize-space(text())='Star']])[last()]`
   取可见的顶部工具条那个。点完按钮文案会变成 `Starred N`。
3. **每日必须 Star 不同仓库**：已 `Starred` 的仓库绝不能再点（那是取消 Star，
   会把任务做废）。必须先读按钮状态再决定。
4. **GitCode 会话有效期 24h**：`GITCODE_ACCESS_TOKEN` 是 `exp-iat=24h` 的 JWT。
   过期后页面仍能打开（公开数据），但 Star 会以匿名身份点击、不落账。
   修复：`hwcloud login-gitcode --account <user> --password <pass>` 自动重登。
5. **部署二进制**：`/opt/hwcloud-demo/hwcloud` 是 10-30 的旧版，
   systemd 实际跑的是 `/opt/hwcloud-demo/go/bin/hwcloud`。改代码后要
   `go build -o /opt/hwcloud-demo/go/bin/hwcloud ./cmd/hwcloud` 再
   `systemctl restart hwcloud-console`。
6. **`PublishBlog` 的 ExpectResponse 用法**：本版本 playwright-go 的
   第一个参数是 url 或 `func(string) bool`，之前传了 `func(Response) bool`，
   运行时 panic `invalid urlOrPredicate`。已改成 glob 字符串。

## 三、原方案记录（保留，供对照）

### A. 每日浏览一个鸿蒙案例（+10）〔已实现〕

浏览器打开 `https://harmonybp.developer.huaweicloud.com/#/cases/<caseId>`
（真实案例详情页，不是 `#practice-tasks`），等 35 秒。页面自己会开 session
+ 每 5 秒心跳，完成时 growth 自动记 done。

需要先拿到一个真实 caseId：
`GET https://harmonybp.developer.huaweicloud.com/api/cases?page_size=1`
（旧代码里已用到，在 `internal/task/lab_list.go` 附近或 harmony.go 的 dwell 部分）。

关键约束：**标签页必须可见**（`document.hidden=false`）。所以：
- 不能用 `headless: true`；
- 用 Xvfb `:99` 跑 headed Chromium；
- 页面要保持在前台（`page.BringToFront()`）。

### B. 每日Star一个鸿蒙案例代码仓（+30）〔已实现〕

必须走「案例详情页 → 前往 GitCode 代码仓 → 点 Star」链路。候选做法：

1. 打开案例详情页（同 A）；
2. 页面上找「前往代码仓 / GitCode」按钮，`Click` 它会新开 GitCode 仓库页；
3. 在新页面点 Star 按钮（而不是调 API）；
4. 回鸿蒙页面，等 `external_check` 自动核验（或手动触发 submit check）。

注意「每日需 Star 不同项目」——记录当天已 star 的仓库名，第二天换一个。

### C. 每日发布文章（+60）〔已实现〕

`https://bbs.huaweicloud.com/blogs/article`（已确认这是「写文章」的真实链接，
不是 `/blogs/write`）。需要在编辑器里填标题 + 正文 + 点发布。
编辑器可能是富文本/iframe，需先探测 DOM 结构。

### D. 每日体验AI DevSpace（+30）〔已实现〕

`https://hands-on-agent.developer.huaweicloud.com/#tasks`。
纯协议的 `/api/track/visit` + `/api/track/aishell_click` 虽然返回 success，
但成长中心不认。需要真实打开页面并做一次交互（点某个入口）。

### 定时器状态（2026-11-06 11:45 CST 关闭）

```
systemctl disable --now hwcloud-daily.timer    -> disabled, inactive
systemctl disable --now hwcloud-weekly.timer   -> disabled, inactive
```

恢复命令：`systemctl enable --now hwcloud-daily.timer hwcloud-weekly.timer`

面板「一键日常」按钮仍可手动触发（`POST /api/...` -> runDaily）。
明天改完浏览器方案后，需要同步替换 runDaily 里对应步骤的实现。
