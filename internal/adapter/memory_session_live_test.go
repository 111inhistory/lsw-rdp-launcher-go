package adapter

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/111inhistory/lsw-rdp-launcher-go/internal/controller"
)

// Explicit memory opt-in. No idle timers or allocation stress. This verifies
// the real Session combines its Windows and QEMU observations independently.
func TestLiveMemorySessionObservations(t *testing.T) {
	if os.Getenv("RDP_LIVE_MEMORY") != "1" {
		t.Skip("set RDP_LIVE_MEMORY=1 for live memory session")
	}
	endpoint := os.Getenv("RDP_LIVE_BACKGROUND")
	if endpoint == "" {
		t.Fatal("RDP_LIVE_BACKGROUND required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	before, err := liveAgent(endpoint).Call(ctx, controller.Request{Action: "status"})
	if err != nil || before.Memory == nil {
		t.Fatal(before, err)
	}
	if before.Memory.AvailableBytes*100 < before.Memory.TotalBytes*10 {
		t.Skip("observation test requires guest usage below90%; no pressure stress")
	}
	socket := os.Getenv("RDP_LIVE_LIBVIRT_SOCKET")
	if socket == "" {
		socket = "/run/libvirt/libvirt-sock"
	}
	session, err := Open(ctx, Config{LibvirtSocket: socket, LibvirtURI: "qemu:///system", Domain: "win11", AgentEndpoint: endpoint, AgentToken: liveAgent(endpoint).AuthToken, Policy: controller.Options{Memory: &controller.MemoryPolicy{}}})
	if err != nil {
		t.Fatal(err)
	}
	initial := session.memory.Execute(ctx, controller.Operation{Kind: controller.QueryMemory})
	if initial.Err != nil || initial.Memory == nil {
		session.Close()
		t.Fatal(initial)
	}
	stopped := make(chan error, 1)
	go func() { stopped <- session.Run() }()
	defer restoreLiveMemorySession(t, session, stopped, *initial.Memory)
	result := session.Controller.Do(ctx, controller.Request{Action: "status", Need: controller.BackendOnly})
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		s, err := session.Controller.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if s.Memory.LastError != nil {
			t.Fatal(s.Memory.LastError)
		}
		if s.Memory.DeviceKnown && s.Memory.GuestKnown && s.BackendReady && s.Memory.WindowPrepared && !s.Memory.Preparing {
			if s.VM != controller.VMRunning || s.Display || s.Memory.Growing {
				t.Fatal(s)
			}
			t.Logf("VM=%s requested=%d actual=%d Windows_total=%d Windows_available=%d", s.VM, s.Memory.Device.RequestedBytes, s.Memory.Device.ActualBytes, s.Memory.Guest.TotalBytes, s.Memory.Guest.AvailableBytes)
			return
		}
		select {
		case err := <-stopped:
			t.Fatal(err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
}
