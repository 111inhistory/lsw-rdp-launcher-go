// Package controller implements the host's event-driven business core.
package controller

import (
	"context"
	"fmt"
	"log"
	"time"
)

type VMState string

const (
	VMUnknown      VMState = "unknown"
	VMRunning      VMState = "running"
	VMPaused       VMState = "paused"
	VMOff          VMState = "shut off"
	VMPMSuspended  VMState = "pmsuspended"
	VMShuttingDown VMState = "in shutdown"
	VMCrashed      VMState = "crashed"
)

type SSHState string

const (
	SSHUnknown SSHState = "unknown"
	SSHZero    SSHState = "zero"
	SSHActive  SSHState = "nonzero"
)

const (
	ModeRemoteApp = "remoteapp"
	ModeDesktop   = "desktop"
)

// The business payload survives connection changes and a single confirmation.
// Confirm belongs to the requesting IPC connection, never to a resident GUI.
type Request struct {
	Action     string                                                      `json:"action"`
	Target     string                                                      `json:"target,omitempty"`
	Params     string                                                      `json:"params,omitempty"`
	WorkingDir string                                                      `json:"working_dir,omitempty"`
	WithIcons  bool                                                        `json:"with_icons,omitempty"`
	File       string                                                      `json:"file,omitempty"`
	Mode       string                                                      `json:"mode,omitempty"`
	Choice     SwitchDecision                                              `json:"choice,omitempty"`
	Batch      []Request                                                   `json:"batch,omitempty"`
	Confirm    func(context.Context, SwitchPrompt) (SwitchDecision, error) `json:"-"`
}

func (r Request) NeedsUser() bool { return r.Mode != "" }

type Result struct {
	Data any
	Err  error
}
type job struct {
	id      uint64
	request Request
	ctx     context.Context
	reply   chan Result
}
type Options struct {
	IdleDisconnect, VMSuspend time.Duration
	IdleLogoff                time.Duration // -1 disables; zero runs after disconnect.
	VMConfirmTimeout          time.Duration
	Memory                    *MemoryPolicy
}
type Kind string

const (
	QueryVM        Kind = "query-vm"
	WakeVM         Kind = "wake-vm"
	StartDisplay   Kind = "start-display"
	StopDisplay    Kind = "stop-display"
	SuspendVM      Kind = "suspend-vm"
	Dispatch       Kind = "dispatch"
	InspectSession Kind = "inspect-session"
	ConfirmSwitch  Kind = "confirm-switch"
	LogoffSession  Kind = "logoff-session"
	QueryMemory    Kind = "query-memory"
	SetMemory      Kind = "set-memory"
	PrepareMemory  Kind = "prepare-memory"
)

type OperationSource string

const (
	SourceUser     OperationSource = "user"
	SourceIdle     OperationSource = "idle"
	SourcePressure OperationSource = "pressure"
	SourceRecovery OperationSource = "recovery"
)

type Operation struct {
	Source               OperationSource
	Reference            SessionRef
	ID                   uint64
	Kind                 Kind
	VM                   VMState
	Request              Request
	Activity             SessionActivity
	Prompt               SwitchPrompt
	MemoryTarget         uint64
	ResumeSafe           bool
	MemoryPrepared       bool
	memoryRevision       uint64
	memorySourceRevision uint64
	revision             uint64
	sessionGeneration    uint64
	idleGeneration       uint64
	job                  *job
	displayExited        bool
	accepted             bool
	confirmDeadline      time.Time
}

func (op *Operation) targetVM() VMState {
	if op.Kind == WakeVM {
		return VMRunning
	}
	return VMPaused
}

type Outcome struct {
	Sessions       *SessionSnapshot
	Binding        *Login
	Activity       *SessionActivity
	Decision       SwitchDecision
	VM             VMState // QueryVM snapshot only. WakeVM/SuspendVM success is command acknowledgement.
	Data           any
	Memory         *MemoryDevice
	ResumeSafe     bool
	MemoryPrepared bool
	Err            error
}
type Snapshot struct {
	VM                               VMState
	BackendReady                     bool
	SSH                              SSHState
	Display                          bool
	DisplayMode                      string // Empty when disconnected; remoteapp or desktop.
	Windows                          int    // -1 means no complete Niri snapshot.
	Locked                           bool
	Queued                           int
	Operation                        Kind
	AwaitingVM                       VMState
	DisplayDeadline, SuspendDeadline time.Time
	LastError                        error
	Binding                          *Login
	Sessions                         SessionSnapshot
	Stage                            string
	BlockingReason                   string
	RetainedLogins                   int
	LogoffDeadline                   time.Time
	Memory                           MemoryState
}
type completion struct {
	job    *job
	result Result
}

// machine is owned by the event loop. It performs no I/O and has no locks.
// Tests pass explicit timestamps instead of sleeping through idle timeouts.
type subscriptionState uint8

const (
	subscriptionReady subscriptionState = iota
	subscriptionLost
	subscriptionNeedsSnapshot
)

type machine struct {
	opts                        Options
	state                       Snapshot
	queue                       []*job
	busy                        *Operation
	memoryBusy                  *Operation
	nextID, revision, displayID uint64
	nextTaskID                  uint64
	needQuery                   bool
	vmSubscription              subscriptionState
	memorySourceRevision        uint64
	memorySourceLost            bool
	cleanupDisplay              bool
	autoFailed                  bool
	completed                   []completion
	memory                      memoryControl
	switching                   *displaySwitch
	idleSince                   time.Time
	idleLogoffDone              bool
	logoffReclaim               bool
	sessionGeneration           uint64
	backendStreamGeneration     uint64
	backendStreamInvalid        bool
	idleGeneration              uint64
}

func newMachine(opts Options) *machine {
	if opts.VMConfirmTimeout == 0 {
		opts.VMConfirmTimeout = 15 * time.Second
	}
	m := &machine{opts: opts, state: Snapshot{VM: VMUnknown, SSH: SSHUnknown, Windows: -1}, needQuery: true}
	if opts.Memory != nil {
		policy := opts.Memory.Defaults()
		m.opts.Memory = &policy
		m.memory.LastError = policy.Validate()
	}
	return m
}
func (m *machine) snapshot() Snapshot {
	s := m.state
	s.Queued = len(m.queue)
	s.RetainedLogins = len(s.Sessions.Sessions)
	if !s.Sessions.Fresh {
		s.RetainedLogins = -1
	}
	if s.Binding != nil && s.Display && s.RetainedLogins > 0 {
		s.RetainedLogins--
	}
	if m.switching != nil {
		s.Stage = fmt.Sprint(m.switching.phase)
	}
	if s.Display && s.Binding == nil {
		s.BlockingReason = "connection is not bound to a logon"
	}
	if !s.Sessions.Fresh && s.Binding != nil {
		s.BlockingReason = "session snapshot is stale"
	}
	if s.Locked && m.opts.Memory != nil && s.VM == VMRunning && s.BackendReady && m.memory.GuestKnown && !m.memory.WindowPrepared {
		s.BlockingReason = "memory initialization requires pause; unlock before preparation"
	}
	s.Memory = m.memory.MemoryState
	s.Memory.Preparing = m.memory.phase != memoryIdle
	if m.memoryBusy != nil {
		s.Memory.Operation = m.memoryBusy.Kind
	} else if m.busy != nil && (m.busy.Kind == QueryMemory || m.busy.Kind == SetMemory || m.busy.Kind == PrepareMemory) {
		s.Memory.Operation = m.busy.Kind
	}
	if m.busy != nil {
		s.Operation = m.busy.Kind
		if m.busy.accepted {
			s.AwaitingVM = m.busy.targetVM()
		}
	}
	return s
}
func (m *machine) enqueue(j *job) {
	m.nextTaskID++
	j.id = m.nextTaskID
	m.queue = append(m.queue, j)
	m.resetTimers()
	m.autoFailed = false
	if m.state.VM == VMUnknown || m.vmSubscription == subscriptionNeedsSnapshot {
		m.needQuery = true
	}
}
func (m *machine) lock(locked bool) {
	m.state.Locked = locked
	m.resetTimers()
}
func (m *machine) windows(count int) {
	if count != m.state.Windows {
		m.state.DisplayDeadline = time.Time{}
	}
	m.state.Windows = count
}
func (m *machine) vmSource(ready bool) {
	if !ready {
		m.vmSubscription = subscriptionLost
		m.vm(VMUnknown)
		m.needQuery = false // No snapshot before the replacement subscription ACK.
		if m.busy != nil && m.busy.accepted {
			m.confirmationLost(m.busy)
		}
		return
	}
	m.vmSubscription = subscriptionNeedsSnapshot
	m.needQuery = true // Missed lifecycle events cannot be replayed.
}

// The command was accepted, but its confirmation stream no longer exists.
// Fail only its caller; later FIFO entries can continue after a fresh snapshot.
func (m *machine) confirmationLost(op *Operation) {
	m.busy = nil
	err := fmt.Errorf("%s accepted, but lifecycle source changed; remote outcome unknown", op.Kind)
	m.state.LastError = err
	if op.job != nil {
		m.finish(op.job, Result{Err: err})
	}
}

func (m *machine) vm(state VMState) {
	if state == m.state.VM && state != VMUnknown {
		return
	}
	m.revision++
	m.invalidateSessions()
	m.backendStreamInvalid = true
	m.state.VM = state
	m.needQuery = state == VMUnknown || m.vmSubscription == subscriptionNeedsSnapshot
	m.resetTimers()
	m.autoFailed = false
	m.state.BackendReady = false
	m.state.SSH = SSHUnknown
	m.memory.GuestKnown, m.memory.DeviceKnown = false, false
	m.memory.Growing, m.memory.Awaiting = false, false
	m.memory.waitGuest = false
	m.memory.deadline = time.Time{}
	if m.memory.phase == memoryPausing && state == VMPaused {
		m.memory.phase = memoryWindow
	}
	if m.memory.phase == memoryResuming && state == VMRunning {
		m.memory.phase = memoryIdle
	}
	if state == VMUnknown && m.memory.phase != memoryIdle {
		m.memoryAbort(fmt.Errorf("VM state lost during memory preparation"))
	}
	m.state.Sessions.Fresh = false
	if state == VMOff {
		m.state.Binding = nil
		m.memory = memoryControl{}
		if m.memorySourceLost {
			m.memory.LastError = ErrMemorySourceLost
		}
		m.state.Locked = false
	}
	if m.busy != nil && m.busy.accepted && state == m.busy.targetVM() {
		m.busy = nil
	}
}
func (m *machine) backend(ready bool, ssh SSHState) {
	if m.state.VM != VMRunning {
		return
	}
	if m.state.SSH != ssh || m.state.BackendReady != ready {
		m.resetTimers()
	}
	m.state.BackendReady = ready
	m.state.SSH = ssh
	if !ready {
		m.invalidateSessions()
		m.backendStreamInvalid = true
		m.state.SSH = SSHUnknown
		m.memory.GuestKnown = false
	}
	if !ready || ssh != SSHZero {
		m.memoryCancelReclaim()
	}
}
func (m *machine) displayExited(id uint64, err error) {
	if m.busy != nil && m.busy.Kind == StartDisplay && m.busy.ID == id {
		m.busy.displayExited = true
	}
	if id != m.displayID {
		return
	}
	m.state.Display = false
	m.state.DisplayMode = ""
	m.displayID = 0
	m.cleanupDisplay = false
	m.resetTimers()
	if err != nil {
		m.state.LastError = err
	}
}
func (m *machine) resetTimers() {
	m.idleGeneration++
	m.state.DisplayDeadline = time.Time{}
	m.state.SuspendDeadline = time.Time{}
	m.state.LogoffDeadline = time.Time{}
	m.idleSince = time.Time{}
	m.idleLogoffDone = false
	m.logoffReclaim = false
	m.state.BlockingReason = ""
	m.memoryCancelReclaim()
}
func (m *machine) finish(j *job, result Result) {
	if result.Err != nil {
		m.state.LastError = result.Err
	}
	log.Printf("request complete task=user-%d action=%s error=%v", j.id, j.request.Action, result.Err)
	// An operation may only complete its own queue head, never the next caller.
	// A mismatch is an internal invariant violation, not a recoverable RPC error.
	if len(m.queue) == 0 || m.queue[0] != j {
		panic("controller: completion is not the FIFO head")
	}
	m.queue = m.queue[1:]
	if m.switching != nil && m.switching.job == j {
		m.switching = nil
	}
	m.completed = append(m.completed, completion{j, result})
	m.resetTimers()
}
func (m *machine) done(id uint64, out Outcome, now time.Time) {
	if m.memoryBusy != nil && m.memoryBusy.ID == id {
		op := m.memoryBusy
		m.memoryBusy = nil
		m.memoryDone(op, out)
		return
	}
	if m.busy == nil || m.busy.ID != id || m.busy.accepted {
		return
	}
	op := m.busy
	m.busy = nil
	if op.Kind == QueryMemory || op.Kind == SetMemory || op.Kind == PrepareMemory {
		m.memoryDone(op, out)
		return
	}
	if out.Err != nil {
		if op.Source == SourceIdle && (op.Kind == InspectSession || op.Kind == LogoffSession) {
			m.state.LastError = out.Err
			m.skipIdleLogoff(out.Err.Error())
			return
		}
		if op.Kind == QueryVM {
			// Fail queued callers consistently whether source loss or this
			// result arrived first. Revision only guards observation state.
			for len(m.queue) > 0 {
				m.finish(m.queue[0], Result{Err: out.Err})
			}
			if op.revision != m.revision {
				return
			}
		}
		preparing := m.memory.phase != memoryIdle
		if preparing {
			m.memoryAbort(out.Err)
		}
		m.state.LastError = out.Err
		// A caller failure must not disable idle management for the whole VM.
		// Only failed autonomous work is held to avoid an immediate retry loop.
		m.autoFailed = op.job == nil
		// The adapter invalidates this observation connection on query failure.
		// Wait for its replacement subscription ACK, not another CLI request.
		if op.Kind == QueryVM {
			m.needQuery = false
		} else if op.job != nil {
			m.finish(op.job, Result{Data: out.Data, Err: out.Err})
		} else if preparing {
			for len(m.queue) > 0 {
				m.finish(m.queue[0], Result{Err: out.Err})
			}
		}
		return
	}
	m.state.LastError = nil
	if op.sessionGeneration != m.sessionGeneration && (out.Sessions != nil || op.Kind == InspectSession || op.Kind == ConfirmSwitch || op.Kind == StartDisplay) {
		if op.Kind == StartDisplay && out.Binding != nil {
			m.state.Display, m.state.DisplayMode, m.displayID, m.cleanupDisplay = true, op.Request.Mode, op.ID, true
		}
		if op.job != nil {
			m.finish(op.job, Result{Data: out.Data, Err: fmt.Errorf("session observation source changed during %s; result not reused", op.Kind)})
		} else {
			m.skipIdleLogoff("session source changed")
		}
		return
	}
	if out.Sessions != nil {
		m.sessions(*out.Sessions)
	}
	if op.Source == SourceIdle && op.Kind == InspectSession {
		if out.Sessions == nil || !out.Sessions.Fresh {
			m.skipIdleLogoff("current session snapshot unavailable")
		}
		return
	}
	if op.Source == SourceIdle && op.Kind == LogoffSession {
		m.state.Sessions.Fresh = false
		if op.idleGeneration != m.idleGeneration {
			return
		}
		m.idleLogoffDone, m.logoffReclaim = true, true
		m.state.LogoffDeadline = time.Time{}
		m.state.Sessions.Fresh = false
		return
	}
	if m.switchDone(op, out) {
		return
	}
	switch op.Kind {
	case WakeVM, SuspendVM:
		if op.Kind == WakeVM && out.MemoryPrepared {
			m.memory.WindowPrepared = true
		}
		// A command acknowledgement is not a VM state observation. Hold this
		// control slot until the target event arrives (or has already arrived).
		if m.state.VM != op.targetVM() {
			if op.revision != m.revision && m.vmSubscription != subscriptionReady {
				m.confirmationLost(op)
				return
			}
			op.accepted = true
			op.confirmDeadline = now.Add(m.opts.VMConfirmTimeout)
			m.busy = op
		}
	case QueryVM:
		// A lifecycle message received during the command wins over its snapshot.
		if op.revision == m.revision {
			m.vm(out.VM)
			m.needQuery = false
			m.vmSubscription = subscriptionReady
		}
	case StartDisplay:
		if op.displayExited {
			m.finish(op.job, Result{Err: fmt.Errorf("FreeRDP exited during startup")})
			return
		}
		if out.Binding == nil || !out.Binding.Reference.Valid() || out.Binding.SID == "" {
			m.finish(op.job, Result{Err: fmt.Errorf("display has no confirmed user logon")})
			return
		}
		m.state.Binding = out.Binding
		m.state.BlockingReason = ""
		m.state.Display = true
		m.state.DisplayMode = op.Request.Mode
		m.displayID = op.ID
		m.cleanupDisplay = op.revision != m.revision
	case StopDisplay:
		m.state.DisplayDeadline = time.Time{}
		m.state.Display = false
		m.state.DisplayMode = ""
		m.displayID = 0
		m.cleanupDisplay = false
	case Dispatch:
		if op.revision != m.revision {
			m.finish(op.job, Result{Err: fmt.Errorf("VM changed during request; remote outcome unknown")})
		} else {
			m.finish(op.job, Result{Data: out.Data})
		}
	}
}
func (m *machine) begin(kind Kind, j *job, source OperationSource) *Operation {
	if source == SourceIdle && m.state.Locked {
		return nil
	}
	m.nextID++
	op := &Operation{ID: m.nextID, Kind: kind, Source: source, VM: m.state.VM, revision: m.revision, sessionGeneration: m.sessionGeneration, idleGeneration: m.idleGeneration, job: j}
	if m.state.Binding != nil {
		op.Reference = m.state.Binding.Reference
	}
	if j != nil {
		op.Request = j.request
		if m.switching != nil && m.switching.job == j {
			op.Activity = m.switching.activity
			if kind == LogoffSession && op.Activity.Reference.Valid() {
				op.Reference = op.Activity.Reference
			}
			if kind == ConfirmSwitch {
				op.Prompt = m.switchPrompt(m.switching)
			}
		}
	}
	op.memoryRevision = m.memory.revision
	op.memorySourceRevision = m.memorySourceRevision
	op.ResumeSafe = kind == WakeVM && m.memory.phase == memoryResuming && m.memory.resumeSafe
	op.MemoryPrepared = m.memory.WindowPrepared
	m.busy = op
	if source == SourceUser && kind != QueryMemory && kind != SetMemory {
		m.resetTimers()
	}
	return op
}
func (m *machine) next(now time.Time) *Operation {
	m.memoryExpire(now)
	if m.busy != nil {
		op := m.busy
		if op.accepted && !now.Before(op.confirmDeadline) {
			err := fmt.Errorf("%s accepted, but no %s lifecycle confirmation", op.Kind, op.targetVM())
			m.busy = nil
			m.vm(VMUnknown)
			m.needQuery = m.vmSubscription == subscriptionNeedsSnapshot
			m.autoFailed = op.job == nil
			m.state.LastError = err
			for len(m.queue) > 0 {
				m.finish(m.queue[0], Result{Err: err})
			}
		}
		if m.busy != nil && uiOperation(m.busy.Kind) && m.memoryBusy == nil && m.memory.WindowPrepared {
			return m.memoryNext(now)
		}
		return nil
	}
	if m.memoryBusy != nil {
		return nil
	}
	for len(m.queue) > 0 && m.queue[0].ctx.Err() != nil {
		m.finish(m.queue[0], Result{Err: m.queue[0].ctx.Err()})
	}
	var head *job
	if len(m.queue) > 0 {
		head = m.queue[0]
	}
	if m.vmSubscription != subscriptionLost && m.needQuery {
		// A snapshot serves the whole controller, not the current caller.
		return m.begin(QueryVM, nil, SourceRecovery)
	}
	if m.vmSubscription == subscriptionNeedsSnapshot {
		return nil // A failed refresh waits for the adapter's replacement ACK.
	}
	if m.memory.phase != memoryIdle {
		if m.memorySourceLost {
			return nil
		}
		return m.memoryPreparationNext()
	}
	if m.state.Display && (m.cleanupDisplay || (m.state.VM != VMRunning && m.state.VM != VMUnknown)) {
		if head != nil || !m.autoFailed {
			return m.begin(StopDisplay, head, SourceRecovery)
		}
		return nil
	}
	if op := m.memoryNext(now); op != nil {
		return op
	}
	if head != nil {
		switch m.state.VM {
		case VMUnknown:
			return nil
		case VMPaused, VMOff, VMPMSuspended:
			if m.opts.Memory != nil && m.memorySourceLost {
				return nil
			}
			return m.begin(WakeVM, head, SourceUser)
		case VMRunning:
		default:
			m.finish(head, Result{Err: fmt.Errorf("cannot execute while VM is %s", m.state.VM)})
			return nil
		}
		if m.opts.Memory != nil && m.memory.LastError == nil && !m.memory.WindowPrepared {
			return nil
		}
		if !m.state.BackendReady {
			return nil
		}
		if head.request.NeedsUser() {
			conflict := m.state.Display && head.request.Mode != m.state.DisplayMode
			if conflict || m.switching != nil {
				if op := m.switchNext(head); op != nil {
					return op
				}
				if len(m.queue) == 0 || m.queue[0] != head || m.switching == nil || m.switching.phase != switchConnect {
					return nil
				}
			} else if head.request.Choice != "" {
				m.finish(head, Result{Err: fmt.Errorf("conflict action %q has no applicable display conflict", head.request.Choice)})
				return nil
			}
			if !m.state.Display {
				return m.begin(StartDisplay, head, SourceUser)
			}
			if !m.boundReady() {
				m.finish(head, Result{Err: fmt.Errorf("owned connection has no current verified logon; no application was dispatched")})
				return nil
			}
		}
		return m.begin(Dispatch, head, SourceUser)
	}
	return m.idleNext(now)
}
func (m *machine) deadline() time.Time {
	if m.busy != nil {
		if uiOperation(m.busy.Kind) {
			return m.memoryDeadline()
		}
		return m.busy.confirmDeadline
	}
	if m.memory.Reclaiming && m.memory.Awaiting && !m.state.SuspendDeadline.IsZero() && m.memory.deadline.After(m.state.SuspendDeadline) {
		return m.memoryDeadline()
	}
	a, b := m.state.DisplayDeadline, m.state.SuspendDeadline
	return earlier(earlier(earlier(a, b), m.state.LogoffDeadline), m.memoryDeadline())
}
