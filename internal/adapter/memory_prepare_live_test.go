package adapter

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/111inhistory/lsw-rdp-launcher-go/internal/controller"
)

// Explicit opt-in: a brief reversible VM pause. The capacity request is never
// exposed to running vCPUs; verify plugged bytes do not increase, restore the
// original requested target, then resume. Does not touch inactive XML or driver.
func TestLiveMemoryPausedWindowPreparation(t *testing.T) {
	if os.Getenv("RDP_LIVE_MEMORY") != "1" {
		t.Skip("set RDP_LIVE_MEMORY=1 for paused window preparation")
	}
	socket := os.Getenv("RDP_LIVE_LIBVIRT_SOCKET")
	if socket == "" {
		socket = "/run/libvirt/libvirt-sock"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	vm, err := OpenLibvirt(ctx, socket, "qemu:///system", "win11")
	if err != nil {
		t.Fatal(err)
	}
	states := make(chan controller.VMState, 16)
	stopped := make(chan error, 1)
	go func() {
		stopped <- vm.Watch(ctx, func(ctx context.Context, e controller.Event) error {
			select {
			case states <- e.(controller.VMObserved).State:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	defer func() {
		cancel()
		vm.Close()
		select {
		case <-stopped:
		case <-time.After(time.Second):
			t.Error("lifecycle source did not stop")
		}
	}()
	memory, err := OpenMemoryLibvirt(ctx, socket, "qemu:///system", "win11", "")
	if err != nil {
		t.Fatal(err)
	}
	defer memory.Close()
	initial := vm.Execute(ctx, controller.Operation{Kind: controller.QueryVM})
	if initial.Err != nil || initial.VM != controller.VMRunning {
		t.Fatal("requires already running VM", initial)
	}
	before := memory.Execute(ctx, controller.Operation{Kind: controller.QueryMemory})
	if before.Err != nil || before.Memory == nil {
		t.Fatal(before)
	}
	old := *before.Memory
	// Installed before the first mutation. Cleanup resumes only the VM we paused.
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		if err := memory.restoreMemoryTarget(cleanup, old.RequestedBytes); err != nil {
			t.Error("restore requested target before resume", err)
			return
		}
		state := vm.Execute(cleanup, controller.Operation{Kind: controller.QueryVM})
		if state.Err != nil {
			t.Error(state.Err)
			return
		}
		if state.VM == controller.VMPaused {
			out := vm.Execute(cleanup, controller.Operation{Kind: controller.WakeVM, VM: controller.VMPaused})
			if out.Err != nil {
				t.Error("resume cleanup", out.Err)
				return
			}
			select {
			case state := <-states:
				if state != controller.VMRunning {
					t.Error("resume confirmation", state)
				}
			case <-cleanup.Done():
				t.Error(cleanup.Err())
			}
		}
		final := vm.Execute(cleanup, controller.Operation{Kind: controller.QueryVM})
		if final.Err != nil || final.VM != controller.VMRunning {
			t.Error("VM not restored Running", final)
		}
	}()
	out := vm.Execute(ctx, controller.Operation{Kind: controller.SuspendVM})
	if out.Err != nil {
		t.Fatal(out.Err)
	}
	select {
	case state := <-states:
		if state != controller.VMPaused {
			t.Fatal(state)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	started := time.Now()
	out = memory.Execute(ctx, controller.Operation{Kind: controller.PrepareMemory})
	if out.Err != nil || !out.ResumeSafe || !out.MemoryPrepared {
		t.Fatal(out)
	}
	current := memory.Execute(ctx, controller.Operation{Kind: controller.QueryMemory})
	if current.Err != nil || current.Memory == nil {
		t.Fatal(current)
	}
	if current.Memory.ActualBytes != old.ActualBytes || current.Memory.RequestedBytes%controller.MemoryQuantum != 0 || current.Memory.RequestedBytes < old.ActualBytes || current.Memory.RequestedBytes-old.ActualBytes >= controller.MemoryQuantum {
		t.Fatal("window preparation changed actual or left unsafe target", old, current.Memory)
	}
	t.Logf("window prepared in %s: capacity=%d actual remained=%d safe requested=%d", time.Since(started), old.CapacityBytes, current.Memory.ActualBytes, current.Memory.RequestedBytes)
}
