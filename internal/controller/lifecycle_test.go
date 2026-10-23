package controller

import (
	"testing"
	"time"
)

func idleMachine(idleLogoff, suspend time.Duration) *machine {
	m := newMachine(Options{IdleDisconnect: 30 * time.Second, VMSuspend: suspend, IdleLogoff: idleLogoff})
	m.vm(VMRunning)
	m.backend(true, SSHZero)
	return m
}

// autoDisconnect completes the automatic RemoteApp window-idle disconnect and
// returns the moment the display became disconnected.
func autoDisconnect(t *testing.T, m *machine, now time.Time) time.Time {
	t.Helper()
	m.windows(0)
	expectIdle(t, m, now)
	at := now.Add(30 * time.Second)
	stop := expectOp(t, m, at, StopDisplay)
	if stop.Source != SourceIdle || stop.job != nil {
		t.Fatalf("automatic disconnect was not idle-sourced: %+v", stop)
	}
	m.done(stop.ID, Outcome{}, at)
	return at
}

func snapshotAt(now time.Time, logins ...Login) *SessionSnapshot {
	return &SessionSnapshot{Revision: 1, Fresh: true, Sessions: logins, ObservedAt: now}
}

func inspectAndLogoff(t *testing.T, m *machine, binding Login, now time.Time) *Operation {
	t.Helper()
	inspect := expectOp(t, m, now, InspectSession)
	if inspect.Source != SourceIdle {
		t.Fatalf("idle session inspection had source %s", inspect.Source)
	}
	disconnected := binding
	disconnected.State = 4 // WTSDisconnected, the retained logon after display exit.
	m.done(inspect.ID, Outcome{Sessions: snapshotAt(now, disconnected)}, now)
	logoff := expectOp(t, m, now, LogoffSession)
	if logoff.Source != SourceIdle || logoff.Reference != binding.Reference {
		t.Fatalf("idle logoff did not target the bound disconnected logon: %+v", logoff)
	}
	m.done(logoff.ID, Outcome{}, now)
	return logoff
}

func TestIdleLogoffDefaultDisabledRetainsLogin(t *testing.T) {
	m := idleMachine(-time.Second, 300*time.Second)
	displayReady(t, m)
	at := autoDisconnect(t, m, epoch)
	expectIdle(t, m, at)
	expectIdle(t, m, at.Add(299*time.Second))
	suspend := expectOp(t, m, at.Add(300*time.Second), SuspendVM)
	if suspend.Source != SourceIdle {
		t.Fatal(suspend)
	}
	if m.state.LogoffDeadline != (time.Time{}) {
		t.Fatal("disabled idle logoff armed a deadline", m.snapshot())
	}
}

func TestIdleLogoffZeroRunsAfterDisconnectBeforeSuspend(t *testing.T) {
	m := idleMachine(0, 300*time.Second)
	displayReady(t, m)
	binding := *m.state.Binding
	at := autoDisconnect(t, m, epoch)
	inspectAndLogoff(t, m, binding, at)
	expectIdle(t, m, at)
	expectIdle(t, m, at.Add(299*time.Second))
	suspend := expectOp(t, m, at.Add(300*time.Second), SuspendVM)
	if suspend.Source != SourceIdle {
		t.Fatal(suspend)
	}
}

func TestIdleLogoffEqualSuspendDeadlineLogsOffBeforeSuspend(t *testing.T) {
	m := idleMachine(300*time.Second, 300*time.Second)
	displayReady(t, m)
	binding := *m.state.Binding
	at := autoDisconnect(t, m, epoch)
	expectIdle(t, m, at)
	expectIdle(t, m, at.Add(299*time.Second))
	inspectAndLogoff(t, m, binding, at.Add(300*time.Second))
	suspend := expectOp(t, m, at.Add(300*time.Second), SuspendVM)
	if suspend.Source != SourceIdle {
		t.Fatal("suspend did not follow confirmed logoff at the shared deadline", suspend)
	}
}

func TestIdleLogoffReclaimsImmediatelyAfterConfirmedLogoff(t *testing.T) {
	m := memoryMachine(t)
	m.opts.IdleDisconnect = 30 * time.Second
	m.opts.IdleLogoff = 0
	m.opts.VMSuspend = 300 * time.Second
	displayReady(t, m)
	binding := *m.state.Binding
	at := autoDisconnect(t, m, epoch)
	inspectAndLogoff(t, m, binding, at)
	reclaim := expectOp(t, m, at, SetMemory)
	if reclaim.Source != SourceIdle || reclaim.MemoryTarget != 0 {
		t.Fatalf("memory reclaim did not follow confirmed logoff: %+v", reclaim)
	}
	m.done(reclaim.ID, Outcome{}, at)
	expectIdle(t, m, at.Add(299*time.Second))
	expectOp(t, m, at.Add(300*time.Second), SuspendVM)
}

func TestLockBlocksAutomaticLogoffAndReclaim(t *testing.T) {
	m := memoryMachine(t)
	m.opts.IdleDisconnect = 30 * time.Second
	m.opts.IdleLogoff = 0
	m.opts.VMSuspend = 300 * time.Second
	displayReady(t, m)
	m.windows(0)
	m.lock(true)
	expectIdle(t, m, epoch.Add(time.Hour))
	m.displayExited(m.displayID, nil)
	expectIdle(t, m, epoch.Add(2*time.Hour))
	if !m.state.LogoffDeadline.IsZero() || !m.state.SuspendDeadline.IsZero() {
		t.Fatal("lock left an automatic deadline armed", m.snapshot())
	}
	if m.memory.Reclaiming || m.memory.Awaiting {
		t.Fatal("lock allowed an automatic memory reclaim", m.snapshot())
	}
	m.lock(false)
	inspect := expectOp(t, m, epoch.Add(3*time.Hour), InspectSession)
	if inspect.Source != SourceIdle {
		t.Fatal(inspect)
	}
	login := *m.state.Binding
	login.State = 4
	m.done(inspect.ID, Outcome{Sessions: snapshotAt(epoch.Add(3*time.Hour), login)}, epoch.Add(3*time.Hour))
	logoff := expectOp(t, m, epoch.Add(3*time.Hour), LogoffSession)
	m.done(logoff.ID, Outcome{}, epoch.Add(3*time.Hour))
	reclaim := expectOp(t, m, epoch.Add(3*time.Hour), SetMemory)
	if reclaim.Source != SourceIdle || reclaim.MemoryTarget != 0 {
		t.Fatal(reclaim)
	}
}

func TestIdleLogoffSkipsMissingOrActiveBoundLogin(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Login)
	}{
		{"missing", func(l *Login) { l.Reference.SessionID = 9 }},
		{"active elsewhere", func(l *Login) { l.State = 0 }},
		{"different sid", func(l *Login) { l.SID = "S-1-5-21-999" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := idleMachine(0, 300*time.Second)
			displayReady(t, m)
			binding := *m.state.Binding
			at := autoDisconnect(t, m, epoch)
			inspect := expectOp(t, m, at, InspectSession)
			login := binding
			login.State = 4
			tc.mutate(&login)
			m.done(inspect.ID, Outcome{Sessions: snapshotAt(at, login)}, at)
			expectIdle(t, m, at)
			expectIdle(t, m, at.Add(299*time.Second))
			expectOp(t, m, at.Add(300*time.Second), SuspendVM)
			if m.state.LogoffDeadline != (time.Time{}) {
				t.Fatal("unsafe logoff target kept a deadline", m.snapshot())
			}
		})
	}
}

func TestStartDisplayRequiresVerifiedBinding(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  func() Outcome
	}{
		{"missing binding", func() Outcome { return Outcome{} }},
		{"invalid reference", func() Outcome {
			login := bindingLogin(2)
			login.Reference.AuthenticationID = ""
			return Outcome{Binding: &login}
		}},
		{"missing sid", func() Outcome {
			login := bindingLogin(2)
			login.SID = ""
			return Outcome{Binding: &login}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := testMachine()
			addJob(m, "run", ModeRemoteApp)
			start := expectOp(t, m, epoch, StartDisplay)
			m.done(start.ID, tc.out(), epoch)
			expectIdle(t, m, epoch)
			if m.state.Display || m.state.Binding != nil || len(m.queue) != 0 {
				t.Fatal(m.snapshot())
			}
			if len(m.completed) != 1 || m.completed[0].result.Err == nil {
				t.Fatal(m.completed)
			}
		})
	}
}

func TestDispatchRejectsStaleOrMismatchedBinding(t *testing.T) {
	for _, tc := range []struct {
		name     string
		snapshot func(Login) *SessionSnapshot
	}{
		{"no snapshot", func(Login) *SessionSnapshot { return nil }},
		{"stale snapshot", func(login Login) *SessionSnapshot {
			snapshot := sessionSnapshot(login)
			snapshot.Fresh = false
			return &snapshot
		}},
		{"different sid", func(login Login) *SessionSnapshot {
			login.SID = "S-1-5-21-999"
			return activitySnapshot(login)
		}},
		{"different connection", func(login Login) *SessionSnapshot {
			login.ConnectTime++
			return activitySnapshot(login)
		}},
		{"disconnected login", func(login Login) *SessionSnapshot {
			login.State = 4
			return activitySnapshot(login)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := testMachine()
			addJob(m, "run", ModeRemoteApp)
			start := expectOp(t, m, epoch, StartDisplay)
			login := bindingLogin(2)
			out := Outcome{Binding: &login, Sessions: tc.snapshot(login)}
			m.done(start.ID, out, epoch)
			expectIdle(t, m, epoch)
			if len(m.queue) != 0 || len(m.completed) != 1 || m.completed[0].result.Err == nil {
				t.Fatal("unverified binding dispatched an application", m.snapshot(), m.completed)
			}
			if !m.state.Display || m.state.Binding == nil {
				t.Fatal("display bookkeeping did not retain the confirmed binding for cleanup/status", m.snapshot())
			}
		})
	}
}

func TestDesktopWaitsForBackgroundBeforeStartingDisplay(t *testing.T) {
	m := testMachine()
	m.backend(false, SSHUnknown)
	addJob(m, "desktop", ModeDesktop)
	expectIdle(t, m, epoch)
	m.backend(true, SSHUnknown)
	start := expectOp(t, m, epoch, StartDisplay)
	completeStart(t, m, start, bindingLogin(2))
	dispatch := expectOp(t, m, epoch, Dispatch)
	m.done(dispatch.ID, Outcome{}, epoch)
	if m.state.DisplayMode != ModeDesktop {
		t.Fatal(m.snapshot())
	}
}

func TestBackendLossInvalidatesBoundDispatchUntilFreshSnapshot(t *testing.T) {
	m := testMachine()
	displayReady(t, m)
	binding := *m.state.Binding
	m.backend(false, SSHUnknown)
	addJob(m, "run", ModeRemoteApp)
	expectIdle(t, m, epoch)
	m.backend(true, SSHUnknown)
	expectIdle(t, m, epoch)
	if !m.state.Display || m.state.Binding == nil || m.state.Binding.Reference != binding.Reference {
		t.Fatal("connection bookkeeping changed on source loss", m.snapshot())
	}
	if len(m.queue) != 0 || len(m.completed) != 1 || m.completed[0].result.Err == nil {
		t.Fatal("stale snapshot allowed dispatch after backend loss", m.snapshot(), m.completed)
	}
}
