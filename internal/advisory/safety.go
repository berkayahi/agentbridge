package advisory

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/berkayahi/agentbridge/internal/security"
)

// The safety pipeline is the single advisory boundary for data entering or
// leaving a provider. It intentionally keeps its vocabulary generic: the
// public engine does not know about Kovan decisions or UI concepts.
const (
	defaultSafetyMaxInputBytes = 512 << 10
	defaultSafetyMaxDepth      = 32
	defaultSafetyMaxNodes      = 8192
	defaultSafetyMaxExpansion  = 2 << 20
	defaultStreamOverlapBytes  = 4096
)

var (
	ErrUnsafeContent       = errors.New("advisory: unsafe content")
	ErrMalformedPayload    = errors.New("advisory: malformed payload")
	ErrSafetyBounds        = errors.New("advisory: safety bounds exceeded")
	errNotJSON             = errors.New("advisory: value is not a JSON document")
	errMalformedJSON       = errors.New("advisory: malformed JSON document")
	errDuplicateJSONKey    = errors.New("advisory: duplicate JSON key")
	errTrailingJSON        = errors.New("advisory: trailing JSON")
	errUnexpectedJSONToken = errors.New("advisory: unexpected JSON token")
)

var (
	privateKeySafetyPattern = regexp.MustCompile(`(?is)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)
	credentialSafetyPattern = regexp.MustCompile(`(?i)(?:api[_ -]?key|access[_ -]?token|refresh[_ -]?token|client[_ -]?secret|password|passwd|secret|credential|authorization|cookie|private[_ -]?key)\s*[:=]\s*(?:"[^"]+"|'[^']+'|[^\s,}\]\[]{8,})`)
	bearerSafetyPattern     = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{12,}`)
	knownTokenSafetyPattern = regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|sk-[A-Za-z0-9_-]{20,}|AKIA[0-9A-Z]{16}|[0-9]{8,12}:[A-Za-z0-9_-]{30,})\b`)
	opaqueSecretPattern     = regexp.MustCompile(`(?i)\bsecret[-_ ]?(?:value|material|data)\b`)
)

// SafetyConfig makes all canonicalization limits explicit. Zero values select
// the conservative defaults above.
type SafetyConfig struct {
	MaxInputBytes int
	MaxDepth      int
	MaxNodes      int
	MaxExpansion  int
	StreamOverlap int
}

// SafetyPipeline performs complete-payload inspection and bounded recursive
// canonicalization. A redactor is an in-memory dependency only; raw configured
// values are never returned by this package.
type SafetyPipeline struct {
	redactor      *security.Redactor
	maxInput      int
	maxDepth      int
	maxNodes      int
	maxExpansion  int
	streamOverlap int
}

func NewSafetyPipeline(redactor *security.Redactor, config SafetyConfig) *SafetyPipeline {
	if config.MaxInputBytes <= 0 {
		config.MaxInputBytes = defaultSafetyMaxInputBytes
	}
	if config.MaxDepth <= 0 {
		config.MaxDepth = defaultSafetyMaxDepth
	}
	if config.MaxNodes <= 0 {
		config.MaxNodes = defaultSafetyMaxNodes
	}
	if config.MaxExpansion <= 0 {
		config.MaxExpansion = defaultSafetyMaxExpansion
	}
	if config.StreamOverlap <= 0 {
		config.StreamOverlap = defaultStreamOverlapBytes
	}
	return &SafetyPipeline{
		redactor: redactor, maxInput: config.MaxInputBytes, maxDepth: config.MaxDepth,
		maxNodes: config.MaxNodes, maxExpansion: config.MaxExpansion, streamOverlap: config.StreamOverlap,
	}
}

// SanitizeText scans a complete bounded string and only then applies the
// optional display bound. A secret after the display bound is still detected.
func (p *SafetyPipeline) SanitizeText(value string, displayLimit int) (string, error) {
	if p == nil || len(value) > p.maxInput || !utf8.ValidString(value) {
		return "", ErrSafetyBounds
	}
	state := newSafetyWalker(p, true)
	safe, _, err := state.text(value, 0, true)
	if err != nil {
		return "", err
	}
	if displayLimit > 0 {
		safe = truncateSafetyRunes(safe, displayLimit)
	}
	return safe, nil
}

// SanitizeJSON canonicalizes a bounded JSON document and redacts only
// high-confidence/configured secrets. Unknown suspicious content fails closed.
// It is intended for provider-bound context, never for provider output.
func (p *SafetyPipeline) SanitizeJSON(payload []byte) ([]byte, error) {
	return p.processJSON(payload, true)
}

// InspectJSON validates a complete JSON document without changing its
// representation. It is used for caller-owned schemas whose digest must stay
// bound to the original bytes.
func (p *SafetyPipeline) InspectJSON(payload []byte) error {
	_, err := p.processJSON(payload, false)
	return err
}

// RejectJSON validates a complete JSON document without redacting it. It is
// used for provider output, schemas, receipts, and any value that must either
// be clean as received or cause a non-secret failure.
func (p *SafetyPipeline) RejectJSON(payload []byte) ([]byte, error) {
	return p.processJSON(payload, false)
}

func (p *SafetyPipeline) processJSON(payload []byte, redact bool) ([]byte, error) {
	if p == nil || len(payload) == 0 || len(payload) > p.maxInput {
		return nil, ErrSafetyBounds
	}
	if !utf8.Valid(payload) {
		return nil, ErrMalformedPayload
	}
	state := newSafetyWalker(p, redact)
	value, err := state.decode(payload)
	if err != nil {
		if errors.Is(err, errNotJSON) || errors.Is(err, errMalformedJSON) || errors.Is(err, errTrailingJSON) || errors.Is(err, errDuplicateJSONKey) || errors.Is(err, errUnexpectedJSONToken) {
			return nil, ErrMalformedPayload
		}
		return nil, err
	}
	safe, _, err := state.value(value, 0)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(safe)
	if err != nil || len(encoded) > p.maxInput {
		return nil, ErrSafetyBounds
	}
	return encoded, nil
}

// StreamGuard performs a bounded-overlap scan while data arrives and keeps a
// bounded complete copy for the final canonical pass. The complete copy is
// the guarantee for long encodings; overlap makes split credential markers
// fail as early as possible without trusting chunk boundaries.
type StreamGuard struct {
	pipeline *SafetyPipeline
	buffer   []byte
	tail     []byte
	failed   bool
}

func (p *SafetyPipeline) NewStream() *StreamGuard {
	return &StreamGuard{pipeline: p}
}

func (s *StreamGuard) Feed(chunk []byte) error {
	if s == nil || s.pipeline == nil || s.failed || len(s.buffer)+len(chunk) > s.pipeline.maxInput {
		if s != nil {
			s.failed = true
		}
		return ErrSafetyBounds
	}
	window := make([]byte, 0, len(s.tail)+len(chunk))
	window = append(window, s.tail...)
	window = append(window, chunk...)
	if suspiciousBytes(s.pipeline.redactor, window) {
		s.failed = true
		return ErrUnsafeContent
	}
	s.buffer = append(s.buffer, chunk...)
	if len(window) > s.pipeline.streamOverlap {
		s.tail = append(s.tail[:0], window[len(window)-s.pipeline.streamOverlap:]...)
	} else {
		s.tail = append(s.tail[:0], window...)
	}
	return nil
}

func (s *StreamGuard) FinishJSON() ([]byte, error) {
	if s == nil || s.pipeline == nil || s.failed {
		return nil, ErrUnsafeContent
	}
	return s.pipeline.RejectJSON(s.buffer)
}

type safetyWalker struct {
	pipeline *SafetyPipeline
	redact   bool
	nodes    int
	expanded int
}

func newSafetyWalker(pipeline *SafetyPipeline, redact bool) *safetyWalker {
	return &safetyWalker{pipeline: pipeline, redact: redact}
}

func (w *safetyWalker) decode(payload []byte) (any, error) {
	w.expanded += len(payload)
	if w.expanded > w.pipeline.maxExpansion {
		return nil, ErrSafetyBounds
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	value, err := w.decodeValue(decoder, 0)
	if err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return nil, errTrailingJSON
	} else if !errors.Is(err, io.EOF) {
		return nil, errMalformedJSON
	}
	return value, nil
}

func (w *safetyWalker) decodeValue(decoder *json.Decoder, depth int) (any, error) {
	if depth > w.pipeline.maxDepth {
		return nil, ErrSafetyBounds
	}
	w.nodes++
	if w.nodes > w.pipeline.maxNodes {
		return nil, ErrSafetyBounds
	}
	token, err := decoder.Token()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errNotJSON
		}
		return nil, errMalformedJSON
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return token, nil
	}
	switch delimiter {
	case '{':
		object := make(map[string]any)
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, errMalformedJSON
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, errUnexpectedJSONToken
			}
			if _, exists := seen[key]; exists {
				return nil, errDuplicateJSONKey
			}
			seen[key] = struct{}{}
			child, err := w.decodeValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			object[key] = child
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return nil, errMalformedJSON
		}
		return object, nil
	case '[':
		array := make([]any, 0)
		for decoder.More() {
			child, err := w.decodeValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			array = append(array, child)
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return nil, errMalformedJSON
		}
		return array, nil
	default:
		return nil, errUnexpectedJSONToken
	}
}

func (w *safetyWalker) value(value any, depth int) (any, bool, error) {
	if depth > w.pipeline.maxDepth {
		return nil, false, ErrSafetyBounds
	}
	switch current := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(current))
		changed := false
		for key, child := range current {
			if safetyKey(key) {
				return nil, false, ErrUnsafeContent
			}
			safeChild, childChanged, err := w.value(child, depth+1)
			if err != nil {
				return nil, false, err
			}
			result[key] = safeChild
			changed = changed || childChanged
		}
		return result, changed, nil
	case []any:
		result := make([]any, len(current))
		changed := false
		for index, child := range current {
			safeChild, childChanged, err := w.value(child, depth+1)
			if err != nil {
				return nil, false, err
			}
			result[index] = safeChild
			changed = changed || childChanged
		}
		return result, changed, nil
	case string:
		safe, childChanged, err := w.text(current, depth, true)
		return safe, childChanged, err
	default:
		return current, false, nil
	}
}

func (w *safetyWalker) text(value string, depth int, allowNested bool) (string, bool, error) {
	if !utf8.ValidString(value) {
		return "", false, ErrUnsafeContent
	}
	safe := value
	changed := false
	if w.pipeline.redactor != nil {
		redacted := w.pipeline.redactor.RedactKnown(safe)
		if redacted != safe {
			if !w.redact {
				return "", false, ErrUnsafeContent
			}
			safe, changed = redacted, true
		}
	}
	if suspiciousText(safe) {
		return "", false, ErrUnsafeContent
	}
	if !allowNested || depth >= w.pipeline.maxDepth || strings.HasPrefix(strings.TrimSpace(safe), "[REDACTED:") || !looksLikeJSON(safe) {
		return safe, changed, nil
	}
	nested, err := w.decode([]byte(safe))
	if err != nil {
		if errors.Is(err, errNotJSON) {
			return safe, changed, nil
		}
		return "", false, err
	}
	safeNested, nestedChanged, err := w.value(nested, depth+1)
	if err != nil {
		return "", false, err
	}
	if !nestedChanged {
		return safe, changed, nil
	}
	encoded, err := json.Marshal(safeNested)
	if err != nil || len(encoded) > w.pipeline.maxInput {
		return "", false, ErrSafetyBounds
	}
	return string(encoded), true, nil
}

func suspiciousBytes(redactor *security.Redactor, value []byte) bool {
	return suspiciousTextWithRedactor(redactor, string(value))
}

func suspiciousText(value string) bool {
	return suspiciousTextWithRedactor(nil, value)
}

func suspiciousTextWithRedactor(redactor *security.Redactor, value string) bool {
	if redactor != nil && redactor.ContainsKnown(value) {
		return true
	}
	return privateKeySafetyPattern.MatchString(value) || credentialSafetyPattern.MatchString(value) || bearerSafetyPattern.MatchString(value) || knownTokenSafetyPattern.MatchString(value) || opaqueSecretPattern.MatchString(value)
}

func safetyKey(value string) bool {
	normalized := strings.ToLower(strings.TrimSpace(value))
	compact := strings.NewReplacer("_", "", "-", "", " ", "", ".", "").Replace(normalized)
	switch compact {
	case "password", "passwd", "secret", "credential", "authorization", "cookie", "setcookie", "apikey", "accesskey", "privatekey", "oauthsecret", "clientsecret", "accesstoken", "refreshtoken", "authtoken", "bearertoken":
		return true
	}
	if strings.HasSuffix(compact, "secret") || strings.HasSuffix(compact, "password") || strings.HasSuffix(compact, "credential") {
		return true
	}
	if strings.HasSuffix(compact, "token") && !strings.Contains(compact, "budget") && !strings.Contains(compact, "count") && !strings.Contains(compact, "limit") {
		return true
	}
	return false
}

func looksLikeJSON(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	switch value[0] {
	case '{', '[', '"':
		return true
	default:
		return false
	}
}

func truncateSafetyRunes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	const marker = "…[TRUNCATED]"
	markerRunes := []rune(marker)
	if limit <= len(markerRunes) {
		return string(markerRunes[:limit])
	}
	return string(runes[:limit-len(markerRunes)]) + marker
}
