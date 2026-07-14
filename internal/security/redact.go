// Package security removes credentials before data leaves the process.
package security

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

const (
	defaultMaxFieldRunes   = 4_096
	defaultMaxPayloadRunes = 32_768
	truncationMarker       = "…[TRUNCATED]"
)

var (
	privateKeyPattern    = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`)
	authorizationPattern = regexp.MustCompile(`(?im)^(\s*(?:authorization|proxy-authorization)\s*[:=]\s*)(?:bearer\s+)?[^\r\n]+`)
	cookiePattern        = regexp.MustCompile(`(?im)^(\s*cookie\s*:\s*)[^\r\n]+`)
	setCookiePattern     = regexp.MustCompile(`(?im)^(\s*set-cookie\s*:\s*)[^\r\n]+`)
	environmentPattern   = regexp.MustCompile(`(?m)^([ \t]*(?:export[ \t]+)?)([A-Za-z_][A-Za-z0-9_]*)([ \t]*=[ \t]*)([^\r\n]*)$`)
	telegramTokenPattern = regexp.MustCompile(`\b[0-9]{8,12}:[A-Za-z0-9_-]{30,}\b`)
	githubClassicPattern = regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}\b`)
	githubFinePattern    = regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}\b`)
)

// Config defines immutable Redactor limits and additional literal secrets.
type Config struct {
	Secrets         []string
	MaxFieldRunes   int
	MaxPayloadRunes int
}

// Redactor is safe for concurrent use.
type Redactor struct {
	secrets         []string
	maxFieldRunes   int
	maxPayloadRunes int
}

// NewRedactor defensively copies secrets and preorders them longest-first.
func NewRedactor(cfg Config) *Redactor {
	secrets := make([]string, 0, len(cfg.Secrets))
	for _, secret := range cfg.Secrets {
		if secret != "" {
			secrets = append(secrets, secret)
		}
	}
	slices.SortFunc(secrets, func(a, b string) int { return len(b) - len(a) })

	return &Redactor{
		secrets:         slices.Clone(secrets),
		maxFieldRunes:   positiveOr(cfg.MaxFieldRunes, defaultMaxFieldRunes),
		maxPayloadRunes: positiveOr(cfg.MaxPayloadRunes, defaultMaxPayloadRunes),
	}
}

// RedactString redacts credentials before applying the payload bound.
func (r *Redactor) RedactString(value string) string {
	return truncateRunes(r.redact(value), r.maxPayloadRunes)
}

// RedactBytes redacts JSON-like payloads without mutating input. Valid JSON is
// kept valid whenever the configured total bound can contain a JSON marker.
func (r *Redactor) RedactBytes(payload []byte) []byte {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil || decoder.More() {
		return []byte(r.RedactString(string(payload)))
	}
	if err := ensureJSONEnd(decoder); err != io.EOF {
		return []byte(r.RedactString(string(payload)))
	}

	redacted := r.redactJSON("", value)
	encoded, err := json.Marshal(redacted)
	if err != nil {
		return []byte(r.RedactString(string(payload)))
	}
	if utf8.RuneCount(encoded) <= r.maxPayloadRunes {
		return encoded
	}

	marker, _ := json.Marshal(truncateRunes(truncationMarker, r.maxPayloadRunes-2))
	if utf8.RuneCount(marker) <= r.maxPayloadRunes {
		return marker
	}
	return []byte(truncateRunes(string(encoded), r.maxPayloadRunes))
}

func ensureJSONEnd(decoder *json.Decoder) error {
	var extra any
	return decoder.Decode(&extra)
}

func (r *Redactor) redactJSON(key string, value any) any {
	if label := sensitiveKeyLabel(key); label != "" {
		return label
	}
	switch value := value.(type) {
	case string:
		return truncateRunes(r.redact(value), r.maxFieldRunes)
	case []any:
		out := make([]any, len(value))
		for i, item := range value {
			out[i] = r.redactJSON("", item)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(value))
		for childKey, item := range value {
			out[childKey] = r.redactJSON(childKey, item)
		}
		return out
	default:
		return value
	}
}

func (r *Redactor) redact(value string) string {
	value = privateKeyPattern.ReplaceAllString(value, "[REDACTED:private-key]")
	value = authorizationPattern.ReplaceAllString(value, "${1}[REDACTED:authorization]")
	value = setCookiePattern.ReplaceAllString(value, "${1}[REDACTED:set-cookie]")
	value = cookiePattern.ReplaceAllString(value, "${1}[REDACTED:cookie]")
	value = telegramTokenPattern.ReplaceAllString(value, "[REDACTED:telegram-token]")
	value = githubClassicPattern.ReplaceAllString(value, "[REDACTED:github-token]")
	value = githubFinePattern.ReplaceAllString(value, "[REDACTED:github-token]")
	value = environmentPattern.ReplaceAllStringFunc(value, redactEnvironment)
	for _, secret := range r.secrets {
		value = strings.ReplaceAll(value, secret, "[REDACTED:configured]")
	}
	return value
}

func redactEnvironment(line string) string {
	parts := environmentPattern.FindStringSubmatch(line)
	if len(parts) != 5 {
		return line
	}
	if strings.HasPrefix(parts[4], "[REDACTED:") {
		return line
	}
	return parts[1] + parts[2] + parts[3] + sensitiveEnvironmentLabel(parts[2])
}

func sensitiveKeyLabel(key string) string {
	normalized := strings.NewReplacer("_", "-", " ", "-").Replace(strings.ToLower(key))
	switch normalized {
	case "authorization", "proxy-authorization":
		return "[REDACTED:authorization]"
	case "cookie":
		return "[REDACTED:cookie]"
	case "set-cookie":
		return "[REDACTED:set-cookie]"
	case "openai-api-key":
		return "[REDACTED:openai-api-key]"
	case "anthropic-api-key":
		return "[REDACTED:anthropic-api-key]"
	case "anthropic-auth-token":
		return "[REDACTED:anthropic-auth-token]"
	case "claude-code-oauth-token":
		return "[REDACTED:claude-code-oauth-token]"
	default:
		return ""
	}
}

func sensitiveEnvironmentLabel(name string) string {
	if label := sensitiveKeyLabel(name); label != "" {
		return label
	}
	return "[REDACTED:env]"
}

func truncateRunes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	marker := []rune(truncationMarker)
	if limit <= len(marker) {
		return string(marker[:limit])
	}
	return string(runes[:limit-len(marker)]) + truncationMarker
}

func positiveOr(value, fallback int) int {
	if value > 0 {
		return value
	}
	return fallback
}
