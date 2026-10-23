package controller

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// D1: completing an automatic StopDisplay must clear the past DisplayDeadline.
// Otherwise the loop would immediately fire an already-expired timer and spin.
func TestAutomaticStopClearsPastDisplayDeadline(t *testing.T) {
	for _, suspend := range []time.Duration{0, 300 * time.Second} {
		t.Run(suspend.String(), func(t *testing.T) {
			m := idleMachine(-time.Second, suspend)
			displayReady(t, m)
			m.windows(0)
			expectIdle(t, m, epoch)
			at := epoch.Add(30 * time.Second)
			stop := expectOp(t, m, at, StopDisplay)
			if stop.Source != SourceIdle || m.state.DisplayDeadline != at {
				t.Fatal("fixture did not arm the disconnect deadline", stop, m.snapshot())
			}
			m.done(stop.ID, Outcome{}, at)
			if !m.state.DisplayDeadline.IsZero() {
				t.Fatal("completed disconnect kept its past deadline", m.snapshot())
			}
			if deadline := m.deadline(); !deadline.IsZero() && deadline.Before(at) {
				t.Fatal("deadline is in the past after disconnect", deadline, at)
			}
			if suspend == 0 {
				if !m.deadline().IsZero() {
					t.Fatal("disabled suspend armed a deadline", m.deadline())
				}
				expectIdle(t, m, at)
				expectIdle(t, m, at.Add(time.Hour))
				return
			}
			expectIdle(t, m, at)
			if m.state.SuspendDeadline != at.Add(suspend) {
				t.Fatal("suspend did not start from disconnect", m.snapshot())
			}
			if m.deadline() != at.Add(suspend) {
				t.Fatal("deadline did not move to the new idle timer", m.deadline())
			}
			expectOp(t, m, at.Add(suspend), SuspendVM)
		})
	}
}

// D4: a result from an operation stamped with an older session generation must
// not reinstall its fresh snapshot after a backend/VM source change.
func TestOldSessionObservationCannotReviveFreshCache(t *testing.T) {
	t.Run("inspect after backend loss", func(t *testing.T) {
		m := testMachine()
		displayReady(t, m)
		one := uint32(1)
		login := bindingLogin(2)
		login.Windows = &one
		addJob(m, "run", ModeDesktop)
		inspect := expectOp(t, m, epoch, InspectSession)
		generation := m.sessionGeneration
		m.backend(false, SSHUnknown)
		if m.sessionGeneration == generation || m.state.Sessions.Fresh {
			t.Fatal("backend loss did not invalidate sessions", m.snapshot())
		}
		partial := map[string]any{"completed": 1}
		m.done(inspect.ID, Outcome{Data: partial, Sessions: activitySnapshot(login)}, epoch)
		if m.sessionGeneration == generation || m.state.Sessions.Fresh {
			t.Fatal("old inspection revived the session cache", m.snapshot())
		}
		if m.switching != nil || len(m.queue) != 0 {
			t.Fatal("old inspection continued the switch", m.snapshot())
		}
		if len(m.completed) != 1 || m.completed[0].result.Err == nil || !strings.Contains(m.completed[0].result.Err.Error(), "session observation source changed") {
			t.Fatal(m.completed)
		}
		if data, ok := m.completed[0].result.Data.(map[string]any); !ok || data["completed"] != 1 {
			t.Fatal("source-change failure dropped partial Data", m.completed[0].result)
		}
	})

	t.Run("start display after VM loss", func(t *testing.T) {
		m := testMachine()
		addJob(m, "run", ModeRemoteApp)
		start := expectOp(t, m, epoch, StartDisplay)
		generation := m.sessionGeneration
		m.vm(VMOff)
		login := bindingLogin(2)
		completeStart(t, m, start, login)
		if m.sessionGeneration == generation || m.state.Sessions.Fresh {
			t.Fatal("old start observation revived the session cache", m.snapshot())
		}
		if m.state.Binding != nil {
			t.Fatal("old start observation installed a binding", m.snapshot())
		}
		if len(m.completed) != 1 || m.completed[0].result.Err == nil {
			t.Fatal("old start observation completed successfully", m.completed)
		}
	})
}

// D4: BackendStreamObserved fences replaced subscriptions. Late events from an
// older generation are dropped, same-generation events are dropped after the
// stream was invalidated, and a higher generation resets the invalid state.
func TestBackendStreamFencesLateEventsAcrossReplacement(t *testing.T) {
	h := newHarness(t)
	h.notify(t, BackendStreamObserved{Generation: 1, Event: BackendObserved{Ready: true, SSH: SSHZero}})
	h.wait(t, func(s Snapshot) bool { return s.BackendReady })

	login := bindingLogin(2)
	result := h.request(h.ctx, Request{Action: "run", Mode: ModeRemoteApp, UserContext: true})
	h.expect(t, StartDisplay)
	snapshot := sessionSnapshot(login)
	h.complete(t, Outcome{Binding: &login, Sessions: &snapshot})
	h.expect(t, Dispatch)
	h.complete(t, Outcome{})
	if r := h.reply(t, result); r.Err != nil {
		t.Fatal(r)
	}
	h.wait(t, func(s Snapshot) bool { return s.Sessions.Fresh })

	// Generation 2 replaces the stream and invalidates the old session cache.
	h.notify(t, BackendStreamObserved{Generation: 2, Event: BackendObserved{Ready: true, SSH: SSHZero}})
	h.wait(t, func(s Snapshot) bool { return !s.Sessions.Fresh })
	// A late generation-1 snapshot is dropped.
	h.notify(t, BackendStreamObserved{Generation: 1, Event: SessionsObserved{Snapshot: *snapshotAt(epoch.Add(time.Minute), login)}})
	h.notify(t, WindowsObserved{Count: 71})
	h.wait(t, func(s Snapshot) bool { return s.Windows == 71 })
	if state, _ := h.c.Snapshot(h.ctx); state.Sessions.Fresh {
		t.Fatal("late generation-1 event revived sessions", state)
	}

	// The current generation can install a fresh snapshot.
	h.notify(t, BackendStreamObserved{Generation: 2, Event: SessionsObserved{Snapshot: *snapshotAt(epoch.Add(time.Minute), login)}})
	h.wait(t, func(s Snapshot) bool { return s.Sessions.Fresh })

	// A lost backend invalidates the same generation; further generation-2
	// session events are fenced until a replacement generation arrives.
	h.notify(t, BackendStreamObserved{Generation: 2, Event: BackendObserved{Ready: false, SSH: SSHUnknown}})
	h.wait(t, func(s Snapshot) bool { return !s.BackendReady && !s.Sessions.Fresh })
	h.notify(t, BackendStreamObserved{Generation: 2, Event: SessionsObserved{Snapshot: *snapshotAt(epoch.Add(2*time.Minute), login)}})
	h.notify(t, WindowsObserved{Count: 72})
	h.wait(t, func(s Snapshot) bool { return s.Windows == 72 })
	if state, _ := h.c.Snapshot(h.ctx); state.Sessions.Fresh {
		t.Fatal("same-generation event bypassed the invalid fence", state)
	}

	// A higher generation resets the fence; older generation-2 events stay
	// dropped, while generation-3 events apply.
	h.notify(t, BackendStreamObserved{Generation: 3, Event: BackendObserved{Ready: true, SSH: SSHZero}})
	h.wait(t, func(s Snapshot) bool { return s.BackendReady })
	h.notify(t, BackendStreamObserved{Generation: 2, Event: SessionsObserved{Snapshot: *snapshotAt(epoch.Add(3*time.Minute), login)}})
	h.notify(t, WindowsObserved{Count: 73})
	h.wait(t, func(s Snapshot) bool { return s.Windows == 73 })
	if state, _ := h.c.Snapshot(h.ctx); state.Sessions.Fresh {
		t.Fatal("old generation event applied after replacement reset", state)
	}
	h.notify(t, BackendStreamObserved{Generation: 3, Event: SessionsObserved{Snapshot: *snapshotAt(epoch.Add(3*time.Minute), login)}})
	h.wait(t, func(s Snapshot) bool { return s.Sessions.Fresh })
}

// D7: an owned display that exits while the WTS logon is still Active must
// invalidate the pending choice before any disconnect/logoff/reconnect step.
func TestDisplayExitDuringConfirmationInvalidatesChoice(t *testing.T) {
	m := testMachine()
	displayID := displayReady(t, m)
	one := uint32(1)
	login := bindingLogin(2)
	login.Windows = &one
	addJob(m, "run", ModeDesktop)
	inspect := expectOp(t, m, epoch, InspectSession)
	m.done(inspect.ID, Outcome{Sessions: activitySnapshot(login)}, epoch)
	confirm := expectOp(t, m, epoch, ConfirmSwitch)

	m.displayExited(displayID, nil)
	if m.state.Display {
		t.Fatal("fixture display did not exit")
	}
	m.done(confirm.ID, Outcome{Decision: SwitchLogoff}, epoch)
	expectIdle(t, m, epoch)
	if m.switching != nil || len(m.queue) != 0 {
		t.Fatal("confirmation survived the owned display exit", m.snapshot())
	}
	if len(m.completed) != 1 || m.completed[0].result.Err == nil || !strings.Contains(m.completed[0].result.Err.Error(), "during confirm-switch") {
		t.Fatal(m.completed)
	}
	// WTS remains Active in the fixture; the display exit invalidated the
	// session cache but must not start a logoff.
	if m.state.Sessions.Fresh || m.state.Display {
		t.Fatal(m.snapshot())
	}
}

// D3: an outcome carrying both partial Data and an error must preserve both.
func TestOutcomeDataAndErrorAreBothPreserved(t *testing.T) {
	m := testMachine()
	backgroundJob(m, "partial")
	op := expectOp(t, m, epoch, Dispatch)
	failure := errors.New("remote command failed after partial work")
	m.done(op.ID, Outcome{Data: map[string]any{"completed": 1}, Err: failure}, epoch)
	if len(m.completed) != 1 {
		t.Fatal(m.completed)
	}
	result := m.completed[0].result
	partial, ok := result.Data.(map[string]any)
	if !ok || partial["completed"] != 1 || !errors.Is(result.Err, failure) {
		t.Fatal("outcome lost its partial data or error", result)
	}
}

// D5: lock blocks a new automatic memory pause, but an already accepted pause
// and its resume are safety cleanup that must continue. See
// See docs/architecture.md: lock does not cancel accepted-operation safety cleanup.
func TestLockBlocksNewPauseButCompletesAcceptedPreparation(t *testing.T) {
	t.Run("new automatic pause blocked", func(t *testing.T) {
		m := memoryMachine(t)
		m.memory.WindowPrepared = false
		m.lock(true)
		m.memoryGuest(&GuestMemory{TotalBytes: 4 * GiB, AvailableBytes: GiB / 32})
		expectIdle(t, m, epoch)
		if m.busy != nil || m.memoryBusy != nil || m.memory.phase != memoryIdle {
			t.Fatal("locked automatic pause started", m.snapshot())
		}
		reason := m.snapshot().BlockingReason
		if !strings.Contains(reason, "unlock") || !strings.Contains(reason, "preparation") {
			t.Fatal("locked unprepared memory did not explain the pause blocker", reason)
		}
		m.lock(false)
		if reason := m.snapshot().BlockingReason; strings.Contains(reason, "memory initialization") {
			t.Fatal("unlock did not clear the pause blocker", reason)
		}
		m.lock(true)
		m.memory.WindowPrepared = true
		if reason := m.snapshot().BlockingReason; strings.Contains(reason, "memory initialization") {
			t.Fatal("prepared memory kept the pause blocker", reason)
		}
	})

	t.Run("accepted preparation and resume continue", func(t *testing.T) {
		m := memoryMachine(t)
		m.memory.WindowPrepared = false
		m.memoryGuest(&GuestMemory{TotalBytes: 4 * GiB, AvailableBytes: GiB / 32})
		pause := expectOp(t, m, epoch, SuspendVM)
		if pause.Source != SourcePressure {
			t.Fatal(pause)
		}
		m.done(pause.ID, Outcome{}, epoch)
		m.lock(true)
		m.vm(VMPaused)
		prepare := expectOp(t, m, epoch, PrepareMemory)
		if prepare.Source != SourceRecovery {
			t.Fatal("accepted safety preparation was not recovery-sourced", prepare)
		}
		m.done(prepare.ID, Outcome{ResumeSafe: true, MemoryPrepared: true}, epoch)
		resume := expectOp(t, m, epoch, WakeVM)
		if resume.Source != SourceRecovery || !resume.ResumeSafe || !resume.MemoryPrepared {
			t.Fatal("accepted safe resume was blocked or downgraded under lock", resume)
		}
		m.done(resume.ID, Outcome{MemoryPrepared: true}, epoch)
		m.vm(VMRunning)
		if m.memory.WindowPrepared != true || m.state.VM != VMRunning {
			t.Fatal("accepted preparation did not finish under lock", m.snapshot())
		}
	})
}

// Idle plan generation: a confirmed idle logoff from an older plan must record
// the real outcome but must not schedule reclaim after a new job, lock or
// activity invalidated that plan. The old plan must not be retried.
func TestLateIdleLogoffDoesNotReclaimUnderInvalidatedPlan(t *testing.T) {
	start := func(t *testing.T) (*machine, Login, *Operation, time.Time) {
		t.Helper()
		m := memoryMachine(t)
		m.opts.IdleDisconnect = 30 * time.Second
		m.opts.IdleLogoff = 0
		m.opts.VMSuspend = 300 * time.Second
		displayReady(t, m)
		binding := *m.state.Binding
		at := autoDisconnect(t, m, epoch)
		inspect := expectOp(t, m, at, InspectSession)
		disconnected := binding
		disconnected.State = WTSDisconnected
		m.done(inspect.ID, Outcome{Sessions: snapshotAt(at, disconnected)}, at)
		logoff := expectOp(t, m, at, LogoffSession)
		if logoff.Source != SourceIdle {
			t.Fatal(logoff)
		}
		return m, binding, logoff, at
	}
	assertNoLatePlan := func(t *testing.T, m *machine) {
		t.Helper()
		if m.logoffReclaim || m.memory.Reclaiming || m.memory.Awaiting || !m.state.LogoffDeadline.IsZero() {
			t.Fatal("late idle logoff scheduled reclaim from an invalidated plan", m.snapshot())
		}
		if m.state.Sessions.Fresh {
			t.Fatal("late idle logoff did not record that the logon snapshot is stale", m.snapshot())
		}
	}

	t.Run("new job", func(t *testing.T) {
		m, _, logoff, at := start(t)
		next := backgroundJob(m, "after-logoff")
		m.done(logoff.ID, Outcome{}, at)
		assertNoLatePlan(t, m)
		op := m.next(at)
		if op == nil || op.Kind != Dispatch || op.job != next {
			t.Fatal("late logoff disturbed the new queued job", op, m.snapshot())
		}
		m.done(op.ID, Outcome{}, at)
	})

	t.Run("lock", func(t *testing.T) {
		m, _, logoff, at := start(t)
		m.lock(true)
		m.done(logoff.ID, Outcome{}, at)
		assertNoLatePlan(t, m)
		expectIdle(t, m, at)
	})

	t.Run("activity", func(t *testing.T) {
		m, _, logoff, at := start(t)
		m.backend(true, SSHActive)
		m.done(logoff.ID, Outcome{}, at)
		assertNoLatePlan(t, m)
		expectIdle(t, m, at)
	})
}
