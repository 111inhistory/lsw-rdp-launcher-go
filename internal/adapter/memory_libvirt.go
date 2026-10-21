package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/111inhistory/lsw-rdp-launcher-go/internal/controller"
	"github.com/beevik/etree"
	libvirt "github.com/digitalocean/go-libvirt"
)

const (
	defaultMemoryAlias = "ua-virtiomem0"
	hostMemoryReserve  = uint64(1 << 30)
)

// memoryAPI is the small part of libvirt used by the memory adapter. Keeping
// it narrow lets the node XML and budget rules be tested without a live VM.
type memoryAPI interface {
	DomainLookupByName(string) (libvirt.Domain, error)
	DomainGetState(libvirt.Domain, uint32) (int32, int32, error)
	DomainGetXMLDesc(libvirt.Domain, libvirt.DomainXMLFlags) (string, error)
	DomainUpdateDeviceFlags(libvirt.Domain, string, libvirt.DomainDeviceModifyFlags) error
}

// memorySource is one acknowledged QEMU monitor-event subscription. The
// registration connection belongs to this stream only; control operations use
// separate connections.
type memorySource struct {
	events   <-chan libvirt.DomainEvent
	eventCtx context.Context
	close    func()
	once     sync.Once
}

func newMemorySource(events <-chan libvirt.DomainEvent, eventCtx context.Context, close func()) *memorySource {
	return &memorySource{events: events, eventCtx: eventCtx, close: close}
}

// shutdown cancels the stream, closes its connection and drains the SDK channel
// so a blocked sender cannot outlive the subscription.
func (s *memorySource) shutdown() {
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

// MemoryLibvirt owns the current acknowledged QEMU monitor-event subscription.
// If the stream breaks, Watch invalidates the observation, replaces it with a
// new subscription, and lets the controller query after the replacement ACK.
// Execute keeps using independent per-operation connections.
type MemoryLibvirt struct {
	domain  string
	alias   string
	open    func(context.Context) (memoryAPI, func(), error)
	meminfo func() (uint64, error)

	mu          sync.Mutex
	initial     *memorySource
	current     *memorySource
	subscribe   func(context.Context) (*memorySource, error)
	watchCancel context.CancelFunc
	closed      bool
}

func subscribeMemory(ctx context.Context, socket, uri, domain string) (*memorySource, error) {
	client, closeClient, err := connectLibvirt(ctx, socket, uri)
	if err != nil {
		return nil, err
	}
	eventCtx, cancel := context.WithCancel(ctx)
	events, err := client.SubscribeQEMUEvents(eventCtx, domain)
	if err != nil {
		cancel()
		closeClient()
		return nil, err
	}
	return newMemorySource(events, eventCtx, func() { cancel(); closeClient() }), nil
}

// OpenMemoryLibvirt registers QEMU events before the controller can issue a
// memory operation. The registration connection is dedicated to this stream.
func OpenMemoryLibvirt(ctx context.Context, socket, uri, domain, alias string) (*MemoryLibvirt, error) {
	if alias == "" {
		alias = defaultMemoryAlias
	}
	m := &MemoryLibvirt{
		domain: domain,
		alias:  alias,
		open: func(ctx context.Context) (memoryAPI, func(), error) {
			return connectLibvirt(ctx, socket, uri)
		},
		subscribe: func(ctx context.Context) (*memorySource, error) {
			return subscribeMemory(ctx, socket, uri, domain)
		},
		meminfo: readMemAvailable,
	}
	source, err := m.subscribe(ctx)
	if err != nil {
		return nil, err
	}
	m.initial = source
	return m, nil
}

func (m *MemoryLibvirt) acquire(ctx context.Context) (*memorySource, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, context.Canceled
	}
	if source := m.initial; source != nil {
		m.initial = nil
		m.current = source
		m.mu.Unlock()
		return source, nil
	}
	subscribe := m.subscribe
	m.mu.Unlock()
	if subscribe == nil {
		return nil, fmt.Errorf("libvirt QEMU event subscription is not configured")
	}
	source, err := subscribe(ctx)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		source.shutdown()
		return nil, context.Canceled
	}
	m.current = source
	m.mu.Unlock()
	return source, nil
}

func (m *MemoryLibvirt) release(source *memorySource) {
	m.mu.Lock()
	if m.current == source {
		m.current = nil
	}
	m.mu.Unlock()
	source.shutdown()
}

// Close is idempotent and also drains the SDK event channel. It only affects
// the event subscription; in-flight control operations own their connections.
func (m *MemoryLibvirt) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	cancel := m.watchCancel
	m.watchCancel = nil
	initial, current := m.initial, m.current
	m.initial, m.current = nil, nil
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	initial.shutdown()
	if current != initial {
		current.shutdown()
	}
}

func (m *MemoryLibvirt) Execute(ctx context.Context, op controller.Operation) controller.Outcome {
	api, close, err := m.open(ctx)
	if err != nil {
		return controller.Outcome{Err: err}
	}
	defer close()
	dom, err := api.DomainLookupByName(m.domain)
	if err != nil {
		return controller.Outcome{Err: err}
	}
	switch op.Kind {
	case controller.QueryMemory:
		device, _, err := m.readDevice(api, dom)
		if err != nil {
			return controller.Outcome{Err: err}
		}
		return controller.Outcome{Memory: &device}
	case controller.SetMemory:
		return m.setMemory(api, dom, op.MemoryTarget)
	case controller.PrepareMemory:
		return m.prepareResume(ctx, api, dom)
	default:
		return controller.Outcome{Err: fmt.Errorf("unsupported memory operation %s", op.Kind)}
	}
}

// setMemory re-reads the live XML for every request. It validates the request
// against the current device and only changes the live device. A successful
// RPC is an acknowledgement; the real size arrives later through Watch.
func (m *MemoryLibvirt) setMemory(api memoryAPI, dom libvirt.Domain, target uint64) controller.Outcome {
	state, _, err := api.DomainGetState(dom, 0)
	if err != nil {
		return controller.Outcome{Err: err}
	}
	if vmState(libvirt.DomainState(state)) != controller.VMRunning {
		return controller.Outcome{Err: fmt.Errorf("cannot set memory while VM is %s", vmState(libvirt.DomainState(state)))}
	}
	device, node, err := m.readDevice(api, dom)
	if err != nil {
		return controller.Outcome{Err: err}
	}
	if target > device.CapacityBytes {
		return controller.Outcome{Err: fmt.Errorf("memory target %d exceeds capacity %d", target, device.CapacityBytes)}
	}
	// The controller works on one fixed 128 MiB grid. The device block size is
	// validated too, but it does not replace this host-side alignment contract.
	if target%controller.MemoryQuantum != 0 {
		return controller.Outcome{Err: fmt.Errorf("memory target %d is not aligned to %d bytes", target, controller.MemoryQuantum)}
	}
	if device.BlockBytes == 0 {
		return controller.Outcome{Err: fmt.Errorf("memory device %s has no block size", device.Alias)}
	}
	if device.BlockBytes > controller.MemoryQuantum || controller.MemoryQuantum%device.BlockBytes != 0 {
		return controller.Outcome{Err: fmt.Errorf("memory device %s block size %d is incompatible with %d-byte alignment", device.Alias, device.BlockBytes, controller.MemoryQuantum)}
	}
	if target%device.BlockBytes != 0 {
		return controller.Outcome{Err: fmt.Errorf("memory target %d is not aligned to block size %d", target, device.BlockBytes)}
	}
	if target > device.ActualBytes {
		if err := m.checkBudget(target - device.ActualBytes); err != nil {
			return controller.Outcome{Err: err}
		}
	}
	deviceXML, err := updateMemoryXML(node, target)
	if err != nil {
		return controller.Outcome{Err: err}
	}
	if err = api.DomainUpdateDeviceFlags(dom, deviceXML, libvirt.DomainDeviceModifyLive); err != nil {
		return controller.Outcome{Err: err}
	}
	return controller.Outcome{}
}

func (m *MemoryLibvirt) checkBudget(growth uint64) error {
	if growth == 0 {
		return nil
	}
	var available uint64
	var err error
	if m.meminfo != nil {
		available, err = m.meminfo()
	} else {
		available, err = readMemAvailable()
	}
	if err != nil {
		return fmt.Errorf("read host MemAvailable: %w", err)
	}
	if available < hostMemoryReserve || available-hostMemoryReserve < growth {
		return fmt.Errorf("insufficient host memory budget: MemAvailable %d bytes, reserve %d bytes, growth %d bytes", available, hostMemoryReserve, growth)
	}
	return nil
}

// readDevice gets live XML and requires exactly one configured virtio-mem
// device with the requested alias. Returning the node lets SetMemory patch the
// same snapshot instead of looking the device up a second time.
func (m *MemoryLibvirt) readDevice(api memoryAPI, dom libvirt.Domain) (controller.MemoryDevice, *etree.Element, error) {
	raw, err := api.DomainGetXMLDesc(dom, 0)
	if err != nil {
		return controller.MemoryDevice{}, nil, err
	}
	document := etree.NewDocument()
	if err := document.ReadFromString(raw); err != nil {
		return controller.MemoryDevice{}, nil, err
	}
	var matches []*etree.Element
	for _, node := range document.FindElements("/domain/devices/memory[@model='virtio-mem']") {
		alias := node.SelectElement("alias")
		if alias != nil && alias.SelectAttrValue("name", "") == m.alias {
			matches = append(matches, node)
		}
	}
	switch len(matches) {
	case 0:
		return controller.MemoryDevice{}, nil, fmt.Errorf("no configured virtio-mem device with alias %q", m.alias)
	case 1:
	default:
		return controller.MemoryDevice{}, nil, fmt.Errorf("multiple virtio-mem devices with alias %q", m.alias)
	}
	device, err := memoryDevice(matches[0], m.alias)
	if err != nil {
		return controller.MemoryDevice{}, nil, err
	}
	return device, matches[0], nil
}

// Watch follows QEMU memory events for the session lifetime. A broken source
// invalidates the device observation, reconnects with transport backoff, and
// after the replacement ACK tells the controller that a fresh query is needed.
// Malformed events and emit failures are terminal; replaying or guessing them
// would turn a source defect into a false observation.
func (m *MemoryLibvirt) Watch(ctx context.Context, emit Sink) (err error) {
	ctx, cancel := context.WithCancel(ctx)
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		cancel()
		return context.Canceled
	}
	m.watchCancel = cancel
	m.mu.Unlock()
	defer func() {
		cancel()
		m.mu.Lock()
		m.watchCancel = nil
		m.mu.Unlock()
		m.Close()
	}()
	var fatal error
	// The first acquisition uses the subscription already acknowledged by Open.
	// Later acquisition failures follow the Ready:false emitted by the lost
	// attempt, so they do not need another invalidation.
	err = retrySource(ctx, "libvirt QEMU events", func(ctx context.Context) error {
		err := m.watchOnce(ctx, emit)
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

func (m *MemoryLibvirt) watchOnce(ctx context.Context, emit Sink) error {
	source, err := m.acquire(ctx)
	if err != nil {
		return err
	}
	defer m.release(source)

	// The subscription is acknowledged by subscribe. Only then may the
	// controller query the device; the disconnected interval has no observation.
	if err = emit(ctx, controller.MemorySourceObserved{Ready: true}); err != nil {
		return libvirtSourceFatalError{err}
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event, ok := <-source.events:
			if !ok {
				// Parent cancellation is shutdown, not a source loss. Any other
				// closed event stream must invalidate before reconnecting, even
				// if the subscription's own child context ended first.
				if err := ctx.Err(); err != nil {
					return err
				}
				cause := error(io.ErrUnexpectedEOF)
				if source.eventCtx != nil && source.eventCtx.Err() != nil {
					cause = source.eventCtx.Err()
				}
				if err := emit(ctx, controller.MemorySourceObserved{Ready: false}); err != nil {
					return libvirtSourceFatalError{err}
				}
				return fmt.Errorf("libvirt QEMU event stream: %w", cause)
			}
			if event.Event != "MEMORY_DEVICE_SIZE_CHANGE" {
				continue
			}
			if event.Domain.Name != "" && event.Domain.Name != m.domain {
				continue
			}
			// QMP's MEMORY_DEVICE_SIZE_CHANGE schema is {id, size}, and
			// libvirt makes id canonical with the device alias.
			var detail struct {
				ID   string  `json:"id"`
				Size *uint64 `json:"size"`
			}
			if err := json.Unmarshal(event.Details, &detail); err != nil {
				fault := fmt.Errorf("decode MEMORY_DEVICE_SIZE_CHANGE: %w", err)
				return m.sourceFault(ctx, emit, fault)
			}
			if detail.ID != m.alias {
				continue
			}
			if detail.Size == nil {
				fault := fmt.Errorf("MEMORY_DEVICE_SIZE_CHANGE for %s is missing size", detail.ID)
				return m.sourceFault(ctx, emit, fault)
			}
			if err := emit(ctx, controller.MemoryDeviceObserved{Alias: detail.ID, ActualBytes: *detail.Size}); err != nil {
				return libvirtSourceFatalError{err}
			}
		}
	}
}

// sourceFault reports a protocol fault as a sticky source error, then returns
// an ordinary error so only the observation stream reconnects. Business
// requests are not replayed. A failed emit is still terminal for Watch.
func (m *MemoryLibvirt) sourceFault(ctx context.Context, emit Sink, fault error) error {
	if err := emit(ctx, controller.MemorySourceObserved{Ready: false, Err: fault}); err != nil {
		return libvirtSourceFatalError{err}
	}
	return fault
}

func memoryDevice(node *etree.Element, alias string) (controller.MemoryDevice, error) {
	target := node.SelectElement("target")
	if target == nil {
		return controller.MemoryDevice{}, fmt.Errorf("virtio-mem device %s has no target", alias)
	}
	size := target.SelectElement("size")
	if size == nil {
		return controller.MemoryDevice{}, fmt.Errorf("virtio-mem device %s has no target size", alias)
	}
	capacity, err := xmlBytes(size)
	if err != nil {
		return controller.MemoryDevice{}, fmt.Errorf("virtio-mem device %s capacity: %w", alias, err)
	}
	var block uint64
	if element := target.SelectElement("block"); element != nil {
		block, err = xmlBytes(element)
		if err != nil {
			return controller.MemoryDevice{}, fmt.Errorf("virtio-mem device %s block: %w", alias, err)
		}
	}
	var requested uint64
	if element := target.SelectElement("requested"); element != nil {
		requested, err = xmlBytes(element)
		if err != nil {
			return controller.MemoryDevice{}, fmt.Errorf("virtio-mem device %s requested: %w", alias, err)
		}
	}
	current := target.SelectElement("current")
	if current == nil {
		// A successful update RPC is not evidence of convergence. Without the
		// live current size, requested must never be reported as actual.
		return controller.MemoryDevice{}, fmt.Errorf("unsupported virtio-mem device %s: missing live current size", alias)
	}
	actual, err := xmlBytes(current)
	if err != nil {
		return controller.MemoryDevice{}, fmt.Errorf("virtio-mem device %s current: %w", alias, err)
	}
	return controller.MemoryDevice{Alias: alias, CapacityBytes: capacity, BlockBytes: block, RequestedBytes: requested, ActualBytes: actual}, nil
}

func xmlBytes(node *etree.Element) (uint64, error) {
	text := strings.TrimSpace(node.Text())
	if text == "" {
		return 0, fmt.Errorf("empty size")
	}
	value, err := strconv.ParseUint(text, 10, 64)
	if err != nil {
		return 0, err
	}
	multiplier, err := xmlUnit(node.SelectAttrValue("unit", ""))
	if err != nil {
		return 0, err
	}
	// Live libvirt memory sizes are bounded by VM and host capacity. A uint64
	// wrap needs a converted size around 16 EiB, outside supported host scale.
	return value * multiplier, nil
}

func xmlUnit(unit string) (uint64, error) {
	switch strings.ToLower(strings.TrimSpace(unit)) {
	case "", "k", "kib":
		return 1 << 10, nil
	case "b", "byte", "bytes":
		return 1, nil
	case "m", "mib":
		return 1 << 20, nil
	case "g", "gib":
		return 1 << 30, nil
	default:
		return 0, fmt.Errorf("unsupported memory unit %q", unit)
	}
}

// updateMemoryXML replaces <requested> with exact bytes and drops the read-only
// <current> element. Everything else in the device XML is preserved.
func updateMemoryXML(device *etree.Element, target uint64) (string, error) {
	// Work on a copy; the read snapshot and unknown device extensions survive.
	device = device.Copy()
	targetNode := device.SelectElement("target")
	if targetNode == nil {
		return "", fmt.Errorf("memory device has no target")
	}
	for _, current := range targetNode.SelectElements("current") {
		targetNode.RemoveChild(current)
	}
	requested := targetNode.SelectElement("requested")
	if requested == nil {
		requested = targetNode.CreateElement("requested")
	}
	requested.CreateAttr("unit", "b")
	requested.SetText(strconv.FormatUint(target, 10))
	document := etree.NewDocument()
	document.SetRoot(device)
	return document.WriteToString()
}

// MemAvailable is the kernel's estimate of memory available for new work and
// includes reclaimable cache. Keep a fixed reserve for the host and never
// shrink on our own when the budget is insufficient.
func readMemAvailable() (uint64, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "MemAvailable:" {
			continue
		}
		kilobytes, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0, err
		}
		return kilobytes * (1 << 10), nil
	}
	return 0, fmt.Errorf("MemAvailable not found in /proc/meminfo")
}
