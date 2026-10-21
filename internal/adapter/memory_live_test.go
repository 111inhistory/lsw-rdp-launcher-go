package adapter

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/111inhistory/lsw-rdp-launcher-go/internal/controller"
)

// Explicit opt-in only: changes the LIVE target by one 128MiB quantum, observes
// QMP and Windows, then restores our original requested target. No pressure
// allocation, cold boot, suspend, inactive XML write, or driver replacement.
func TestLiveMemorySmallResize(t *testing.T) {
	if os.Getenv("RDP_LIVE_MEMORY") != "1" {
		t.Skip("set RDP_LIVE_MEMORY=1 for reversible live memory resize")
	}
	endpoint := os.Getenv("RDP_LIVE_BACKGROUND")
	if endpoint == "" {
		t.Fatal("RDP_LIVE_BACKGROUND required")
	}
	socket := os.Getenv("RDP_LIVE_LIBVIRT_SOCKET")
	if socket == "" {
		socket = "/run/libvirt/libvirt-sock"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	memory, err := OpenMemoryLibvirt(ctx, socket, "qemu:///system", "win11", "")
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan controller.MemoryDeviceObserved, 32)
	stopped := make(chan error, 1)
	go func() {
		stopped <- memory.Watch(ctx, func(ctx context.Context, event controller.Event) error {
			select {
			case events <- event.(controller.MemoryDeviceObserved):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	defer func() {
		cancel()
		memory.Close()
		select {
		case <-stopped:
		case <-time.After(time.Second):
			t.Error("memory watch did not stop")
		}
	}()
	baseline := memory.Execute(ctx, controller.Operation{Kind: controller.QueryMemory})
	if baseline.Err != nil || baseline.Memory == nil {
		t.Fatal(baseline)
	}
	old := *baseline.Memory
	if old.RequestedBytes%controller.MemoryQuantum != 0 {
		t.Fatal("baseline requested must be restorable on the128MiB grid", old)
	}
	target := (old.ActualBytes/controller.MemoryQuantum + 1) * controller.MemoryQuantum
	if target > old.CapacityBytes {
		t.Skip("no headroom for one128MiB test quantum")
	}
	agent := Agent{Endpoint: endpoint}
	before, err := agent.Call(ctx, controller.Request{Action: "status"})
	if err != nil || before.Memory == nil {
		t.Fatal(before, err)
	}
	// Register cleanup before writing: even a failed RPC can have reached libvirt.
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 12*time.Second)
		defer stop()
		current := memory.Execute(cleanup, controller.Operation{Kind: controller.QueryMemory})
		if current.Err != nil || current.Memory == nil {
			t.Error("cannot inspect cleanup target", current)
			return
		}
		if current.Memory.RequestedBytes != target {
			t.Log("target no longer ours; not overwriting", current.Memory)
			return
		}
		out := memory.Execute(cleanup, controller.Operation{Kind: controller.SetMemory, MemoryTarget: old.RequestedBytes})
		if out.Err != nil {
			t.Error("restore requested target", out.Err)
			return
		}
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		for current.Memory.ActualBytes > old.ActualBytes {
			select {
			case e := <-events:
				current.Memory.ActualBytes = e.ActualBytes
			case <-timer.C:
				final := memory.Execute(cleanup, controller.Operation{Kind: controller.QueryMemory})
				t.Logf("restore accepted; reclaim may be partial: %+v error=%v", final.Memory, final.Err)
				return
			case <-cleanup.Done():
				t.Error(cleanup.Err())
				return
			}
		}
		final := memory.Execute(cleanup, controller.Operation{Kind: controller.QueryMemory})
		if final.Err != nil || final.Memory == nil || final.Memory.RequestedBytes != old.RequestedBytes {
			t.Error("restore verification", final)
			return
		}
		t.Logf("restored requested=%d actual=%d", final.Memory.RequestedBytes, final.Memory.ActualBytes)
	}()
	out := memory.Execute(ctx, controller.Operation{Kind: controller.SetMemory, MemoryTarget: target})
	if out.Err != nil {
		t.Fatal(out.Err)
	}
	if out.Memory != nil {
		t.Fatal("mutation synthesized a memory snapshot")
	}
	for {
		select {
		case e := <-events:
			if e.Lost {
				t.Fatal("lost QMP stream")
			}
			if e.ActualBytes == target {
				goto visible
			}
		case err := <-stopped:
			t.Fatal(err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
visible:
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		response, err := agent.Call(ctx, controller.Request{Action: "status"})
		if err != nil {
			t.Fatal(err)
		}
		if response.Memory != nil && response.Memory.TotalBytes > before.Memory.TotalBytes {
			final := memory.Execute(ctx, controller.Operation{Kind: controller.QueryMemory})
			if final.Err != nil || final.Memory == nil || final.Memory.RequestedBytes != target || final.Memory.ActualBytes != target {
				t.Fatal(final)
			}
			t.Logf("QMP bytes=%d Windows physical before=%d after=%d", target, before.Memory.TotalBytes, response.Memory.TotalBytes)
			return
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}
