package task

import (
	"fmt"
	"strings"
	"time"

	"github.com/playwright-community/playwright-go"

	"hwcloud/internal/browser"
)

// 鸿蒙实践中心的「云端实践」(+100，每周)。
//
// 为什么纯协议走不通：
//   evaluator 是 external_report，`submit` 接口直接 403
//   `browser_submit_not_allowed` —— 上报由 DevSpace 环境内部完成，
//   不对外暴露提交口。
//
// 完整链路（全部在浏览器里完成）：
//  1. 打开 devstation 的 harmony-practice-center 页，让它走
//     index/islogin?ticket=ST-... 完成 DevSpace 侧登录
//  2. 点「临时凭据自动配置」弹窗的「同意」—— 不点会一直卡在这个弹窗，
//     永远进不了环境
//  3. 环境创建/启动（容器 devenv），等「环境准备中」消失
//  4. 环境就绪后 floor-practice-center 会加载，上报由平台侧自己发
//
// 关于内测资格：
//   案例详情页的「云端实践」按钮会先查 beta-harmony-practice-application，
//   但那个接口在 devstation-svc.connect 域，用 Go 发要过 WAF + CSRF，
//   实测拿不到登录态（403 not login）。好在这不影响主流程 ——
//   直接打开 DevSpace 页面本身就会走完整登录并创建环境。
//   申请状态只是给人看的信息，不是执行前提。
//
// 注意：这个函数会真的创建/启动一个 DevSpace 容器环境，消耗核时。
// 调用前应先确认任务未完成。

const (
	devSpacePageURL = "https://devstation.developer.huaweicloud.com/ai-devspace/beta/harmony-practice-center"
)

// RunCloudPractice 完成「云端实践」：打开 DevSpace → 授权 → 等环境就绪。
func RunCloudPractice(headless bool) (map[string]any, error) {
	pw, err := playwright.Run()
	if err != nil {
		return nil, err
	}
	defer pw.Stop()

	br, ctx, err := browser.NewHuaweiContext(pw, headless)
	if err != nil {
		return nil, err
	}
	defer br.Close()
	defer ctx.Close()

	page, err := ctx.NewPage()
	if err != nil {
		return nil, err
	}

	if _, err := page.Goto(devSpacePageURL, playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateDomcontentloaded,
		Timeout:   playwright.Float(90000),
	}); err != nil {
		return nil, fmt.Errorf("打开 DevSpace 失败: %v", err)
	}
	page.WaitForTimeout(16000)

	// 点「同意」授权临时凭据。
	//
	// 这个弹窗是进入环境的必经步骤：不点，页面就一直停在
	// 「是否同意将您的临时访问凭据(AK/SK) 同步至 AI 开发者空间环境」，
	// 环境根本不会开始创建。
	agreed := false
	for _, label := range []string{"同意", "同意并继续"} {
		for _, sel := range []string{
			`button:has-text("%s")`,
			`.el-button:has-text("%s")`,
			`[class*="dialog"] button:has-text("%s")`,
			`[class*="modal"] button:has-text("%s")`,
		} {
			loc := page.Locator(fmt.Sprintf(sel, label))
			n, _ := loc.Count()
			if n == 0 {
				continue
			}
			if vis, _ := loc.First().IsVisible(); !vis {
				continue
			}
			if err := loc.First().Click(playwright.LocatorClickOptions{
				Timeout: playwright.Float(6000),
			}); err == nil {
				agreed = true
				break
			}
		}
		if agreed {
			break
		}
	}

	// 等环境就绪。
	// 判据：「环境准备中」消失，且出现「剩余核时」或「当前环境运行中」。
	// 实测创建容器约 30~60 秒。
	ready := false
	deadline := time.Now().Add(6 * time.Minute)
	for time.Now().Before(deadline) {
		page.WaitForTimeout(6000)
		b, err := page.Locator("body").InnerText()
		if err != nil {
			continue
		}
		if strings.Contains(b, "环境准备中") {
			continue
		}
		if strings.Contains(b, "剩余核时") || strings.Contains(b, "当前环境运行中") {
			ready = true
			break
		}
	}

	// 环境就绪后再停一会儿，让 floor-practice-center 加载并触发上报
	if ready {
		page.WaitForTimeout(20000)
	}

	msg := "环境未在 6 分钟内就绪"
	if ready {
		msg = "环境已就绪，云端实践已触发"
	}
	return map[string]any{
		"ok":        ready,
		"agreed":    agreed,
		"env_ready": ready,
		"msg":       msg,
	}, nil
}
