package controller

import (
	"context"
	"errors"
	"testing"
)

func switchActivity(windows, tray *uint32) *SessionActivity {
	return &SessionActivity{SessionID: 2, User: "user", Fingerprint: "logon-2", Windows: windows, TrayIcons: tray}
}
func TestCheckedSwitchPreserveLogoffCancelAndUnknown(t *testing.T) {
	zero, one := uint32(0), uint32(1)
	for _, tc := range []struct {
		name       string
		activity   *SessionActivity
		decision   SwitchDecision
		wantLogoff bool
	}{
		{"empty automatic logoff", switchActivity(&zero, &zero), "", true},
		{"windows preserve", switchActivity(&one, &zero), SwitchKeep, false},
		{"tray explicit logoff", switchActivity(&zero, &one), SwitchLogoff, true},
		{"unknown cancel", switchActivity(&zero, nil), SwitchCancel, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := testMachine()
			old := displayReady(t, m)
			j := addJob(m, "connect", Desktop)
			j.request.SwitchDisplay = true
			op := expectOp(t, m, epoch, InspectSession)
			m.done(op.ID, Outcome{Activity: tc.activity}, epoch)
			if !tc.activity.Empty() {
				op = expectOp(t, m, epoch, ConfirmSwitch)
				if !m.state.Display || m.displayID != old {
					t.Fatal("old connection changed before consent")
				}
				m.done(op.ID, Outcome{Decision: tc.decision}, epoch)
				if tc.decision == SwitchCancel {
					expectIdle(t, m, epoch)
					if !m.state.Display || m.displayID != old || m.switching != nil {
						t.Fatal(m.snapshot())
					}
					return
				}
			}
			op = expectOp(t, m, epoch, StopDisplay)
			m.done(op.ID, Outcome{}, epoch)
			if tc.wantLogoff {
				op = expectOp(t, m, epoch, LogoffSession)
				if op.Activity.SessionID != 2 || op.Activity.Fingerprint != "logon-2" || op.RequireEmpty != tc.activity.Empty() {
					t.Fatal(op)
				}
				m.done(op.ID, Outcome{}, epoch)
			}
			op = expectOp(t, m, epoch, StartDisplay)
			m.done(op.ID, Outcome{}, epoch)
			op = expectOp(t, m, epoch, Dispatch)
			m.done(op.ID, Outcome{}, epoch)
			if m.switching != nil || m.state.DisplayMode != "desktop" {
				t.Fatal(m.snapshot())
			}
		})
	}
}
func TestSwitchObservationFailureAndCancellationPreserveConnection(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		m := testMachine()
		old := displayReady(t, m)
		j := addJob(m, "connect", Desktop)
		j.request.SwitchDisplay = true
		op := expectOp(t, m, epoch, InspectSession)
		if cancel {
			ctx, stop := context.WithCancel(context.Background())
			j.ctx = ctx
			stop()
			m.done(op.ID, Outcome{Activity: &SessionActivity{}}, epoch)
			expectIdle(t, m, epoch)
		} else {
			m.done(op.ID, Outcome{Err: errors.New("source unavailable")}, epoch)
		}
		if !m.state.Display || m.displayID != old || m.switching != nil {
			t.Fatal(m.snapshot())
		}
	}
}
func TestSwitchLogoffFailureDoesNotConnectAnotherMode(t *testing.T) {
	zero := uint32(0)
	m := testMachine()
	displayReady(t, m)
	j := addJob(m, "connect", Desktop)
	j.request.SwitchDisplay = true
	op := expectOp(t, m, epoch, InspectSession)
	m.done(op.ID, Outcome{Activity: switchActivity(&zero, &zero)}, epoch)
	op = expectOp(t, m, epoch, StopDisplay)
	m.done(op.ID, Outcome{}, epoch)
	op = expectOp(t, m, epoch, LogoffSession)
	m.done(op.ID, Outcome{Err: errors.New("activity changed")}, epoch)
	expectIdle(t, m, epoch)
	if m.state.Display || m.switching != nil || len(m.queue) != 0 {
		t.Fatal(m.snapshot())
	}
}
