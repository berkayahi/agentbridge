package gitref

import "testing"

func TestValidMatchesGitReferenceRules(t *testing.T) {
	for _, value := range []string{
		"refs/heads/main",
		"refs/tags/v2.0.0",
		"refs/heads/agentbridge/task-1",
	} {
		if !Valid(value) {
			t.Fatalf("Valid(%q) = false", value)
		}
	}
	for _, value := range []string{
		"main", "refs/heads/", "refs//heads/main", "refs/.heads/main",
		"refs/heads/main.", "refs/heads/main.lock", "refs/heads/main..next",
		"refs/heads/main\nbad", "refs/heads/main?bad", "refs/heads/@{bad",
	} {
		if Valid(value) {
			t.Fatalf("Valid(%q) = true", value)
		}
	}
}
