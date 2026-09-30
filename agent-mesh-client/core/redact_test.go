package core

import (
	"strings"
	"testing"
)

// G-2 的验收标准是「构造含敏感信息的样本全部被替换并有 redacted 标记」，
// 所以这里的用例用贴近真实形态的数据，而不是 aaaa/bbbb 这类玩具串。
//
// 注意：所有样本一律是**合成数据**。
// 测试脱敏代码时很容易顺手从本机剪一段真实值贴进来，但那等于把真实凭据写进 git 历史，
// 之后要改史才能清除。这里刻意保持样本一看就是假的。

// syntheticPEM 组装成一个形态完整但内容是假的私钥块。
// 拆成片段是为了避免编辑器/扫描器把完整 PEM 头当成真密钥告警。
func syntheticPEM() string {
	hdr := "-----BEGIN " + "PRIVATE KEY-----"
	ftr := "-----END " + "PRIVATE KEY-----"
	// body 是 "SYNTHETIC-BODY-NOT-A-REAL-KEY" 的 base64，解码后自证为假
	body := "U1lOVEhFVElDLUJPRFktTk9ULUEtUkVBTC1LRVk="
	return hdr + "\n" + body + "\n" + ftr
}

func TestRedactSensitivePatterns(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantNot []string // 这些原文片段必须消失
		wantHit string   // 必须命中这个类型
	}{
		{
			name:    "手机号",
			input:   "帮我订张票，联系我 13812345678，谢谢",
			wantNot: []string{"13812345678"},
			wantHit: "phone",
		},
		{
			name:    "身份证",
			input:   "员工身份证号 11010519491231002X 请核对",
			wantNot: []string{"11010519491231002X"},
			wantHit: "id_card",
		},
		{
			name:    "银行卡",
			input:   "工资卡号 6222020200012345678",
			wantNot: []string{"6222020200012345678"},
			wantHit: "bank_card",
		},
		{
			name:    "邮箱",
			input:   "发给 zhang@example-corp.com.cn 一份",
			wantNot: []string{"zhang@example-corp.com.cn"},
			wantHit: "email",
		},
		{
			name:    "OpenAI 风格密钥",
			input:   "api 用 sk-EXAMPLEonly0000000000 这个",
			wantNot: []string{"sk-EXAMPLEonly0000000000"},
			wantHit: "api_key",
		},
		{
			name:    "阿里云风格 AccessKey",
			input:   "LTAIEXAMPLE0000000a",
			wantNot: []string{"LTAIEXAMPLE0000000a"},
			wantHit: "api_key",
		},
		{
			name:    "JWT",
			input:   "token 是 eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJFWEFNUExFIn0.SYNTHETICsignature0",
			wantNot: []string{"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJFWEFNUExFIn0.SYNTHETICsignature0"},
			wantHit: "jwt",
		},
		{
			name:    "私钥块",
			input:   syntheticPEM(),
			wantNot: []string{"U1lOVEhFVElDLUJPRFktTk9ULUEtUkVBTC1LRVk="},
			wantHit: "private_key",
		},
		{
			name:    "只有开头没有结尾的私钥块",
			input:   "-----BEGIN " + "PRIVATE KEY-----\nU1lOVEhFVElDLUJPRFktTk9ULUEtUkVBTC1LRVk=",
			wantNot: []string{"U1lOVEhFVElDLUJPRFktTk9ULUEtUkVBTC1LRVk="},
			wantHit: "private_key",
		},
		{
			name:    "密码赋值",
			input:   `配置里写的是 password = "MyExample!Pass" 别外传`,
			wantNot: []string{"MyExample!Pass"},
			wantHit: "credential",
		},
		{
			name:    "车牌",
			input:   "公司车停在 京A12345 那个位",
			wantNot: []string{"京A12345"},
			wantHit: "plate",
		},
		{
			name:    "车牌紧贴中文（无空格）",
			input:   "停在粤B12C34那里",
			wantNot: []string{"粤B12C34"},
			wantHit: "plate",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Redact(tc.input)
			for _, frag := range tc.wantNot {
				if strings.Contains(got.Text, frag) {
					t.Errorf("敏感原文未被替换，仍包含 %q\n结果=%q", frag, got.Text)
				}
			}
			if !containsHit(got.Hits, tc.wantHit) {
				t.Errorf("未标记 redacted 类型 %q，实际=%v", tc.wantHit, got.Hits)
			}
			if got.Count == 0 {
				t.Errorf("命中数为 0，规则显然没生效")
			}
		})
	}
}

func TestRedactKeepsContext(t *testing.T) {
	// 脱敏不是删除：上下文要留下来，否则审计看不出这条对话在说什么。
	got := Redact("帮我订张票，联系我 13812345678，谢谢")
	if !strings.Contains(got.Text, "帮我订张票") || !strings.Contains(got.Text, "谢谢") {
		t.Errorf("上下文被误删：%q", got.Text)
	}
	if got.FullyRedacted {
		t.Errorf("仍有可读上下文，不应判定为整体屏蔽")
	}
}

func TestRedactFullyRedacted(t *testing.T) {
	// 整条内容就是一个敏感值 -> 剥掉后无有效信息。
	got := Redact("13812345678")
	if !got.FullyRedacted {
		t.Errorf("期望判定为 FullyRedacted，实际=%+v", got)
	}
}

func TestRedactIdCardBeforeBankCard(t *testing.T) {
	// 规则顺序的回归锁：18 位身份证必须被判成 id_card，
	// 而不是被更早的银行卡规则（16-19 位）抢先吸收。
	got := Redact("11010519491231002X")
	if !containsHit(got.Hits, "id_card") {
		t.Errorf("身份证被误判，实际=%v", got.Hits)
	}
	if containsHit(got.Hits, "bank_card") {
		t.Errorf("不应同时被判成银行卡：%v", got.Hits)
	}
}

func TestRedactNoFalsePositiveOnNormalText(t *testing.T) {
	// 普通工作对话不该被误伤，否则审计数据会变成一堆占位符，失去价值。
	inputs := []string{
		"帮我把这份周报总结一下，重点写上季度完成的三件事",
		"这个需求排到下个迭代，先做登录，注册往后放",
		"用 Python 写个脚本，把 2026 年的月度报表合并成一个表",
		"对比一下 MySQL 和 PostgreSQL 的索引差异",
	}
	for _, in := range inputs {
		got := Redact(in)
		if got.Text != in {
			t.Errorf("正常文本被误改：\n原=%q\n后=%q", in, got.Text)
		}
		if got.Count != 0 {
			t.Errorf("正常文本不应命中任何规则：%v", got.Hits)
		}
	}
}

func TestRedactPayload(t *testing.T) {
	p := &TaskPayload{
		TaskID:       "t1",
		Prompt:       "客户的手机号是 13900001111",
		Result:       "已记录，另外他的邮箱 abc@corp.com 也在备注里",
		InputTokens:  12,
		OutputTokens: 34,
	}

	RedactPayload(p)

	if strings.Contains(p.Prompt, "13900001111") {
		t.Errorf("prompt 未脱敏：%q", p.Prompt)
	}
	if strings.Contains(p.Result, "abc@corp.com") {
		t.Errorf("result 未脱敏：%q", p.Result)
	}
	meta, _ := p.Metadata[MetadataKeyRedacted].(string)
	if meta == "" {
		t.Fatalf("metadata.redacted 为空，服务端无法知道这条被脱敏过")
	}
	if !strings.Contains(meta, "phone") || !strings.Contains(meta, "email") {
		t.Errorf("metadata.redacted 应同时包含 phone 与 email，实际=%q", meta)
	}
	// 用量维度必须原样保留——那是 G-3 唯一的依据。
	if p.InputTokens != 12 || p.OutputTokens != 34 {
		t.Errorf("token 数被意外改动：%d/%d", p.InputTokens, p.OutputTokens)
	}
}

func TestRedactPayloadFullyRedactedKeepsUsage(t *testing.T) {
	p := &TaskPayload{
		TaskID:       "t2",
		Prompt:       "13812345678",
		InputTokens:  7,
		OutputTokens: 9,
	}
	RedactPayload(p)
	if _, ok := p.Metadata[MetadataKeyFullyRedacted].(bool); !ok {
		t.Errorf("应标记 fully_redacted")
	}
	if strings.Contains(p.Prompt, "13812345678") {
		t.Errorf("整条敏感值应被清空：%q", p.Prompt)
	}
	if p.InputTokens != 7 || p.OutputTokens != 9 {
		t.Errorf("整体屏蔽时也必须保住用量数据")
	}
}

func TestRedactEmptyAndNil(t *testing.T) {
	if got := Redact(""); got.Text != "" || got.Count != 0 {
		t.Errorf("空串应原样返回：%+v", got)
	}
	var p *TaskPayload
	RedactPayload(p) // 不应 panic
}

func containsHit(hits []string, want string) bool {
	for _, h := range hits {
		if h == want {
			return true
		}
	}
	return false
}
