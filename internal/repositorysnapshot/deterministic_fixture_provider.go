package repositorysnapshot

import (
	"context"
	"encoding/json"
)

// DeterministicFixtureProvider is a compiled acceptance provider. It owns no
// filesystem, network, process, credential, or runtime-inspection dependency;
// Analyze derives its output only from the typed request value.
type DeterministicFixtureProvider struct {
	providerID string
	model      string
}

func NewDeterministicFixtureProvider(providerID, model string) DeterministicFixtureProvider {
	return DeterministicFixtureProvider{providerID: providerID, model: model}
}

func (p DeterministicFixtureProvider) Analyze(ctx context.Context, request ProviderRequest) (ProviderResult, error) {
	if err := ctx.Err(); err != nil {
		return ProviderResult{}, err
	}
	if !request.Role.Valid() || len(request.Evidence) == 0 {
		return ProviderResult{}, ErrEvidenceMissing
	}
	evidencePath := request.Evidence[0].Path
	areas := RequiredCoverage(request.Role)
	findings := make([]Finding, 0, len(areas))
	states := []KnowledgeState{KnowledgeObserved, KnowledgeDeclared, KnowledgeInferred, KnowledgeUnknown, KnowledgeConflicting}
	for index, area := range areas {
		state := KnowledgeUnknown
		if request.Role == RoleSynthesizer {
			state = states[index%len(states)]
		} else if index == 0 {
			state = KnowledgeObserved
		}
		finding := Finding{
			ID:   "fixture-" + string(request.Role) + "-" + area,
			Area: area, Statement: "Deterministic fixture result for " + area + ".",
			KnowledgeState: state,
		}
		if state == KnowledgeObserved || state == KnowledgeDeclared {
			finding.EvidencePaths = []string{evidencePath}
		}
		findings = append(findings, finding)
	}

	capabilities := deterministicFixtureCapabilities(request.Role, evidencePath)
	output, err := json.Marshal(StructuredOutput{
		Role: request.Role, Findings: findings, Capabilities: capabilities,
		Assumptions: []string{"Fixture output is deterministic and is not a production analysis."},
		Conflicts:   []string{"Fixture conflict remains explicit."},
		Unknowns:    []string{"Fixture unknown remains explicit."},
	})
	if err != nil {
		return ProviderResult{}, err
	}
	return ProviderResult{ProviderID: p.providerID, Model: p.model, Output: output}, nil
}

func deterministicFixtureCapabilities(role AnalysisRole, evidencePath string) []Capability {
	if role != RoleSynthesizer {
		confidence := 1.0
		return []Capability{{
			ID: "fixture-" + string(role) + "-capability", Name: "Deterministic fixture capability", Actor: "fixture operator",
			VerifiedBehavior: "The compiled fixture emits a bounded role result.", ImplementationStatus: CapabilityVerified,
			KnowledgeState: KnowledgeObserved, EvidencePaths: []string{evidencePath}, Confidence: &confidence,
		}}
	}
	statuses := []CapabilityImplementationStatus{CapabilityVerified, CapabilityPartial, CapabilityAbsent, CapabilityUnknown, CapabilityConflicting}
	states := []KnowledgeState{KnowledgeObserved, KnowledgeDeclared, KnowledgeInferred, KnowledgeUnknown, KnowledgeConflicting}
	result := make([]Capability, 0, len(statuses))
	for index, status := range statuses {
		confidence := float64(len(statuses)-index) / float64(len(statuses))
		capability := Capability{
			ID: "fixture-synthesizer-capability-" + string(status), Name: "Fixture capability " + string(status), Actor: "fixture operator",
			VerifiedBehavior:     "The fixture preserves the " + string(status) + " implementation state.",
			ImplementationStatus: status, KnowledgeState: states[index], Confidence: &confidence,
		}
		if capability.KnowledgeState == KnowledgeObserved || capability.KnowledgeState == KnowledgeDeclared {
			capability.EvidencePaths = []string{evidencePath}
		}
		result = append(result, capability)
	}
	return result
}
