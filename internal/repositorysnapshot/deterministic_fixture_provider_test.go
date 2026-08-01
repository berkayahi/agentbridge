package repositorysnapshot_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/berkayahi/agentbridge/internal/repositorysnapshot"
)

func TestDeterministicFixtureProviderUsesOnlyTypedEvidenceMetadata(t *testing.T) {
	provider := repositorysnapshot.NewDeterministicFixtureProvider("agentbridge-fixture", "deterministic-v1")
	request := repositorysnapshot.ProviderRequest{
		Role: repositorysnapshot.RoleSynthesizer, ExactCommitSHA: "0123456789abcdef0123456789abcdef01234567",
		WorkspacePath: "/path/that/does/not/exist", Prompt: "ignored deterministic prompt",
		Evidence: []repositorysnapshot.EvidenceReference{{Path: "m1-snapshot.json", ContentDigest: "sha256:fixture", Size: 1}},
	}
	first, err := provider.Analyze(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := provider.Analyze(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if string(first.Output) != string(second.Output) || first.ProviderID != "agentbridge-fixture" || first.Model != "deterministic-v1" {
		t.Fatalf("fixture output is not deterministic: first=%#v second=%#v", first, second)
	}
	var output repositorysnapshot.StructuredOutput
	if err := json.Unmarshal(first.Output, &output); err != nil {
		t.Fatal(err)
	}
	if len(output.Findings) != len(repositorysnapshot.RequiredCoverage(repositorysnapshot.RoleSynthesizer)) || len(output.Capabilities) != 5 {
		t.Fatalf("fixture coverage = %#v", output)
	}
}
