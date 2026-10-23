package controller

import "fmt"

// Activity is a view of one identified logon, never authority inferred from UI counts.
type SessionActivity struct {
	SessionID uint32     `json:"session_id"`
	User      string     `json:"user"`
	Reference SessionRef `json:"reference"`
	SID       string     `json:"sid"`
	Windows   *uint32    `json:"windows"`
	TrayIcons *uint32    `json:"tray_icons"`
	Note      string     `json:"note,omitempty"`
}

func (a SessionActivity) Empty() bool {
	return a.Windows != nil && a.TrayIcons != nil && *a.Windows == 0 && *a.TrayIcons == 0
}
func (a SessionActivity) CanLogoff() bool { return a.Reference.Valid() && a.SID != "" && a.User != "" }

type SwitchPrompt struct {
	Activity   SessionActivity  `json:"activity"`
	TargetMode string           `json:"target_mode"`
	Allowed    []SwitchDecision `json:"allowed"`
}

func (p SwitchPrompt) Allows(decision SwitchDecision) bool {
	for _, allowed := range p.Allowed {
		if allowed == decision {
			return true
		}
	}
	return false
}

type SwitchDecision string

const (
	SwitchCurrent SwitchDecision = "current"
	SwitchCancel  SwitchDecision = "cancel"
	SwitchKeep    SwitchDecision = "keep"
	SwitchLogoff  SwitchDecision = "logoff"
)

type switchPhase uint8

const (
	switchInspect switchPhase = iota
	switchConfirm
	switchDisconnect
	switchLogoff
	switchConnect
)

func (p switchPhase) String() string {
	return [...]string{"inspect", "confirm", "disconnect", "logoff", "connect"}[p]
}

type displaySwitch struct {
	job       *job
	phase     switchPhase
	activity  SessionActivity
	decision  SwitchDecision
	observed  Login
	displayID uint64
	mode      string
}

func (m *machine) switchPrompt(s *displaySwitch) SwitchPrompt {
	allowed := []SwitchDecision{SwitchCancel, SwitchKeep}
	if s.activity.CanLogoff() {
		allowed = append(allowed, SwitchLogoff)
	}
	if m.state.Display && m.state.DisplayMode == ModeDesktop && s.job.request.Action != "connect" && s.activity.CanLogoff() {
		allowed = append(allowed, SwitchCurrent)
	}
	return SwitchPrompt{Activity: s.activity, TargetMode: s.job.request.Mode, Allowed: allowed}
}

func (m *machine) switchNext(head *job) *Operation {
	if m.switching == nil {
		m.switching = &displaySwitch{job: head, displayID: m.displayID, mode: m.state.DisplayMode}
	}
	s := m.switching
	for {
		switch s.phase {
		case switchInspect:
			if !m.state.BackendReady {
				return nil
			}
			return m.begin(InspectSession, head, SourceUser)
		case switchConfirm:
			return m.begin(ConfirmSwitch, head, SourceUser)
		case switchDisconnect:
			if m.state.Display {
				return m.begin(StopDisplay, head, SourceUser)
			}
			if s.decision == SwitchLogoff {
				s.phase = switchLogoff
			} else {
				s.phase = switchConnect
			}
		case switchLogoff:
			return m.begin(LogoffSession, head, SourceUser)
		case switchConnect:
			return nil
		}
	}
}

func (m *machine) switchDone(op *Operation, out Outcome) bool {
	if op.Kind != InspectSession && op.Kind != ConfirmSwitch && op.Kind != LogoffSession {
		return false
	}
	if op.Source == SourceIdle {
		return false
	}
	s := m.switching
	if s == nil || s.job != op.job {
		return false
	}
	if op.revision != m.revision {
		m.finish(s.job, Result{Err: fmt.Errorf("VM changed during display switch")})
		return true
	}
	switch op.Kind {
	case InspectSession:
		if out.Sessions == nil || !out.Sessions.Fresh || m.state.Binding == nil {
			m.finish(s.job, Result{Err: fmt.Errorf("cannot confirm the owned connection logon")})
			return true
		}
		login, ok := out.Sessions.Find(m.state.Binding.Reference)
		if !ok || login.State != WTSActive || login.SID != m.state.Binding.SID {
			m.finish(s.job, Result{Err: fmt.Errorf("bound logon changed; connection kept")})
			return true
		}
		m.sessions(*out.Sessions)
		s.observed, s.activity = login, login.Activity()
		s.phase = switchConfirm
		if s.job.request.Choice == "" && s.activity.Empty() && s.mode != ModeDesktop {
			// Empty UI never authorizes logoff or silently abandons a desktop.
			s.decision, s.phase = SwitchKeep, switchDisconnect
		}
	case ConfirmSwitch:
		if !m.state.Display || m.displayID != s.displayID || m.state.DisplayMode != s.mode {
			m.finish(s.job, Result{Err: fmt.Errorf("owned connection changed during confirmation")})
			return true
		}
		login, ok := m.state.Sessions.Find(s.activity.Reference)
		if !ok || login.SID != s.observed.SID || login.State != s.observed.State || login.ConnectTime != s.observed.ConnectTime || login.ClientName != s.observed.ClientName {
			m.finish(s.job, Result{Err: fmt.Errorf("logon changed during confirmation; connection kept")})
			return true
		}
		if !m.switchPrompt(s).Allows(out.Decision) {
			m.finish(s.job, Result{Err: fmt.Errorf("action %q is not allowed for this logon", out.Decision)})
			return true
		}
		s.decision = out.Decision
		switch out.Decision {
		case SwitchCancel:
			m.finish(s.job, Result{Data: map[string]string{"status": "cancelled"}})
		case SwitchCurrent:
			s.job.request.Mode = ModeDesktop
			s.phase = switchConnect
		case SwitchKeep, SwitchLogoff:
			s.phase = switchDisconnect
		}
	case LogoffSession:
		s.phase = switchConnect
	}
	return true
}
