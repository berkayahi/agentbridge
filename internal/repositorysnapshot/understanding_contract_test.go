package repositorysnapshot

import (
	"strings"
	"testing"
)

func TestRolePromptsAndValidationRequireExactCoverage(t *testing.T) {
	for _, role := range []AnalysisRole{RoleCartographer, RoleProductArchaeologist, RoleQualityOperations, RoleSynthesizer} {
		prompt := analysisPrompt(role)
		for _, area := range RequiredCoverage(role) {
			if !strings.Contains(prompt, area) {
				t.Fatalf("%s prompt omitted required area %q", role, area)
			}
		}
		for _, term := range []string{"observed", "declared", "inferred", "unknown", "conflicting", "evidence_paths"} {
			if !strings.Contains(prompt, term) {
				t.Fatalf("%s prompt omitted contract term %q", role, term)
			}
		}
		output := validContractOutput(role)
		if err := validateStructuredOutput(output, role, []EvidenceReference{{Path: "README.md", ContentDigest: "sha256:evidence", Size: 8}}); err != nil {
			t.Fatalf("%s valid contract rejected: %v", role, err)
		}
		output.Findings = output.Findings[:len(output.Findings)-1]
		if err := validateStructuredOutput(output, role, []EvidenceReference{{Path: "README.md"}}); err != ErrProviderOutput {
			t.Fatalf("%s missing coverage error = %v", role, err)
		}
	}
	if prompt := analysisPrompt(RoleSynthesizer); !strings.Contains(prompt, "m1-snapshot.json") || !strings.Contains(prompt, "separate prior role file") || !strings.Contains(prompt, "never convert inference into fact") {
		t.Fatalf("synthesizer deterministic input contract is incomplete: %q", prompt)
	}
}

func TestTypedCapabilitiesPreserveAllStatusesAndRequireEvidenceForClaims(t *testing.T) {
	statuses := []CapabilityImplementationStatus{CapabilityVerified, CapabilityPartial, CapabilityAbsent, CapabilityUnknown, CapabilityConflicting}
	for _, status := range statuses {
		output := validContractOutput(RoleProductArchaeologist)
		confidence := 0.8
		output.Capabilities = []Capability{{
			ID: "capability-" + string(status), Name: "Repository capability", Actor: "operator",
			VerifiedBehavior: "The operator can exercise the repository behavior.", ImplementationStatus: status,
			KnowledgeState: KnowledgeObserved, EvidencePaths: []string{"README.md"}, Confidence: &confidence,
		}}
		if err := validateStructuredOutput(output, output.Role, []EvidenceReference{{Path: "README.md"}}); err != nil {
			t.Fatalf("status %s rejected: %v", status, err)
		}
	}

	output := validContractOutput(RoleCartographer)
	output.Findings[0].KnowledgeState = KnowledgeObserved
	output.Findings[0].EvidencePaths = nil
	if err := validateStructuredOutput(output, output.Role, []EvidenceReference{{Path: "README.md"}}); err != ErrProviderOutput {
		t.Fatalf("observed finding without evidence error = %v", err)
	}
	output = validContractOutput(RoleProductArchaeologist)
	output.Capabilities = []Capability{{
		ID: "declared-capability", Name: "Declared capability", Actor: "visitor", VerifiedBehavior: "Documentation declares the behavior.",
		ImplementationStatus: CapabilityUnknown, KnowledgeState: KnowledgeDeclared,
	}}
	if err := validateStructuredOutput(output, output.Role, []EvidenceReference{{Path: "README.md"}}); err != ErrProviderOutput {
		t.Fatalf("declared capability without evidence error = %v", err)
	}
}

func validContractOutput(role AnalysisRole) StructuredOutput {
	areas := RequiredCoverage(role)
	findings := make([]Finding, 0, len(areas))
	for _, area := range areas {
		findings = append(findings, Finding{
			ID: "coverage-" + area, Area: area, Statement: "Evidence is insufficient for area " + area,
			KnowledgeState: KnowledgeUnknown,
		})
	}
	return StructuredOutput{
		Role: role, Findings: findings, Capabilities: []Capability{},
		Assumptions: []string{}, Conflicts: []string{}, Unknowns: []string{},
	}
}
