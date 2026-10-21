package controller

import (
	"context"
	"errors"
	"testing"
	"time"
)

const MiB = uint64(1024 * 1024)
const GiB = 1024 * MiB

func memoryMachine(t *testing.T) *machine {
	t.Helper()
	m := newMachine(Options{Memory: &MemoryPolicy{}, VMSuspend: 300 * time.Second})
	m.vm(VMRunning)
	m.needQuery = false
	m.backend(true, SSHZero)
	m.memoryGuest(&GuestMemory{TotalBytes: 4 * GiB, AvailableBytes: 2 * GiB})
	op := m.next(epoch)
	if op == nil || op.Kind != QueryMemory {
		t.Fatalf("discovery=%+v", op)
	}
	m.done(op.ID, Outcome{Memory: &MemoryDevice{Alias: "ua-virtiomem0", CapacityBytes: 13 * GiB, BlockBytes: 2 * MiB, RequestedBytes: GiB, ActualBytes: GiB}}, epoch)
	m.memory.WindowPrepared = true // Tests below isolate resizing from first-use preparation.
	return m
}
func TestMemoryPolicyDefaultsAndValidation(t *testing.T) {
	p := (MemoryPolicy{}).Defaults()
	if p.StartPercent != 95 || p.StopPercent != 80 {
		t.Fatal(p)
	}
	for _, p := range []MemoryPolicy{{95, 95}, {80, 95}, {101, 80}, {95, -1}, {-1, 80}} {
		if p.Validate() == nil {
			t.Fatal(p)
		}
	}
}
func TestMemoryHysteresis(t *testing.T) {
	m := memoryMachine(t)
	for _, tc := range []struct {
		avail uint64
		want  bool
	}{{51, false}, {50, true}, {100, true}, {199, true}, {200, false}, {100, false}, {50, true}} {
		m.memoryGuest(&GuestMemory{TotalBytes: 1000, AvailableBytes: tc.avail})
		if m.memory.Growing != tc.want {
			t.Fatalf("available=%d growing=%v", tc.avail, m.memory.Growing)
		}
	}
}
func TestMemoryGrowthRoundsFinalTargetNotIncrement(t *testing.T) {
	target, err := GrowthTarget(GuestMemory{TotalBytes: 4 * GiB, AvailableBytes: GiB / 4}, MemoryDevice{ActualBytes: 386 * MiB, BlockBytes: 2 * MiB, CapacityBytes: 13 * GiB}, 80)
	if err != nil || target != 1152*MiB || target%MemoryQuantum != 0 {
		t.Fatal(target, err)
	}
}
func TestMemoryGrowthCapacityAndInvalidData(t *testing.T) {
	d := MemoryDevice{ActualBytes: GiB, BlockBytes: 2 * MiB, CapacityBytes: GiB + 80*MiB}
	target, err := GrowthTarget(GuestMemory{TotalBytes: 4 * GiB, AvailableBytes: 1}, d, 80)
	if err != nil || target != GiB {
		t.Fatal(target, err)
	}
	for _, g := range []GuestMemory{{}, {TotalBytes: 100, AvailableBytes: 101}} {
		if _, err := GrowthTarget(g, d, 80); err == nil {
			t.Fatal(g)
		}
	}
	d.BlockBytes = 3 * MiB
	if _, err := GrowthTarget(GuestMemory{TotalBytes: 100, AvailableBytes: 1}, d, 80); err == nil {
		t.Fatal("bad block accepted")
	}
}
func TestMemoryReclaimThirtySecondsBeforeOriginalSuspend(t *testing.T) {
	m := memoryMachine(t)
	if op := m.next(epoch); op != nil {
		t.Fatal(op)
	}
	suspend := epoch.Add(300 * time.Second)
	if m.deadline() != epoch.Add(270*time.Second) {
		t.Fatal(m.deadline())
	}
	if op := m.next(epoch.Add(269 * time.Second)); op != nil {
		t.Fatal(op)
	}
	op := m.next(epoch.Add(270 * time.Second))
	if op == nil || op.Kind != SetMemory || op.MemoryTarget != 0 {
		t.Fatal(op)
	}
	if m.state.SuspendDeadline != suspend {
		t.Fatal("reclaim moved suspend deadline")
	}
	m.done(op.ID, Outcome{}, epoch.Add(270*time.Second))
	if !m.memory.Awaiting || m.memory.Device.ActualBytes != GiB {
		t.Fatal(m.memory)
	}
	if op := m.next(epoch.Add(299 * time.Second)); op != nil {
		t.Fatal(op)
	}
	op = m.next(suspend)
	if op == nil || op.Kind != SuspendVM {
		t.Fatal(op)
	}
	if m.memory.Device.ActualBytes != GiB || m.memory.LastError != nil {
		t.Fatal("partial reclaim misreported", m.memory)
	}
}
func TestMemoryShortSuspendTimerReclaimsImmediatelyWithoutSpinning(t *testing.T) {
	m := memoryMachine(t)
	m.opts.VMSuspend = 10 * time.Second
	op := m.next(epoch)
	if op == nil || op.Kind != SetMemory {
		t.Fatal(op)
	}
	m.done(op.ID, Outcome{}, epoch)
	if op := m.next(epoch); op != nil {
		t.Fatal(op)
	}
	if m.deadline() != epoch.Add(10*time.Second) {
		t.Fatal(m.deadline())
	}
}
func TestMemoryNewRequestCancelsSuspendNotPreviouslyRemovedPages(t *testing.T) {
	m := memoryMachine(t)
	m.next(epoch)
	op := m.next(epoch.Add(270 * time.Second))
	m.done(op.ID, Outcome{}, epoch.Add(270*time.Second))
	m.enqueue(&job{ctx: context.Background(), request: Request{Action: "status"}})
	if m.memory.Reclaiming || m.memory.Awaiting || !m.state.SuspendDeadline.IsZero() {
		t.Fatal(m.snapshot())
	}
	op = m.next(epoch.Add(275 * time.Second))
	if op == nil || op.Kind != Dispatch {
		t.Fatal(op)
	}
	if m.memory.Device.RequestedBytes != 0 {
		t.Fatal("invented a fixed working target")
	}
}
func TestMemoryPressureSupersedesPendingReclaim(t *testing.T) {
	m := memoryMachine(t)
	m.next(epoch)
	op := m.next(epoch.Add(270 * time.Second))
	m.done(op.ID, Outcome{}, epoch.Add(270*time.Second))
	m.memoryGuest(&GuestMemory{TotalBytes: 4 * GiB, AvailableBytes: GiB / 32})
	op = m.next(epoch.Add(271 * time.Second))
	if op == nil || op.Kind != SetMemory || op.MemoryTarget <= GiB || op.MemoryTarget%MemoryQuantum != 0 {
		t.Fatal(op)
	}
	if m.memory.Reclaiming || !m.state.SuspendDeadline.IsZero() {
		t.Fatal(m.snapshot())
	}
	m.memoryDevice(MemoryDeviceObserved{Alias: "ua-virtiomem0", ActualBytes: 0})
	if !m.memory.Awaiting {
		t.Fatal("late shrink event completed growth")
	}
}
func startMemoryGrowth(t *testing.T, m *machine) *Operation {
	t.Helper()
	m.memoryGuest(&GuestMemory{TotalBytes: 4 * GiB, AvailableBytes: GiB / 32})
	op := m.next(epoch)
	if op == nil || op.Kind != SetMemory || op.MemoryTarget <= GiB {
		t.Fatal(op)
	}
	return op
}
func TestMemoryAckIsNotConvergenceAndDeviceIsNotGuestVisibility(t *testing.T) {
	m := memoryMachine(t)
	op := startMemoryGrowth(t, m)
	m.done(op.ID, Outcome{}, epoch)
	if !m.memory.Awaiting || m.memory.Device.ActualBytes != GiB {
		t.Fatal(m.memory)
	}
	m.memoryDevice(MemoryDeviceObserved{Alias: "ua-virtiomem0", ActualBytes: op.MemoryTarget})
	if !m.memory.Awaiting {
		t.Fatal("QEMU actual treated as Windows visibility")
	}
	if next := m.next(epoch.Add(time.Second)); next != nil {
		t.Fatal("grew again from old guest snapshot", next)
	}
	m.memoryGuest(&GuestMemory{TotalBytes: 6 * GiB, AvailableBytes: 2 * GiB})
	if m.memory.Awaiting || m.memory.Growing {
		t.Fatal(m.memory)
	}
}
func TestMemoryEventsBeforeAckRemainEffective(t *testing.T) {
	m := memoryMachine(t)
	op := startMemoryGrowth(t, m)
	m.memoryDevice(MemoryDeviceObserved{Alias: "ua-virtiomem0", ActualBytes: op.MemoryTarget})
	m.memoryGuest(&GuestMemory{TotalBytes: 6 * GiB, AvailableBytes: 2 * GiB})
	m.done(op.ID, Outcome{}, epoch)
	if m.memory.Awaiting || m.memory.Device.RequestedBytes != op.MemoryTarget {
		t.Fatal(m.memory)
	}
}
func TestMemoryStallStopsAutomaticWritesWithoutRetries(t *testing.T) {
	m := memoryMachine(t)
	op := startMemoryGrowth(t, m)
	m.done(op.ID, Outcome{}, epoch)
	if m.deadline() != epoch.Add(30*time.Second) {
		t.Fatal(m.deadline())
	}
	if op := m.next(epoch.Add(30 * time.Second)); op != nil {
		t.Fatal(op)
	}
	if m.memory.LastError == nil || m.memory.Awaiting {
		t.Fatal(m.memory)
	}
	m.memoryGuest(&GuestMemory{TotalBytes: 4 * GiB, AvailableBytes: GiB / 32})
	if op := m.next(epoch.Add(time.Minute)); op != nil {
		t.Fatal("retried stalled driver", op)
	}
	m.vm(VMPaused)
	m.vm(VMRunning)
	if m.memory.LastError == nil {
		t.Fatal("resume pretended to repair driver")
	}
	m.vm(VMOff)
	if m.memory.LastError != nil {
		t.Fatal("cold boot did not reset")
	}
}
func TestMemoryFailureDoesNotBreakOrdinaryRequests(t *testing.T) {
	m := memoryMachine(t)
	op := startMemoryGrowth(t, m)
	m.done(op.ID, Outcome{Err: errors.New("budget")}, epoch)
	m.enqueue(&job{ctx: context.Background(), request: Request{Action: "status"}})
	op = m.next(epoch)
	if op == nil || op.Kind != Dispatch {
		t.Fatal(op)
	}
}
func TestMemoryUnknownDoesNotReclaimOrTreatMissingSampleAsZero(t *testing.T) {
	m := memoryMachine(t)
	m.memoryGuest(nil)
	if op := m.next(epoch); op != nil {
		t.Fatal(op)
	}
	if !m.deadline().IsZero() {
		t.Fatal("unknown memory armed an idle timer", m.deadline())
	}
	if op := m.next(epoch.Add(300 * time.Second)); op != nil {
		t.Fatal(op)
	}
	m.memoryGuest(&GuestMemory{TotalBytes: 100, AvailableBytes: 101})
	if m.memory.GuestKnown {
		t.Fatal("bad sample known")
	}
}
func TestMemoryNewActualWinsOlderDiscoverySnapshot(t *testing.T) {
	m := newMachine(Options{Memory: &MemoryPolicy{}})
	m.vm(VMRunning)
	m.needQuery = false
	op := m.next(epoch)
	m.memoryDevice(MemoryDeviceObserved{Alias: "ua-virtiomem0", ActualBytes: 384 * MiB})
	m.done(op.ID, Outcome{Memory: &MemoryDevice{Alias: "ua-virtiomem0", CapacityBytes: 13 * GiB, BlockBytes: 2 * MiB, ActualBytes: GiB}}, epoch)
	if !m.memory.DeviceKnown || m.memory.Device.ActualBytes != 384*MiB {
		t.Fatal(m.memory)
	}
}
