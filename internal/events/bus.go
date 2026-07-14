// Package events provides live, in-process event fan-out. It deliberately does
// not own replay; durable stores such as SQLite provide replay separately.
package events

import (
	"context"
	"errors"
	"sync"
)

// ErrClosed is returned when publishing to a closed Bus.
var ErrClosed = errors.New("event bus closed")

// Event is a provider- and transport-independent task event envelope.
type Event struct {
	ID      string
	Type    string
	Payload []byte
}

// Delivery reports the number of events dropped for this slow subscriber
// since its previous successful delivery.
type Delivery struct {
	Event   Event
	Dropped uint64
}

// Subscription receives live deliveries until cancellation or Bus.Close.
type Subscription struct {
	C      <-chan Delivery
	bus    *Bus
	id     uint64
	cancel sync.Once
}

// Cancel unsubscribes and closes C. It is safe to call concurrently or more
// than once.
func (s *Subscription) Cancel() {
	if s == nil {
		return
	}
	s.cancel.Do(func() {
		if s.bus != nil {
			s.bus.unsubscribe(s.id)
		}
	})
}

type subscriber struct {
	ch      chan Delivery
	done    chan struct{}
	dropped uint64
}

// Bus is a concurrency-safe, bounded, non-blocking live fan-out bus. When a
// subscriber's buffer is full, the event is dropped only for that subscriber.
type Bus struct {
	mu          sync.Mutex
	buffer      int
	nextID      uint64
	closed      bool
	subscribers map[uint64]*subscriber
}

// NewBus creates a Bus with a per-subscriber buffer. Non-positive sizes use 1.
func NewBus(buffer int) *Bus {
	if buffer <= 0 {
		buffer = 1
	}
	return &Bus{buffer: buffer, subscribers: make(map[uint64]*subscriber)}
}

// Subscribe registers a live subscriber. Canceling ctx cancels the returned
// subscription. A subscription created after Close is already closed.
func (b *Bus) Subscribe(ctx context.Context) *Subscription {
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		ch := make(chan Delivery)
		close(ch)
		return &Subscription{C: ch}
	}

	b.mu.Lock()
	b.nextID++
	id := b.nextID
	s := &subscriber{ch: make(chan Delivery, b.buffer), done: make(chan struct{})}
	sub := &Subscription{C: s.ch, bus: b, id: id}
	if b.closed {
		close(s.done)
		close(s.ch)
		b.mu.Unlock()
		return sub
	}
	b.subscribers[id] = s
	b.mu.Unlock()

	if ctx.Done() != nil {
		go func() {
			select {
			case <-ctx.Done():
				sub.Cancel()
			case <-s.done:
			}
		}()
	}
	return sub
}

// Publish fans out without waiting for subscribers. Delivered event payloads
// are copied so callers and subscribers cannot mutate one another's data.
func (b *Bus) Publish(ctx context.Context, event Event) error {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrClosed
	}
	for _, s := range b.subscribers {
		delivery := Delivery{Event: cloneEvent(event), Dropped: s.dropped}
		select {
		case s.ch <- delivery:
			s.dropped = 0
		default:
			s.dropped++
		}
	}
	return nil
}

// Close closes all subscriptions. It is safe to call more than once.
func (b *Bus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for id, s := range b.subscribers {
		close(s.done)
		close(s.ch)
		delete(b.subscribers, id)
	}
}

func (b *Bus) unsubscribe(id uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.subscribers[id]
	if !ok {
		return
	}
	delete(b.subscribers, id)
	close(s.done)
	close(s.ch)
}

func cloneEvent(event Event) Event {
	event.Payload = append([]byte(nil), event.Payload...)
	return event
}
