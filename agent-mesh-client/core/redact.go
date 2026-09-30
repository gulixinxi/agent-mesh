package core

import (
	"regexp"
	"sort"
	"strings"
)

// 本文件实现「上报前脱敏」——对应 GOAL.md 的 G-2。
//
// 为什么必须有这一层：在这之前，员工与 AI 的对话是**原文裸奔**进 audit_logs 的。
// 加密只保护了传输通道（HMAC + TLS），一旦到了数据库就是明文，
// 而控制台只要一个 Basic Auth 就能把全文翻出来。这是当前最大的合规风险。
//
// 设计取舍：
//  1. 零外部依赖（只用 regexp），保证在客户机上不会因为缺依赖而失效。
//  2. 脱敏发生在**客户端上报之前**，敏感内容从来不出本机。
//     服务端即使被攻破，拿到的也只有占位符——这是与"事后清洗"的本质区别。
//  3. 规则从"最具体/最长"排到"最宽泛"，避免先被宽规则吃掉导致漏判
//     （例如身份证的 18 位必须先于银行卡的 16-19 位处理，否则会被银行规则误标）。
//  4. 只替换不删除：保留占位符，让审计人员知道"这里原本有一串手机号"，
//     而不是让上下文悄无声息地缺一块。

// RedactRule 描述一条脱敏规则。
type RedactRule struct {
	// Kind 是上报到服务端的敏感类型标识，会写进 audit_logs.redacted。
	Kind string
	// Pattern 用于匹配敏感片段。必须是 RE2 语法——Go 的 regexp 不支持反向预查。
	Pattern *regexp.Regexp
	// Replace 是替换模板，$1 $2 之类捕获组可用来保留上下文结构。
	Replace string
}

// 规则顺序即执行顺序，**不要随意调整**：见文件头第 3 条取舍说明。
var redactRules = []RedactRule{
	// 私钥块必须在最前：它内部含有大量 base64，容易被后面的宽规则切碎留下残余。
	//
	// 这里写成「成对块先于孤立块」的二选一：非贪婪的 .*? 遇上可选的收尾标记时
	// 会直接匹配到空，导致只剩 BEGIN 那一行被替换、后面的密钥正文全文保留——
	// 这是初版实测踩到的坑（测试用例「私钥块」捕获）。
	{
		Kind: "private_key",
		Pattern: regexp.MustCompile(`(?s)` +
			`-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----` +
			`|` +
			`-----BEGIN [A-Z ]*PRIVATE KEY-----[A-Za-z0-9+=/\\\s]+`),
		Replace: "[屏蔽:私钥]",
	},

	// 各类平台密钥。带明确前缀，误伤概率低，因此排在宽规则之前。
	{
		Kind: "api_key",
		Pattern: regexp.MustCompile(`\b(?:` +
			`sk-[A-Za-z0-9_\-]{16,}` + // OpenAI / 兼容端点
			`|sk-ant-[A-Za-z0-9_\-]{16,}` + // Anthropic
			`|LTAI[A-Za-z0-9]{12,}` + // 阿里云 AccessKey ID
			`|gh[pousr]_[A-Za-z0-9]{20,}` + // GitHub 各类 token
			`|github_pat_[A-Za-z0-9_]{20,}` +
			`|AIza[0-9A-Za-z_\-]{30,}` + // Google API Key
			`|xox[baprs]-[A-Za-z0-9\-]{10,}` + // Slack
			`)\b`),
		Replace: "[屏蔽:密钥]",
	},

	// JWT：三段式结构足够独特。
	{
		Kind:    "jwt",
		Pattern: regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\b`),
		Replace: "[屏蔽:令牌]",
	},

	// 凭据赋值句式：password=xxx / "secret": "xxx"。
	// 保留键名是为了让审计仍能看出"这行在配什么"，只挖掉值。
	{
		Kind:    "credential",
		Pattern: regexp.MustCompile(`(?i)(pass(?:word|wd)|pwd|secret|api[_-]?key|access[_-]?token|refresh[_-]?token|authorization|client[_-]?secret)(\s*[:=]\s*)("[^"\n]{1,200}"|'[^'\n]{1,200}'|[^\s,;}"']{1,200})`),
		Replace: "${1}${2}[屏蔽:凭据]",
	},

	{
		Kind:    "email",
		Pattern: regexp.MustCompile(`\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b`),
		Replace: "[屏蔽:邮箱]",
	},

	// 中国身份证 18 位（含末尾 X）。出生年月日段做了基本合法性收敛，降低误伤。
	{
		Kind:    "id_card",
		Pattern: regexp.MustCompile(`\b[1-9]\d{5}(?:19|20)\d{2}(?:0[1-9]|1[0-2])(?:0[1-9]|[12]\d|3[01])\d{3}[\dXx]\b`),
		Replace: "[屏蔽:身份证]",
	},

	// 银行卡号 16-19 位。放在身份证之后、手机号之前。
	{
		Kind:    "bank_card",
		Pattern: regexp.MustCompile(`\b\d{16,19}\b`),
		Replace: "[屏蔽:银行卡]",
	},

	// 中国手机号 11 位。\b 天然卡住两侧，Go 的 RE2 不支持反向预查也不需要它。
	{
		Kind:    "phone",
		Pattern: regexp.MustCompile(`\b1[3-9]\d{9}\b`),
		Replace: "[屏蔽:手机号]",
	},

	// 中国车牌。省份简称 + 发牌机关字母，长度 7-8 位。
	//
	// 首尾刻意不使用 \b：Go 的 RE2 把 \b 定义为 **ASCII 单词边界**，
	// 「京」不是 ASCII 字符，因此 "...停在 京A12345 ..." 里空格与「京」之间
	// 不构成边界，整条规则会静默失效（测试用例「车牌」捕获）。
	// 改用「前一个字符必须是非字母数字或行首」来自己卡边界。
	{
		Kind: "plate",
		Pattern: regexp.MustCompile(`(^|[^0-9A-Za-z])([京津沪渝冀豫云辽黑湘皖鲁新苏浙赣鄂桂甘晋蒙陕吉闽贵粤青藏川宁琼]` +
			`[A-HJ-NP-Z][A-HJ-NP-Z0-9]{4,5}[A-HJ-NP-Z0-9挂学警港澳领])`),
		Replace: "${1}[屏蔽:车牌]",
	},
}

// RedactResult 是一次脱敏的结果。
type RedactResult struct {
	// Text 脱敏后的文本。
	Text string
	// Hits 命中的敏感类型，去重后按字典序排列。
	// 排序而非按发现顺序，是为了让同一内容的脱敏结果稳定可比较
	// （否则规则顺序一变，同一个 forensic 查询前后两次就对不上）。
	Hits []string
	// Count 被替换的片段总数，用于判断"这条对话有多敏感"。
	Count int
	// FullyRedacted 为真表示剥掉占位符后已无任何有效内容。
	FullyRedacted bool
}

// placeholder 判断：占位符形如 [屏蔽:xxx]。
var redactPlaceholder = regexp.MustCompile(`\[屏蔽:[^\]]*\]`)

// Redact 对单段文本执行全规则脱敏。
func Redact(s string) RedactResult {
	if s == "" {
		return RedactResult{Text: s}
	}

	out := s
	seen := make(map[string]struct{})
	var hits []string
	total := 0

	for _, rule := range redactRules {
		found := rule.Pattern.FindAllString(out, -1)
		if len(found) == 0 {
			continue
		}
		out = rule.Pattern.ReplaceAllString(out, rule.Replace)
		total += len(found)
		if _, ok := seen[rule.Kind]; !ok {
			seen[rule.Kind] = struct{}{}
			hits = append(hits, rule.Kind)
		}
	}
	if len(hits) > 1 {
		sort.Strings(hits)
	}

	return RedactResult{
		Text:          out,
		Hits:          hits,
		Count:         total,
		FullyRedacted: noUsefulContent(out),
	}
}

// noUsefulContent 判断剥掉占位符后还剩不剩可读信息。
//
// 之所以多做这一步，是为了区分两种完全不同的情形：
//   - "我的手机号是 13800138000" -> 还剩"我的手机号是"，可保留用于审计 ；
//   - "13800138000"             -> 整条就是一个敏感值，内容应被清空。
//
// 判定标准刻意宽松：只要还剩至少一个字母、数字或汉字，就认为还有信息量。
func noUsefulContent(s string) bool {
	stripped := redactPlaceholder.ReplaceAllString(s, "")
	stripped = strings.TrimSpace(stripped)
	if stripped == "" {
		return true
	}
	for _, r := range stripped {
		switch {
		case r >= '0' && r <= '9':
			return false
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
			return false
		case r >= 0x4e00 && r <= 0x9fff: // CJK 统一表意文字
			return false
		}
	}
	return true
}

// MetadataKeyRedacted 是脱敏类型写入 TaskPayload.Metadata 时使用的键。
const MetadataKeyRedacted = "redacted"

// MetadataKeyFullyRedacted 标记该条内容的全部信息都已被屏蔽。
const MetadataKeyFullyRedacted = "fully_redacted"

// redactApply 把脱敏结果写回 payload 的指定字段，并把类型累积到 metadata。
func redactApply(dst *string, meta map[string]interface{}) {
	res := Redact(*dst)
	if res.Count == 0 {
		return
	}
	*dst = res.Text
	if res.FullyRedacted {
		// 内容整体就是一个敏感值。这里**不丢掉整条记录**——丢掉就连
		// "谁在什么时候用了多少次"的用量维度也没了（那正是 G-3 要的数据）。
		// 只把内容清空，时间、节点、token 数全部保留。
		*dst = "[内容已整体屏蔽]"
		meta[MetadataKeyFullyRedacted] = true
	}
	if cur, ok := meta[MetadataKeyRedacted].(string); ok && cur != "" {
		meta[MetadataKeyRedacted] = cur + "," + strings.Join(res.Hits, ",")
	} else {
		meta[MetadataKeyRedacted] = strings.Join(res.Hits, ",")
	}
}

// RedactPayload 就地对一个待上报载荷执行脱敏。
//
// 只处理 Prompt 与 Result 两个承载自然语言内容的字段；
// token 数、时间戳、节点标识不含敏感内容，原样保留——
// 它们是"用量登记"的唯一依据。
func RedactPayload(p *TaskPayload) {
	if p == nil {
		return
	}
	if p.Metadata == nil {
		p.Metadata = map[string]interface{}{}
	}
	redactApply(&p.Prompt, p.Metadata)
	redactApply(&p.Result, p.Metadata)
}
