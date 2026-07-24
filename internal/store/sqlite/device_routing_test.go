package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/deviceidentity"
	"github.com/berkayahi/agentbridge/internal/localcontrol"
	"github.com/berkayahi/agentbridge/internal/workmodel"
)

func TestRuntimeStoreDeviceLinkSequencePersistsAcrossRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "device-link-sequence.db")
	deviceKey, err := deviceidentity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(2_000, 0).UTC()
	store, err := OpenV2Runtime(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateDevice(ctx, localcontrol.Device{
		ID: "pi-one", Name: "Build Pi", Kind: localcontrol.DeviceKindRaspberryPi,
		Fingerprint: deviceKey.Fingerprint(), State: localcontrol.DeviceStatePaired,
		ConnectionEpoch: 1, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}, deviceKey.PublicKey()); err != nil {
		t.Fatal(err)
	}
	firstMessage, firstSequence, err := store.NextDeviceLinkSequence(ctx, "pi-one")
	if err != nil {
		t.Fatal(err)
	}
	secondMessage, secondSequence, err := store.NextDeviceLinkSequence(ctx, "pi-one")
	if err != nil {
		t.Fatal(err)
	}
	if firstMessage != 1 || firstSequence != 1 || secondMessage != 2 || secondSequence != 2 {
		t.Fatalf("reserved sequences = (%d, %d), (%d, %d)", firstMessage, firstSequence, secondMessage, secondSequence)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenV2Runtime(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	thirdMessage, thirdSequence, err := reopened.NextDeviceLinkSequence(ctx, "pi-one")
	if err != nil {
		t.Fatal(err)
	}
	if thirdMessage != 3 || thirdSequence != 3 {
		t.Fatalf("reopened reserved sequence = (%d, %d), want (3, 3)", thirdMessage, thirdSequence)
	}
}

func TestRuntimeStoreLocalDeviceCursorAndFenceAreDurable(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(3_000, 0).UTC()
	store, err := OpenV2Runtime(ctx, filepath.Join(t.TempDir(), "device-routing.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	project := localcontrol.Project{ID: "project-cursor", Name: "Cursor project", Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := store.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	board := localcontrol.Board{ID: "board-cursor", ProjectID: project.ID, Name: "Build", Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := store.CreateBoard(ctx, board); err != nil {
		t.Fatal(err)
	}
	repository := localcontrol.Repository{ID: "repository-cursor", Remote: "origin", CreatedAt: now}
	if err := store.CreateRepository(ctx, repository); err != nil {
		t.Fatal(err)
	}
	piKey, err := deviceidentity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateDevice(ctx, localcontrol.Device{
		ID: "pi-cursor", Name: "Cursor Pi", Kind: localcontrol.DeviceKindRaspberryPi,
		Fingerprint: piKey.Fingerprint(), State: localcontrol.DeviceStatePaired,
		ConnectionEpoch: 1, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}, piKey.PublicKey()); err != nil {
		t.Fatal(err)
	}
	task := workmodel.Task{
		ID: "task-cursor", RepoProfileID: repository.ID, Title: "Cursor task", Prompt: "preserve the cursor",
		Provider: workmodel.CodexSubscription, State: workmodel.Queued, CreatedAt: now, UpdatedAt: now,
	}
	first, err := store.CreateTaskInContext(ctx, project.ID, board.ID, localcontrol.LocalDeviceID, task,
		workmodel.Event{ID: "runtime-task-cursor", TaskID: task.ID, Type: workmodel.EventTaskCreated, Visibility: workmodel.VisibilityUser, Payload: []byte(`{"state":"queued"}`), CreatedAt: now},
		localcontrol.Event{ID: "local-task-cursor", ResourceType: "task", ResourceID: task.ID, TaskID: task.ID, Revision: 1, Type: "task_created", Payload: []byte(`{"state":"queued"}`), CreatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	assignment, err := store.TaskDevice(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if assignment.LastAckCursor != first.Cursor || assignment.State != "assigned" {
		t.Fatalf("initial assignment = %#v, first event = %#v", assignment, first)
	}
	if assignment.LastObservedCursor != 0 {
		t.Fatalf("initial remote observation cursor = %d, want 0", assignment.LastObservedCursor)
	}
	if err := store.AdvanceTaskDeviceObservationCursor(ctx, task.ID, "pi-cursor", 1, 4); err == nil {
		t.Fatal("advanced observation cursor through the wrong device")
	}
	if err := store.AdvanceTaskDeviceObservationCursor(ctx, task.ID, localcontrol.LocalDeviceID, 1, 4); err != nil {
		t.Fatal(err)
	}
	assignment, err = store.TaskDevice(ctx, task.ID)
	if err != nil || assignment.LastObservedCursor != 4 {
		t.Fatalf("remote observation cursor = %#v err=%v", assignment, err)
	}
	if err := store.AdvanceTaskDeviceObservationCursor(ctx, task.ID, localcontrol.LocalDeviceID, 1, 2); err != nil {
		t.Fatal(err)
	}
	assignment, err = store.TaskDevice(ctx, task.ID)
	if err != nil || assignment.LastObservedCursor != 4 {
		t.Fatalf("remote observation cursor regressed = %#v err=%v", assignment, err)
	}
	noise, err := store.AppendLocalEvent(ctx, localcontrol.Event{
		ID: "local-global-noise", ResourceType: "device", ResourceID: "pi-cursor",
		Revision: 1, Type: "device_observed", Payload: []byte(`{"task_id":"other-task"}`), CreatedAt: now.Add(500 * time.Millisecond),
	})
	if err != nil {
		t.Fatal(err)
	}
	if noise.TaskCursor != 0 || noise.Cursor <= first.Cursor {
		t.Fatalf("global event cursors = %#v, first = %#v", noise, first)
	}

	second, err := store.AppendLocalEvent(ctx, localcontrol.Event{
		ID: "local-task-cursor-2", ResourceType: "task", ResourceID: task.ID, TaskID: task.ID,
		Revision: 1, Type: "evidence", Payload: []byte(`{"ordered":true}`), CreatedAt: now.Add(time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	assignment, err = store.TaskDevice(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if assignment.LastAckCursor != second.Cursor {
		t.Fatalf("event cursor = %d, assignment cursor = %d", second.Cursor, assignment.LastAckCursor)
	}
	if second.TaskCursor != first.TaskCursor+1 {
		t.Fatalf("task event cursors = %d, %d; global noise must not create a task gap", first.TaskCursor, second.TaskCursor)
	}
	events, err := store.ListLocalEvents(ctx, task.ID, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Cursor != first.Cursor || events[1].Cursor != second.Cursor || events[1].TaskCursor != 2 {
		t.Fatalf("task event projection = %#v, want global gap with task cursors 1,2", events)
	}

	selected, selectedEvent, err := store.AssignTaskDevice(ctx, task.ID, 1, "pi-cursor", 1, localcontrol.Event{
		ID: "local-device-selected", ResourceType: "task", ResourceID: task.ID, TaskID: task.ID,
		Revision: 2, Type: "device_selected", Payload: []byte(`{"device_id":"pi-cursor"}`), CreatedAt: now.Add(2 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	if selected.State != "assigned" || selected.LastAckCursor != selectedEvent.Cursor {
		t.Fatalf("selected assignment = %#v, event = %#v", selected, selectedEvent)
	}
	if selected.LastObservedCursor != 0 {
		t.Fatalf("new device assignment retained remote cursor = %d", selected.LastObservedCursor)
	}
	if err := store.AdvanceTaskDeviceObservationCursor(ctx, task.ID, "pi-cursor", 1, 4); err != nil {
		t.Fatal(err)
	}
	selected, err = store.TaskDevice(ctx, task.ID)
	if err != nil || selected.LastObservedCursor != 4 {
		t.Fatalf("selected device remote cursor = %#v err=%v", selected, err)
	}
	if err := store.AdvanceTaskDeviceObservationCursor(ctx, task.ID, "pi-cursor", 1, 2); err != nil {
		t.Fatal(err)
	}
	selected, err = store.TaskDevice(ctx, task.ID)
	if err != nil || selected.LastObservedCursor != 4 {
		t.Fatalf("selected device remote cursor regressed = %#v err=%v", selected, err)
	}
	gapEvent := localcontrol.Event{
		ID: "remote-observation-gap", ResourceType: "task", ResourceID: task.ID, TaskID: task.ID,
		Revision: 2, Type: "device_event", Payload: []byte(`{"device_id":"pi-cursor","remote_cursor":6}`), CreatedAt: now.Add(2400 * time.Millisecond),
	}
	if err := store.ApplyDeviceObservation(ctx, task.ID, "pi-cursor", 1, 2, 6, []localcontrol.Event{gapEvent}, nil); !errors.Is(err, localcontrol.ErrInvalidRequest) {
		t.Fatalf("remote observation cursor gap = %v, want ErrInvalidRequest", err)
	}
	selected, err = store.TaskDevice(ctx, task.ID)
	if err != nil || selected.LastObservedCursor != 4 {
		t.Fatalf("remote observation cursor gap advanced state = %#v err=%v", selected, err)
	}
	remoteEvent := localcontrol.Event{
		ID: "remote-observation-1", ResourceType: "task", ResourceID: task.ID, TaskID: task.ID,
		Revision: 2, Type: "device_event", Payload: []byte(`{"device_id":"pi-cursor","remote_cursor":5}`), CreatedAt: now.Add(2500 * time.Millisecond),
	}
	if err := store.ApplyDeviceObservation(ctx, task.ID, "pi-cursor", 1, 2, 5, []localcontrol.Event{remoteEvent}, nil); err != nil {
		t.Fatal(err)
	}
	selected, err = store.TaskDevice(ctx, task.ID)
	if err != nil || selected.LastObservedCursor != 5 {
		t.Fatalf("applied remote observation = %#v err=%v", selected, err)
	}
	expectedAckCursor := selected.LastAckCursor
	partial := localcontrol.Event{
		ID: "remote-observation-partial", ResourceType: "task", ResourceID: task.ID, TaskID: task.ID,
		Revision: 2, Type: "device_event", Payload: []byte(`{"device_id":"pi-cursor","remote_cursor":6}`), CreatedAt: now.Add(2600 * time.Millisecond),
	}
	conflict := remoteEvent
	conflict.Payload = []byte(`{"device_id":"pi-cursor","remote_cursor":5,"changed":true}`)
	if err := store.ApplyDeviceObservation(ctx, task.ID, "pi-cursor", 1, 2, 6, []localcontrol.Event{partial, conflict}, nil); err == nil {
		t.Fatal("partial remote observation committed despite a conflicting event")
	}
	selected, err = store.TaskDevice(ctx, task.ID)
	if err != nil || selected.LastObservedCursor != 5 {
		t.Fatalf("partial remote observation advanced cursor = %#v err=%v", selected, err)
	}
	events, err = store.ListLocalEvents(ctx, task.ID, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.ID == partial.ID {
			t.Fatal("partial remote observation event survived rollback")
		}
	}
	if _, err := store.UpdateTaskAtRevision(ctx, task.ID, 2, task.Title, task.Prompt, localcontrol.Event{
		ID: "local-task-revision-3", ResourceType: "task", ResourceID: task.ID, TaskID: task.ID,
		Revision: 3, Type: "task_updated", Payload: []byte(`{"revision":3}`), CreatedAt: now.Add(2700 * time.Millisecond),
	}); err != nil {
		t.Fatal(err)
	}
	selected, err = store.TaskDevice(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ApplyDeviceObservation(ctx, task.ID, "pi-cursor", 1, 3, 5, []localcontrol.Event{{
		ID: remoteEvent.ID, ResourceType: remoteEvent.ResourceType, ResourceID: remoteEvent.ResourceID, TaskID: remoteEvent.TaskID,
		Revision: 3, Type: remoteEvent.Type, Payload: append([]byte(nil), remoteEvent.Payload...), CreatedAt: remoteEvent.CreatedAt,
	}}, nil); err != nil {
		t.Fatalf("remote observation replay after local revision advance: %v", err)
	}
	expectedAckCursor = selected.LastAckCursor

	device, err := store.GetDevice(ctx, "pi-cursor")
	if err != nil {
		t.Fatal(err)
	}
	device.ConnectionEpoch = 2
	device.Revision = 2
	device.UpdatedAt = now.Add(3 * time.Second)
	if err := store.UpdateDevice(ctx, device, piKey.PublicKey()); err != nil {
		t.Fatal(err)
	}
	assignment, err = store.TaskDevice(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if assignment.State != "fenced" || assignment.LastAckCursor != expectedAckCursor {
		t.Fatalf("rotated assignment = %#v, want fenced cursor %d", assignment, expectedAckCursor)
	}

	device.State = localcontrol.DeviceStateUnreachable
	device.Revision = 3
	device.UpdatedAt = now.Add(4 * time.Second)
	if err := store.UpdateDevice(ctx, device, nil); err != nil {
		t.Fatal(err)
	}
	assignment, err = store.TaskDevice(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if assignment.State != "unreachable" {
		t.Fatalf("unreachable assignment = %#v", assignment)
	}
}
