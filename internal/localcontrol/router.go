package localcontrol

import "context"

type deviceRouter struct {
	local   localRuntime
	remotes map[string]DeviceRuntime
	factory RemoteDeviceFactory
}

func newDeviceRouter(local Executor, verifier Verifier, committer Committer, remotes map[string]DeviceRuntime, factory RemoteDeviceFactory) *deviceRouter {
	if local == nil && verifier == nil && committer == nil && len(remotes) == 0 && factory == nil {
		return nil
	}
	copyRemotes := make(map[string]DeviceRuntime, len(remotes))
	for id, runtime := range remotes {
		copyRemotes[id] = runtime
	}
	return &deviceRouter{local: localRuntime{Executor: local, Verifier: verifier, Committer: committer}, remotes: copyRemotes, factory: factory}
}

func (r *deviceRouter) target(ctx context.Context, view TaskView) (DeviceRuntime, error) {
	if r == nil {
		return nil, ErrNotConfigured
	}
	if view.TargetDeviceID == LocalDeviceID {
		if r.local.Executor == nil && r.local.Verifier == nil && r.local.Committer == nil {
			return nil, ErrNotConfigured
		}
		return r.local, nil
	}
	if target := r.remotes[view.TargetDeviceID]; target != nil {
		return target, nil
	}
	if r.factory != nil {
		return r.factory(ctx, view)
	}
	return nil, ErrNotConfigured
}

type localRuntime struct {
	Executor
	Verifier
	Committer
}

func (r *deviceRouter) Start(ctx context.Context, view TaskView, request StartRequest) error {
	target, err := r.target(ctx, view)
	if err != nil {
		return err
	}
	return target.Start(ctx, view, request)
}

func (r *deviceRouter) Resume(ctx context.Context, view TaskView, request ResumeRequest) error {
	target, err := r.target(ctx, view)
	if err != nil {
		return err
	}
	return target.Resume(ctx, view, request)
}

func (r *deviceRouter) Approve(ctx context.Context, view TaskView, approvalID, userID string, allow bool) error {
	target, err := r.target(ctx, view)
	if err != nil {
		return err
	}
	return target.Approve(ctx, view, approvalID, userID, allow)
}

func (r *deviceRouter) Cancel(ctx context.Context, view TaskView) error {
	target, err := r.target(ctx, view)
	if err != nil {
		return err
	}
	return target.Cancel(ctx, view)
}

func (r *deviceRouter) Verify(ctx context.Context, view TaskView) (VerificationReceipt, error) {
	target, err := r.target(ctx, view)
	if err != nil {
		return VerificationReceipt{}, err
	}
	return target.Verify(ctx, view)
}

func (r *deviceRouter) Commit(ctx context.Context, view TaskView) (CommitReceipt, error) {
	target, err := r.target(ctx, view)
	if err != nil {
		return CommitReceipt{}, err
	}
	return target.Commit(ctx, view)
}

func (r *deviceRouter) Observe(ctx context.Context, view TaskView, after uint64) (DeviceObservation, error) {
	target, err := r.target(ctx, view)
	if err != nil {
		return DeviceObservation{}, err
	}
	observer, ok := target.(DeviceObserver)
	if !ok {
		return DeviceObservation{}, ErrNotConfigured
	}
	return observer.Observe(ctx, view, after)
}

var _ Executor = (*deviceRouter)(nil)
var _ Verifier = (*deviceRouter)(nil)
var _ Committer = (*deviceRouter)(nil)
var _ DeviceObserver = (*deviceRouter)(nil)
