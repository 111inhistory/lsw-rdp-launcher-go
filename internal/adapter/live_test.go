package adapter

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/111inhistory/lsw-rdp-launcher-go/internal/controller"
)

// Opt-in: SSH stays unchanged for 32 seconds, then changes on the same
// subscription. Memory changes may still arrive; they are not heartbeats.
func TestLiveBackgroundWatchQuiet(t *testing.T) {
	endpoint := os.Getenv("RDP_LIVE_BACKGROUND")
	if endpoint == "" {
		t.Skip("set RDP_LIVE_BACKGROUND for the Windows long-connection test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	events := make(chan controller.BackendObserved, 8)
	stopped := make(chan error, 1)
	go func() {
		var previous controller.BackendObserved
		stopped <- (Agent{Endpoint: endpoint}).Watch(ctx, func(_ context.Context, e controller.Event) error {
			if state, ok := e.(controller.BackendObserved); ok && state != previous {
				previous = state
				events <- state
			}
			return nil
		})
	}()
	defer func() {
		cancel()
		select {
		case <-stopped:
		case <-time.After(time.Second):
			t.Error("watch did not stop")
		}
	}()
	for {
		select {
		case e := <-events:
			if e.Ready && e.SSH == controller.SSHZero {
				goto quiet
			}
		case err := <-stopped:
			t.Fatal(err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
quiet:
	select {
	case e := <-events:
		t.Fatalf("unexpected change during quiet test: %+v", e)
	case err := <-stopped:
		t.Fatal(err)
	case <-time.After(32 * time.Second):
	}
	command := exec.CommandContext(ctx, "ssh", "win11", "Start-Sleep -Seconds 2")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer command.Wait()
	select {
	case e := <-events:
		if e.SSH != controller.SSHActive {
			t.Fatal(e)
		}
	case err := <-stopped:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	t.Log("SSH unchanged for 32 seconds, then received SSH-active on the same connection; memory changes permitted")
}

// Opt-in integration test. Both the VM and new background endpoint must already
// be running. Idle policies stay disabled; this test never launches FreeRDP.
func TestLiveBackgroundOnlySession(t *testing.T) {
	endpoint := os.Getenv("RDP_LIVE_BACKGROUND")
	if endpoint == "" {
		t.Skip("set RDP_LIVE_BACKGROUND to an already running test backend")
	}
	socket := os.Getenv("RDP_LIVE_LIBVIRT_SOCKET")
	if socket == "" {
		socket = "/run/libvirt/libvirt-sock"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	session, err := Open(ctx, Config{LibvirtSocket: socket, LibvirtURI: "qemu:///system", Domain: "win11", AgentEndpoint: endpoint})
	if err != nil {
		t.Fatal(err)
	}
	stopped := make(chan error, 1)
	go func() { stopped <- session.Run() }()
	defer func() {
		session.Close()
		select {
		case <-stopped:
		case <-time.After(time.Second):
			t.Error("session did not stop")
		}
	}()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		state, err := session.Controller.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if state.VM == controller.VMOff {
			t.Fatal("live test requires an already running VM")
		}
		if state.VM == controller.VMRunning && state.BackendReady {
			break
		}
		select {
		case err := <-stopped:
			t.Fatalf("session ended: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
	result := session.Controller.Do(ctx, controller.Request{Action: "status", Need: controller.BackendOnly})
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	response, ok := result.Data.(AgentResponse)
	if !ok || response.Role != "background" {
		t.Fatalf("unexpected response: %+v", result.Data)
	}
	state, err := session.Controller.Snapshot(ctx)
	if err != nil || state.Display {
		t.Fatal(state, err)
	}
	t.Logf("VM=%s backend=%s SSH=%s FreeRDP=%v", state.VM, response.Role, state.SSH, state.Display)
}
