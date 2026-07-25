package localcontrol_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/localcontrol"
	"github.com/berkayahi/agentbridge/internal/store/sqlite"
)

type recordingIntegrator struct {
	calls int
}

func (i *recordingIntegrator) Integrate(_ context.Context, request localcontrol.IntegrateRepositoryRequest) (localcontrol.IntegrationReceipt, error) {
	i.calls++
	return localcontrol.IntegrationReceipt{
		ID: "integration-proof", RepositoryID: request.RepositoryID,
		SourceRef: request.SourceRef, TargetRef: request.TargetRef,
		SourceSHA: request.ExpectedSourceSHA, PreviousTargetSHA: testSHA("b"),
		MergeSHA: testSHA("c"), SourceUpdated: true,
		Verification: "configured verification passed", ObservedAt: time.Unix(1_700_000_000, 0).UTC(),
	}, nil
}

func TestRepositoryIntegrationIsFencedAndIdempotent(t *testing.T) {
	ctx := context.Background()
	data, err := sqlite.OpenV2Runtime(ctx, filepath.Join(t.TempDir(), "local.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = data.Close() }()
	integrator := &recordingIntegrator{}
	service, err := localcontrol.New(localcontrol.Config{
		Store: data, Runtimes: fakeCatalog{}, Integrator: integrator,
		Clock: func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
		NewID: deterministicIDs(),
	})
	if err != nil {
		t.Fatal(err)
	}
	request := localcontrol.IntegrateRepositoryRequest{
		RepositoryID: "platform", SourceRef: "refs/heads/hive/landing",
		TargetRef: "refs/heads/main", ExpectedSourceSHA: testSHA("a"),
		Message: "merge: integrate objective", UpdateSource: true,
		IdempotencyKey: "integration-key",
	}
	first, err := service.IntegrateRepository(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.IntegrateRepository(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if integrator.calls != 1 || first.Receipt.MergeSHA != second.Receipt.MergeSHA {
		t.Fatalf("calls=%d first=%#v second=%#v", integrator.calls, first, second)
	}
	request.TargetRef = "refs/heads/other"
	if _, err := service.IntegrateRepository(ctx, request); err == nil {
		t.Fatal("conflicting replay was accepted")
	}
}

func testSHA(letter string) string {
	value := ""
	for len(value) < 40 {
		value += letter
	}
	return value
}
