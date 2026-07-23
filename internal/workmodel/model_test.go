package workmodel

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
	"unicode/utf8"
)

func TestPayloadsUseRawJSONAndDomainHasNoSerializationTags(t *testing.T) {
	approvalType := reflect.TypeOf(Approval{})
	for _, fieldName := range []string{"RequestPayload", "DecisionPayload"} {
		field, ok := approvalType.FieldByName(fieldName)
		if !ok || field.Type != reflect.TypeOf(json.RawMessage{}) {
			t.Fatalf("Approval.%s must use json.RawMessage", fieldName)
		}
	}

	for _, value := range []any{Task{}, Event{}, Attachment{}, Session{}, Approval{}} {
		typeOf := reflect.TypeOf(value)
		for i := range typeOf.NumField() {
			if tag := typeOf.Field(i).Tag; tag != "" {
				t.Fatalf("%s.%s leaks serialization tag %q into domain", typeOf.Name(), typeOf.Field(i).Name, tag)
			}
		}
	}
}

func TestTitleNormalizesWhitespaceAndTruncatesByRunes(t *testing.T) {
	got := Title("  Şimdi\n  güzel\tbir   görev başlığı  ", 12)
	if got != "Şimdi güzel…" {
		t.Fatalf("Title() = %q, want %q", got, "Şimdi güzel…")
	}
	if !utf8.ValidString(got) {
		t.Fatalf("Title() returned invalid UTF-8: %q", got)
	}
}

func TestTitleUsesSensibleDefaultLimit(t *testing.T) {
	got := Title("short title", 0)
	if got != "short title" {
		t.Fatalf("Title() = %q, want short title", got)
	}
	if DefaultTitleRunes < 40 || DefaultTitleRunes > 120 {
		t.Fatalf("DefaultTitleRunes = %d, want a practical display limit", DefaultTitleRunes)
	}
}

func TestElapsed(t *testing.T) {
	base := time.Date(2026, time.July, 14, 8, 0, 0, 0, time.FixedZone("TRT", 3*60*60))
	now := base.Add(15 * time.Minute)

	tests := []struct {
		name string
		task Task
		want time.Duration
	}{
		{name: "queued", task: Task{CreatedAt: base}, want: 15 * time.Minute},
		{name: "running", task: Task{CreatedAt: base, StartedAt: ptrTime(base.Add(2 * time.Minute))}, want: 13 * time.Minute},
		{name: "finished", task: Task{CreatedAt: base, StartedAt: ptrTime(base.Add(2 * time.Minute)), FinishedAt: ptrTime(base.Add(7 * time.Minute))}, want: 5 * time.Minute},
		{name: "future clock clamps to zero", task: Task{CreatedAt: now}, want: 0},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.task.Elapsed(now); got != test.want {
				t.Fatalf("Elapsed() = %s, want %s", got, test.want)
			}
		})
	}
}

func ptrTime(value time.Time) *time.Time { return &value }
