package security

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestLogHandlerRedactsMessagesErrorsConfiguredSecretsAndURLs(t *testing.T) {
	const (
		token = "123456789:ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghi"
		code  = "ZXCV-ASDF"
		url   = "https://auth.example/device/private?code=ZXCV-ASDF"
	)
	var output bytes.Buffer
	handler := NewLogHandler(
		slog.NewTextHandler(&output, nil),
		NewRedactor(Config{Secrets: []string{code}}),
	)
	logger := slog.New(handler)
	logger.LogAttrs(context.Background(), slog.LevelError, "failed at "+url,
		slog.Any("error", errors.New("provider returned "+token+" via "+url)),
		slog.Group("detail", slog.String("callback", code)),
	)

	got := output.String()
	for _, secret := range []string{token, code, url} {
		if strings.Contains(got, secret) {
			t.Fatalf("log leaked %q: %s", secret, got)
		}
	}
	for _, marker := range []string{"[REDACTED:telegram-token]", "[REDACTED:url]", "[REDACTED:configured]"} {
		if !strings.Contains(got, marker) {
			t.Fatalf("log missing %q: %s", marker, got)
		}
	}
}
