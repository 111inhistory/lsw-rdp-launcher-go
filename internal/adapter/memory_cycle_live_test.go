package adapter

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/111inhistory/lsw-rdp-launcher-go/internal/controller"
)

// Explicit opt-in: real pre-reclaim, pause and resume. No allocation stress or
// FreeRDP. A35s idle deadline proves reclaim starts30s BEFORE it, not after it.
func TestLiveMemoryReclaimSuspendResume(t *testing.T) {
	if os.Getenv("RDP_LIVE_MEMORY") != "1" {
		t.Skip("set RDP_LIVE_MEMORY=1 for real memory/lifecycle cycle")
	}
	endpoint := os.Getenv("RDP_LIVE_BACKGROUND")
	if endpoint == "" {
		t.Fatal("RDP_LIVE_BACKGROUND required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 65*time.Second)
	defer cancel()
	probe, err := (Agent{Endpoint: endpoint}).Call(ctx, controller.Request{Action: "status"})
	if err != nil || probe.Memory == nil {
		t.Fatal(probe, err)
	}
	if probe.Memory.AvailableBytes*100 < probe.Memory.TotalBytes*10 {
		t.Skip("test requires below90% physical usage; no pressure stress")
	}
	socket := os.Getenv("RDP_LIVE_LIBVIRT_SOCKET")
	if socket == "" {
		socket = "/run/libvirt/libvirt-sock"
	}
	session, err := Open(ctx, Config{LibvirtSocket: socket, LibvirtURI: "qemu:///system", Domain: "win11", AgentEndpoint: endpoint, Policy: controller.Options{VMSuspend: 35 * time.Second, Memory: &controller.MemoryPolicy{}}})
	if err != nil {
		t.Fatal(err)
	}
	initial := session.memory.Execute(ctx, controller.Operation{Kind: controller.QueryMemory})
	if initial.Err != nil || initial.Memory == nil {
		session.Close()
		t.Fatal(initial)
	}
	old := *initial.Memory
	stopped := make(chan error, 1)
	go func() { stopped <- session.Run() }()
	defer restoreLiveMemorySession(t, session, stopped, old)
	if err := session.Controller.SetLocked(ctx, true); err != nil {
		t.Fatal(err)
	}
	result := session.Controller.Do(ctx, controller.Request{Action: "status"})
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	if err := session.Controller.SetLocked(ctx, false); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	var reclaimed time.Time
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		s, err := session.Controller.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if s.Memory.LastError != nil {
			t.Fatal(s.Memory.LastError)
		}
		if reclaimed.IsZero() && s.Memory.Reclaiming && s.Memory.Device.RequestedBytes == 0 {
			reclaimed = time.Now()
			t.Logf("reclaim accepted at %s", reclaimed.Sub(started))
		}
		if s.VM == controller.VMPaused {
			if reclaimed.IsZero() || time.Since(reclaimed) < 25*time.Second {
				t.Fatal("pause did not preserve pre-reclaim window", time.Since(reclaimed), s)
			}
			t.Logf("Paused at %s; remaining device actual=%d", time.Since(started), s.Memory.Device.ActualBytes)
			break
		}
		select {
		case <-ticker.C:
		case err := <-stopped:
			t.Fatal(err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if err := session.Controller.SetLocked(ctx, true); err != nil {
		t.Fatal(err)
	}
	result = session.Controller.Do(ctx, controller.Request{Action: "status", Need: controller.BackendOnly})
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	s, err := session.Controller.Snapshot(ctx)
	if err != nil || s.VM != controller.VMRunning || !s.BackendReady || !s.Memory.GuestKnown || !s.Memory.DeviceKnown || !s.Memory.WindowPrepared || s.Display || s.Memory.Device.RequestedBytes%controller.MemoryQuantum != 0 {
		t.Fatal(s, err)
	}
	t.Logf("resumed with fresh subscription, requested=%d actual=%d Windows_total=%d", s.Memory.Device.RequestedBytes, s.Memory.Device.ActualBytes, s.Memory.Guest.TotalBytes)
}
