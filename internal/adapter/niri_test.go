package adapter

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/111inhistory/lsw-rdp-launcher-go/internal/controller"
)

func TestNiriSnapshotUpdatesAndCancellation(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "niri.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := make(chan struct{})
	go func() {
		defer close(server)
		conn, err := listener.Accept()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		var request string
		if err = json.NewDecoder(conn).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if request != "EventStream" {
			t.Error(request)
			return
		}
		encoder := json.NewEncoder(conn)
		if err = encoder.Encode(map[string]string{"Ok": "Handled"}); err != nil {
			t.Error(err)
			return
		}
		frames := []string{
			`{"WindowClosed":{"id":99}}`,
			`{"WindowsChanged":{"windows":[{"id":1,"app_id":"org.example.OtherRDP"},{"id":2,"app_id":"com.freerdp.client.sdl3"}]}}`,
			`{"WindowOpenedOrChanged":{"window":{"id":2,"app_id":"org.example.OtherRDP"}}}`,
			`{"WindowOpenedOrChanged":{"window":{"id":1,"app_id":"editor"}}}`,
			`{"WindowClosed":{"id":2}}`,
			`{"WindowFocusChanged":{"id":1}}`,
		}
		for _, frame := range frames {
			if _, err = conn.Write([]byte(frame + "\n")); err != nil {
				t.Error(err)
				return
			}
		}
		<-ctx.Done()
	}()
	counts := make(chan int, 8)
	done := make(chan error, 1)
	go func() {
		done <- WatchNiri(ctx, socket, "org.example.OtherRDP", func(_ context.Context, event controller.Event) error {
			counts <- event.(controller.WindowsObserved).Count
			return nil
		})
	}()
	for _, want := range []int{1, 2, 1, 0} {
		select {
		case got := <-counts:
			if got != want {
				t.Fatalf("want %d got %d", want, got)
			}
		case <-time.After(time.Second):
			t.Fatal("missing window message")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Niri reader did not stop")
	}
	select {
	case <-server:
	case <-time.After(time.Second):
		t.Fatal("server did not stop")
	}
	select {
	case got := <-counts:
		if got != -1 {
			t.Fatal(got)
		}
	default:
		t.Fatal("missing unknown window state")
	}
}
