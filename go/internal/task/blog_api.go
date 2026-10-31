package task

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"hwcloud/internal/config"
	"hwcloud/internal/huawei"
)

// 华为云社区博客的写接口都在 devdata 域下，走 cookie + csrf 头，不需要浏览器。
const (
	blogAPIBase       = "https://devdata.huaweicloud.com/rest/developer/fwdu/rest/developer/user/hdblogservice/v1/blog"
	blogPublishPath   = "/blogging"
	blogDraftPath     = "/save-draft"
	blogDeletePath    = "/delete-blog"
	blogDraftDelPath  = "/delete-draft"
	blogMyListPath    = "/get-my-blog-list"
	blogDraftListPath = "/get-draft"
)

// BlogPayload 对应编辑器 saveBlog() 拼出的 submitData。
// 字段名取自页面脚本，不要按语义改名。
type BlogPayload struct {
	TitleData        string   `json:"titleData"`
	IntroData        string   `json:"introData"`
	ContentData      string   `json:"contentData"`
	MarkdownContent  string   `json:"markdownContent"`
	TagNameData      string   `json:"tagNameData"`
	ImageURL         string   `json:"imageUrl"`
	AccessoryList    []any    `json:"accessoryList"`
	LitimgURL        string   `json:"litimgUrl"`
	IsLock           string   `json:"isLock"`
	BlogResource     int      `json:"blogResource"`
	IsReship         int      `json:"isReship"`
	IsMandatorySub   int      `json:"isMandatorySub"`
	ColumnID         []string `json:"column_id"`
	DisclaimerFlag   int      `json:"disclaimerFlag"`
	Source           int      `json:"source"` // 1=富文本, 2=Markdown
	DraftID          string   `json:"draftId,omitempty"`
}

type blogResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	BlogID  any    `json:"blogId"`
	DraftID any    `json:"draftID"`
}

// draftPayload 是 save-draft 接口的真实结构（只有四个字段）。
type draftPayload struct {
	Title   string `json:"title"`
	Content string `json:"content"`
	DraftID string `json:"draftID"`
	Source  int    `json:"source"`
}

// markdownToHTML 把 Markdown 转成编辑器要求的 HTML 片段。
// 华为云博客接口的 contentData 是 HTML，markdownContent 是原文。
func markdownToHTML(md string) string {
	var b strings.Builder
	inCode := false
	var para []string

	flushPara := func() {
		if len(para) == 0 {
			return
		}
		b.WriteString("<p>")
		b.WriteString(strings.Join(para, "<br>"))
		b.WriteString("</p>")
		para = nil
	}

	for _, line := range strings.Split(md, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			flushPara()
			if inCode {
				b.WriteString("</code></pre>")
				inCode = false
			} else {
				b.WriteString("<pre><code>")
				inCode = true
			}
			continue
		}
		if inCode {
			b.WriteString(htmlEscape(line))
			b.WriteString("\n")
			continue
		}
		if trimmed == "" {
			flushPara()
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			flushPara()
			level := 0
			for level < len(trimmed) && trimmed[level] == '#' {
				level++
			}
			if level > 6 {
				level = 6
			}
			text := strings.TrimSpace(trimmed[level:])
			fmt.Fprintf(&b, "<h%d>%s</h%d>", level, htmlEscape(text), level)
			continue
		}
		if strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") {
			flushPara()
			fmt.Fprintf(&b, "<p>• %s</p>", htmlEscape(strings.TrimSpace(trimmed[2:])))
			continue
		}
		para = append(para, htmlEscape(trimmed))
	}
	flushPara()
	if inCode {
		b.WriteString("</code></pre>")
	}
	return b.String()
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

// clipBody 截断响应体用于错误信息，避免把整页 HTML 打进日志。
func clipBody(data []byte) string {
	if len(data) > 200 {
		return string(data[:200])
	}
	return string(data)
}

// blogCall 用会话 cookie + csrf 打博客接口，返回原始响应体。
func blogCall(session *huawei.Session, method, path string, payload any) ([]byte, error) {
	var body []byte
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = b
	}
	csrf, err := session.CSRF()
	if err != nil {
		return nil, err
	}
	if csrf == "" {
		return nil, fmt.Errorf("未取得 csrf，登录态可能已失效")
	}

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, blogAPIBase+path, reader)
	if err != nil {
		return nil, err
	}
	// UA 必须与登录时一致：华为云 WAF 对不匹配的 UA 直接返回 404/403，
	// 看起来像「登录态失效」。
	req.Header.Set("User-Agent", session.UserAgent())
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	req.Header.Set("csrf", csrf)
	req.Header.Set("Referer", config.BlogEditorURL)
	req.Header.Set("Origin", "https://bbs.huaweicloud.com")

	resp, err := session.HTTPClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusForbidden && strings.Contains(string(data), "not login") {
		return nil, fmt.Errorf("登录态已失效，请重新运行 `hwcloud login`")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("博客接口返回 HTTP %d: %s", resp.StatusCode, clipBody(data))
	}
	return data, nil
}

// blogPost 调用写接口并校验业务码。
func blogPost(session *huawei.Session, path string, payload any) (*blogResponse, error) {
	data, err := blogCall(session, http.MethodPost, path, payload)
	if err != nil {
		return nil, err
	}

	var result blogResponse
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("响应解析失败: %w (%s)", err, clipBody(data))
	}
	if result.Code != "HD.92300000" {
		return nil, fmt.Errorf("提交失败 %s: %s", result.Code, result.Message)
	}
	return &result, nil
}

// PublishBlogAPI 纯协议发布博客（或存草稿）。
func PublishBlogAPI(title, summary, content string, draft bool, tags []string) (map[string]any, error) {
	draftID := ""
	session, err := huawei.New("")
	if err != nil {
		return nil, err
	}
	if _, err := session.RequireLogin(); err != nil {
		return nil, err
	}

	var payload any
	if draft {
		// save-draft 只接受这四个字段；多传会被判 HD.92311001 参数错误。
		payload = draftPayload{
			Title:   title,
			Content: content,
			DraftID: draftID,
			Source:  2,
		}
	} else {
		payload = BlogPayload{
			TitleData: title,
			IntroData: summary,
			// Markdown 模式下编辑器把原文同时放进两个字段：contentData 是渲染前的
			// Markdown（服务端自己转 HTML），markdownContent 也是原文。
			ContentData:     content,
			MarkdownContent: content,
			TagNameData:     strings.Join(tags, ","),
			ImageURL:        "",
			AccessoryList:   []any{},
			LitimgURL:       "",
			IsLock:          "1", // 允许评论
			BlogResource:    1,   // 原创
			IsReship:        1,   // 非转载
			IsMandatorySub:  0,
			ColumnID:        []string{},
			DisclaimerFlag:  1,
			Source:          2, // Markdown 编辑器
			DraftID:         draftID,
		}
	}

	path := blogPublishPath
	if draft {
		path = blogDraftPath
	}
	start := time.Now()
	result, err := blogPost(session, path, payload)
	if err != nil {
		return nil, err
	}

	out := map[string]any{
		"mode":       map[bool]string{true: "draft", false: "publish"}[draft],
		"elapsed_ms": time.Since(start).Milliseconds(),
		"code":       result.Code,
	}
	if id, ok := result.DraftID.(string); ok && id != "" {
		out["draft_id"] = id
	}
	if result.BlogID != nil {
		if id, ok := result.BlogID.(string); ok && id != "" {
			out["blog_id"] = id
			out["url"] = "https://bbs.huaweicloud.com/blogs/" + id
		}
	}
	return out, nil
}

// BlogItem 是「我的博客」列表里的一条记录。
type BlogItem struct {
	ID    string
	Title string
	State string
	Date  string
}

// ListBlogs 返回当前账号已发布的文章。
func ListBlogs() ([]BlogItem, error) {
	session, err := huawei.New("")
	if err != nil {
		return nil, err
	}
	data, err := blogCall(session, http.MethodGet, blogMyListPath, nil)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Count    int `json:"count"`
		BlogInfo []struct {
			Vo map[string]any `json:"CommunityBlogVo"`
		} `json:"CommunityBlogInfo"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("列表解析失败: %w (%s)", err, clipBody(data))
	}
	items := make([]BlogItem, 0, len(raw.BlogInfo))
	for _, it := range raw.BlogInfo {
		m := it.Vo
		item := BlogItem{
			ID:    pickString(m, "blogId", "id"),
			Title: pickString(m, "blogTitle", "titleData", "title"),
			State: pickString(m, "status"),
		}
		// createAt 是 Unix 秒
		if ts, ok := m["createAt"].(float64); ok && ts > 0 {
			item.Date = time.Unix(int64(ts), 0).Format("2006-01-02 15:04")
		} else {
			item.Date = pickString(m, "createTime", "publishTime", "updateTime")
		}
		items = append(items, item)
	}
	return items, nil
}

// ListDrafts 返回草稿列表。
func ListDrafts() ([]BlogItem, error) {
	session, err := huawei.New("")
	if err != nil {
		return nil, err
	}
	data, err := blogCall(session, http.MethodGet, blogDraftListPath, nil)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Count int `json:"count"`
		List  []struct {
			DraftVo map[string]any `json:"DraftVo"`
		} `json:"DraftList"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("草稿解析失败: %w (%s)", err, clipBody(data))
	}
	items := make([]BlogItem, 0, len(raw.List))
	for _, it := range raw.List {
		items = append(items, BlogItem{
			ID:    pickString(it.DraftVo, "draftID", "id"),
			Title: pickString(it.DraftVo, "draftTitle", "title"),
			Date:  pickString(it.DraftVo, "draftTime", "updateTime"),
		})
	}
	return items, nil
}

func pickString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
		if v, ok := m[k].(float64); ok {
			return fmt.Sprintf("%.0f", v)
		}
	}
	return ""
}

// DeleteBlog 删除一篇已发布文章；draft=true 时删的是草稿。
func DeleteBlog(id string, draft bool) (map[string]any, error) {
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("需要提供文章 ID")
	}
	session, err := huawei.New("")
	if err != nil {
		return nil, err
	}
	path := blogDeletePath
	if draft {
		path = blogDraftDelPath + "?draftID=" + id
	}
	data, err := blogCall(session, http.MethodPost, path, map[string]string{"id": id})
	if err != nil {
		return nil, err
	}
	var result blogResponse
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("响应解析失败: %w (%s)", err, clipBody(data))
	}
	if result.Code != "HD.92300000" {
		return nil, fmt.Errorf("删除失败 %s: %s", result.Code, result.Message)
	}
	return map[string]any{"deleted": id, "draft": draft, "code": result.Code}, nil
}
