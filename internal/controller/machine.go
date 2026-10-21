// Package controller implements the host's event-driven business core.
package controller

import (
	"context"
	"fmt"
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

type Requirement uint8

const (
	BackendOnly Requirement = iota
	Interactive
	Desktop
)

// The caller classifies the request. The machine does not infer user-session
// requirements from action names (for example, sync-apps versus mounts).
type Request struct {
	Action        string
	Target        string
	Params        string
	WorkingDir    string
	WithIcons     bool
	SwitchDisplay bool `json:"-"` // Set only by the checked host switch action.
	File          string
	Need          Requirement
}
type Result struct {
	Data any
	Err  error
}
type job struct {
	request Request
	ctx     context.Context
	reply   chan Result
}
type Options struct {
	IdleDisconnect, VMSuspend time.Duration
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

type Operation struct {
	ID                   uint64
	Kind                 Kind
	VM                   VMState
	Request              Request
	Activity             SessionActivity
	RequireEmpty         bool
	MemoryTarget         uint64
	ResumeSafe           bool
	MemoryPrepared       bool
	memoryRevision       uint64
	memorySourceRevision uint64
	revision             uint64
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
	needQuery                   bool
	vmSubscription              subscriptionState
	memorySourceRevision        uint64
	memorySourceLost            bool
	cleanupDisplay              bool
	autoFailed                  bool
	completed                   []completion
	memory                      memoryControl
	switching                   *displaySwitch
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
	m.queue = append(m.queue, j)
	m.resetTimers()
	m.autoFailed = false
	if m.state.VM == VMUnknown || m.vmSubscription == subscriptionNeedsSnapshot {
		m.needQuery = true
	}
}
func (m *machine) lock(locked bool) {
	m.state.Locked = locked
	m.state.SuspendDeadline = time.Time{}
	m.memoryCancelReclaim()
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
		return
	}
	m.vmSubscription = subscriptionNeedsSnapshot
	m.needQuery = true // Missed lifecycle events cannot be replayed.
}

func (m *machine) vm(state VMState) {
	if state == m.state.VM && state != VMUnknown {
		return
	}
	m.revision++
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
	if state == VMOff {
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
		m.state.SuspendDeadline = time.Time{}
	}
	m.state.BackendReady = ready
	m.state.SSH = ssh
	if !ready {
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
	m.state.DisplayDeadline = time.Time{}
	m.state.SuspendDeadline = time.Time{}
	m.memoryCancelReclaim()
}
func (m *machine) finish(j *job, result Result) {
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
		if op.Kind == QueryVM && op.revision != m.revision {
			return
		}
		preparing := m.memory.phase != memoryIdle
		if preparing {
			m.memoryAbort(out.Err)
		}
		m.state.LastError = out.Err
		// A caller failure must not disable idle management for the whole VM.
		// Only failed autonomous work is held to avoid an immediate retry loop.
		m.autoFailed = op.job == nil
		// A failed query waits for another request/observation, not a retry loop.
		if op.Kind == QueryVM {
			m.needQuery = false
			for len(m.queue) > 0 {
				m.finish(m.queue[0], Result{Err: out.Err})
			}
		} else if op.job != nil {
			m.finish(op.job, Result{Err: out.Err})
		} else if preparing {
			for len(m.queue) > 0 {
				m.finish(m.queue[0], Result{Err: out.Err})
			}
		}
		return
	}
	m.state.LastError = nil
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
		m.state.Display = true
		m.state.DisplayMode = "remoteapp"
		if op.Request.Need == Desktop {
			m.state.DisplayMode = "desktop"
		}
		m.displayID = op.ID
		m.cleanupDisplay = op.revision != m.revision
	case StopDisplay:
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
func (m *machine) begin(kind Kind, j *job) *Operation {
	m.nextID++
	op := &Operation{ID: m.nextID, Kind: kind, VM: m.state.VM, revision: m.revision, job: j}
	if j != nil {
		op.Request = j.request
		if m.switching != nil && m.switching.job == j {
			op.Activity = m.switching.activity
			op.RequireEmpty = m.switching.automatic
		}
	}
	op.memoryRevision = m.memory.revision
	op.memorySourceRevision = m.memorySourceRevision
	op.ResumeSafe = kind == WakeVM && m.memory.phase == memoryResuming && m.memory.resumeSafe
	op.MemoryPrepared = m.memory.WindowPrepared
	m.busy = op
	if kind != QueryMemory && kind != SetMemory {
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
			m.needQuery = false // Another request can query; no automatic polling.
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
		return m.begin(QueryVM, head)
	}
	if m.vmSubscription == subscriptionNeedsSnapshot {
		return nil // A failed refresh waits for a new request/observation, not a spin loop.
	}
	if m.memory.phase != memoryIdle {
		if m.memorySourceLost {
			return nil
		}
		return m.memoryPreparationNext()
	}
	if m.state.Display && (m.cleanupDisplay || (m.state.VM != VMRunning && m.state.VM != VMUnknown)) {
		if head != nil || !m.autoFailed {
			return m.begin(StopDisplay, head)
		}
		return nil
	}
	if op := m.memoryNext(now); op != nil {
		return op
	}
	if head != nil {
		if head.request.Need != BackendOnly && !head.request.SwitchDisplay && m.state.Display && (head.request.Need == Desktop) != (m.state.DisplayMode == "desktop") {
			m.finish(head, Result{Err: fmt.Errorf("%s display is already active; use desktop or remoteapp to switch", m.state.DisplayMode)})
			return nil
		}
		switch m.state.VM {
		case VMUnknown:
			return nil
		case VMPaused, VMOff, VMPMSuspended:
			if m.opts.Memory != nil && m.memorySourceLost {
				return nil
			}
			return m.begin(WakeVM, head)
		case VMRunning:
		default:
			m.finish(head, Result{Err: fmt.Errorf("cannot execute while VM is %s", m.state.VM)})
			return nil
		}
		if m.opts.Memory != nil && m.memory.LastError == nil && !m.memory.WindowPrepared {
			return nil
		}
		if head.request.SwitchDisplay && (!m.state.Display || (head.request.Need == Desktop) != (m.state.DisplayMode == "desktop")) {
			if op := m.switchNext(head); op != nil {
				return op
			}
			if len(m.queue) == 0 || m.queue[0] != head || m.switching == nil || m.switching.phase != switchConnect {
				return nil
			}
		}
		// The display may bootstrap the current Rust agent; neither readiness
		// condition implies an ordering dependency between backend and display.
		if head.request.Need != BackendOnly && !m.state.Display {
			return m.begin(StartDisplay, head)
		}
		if m.state.BackendReady || head.request.Need == Desktop {
			return m.begin(Dispatch, head)
		}
		return nil
	}
	if m.state.VM != VMRunning || m.autoFailed {
		m.resetTimers()
		return nil
	}
	if m.state.Display && m.state.DisplayMode != "desktop" && m.state.Windows == 0 && m.opts.IdleDisconnect > 0 {
		if m.state.DisplayDeadline.IsZero() {
			m.state.DisplayDeadline = now.Add(m.opts.IdleDisconnect)
		}
		if !now.Before(m.state.DisplayDeadline) {
			return m.begin(StopDisplay, nil)
		}
	} else {
		m.state.DisplayDeadline = time.Time{}
	}
	pressure := m.opts.Memory != nil && m.memory.GuestKnown && m.memory.Guest.atLeast(m.opts.Memory.StartPercent)
	memoryKnown := m.opts.Memory == nil || m.memory.GuestKnown
	if !m.state.Display && m.state.BackendReady && m.state.SSH == SSHZero && !m.state.Locked && memoryKnown && !m.memory.Growing && !pressure && m.opts.VMSuspend > 0 {
		if m.state.SuspendDeadline.IsZero() {
			m.state.SuspendDeadline = now.Add(m.opts.VMSuspend)
		}
		if !now.Before(m.state.SuspendDeadline) {
			m.memoryCancelReclaim()
			return m.begin(SuspendVM, nil)
		}
		if op := m.memoryReclaim(now); op != nil {
			return op
		}
	} else {
		m.state.SuspendDeadline = time.Time{}
		m.memoryCancelReclaim()
	}
	return nil
}
func (m *machine) deadline() time.Time {
	if m.busy != nil {
		if uiOperation(m.busy.Kind) {
			return m.memoryDeadline()
		}
		return m.busy.confirmDeadline
	}
	a, b := m.state.DisplayDeadline, m.state.SuspendDeadline
	return earlier(earlier(a, b), m.memoryDeadline())
}
