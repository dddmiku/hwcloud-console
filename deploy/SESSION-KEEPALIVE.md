# 会话保活机制

华为云成长中心的会话有个隐蔽的短效瓶颈：`ECO_CN_ID`。

## 问题

登录后 `devdata.huaweicloud.com` 的所有接口都需要 `ECO_CN_ID`（`.huaweicloud.com`
域下的会话 cookie）。它的值形如：

```
TJ8HiSaWGtJ2ipmlvW7QCA.._-_1793300956_-_mcRRoxP8LAQrlOOSynKs
                        ^^^^^^^^^^ Unix 时间戳，内嵌的过期时刻
```

有效期只有 **1 小时**。过期后接口一律返回：

```
404 {"message":"no Route matched with those values"}
```

这个 404 很容易被误判成「路由不存在」或「cookie 全废了」，实际只是这一个 cookie 过期。

而真正长期的凭据其实都在：

| cookie | 域 | 有效期 |
|---|---|---|
| `HW_refts_id1_cloud_huawei_com_auth_huaweicloud_com` | auth.huaweicloud.com | **约 6 个月**（refresh token） |
| `hwid_cas_sid` | .id1.cloud.huawei.com | 约 1 年 |
| `HW_id_*` / `HW_idts_*` / `HW_idvc_*` | auth.huaweicloud.com | 约 1 年 |

所以「本机登录有效期太短」的感觉来自 `ECO_CN_ID`，不是账号真的掉了。

## 解法

`hwcloud keepalive` 会：

1. 检查当前会话。有效就直接返回（带 `--min-remain` 时按剩余时间判断是否强制刷新）。
2. 失效时先尝试**纯协议**重建：访问 `developer.huaweicloud.com/grow` 让 SSO 用
   refresh token 重新签发 `ECO_CN_ID`。
3. 纯协议不行则回退到**浏览器 profile**：拉起持久化 Chrome，注入上次导出的
   cookie，打开成长中心，等页面自己完成 SSO 续期，再导出新 cookie。

第 3 步在服务器上实测有效（约 12 秒完成），是当前可靠的兜底路径。

## 部署

服务器上已配置：

```
/etc/systemd/system/hwcloud-keepalive.timer   # 每 20 分钟
/etc/systemd/system/hwcloud-keepalive.service
/usr/local/bin/hwcloud-keepalive.sh
```

查看状态：

```bash
systemctl list-timers hwcloud*
tail -20 /opt/hwcloud-demo/logs/keepalive.log
hwcloud session
```

`hwcloud session` 现在会区分两类时间：

- `auth_remaining_days` —— 真正决定还能不能续期的 refresh token 剩余天数（正常是 170+ 天）
- `shortest_remaining_hours` —— 最短命的 cookie（通常是 `_fr_ssid`，几十分钟），过期不影响登录

## 失败时

如果某天 refresh token 也过期了（约半年一次），
`/opt/hwcloud-demo/state/NEED_MANUAL_LOGIN` 会出现，内容是时间戳加说明。
这时在本机执行：

```bash
hwcloud login      # 打开浏览器，用已保存的 profile 自动登录，通常不需要验证码
```

然后把 `state/cookies.json` 和 `state/meta.json` 同步到服务器的
`/opt/hwcloud-demo/state/` 即可。之后标记文件会在下一次 keepalive 成功时自动删除。

## 踩过的坑

- **`http.CookieJar.Cookies()` 会清空 Domain/Path**。直接把它返回的 cookie 写进
  JSON 文件会得到 `domain: ""` 的废条目，下次加载时全部失效。必须用查询时的 host
  反填 Domain。
- **无头浏览器的 UA 带 `HeadlessChrome`**，华为云 WAF 会直接返回 404，表现为
  「get-ainfo 拿不到 csrf」。所以 `LaunchPersistent` 必须显式设置
  `UserAgent: config.UserAgent()`（读 meta.json 里登录时记录的 UA）。
- **headless 模式不能把 `Channel` 置空**，否则会去找 `chrome-headless-shell`，
  而它默认不随 chromium 一起安装。
- **`_fr_ssid` / `SessionID` 不是登录凭据**。它们过期不影响登录，不要拿它们的
  过期时间判断会话状态——真正要看的是 `ECO_CN_ID` 和 refresh token。
