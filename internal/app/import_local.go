package app

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"wild-work/internal/provider"
)

// 本文件实现「导入型」渠道的凭据获取：凭据由官方客户端在本机维护，
// 本工具不去复现它的登录流程，而是直接读取客户端落盘的凭据文件。
//
// 小浣熊的凭据来自官方客户端（%USERPROFILE%\.box-agent\config\auth.json 明文 JSON）。
// 面板另有「浏览器授权登录」主路径（见 internal/login_raccoon），导入作为回退。
//
// 设计要点：
//   - 路径**自适应探测**（多候选 + 环境变量覆盖），不做硬编码单一路径；
//   - 只在 Windows 生效（客户端只有 Windows 版）；
//   - 写入走 tmp+rename 原子替换、0600，并与 internal/auth.Parse 的嵌套格式逐字段对齐；
//   - **绝不打印凭据值**（日志只记文件名与 uid）。

// ImportLocalResult 导入结果（供面板展示）。
type ImportLocalResult struct {
	Channel string `json:"channel"`
	UID     string `json:"uid"`
	File    string `json:"file"`
	Note    string `json:"note,omitempty"`
}

// authDoc / authSection / accountSection 与 internal/auth.Parse 的嵌套分支严格对应。
type authDoc struct {
	Auth    authSection    `json:"auth"`
	Account accountSection `json:"account"`
}

type authSection struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"`
	Domain       string `json:"domain"`
	ApiHost      string `json:"apiHost"`
	MachineID    string `json:"machineId"`
	DeviceID     string `json:"deviceId"`
	MachineToken string `json:"machineToken"`
	MachineType  string `json:"machineType"`
}

type accountSection struct {
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterpriseId"`
	Nickname     string `json:"nickname"`
}

// ImportLocalCredentials 从本机已安装的官方客户端导入指定渠道凭据。
func (a *App) ImportLocalCredentials(channel string) (*ImportLocalResult, error) {
	if runtime.GOOS != "windows" {
		return nil, errors.New("「从本机客户端导入」目前仅支持 Windows（小浣熊客户端只有 Windows 版）")
	}
	switch provider.Kind(strings.TrimSpace(channel)) {
	case provider.Raccoon:
		return a.importRaccoon()
	}
	return nil, fmt.Errorf("渠道 %q 不支持本地导入（该渠道请用面板的登录按钮）", channel)
}

// importRaccoon 读取商汤小浣熊客户端的 auth.json 并转换成本工具格式。
func (a *App) importRaccoon() (*ImportLocalResult, error) {
	path, err := raccoonClientAuthPath()
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取客户端凭据失败（%s）：%w", path, err)
	}
	var payload struct {
		AccessToken    string `json:"access_token"`
		RefreshToken   string `json:"refresh_token"`
		OfficeIdentity string `json:"office_identity"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("解析客户端凭据失败（%s）：%w", path, err)
	}
	access := strings.TrimSpace(payload.AccessToken)
	if access == "" {
		return nil, fmt.Errorf("客户端凭据里没有 access_token（%s）：请先在客户端完成登录", path)
	}
	uid := strings.TrimSpace(jwtClaim(access, "name"))
	if uid == "" {
		uid = "default"
	}
	doc := authDoc{
		Auth: authSection{
			AccessToken:  access,
			RefreshToken: strings.TrimSpace(payload.RefreshToken),
			ExpiresAt:    jwtExp(access),
			Domain:       "/api/web/llm/v2",
			ApiHost:      "https://xiaohuanxiong.com",
		},
		Account: accountSection{
			UID:          uid,
			EnterpriseID: strings.TrimSpace(payload.OfficeIdentity),
			Nickname:     uid,
		},
	}
	file, err := a.writeAuthFile("raccoon", uid, doc)
	if err != nil {
		return nil, err
	}
	a.reloadAccounts()
	a.afterAccountAdded(provider.Raccoon)
	return &ImportLocalResult{
		Channel: "raccoon", UID: uid, File: filepath.Base(file),
		Note: "access_token 约 2 小时有效，wild-work 会用 refresh_token 自动续期（上游会轮换 refresh_token，新值已随刷新落盘）",
	}, nil
}

// raccoonClientAuthPath 定位小浣熊客户端凭据（自适应多候选）。
func raccoonClientAuthPath() (string, error) {
	var cands []string
	// 客户端支持用 BOX_AGENT_CONFIG_DIR 覆盖配置目录（见 boxAgentAuthFile.js）。
	if dir := strings.TrimSpace(os.Getenv("BOX_AGENT_CONFIG_DIR")); dir != "" {
		cands = append(cands, filepath.Join(dir, "auth.json"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		cands = append(cands, filepath.Join(home, ".box-agent", "config", "auth.json"))
	}
	for _, p := range cands {
		if fileExists(p) {
			return p, nil
		}
	}
	return "", fmt.Errorf("未找到小浣熊客户端凭据（已尝试 %d 个路径，最后一个是 %s）：请先安装并登录「商汤小浣熊」客户端",
		len(cands), cands[len(cands)-1])
}

// writeAuthFile 原子写 auth 文件（tmp + rename，0600），文件名前缀即渠道 Kind。
func (a *App) writeAuthFile(prefix, uid string, doc authDoc) (string, error) {
	if err := os.MkdirAll(a.cfg.AuthDir, 0o700); err != nil {
		return "", fmt.Errorf("创建凭据目录失败：%w", err)
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	raw = append(raw, '\n')
	file := filepath.Join(a.cfg.AuthDir, prefix+"-"+sanitizeUID(uid)+".json")
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return "", fmt.Errorf("写入凭据失败：%w", err)
	}
	if err := os.Rename(tmp, file); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("保存凭据失败：%w", err)
	}
	return file, nil
}

// sanitizeUID 过滤文件名里的危险字符（uid 来自 token，可能含路径分隔符等）。
func sanitizeUID(uid string) string {
	var sb strings.Builder
	for _, r := range uid {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			sb.WriteRune(r)
		}
	}
	out := sb.String()
	if out == "" {
		return "default"
	}
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}

// jwtPayload 解出 JWT 的 payload（失败返回 nil）。
func jwtPayload(tok string) map[string]any {
	parts := strings.Split(strings.TrimSpace(tok), ".")
	if len(parts) < 2 {
		return nil
	}
	seg := strings.NewReplacer("-", "+", "_", "/").Replace(parts[1])
	if m := len(seg) % 4; m != 0 {
		seg += strings.Repeat("=", 4-m)
	}
	raw, err := base64.StdEncoding.DecodeString(seg)
	if err != nil {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

func jwtClaim(tok, key string) string {
	m := jwtPayload(tok)
	if m == nil {
		return ""
	}
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}

func jwtExp(tok string) int64 {
	m := jwtPayload(tok)
	if m == nil {
		return 0
	}
	if f, ok := m["exp"].(float64); ok {
		return int64(f)
	}
	return 0
}

func fileExists(p string) bool {
	if p == "" {
		return false
	}
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
