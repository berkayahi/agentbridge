package localcontrol_test

import (
	"context"
	"errors"
	"testing"

	"github.com/berkayahi/agentbridge/internal/localcontrol"
)

func TestFencedLinkReplaysAcceptedCommandAndRejectsConflicts(t *testing.T) {
	calls := 0
	link, err := localcontrol.NewFencedLink("pi-1", 7, linkFunc(func(_ context.Context, command localcontrol.DeviceCommand) (localcontrol.DeviceReply, error) {
		calls++
		return localcontrol.DeviceReply{MessageID: uint64(calls), DeviceID: command.DeviceID, ConnectionEpoch: command.ConnectionEpoch, Accepted: true, Payload: []byte(`{"ok":true}`)}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	command := localcontrol.DeviceCommand{ID: "command-1", Operation: "start", DeviceID: "pi-1", ConnectionEpoch: 7, Payload: []byte(`{"input":"run"}`)}
	first, err := link.Execute(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	second, err := link.Execute(context.Background(), command)
	if err != nil || second.MessageID != first.MessageID || calls != 1 {
		t.Fatalf("replay = %#v err=%v calls=%d", second, err, calls)
	}
	command.Payload = []byte(`{"input":"different"}`)
	if _, err := link.Execute(context.Background(), command); !errors.Is(err, localcontrol.ErrIdempotencyConflict) {
		t.Fatalf("conflicting replay = %v, want ErrIdempotencyConflict", err)
	}
	command.ID = "command-2"
	command.ConnectionEpoch = 6
	if _, err := link.Execute(context.Background(), command); !errors.Is(err, localcontrol.ErrDeviceFence) {
		t.Fatalf("stale command = %v, want ErrDeviceFence", err)
	}
}
