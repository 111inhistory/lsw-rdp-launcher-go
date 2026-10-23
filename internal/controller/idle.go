package controller

import (
	"log"
	"strings"
	"time"
)

const (
	idleLogoffTransitionTimeout = 30 * time.Second
	idleLogoffRetryInterval     = 500 * time.Millisecond
)

func (m *machine) idleNext(now time.Time) *Operation {
	if m.state.VM != VMRunning || m.autoFailed || m.state.Locked {
		m.resetTimers()
		return nil
	}
	if m.state.Display {
		if m.state.DisplayMode != ModeDesktop && m.state.Windows == 0 && m.opts.IdleDisconnect > 0 {
			if m.state.DisplayDeadline.IsZero() {
				m.state.DisplayDeadline = now.Add(m.opts.IdleDisconnect)
			}
			if !now.Before(m.state.DisplayDeadline) {
				return m.begin(StopDisplay, nil, SourceIdle)
			}
		} else {
			m.state.DisplayDeadline = time.Time{}
		}
		return nil
	}
	// Logoff frees pinned/user memory; pressure must not reset its clock.
	if !m.state.BackendReady || m.state.SSH != SSHZero {
		m.resetTimers()
		return nil
	}
	if m.idleSince.IsZero() {
		m.idleSince = now
		if m.opts.IdleLogoff >= 0 {
			m.state.LogoffDeadline = now.Add(m.opts.IdleLogoff)
		}
	}
	pressure := m.opts.Memory != nil && m.memory.GuestKnown && m.memory.Guest.atLeast(m.opts.Memory.StartPercent)
	memoryKnown := m.opts.Memory == nil || m.memory.GuestKnown
	memoryReady := memoryKnown && !m.memory.Growing && !pressure
	if !memoryReady {
		m.state.SuspendDeadline = time.Time{}
		m.memoryCancelReclaim()
	} else if m.opts.VMSuspend > 0 && m.state.SuspendDeadline.IsZero() {
		m.state.SuspendDeadline = now.Add(m.opts.VMSuspend)
	}

	if op, waiting := m.idleLogoffNext(now); waiting {
		return op
	}

	if m.logoffReclaim {
		if m.opts.Memory == nil || m.memory.LastError != nil {
			m.logoffReclaim = false
		} else if memoryReady && m.memory.DeviceKnown {
			m.logoffReclaim = false
			mem := &m.memory
			mem.reclaimedFor = m.state.SuspendDeadline
			mem.Reclaiming = true
			if mem.Device.RequestedBytes != 0 {
				op := m.memorySet(0, now)
				mem.deadline = now.Add(MemoryReclaimLead)
				return op
			}
		}
	}
	if !memoryReady {
		return nil
	}
	if !m.state.SuspendDeadline.IsZero() {
		if !now.Before(m.state.SuspendDeadline) {
			if m.memory.Reclaiming && m.memory.Awaiting {
				return nil
			}
			m.memoryCancelReclaim()
			return m.begin(SuspendVM, nil, SourceIdle)
		}
		// An enabled logoff owns the first reclaim; don't shrink ahead of it.
		if m.opts.IdleLogoff < 0 || m.idleLogoffDone {
			if op := m.memoryReclaim(now); op != nil {
				return op
			}
		}
	}
	return nil
}

// A fresh query is mandatory at the deadline. An unchanged Active login just
// after our own disconnect may be WTS lag, not another client taking over.
func (m *machine) idleLogoffNext(now time.Time) (*Operation, bool) {
	if m.idleLogoffDone || m.state.LogoffDeadline.IsZero() || now.Before(m.state.LogoffDeadline) {
		return nil, false
	}
	if m.state.Binding == nil {
		m.skipIdleLogoff("no previously bound logon")
		return nil, false
	}
	if m.idleLogoffTransitionUntil.IsZero() {
		m.idleLogoffTransitionUntil = now.Add(idleLogoffTransitionTimeout)
	}
	if !now.Before(m.idleLogoffTransitionUntil) {
		m.skipIdleLogoff("disconnect was not confirmed before the transition deadline")
		return nil, false
	}
	if !m.idleLogoffRetryAt.IsZero() && now.Before(m.idleLogoffRetryAt) {
		return nil, true
	}
	if !m.idleLogoffInspected || !m.state.Sessions.Fresh || m.state.Sessions.ObservedAt.IsZero() || now.Sub(m.state.Sessions.ObservedAt) > 5*time.Second {
		return m.begin(InspectSession, nil, SourceIdle), true
	}
	login, ok := m.state.Sessions.Find(m.state.Binding.Reference)
	if !ok || login.SID != m.state.Binding.SID {
		m.skipIdleLogoff("last owned logon is missing or changed")
		return nil, false
	}
	if login.ConnectTime != m.state.Binding.ConnectTime {
		m.skipIdleLogoff("last owned logon was reconnected by another connection")
		return nil, false
	}
	switch login.State {
	case WTSActive:
		if login.ClientName != "" && !strings.EqualFold(login.ClientName, m.state.Binding.ClientName) {
			m.skipIdleLogoff("last owned logon is active on another client")
			return nil, false
		}
		m.idleLogoffInspected = false
		m.idleLogoffRetryAt = earlier(now.Add(idleLogoffRetryInterval), m.idleLogoffTransitionUntil)
		m.state.BlockingReason = "waiting for the last owned logon to become disconnected"
		return nil, true
	case WTSDisconnected:
		m.idleLogoffRetryAt = time.Time{}
		m.state.BlockingReason = ""
		return m.begin(LogoffSession, nil, SourceIdle), true
	default:
		m.skipIdleLogoff("last owned logon is not confirmed disconnected")
		return nil, false
	}
}

func (m *machine) skipIdleLogoff(reason string) {
	var sessionID uint32
	if m.state.Binding != nil {
		sessionID = m.state.Binding.Reference.SessionID
	}
	log.Printf("idle logoff skipped session=%d reason=%s", sessionID, reason)
	m.idleLogoffDone = true
	m.state.LogoffDeadline = time.Time{}
	m.idleLogoffRetryAt = time.Time{}
	m.idleLogoffTransitionUntil = time.Time{}
	m.state.BlockingReason = "logon retained: " + reason
}
