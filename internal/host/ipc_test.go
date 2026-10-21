package host

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/111inhistory/lsw-rdp-launcher-go/internal/controller"
)

func TestIPCSharedControllerCancellationAndShutdown(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	root, shutdown := context.WithCancel(ctx)
	defer shutdown()
	started, canceled := make(chan struct{}), make(chan struct{})
	c := controller.New(controller.Options{}, func(ctx context.Context, op controller.Operation) controller.Outcome {
		if op.Kind == controller.QueryVM {
			return controller.Outcome{VM: controller.VMRunning}
		}
		if op.Kind == controller.Dispatch {
			if op.Request.Action == "blocked" {
				close(started)
				<-ctx.Done()
				close(canceled)
				return controller.Outcome{Err: ctx.Err()}
			}
			return controller.Outcome{Data: op.Request}
		}
		return controller.Outcome{}
	})
	coreDone := make(chan error, 1)
	go func() { coreDone <- c.Run(root) }()
	defer func() {
		shutdown()
		select {
		case <-coreDone:
		case <-ctx.Done():
			t.Error("core did not stop")
		}
	}()
	for {
		s, err := c.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if s.VM == controller.VMRunning {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err := c.Notify(ctx, controller.BackendObserved{Ready: true, SSH: controller.SSHZero}); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(t.TempDir(), "host.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- Serve(root, listener, c, shutdown) }()
	defer func() {
		shutdown()
		select {
		case <-served:
		case <-ctx.Done():
			t.Error("IPC server did not stop")
		}
	}()
	if _, err = Call(ctx, socket, Request{Action: "lock"}); err != nil {
		t.Fatal(err)
	}
	data, err := Call(ctx, socket, Request{Action: "status"})
	var snapshot struct{ Locked bool }
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &snapshot); err != nil || !snapshot.Locked {
		t.Fatal(string(data), err)
	}
	caller, disconnect := context.WithCancel(ctx)
	reply := make(chan error, 1)
	go func() {
		_, err := Call(caller, socket, Request{Action: "request", Request: controller.Request{Action: "blocked"}})
		reply <- err
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("request did not dispatch")
	}
	// Status remains responsive while the FIFO head waits on external I/O.
	if _, err = Call(ctx, socket, Request{Action: "status"}); err != nil {
		t.Fatal(err)
	}
	disconnect()
	select {
	case err := <-reply:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("client cancellation blocked")
	}
	select {
	case <-canceled:
	case <-ctx.Done():
		t.Fatal("disconnected caller's operation was not canceled")
	}
	want := controller.Request{Action: "get_mapped_drives", Need: controller.Interactive}
	data, err = Call(ctx, socket, Request{Action: "request", Request: want})
	if err != nil {
		t.Fatal(err)
	}
	var got controller.Request
	if err = json.Unmarshal(data, &got); err != nil || got != want {
		t.Fatal(string(data), err)
	}
	if _, err = Call(ctx, socket, Request{Action: "unlock"}); err != nil {
		t.Fatal(err)
	}
	if _, err = Call(ctx, socket, Request{Action: "shutdown"}); err != nil {
		t.Fatal(err)
	}
}
