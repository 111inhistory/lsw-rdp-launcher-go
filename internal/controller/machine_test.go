package controller

import (
	"context"
	"errors"
	"testing"
	"time"
)

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func testMachine() *machine {
	m := newMachine(Options{IdleDisconnect: 30 * time.Second, VMSuspend: 5 * time.Minute, IdleLogoff: -time.Second})
	m.vm(VMRunning)
	m.backend(true, SSHZero)
	return m
}

// addJob uses an empty mode for background work. User requests carry the
// connection mode that selects RemoteApp or the desktop.
func addJob(m *machine, action, mode string) *job {
	j := &job{ctx: context.Background(), request: Request{Action: action, Mode: mode, UserContext: mode != ""}, reply: make(chan Result, 1)}
	m.enqueue(j)
	return j
}

func backgroundJob(m *machine, action string) *job { return addJob(m, action, "") }

func bindingLogin(id uint32) Login {
	return Login{
		Reference: SessionRef{
			SessionID:        id,
			AuthenticationID: "00000000000003E7",
			BootIdentifier:   "11111111-2222-4333-8444-555555555555",
		},
		SID:           "S-1-5-21-100",
		User:          "user",
		Domain:        "DOMAIN",
		State:         WTSActive,
		ClientName:    "rdp-launcher",
		ClientAddress: "10.0.0.2",
		LogonTime:     1000,
		ConnectTime:   2000,
	}
}

func sessionSnapshot(logins ...Login) SessionSnapshot {
	return SessionSnapshot{Revision: 1, Fresh: true, Sessions: logins, ObservedAt: epoch}
}

func completeStart(t *testing.T, m *machine, op *Operation, login Login) {
	t.Helper()
	snapshot := sessionSnapshot(login)
	m.done(op.ID, Outcome{Binding: &login, Sessions: &snapshot}, epoch)
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

// displayReady establishes a bound RemoteApp connection and completes its
// initial application dispatch. The user job models the request that owns the
// connection.
func displayReady(t *testing.T, m *machine) uint64 {
	t.Helper()
	return displayReadyMode(t, m, ModeRemoteApp)
}

func displayReadyMode(t *testing.T, m *machine, mode string) uint64 {
	t.Helper()
	login := bindingLogin(2)
	addJob(m, "run", mode)
	op := expectOp(t, m, epoch, StartDisplay)
	completeStart(t, m, op, login)
	dispatch := expectOp(t, m, epoch, Dispatch)
	m.done(dispatch.ID, Outcome{}, epoch)
	m.completed = nil
	return m.displayID
}

func TestBackendOnlyDoesNotStartDisplay(t *testing.T) {
	m := testMachine()
	backgroundJob(m, "background-query")
	op := expectOp(t, m, epoch, Dispatch)
	m.done(op.ID, Outcome{Data: "answer"}, epoch)
	if m.state.Display || len(m.completed) != 1 || m.completed[0].result.Data != "answer" {
		t.Fatal(m.snapshot(), m.completed)
	}
}

func TestUserRequestWaitsForBackendBeforeDisplay(t *testing.T) {
	m := testMachine()
	m.backend(false, SSHUnknown)
	addJob(m, "run", ModeRemoteApp)
	expectIdle(t, m, epoch)
	m.backend(true, SSHUnknown)
	op := expectOp(t, m, epoch, StartDisplay)
	login := bindingLogin(2)
	completeStart(t, m, op, login)
	dispatch := expectOp(t, m, epoch, Dispatch)
	m.done(dispatch.ID, Outcome{Data: "started"}, epoch)
	if !m.state.Display || m.state.DisplayMode != ModeRemoteApp {
		t.Fatal(m.snapshot())
	}
}

func TestFIFOUsesOneWakeAndOneDisplay(t *testing.T) {
	m := testMachine()
	m.vm(VMPaused)
	first := addJob(m, "first", ModeRemoteApp)
	second := addJob(m, "second", ModeRemoteApp)
	op := expectOp(t, m, epoch, WakeVM)
	if op.VM != VMPaused {
		t.Fatal(op)
	}
	m.done(op.ID, Outcome{}, epoch)
	m.vm(VMRunning)
	m.backend(true, SSHZero)
	op = expectOp(t, m, epoch, StartDisplay)
	completeStart(t, m, op, bindingLogin(2))
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
			backgroundJob(m, "query")
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
	addJob(m, "run", ModeRemoteApp)
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

func TestLockBlocksAutomaticDisconnectAndSuspend(t *testing.T) {
	m := testMachine()
	m.lock(true)
	expectIdle(t, m, epoch.Add(time.Hour))
	if !m.state.SuspendDeadline.IsZero() {
		t.Fatal("lock failed")
	}
	displayReady(t, m)
	m.windows(0)
	expectIdle(t, m, epoch.Add(time.Hour))
	m.lock(false)
	expectIdle(t, m, epoch.Add(2*time.Hour))
	if m.state.DisplayDeadline != epoch.Add(2*time.Hour+30*time.Second) {
		t.Fatal("unlock did not start a new disconnect timer", m.snapshot())
	}
	expectOp(t, m, epoch.Add(2*time.Hour+30*time.Second), StopDisplay)
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
	backgroundJob(m, "run")
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
	j := backgroundJob(m, "canceled")
	ctx, cancel := context.WithCancel(context.Background())
	j.ctx = ctx
	cancel()
	backgroundJob(m, "next")
	op := expectOp(t, m, epoch, Dispatch)
	if op.Request.Action != "next" || len(m.completed) != 1 || !errors.Is(m.completed[0].result.Err, context.Canceled) {
		t.Fatal(op, m.completed)
	}
}

func TestFailureRepliesAndDoesNotRetryMutation(t *testing.T) {
	m := testMachine()
	backgroundJob(m, "run")
	op := expectOp(t, m, epoch, Dispatch)
	failure := errors.New("connection lost; outcome unknown")
	m.done(op.ID, Outcome{Err: failure}, epoch)
	if len(m.queue) != 0 || m.completed[0].result.Err != failure {
		t.Fatal(m.completed)
	}
	expectIdle(t, m, epoch.Add(time.Hour))
}

func TestQueryFailureRepliesToJobsAddedDuringQuery(t *testing.T) {
	m := newMachine(Options{IdleLogoff: -time.Second})
	op := expectOp(t, m, epoch, QueryVM)
	backgroundJob(m, "a")
	backgroundJob(m, "b")
	m.done(op.ID, Outcome{Err: errors.New("virsh unavailable")}, epoch)
	if len(m.queue) != 0 || len(m.completed) != 2 {
		t.Fatal(m.snapshot(), m.completed)
	}
	expectIdle(t, m, epoch)
	backgroundJob(m, "manual retry")
	expectOp(t, m, epoch, QueryVM)
}

func TestAutomaticFailureDoesNotLoop(t *testing.T) {
	m := testMachine()
	expectIdle(t, m, epoch)
	op := expectOp(t, m, epoch.Add(5*time.Minute), SuspendVM)
	m.done(op.ID, Outcome{Err: errors.New("suspend failed")}, epoch)
	expectIdle(t, m, epoch.Add(10*time.Minute))
}
