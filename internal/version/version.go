package version

import "strings"

// Commit is the git SHA injected at image build via ldflags.
// Coolify docker-image apps set SOURCE_COMMIT to "HEAD" (no git checkout).
var Commit string

func IsSHA(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) < 6 || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

func Resolve(env string) string {
	if IsSHA(env) {
		return strings.TrimSpace(env)
	}
	if IsSHA(Commit) {
		return strings.TrimSpace(Commit)
	}
	return ""
}
