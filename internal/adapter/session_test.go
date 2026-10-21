package adapter

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/111inhistory/lsw-rdp-launcher-go/internal/controller"
)

func TestBackendSubscriptionRefreshesOnVMResumeWithoutHeartbeat(t *testing.T) {
	var subscriptions atomic.Int32
	agent := watchAgent(t, func(conn net.Conn) {
		subscriptions.Add(1)
		_ = json.NewEncoder(conn).Encode(map[string]any{"status": "ok", "kind": "snapshot", "ssh_active": false})
		_, _ = io.Copy(io.Discard, conn)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c := controller.New(controller.Options{}, func(context.Context, controller.Operation) controller.Outcome {
		return controller.Outcome{VM: controller.VMRunning}
	})
	s := &Session{Controller: c, agent: agent}
	stopped := make(chan error, 2)
	go func() { stopped <- c.Run(ctx) }()
	go func() { stopped <- s.watchBackend(ctx) }()
	defer func() {
		cancel()
		for i := 0; i < 2; i++ {
			select {
			case <-stopped:
			case <-time.After(time.Second):
				t.Error("source did not stop")
			}
		}
	}()
	wait := func(predicate func(controller.Snapshot) bool) {
		t.Helper()
		for {
			state, err := c.Snapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if predicate(state) {
				return
			}
		}
	}
	wait(func(s controller.Snapshot) bool { return s.BackendReady && s.SSH == controller.SSHZero })
	if err := c.Notify(ctx, controller.VMObserved{State: controller.VMPaused}); err != nil {
		t.Fatal(err)
	}
	wait(func(s controller.Snapshot) bool { return s.VM == controller.VMPaused && !s.BackendReady })
	if err := c.Notify(ctx, controller.VMObserved{State: controller.VMRunning}); err != nil {
		t.Fatal(err)
	}
	wait(func(s controller.Snapshot) bool { return s.BackendReady && s.SSH == controller.SSHZero })
	if subscriptions.Load() != 2 {
		t.Fatalf("want two subscriptions, got %d", subscriptions.Load())
	}
}
