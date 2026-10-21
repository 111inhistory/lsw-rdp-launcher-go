package controller

import "fmt"

// Nil counts mean unknown, never empty. SessionID 0 means no unambiguous user
// session was selected. Fingerprint identifies one logon, not merely a reused ID.
type SessionActivity struct {
	SessionID   uint32  `json:"session_id"`
	User        string  `json:"user"`
	Fingerprint string  `json:"fingerprint"`
	Windows     *uint32 `json:"windows"`
	TrayIcons   *uint32 `json:"tray_icons"`
	Note        string  `json:"note,omitempty"`
}

func (a SessionActivity) Empty() bool {
	return a.Windows != nil && a.TrayIcons != nil && *a.Windows == 0 && *a.TrayIcons == 0
}

// CanLogoff identifies a target; it does not say that ending it is harmless.
// Unknown activity still needs consent, even when its session is identified.
func (a SessionActivity) CanLogoff() bool {
	return a.SessionID != 0 && a.User != "" && a.Fingerprint != ""
}

// SwitchPrompt grants the UI observations, not session-control authority.
type SwitchPrompt struct {
	Activity   SessionActivity `json:"activity"`
	TargetMode string          `json:"target_mode"`
}

type SwitchDecision string

const (
	SwitchCancel SwitchDecision = "cancel"
	SwitchKeep   SwitchDecision = "keep"
	SwitchLogoff SwitchDecision = "logoff"
)

type switchPhase uint8

const (
	switchInspect switchPhase = iota
	switchConfirm
	switchDisconnect
	switchLogoff
	switchConnect
)

// One queue head owns the whole switch. Releasing it between disconnect and
// reconnect would let idle suspension run while the user is changing modes.
type displaySwitch struct {
	job       *job
	phase     switchPhase
	activity  SessionActivity
	decision  SwitchDecision
	automatic bool
}

func (m *machine) switchNext(head *job) *Operation {
	if m.switching == nil {
		m.switching = &displaySwitch{job: head}
	}
	s := m.switching
	for {
		switch s.phase {
		case switchInspect:
			if !m.state.BackendReady {
				return nil
			}
			return m.begin(InspectSession, head)
		case switchConfirm:
			return m.begin(ConfirmSwitch, head)
		case switchDisconnect:
			if m.state.Display {
				return m.begin(StopDisplay, head)
			}
			if s.decision == SwitchLogoff {
				s.phase = switchLogoff
			} else {
				s.phase = switchConnect
			}
		case switchLogoff:
			return m.begin(LogoffSession, head)
		case switchConnect:
			return nil
		}
	}
}
func (m *machine) switchDone(op *Operation, out Outcome) bool {
	if op.Kind != InspectSession && op.Kind != ConfirmSwitch && op.Kind != LogoffSession {
		return false
	}
	s := m.switching
	if s == nil || s.job != op.job {
		return true
	}
	// A VM lifecycle change invalidates the observation made before the dialog.
	// The guest separately checks the logon fingerprint for session-ID reuse.
	if op.revision != m.revision {
		m.finish(s.job, Result{Err: fmt.Errorf("VM changed during display switch")})
		return true
	}
	switch op.Kind {
	case InspectSession:
		if out.Activity == nil {
			m.finish(s.job, Result{Err: fmt.Errorf("session observation is missing")})
			return true
		}
		s.activity = *out.Activity
		s.phase = switchConfirm
		if s.activity.Empty() {
			// When the guest reports no user session, connect without logoff.
			// For an identified empty session, the guest must recheck it.
			s.automatic = true
			s.decision = SwitchKeep
			if s.activity.CanLogoff() {
				s.decision = SwitchLogoff
			}
			s.phase = switchDisconnect
		}
	case ConfirmSwitch:
		switch out.Decision {
		case SwitchCancel:
			m.finish(s.job, Result{Data: map[string]string{"status": "cancelled"}})
		case SwitchKeep:
			s.decision = SwitchKeep
			s.phase = switchDisconnect
		case SwitchLogoff:
			if !s.activity.CanLogoff() {
				m.finish(s.job, Result{Err: fmt.Errorf("cannot log off an unidentified session")})
				return true
			}
			s.decision = SwitchLogoff
			s.phase = switchDisconnect
		default:
			m.finish(s.job, Result{Err: fmt.Errorf("invalid display switch decision %q", out.Decision)})
		}
	case LogoffSession:
		s.phase = switchConnect
	}
	return true
}
