package task

import (
	"fmt"
	"strings"

	"github.com/playwright-community/playwright-go"

	"hwcloud/internal/browser"
	"hwcloud/internal/config"
)

type BlogOptions struct {
	Title    string
	Summary  string
	Content  string
	Headless bool
	Draft    bool
	DryRun   bool
}

func dismissStartupModal(page playwright.Page) {
	for _, label := range []string{"继续编辑", "新建内容", "我知道了", "确定"} {
		items, err := page.GetByText(label, playwright.PageGetByTextOptions{Exact: playwright.Bool(true)}).All()
		if err != nil {
			continue
		}
		for _, item := range items {
			if visible(item) {
				_ = item.Click()
				page.WaitForTimeout(1500)
				return
			}
		}
	}
}

// PublishBlog 在华为云社区编辑器中发布 Markdown 文章。
func PublishBlog(opts BlogOptions) (map[string]any, error) {
	if err := browser.EnsureDriver(); err != nil {
		return nil, err
	}
	pw, err := playwright.Run()
	if err != nil {
		return nil, err
	}
	defer pw.Stop()

	br, ctx, err := browser.NewHuaweiContext(pw, opts.Headless)
	if err != nil {
		return nil, err
	}
	defer br.Close()

	page, err := ctx.NewPage()
	if err != nil {
		return nil, err
	}
	if _, err := page.Goto(config.BlogEditorURL, playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateDomcontentloaded,
		Timeout:   playwright.Float(60000),
	}); err != nil {
		return nil, err
	}
	page.WaitForTimeout(9000)
	dismissStartupModal(page)

	if err := page.Locator("#blogTitle").Fill(opts.Title); err != nil {
		return nil, err
	}
	if err := page.GetByText("Markdown格式编辑", playwright.PageGetByTextOptions{Exact: playwright.Bool(true)}).First().Click(); err != nil {
		return nil, err
	}
	page.WaitForTimeout(4000)

	var frame playwright.Frame
	for _, f := range page.Frames() {
		if strings.Contains(f.URL(), "HDMDEditorPortal") {
			frame = f
			break
		}
	}
	if frame == nil {
		return nil, fmt.Errorf("Markdown 编辑器 iframe 未加载")
	}
	editor := frame.Locator("[contenteditable=true]").First()
	if err := editor.Click(); err != nil {
		return nil, err
	}
	if err := editor.Fill(opts.Content); err != nil {
		return nil, err
	}
	page.WaitForTimeout(5000)

	if opts.Summary != "" {
		_ = page.Locator("textarea[placeholder='请输入文章摘要']").First().Fill(opts.Summary)
	}

	if opts.DryRun {
		tags, _ := page.Locator(".tagator_tag").AllInnerTexts()
		return map[string]any{"dry_run": true, "title": opts.Title, "tags": tags}, nil
	}

	if opts.Draft {
		resp, err := page.ExpectResponse(func(r playwright.Response) bool {
			return strings.Contains(r.URL(), "save-draft")
		}, func() error {
			return page.Locator("#draft-btn").Click(playwright.LocatorClickOptions{Force: playwright.Bool(true)})
		}, playwright.PageExpectResponseOptions{Timeout: playwright.Float(60000)})
		if err != nil {
			return nil, err
		}
		var data map[string]any
		_ = resp.JSON(&data)
		return map[string]any{"draft_id": data["draftID"], "response": data}, nil
	}

	if err := page.Locator("#agreementBox").First().Click(playwright.LocatorClickOptions{Force: playwright.Bool(true)}); err != nil {
		return nil, err
	}
	page.WaitForTimeout(1000)

	resp, err := page.ExpectResponse(func(r playwright.Response) bool {
		return strings.Contains(r.URL(), "blog/blogging")
	}, func() error {
		return page.Locator("#submit-btn").Click(playwright.LocatorClickOptions{Force: playwright.Bool(true)})
	}, playwright.PageExpectResponseOptions{Timeout: playwright.Float(60000)})
	if err != nil {
		return nil, err
	}
	var data map[string]any
	if err := resp.JSON(&data); err != nil {
		return nil, err
	}
	blogID, _ := data["blogId"].(string)
	result := map[string]any{"blog_id": blogID, "response": data}
	if blogID != "" {
		result["url"] = "https://bbs.huaweicloud.com/blogs/" + blogID
	}
	return result, nil
}