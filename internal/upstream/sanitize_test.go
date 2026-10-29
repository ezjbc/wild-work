package upstream

// 出站脱敏回归测试。纯脱敏单元用例 + 逐字节快照护栏从参考项目 workbuddy2api 平移；
// 依赖参考项目私有 API（PrepareBodyOpt / Client.SanitizeFingerprints / 带 ChatMeta 的
// ChatStream 签名）的用例改用本项目对应入口（PrepareBody / testClient.ChatStream），
// 且本项目固定开启脱敏、无开关，故不再有 "disable 保留指纹" 的用例。

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"wild-work/internal/auth"
)

const (
	ccIdentity = "You are Claude Code, Anthropic's official CLI for Claude."
	ccBranch   = "Main branch (you will usually use this for PRs)"
	ccHeader   = "x-anthropic-billing-header: cc_version=1.0; cc_entrypoint=cli;"
	// Codex instructions 首段（上游逐字精确指纹，三句缺一不可）。
	codexInstructions = "You are a coding agent running in the Codex CLI, a terminal-based coding assistant. Codex CLI is an open source project led by OpenAI. You are expected to be precise, safe, and helpful."
	// DSH 身份句：上游对 system 消息**开头**逐字前缀匹配这一串即回 400 code=11-128
	// （放 user role、非开头、或写成 "powered by DeepSeek." 都放行，2026-10-21 单发 A/B 实测）。
	dshIdentity = "You are an AI agent powered by DeepSeek Harness"
	// DSH Web GUI 段：与后半句（"they mean this GUI."）连成 221 字符整段逐字串；单独出现任一半
	// 或截短一字符均放行，只有完整串回 400 code=11-128（2026-10-21 单发 A/B 实测）。
	dshWebGUI = "DeepSeek Harness Web GUI"
)

// DSH 身份句必须被改写：整句替换为 Claude Code 身份句（品牌串从 system 彻底消失，
// 整包兜底才不会二次改写已净化内容；R115 的「插冠词 the」产物仍含品牌串，已废弃）。
func TestDSHIdentityRewritten(t *testing.T) {
	out := sanitizeText(dshIdentity + " You own the repository.")
	if strings.Contains(out, dshIdentity) {
		t.Errorf("DSH identity still present: %q", out)
	}
	if !strings.Contains(out, claudeCodeIdentity) {
		t.Errorf("DSH rewrite missing: %q", out)
	}
	if strings.Contains(strings.ToLower(out), "deepseek harness") {
		t.Errorf("brand string remains (BodyFallback would double-rewrite): %q", out)
	}
	if !strings.Contains(out, " You own the repository.") {
		t.Errorf("suffix lost: %q", out)
	}
}

// DSH 身份句必须经 PrepareBody 落进 system content（真实调用链）。
func TestPrepareBodySanitizesDSHSystem(t *testing.T) {
	body := []byte(`{"model":"glm-5.2","messages":[` +
		`{"role":"system","content":"` + dshIdentity + `"},` +
		`{"role":"user","content":"hi"}]}`)
	var obj map[string]any
	if err := json.Unmarshal(PrepareBody(body), &obj); err != nil {
		t.Fatal(err)
	}
	sys, _ := obj["messages"].([]any)[0].(map[string]any)["content"].(string)
	if strings.Contains(sys, dshIdentity) {
		t.Errorf("DSH fingerprint remains: %q", sys)
	}
	if !strings.Contains(sys, claudeCodeIdentity) {
		t.Errorf("DSH rewrite missing: %q", sys)
	}
}

// Web GUI 身份段必须被改写：缩写为 DSH 即破坏整段逐字前缀，其余措辞原样保留。
func TestDSHWebGuiRewritten(t *testing.T) {
	in := dshWebGUI + ", not the CLI or desktop app. If the user says GUI, they mean this GUI."
	out := sanitizeText(in)
	if strings.Contains(out, dshWebGUI) {
		t.Errorf("DSH Web GUI 指纹仍在: %q", out)
	}
	if !strings.Contains(out, "DSH Web GUI") {
		t.Errorf("DSH Web GUI 改写缺失: %q", out)
	}
	if !strings.Contains(out, ", not the CLI or desktop app.") {
		t.Errorf("后缀被误删: %q", out)
	}
}

// 上游回 400 的是「身份句 + Web GUI 段 + 后缀」组成的整段逐字串，改写后两处指纹都必须消失。
func TestDSHFullPromptBroken(t *testing.T) {
	in := dshIdentity + ". " + dshWebGUI + ", not the CLI or desktop app. " +
		"If the user says gui, they mean this GUI."
	out := sanitizeText(in)
	if strings.Contains(out, dshIdentity) || strings.Contains(out, dshWebGUI) {
		t.Errorf("整段逐字串未被破坏，上游仍会回 11-128: %q", out)
	}
	if !strings.Contains(out, "DSH Web GUI") {
		t.Errorf("Web GUI 改写缺失: %q", out)
	}
	if !strings.Contains(out, "they mean this GUI.") {
		t.Errorf("后半句被误删: %q", out)
	}
}

// 反向护栏：不含 Web GUI 段的文本不得被该规则介入（实测只含身份句的 prompt 单发回 200）。
func TestDSHWebGuiRuleDoesNotLeak(t *testing.T) {
	for _, in := range []string{
		"You use the web GUI to do work.", // 含 web gui 但不是 DSH 品牌短语
		"DeepSeek Harness CLI",            // 含品牌但不是 Web GUI 段
	} {
		if out := sanitizeText(in); out != in {
			t.Errorf("非指纹文本被改动: %q -> %q", in, out)
		}
	}
}

// 反向护栏：措辞相近但非逐字形态不得被改写（防误伤）。
func TestDSHVariantNotTouched(t *testing.T) {
	for _, in := range []string{
		"You are an AI agent, powered by DeepSeek Harness.", // 多一个逗号
		"You are an AI agent powered by DeepSeek.",          // 截去 Harness
		"An AI agent powered by DeepSeek Harness.",          // 缺 "You are an"
	} {
		if out := sanitizeText(in); out != in {
			t.Errorf("variant wrongly rewritten: %q -> %q", in, out)
		}
	}
}

func TestIdentityRewritten(t *testing.T) {
	out := sanitizeText(ccIdentity)
	if !strings.Contains(out, "official CLI tool for Claude.") {
		t.Errorf("identity not rewritten: %q", out)
	}
	if strings.Contains(out, ccIdentity) {
		t.Errorf("original identity still present: %q", out)
	}
}

// 桌面版（claude-desktop-3p / Agent SDK）的身份句以逗号接后继内容，结尾不是句号。
// 回归用例：匹配串曾带结尾句号，导致该形态漏网、指纹原样发上游 → 400 code=11128。
func TestIdentityDesktopVariantRewritten(t *testing.T) {
	in := "You are Claude Code, Anthropic's official CLI for Claude, running within the Claude Agent SDK."
	out := sanitizeText(in)
	if strings.Contains(out, "official CLI for Claude") {
		t.Errorf("desktop identity not rewritten: %q", out)
	}
	if !strings.Contains(out, "official CLI tool for Claude, running within the Claude Agent SDK.") {
		t.Errorf("desktop identity suffix not preserved: %q", out)
	}
}

func TestBranchRewritten(t *testing.T) {
	out := sanitizeText(ccBranch)
	if !strings.Contains(out, "Default branch (you will usually use this for PRs)") {
		t.Errorf("branch not rewritten: %q", out)
	}
	if strings.Contains(out, "Main branch") {
		t.Errorf("original branch still present: %q", out)
	}
}

// 反馈句带 Anthropic 仓库链接，上游按整句拦截（实测只留链接或只留半边均不拦）。
// 回归用例：give→provide 一词之差即可绕过。
func TestFeedbackSentenceRewritten(t *testing.T) {
	in := "To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues"
	out := sanitizeText(in)
	if strings.Contains(out, "To give feedback") {
		t.Errorf("feedback sentence not rewritten: %q", out)
	}
	if !strings.Contains(out, "To provide feedback, users should report the issue at https://github.com/anthropics/claude-code/issues") {
		t.Errorf("feedback sentence not rewritten as expected: %q", out)
	}
}

// 上游反探测：请求体里出现裸数字 11128 即整单拦截（与上下文无关）。
// 回归用例：该串会被改写为 11‑128（U+2011 非断行连字符）以打断精确匹配——
// 目标若是 ASCII "11-128"，与源串 "11-128"（ASCII 31312d313238）同形时
// old==new 空转等于没改（文档第三节）；U+2011（3131e28091313238）渲染一致但字节不同。
func TestUpstreamErrorCodeRewritten(t *testing.T) {
	in := "upstream returned code=11128 for this request"
	out := sanitizeText(in)
	if strings.Contains(out, "11128") {
		t.Errorf("error code not rewritten: %q", out)
	}
	if !strings.Contains(out, "11\u2011128") {
		t.Errorf("error code not rewritten to U+2011 form: %q", out)
	}
}

// 回归：工具调用消息的 content 常为 null，而旧版 sanitizeMessages 在 content 缺失时
// 直接 continue，整条消息连 tool_calls 一起被跳过 → arguments 里的被拦字符串原样漏出。
func TestToolCallArgumentsSanitized(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "user", "content": "run"},
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
			map[string]any{"id": "c1", "type": "function", "function": map[string]any{
				"name":      "Bash",
				"arguments": `{"command":"echo 11128"}`,
			}},
		}},
	}
	if !sanitizeMessages(msgs) {
		t.Fatal("sanitizeMessages 未报告任何改动，tool_calls 被跳过")
	}
	fn := msgs[1].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
	got := fn["arguments"].(string)
	if strings.Contains(got, "11128") {
		t.Errorf("tool_call arguments 未被净化: %q", got)
	}
}

func TestBillingHeaderStrippedValueIrrelevant(t *testing.T) {
	out := sanitizeText(ccHeader)
	if strings.Contains(out, "x-anthropic-billing-header") {
		t.Errorf("header not stripped: %q", out)
	}
}

// 附加验证：正常对话里出现 github.com/anthropics/ 链接（但不是反馈句整句）
// 时，预检特征命中（进入净化），但改写层只动精确匹配的整句——普通链接文本
// 不该被改写。同理，既不含 11128 也不含反馈整句的文本原样返回。
func TestNormalAnthropicLinkNotRewritten(t *testing.T) {
	in := "see https://github.com/anthropics/anthropic-cookbook for examples"
	if out := sanitizeText(in); out != in {
		t.Errorf("normal anthropic link should be untouched: %q -> %q", in, out)
	}
}

// Codex instructions 首段：命中预告且整句改写，逐字指纹被破坏、语义保留。
func TestCodexInstructionsRewritten(t *testing.T) {
	out := sanitizeText(codexInstructions)
	if strings.Contains(out, codexInstructions) {
		t.Errorf("codex fingerprint still present: %q", out)
	}
	if !strings.Contains(out, "You are a coding agent running in the Codex CLI tool, a terminal-based coding assistant.") {
		t.Errorf("codex first sentence not rewritten: %q", out)
	}
	// 其余两句原样保留，语义不变。
	if !strings.Contains(out, "Codex CLI is an open source project led by OpenAI.") ||
		!strings.Contains(out, "You are expected to be precise, safe, and helpful.") {
		t.Errorf("codex remaining sentences altered: %q", out)
	}
}

// 预检必须能认出 Codex 特征（此前只含 Claude Code，导致提前放行）。
func TestCodexFingerprintDetected(t *testing.T) {
	if !hasFingerprint(codexInstructions) {
		t.Error("codex fingerprint not detected by precheck")
	}
}

// 非精确变体不应被改写：仅去掉其中一个词即视为已破坏，无需改动。
func TestCodexVariantNotTouched(t *testing.T) {
	in := "You are a coding agent running in a CLI, a terminal-based coding assistant."
	if out := sanitizeText(in); out != in {
		t.Errorf("already-broken variant should be untouched: %q -> %q", in, out)
	}
}

func TestBillingHeaderCaseInsensitive(t *testing.T) {
	alt := "X-Anthropic-Billing-Header: cc_version=1.0;"
	out := strings.ToLower(sanitizeText(alt))
	if strings.Contains(out, "billing") {
		t.Errorf("case-insensitive header not stripped: %q", out)
	}
}

func TestTrailingKVStripped(t *testing.T) {
	out := sanitizeText("...; cc_version=2.0; cc_entrypoint=cli;")
	if strings.Contains(out, "cc_version") || strings.Contains(out, "cc_entrypoint") {
		t.Errorf("trailing kv not stripped: %q", out)
	}
}

func TestExactMatchOnlyVariantNotTouched(t *testing.T) {
	in := "...official CLI for Claude!"
	if out := sanitizeText(in); out != in {
		t.Errorf("variant should be untouched: %q -> %q", in, out)
	}
}

func TestUserFreeTextNotTouched(t *testing.T) {
	in := "please use main branch for this repo"
	if out := sanitizeText(in); out != in {
		t.Errorf("free text should be untouched: %q -> %q", in, out)
	}
}

func TestNoFeatureReturnsSameString(t *testing.T) {
	in := "ordinary user message"
	if out := sanitizeText(in); out != in {
		t.Errorf("no-feature text should pass through unchanged: %q -> %q", in, out)
	}
}

func TestMultimodalTextPartOnly(t *testing.T) {
	imgPart := map[string]any{"type": "image", "source": map[string]any{"type": "base64", "data": "..."}}
	content := []any{
		map[string]any{"type": "text", "text": ccIdentity},
		imgPart,
	}
	out, changed := sanitizeContent(content)
	if !changed {
		t.Fatal("expected change")
	}
	parts := out.([]any)
	txt, _ := parts[0].(map[string]any)["text"].(string)
	if !strings.Contains(txt, "CLI tool") {
		t.Errorf("text part not sanitized: %q", txt)
	}
	img, _ := parts[1].(map[string]any)
	if img["type"] != "image" || img["source"].(map[string]any)["data"] != "..." {
		t.Error("image part modified")
	}
}

// 出站边界集成：ChatStream 发往上游的 wire body 必须无残留指纹。
func TestChatStreamWireBodySanitized(t *testing.T) {
	var gotBody []byte
	c := testClient(func(r *http.Request) (*http.Response, error) {
		gotBody, _ = io.ReadAll(r.Body)
		return jsonResp(200, "data: [DONE]\n\n"), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1", EnterpriseID: "e1"}

	body := []byte(`{"model":"glm-5.2","messages":[` +
		`{"role":"system","content":"` + ccIdentity + ` ` + ccHeader + `"},` +
		`{"role":"user","content":"hi"}]}`)
	rc, status, respBody, err := c.ChatStream(a, body)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if status >= 400 {
		t.Fatalf("upstream status %d: %s", status, respBody)
	}
	// 上游收到的 body：stream 强制 + 指纹已净化
	var obj map[string]any
	if err := json.Unmarshal(gotBody, &obj); err != nil {
		t.Fatalf("wire body not json: %v", err)
	}
	if obj["stream"] != true {
		t.Error("wire body stream not forced")
	}
	sys, _ := obj["messages"].([]any)[0].(map[string]any)["content"].(string)
	for _, fp := range []string{"x-anthropic-billing-header", ccIdentity, ccBranch} {
		if strings.Contains(sys, fp) {
			t.Errorf("wire body contains fingerprint %q: %q", fp, sys)
		}
	}
}

// TestBareHeaderAbbreviated 裸键名（无冒号）兜底缩写——2026-09-13 实验 F4：
// assistant 消息反引号引用裸键名即触发 11128，剥离层 sanitizeHdrRe 要求冒号、
// 对裸串无效。键值形态被整段删除后，残留裸键名做最小缩写（header→hdr），
// 破坏逐字匹配、语义不变、保留可读性。
func TestBareHeaderAbbreviated(t *testing.T) {
	for _, in := range []string{
		"引用 `x-anthropic-billing-header` 这个键",
		"lower: x-anthropic-billing-header",
		"mixed: X-Anthropic-Billing-Header",
	} {
		out := sanitizeText(in)
		if strings.Contains(strings.ToLower(out), "x-anthropic-billing-header") {
			t.Fatalf("裸键名未被兜底: in=%q out=%q", in, out)
		}
		if !strings.Contains(strings.ToLower(out), "x-anthropic-billing-hdr") {
			t.Fatalf("裸键名未缩写为 hdr 形态: in=%q out=%q", in, out)
		}
	}
	// 键值形态仍走整段删除（不留 hdr 残骸）
	out := sanitizeText("prefix x-anthropic-billing-header: cc_version=1.0; cc_entrypoint=cli; suffix")
	if strings.Contains(strings.ToLower(out), "x-anthropic-billing") {
		t.Fatalf("键值形态应整段删除: out=%q", out)
	}
}

// TestReasoningContentSanitized reasoning_content（思维链回填字段）与 content
// 同等净化——实测该字段同样携带指纹。
func TestReasoningContentSanitized(t *testing.T) {
	msgs := []any{
		map[string]any{
			"role":              "assistant",
			"content":           nil,
			"reasoning_content": "上文出现过 `x-anthropic-billing-header` 键名",
		},
	}
	if !sanitizeMessages(msgs) {
		t.Fatal("reasoning_content 中的指纹未被净化")
	}
	rc := msgs[0].(map[string]any)["reasoning_content"].(string)
	if strings.Contains(rc, "x-anthropic-billing-header") {
		t.Fatalf("reasoning_content 指纹残留: %q", rc)
	}
}

// 集成：完整请求体经 PrepareBody 净化后无残留指纹，且 stream/tool_choice 行为不受影响。
func TestPrepareBodySanitizesSystem(t *testing.T) {
	body := []byte(`{"model":"glm-5.2","messages":[` +
		`{"role":"system","content":"` + ccIdentity + ` ` + ccHeader + `"},` +
		`{"role":"user","content":"hi"}]}`)
	out := PrepareBody(body)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["stream"] != true {
		t.Error("stream not forced")
	}
	msgs := obj["messages"].([]any)
	sys, _ := msgs[0].(map[string]any)["content"].(string)
	if strings.Contains(sys, "x-anthropic-billing-header") || strings.Contains(sys, ccIdentity) || strings.Contains(sys, ccBranch) {
		t.Errorf("fingerprints remain: %q", sys)
	}
	if !strings.Contains(sys, "CLI tool") {
		t.Errorf("rewrite missing: %q", sys)
	}
}

// TestSanitizeLiteralByteExact 逐字节快照护栏：把 sanitizeFeatures 13 项 +
// sanitizeRewrites 7 对 + 6 个正则的当前字节值硬编码断言。
// 这些字面量是实验逆向出的上游内容审核黑名单（无契约可引用），上游按逐字精确
// 匹配拦截，一字之差即漏拦（400 code=11128）或误伤。任何未来改动（含看似无害的
// 统一常量/抽配置）都会先红在本测试——必须走 analysis 报告 #6 的逐字节验证
// 步骤：grep 全部出现点（表/正则/测试断言四处同查）+ 真实账号上游实测 +
// TestSanitize 全族回归，方可同步更新本快照。
func TestSanitizeLiteralByteExact(t *testing.T) {
	// 全小写表（上游判据大小写不敏感，2026-10-28 真机实测小写变体同样 400），
	// 配合 asciiContainsFold 折叠匹配。
	features := []string{
		"x-anthropic-billing-hdr",
		"x-anthropic-billing-header", // header 键值段键名
		"cc_entrypoint=",             // 尾随裸键值（截断前缀即可命中）
		"you are claude code",        // 身份句（截断前缀即可命中）
		"main branch (",              // 注入指令句（截断前缀即可命中）
		"you are a coding agent running in the codex cli", // Codex instructions 首段（截断前缀即可命中）
		"github.com/anthropics/",                          // 反馈句里的 Anthropic 仓库链接
		"11-128",
		"11128", // 上游反探测：裸数字错误码
		// DSH 身份句：上游按 system 开头逐字锚定（2026-10-21 实测）
		"you are an ai agent powered by deepseek harness",
		// DSH Web GUI 段：整段 221 字符逐字串的前半段（2026-10-21 实测）
		"deepseek harness web gui",
		// DSH 品牌串（超集，兜住 "DeepSeek Harness CLI" 等变体触发预检）
		"deepseek harness",
		// DSH @ 路径句（跨句指纹后半，2026-10-28 实测）
		"relative paths resolve from the workspace root",
	}
	if len(sanitizeFeatures) != len(features) {
		t.Fatalf("sanitizeFeatures 项数=%d want %d（快照与实现不同步，见测试头注释的验证步骤）", len(sanitizeFeatures), len(features))
	}
	for i, want := range features {
		if sanitizeFeatures[i] != want {
			t.Errorf("sanitizeFeatures[%d]=%q want %q（逐字节不一致，改动前先走快照验证流程）", i, sanitizeFeatures[i], want)
		}
	}

	rewrites := [][2]string{
		{
			"You are Claude Code, Anthropic's official CLI for Claude",
			"You are Claude Code, Anthropic's official CLI tool for Claude",
		},
		{
			"Main branch (you will usually use this for PRs)",
			"Default branch (you will usually use this for PRs)",
		},
		{
			"You are a coding agent running in the Codex CLI, a terminal-based coding assistant.",
			"You are a coding agent running in the Codex CLI tool, a terminal-based coding assistant.",
		},
		{
			"To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
			"To provide feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
		},
		{
			// 上游反探测：裸数字 11128 即拦。目标用 U+2011（\u2011，非断行连字符）：
			// 渲染同普通连字符但字节不同（3131e28091313238），才真正打断逐字匹配——
			// ASCII "11-128" 与源串同形时 old==new 空转。
			"11128",
			"11\u2011128",
		},
		{
			// DSH 身份句：精确改写先插冠词 the（R115 原样），随后 dshIdentityRe
			// 二次命中整句替换为 claudeCodeIdentity（品牌串从 system 彻底消失）。
			"You are an AI agent powered by DeepSeek Harness",
			"You are an AI agent powered by the DeepSeek Harness",
		},
		{
			// DSH Web GUI：缩写为 DSH 即破坏整段逐字前缀，语义不变（实测 200）
			"DeepSeek Harness Web GUI",
			"DSH Web GUI",
		},
	}
	if len(sanitizeRewrites) != len(rewrites) {
		t.Fatalf("sanitizeRewrites 对数=%d want %d（快照与实现不同步，见测试头注释的验证步骤）", len(sanitizeRewrites), len(rewrites))
	}
	for i, want := range rewrites {
		if sanitizeRewrites[i][0] != want[0] || sanitizeRewrites[i][1] != want[1] {
			t.Errorf("sanitizeRewrites[%d]=%q→%q want %q→%q（逐字节不一致，改动前先走快照验证流程）",
				i, sanitizeRewrites[i][0], sanitizeRewrites[i][1], want[0], want[1])
		}
	}

	regexes := map[string]string{
		// 宽松键名正则（hdrKeyRe）：h/e/a/d/e/r 逐字母容忍分隔，header 全形态与
		// hdr 缩写形态都命中；直接写 `hdr` 会漏 "header"（不含连续 hdr）。
		"sanitizeHdrRe":     `(?i)x[^a-z]*anthropic[^a-z]*billing[^a-z]*h[^a-z]*(?:e[^a-z]*)?(?:a[^a-z]*)?d[^a-z]*(?:e[^a-z]*)?r\s*:[^;\n]*;?\s*`,
		"sanitizeBareHdrRe": `(?i)x[^a-z]*anthropic[^a-z]*billing[^a-z]*h[^a-z]*(?:e[^a-z]*)?(?:a[^a-z]*)?d[^a-z]*(?:e[^a-z]*)?r`,
		"sanitizeKvRe":      `(?i)\bcc_[a-z0-9_]+=[^;\n]*;?\s*`,
		// DSH 三正则：容忍 the 冠词 / [\s\-_]* 分隔符 / 大小写变体（doc 4.3 节）。
		"dshIdentityRe": `(?i)you\s+are\s+an\s+ai\s+agent\s+powered\s+by\s+(?:the\s+)?deepseek[\s\-_]*harness`,
		"dshWebGuiRe":   `(?i)deepseek[\s\-_]*harness\s+web\s+gui`,
		"acpPathRe":     `(?i)relative\s+paths\s+resolve\s+from\s+the\s+workspace\s+root`,
	}
	for name, want := range regexes {
		var got string
		switch name {
		case "sanitizeHdrRe":
			got = sanitizeHdrRe.String()
		case "sanitizeBareHdrRe":
			got = sanitizeBareHdrRe.String()
		case "sanitizeKvRe":
			got = sanitizeKvRe.String()
		case "dshIdentityRe":
			got = dshIdentityRe.String()
		case "dshWebGuiRe":
			got = dshWebGuiRe.String()
		case "acpPathRe":
			got = acpPathRe.String()
		}
		if got != want {
			t.Errorf("%s=%q want %q（逐字节不一致，改动前先走快照验证流程）", name, got, want)
		}
	}
}

// 第三条跨句 @ 路径指纹（2026-10-28 真机实测：t_join 跨句连写回 400、单句各自回 200、
// 插冠词 The 回 200）：后半句必须被改写，前半句单独出现不是指纹、不得改动。
func TestACPPathCrossSentenceRewritten(t *testing.T) {
	in := "Tokens prefixed with @ are paths the user explicitly referenced. " +
		"Relative paths resolve from the workspace root; anything else is a file: use the read tool."
	out := sanitizeText(in)
	if strings.Contains(out, "Relative paths resolve from the workspace root") {
		t.Errorf("@ 路径句跨句指纹未被改写: %q", out)
	}
	if !strings.Contains(out, "The relative paths resolve from the workspace root") {
		t.Errorf("@ 路径句改写缺失: %q", out)
	}
	// 前半句单独出现：不是指纹（上游按跨句连写锚定），零改动。
	if got := sanitizeText("Tokens prefixed with @ are paths the user explicitly referenced."); got != "Tokens prefixed with @ are paths the user explicitly referenced." {
		t.Errorf("前半句被误改: %q", got)
	}
}

// 上游大小写不敏感（2026-10-28 真机实测：全小写身份句回 400）：预检与改写都必须认出全小写变体。
func TestLowercaseIdentityRewritten(t *testing.T) {
	in := "you are an ai agent powered by deepseek harness. 你是助手。"
	out := sanitizeText(in)
	if strings.Contains(strings.ToLower(out), "deepseek harness") {
		t.Errorf("小写变体未被改写（上游大小写不敏感，仍会 400）: %q", out)
	}
	if !strings.Contains(out, claudeCodeIdentity) {
		t.Errorf("小写身份句未整句替换: %q", out)
	}
}

// 整包兜底（BodyFallback 接线层）：结构化清洗只覆盖 messages 的
// content/reasoning_content/tool_calls.arguments，tools[].function.description 等
// 字段摸不到——上游对任意位置的 DSH 品牌串都按逐字指纹拦截，最终 wire body 里
// 残留的品牌串必须被兜底替换（2026-10-28 文档第五节实测）。
func TestBodyFallbackCoversUntouchedFields(t *testing.T) {
	body := []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],` +
		`"tools":[{"type":"function","function":{"name":"f","description":"Powered by DeepSeek Harness"}}]}`)
	out := PrepareBody(body)
	if strings.Contains(string(out), "DeepSeek Harness") {
		t.Errorf("整包兜底未命中 tools[].function.description: %q", out)
	}
}
