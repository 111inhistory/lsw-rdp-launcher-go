package controller

import (
	"errors"
	"fmt"
	"time"
)

const (
	MemoryQuantum       = uint64(128 * 1024 * 1024) // MiB, applied to the final device target.
	MemoryReclaimLead   = 30 * time.Second
	memoryResizeTimeout = 30 * time.Second
	memoryDrainDelay    = time.Second
	memoryDrainLimit    = 8
)

// MemoryPolicy has only the two hysteresis thresholds. A nil Options.Memory
// disables memory control; zero fields select 95/80. The guest has no policy.
type MemoryPolicy struct{ StartPercent, StopPercent int }

func (p MemoryPolicy) Defaults() MemoryPolicy {
	if p.StartPercent == 0 {
		p.StartPercent = 95
	}
	if p.StopPercent == 0 {
		p.StopPercent = 80
	}
	return p
}
func (p MemoryPolicy) Validate() error {
	p = p.Defaults()
	if p.StopPercent <= 0 || p.StartPercent > 100 || p.StopPercent >= p.StartPercent {
		return fmt.Errorf("memory thresholds require 0 < stop < start <= 100")
	}
	return nil
}

type GuestMemory struct {
	TotalBytes     uint64 `json:"total_bytes"`
	AvailableBytes uint64 `json:"available_bytes"`
}

func (g GuestMemory) valid() bool {
	return g.TotalBytes > 0 && g.AvailableBytes <= g.TotalBytes
}
func (g GuestMemory) atLeast(percent int) bool {
	return (g.TotalBytes-g.AvailableBytes)*100 >= g.TotalBytes*uint64(percent)
}
func (g GuestMemory) atMost(percent int) bool {
	return (g.TotalBytes-g.AvailableBytes)*100 <= g.TotalBytes*uint64(percent)
}

type MemoryDevice struct {
	Alias                                                  string
	CapacityBytes, BlockBytes, RequestedBytes, ActualBytes uint64
}
type memoryPhase uint8

const (
	memoryIdle memoryPhase = iota
	memoryPausing
	memoryWindow
	memoryResuming
)

type MemoryState struct {
	Operation                     Kind
	Guest                         GuestMemory
	GuestKnown                    bool
	Device                        MemoryDevice
	DeviceKnown                   bool
	Growing, Awaiting, Reclaiming bool
	Preparing, WindowPrepared     bool
	TargetBytes                   uint64
	LastError                     error // Resize/safety failures are sticky; only source-loss errors clear on reconnect.
}

// A transport loss is recoverable; write, driver and resume-safety failures are not.
var ErrMemorySourceLost = errors.New("memory event source lost")

type memoryControl struct {
	MemoryState
	revision     uint64
	needQuery    bool // One post-subscription read is allowed even while writes stay disabled.
	beforeTotal  uint64
	waitGuest    bool
	deadline     time.Time
	reclaimedFor time.Time
	phase        memoryPhase
	resumeSafe   bool
	lastPoked    uint64
	pokeAt       time.Time
	pokes        int
}

// GrowthTarget accounts for Windows-visible RAM separately from QEMU plugged
// bytes. Rounding only the increment would preserve an unaligned reclaim tail.
func GrowthTarget(g GuestMemory, d MemoryDevice, stopPercent int) (uint64, error) {
	if !g.valid() || stopPercent <= 0 || stopPercent >= 100 || d.BlockBytes == 0 ||
		d.BlockBytes > MemoryQuantum || MemoryQuantum%d.BlockBytes != 0 || d.ActualBytes > d.CapacityBytes {
		return 0, fmt.Errorf("unsupported memory sample/device or stop threshold")
	}
	used := g.TotalBytes - g.AvailableBytes
	wantedTotal := (used*100 + uint64(stopPercent) - 1) / uint64(stopPercent)
	if wantedTotal <= g.TotalBytes {
		return d.ActualBytes, nil
	}
	extra := wantedTotal - g.TotalBytes
	target := (d.ActualBytes + extra + MemoryQuantum - 1) / MemoryQuantum * MemoryQuantum
	capacity := d.CapacityBytes / MemoryQuantum * MemoryQuantum
	return min(target, capacity), nil
}
func (m *machine) memoryGuest(g *GuestMemory) {
	mem := &m.memory
	if m.state.VM != VMRunning {
		mem.GuestKnown, mem.Growing = false, false
		return
	}
	mem.GuestKnown = g != nil && g.valid()
	if !mem.GuestKnown {
		mem.Growing = false
		return
	}
	mem.Guest = *g
	if m.opts.Memory == nil || mem.LastError != nil {
		return
	}
	if mem.Growing {
		if g.atMost(m.opts.Memory.StopPercent) {
			mem.Growing = false
		}
	} else if g.atLeast(m.opts.Memory.StartPercent) {
		mem.Growing = true
		m.state.SuspendDeadline = time.Time{}
	}
	if mem.waitGuest && (g.TotalBytes > mem.beforeTotal || !mem.Growing) {
		mem.waitGuest = false
	}
	m.memoryProgress()
}
func (m *machine) memorySource(ready bool) {
	mem := &m.memory
	m.memorySourceRevision++
	m.memorySourceLost = !ready
	mem.DeviceKnown = false // Every new subscription needs a fresh QueryMemory.
	if !ready {
		if mem.LastError == nil || errors.Is(mem.LastError, ErrMemorySourceLost) {
			mem.LastError = ErrMemorySourceLost
		}
		mem.Awaiting, mem.Growing = false, false
		mem.deadline, mem.pokeAt = time.Time{}, time.Time{}
		return
	}
	mem.needQuery = true // One read-only refresh is allowed even with a sticky write fault.
	if errors.Is(mem.LastError, ErrMemorySourceLost) {
		mem.LastError = nil
	}
	if mem.GuestKnown {
		m.memoryGuest(&mem.Guest)
	}
}

func (m *machine) memoryDevice(e MemoryDeviceObserved) { m.memoryDeviceAt(e, time.Now()) }
func (m *machine) memoryDeviceAt(e MemoryDeviceObserved, now time.Time) {
	mem := &m.memory
	if e.Lost {
		m.memorySource(false)
		return
	}
	if m.state.VM != VMRunning || (mem.Device.Alias != "" && mem.Device.Alias != e.Alias) {
		return
	}
	if mem.Awaiting && mem.TargetBytes > 0 && e.ActualBytes > mem.Device.ActualBytes && e.ActualBytes < mem.TargetBytes {
		mem.pokeAt = now.Add(memoryDrainDelay)
	}
	mem.revision++
	mem.Device.Alias = e.Alias
	mem.Device.ActualBytes = e.ActualBytes
	m.memoryProgress()
}
func (m *machine) memoryProgress() {
	mem := &m.memory
	if !mem.Awaiting {
		return
	}
	if mem.TargetBytes > 0 && mem.GuestKnown && !mem.Growing && mem.Guest.atMost(m.opts.Memory.StopPercent) {
		mem.Awaiting = false
		mem.deadline = time.Time{}
		return
	}
	if mem.TargetBytes == 0 {
		if mem.Device.ActualBytes != 0 {
			return
		}
	} else if mem.Device.ActualBytes < mem.TargetBytes || mem.waitGuest {
		return
	}
	mem.Awaiting = false
	mem.deadline = time.Time{}
}
func (m *machine) memoryDone(op *Operation, out Outcome) {
	mem := &m.memory
	if op.memorySourceRevision != m.memorySourceRevision {
		// An old query cannot make a new subscription current. Keep genuine
		// write errors sticky even if the event connection was also interrupted.
		if out.Err != nil && op.Kind != QueryMemory {
			mem.LastError = out.Err
			mem.Awaiting, mem.Growing = false, false
			mem.deadline, mem.pokeAt = time.Time{}, time.Time{}
		}
		if op.Kind == PrepareMemory {
			m.memoryAbort(fmt.Errorf("memory source changed during preparation"))
		}
		return
	}
	if op.revision != m.revision {
		if op.Kind == PrepareMemory {
			m.memoryAbort(fmt.Errorf("VM changed during memory preparation"))
		}
		return
	}
	if op.Kind == PrepareMemory {
		if out.Err == nil && out.ResumeSafe {
			mem.WindowPrepared = true
		} else {
			mem.LastError = out.Err
			if mem.LastError == nil {
				mem.LastError = fmt.Errorf("memory preparation did not confirm a safe resume target")
			}
			if !out.ResumeSafe {
				mem.LastError = fmt.Errorf("safe memory resume target not confirmed; VM remains paused; recovery: docs/memory.md#recovery: %w", mem.LastError)
				m.memoryAbort(mem.LastError)
				for len(m.queue) > 0 {
					m.finish(m.queue[0], Result{Err: mem.LastError})
				}
				return
			}
		}
		mem.phase, mem.resumeSafe = memoryResuming, true
		return
	}
	if out.Err != nil {
		if op.Kind != QueryMemory || mem.LastError == nil || errors.Is(mem.LastError, ErrMemorySourceLost) {
			mem.LastError = out.Err
		}
		mem.Awaiting, mem.Growing = false, false
		mem.deadline = time.Time{}
		return
	}
	if op.Kind == QueryMemory {
		if out.Memory == nil {
			mem.LastError = fmt.Errorf("missing memory device snapshot")
			return
		}
		actual := mem.Device.ActualBytes
		mem.Device = *out.Memory
		if op.memoryRevision != mem.revision {
			mem.Device.ActualBytes = actual
		}
		mem.DeviceKnown = true
	} else {
		mem.Device.RequestedBytes = op.MemoryTarget // Accepted, not converged.
		m.memoryProgress()
	}
}
func (m *machine) memoryExpire(now time.Time) {
	mem := &m.memory
	if !mem.Awaiting || now.Before(mem.deadline) {
		return
	}
	mem.Awaiting = false
	mem.deadline = time.Time{}
	if mem.TargetBytes != 0 {
		mem.LastError = fmt.Errorf("memory growth stalled: requested=%d actual=%d guest_total=%d; automatic writes stopped", mem.TargetBytes, mem.Device.ActualBytes, mem.Guest.TotalBytes)
		mem.Growing = false
	}
	// A partial reclaim is normal: pause at the original deadline, retain actual.
}
func (m *machine) memorySet(target uint64, now time.Time) *Operation {
	mem := &m.memory
	continuing := mem.Awaiting && target > 0 && target == mem.TargetBytes
	op := m.beginMemory(SetMemory)
	op.MemoryTarget = target
	if continuing {
		mem.pokes++
		mem.lastPoked = mem.Device.ActualBytes
		mem.pokeAt = time.Time{}
		return op
	}
	mem.pokes, mem.lastPoked, mem.pokeAt = 0, mem.Device.ActualBytes, time.Time{}
	mem.TargetBytes, mem.Awaiting = target, true
	mem.beforeTotal = mem.Guest.TotalBytes
	mem.waitGuest = target > mem.Device.ActualBytes
	mem.deadline = now.Add(memoryResizeTimeout)
	if target == 0 {
		mem.deadline = m.state.SuspendDeadline
	}
	return op
}
func (m *machine) memoryNext(now time.Time) *Operation {
	mem := &m.memory
	if m.opts.Memory == nil || m.memorySourceLost || m.state.VM != VMRunning {
		return nil
	}
	if !mem.DeviceKnown && (mem.LastError == nil || mem.needQuery) {
		mem.needQuery = false
		return m.beginMemory(QueryMemory)
	}
	if mem.LastError != nil {
		return nil
	}
	if !m.state.BackendReady || !mem.GuestKnown {
		return nil
	}
	if !mem.WindowPrepared {
		mem.phase = memoryPausing
		return m.begin(SuspendVM, nil)
	}
	if !mem.Growing {
		return nil
	}
	// A pressure transition supersedes reclaim; it does not wait for target0.
	if mem.Awaiting && mem.TargetBytes > 0 {
		// WORKAROUND: nudge partial Windows growth by reissuing the same target,
		// but only after observed progress. This is not a higher growth request
		// or a general retry loop; memorySet preserves the original deadline.
		if mem.WindowPrepared && mem.pokes < memoryDrainLimit && mem.Device.ActualBytes > mem.lastPoked && !mem.pokeAt.IsZero() && !now.Before(mem.pokeAt) {
			return m.memorySet(mem.TargetBytes, now)
		}
		return nil
	}
	mem.Reclaiming = false
	target, err := GrowthTarget(mem.Guest, mem.Device, m.opts.Memory.StopPercent)
	if err != nil {
		mem.LastError = err
		return nil
	}
	if target <= mem.Device.ActualBytes {
		return nil
	} // Capacity reached; no retry loop.
	return m.memorySet(target, now)
}
func (m *machine) memoryReclaim(now time.Time) *Operation {
	mem := &m.memory
	if m.opts.Memory == nil || mem.LastError != nil || !mem.DeviceKnown || !mem.GuestKnown || mem.Growing ||
		m.state.SuspendDeadline.IsZero() || mem.reclaimedFor == m.state.SuspendDeadline {
		return nil
	}
	if now.Before(m.state.SuspendDeadline.Add(-MemoryReclaimLead)) {
		return nil
	}
	mem.reclaimedFor = m.state.SuspendDeadline
	mem.Reclaiming = true
	if mem.Device.RequestedBytes == 0 {
		return nil
	}
	return m.memorySet(0, now)
}
func (m *machine) memoryCancelReclaim() {
	mem := &m.memory
	if mem.Reclaiming {
		mem.Reclaiming = false
		if mem.TargetBytes == 0 {
			mem.Awaiting = false
			mem.deadline = time.Time{}
		}
	}
	// Do not roll back pages already removed or invent a fixed working target.
}
func earlier(a, b time.Time) time.Time {
	if a.IsZero() || (!b.IsZero() && b.Before(a)) {
		return b
	}
	return a
}
func (m *machine) memoryDeadline() time.Time {
	mem := &m.memory
	at := mem.deadline
	if mem.Awaiting && mem.TargetBytes > 0 && mem.Growing && mem.GuestKnown && m.state.BackendReady && mem.LastError == nil && mem.WindowPrepared && mem.pokes < memoryDrainLimit && mem.Device.ActualBytes > mem.lastPoked {
		at = earlier(at, mem.pokeAt)
	}
	if m.opts.Memory != nil && mem.LastError == nil && mem.DeviceKnown && mem.GuestKnown && !mem.Growing &&
		!m.state.SuspendDeadline.IsZero() && mem.reclaimedFor != m.state.SuspendDeadline {
		at = earlier(at, m.state.SuspendDeadline.Add(-MemoryReclaimLead))
	}
	return at
}

func uiOperation(kind Kind) bool {
	return kind == StartDisplay || kind == StopDisplay || kind == Dispatch || kind == InspectSession || kind == ConfirmSwitch || kind == LogoffSession
}

// Only UI/RPC work may overlap memory I/O. All VM mutations still share one
// control slot; preparation finishes before the first UI operation can start.
func (m *machine) beginMemory(kind Kind) *Operation {
	if m.busy == nil {
		return m.begin(kind, nil)
	}
	ui := m.busy
	op := m.begin(kind, nil)
	m.busy, m.memoryBusy = ui, op
	return op
}

func (m *machine) memoryAbort(err error) {
	mem := &m.memory
	mem.phase = memoryIdle
	mem.LastError = err
	mem.Growing, mem.Awaiting = false, false
	mem.deadline = time.Time{}
}
func (m *machine) memoryPreparationNext() *Operation {
	switch m.memory.phase {
	case memoryWindow:
		if m.state.VM == VMPaused {
			return m.begin(PrepareMemory, nil)
		}
	case memoryResuming:
		if m.state.VM == VMPaused {
			return m.begin(WakeVM, nil)
		}
	}
	m.memoryAbort(fmt.Errorf("unexpected VM state %s during memory preparation", m.state.VM))
	return nil
}
