package telegram

import (
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/workmodel"
)

func TestParseCommandProviderPrompts(t *testing.T) {
	tests := []struct {
		name, input, bot string
		provider         workmodel.Provider
		prompt           string
	}{
		{"codex", "/codex menüyü düzelt", "", workmodel.CodexSubscription, "menüyü düzelt"},
		{"claude suffix multiline", "  /claude@agent_bridge_bot ilk satır\nikinci satır", "agent_bridge_bot", workmodel.ClaudeSubscription, "ilk satır\nikinci satır"},
		{"caption", "/codex ekran görüntüsünü incele", "", workmodel.CodexSubscription, "ekran görüntüsünü incele"},
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

func TestParseUpdateTurnsSignedApprovalCallbackIntoBoundedCommand(t *testing.T) {
	now := time.Unix(1_000, 0)
	signer := NewCallbackSigner([]byte("a sufficiently long signing secret"), func() time.Time { return now })
	token, err := signer.Sign(CallbackAction{Action: "approve", TaskID: "task-1", ApprovalID: "approval-1"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	command, err := ParseUpdate(Update{ID: 9, Callback: &CallbackQuery{ID: "callback-1", Data: token}}, "", signer)
	if err != nil {
		t.Fatal(err)
	}
	if command.Kind != KindApprove || command.TaskID != "task-1" || command.ApprovalID != "approval-1" || command.CallbackID != "callback-1" {
		t.Fatalf("command = %#v", command)
	}
}

func TestParseUpdateRejectsUnsignedUnknownAndMalformedCallbacks(t *testing.T) {
	signer := NewCallbackSigner([]byte("a sufficiently long signing secret"), time.Now)
	for _, update := range []Update{
		{},
		{Callback: &CallbackQuery{ID: "callback", Data: "approve|task|approval"}},
		{Message: &IncomingMessage{Text: "/status"}, Callback: &CallbackQuery{ID: "callback", Data: "ignored"}},
	} {
		if _, err := ParseUpdate(update, "", signer); err == nil {
			t.Errorf("update accepted: %#v", update)
		}
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

func TestParseCommandProviderUsageDoesNotBecomePrompt(t *testing.T) {
	for _, name := range []string{"codex", "claude"} {
		got, err := ParseCommand("/"+name+" usage", "")
		if err != nil {
			t.Fatalf("ParseCommand(%q): %v", name, err)
		}
		if got.Kind != KindUsage || string(got.Provider) != name || got.Argument != "" {
			t.Fatalf("ParseCommand(%q) = %#v", name, got)
		}
	}
}

func TestParseCommandRejectsInvalidInput(t *testing.T) {
	for _, input := range []string{"", "hello", "/codex", "/unknown x", "/codex@other_bot x", "/diff", "/usage extra", "/cancel ../../etc/passwd"} {
		if _, err := ParseCommand(input, "agent_bridge_bot"); err == nil {
			t.Errorf("ParseCommand(%q) succeeded", input)
		}
	}
}
