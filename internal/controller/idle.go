package controller

import "time"

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
	pressure := m.opts.Memory != nil && m.memory.GuestKnown && m.memory.Guest.atLeast(m.opts.Memory.StartPercent)
	memoryKnown := m.opts.Memory == nil || m.memory.GuestKnown
	if !m.state.BackendReady || m.state.SSH != SSHZero || !memoryKnown || m.memory.Growing || pressure {
		m.resetTimers()
		return nil
	}
	if m.idleSince.IsZero() {
		m.idleSince = now
		if m.opts.VMSuspend > 0 {
			m.state.SuspendDeadline = now.Add(m.opts.VMSuspend)
		}
		if m.opts.IdleLogoff >= 0 {
			m.state.LogoffDeadline = now.Add(m.opts.IdleLogoff)
		}
	}
	if !m.idleLogoffDone && !m.state.LogoffDeadline.IsZero() && !now.Before(m.state.LogoffDeadline) {
		if m.state.Binding == nil {
			m.skipIdleLogoff("no previously bound logon")
		} else if !m.state.Sessions.Fresh || m.state.Sessions.ObservedAt.IsZero() || now.Sub(m.state.Sessions.ObservedAt) > 5*time.Second {
			return m.begin(InspectSession, nil, SourceIdle)
		} else if login, ok := m.state.Sessions.Find(m.state.Binding.Reference); !ok || login.SID != m.state.Binding.SID || login.State != 4 {
			m.skipIdleLogoff("last owned logon is missing, changed, or active elsewhere")
		} else {
			return m.begin(LogoffSession, nil, SourceIdle)
		}
	}
	if m.logoffReclaim {
		m.logoffReclaim = false
		mem := &m.memory
		if m.opts.Memory != nil && mem.LastError == nil && mem.DeviceKnown && mem.GuestKnown && !mem.Growing {
			mem.reclaimedFor = m.state.SuspendDeadline
			mem.Reclaiming = true
			if mem.Device.RequestedBytes != 0 {
				op := m.memorySet(0, now)
				// Equal logoff/suspend deadlines still permit bounded convergence.
				mem.deadline = now.Add(MemoryReclaimLead)
				return op
			}
		}
	}
	if !m.state.SuspendDeadline.IsZero() {
		if !now.Before(m.state.SuspendDeadline) {
			if m.memory.Reclaiming && m.memory.Awaiting {
				return nil
			}
			m.memoryCancelReclaim()
			return m.begin(SuspendVM, nil, SourceIdle)
		}
		if op := m.memoryReclaim(now); op != nil {
			return op
		}
	}
	return nil
}

func (m *machine) skipIdleLogoff(reason string) {
	m.idleLogoffDone = true
	m.state.LogoffDeadline = time.Time{}
	m.state.BlockingReason = "logon retained: " + reason
}
