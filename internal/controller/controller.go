package controller

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Execute performs one external operation and must finish bounded cleanup when
// ctx is canceled. UI work may overlap memory I/O, never a second VM mutation.
// StartDisplay returns once the interactive endpoint is ready. Its process
// watcher later reports DisplayExited with the same Operation.ID.
// QueryVM returns a snapshot. WakeVM/SuspendVM only acknowledge the command;
// lifecycle observations confirm the transition in the main loop.
type Execute func(context.Context, Operation) Outcome

type Event interface{ controllerEvent() }
type VMObserved struct{ State VMState }

// Ready means the source subscription has been acknowledged, not that a cached
// VM/device snapshot is current. The controller queries after this event.
type VMSourceObserved struct{ Ready bool }
type MemorySourceObserved struct {
	Ready bool
	Err   error // Protocol/device faults stay sticky; plain transport loss uses nil.
}
type BackendObserved struct {
	Ready bool
	SSH   SSHState
}
type MemoryObserved struct{ Memory *GuestMemory }
type MemoryDeviceObserved struct {
	Alias       string
	ActualBytes uint64
	Lost        bool
}
type WindowsObserved struct{ Count int }
type DisplayExited struct {
	OperationID uint64
	Err         error
}

func (VMObserved) controllerEvent()           {}
func (VMSourceObserved) controllerEvent()     {}
func (MemorySourceObserved) controllerEvent() {}
func (BackendObserved) controllerEvent()      {}
func (MemoryObserved) controllerEvent()       {}
func (MemoryDeviceObserved) controllerEvent() {}
func (WindowsObserved) controllerEvent()      {}
func (DisplayExited) controllerEvent()        {}

type queueWake struct{}

type operationDone struct {
	id  uint64
	out Outcome
}
type lockRequest struct {
	locked bool
	reply  chan struct{}
}
type snapshotRequest struct{ reply chan Snapshot }

type Controller struct {
	opts      Options
	execute   Execute
	events    chan any
	lifecycle chan Event
	vmChanges chan VMState
	done      chan struct{}
}

func New(opts Options, execute Execute) *Controller {
	return &Controller{opts: opts, execute: execute, events: make(chan any, 64), lifecycle: make(chan Event, 16), vmChanges: make(chan VMState, 1), done: make(chan struct{})}
}

// VMChanges has one consumer: the backend source. It coalesces to the latest
// state so subscription refreshes never block business-state processing.
func (c *Controller) VMChanges() <-chan VMState { return c.vmChanges }

func (c *Controller) send(ctx context.Context, event any) error {
	select {
	case c.events <- event:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return context.Canceled
	}
}
func (c *Controller) Notify(ctx context.Context, event Event) error {
	switch event.(type) {
	case VMObserved, VMSourceObserved:
		select {
		case c.lifecycle <- event:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-c.done:
			return context.Canceled
		}
	}
	return c.send(ctx, event)
}
func (c *Controller) Do(ctx context.Context, req Request) Result {
	j := &job{request: req, ctx: ctx, reply: make(chan Result, 1)}
	if err := c.send(ctx, j); err != nil {
		return Result{Err: err}
	}
	// Cancellation wakes a queue that is waiting for backend readiness.
	stop := context.AfterFunc(ctx, func() { _ = c.send(context.WithoutCancel(ctx), queueWake{}) })
	defer stop()
	select {
	case result := <-j.reply:
		return result
	case <-ctx.Done():
		return Result{Err: ctx.Err()}
	case <-c.done:
		return Result{Err: context.Canceled}
	}
}
func (c *Controller) SetLocked(ctx context.Context, locked bool) error {
	reply := make(chan struct{}, 1)
	if err := c.send(ctx, lockRequest{locked, reply}); err != nil {
		return err
	}
	select {
	case <-reply:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return context.Canceled
	}
}
func (c *Controller) Snapshot(ctx context.Context) (Snapshot, error) {
	reply := make(chan Snapshot, 1)
	if err := c.send(ctx, snapshotRequest{reply}); err != nil {
		return Snapshot{}, err
	}
	select {
	case state := <-reply:
		return state, nil
	case <-ctx.Done():
		return Snapshot{}, ctx.Err()
	case <-c.done:
		return Snapshot{}, context.Canceled
	}
}

// Run has one owner goroutine. Only external I/O runs in a worker. A timer is
// armed for the nearest business deadline; there is no polling/retry loop.
func (c *Controller) Run(ctx context.Context) error {
	defer close(c.done)
	defer close(c.vmChanges)
	var publishedRevision uint64
	m := newMachine(c.opts)
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	cancelOperations := make(map[uint64]context.CancelFunc)
	var workers sync.WaitGroup
	defer func() {
		for _, cancel := range cancelOperations {
			cancel()
		}
		workers.Wait()
	}()
	apply := func(event any) {
		switch e := event.(type) {
		case queueWake: // Re-evaluate canceled jobs before choosing another operation.
		case *job:
			m.enqueue(e)
		case VMObserved:
			m.vm(e.State)
		case VMSourceObserved:
			m.vmSource(e.Ready)
		case MemorySourceObserved:
			m.memorySource(e.Ready)
			if e.Err != nil && (m.memory.LastError == nil || errors.Is(m.memory.LastError, ErrMemorySourceLost)) {
				m.memory.LastError = e.Err
			}
		case BackendObserved:
			m.backend(e.Ready, e.SSH)
		case MemoryObserved:
			m.memoryGuest(e.Memory)
		case MemoryDeviceObserved:
			m.memoryDevice(e)
		case WindowsObserved:
			m.windows(e.Count)
		case DisplayExited:
			m.displayExited(e.OperationID, e.Err)
		case operationDone:
			if cancel := cancelOperations[e.id]; cancel != nil {
				cancel()
				delete(cancelOperations, e.id)
			}
			m.done(e.id, e.out, time.Now())
		case lockRequest:
			m.lock(e.locked)
			e.reply <- struct{}{}
		case snapshotRequest:
			e.reply <- m.snapshot()
		}
	}
	drainLifecycle := func() {
		for n := len(c.lifecycle); n > 0; n-- {
			apply(<-c.lifecycle)
		}
	}
	var pending any
	for {
		// Lifecycle observations win over older operation results. Apply the
		// arrived request batch before considering an expired idle deadline.
		drainLifecycle()
		if pending != nil {
			apply(pending)
			pending = nil
		}
		for n := len(c.events); n > 0; n-- {
			drainLifecycle()
			apply(<-c.events)
		}
		drainLifecycle()
		if err := ctx.Err(); err != nil {
			return err
		}
		for {
			op := m.next(time.Now())
			completed := m.completed
			m.completed = nil
			for _, item := range completed {
				item.job.reply <- item.result
			}
			if op != nil {
				opCtx, cancel := context.WithCancel(ctx)
				cancelOperations[op.ID] = cancel
				var stop func() bool
				if op.job != nil {
					stop = context.AfterFunc(op.job.ctx, cancel)
				}
				workers.Add(1)
				go func(op Operation) {
					defer workers.Done()
					out := c.execute(opCtx, op)
					if stop != nil {
						stop()
					}
					_ = c.send(ctx, operationDone{op.ID, out})
				}(*op)
				break
			}
			if len(completed) == 0 {
				break
			}
		}
		if m.revision != publishedRevision {
			select {
			case <-c.vmChanges:
			default:
			}
			c.vmChanges <- m.state.VM
			publishedRevision = m.revision
		}
		var deadline <-chan time.Time
		if at := m.deadline(); !at.IsZero() {
			timer.Reset(time.Until(at))
			deadline = timer.C
		} else {
			timer.Stop()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case vm := <-c.lifecycle:
			apply(vm)
		case event := <-c.events:
			pending = event
		case <-deadline:
		}
	}
}
