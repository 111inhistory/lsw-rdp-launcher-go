package controller

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestFirstIdleLogoffDueAlwaysStartsInspection(t *testing.T) {
	m := idleMachine(0, 300*time.Second)
	displayReady(t, m)
	binding := *m.state.Binding
	at := autoDisconnect(t, m, epoch)
	// Even a fresh cached snapshot must not skip the mandatory first query.
	disconnected := binding
	disconnected.State = WTSDisconnected
	m.sessions(*snapshotAt(at, disconnected))
	if !m.state.Sessions.Fresh {
		t.Fatal("fixture cache is not fresh")
	}
	inspect := expectOp(t, m, at, InspectSession)
	if inspect.Source != SourceIdle || inspect.Deadline != at.Add(idleLogoffTransitionTimeout) {
		t.Fatal("first due did not start a bounded inspection", inspect)
	}
	m.done(inspect.ID, Outcome{Sessions: snapshotAt(at, disconnected)}, at)
	logoff := expectOp(t, m, at, LogoffSession)
	if logoff.Source != SourceIdle || logoff.Reference != binding.Reference {
		t.Fatal(logoff)
	}
}

func TestIdleLogoffRetriesTransitionalActiveThenLogsOff(t *testing.T) {
	m := idleMachine(0, 300*time.Second)
	displayReady(t, m)
	binding := *m.state.Binding
	at := autoDisconnect(t, m, epoch)
	inspect := expectOp(t, m, at, InspectSession)
	active := binding
	active.State = WTSActive
	m.done(inspect.ID, Outcome{Sessions: snapshotAt(at, active)}, at)
	expectIdle(t, m, at)
	if m.idleLogoffDone {
		t.Fatal("transitional Active login was skipped immediately")
	}
	retry := at.Add(idleLogoffRetryInterval)
	if m.idleLogoffRetryAt != retry || m.deadline() != retry {
		t.Fatal("transitional retry was not bounded to 500ms", m.idleLogoffRetryAt, m.deadline())
	}
	expectIdle(t, m, retry.Add(-time.Millisecond))
	inspect = expectOp(t, m, retry, InspectSession)
	disconnected := binding
	disconnected.State = WTSDisconnected
	m.done(inspect.ID, Outcome{Sessions: snapshotAt(retry, disconnected)}, retry)
	logoff := expectOp(t, m, retry, LogoffSession)
	if logoff.Reference != binding.Reference {
		t.Fatal(logoff)
	}
}

func TestIdleLogoffSkipsChangedLoginAfterFreshInspection(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Login)
	}{
		{"changed connect time", func(l *Login) { l.ConnectTime++; l.State = WTSActive }},
		{"changed client", func(l *Login) { l.ClientName = "another-client"; l.State = WTSActive }},
		{"reconnected then disconnected", func(l *Login) { l.ConnectTime++; l.State = WTSDisconnected }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := idleMachine(0, 300*time.Second)
			displayReady(t, m)
			binding := *m.state.Binding
			at := autoDisconnect(t, m, epoch)
			inspect := expectOp(t, m, at, InspectSession)
			login := binding
			tc.mutate(&login)
			m.done(inspect.ID, Outcome{Sessions: snapshotAt(at, login)}, at)
			expectIdle(t, m, at)
			if !m.idleLogoffDone || !strings.Contains(m.state.BlockingReason, "retained") {
				t.Fatal("changed login was not skipped with a reason", m.snapshot())
			}
			expectIdle(t, m, at.Add(idleLogoffRetryInterval))
			expectIdle(t, m, at.Add(299*time.Second))
			expectOp(t, m, at.Add(300*time.Second), SuspendVM)
		})
	}
}

func TestIdleLogoffUnchangedActiveStopsAtTransitionDeadline(t *testing.T) {
	m := idleMachine(0, 300*time.Second)
	displayReady(t, m)
	binding := *m.state.Binding
	at := autoDisconnect(t, m, epoch)
	inspect := expectOp(t, m, at, InspectSession)
	active := binding
	active.State = WTSActive
	for i := 0; i < 60; i++ {
		now := at.Add(time.Duration(i) * idleLogoffRetryInterval)
		if i > 0 {
			inspect = expectOp(t, m, now, InspectSession)
		}
		m.done(inspect.ID, Outcome{Sessions: snapshotAt(now, active)}, now)
		if i < 59 {
			expectIdle(t, m, now)
			if m.idleLogoffDone {
				t.Fatalf("retry %d skipped before the transition deadline", i)
			}
			if want := earlier(now.Add(idleLogoffRetryInterval), at.Add(idleLogoffTransitionTimeout)); m.idleLogoffRetryAt != want {
				t.Fatalf("retry %d deadline=%s want %s", i, m.idleLogoffRetryAt, want)
			}
		}
	}
	expectIdle(t, m, at.Add(idleLogoffTransitionTimeout))
	if !m.idleLogoffDone || !strings.Contains(m.state.BlockingReason, "transition deadline") {
		t.Fatal("unchanged Active login did not stop at 30s", m.snapshot())
	}
	expectIdle(t, m, at.Add(30*time.Second+time.Millisecond))
	expectOp(t, m, at.Add(300*time.Second), SuspendVM)
}

func TestIdleLogoffTransitionWaitCannotSuspendEarly(t *testing.T) {
	m := idleMachine(0, time.Millisecond)
	displayReady(t, m)
	binding := *m.state.Binding
	at := autoDisconnect(t, m, epoch)
	inspect := expectOp(t, m, at, InspectSession)
	active := binding
	active.State = WTSActive
	m.done(inspect.ID, Outcome{Sessions: snapshotAt(at, active)}, at)
	expectIdle(t, m, at)
	retry := at.Add(idleLogoffRetryInterval)
	if m.deadline() != retry {
		t.Fatal("suspend deadline outran the transitional retry", m.deadline())
	}
	expectIdle(t, m, at.Add(time.Millisecond))
	expectIdle(t, m, retry.Add(-time.Millisecond))
	inspect = expectOp(t, m, retry, InspectSession)
	disconnected := binding
	disconnected.State = WTSDisconnected
	m.done(inspect.ID, Outcome{Sessions: snapshotAt(retry, disconnected)}, retry)
	expectOp(t, m, retry, LogoffSession)
}

func TestStopAndNaturalDisplayExitInvalidateSessionCache(t *testing.T) {
	t.Run("stop display", func(t *testing.T) {
		m := idleMachine(-time.Second, 300*time.Second)
		displayReady(t, m)
		generation := m.sessionGeneration
		m.windows(0)
		expectIdle(t, m, epoch)
		stop := expectOp(t, m, epoch.Add(30*time.Second), StopDisplay)
		m.done(stop.ID, Outcome{}, epoch.Add(30*time.Second))
		if m.state.Sessions.Fresh || m.sessionGeneration == generation {
			t.Fatal("StopDisplay completion kept the session cache fresh", m.snapshot())
		}
	})
	t.Run("natural display exit", func(t *testing.T) {
		m := idleMachine(-time.Second, 300*time.Second)
		displayID := displayReady(t, m)
		generation := m.sessionGeneration
		m.displayExited(displayID, nil)
		if m.state.Sessions.Fresh || m.sessionGeneration == generation {
			t.Fatal("owned display exit kept the session cache fresh", m.snapshot())
		}
	})
}

func TestIdleLogoffClockIgnoresMemoryUnknownAndPressure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		memory func(*machine)
	}{
		{"unknown", func(m *machine) { m.memoryGuest(nil) }},
		{"pressure and growing", func(m *machine) {
			m.memoryGuest(&GuestMemory{TotalBytes: 4 * GiB, AvailableBytes: GiB / 32})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := memoryMachine(t)
			m.opts.IdleDisconnect = 30 * time.Second
			m.opts.IdleLogoff = 60 * time.Second
			m.opts.VMSuspend = 300 * time.Second
			displayReady(t, m)
			binding := *m.state.Binding
			at := autoDisconnect(t, m, epoch)
			tc.memory(m)
			// Pressure may queue an allowed growth write before the idle path.
			if op := m.next(at); op != nil {
				if op.Kind != SetMemory || op.Source != SourcePressure {
					t.Fatalf("unexpected pre-logoff operation: %+v", op)
				}
				m.done(op.ID, Outcome{}, at)
			}
			expectIdle(t, m, at)
			if m.state.LogoffDeadline != at.Add(60*time.Second) {
				t.Fatal("memory state reset the logout clock", m.snapshot())
			}
			expectIdle(t, m, at.Add(59*time.Second))
			inspect := expectOp(t, m, at.Add(60*time.Second), InspectSession)
			disconnected := binding
			disconnected.State = WTSDisconnected
			m.done(inspect.ID, Outcome{Sessions: snapshotAt(at.Add(60*time.Second), disconnected)}, at.Add(60*time.Second))
			logoff := expectOp(t, m, at.Add(60*time.Second), LogoffSession)
			if logoff.Reference != binding.Reference {
				t.Fatal(logoff)
			}
			m.done(logoff.ID, Outcome{}, at.Add(60*time.Second))
			expectIdle(t, m, at.Add(60*time.Second))
			expectIdle(t, m, at.Add(300*time.Second))
			if m.memory.Reclaiming {
				t.Fatal("unaligned memory allowed reclaim before it was safe", m.snapshot())
			}
		})
	}
}

func TestEnabledLogoffDefersPreSuspendReclaimUntilConfirmation(t *testing.T) {
	m := memoryMachine(t)
	m.opts.IdleDisconnect = 30 * time.Second
	m.opts.IdleLogoff = 300 * time.Second
	m.opts.VMSuspend = 310 * time.Second
	displayReady(t, m)
	binding := *m.state.Binding
	at := autoDisconnect(t, m, epoch)
	expectIdle(t, m, at)
	if m.state.LogoffDeadline != at.Add(300*time.Second) || m.state.SuspendDeadline != at.Add(310*time.Second) {
		t.Fatal("logout/suspend clocks are not disconnect-based", m.snapshot())
	}
	// The old pre-suspend reclaim lead would be at+280s, before the enabled
	// logout is due. It must not run ahead of the logout decision.
	expectIdle(t, m, at.Add(279*time.Second))
	expectIdle(t, m, at.Add(280*time.Second))
	if m.memory.Reclaiming || m.memory.Awaiting {
		t.Fatal("pre-suspend reclaim ran before an enabled future logout", m.snapshot())
	}
	if m.deadline() != at.Add(300*time.Second) {
		t.Fatal("old reclaim deadline leaked into the timer", m.deadline())
	}
	expectIdle(t, m, at.Add(299*time.Second))
	inspect := expectOp(t, m, at.Add(300*time.Second), InspectSession)
	disconnected := binding
	disconnected.State = WTSDisconnected
	m.done(inspect.ID, Outcome{Sessions: snapshotAt(at.Add(300*time.Second), disconnected)}, at.Add(300*time.Second))
	logoff := expectOp(t, m, at.Add(300*time.Second), LogoffSession)
	m.done(logoff.ID, Outcome{}, at.Add(300*time.Second))
	reclaim := expectOp(t, m, at.Add(300*time.Second), SetMemory)
	if reclaim.Source != SourceIdle || reclaim.MemoryTarget != 0 {
		t.Fatal("confirmed logout did not immediately queue the pending reclaim", reclaim)
	}
}

func TestConfirmedLogoffKeepsPendingReclaimUntilMemorySafe(t *testing.T) {
	m := memoryMachine(t)
	m.opts.IdleDisconnect = 30 * time.Second
	m.opts.IdleLogoff = 0
	m.opts.VMSuspend = 300 * time.Second
	displayReady(t, m)
	binding := *m.state.Binding
	at := autoDisconnect(t, m, epoch)
	m.memoryGuest(&GuestMemory{TotalBytes: 4 * GiB, AvailableBytes: GiB / 32})
	if op := m.next(at); op != nil {
		if op.Kind != SetMemory || op.Source != SourcePressure {
			t.Fatalf("unexpected pre-logoff operation: %+v", op)
		}
		m.done(op.ID, Outcome{}, at)
	}
	inspect := expectOp(t, m, at, InspectSession)
	disconnected := binding
	disconnected.State = WTSDisconnected
	m.done(inspect.ID, Outcome{Sessions: snapshotAt(at, disconnected)}, at)
	logoff := expectOp(t, m, at, LogoffSession)
	m.done(logoff.ID, Outcome{}, at)
	expectIdle(t, m, at)
	if !m.logoffReclaim || m.memory.Reclaiming {
		t.Fatal("unsafe memory did not retain the pending reclaim", m.snapshot())
	}
	m.memoryGuest(&GuestMemory{TotalBytes: 4 * GiB, AvailableBytes: 2 * GiB})
	reclaim := expectOp(t, m, at, SetMemory)
	if reclaim.Source != SourceIdle || reclaim.MemoryTarget != 0 {
		t.Fatal("pending reclaim did not run as soon as memory was safe", reclaim)
	}
}

func TestLateOldPlanErrorAfterResetDoesNotFinishNewIdlePlan(t *testing.T) {
	oldError := errors.New("old idle source failed")
	for _, tc := range []struct {
		name  string
		kind  Kind
		reset func(*machine)
	}{
		{"inspect backend reset", InspectSession, func(m *machine) {
			m.backend(false, SSHUnknown)
			m.backend(true, SSHZero)
		}},
		{"inspect lock reset", InspectSession, func(m *machine) {
			m.lock(true)
			m.lock(false)
		}},
		{"logoff backend reset", LogoffSession, func(m *machine) {
			m.backend(false, SSHUnknown)
			m.backend(true, SSHZero)
		}},
		{"logoff lock reset", LogoffSession, func(m *machine) {
			m.lock(true)
			m.lock(false)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := idleMachine(0, 300*time.Second)
			displayReady(t, m)
			binding := *m.state.Binding
			at := autoDisconnect(t, m, epoch)
			inspect := expectOp(t, m, at, InspectSession)
			var old *Operation
			if tc.kind == InspectSession {
				old = inspect
			} else {
				disconnected := binding
				disconnected.State = WTSDisconnected
				m.done(inspect.ID, Outcome{Sessions: snapshotAt(at, disconnected)}, at)
				old = expectOp(t, m, at, LogoffSession)
			}

			tc.reset(m)
			m.done(old.ID, Outcome{Err: oldError}, at)
			if m.idleLogoffDone {
				t.Fatal("late old-plan error marked the current idle plan done", m.snapshot())
			}
			if !m.state.LogoffDeadline.IsZero() || !m.idleLogoffRetryAt.IsZero() || !m.idleLogoffTransitionUntil.IsZero() {
				t.Fatal("late old-plan error left stale timers", m.snapshot(), m.idleLogoffRetryAt, m.idleLogoffTransitionUntil)
			}
			if len(m.completed) != 0 {
				t.Fatal("idle operation was retried or completed a caller", m.completed)
			}

			// The new generation must be able to run its own inspection and then
			// its own logoff; the old error must not suppress or replay either.
			fresh := expectOp(t, m, at, InspectSession)
			if fresh.ID == old.ID || fresh.Source != SourceIdle || fresh.Deadline != at.Add(idleLogoffTransitionTimeout) {
				t.Fatal("new idle cycle did not start a fresh inspection", fresh)
			}
			disconnected := binding
			disconnected.State = WTSDisconnected
			m.done(fresh.ID, Outcome{Sessions: snapshotAt(at, disconnected)}, at)
			logoff := expectOp(t, m, at, LogoffSession)
			if logoff.ID == old.ID || logoff.Source != SourceIdle || logoff.Reference != binding.Reference {
				t.Fatal("new idle cycle did not reach logoff", logoff)
			}
			m.done(logoff.ID, Outcome{}, at)
			if !m.idleLogoffDone {
				t.Fatal("new idle plan did not record its confirmed logoff", m.snapshot())
			}
		})
	}
}
