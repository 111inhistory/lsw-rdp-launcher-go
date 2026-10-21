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

// Exercise the real controller -> QueryVM -> source close -> retrySource ->
// subscription ACK -> QueryVM cycle, without a CLI request or lifecycle event.
func TestFailedSnapshotsReconnectWithoutRequests(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	var queries, subscriptions atomic.Int32
	newSource := func() *lifecycleSource {
		events := make(chan libvirt.DomainEventLifecycleMsg)
		return newLifecycleSource(events, func() { close(events) })
	}
	v := &Libvirt{domain: "win11", initial: newSource()}
	v.subscribe = func(context.Context) (*lifecycleSource, error) {
		subscriptions.Add(1)
		return newSource(), nil
	}
	v.open = func(context.Context) (domainAPI, func(), error) {
		f := &fakeDomain{state: libvirt.DomainRunning}
		if queries.Add(1) <= 2 {
			f.err = errors.New("snapshot unavailable")
		}
		return f, func() {}, nil
	}
	c := controller.New(controller.Options{VMSuspend: time.Hour}, func(ctx context.Context, op controller.Operation) controller.Outcome {
		if op.Kind != controller.QueryVM {
			return controller.Outcome{Err: errors.New("unexpected mutation")}
		}
		return v.Execute(ctx, op)
	})
	ack := make(chan struct{})
	var ackOnce sync.Once
	watchDone := make(chan error, 1)
	go func() {
		watchDone <- v.Watch(ctx, func(ctx context.Context, event controller.Event) error {
			err := c.Notify(ctx, event)
			if e, ok := event.(controller.VMSourceObserved); ok && e.Ready {
				ackOnce.Do(func() { close(ack) })
			}
			return err
		})
	}()
	select {
	case <-ack:
	case <-ctx.Done():
		t.Fatal("initial subscription not acknowledged")
	}
	runDone := make(chan error, 1)
	go func() { runDone <- c.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-watchDone:
		case <-time.After(time.Second):
			t.Error("source did not stop")
		}
		select {
		case <-runDone:
		case <-time.After(time.Second):
			t.Error("controller did not stop")
		}
	}()
	started := time.Now()
	for {
		s, err := c.Snapshot(ctx)
		if err != nil {
			t.Fatalf("observation never recovered: queries=%d subscriptions=%d: %v", queries.Load(), subscriptions.Load(), err)
		}
		if s.VM == controller.VMRunning {
			break
		}
		select {
		case <-time.After(time.Millisecond):
		case <-ctx.Done():
			t.Fatal("observation recovery timed out")
		}
	}
	if queries.Load() != 3 || subscriptions.Load() != 2 {
		t.Fatalf("queries=%d subscriptions=%d", queries.Load(), subscriptions.Load())
	}
	if time.Since(started) < 250*time.Millisecond {
		t.Fatal("failed snapshots retried without source backoff")
	}
}

func TestOldQueryFailureDoesNotCloseReplacementSubscription(t *testing.T) {
	oldEvents := make(chan libvirt.DomainEventLifecycleMsg)
	old := newLifecycleSource(oldEvents, func() { close(oldEvents) })
	defer old.shutdown()
	newEvents := make(chan libvirt.DomainEventLifecycleMsg)
	replacement := newLifecycleSource(newEvents, func() { close(newEvents) })
	defer replacement.shutdown()
	v := &Libvirt{current: old}
	failure := errors.New("old query failed late")
	v.open = func(context.Context) (domainAPI, func(), error) {
		v.mu.Lock()
		v.current = replacement
		v.mu.Unlock()
		return nil, nil, failure
	}
	out := v.Execute(context.Background(), controller.Operation{Kind: controller.QueryVM})
	if !errors.Is(out.Err, failure) {
		t.Fatal(out)
	}
	select {
	case <-newEvents:
		t.Fatal("an old query closed the replacement subscription")
	default:
	}
}
