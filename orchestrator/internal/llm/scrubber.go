package llm

import (
	"encoding/json"
	"regexp"
	"strings"
)

var (
	reAuthScheme     = regexp.MustCompile(`(?i)\b(Bearer|Basic|Token)\s+[A-Za-z0-9\-._~+/]+=*`)
	reJWT            = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{4,}\.(?:[A-Za-z0-9_-]+\b)?`)
	reGitHubToken    = regexp.MustCompile(`\b(?:(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9_]{16,}|github_pat_[A-Za-z0-9_]{20,})\b`)
	reOpenAIKey      = regexp.MustCompile(`\b(?:(?:sk|rk|pk)_(?:live|test)_[A-Za-z0-9]{12,}|sk-(?:proj-|svcacct-)?[A-Za-z0-9_-]{16,})\b`)
	reGoogleKey      = regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{20,}\b`)
	reAWSAccess      = regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)
	reJSONSecret     = regexp.MustCompile(`(?i)("(?:password|passwd|pwd|api_key|apikey|api-key|token|access_token|auth_token|refresh_token|secret|client_secret|authorization|proxy-authorization|x-api-key|x-auth-token|x-csrf-token)"\s*:\s*")((?:[^"\\]|\\.)+)(")`)
	reEscapedJSONSec = regexp.MustCompile(`(?i)(\\"(?:password|passwd|pwd|api_key|apikey|api-key|token|access_token|auth_token|refresh_token|secret|client_secret|authorization|proxy-authorization|x-api-key|x-auth-token|x-csrf-token)\\"\s*:\s*\\")([^"\\]+)(\\")`)
	reKVSecret       = regexp.MustCompile(`(?i)(^|[^A-Za-z0-9_-])(password|passwd|pwd|api_key|apikey|api-key|token|access_token|auth_token|refresh_token|secret|client_secret|authorization|proxy-authorization|x-api-key|x-auth-token|x-csrf-token)\s*([:=])\s*(?:"[^"]*"|'[^']*'|[^\s"'&,;}{]+)`)
	reEmail          = regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`)
)

// ScrubSecrets redacts JWTs, API keys, OAuth/Bearer/Basic/Token credentials, passwords,
// and email addresses from arbitrary text before database persistence or LLM submission.
func ScrubSecrets(input string) string {
	if input == "" {
		return ""
	}
	out := input
	out = reAuthScheme.ReplaceAllString(out, "${1} [REDACTED_TOKEN]")
	out = reJWT.ReplaceAllString(out, "[REDACTED_JWT]")
	out = reGitHubToken.ReplaceAllString(out, "[REDACTED_GITHUB_TOKEN]")
	out = reOpenAIKey.ReplaceAllString(out, "[REDACTED_API_KEY]")
	out = reGoogleKey.ReplaceAllString(out, "[REDACTED_API_KEY]")
	out = reAWSAccess.ReplaceAllString(out, "[REDACTED_AWS_KEY]")
	out = reEscapedJSONSec.ReplaceAllString(out, "${1}[REDACTED]${3}")
	out = reJSONSecret.ReplaceAllString(out, "${1}[REDACTED]${3}")
	out = reKVSecret.ReplaceAllStringFunc(out, func(match string) string {
		sub := reKVSecret.FindStringSubmatch(match)
		if len(sub) < 4 {
			return "[REDACTED]"
		}
		prefix, key, sep := sub[1], sub[2], sub[3]
		if strings.Contains(match, `"`) && sep == ":" {
			return prefix + key + `: "[REDACTED]"`
		}
		return prefix + key + sep + "[REDACTED]"
	})
	out = reEmail.ReplaceAllString(out, "[REDACTED_EMAIL]")
	return out
}

// ScrubAny recursively scrubs strings inside JSON-compatible slices and maps.
func ScrubAny(v any) any {
	switch val := v.(type) {
	case string:
		return ScrubSecrets(val)
	case []any:
		res := make([]any, len(val))
		for i, item := range val {
			res[i] = ScrubAny(item)
		}
		return res
	case []string:
		res := make([]any, len(val))
		for i, item := range val {
			res[i] = ScrubSecrets(item)
		}
		return res
	case map[string]any:
		res := make(map[string]any, len(val))
		for k, item := range val {
			lowerKey := strings.ToLower(k)
			if isSensitiveKey(lowerKey) {
				switch tv := item.(type) {
				case string:
					res[k] = "[REDACTED]"
					continue
				case []string:
					redacted := make([]any, len(tv))
					for i := range tv {
						redacted[i] = "[REDACTED]"
					}
					res[k] = redacted
					continue
				case []any:
					redacted := make([]any, len(tv))
					for i, elem := range tv {
						if _, ok := elem.(string); ok {
							redacted[i] = "[REDACTED]"
						} else {
							redacted[i] = ScrubAny(elem)
						}
					}
					res[k] = redacted
					continue
				}
			}
			res[k] = ScrubAny(item)
		}
		return res
	default:
		return v
	}
}

func isSensitiveKey(lowerKey string) bool {
	normalized := strings.ReplaceAll(strings.TrimSpace(lowerKey), "-", "_")
	switch normalized {
	case "password", "passwd", "pwd", "api_key", "apikey", "token",
		"access_token", "auth_token", "refresh_token", "secret",
		"client_secret", "authorization", "proxy_authorization",
		"x_api_key", "x_auth_token", "x_csrf_token", "cookie", "set_cookie":
		return true
	default:
		return false
	}
}

// ScrubRawJSON scrubs all string/secret values inside a JSON payload.
func ScrubRawJSON(raw json.RawMessage, emptyFallback string) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(emptyFallback)
	}
	var parsed any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		scrubbed := ScrubSecrets(string(raw))
		b, mErr := json.Marshal(scrubbed)
		if mErr != nil {
			return json.RawMessage(emptyFallback)
		}
		return b
	}
	scrubbed := ScrubAny(parsed)
	b, err := json.Marshal(scrubbed)
	if err != nil {
		return json.RawMessage(emptyFallback)
	}
	return b
}

// ScrubFeedback scrubs user_message, console_errors, network_logs, and breadcrumbs.
func ScrubFeedback(userMessage string, consoleErrors, networkLogs, breadcrumbs []any) (string, []any, []any, []any) {
	scrubbedMsg := ScrubSecrets(userMessage)
	scrubSlice := func(items []any) []any {
		if items == nil {
			return []any{}
		}
		out, _ := ScrubAny(items).([]any)
		if out == nil {
			return []any{}
		}
		return out
	}
	return scrubbedMsg, scrubSlice(consoleErrors), scrubSlice(networkLogs), scrubSlice(breadcrumbs)
}
