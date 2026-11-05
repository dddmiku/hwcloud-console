package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"hwcloud/internal/account"
	"hwcloud/internal/config"
	"hwcloud/internal/huawei"
	"hwcloud/internal/task"
	"hwcloud/internal/web"
)

func usage() {
	fmt.Print(`huaweicloud 成长中心任务自动化

用法: hwcloud <命令> [参数]

命令:
  login          华为云交互登录并保存会话（浏览器）
  login-pw       纯协议密码登录（--account/--password/--sms）
  import-cookie  从 Cookie 头或文件导入华为云会话
  login-gitcode  GitCode 交互登录并保存会话
  check          查看登录态、签到和任务状态
  sign           自动签到（已签到则跳过）
  tasks          列出任务
  lab            启动云实验并提交过程记录
  blog           发布博客文章
  blogs          列出已发布文章
  drafts         列出草稿
  delete         删除已发布文章（--draft 删草稿）
  devspace       上报「体验 AI DevSpace」行为
  daily          一键日常（签到+DevSpace+发文章+鸿蒙浏览+鸿蒙Star）
  weekly         一键周常（云实验+Issue+Star+活动+赛事+鸿蒙云端实践）
  status         实践中心账号与 GitCode 绑定状态
  bind           绑定 GitCode 账号（--gitcode <用户名>）
  keepalive      用持久化 profile 刷新会话（失效时自动续期）
  pure-login     纯协议登录（--account/--password/--sms/--fp）
  verify-pw      纯协议验证密码（只读，不登录）
  send-sms       纯协议发送短信验证码
  login-pw2      纯协议账密登录（设备已信任时无需验证码）
  save-sid       保存 hwid_cas_sid（信任设备后得到）
  serve          启动 Web 管理面板（--addr :8080）
  session        查看会话剩余有效期
  issue          创建 GitCode Issue
  star           给实践中心仓库点 Star（自动挑未 star 的）
  eventjoin      报名一个进行中的开发者活动
`)
}

func parseStars(s string) []int {
	s = strings.ReplaceAll(s, "，", ",")
	var out []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if n, err := strconv.Atoi(part); err == nil {
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		out = []int{1, 2}
	}
	return out
}

func resolve(path, base string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(base, path)
}

func readText(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func deriveTitle(content string) string {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "# ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "# "))
		}
	}
	return ""
}

func stripLeadingTitle(content string) string {
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		if strings.HasPrefix(t, "# ") {
			lines = append(lines[:i], lines[i+1:]...)
			for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
				lines = append(lines[:i], lines[i+1:]...)
			}
		}
		break
	}
	return strings.Join(lines, "\n")
}

func deriveSummary(content string, maxRunes int) string {
	var parts []string
	inCode := false
	for _, line := range strings.Split(content, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "```") {
			inCode = !inCode
			continue
		}
		if inCode {
			continue
		}
		if t == "" {
			if len(parts) > 0 {
				break
			}
			continue
		}
		if strings.HasPrefix(t, "#") {
			continue
		}
		parts = append(parts, t)
		if len([]rune(strings.Join(parts, ""))) >= maxRunes {
			break
		}
	}
	text := strings.Join(parts, " ")
	runes := []rune(text)
	if len(runes) > maxRunes {
		text = string(runes[:maxRunes])
	}
	return text
}

// runWithStdoutToStderr 执行 fn，期间把进程的 stdout 重定向到 stderr。
//
// 用途：daily / weekly 的结果要是一段干净 JSON 给脚本解析，
// 但内部逻辑（runDaily/runWeekly → LoginGitCodeAuto 等）里有大量
// fmt.Printf 进度输出，会混进 stdout 把 JSON 冲散。
//
// 做法是临时替换 os.Stdout 这个文件描述符指向的句柄：
// 保留原 stdout 的副本，把 os.Stdout 指向 stderr，跑完再换回来。
// 这样 fn 里所有 fmt.Print* 都落到 stderr，而 printJSON 在恢复后
// 仍然写真正的 stdout。
//
// 注意：这是进程级的，不能并发调用。CLI 是单进程单命令，没问题。
func runWithStdoutToStderr(fn func() map[string]any) map[string]any {
	orig := os.Stdout
	os.Stdout = os.Stderr
	defer func() { os.Stdout = orig }()
	return fn()
}

func printJSON(v any) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Printf("%v\n", v)
		return
	}
	fmt.Println(string(data))
}

func fail(err error) int {
	fmt.Fprintf(os.Stderr, "[-] %v\n", err)
	return 2
}

// ensureSession 在任务命令前自愈会话：失效时用 refresh token 自动续期。
// 失败不阻断命令本身——让真正的请求去报错，比在这里猜更有信息量。
func ensureSession() {
	if err := task.EnsureSession(); err != nil {
		fmt.Fprintf(os.Stderr, "[!] 会话自愈失败：%v\n", err)
	}
}

// cliAutoLogin 用当前账号（HWCLOUD_ACCOUNT）里存的手机号+密码自动登录。
//
// 供 keepalive 的兜底使用：纯协议续期和浏览器 profile 都失败时，
// 账密登录是唯一能恢复会话的手段。登录成功会把 cookie/meta/fp 写回该账号 state。
func cliAutoLogin() (bool, string) {
	id := config.AccountID()
	if id == "" {
		return false, "未指定账号（HWCLOUD_ACCOUNT 为空）"
	}
	store, err := account.NewStore()
	if err != nil {
		return false, "读取账号失败: " + err.Error()
	}
	acc, ok := store.Get(id)
	if !ok {
		return false, "账号不存在: " + id
	}
	if acc.Password == "" {
		return false, "未保存密码，无法自动登录"
	}
	// 用户关掉了自动保活就别再拉浏览器登录。
	//
	// 开了双重验证的账号，自动登录必然停在等短信 ——
	// 每跑一次保活就发一条，用户手机上被反复轰炸。
	// 面板的 autoLogin 已经检查过这个开关，CLI 这条路径也要检查，
	// 否则定时脚本走 CLI 时照样会发短信。
	if !acc.AutoKeepAliveOn() {
		return false, "该账号已关闭自动保活，请手动登录"
	}

	fmt.Fprintf(os.Stderr, "[*] 会话失效，尝试账密自动登录（%s）…\n", id)
	res, err := task.CamoufoxLogin(acc.Phone, acc.Password, "")
	if err != nil && (res == nil || res.Stage == "") {
		return false, err.Error()
	}
	if res == nil {
		return false, "登录服务未返回结果"
	}
	switch res.Stage {
	case "need_sms":
		return false, "需要短信验证码，自动登录无法完成"
	case "sms_not_sent":
		return false, "短信未发出"
	case "slider_failed":
		return false, "滑块验证未通过"
	}

	names := task.CamoufoxCookieNames(res)
	if !names["SSOTGC"] && !names["CASTGC"] {
		return false, "登录未完成（缺少 SSOTGC/CASTGC）"
	}
	if err := task.SaveCamoufoxArtifacts(res); err != nil {
		return false, "保存会话失败: " + err.Error()
	}
	// 用户名必须从会话里读，不能写手机号。
	// 早先这里写 acc.Phone，导致 account.json 的 username 变成手机号，
	// 和面板/check 从 cookie 读到的 yd_xxxxxxxxx 对不上，看起来像串号。
	uname := ""
	if info, err := task.SessionInfo(); err == nil {
		if v, ok := info["username"].(string); ok {
			uname = v
		}
	}
	store.MarkChecked(id, true, uname, 0, "")
	if res.Trusted {
		_ = store.Update(id, func(a *account.Account) {
			a.Trusted = true
			a.TrustedAt = time.Now().Format("2006-01-02 15:04:05")
		})
	}
	fmt.Fprintf(os.Stderr, "[+] 账密自动登录成功\n")
	return true, "自动登录成功"
}

func Main(args []string) int {
	if len(args) < 2 {
		usage()
		return 1
	}
	cmd := args[1]
	rest := args[2:]

	switch cmd {
	case "login":
		fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
		timeout := fs.Int("timeout", 600, "等待登录秒数")
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		if err := task.LoginHuawei(*timeout); err != nil {
			return fail(err)
		}
		return 0

	case "login-pw":
		fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
		account := fs.String("account", "", "手机号/邮箱/账号名")
		password := fs.String("password", "", "密码")
		sms := fs.String("sms", "", "短信验证码（首次登录通常需要）")
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		result, err := task.PasswordLogin(task.PasswordLoginOptions{
			Account:  *account,
			Password: *password,
			SMSCode:  *sms,
		})
		if err != nil {
			return fail(err)
		}
		printJSON(result)
		return 0

	case "import-cookie":
		fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
		cookie := fs.String("cookie", "", "Cookie 头字符串")
		cookieFile := fs.String("cookie-file", "", "cookie JSON/文本文件")
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		session, err := huawei.New(*cookieFile)
		if err != nil {
			return fail(err)
		}
		if *cookie != "" {
			n := session.ImportCookieString(*cookie)
			if err := session.SaveCookies(); err != nil {
				return fail(err)
			}
			fmt.Printf("[+] 已导入 %d 个 cookie\n", n)
		}
		info, err := session.CheckLogin()
		if err != nil {
			return fail(err)
		}
		if info == nil {
			fmt.Println("[-] 登录态无效或已过期")
			return 2
		}
		fmt.Printf("[+] 登录态有效：%s\n", session.Username)
		return 0

	case "login-gitcode":
		fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
		timeout := fs.Int("timeout", 900, "等待登录秒数")
		account := fs.String("account", "", "GitCode 用户名（给了就自动登录，否则等人工）")
		password := fs.String("password", "", "GitCode 密码")
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		var (
			result map[string]any
			err    error
		)
		if *account != "" && *password != "" {
			result, err = task.LoginGitCodeAuto(*account, *password, *timeout)
		} else {
			result, err = task.LoginGitCode(*timeout)
		}
		if err != nil {
			return fail(err)
		}
		printJSON(result)
		return 0

	case "check":
		fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
		stars := fs.String("stars", "1,2", "任务星级，例如 1,2")
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		ensureSession()
		session, err := huawei.New("")
		if err != nil {
			return fail(err)
		}
		summary, err := huawei.Summarize(session, parseStars(*stars)...)
		if err != nil {
			return fail(err)
		}
		fmt.Println(huawei.FormatSummary(summary))
		return 0

	case "sign":
		ensureSession()
		session, err := huawei.New("")
		if err != nil {
			return fail(err)
		}
		already, err := session.AutoSign()
		if err != nil {
			return fail(err)
		}
		if already {
			fmt.Println("今日已签到，无需重复签到")
		} else {
			fmt.Println("签到成功")
		}
		return 0

	case "tasks":
		fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
		stars := fs.String("stars", "1,2", "任务星级，例如 1,2")
		asJSON := fs.Bool("json", false, "输出原始 JSON（含 url / 描述等字段）")
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		ensureSession()
		session, err := huawei.New("")
		if err != nil {
			return fail(err)
		}
		missions, err := huawei.TasksByStar(session, parseStars(*stars)...)
		if err != nil {
			return fail(err)
		}
		if *asJSON {
			printJSON(missions)
			return 0
		}
		degrees := map[int]bool{}
		for _, m := range missions {
			degrees[m.Degree] = true
		}
		var ordered []int
		for d := range degrees {
			ordered = append(ordered, d)
		}
		sort.Ints(ordered)
		for _, d := range ordered {
			label := huawei.StarLabel[d]
			if label == "" {
				label = fmt.Sprintf("%d星", d)
			}
			fmt.Printf("%s:\n", label)
			for _, m := range missions {
				if m.Degree != d {
					continue
				}
				mark := "未完成"
				if m.IsFinish == 1 {
					mark = "已完成"
				}
				fmt.Printf("  [%s] %s (+%d)\n", mark, m.Title, m.Bonus)
			}
		}
		return 0

	case "lab":
		fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
		labID := fs.Int("lab-id", config.DefaultLabID, "实验 ID")
		recordFile := fs.String("record-file", "", "过程记录文本文件")
		keep := fs.Bool("keep", false, "保留实验环境，不自动结束")
		headless := fs.Bool("headless", false, "无头模式")
		dryRun := fs.Bool("dry-run", false, "只检查页面，不启动实验")
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		ensureSession()
		recordPath := ""
		if *recordFile != "" {
			recordPath = resolve(*recordFile, config.DataDir())
		} else {
			recordPath = filepath.Join(config.DataDir(), "lab_record.txt")
		}
		text, err := readText(recordPath)
		if err != nil {
			return fail(fmt.Errorf("读取实验过程记录失败：%w（可用 --record-file 指定）", err))
		}
		recordText := strings.TrimSpace(text)
		if recordText == "" {
			return fail(fmt.Errorf("实验过程记录为空：请填写 %s 或传 --record-file", recordPath))
		}
		result, err := task.RunLabTask(task.LabOptions{
			LabID:      *labID,
			RecordText: recordText,
			Headless:   *headless,
			End:        !*keep,
			Timeout:    180,
			DryRun:     *dryRun,
		})
		if err != nil {
			return fail(err)
		}
		printJSON(result)
		return 0

	case "blog":
		fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
		file := fs.String("file", "blog_post.md", "Markdown 文件")
		title := fs.String("title", "", "标题")
		summary := fs.String("summary", "", "摘要")
		tags := fs.String("tags", "", "标签，逗号分隔（留空由平台自动匹配）")
		draft := fs.Bool("draft", false, "只保存草稿")
		headless := fs.Bool("headless", false, "无头模式")
		dryRun := fs.Bool("dry-run", false, "只填写，不发布")
		browserMode := fs.Bool("browser", false, "改用浏览器操作编辑器")
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		ensureSession()
		content, err := readText(resolve(*file, config.DataDir()))
		if err != nil {
			return fail(err)
		}
		if *title == "" {
			*title = deriveTitle(content)
		}
		if *title == "" {
			return fail(fmt.Errorf("标题为空：请在 Markdown 中用 `# 标题` 指定，或传 --title"))
		}
		if *summary == "" {
			*summary = deriveSummary(stripLeadingTitle(content), 100)
		}
		content = stripLeadingTitle(content)
		if *dryRun {
			printJSON(map[string]any{
				"dry_run": true,
				"title":   *title,
				"summary": *summary,
				"mode":    "protocol",
				"html":    len(content),
			})
			return 0
		}
		if *browserMode {
			result, err := task.PublishBlog(task.BlogOptions{
				Title:    *title,
				Summary:  *summary,
				Content:  content,
				Headless: *headless,
				Draft:    *draft,
				DryRun:   *dryRun,
			})
			if err != nil {
				return fail(err)
			}
			printJSON(result)
			return 0
		}
		var tagList []string
		for _, t := range strings.Split(*tags, ",") {
			if t = strings.TrimSpace(t); t != "" {
				tagList = append(tagList, t)
			}
		}
		result, err := task.PublishBlogAPI(*title, *summary, content, *draft, tagList)
		if err != nil {
			return fail(err)
		}
		printJSON(result)
		return 0

	case "blogs":
		items, err := task.ListBlogs()
		if err != nil {
			return fail(err)
		}
		if len(items) == 0 {
			fmt.Println("没有已发布的文章")
			return 0
		}
		fmt.Printf("共 %d 篇：\n", len(items))
		for _, it := range items {
			fmt.Printf("  %s  %s  %s\n", it.ID, it.Date, it.Title)
		}
		return 0

	case "drafts":
		ensureSession()
		items, err := task.ListDrafts()
		if err != nil {
			return fail(err)
		}
		if len(items) == 0 {
			fmt.Println("没有草稿")
			return 0
		}
		fmt.Printf("共 %d 篇草稿：\n", len(items))
		for _, it := range items {
			fmt.Printf("  %s  %s  %s\n", it.ID, it.Date, it.Title)
		}
		return 0

	case "delete":
		fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
		id := fs.String("id", "", "文章 ID（不填则列出可选）")
		draft := fs.Bool("draft", false, "删除草稿而不是已发布文章")
		yes := fs.Bool("yes", false, "确认删除，不加只做预演")
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		ensureSession()
		if *id == "" {
			var items []task.BlogItem
			var err error
			if *draft {
				items, err = task.ListDrafts()
			} else {
				items, err = task.ListBlogs()
			}
			if err != nil {
				return fail(err)
			}
			kind := "文章"
			if *draft {
				kind = "草稿"
			}
			if len(items) == 0 {
				fmt.Printf("没有可删除的%s\n", kind)
				return 0
			}
			fmt.Printf("可删除的%s：\n", kind)
			for _, it := range items {
				fmt.Printf("  %s  %s  %s\n", it.ID, it.Date, it.Title)
			}
			fmt.Printf("\n用 --id <ID> --yes 执行删除\n")
			return 0
		}
		if !*yes {
			fmt.Printf("预演：将删除%s %s，加 --yes 才会真正执行\n", map[bool]string{true: "草稿", false: "文章"}[*draft], *id)
			return 0
		}
		result, err := task.DeleteBlog(*id, *draft)
		if err != nil {
			return fail(err)
		}
		printJSON(result)
		return 0

	case "devspace":
		result, err := task.TrackDevSpace()
		if err != nil {
			return fail(err)
		}
		printJSON(result)
		return 0

	case "daily":
		// 一键日常：签到 + DevSpace + 发文章 + 鸿蒙浏览 + 鸿蒙 Star。
		// 和面板上那个「一键日常」是同一份逻辑（web.RunDailyForCLI）。
		//
		// 为什么要重定向 stdout：
		//   runDaily 内部会调 LoginGitCodeAuto（GitCode 会话过期时重登），
		//   那个函数里有多处 fmt.Printf("[*] ...") 进度输出。
		//   它们会混进 stdout，把结果 JSON 冲散 ——
		//   定时脚本的 json.load 直接失败，日志里只剩「(输出非 JSON)」。
		//   这里把执行期间的 stdout 全部转到 stderr，只让最后的 JSON 走 stdout。
		result := runWithStdoutToStderr(web.RunDailyForCLI)
		printJSON(result)
		if e, ok := result["error"].(string); ok && e != "" {
			return 1
		}
		if ok, _ := result["ok"].(bool); !ok {
			return 1
		}
		return 0

	case "weekly":
		// 一键周常：云实验 + 提Issue + Star仓库 + 活动 + 赛事 + 鸿蒙云端实践。
		// 同样重定向 stdout，理由见上面 daily 分支的注释。
		result := runWithStdoutToStderr(web.RunWeeklyForCLI)
		printJSON(result)
		if e, ok := result["error"].(string); ok && e != "" {
			return 1
		}
		if ok, _ := result["ok"].(bool); !ok {
			return 1
		}
		return 0

	case "serve":
		fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
		addr := fs.String("addr", ":8080", "监听地址")
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		srv, err := web.NewServer(*addr)
		if err != nil {
			return fail(err)
		}
		if err := srv.Start(); err != nil {
			return fail(err)
		}
		return 0

	case "login-pw2":
		fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
		account := fs.String("account", "", "手机号")
		password := fs.String("password", "", "密码")
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		if *account == "" || *password == "" {
			return fail(fmt.Errorf("需要 --account 和 --password"))
		}
		result, err := task.LoginByPassword(*account, *password)
		if err != nil {
			return fail(err)
		}
		printJSON(result)
		if result.Stage != "done" {
			return 3
		}
		return 0

	case "save-sid":
		fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
		sid := fs.String("sid", "", "hwid_cas_sid 值")
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		if *sid == "" {
			return fail(fmt.Errorf("需要 --sid"))
		}
		if err := task.SaveHwidSid(*sid); err != nil {
			return fail(err)
		}
		fmt.Println("[+] 已保存 hwid_cas_sid")
		return 0

	case "verify-pw":
		fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
		account := fs.String("account", "", "手机号/邮箱/账号名")
		password := fs.String("password", "", "密码")
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		if *account == "" || *password == "" {
			return fail(fmt.Errorf("需要 --account 和 --password"))
		}
		result, err := task.VerifyPassword(*account, *password)
		if err != nil {
			return fail(err)
		}
		printJSON(result)
		return 0

	case "send-sms":
		fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
		account := fs.String("account", "", "手机号")
		password := fs.String("password", "", "密码")
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		if *account == "" || *password == "" {
			return fail(fmt.Errorf("需要 --account 和 --password"))
		}
		result, err := task.SendSMSCode(*account, *password)
		if err != nil {
			return fail(err)
		}
		printJSON(result)
		return 0

	case "pure-login":
		fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
		account := fs.String("account", "", "手机号/邮箱/账号名")
		password := fs.String("password", "", "密码")
		sms := fs.String("sms", "", "短信验证码（首次调用不带，拿到短信后再带）")
		fp := fs.String("fp", "", "浏览器指纹（首次从浏览器抓取，之后可复用）")
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		if *account == "" || *password == "" {
			return fail(fmt.Errorf("需要 --account 和 --password"))
		}
		if *fp != "" {
			if err := task.SaveFp(*fp); err != nil {
				return fail(err)
			}
			fmt.Println("[+] 已保存指纹缓存")
		}
		var result *task.PureLoginResult
		var err error
		if *sms == "" {
			result, err = task.PureLoginStep(*account, *password)
		} else {
			result, err = task.PureLoginWithSMS(*account, *password, *sms)
		}
		if err != nil {
			return fail(err)
		}
		printJSON(result)
		return 0

	case "keepalive":
		fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
		headless := fs.Bool("headless", true, "无头模式")
		minRemain := fs.Float64("min-remain", 0, "剩余小时数低于此值时强制刷新（0=仅失效时刷新）")
		autoLogin := fs.Bool("auto-login", true, "会话失效时用账号密码自动登录（需要已保存密码）")
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		ensureSession()
		result, err := task.KeepAlive(*headless, *minRemain)
		if err != nil {
			return fail(err)
		}
		// 保活失败（会话确实失效）时，用账号里存的密码再登录一次。
		//
		// 这是定时任务链路的兜底：纯协议续期和浏览器 profile 都救不回来时，
		// 只有账密登录能恢复。登录走 Camoufox（要过滑块，约 30-60 秒），
		// 所以只在 keepalive 明确返回非 alive 时才触发，不抢跑。
		if result.After != "alive" && *autoLogin {
			if ok, msg := cliAutoLogin(); ok {
				result.After = "alive"
				result.Refreshed = true
				result.Message = "会话失效，已自动账密登录"
				if sess, err := task.SessionInfo(); err == nil {
					if u, ok := sess["username"].(string); ok {
						result.Username = u
					}
				}
			} else {
				result.Message = result.Message + "；自动登录失败: " + msg
			}
		}
		printJSON(result)
		if result.After != "alive" {
			return 3
		}
		return 0

	case "session":
		result, err := task.SessionInfo()
		if err != nil {
			return fail(err)
		}
		printJSON(result)
		return 0

	case "status":
		result, err := task.HohStatus()
		if err != nil {
			return fail(err)
		}
		printJSON(result)
		return 0

	case "bind":
		fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
		gcUser := fs.String("gitcode", "", "要绑定的 GitCode 用户名")
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		if *gcUser == "" {
			return fail(fmt.Errorf("需要 --gitcode <GitCode 用户名>"))
		}
		ensureSession()
		result, err := task.HohBind(*gcUser)
		if err != nil {
			return fail(err)
		}
		printJSON(result)
		return 0

	case "star":
		fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
		dryRun := fs.Bool("dry-run", false, "只检查，不点击")
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		ensureSession()
		result, err := task.StarRepo(*dryRun)
		if err != nil {
			return fail(err)
		}
		printJSON(result)
		return 0

	case "eventjoin":
		fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
		dryRun := fs.Bool("dry-run", false, "只检查，不提交")
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		ensureSession()
		result, err := task.JoinEvent(*dryRun)
		if err != nil {
			return fail(err)
		}
		printJSON(result)
		return 0

	case "issue":
		fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
		file := fs.String("file", "issue_feature.md", "Issue 内容文件")
		title := fs.String("title", "", "标题")
		template := fs.String("template", "feature", "模板: feature 或 bug")
		headless := fs.Bool("headless", false, "无头模式")
		dryRun := fs.Bool("dry-run", false, "只填写，不提交")
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		ensureSession()
		body, err := readText(resolve(*file, config.DataDir()))
		if err != nil {
			return fail(err)
		}
		if *title == "" {
			*title = deriveTitle(body)
		}
		if *title == "" {
			return fail(fmt.Errorf("标题为空：请在文件中用 `# 标题` 指定，或传 --title"))
		}
		result, err := task.CreateIssue(*title, body, *template, *headless, *dryRun)
		if err != nil {
			return fail(err)
		}
		printJSON(result)
		return 0

	default:
		usage()
		return 1
	}
}
