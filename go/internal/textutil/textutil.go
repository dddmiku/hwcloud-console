// Package textutil 提供用户输入清洗。
//
// 为什么单独做：用户从网页、微信、PDF、密码管理器复制账号密码时，
// 经常带进看不见的字符。这些字符在输入框里显示不出来，
// 但会让「密码正确」变成「账号或密码错误」，排查成本极高。
//
// 实测踩过的字符：
//
//	U+00A0  不换行空格 NBSP     —— 从网页表格/富文本复制最常见
//	U+200B  零宽空格             —— 从微信/QQ 聊天记录复制常见
//	U+200C  零宽非连接符
//	U+200D  零宽连接符
//	U+FEFF  BOM（字节顺序标记） —— 从 CSV/Excel 复制常见
//	U+3000  全角空格             —— 中文输入法下容易打出来
//	U+2028  行分隔符
//	U+2029  段分隔符
//	\r \n \t 常规空白
package textutil

import "strings"

// invisible 是需要剥离的不可见字符集合。
//
// 这里必须用 \u 转义写，不能直接粘贴字符本身 ——
// 直接写会让源码里混进真正的不可见字符，
// Go 编译器遇到 U+FEFF 会报 "invalid BOM in the middle of the file"。
const invisible = "\u00a0\u200b\u200c\u200d\ufeff\u3000\u2028\u2029"

var invisibleRunes = func() map[rune]struct{} {
	m := make(map[rune]struct{}, len(invisible))
	for _, r := range invisible {
		m[r] = struct{}{}
	}
	return m
}()

// stripInvisible 去掉不可见字符（不动常规空格，交给调用方决定）。
func stripInvisible(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if _, bad := invisibleRunes[r]; bad {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// CleanPhone 清洗手机号：只保留数字。
//
// 会去掉空格、横线、括号、点号，以及 +86 / 86 国际区号前缀。
// 例：
//
//	" 130 0599 9962 "      -> "13005999962"
//	"+86 130-0599-9962"    -> "13005999962"
//	"130 0599　9962" -> "13005999962"
func CleanPhone(s string) string {
	if s == "" {
		return ""
	}
	s = stripInvisible(s)
	digits := make([]rune, 0, len(s))
	for _, r := range s {
		if r >= '0' && r <= '9' {
			digits = append(digits, r)
		}
	}
	out := string(digits)
	// 去掉国际区号 86：中国大陆手机号是 11 位
	if len(out) == 13 && strings.HasPrefix(out, "86") {
		out = out[2:]
	}
	return out
}

// CleanPassword 清洗密码：只去掉首尾空白和不可见字符。
//
// **中间的内容一律保留** —— 密码里可能有合法的空格、制表符，
// 全删会把正确密码改错，比不清洗更糟。
func CleanPassword(s string) string {
	if s == "" {
		return ""
	}
	return strings.TrimSpace(stripInvisible(s))
}

// CleanUsername 清洗用户名：去掉首尾空白和不可见字符，中间保留。
//
// 用户名理论上不含空格，但从聊天记录复制时中间也可能混进零宽字符，
// 所以中间只剥不可见字符、不动常规空格。
func CleanUsername(s string) string {
	if s == "" {
		return ""
	}
	return strings.TrimSpace(stripInvisible(s))
}
