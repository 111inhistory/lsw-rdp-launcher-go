package controller

import (
	"context"
	"testing"
	"time"
)

func TestMemoryResizeOverlapsSlowUIWithoutLosingItsSlot(t *testing.T) {
	m := memoryMachine(t)
	m.enqueue(&job{ctx: context.Background(), request: Request{Action: "list_apps"}})
	ui := m.next(epoch)
	if ui == nil || ui.Kind != Dispatch {
		t.Fatal(ui)
	}
	m.memoryGuest(&GuestMemory{TotalBytes: 4 * GiB, AvailableBytes: GiB / 32})
	mem := m.next(epoch)
	if mem == nil || mem.Kind != SetMemory || m.busy != ui || m.memoryBusy != mem {
		t.Fatal(ui, mem, m.snapshot())
	}
	m.done(mem.ID, Outcome{}, epoch)
	if m.busy != ui || m.memoryBusy != nil || !m.memory.Awaiting {
		t.Fatal(m.snapshot())
	}
	m.memoryDeviceAt(MemoryDeviceObserved{Alias: "ua-virtiomem0", ActualBytes: GiB + 2*MiB}, epoch)
	if m.deadline() != epoch.Add(time.Second) {
		t.Fatal("UI work hid memory deadline", m.deadline())
	}
	mem = m.next(epoch.Add(time.Second))
	if mem == nil || mem.Kind != SetMemory || m.busy != ui {
		t.Fatal(mem)
	}
	m.done(ui.ID, Outcome{Data: "apps"}, epoch.Add(time.Second))
	if m.next(epoch.Add(time.Second)) != nil {
		t.Fatal("started another operation while VM-control RPC pending")
	}
	m.done(mem.ID, Outcome{}, epoch.Add(time.Second))
	if len(m.completed) != 1 || m.completed[0].result.Data != "apps" {
		t.Fatal(m.completed)
	}
}
func TestMemoryUnknownDoesNotLeaveAnExpiredDrainTimer(t *testing.T) {
	m := memoryMachine(t)
	op := startMemoryGrowth(t, m)
	m.done(op.ID, Outcome{}, epoch)
	m.memoryDeviceAt(MemoryDeviceObserved{Alias: "ua-virtiomem0", ActualBytes: GiB + 2*MiB}, epoch)
	m.backend(false, SSHUnknown)
	if m.deadline() != epoch.Add(30*time.Second) {
		t.Fatal(m.deadline())
	}
	if op := m.next(epoch.Add(time.Second)); op != nil {
		t.Fatal(op)
	}
}
func TestControllerMemoryIOIsNotBlockedByDispatchAndDoesNotCancelIt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	dispatchStarted := make(chan struct{})
	release := make(chan struct{})
	dispatchCanceled := make(chan struct{})
	memoryStarted := make(chan Operation, 1)
	var c *Controller
	c = New(Options{Memory: &MemoryPolicy{}}, func(ctx context.Context, op Operation) Outcome {
		switch op.Kind {
		case QueryVM:
			return Outcome{VM: VMRunning}
		case QueryMemory:
			return Outcome{Memory: &MemoryDevice{Alias: "ua-virtiomem0", CapacityBytes: 13 * GiB, BlockBytes: 2 * MiB, RequestedBytes: GiB, ActualBytes: GiB}}
		case SuspendVM:
			_ = c.Notify(ctx, VMObserved{State: VMPaused})
			return Outcome{}
		case PrepareMemory:
			return Outcome{ResumeSafe: true, MemoryPrepared: true}
		case WakeVM:
			_ = c.Notify(ctx, VMObserved{State: VMRunning})
			return Outcome{MemoryPrepared: true}
		case Dispatch:
			close(dispatchStarted)
			select {
			case <-release:
				return Outcome{Data: "apps"}
			case <-ctx.Done():
				close(dispatchCanceled)
				return Outcome{Err: ctx.Err()}
			}
		case SetMemory:
			memoryStarted <- op
			return Outcome{}
		default:
			return Outcome{}
		}
	})
	sourceDone := make(chan struct{})
	go func() {
		defer close(sourceDone)
		for state := range c.VMChanges() {
			if state == VMRunning {
				_ = c.Notify(ctx, BackendObserved{true, SSHZero})
				_ = c.Notify(ctx, MemoryObserved{Memory: &GuestMemory{TotalBytes: 4 * GiB, AvailableBytes: 2 * GiB}})
			}
		}
	}()
	stopped := make(chan error, 1)
	go func() { stopped <- c.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-stopped:
		case <-time.After(time.Second):
			t.Error("controller did not stop")
		}
		select {
		case <-sourceDone:
		case <-time.After(time.Second):
			t.Error("source did not stop")
		}
	}()
	replied := make(chan Result, 1)
	go func() { replied <- c.Do(ctx, Request{Action: "list_apps"}) }()
	select {
	case <-dispatchStarted:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := c.Notify(ctx, MemoryObserved{Memory: &GuestMemory{TotalBytes: 4 * GiB, AvailableBytes: GiB / 32}}); err != nil {
		t.Fatal(err)
	}
	select {
	case op := <-memoryStarted:
		if op.MemoryTarget <= GiB {
			t.Fatal(op)
		}
	case <-ctx.Done():
		t.Fatal("dispatch blocked memory I/O", ctx.Err())
	}
	for {
		s, err := c.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if s.Memory.Awaiting && s.Memory.Operation == "" {
			break
		}
	}
	select {
	case <-dispatchCanceled:
		t.Fatal("memory completion canceled UI worker")
	case <-time.After(10 * time.Millisecond):
	}
	close(release)
	select {
	case result := <-replied:
		if result.Err != nil || result.Data != "apps" {
			t.Fatal(result)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
