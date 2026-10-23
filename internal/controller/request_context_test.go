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

func TestDisplayModeAndConflictRequireUserContext(t *testing.T) {
	if err := (Request{Action: "list_apps"}).Validate(); err != nil {
		t.Fatal(err)
	}
	for _, request := range []Request{
		{Action: "list_apps", Mode: ModeRemoteApp},
		{Action: "list_apps", Mode: ModeDesktop},
		{Action: "run", Target: "app", Mode: ModeRemoteApp},
	} {
		if err := request.Validate(); err == nil || !strings.Contains(err.Error(), "user context") {
			t.Fatalf("display mode without user context was accepted: %+v (%v)", request, err)
		}
	}
	if err := (Request{Action: "list_apps", Mode: ModeRemoteApp, UserContext: true}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (Request{Action: "run", Target: "app", UserContext: true, Choice: SwitchKeep}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (Request{Action: "run", Target: "app", Choice: SwitchKeep}).Validate(); err == nil {
		t.Fatal("conflict action without user context was accepted")
	}
}

func TestBatchUserContextBelongsToOuterRequestOnly(t *testing.T) {
	batch := Request{
		Action:      "batch",
		UserContext: true,
		Mode:        ModeRemoteApp,
		Batch:       []Request{{Action: "open", Target: `C:\a.txt`}, {Action: "open", Target: `C:\b.txt`}},
	}
	if err := batch.Validate(); err != nil {
		t.Fatal(err)
	}
	batch.Batch[0].UserContext = true
	if err := batch.Validate(); err == nil {
		t.Fatal("batch item user context was accepted")
	}
	batch.Batch[0].UserContext = false
	batch.UserContext = false
	if err := batch.Validate(); err == nil {
		t.Fatal("batch without outer user context was accepted")
	}
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
