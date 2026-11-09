//go:build darwin && arm64 && cgo

package report

import (
	"os"
	"regexp"
	"strings"
	"unicode"
)

var (
	darwinSecrets    = regexp.MustCompile(`(?i)token|password|passwd|secret|credential|authorization|cookie|api.?key|adbkey|private.?key|cdk|account|username|email|login|access.?key|(?:^|\W)(?:key|user|uid|serial|device.?id)\s*[:=]|账号|账户|密码|口令|兑换码|密钥|用户名|手机号`)
	darwinURL        = regexp.MustCompile(`(?i)https?://[^\s<>"']+`)
	darwinEmail      = regexp.MustCompile(`[\w.+-]+@[\w.-]+\.[A-Za-z]{2,}`)
	darwinQuotedPath = regexp.MustCompile(`"(?:/|[A-Za-z]:[\\/])[^"\r\n]*"|'(?:/|[A-Za-z]:[\\/])[^'\r\n]*'`)
	darwinPath       = regexp.MustCompile(`(?:^|[\s=:,(])(?:/[^\s"'<>]+|[A-Za-z]:[\\/][^\s"'<>]+)`)
	darwinIP         = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}(?::\d+)?\b|(?:[0-9A-Fa-f]{0,4}:){2,}[0-9A-Fa-f:.%]+`)
	darwinMAC        = regexp.MustCompile(`(?i)\b(?:[0-9a-f]{2}:){5}[0-9a-f]{2}\b`)
	darwinUUID       = regexp.MustCompile(`(?i)\b[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}\b`)
	darwinLongID     = regexp.MustCompile(`\b(?:[0-9A-Fa-f]{16,}|[A-Za-z0-9_+/=-]{32,})\b`)
	darwinPhone      = regexp.MustCompile(`\b1[3-9]\d{9}\b`)
)

// darwinSanitizer removes credential-bearing lines before replacing common
// identifiers. It is applied to every collected text file and the report itself.
// This is a local review aid; the user still reviews a package before sharing it.
func darwinSanitizer(rootDir string) func(string) string {
	home, _ := os.UserHomeDir()
	hostname, _ := os.Hostname()
	identifiers := []string{rootDir, home, hostname, os.Getenv("USER"), os.Getenv("LOGNAME")}
	return func(text string) string {
		var result []string
		for _, line := range strings.Split(strings.ToValidUTF8(text, "�"), "\n") {
			if darwinSecrets.MatchString(line) {
				result = append(result, "[含账号或凭据字段的整行已移除]")
				continue
			}
			for _, identifier := range identifiers {
				if len(identifier) >= 3 {
					line = strings.ReplaceAll(line, identifier, "[本机标识]")
				}
			}
			for _, rule := range []struct {
				re          *regexp.Regexp
				replacement string
			}{
				{darwinURL, "[URL]"}, {darwinEmail, "[邮箱]"}, {darwinQuotedPath, "[路径]"},
				{darwinPath, " [路径]"}, {darwinMAC, "[MAC]"}, {darwinIP, "[IP]"},
				{darwinUUID, "[UUID]"}, {darwinLongID, "[长标识]"}, {darwinPhone, "[手机号]"},
			} {
				line = rule.re.ReplaceAllString(line, rule.replacement)
			}
			line = strings.Map(func(r rune) rune {
				if unicode.IsControl(r) && r != '\t' {
					return -1
				}
				return r
			}, line)
			if runes := []rune(line); len(runes) > 2048 {
				line = string(runes[:2048]) + "[截断]"
			}
			result = append(result, line)
		}
		return strings.Join(result, "\n")
	}
}
