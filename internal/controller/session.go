package controller

import (
	"fmt"
	"time"
)

// WTS state values are part of the Windows wire protocol.
const (
	WTSActive       = 0
	WTSDisconnected = 4
)

// A logon identity survives reconnects but not a new logon or kernel boot.
type SessionRef struct {
	SessionID        uint32 `json:"session_id"`
	AuthenticationID string `json:"authentication_id"`
	BootIdentifier   string `json:"boot_identifier"`
}

func (r SessionRef) Valid() bool {
	return r.SessionID != 0 && r.AuthenticationID != "" && r.BootIdentifier != ""
}

type Login struct {
	Reference     SessionRef `json:"reference"`
	SID           string     `json:"sid"`
	User          string     `json:"user"`
	Domain        string     `json:"domain"`
	State         int        `json:"state"`
	ClientName    string     `json:"client_name"`
	ClientAddress string     `json:"client_address"`
	LogonTime     int64      `json:"logon_time"`
	ConnectTime   int64      `json:"connect_time"`
	Windows       *uint32    `json:"windows"`
	TrayIcons     *uint32    `json:"tray_icons"`
	Note          string     `json:"note,omitempty"`
}

type SessionSnapshot struct {
	Revision   uint64    `json:"revision"`
	Fresh      bool      `json:"fresh"`
	Sessions   []Login   `json:"sessions"`
	ObservedAt time.Time `json:"-"`
}

func (r Request) Validate() error {
	if r.Action == "" {
		return fmt.Errorf("request action is required")
	}
	if r.Mode != "" && r.Mode != ModeDesktop && r.Mode != ModeRemoteApp {
		return fmt.Errorf("invalid connection mode %q", r.Mode)
	}
	if r.Mode != "" && !r.UserContext {
		return fmt.Errorf("a display mode requires user context")
	}
	if r.Action == "connect" && (r.Mode == "" || !r.UserContext) {
		return fmt.Errorf("connect requires a display mode and user context")
	}
	if r.Choice != "" && !r.UserContext {
		return fmt.Errorf("a conflict action requires user context")
	}
	if r.Choice != "" && r.Choice != SwitchCurrent && r.Choice != SwitchKeep && r.Choice != SwitchLogoff && r.Choice != SwitchCancel {
		return fmt.Errorf("invalid conflict action %q", r.Choice)
	}
	if r.Action == "batch" && (len(r.Batch) == 0 || !r.NeedsUser()) {
		return fmt.Errorf("a user batch requires commands and user context")
	}
	return nil
}

func (m *machine) invalidateSessions() {
	m.sessionGeneration++
	m.state.Sessions.Fresh = false
}

func (m *machine) sessions(snapshot SessionSnapshot) {
	if !snapshot.Fresh && m.state.Sessions.Fresh {
		m.sessionGeneration++
	}
	if snapshot.Fresh && m.state.Sessions.Fresh && snapshot.Revision < m.state.Sessions.Revision {
		return
	}
	m.state.Sessions = snapshot
}

func (m *machine) boundReady() bool {
	if !m.state.Display || m.state.Binding == nil {
		return false
	}
	login, ok := m.state.Sessions.Find(m.state.Binding.Reference)
	return ok && login.State == WTSActive && login.SID == m.state.Binding.SID && login.ClientName == m.state.Binding.ClientName && login.ConnectTime == m.state.Binding.ConnectTime
}

func (s SessionSnapshot) Find(ref SessionRef) (Login, bool) {
	if !s.Fresh || !ref.Valid() {
		return Login{}, false
	}
	for _, login := range s.Sessions {
		if login.Reference == ref && login.SID != "" {
			return login, true
		}
	}
	return Login{}, false
}

func (l Login) Activity() SessionActivity {
	return SessionActivity{Reference: l.Reference, SID: l.SID, SessionID: l.Reference.SessionID, User: l.Domain + `\` + l.User, Windows: l.Windows, TrayIcons: l.TrayIcons, Note: l.Note}
}
