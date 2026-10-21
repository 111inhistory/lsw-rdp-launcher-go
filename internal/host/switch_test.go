package host

import (
	"context"
	"encoding/json"
	"github.com/111inhistory/lsw-rdp-launcher-go/internal/controller"
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestCheckedSwitchIPCAndNoLegacyWireBypass(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	zero := uint32(0)
	c := controller.New(controller.Options{}, func(ctx context.Context, op controller.Operation) controller.Outcome {
		switch op.Kind {
		case controller.QueryVM:
			return controller.Outcome{VM: controller.VMRunning}
		case controller.InspectSession:
			return controller.Outcome{Activity: &controller.SessionActivity{SessionID: 2, User: "user", Fingerprint: "logon", Windows: &zero, TrayIcons: &zero}}
		case controller.ConfirmSwitch:
			t.Error("empty session unexpectedly asked for confirmation")
		case controller.LogoffSession:
			if !op.RequireEmpty || op.Activity.SessionID != 2 {
				t.Error(op)
			}
		case controller.Dispatch:
			return controller.Outcome{Data: op.Request.Action}
		}
		return controller.Outcome{}
	})
	done := make(chan struct{})
	go func() { defer close(done); _ = c.Run(ctx) }()
	defer func() { cancel(); <-done }()
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
	data, err := Call(ctx, socket, Request{Action: "switch-display", Request: controller.Request{Action: "run", Need: controller.Desktop}})
	if err != nil || string(data) != `"connect"` {
		t.Fatal(string(data), err)
	}
	// An old-style request must not silently bypass the checked endpoint. Its
	// in-process permission is deliberately not part of the JSON wire format.
	raw, _ := json.Marshal(controller.Request{SwitchDisplay: true})
	var fields map[string]any
	_ = json.Unmarshal(raw, &fields)
	if _, exists := fields["SwitchDisplay"]; exists {
		t.Fatal(string(raw))
	}
	_, err = Call(ctx, socket, Request{Action: "request", Request: controller.Request{Action: "run", Need: controller.Interactive, SwitchDisplay: true}})
	if err == nil {
		t.Fatal("ordinary request replaced the display")
	}
	data, err = Call(ctx, socket, Request{Action: "switch-display", Request: controller.Request{Need: controller.Interactive}})
	if err != nil || string(data) != `"connect"` {
		t.Fatal(string(data), err)
	}
	snapshot, err := c.Snapshot(ctx)
	if err != nil || snapshot.DisplayMode != "remoteapp" {
		t.Fatal(snapshot, err)
	}
}
