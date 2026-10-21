package adapter

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/111inhistory/lsw-rdp-launcher-go/internal/controller"
	libvirt "github.com/digitalocean/go-libvirt"
)

type waitingQueryDomain struct {
	*fakeDomain
	ctx     context.Context
	release <-chan struct{}
}

func (f *waitingQueryDomain) DomainGetState(libvirt.Domain, uint32) (int32, int32, error) {
	select {
	case <-f.release:
	case <-f.ctx.Done():
	}
	if err := f.ctx.Err(); err != nil {
		return 0, 0, err
	}
	return int32(libvirt.DomainRunning), 0, nil
}

func TestCancelQueryHeadPreservesNextCallerAndLifecycleSubscription(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var subscriptions, closes, queries atomic.Int32
	newSource := func() *lifecycleSource {
		subscriptions.Add(1)
		events := make(chan libvirt.DomainEventLifecycleMsg)
		return newLifecycleSource(events, func() { closes.Add(1); close(events) })
	}
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	queryStarted := make(chan controller.Operation, 4)
	v := &Libvirt{domain: "win11", initial: newSource()}
	v.subscribe = func(context.Context) (*lifecycleSource, error) { return newSource(), nil }
	v.open = func(ctx context.Context) (domainAPI, func(), error) {
		queries.Add(1)
		return &waitingQueryDomain{fakeDomain: &fakeDomain{}, ctx: ctx, release: release}, func() {}, nil
	}
	c := controller.New(controller.Options{VMSuspend: time.Hour}, func(ctx context.Context, op controller.Operation) controller.Outcome {
		switch op.Kind {
		case controller.QueryVM:
			select {
			case queryStarted <- op:
			case <-ctx.Done():
				return controller.Outcome{Err: ctx.Err()}
			}
			return v.Execute(ctx, op)
		case controller.Dispatch:
			return controller.Outcome{Data: op.Request.Action}
		default:
			return controller.Outcome{Err: errors.New("unexpected mutation")}
		}
	})
	// Keep the initial Unknown state until both callers have reached the FIFO.
	if err := c.Notify(ctx, controller.VMSourceObserved{Ready: false}); err != nil {
		t.Fatal(err)
	}
	runDone := make(chan error, 1)
	go func() { runDone <- c.Run(ctx) }()
	defer func() { cancel(); <-runDone }()
	request := func(ctx context.Context, action string) <-chan controller.Result {
		result := make(chan controller.Result, 1)
		go func() { result <- c.Do(ctx, controller.Request{Action: action, Need: controller.BackendOnly}) }()
		return result
	}
	waitQueued := func(count int) {
		t.Helper()
		for {
			s, err := c.Snapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if s.Queued == count {
				return
			}
		}
	}
	ctxA, cancelA := context.WithCancel(ctx)
	defer cancelA()
	a := request(ctxA, "A")
	waitQueued(1)
	b := request(ctx, "B")
	waitQueued(2)
	watchDone := make(chan error, 1)
	go func() { watchDone <- v.Watch(ctx, c.Notify) }()
	defer func() { cancel(); <-watchDone }()
	var query controller.Operation
	select {
	case query = <-queryStarted:
	case <-ctx.Done():
		t.Fatal("query did not start")
	}
	cancelA()
	select {
	case result := <-a:
		if !errors.Is(result.Err, context.Canceled) {
			t.Fatal("A did not receive its own cancellation", result)
		}
	case <-ctx.Done():
		t.Fatal("A did not finish")
	}
	if query.Request.Action != "" {
		t.Error("shared query is still bound to caller A", query.Request)
	}
	releaseOnce.Do(func() { close(release) })
	for {
		s, err := c.Snapshot(ctx)
		if err != nil {
			t.Fatal("B lost its shared observation", err)
		}
		if s.VM == controller.VMRunning {
			break
		}
	}
	if err := c.Notify(ctx, controller.BackendObserved{Ready: true, SSH: controller.SSHZero}); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-b:
		if result.Err != nil || result.Data != "B" {
			t.Fatal("A cancellation affected B", result)
		}
	case <-ctx.Done():
		t.Fatal("B did not finish")
	}
	if queries.Load() != 1 || subscriptions.Load() != 1 || closes.Load() != 0 {
		t.Fatalf("caller cancellation disturbed observation: queries=%d subscriptions=%d closes=%d", queries.Load(), subscriptions.Load(), closes.Load())
	}
}

func TestQueryCancellationPreservesSourceButDeadlineTriggersRecovery(t *testing.T) {
	for _, canceled := range []bool{true, false} {
		name := "deadline"
		if canceled {
			name = "cancel"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			if canceled {
				cancel()
				ctx, cancel = context.WithCancel(context.Background())
				cancel()
			}
			defer cancel()
			events := make(chan libvirt.DomainEventLifecycleMsg)
			closed := false
			source := newLifecycleSource(events, func() { closed = true; close(events) })
			defer source.shutdown()
			v := &Libvirt{current: source, open: func(context.Context) (domainAPI, func(), error) { return nil, nil, ctx.Err() }}
			out := v.Execute(ctx, controller.Operation{Kind: controller.QueryVM})
			if !errors.Is(out.Err, ctx.Err()) || closed == canceled {
				t.Fatalf("out=%+v closed=%t canceled=%t", out, closed, canceled)
			}
		})
	}
}
