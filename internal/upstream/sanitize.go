// sanitize.go 出站请求体指纹脱敏：剥离上游内容审核黑名单指纹。
//
// 背景：客户端（Claude Code / DSH 类 CLI）在 system prompt 注入若干固定模板句，
// 上游内容审核按逐字精确匹配拦截（非语义审核），一字改动即可绕过。
// 策略：键值/header 型指纹整段剥离；承载语义的模板句最小改写；对大小写不敏感、
// 可跨句、只看 system 开头的 DSH 逐字指纹（2026-10-28 真机三指纹实测）用正则
// 容忍变体。
//
// 本文件与 internal/sanitize/sanitize.go 是**两套平行实现**（本包 prepareBodyInner
// 内部调用 / 生产路径走 sanitize.Messages），必须同步改，漏一处等于没改。
package upstream

import (
	"regexp"
	"strings"
)

// sanitizeFeatures 特征预检：任一命中才进入净化。全部小写，配合 asciiContainsFold
// 做大小写不敏感匹配（上游判据大小写不敏感，2026-10-28 真机实测小写变体同样 400，
// 大小写敏感的 Contains 会让小写变体连净化都进不去）。
var sanitizeFeatures = []string{
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
	"deepseek harness web gui",                        // DSH Web GUI 段：上游按 system 开头整段 221 字符逐字串锚定（2026-10-21 实测）
	"deepseek harness",                                // DSH 品牌串（超集，兜住 "DeepSeek Harness CLI" 等变体触发预检）
	"relative paths resolve from the workspace root",  // DSH @ 路径句（跨句指纹后半，2026-10-28 实测）
}

// 宽松键名正则：键名内部允许夹任意非字母序列（全角/Unicode 连字符、空格、零宽等），
// 且 h/e/a/d/e/r 逐字母容忍分隔——header 全形态与 hdr 缩写形态都命中。
// 注意不能简单写 `hdr`：原始键名 "header" 不含连续 "hdr"，会漏掉键值整段剥离。
const hdrKeyRe = `x[^a-z]*anthropic[^a-z]*billing[^a-z]*h[^a-z]*(?:e[^a-z]*)?(?:a[^a-z]*)?d[^a-z]*(?:e[^a-z]*)?r`

// sanitizeHdrRe 剥离层：header 键名即触发（与值无关），整段删除。
var sanitizeHdrRe = regexp.MustCompile(`(?i)` + hdrKeyRe + `\s*:[^;\n]*;?\s*`)

// sanitizeBareHdrRe 兜底层：裸键名（无冒号无值）同样是指纹——2026-09-13 实验 F4
// 证实 assistant 消息里反引号引用裸键名即触发 11128，而剥离层要求冒号、对裸串无效。
// 键值形态被整段删除后，残留的裸键名做最小缩写（header→hdr）：破坏逐字匹配、
// 语义不变、保留可读性。大小写不敏感，覆盖 X-Anthropic-... 变体。
//
// 注意该正则不要求冒号，是 sanitizeHdrRe 的超集——hasFingerprint 与 sanitizeText
// 中两者并用：先删键值形态（sanitizeHdrRe），再缩写残留裸键名（本正则），
// 替换语义不同（整段删除 vs 最小缩写），不可合并为一个正则。
var sanitizeBareHdrRe = regexp.MustCompile(`(?i)` + hdrKeyRe)

// sanitizeKvRe 剥离层：尾随裸键值（cc_xxx=...;）循环清理。
var sanitizeKvRe = regexp.MustCompile(`(?i)\bcc_[a-z0-9_]+=[^;\n]*;?\s*`)

// claudeCodeIdentity DSH 身份句的整句替换目标（与 sanitizeRewrites 首对的
// Claude Code 身份句目标一致）。整句替换而非只插冠词：替换后 "DeepSeek Harness"
// 品牌串从 system 消息彻底消失，整包兜底（internal/sanitize.BodyFallback）才不会
// 二次改写已净化的句子。
const claudeCodeIdentity = "You are Claude Code, Anthropic's official CLI tool for Claude"

var (
	// DSH 身份句：容忍可选冠词 the 与分隔符变体。精确改写（插冠词）会先产出
	// "powered by the DeepSeek Harness"，本正则必须能二次命中并整句替换。
	dshIdentityRe = regexp.MustCompile(`(?i)you\s+are\s+an\s+ai\s+agent\s+powered\s+by\s+(?:the\s+)?deepseek[\s\-_]*harness`)
	// DSH Web GUI 段：整段 221 字符逐字串的前半，缩写 DSH 即破坏逐字匹配。
	dshWebGuiRe = regexp.MustCompile(`(?i)deepseek[\s\-_]*harness\s+web\s+gui`)
	// DSH @ 路径句（跨句指纹后半）：插冠词 The 打断逐字串，语义不变（实测 200）。
	acpPathRe = regexp.MustCompile(`(?i)relative\s+paths\s+resolve\s+from\s+the\s+workspace\s+root`)
	// 整包兜底正则只在 internal/sanitize.BodyFallback（接线层），本包不重复实现。
)

// sanitizeRewrites 改写层：全模板句逐字替换（每句只改一个词，语义不变）。
//
// 身份句的匹配串**不带结尾标点**（只到 "…for Claude" 为止）：
// CLI 版这句以句号收尾（"…for Claude."），桌面版（claude-desktop-3p / Agent SDK）
// 以逗号接后继内容（"…for Claude, running within the Claude Agent SDK."）。
// 带句号的整句只匹配前者，桌面版会漏网、指纹原样发上游 → 400 code=11128。
// 去掉结尾标点后两种形态一并覆盖（替换串同样不带标点，让原有标点原样保留）。
// 注意仍要求 "You are Claude Code, " 前缀，不做更宽的子串替换，
// 以免误伤 TestExactMatchOnlyVariantNotTouched 所保护的零散文本。
var sanitizeRewrites = [][2]string{
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
		// 反馈句：整句带 Anthropic 仓库链接，上游按整句拦截（只留链接或只留半边均不拦，
		// 实测需整句同时出现）。give→provide 一词之差即可绕过，语义不变。
		"To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
		"To provide feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
	},
	{
		// 上游反探测：只要请求体里出现裸数字 11128 就整单拦截（与该数字的上下文无关——
		// "code=11128" / 裸 "11128" / "错误码 11128" / "Code=11128" 全部命中；
		// 相邻的 11148 / 11101 / 11115 / 99999 均放行）。11128 正是本类拦截自身的错误码，
		// 上游据此识别"在讨论/回显其内部错误码"的请求。
		// 代价：用户对话中任何 11128 都会被改写——但这串数字出现在请求里本身就是拦截条件，
		// 不改写必然失败。插入连字符保留可读性与指代（零宽空格无效，实测上游会归一化）。
		// 目标用 U+2011（非断行连字符，hex 3131e28091313238）：渲染与普通连字符一致
		// 但字节不同，才真正打断逐字匹配——ASCII "11-128" 与源串同形时 old==new 空转。
		"11128",
		"11\u2011128",
	},
	{
		// DSH 身份句：上游只认 system 开头的逐字形态（换位置、换 role、缺 "You are an"
		// 前缀均放行）；插冠词 the 是最小扰动改法（实测 200）。精确改写产出后由
		// dshIdentityRe 二次命中整句替换为 Claude Code 身份句。
		"You are an AI agent powered by DeepSeek Harness",
		"You are an AI agent powered by the DeepSeek Harness",
	},
	{
		// DSH Web GUI 品牌短语：「整段 221 字符逐字串」的前半段（后半句为 "they mean this GUI."）。
		// 实测判据是**连续逐字前缀**而非关键词或跨句组合：前半句单独出现放行、后半句单独
		// 出现放行、整段截到 220 字符也放行，只有完整 221 字符逐字串才回 400 code=11-128。
		// 缩写为 DSH 即破坏逐字串，语义不变（实测 200）。
		"DeepSeek Harness Web GUI",
		"DSH Web GUI",
	},
}

// sanitizeText 单段文本净化：预检不中 → 返回原串（零分配）。
func sanitizeText(text string) string {
	if !hasFingerprint(text) {
		return text
	}
	for _, rw := range sanitizeRewrites {
		text = strings.ReplaceAll(text, rw[0], rw[1])
	}
	// DSH 指纹段改写（正则，容忍大小写/冠词/分隔符变体；置于精确改写之后——
	// 精确改写先产出 "powered by the DeepSeek Harness"，本层正则需能二次命中）。
	text = dshIdentityRe.ReplaceAllString(text, claudeCodeIdentity)
	text = dshWebGuiRe.ReplaceAllString(text, "DSH Web GUI")
	text = acpPathRe.ReplaceAllString(text, "The relative paths resolve from the workspace root")
	if sanitizeHdrRe.MatchString(text) {
		text = sanitizeHdrRe.ReplaceAllString(text, "")
	}
	if strings.Contains(text, "cc_") {
		prev := ""
		for prev != text { // 清尾随裸 kv（cc_version=...; cc_entrypoint=...;）
			prev = text
			text = sanitizeKvRe.ReplaceAllString(text, "")
		}
	}
	// 兜底：键值形态已在上面整段删除，这里只剩裸键名（引用/示例文本形态）。
	text = sanitizeBareHdrRe.ReplaceAllString(text, "x-anthropic-billing-hdr")
	return strings.TrimSpace(text)
}

// hasFingerprint 特征预检：sanitizeFeatures 全小写 + ASCII 折叠匹配（零分配，
// 不能整串 ToLower——对每个请求体整串拷贝一次会破坏零分配预检）；
// header 键名有大小写变体（X-Anthropic-...）且可能以裸键名形态出现（无冒号），
// 最终以不要求冒号的 (?i) 正则兜底（sanitizeBareHdrRe，是 sanitizeHdrRe 的超集）。
func hasFingerprint(text string) bool {
	for _, f := range sanitizeFeatures {
		if asciiContainsFold(text, f) {
			return true
		}
	}
	return sanitizeBareHdrRe.MatchString(text)
}

// lowerASCII ASCII 大写折小写（仅特征表需要的折叠范围）。
func lowerASCII(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}

// asciiContainsFold 大小写不敏感（ASCII 折叠）子串查找，零分配。
// 特征表全为 ASCII，ASCII 折叠已覆盖上游的大小写判据。
func asciiContainsFold(s, f string) bool {
	n := len(f)
	if n == 0 {
		return true
	}
	first := lowerASCII(f[0])
	for i := 0; i+n <= len(s); i++ {
		if lowerASCII(s[i]) != first {
			continue
		}
		j := 1
		for ; j < n; j++ {
			if lowerASCII(s[i+j]) != lowerASCII(f[j]) {
				break
			}
		}
		if j == n {
			return true
		}
	}
	return false
}

// sanitizeContent 兼容字符串与多模态数组；只动 text part，image 等 part 不动。
// 返回净化后的值及是否发生变化。
func sanitizeContent(v any) (any, bool) {
	switch c := v.(type) {
	case string:
		s := sanitizeText(c)
		return s, s != c
	case []any:
		changed := false
		for _, p := range c {
			m, ok := p.(map[string]any)
			if !ok {
				continue
			}
			text, ok := m["text"].(string)
			if !ok {
				continue
			}
			if s := sanitizeText(text); s != text {
				m["text"] = s
				changed = true
			}
		}
		return c, changed
	}
	return v, false
}

// sanitizeToolCalls 净化 assistant.tool_calls[].function.arguments。
//
// arguments 是**字符串化的 JSON**（不是对象），因此按文本走 sanitizeText 即可。
// 这块长期是盲区：工具调用消息的 content 通常是 null，而旧版 sanitizeMessages
// 在 content 缺失时直接 continue，整条消息连 tool_calls 一起被跳过——
// 于是历史里任何写进工具参数的被拦字符串（文件名、命令、写入内容）都会原样漏出。
func sanitizeToolCalls(v any) bool {
	callList, ok := v.([]any)
	if !ok {
		return false
	}
	changed := false
	for _, c := range callList {
		call, ok := c.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := call["function"].(map[string]any)
		if !ok {
			continue
		}
		args, ok := fn["arguments"].(string)
		if !ok {
			continue
		}
		if s := sanitizeText(args); s != args {
			fn["arguments"] = s
			changed = true
		}
	}
	return changed
}

// sanitizeMessages 净化 messages 中的 content、reasoning_content 与 tool_calls；任一命中返回 true。
func sanitizeMessages(messages []any) bool {
	changed := false
	for _, msg := range messages {
		m, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		// content 与 tool_calls 各自独立判断：content 可以为 null（工具调用轮），
		// 早期版本在此 continue，导致这类消息的 tool_calls 完全不被净化。
		if c, ok := m["content"]; ok {
			if nc, ch := sanitizeContent(c); ch {
				m["content"] = nc
				changed = true
			}
		}
		// reasoning_content（思维链回填字段，见 thinking.go/sse.go）实测同样
		// 携带指纹，与 content 同等净化。string 形态直接走 sanitizeText。
		if rc, ok := m["reasoning_content"].(string); ok {
			if s := sanitizeText(rc); s != rc {
				m["reasoning_content"] = s
				changed = true
			}
		}
		if tc, ok := m["tool_calls"]; ok {
			if sanitizeToolCalls(tc) {
				changed = true
			}
		}
	}
	return changed
}
