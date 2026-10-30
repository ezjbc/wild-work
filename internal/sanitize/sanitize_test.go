package sanitize

// 生产路径脱敏表的回归护栏。
//
// 背景（R115 / PR #185）：脱敏表在本仓有**两份实现**——本包（`upstream.PrepareBody` 与
// `workbuddyai` 都调它）与 `internal/upstream/sanitize.go` 的包内平行实现。历史上两次
// 「改了上游没改这里」都靠人肉同步发现，本文件把两件事锁死：
//  1. 本包表的逐字节内容快照（与 upstream 侧快照同版本号，改一处必两处同时红）；
//  2. 与 `upstream.PrepareBody` 的**行为一致性**（同一段指纹文本，两路净化结果必须逐字相同）。
//
// 判据层级：身份句/整段逐字串是 2026-10-21 单发 A/B 实测；「上游按连续逐字前缀匹配」
// 是实测结论，不是关键词或语义审核——所以改写只要能破坏逐字串即可，措辞尽量原样保留。

import (
	"encoding/json"
	"strings"
	"testing"
)

const (
	// DSH 身份句：上游对 system 消息**开头**逐字前缀匹配即回 400 code=11-128。
	dshIdentity = "You are an AI agent powered by DeepSeek Harness"
	// DSH Web GUI 段：与后半句连成 221 字符整段逐字串才拦，单句放行。
	dshWebGUI = "DeepSeek Harness Web GUI"
)

// 本包 featHeaders 的逐字节快照（13 项，全小写——上游判据大小写不敏感，
// 2026-10-28 真机实测全小写身份句同样 400，配合 asciiContainsFold 折叠匹配）。
var wantFeatures = []string{
	"x-anthropic-billing-hdr",
	"x-anthropic-billing-header", // header 键值段键名
	"cc_entrypoint=",             // 尾随裸键值（截断前缀即可命中）
	"you are claude code",        // 身份句（截断前缀即可命中）
	"main branch (",              // 注入指令句（截断前缀即可命中）
	"you are a coding agent running in the codex cli", // Codex instructions 首段（截断前缀即可命中）
	"github.com/anthropics/",                          // 反馈句里的 Anthropic 仓库链接
	"11-128",
	"11128", // 上游反探测：裸数字错误码
	"you are an ai agent powered by deepseek harness", // DSH 身份句：上游按 system 开头逐字锚定（2026-10-21 实测）
	"deepseek harness web gui",                        // DSH Web GUI 段：整段 221 字符逐字串前半（2026-10-21 实测）
	"deepseek harness",                                // DSH 品牌串（超集，兜住 "DeepSeek Harness CLI" 等变体触发预检）
	"relative paths resolve from the workspace root",  // DSH @ 路径句（跨句指纹后半，2026-10-28 实测）
}

// 本包 rewrites 的逐字节快照（7 对）。
var wantRewrites = [][2]string{
	{"You are Claude Code, Anthropic's official CLI for Claude",
		"You are Claude Code, Anthropic's official CLI tool for Claude"},
	{"Main branch (you will usually use this for PRs)",
		"Default branch (you will usually use this for PRs)"},
	{"You are a coding agent running in the Codex CLI, a terminal-based coding assistant.",
		"You are a coding agent running in the Codex CLI tool, a terminal-based coding assistant."},
	{"To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
		"To provide feedback, users should report the issue at https://github.com/anthropics/claude-code/issues"},
	{"11128", "11\u2011128"},
	{dshIdentity, "You are an AI agent powered by the DeepSeek Harness"},
	{dshWebGUI, "DSH Web GUI"},
}

func TestTablesByteExactSnapshot(t *testing.T) {
	if len(featHeaders) != len(wantFeatures) {
		t.Fatalf("featHeaders 项数=%d want %d（改表必须同步本快照 + upstream 侧快照）",
			len(featHeaders), len(wantFeatures))
	}
	for i, want := range wantFeatures {
		if featHeaders[i] != want {
			t.Errorf("featHeaders[%d]=%q want %q", i, featHeaders[i], want)
		}
	}
	if len(rewrites) != len(wantRewrites) {
		t.Fatalf("rewrites 对数=%d want %d", len(rewrites), len(wantRewrites))
	}
	for i, want := range wantRewrites {
		if rewrites[i][0] != want[0] || rewrites[i][1] != want[1] {
			t.Errorf("rewrites[%d]=%q→%q want %q→%q", i,
				rewrites[i][0], rewrites[i][1], want[0], want[1])
		}
	}
}

// 整段逐字串（身份句 + Web GUI 段 + 后缀）必须被完全破坏，且非指纹部分保留。
func TestDSHFullPromptSanitizedViaMessages(t *testing.T) {
	in := dshIdentity + ". " + dshWebGUI + ", not the CLI or desktop app. If the user says gui, they mean this GUI."
	out := string(Messages(bodyWithSystem(t, in)))
	for _, fp := range []string{dshIdentity, dshWebGUI} {
		if strings.Contains(out, fp) {
			t.Fatalf("指纹残留 %q：%q", fp, out)
		}
	}
	if !strings.Contains(out, "DSH Web GUI") {
		t.Errorf("Web GUI 改写缺失: %q", out)
	}
	if !strings.Contains(out, claudeCodeIdentity) {
		t.Errorf("身份句改写缺失（应整句替换为 Claude Code 身份句）: %q", out)
	}
	if strings.Contains(strings.ToLower(out), "deepseek harness") {
		t.Errorf("品牌串残留（BodyFallback 会二次改写它，说明两条规则打架）: %q", out)
	}
	if !strings.Contains(out, "they mean this GUI.") {
		t.Errorf("非指纹措辞被误删: %q", out)
	}
}

// 反向护栏：措辞相近但不是逐字形态的文本一律不得改动（防误伤用户内容）。
func TestDSHVariantsUntouched(t *testing.T) {
	for _, in := range []string{
		"You are an AI agent, powered by DeepSeek Harness.", // 多一个逗号
		"You are an AI agent powered by DeepSeek.",          // 截去 Harness
		"An AI agent powered by DeepSeek Harness.",          // 缺 "You are an"
		"DeepSeek Harness CLI",                              // 含品牌但不是 Web GUI 段
		"You use the web GUI to do work.",                   // 含 web gui 但不是 DSH 短语
	} {
		if got := systemOf(t, Messages(bodyWithSystem(t, in))); got != in {
			t.Errorf("变体被误改: %q -> %q", in, got)
		}
	}
}

// 无指纹请求必须零改动（同一切片返回，不重新序列化）。
func TestNoFingerprintSameBytes(t *testing.T) {
	body := bodyWithSystem(t, "帮我看下这段日志。")
	if got := Messages(body); string(got) != string(body) {
		t.Errorf("无指纹请求被改写: %s", got)
	}
}

// 第三条跨句 @ 路径指纹（2026-10-28 真机实测：t_join 跨句连写回 400、单句各自回 200、
// 插冠词 The 回 200）：后半句必须被改写，前半句单独出现不是指纹、不得改动。
func TestACPCrossSentenceFingerprintSanitized(t *testing.T) {
	in := "Tokens prefixed with @ are paths the user explicitly referenced. " +
		"Relative paths resolve from the workspace root; anything else is a file: use the read tool."
	out := string(Messages(bodyWithSystem(t, in)))
	if strings.Contains(out, "Relative paths resolve from the workspace root") {
		t.Fatalf("@ 路径句跨句指纹未被改写: %q", out)
	}
	if !strings.Contains(out, "The relative paths resolve from the workspace root") {
		t.Errorf("@ 路径句改写缺失: %q", out)
	}
	// 前半句单独出现：不是指纹（上游按跨句连写锚定），零改动。
	if got := systemOf(t, Messages(bodyWithSystem(t, "Tokens prefixed with @ are paths the user explicitly referenced."))); got != "Tokens prefixed with @ are paths the user explicitly referenced." {
		t.Errorf("前半句被误改: %q", got)
	}
}

// 上游大小写不敏感（2026-10-28 真机实测：全小写身份句回 400）：预检与改写必须认出全小写变体。
func TestLowercaseIdentitySanitized(t *testing.T) {
	in := "you are an ai agent powered by deepseek harness. 你是助手。"
	out := systemOf(t, Messages(bodyWithSystem(t, in)))
	if strings.Contains(strings.ToLower(out), "deepseek harness") {
		t.Fatalf("小写变体未被改写（上游大小写不敏感，仍会 400）: %q", out)
	}
	if !strings.Contains(out, claudeCodeIdentity) {
		t.Errorf("小写身份句未整句替换: %q", out)
	}
}

// BodyFallback 整包兜底：Messages 只动 messages 的结构化字段，metadata/tools
// 等字段摸不到——上游对任意位置的 DSH 品牌串都按逐字指纹拦截，最终 body 里
// 残留的品牌串必须被兜底替换（2026-10-28 文档第五节实测）。
func TestBodyFallbackReplacesBrand(t *testing.T) {
	in := []byte(`{"metadata":{"user_id":"Powered by DeepSeek Harness"}}`)
	out := BodyFallback(in)
	if strings.Contains(string(out), "DeepSeek Harness") {
		t.Fatalf("整包兜底未命中: %s", out)
	}
	if !strings.Contains(string(out), "Claude Code") {
		t.Errorf("兜底替换目标缺失: %s", out)
	}
	// 大小写不敏感 + 分隔符变体（上游判据）。
	out2 := BodyFallback([]byte("powered by DEEPSEEK-HARNESS engine"))
	if strings.Contains(strings.ToLower(string(out2)), "deepseek") {
		t.Errorf("大小写/分隔符变体未被兜底: %s", out2)
	}
}

// 无品牌串的 body 必须零改动（不重编码：保持原字节/原空格，零分配路径）。
func TestBodyFallbackNoOpWithoutBrand(t *testing.T) {
	in := []byte(`{ "model" : "m" , "messages" : [ ] }`)
	out := BodyFallback(in)
	if string(out) != string(in) {
		t.Errorf("无品牌 body 被重编码（原样返回被破坏）: %s", out)
	}
}

// SystemHead：取首条 system/developer 的 content 开头（供 diag 日志），rune 截断。
func TestSystemHead(t *testing.T) {
	b, err := json.Marshal(map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": "u"},
		map[string]any{"role": "system", "content": "You are an AI agent powered by DeepSeek Harness. 你是助手。"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if head := SystemHead(b, 20); head != "You are an AI agent" {
		t.Errorf("SystemHead=%q want %q", head, "You are an AI agent")
	}
	// rune 截断：中文按 rune 计，不切坏 UTF-8。
	b2, err := json.Marshal(map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": "你好，世界。这是系统提示。"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := SystemHead(b2, 2); got != "你好" {
		t.Errorf("SystemHead rune 截断=%q want %q", got, "你好")
	}
}

func bodyWithSystem(t *testing.T, sys string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"model": "m", "messages": []any{
		map[string]any{"role": "system", "content": sys},
		map[string]any{"role": "user", "content": "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func systemOf(t *testing.T, body []byte) string {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, body)
	}
	ms, _ := obj["messages"].([]any)
	if len(ms) == 0 {
		t.Fatalf("无 messages: %s", body)
	}
	s, _ := ms[0].(map[string]any)["content"].(string)
	return s
}
