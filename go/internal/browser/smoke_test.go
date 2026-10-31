//go:build integration

package browser

import (
	"testing"

	"github.com/playwright-community/playwright-go"

	"hwcloud/internal/config"
)

func TestSmokeLaunch(t *testing.T) {
	if err := EnsureDriver(); err != nil {
		t.Fatal(err)
	}
	pw, err := playwright.Run()
	if err != nil {
		t.Fatal(err)
	}
	defer pw.Stop()

	br, ctx, err := NewHuaweiContext(pw, true)
	if err != nil {
		t.Fatal(err)
	}
	defer br.Close()

	page, err := ctx.NewPage()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := page.Goto(config.GrowURL, playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateDomcontentloaded,
		Timeout:   playwright.Float(60000),
	}); err != nil {
		t.Fatal(err)
	}
	title, _ := page.Title()
	t.Log("title:", title)
}