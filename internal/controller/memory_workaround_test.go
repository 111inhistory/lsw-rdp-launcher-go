package controller

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMemoryPreparationUsesAcknowledgedLifecycleAndKeepsDisplay(t *testing.T) {
	m := memoryMachine(t)
	m.memory.WindowPrepared = false
	m.state.Display = true
	m.memoryGuest(&GuestMemory{TotalBytes: 4 * GiB, AvailableBytes: GiB / 32})
	pause := m.next(epoch)
	if pause == nil || pause.Kind != SuspendVM {
		t.Fatal(pause)
	}
	m.done(pause.ID, Outcome{}, epoch)
	if next := m.next(epoch); next != nil || m.snapshot().AwaitingVM != VMPaused {
		t.Fatal(next, m.snapshot())
	}
	m.vm(VMPaused)
	prime := m.next(epoch)
	if prime == nil || prime.Kind != PrepareMemory || !m.state.Display {
		t.Fatal(prime, m.snapshot())
	}
	m.done(prime.ID, Outcome{ResumeSafe: true, MemoryPrepared: true}, epoch)
	resume := m.next(epoch)
	if resume == nil || resume.Kind != WakeVM || !resume.ResumeSafe || !resume.MemoryPrepared {
		t.Fatal(resume)
	}
	m.done(resume.ID, Outcome{MemoryPrepared: true}, epoch)
	if next := m.next(epoch); next != nil {
		t.Fatal("resume ack treated as Running", next)
	}
	m.vm(VMRunning)
	if !m.memory.WindowPrepared || m.snapshot().Memory.Preparing || !m.state.Display {
		t.Fatal(m.snapshot())
	}
	next := m.next(epoch)
	if next == nil || next.Kind != QueryMemory {
		t.Fatal(next)
	}
}
func TestMemoryPreparationFailureWithSafeRestoreStillResumes(t *testing.T) {
	m := memoryMachine(t)
	m.memory.WindowPrepared = false
	m.memory.phase = memoryWindow
	m.state.VM = VMPaused
	op := m.next(epoch)
	m.done(op.ID, Outcome{Err: errors.New("primefailed"), ResumeSafe: true}, epoch)
	op = m.next(epoch)
	if op == nil || op.Kind != WakeVM || !op.ResumeSafe || op.MemoryPrepared {
		t.Fatal(op)
	}
	m.vm(VMRunning)
	m.done(op.ID, Outcome{}, epoch)
	if m.memory.WindowPrepared || m.memory.LastError == nil || m.memory.phase != memoryIdle {
		t.Fatal(m.memory)
	}
}
func TestMemoryPreparationUnsafeFailureDoesNotAutoWake(t *testing.T) {
	m := memoryMachine(t)
	m.memory.WindowPrepared = false
	m.memory.phase = memoryWindow
	m.state.VM = VMPaused
	m.enqueue(&job{ctx: context.Background()})
	op := m.next(epoch)
	if op == nil || op.Kind != PrepareMemory {
		t.Fatal(op)
	}
	m.done(op.ID, Outcome{Err: errors.New("restorefailed")}, epoch)
	if op := m.next(epoch); op != nil {
		t.Fatal("resumed with unsafe target", op)
	}
	if len(m.queue) != 0 || m.memory.LastError == nil || m.state.VM != VMPaused {
		t.Fatal(m.snapshot())
	}
}
func TestMemoryPreparationResumeFailureDoesNotRetryQueuedRequest(t *testing.T) {
	m := memoryMachine(t)
	m.memory.phase = memoryResuming
	m.memory.resumeSafe = true
	m.state.VM = VMPaused
	m.enqueue(&job{ctx: context.Background()})
	op := m.next(epoch)
	if op == nil || op.Kind != WakeVM {
		t.Fatal(op)
	}
	m.done(op.ID, Outcome{Err: errors.New("resumefailed")}, epoch)
	if op := m.next(epoch); op != nil || len(m.queue) != 0 {
		t.Fatal(op, m.snapshot())
	}
}
func TestMemoryDrainRequiresActualProgressAndKeepsOriginalDeadline(t *testing.T) {
	m := memoryMachine(t)
	op := startMemoryGrowth(t, m)
	target := op.MemoryTarget
	m.done(op.ID, Outcome{}, epoch)
	if next := m.next(epoch.Add(2 * time.Second)); next != nil {
		t.Fatal("poked without progress", next)
	}
	m.memoryDeviceAt(MemoryDeviceObserved{Alias: "ua-virtiomem0", ActualBytes: GiB + 2*MiB}, epoch.Add(3*time.Second))
	if m.deadline() != epoch.Add(4*time.Second) {
		t.Fatal(m.deadline())
	}
	if next := m.next(epoch.Add(3500 * time.Millisecond)); next != nil {
		t.Fatal(next)
	}
	next := m.next(epoch.Add(4 * time.Second))
	if next == nil || next.Kind != SetMemory || next.MemoryTarget != target {
		t.Fatal(next)
	}
	m.done(next.ID, Outcome{}, epoch.Add(4*time.Second))
	if m.deadline() != epoch.Add(30*time.Second) {
		t.Fatal("poke reset total deadline", m.deadline())
	}
	if next := m.next(epoch.Add(6 * time.Second)); next != nil {
		t.Fatal("re-poked without new progress", next)
	}
}
func TestMemoryDrainStopsAt80EvenWithPartialDeviceTarget(t *testing.T) {
	m := memoryMachine(t)
	op := startMemoryGrowth(t, m)
	m.done(op.ID, Outcome{}, epoch)
	m.memoryDeviceAt(MemoryDeviceObserved{Alias: "ua-virtiomem0", ActualBytes: GiB + 2*MiB}, epoch)
	m.memoryGuest(&GuestMemory{TotalBytes: 4 * GiB, AvailableBytes: GiB})
	if m.memory.Growing || m.memory.Awaiting {
		t.Fatal(m.memory)
	}
	if next := m.next(epoch.Add(time.Second)); next != nil {
		t.Fatal(next)
	}
}
func TestMemoryDrainHasBoundedCount(t *testing.T) {
	m := memoryMachine(t)
	op := startMemoryGrowth(t, m)
	m.done(op.ID, Outcome{}, epoch)
	for i := 1; i <= memoryDrainLimit; i++ {
		now := epoch.Add(time.Duration(i) * 2 * time.Second)
		m.memoryDeviceAt(MemoryDeviceObserved{Alias: "ua-virtiomem0", ActualBytes: GiB + uint64(i)*2*MiB}, now)
		next := m.next(now.Add(time.Second))
		if next == nil || next.Kind != SetMemory {
			t.Fatal(i, next)
		}
		m.done(next.ID, Outcome{}, now.Add(time.Second))
	}
	m.memoryDeviceAt(MemoryDeviceObserved{Alias: "ua-virtiomem0", ActualBytes: GiB + 20*MiB}, epoch.Add(18*time.Second))
	if m.deadline() != epoch.Add(30*time.Second) {
		t.Fatal(m.deadline())
	}
	if next := m.next(epoch.Add(19 * time.Second)); next != nil {
		t.Fatal("unbounded drain", next)
	}
}
