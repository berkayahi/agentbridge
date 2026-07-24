package localcontrol

import (
	"errors"
	"testing"
)

func TestParseIfMatchRevision(t *testing.T) {
	for value, want := range map[string]int64{
		"":       0,
		`"1"`:    1,
		` "42" `: 42,
	} {
		got, err := parseIfMatchRevision(value)
		if err != nil || got != want {
			t.Fatalf("parseIfMatchRevision(%q) = %d, %v; want %d", value, got, err, want)
		}
	}
	for _, value := range []string{`*`, `1`, `W/"1"`, `"0"`, `"not-a-revision"`, `"1","2"`} {
		if _, err := parseIfMatchRevision(value); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("parseIfMatchRevision(%q) = %v, want ErrInvalidRequest", value, err)
		}
	}
}
