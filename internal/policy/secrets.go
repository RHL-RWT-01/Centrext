package policy

import "regexp"

// secretPatterns catch common credential formats. This is a guard against
// accidental capture, not a complete scanner.
var secretPatterns = []struct {
	name string
	re   *regexp.Regexp
}{
	{"private key", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{"AWS access key", regexp.MustCompile(`\b(AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{"GitHub token", regexp.MustCompile(`\b(ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{36,}\b|\bgithub_pat_[A-Za-z0-9_]{50,}\b`)},
	{"Slack token", regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}\b`)},
	{"API key (sk-)", regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}\b`)},
	{"Google API key", regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`)},
	{"JWT", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`)},
	{"password assignment", regexp.MustCompile(`(?i)\b(password|passwd|secret|api[_-]?key|access[_-]?token)\s*[:=]\s*['"]?[^\s'"]{8,}`)},
	{"connection string with password", regexp.MustCompile(`\b[a-z][a-z0-9+.-]*://[^\s:/@]+:[^\s@/]{3,}@`)},
}

// DetectSecret returns the name of the first secret pattern found, or "".
func DetectSecret(texts ...string) string {
	for _, t := range texts {
		for _, p := range secretPatterns {
			if p.re.MatchString(t) {
				return p.name
			}
		}
	}
	return ""
}
