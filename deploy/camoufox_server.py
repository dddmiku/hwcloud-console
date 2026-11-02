#!/usr/bin/env python3
"""华为云登录：Camoufox + 易盾滑块自动过。

为什么这么设计：
  - 华为 SSO 的 fp 是 SDK 采集浏览器环境后加密生成的，每次会话都不同，
    纯协议复用旧值会被判「设备不安全」，所以必须用真实浏览器（Camoufox）。
  - 华为把验证委托给网易易盾（necaptcha.nosdn.127.net），滑块是拼图式：
      .yidun_bg-img  背景图 260x130（带缺口）
      .yidun_jigsaw  滑块图  49x130（小拼图块）
      .yidun_control 拖动条 260x40
  - 过法：把两张图交给打码服务算缺口 x，再用带加减速的真人轨迹拖动。

服务接口：
  GET  /health
  POST /login  {"phone","password","sms_code"}
       sms_code 为空 → 新开会话，提交账密，自动过滑块，让华为发短信
       sms_code 非空 → 复用会话填码完成登录
  POST /logout {"phone"}
"""

import asyncio
import base64
import io
import json
import os
import random
import re
import sys
import threading
import time
import traceback
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import urlparse

sys.stdout.reconfigure(encoding="utf-8", errors="replace")
sys.stderr.reconfigure(encoding="utf-8", errors="replace")

PORT = int(os.environ.get("HWLOGIN_PORT", "8901"))
WORK = Path(os.environ.get("HWLOGIN_WORK", "/opt/hwcam/sessions"))
SMS_TTL = float(os.environ.get("HWLOGIN_SMS_TTL", "480"))

# 打码服务（AntiCAP）
CAP_BASE = os.environ.get("HWLOGIN_CAP", "")
CAP_KEY = os.environ.get("HWLOGIN_CAP_KEY", "")
CAP_USER = os.environ.get("HWLOGIN_CAP_USER", "")
CAP_PASS = os.environ.get("HWLOGIN_CAP_PASS", "")

LOGIN_URL = (
    "https://auth.huaweicloud.com/authui/login.html"
    "?service=https%3A%2F%2Fdeveloper.huaweicloud.com%2Fgrow&locale=zh-cn"
)


def log(m):
    print(f"[{time.strftime('%H:%M:%S')}] {m}", flush=True)


# ---------- 打码服务 ----------

_cap_token = {"v": None, "at": 0.0}

# AntiCAP 的 urllib 默认 UA 会被 Cloudflare 拦（error 1010），必须伪装成浏览器。
UA_BROWSER = ("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
              "(KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36")


def cap_token():
    """取 AntiCAP 的 JWT。

    两层鉴权都要：nginx 用 X-Api-Key 白名单挡在最前面，
    AntiCAP 自己还要 JWT。少一层就是 404 / 403。
    """
    if _cap_token["v"] and time.time() - _cap_token["at"] < 3300:
        return _cap_token["v"]
    data = urllib.parse.urlencode({
        "username": CAP_USER, "password": CAP_PASS,
    }).encode()
    req = urllib.request.Request(
        CAP_BASE + "/api/login", data=data,
        headers={
            "Content-Type": "application/x-www-form-urlencoded",
            "X-Api-Key": CAP_KEY,
            "User-Agent": UA_BROWSER,
        })
    with urllib.request.urlopen(req, timeout=20) as r:
        obj = json.loads(r.read())
    tok = obj.get("access_token") or obj.get("token") or ""
    if not tok:
        raise RuntimeError(f"打码服务登录失败: {obj}")
    _cap_token["v"] = tok
    _cap_token["at"] = time.time()
    return tok


def cap_slider_match(bg_bytes, jigsaw_bytes):
    """把背景图和滑块图交给打码服务，返回缺口左上角 x 坐标。

    接口是 JSON（不是 multipart），字段 target_base64 / background_base64，
    注意 target 是滑块小图、background 是背景图，别传反。
    返回形如 {"result":{"target":[x1,y1,x2,y2]}}，取 x1。
    """
    bg64 = base64.b64encode(bg_bytes).decode()
    fg64 = base64.b64encode(jigsaw_bytes).decode()
    payload = json.dumps({
        "target_base64": fg64,
        "background_base64": bg64,
    }).encode()

    req = urllib.request.Request(
        CAP_BASE + "/api/slider/match", data=payload,
        headers={
            "Content-Type": "application/json",
            "X-Api-Key": CAP_KEY,
            "Authorization": "Bearer " + cap_token(),
            "User-Agent": UA_BROWSER,
        })
    with urllib.request.urlopen(req, timeout=40) as r:
        obj = json.loads(r.read())

    # {"result": {"target": [x1,y1,x2,y2]}}
    res = obj.get("result")
    if isinstance(res, dict):
        t = res.get("target")
        if isinstance(t, list) and t:
            return int(t[0])
        for k in ("x", "distance", "offset", "gap"):
            if isinstance(res.get(k), (int, float)):
                return int(res[k])
    # 兜底：顶层直接给数字
    for k in ("x", "distance", "offset", "gap"):
        if isinstance(obj.get(k), (int, float)):
            return int(obj[k])
    raise RuntimeError(f"打码返回无法解析: {obj}")


# ---------- 会话 ----------

class Session:
    def __init__(self, phone, password):
        # 清洗输入：用户从别处复制账密时经常带上首尾空格、
        # 不可见字符（NBSP U+00A0、零宽空格 U+200B、BOM U+FEFF），
        # 甚至换行。直接拿去填表单会「账号或密码错误」，
        # 但用户肉眼完全看不出来 —— 排查成本极高。
        #
        # 手机号：只留数字（去掉空格、横线、括号、+86 之类）
        # 密码：只去掉首尾空白和零宽字符，**中间的空格要保留**
        #       （密码里可能有合法的空格）
        self.phone = self._clean_phone(phone)
        self.password = self._clean_password(password)
        self.page = None
        self.browser = None
        self.cm = None
        self.created = time.time()
        self.sms_sent_at = 0.0
        self.sms_sent_ok = False
        self.trusted = False
        self.verified_free = False
        self.anon_id = ""
        self.sdk_info = {}
        # 短信相关网络请求记录。用来验证「获取验证码」到底有没有真发出去——
        # DOM 里同名元素有 8 个，点错一个页面毫无变化，只看点击成功是假证据。
        self.sms_requests = []
        self.stage = "new"
        self.cap = {}
        self.dir = WORK / re.sub(r"[^0-9A-Za-z_.-]", "_", phone)
        # 建会话目录。这行原来被误放进了 _clean_password 的 return 之后，
        # 成了永远执行不到的死代码 —— 结果 browser_profile 目录从来没被
        # 创建过，playwright 用持久化 context 时会自己建，但 clean-locks
        # 和备份脚本按目录找就会漏。
        self.dir.mkdir(parents=True, exist_ok=True)

    # 需要剥掉的不可见字符。除了常规空白，还有几个从网页/微信/PDF
    # 复制时经常混进来的：
    #     不换行空格 (NBSP)      —— 从网页表格复制最常见
    #   ​ 零宽空格               —— 从微信/QQ 复制常见
    #   ‌ ‍ 零宽连接符
    #   ﻿ BOM
    #   　 全角空格                —— 中文输入法下容易打出来
    _INVISIBLE = " ​‌‍﻿　  "

    @classmethod
    def _clean_phone(cls, s):
        """手机号只留数字。去掉空格、横线、括号、+86 前缀等。"""
        if not s:
            return ""
        s = str(s).translate({ord(c): None for c in cls._INVISIBLE})
        digits = "".join(ch for ch in s if ch.isdigit())
        # 去掉国际区号 86（13005999962 是 11 位，8613005999962 是 13 位）
        if len(digits) == 13 and digits.startswith("86"):
            digits = digits[2:]
        return digits

    @classmethod
    def _clean_password(cls, s):
        """密码只去掉首尾的空白和不可见字符，中间的原样保留。

        为什么不能全去掉空格：密码里可能有合法的空格，
        全删会把正确密码改错。
        """
        if s is None:
            return ""
        s = str(s).translate({ord(c): None for c in cls._INVISIBLE})
        return s.strip()

    @property
    def alive(self):
        return self.page is not None

    def sms_age(self):
        # 用 getattr 兜底：这个方法是全局 LOCK 持有期间被调的，
        # 一旦抛 AttributeError 会让整个登录请求 500，而调用方只看到
        # 「连接 Camoufox 服务失败」，完全看不出是属性缺失。
        # 实测 2026-11-02 22:20 就栽在这：SESSIONS 里混进了早期代码
        # 创建的 Session（那时 __init__ 还没有 sms_sent_at），
        # 四个账号的登录请求全部 500。
        ts = getattr(self, "sms_sent_at", 0.0)
        return time.time() - ts if ts else -1

    async def _shot(self, name):
        # 必须加硬超时。华为 CDN（portal.hc-cdn.com）的请求常年不结束，
        # 页面一直处于 loading 状态，Firefox 的 screenshot 会等页面稳定，
        # 实测卡死 5 分钟以上，把整个登录流程连同全局 LOCK 一起拖住。
        try:
            await asyncio.wait_for(
                self.page.screenshot(path=str(self.dir / f"{name}.png"),
                                     timeout=15000),
                timeout=20)
        except Exception:
            pass

    async def start(self):
        from camoufox.async_api import AsyncCamoufox

        # 用持久化 profile，让设备指纹跨次保持一致。
        #
        # 为什么必须持久化：
        #   华为的「信任设备」是绑定到「设备指纹 + hwid_cas_sid」这个组合的。
        #   Camoufox 默认每次启动都是全新 profile，fp 每次都变，
        #   上次 updateTrustBrowser 的成功结果对下一次完全无效——
        #   实测信任成功后重新登录仍然要滑块 + 短信。
        #   用固定 user_data_dir 后，localStorage / 指纹种子 / cookie 都留在磁盘上，
        #   下次启动还是同一台「设备」，信任才可能生效。
        profile_dir = str(self.dir / "browser_profile")
        self.cm = AsyncCamoufox(headless=False, os="windows", locale="zh-CN",
                                humanize=True, geoip=False,
                                persistent_context=True,
                                user_data_dir=profile_dir)
        self.browser = await self.cm.__aenter__()
        # 持久化模式下 __aenter__ 返回的是 BrowserContext（不是 Browser），
        # 两者都有 new_page()，直接调即可。
        self.page = await self.browser.new_page()
        self.page.set_default_timeout(60000)

        async def on_req(req):
            if req.method != "POST":
                return
            body = req.post_data or ""
            if not body:
                return
            # 短信相关请求单独记下来，用来确认「获取验证码」是否真发出
            u = req.url.lower()
            if any(k in u for k in ("smscode", "sendsms", "sendcode", "getcode",
                                    "verifycode")):
                self.sms_requests.append({"url": req.url[:150],
                                          "body": body[:200]})
                log(f"[{self.phone}] 抓到短信请求 {req.url[:80]}")
            # unionLoginByPwd 的请求体里有 anonymousLoginID，
            # 信任设备要用，先抓下来
            if "unionloginbypwd" in u:
                m = re.search(r"(?:^|&)anonymousLoginID=([^&]*)", body)
                if m and m.group(1):
                    self.anon_id = urllib.parse.unquote(m.group(1))
                    log(f"[{self.phone}] 抓到 anonymousLoginID={self.anon_id}")
            for key in ("fp", "hwmeta"):
                if key in self.cap:
                    continue
                m = re.search(r"(?:^|&)" + key + r"=([^&]+)", body)
                if m and len(m.group(1)) > 100:
                    self.cap[key] = m.group(1)
                    log(f"[{self.phone}] 抓到 {key} ({len(m.group(1))} 字符)")

        self.page.on("request", on_req)

        async def on_resp(resp):
            u = resp.url.lower()
            if any(k in u for k in ("smscode", "sendsms", "sendcode", "getcode",
                                    "verifycode")):
                try:
                    b = await resp.text()
                except Exception:
                    b = ""
                log(f"[{self.phone}] 短信响应 {resp.status}: {b[:200]}")
            # getSDKBaseInfo 里有 pageToken/pageTokenKey/state，
            # updateTrustBrowser 要用，先抓下来
            if "getsdkbaseinfo" in u:
                try:
                    b = await resp.text()
                    obj = json.loads(b)
                    for k in ("pageToken", "pageTokenKey", "state"):
                        if obj.get(k):
                            self.sdk_info[k] = obj[k]
                    log(f"[{self.phone}] 抓到 SDK 字段: "
                        f"{list(self.sdk_info.keys())}")
                except Exception:
                    pass

        self.page.on("response", on_resp)

        await self.page.goto(LOGIN_URL, wait_until="domcontentloaded",
                                 timeout=120000)
        await self.page.wait_for_timeout(2000)

        for sel in ['text=密码登录', 'a:has-text("密码登录")']:
            try:
                await self.page.click(sel, timeout=5000)
                break
            except Exception:
                continue
        await self.page.wait_for_timeout(1200)

        await self.page.fill('input[name="userAccount"]', self.phone, timeout=15000)
        await self.page.fill('input.hwid-input-pwd', self.password, timeout=15000)

        await self._click_by_text("登录")

        # 轮询等待验证框出现，而不是死等 6 秒。
        #
        # 为什么必须轮询：开了双重验证（MFA）的账号，弹窗出现得比
        # 普通账号晚。实测 13005999962 在点击后 6 秒时还没弹，
        # 6 秒后 _detect 返回 none -> 误判成「免验证」-> 直接 CAS 换票
        # -> 被弹回登录页，最终 stage=failed。
        #
        # 轮询到就立即返回，正常账号不会因此变慢。
        kind = "none"
        for _i in range(25):          # 最多 25 秒
            await self.page.wait_for_timeout(1000)
            kind = await self._detect()
            if kind != "none":
                log(f"[{self.phone}] 第 {_i+1}s 检测到验证类型: {kind}")
                break
            # 已经跳到业务页说明真的免验证登录成功
            if "developer.huaweicloud.com" in self.page.url and "authui" not in self.page.url:
                break

        await self._shot("01-submitted")
        log(f"[{self.phone}] 验证类型: {kind}")
        # 记下本次是否需要验证。免验证码直接登录成功 = 设备已被华为信任，
        # 这个信号比 updateTrustBrowser 的返回值更可靠：
        # 后者只在「走验证流程后主动标记」时才会被调用，
        # 而免验证登录根本不会走到那里，但信任其实早就生效了。
        self.verified_free = (kind == "none")

        if kind == "slider":
            ok = await self._solve_slider()
            if not ok:
                self.stage = "slider_failed"
                return self.stage
            await self.page.wait_for_timeout(4000)
            await self._shot("03-after-slider")
            kind = await self._detect()
            log(f"[{self.phone}] 滑块后类型: {kind}")

        if kind == "sms":
            # 验证弹窗有两种形态，取决于账号绑了哪些验证渠道：
            #   A. 账号绑了多个渠道（手机/邮箱/已登录设备）
            #      → 默认发到「已登录账号设备」，必须点「选择其他验证方式」
            #        再选手机号，才会真发短信
            #   B. 账号只绑了手机号
            #      → 弹窗直接显示「请输入 157******41 的验证码」
            #        + 「获取验证码」按钮，没有「选择其他验证方式」链接
            #        必须点「获取验证码」才会发
            # 两种情况都要处理，不能只做 A。
            switched = await self._pick_sms_channel()
            if not switched:
                log(f"[{self.phone}] 无「选择其他验证方式」，按单渠道账号处理")

            # 不管走哪条路，最后都要确保「获取验证码」被点过。
            # _pick_sms_channel 里点过一次，这里再兜一次：
            # 如果短信请求没发出去，说明按钮还没点或点错了。
            if not self.sms_requests:
                await self._click_get_code()

            if not self.sms_requests:
                self.stage = "sms_not_sent"
                log(f"[{self.phone}] 短信未发出（没观察到 getSMSCode 请求）")
                return self.stage

            self.stage = "need_sms"
            self.sms_sent_at = time.time()
            log(f"[{self.phone}] 短信已发送，有效期 {int(SMS_TTL)}s")
            return self.stage

        if kind == "badcred":
            # 账号或密码错误。必须在这里拦下：
            # 早先没有这个分支，会落到下面的 none，误判成「免验证登录成功」，
            # 然后去 CAS 换票被弹回，最终报「缺少 SSOTGC/CASTGC」，
            # 用户完全看不出是密码错了。
            self.stage = "bad_credentials"
            self.error = "账号或密码错误，请在面板里更正后重试"
            log(f"[{self.phone}] 账号或密码错误，终止登录")
            return self.stage

        if kind == "none":
            # 没弹任何验证框就直接进了登录流程 = 设备已被华为信任。
            # 这里必须把 trusted 置 true，否则面板永远显示「未信任」——
            # 之前就栽在这：免验证登录根本不经过 submit_sms，
            # 那边的 trust_device() 逻辑永远跑不到。
            self.trusted = True
            self.stage = "checking"
            log(f"[{self.phone}] 免验证登录，设备已信任")
            return self.stage

        self.stage = kind
        return self.stage

    async def _detect(self):
        """判断当前是什么验证。

        顺序很重要：短信优先。
        滑块通过后「请完成安全验证」的 DOM 可能还残留一会儿，
        先判 slider 会把已经过了的状态误判成还需要滑块。
        """
        probes = [
            # 双重验证（MFA）。华为的文案有好几种，实测 2026-11：
            #   弹窗标题「身份验证」
            #   正文「您已开启双重验证，为了您的账号安全，请进行身份验证。」
            #   正文「请输入 130******62 的验证码」
            # 早先只探测「需要验证您的身份」，开了 MFA 的账号匹配不到，
            # 被误判成 none（免验证），后续直接去 CAS 换票 -> 被弹回登录页。
            ("sms", "身份验证"),
            ("sms", "双重验证"),
            ("sms", "请输入"),
            ("sms", "验证码"),
            # 老文案，保留兼容
            ("sms", "需要验证您的身份"),
            ("slider", "请完成安全验证"),
            ("slider", "向右拖动滑块填充拼图"),
        ]
        for kind, txt in probes:
            try:
                if await self.page.locator(f"text={txt}").count() > 0:
                    return kind
            except Exception:
                pass

        # 账号/密码错误：页面会显示红字提示，**不会弹任何验证框**。
        # 这是最容易被误判成 none 的情况 —— 没弹窗 != 登录成功。
        # 实测文案：「账号或密码错误，建议尝试手机号登录」
        for txt in ["账号或密码错误", "帐号或密码错误", "密码错误", "账号不存在"]:
            try:
                if await self.page.locator(f"text={txt}").count() > 0:
                    return "badcred"
            except Exception:
                pass

        # 交叉验证：页面上有「获取验证码」按钮 = 一定在等短信。
        # 这个信号比文案可靠，因为按钮是功能性的，改版也大概率保留。
        try:
            if await self.page.locator("text=获取验证码").count() > 0:
                return "sms"
        except Exception:
            pass
        try:
            if await self.page.locator('input[placeholder*="验证码"]').count() > 0:
                return "sms"
        except Exception:
            pass

        # 看有没有登录成功的迹象
        if "developer.huaweicloud.com" in self.page.url:
            return "none"
        return "none"

    async def _click_by_text(self, text):
        """按可见文字用坐标点击（华为的「登录」是 div，选择器匹配不到）。"""
        try:
            opts = await self.page.evaluate("""(want) => {
                const out = [];
                document.querySelectorAll('button, a, div, span, input[type=submit]').forEach(el => {
                    const r = el.getBoundingClientRect();
                    const t = (el.innerText || el.value || '').trim();
                    if (r.width > 30 && r.height > 15 && t === want)
                        out.push({x: Math.round(r.x), y: Math.round(r.y),
                                  w: Math.round(r.width), h: Math.round(r.height)});
                });
                return out;
            }""", text)
            if not opts:
                return False
            o = opts[0]
            await self.page.mouse.click(o["x"] + o["w"] // 2, o["y"] + o["h"] // 2)
            log(f"[{self.phone}] 点击「{text}」@({o['x']},{o['y']})")
            return True
        except Exception as e:
            log(f"[{self.phone}] 点击「{text}」失败: {e}")
            return False

    async def _solve_slider(self):
        """自动过易盾拼图滑块。"""
        try:
            # 1. 等滑块渲染出来
            for _ in range(20):
                n = await self.page.locator(".yidun_bg-img").count()
                if n:
                    break
                await self.page.wait_for_timeout(500)
            else:
                log(f"[{self.phone}] 滑块没渲染出来")
                return False

            await self.page.wait_for_timeout(800)
            await self._shot("02-slider")

            # 2. 取两张图（直接下 CDN，比从 DOM 解 base64 稳）
            urls = await self.page.evaluate("""() => ({
                bg: (document.querySelector('.yidun_bg-img') || {}).src || '',
                fg: (document.querySelector('.yidun_jigsaw') || {}).src || '',
                bgW: (document.querySelector('.yidun_bg-img') || {}).naturalWidth || 0,
                bgH: (document.querySelector('.yidun_bg-img') || {}).naturalHeight || 0,
                bgCssW: (document.querySelector('.yidun_bg-img') || {}).clientWidth || 0,
                ctrl: (() => {
                    const el = document.querySelector('.yidun_control');
                    if (!el) return null;
                    const r = el.getBoundingClientRect();
                    return {x: Math.round(r.x), y: Math.round(r.y),
                            w: Math.round(r.width), h: Math.round(r.height)};
                })(),
            })""")
            log(f"[{self.phone}] 图片: bg={urls['bg'][:60]} fg={urls['fg'][:60]}")
            log(f"[{self.phone}] 尺寸: natural={urls['bgW']}x{urls['bgH']} css={urls['bgCssW']}")
            log(f"[{self.phone}] 拖动条: {urls['ctrl']}")

            if not urls["bg"] or not urls["fg"]:
                log(f"[{self.phone}] 拿不到滑块图片 URL")
                return False

            bg_bytes = self._fetch(urls["bg"])
            fg_bytes = self._fetch(urls["fg"])
            (self.dir / "slider-bg.png").write_bytes(bg_bytes)
            (self.dir / "slider-fg.png").write_bytes(fg_bytes)
            log(f"[{self.phone}] 图片已下载 bg={len(bg_bytes)}B fg={len(fg_bytes)}B")

            # 3. 定位缺口。
            #    自研算法（边缘密度 + alpha 形状模板）实测比打码服务准
            #    （同一张图打码 125 不过、自研 134 过），且快 5-6 秒。
            #    所以自研优先；只有自研失败才调打码兜底。
            gap_self = None
            gap_cap = None
            try:
                import slider_locate
                gx, gy, sc = slider_locate.find_gap(bg_bytes, fg_bytes)
                gap_self = gx
                log(f"[{self.phone}] 自研定位缺口 x={gx} y={gy} score={sc:.0f}")
            except Exception as e:
                log(f"[{self.phone}] 自研定位失败: {e}")

            if gap_self is None:
                try:
                    gap_cap = cap_slider_match(bg_bytes, fg_bytes)
                    log(f"[{self.phone}] 打码兜底 x={gap_cap}")
                except Exception as e:
                    log(f"[{self.phone}] 打码也失败: {e}")

            if gap_self is None and gap_cap is None:
                return False

            # 4. 换算成 CSS 像素。
            #    背景图原始 320x160，页面上显示 260x130（有缩放），
            #    拖动距离必须按同一个比例换算，否则会偏。
            natural = urls["bgW"] or 320
            cssw = urls["bgCssW"] or 260
            scale = cssw / natural if natural else 1.0

            # 5. 拖动。
            #    优先用自研结果（实测更准），打码值作为第二候选，
            #    再补几个邻近距离兜底。每次拖完看是否切到短信框。
            cands = []
            if gap_self is not None:
                cands.append(round(gap_self * scale, 1))
            if gap_cap is not None:
                cands.append(round(gap_cap * scale, 1))
            if cands:
                base = cands[0]
                cands += [round(base - 6, 1), round(base + 6, 1),
                          round(base - 12, 1), round(base + 12, 1)]
            # 去重保序
            seen = set()
            cands = [c for c in cands if not (c in seen or seen.add(c))]
            log(f"[{self.phone}] 缩放 {scale:.3f}（{natural}→{cssw}）候选位移 {cands}")

            ok = await self._try_drag_distances(cands)
            return ok
        except Exception as e:
            log(f"[{self.phone}] 滑块处理异常: {e}")
            traceback.print_exc()
            return False

    async def _try_drag_distances(self, candidates):
        """按候选位移依次试拖，返回是否通过。

        易盾失败后滑块会回弹到 left=0，所以可以连续试。
        判据见 _drag：滑块文案消失或短信框出现都算过。

        重试前要等回弹动画结束（约 600ms），否则下一次 pointerdown
        会落在还在移动的元素上，坐标算错。
        """
        for i, dist in enumerate(candidates):
            if dist <= 5:
                continue
            # 已经过了吗（短信框出现 / 滑块文案消失）
            try:
                state = await self.page.evaluate("""() => {
                    const txt = document.body.innerText || '';
                    return {slider: txt.includes('请完成安全验证'),
                            sms: txt.includes('需要验证您的身份')};
                }""")
            except Exception:
                state = {"slider": False, "sms": False}
            if state.get("sms") or not state.get("slider"):
                log(f"[{self.phone}] 滑块已通过（state={state}）")
                return True

            # 等回弹动画结束，再重新读一次滑块位置（回弹后 x 会变回起点）
            await self.page.wait_for_timeout(700)

            log(f"[{self.phone}] 第 {i+1} 次拖动 {dist:.1f}px")
            ok = await self._drag(None, dist)
            if ok:
                log(f"[{self.phone}] 位移 {dist:.1f}px 通过")
                return True
        log(f"[{self.phone}] 所有候选位移都没过")
        return False

    @staticmethod
    def _fetch(url):
        req = urllib.request.Request(url, headers={
            "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:156.0) "
                          "Gecko/20100101 Firefox/156.0",
            "Referer": "https://auth.huaweicloud.com/",
        })
        with urllib.request.urlopen(req, timeout=20) as r:
            return r.read()

    async def _drag(self, ctrl, distance):
        """用 JS 直接派发 PointerEvent 拖动滑块。

        为什么不用 page.mouse：
          实测 page.mouse.down() 落在 .yidun_slider 上只产生 2 条 pointerdown，
          滑块 style.left 完全不动——易盾没把 Playwright 合成的事件当成拖动开始。
          而用 JS 在滑块元素上 dispatchEvent，style.left 立刻跟着变（831→981），
          说明事件链是通的，只是入口不对。

        轨迹要真人化：易盾校验加速度分布，匀速直线会被判机器。
        用「ease-out + 微抖动 + 末端回拉」。
        """
        slider = await self.page.evaluate("""() => {
            const el = document.querySelector('.yidun_slider');
            if (!el) return null;
            const r = el.getBoundingClientRect();
            return {x: Math.round(r.x), y: Math.round(r.y),
                    w: Math.round(r.width), h: Math.round(r.height)};
        }""")
        if not slider:
            log(f"[{self.phone}] 找不到 .yidun_slider")
            return False

        x0 = slider["x"] + slider["w"] // 2
        y0 = slider["y"] + slider["h"] // 2

        # 生成轨迹点（Python 侧算，带随机抖动，避免 JS 里随机性太规律）
        steps = random.randint(30, 45)
        pts = []
        for i in range(1, steps + 1):
            p = i / steps
            ease = 1 - (1 - p) ** 3          # 先快后慢
            x = distance * ease
            jitter = random.uniform(-1.5, 1.5) if i < steps - 3 else 0
            pts.append([round(x0 + x, 1), round(y0 + jitter, 1)])
        # 末端回拉
        overshoot = distance + random.uniform(2, 6)
        pts.append([round(x0 + overshoot, 1), round(y0, 1)])
        pts.append([round(x0 + distance, 1), round(y0, 1)])

        # 关键：必须逐点派发并留真实时间间隔。
        # 一次性 for 循环派发 34 个 pointermove 只花几毫秒，
        # 易盾按时间戳算速度会得到「无限快」，直接判机器。
        # 真人拖 165px 约 300-800ms，这里按步数均摊。
        #
        # 注意用 await 等整个序列跑完再返回，不用 window 标志位——
        # 标志位方式下 Python 侧可能在轨迹还没跑完时就去做别的操作，
        # 把后续事件插进拖动中间，轨迹就乱了。
        ok = await self.page.evaluate("""async ([x0, y0, pts, totalMs]) => {
            const el = document.querySelector('.yidun_slider');
            if (!el) return {err: 'no slider'};
            const mk = (type, x, y) => new PointerEvent(type, {
                bubbles: true, cancelable: true, composed: true,
                pointerId: 1, pointerType: 'mouse', isPrimary: true,
                clientX: x, clientY: y, button: 0,
                buttons: type === 'pointerup' ? 0 : 1,
            });
            const fire = (type, x, y) => {
                el.dispatchEvent(mk(type, x, y));
                el.dispatchEvent(new MouseEvent(type.replace('pointer', 'mouse'), {
                    bubbles: true, cancelable: true, composed: true,
                    clientX: x, clientY: y, button: 0,
                    buttons: type === 'pointerup' ? 0 : 1,
                }));
            };
            const sleep = (ms) => new Promise(r => setTimeout(r, ms));

            fire('pointerdown', x0, y0);
            await sleep(120);
            const N = pts.length;
            for (let i = 0; i < N; i++) {
                const [x, y] = pts[i];
                await sleep(totalMs / N * (0.7 + Math.random() * 0.6));
                fire('pointermove', x, y);
            }
            await sleep(150);
            fire('pointerup', pts[N - 1][0], pts[N - 1][1]);
            return {ok: true, n: N, totalMs};
        }""", [x0, y0, pts, random.randint(320, 620)])

        log(f"[{self.phone}] 拖动派发 {ok} 距离={distance:.1f}px")

        # 判据：不能只看 .yidun_slider 是否消失。
        # 实测滑块通过后易盾元素还在，但「请完成安全验证」文案消失、
        # 「需要验证您的身份」出现——华为接管去提交 unionLoginByPwd 了。
        for _ in range(14):
            await self.page.wait_for_timeout(500)
            try:
                state = await self.page.evaluate("""() => {
                    const txt = document.body.innerText || '';
                    return {
                        slider: txt.includes('请完成安全验证'),
                        sms: txt.includes('需要验证您的身份'),
                        noSliderEl: !document.querySelector('.yidun_slider'),
                    };
                }""")
            except Exception:
                state = {"slider": False, "sms": False, "noSliderEl": True}

            if state["noSliderEl"] or state["sms"] or not state["slider"]:
                log(f"[{self.phone}] 滑块已通过（state={state}）")
                return True

        log(f"[{self.phone}] 本次位移未通过")
        return False

    async def _click_get_code(self):
        """点「获取验证码」发短信。

        两种弹窗形态都有这个按钮，所以抽成独立方法，主流程和切通道后都能调。

        坑：DOM 里「获取验证码」会命中 8 个嵌套元素，
        外层是整行容器（461x48），点它的中心等于点在输入框上，
        不会有任何反应。真正的链接是最小的那个（78x17）。
        所以按面积升序取最小的可点元素。
        """
        # 先试 Playwright locator（force=True 绕过遮挡检查）
        for sel in ['text=获取验证码', 'a:has-text("获取验证码")',
                    'span:has-text("获取验证码")',
                    'button:has-text("获取验证码")']:
            try:
                loc = self.page.locator(sel).first
                if await loc.count() == 0:
                    continue
                await loc.click(timeout=3000, force=True)
                log(f"[{self.phone}] 点击「获取验证码」成功（{sel}）")
                return True
            except Exception as e:
                log(f"[{self.phone}] 获取验证码 {sel}: {type(e).__name__}")
                continue

        # 坐标兜底：按面积取最小的那个
        try:
            pos = await self.page.evaluate("""() => {
                const hits = [];
                document.querySelectorAll('a, span, div, button').forEach(el => {
                    const t = (el.innerText || '').trim();
                    if (t === '获取验证码' || t === '重新获取') {
                        const r = el.getBoundingClientRect();
                        if (r.width > 20 && r.height > 10)
                            hits.push({t, x: Math.round(r.x), y: Math.round(r.y),
                                       w: Math.round(r.width), h: Math.round(r.height),
                                       area: Math.round(r.width * r.height)});
                    }
                });
                hits.sort((a, b) => a.area - b.area);
                return hits;
            }""")
            if pos:
                p0 = pos[0]
                await self.page.mouse.click(p0["x"] + p0["w"] // 2,
                                            p0["y"] + p0["h"] // 2)
                log(f"[{self.phone}] 坐标点击「{p0['t']}」"
                    f"@({p0['x']},{p0['y']}) {p0['w']}x{p0['h']}")
                return True
            log(f"[{self.phone}] DOM 里找不到「获取验证码」")
        except Exception as e:
            log(f"[{self.phone}] 坐标点击失败: {e}")
        return False

    async def _pick_sms_channel(self):
        """切到「使用 <手机号>」这个短信通道。

        华为默认把验证码「推送到已登录账号的设备」（authCodeSentList 里
        accountType=-1 那项 sent=1），手机号那项 sent=0——也就是没发短信。
        必须点「选择其他验证方式」展开渠道列表，再选手机号那一项。
        """
        try:
            # 1. 点「选择其他验证方式」。
            #    用 Playwright 的 locator 点击（自动算元素中心 + 滚动到可见），
            #    不要用自己扫 DOM 算坐标——之前扫到的是页面别处的同名文本，
            #    点到了弹窗外面的空白。
            clicked = False
            for sel in ['text=选择其他验证方式',
                        'a:has-text("选择其他验证方式")',
                        'span:has-text("选择其他验证方式")',
                        ':has-text("选择其他验证方式")']:
                try:
                    loc = self.page.locator(sel).first
                    await loc.scroll_into_view_if_needed(timeout=3000)
                    await loc.click(timeout=5000)
                    clicked = True
                    log(f"[{self.phone}] 点击「选择其他验证方式」成功（{sel}）")
                    break
                except Exception as e:
                    log(f"[{self.phone}] {sel} 失败: {type(e).__name__}")
                    continue
            if not clicked:
                return False

            # 2. 等面板展开。华为这个面板是异步渲染的，
            #    点完立刻查会查不到选项。
            #    注意：页面底部有「我们使用cookie…」这类文案也含「使用」，
            #    必须找「使用 <号码/邮箱>」这种形态。
            prefix = self.phone[:3] if len(self.phone) >= 3 else ""
            tail = self.phone[-2:] if len(self.phone) >= 2 else ""

            def is_channel_option(t):
                """「使用 xxx」且 xxx 是账号标识（手机号带星号 或 邮箱）。"""
                if not t.startswith("使用"):
                    return False
                if len(t) > 40:          # 长句是页面文案，不是选项
                    return False
                if "cookie" in t.lower():
                    return False
                body = t[2:].strip()
                return ("*" in body) or ("@" in body)

            opts = []
            for attempt in range(10):
                await self.page.wait_for_timeout(600)
                try:
                    opts = await self.page.evaluate("""() => {
                        const out = [];
                        document.querySelectorAll('div, li, span, label').forEach(el => {
                            const r = el.getBoundingClientRect();
                            const t = (el.innerText || '').trim();
                            if (r.width > 40 && r.height > 12 && t && t.length < 60)
                                out.push({txt: t, x: Math.round(r.x), y: Math.round(r.y),
                                          w: Math.round(r.width), h: Math.round(r.height)});
                        });
                        return out;
                    }""")
                except Exception:
                    opts = []
                if any(is_channel_option(o["txt"]) for o in opts):
                    log(f"[{self.phone}] 渠道面板已展开（第 {attempt+1} 次探测）")
                    break
            await self._shot("04-verify-methods")

            # 3. 找手机号项：文字形如「使用 157******41」。
            #    华为会打码成 157******41，中间是星号，
            #    所以用前缀 + 后缀匹配，不能要求后四位连续出现。
            cand = None
            for o in opts:
                t = o["txt"]
                if not is_channel_option(t):
                    continue
                if "@" in t:
                    continue
                if prefix and prefix in t and tail in t:
                    cand = o
                    break
            if cand is None:
                for o in opts:
                    t = o["txt"]
                    if is_channel_option(t) and "@" not in t and prefix and prefix in t:
                        cand = o
                        break
            if cand is None:
                uses = [o["txt"] for o in opts if is_channel_option(o["txt"])]
                log(f"[{self.phone}] 没找到手机号通道项，真实候选={uses[:6]}")
                return False

            log(f"[{self.phone}] 选短信通道: {cand['txt']!r} @({cand['x']},{cand['y']})")
            await self.page.mouse.click(cand["x"] + cand["w"] // 2,
                                        cand["y"] + cand["h"] // 2)
            await self.page.wait_for_timeout(3000)
            await self._shot("05-sms-channel")

            # 4. 切到手机号通道后，点「获取验证码」发短信。
            await self._click_get_code()

            # 5. 验证短信是否真的发出去了。
            #    只看「点到了按钮」不够——DOM 里同名元素有 8 个，
            #    点错一个等于没点，页面毫无变化。必须看网络请求。
            self.sms_sent_ok = False
            for _ in range(12):
                await self.page.wait_for_timeout(500)
                if self.sms_requests:
                    self.sms_sent_ok = True
                    break
                # 按钮变「重新获取」或出现倒计时也算发出
                try:
                    st = await self.page.evaluate("""() => {
                        const t = document.body.innerText || '';
                        return {countdown: t.includes('重新获取') || /\\d+s/.test(t)};
                    }""")
                    if st.get("countdown"):
                        self.sms_sent_ok = True
                        break
                except Exception:
                    pass

            if self.sms_sent_ok:
                log(f"[{self.phone}] 短信请求已发出"
                    f"（{len(self.sms_requests)} 条网络记录）")
            else:
                log(f"[{self.phone}] 警告：没观察到短信请求，可能被限流")

            await self.page.wait_for_timeout(1500)
            await self._shot("06-sms-requested")
            return True
        except Exception as e:
            log(f"[{self.phone}] 切短信通道异常: {e}")
            return False

    async def submit_sms(self, code):
        """填入短信验证码并提交。

        弹窗有两种形态：
          - 6 个单字符框（input[maxlength=1]）
          - 单个「短信验证码」输入框（切到手机号通道后是这种）

        坑：页面上有**两个** placeholder=「短信验证码」的 input，
        第一个是隐藏的（0x0，用于内部逻辑），第二个才是可见的。
        用 locator(...).first 会命中隐藏那个，fill 必然失败。
        所以必须按可见性（宽高 > 0）过滤。
        """
        boxes = self.page.locator('input[maxlength="1"]')
        n = await boxes.count()
        log(f"[{self.phone}] 验证码输入框 {n} 个，填入 {code}")
        if n >= len(code):
            for i, ch in enumerate(code):
                await boxes.nth(i).fill(ch)
        else:
            # 单框：在可见的 input 里找 placeholder 含「验证码」的那个
            filled = False
            try:
                handle = await self.page.evaluate_handle("""(code) => {
                    const cands = [];
                    document.querySelectorAll('input').forEach(el => {
                        const r = el.getBoundingClientRect();
                        const ph = el.placeholder || '';
                        if (r.width > 20 && r.height > 10 &&
                            (ph.includes('验证码') || ph.includes('短信'))) {
                            cands.push(el);
                        }
                    });
                    if (!cands.length) return null;
                    const el = cands[0];
                    // 用原生 setter 触发框架的 input 事件（Angular 需要）
                    const setter = Object.getOwnPropertyDescriptor(
                        window.HTMLInputElement.prototype, 'value').set;
                    setter.call(el, code);
                    el.dispatchEvent(new Event('input', {bubbles: true}));
                    el.dispatchEvent(new Event('change', {bubbles: true}));
                    el.focus();
                    return {ok: true, ph: el.placeholder,
                            w: Math.round(el.getBoundingClientRect().width)};
                }""", code)
                info = await handle.json_value()
                if info:
                    filled = True
                    log(f"[{self.phone}] 已填入可见输入框 {info}")
            except Exception as e:
                log(f"[{self.phone}] JS 填值失败: {e}")

            if not filled:
                # Playwright 兜底：用 :visible 过滤
                for sel in ['input[placeholder*="验证码"]:visible',
                            'input[placeholder*="短信"]:visible']:
                    try:
                        loc = self.page.locator(sel).first
                        if await loc.count() == 0:
                            continue
                        await loc.fill(code, timeout=8000)
                        filled = True
                        log(f"[{self.phone}] 用 {sel} 填入验证码")
                        break
                    except Exception:
                        continue
            if not filled:
                log(f"[{self.phone}] 找不到可见的验证码输入框")
                return {"ok": False, "stage": "no_input",
                        "error": "找不到验证码输入框"}
        await self._shot("07-sms-filled")

        clicked = False
        # 点「验证」按钮。
        # 坑和「获取验证码」一样：DOM 里有多个同名元素（外层容器 + 内层文字），
        # Playwright 的 text= 经常超时。用 JS 找可见的最小元素直接 click()。
        try:
            r = await self.page.evaluate("""() => {
                const cands = [];
                document.querySelectorAll('button, a, div, span, input[type=button]').forEach(el => {
                    const t = (el.innerText || el.value || '').trim();
                    if (t === '验证' || t === '确定' || t === '提交') {
                        const rc = el.getBoundingClientRect();
                        if (rc.width > 30 && rc.height > 15)
                            cands.push({el, area: rc.width * rc.height,
                                        x: Math.round(rc.x), y: Math.round(rc.y),
                                        w: Math.round(rc.width), h: Math.round(rc.height),
                                        t});
                    }
                });
                if (!cands.length) return null;
                cands.sort((a, b) => a.area - b.area);
                const c0 = cands[0];
                c0.el.click();
                return {t: c0.t, x: c0.x, y: c0.y, w: c0.w, h: c0.h,
                        cls: (c0.el.className || '').toString().slice(0, 40)};
            }""")
            if r:
                clicked = True
                log(f"[{self.phone}] JS 点击「{r['t']}」@({r['x']},{r['y']}) "
                    f"{r['w']}x{r['h']} cls={r['cls']}")
        except Exception as e:
            log(f"[{self.phone}] JS 点击验证失败: {e}")

        if not clicked:
            for sel in ['button:has-text("验证")', 'text=验证']:
                try:
                    loc = self.page.locator(sel).first
                    await loc.click(timeout=4000, force=True)
                    clicked = True
                    break
                except Exception:
                    continue
        if not clicked:
            # 坐标兜底
            try:
                pos = await self.page.evaluate("""() => {
                    const hits = [];
                    document.querySelectorAll('button, a, div, span').forEach(el => {
                        const t = (el.innerText || '').trim();
                        if (t === '验证') {
                            const rc = el.getBoundingClientRect();
                            if (rc.width > 30 && rc.height > 15)
                                hits.push({x: Math.round(rc.x), y: Math.round(rc.y),
                                           w: Math.round(rc.width), h: Math.round(rc.height),
                                           area: Math.round(rc.width * rc.height)});
                        }
                    });
                    hits.sort((a, b) => a.area - b.area);
                    return hits;
                }""")
                if pos:
                    p0 = pos[0]
                    await self.page.mouse.click(p0["x"] + p0["w"] // 2,
                                                p0["y"] + p0["h"] // 2)
                    clicked = True
                    log(f"[{self.phone}] 坐标点击「验证」@({p0['x']},{p0['y']})")
            except Exception:
                pass
        if not clicked:
            log(f"[{self.phone}] 找不到「验证」按钮")
        await self.page.wait_for_timeout(6000)
        await self._shot("08-after-sms")

        try:
            if await self.page.locator("text=验证码错误").count() > 0 or \
               await self.page.locator("text=验证码已失效").count() > 0:
                self.stage = "need_sms"
                return {"ok": False, "stage": "sms_wrong",
                        "error": "验证码错误或已失效"}
        except Exception:
            pass

        # 验证通过后立刻把设备标记为可信，这样下次登录不用再过验证码。
        # 必须在 finish() 之前——finish 会导航到别的域名，
        # 届时 id1.cloud.huawei.com 的会话上下文就不好用了。
        #
        # 免验证登录（verified_free）说明设备本来就被信任了，
        # 这种情况不需要再调 updateTrustBrowser，直接认定已信任。
        if self.verified_free:
            self.trusted = True
            log(f"[{self.phone}] 免验证登录，设备已信任")
        else:
            self.trusted = await self.trust_device()

        return await self.finish()

    async def trust_device(self):
        """把当前设备标记为「可信浏览器」，以后登录免验证码。

        为什么要用浏览器里发请求：
          updateTrustBrowser 需要 hwid_cas_sid 和 anonymousLoginID，
          这两个值都在浏览器的 JS 上下文和 cookie 里。用浏览器自己的
          fetch 发，能自动带上正确的 cookie 和指纹。

        接口（从 SDK 逆向）：
          POST /UnifiedIDMPortal/ajaxHandler/login/updateTrustBrowser
          operType=1&trustBrowser=1&hwid_cas_sid=...&anonymousLoginID=...
        成功返回 {"isSuccess":1,"localStorageID":"..."}
        """
        try:
            # hwid_cas_sid 是 httpOnly，document.cookie 读不到，
            # 必须用 Playwright 的 context.cookies() 拿。
            sid = ""
            try:
                all_cookies = await self.page.context.cookies()
                for ck in all_cookies:
                    if ck.get("name") == "hwid_cas_sid" and ck.get("value"):
                        sid = ck["value"]
                        break
            except Exception as e:
                log(f"[{self.phone}] 读 cookie 失败: {e}")

            # anonymousLoginID：登录过程中页面的隐藏字段 hwmeta 附近可能有，
            # 也可能在 localStorage。先都试一遍。
            anon = self.anon_id
            if not anon:
                anon = await self.page.evaluate("""() => {
                // 1. 隐藏 input
                const inp = document.querySelector('input[name="anonymousLoginID"]');
                if (inp && inp.value) return inp.value;
                // 2. localStorage
                for (const k of Object.keys(localStorage)) {
                    const v = localStorage.getItem(k) || '';
                    const kl = k.toLowerCase();
                    if (v && v.length < 80 && /^[a-z0-9*_.-]+$/i.test(v) &&
                        (kl.includes('anonymous') || kl.includes('loginid'))) {
                        return v;
                    }
                }
                // 3. sessionStorage
                for (const k of Object.keys(sessionStorage)) {
                    const v = sessionStorage.getItem(k) || '';
                    const kl = k.toLowerCase();
                    if (v && v.length < 80 && /^[a-z0-9*_.-]+$/i.test(v) &&
                        (kl.includes('anonymous') || kl.includes('loginid'))) {
                        return v;
                    }
                }
                    return '';
                }""")
            log(f"[{self.phone}] 信任设备: sid={sid[:24] if sid else 'EMPTY'}... anon={anon!r}")

            if not sid:
                log(f"[{self.phone}] 拿不到 hwid_cas_sid，跳过信任")
                return False

            # 用 Playwright 的 context.request 发请求。
            # 它共享浏览器的 cookie jar，不受 CORS 限制，
            # 比页面内 fetch 可靠（页面可能不在 id1 域下）。
            #
            # 字段必须完整：updateTrustBrowser 走的是 CAS 的通用校验，
            # 少 pageToken/pageTokenKey/state 会直接回
            # {"errorCode":"10000004","errorDesc":"Illegal Operation"}。
            # 这些值从页面的 getSDKBaseInfo 拿。
            sdk = dict(self.sdk_info)
            if not sdk:
                sdk = await self.page.evaluate("""() => {
                    const pick = (names) => {
                        for (const n of names) {
                            const el = document.querySelector(`input[name="${n}"]`);
                            if (el && el.value) return el.value;
                        }
                        return '';
                    };
                    return {
                        pageToken: pick(['pageToken']),
                        pageTokenKey: pick(['pageTokenKey']),
                        state: pick(['state']),
                        localStorageID: pick(['localStorageID']),
                    };
                }""")
            log(f"[{self.phone}] SDK 字段: { {k: (v[:16] + '...' if len(v) > 16 else v) for k, v in sdk.items()} }")

            form = {
                "pageToken": sdk.get("pageToken", ""),
                "pageTokenKey": sdk.get("pageTokenKey", ""),
                "reqClientType": "88",
                "loginChannel": "88000000",
                "clientID": "103493351",
                "lang": "zh-cn",
                "languageCode": "zh-cn",
                "state": sdk.get("state", ""),
                "localStorageID": sdk.get("localStorageID", ""),
                "userAccount": self.phone,
                "operType": "1",
                "trustBrowser": "1",
                "anonymousLoginID": anon or "",
                "hwid_cas_sid": sid,
            }
            url = ("https://id1.cloud.huawei.com/UnifiedIDMPortal/ajaxHandler/"
                   "login/updateTrustBrowser")
            try:
                resp = await self.page.context.request.post(
                    url, form=form,
                    headers={
                        "Accept": "application/json, text/plain, */*",
                        "Referer": "https://auth.huaweicloud.com/authui/login.html",
                        "Origin": "https://auth.huaweicloud.com",
                    },
                    timeout=30000)
                body = await resp.text()
                log(f"[{self.phone}] updateTrustBrowser {resp.status}: {body[:250]}")
                if '"isSuccess":1' in body.replace(" ", ""):
                    log(f"[{self.phone}] 设备信任成功")
                    return True
                return False
            except Exception as e:
                log(f"[{self.phone}] updateTrustBrowser 请求失败: {e}")
                return False
        except Exception as e:
            log(f"[{self.phone}] 信任设备异常: {e}")
            return False

    async def finish(self):
        """登录成功后走完 SSO 换票，导出 cookie。

        为什么不能只等跳转：
          短信验证通过后，华为只是把 CASTGC 种到 id1.cloud.huawei.com，
          页面停在原地不会自动跳。SSOTGC（devdata 会话的钥匙）和
          HW_refts_*（refresh token）都要靠 CAS 换票才会被签发。

        换票链路（实测）：
          1. 先回 authui/login.html?service=<grow>，CAS 会用 CASTGC 换出 SSOTGC
          2. 再访问 grow，SSO 用 SSOTGC 种下站点会话
          3. 最后走一遍 devdata islogin，确保 devdata 会话也建立
        """
        await self._shot("08-after-sms")

        # 1. CAS 换票：authui 的 login 页带 service 参数，
        #    已登录状态下 CAS 会直接签 SSOTGC 并 302 到 service。
        cas_url = ("https://auth.huaweicloud.com/authui/login"
                   "?service=https%3A%2F%2Fdeveloper.huaweicloud.com%2Fgrow")
        try:
            await self.page.goto(cas_url, wait_until="domcontentloaded", timeout=60000)
            await self.page.wait_for_timeout(4000)
            log(f"[{self.phone}] CAS 换票后 URL: {self.page.url[:120]}")
        except Exception as e:
            log(f"[{self.phone}] CAS 换票失败: {e}")

        # 2. 访问 grow，让 SSO 种站点会话
        try:
            await self.page.goto("https://developer.huaweicloud.com/grow",
                                 wait_until="domcontentloaded", timeout=60000)
            await self.page.wait_for_timeout(4000)
            # 看页面是否还是「登录/注册」——那样说明没换成功
            try:
                logged = await self.page.evaluate("""() => {
                    const t = document.body.innerText || '';
                    // 已登录会显示「我的积分」数值或用户名，未登录有「登录/注册」按钮
                    return {hasLoginBtn: t.includes('登录/注册'),
                            hasPoints: /我的积分\\s*\\n?\\s*\\d+/.test(t),
                            url: location.href.slice(0, 100)};
                }""")
                log(f"[{self.phone}] grow 状态: {logged}")
            except Exception:
                pass
        except Exception as e:
            log(f"[{self.phone}] 访问 grow 失败: {e}")

        # 3. devdata islogin
        try:
            await self.page.goto("https://devdata.huaweicloud.com/index/islogin",
                                 wait_until="domcontentloaded", timeout=60000)
            await self.page.wait_for_timeout(3000)
            log(f"[{self.phone}] devdata islogin 完成")
        except Exception as e:
            log(f"[{self.phone}] devdata islogin 失败: {e}")

        # 4. 回 grow 落定（SSOTGC 挂在 /authui/ 路径下）
        try:
            await self.page.goto("https://developer.huaweicloud.com/grow",
                                 wait_until="domcontentloaded", timeout=60000)
            await self.page.wait_for_timeout(3000)
        except Exception:
            pass

        await self._shot("09-final")
        cookies = await self.page.context.cookies()
        (self.dir / "cookies.json").write_text(
            json.dumps(cookies, ensure_ascii=False, indent=2), encoding="utf-8")

        ua = await self.page.evaluate("navigator.userAgent")
        meta = {
            "user_agent": ua,
            "login_at": time.strftime("%Y-%m-%d %H:%M:%S"),
            "login_via": "camoufox",
            "final_url": self.page.url,
        }
        (self.dir / "meta.json").write_text(
            json.dumps(meta, ensure_ascii=False, indent=2), encoding="utf-8")
        for k, v in self.cap.items():
            (self.dir / f"{k}.txt").write_text(v, encoding="utf-8")

        names = {c["name"] for c in cookies}
        self.stage = "done" if ("SSOTGC" in names or "CASTGC" in names) else "failed"
        log(f"[{self.phone}] 完成 stage={self.stage} cookies={len(cookies)} "
            f"SSOTGC={'SSOTGC' in names} CASTGC={'CASTGC' in names}")
        return {
            "ok": self.stage == "done",
            "stage": self.stage,
            "trusted": self.trusted,
            "final_url": self.page.url,
            "cookies": cookies,
            "meta": meta,
            "fp": self.cap.get("fp", ""),
            "hwmeta": self.cap.get("hwmeta", ""),
        }

    async def close(self):
        """关闭浏览器，确保 profile 正确落盘。

        为什么不能只调 __aexit__ 就完事：
          持久化 profile 的 cookie/localStorage 存在 SQLite 里，
          Firefox 需要在退出时 flush。如果进程被强杀（systemctl restart），
          WAL 没合并，下次读到的可能是旧状态甚至损坏的状态——
          实测这会让「信任设备」失效，重新要求滑块+短信。
          所以这里先正常关页面，再 __aexit__，最后等一会儿让 SQLite 落盘。
        """
        try:
            if self.page is not None:
                try:
                    await self.page.close()
                except Exception:
                    pass
            if self.cm:
                await self.cm.__aexit__(None, None, None)
        except Exception:
            pass
        # 给 Firefox 一点时间把 SQLite WAL 合并回主库
        await asyncio.sleep(1.5)
        self.cm = self.browser = self.page = None


SESSIONS = {}
LOCK = asyncio.Lock()


async def drop_session(phone):
    s = SESSIONS.pop(phone, None)
    if s:
        await s.close()
        log(f"[{phone}] 会话已丢弃")


async def do_login(phone, password, sms_code):
    # LOCK 必须带超时获取。早先是 `async with LOCK` 无限等：
    # 上一个登录卡死时，后续所有请求会在 LOCK 上排队挂到 HTTP 超时，
    # 面板表现为「整个服务没反应」。
    try:
        await asyncio.wait_for(LOCK.acquire(), timeout=8)
    except asyncio.TimeoutError:
        return {"ok": False, "stage": "busy",
                "error": "上一个登录还在进行（可能卡住了），请点「重置会话」后重试"}
    try:
        s = SESSIONS.get(phone)

        # 旧会话可能来自更早版本的代码（缺属性），也可能是关闭过程中
        # 半初始化的对象。这种会话留着只会让后续每一步都抛异常，
        # 不如直接丢掉重开 —— 重开的成本是一次浏览器启动，
        # 而带着坏会话跑的成本是用户反复看到「连接服务失败」。
        if s is not None and not hasattr(s, "sms_sent_at"):
            log(f"[{phone}] 丢弃残缺会话（缺 sms_sent_at，可能是旧版本残留）")
            await drop_session(phone)
            s = None

        if sms_code:
            if not s or not s.alive or s.stage != "need_sms":
                await drop_session(phone)
                return {"ok": False, "stage": "no_session",
                        "error": "会话已失效，请重新点登录获取新验证码"}
            age = s.sms_age()
            if age > SMS_TTL:
                await drop_session(phone)
                return {"ok": False, "stage": "sms_expired",
                        "error": f"验证码已过期（{int(age)}s 前发送），请重新点登录"}
            log(f"[{phone}] 复用会话填码（短信发出 {int(age)}s）")
            res = await s.submit_sms(sms_code)
            if res.get("stage") in ("done", "failed"):
                await drop_session(phone)
            return res

        # 不带验证码 → 一律重开会话，让华为重新发短信
        if s:
            log(f"[{phone}] 丢弃旧会话（短信发出 {int(s.sms_age())}s）后重开")
            await drop_session(phone)

        s = Session(phone, password)
        SESSIONS[phone] = s
        log(f"[{phone}] 新建会话")
        # start() 内部有多个 goto/evaluate，任何一个都可能被 CDN 拖住。
        # 4 分钟是实测正常流程（~40s）的 6 倍余量。
        try:
            stage = await asyncio.wait_for(s.start(), timeout=240)
        except asyncio.TimeoutError:
            log(f"[{phone}] start() 超时 240s，清理会话")
            await drop_session(phone)
            return {"ok": False, "stage": "timeout",
                    "error": "登录超时（页面加载卡住），已清理会话，请重试"}

        if stage == "need_sms":
            return {"ok": True, "stage": "need_sms", "phone": phone,
                    "sms_sent_at": int(s.sms_sent_at), "ttl": int(SMS_TTL)}
        if stage == "slider_failed":
            await drop_session(phone)
            return {"ok": False, "stage": "slider_failed",
                    "error": "滑块验证未通过，请重试"}
        if stage in ("slider", "sms_unknown"):
            return {"ok": False, "stage": stage, "error": "需要人工处理验证"}

        try:
            res = await asyncio.wait_for(s.finish(), timeout=180)
        except asyncio.TimeoutError:
            log(f"[{phone}] finish() 超时 180s，清理会话")
            await drop_session(phone)
            return {"ok": False, "stage": "timeout",
                    "error": "登录收尾超时（华为页面加载卡住），已清理会话，请重试"}
        await drop_session(phone)
        return res
    finally:
        # 无论成功、失败、超时还是异常，LOCK 必须释放，
        # 否则后续所有登录请求都会卡在 acquire 上。
        if LOCK.locked():
            LOCK.release()


# 会话最长存活时间。超过就自动关闭浏览器。
#
# 为什么必须自动回收：
#   need_sms 状态的会话要等用户填验证码，用户可能关掉弹窗、关掉标签页、
#   或者干脆走开了。浏览器不会自己退出，一直占着 1-2 GB 内存和全局 LOCK。
#   实测两个这样的僵尸会话挂 40 分钟，后续所有账号的登录全部排队超时，
#   报「连接 Camoufox 服务失败」，没人知道是「验证码没人填」导致的。
#
# 取 SMS_TTL + 2 分钟：短信有效期 480s，再加点余量给用户输码。
# 最短 10 分钟，避免 SMS_TTL 被调小时回收得太激进。
SESSION_MAX_AGE = max(float(os.environ.get("HWLOGIN_SESSION_MAX", "0")) or 0,
                      SMS_TTL + 120, 600)


async def _reaper():
    """后台线程：定期关掉超龄会话。"""
    while True:
        await asyncio.sleep(60)
        now = time.time()
        for ph in list(SESSIONS.keys()):
            s = SESSIONS.get(ph)
            if not s:
                continue
            age = now - getattr(s, "created", now)
            # 正在跑登录流程的不动（stage 还在推进）
            if s.stage in ("new", "checking"):
                if age < 900:      # 15 分钟还没推进 = 卡死
                    continue
            if age > SESSION_MAX_AGE:
                log(f"[reaper] {ph} 会话存活 {int(age)}s（上限 {int(SESSION_MAX_AGE)}s），自动关闭")
                try:
                    await drop_session(ph)
                except Exception as e:
                    log(f"[reaper] {ph} 关闭失败: {e}")
                    SESSIONS.pop(ph, None)


def _start_reaper():
    if LOOP is None:
        return
    asyncio.run_coroutine_threadsafe(_reaper(), LOOP)
    log(f"会话回收线程已启动（上限 {int(SESSION_MAX_AGE)}s）")


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):
        pass

    def _json(self, code, obj):
        body = json.dumps(obj, ensure_ascii=False).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        p = urlparse(self.path).path
        if p == "/health":
            now = time.time()
            info = [{"phone": ph, "stage": s.stage,
                     "sms_age": int(s.sms_age()),
                     "browser_age": int(now - s.created)}
                    for ph, s in SESSIONS.items()]
            self._json(200, {
                "ok": True, "service": "hwlogin-camoufox",
                "browser": "busy" if any(s.alive for s in SESSIONS.values()) else "idle",
                "display": os.environ.get("DISPLAY", ""),
                "sessions": len(SESSIONS), "detail": info,
                "sms_ttl": int(SMS_TTL),
                "session_max_age": int(SESSION_MAX_AGE),
            })
            return
        self._json(404, {"ok": False, "error": "not found"})

    def do_POST(self):
        p = urlparse(self.path).path
        n = int(self.headers.get("Content-Length") or 0)
        try:
            payload = json.loads(self.rfile.read(n) or b"{}")
        except Exception:
            self._json(400, {"ok": False, "error": "bad json"})
            return

        phone = (payload.get("phone") or "").strip()
        password = payload.get("password") or ""
        sms = (payload.get("sms_code") or "").strip()

        # /reset 是全局操作，不针对单个账号，不能要求 phone
        if p == "/reset":
            n = len(SESSIONS)

            async def _reset_all():
                for ph in list(SESSIONS.keys()):
                    s = SESSIONS.pop(ph, None)
                    if s:
                        try:
                            await s.close()
                        except Exception:
                            pass
                if LOCK.locked():
                    try:
                        LOCK.release()
                    except Exception:
                        pass
                log(f"[reset] 已清理 {n} 个会话并释放 LOCK")

            asyncio.run_coroutine_threadsafe(_reset_all(), LOOP)
            self._json(200, {"ok": True, "cleared": n})
            return

        if not phone:
            self._json(400, {"ok": False, "error": "phone required"})
            return

        if p == "/login":
            if not password and not sms:
                self._json(400, {"ok": False, "error": "password required"})
                return
            try:
                res = asyncio.run_coroutine_threadsafe(
                    do_login(phone, password, sms), LOOP).result(timeout=300)
                self._json(200, res)
            except Exception as e:
                log(f"[{phone}] 异常 {e}")
                traceback.print_exc()
                self._json(500, {"ok": False, "error": str(e)[:300]})
            return

        if p == "/logout":
            asyncio.run_coroutine_threadsafe(drop_session(phone), LOOP)
            self._json(200, {"ok": True})
            return

        self._json(404, {"ok": False, "error": "not found"})


LOOP = None


def main():
    global LOOP
    WORK.mkdir(parents=True, exist_ok=True)
    LOOP = asyncio.new_event_loop()
    asyncio.set_event_loop(LOOP)
    threading.Thread(target=LOOP.run_forever, daemon=True).start()
    log(f"hwlogin 服务启动 :{PORT} work={WORK} display={os.environ.get('DISPLAY','')} "
        f"sms_ttl={int(SMS_TTL)}s cap={CAP_BASE}")
    _start_reaper()
    srv = ThreadingHTTPServer(("127.0.0.1", PORT), Handler)
    srv.serve_forever()


if __name__ == "__main__":
    main()
