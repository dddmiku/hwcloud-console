package cli

import (
	"strings"
	"testing"
)

func TestDeriveTitle(t *testing.T) {
	content := "# 测试标题\n\n## 小节\n正文"
	if got := deriveTitle(content); got != "测试标题" {
		t.Fatalf("deriveTitle = %q", got)
	}
	if got := deriveTitle("## 没有一级标题\n正文"); got != "" {
		t.Fatalf("deriveTitle without H1 = %q", got)
	}
}

func TestStripLeadingTitle(t *testing.T) {
	content := "# 测试标题\n\n## 小节\n正文"
	got := stripLeadingTitle(content)
	if strings.Contains(got, "# 测试标题") {
		t.Fatalf("title not stripped: %q", got)
	}
	if !strings.HasPrefix(got, "## 小节") {
		t.Fatalf("unexpected body: %q", got)
	}
}

func TestDeriveSummary(t *testing.T) {
	content := "# 测试标题\n\n第一段内容。\n\n第二段不应出现。"
	if got := deriveSummary(stripLeadingTitle(content), 100); got != "第一段内容。" {
		t.Fatalf("deriveSummary = %q", got)
	}
}

func TestDeriveSummaryTruncate(t *testing.T) {
	content := "# 标题\n\n" + strings.Repeat("字", 200)
	got := deriveSummary(stripLeadingTitle(content), 50)
	if len([]rune(got)) != 50 {
		t.Fatalf("summary length = %d", len([]rune(got)))
	}
}