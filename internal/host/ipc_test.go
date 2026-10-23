package host

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"reflect"
	"strings"
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
	ref := controller.SessionRef{SessionID: 7, AuthenticationID: "AUTH", BootIdentifier: "BOOT"}
	login := controller.Login{Reference: ref, SID: "S-1", User: "user", Domain: "DOMAIN", State: 0, ClientName: "rdp-launcher"}
	bindingSnapshot := controller.SessionSnapshot{Revision: 1, Fresh: true, Sessions: []controller.Login{login}, ObservedAt: time.Now()}
	c := controller.New(controller.Options{}, func(ctx context.Context, op controller.Operation) controller.Outcome {
		if op.Kind == controller.QueryVM {
			return controller.Outcome{VM: controller.VMRunning}
		}
		if op.Kind == controller.StartDisplay {
			return controller.Outcome{Binding: &login, Sessions: &bindingSnapshot}
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
	want := controller.Request{Action: "get_mapped_drives", UserContext: true}
	data, err = Call(ctx, socket, Request{Action: "request", Request: want})
	if err != nil {
		t.Fatal(err)
	}
	var got controller.Request
	if err = json.Unmarshal(data, &got); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatal(string(data), err)
	}
	if _, err = Call(ctx, socket, Request{Action: "unlock"}); err != nil {
		t.Fatal(err)
	}
	if _, err = Call(ctx, socket, Request{Action: "shutdown"}); err != nil {
		t.Fatal(err)
	}
}

func startIPC(t *testing.T, ctx context.Context, execute controller.Execute) string {
	t.Helper()
	c := controller.New(controller.Options{}, execute)
	done := make(chan struct{})
	go func() { defer close(done); _ = c.Run(ctx) }()
	t.Cleanup(func() { <-done })
	for {
		snapshot, err := c.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.VM == controller.VMRunning {
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
	t.Cleanup(func() { listener.Close() })
	go func() { _ = Serve(ctx, listener, c, func() {}) }()
	return socket
}

func TestIPCVersionMismatchRequiresPairedUpgrade(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	socket := startIPC(t, ctx, func(context.Context, controller.Operation) controller.Outcome {
		return controller.Outcome{VM: controller.VMRunning}
	})
	// An old client does not greet and must not be silently served.
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err = json.NewEncoder(conn).Encode(Request{Action: "status"}); err != nil {
		t.Fatal(err)
	}
	var response Response
	if err = json.NewDecoder(conn).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(response.Error, "upgrade") || !strings.Contains(response.Error, "together") {
		t.Fatal(response)
	}
	// A new client against an old service sees the old service's hello error
	// and reports the same paired-upgrade requirement.
	oldSocket := filepath.Join(t.TempDir(), "old.sock")
	listener, err := net.Listen("unix", oldSocket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var ignored Request
		_ = json.NewDecoder(conn).Decode(&ignored)
		_ = json.NewEncoder(conn).Encode(Response{Error: `unknown host action "hello"`})
	}()
	_, err = Call(ctx, oldSocket, Request{Action: "status"})
	if err == nil || !strings.Contains(err.Error(), "upgrade") || !strings.Contains(err.Error(), "hello") {
		t.Fatal(err)
	}
}

func TestIPCConfirmationUsesSameConnectionAndMatchingReply(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	socket := startIPC(t, ctx, func(ctx context.Context, op controller.Operation) controller.Outcome {
		switch op.Kind {
		case controller.QueryVM:
			return controller.Outcome{VM: controller.VMRunning}
		case controller.Dispatch:
			prompt := controller.SwitchPrompt{TargetMode: controller.ModeRemoteApp, Allowed: []controller.SwitchDecision{controller.SwitchKeep, controller.SwitchCancel}}
			decision, err := op.Request.Confirm(ctx, prompt)
			return controller.Outcome{Data: decision, Err: err}
		}
		return controller.Outcome{}
	})
	prompts := 0
	data, err := CallWithConfirm(ctx, socket, Request{Action: "request", Request: controller.Request{Action: "probe"}}, func(_ context.Context, prompt controller.SwitchPrompt) (controller.SwitchDecision, error) {
		prompts++
		if !prompt.Allows(controller.SwitchKeep) || prompt.Allows(controller.SwitchLogoff) {
			t.Fatal(prompt)
		}
		return controller.SwitchKeep, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `"keep"` || prompts != 1 {
		t.Fatal(string(data), prompts)
	}
	// A reply for a different task is rejected instead of being applied.
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	decoder, encoder := json.NewDecoder(conn), json.NewEncoder(conn)
	if err = encoder.Encode(Request{Version: ProtocolVersion, Action: "hello"}); err != nil {
		t.Fatal(err)
	}
	var hello Response
	if err = decoder.Decode(&hello); err != nil || hello.Protocol != ProtocolVersion {
		t.Fatal(hello, err)
	}
	if err = encoder.Encode(Request{Action: "request", Request: controller.Request{Action: "probe"}}); err != nil {
		t.Fatal(err)
	}
	var frame Response
	if err = decoder.Decode(&frame); err != nil || frame.Type != "confirm" || frame.ID == 0 {
		t.Fatal(frame, err)
	}
	if err = encoder.Encode(decision{Type: "decision", ID: frame.ID + 1, Decision: controller.SwitchKeep}); err != nil {
		t.Fatal(err)
	}
	var result Response
	if err = decoder.Decode(&result); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Error, "does not match") {
		t.Fatal(result)
	}
}

func TestIPCStatusAndLockRemainResponsiveWhileConfirmPending(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	socket := startIPC(t, ctx, func(ctx context.Context, op controller.Operation) controller.Outcome {
		switch op.Kind {
		case controller.QueryVM:
			return controller.Outcome{VM: controller.VMRunning}
		case controller.Dispatch:
			prompt := controller.SwitchPrompt{TargetMode: controller.ModeRemoteApp, Allowed: []controller.SwitchDecision{controller.SwitchKeep, controller.SwitchCancel}}
			decision, err := op.Request.Confirm(ctx, prompt)
			return controller.Outcome{Data: decision, Err: err}
		}
		return controller.Outcome{}
	})
	waiting := make(chan struct{})
	release := make(chan struct{})
	checked := make(chan error, 1)
	go func() {
		<-waiting
		if _, err := Call(ctx, socket, Request{Action: "status"}); err != nil {
			checked <- err
			return
		}
		if _, err := Call(ctx, socket, Request{Action: "lock"}); err != nil {
			checked <- err
			return
		}
		checked <- nil
		close(release)
	}()
	data, err := CallWithConfirm(ctx, socket, Request{Action: "request", Request: controller.Request{Action: "probe"}}, func(context.Context, controller.SwitchPrompt) (controller.SwitchDecision, error) {
		close(waiting)
		<-release
		return controller.SwitchKeep, nil
	})
	if err != nil || string(data) != `"keep"` {
		t.Fatal(string(data), err)
	}
	if err = <-checked; err != nil {
		t.Fatal(err)
	}
}

func TestIPCRejectsDisallowedConfirmationChoice(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	socket := startIPC(t, ctx, func(ctx context.Context, op controller.Operation) controller.Outcome {
		switch op.Kind {
		case controller.QueryVM:
			return controller.Outcome{VM: controller.VMRunning}
		case controller.Dispatch:
			prompt := controller.SwitchPrompt{TargetMode: controller.ModeRemoteApp, Allowed: []controller.SwitchDecision{controller.SwitchKeep, controller.SwitchCancel}}
			decision, err := op.Request.Confirm(ctx, prompt)
			return controller.Outcome{Data: decision, Err: err}
		}
		return controller.Outcome{}
	})
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	decoder, encoder := json.NewDecoder(conn), json.NewEncoder(conn)
	if err = encoder.Encode(Request{Version: ProtocolVersion, Action: "hello"}); err != nil {
		t.Fatal(err)
	}
	var hello Response
	if err = decoder.Decode(&hello); err != nil {
		t.Fatal(err)
	}
	if err = encoder.Encode(Request{Action: "request", Request: controller.Request{Action: "probe"}}); err != nil {
		t.Fatal(err)
	}
	var frame Response
	if err = decoder.Decode(&frame); err != nil || frame.Type != "confirm" {
		t.Fatal(frame, err)
	}
	// The client cannot widen the controller's allowed actions.
	if err = encoder.Encode(decision{Type: "decision", ID: frame.ID, Decision: controller.SwitchLogoff}); err != nil {
		t.Fatal(err)
	}
	var result Response
	if err = decoder.Decode(&result); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Error, "not allowed") {
		t.Fatal(result)
	}
}

func TestIPCConnectHonorsDeadlineDuringStalledHandshake(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "stalled.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	release := make(chan struct{})
	defer close(release)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		<-release // Accept, read nothing, never send a greeting.
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err = Call(ctx, socket, Request{Action: "status"}); err == nil {
		t.Fatal("stalled handshake unexpectedly succeeded")
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("handshake ignored the caller deadline after %v", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline error, got %v", err)
	}
}

func TestResultResponseKeepsPartialDataAndBusinessError(t *testing.T) {
	response := resultResponse(map[string]any{"results": []string{"first"}, "completed": 1}, errors.New("batch stopped"))
	if !strings.Contains(response.Error, "batch stopped") {
		t.Fatal(response)
	}
	if !strings.Contains(string(response.Data), `"first"`) || !strings.Contains(string(response.Data), `"completed":1`) {
		t.Fatal(response)
	}
}

func TestIPCResultCarriesPartialBatchDataWithBusinessError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ref := controller.SessionRef{SessionID: 4, AuthenticationID: "AUTH", BootIdentifier: "BOOT"}
	login := controller.Login{Reference: ref, SID: "S-1", User: "user", Domain: "DOMAIN", State: 0, ClientName: "rdp-launcher"}
	snapshot := controller.SessionSnapshot{Revision: 1, Fresh: true, Sessions: []controller.Login{login}, ObservedAt: time.Now()}
	socket := startIPC(t, ctx, func(ctx context.Context, op controller.Operation) controller.Outcome {
		switch op.Kind {
		case controller.QueryVM:
			return controller.Outcome{VM: controller.VMRunning}
		case controller.StartDisplay:
			return controller.Outcome{Binding: &login, Sessions: &snapshot}
		case controller.Dispatch:
			return controller.Outcome{Data: map[string]any{"results": []any{"first"}, "completed": 1}, Err: errors.New("batch stopped after 1 of 2 commands")}
		}
		return controller.Outcome{}
	})
	batch := controller.Request{Action: "batch", UserContext: true, Mode: controller.ModeRemoteApp, Batch: []controller.Request{{Action: "open", File: `J:\a.txt`}, {Action: "open", File: `J:\b.txt`}}}
	data, err := CallWithConfirm(ctx, socket, Request{Action: "request", Request: batch}, nil)
	if err == nil || !strings.Contains(err.Error(), "batch stopped") {
		t.Fatalf("business error lost: data=%s err=%v", data, err)
	}
	if len(data) == 0 || !strings.Contains(string(data), `"first"`) || !strings.Contains(string(data), `"completed":1`) {
		t.Fatalf("partial result lost: %s", data)
	}
}
