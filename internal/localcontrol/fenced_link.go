package localcontrol

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sync"
)

// FencedLink adds controller-side replay and connection-epoch protection to a
// concrete device transport. The durable local command idempotency table still
// owns the Desktop request; this layer prevents a retried device command from
// producing a second native effect while the link is alive.
type FencedLink struct {
	deviceID string
	epoch    uint64
	link     DeviceLink

	mu        sync.Mutex
	responses map[string]fencedResponse
}

type fencedResponse struct {
	digest   [32]byte
	response DeviceReply
}

func NewFencedLink(deviceID string, epoch uint64, link DeviceLink) (*FencedLink, error) {
	if !validID(deviceID) || epoch == 0 || link == nil {
		return nil, ErrInvalidRequest
	}
	return &FencedLink{deviceID: deviceID, epoch: epoch, link: link, responses: make(map[string]fencedResponse)}, nil
}

func (l *FencedLink) Execute(ctx context.Context, command DeviceCommand) (DeviceReply, error) {
	if l == nil || l.link == nil {
		return DeviceReply{}, ErrDeviceLinkUnavailable
	}
	if err := validateDeviceCommand(command); err != nil {
		return DeviceReply{}, err
	}
	if command.DeviceID != l.deviceID || command.ConnectionEpoch != l.epoch {
		return DeviceReply{}, fmt.Errorf("device command is outside the active fence: %w", ErrDeviceFence)
	}
	digest, err := commandDigest(command)
	if err != nil {
		return DeviceReply{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if existing, ok := l.responses[command.ID]; ok {
		if existing.digest != digest {
			return DeviceReply{}, ErrIdempotencyConflict
		}
		return cloneDeviceReply(existing.response), nil
	}
	response, err := l.link.Execute(ctx, command)
	if err != nil {
		return DeviceReply{}, err
	}
	if response.DeviceID != l.deviceID || response.ConnectionEpoch != l.epoch {
		return DeviceReply{}, fmt.Errorf("device reply is outside the active fence: %w", ErrDeviceFence)
	}
	l.responses[command.ID] = fencedResponse{digest: digest, response: cloneDeviceReply(response)}
	return response, nil
}

func commandDigest(command DeviceCommand) ([32]byte, error) {
	encoded, err := json.Marshal(command)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(encoded), nil
}

func cloneDeviceReply(value DeviceReply) DeviceReply {
	value.Payload = append([]byte(nil), value.Payload...)
	return value
}

var _ DeviceLink = (*FencedLink)(nil)
