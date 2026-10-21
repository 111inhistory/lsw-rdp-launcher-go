package controller

import (
	"context"
	"testing"
	"time"
)

func TestControllerWaitsForCanceledOperationCleanup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	finishCleanup := make(chan struct{})
	cleaned := make(chan struct{})
	c := New(Options{}, func(ctx context.Context, _ Operation) Outcome {
		close(started)
		<-ctx.Done()
		<-finishCleanup
		close(cleaned)
		return Outcome{Err: ctx.Err()}
	})
	stopped := make(chan error, 1)
	go func() { stopped <- c.Run(ctx) }()
	<-started
	cancel()
	select {
	case <-stopped:
		t.Fatal("Run returned before operation cleanup")
	case <-time.After(10 * time.Millisecond):
	}
	close(finishCleanup)
	select {
	case <-stopped:
		select {
		case <-cleaned:
		default:
			t.Fatal("cleanup missing")
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not finish")
	}
}
