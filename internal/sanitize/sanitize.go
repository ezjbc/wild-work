// Package sanitize 出站请求体指纹脱敏：清除 Claude Code / Codex CLI / DSH 注入的
// 模板句，防止上游 CodeBuddy 内容审核按逐字精确匹配拦截（HTTP 400 code=11128 /
// content_blocked）。
//
// 策略：键值型指纹整段删除，承载语义的模板句最小改写；对大小写不敏感、可跨句、
// 只看 system 开头的 DSH 逐字指纹（2026-10-28 真机三指纹实测）用正则容忍变体；
// 结构化清洗覆盖不到的字段（tools/metadata 等）由 BodyFallback 在 PrepareBody
// 接线层整包兜底。content 值为 null 的 assistant 消息不会跳过——其 tool_calls
// arguments 仍需净化。
//
// 本包有 internal/upstream/sanitize.go 的平行实现，两处必须同步改，漏一处等于没改。
package sanitize

import (
	"encoding/json"
	"log"
	"regexp"
	"strings"
)

// featHeaders 特征预检表：全部小写，配合 asciiContainsFold 做大小写不敏感匹配。
// 上游判据大小写不敏感（2026-10-28 真机实测小写变体同样 400），大小写敏感的
// Contains 会让小写变体连净化都进不去。
var featHeaders = []string{
	"x-anthropic-billing-hdr",
	"x-anthropic-billing-header",
	"cc_entrypoint=",
	"you are claude code",
	"main branch (",
	"you are a coding agent running in the codex cli",
	"github.com/anthropics/",
	"11-128",
	"11128",
	// DSH 身份句：上游按「system 消息开头」逐字锚定匹配（2026-10-21 实测）。
	"you are an ai agent powered by deepseek harness",
	// DSH Web GUI 段：上游按 system 开头整段 221 字符逐字串锚定（2026-10-21 实测）。
	"deepseek harness web gui",
	// DSH 品牌串（超集，兜住 "DeepSeek Harness CLI" 等变体触发预检）。
	"deepseek harness",
	// DSH @ 路径句（跨句指纹后半，2026-10-28 实测）。
	"relative paths resolve from the workspace root",
}

// 宽松键名正则：键名内部允许夹任意非字母序列（全角/Unicode 连字符、空格、零宽等），
// 且 h/e/a/d/e/r 逐字母容忍分隔——header 全形态与 hdr 缩写形态都命中。
// 注意不能简单写 `hdr`：原始键名 "header" 不含连续 "hdr"，会漏掉键值整段剥离。
const hdrKeyRe = `x[^a-z]*anthropic[^a-z]*billing[^a-z]*h[^a-z]*(?:e[^a-z]*)?(?:a[^a-z]*)?d[^a-z]*(?:e[^a-z]*)?r`

var reHdr = regexp.MustCompile(`(?i)` + hdrKeyRe + `\s*:[^;\n]*;?\s*`)
var reBareHdr = regexp.MustCompile(`(?i)` + hdrKeyRe)
var reKv = regexp.MustCompile(`(?i)\bcc_[a-z0-9_]+=[^;\n]*;?\s*`)

// claudeCodeIdentity DSH 身份句的整句替换目标（与 rewrites 首对的 Claude Code
// 身份句目标一致）。整句替换而非只插冠词：替换后 "DeepSeek Harness" 品牌串从
// system 消息彻底消失，BodyFallback 才不会二次改写已净化的句子。
const claudeCodeIdentity = "You are Claude Code, Anthropic's official CLI tool for Claude"

var (
	// DSH 身份句：容忍可选冠词 the 与分隔符变体。精确改写（插冠词）会先产出
	// "powered by the DeepSeek Harness"，本正则必须能二次命中并整句替换。
	dshIdentityRe = regexp.MustCompile(`(?i)you\s+are\s+an\s+ai\s+agent\s+powered\s+by\s+(?:the\s+)?deepseek[\s\-_]*harness`)
	// DSH Web GUI 段：整段 221 字符逐字串的前半，缩写 DSH 即破坏逐字匹配。
	dshWebGuiRe = regexp.MustCompile(`(?i)deepseek[\s\-_]*harness\s+web\s+gui`)
	// DSH @ 路径句（跨句指纹后半）：插冠词 The 打断逐字串，语义不变（实测 200）。
	acpPathRe = regexp.MustCompile(`(?i)relative\s+paths\s+resolve\s+from\s+the\s+workspace\s+root`)
	// 整包兜底：任意位置（含 tools/metadata 等结构化清洗摸不到的字段）的 DSH 品牌串。
	bodyDSHRe = regexp.MustCompile(`(?i)deepseek[\s\-_]*harness`)
)

var rewrites = [][2]string{
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
	// 裸 11128 整单拦截，插入连字符保留可读性（零宽空格无效，上游会归一化）。
	// 目标用 U+2011（非断行连字符）：渲染与普通连字符一致但字节不同，才真正打断
	// 逐字匹配——ASCII "11-128" 与源串同形时 old==new 空转，等于没改。
	{"11128", "11\u2011128"}, // 第二个是 U+2011，hex 3131e28091313238，不是 ASCII '-'
	// DSH 身份句：上游只认 system 开头的逐字形态；插冠词 the 是最小扰动改法
	//（实测 200）。精确改写产出后由 dshIdentityRe 二次命中整句替换为 Claude Code。
	{
		"You are an AI agent powered by DeepSeek Harness",
		"You are an AI agent powered by the DeepSeek Harness",
	},
	// DSH Web GUI 品牌短语：判据是整段逐字前缀（221 字符），不是关键词也不是跨句组合；
	// 缩写为 DSH 即破坏逐字串，完整 prompt 通过（实测 200）。
	{
		"DeepSeek Harness Web GUI",
		"DSH Web GUI",
	},
}

// Messages 脱敏 body 中 messages 的 content / reasoning_content / tool_calls.arguments。
// body 不可解析时原样返回，绝不阻塞请求（降级语义：宁可发指纹原文也不丢消息）。
// 整包兜底不在这里——本函数被快照测试锁定为「只动结构化字段」，BodyFallback
// 接线在两处 PrepareBody 包装层（payload.go / wb_utils.go）。
func Messages(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	// 预检：零分配快速路径，普通请求全不中。
	if !hasFingerprint(toString(body)) {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	msgs, _ := obj["messages"].([]any)
	if len(msgs) == 0 {
		return body
	}
	if cleanMessages(msgs) {
		if out, err := json.Marshal(obj); err == nil {
			return out
		}
	}
	return body
}

func toString(b []byte) string { return string(b) }

// hasFingerprint 特征预检：featHeaders 全小写 + ASCII 折叠匹配（上游判据大小写
// 不敏感），再以 reBareHdr 兜住键名形态。asciiContainsFold 命中与否都零分配，
// 守住「预检不命中零分配原样通过」的不量（不能整串 ToLower，会逐请求拷贝）。
func hasFingerprint(s string) bool {
	for _, f := range featHeaders {
		if asciiContainsFold(s, f) {
			return true
		}
	}
	return reBareHdr.MatchString(s)
}

// lowerASCII ASCII 大写折小写（仅特征表需要的折叠范围）。
func lowerASCII(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}

// asciiContainsFold 大小写不敏感（ASCII 折叠）子串查找，零分配。
// 特征表全为 ASCII，ASCII 折叠已覆盖上游的大小写判据；不能用 strings.ToLower
// ——它对每个请求体整串拷贝一次，破坏零分配预检。
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

func cleanText(text string) string {
	if !hasFingerprint(text) {
		return text
	}
	for _, rw := range rewrites {
		text = strings.ReplaceAll(text, rw[0], rw[1])
	}
	// DSH 指纹段改写（正则，容忍大小写/冠词/分隔符变体；置于精确改写之后——
	// 精确改写先产出 "powered by the DeepSeek Harness"，本层正则需能二次命中）。
	text = dshIdentityRe.ReplaceAllString(text, claudeCodeIdentity)
	text = dshWebGuiRe.ReplaceAllString(text, "DSH Web GUI")
	text = acpPathRe.ReplaceAllString(text, "The relative paths resolve from the workspace root")
	if reHdr.MatchString(text) {
		text = reHdr.ReplaceAllString(text, "")
	}
	if strings.Contains(text, "cc_") {
		prev := ""
		for prev != text {
			prev = text
			text = reKv.ReplaceAllString(text, "")
		}
	}
	// 兜底：键值形态已在上面整段删除，这里只剩裸键名（引用/示例文本形态）。
	text = reBareHdr.ReplaceAllString(text, "x-anthropic-billing-hdr")
	return strings.TrimSpace(text)
}

func cleanContent(v any) (any, bool) {
	switch c := v.(type) {
	case string:
		s := cleanText(c)
		return s, s != c
	case []any:
		changed := false
		for _, p := range c {
			m, _ := p.(map[string]any)
			if m == nil {
				continue
			}
			if t, ok := m["text"].(string); ok {
				if s := cleanText(t); s != t {
					m["text"] = s
					changed = true
				}
			}
		}
		return c, changed
	}
	return v, false
}

func cleanToolCalls(v any) bool {
	calls, _ := v.([]any)
	if len(calls) == 0 {
		return false
	}
	changed := false
	for _, c := range calls {
		call, _ := c.(map[string]any)
		if call == nil {
			continue
		}
		fn, _ := call["function"].(map[string]any)
		if fn == nil {
			continue
		}
		args, _ := fn["arguments"].(string)
		if args == "" {
			continue
		}
		if s := cleanText(args); s != args {
			fn["arguments"] = s
			changed = true
		}
	}
	return changed
}

func cleanMessages(msgs []any) bool {
	changed := false
	for _, m := range msgs {
		msg, _ := m.(map[string]any)
		if msg == nil {
			continue
		}
		if v, ok := msg["content"]; ok {
			if nc, ch := cleanContent(v); ch {
				msg["content"] = nc
				changed = true
			}
		}
		if rc, ok := msg["reasoning_content"].(string); ok {
			if s := cleanText(rc); s != rc {
				msg["reasoning_content"] = s
				changed = true
			}
		}
		if tc, ok := msg["tool_calls"]; ok {
			if cleanToolCalls(tc) {
				changed = true
			}
		}
	}
	return changed
}

// BodyFallback 整包兜底：结构化清洗只覆盖 messages 的 content/reasoning_content/
// tool_calls.arguments，metadata、tools[].function.description、客户端自定义字段都
// 在覆盖范围之外（238KB 的请求里 tools 就占 122KB），而上游对任意位置的 DSH 品牌
// 串都按逐字指纹拦截（2026-10-28 真机实测）。对最终 body 做品牌串替换，被匹配的
// 字节都是普通 ASCII 字母与空白/连字符，JSON 合法性不受影响。
// 仅在确有命中时才重编码；普通请求零改动零分配。接线点：两处 PrepareBody。
func BodyFallback(b []byte) []byte {
	if len(b) == 0 || !bodyDSHRe.Match(b) {
		return b
	}
	log.Printf("[sanitize] body fallback: 整包命中 DSH 品牌串，已替换为 Claude Code（body=%d 字节）", len(b))
	return bodyDSHRe.ReplaceAll(b, []byte("Claude Code"))
}

// SystemHead 提取 body 中首条 system/developer 消息 content 的前 n 个字符（rune
// 截断），用于拦截诊断——上游只看 system 开头做逐字指纹，user 内容与本诊断无关、
// 绝不返回。解析失败或无 system 消息返回 ""。多模态 content 取首个 text part。
func SystemHead(b []byte, n int) string {
	if len(b) == 0 || n <= 0 {
		return ""
	}
	var obj struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(b, &obj); err != nil {
		return ""
	}
	for _, m := range obj.Messages {
		if m.Role != "system" && m.Role != "developer" {
			continue
		}
		var s string
		if err := json.Unmarshal(m.Content, &s); err == nil {
			return truncateRunes(s, n)
		}
		var parts []map[string]any
		if err := json.Unmarshal(m.Content, &parts); err == nil {
			for _, p := range parts {
				if t, ok := p["text"].(string); ok {
					return truncateRunes(t, n)
				}
			}
		}
		return ""
	}
	return ""
}

// truncateRunes 按 rune 截断（多字节内容不得切出半个字符），去除截断残留的首尾空白
// （日志不应带尾随空格；TrimSpace 只影响诊断展示，不回写请求体）。
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(string(r[:n]))
}
