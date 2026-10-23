package controller

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestRequestFailureDoesNotDisableIdleManagement(t *testing.T) {
	for _, failure := range []error{errors.New("agent business error"), context.Canceled} {
		for _, display := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/display=%t", failure, display), func(t *testing.T) {
				m := testMachine()
				mode := ""
				if display {
					displayReady(t, m)
					m.windows(0)
					mode = ModeRemoteApp
				}
				j := addJob(m, "failing", mode)
				op := expectOp(t, m, epoch, Dispatch)
				m.done(op.ID, Outcome{Err: failure}, epoch)
				if len(m.completed) != 1 || m.completed[0].job != j || !errors.Is(m.completed[0].result.Err, failure) {
					t.Fatal(m.completed)
				}
				m.backend(true, SSHActive)
				m.backend(true, SSHZero)
				expectIdle(t, m, epoch)
				wait, want := 5*time.Minute, SuspendVM
				if display {
					wait, want = 30*time.Second, StopDisplay
				}
				expectOp(t, m, epoch.Add(wait), want)
			})
		}
	}
}

func TestQueuedCancellationStillStartsIdleDeadline(t *testing.T) {
	m := testMachine()
	j := addJob(m, "not-started", "")
	ctx, cancel := context.WithCancel(context.Background())
	j.ctx = ctx
	cancel()
	expectIdle(t, m, epoch)
	if m.autoFailed || m.state.SuspendDeadline.IsZero() {
		t.Fatal(m.snapshot(), m.autoFailed)
	}
}

func TestAutonomousFailureDoesNotBecomeRetryLoop(t *testing.T) {
	m := testMachine()
	displayReady(t, m)
	m.windows(0)
	expectIdle(t, m, epoch)
	op := expectOp(t, m, epoch.Add(30*time.Second), StopDisplay)
	m.done(op.ID, Outcome{Err: errors.New("stop failed")}, epoch.Add(30*time.Second))
	expectIdle(t, m, epoch.Add(time.Hour))
	if !m.autoFailed {
		t.Fatal("automatic error must suppress immediate retry")
	}
}

func TestRequestedStopFailureGetsExactlyOneAutonomousRetry(t *testing.T) {
	m := testMachine()
	displayReady(t, m)
	m.cleanupDisplay = true
	j := backgroundJob(m, "ping")
	stop := expectOp(t, m, epoch, StopDisplay)
	if stop.job != j {
		t.Fatal("first stop was not request-associated")
	}
	m.done(stop.ID, Outcome{Err: errors.New("requested stop failed")}, epoch)
	if m.autoFailed {
		t.Fatal("request failure latched automatic management")
	}
	retry := expectOp(t, m, epoch, StopDisplay)
	if retry.job != nil {
		t.Fatal("retry must be autonomous")
	}
	m.done(retry.ID, Outcome{Err: errors.New("automatic stop failed")}, epoch)
	expectIdle(t, m, epoch.Add(time.Hour))
	if !m.autoFailed || len(m.queue) != 0 {
		t.Fatal(m.snapshot())
	}
}

func TestFinishRejectsAnotherJobsCompletion(t *testing.T) {
	m := testMachine()
	first := backgroundJob(m, "first")
	second := backgroundJob(m, "second")
	defer func() {
		if recover() == nil {
			t.Fatal("completion for non-head did not reject invariant violation")
		}
		if len(m.queue) != 2 || m.queue[0] != first || len(m.completed) != 0 {
			t.Fatal("wrong caller was completed")
		}
	}()
	m.finish(second, Result{})
}
