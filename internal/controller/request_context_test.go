package controller

import (
	"context"
	"strings"
	"testing"
)

func addUserJob(m *machine, action, mode string) *job {
	j := &job{ctx: context.Background(), request: Request{Action: action, Mode: mode, UserContext: true}, reply: make(chan Result, 1)}
	m.enqueue(j)
	return j
}

func TestDisplayModeBatchAndConflictRequireUserContext(t *testing.T) {
	if err := (Request{Action: "ping"}).Validate(); err != nil {
		t.Fatal(err)
	}
	for _, request := range []Request{
		{Action: "list_apps", Mode: ModeRemoteApp},
		{Action: "list_apps", Mode: ModeDesktop},
		{Action: "connect", Mode: ModeDesktop},
		{Action: "run", Target: "app", Choice: SwitchKeep},
		{Action: "batch", Batch: []Request{{Action: "open", File: `C:\a.txt`}}},
	} {
		if err := request.Validate(); err == nil || !strings.Contains(err.Error(), "user context") {
			t.Fatalf("request without user context was accepted: %+v (%v)", request, err)
		}
	}
	if err := (Request{Action: "batch", UserContext: true, Mode: ModeRemoteApp, Batch: []Request{{Action: "open", File: `C:\a.txt`}}}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestNonUserDispatchNeverCarriesRetainedLogonReference(t *testing.T) {
	m := testMachine()
	oldDisplay := displayReady(t, m)
	m.displayExited(oldDisplay, nil)
	if m.state.Display || m.state.Binding == nil {
		t.Fatal("fixture did not retain disconnected binding", m.snapshot())
	}
	m.enqueue(&job{ctx: context.Background(), request: Request{Action: "ping"}, reply: make(chan Result, 1)})
	dispatch := expectOp(t, m, epoch, Dispatch)
	if dispatch.Reference != (SessionRef{}) {
		t.Fatalf("background request carried retained logon reference: %+v", dispatch.Reference)
	}
	m.done(dispatch.ID, Outcome{}, epoch)
}

func TestBoundDesktopModeNeutralQueriesDispatchOnExactLoginWithoutConfirmation(t *testing.T) {
	m := testMachine()
	displayID := displayReadyMode(t, m, ModeDesktop)
	binding := *m.state.Binding
	for _, action := range []string{"list_apps", "get_mapped_drives"} {
		t.Run(action, func(t *testing.T) {
			addUserJob(m, action, "")
			dispatch := expectOp(t, m, epoch, Dispatch)
			if dispatch.Source != SourceUser || dispatch.Reference != binding.Reference {
				t.Fatalf("mode-neutral query did not use the bound desktop login: %+v", dispatch)
			}
			if dispatch.Request.UserContext != true || dispatch.Request.Mode != "" {
				t.Fatal(dispatch.Request)
			}
			m.done(dispatch.ID, Outcome{Data: "ok"}, epoch)
			if !m.state.Display || m.displayID != displayID || m.state.Binding.Reference != binding.Reference {
				t.Fatal("mode-neutral dispatch reconnected or rebound", m.snapshot())
			}
		})
	}
}

func TestModeNeutralWithoutDisplayStartsDefaultRemoteApp(t *testing.T) {
	m := testMachine()
	addUserJob(m, "list_apps", "")
	start := expectOp(t, m, epoch, StartDisplay)
	if start.Source != SourceUser || start.Request.Mode != ModeRemoteApp {
		t.Fatalf("mode-neutral request did not default to RemoteApp: %+v", start)
	}
	login := bindingLogin(2)
	completeStart(t, m, start, login)
	dispatch := expectOp(t, m, epoch, Dispatch)
	if dispatch.Reference != login.Reference || m.state.DisplayMode != ModeRemoteApp {
		t.Fatal("defaulted connection did not bind before dispatch", dispatch, m.snapshot())
	}
	m.done(dispatch.ID, Outcome{}, epoch)
}

func TestModeNeutralAfterDisconnectDoesNotReuseOldLogin(t *testing.T) {
	m := testMachine()
	oldDisplay := displayReady(t, m)
	oldRef := m.state.Binding.Reference
	m.displayExited(oldDisplay, nil)
	if m.state.Display || m.state.Binding == nil || m.state.Binding.Reference != oldRef {
		t.Fatal("fixture did not retain the old binding while disconnected", m.snapshot())
	}
	addUserJob(m, "list_apps", "")
	start := expectOp(t, m, epoch, StartDisplay)
	if start.Request.Mode != ModeRemoteApp {
		t.Fatal("disconnected mode-neutral request did not start the default mode", start)
	}
	newLogin := bindingLogin(6)
	completeStart(t, m, start, newLogin)
	if m.state.Binding.Reference != newLogin.Reference {
		t.Fatal("disconnected login was reused", m.snapshot())
	}
	dispatch := expectOp(t, m, epoch, Dispatch)
	if dispatch.Reference != newLogin.Reference || dispatch.Reference == oldRef {
		t.Fatal("dispatch reused the disconnected login", dispatch.Reference, oldRef, newLogin.Reference)
	}
	m.done(dispatch.ID, Outcome{}, epoch)
}
