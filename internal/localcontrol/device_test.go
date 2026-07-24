package localcontrol_test

import (
	"context"
	"crypto/ed25519"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/deviceidentity"
	"github.com/berkayahi/agentbridge/internal/localcontrol"
	"github.com/berkayahi/agentbridge/internal/store/sqlite"
	"github.com/berkayahi/agentbridge/internal/workmodel"
)

func TestPairingChallengeRequiresControllerIdentity(t *testing.T) {
	ctx := context.Background()
	data, err := sqlite.OpenV2Runtime(ctx, filepath.Join(t.TempDir(), "pairing.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	service, err := localcontrol.New(localcontrol.Config{
		Store: data, Runtimes: fakeCatalog{}, Executor: &fakeExecutor{}, Verifier: fakeVerifier{}, Committer: fakeCommitter{},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.CreatePairingChallenge(ctx, localcontrol.CreatePairingChallengeRequest{
		DeviceID: "pi-one", BrowserFingerprint: "browser", IdempotencyKey: "pairing-key",
	})
	if !errors.Is(err, localcontrol.ErrNotConfigured) {
		t.Fatalf("pairing without controller identity = %v, want ErrNotConfigured", err)
	}
}

func TestPairedDeviceSelectionRotationAndFence(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0).UTC()
	data, err := sqlite.OpenV2Runtime(ctx, filepath.Join(t.TempDir(), "devices.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	remoteExecutor := &fakeExecutor{}
	key, err := deviceidentity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	controllerKey, err := deviceidentity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	service, err := localcontrol.New(localcontrol.Config{
		Store: data, Identity: controllerKey, Runtimes: fakeCatalog{}, Executor: &fakeExecutor{}, Verifier: fakeVerifier{}, Committer: fakeCommitter{},
		RemoteDevices: map[string]localcontrol.DeviceRuntime{"pi-one": fakeDeviceRuntime{executor: remoteExecutor}},
		Clock:         func() time.Time { return now }, NewID: deterministicIDs(),
	})
	if err != nil {
		t.Fatal(err)
	}
	challengeResponse, err := service.CreatePairingChallenge(ctx, localcontrol.CreatePairingChallengeRequest{
		DeviceID: "pi-one", BrowserFingerprint: "browser-fingerprint", IdempotencyKey: "challenge-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(challengeResponse.Challenge.ControllerPublicKey) != string(controllerKey.PublicKey()) {
		t.Fatalf("pairing challenge controller key = %x, want %x", challengeResponse.Challenge.ControllerPublicKey, controllerKey.PublicKey())
	}
	challenge := challengeResponse.Challenge
	claim := deviceidentity.Claim{ID: challenge.ID, OrganizationID: "local", DeviceID: challenge.DeviceID, BrowserFingerprint: challenge.BrowserFingerprint, ExpiresAt: challenge.ExpiresAt}
	proofChallenge := deviceidentity.Challenge{ClaimID: challenge.ID, OrganizationID: "local", DeviceID: challenge.DeviceID, Nonce: challenge.Nonce, TrustSetDigest: challenge.TrustSetDigest, ExpiresAt: challenge.ExpiresAt}
	proof, err := key.Prove(claim, proofChallenge, now)
	if err != nil {
		t.Fatal(err)
	}
	paired, err := service.PairDevice(ctx, localcontrol.PairDeviceRequest{
		ChallengeID: challenge.ID, Name: "Build Pi", Kind: localcontrol.DeviceKindRaspberryPi,
		Endpoint: "wss://pi.local/agentbridge", PublicKey: proof.PublicKey, Signature: proof.Signature, IdempotencyKey: "pair-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	if paired.Device.Fingerprint != key.Fingerprint() || paired.Device.ConnectionEpoch != 1 {
		t.Fatalf("paired device = %#v", paired.Device)
	}
	storedPublicKey, err := data.DevicePublicKey(ctx, paired.Device.ID)
	if err != nil || string(storedPublicKey) != string(key.PublicKey()) {
		t.Fatalf("stored Pi public key = %x err=%v", storedPublicKey, err)
	}

	rotated, err := deviceidentity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	rotatedResponse, err := service.RotateDevice(ctx, localcontrol.RotateDeviceRequest{DeviceID: paired.Device.ID, Revision: paired.Device.Revision, PublicKey: rotated.PublicKey(), IdempotencyKey: "rotate-key"})
	if err != nil {
		t.Fatal(err)
	}
	if rotatedResponse.Device.Fingerprint != rotated.Fingerprint() || rotatedResponse.Device.ConnectionEpoch != 2 {
		t.Fatalf("rotated device = %#v", rotatedResponse.Device)
	}

	project, err := service.CreateProject(ctx, localcontrol.CreateProjectRequest{Name: "Pi project", IdempotencyKey: "project-key"})
	if err != nil {
		t.Fatal(err)
	}
	repository, err := service.RegisterRepository(ctx, localcontrol.RegisterRepositoryRequest{Remote: "origin", IdempotencyKey: "repository-key"})
	if err != nil {
		t.Fatal(err)
	}
	board, err := service.CreateBoard(ctx, localcontrol.CreateBoardRequest{ProjectID: project.Project.ID, Name: "Build", IdempotencyKey: "board-key"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := service.CreateTask(ctx, localcontrol.CreateTaskRequest{ProjectID: project.Project.ID, BoardID: board.Board.ID, RepositoryID: repository.Repository.ID, TargetDeviceID: paired.Device.ID, Provider: workmodel.CodexSubscription, Prompt: "run on Pi", IdempotencyKey: "task-key"})
	if err != nil {
		t.Fatal(err)
	}
	if task.Task.TargetDeviceID != paired.Device.ID || task.Task.TargetEpoch != 2 {
		t.Fatalf("task target = %#v", task.Task)
	}

	unreachable, err := service.SetDeviceState(ctx, localcontrol.DeviceMutationRequest{DeviceID: paired.Device.ID, Revision: rotatedResponse.Device.Revision, IdempotencyKey: "unreachable-key"}, localcontrol.DeviceStateUnreachable)
	if err != nil {
		t.Fatal(err)
	}
	queued, err := service.Start(ctx, localcontrol.StartRequest{TaskID: task.Task.ID, Revision: task.Task.Revision, IdempotencyKey: "blocked-unreachable"})
	if err != nil || !queued.Queued || queued.Task.State != workmodel.Queued {
		t.Fatalf("start while unreachable = %#v err=%v, want durable queued command", queued, err)
	}
	pending, err := data.ListPendingDeviceCommands(ctx, paired.Device.ID, 10)
	if err != nil || len(pending) != 1 || pending[0].Operation != "start" {
		t.Fatalf("pending device commands = %#v err=%v, want start command", pending, err)
	}
	reachable, err := service.SetDeviceState(ctx, localcontrol.DeviceMutationRequest{DeviceID: paired.Device.ID, Revision: unreachable.Device.Revision, IdempotencyKey: "reachable-key"}, localcontrol.DeviceStatePaired)
	if err != nil {
		t.Fatal(err)
	}
	if reachable.Device.ConnectionEpoch != 3 {
		t.Fatalf("reachable device epoch = %d, want 3", reachable.Device.ConnectionEpoch)
	}
	if _, err := service.Start(ctx, localcontrol.StartRequest{TaskID: task.Task.ID, Revision: task.Task.Revision, IdempotencyKey: "blocked-fence"}); !errors.Is(err, localcontrol.ErrDeviceFence) {
		t.Fatalf("start after reconnect = %v, want ErrDeviceFence", err)
	}
	selected, err := service.SelectTaskDevice(ctx, localcontrol.SelectDeviceRequest{TaskID: task.Task.ID, Revision: task.Task.Revision, TargetDeviceID: paired.Device.ID, IdempotencyKey: "select-key"})
	if err != nil {
		t.Fatal(err)
	}
	started, err := service.Start(ctx, localcontrol.StartRequest{TaskID: selected.Task.ID, Revision: selected.Task.Revision, IdempotencyKey: "start-key"})
	if err != nil || started.Task.State != workmodel.Running {
		t.Fatalf("start after selection = %#v err=%v", started, err)
	}
	revoked, err := service.SetDeviceState(ctx, localcontrol.DeviceMutationRequest{DeviceID: paired.Device.ID, Revision: reachable.Device.Revision, IdempotencyKey: "revoke-key"}, localcontrol.DeviceStateRevoked)
	if err != nil {
		t.Fatal(err)
	}
	if revoked.Device.State != localcontrol.DeviceStateRevoked {
		t.Fatalf("revoked device = %#v", revoked.Device)
	}
	if _, err := service.Verify(ctx, localcontrol.VerifyRequest{TaskID: started.Task.ID, Revision: started.Task.Revision, IdempotencyKey: "verify-revoked"}); !errors.Is(err, localcontrol.ErrDeviceRevoked) {
		t.Fatalf("verify after revoke = %v, want ErrDeviceRevoked", err)
	}
	reenrollmentKey, err := deviceidentity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	reenrollmentChallengeResponse, err := service.CreatePairingChallenge(ctx, localcontrol.CreatePairingChallengeRequest{
		DeviceID: paired.Device.ID, BrowserFingerprint: "browser-fingerprint", IdempotencyKey: "reenrollment-challenge-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	reenrollmentChallenge := reenrollmentChallengeResponse.Challenge
	reenrollmentProof, err := reenrollmentKey.Prove(
		deviceidentity.Claim{ID: reenrollmentChallenge.ID, OrganizationID: "local", DeviceID: reenrollmentChallenge.DeviceID, BrowserFingerprint: reenrollmentChallenge.BrowserFingerprint, ExpiresAt: reenrollmentChallenge.ExpiresAt},
		deviceidentity.Challenge{ClaimID: reenrollmentChallenge.ID, OrganizationID: "local", DeviceID: reenrollmentChallenge.DeviceID, Nonce: reenrollmentChallenge.Nonce, TrustSetDigest: reenrollmentChallenge.TrustSetDigest, ExpiresAt: reenrollmentChallenge.ExpiresAt},
		now,
	)
	if err != nil {
		t.Fatal(err)
	}
	reenrolled, err := service.PairDevice(ctx, localcontrol.PairDeviceRequest{
		ChallengeID: reenrollmentChallenge.ID, Name: "Re-enrolled Build Pi", Kind: localcontrol.DeviceKindRaspberryPi,
		Endpoint: "wss://pi-new.local/agentbridge", PublicKey: reenrollmentProof.PublicKey, Signature: reenrollmentProof.Signature, IdempotencyKey: "reenrollment-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	if reenrolled.Device.State != localcontrol.DeviceStatePaired || reenrolled.Device.Fingerprint != reenrollmentKey.Fingerprint() || reenrolled.Device.ConnectionEpoch != revoked.Device.ConnectionEpoch+1 || reenrolled.Device.Revision != revoked.Device.Revision+1 {
		t.Fatalf("re-enrolled device = %#v, want a new key and fenced epoch", reenrolled.Device)
	}
	assignment, err := data.TaskDevice(ctx, task.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if assignment.State != "fenced" {
		t.Fatalf("old task assignment after re-enrollment = %#v, want fenced", assignment)
	}
	if len(key.PublicKey()) != ed25519.PublicKeySize {
		t.Fatal("device key fixture is not Ed25519")
	}
}

type fakeDeviceRuntime struct{ executor *fakeExecutor }

func (r fakeDeviceRuntime) Start(ctx context.Context, task localcontrol.TaskView, request localcontrol.StartRequest) error {
	return r.executor.Start(ctx, task, request)
}

func (r fakeDeviceRuntime) Resume(ctx context.Context, task localcontrol.TaskView, request localcontrol.ResumeRequest) error {
	return r.executor.Resume(ctx, task, request)
}

func (r fakeDeviceRuntime) Approve(ctx context.Context, task localcontrol.TaskView, approvalID, userID string, allow bool) error {
	return r.executor.Approve(ctx, task, approvalID, userID, allow)
}

func (r fakeDeviceRuntime) Cancel(ctx context.Context, task localcontrol.TaskView) error {
	return r.executor.Cancel(ctx, task)
}

func (fakeDeviceRuntime) Verify(ctx context.Context, task localcontrol.TaskView) (localcontrol.VerificationReceipt, error) {
	return fakeVerifier{}.Verify(ctx, task)
}

func (fakeDeviceRuntime) Commit(ctx context.Context, task localcontrol.TaskView) (localcontrol.CommitReceipt, error) {
	return fakeCommitter{}.Commit(ctx, task)
}
