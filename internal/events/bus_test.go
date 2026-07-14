package events

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestBusFansOutInOrderAndCopiesPayload(t *testing.T) {
	b := NewBus(4)
	t.Cleanup(b.Close)
	a := b.Subscribe(context.Background())
	bsub := b.Subscribe(context.Background())
	t.Cleanup(a.Cancel)
	t.Cleanup(bsub.Cancel)

	for i := range 3 {
		payload := []byte(strconv.Itoa(i))
		if err := b.Publish(context.Background(), Event{Type: "progress", Payload: payload}); err != nil {
			t.Fatal(err)
		}
		payload[0] = 'x'
	}

	for _, sub := range []*Subscription{a, bsub} {
		for i := range 3 {
			select {
			case delivery := <-sub.C:
				if got := string(delivery.Event.Payload); got != strconv.Itoa(i) {
					t.Fatalf("payload %d = %q", i, got)
				}
				if delivery.Dropped != 0 {
					t.Fatalf("unexpected drop count: %d", delivery.Dropped)
				}
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for event")
			}
		}
	}
}

func TestBusSlowSubscriberNeverBlocksPublisherAndReportsDrops(t *testing.T) {
	b := NewBus(1)
	t.Cleanup(b.Close)
	sub := b.Subscribe(context.Background())
	t.Cleanup(sub.Cancel)

	done := make(chan struct{})
	go func() {
		for i := range 1000 {
			_ = b.Publish(context.Background(), Event{Type: strconv.Itoa(i)})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("publisher blocked on slow subscriber")
	}

	first := <-sub.C
	if first.Event.Type != "0" {
		t.Fatalf("first delivered event = %q", first.Event.Type)
	}
	if err := b.Publish(context.Background(), Event{Type: "after-drain"}); err != nil {
		t.Fatal(err)
	}
	next := <-sub.C
	if next.Event.Type != "after-drain" || next.Dropped != 999 {
		t.Fatalf("delivery = %#v", next)
	}
}

func TestSubscriptionCancelIsConcurrentAndIdempotent(t *testing.T) {
	b := NewBus(8)
	sub := b.Subscribe(context.Background())

	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() { sub.Cancel() })
	}
	wg.Wait()
	if _, ok := <-sub.C; ok {
		t.Fatal("subscription channel remains open")
	}
	if err := b.Publish(context.Background(), Event{Type: "ignored"}); err != nil {
		t.Fatal(err)
	}
	b.Close()
}

func TestSubscriptionContextCancellationClosesChannel(t *testing.T) {
	b := NewBus(1)
	t.Cleanup(b.Close)
	ctx, cancel := context.WithCancel(context.Background())
	sub := b.Subscribe(ctx)
	cancel()
	select {
	case _, ok := <-sub.C:
		if ok {
			t.Fatal("subscription produced event after cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("subscription did not observe context cancellation")
	}
}

func TestSubscriptionWithCanceledContextStartsClosed(t *testing.T) {
	b := NewBus(1)
	t.Cleanup(b.Close)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	sub := b.Subscribe(ctx)
	select {
	case _, ok := <-sub.C:
		if ok {
			t.Fatal("subscription produced event for canceled context")
		}
	default:
		t.Fatal("subscription was not closed before Subscribe returned")
	}
}

func TestBusCloseAndCanceledPublishAreSafe(t *testing.T) {
	b := NewBus(1)
	sub := b.Subscribe(context.Background())
	b.Close()
	b.Close()
	if _, ok := <-sub.C; ok {
		t.Fatal("subscription channel remains open after close")
	}
	if err := b.Publish(context.Background(), Event{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("publish after close error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b2 := NewBus(1)
	t.Cleanup(b2.Close)
	if err := b2.Publish(ctx, Event{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled publish error = %v", err)
	}
}

func TestBusConcurrentPublishSubscribeCancelAndClose(t *testing.T) {
	b := NewBus(2)
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			sub := b.Subscribe(context.Background())
			for range 20 {
				_ = b.Publish(context.Background(), Event{Type: "event"})
			}
			sub.Cancel()
		})
	}
	wg.Wait()
	b.Close()
}
