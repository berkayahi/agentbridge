package execution

import (
	"strings"
	"testing"
	"time"
)

func FuzzExecutionRejectsInvalidIdentifiers(f *testing.F) {
	f.Add("")
	f.Add(strings.Repeat("x", 129))
	f.Add("execution-Δ")
	f.Fuzz(func(t *testing.T, id string) {
		_, err := New(NewInput{
			ID:                   id,
			LocalTaskID:          "task-1",
			SessionID:            "session-1",
			RuntimeID:            "codex",
			RepositoryID:         "repository-1",
			CommandID:            "command-1",
			FencingEpoch:         1,
			MaxTransientAttempts: 1,
			PolicySnapshot:       []byte("policy"),
			CreatedAt:            time.Unix(1_000, 0).UTC(),
			UpdatedAt:            time.Unix(1_000, 0).UTC(),
		})
		if id == "" || len(id) > 128 || !asciiIdentifier(id) {
			if err == nil {
				t.Fatalf("New accepted invalid identifier %q", id)
			}
		}
	})
}

func asciiIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for _, runeValue := range value {
		if !((runeValue >= 'a' && runeValue <= 'z') ||
			(runeValue >= 'A' && runeValue <= 'Z') ||
			(runeValue >= '0' && runeValue <= '9') ||
			runeValue == '-' || runeValue == '_') {
			return false
		}
	}
	return true
}
