package claude

import "strings"

// BlockedEnv lists the variables that must not reach a launched or refreshing
// CLI: an API key outranks account credentials, and the CLAUDE_CODE_* set makes
// it believe it is a nested child.
var BlockedEnv = []string{
	"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN",
	"CLAUDECODE", "CLAUDE_CODE_SESSION_ID", "CLAUDE_CODE_CHILD_SESSION",
	"CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_EXECPATH", "CLAUDE_CODE_SESSION_ATTENDED",
	"CLAUDE_CODE_ATTRIBUTION_HEADER", "CLAUDE_CODE_MESSAGING_SOCKET",
	"CLAUDE_CODE_MESSAGING_TOKEN", "CLAUDE_PID", "CLAUDE_EFFORT",
}

// IsBlockedEnv reports whether a variable name is in BlockedEnv.
func IsBlockedEnv(name string) bool {
	for _, blocked := range BlockedEnv {
		if name == blocked {
			return true
		}
	}
	return false
}

// StripBlockedEnv returns a copy of an os.Environ-style list without the
// BlockedEnv variables. The input is never mutated.
func StripBlockedEnv(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if IsBlockedEnv(name) {
			continue
		}
		out = append(out, kv)
	}
	return out
}
