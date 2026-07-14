package scheduler

import (
	"context"
	"sync"
	"testing"
	"time"
)

type memoryLeases struct {
	mu       sync.Mutex
	held     map[string]string
	hearts   int
	releases int
}

func (m *memoryLeases) AcquireLease(_ context.Context, repo, owner string, _ time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.held == nil {
		m.held = make(map[string]string)
	}
	if _, ok := m.held[repo]; ok {
		return false, nil
	}
	m.held[repo] = owner
	return true, nil
}
func (m *memoryLeases) HeartbeatLease(_ context.Context, repo, owner string, _ time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.held[repo] == owner {
		m.hearts++
	}
	return nil
}
func (m *memoryLeases) ReleaseLease(_ context.Context, repo, owner string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.held[repo] == owner {
		delete(m.held, repo)
		m.releases++
	}
	return nil
}

func TestSchedulerSerializesWritersFIFOAndAllowsReaders(t *testing.T) {
	leases := &memoryLeases{}
	s := New(leases, "owner", time.Minute, 5*time.Millisecond)
	t.Cleanup(func() { _ = s.Close(context.Background()) })

	p1, err := s.Acquire(context.Background(), Request{TaskID: "one", Repository: "repo"})
	if err != nil {
		t.Fatal(err)
	}
	second := make(chan *Permit, 1)
	go func() {
		p, _ := s.Acquire(context.Background(), Request{TaskID: "two", Repository: "repo"})
		second <- p
	}()
	select {
	case <-second:
		t.Fatal("second writer started early")
	case <-time.After(10 * time.Millisecond):
	}
	reader, err := s.Acquire(context.Background(), Request{TaskID: "usage", Repository: "repo", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	reader.Release()
	p1.Release()
	select {
	case p2 := <-second:
		p2.Release()
	case <-time.After(time.Second):
		t.Fatal("second writer did not start")
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	leases.mu.Lock()
	defer leases.mu.Unlock()
	if leases.hearts == 0 {
		t.Fatal("lease was never heartbeated")
	}
	if leases.releases != 2 {
		t.Fatalf("releases = %d", leases.releases)
	}
}

func TestSchedulerCanceledWaiterAndClose(t *testing.T) {
	s := New(&memoryLeases{}, "owner", time.Minute, time.Hour)
	p, err := s.Acquire(context.Background(), Request{TaskID: "one", Repository: "repo"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Acquire(ctx, Request{TaskID: "two", Repository: "repo"}); err == nil {
		t.Fatal("expected cancellation")
	}
	p.Release()
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Acquire(context.Background(), Request{TaskID: "three", Repository: "repo"}); err == nil {
		t.Fatal("expected closed error")
	}
}
