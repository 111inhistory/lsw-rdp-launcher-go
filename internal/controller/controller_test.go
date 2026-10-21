package controller

import (
	"context"
	"errors"
	"testing"
	"time"
)

type harness struct {
	c          *Controller
	ctx        context.Context
	operations chan Operation
	results    chan Outcome
}

func newHarness(t *testing.T, opts ...Options) *harness {
	t.Helper()
	var options Options
	if len(opts) != 0 {
		options = opts[0]
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	h := &harness{ctx: ctx, operations: make(chan Operation, 8), results: make(chan Outcome)}
	h.c = New(options, func(ctx context.Context, op Operation) Outcome {
		select {
		case h.operations <- op:
		case <-ctx.Done():
			return Outcome{Err: ctx.Err()}
		}
		select {
		case out := <-h.results:
			return out
		case <-ctx.Done():
			return Outcome{Err: ctx.Err()}
		}
	})
	stopped := make(chan error, 1)
	go func() { stopped <- h.c.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-stopped:
			if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				t.Error(err)
			}
		case <-time.After(time.Second):
			t.Error("controller did not stop")
		}
	})
	h.expect(t, QueryVM)
	h.complete(t, Outcome{VM: VMRunning})
	h.wait(t, func(s Snapshot) bool { return s.VM == VMRunning })
	return h
}
func (h *harness) expect(t *testing.T, kind Kind) Operation {
	t.Helper()
	select {
	case op := <-h.operations:
		if op.Kind != kind {
			t.Fatalf("want %s, got %s", kind, op.Kind)
		}
		return op
	case <-h.ctx.Done():
		t.Fatal("waiting for", kind, h.ctx.Err())
		return Operation{}
	}
}
func (h *harness) complete(t *testing.T, out Outcome) {
	t.Helper()
	select {
	case h.results <- out:
	case <-h.ctx.Done():
		t.Fatal(h.ctx.Err())
	}
}
func (h *harness) wait(t *testing.T, predicate func(Snapshot) bool) Snapshot {
	t.Helper()
	for {
		state, err := h.c.Snapshot(h.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if predicate(state) {
			return state
		}
	}
}
func (h *harness) notify(t *testing.T, event Event) {
	t.Helper()
	if err := h.c.Notify(h.ctx, event); err != nil {
		t.Fatal(err)
	}
}
func (h *harness) request(ctx context.Context, request Request) <-chan Result {
	result := make(chan Result, 1)
	go func() { result <- h.c.Do(ctx, request) }()
	return result
}
func (h *harness) reply(t *testing.T, ch <-chan Result) Result {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-h.ctx.Done():
		t.Fatal(h.ctx.Err())
		return Result{}
	}
}

func TestLoopRemainsResponsiveDuringExternalOperation(t *testing.T) {
	h := newHarness(t)
	h.notify(t, BackendObserved{true, SSHZero})
	first := h.request(h.ctx, Request{Action: "first", Need: BackendOnly})
	op := h.expect(t, Dispatch)
	if op.Request.Action != "first" {
		t.Fatal(op)
	}
	second := h.request(h.ctx, Request{Action: "second", Need: BackendOnly})
	h.wait(t, func(s Snapshot) bool { return s.Queued == 2 })
	if err := h.c.SetLocked(h.ctx, true); err != nil {
		t.Fatal(err)
	}
	h.notify(t, WindowsObserved{Count: 2})
	h.wait(t, func(s Snapshot) bool { return s.Locked && s.Windows == 2 })
	h.complete(t, Outcome{Data: "first reply"})
	if r := h.reply(t, first); r.Err != nil || r.Data != "first reply" {
		t.Fatal(r)
	}
	op = h.expect(t, Dispatch)
	if op.Request.Action != "second" {
		t.Fatal(op)
	}
	h.complete(t, Outcome{Data: "second reply"})
	if r := h.reply(t, second); r.Err != nil || r.Data != "second reply" {
		t.Fatal(r)
	}
}
func TestLoopProcessesLifecycleWhileDispatchIsPending(t *testing.T) {
	h := newHarness(t)
	h.notify(t, BackendObserved{true, SSHZero})
	result := h.request(h.ctx, Request{Action: "slow", Need: BackendOnly})
	h.expect(t, Dispatch)
	h.notify(t, VMObserved{State: VMOff})
	h.wait(t, func(s Snapshot) bool { return s.VM == VMOff })
	h.complete(t, Outcome{Data: "late reply"})
	if r := h.reply(t, result); r.Err == nil {
		t.Fatal("late completion was accepted", r)
	}
}
func TestCancelWakesQueueWaitingForBackend(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(h.ctx)
	result := h.request(ctx, Request{Action: "waiting", Need: BackendOnly})
	h.wait(t, func(s Snapshot) bool { return s.Queued == 1 })
	cancel()
	if r := h.reply(t, result); !errors.Is(r.Err, context.Canceled) {
		t.Fatal(r)
	}
	h.wait(t, func(s Snapshot) bool { return s.Queued == 0 })
}
func TestCancelReachesExecutor(t *testing.T) {
	h := newHarness(t)
	h.notify(t, BackendObserved{true, SSHZero})
	ctx, cancel := context.WithCancel(h.ctx)
	result := h.request(ctx, Request{Action: "pending", Need: BackendOnly})
	h.expect(t, Dispatch)
	cancel()
	if r := h.reply(t, result); !errors.Is(r.Err, context.Canceled) {
		t.Fatal(r)
	}
	h.wait(t, func(s Snapshot) bool { return s.Queued == 0 && s.Operation == "" })
}
func TestLoopInteractiveWaitsForBackendAfterDisplay(t *testing.T) {
	h := newHarness(t)
	result := h.request(h.ctx, Request{Action: "run", Need: Interactive})
	h.expect(t, StartDisplay)
	h.complete(t, Outcome{})
	h.wait(t, func(s Snapshot) bool { return s.Display && s.Operation == "" })
	select {
	case op := <-h.operations:
		t.Fatalf("dispatched before backend ready: %+v", op)
	default:
	}
	h.notify(t, BackendObserved{true, SSHUnknown})
	h.expect(t, Dispatch)
	h.complete(t, Outcome{Data: "started"})
	if r := h.reply(t, result); r.Err != nil || r.Data != "started" {
		t.Fatal(r)
	}
}
