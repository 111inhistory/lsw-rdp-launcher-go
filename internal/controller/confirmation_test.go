package controller

import (
	"testing"
	"time"
)

func TestVMCommandAckDoesNotChangeState(t *testing.T) {
	m := testMachine()
	m.vm(VMPaused)
	addJob(m, "run", Interactive)
	op := expectOp(t, m, epoch, WakeVM)
	m.done(op.ID, Outcome{}, epoch)
	if s := m.snapshot(); s.VM != VMPaused || s.AwaitingVM != VMRunning {
		t.Fatal(s)
	}
	expectIdle(t, m, epoch.Add(time.Second))
	m.vm(VMRunning)
	expectOp(t, m, epoch.Add(time.Second), StartDisplay)
}
func TestVMConfirmationCanPrecedeAck(t *testing.T) {
	m := testMachine()
	m.vm(VMPaused)
	addJob(m, "run", Interactive)
	op := expectOp(t, m, epoch, WakeVM)
	m.vm(VMRunning)
	expectIdle(t, m, epoch) // Control call still owns the slot until it returns.
	m.done(op.ID, Outcome{}, epoch)
	expectOp(t, m, epoch, StartDisplay)
}
func TestUnrelatedStateIsNotConfirmation(t *testing.T) {
	m := testMachine()
	m.vm(VMPaused)
	addJob(m, "run", Interactive)
	op := expectOp(t, m, epoch, WakeVM)
	m.done(op.ID, Outcome{}, epoch)
	m.vm(VMOff)
	if m.busy == nil {
		t.Fatal("Off confirmed a wake operation")
	}
	expectIdle(t, m, epoch.Add(time.Second))
}
func TestLoopConfirmationTimeoutDoesNotPoll(t *testing.T) {
	h := newHarness(t, Options{VMConfirmTimeout: 10 * time.Millisecond})
	h.notify(t, VMObserved{VMPaused})
	h.wait(t, func(s Snapshot) bool { return s.VM == VMPaused })
	result := h.request(h.ctx, Request{Action: "background", Need: BackendOnly})
	h.expect(t, WakeVM)
	h.complete(t, Outcome{})
	if r := h.reply(t, result); r.Err == nil {
		t.Fatal("missing confirmation was accepted")
	}
	h.wait(t, func(s Snapshot) bool { return s.VM == VMUnknown && s.Operation == "" })
	select {
	case op := <-h.operations:
		t.Fatalf("unexpected fallback operation: %+v", op)
	default:
	}
}
func TestLoopLifecycleBeforeAckPreservesBackendObservation(t *testing.T) {
	h := newHarness(t)
	h.notify(t, VMObserved{VMPaused})
	h.wait(t, func(s Snapshot) bool { return s.VM == VMPaused })
	result := h.request(h.ctx, Request{Action: "background", Need: BackendOnly})
	h.expect(t, WakeVM)
	h.notify(t, VMObserved{VMRunning})
	h.notify(t, BackendObserved{true, SSHZero})
	h.wait(t, func(s Snapshot) bool { return s.VM == VMRunning && s.BackendReady })
	h.complete(t, Outcome{})
	h.expect(t, Dispatch)
	h.complete(t, Outcome{Data: "ok"})
	if r := h.reply(t, result); r.Err != nil {
		t.Fatal(r)
	}
}

func TestLoopWaitsForLifecycleAfterAck(t *testing.T) {
	h := newHarness(t)
	h.notify(t, VMObserved{VMPaused})
	h.wait(t, func(s Snapshot) bool { return s.VM == VMPaused })
	result := h.request(h.ctx, Request{Action: "background", Need: BackendOnly})
	h.expect(t, WakeVM)
	h.complete(t, Outcome{})
	h.wait(t, func(s Snapshot) bool { return s.AwaitingVM == VMRunning })
	if err := h.c.SetLocked(h.ctx, true); err != nil {
		t.Fatal(err)
	}
	h.notify(t, VMObserved{VMRunning})
	h.notify(t, BackendObserved{true, SSHZero})
	h.expect(t, Dispatch)
	h.complete(t, Outcome{Data: "ok"})
	if r := h.reply(t, result); r.Err != nil {
		t.Fatal(r)
	}
}
