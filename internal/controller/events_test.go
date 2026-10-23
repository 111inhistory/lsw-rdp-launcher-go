package controller

import (
	"context"
	"testing"
	"time"
)

func TestDuplicateVMStateDoesNotInvalidateRequest(t *testing.T) {
	m := testMachine()
	backgroundJob(m, "run")
	op := expectOp(t, m, epoch, Dispatch)
	m.vm(VMRunning)
	m.done(op.ID, Outcome{Data: "ok"}, epoch)
	if !m.state.BackendReady || len(m.completed) != 1 || m.completed[0].result.Err != nil {
		t.Fatal(m.snapshot(), m.completed)
	}
}

func TestEarlyDisplayExitCannotBecomeReady(t *testing.T) {
	m := testMachine()
	addJob(m, "run", ModeRemoteApp)
	op := expectOp(t, m, epoch, StartDisplay)
	m.displayExited(op.ID, nil)
	m.done(op.ID, Outcome{}, epoch)
	if m.state.Display || len(m.completed) != 1 || m.completed[0].result.Err == nil {
		t.Fatal(m.snapshot(), m.completed)
	}
}

func TestOldCompletionDoesNotCancelCurrentExecutor(t *testing.T) {
	h := newHarness(t)
	h.notify(t, BackendObserved{true, SSHZero})
	first := h.request(h.ctx, Request{Action: "first"})
	old := h.expect(t, Dispatch)
	h.complete(t, Outcome{})
	if r := h.reply(t, first); r.Err != nil {
		t.Fatal(r)
	}
	second := h.request(h.ctx, Request{Action: "second"})
	current := h.expect(t, Dispatch)
	if err := h.c.send(h.ctx, operationDone{old.ID, Outcome{}}); err != nil {
		t.Fatal(err)
	}
	h.wait(t, func(s Snapshot) bool { return s.Operation == Dispatch })
	h.complete(t, Outcome{Data: current.Request.Action})
	if r := h.reply(t, second); r.Err != nil || r.Data != "second" {
		t.Fatal(r)
	}
}

func TestLifecycleQueuedBeforeStartupIsAppliedFirst(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	operations := make(chan Operation, 1)
	c := New(Options{}, func(_ context.Context, op Operation) Outcome {
		operations <- op
		return Outcome{Data: "ok"}
	})
	if err := c.Notify(ctx, VMObserved{VMRunning}); err != nil {
		t.Fatal(err)
	}
	if err := c.Notify(ctx, BackendObserved{true, SSHZero}); err != nil {
		t.Fatal(err)
	}
	j := &job{ctx: ctx, request: Request{Action: "query"}, reply: make(chan Result, 1)}
	if err := c.send(ctx, j); err != nil {
		t.Fatal(err)
	}
	stopped := make(chan error, 1)
	go func() { stopped <- c.Run(ctx) }()
	defer func() { cancel(); <-stopped }()
	select {
	case op := <-operations:
		if op.Kind != Dispatch {
			t.Fatalf("expected dispatch, got %+v", op)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case r := <-j.reply:
		if r.Err != nil {
			t.Fatal(r)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestRealDeadlineDisconnectsThenPausesWithoutPolling(t *testing.T) {
	h := newHarness(t, Options{IdleDisconnect: 10 * time.Millisecond, VMSuspend: 10 * time.Millisecond, IdleLogoff: -time.Second})
	h.notify(t, BackendObserved{true, SSHZero})
	result := h.request(h.ctx, Request{Action: "run", Mode: ModeRemoteApp, UserContext: true})
	h.expect(t, StartDisplay)
	login := bindingLogin(2)
	snapshot := sessionSnapshot(login)
	h.complete(t, Outcome{Binding: &login, Sessions: &snapshot})
	h.expect(t, Dispatch)
	h.complete(t, Outcome{})
	if r := h.reply(t, result); r.Err != nil {
		t.Fatal(r)
	}
	h.notify(t, WindowsObserved{0})
	h.expect(t, StopDisplay)
	h.complete(t, Outcome{})
	h.expect(t, SuspendVM)
	h.complete(t, Outcome{})
	h.notify(t, VMObserved{VMPaused})
	h.wait(t, func(s Snapshot) bool { return s.VM == VMPaused })
}

func TestRequestDuringSuspendWakesAgain(t *testing.T) {
	m := testMachine()
	expectIdle(t, m, epoch)
	suspend := expectOp(t, m, epoch.Add(5*time.Minute), SuspendVM)
	addJob(m, "run", ModeRemoteApp)
	m.done(suspend.ID, Outcome{}, epoch)
	m.vm(VMPaused)
	expectOp(t, m, epoch.Add(5*time.Minute), WakeVM)
}

func TestDisplayStartedAcrossVMChangeIsCleanedUpWithoutRevivingSessions(t *testing.T) {
	m := testMachine()
	addJob(m, "run", ModeRemoteApp)
	op := expectOp(t, m, epoch, StartDisplay)
	generation := m.sessionGeneration
	m.vm(VMOff)
	completeStart(t, m, op, bindingLogin(2))
	if m.sessionGeneration == generation || m.state.Sessions.Fresh {
		t.Fatal("old start observation revived sessions after VM loss", m.snapshot())
	}
	if !m.state.Display || m.state.Binding != nil {
		t.Fatal("successful process start was not queued for cleanup", m.snapshot())
	}
	if len(m.queue) != 0 || len(m.completed) != 1 || m.completed[0].result.Err == nil || m.completed[0].result.Data != nil {
		t.Fatal("old observation did not fail its caller", m.snapshot(), m.completed)
	}
	op = expectOp(t, m, epoch, StopDisplay)
	m.done(op.ID, Outcome{}, epoch)
	expectIdle(t, m, epoch)
	if m.state.Display || m.state.Sessions.Fresh {
		t.Fatal(m.snapshot())
	}
}
