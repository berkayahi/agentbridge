package localcontrol_test

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/deviceidentity"
	"github.com/berkayahi/agentbridge/internal/kernel"
	"github.com/berkayahi/agentbridge/internal/localcontrol"
	"github.com/berkayahi/agentbridge/internal/managed"
	"github.com/berkayahi/agentbridge/internal/store/sqlite"
	"github.com/berkayahi/agentbridge/internal/workmodel"
)

func TestPiSignedDeviceLinkUsesPairingIdentityAndFencedReplay(t *testing.T) {
	now := time.Unix(2_000, 0).UTC()
	controller, err := deviceidentity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	pi, err := deviceidentity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	transport := &piLinkTransport{now: now, controllerPublicKey: controller.PublicKey(), replyKey: pi}
	link, err := localcontrol.NewSignedDeviceLink(localcontrol.SignedDeviceLinkConfig{
		Transport: transport, Identity: controller, PeerPublicKey: pi.PublicKey(),
		OrganizationID: "local", DeviceID: "pi-one", ConnectionEpoch: 7, ControllerEpoch: 1,
		Clock: func() time.Time { return now }, ExpiresAfter: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	fenced, err := localcontrol.NewFencedLink("pi-one", 7, link)
	if err != nil {
		t.Fatal(err)
	}
	command := localcontrol.DeviceCommand{
		ID: "start:task-1", Operation: "start", TaskID: "task-1", ExecutionID: "execution-1",
		SessionID: "session-1", DeviceID: "pi-one", ConnectionEpoch: 7, Revision: 2,
		Payload: json.RawMessage(`{"input":"run"}`),
	}
	first, err := fenced.Execute(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	second, err := fenced.Execute(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	if first.MessageID != second.MessageID || first.DeviceID != "pi-one" || first.ConnectionEpoch != 7 || transport.sendCount != 1 || !transport.handshaken {
		t.Fatalf("reply=%#v duplicate=%#v sends=%d handshaken=%v", first, second, transport.sendCount, transport.handshaken)
	}
	if transport.lastCommand.SigningKeyID != controller.Fingerprint() || transport.lastCommand.PayloadType != "command" || transport.lastCommand.ResourceID != "start" {
		t.Fatalf("signed command frame = %#v", transport.lastCommand)
	}
	if _, err := fenced.Execute(context.Background(), localcontrol.DeviceCommand{
		ID: "start:task-1", Operation: "cancel", DeviceID: "pi-one", ConnectionEpoch: 7,
	}); !errors.Is(err, localcontrol.ErrIdempotencyConflict) {
		t.Fatalf("conflicting command = %v, want ErrIdempotencyConflict", err)
	}
	if _, err := link.Execute(context.Background(), localcontrol.DeviceCommand{ID: "stale", Operation: "start", DeviceID: "pi-one", ConnectionEpoch: 6}); !errors.Is(err, localcontrol.ErrDeviceFence) {
		t.Fatalf("stale command = %v, want ErrDeviceFence", err)
	}
}

func TestPiSignedDeviceLinkRejectsUntrustedReply(t *testing.T) {
	now := time.Unix(2_000, 0).UTC()
	controller, err := deviceidentity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	pi, err := deviceidentity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	other, err := deviceidentity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	transport := &piLinkTransport{now: now, controllerPublicKey: controller.PublicKey(), replyKey: other, handshakeKey: pi}
	link, err := localcontrol.NewSignedDeviceLink(localcontrol.SignedDeviceLinkConfig{
		Transport: transport, Identity: controller, PeerPublicKey: pi.PublicKey(),
		OrganizationID: "local", DeviceID: "pi-one", ConnectionEpoch: 7, ControllerEpoch: 1,
		Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = link.Execute(context.Background(), localcontrol.DeviceCommand{ID: "cancel:task-1", Operation: "cancel", DeviceID: "pi-one", ConnectionEpoch: 7})
	if !errors.Is(err, localcontrol.ErrDeviceLinkUnauthenticated) {
		t.Fatalf("untrusted reply = %v, want ErrDeviceLinkUnauthenticated", err)
	}
}

func TestPiDeviceAgentPersistsAcceptedReplyAcrossRestart(t *testing.T) {
	now := time.Unix(2_000, 0).UTC()
	controller, err := deviceidentity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	pi, err := deviceidentity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(t.TempDir(), "agent-state.json")
	resultStore, err := localcontrol.NewFileDeviceResultStore(filepath.Join(t.TempDir(), "agent-results.json"))
	if err != nil {
		t.Fatal(err)
	}
	controllerDB, err := sqlite.OpenV2Runtime(context.Background(), filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer controllerDB.Close()
	if err := controllerDB.CreateDevice(context.Background(), localcontrol.Device{
		ID: "pi-one", Name: "Build Pi", Kind: localcontrol.DeviceKindRaspberryPi,
		Fingerprint: pi.Fingerprint(), State: localcontrol.DeviceStatePaired, ConnectionEpoch: 1, Revision: 1,
		CreatedAt: now, UpdatedAt: now,
	}, pi.PublicKey()); err != nil {
		t.Fatal(err)
	}
	nextSequence := func(ctx context.Context) (uint64, uint64, error) {
		return controllerDB.NextDeviceLinkSequence(ctx, "pi-one")
	}
	handled := 0
	handler := func(context.Context, localcontrol.DeviceCommand) (localcontrol.DeviceReply, error) {
		handled++
		return localcontrol.DeviceReply{Accepted: true, Payload: json.RawMessage(`{"ok":true}`)}, nil
	}
	newAgent := func() *localcontrol.DeviceAgent {
		state, stateErr := managed.NewFileStateStore(statePath)
		if stateErr != nil {
			t.Fatal(stateErr)
		}
		replay, replayErr := managed.NewReplayGuardWithInbox(state, "local", "pi-one")
		if replayErr != nil {
			t.Fatal(replayErr)
		}
		agent, agentErr := localcontrol.NewDeviceAgent(localcontrol.DeviceAgentConfig{
			Identity: pi, ControllerPublicKey: controller.PublicKey(), OrganizationID: "local", DeviceID: "pi-one",
			ConnectionEpoch: 1, ControllerEpoch: 1, Replay: replay, Results: resultStore, Handler: handler,
			Clock: func() time.Time { return now },
		})
		if agentErr != nil {
			t.Fatal(agentErr)
		}
		return agent
	}
	command := localcontrol.DeviceCommand{ID: "start:durable", Operation: "start", TaskID: "task-1", DeviceID: "pi-one", ConnectionEpoch: 1, Payload: json.RawMessage(`{"input":"run"}`)}
	firstTransport := &piLinkTransport{now: now, controllerPublicKey: controller.PublicKey(), replyKey: pi, agent: newAgent()}
	firstLink, err := localcontrol.NewSignedDeviceLink(localcontrol.SignedDeviceLinkConfig{Transport: firstTransport, Identity: controller, PeerPublicKey: pi.PublicKey(), OrganizationID: "local", DeviceID: "pi-one", ConnectionEpoch: 1, ControllerEpoch: 1, Clock: func() time.Time { return now }, NextSequence: nextSequence})
	if err != nil {
		t.Fatal(err)
	}
	first, err := firstLink.Execute(context.Background(), command)
	if err != nil || !first.Accepted {
		t.Fatalf("first agent reply = %#v err=%v", first, err)
	}
	firstFrame := firstTransport.lastCommand
	secondTransport := &piLinkTransport{now: now, controllerPublicKey: controller.PublicKey(), replyKey: pi, agent: newAgent()}
	if _, err := secondTransport.agent.Handle(context.Background(), firstFrame); !errors.Is(err, managed.ErrReplay) {
		t.Fatalf("old signed frame = %v, want managed.ErrReplay", err)
	}
	secondLink, err := localcontrol.NewSignedDeviceLink(localcontrol.SignedDeviceLinkConfig{Transport: secondTransport, Identity: controller, PeerPublicKey: pi.PublicKey(), OrganizationID: "local", DeviceID: "pi-one", ConnectionEpoch: 1, ControllerEpoch: 1, Clock: func() time.Time { return now }, NextSequence: nextSequence})
	if err != nil {
		t.Fatal(err)
	}
	second, err := secondLink.Execute(context.Background(), command)
	if err != nil || !second.Accepted || handled != 1 {
		t.Fatalf("replayed agent reply = %#v err=%v handled=%d", second, err, handled)
	}
	command.Payload = json.RawMessage(`{"input":"different"}`)
	if _, err := secondLink.Execute(context.Background(), command); !errors.Is(err, localcontrol.ErrIdempotencyConflict) {
		t.Fatalf("conflicting durable command = %v, want ErrIdempotencyConflict", err)
	}
	command = localcontrol.DeviceCommand{ID: "resume:durable", Operation: "resume", TaskID: "task-1", DeviceID: "pi-one", ConnectionEpoch: 1, Payload: json.RawMessage(`{"input":"continue"}`)}
	if _, err := secondLink.Execute(context.Background(), command); err != nil || handled != 2 {
		t.Fatalf("new command after restart = err=%v handled=%d", err, handled)
	}
}

func TestPiLinkedRuntimeDrivesLocalVerticalSlice(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(2_000, 0).UTC()
	data, err := sqlite.OpenV2Runtime(ctx, filepath.Join(t.TempDir(), "pi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	controller, err := deviceidentity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	pi, err := deviceidentity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	agent, err := localcontrol.NewDeviceAgent(localcontrol.DeviceAgentConfig{
		Identity: pi, ControllerPublicKey: controller.PublicKey(), OrganizationID: "local", DeviceID: "pi-one",
		ConnectionEpoch: 1, ControllerEpoch: 1, Handler: piCommandHandler, Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	transport := &piLinkTransport{now: now, controllerPublicKey: controller.PublicKey(), replyKey: pi, agent: agent}
	signed, err := localcontrol.NewSignedDeviceLink(localcontrol.SignedDeviceLinkConfig{
		Transport: transport, Identity: controller, PeerPublicKey: pi.PublicKey(),
		OrganizationID: "local", DeviceID: "pi-one", ConnectionEpoch: 1, ControllerEpoch: 1,
		Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	fenced, err := localcontrol.NewFencedLink("pi-one", 1, signed)
	if err != nil {
		t.Fatal(err)
	}
	remote, err := localcontrol.NewLinkedRuntime(fenced)
	if err != nil {
		t.Fatal(err)
	}
	localController := &recordingController{}
	service, err := localcontrol.New(localcontrol.Config{
		Store: data, Identity: controller, Runtimes: fakeCatalog{}, Controller: localController, Executor: &fakeExecutor{},
		Verifier: fakeVerifier{}, Committer: fakeCommitter{}, RemoteDevices: map[string]localcontrol.DeviceRuntime{"pi-one": remote},
		Clock: func() time.Time { return now }, NewID: deterministicIDs(),
	})
	if err != nil {
		t.Fatal(err)
	}
	deviceKey, err := deviceidentity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	challengeResponse, err := service.CreatePairingChallenge(ctx, localcontrol.CreatePairingChallengeRequest{DeviceID: "pi-one", BrowserFingerprint: "browser", IdempotencyKey: "pi-challenge"})
	if err != nil {
		t.Fatal(err)
	}
	challenge := challengeResponse.Challenge
	proof, err := deviceKey.Prove(deviceidentity.Claim{ID: challenge.ID, OrganizationID: "local", DeviceID: challenge.DeviceID, BrowserFingerprint: challenge.BrowserFingerprint, ExpiresAt: challenge.ExpiresAt}, deviceidentity.Challenge{ClaimID: challenge.ID, OrganizationID: "local", DeviceID: challenge.DeviceID, Nonce: challenge.Nonce, TrustSetDigest: challenge.TrustSetDigest, ExpiresAt: challenge.ExpiresAt}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.PairDevice(ctx, localcontrol.PairDeviceRequest{ChallengeID: challenge.ID, Name: "Build Pi", Kind: localcontrol.DeviceKindRaspberryPi, Endpoint: "wss://pi.local/agentbridge", PublicKey: proof.PublicKey, Signature: proof.Signature, IdempotencyKey: "pi-pair"}); err != nil {
		t.Fatal(err)
	}
	project, err := service.CreateProject(ctx, localcontrol.CreateProjectRequest{Name: "Pi project", IdempotencyKey: "pi-project"})
	if err != nil {
		t.Fatal(err)
	}
	repository, err := service.RegisterRepository(ctx, localcontrol.RegisterRepositoryRequest{Remote: "origin", IdempotencyKey: "pi-repository"})
	if err != nil {
		t.Fatal(err)
	}
	board, err := service.CreateBoard(ctx, localcontrol.CreateBoardRequest{ProjectID: project.Project.ID, Name: "Build", IdempotencyKey: "pi-board"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := service.CreateTask(ctx, localcontrol.CreateTaskRequest{ProjectID: project.Project.ID, BoardID: board.Board.ID, RepositoryID: repository.Repository.ID, TargetDeviceID: "pi-one", Provider: workmodel.CodexSubscription, Prompt: "run on Pi", IdempotencyKey: "pi-task"})
	if err != nil {
		t.Fatal(err)
	}
	started, err := service.Start(ctx, localcontrol.StartRequest{TaskID: task.Task.ID, Revision: task.Task.Revision, IdempotencyKey: "pi-start"})
	if err != nil || started.Task.State != workmodel.Running {
		t.Fatalf("Pi start = %#v err=%v", started, err)
	}
	if localController.starts != 0 || localController.cancels != 0 {
		t.Fatalf("remote start invoked local kernel controller: %#v", localController)
	}
	if err := data.UpsertApproval(ctx, workmodel.Approval{ID: "pi-approval", TaskID: task.Task.ID, Kind: "command", Status: workmodel.ApprovalPending, RequestPayload: []byte(`{"summary":"Pi approval"}`), RequestedAt: now, ExpiresAt: timePtr(now.Add(time.Hour))}); err != nil {
		t.Fatal(err)
	}
	approved, err := service.Approve(ctx, localcontrol.ApproveRequest{TaskID: task.Task.ID, ApprovalID: "pi-approval", Revision: started.Task.Revision, UserID: "desktop", Allow: true, IdempotencyKey: "pi-approve"})
	if err != nil || approved.Task.State != workmodel.Running {
		t.Fatalf("Pi approve = %#v err=%v", approved, err)
	}
	verified, err := service.Verify(ctx, localcontrol.VerifyRequest{TaskID: task.Task.ID, Revision: approved.Task.Revision, IdempotencyKey: "pi-verify"})
	if err != nil || !verified.Receipt.Passed || verified.Task.State != workmodel.Verifying {
		t.Fatalf("Pi verify = %#v err=%v", verified, err)
	}
	committed, err := service.Commit(ctx, localcontrol.CommitRequest{TaskID: task.Task.ID, Revision: verified.Task.Revision, IdempotencyKey: "pi-commit"})
	if err != nil || committed.Task.State != workmodel.Completed || committed.Receipt.CommitSHA == "" {
		t.Fatalf("Pi commit = %#v err=%v", committed, err)
	}
	wantOperations := []string{"start", "approve", "verify", "commit"}
	if len(transport.operations) != len(wantOperations) {
		t.Fatalf("Pi operations = %#v, want %#v", transport.operations, wantOperations)
	}
	for index, operation := range wantOperations {
		if transport.operations[index] != operation {
			t.Fatalf("Pi operations = %#v, want %#v", transport.operations, wantOperations)
		}
	}
}

func TestPiWSSDeviceLinkDrivesLocalVerticalSlice(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	now := time.Unix(2_000, 0).UTC()
	controller, err := deviceidentity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	pi, err := deviceidentity.Generate()
	if err != nil {
		t.Fatal(err)
	}

	var operations []string
	var operationsMu sync.Mutex
	deviceStateDir := t.TempDir()
	deviceReplayState, err := managed.NewFileStateStore(filepath.Join(deviceStateDir, "replay.json"))
	if err != nil {
		t.Fatal(err)
	}
	deviceReplay, err := managed.NewReplayGuardWithInbox(deviceReplayState, "local", "pi-one")
	if err != nil {
		t.Fatal(err)
	}
	deviceResults, err := localcontrol.NewFileDeviceResultStore(filepath.Join(deviceStateDir, "results.json"))
	if err != nil {
		t.Fatal(err)
	}
	agent, err := localcontrol.NewDeviceAgent(localcontrol.DeviceAgentConfig{
		Identity: pi, ControllerPublicKey: controller.PublicKey(), OrganizationID: "local", DeviceID: "pi-one",
		Replay: deviceReplay, Results: deviceResults,
		ConnectionEpoch: 1, ControllerEpoch: 1, Handler: func(_ context.Context, command localcontrol.DeviceCommand) (localcontrol.DeviceReply, error) {
			operationsMu.Lock()
			operations = append(operations, command.Operation)
			operationsMu.Unlock()
			resultPayload := json.RawMessage(`{"accepted":true}`)
			var marshalErr error
			switch command.Operation {
			case "observe":
				resultPayload, marshalErr = json.Marshal(localcontrol.DeviceObservation{
					Cursor: 1,
					Events: []localcontrol.DeviceEvent{{
						Cursor: 1, ID: "pi-event-1", TaskID: command.TaskID, Type: "output",
						Payload: json.RawMessage(`{"message":"ready"}`), CreatedAt: now,
					}},
				})
			case "verify":
				resultPayload, marshalErr = json.Marshal(localcontrol.VerificationReceipt{
					ID: "pi-verification-wss", Passed: true, Summary: "Pi WSS verification passed", ObservedAt: now,
				})
			case "commit":
				resultPayload, marshalErr = json.Marshal(localcontrol.CommitReceipt{
					ID: "pi-commit-wss", CommitSHA: "dddddddddddddddddddddddddddddddddddddddd",
					RemoteRef: "refs/heads/task/pi-wss", ObservedAt: now,
				})
			}
			if marshalErr != nil {
				return localcontrol.DeviceReply{}, marshalErr
			}
			return localcontrol.DeviceReply{Accepted: true, Payload: resultPayload}, nil
		},
		Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := localcontrol.NewDeviceAgentWebSocketHandler(agent, managed.MaxFrameBytes, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(handler)
	defer server.Close()
	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	endpoint := "wss" + strings.TrimPrefix(server.URL, "https")

	data, err := sqlite.OpenV2Runtime(ctx, filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	localController := &recordingController{}
	remoteAvailable := true
	service, err := localcontrol.New(localcontrol.Config{
		Store: data, Identity: controller, Runtimes: fakeCatalog{}, Controller: localController,
		Executor: &fakeExecutor{}, Verifier: fakeVerifier{}, Committer: fakeCommitter{},
		RemoteDeviceFactory: func(factoryContext context.Context, view localcontrol.TaskView) (localcontrol.DeviceRuntime, error) {
			if !remoteAvailable {
				return nil, localcontrol.ErrDeviceLinkUnavailable
			}
			device, deviceErr := data.GetDevice(factoryContext, view.TargetDeviceID)
			if deviceErr != nil {
				return nil, deviceErr
			}
			peerPublicKey, keyErr := data.DevicePublicKey(factoryContext, device.ID)
			if keyErr != nil {
				return nil, keyErr
			}
			link, linkErr := localcontrol.NewWebSocketDeviceLink(factoryContext, localcontrol.WebSocketDeviceLinkConfig{
				Identity: controller, PeerPublicKey: peerPublicKey, OrganizationID: "local", DeviceID: device.ID,
				ConnectionEpoch: view.TargetEpoch, ControllerEpoch: 1, Endpoint: device.Endpoint,
				TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}, Clock: func() time.Time { return now },
				NextSequence: func(sequenceContext context.Context) (uint64, uint64, error) {
					return data.NextDeviceLinkSequence(sequenceContext, device.ID)
				},
			})
			if linkErr != nil {
				return nil, linkErr
			}
			return localcontrol.NewFencedLinkedRuntime(device.ID, view.TargetEpoch, link, link.Close)
		},
		Clock: func() time.Time { return now }, NewID: deterministicIDs(),
	})
	if err != nil {
		t.Fatal(err)
	}
	challengeResponse, err := service.CreatePairingChallenge(ctx, localcontrol.CreatePairingChallengeRequest{
		DeviceID: "pi-one", BrowserFingerprint: "browser", IdempotencyKey: "wss-challenge",
	})
	if err != nil {
		t.Fatal(err)
	}
	challenge := challengeResponse.Challenge
	proof, err := pi.Prove(deviceidentity.Claim{
		ID: challenge.ID, OrganizationID: "local", DeviceID: challenge.DeviceID,
		BrowserFingerprint: challenge.BrowserFingerprint, ExpiresAt: challenge.ExpiresAt,
	}, deviceidentity.Challenge{
		ClaimID: challenge.ID, OrganizationID: "local", DeviceID: challenge.DeviceID,
		Nonce: challenge.Nonce, TrustSetDigest: challenge.TrustSetDigest, ExpiresAt: challenge.ExpiresAt,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.PairDevice(ctx, localcontrol.PairDeviceRequest{
		ChallengeID: challenge.ID, Name: "Build Pi", Kind: localcontrol.DeviceKindRaspberryPi,
		Endpoint: endpoint, PublicKey: proof.PublicKey, Signature: proof.Signature, IdempotencyKey: "wss-pair",
	}); err != nil {
		t.Fatal(err)
	}
	project, err := service.CreateProject(ctx, localcontrol.CreateProjectRequest{Name: "WSS project", IdempotencyKey: "wss-project"})
	if err != nil {
		t.Fatal(err)
	}
	repository, err := service.RegisterRepository(ctx, localcontrol.RegisterRepositoryRequest{Remote: "origin", IdempotencyKey: "wss-repository"})
	if err != nil {
		t.Fatal(err)
	}
	board, err := service.CreateBoard(ctx, localcontrol.CreateBoardRequest{ProjectID: project.Project.ID, Name: "Build", IdempotencyKey: "wss-board"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := service.CreateTask(ctx, localcontrol.CreateTaskRequest{
		ProjectID: project.Project.ID, BoardID: board.Board.ID, RepositoryID: repository.Repository.ID,
		TargetDeviceID: "pi-one", Provider: workmodel.CodexSubscription, Title: "WSS task", Prompt: "run on Pi",
		IdempotencyKey: "wss-task",
	})
	if err != nil {
		t.Fatal(err)
	}
	started, err := service.Start(ctx, localcontrol.StartRequest{TaskID: task.Task.ID, Revision: task.Task.Revision, IdempotencyKey: "wss-start"})
	if err != nil || started.Task.State != workmodel.Running {
		t.Fatalf("WSS start = %#v err=%v", started, err)
	}
	observed, err := service.Observe(ctx, localcontrol.ObserveRequest{TaskID: task.Task.ID, AfterCursor: 0, Limit: 20})
	if err != nil || len(observed.Events) == 0 {
		t.Fatalf("WSS observe = %#v err=%v", observed, err)
	}
	if localController.starts != 0 || localController.cancels != 0 {
		t.Fatalf("WSS remote runtime invoked local kernel controller: %#v", localController)
	}
	if err := data.UpsertApproval(ctx, workmodel.Approval{
		ID: "wss-approval", TaskID: task.Task.ID, Kind: "command", Status: workmodel.ApprovalPending,
		RequestPayload: []byte(`{"summary":"WSS approval"}`), RequestedAt: now, ExpiresAt: timePtr(now.Add(time.Hour)),
	}); err != nil {
		t.Fatal(err)
	}
	approved, err := service.Approve(ctx, localcontrol.ApproveRequest{
		TaskID: task.Task.ID, ApprovalID: "wss-approval", Revision: observed.Task.Revision,
		UserID: localcontrol.LocalAuthorityUserID, Allow: true, IdempotencyKey: "wss-approve",
	})
	if err != nil || approved.Task.State != workmodel.Running {
		t.Fatalf("WSS approve = %#v err=%v", approved, err)
	}
	remoteAvailable = false
	queuedVerify, err := service.Verify(ctx, localcontrol.VerifyRequest{TaskID: task.Task.ID, Revision: approved.Task.Revision, IdempotencyKey: "wss-verify"})
	if err != nil || !queuedVerify.Queued || queuedVerify.Task.State != workmodel.Verifying {
		t.Fatalf("WSS disconnected verify = %#v err=%v", queuedVerify, err)
	}
	remoteAvailable = true
	replayed, err := service.ReplayDeviceCommands(ctx, localcontrol.ReplayDeviceCommandsRequest{DeviceID: "pi-one", Limit: 10})
	if err != nil || replayed.Replayed != 1 || len(replayed.Pending) != 0 {
		t.Fatalf("WSS reconnect replay = %#v err=%v", replayed, err)
	}
	currentTask, err := data.Task(ctx, task.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	committed, err := service.Commit(ctx, localcontrol.CommitRequest{TaskID: task.Task.ID, Revision: currentTask.Revision, IdempotencyKey: "wss-commit"})
	if err != nil || committed.Task.State != workmodel.Completed || committed.Receipt.CommitSHA == "" {
		t.Fatalf("WSS commit = %#v err=%v", committed, err)
	}
	wantOperations := []string{"start", "observe", "approve", "verify", "commit"}
	operationsMu.Lock()
	defer operationsMu.Unlock()
	if len(operations) != len(wantOperations) {
		t.Fatalf("WSS operations = %#v, want %#v", operations, wantOperations)
	}
	for index, operation := range wantOperations {
		if operations[index] != operation {
			t.Fatalf("WSS operations = %#v, want %#v", operations, wantOperations)
		}
	}
}

type piLinkTransport struct {
	now                 time.Time
	controllerPublicKey []byte
	handshakeKey        deviceidentity.Key
	replyKey            deviceidentity.Key
	handshaken          bool
	sendCount           int
	operations          []string
	lastCommand         managed.Frame
	response            managed.Frame
	agent               *localcontrol.DeviceAgent
}

func (t *piLinkTransport) PerformHandshake(_ context.Context, local managed.Handshake) (managed.Handshake, error) {
	controller, err := deviceidentity.FromPublic(t.controllerPublicKey)
	if err != nil {
		return managed.Handshake{}, err
	}
	if err := managed.VerifyHandshakeSignature(local, controller.PublicKey()); err != nil {
		return managed.Handshake{}, err
	}
	if !t.handshakeKey.HasPrivate() {
		t.handshakeKey = t.replyKey
	}
	remote, err := managed.SignHandshake(managed.Handshake{
		Major: managed.ProtocolMajor, Minor: managed.ProtocolMinor,
		OrganizationID: local.OrganizationID, DeviceID: local.DeviceID,
		ConnectionEpoch: local.ConnectionEpoch, ControllerEpoch: local.ControllerEpoch,
		Capabilities: []string{"local-control-request-response"},
	}, t.handshakeKey)
	if err != nil {
		return managed.Handshake{}, err
	}
	t.handshaken = true
	return remote, nil
}

func (t *piLinkTransport) Send(ctx context.Context, frame managed.Frame) error {
	controller, err := deviceidentity.FromPublic(t.controllerPublicKey)
	if err != nil {
		return err
	}
	now := t.now
	if err := frame.Validate(now); err != nil {
		return err
	}
	canonical, err := frame.CanonicalSigningBytes()
	if err != nil || frame.SigningKeyID != controller.Fingerprint() || !controller.Verify(canonical, frame.Signature) {
		return errors.New("controller command signature rejected")
	}
	var command localcontrol.DeviceCommand
	if err := json.Unmarshal(frame.Payload, &command); err != nil {
		return err
	}
	if command.DeviceID != frame.DeviceID || command.ConnectionEpoch != frame.ConnectionEpoch || command.ID != frame.CommandID {
		return errors.New("typed command does not match signed frame")
	}
	t.lastCommand = frame
	t.sendCount++
	t.operations = append(t.operations, frame.ResourceID)
	if t.agent != nil {
		t.response, err = t.agent.Handle(ctx, frame)
		return err
	}
	resultPayload := json.RawMessage(`{"accepted":true}`)
	if frame.ResourceID == "verify" {
		resultPayload, err = json.Marshal(localcontrol.VerificationReceipt{ID: "pi-verification", Passed: true, Summary: "Pi verification passed", ObservedAt: t.now})
		if err != nil {
			return err
		}
	}
	if frame.ResourceID == "commit" {
		resultPayload, err = json.Marshal(localcontrol.CommitReceipt{ID: "pi-commit-receipt", CommitSHA: "cccccccccccccccccccccccccccccccccccccccc", RemoteRef: "refs/heads/task/pi", ObservedAt: t.now})
		if err != nil {
			return err
		}
	}
	payload, err := json.Marshal(localcontrol.DeviceReply{MessageID: frame.MessageID, DeviceID: frame.DeviceID, ConnectionEpoch: frame.ConnectionEpoch, Accepted: true, Payload: resultPayload})
	if err != nil {
		return err
	}
	digest := sha256.Sum256(payload)
	response := managed.Frame{
		Major: managed.ProtocolMajor, Minor: managed.ProtocolMinor,
		OrganizationID: frame.OrganizationID, DeviceID: frame.DeviceID,
		ConnectionEpoch: frame.ConnectionEpoch, ControllerEpoch: frame.ControllerEpoch,
		MessageID: frame.MessageID + 100, CommandID: frame.CommandID,
		ExecutionID: frame.ExecutionID, SessionID: frame.SessionID, ResourceID: frame.ResourceID,
		CausationID: frame.CommandID, CorrelationID: frame.CommandID, Sequence: frame.Sequence,
		IssuedAt: now, ExpiresAt: now.Add(time.Minute), PayloadType: "event", PayloadDigest: digest[:], Payload: payload,
		SigningKeyID: t.replyKey.Fingerprint(),
	}
	canonical, err = response.CanonicalSigningBytes()
	if err != nil {
		return err
	}
	response.Signature, err = t.replyKey.Sign(canonical)
	if err != nil {
		return err
	}
	t.response = response
	return nil
}

func (t *piLinkTransport) Receive(ctx context.Context) (managed.Frame, error) {
	if err := ctx.Err(); err != nil {
		return managed.Frame{}, err
	}
	return t.response, nil
}

func (*piLinkTransport) Close() error { return nil }

var _ managed.Transport = (*piLinkTransport)(nil)
var _ managed.HandshakeTransport = (*piLinkTransport)(nil)

type recordingController struct {
	starts  int
	cancels int
}

func (c *recordingController) Start(context.Context, kernel.StartExecution) error {
	c.starts++
	return nil
}

func (c *recordingController) Cancel(context.Context, kernel.CancelExecution) error {
	c.cancels++
	return nil
}

func piCommandHandler(_ context.Context, command localcontrol.DeviceCommand) (localcontrol.DeviceReply, error) {
	resultPayload := json.RawMessage(`{"accepted":true}`)
	if command.Operation == "verify" {
		encoded, err := json.Marshal(localcontrol.VerificationReceipt{ID: "pi-verification", Passed: true, Summary: "Pi verification passed", ObservedAt: time.Unix(2_000, 0).UTC()})
		if err != nil {
			return localcontrol.DeviceReply{}, err
		}
		resultPayload = encoded
	}
	if command.Operation == "commit" {
		encoded, err := json.Marshal(localcontrol.CommitReceipt{ID: "pi-commit-receipt", CommitSHA: "cccccccccccccccccccccccccccccccccccccccc", RemoteRef: "refs/heads/task/pi", ObservedAt: time.Unix(2_000, 0).UTC()})
		if err != nil {
			return localcontrol.DeviceReply{}, err
		}
		resultPayload = encoded
	}
	return localcontrol.DeviceReply{Accepted: true, Payload: resultPayload}, nil
}
