package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/111inhistory/lsw-rdp-launcher-go/internal/controller"
)

func okReply(extra map[string]any) map[string]any {
	reply := map[string]any{"protocol": agentProtocol, "status": "ok"}
	for key, value := range extra {
		reply[key] = value
	}
	return reply
}

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
		if req.Protocol != agentProtocol {
			t.Errorf("request protocol = %d, want %d", req.Protocol, agentProtocol)
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
		if req.Protocol != agentProtocol {
			t.Errorf("watch protocol = %d, want %d", req.Protocol, agentProtocol)
			return
		}
		handle(conn)
	})
}

func sampleRef() controller.SessionRef {
	return controller.SessionRef{
		SessionID:        4,
		AuthenticationID: "0000000003C517EA",
		BootIdentifier:   "11111111-2222-4333-8444-555555555555",
	}
}

func sampleSnapshot(revision uint64, fresh bool) controller.SessionSnapshot {
	return controller.SessionSnapshot{
		Revision: revision,
		Fresh:    fresh,
		Sessions: []controller.Login{{
			Reference:     sampleRef(),
			SID:           "S-1-5-21-1",
			User:          "user",
			Domain:        "MACHINE",
			State:         controller.WTSActive,
			ClientName:    "lswD2210a001",
			ClientAddress: "192.0.2.10",
			LogonTime:     100,
			ConnectTime:   200,
		}},
	}
}

func TestAgentSendsTokenForEveryAction(t *testing.T) {
	for _, action := range []string{
		"ping", "status", "run", "open", "list_apps", "get_mapped_drives",
	} {
		t.Run(action, func(t *testing.T) {
			a := jsonAgent(t, func(req agentRequest) any {
				if req.Action != action || req.Token != "shared-token" {
					t.Errorf("request = %+v", req)
				}
				if req.SessionRef != nil {
					t.Errorf("backend-only action carried session_ref: %+v", req.SessionRef)
				}
				return okReply(nil)
			})
			a.AuthToken = func(context.Context) (string, error) { return "shared-token", nil }
			if _, err := a.Call(context.Background(), controller.Request{Action: action}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAgentSessionActionsCarryTokenAndReference(t *testing.T) {
	ref := sampleRef()
	a := jsonAgent(t, func(req agentRequest) any {
		if req.Token != "shared-token" {
			t.Errorf("token = %q", req.Token)
		}
		switch req.Action {
		case "inspect_sessions":
			if req.User != "user" || req.SessionRef != nil {
				t.Errorf("inspect request = %+v", req)
			}
			return okReply(map[string]any{"snapshot": map[string]any{"revision": 3, "fresh": true, "sessions": []any{}}})
		case "session_status":
			if req.SessionRef == nil || *req.SessionRef != ref {
				t.Errorf("session_status ref = %+v", req.SessionRef)
			}
			active := true
			return okReply(map[string]any{"role": "user", "session_ref": ref, "session_active": active, "activity": map[string]any{"reference": ref, "sid": "S-1-5-21-1", "session_id": 4, "user": `MACHINE\user`}})
		case "logoff_session":
			if req.User != "user" || req.SessionRef == nil || *req.SessionRef != ref {
				t.Errorf("logoff request = %+v", req)
			}
			return map[string]any{"protocol": agentProtocol, "status": "error", "message": "session ID was reused", "outcome": "anomaly"}
		default:
			t.Errorf("unexpected action %q", req.Action)
			return okReply(nil)
		}
	})
	a.AuthToken = func(context.Context) (string, error) { return "shared-token", nil }

	inspect, err := a.InspectSessions(context.Background(), "user")
	if err != nil || inspect.Snapshot == nil || inspect.Snapshot.Revision != 3 || inspect.Snapshot.ObservedAt.IsZero() {
		t.Fatalf("inspect = %+v, err=%v", inspect, err)
	}
	status, err := a.SessionStatus(context.Background(), ref)
	if err != nil || status.SessionRef == nil || *status.SessionRef != ref || status.SessionActive == nil || !*status.SessionActive || status.Activity == nil || status.Activity.SessionID != 4 {
		t.Fatalf("session_status = %+v, err=%v", status, err)
	}
	logoff, err := a.call(context.Background(), agentRequest{Action: "logoff_session", User: "user", SessionRef: &ref})
	if err == nil || logoff.Outcome != "anomaly" || !strings.Contains(err.Error(), "reused") {
		t.Fatalf("logoff = %+v, err=%v", logoff, err)
	}
}

func TestAgentCallInSessionCarriesExactReference(t *testing.T) {
	ref := sampleRef()
	a := fakeAgent(t, func(conn net.Conn) {
		var raw map[string]any
		if err := json.NewDecoder(conn).Decode(&raw); err != nil {
			t.Error(err)
			return
		}
		if raw["protocol"] != float64(agentProtocol) || raw["action"] != "run" || raw["target"] != `C:\Program Files\app.exe` || raw["params"] != `"J:\some file.docx"` || raw["working_dir"] != `C:\work` {
			t.Errorf("request = %+v", raw)
		}
		for _, legacy := range []string{"fingerprint", "require_empty", "session_id"} {
			if _, exists := raw[legacy]; exists {
				t.Errorf("legacy field %q survived in request: %+v", legacy, raw)
			}
		}
		want := map[string]any{"session_id": float64(4), "authentication_id": ref.AuthenticationID, "boot_identifier": ref.BootIdentifier}
		got, _ := raw["session_ref"].(map[string]any)
		if len(got) != len(want) {
			t.Errorf("session_ref = %+v", raw["session_ref"])
		}
		for key, value := range want {
			if got[key] != value {
				t.Errorf("session_ref[%s] = %v, want %v", key, got[key], value)
			}
		}
		_ = json.NewEncoder(conn).Encode(okReply(nil))
	})
	if _, err := a.CallInSession(context.Background(), controller.Request{Action: "run", UserContext: true, Target: `C:\Program Files\app.exe`, Params: `"J:\some file.docx"`, WorkingDir: `C:\work`}, ref); err != nil {
		t.Fatal(err)
	}
}

func TestAgentMutatingTransportFailureIsUnknownAndNotRetried(t *testing.T) {
	ref := sampleRef()
	cases := []struct {
		action string
		call   func(Agent) (AgentResponse, error)
	}{
		{
			action: "run",
			call: func(a Agent) (AgentResponse, error) {
				return a.CallInSession(context.Background(), controller.Request{Action: "run", UserContext: true, Target: "notepad.exe"}, ref)
			},
		},
		{
			action: "open",
			call: func(a Agent) (AgentResponse, error) {
				return a.CallInSession(context.Background(), controller.Request{Action: "open", UserContext: true, File: `C:\work\file.txt`}, ref)
			},
		},
		{
			action: "logoff_session",
			call: func(a Agent) (AgentResponse, error) {
				return a.call(context.Background(), agentRequest{Action: "logoff_session", User: "user", SessionRef: &ref})
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.action, func(t *testing.T) {
			var received atomic.Int32
			a := fakeAgent(t, func(conn net.Conn) {
				var req agentRequest
				if err := json.NewDecoder(conn).Decode(&req); err != nil {
					t.Error(err)
					return
				}
				if req.Action != tc.action {
					t.Errorf("request action = %q, want %q", req.Action, tc.action)
				}
				received.Add(1)
				// Return without a response: the action may already have run.
			})
			_, err := tc.call(a)
			if err == nil {
				t.Fatal("transport failure was reported as success")
			}
			if !strings.Contains(err.Error(), tc.action+" submitted; remote outcome unknown; no automatic retry") {
				t.Fatalf("error did not mark the submitted action unknown: %v", err)
			}
			if !errors.Is(err, io.EOF) {
				t.Fatalf("underlying transport error was not preserved: %v", err)
			}
			time.Sleep(100 * time.Millisecond)
			if got := received.Load(); got != 1 {
				t.Fatalf("request was sent %d times, want exactly one attempt", got)
			}
		})
	}
}

func TestAgentReadOnlyTransportFailureStaysOrdinary(t *testing.T) {
	a := fakeAgent(t, func(conn net.Conn) {
		var req agentRequest
		if err := json.NewDecoder(conn).Decode(&req); err != nil {
			t.Error(err)
			return
		}
	})
	_, err := a.Call(context.Background(), controller.Request{Action: "ping"})
	if !errors.Is(err, io.EOF) {
		t.Fatalf("ping error = %v, want EOF", err)
	}
	if strings.Contains(err.Error(), "submitted; remote outcome unknown") {
		t.Fatalf("read-only ping was mislabeled as a submitted mutation: %v", err)
	}
}

func TestAgentDialFailureIsNotMarkedSubmitted(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := listener.Addr().String()
	listener.Close()
	a := Agent{Endpoint: endpoint}
	_, err = a.CallInSession(context.Background(), controller.Request{Action: "run", UserContext: true, Target: "notepad.exe"}, sampleRef())
	if err == nil {
		t.Fatal("dial to a closed listener succeeded")
	}
	if strings.Contains(err.Error(), "submitted; remote outcome unknown") {
		t.Fatalf("dial failure was mislabeled as a submitted action: %v", err)
	}
}

func TestAgentStructuredOutcomeAppearsInError(t *testing.T) {
	ref := sampleRef()
	for _, outcome := range []string{"unknown", "not_started", "failed", "anomaly"} {
		t.Run(outcome, func(t *testing.T) {
			a := jsonAgent(t, func(req agentRequest) any {
				if req.Action != "logoff_session" {
					t.Errorf("request action = %q", req.Action)
				}
				return map[string]any{"protocol": agentProtocol, "status": "error", "message": "logoff result", "outcome": outcome}
			})
			response, err := a.call(context.Background(), agentRequest{Action: "logoff_session", User: "user", SessionRef: &ref})
			if err == nil {
				t.Fatal("structured failure was reported as success")
			}
			if !strings.Contains(err.Error(), "logoff result") || !strings.Contains(err.Error(), "outcome "+outcome) {
				t.Fatalf("error lost the structured outcome: %v", err)
			}
			if response.Outcome != outcome {
				t.Fatalf("response outcome = %q, want %q", response.Outcome, outcome)
			}
		})
	}
}

func TestAgentProtocolMismatchIsExplicit(t *testing.T) {
	for _, protocol := range []int{0, 1, 3} {
		t.Run(fmt.Sprint(protocol), func(t *testing.T) {
			a := jsonAgent(t, func(agentRequest) any {
				return map[string]any{"status": "ok", "protocol": protocol}
			})
			// jsonAgent asserts our request protocol, not the reply protocol.
			if _, err := a.Call(context.Background(), controller.Request{Action: "status"}); err == nil || !strings.Contains(err.Error(), "upgraded together") {
				t.Fatalf("protocol %d response error = %v", protocol, err)
			}
		})
	}
}

func TestAgentWatchProtocolMismatchInvalidatesSessions(t *testing.T) {
	a := watchAgent(t, func(conn net.Conn) {
		_ = json.NewEncoder(conn).Encode(map[string]any{"status": "ok", "ssh_active": false})
	})
	var backend []controller.BackendObserved
	var sessions []controller.SessionSnapshot
	err := a.Watch(context.Background(), func(_ context.Context, event controller.Event) error {
		switch e := event.(type) {
		case controller.BackendObserved:
			backend = append(backend, e)
		case controller.SessionsObserved:
			sessions = append(sessions, e.Snapshot)
		}
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "upgraded together") {
		t.Fatalf("watch error = %v", err)
	}
	if len(backend) != 1 || backend[0].Ready || backend[0].SSH != controller.SSHUnknown {
		t.Fatalf("backend events = %+v", backend)
	}
	if len(sessions) != 1 || sessions[0].Fresh {
		t.Fatalf("session invalidation = %+v", sessions)
	}
}

func TestAgentSendsProviderTokenOnEveryCall(t *testing.T) {
	a := jsonAgent(t, func(req agentRequest) any {
		if req.Token != "shared-token" {
			t.Errorf("token = %q", req.Token)
		}
		return okReply(nil)
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
	if strings.Contains(err.Error(), "submitted; remote outcome unknown") {
		t.Fatalf("token failure was mislabeled as a submitted action: %v", err)
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
		if req.Action != "watch_status" || req.Token != "shared-token" || req.Protocol != agentProtocol {
			t.Errorf("watch request = %+v", req)
			return
		}
		_ = json.NewEncoder(conn).Encode(okReply(map[string]any{"kind": "snapshot", "ssh_active": false}))
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

func TestAgentAppAndDriveRequests(t *testing.T) {
	a := jsonAgent(t, func(req agentRequest) any {
		switch req.Action {
		case "list_apps":
			if !req.WithIcons {
				t.Error("with_icons was not sent")
			}
			return okReply(map[string]any{"apps": []any{map[string]any{"id": "word", "name": "Word", "app_type": "win32", "target": "WINWORD.EXE", "arguments": nil}}})
		case "get_mapped_drives":
			return okReply(map[string]any{"drives": [][2]string{{"J:", `\\server\share`}}})
		default:
			t.Error("unexpected request", req)
			return map[string]any{"protocol": agentProtocol, "status": "error"}
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

func TestAgentAcceptsLargeIconResponse(t *testing.T) {
	icon := strings.Repeat("A", 1<<20)
	a := jsonAgent(t, func(agentRequest) any {
		return okReply(map[string]any{"apps": []any{map[string]any{
			"id": "uwp", "name": "UWP", "app_type": "uwp", "target": "Package!App", "icon_base64": icon,
		}}})
	})
	response, err := a.Call(context.Background(), controller.Request{Action: "list_apps", WithIcons: true})
	if err != nil || len(response.Apps) != 1 || len(response.Apps[0].IconBase64) != len(icon) {
		t.Fatalf("large icon response apps=%d, err=%v", len(response.Apps), err)
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

type watchEvents struct {
	memory   []*controller.GuestMemory
	sessions []controller.SessionSnapshot
	backend  []controller.BackendObserved
}

func collectWatch(t *testing.T, a Agent, stop func(watchEvents) bool) (watchEvents, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var events watchEvents
	err := a.Watch(ctx, func(_ context.Context, event controller.Event) error {
		switch e := event.(type) {
		case controller.MemoryObserved:
			events.memory = append(events.memory, e.Memory)
		case controller.SessionsObserved:
			events.sessions = append(events.sessions, e.Snapshot)
		case controller.BackendObserved:
			events.backend = append(events.backend, e)
		}
		if stop(events) {
			cancel()
		}
		return nil
	})
	return events, err
}

func TestAgentWatchReportsInitialSnapshotAndSessions(t *testing.T) {
	snapshot := sampleSnapshot(1, true)
	a := watchAgent(t, func(conn net.Conn) {
		_ = json.NewEncoder(conn).Encode(map[string]any{
			"protocol": agentProtocol, "status": "ok", "kind": "snapshot",
			"ssh_active": false, "source": "poll", "snapshot": snapshot,
		})
		_, _ = io.Copy(io.Discard, conn)
	})
	events, err := collectWatch(t, a, func(events watchEvents) bool {
		return len(events.backend) == 1 && len(events.sessions) == 1
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(events.backend) != 1 || !events.backend[0].Ready || events.backend[0].SSH != controller.SSHZero {
		t.Fatalf("backend = %+v", events.backend)
	}
	if len(events.sessions) != 1 || !events.sessions[0].Fresh || events.sessions[0].Revision != 1 || events.sessions[0].ObservedAt.IsZero() || !events.sessions[0].ObservedAt.After(time.Now().Add(-time.Minute)) {
		t.Fatalf("sessions = %+v", events.sessions)
	}
}

func TestAgentWatchStreamsSnapshotChangesAndUnknown(t *testing.T) {
	a := watchAgent(t, func(conn net.Conn) {
		for _, frame := range []map[string]any{
			{"protocol": agentProtocol, "status": "ok", "kind": "snapshot", "ssh_active": false, "source": "poll"},
			{"protocol": agentProtocol, "status": "ok", "kind": "changed", "ssh_active": true, "source": "events"},
			{"protocol": agentProtocol, "status": "ok", "kind": "changed", "ssh_active": nil, "source": "poll"},
		} {
			if err := json.NewEncoder(conn).Encode(frame); err != nil {
				t.Error(err)
				return
			}
		}
		_, _ = io.Copy(io.Discard, conn)
	})
	events, err := collectWatch(t, a, func(events watchEvents) bool { return len(events.backend) == 3 })
	want := []controller.SSHState{controller.SSHZero, controller.SSHActive, controller.SSHUnknown}
	if !errors.Is(err, context.Canceled) || len(events.backend) != len(want) {
		t.Fatal(events.backend, err)
	}
	for i, state := range want {
		if !events.backend[i].Ready || events.backend[i].SSH != state {
			t.Fatalf("frame %d = %+v, want SSH %s", i, events.backend[i], state)
		}
	}
}

func TestAgentWatchPartialFramesRetainLastKnownSSH(t *testing.T) {
	a := watchAgent(t, func(conn net.Conn) {
		frames := []map[string]any{
			{"protocol": agentProtocol, "status": "ok", "kind": "snapshot", "ssh_active": false, "snapshot": sampleSnapshot(1, true)},
			{"protocol": agentProtocol, "status": "ok", "kind": "changed", "memory": map[string]any{"total_bytes": 4000, "available_bytes": 200}},
			{"protocol": agentProtocol, "status": "ok", "kind": "changed", "ssh_active": nil},
		}
		for _, frame := range frames {
			if err := json.NewEncoder(conn).Encode(frame); err != nil {
				t.Error(err)
				return
			}
		}
		_, _ = io.Copy(io.Discard, conn)
	})
	events, err := collectWatch(t, a, func(events watchEvents) bool { return len(events.backend) == 3 })
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(events.backend) != 3 || events.backend[0].SSH != controller.SSHZero || events.backend[1].SSH != controller.SSHZero || events.backend[2].SSH != controller.SSHUnknown {
		t.Fatalf("backend = %+v", events.backend)
	}
	if len(events.sessions) != 1 {
		t.Fatalf("memory-only frames must not re-emit or refresh sessions: %+v", events.sessions)
	}
	if len(events.memory) != 1 || events.memory[0] == nil || events.memory[0].TotalBytes != 4000 {
		t.Fatalf("memory = %+v", events.memory)
	}
}

func TestAgentWatchEOFInvalidatesSessions(t *testing.T) {
	a := watchAgent(t, func(conn net.Conn) {
		_ = json.NewEncoder(conn).Encode(map[string]any{
			"protocol": agentProtocol, "status": "ok", "kind": "snapshot",
			"ssh_active": false, "snapshot": sampleSnapshot(7, true),
		})
	})
	events, err := collectWatch(t, a, func(watchEvents) bool { return false })
	if !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if len(events.backend) != 2 || !events.backend[0].Ready || events.backend[0].SSH != controller.SSHZero || events.backend[1].Ready || events.backend[1].SSH != controller.SSHUnknown {
		t.Fatalf("backend = %+v", events.backend)
	}
	if len(events.sessions) != 2 || !events.sessions[0].Fresh || events.sessions[0].Revision != 7 || events.sessions[1].Fresh || events.sessions[1].Revision != 7 || len(events.sessions[1].Sessions) != 1 {
		t.Fatalf("sessions = %+v", events.sessions)
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
