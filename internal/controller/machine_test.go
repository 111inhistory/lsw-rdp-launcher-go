package controller

import (
	"context"
	"errors"
	"testing"
	"time"
)

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func testMachine() *machine {
	m := newMachine(Options{IdleDisconnect: 30 * time.Second, VMSuspend: 5 * time.Minute})
	m.vm(VMRunning)
	m.backend(true, SSHZero)
	return m
}
func addJob(m *machine, action string, need Requirement) *job {
	j := &job{ctx: context.Background(), request: Request{Action: action, Need: need}, reply: make(chan Result, 1)}
	m.enqueue(j)
	return j
}
func expectOp(t *testing.T, m *machine, now time.Time, kind Kind) *Operation {
	t.Helper()
	op := m.next(now)
	if op == nil || op.Kind != kind {
		t.Fatalf("want %s, got %+v (state %+v)", kind, op, m.snapshot())
	}
	return op
}
func expectIdle(t *testing.T, m *machine, now time.Time) {
	t.Helper()
	if op := m.next(now); op != nil {
		t.Fatalf("unexpected operation %+v", op)
	}
}
func displayReady(t *testing.T, m *machine) uint64 {
	t.Helper()
	addJob(m, "run", Interactive)
	op := expectOp(t, m, epoch, StartDisplay)
	m.done(op.ID, Outcome{}, epoch)
	op = expectOp(t, m, epoch, Dispatch)
	m.done(op.ID, Outcome{}, epoch)
	m.completed = nil
	return m.displayID
}

func TestBackendOnlyDoesNotStartDisplay(t *testing.T) {
	m := testMachine()
	addJob(m, "background-query", BackendOnly)
	op := expectOp(t, m, epoch, Dispatch)
	m.done(op.ID, Outcome{Data: "answer"}, epoch)
	if m.state.Display || len(m.completed) != 1 || m.completed[0].result.Data != "answer" {
		t.Fatal(m.snapshot(), m.completed)
	}
}
func TestInteractiveRequiresBothCapabilities(t *testing.T) {
	m := testMachine()
	m.backend(false, SSHUnknown)
	addJob(m, "run", Interactive)
	op := expectOp(t, m, epoch, StartDisplay)
	m.done(op.ID, Outcome{}, epoch)
	expectIdle(t, m, epoch)
	if !m.state.Display || m.state.BackendReady {
		t.Fatal(m.snapshot())
	}
	m.backend(true, SSHZero)
	expectOp(t, m, epoch, Dispatch)
}
func TestFIFOUsesOneWakeAndOneDisplay(t *testing.T) {
	m := testMachine()
	m.vm(VMPaused)
	first := addJob(m, "first", Interactive)
	second := addJob(m, "second", Interactive)
	op := expectOp(t, m, epoch, WakeVM)
	if op.VM != VMPaused {
		t.Fatal(op)
	}
	m.done(op.ID, Outcome{}, epoch)
	m.vm(VMRunning)
	op = expectOp(t, m, epoch, StartDisplay)
	m.done(op.ID, Outcome{}, epoch)
	m.backend(true, SSHZero)
	op = expectOp(t, m, epoch, Dispatch)
	if op.Request.Action != "first" {
		t.Fatal(op)
	}
	m.done(op.ID, Outcome{}, epoch)
	op = expectOp(t, m, epoch, Dispatch)
	if op.Request.Action != "second" {
		t.Fatal(op)
	}
	m.done(op.ID, Outcome{}, epoch)
	if len(m.completed) != 2 || m.completed[0].job != first || m.completed[1].job != second {
		t.Fatal(m.completed)
	}
}
func TestWakePreservesStateForCorrectCommandChoice(t *testing.T) {
	for _, state := range []VMState{VMOff, VMPaused, VMPMSuspended} {
		t.Run(string(state), func(t *testing.T) {
			m := testMachine()
			m.vm(state)
			addJob(m, "query", BackendOnly)
			op := expectOp(t, m, epoch, WakeVM)
			if op.VM != state {
				t.Fatal(op.VM)
			}
			m.done(op.ID, Outcome{}, epoch)
			m.vm(VMRunning)
			expectIdle(t, m, epoch) // Wait for a fresh backend observation.
		})
	}
}
func TestSSHBoundaryAndUnknownControlSuspendTimer(t *testing.T) {
	m := testMachine()
	expectIdle(t, m, epoch)
	want := epoch.Add(5 * time.Minute)
	if m.state.SuspendDeadline != want {
		t.Fatal(m.state.SuspendDeadline)
	}
	m.backend(true, SSHZero)
	expectIdle(t, m, epoch.Add(time.Minute))
	if m.state.SuspendDeadline != want {
		t.Fatal("unchanged zero reset the deadline")
	}
	m.backend(true, SSHActive)
	expectIdle(t, m, want)
	if !m.state.SuspendDeadline.IsZero() {
		t.Fatal("active SSH retained timer")
	}
	m.backend(true, SSHZero)
	expectIdle(t, m, want)
	if m.state.SuspendDeadline != want.Add(5*time.Minute) {
		t.Fatal(m.state.SuspendDeadline)
	}
	m.backend(false, SSHZero)
	expectIdle(t, m, want.Add(10*time.Minute))
	if m.state.SSH != SSHUnknown || !m.state.SuspendDeadline.IsZero() {
		t.Fatal(m.snapshot())
	}
}
func TestLaunchAtIdleDeadlineCancelsSuspend(t *testing.T) {
	m := testMachine()
	expectIdle(t, m, epoch)
	addJob(m, "run", Interactive)
	expectOp(t, m, epoch.Add(5*time.Minute), StartDisplay)
}
func TestWindowTransitionsRestartDisplayTimer(t *testing.T) {
	m := testMachine()
	displayReady(t, m)
	m.windows(0)
	expectIdle(t, m, epoch)
	m.windows(1)
	m.windows(0) // Includes activity that a snapshot-only loop misses.
	expectIdle(t, m, epoch.Add(20*time.Second))
	if m.state.DisplayDeadline != epoch.Add(50*time.Second) {
		t.Fatal(m.state.DisplayDeadline)
	}
	expectIdle(t, m, epoch.Add(30*time.Second))
	m.windows(-1)
	expectIdle(t, m, epoch.Add(time.Minute))
	if !m.state.DisplayDeadline.IsZero() {
		t.Fatal("unknown windows kept deadline")
	}
	m.windows(0)
	expectIdle(t, m, epoch.Add(time.Minute))
	expectOp(t, m, epoch.Add(90*time.Second), StopDisplay)
}
func TestDisplayDisconnectPreservesBackend(t *testing.T) {
	m := testMachine()
	displayReady(t, m)
	m.windows(0)
	expectIdle(t, m, epoch)
	op := expectOp(t, m, epoch.Add(30*time.Second), StopDisplay)
	m.done(op.ID, Outcome{}, epoch)
	if !m.state.BackendReady || m.state.SSH != SSHZero {
		t.Fatal(m.snapshot())
	}
	expectIdle(t, m, epoch.Add(30*time.Second))
	expectOp(t, m, epoch.Add(330*time.Second), SuspendVM)
}
func TestLockOnlyBlocksVMSuspend(t *testing.T) {
	m := testMachine()
	m.lock(true)
	expectIdle(t, m, epoch.Add(time.Hour))
	if !m.state.SuspendDeadline.IsZero() {
		t.Fatal("lock failed")
	}
	displayReady(t, m)
	m.windows(0)
	expectIdle(t, m, epoch)
	expectOp(t, m, epoch.Add(30*time.Second), StopDisplay)
}
func TestUnlockStartsNewTimerAndVMOffClearsLock(t *testing.T) {
	m := testMachine()
	m.lock(true)
	expectIdle(t, m, epoch)
	m.lock(false)
	expectIdle(t, m, epoch.Add(time.Hour))
	if m.state.SuspendDeadline != epoch.Add(time.Hour+5*time.Minute) {
		t.Fatal(m.snapshot())
	}
	m.lock(true)
	m.vm(VMOff)
	if m.state.Locked {
		t.Fatal("lock survived VM shutdown")
	}
}
func TestLifecycleBeatsOldControlSnapshot(t *testing.T) {
	m := newMachine(Options{})
	op := expectOp(t, m, epoch, QueryVM)
	m.vm(VMOff)
	m.done(op.ID, Outcome{VM: VMRunning}, epoch)
	if m.state.VM != VMOff {
		t.Fatal("old query overwrote lifecycle", m.snapshot())
	}
}
func TestVMChangeInvalidatesRemoteOutcome(t *testing.T) {
	m := testMachine()
	addJob(m, "run", BackendOnly)
	op := expectOp(t, m, epoch, Dispatch)
	m.vm(VMOff)
	m.done(op.ID, Outcome{Data: "ok"}, epoch)
	if len(m.completed) != 1 || m.completed[0].result.Err == nil {
		t.Fatal(m.completed)
	}
}
func TestLateDisplayExitDoesNotClearReplacement(t *testing.T) {
	m := testMachine()
	old := displayReady(t, m)
	m.displayExited(old, nil)
	current := displayReady(t, m)
	m.displayExited(old, errors.New("old process"))
	if !m.state.Display || m.displayID != current {
		t.Fatal(m.snapshot())
	}
}
func TestCanceledHeadDoesNotBlockNextRequest(t *testing.T) {
	m := testMachine()
	j := addJob(m, "canceled", BackendOnly)
	ctx, cancel := context.WithCancel(context.Background())
	j.ctx = ctx
	cancel()
	addJob(m, "next", BackendOnly)
	op := expectOp(t, m, epoch, Dispatch)
	if op.Request.Action != "next" || len(m.completed) != 1 || !errors.Is(m.completed[0].result.Err, context.Canceled) {
		t.Fatal(op, m.completed)
	}
}
func TestFailureRepliesAndDoesNotRetryMutation(t *testing.T) {
	m := testMachine()
	addJob(m, "run", BackendOnly)
	op := expectOp(t, m, epoch, Dispatch)
	failure := errors.New("connection lost; outcome unknown")
	m.done(op.ID, Outcome{Err: failure}, epoch)
	if len(m.queue) != 0 || m.completed[0].result.Err != failure {
		t.Fatal(m.completed)
	}
	expectIdle(t, m, epoch.Add(time.Hour))
}
func TestQueryFailureRepliesToJobsAddedDuringQuery(t *testing.T) {
	m := newMachine(Options{})
	op := expectOp(t, m, epoch, QueryVM)
	addJob(m, "a", BackendOnly)
	addJob(m, "b", BackendOnly)
	m.done(op.ID, Outcome{Err: errors.New("virsh unavailable")}, epoch)
	if len(m.queue) != 0 || len(m.completed) != 2 {
		t.Fatal(m.snapshot(), m.completed)
	}
	expectIdle(t, m, epoch)
	addJob(m, "manual retry", BackendOnly)
	expectOp(t, m, epoch, QueryVM)
}
func TestAutomaticFailureDoesNotLoop(t *testing.T) {
	m := testMachine()
	expectIdle(t, m, epoch)
	op := expectOp(t, m, epoch.Add(5*time.Minute), SuspendVM)
	m.done(op.ID, Outcome{Err: errors.New("suspend failed")}, epoch)
	expectIdle(t, m, epoch.Add(10*time.Minute))
}
