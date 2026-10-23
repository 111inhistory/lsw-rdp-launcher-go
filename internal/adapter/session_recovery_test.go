package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/111inhistory/lsw-rdp-launcher-go/internal/controller"
)

func TestBackendReconnectsWithoutAnotherVMLifecycleEvent(t *testing.T) {
	var subscriptions atomic.Int32
	closeFirst := make(chan struct{})
	secondAccepted := make(chan struct{})
	releaseSecond := make(chan struct{})
	agent := watchAgent(t, func(conn net.Conn) {
		n := subscriptions.Add(1)
		if n == 2 {
			close(secondAccepted)
			<-releaseSecond
		}
		_ = json.NewEncoder(conn).Encode(map[string]any{"protocol": agentProtocol, "status": "ok", "ssh_active": false})
		if n == 1 {
			<-closeFirst
			return
		}
		_, _ = io.Copy(io.Discard, conn)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
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
	waitReady := func() {
		t.Helper()
		for {
			snapshot, err := c.Snapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.BackendReady {
				return
			}
		}
	}
	waitReady()
	close(closeFirst)
	select {
	case <-secondAccepted:
	case <-ctx.Done():
		t.Fatal("no reconnect", ctx.Err())
	}
	snapshot, err := c.Snapshot(ctx)
	if err != nil || snapshot.BackendReady || snapshot.SSH != controller.SSHUnknown {
		t.Fatal("lost source kept healthy state", snapshot, err)
	}
	close(releaseSecond)
	waitReady()
	if subscriptions.Load() != 2 {
		t.Fatal(subscriptions.Load())
	}
}

func TestNiriReconnectReplacesSnapshotAndInvalidatesGap(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "niri.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		for i := 0; i < 2; i++ {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			var request string
			if err = json.NewDecoder(conn).Decode(&request); err != nil {
				conn.Close()
				return
			}
			if request != "EventStream" {
				t.Error(request)
			}
			_, _ = io.WriteString(conn, "{\"Ok\":\"Handled\"}\n")
			if i == 0 {
				_, _ = io.WriteString(conn, "{\"WindowsChanged\":{\"windows\":[{\"id\":1,\"app_id\":\"rdp.test\"}]}}\n")
				conn.Close()
			} else {
				_, _ = io.WriteString(conn, "{\"WindowsChanged\":{\"windows\":[]}}\n")
				<-ctx.Done()
				conn.Close()
			}
		}
	}()
	counts := make(chan int, 8)
	watchDone := make(chan error, 1)
	go func() {
		watchDone <- retrySource(ctx, "niri-test", func(ctx context.Context) error {
			return WatchNiri(ctx, socket, "rdp.test", func(_ context.Context, event controller.Event) error {
				counts <- event.(controller.WindowsObserved).Count
				return nil
			})
		})
	}()
	for _, want := range []int{1, -1, 0} {
		select {
		case got := <-counts:
			if got != want {
				t.Fatalf("want %d got %d", want, got)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	cancel()
	if err := <-watchDone; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	<-serverDone
}

func TestReconnectBackoffIsCanceledWithoutAnotherAttempt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := retrySource(ctx, "cancel-test", func(context.Context) error { calls++; cancel(); return io.EOF })
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatal(err, calls)
	}
}
