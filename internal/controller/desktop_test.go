package controller

import (
	"testing"
	"time"
)

func TestDesktopStartsWithBackgroundBindingAndNoWindowIdleTimer(t *testing.T) {
	m := testMachine()
	addJob(m, "desktop", ModeDesktop)
	start := expectOp(t, m, epoch, StartDisplay)
	completeStart(t, m, start, bindingLogin(2))
	dispatch := expectOp(t, m, epoch, Dispatch)
	m.done(dispatch.ID, Outcome{Data: "process started"}, epoch)
	if !m.state.Display || m.state.DisplayMode != ModeDesktop || m.state.Binding == nil {
		t.Fatal(m.snapshot())
	}
	m.windows(0)
	expectIdle(t, m, epoch)
	expectIdle(t, m, epoch.Add(10*time.Minute))
	if !m.state.DisplayDeadline.IsZero() || !m.state.SuspendDeadline.IsZero() {
		t.Fatal("desktop acquired an idle timer", m.snapshot())
	}
	// Repeating the same mode reuses the bound connection rather than starting
	// another RDP process.
	addJob(m, "desktop", ModeDesktop)
	dispatch = expectOp(t, m, epoch, Dispatch)
	m.done(dispatch.ID, Outcome{}, epoch)
	if m.state.DisplayMode != ModeDesktop {
		t.Fatal(m.snapshot())
	}
	// A different mode is a conflict and must not silently replace the desktop.
	addJob(m, "run", ModeRemoteApp)
	inspect := expectOp(t, m, epoch, InspectSession)
	m.done(inspect.ID, Outcome{Sessions: activitySnapshot(bindingLogin(2))}, epoch)
	if m.state.DisplayMode != ModeDesktop {
		t.Fatal("old connection changed before consent", m.snapshot())
	}
	confirm := expectOp(t, m, epoch, ConfirmSwitch)
	m.done(confirm.ID, Outcome{Decision: SwitchCancel}, epoch)
	if !m.state.Display || m.state.DisplayMode != ModeDesktop {
		t.Fatal("cancel changed the connection", m.snapshot())
	}
	m.displayExited(start.ID, nil)
	if m.state.Display || m.state.DisplayMode != "" {
		t.Fatal(m.snapshot())
	}
}

func TestDesktopSwitchRequiresConsentAndCancelKeepsOldConnection(t *testing.T) {
	m := testMachine()
	old := displayReady(t, m)
	addJob(m, "desktop", ModeDesktop)
	inspect := expectOp(t, m, epoch, InspectSession)
	if !m.state.Display || m.displayID != old {
		t.Fatal("old connection changed before inspection")
	}
	one := uint32(1)
	login := bindingLogin(2)
	login.Windows = &one
	m.done(inspect.ID, Outcome{Sessions: activitySnapshot(login)}, epoch)
	confirm := expectOp(t, m, epoch, ConfirmSwitch)
	if confirm.Prompt.Allows(SwitchCurrent) {
		t.Fatal("current desktop was offered while the bound connection is RemoteApp", confirm.Prompt)
	}
	m.done(confirm.ID, Outcome{Decision: SwitchCancel}, epoch)
	if !m.state.Display || m.displayID != old || m.switching != nil || len(m.queue) != 0 {
		t.Fatal(m.snapshot())
	}
	if len(m.completed) != 1 || m.completed[0].result.Err != nil {
		t.Fatal(m.completed)
	}
}

func TestEmptySessionKeepsLoginWithoutAutomaticLogoff(t *testing.T) {
	m := testMachine()
	displayReady(t, m)
	addJob(m, "desktop", ModeDesktop)
	inspect := expectOp(t, m, epoch, InspectSession)
	zero := uint32(0)
	login := bindingLogin(2)
	login.Windows, login.TrayIcons = &zero, &zero
	m.done(inspect.ID, Outcome{Sessions: activitySnapshot(login)}, epoch)
	stop := expectOp(t, m, epoch, StopDisplay)
	m.done(stop.ID, Outcome{}, epoch)
	start := expectOp(t, m, epoch, StartDisplay)
	completeStart(t, m, start, bindingLogin(3))
	dispatch := expectOp(t, m, epoch, Dispatch)
	m.done(dispatch.ID, Outcome{}, epoch)
	if m.state.DisplayMode != ModeDesktop || m.switching != nil {
		t.Fatal(m.snapshot())
	}
	for _, item := range m.completed {
		if item.result.Err != nil {
			t.Fatal(item.result.Err)
		}
	}
}

func activitySnapshot(login Login) *SessionSnapshot {
	snapshot := sessionSnapshot(login)
	return &snapshot
}
