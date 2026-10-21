package controller

import (
	"testing"
	"time"
)

func TestDesktopDoesNotNeedInteractiveAgentOrWindowIdleTimer(t *testing.T) {
	m := testMachine()
	m.backend(false, SSHUnknown)
	addJob(m, "desktop", Desktop)
	start := expectOp(t, m, epoch, StartDisplay)
	m.done(start.ID, Outcome{}, epoch)
	dispatch := expectOp(t, m, epoch, Dispatch)
	m.done(dispatch.ID, Outcome{Data: "process started"}, epoch)
	if !m.state.Display || m.state.DisplayMode != "desktop" || m.state.BackendReady {
		t.Fatal(m.snapshot())
	}
	m.windows(0)
	expectIdle(t, m, epoch)
	expectIdle(t, m, epoch.Add(10*time.Minute))
	if !m.state.DisplayDeadline.IsZero() || !m.state.SuspendDeadline.IsZero() {
		t.Fatal(m.snapshot())
	}
	// Repeating the request reuses the process rather than starting another RDP
	// connection. An Interactive request must not assume a desktop started its agent.
	addJob(m, "desktop", Desktop)
	dispatch = expectOp(t, m, epoch, Dispatch)
	m.done(dispatch.ID, Outcome{}, epoch)
	addJob(m, "run", Interactive)
	expectIdle(t, m, epoch)
	if m.completed[len(m.completed)-1].result.Err == nil || m.state.DisplayMode != "desktop" {
		t.Fatal(m.snapshot(), m.completed)
	}
	m.displayExited(start.ID, nil)
	if m.state.Display || m.state.DisplayMode != "" {
		t.Fatal(m.snapshot())
	}
}

func TestDesktopReplacementRequiresExplicitPermission(t *testing.T) {
	m := testMachine()
	old := displayReady(t, m)
	addJob(m, "desktop", Desktop)
	expectIdle(t, m, epoch)
	if len(m.completed) != 1 || m.completed[0].result.Err == nil || m.state.DisplayMode != "remoteapp" {
		t.Fatal(m.snapshot(), m.completed)
	}
	j := addJob(m, "desktop", Desktop)
	j.request.SwitchDisplay = true
	inspect := expectOp(t, m, epoch, InspectSession)
	zero := uint32(0)
	m.done(inspect.ID, Outcome{Activity: &SessionActivity{Windows: &zero, TrayIcons: &zero}}, epoch)
	stop := expectOp(t, m, epoch, StopDisplay)
	m.done(stop.ID, Outcome{}, epoch)
	start := expectOp(t, m, epoch, StartDisplay)
	m.done(start.ID, Outcome{}, epoch)
	m.displayExited(old, nil)
	if !m.state.Display || m.state.DisplayMode != "desktop" {
		t.Fatal(m.snapshot())
	}
	dispatch := expectOp(t, m, epoch, Dispatch)
	m.done(dispatch.ID, Outcome{}, epoch)
}
