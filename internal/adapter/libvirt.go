package adapter

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/111inhistory/lsw-rdp-launcher-go/internal/controller"
	libvirt "github.com/digitalocean/go-libvirt"
)

type Sink func(context.Context, controller.Event) error

type domainAPI interface {
	DomainLookupByName(string) (libvirt.Domain, error)
	DomainGetState(libvirt.Domain, uint32) (int32, int32, error)
	DomainCreate(libvirt.Domain) error
	DomainResume(libvirt.Domain) error
	DomainPmWakeup(libvirt.Domain, uint32) error
	DomainSuspend(libvirt.Domain) error
}

// libvirtSourceFatalError marks a failure that reconnecting cannot repair, such as an
// emit failure or a malformed event. Transport loss is retried instead.
type libvirtSourceFatalError struct{ err error }

func (e libvirtSourceFatalError) Error() string { return e.err.Error() }
func (e libvirtSourceFatalError) Unwrap() error { return e.err }

// lifecycleSource is one acknowledged lifecycle subscription. Each attempt
// owns its connection; replacing the source never affects control operations.
type lifecycleSource struct {
	events <-chan libvirt.DomainEventLifecycleMsg
	close  func()
	once   sync.Once
}

func newLifecycleSource(events <-chan libvirt.DomainEventLifecycleMsg, close func()) *lifecycleSource {
	return &lifecycleSource{events: events, close: close}
}

// shutdown closes the subscription and drains the SDK channel. The library can
// be blocked sending an event after the connection is closed, so draining is
// part of the close contract rather than optional cleanup.
func (s *lifecycleSource) shutdown() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		if s.close != nil {
			s.close()
		}
		if s.events != nil {
			for range s.events {
			}
		}
	})
}

// Libvirt uses the library's acknowledged lifecycle subscription. OpenLibvirt
// must succeed before starting the controller: no command races registration.
// If the stream later breaks, Watch invalidates the observation, reconnects,
// then tells the controller to query after the replacement ACK. Execute keeps
// owning an independent connection for every request.
type Libvirt struct {
	domain string
	open   func(context.Context) (domainAPI, func(), error)

	mu          sync.Mutex
	initial     *lifecycleSource
	current     *lifecycleSource
	subscribe   func(context.Context) (*lifecycleSource, error)
	watchCancel context.CancelFunc
	closed      bool
}

func connectLibvirt(ctx context.Context, socket, uri string) (*libvirt.Libvirt, func(), error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, nil, err
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	close := func() { stop(); conn.Close() }
	client := libvirt.New(conn)
	if err = client.ConnectToURI(libvirt.ConnectURI(uri)); err != nil {
		close()
		return nil, nil, err
	}
	return client, close, nil
}

func subscribeLifecycle(ctx context.Context, socket, uri string) (*lifecycleSource, error) {
	client, close, err := connectLibvirt(ctx, socket, uri)
	if err != nil {
		return nil, err
	}
	events, err := client.LifecycleEvents(ctx)
	if err != nil {
		close()
		return nil, err
	}
	return newLifecycleSource(events, close), nil
}

func OpenLibvirt(ctx context.Context, socket, uri, domain string) (*Libvirt, error) {
	v := &Libvirt{
		domain: domain,
		open: func(ctx context.Context) (domainAPI, func(), error) {
			return connectLibvirt(ctx, socket, uri)
		},
		subscribe: func(ctx context.Context) (*lifecycleSource, error) {
			return subscribeLifecycle(ctx, socket, uri)
		},
	}
	source, err := v.subscribe(ctx)
	if err != nil {
		return nil, err
	}
	v.initial = source
	return v, nil
}

// Mutation RPCs own independent connections and never reset the lifecycle
// stream. A failed QueryVM instead invalidates its observation attempt so Watch
// retries the subscription and snapshot together, without replaying mutations.
func (v *Libvirt) Execute(ctx context.Context, op controller.Operation) (out controller.Outcome) {
	if op.Kind == controller.QueryVM {
		v.mu.Lock()
		source := v.current
		if source == nil {
			source = v.initial
		}
		v.mu.Unlock()
		defer func() {
			// Explicit cancellation belongs to the operation owner, not the
			// observation source. A query deadline is still an observation
			// failure and must recover through the normal source backoff.
			if out.Err == nil || source == nil || ctx.Err() == context.Canceled {
				return
			}
			v.mu.Lock()
			current := v.current == source || v.initial == source
			v.mu.Unlock()
			if current {
				// A failed snapshot invalidates this observation attempt. Watch
				// emits Ready:false and retries with backoff, then ACKs a new
				// subscription before the controller queries again. Never close
				// a replacement source for an older query's late failure.
				source.shutdown()
			}
		}()
	}
	api, close, err := v.open(ctx)
	if err != nil {
		return controller.Outcome{Err: err}
	}
	defer close()
	dom, err := api.DomainLookupByName(v.domain)
	if err != nil {
		return controller.Outcome{Err: err}
	}
	switch op.Kind {
	case controller.QueryVM:
		state, _, err := api.DomainGetState(dom, 0)
		return controller.Outcome{VM: vmState(libvirt.DomainState(state)), Err: err}
	case controller.WakeVM:
		switch op.VM {
		case controller.VMOff:
			err = api.DomainCreate(dom)
		case controller.VMPaused:
			err = api.DomainResume(dom)
		case controller.VMPMSuspended:
			err = api.DomainPmWakeup(dom, 0)
		default:
			err = fmt.Errorf("cannot wake VM in %s", op.VM)
		}
	case controller.SuspendVM:
		err = api.DomainSuspend(dom)
	default:
		err = fmt.Errorf("unsupported VM operation %s", op.Kind)
	}
	// Successful mutation means accepted, never a synthesized VM state.
	return controller.Outcome{Err: err}
}

func (v *Libvirt) acquire(ctx context.Context) (*lifecycleSource, error) {
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		return nil, context.Canceled
	}
	if source := v.initial; source != nil {
		v.initial = nil
		v.current = source
		v.mu.Unlock()
		return source, nil
	}
	subscribe := v.subscribe
	v.mu.Unlock()
	if subscribe == nil {
		return nil, fmt.Errorf("libvirt lifecycle subscription is not configured")
	}
	source, err := subscribe(ctx)
	if err != nil {
		return nil, err
	}
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		source.shutdown()
		return nil, context.Canceled
	}
	v.current = source
	v.mu.Unlock()
	return source, nil
}

func (v *Libvirt) release(source *lifecycleSource) {
	v.mu.Lock()
	if v.current == source {
		v.current = nil
	}
	v.mu.Unlock()
	source.shutdown()
}

func (v *Libvirt) Close() {
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		return
	}
	v.closed = true
	cancel := v.watchCancel
	v.watchCancel = nil
	initial, current := v.initial, v.current
	v.initial, v.current = nil, nil
	v.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	initial.shutdown()
	if current != initial {
		current.shutdown()
	}
}

// Watch follows lifecycle events for the session lifetime. A broken source
// invalidates the VM observation, reconnects with transport backoff, and only
// after the replacement ACK tells the controller that a fresh query is needed.
func (v *Libvirt) Watch(ctx context.Context, emit Sink) error {
	ctx, cancel := context.WithCancel(ctx)
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		cancel()
		return context.Canceled
	}
	v.watchCancel = cancel
	v.mu.Unlock()
	defer func() {
		cancel()
		v.mu.Lock()
		v.watchCancel = nil
		v.mu.Unlock()
		v.Close()
	}()
	var fatal error
	// The first acquisition uses the subscription already acknowledged by Open.
	// Later acquisition failures follow the Ready:false emitted by the lost
	// attempt, so they do not need another invalidation.
	err := retrySource(ctx, "libvirt lifecycle", func(ctx context.Context) error {
		err := v.watchOnce(ctx, emit)
		var sourceErr libvirtSourceFatalError
		if errors.As(err, &sourceErr) {
			fatal = sourceErr.err
			return nil // retrySource treats nil as a deliberate stop.
		}
		return err
	})
	if fatal != nil {
		return fatal
	}
	return err
}

func (v *Libvirt) watchOnce(ctx context.Context, emit Sink) error {
	source, err := v.acquire(ctx)
	if err != nil {
		return err
	}
	defer v.release(source)

	// The subscription is acknowledged by subscribe. Tell the controller only
	// afterwards that it may query; the disconnected interval stays VMUnknown.
	if err = emit(ctx, controller.VMSourceObserved{Ready: true}); err != nil {
		return libvirtSourceFatalError{err}
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event, ok := <-source.events:
			if !ok {
				if err := ctx.Err(); err != nil {
					return err
				}
				if err := emit(ctx, controller.VMSourceObserved{Ready: false}); err != nil {
					return libvirtSourceFatalError{err}
				}
				return fmt.Errorf("libvirt lifecycle stream closed")
			}
			if event.Dom.Name != v.domain {
				continue
			}
			if state, ok := lifecycleState(libvirt.DomainEventType(event.Event)); ok {
				if err := emit(ctx, controller.VMObserved{State: state}); err != nil {
					return libvirtSourceFatalError{err}
				}
			}
		}
	}
}

func vmState(state libvirt.DomainState) controller.VMState {
	switch state {
	case libvirt.DomainRunning, libvirt.DomainBlocked:
		return controller.VMRunning
	case libvirt.DomainPaused:
		return controller.VMPaused
	case libvirt.DomainShutoff:
		return controller.VMOff
	case libvirt.DomainPmsuspended:
		return controller.VMPMSuspended
	case libvirt.DomainShutdown:
		return controller.VMShuttingDown
	case libvirt.DomainCrashed:
		return controller.VMCrashed
	default:
		return controller.VMUnknown
	}
}
func lifecycleState(event libvirt.DomainEventType) (controller.VMState, bool) {
	switch event {
	case libvirt.DomainEventStarted, libvirt.DomainEventResumed:
		return controller.VMRunning, true
	case libvirt.DomainEventSuspended:
		return controller.VMPaused, true
	case libvirt.DomainEventStopped:
		return controller.VMOff, true
	case libvirt.DomainEventShutdown:
		return controller.VMShuttingDown, true
	case libvirt.DomainEventPmsuspended:
		return controller.VMPMSuspended, true
	case libvirt.DomainEventCrashed:
		return controller.VMCrashed, true
	default:
		return controller.VMUnknown, false
	}
}
