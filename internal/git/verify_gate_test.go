package git

import (
	"context"
	"errors"
	"testing"
)

// Verification runs the repository's own configured checks inside the isolated
// worktree and performs no external effect, so it must not be gated on delivery
// being enabled. A keeper who has not opted into pushing still needs to know
// whether the work passes.
func TestVerifyRunsWithDeliveryDisabled(t *testing.T) {
	_, profile, workspace := preparedDeliveryFixture(t)
	profile.Enabled = false
	verified := false
	delivery := Delivery{Git: &recordingGit{}, Verifier: verifierFunc(func(context.Context, string) error {
		verified = true
		return nil
	})}
	if err := delivery.Verify(context.Background(), DeliveryRequest{
		Profile: profile, Workspace: workspace, CommitMessage: "chore: verify only",
	}); err != nil {
		t.Fatalf("verify with delivery disabled = %v, want success", err)
	}
	if !verified {
		t.Fatal("the configured verification never ran")
	}
}

// Committing writes to the repository, so it stays behind the delivery opt-in.
func TestCommitStillRequiresDeliveryEnabled(t *testing.T) {
	_, profile, workspace := preparedDeliveryFixture(t)
	profile.Enabled = false
	delivery := Delivery{Git: &recordingGit{}, Verifier: verifierFunc(func(context.Context, string) error { return nil })}
	if _, err := delivery.Commit(context.Background(), DeliveryRequest{
		Profile: profile, Workspace: workspace, CommitMessage: "chore: should not commit",
	}); !errors.Is(err, ErrDeliveryDisabled) {
		t.Fatalf("commit with delivery disabled = %v, want ErrDeliveryDisabled", err)
	}
}

// A failing check is still a failing verification.
func TestVerifyReportsCheckFailureWithDeliveryDisabled(t *testing.T) {
	_, profile, workspace := preparedDeliveryFixture(t)
	profile.Enabled = false
	want := errors.New("go test failed")
	delivery := Delivery{Git: &recordingGit{}, Verifier: verifierFunc(func(context.Context, string) error { return want })}
	err := delivery.Verify(context.Background(), DeliveryRequest{
		Profile: profile, Workspace: workspace, CommitMessage: "chore: verify only",
	})
	if !errors.Is(err, ErrVerificationFailed) {
		t.Fatalf("verify = %v, want ErrVerificationFailed", err)
	}
}

// Verification still refuses an unsafe request: without a verifier there is
// nothing to gather evidence with.
func TestVerifyStillRequiresAVerifier(t *testing.T) {
	_, profile, workspace := preparedDeliveryFixture(t)
	profile.Enabled = false
	delivery := Delivery{Git: &recordingGit{}}
	if err := delivery.Verify(context.Background(), DeliveryRequest{
		Profile: profile, Workspace: workspace, CommitMessage: "chore: verify only",
	}); !errors.Is(err, ErrUnsafeDelivery) {
		t.Fatalf("verify without a verifier = %v, want ErrUnsafeDelivery", err)
	}
}
