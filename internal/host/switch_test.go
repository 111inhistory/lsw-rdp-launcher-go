package host

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/111inhistory/lsw-rdp-launcher-go/internal/controller"
)

func TestIPCPassesRequestsAndRejectsRemovedSwitchAction(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	clientCtx, stopClient := context.WithCancel(ctx)
	defer stopClient()
	ref := controller.SessionRef{SessionID: 5, AuthenticationID: "AUTH", BootIdentifier: "BOOT"}
	login := controller.Login{Reference: ref, SID: "S-1", User: "user", Domain: "DOMAIN", State: 0, ClientName: "rdp-launcher"}
	bindingSnapshot := controller.SessionSnapshot{Revision: 1, Fresh: true, Sessions: []controller.Login{login}, ObservedAt: time.Now()}
	c := controller.New(controller.Options{}, func(ctx context.Context, op controller.Operation) controller.Outcome {
		switch op.Kind {
		case controller.QueryVM:
			return controller.Outcome{VM: controller.VMRunning}
		case controller.StartDisplay:
			return controller.Outcome{Binding: &login, Sessions: &bindingSnapshot}
		case controller.Dispatch:
			return controller.Outcome{Data: op.Request}
		}
		return controller.Outcome{}
	})
	done := make(chan struct{})
	go func() { defer close(done); _ = c.Run(clientCtx) }()
	defer func() { stopClient(); <-done }()
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
	go func() { _ = Serve(ctx, listener, c, cancel) }()
	defer listener.Close()

	// The old privileged switch invitation and rewritten action are gone. A
	// desktop/remoteapp command is an ordinary connect request instead.
	_, err = Call(ctx, socket, Request{Action: "switch-display", Request: controller.Request{Action: "connect", Mode: controller.ModeDesktop}})
	if err == nil || !strings.Contains(err.Error(), "unknown host action") {
		t.Fatal("removed switch action was accepted", err)
	}
	want := controller.Request{Action: "run", UserContext: true, Mode: controller.ModeRemoteApp, Target: `C:\Windows\notepad.exe`, Params: "/x", WorkingDir: `C:\Windows`, File: `J:\notes.txt`}
	data, err := Call(ctx, socket, Request{Action: "request", Request: want})
	if err != nil {
		t.Fatal(err)
	}
	var got controller.Request
	if err = json.Unmarshal(data, &got); err != nil || got.Action != want.Action || got.UserContext != want.UserContext || got.Mode != want.Mode || got.Target != want.Target || got.Params != want.Params || got.WorkingDir != want.WorkingDir || got.File != want.File {
		t.Fatal(string(data), err)
	}
}

func TestRequestWireFieldsHaveBusinessTagsOnly(t *testing.T) {
	raw, err := json.Marshal(controller.Request{Action: "open", UserContext: true, Mode: controller.ModeDesktop, Choice: controller.SwitchKeep, File: `J:\a.txt`, Batch: []controller.Request{{Action: "open", File: `J:\a.txt`}}})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"action", "user_context", "mode", "choice", "file", "batch"} {
		if _, ok := fields[name]; !ok {
			t.Fatalf("missing %q in %s", name, raw)
		}
	}
	for _, removed := range []string{"Need", "SwitchDisplay", "SwitchPrompt", "Confirm"} {
		if _, ok := fields[removed]; ok {
			t.Fatalf("legacy/private field %q leaked onto the wire: %s", removed, raw)
		}
	}
}
