package telegram

import (
	"testing"

	"github.com/berkayahi/agentbridge/internal/task"
)

func TestParseCommandProviderPrompts(t *testing.T) {
	tests := []struct {
		name, input, bot string
		provider         task.Provider
		prompt           string
	}{
		{"codex", "/codex menüyü düzelt", "", task.ProviderCodex, "menüyü düzelt"},
		{"claude suffix multiline", "  /claude@agent_bridge_bot ilk satır\nikinci satır", "agent_bridge_bot", task.ProviderClaude, "ilk satır\nikinci satır"},
		{"caption", "/codex ekran görüntüsünü incele", "", task.ProviderCodex, "ekran görüntüsünü incele"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseCommand(tt.input, tt.bot)
			if err != nil {
				t.Fatal(err)
			}
			if got.Kind != KindPrompt || got.Provider != tt.provider || got.Argument != tt.prompt {
				t.Fatalf("command = %#v", got)
			}
		})
	}
}

func TestParseCommandDirectControls(t *testing.T) {
	tests := []struct {
		input  string
		kind   Kind
		taskID string
	}{
		{"/usage", KindUsage, ""}, {"/status", KindStatus, ""}, {"/tasks", KindTasks, ""},
		{"/sessions", KindSessions, ""}, {"/health", KindHealth, ""},
		{"/diff task_abc-12", KindDiff, "task_abc-12"}, {"/logs task_abc-12", KindLogs, "task_abc-12"},
		{"/cancel task_abc-12", KindCancel, "task_abc-12"}, {"/retry task_abc-12", KindRetry, "task_abc-12"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := ParseCommand(tt.input, "")
			if err != nil {
				t.Fatal(err)
			}
			if got.Kind != tt.kind || got.TaskID != tt.taskID || got.Provider != "" || got.Argument != "" {
				t.Fatalf("control command leaked into provider prompt: %#v", got)
			}
		})
	}
}

func TestParseCommandRejectsInvalidInput(t *testing.T) {
	for _, input := range []string{"", "hello", "/codex", "/unknown x", "/codex@other_bot x", "/diff", "/usage extra", "/cancel ../../etc/passwd"} {
		if _, err := ParseCommand(input, "agent_bridge_bot"); err == nil {
			t.Errorf("ParseCommand(%q) succeeded", input)
		}
	}
}
