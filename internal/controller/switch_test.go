package controller

import (
	"context"
	"errors"
	"testing"
)

func TestSwitchKeepUsesNewLoginReferenceForDispatch(t *testing.T) {
	m := testMachine()
	oldDisplay := displayReady(t, m)
	oldRef := m.state.Binding.Reference
	one := uint32(1)
	oldLogin := bindingLogin(2)
	oldLogin.Windows = &one

	addJob(m, "run", ModeDesktop)
	inspect := expectOp(t, m, epoch, InspectSession)
	if inspect.Source != SourceUser || inspect.Reference != oldRef {
		t.Fatal(inspect)
	}
	m.done(inspect.ID, Outcome{Sessions: activitySnapshot(oldLogin)}, epoch)
	confirm := expectOp(t, m, epoch, ConfirmSwitch)
	if confirm.Source != SourceUser || !confirm.Prompt.Allows(SwitchKeep) || confirm.Prompt.Allows(SwitchCurrent) {
		t.Fatal(confirm)
	}
	m.done(confirm.ID, Outcome{Decision: SwitchKeep}, epoch)

	stop := expectOp(t, m, epoch, StopDisplay)
	if stop.Source != SourceUser || stop.Reference != oldRef || !m.state.Display || m.displayID != oldDisplay {
		t.Fatal(stop, m.snapshot())
	}
	m.done(stop.ID, Outcome{}, epoch)

	start := expectOp(t, m, epoch, StartDisplay)
	if start.Source != SourceUser || start.Request.Action != "run" || start.Request.Mode != ModeDesktop {
		t.Fatal(start)
	}
	// RD and RemoteApp can create two different logons for the same account.
	newLogin := bindingLogin(6)
	completeStart(t, m, start, newLogin)
	dispatch := expectOp(t, m, epoch, Dispatch)
	if dispatch.Reference != newLogin.Reference {
		t.Fatal("dispatch kept the old switch activity instead of the new binding", dispatch.Reference, newLogin.Reference)
	}
	if dispatch.Request.Action != "run" || dispatch.Request.Mode != ModeDesktop {
		t.Fatal("business payload changed during preserve switch", dispatch.Request)
	}
	m.done(dispatch.ID, Outcome{}, epoch)
	if m.state.Binding == nil || m.state.Binding.Reference != newLogin.Reference || m.state.DisplayMode != ModeDesktop || m.switching != nil {
		t.Fatal(m.snapshot())
	}
}

func TestSwitchLogoffUsesOldLogonThenDispatchesNewBinding(t *testing.T) {
	m := testMachine()
	displayReady(t, m)
	oldRef := m.state.Binding.Reference
	one := uint32(1)
	oldLogin := bindingLogin(2)
	oldLogin.Windows = &one

	addJob(m, "run", ModeDesktop)
	inspect := expectOp(t, m, epoch, InspectSession)
	m.done(inspect.ID, Outcome{Sessions: activitySnapshot(oldLogin)}, epoch)
	confirm := expectOp(t, m, epoch, ConfirmSwitch)
	m.done(confirm.ID, Outcome{Decision: SwitchLogoff}, epoch)

	stop := expectOp(t, m, epoch, StopDisplay)
	m.done(stop.ID, Outcome{}, epoch)
	logoff := expectOp(t, m, epoch, LogoffSession)
	if logoff.Source != SourceUser || logoff.Reference != oldRef || logoff.Activity.Reference != oldRef || logoff.Activity.SID != oldLogin.SID {
		t.Fatal("logoff did not target the confirmed old logon", logoff)
	}
	m.done(logoff.ID, Outcome{}, epoch)

	start := expectOp(t, m, epoch, StartDisplay)
	newLogin := bindingLogin(6)
	completeStart(t, m, start, newLogin)
	dispatch := expectOp(t, m, epoch, Dispatch)
	if dispatch.Reference != newLogin.Reference {
		t.Fatal("dispatch used the logged-off reference", dispatch.Reference, newLogin.Reference)
	}
	m.done(dispatch.ID, Outcome{}, epoch)
	if m.state.Binding.Reference != newLogin.Reference || m.switching != nil {
		t.Fatal(m.snapshot())
	}
}

func TestSwitchCancelKeepsOriginalConnection(t *testing.T) {
	m := testMachine()
	oldDisplay := displayReady(t, m)
	one := uint32(1)
	login := bindingLogin(2)
	login.Windows = &one
	addJob(m, "run", ModeDesktop)
	inspect := expectOp(t, m, epoch, InspectSession)
	m.done(inspect.ID, Outcome{Sessions: activitySnapshot(login)}, epoch)
	confirm := expectOp(t, m, epoch, ConfirmSwitch)
	m.done(confirm.ID, Outcome{Decision: SwitchCancel}, epoch)
	expectIdle(t, m, epoch)
	if !m.state.Display || m.displayID != oldDisplay || m.switching != nil || len(m.queue) != 0 {
		t.Fatal(m.snapshot())
	}
	if len(m.completed) != 1 || m.completed[0].result.Err != nil {
		t.Fatal(m.completed)
	}
}

func TestSwitchRejectsExpiredOrChangedIdentityBeforeDestructiveSteps(t *testing.T) {
	t.Run("changed during confirmation", func(t *testing.T) {
		m := testMachine()
		oldDisplay := displayReady(t, m)
		one := uint32(1)
		login := bindingLogin(2)
		login.Windows = &one
		addJob(m, "run", ModeDesktop)
		inspect := expectOp(t, m, epoch, InspectSession)
		m.done(inspect.ID, Outcome{Sessions: activitySnapshot(login)}, epoch)
		confirm := expectOp(t, m, epoch, ConfirmSwitch)
		changed := login
		changed.ConnectTime++
		m.sessions(sessionSnapshot(changed))
		m.done(confirm.ID, Outcome{Decision: SwitchLogoff}, epoch)
		expectIdle(t, m, epoch)
		if !m.state.Display || m.displayID != oldDisplay || m.switching != nil || m.state.Binding.Reference != login.Reference {
			t.Fatal(m.snapshot())
		}
		if len(m.completed) != 1 || m.completed[0].result.Err == nil {
			t.Fatal(m.completed)
		}
	})

	t.Run("stale snapshot during inspection", func(t *testing.T) {
		m := testMachine()
		oldDisplay := displayReady(t, m)
		login := bindingLogin(2)
		snapshot := sessionSnapshot(login)
		snapshot.Fresh = false
		addJob(m, "run", ModeDesktop)
		inspect := expectOp(t, m, epoch, InspectSession)
		m.done(inspect.ID, Outcome{Sessions: &snapshot}, epoch)
		expectIdle(t, m, epoch)
		if !m.state.Display || m.displayID != oldDisplay || m.switching != nil {
			t.Fatal(m.snapshot())
		}
		if len(m.completed) != 1 || m.completed[0].result.Err == nil {
			t.Fatal(m.completed)
		}
	})
}

func TestSwitchObservationFailurePreservesConnection(t *testing.T) {
	m := testMachine()
	oldDisplay := displayReady(t, m)
	addJob(m, "run", ModeDesktop)
	inspect := expectOp(t, m, epoch, InspectSession)
	m.done(inspect.ID, Outcome{Err: errors.New("source unavailable")}, epoch)
	expectIdle(t, m, epoch)
	if !m.state.Display || m.displayID != oldDisplay || m.switching != nil {
		t.Fatal(m.snapshot())
	}
	if len(m.completed) != 1 || m.completed[0].result.Err == nil {
		t.Fatal(m.completed)
	}
}

func TestCanceledSwitchDoesNotContinueAfterObservation(t *testing.T) {
	m := testMachine()
	oldDisplay := displayReady(t, m)
	login := bindingLogin(2)
	addJob(m, "run", ModeDesktop)
	inspect := expectOp(t, m, epoch, InspectSession)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m.queue[0].ctx = ctx
	m.done(inspect.ID, Outcome{Sessions: activitySnapshot(login)}, epoch)
	expectIdle(t, m, epoch)
	if !m.state.Display || m.displayID != oldDisplay || m.switching != nil || len(m.queue) != 0 {
		t.Fatal(m.snapshot())
	}
	if len(m.completed) != 1 || !errors.Is(m.completed[0].result.Err, context.Canceled) {
		t.Fatal(m.completed)
	}
}

func TestSwitchLogoffFailureDoesNotConnectAnotherMode(t *testing.T) {
	m := testMachine()
	displayReady(t, m)
	one := uint32(1)
	login := bindingLogin(2)
	login.Windows = &one
	addJob(m, "run", ModeDesktop)
	inspect := expectOp(t, m, epoch, InspectSession)
	m.done(inspect.ID, Outcome{Sessions: activitySnapshot(login)}, epoch)
	confirm := expectOp(t, m, epoch, ConfirmSwitch)
	m.done(confirm.ID, Outcome{Decision: SwitchLogoff}, epoch)
	stop := expectOp(t, m, epoch, StopDisplay)
	m.done(stop.ID, Outcome{}, epoch)
	logoff := expectOp(t, m, epoch, LogoffSession)
	m.done(logoff.ID, Outcome{Err: errors.New("activity changed")}, epoch)
	expectIdle(t, m, epoch)
	if m.state.Display || m.switching != nil || len(m.queue) != 0 {
		t.Fatal(m.snapshot())
	}
	if len(m.completed) != 1 || m.completed[0].result.Err == nil {
		t.Fatal(m.completed)
	}
}

func TestCurrentDesktopChoicePreservesPayloadAndSkipsReconnect(t *testing.T) {
	m := testMachine()
	displayID := displayReadyMode(t, m, ModeDesktop)
	binding := *m.state.Binding
	one := uint32(1)
	login := bindingLogin(2)
	login.Windows = &one
	m.sessions(sessionSnapshot(login))

	j := &job{
		ctx: context.Background(),
		request: Request{
			Action:      "open",
			Target:      `C:\Users\user\report.txt`,
			Params:      "--read-only",
			WorkingDir:  `C:\Users\user`,
			Mode:        ModeRemoteApp,
			UserContext: true,
		},
		reply: make(chan Result, 1),
	}
	m.enqueue(j)
	inspect := expectOp(t, m, epoch, InspectSession)
	m.done(inspect.ID, Outcome{Sessions: activitySnapshot(login)}, epoch)
	confirm := expectOp(t, m, epoch, ConfirmSwitch)
	if !confirm.Prompt.Allows(SwitchCurrent) || !confirm.Prompt.Allows(SwitchKeep) || !confirm.Prompt.Allows(SwitchLogoff) {
		t.Fatal("current desktop action was not offered with the other choices", confirm.Prompt)
	}
	m.done(confirm.ID, Outcome{Decision: SwitchCurrent}, epoch)

	dispatch := expectOp(t, m, epoch, Dispatch)
	if dispatch.Source != SourceUser || dispatch.Reference != binding.Reference {
		t.Fatal("current desktop changed the bound logon", dispatch.Reference, binding.Reference)
	}
	if dispatch.Request.Action != "open" || dispatch.Request.Target != j.request.Target || dispatch.Request.Params != j.request.Params || dispatch.Request.WorkingDir != j.request.WorkingDir {
		t.Fatal("current desktop lost the original business payload", dispatch.Request)
	}
	if dispatch.Request.Mode != ModeDesktop {
		t.Fatal("current desktop did not select desktop mode", dispatch.Request.Mode)
	}
	if !m.state.Display || m.displayID != displayID || m.state.Binding.Reference != binding.Reference {
		t.Fatal("current desktop reconnected", m.snapshot())
	}
	m.done(dispatch.ID, Outcome{Data: "opened"}, epoch)
	if m.switching != nil || m.state.DisplayMode != ModeDesktop {
		t.Fatal(m.snapshot())
	}
}

func TestConnectRequestDoesNotOfferCurrentDesktop(t *testing.T) {
	m := testMachine()
	displayReadyMode(t, m, ModeDesktop)
	login := bindingLogin(2)
	addJob(m, "connect", ModeRemoteApp)
	inspect := expectOp(t, m, epoch, InspectSession)
	m.done(inspect.ID, Outcome{Sessions: activitySnapshot(login)}, epoch)
	confirm := expectOp(t, m, epoch, ConfirmSwitch)
	if confirm.Prompt.Allows(SwitchCurrent) {
		t.Fatal("connect request offered to run in the current desktop", confirm.Prompt)
	}
	m.done(confirm.ID, Outcome{Decision: SwitchCancel}, epoch)
}

func TestDesktopRunOrOpenWithEmptyActivityStillConfirmsWithCurrent(t *testing.T) {
	for _, action := range []string{"run", "open"} {
		t.Run(action, func(t *testing.T) {
			m := testMachine()
			oldDisplay := displayReadyMode(t, m, ModeDesktop)
			binding := *m.state.Binding
			zero := uint32(0)
			login := bindingLogin(2)
			login.Windows, login.TrayIcons = &zero, &zero
			addJob(m, action, ModeRemoteApp)
			inspect := expectOp(t, m, epoch, InspectSession)
			m.done(inspect.ID, Outcome{Sessions: activitySnapshot(login)}, epoch)
			confirm := expectOp(t, m, epoch, ConfirmSwitch)
			if !confirm.Prompt.Activity.Empty() || !confirm.Prompt.Allows(SwitchCurrent) {
				t.Fatal("empty desktop app request skipped confirmation/current", confirm.Prompt)
			}
			m.done(confirm.ID, Outcome{Decision: SwitchCurrent}, epoch)
			dispatch := expectOp(t, m, epoch, Dispatch)
			if dispatch.Reference != binding.Reference || m.displayID != oldDisplay {
				t.Fatal("current desktop choice did not keep the exact bound login", dispatch, m.snapshot())
			}
			m.done(dispatch.ID, Outcome{}, epoch)
		})
	}
}
