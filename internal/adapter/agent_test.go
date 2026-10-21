package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/111inhistory/lsw-rdp-launcher-go/internal/controller"
)

func fakeAgent(t *testing.T, handle func(net.Conn)) Agent {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
				handle(conn)
			}()
		}
	}()
	t.Cleanup(func() { listener.Close(); <-stopped; workers.Wait() })
	return Agent{Endpoint: listener.Addr().String()}
}
func jsonAgent(t *testing.T, reply func(agentRequest) any) Agent {
	return fakeAgent(t, func(conn net.Conn) {
		var req agentRequest
		if err := json.NewDecoder(conn).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		if err := json.NewEncoder(conn).Encode(reply(req)); err != nil {
			t.Error(err)
		}
	})
}
func watchAgent(t *testing.T, handle func(net.Conn)) Agent {
	t.Helper()
	return fakeAgent(t, func(conn net.Conn) {
		var req agentRequest
		if err := json.NewDecoder(conn).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		if req.Action != "watch_status" {
			t.Errorf("watch action = %q", req.Action)
			return
		}
		handle(conn)
	})
}
func TestAgentSendsTokenForEveryAction(t *testing.T) {
	for _, action := range []string{
		"ping", "status", "run", "open", "list_apps", "get_mapped_drives", "inspect_session", "logoff_session",
	} {
		t.Run(action, func(t *testing.T) {
			a := jsonAgent(t, func(req agentRequest) any {
				if req.Action != action || req.Token != "shared-token" {
					t.Errorf("request = %+v", req)
				}
				return map[string]any{"status": "ok", "session_id": 2, "session_active": true}
			})
			a.AuthToken = func(context.Context) (string, error) { return "shared-token", nil }
			if _, err := a.Call(context.Background(), controller.Request{Action: action}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAgentSendsProviderTokenOnEveryCall(t *testing.T) {
	a := jsonAgent(t, func(req agentRequest) any {
		if req.Token != "shared-token" {
			t.Errorf("token = %q", req.Token)
		}
		return map[string]any{"status": "ok"}
	})
	a.AuthToken = func(context.Context) (string, error) { return "shared-token", nil }
	if _, err := a.Call(context.Background(), controller.Request{Action: "status"}); err != nil {
		t.Fatal(err)
	}
}

func TestAgentTokenFailureDoesNotSendAnUnauthenticatedRequest(t *testing.T) {
	dialed := make(chan struct{}, 1)
	a := fakeAgent(t, func(net.Conn) { dialed <- struct{}{} })
	a.AuthToken = func(context.Context) (string, error) { return "", errors.New("token unavailable") }
	_, err := a.Call(context.Background(), controller.Request{Action: "run"})
	if err == nil || !strings.Contains(err.Error(), "token unavailable") {
		t.Fatal(err)
	}
	select {
	case <-dialed:
		t.Fatal("sent a request without a token")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestAgentEmptyTokenIsRejectedLocally(t *testing.T) {
	a := fakeAgent(t, func(net.Conn) { t.Error("dialed with an empty token") })
	a.AuthToken = func(context.Context) (string, error) { return "", nil }
	if _, err := a.Call(context.Background(), controller.Request{Action: "status"}); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatal(err)
	}
}

func TestAgentWatchSendsProviderToken(t *testing.T) {
	a := fakeAgent(t, func(conn net.Conn) {
		var req agentRequest
		if err := json.NewDecoder(conn).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		if req.Action != "watch_status" || req.Token != "shared-token" {
			t.Errorf("watch request = %+v", req)
			return
		}
		_ = json.NewEncoder(conn).Encode(map[string]any{"status": "ok", "ssh_active": false})
		_, _ = io.Copy(io.Discard, conn)
	})
	a.AuthToken = func(context.Context) (string, error) { return "shared-token", nil }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := a.Watch(ctx, func(context.Context, controller.Event) error {
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestAgentUsesExistingRustJSONContract(t *testing.T) {
	a := jsonAgent(t, func(req agentRequest) any {
		if req.Action != "run" || req.Target != `C:\Program Files\app.exe` || req.Params != `"J:\some file.docx"` || req.WorkingDir != `C:\work` {
			t.Error(req)
		}
		return map[string]any{"status": "ok"}
	})
	_, err := a.Call(context.Background(), controller.Request{Action: "run", Target: `C:\Program Files\app.exe`, Params: `"J:\some file.docx"`, WorkingDir: `C:\work`})
	if err != nil {
		t.Fatal(err)
	}
}
func TestAgentAppAndDriveRequests(t *testing.T) {
	a := jsonAgent(t, func(req agentRequest) any {
		switch req.Action {
		case "list_apps":
			return map[string]any{"status": "ok", "apps": []any{map[string]any{"id": "word", "name": "Word", "app_type": "win32", "target": "WINWORD.EXE", "arguments": nil}}}
		case "get_mapped_drives":
			return map[string]any{"status": "ok", "drives": [][2]string{{"J:", `\\server\share`}}}
		default:
			t.Error("unexpected request", req)
			return map[string]any{"status": "error"}
		}
	})
	response, err := a.Call(context.Background(), controller.Request{Action: "list_apps", WithIcons: true})
	if err != nil || len(response.Apps) != 1 || response.Apps[0].ID != "word" {
		t.Fatal(response, err)
	}
	response, err = a.Call(context.Background(), controller.Request{Action: "get_mapped_drives"})
	if err != nil || len(response.Drives) != 1 || response.Drives[0] != [2]string{"J:", `\\server\share`} {
		t.Fatal(response, err)
	}
}
func TestAgentCancellationClosesPendingRead(t *testing.T) {
	accepted := make(chan struct{})
	a := fakeAgent(t, func(conn net.Conn) {
		var request agentRequest
		if err := json.NewDecoder(conn).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		close(accepted)
		_, _ = io.Copy(io.Discard, conn)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := a.Call(ctx, controller.Request{Action: "status"}); done <- err }()
	select {
	case <-accepted:
	case <-time.After(time.Second):
		t.Fatal("request not accepted")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled request succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("read did not stop")
	}
}
func TestAgentWatchReportsInitialSnapshot(t *testing.T) {
	a := watchAgent(t, func(conn net.Conn) {
		_ = json.NewEncoder(conn).Encode(map[string]any{"status": "ok", "kind": "snapshot", "ssh_active": false, "source": "poll"})
		_, _ = io.Copy(io.Discard, conn)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var observed controller.BackendObserved
	err := a.Watch(ctx, func(_ context.Context, event controller.Event) error {
		observed = event.(controller.BackendObserved)
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) || !observed.Ready || observed.SSH != controller.SSHZero {
		t.Fatal(observed, err)
	}
}
func TestAgentWatchStreamsSnapshotChangesAndUnknown(t *testing.T) {
	a := watchAgent(t, func(conn net.Conn) {
		for _, frame := range []map[string]any{
			{"status": "ok", "kind": "snapshot", "ssh_active": false, "source": "poll"},
			{"status": "ok", "kind": "changed", "ssh_active": true, "source": "events"},
			{"status": "ok", "kind": "changed", "ssh_active": nil, "source": "poll"},
		} {
			if err := json.NewEncoder(conn).Encode(frame); err != nil {
				t.Error(err)
				return
			}
		}
		_, _ = io.Copy(io.Discard, conn)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var seen []controller.BackendObserved
	err := a.Watch(ctx, func(_ context.Context, event controller.Event) error {
		seen = append(seen, event.(controller.BackendObserved))
		if len(seen) == 3 {
			cancel()
		}
		return nil
	})
	want := []controller.SSHState{controller.SSHZero, controller.SSHActive, controller.SSHUnknown}
	if !errors.Is(err, context.Canceled) || len(seen) != len(want) {
		t.Fatal(seen, err)
	}
	for i, state := range want {
		if !seen[i].Ready || seen[i].SSH != state {
			t.Fatalf("frame %d = %+v, want SSH %s", i, seen[i], state)
		}
	}
}
func TestAgentWatchEOFInvalidatesObservation(t *testing.T) {
	a := watchAgent(t, func(conn net.Conn) {
		_ = json.NewEncoder(conn).Encode(map[string]any{"status": "ok", "kind": "snapshot", "ssh_active": false, "source": "poll"})
	})
	var seen []controller.BackendObserved
	err := a.Watch(context.Background(), func(_ context.Context, event controller.Event) error {
		seen = append(seen, event.(controller.BackendObserved))
		return nil
	})
	if !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if len(seen) != 2 || !seen[0].Ready || seen[0].SSH != controller.SSHZero || seen[1].Ready || seen[1].SSH != controller.SSHUnknown {
		t.Fatal(seen)
	}
}
func TestAgentWatchCancellationClosesPendingRead(t *testing.T) {
	accepted := make(chan struct{})
	a := watchAgent(t, func(conn net.Conn) {
		close(accepted)
		_, _ = io.Copy(io.Discard, conn)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	events := 0
	go func() {
		done <- a.Watch(ctx, func(context.Context, controller.Event) error {
			events++
			return nil
		})
	}()
	select {
	case <-accepted:
	case <-time.After(time.Second):
		t.Fatal("watch not accepted")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || events != 0 {
			t.Fatal(err, events)
		}
	case <-time.After(time.Second):
		t.Fatal("watch did not stop")
	}
}
