//go:build integration

package task

import (
	"testing"

	"github.com/playwright-community/playwright-go"

	"hwcloud/internal/browser"
	"hwcloud/internal/config"
)

func TestProbeLogin(t *testing.T) {
	if err := browser.EnsureDriver(); err != nil {
		t.Fatal(err)
	}
	pw, err := playwright.Run()
	if err != nil {
		t.Fatal(err)
	}
	defer pw.Stop()

	br, ctx, err := browser.NewHuaweiContext(pw, true)
	if err != nil {
		t.Fatal(err)
	}
	defer br.Close()

	page, err := ctx.NewPage()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := page.Goto(config.GrowURL, playwright.PageGotoOptions{WaitUntil: playwright.WaitUntilStateDomcontentloaded}); err != nil {
		t.Fatal(err)
	}
	data := probeLoginPage(page)
	if data == nil {
		t.Fatal("login probe returned nil")
	}
	t.Log("memAlias:", data["memAlias"], "memName:", data["memName"])
}