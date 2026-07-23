package runtime

import (
	"context"
	"fmt"
	"sort"
)

type Registry struct{ adapters map[string]Adapter }

func NewRegistry(adapters ...Adapter) (*Registry, error) {
	registry := &Registry{adapters: make(map[string]Adapter, len(adapters))}
	for _, adapter := range adapters {
		if err := registry.Register(adapter); err != nil {
			return nil, err
		}
	}
	return registry, nil
}

func (r *Registry) Register(adapter Adapter) error {
	if adapter == nil || (adapter.ID() != "codex" && adapter.ID() != "claude") {
		return ErrUnknownRuntime
	}
	if _, exists := r.adapters[adapter.ID()]; exists {
		return fmt.Errorf("runtime %s already registered", adapter.ID())
	}
	r.adapters[adapter.ID()] = adapter
	return nil
}

func (r *Registry) Get(id string) (Adapter, error) {
	adapter, ok := r.adapters[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownRuntime, id)
	}
	return adapter, nil
}

func (r *Registry) IDs() []string {
	ids := make([]string, 0, len(r.adapters))
	for id := range r.adapters {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (r *Registry) Detect(ctx context.Context) (map[string]Installation, error) {
	result := make(map[string]Installation, len(r.adapters))
	for id, adapter := range r.adapters {
		value, err := adapter.Detect(ctx)
		if err != nil {
			return nil, fmt.Errorf("detect %s: %w", id, err)
		}
		result[id] = value
	}
	return result, nil
}
