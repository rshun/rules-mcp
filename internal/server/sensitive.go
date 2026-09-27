package server

import "regexp"

// Fail closed for credential-looking assignments in comments and common key
// formats. This is an additional guard, not a universal secret detector.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|authorization|bearer|cookie|credential)\s*[:=]\s*\S+`),
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`\b(?:sk-|ghp_|github_pat_)[A-Za-z0-9_-]{16,}`),
	regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~-]{16,}`),
	regexp.MustCompile(`\b[0-9]{6,12}:[A-Za-z0-9_-]{30,}\b`),
}

func hasSensitive(data []byte) bool {
	for _, pattern := range secretPatterns {
		if pattern.Match(data) {
			return true
		}
	}
	return false
}
