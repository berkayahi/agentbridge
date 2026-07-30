package repositorysnapshot

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestPublicV1FixtureMatchesCanonicalSnapshotContract(t *testing.T) {
	contents, err := os.ReadFile("../../protocol/fixtures/v1/repository-snapshot.json")
	if err != nil {
		t.Fatal(err)
	}
	var response Response
	if err := json.Unmarshal(contents, &response); err != nil {
		t.Fatal(err)
	}
	digest, err := digestResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	if response.ContractVersion != RepositorySnapshotV1 ||
		response.OperationID == "" || response.ExactCommitSHA == "" ||
		response.ResultDigest != digest {
		t.Fatalf("fixture contract/digest mismatch: %#v digest=%q", response, digest)
	}
	if response.Bounds.MaxTreeEntries != MaxTreeEntries ||
		response.Bounds.MaxSelectedBlobs != MaxSelectedBlobs ||
		response.Bounds.MaxBlobBytes != MaxBlobBytes ||
		response.Bounds.MaxTotalBlobBytes != MaxTotalBlobBytes {
		t.Fatalf("fixture bounds are stale: %#v", response.Bounds)
	}
	for _, forbidden := range []string{"checkout_path", "/Users/", "/home/", "secret", "token="} {
		if strings.Contains(strings.ToLower(string(contents)), strings.ToLower(forbidden)) {
			t.Fatalf("public fixture contains forbidden path/value marker %q", forbidden)
		}
	}
}
