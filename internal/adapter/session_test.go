package adapter

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/111inhistory/lsw-rdp-launcher-go/internal/controller"
)

func TestBackendSubscriptionRefreshesOnVMResumeWithoutHeartbeat(t *testing.T) {
	var subscriptions atomic.Int32
	agent := watchAgent(t, func(conn net.Conn) {
		subscriptions.Add(1)
		_ = json.NewEncoder(conn).Encode(map[string]any{"protocol": agentProtocol, "status": "ok", "kind": "snapshot", "ssh_active": false})
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

func bindingLogin(clientName, sid, user string, state int, ref controller.SessionRef) controller.Login {
	return controller.Login{
		Reference:     ref,
		SID:           sid,
		User:          user,
		Domain:        "MACHINE",
		State:         state,
		ClientName:    clientName,
		ClientAddress: "192.0.2.10",
		LogonTime:     100,
		ConnectTime:   200,
	}
}

func bindingSnapshot(revision uint64, fresh bool, logins ...controller.Login) controller.SessionSnapshot {
	return controller.SessionSnapshot{Revision: revision, Fresh: fresh, Sessions: logins}
}

func workerStatusReply(ref controller.SessionRef, sid, user string, active bool) map[string]any {
	return okReply(map[string]any{
		"role":           "user",
		"session_ref":    ref,
		"session_active": active,
		"activity": map[string]any{
			"reference": ref, "sid": sid, "session_id": ref.SessionID, "user": user,
		},
	})
}

func bindingSession(agent Agent) *Session {
	return &Session{
		agent:       agent,
		sessionUser: "user",
		display: &Display{
			config: DisplayConfig{ClientHostname: "rdp-test-client"},
			exited: make(chan struct{}),
		},
	}
}

func TestWaitBindingPermitsOnlyWorkerConfirmedActiveLogon(t *testing.T) {
	ref := sampleRef()
	login := bindingLogin("rdp-test-client", "S-1-5-21-1", "user", controller.WTSActive, ref)
	var statusCalls atomic.Int32
	agent := jsonAgent(t, func(req agentRequest) any {
		switch req.Action {
		case "inspect_sessions":
			return okReply(map[string]any{"snapshot": bindingSnapshot(3, true, login)})
		case "session_status":
			statusCalls.Add(1)
			if req.SessionRef == nil || *req.SessionRef != ref {
				t.Errorf("session_status ref = %+v", req.SessionRef)
			}
			return workerStatusReply(ref, login.SID, `MACHINE\user`, true)
		default:
			t.Errorf("unexpected action %q", req.Action)
			return map[string]any{"protocol": agentProtocol, "status": "error"}
		}
	})
	s := bindingSession(agent)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	binding, snapshot, err := s.waitBinding(ctx, nil)
	if err != nil || binding == nil || binding.Reference != ref || binding.SID != login.SID || snapshot == nil || !snapshot.Fresh || statusCalls.Load() != 1 {
		t.Fatalf("binding=%+v snapshot=%+v statusCalls=%d err=%v", binding, snapshot, statusCalls.Load(), err)
	}
}

func TestWaitBindingIgnoresBaselineLogonUntilConnectTimeChanges(t *testing.T) {
	ref := sampleRef()
	hostname := "rdp-test-client"
	old := bindingLogin(hostname, "S-1-5-21-1", "user", controller.WTSActive, ref)
	reconnected := old
	reconnected.ConnectTime = old.ConnectTime + 100
	before := bindingSnapshot(1, true, old)
	var inspects, statusCalls atomic.Int32
	agent := jsonAgent(t, func(req agentRequest) any {
		switch req.Action {
		case "inspect_sessions":
			// The lingering pre-connection Active login must not bind. A
			// reconnect of the same SessionRef is accepted only once its
			// ConnectTime changes.
			if inspects.Add(1) == 1 {
				return okReply(map[string]any{"snapshot": bindingSnapshot(2, true, old)})
			}
			return okReply(map[string]any{"snapshot": bindingSnapshot(3, true, reconnected)})
		case "session_status":
			statusCalls.Add(1)
			if req.SessionRef == nil || *req.SessionRef != ref {
				t.Errorf("session_status ref = %+v", req.SessionRef)
			}
			return workerStatusReply(ref, reconnected.SID, `MACHINE\user`, true)
		default:
			t.Errorf("unexpected action %q", req.Action)
			return map[string]any{"protocol": agentProtocol, "status": "error"}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	binding, snapshot, err := bindingSession(agent).waitBinding(ctx, &before)
	if err != nil || binding == nil || binding.Reference != ref || binding.ConnectTime != reconnected.ConnectTime || snapshot == nil || snapshot.Revision != 3 {
		t.Fatalf("binding=%+v snapshot=%+v err=%v", binding, snapshot, err)
	}
	if statusCalls.Load() != 1 {
		t.Fatalf("worker status calls = %d, want 1 after reconnect", statusCalls.Load())
	}
}

func TestWaitBindingOverlapAcceptsOnlyNewLogon(t *testing.T) {
	oldRef := sampleRef()
	newRef := controller.SessionRef{SessionID: oldRef.SessionID + 2, AuthenticationID: "00000000000003E8", BootIdentifier: oldRef.BootIdentifier}
	hostname := "rdp-test-client"
	old := bindingLogin(hostname, "S-1-5-21-1", "user", controller.WTSActive, oldRef)
	created := bindingLogin(hostname, "S-1-5-21-1", "user", controller.WTSActive, newRef)
	created.ConnectTime = old.ConnectTime + 100
	before := bindingSnapshot(1, true, old)
	var statusCalls atomic.Int32
	agent := jsonAgent(t, func(req agentRequest) any {
		switch req.Action {
		case "inspect_sessions":
			// Both are Active during a cross-mode transition. The unchanged
			// baseline login is ignored; the newly created login binds.
			return okReply(map[string]any{"snapshot": bindingSnapshot(2, true, old, created)})
		case "session_status":
			statusCalls.Add(1)
			if req.SessionRef == nil || *req.SessionRef != newRef {
				t.Errorf("worker was asked about old/unexpected ref %+v", req.SessionRef)
			}
			return workerStatusReply(newRef, created.SID, `MACHINE\user`, true)
		default:
			t.Errorf("unexpected action %q", req.Action)
			return map[string]any{"protocol": agentProtocol, "status": "error"}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	binding, snapshot, err := bindingSession(agent).waitBinding(ctx, &before)
	if err != nil || binding == nil || binding.Reference != newRef || snapshot == nil || snapshot.Revision != 2 {
		t.Fatalf("binding=%+v snapshot=%+v err=%v", binding, snapshot, err)
	}
	if statusCalls.Load() != 1 {
		t.Fatalf("worker status calls = %d, want 1 for the new logon", statusCalls.Load())
	}
}

func TestWaitBindingRejectsUnconfirmedOrAmbiguousLogons(t *testing.T) {
	ref := sampleRef()
	hostname := "rdp-test-client"
	matching := bindingLogin(hostname, "S-1-5-21-1", "user", controller.WTSActive, ref)
	cases := []struct {
		name       string
		snapshot   controller.SessionSnapshot
		status     func(controller.SessionRef) any
		wantErr    string
		wantStatus int32
	}{
		{
			name:     "wrong client hostname",
			snapshot: bindingSnapshot(1, true, bindingLogin("other-host", "S-1-5-21-1", "user", controller.WTSActive, ref)),
			wantErr:  "was not bound",
		},
		{
			name:     "wrong user",
			snapshot: bindingSnapshot(1, true, bindingLogin(hostname, "S-1-5-21-1", "other", controller.WTSActive, ref)),
			wantErr:  "was not bound",
		},
		{
			name:     "disconnected login",
			snapshot: bindingSnapshot(1, true, bindingLogin(hostname, "S-1-5-21-1", "user", controller.WTSDisconnected, ref)),
			wantErr:  "was not bound",
		},
		{
			name:     "stale snapshot",
			snapshot: bindingSnapshot(1, false, matching),
			wantErr:  "was not bound",
		},
		{
			name: "multiple active matches",
			snapshot: bindingSnapshot(1, true, matching,
				bindingLogin(hostname, "S-1-5-21-1", "user", controller.WTSActive, controller.SessionRef{SessionID: 6, AuthenticationID: "00000000000003E8", BootIdentifier: ref.BootIdentifier})),
			wantErr: "multiple active logons",
		},
		{
			name:     "worker sid mismatch",
			snapshot: bindingSnapshot(1, true, matching),
			status: func(controller.SessionRef) any {
				return workerStatusReply(ref, "S-1-5-21-OTHER", `MACHINE\user`, true)
			},
			wantErr:    "worker did not confirm",
			wantStatus: 1,
		},
		{
			name:     "worker ref mismatch",
			snapshot: bindingSnapshot(1, true, matching),
			status: func(controller.SessionRef) any {
				other := ref
				other.AuthenticationID = "00000000000003E8"
				return workerStatusReply(other, matching.SID, `MACHINE\user`, true)
			},
			wantErr:    "worker did not confirm",
			wantStatus: 1,
		},
		{
			name:     "worker reports disconnected",
			snapshot: bindingSnapshot(1, true, matching),
			status: func(controller.SessionRef) any {
				return workerStatusReply(ref, matching.SID, `MACHINE\user`, false)
			},
			wantErr:    "worker did not confirm",
			wantStatus: 1,
		},
		{
			name:     "worker unavailable",
			snapshot: bindingSnapshot(1, true, matching),
			status: func(controller.SessionRef) any {
				return map[string]any{"protocol": agentProtocol, "status": "error", "message": "worker did not start"}
			},
			wantErr:    "bound user worker unavailable",
			wantStatus: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var statusCalls atomic.Int32
			agent := jsonAgent(t, func(req agentRequest) any {
				switch req.Action {
				case "inspect_sessions":
					return okReply(map[string]any{"snapshot": tc.snapshot})
				case "session_status":
					statusCalls.Add(1)
					if tc.status == nil {
						t.Error("session_status was called for an unselected login")
						return map[string]any{"protocol": agentProtocol, "status": "error"}
					}
					if req.SessionRef == nil {
						t.Error("session_status is missing its session_ref")
						return map[string]any{"protocol": agentProtocol, "status": "error"}
					}
					return tc.status(*req.SessionRef)
				default:
					t.Errorf("unexpected action %q", req.Action)
					return map[string]any{"protocol": agentProtocol, "status": "error"}
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
			defer cancel()
			binding, _, err := bindingSession(agent).waitBinding(ctx, nil)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
			if binding != nil || statusCalls.Load() != tc.wantStatus {
				t.Fatalf("binding=%+v statusCalls=%d want %d", binding, statusCalls.Load(), tc.wantStatus)
			}
		})
	}
}

func TestWaitBindingStopsWhenOwnedFreeRDPExits(t *testing.T) {
	agent := jsonAgent(t, func(req agentRequest) any {
		if req.Action != "inspect_sessions" {
			t.Errorf("unexpected action %q", req.Action)
		}
		return okReply(map[string]any{"snapshot": bindingSnapshot(1, true, bindingLogin("other-host", "S-1-5-21-1", "user", controller.WTSActive, sampleRef()))})
	})
	s := bindingSession(agent)
	exited := make(chan struct{})
	s.display.exited = exited
	close(exited)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _, err := s.waitBinding(ctx, nil)
	if err == nil || !strings.Contains(err.Error(), "FreeRDP exited before") {
		t.Fatal(err)
	}
}

func TestDispatchRoutesCurrentUserPayloadAndBackendOnlyRequests(t *testing.T) {
	ref := sampleRef()
	var backendOnly, user atomic.Int32
	agent := fakeAgent(t, func(conn net.Conn) {
		var req agentRequest
		if err := json.NewDecoder(conn).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		var reply any
		switch req.Action {
		case "status":
			backendOnly.Add(1)
			if req.SessionRef != nil {
				t.Errorf("backend-only status carried session_ref: %+v", req.SessionRef)
			}
			reply = okReply(map[string]any{"role": "background"})
		case "run":
			user.Add(1)
			if req.SessionRef == nil || *req.SessionRef != ref || req.Target != "notepad.exe" || req.Params != `"J:\report.docx"` || req.WorkingDir != `C:\work` {
				t.Errorf("run request = %+v", req)
			}
			reply = okReply(nil)
		default:
			t.Errorf("unexpected action %q", req.Action)
			reply = map[string]any{"protocol": agentProtocol, "status": "error"}
		}
		_ = json.NewEncoder(conn).Encode(reply)
	})
	s := &Session{agent: agent}
	out := s.execute(context.Background(), controller.Operation{
		Kind:      controller.Dispatch,
		Reference: ref,
		Request: controller.Request{
			Action: "run", UserContext: true, Mode: controller.ModeRemoteApp, Target: "notepad.exe",
			Params: `"J:\report.docx"`, WorkingDir: `C:\work`,
		},
	})
	if out.Err != nil {
		t.Fatal(out.Err)
	}
	out = s.execute(context.Background(), controller.Operation{Kind: controller.Dispatch, Request: controller.Request{Action: "status"}})
	if out.Err != nil || backendOnly.Load() != 1 || user.Load() != 1 {
		t.Fatal(out, backendOnly.Load(), user.Load())
	}
}

func TestDispatchExecutesCurrentDesktopPayloadInBoundSession(t *testing.T) {
	ref := sampleRef()
	for _, mode := range []string{controller.ModeDesktop, controller.ModeRemoteApp} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			agent := jsonAgent(t, func(req agentRequest) any {
				calls.Add(1)
				if req.Action != "open" || req.File != `C:\work\report.txt` || req.SessionRef == nil || *req.SessionRef != ref {
					t.Errorf("current-desktop request = %+v", req)
				}
				return okReply(nil)
			})
			s := &Session{agent: agent}
			out := s.execute(context.Background(), controller.Operation{
				Kind:      controller.Dispatch,
				Reference: ref,
				Request:   controller.Request{Action: "open", UserContext: true, Mode: mode, File: `C:\work\report.txt`},
			})
			if out.Err != nil || calls.Load() != 1 {
				t.Fatalf("out=%+v calls=%d", out, calls.Load())
			}
		})
	}
}

func TestDispatchConnectRequiresBoundReference(t *testing.T) {
	s := &Session{}
	out := s.execute(context.Background(), controller.Operation{
		Kind:      controller.Dispatch,
		Reference: sampleRef(),
		Request:   controller.Request{Action: "connect", UserContext: true, Mode: controller.ModeDesktop},
	})
	response, ok := out.Data.(AgentResponse)
	if out.Err != nil || !ok || response.Message != "display bound to user logon" || response.SessionRef == nil || *response.SessionRef != sampleRef() {
		t.Fatal(out)
	}
	out = s.execute(context.Background(), controller.Operation{Kind: controller.Dispatch, Request: controller.Request{Action: "connect", UserContext: true}})
	if out.Err == nil || !strings.Contains(out.Err.Error(), "not bound") {
		t.Fatal(out)
	}
}

func TestDispatchBatchStopsWithPartialResultsAndFailingIndex(t *testing.T) {
	ref := sampleRef()
	var calls atomic.Int32
	agent := jsonAgent(t, func(req agentRequest) any {
		calls.Add(1)
		if req.SessionRef == nil || *req.SessionRef != ref {
			t.Errorf("batch command lost session_ref: %+v", req.SessionRef)
		}
		switch req.File {
		case "one.txt":
			return okReply(nil)
		case "two.txt":
			return map[string]any{"protocol": agentProtocol, "status": "error", "message": "second open failed"}
		default:
			t.Errorf("batch continued after failure with %q", req.File)
			return map[string]any{"protocol": agentProtocol, "status": "error"}
		}
	})
	s := &Session{agent: agent}
	out := s.execute(context.Background(), controller.Operation{
		Kind:      controller.Dispatch,
		Reference: ref,
		Request: controller.Request{
			Action: "batch", UserContext: true, Mode: controller.ModeRemoteApp,
			Batch: []controller.Request{
				{Action: "open", File: "one.txt"},
				{Action: "open", File: "two.txt"},
				{Action: "open", File: "three.txt"},
			},
		},
	})
	if out.Err == nil || !strings.Contains(out.Err.Error(), "batch stopped after 1 of 3") {
		t.Fatal(out.Err)
	}
	partial, ok := out.Data.(map[string]any)
	if !ok {
		t.Fatalf("batch data = %#v", out.Data)
	}
	results, ok := partial["results"].([]AgentResponse)
	if !ok || len(results) != 1 || partial["completed"] != 1 || partial["failed_index"] != 1 {
		t.Fatalf("partial batch = %#v", partial)
	}
	if calls.Load() != 2 {
		t.Fatalf("batch executed %d commands after a failure", calls.Load())
	}
}

func TestDispatchUserContextWithoutModeUsesBoundSession(t *testing.T) {
	ref := sampleRef()
	agent := jsonAgent(t, func(req agentRequest) any {
		if req.Action != "open" || req.File != `C:\work\report.txt` {
			t.Errorf("unexpected request = %+v", req)
		}
		if req.SessionRef == nil || *req.SessionRef != ref {
			t.Errorf("mode-neutral user request did not carry the bound reference: %+v", req.SessionRef)
		}
		return okReply(nil)
	})
	out := (&Session{agent: agent}).execute(context.Background(), controller.Operation{
		Kind:      controller.Dispatch,
		Reference: ref,
		Request:   controller.Request{Action: "open", UserContext: true, File: `C:\work\report.txt`},
	})
	if out.Err != nil {
		t.Fatal(out.Err)
	}
}

func TestOperationTimeoutBudgets(t *testing.T) {
	if got := operationTimeout(controller.StartDisplay); got != 90*time.Second {
		t.Fatalf("StartDisplay timeout = %v, want 90s", got)
	}
	if got := operationTimeout(controller.InspectSession); got != 30*time.Second {
		t.Fatalf("InspectSession timeout = %v, want 30s", got)
	}
	logoff := operationTimeout(controller.LogoffSession)
	if logoff != agentTimeout("logoff_session") {
		t.Fatalf("LogoffSession timeout = %v, agent budget = %v", logoff, agentTimeout("logoff_session"))
	}
	if logoff <= 30*time.Second {
		t.Fatalf("LogoffSession timeout was capped at 30s: %v", logoff)
	}
	if got := operationTimeout(controller.QueryVM); got != 15*time.Second {
		t.Fatalf("default timeout = %v, want 15s", got)
	}
}

func assertInspectUsesOperationDeadline(t *testing.T, caller context.Context, opDeadline time.Time) {
	t.Helper()
	seen := make(chan time.Time, 1)
	agent := Agent{AuthToken: func(ctx context.Context) (string, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Error("inspection context has no deadline")
			seen <- time.Time{}
			return "", context.DeadlineExceeded
		}
		seen <- deadline
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Second):
			t.Error("inspection context was not bounded by the operation deadline")
			return "", context.DeadlineExceeded
		}
	}}
	start := time.Now()
	out := (&Session{agent: agent, sessionUser: "user"}).execute(caller, controller.Operation{
		Kind:     controller.InspectSession,
		Deadline: opDeadline,
	})
	elapsed := time.Since(start)
	if out.Err == nil {
		t.Fatal("inspection with an expired/short operation deadline succeeded")
	}
	if elapsed >= 1500*time.Millisecond {
		t.Fatalf("inspection exceeded its operation deadline: %v", elapsed)
	}
	var got time.Time
	select {
	case got = <-seen:
	case <-time.After(time.Second):
		t.Fatal("inspection provider was not called")
	}
	want := opDeadline
	if callerDeadline, ok := caller.Deadline(); ok && callerDeadline.Before(want) {
		want = callerDeadline
	}
	if got.After(want.Add(20 * time.Millisecond)) {
		t.Fatalf("inspection deadline = %v, want no later than %v", got, want)
	}
}

func TestInspectSessionOperationDeadlineCapsInspection(t *testing.T) {
	t.Run("expired", func(t *testing.T) {
		assertInspectUsesOperationDeadline(t, context.Background(), time.Now().Add(-time.Second))
	})
	t.Run("short", func(t *testing.T) {
		assertInspectUsesOperationDeadline(t, context.Background(), time.Now().Add(50*time.Millisecond))
	})
	t.Run("caller shorter", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
		defer cancel()
		assertInspectUsesOperationDeadline(t, ctx, time.Now().Add(5*time.Second))
	})
}

func TestLogoffOperationHonorsShorterCallerContext(t *testing.T) {
	agent := fakeAgent(t, func(conn net.Conn) {
		var req agentRequest
		if err := json.NewDecoder(conn).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		if req.Action != "logoff_session" {
			t.Errorf("request action = %q", req.Action)
		}
		_, _ = io.Copy(io.Discard, conn)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	out := (&Session{agent: agent, sessionUser: "user"}).execute(ctx, controller.Operation{
		Kind:      controller.LogoffSession,
		Reference: sampleRef(),
	})
	if out.Err == nil {
		t.Fatal("logoff without a response succeeded")
	}
	if elapsed := time.Since(start); elapsed >= 2*time.Second {
		t.Fatalf("caller deadline did not cap logoff wait: %v", elapsed)
	}
}

func startDisplaySession(t *testing.T, agent Agent) (*Session, context.Context, context.CancelFunc) {
	t.Helper()
	t.Setenv("RDP_TEST_CHILD", "1")
	root, cancelRoot := context.WithTimeout(context.Background(), 5*time.Second)
	config := displayTestConfig()
	config.Log = io.Discard
	display := NewDisplay(root, config, nil)
	s := &Session{agent: agent, display: display, sessionUser: "user"}
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.WithoutCancel(root), 2*time.Second)
		_ = display.Stop(stop)
		cancel()
		cancelRoot()
	})
	ctx, cancel := context.WithTimeout(root, 2*time.Second)
	return s, ctx, cancel
}

func TestStartDisplayBindsOnlyAfterWorkerConfirmation(t *testing.T) {
	ref := sampleRef()
	login := bindingLogin("rdp-test-client", "S-1-5-21-1", "user", controller.WTSActive, ref)
	var inspects atomic.Int32
	agent := jsonAgent(t, func(req agentRequest) any {
		switch req.Action {
		case "inspect_sessions":
			// The pre-connection baseline must not already contain the new
			// connection's logon; only the post-spawn observation may bind.
			if inspects.Add(1) == 1 {
				return okReply(map[string]any{"snapshot": bindingSnapshot(1, true)})
			}
			return okReply(map[string]any{"snapshot": bindingSnapshot(2, true, login)})
		case "session_status":
			return workerStatusReply(ref, login.SID, `MACHINE\user`, true)
		default:
			t.Errorf("unexpected action %q", req.Action)
			return map[string]any{"protocol": agentProtocol, "status": "error"}
		}
	})
	s, ctx, cancel := startDisplaySession(t, agent)
	defer cancel()
	out := s.execute(ctx, controller.Operation{Kind: controller.StartDisplay, ID: 7, Request: controller.Request{Action: "connect", UserContext: true, Mode: controller.ModeRemoteApp}})
	if out.Err != nil || out.Binding == nil || out.Binding.Reference != ref || out.Sessions == nil || !out.Sessions.Fresh {
		t.Fatal(out)
	}
	if s.display.command == nil {
		t.Fatal("bound StartDisplay did not leave a running FreeRDP process")
	}
	if stop := s.execute(context.WithoutCancel(ctx), controller.Operation{Kind: controller.StopDisplay}); stop.Err != nil {
		t.Fatal(stop.Err)
	}
	if s.display.command != nil {
		t.Fatal("StopDisplay did not release the process")
	}
}

func TestStartDisplayUsesPreSpawnBaselineToIgnoreLingeringLogon(t *testing.T) {
	oldRef := sampleRef()
	newRef := controller.SessionRef{SessionID: oldRef.SessionID + 2, AuthenticationID: "00000000000003E8", BootIdentifier: oldRef.BootIdentifier}
	hostname := "rdp-test-client"
	old := bindingLogin(hostname, "S-1-5-21-1", "user", controller.WTSActive, oldRef)
	created := bindingLogin(hostname, "S-1-5-21-1", "user", controller.WTSActive, newRef)
	created.ConnectTime = old.ConnectTime + 100
	var inspects atomic.Int32
	agent := jsonAgent(t, func(req agentRequest) any {
		switch req.Action {
		case "inspect_sessions":
			if inspects.Add(1) == 1 {
				// Pre-spawn baseline: the old connection's login is still Active.
				return okReply(map[string]any{"snapshot": bindingSnapshot(1, true, old)})
			}
			// Post-spawn overlap: only the newly created login may bind.
			return okReply(map[string]any{"snapshot": bindingSnapshot(2, true, old, created)})
		case "session_status":
			if req.SessionRef == nil || *req.SessionRef != newRef {
				t.Errorf("worker was asked about old/unexpected ref %+v", req.SessionRef)
			}
			return workerStatusReply(newRef, created.SID, `MACHINE\user`, true)
		default:
			t.Errorf("unexpected action %q", req.Action)
			return map[string]any{"protocol": agentProtocol, "status": "error"}
		}
	})
	s, ctx, cancel := startDisplaySession(t, agent)
	defer cancel()
	out := s.execute(ctx, controller.Operation{Kind: controller.StartDisplay, ID: 9, Request: controller.Request{Action: "connect", UserContext: true, Mode: controller.ModeRemoteApp}})
	if out.Err != nil || out.Binding == nil || out.Binding.Reference != newRef {
		t.Fatalf("out=%+v", out)
	}
	if stop := s.execute(context.WithoutCancel(ctx), controller.Operation{Kind: controller.StopDisplay}); stop.Err != nil {
		t.Fatal(stop.Err)
	}
}

func TestStartDisplayCleansUpWhenBindingFails(t *testing.T) {
	var inspects atomic.Int32
	agent := jsonAgent(t, func(req agentRequest) any {
		if req.Action != "inspect_sessions" {
			t.Errorf("unexpected action %q", req.Action)
		}
		if inspects.Add(1) == 1 {
			return okReply(map[string]any{"snapshot": bindingSnapshot(1, true)})
		}
		return okReply(map[string]any{"snapshot": bindingSnapshot(2, true, bindingLogin("other-host", "S-1-5-21-1", "user", controller.WTSActive, sampleRef()))})
	})
	s, ctx, cancel := startDisplaySession(t, agent)
	defer cancel()
	ctx, cancelShort := context.WithTimeout(ctx, 400*time.Millisecond)
	defer cancelShort()
	out := s.execute(ctx, controller.Operation{Kind: controller.StartDisplay, ID: 8, Request: controller.Request{Action: "connect", UserContext: true, Mode: controller.ModeRemoteApp}})
	if out.Err == nil || !strings.Contains(out.Err.Error(), "not bound") {
		t.Fatal(out)
	}
	if s.display.command != nil {
		t.Fatal("failed binding left a FreeRDP process running")
	}
}
