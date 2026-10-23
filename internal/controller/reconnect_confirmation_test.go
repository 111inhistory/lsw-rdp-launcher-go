package controller

import (
	"strings"
	"testing"
)

func TestSourceLossReleasesAcceptedVMCommandWithoutReplayingIt(t *testing.T) {
	for _, kind := range []Kind{WakeVM, SuspendVM} {
		t.Run(string(kind), func(t *testing.T) {
			m := testMachine()
			now := epoch
			if kind == WakeVM {
				m.vm(VMPaused)
				backgroundJob(m, "original")
			} else {
				expectIdle(t, m, now)
				now = now.Add(m.opts.VMSuspend)
			}
			op := expectOp(t, m, now, kind)
			m.done(op.ID, Outcome{}, now)
			if m.busy == nil || !m.busy.accepted {
				t.Fatal("command did not wait for confirmation")
			}
			tail := backgroundJob(m, "later")
			m.vmSource(false)
			if m.busy != nil || m.state.VM != VMUnknown {
				t.Fatal("lost confirmation still owns control slot", m.snapshot())
			}
			if op.job != nil && (len(m.completed) != 1 || !strings.Contains(m.completed[0].result.Err.Error(), "outcome unknown")) {
				t.Fatal("original caller did not receive unknown outcome", m.completed)
			}
			if len(m.queue) != 1 || m.queue[0] != tail {
				t.Fatal("source loss completed an unrelated caller")
			}
			expectIdle(t, m, now)
			m.vmSource(true)
			query := expectOp(t, m, now, QueryVM)
			m.done(op.ID, Outcome{}, now) // Late duplicate cannot release the new query.
			if m.busy == nil || m.busy.ID != query.ID {
				t.Fatal("old acknowledgement affected replacement query")
			}
			m.done(query.ID, Outcome{VM: VMRunning}, now)
			m.backend(true, SSHZero)
			if dispatch := expectOp(t, m, now, Dispatch); dispatch.job != tail {
				t.Fatal("original command was replayed")
			}
		})
	}
}

func TestLateVMCommandAckAfterReconnectDoesNotWaitForLostConfirmation(t *testing.T) {
	m := testMachine()
	m.vm(VMPaused)
	backgroundJob(m, "original")
	op := expectOp(t, m, epoch, WakeVM)
	m.vmSource(false)
	m.vmSource(true)
	m.done(op.ID, Outcome{}, epoch)
	if m.busy != nil || len(m.completed) != 1 || m.completed[0].result.Err == nil {
		t.Fatal("late ACK waits for a confirmation from the old source", m.snapshot())
	}
	expectOp(t, m, epoch, QueryVM)
}

func TestConfirmationTimeoutPreservesPendingSubscriptionSnapshot(t *testing.T) {
	m := testMachine()
	expectIdle(t, m, epoch)
	now := epoch.Add(m.opts.VMSuspend)
	op := expectOp(t, m, now, SuspendVM)
	m.done(op.ID, Outcome{}, now)
	m.vmSource(true) // A post-ACK snapshot is pending behind the accepted command.
	now = now.Add(m.opts.VMConfirmTimeout)
	expectIdle(t, m, now) // Expire the confirmation slot.
	expectOp(t, m, now, QueryVM)
}
