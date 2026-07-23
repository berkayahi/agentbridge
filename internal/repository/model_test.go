package repository

import (
	"strings"
	"testing"
	"time"
)

func TestCheckpointRequiresGitObjectAndExactRemoteRef(t *testing.T) {
	binding, err := NewBinding(BindingInput{
		ID:        "repository-1",
		RemoteURL: "ssh://git@example.invalid/team/repository.git",
		CreatedAt: time.Unix(1_000, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("a", 40)
	checkpoint, err := NewCheckpoint(CheckpointInput{
		ID:           "checkpoint-1",
		RepositoryID: binding.ID(),
		CommitSHA:    sha,
		RemoteRef:    "refs/heads/agentbridge/task-1",
		CreatedAt:    time.Unix(1_001, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.CommitSHA() != sha || checkpoint.RemoteRef() != "refs/heads/agentbridge/task-1" {
		t.Fatalf("checkpoint = %#v", checkpoint)
	}
	for _, input := range []CheckpointInput{
		{ID: "checkpoint-2", RepositoryID: binding.ID(), CommitSHA: "abc", RemoteRef: "refs/heads/main", CreatedAt: time.Unix(1_001, 0).UTC()},
		{ID: "checkpoint-3", RepositoryID: binding.ID(), CommitSHA: strings.Repeat("a", 64), RemoteRef: "main", CreatedAt: time.Unix(1_001, 0).UTC()},
		{ID: "checkpoint-4", RepositoryID: binding.ID(), CommitSHA: strings.Repeat("g", 40), RemoteRef: "refs/heads/main", CreatedAt: time.Unix(1_001, 0).UTC()},
		{ID: "checkpoint-5", RepositoryID: binding.ID(), CommitSHA: strings.Repeat("a", 40), RemoteRef: "refs/heads/", CreatedAt: time.Unix(1_001, 0).UTC()},
		{ID: "checkpoint-6", RepositoryID: binding.ID(), CommitSHA: strings.Repeat("a", 40), RemoteRef: "refs//heads/main", CreatedAt: time.Unix(1_001, 0).UTC()},
		{ID: "checkpoint-7", RepositoryID: binding.ID(), CommitSHA: strings.Repeat("a", 40), RemoteRef: "refs/heads/main.lock", CreatedAt: time.Unix(1_001, 0).UTC()},
		{ID: "checkpoint-8", RepositoryID: binding.ID(), CommitSHA: strings.Repeat("a", 40), RemoteRef: "refs/heads/main\nbad", CreatedAt: time.Unix(1_001, 0).UTC()},
	} {
		if _, err := NewCheckpoint(input); err == nil {
			t.Fatalf("NewCheckpoint(%#v) succeeded", input)
		}
	}
}

func FuzzCheckpointRejectsInvalidObjectIDs(f *testing.F) {
	f.Add("")
	f.Add(strings.Repeat("a", 39))
	f.Add(strings.Repeat("a", 65))
	f.Add(strings.Repeat("Δ", 40))
	f.Fuzz(func(t *testing.T, sha string) {
		_, err := NewCheckpoint(CheckpointInput{
			ID:           "checkpoint-1",
			RepositoryID: "repository-1",
			CommitSHA:    sha,
			RemoteRef:    "refs/heads/main",
			CreatedAt:    time.Unix(1_000, 0).UTC(),
		})
		if !gitObjectID(sha) && err == nil {
			t.Fatalf("NewCheckpoint accepted invalid SHA %q", sha)
		}
	})
}

func gitObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, runeValue := range value {
		if !((runeValue >= 'a' && runeValue <= 'f') || (runeValue >= 'A' && runeValue <= 'F') || (runeValue >= '0' && runeValue <= '9')) {
			return false
		}
	}
	return true
}
