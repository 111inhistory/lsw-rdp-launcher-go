package adapter

import (
	"context"
	"testing"
	"time"

	"github.com/111inhistory/lsw-rdp-launcher-go/internal/controller"
)

// Stop the sole writer and wait for bounded operation cleanup BEFORE restoring
// its original live target. A paused VM is prepared safely before resuming.
func restoreLiveMemorySession(t *testing.T, s *Session, stopped <-chan error, old controller.MemoryDevice) {
	t.Helper()
	s.Close()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Error("session workers did not stop; not racing cleanup")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	state := s.vm.Execute(ctx, controller.Operation{Kind: controller.QueryVM})
	if state.Err != nil {
		t.Error(state.Err)
		return
	}
	if state.VM == controller.VMPaused {
		out := s.memory.Execute(ctx, controller.Operation{Kind: controller.PrepareMemory})
		if out.Err != nil || !out.ResumeSafe {
			t.Error("safe resume preparation", out)
			return
		}
	}
	if err := s.memory.restoreMemoryTarget(ctx, old.RequestedBytes); err != nil {
		t.Error("restore old requested", err)
		return
	}
	if state.VM == controller.VMPaused {
		out := s.vm.Execute(ctx, controller.Operation{Kind: controller.WakeVM, VM: controller.VMPaused})
		if out.Err != nil {
			t.Error("resume cleanup", out.Err)
			return
		}
	}
	final := s.vm.Execute(ctx, controller.Operation{Kind: controller.QueryVM})
	device := s.memory.Execute(ctx, controller.Operation{Kind: controller.QueryMemory})
	if final.Err != nil || final.VM != controller.VMRunning || device.Err != nil || device.Memory == nil || device.Memory.RequestedBytes != old.RequestedBytes {
		t.Error("cleanup state", final, device)
		return
	}
	t.Logf("cleanup Running; restored requested=%d actual=%d", device.Memory.RequestedBytes, device.Memory.ActualBytes)
}
