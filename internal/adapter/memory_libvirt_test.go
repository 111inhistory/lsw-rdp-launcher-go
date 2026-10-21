package adapter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/111inhistory/lsw-rdp-launcher-go/internal/controller"
	"github.com/beevik/etree"
	libvirt "github.com/digitalocean/go-libvirt"
)

const (
	testMiB = uint64(1 << 20)
	testGiB = uint64(1 << 30)

	sampleMemoryDevice = `<memory model='virtio-mem' access='shared' discard='yes'>` +
		`<target><size unit='GiB'>13</size><node>0</node><block unit='MiB'>2</block>` +
		`<requested unit='GiB'>1</requested><current unit='GiB'>1</current></target>` +
		`<alias name='ua-virtiomem0'/>` +
		`<address type='pci' domain='0x0000' bus='0x00' slot='0x08' function='0x0'/>` +
		`</memory>`
)

func parseTestMemoryXML(raw string) (*etree.Element, error) {
	doc := etree.NewDocument()
	err := doc.ReadFromString(raw)
	return doc.Root(), err
}

func memoryDomain(devices ...string) string {
	return `<domain type='kvm'><name>win11</name><memory unit='GiB'>3</memory><devices>` +
		strings.Join(devices, "") + `</devices></domain>`
}

type fakeMemoryAPI struct {
	state       libvirt.DomainState
	xml         string
	getXMLCalls int
	updateCalls int
	updateXML   string
	updateFlags libvirt.DomainDeviceModifyFlags
	lookupErr   error
	stateErr    error
	getXMLErr   error
	updateErr   error
}

func (f *fakeMemoryAPI) DomainLookupByName(name string) (libvirt.Domain, error) {
	return libvirt.Domain{Name: name}, f.lookupErr
}
func (f *fakeMemoryAPI) DomainGetState(libvirt.Domain, uint32) (int32, int32, error) {
	return int32(f.state), 0, f.stateErr
}
func (f *fakeMemoryAPI) DomainGetXMLDesc(libvirt.Domain, libvirt.DomainXMLFlags) (string, error) {
	f.getXMLCalls++
	return f.xml, f.getXMLErr
}
func (f *fakeMemoryAPI) DomainUpdateDeviceFlags(_ libvirt.Domain, document string, flags libvirt.DomainDeviceModifyFlags) error {
	f.updateCalls++
	f.updateXML = document
	f.updateFlags = flags
	return f.updateErr
}

func fakeMemory(f *fakeMemoryAPI) *MemoryLibvirt {
	if f.xml == "" {
		f.xml = memoryDomain(sampleMemoryDevice)
	}
	return &MemoryLibvirt{
		domain: "win11",
		alias:  "ua-virtiomem0",
		open: func(context.Context) (memoryAPI, func(), error) {
			return f, func() {}, nil
		},
		meminfo: func() (uint64, error) { return 8 * testGiB, nil },
	}
}

func TestMemoryQueryLiveDevice(t *testing.T) {
	f := &fakeMemoryAPI{state: libvirt.DomainRunning}
	out := fakeMemory(f).Execute(context.Background(), controller.Operation{Kind: controller.QueryMemory})
	if out.Err != nil || out.Memory == nil {
		t.Fatal(out)
	}
	device := *out.Memory
	if device.Alias != "ua-virtiomem0" || device.CapacityBytes != 13*testGiB ||
		device.BlockBytes != 2*testMiB || device.RequestedBytes != testGiB || device.ActualBytes != testGiB {
		t.Fatal(device)
	}
	if f.getXMLCalls != 1 || f.updateCalls != 0 {
		t.Fatal(f.getXMLCalls, f.updateCalls)
	}
}

func TestMemoryQueryNonGridActual(t *testing.T) {
	actual := testGiB + 512*(1<<10) // deliberately not 2 MiB or 128 MiB aligned
	f := &fakeMemoryAPI{xml: memoryDomain(strings.Replace(sampleMemoryDevice,
		`<current unit='GiB'>1</current>`, `<current unit='B'>1074266112</current>`, 1))}
	out := fakeMemory(f).Execute(context.Background(), controller.Operation{Kind: controller.QueryMemory})
	if out.Err != nil || out.Memory == nil || out.Memory.ActualBytes != actual {
		t.Fatal(out, actual)
	}
	if out.Memory.RequestedBytes != testGiB {
		t.Fatal(out.Memory)
	}
}

func TestMemoryQueryDefaultKiB(t *testing.T) {
	f := &fakeMemoryAPI{xml: memoryDomain(
		`<memory model='virtio-mem'><target><size>13631488</size><node>0</node>` +
			`<block>2048</block><requested>1048576</requested>` +
			`<current>1048576</current></target>` +
			`<alias name='ua-virtiomem0'/></memory>`)}
	m := fakeMemory(f)
	out := m.Execute(context.Background(), controller.Operation{Kind: controller.QueryMemory})
	if out.Err != nil || out.Memory == nil {
		t.Fatal(out)
	}
	device := *out.Memory
	if device.Alias != defaultMemoryAlias || device.CapacityBytes != 13*testGiB ||
		device.BlockBytes != 2*testMiB || device.RequestedBytes != testGiB || device.ActualBytes != testGiB {
		t.Fatal(device)
	}
}

func TestMemoryQueryRejectsMissingCurrent(t *testing.T) {
	device := strings.Replace(sampleMemoryDevice, `<current unit='GiB'>1</current>`, "", 1)
	f := &fakeMemoryAPI{xml: memoryDomain(device)}
	out := fakeMemory(f).Execute(context.Background(), controller.Operation{Kind: controller.QueryMemory})
	if out.Err == nil || out.Memory != nil || !strings.Contains(out.Err.Error(), "current") {
		t.Fatal(out)
	}
}

func TestMemoryQueryRejectsBadAliasModelAndDuplicates(t *testing.T) {
	for _, tc := range []struct {
		name   string
		device string
		alias  string
	}{
		{"bad alias", strings.Replace(sampleMemoryDevice, "ua-virtiomem0", "ua-other", 1), "ua-virtiomem0"},
		{"default alias bad", strings.Replace(sampleMemoryDevice, "ua-virtiomem0", "ua-other", 1), ""},
		{"unsupported model", strings.Replace(sampleMemoryDevice, "virtio-mem", "dimm", 1), "ua-virtiomem0"},
		{"duplicate", sampleMemoryDevice + sampleMemoryDevice, "ua-virtiomem0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeMemoryAPI{xml: memoryDomain(tc.device)}
			m := fakeMemory(f)
			m.alias = tc.alias
			out := m.Execute(context.Background(), controller.Operation{Kind: controller.QueryMemory})
			if out.Err == nil || out.Memory != nil || f.updateCalls != 0 {
				t.Fatal(out, f.updateCalls)
			}
		})
	}
}

func TestMemorySetPreservesXMLAndUsesLiveOnly(t *testing.T) {
	f := &fakeMemoryAPI{state: libvirt.DomainRunning}
	m := fakeMemory(f)
	out := m.Execute(context.Background(), controller.Operation{Kind: controller.SetMemory, MemoryTarget: 2 * testGiB})
	if out.Err != nil || out.Memory != nil {
		t.Fatal(out)
	}
	if f.updateCalls != 1 || f.updateFlags != libvirt.DomainDeviceModifyLive {
		t.Fatal(f.updateCalls, f.updateFlags)
	}
	if strings.Contains(f.updateXML, "<current") {
		t.Fatalf("read-only current kept: %s", f.updateXML)
	}
	root, err := parseTestMemoryXML(f.updateXML)
	if err != nil || root.Tag != "memory" {
		t.Fatal(root, err)
	}
	if root.SelectAttrValue("model", "") != "virtio-mem" || root.SelectAttrValue("access", "") != "shared" || root.SelectAttrValue("discard", "") != "yes" {
		t.Fatalf("memory attributes dropped: %s", f.updateXML)
	}
	if element := root.SelectElement("alias"); element == nil || element.SelectAttrValue("name", "") != "ua-virtiomem0" {
		t.Fatalf("alias dropped: %s", f.updateXML)
	}
	if element := root.SelectElement("address"); element == nil || element.SelectAttrValue("slot", "") != "0x08" {
		t.Fatalf("address dropped: %s", f.updateXML)
	}
	target := root.SelectElement("target")
	if target == nil || target.SelectElement("node") == nil || target.SelectElement("size") == nil || target.SelectElement("block") == nil {
		t.Fatalf("target children dropped: %s", f.updateXML)
	}
	requested := target.SelectElement("requested")
	if requested == nil || requested.SelectAttrValue("unit", "") != "b" || strings.TrimSpace(requested.Text()) != "2147483648" {
		t.Fatalf("requested not replaced exactly: %s", f.updateXML)
	}
	size := target.SelectElement("size")
	block := target.SelectElement("block")
	capacity, capacityErr := xmlBytes(size)
	blockBytes, blockErr := xmlBytes(block)
	requestedBytes, requestedErr := xmlBytes(requested)
	if capacityErr != nil || blockErr != nil || requestedErr != nil ||
		capacity != 13*testGiB || blockBytes != 2*testMiB || requestedBytes != 2*testGiB {
		t.Fatal(capacity, blockBytes, requestedBytes, capacityErr, blockErr, requestedErr)
	}
}

func TestMemoryUpdatePreservesUnknownDeviceXML(t *testing.T) {
	input := strings.Replace(sampleMemoryDevice, "</target>", `<vendor:hint xmlns:vendor="urn:vendor" label="a&amp;b"/><!-- keep extension --></target>`, 1)
	node, err := parseTestMemoryXML(input)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := updateMemoryXML(node, 2*testGiB)
	if err != nil {
		t.Fatal(err)
	}
	out, err := parseTestMemoryXML(updated)
	if err != nil || out.FindElement("target/hint") == nil || !strings.Contains(updated, "keep extension") || !strings.Contains(updated, `label="a&amp;b"`) {
		t.Fatal(updated, err)
	}
	if node.FindElement("target/current") == nil {
		t.Fatal("updating mutated the original read snapshot")
	}
}

func TestMemorySetAckDoesNotClaimActual(t *testing.T) {
	f := &fakeMemoryAPI{state: libvirt.DomainRunning}
	m := fakeMemory(f)
	out := m.Execute(context.Background(), controller.Operation{Kind: controller.SetMemory, MemoryTarget: 2 * testGiB})
	if out.Err != nil || out.Memory != nil {
		t.Fatal(out)
	}
	// The live XML still reports the old current size. SetMemory must not
	// synthesize a new actual size from the acknowledged RPC.
	query := m.Execute(context.Background(), controller.Operation{Kind: controller.QueryMemory})
	if query.Err != nil || query.Memory == nil || query.Memory.ActualBytes != testGiB {
		t.Fatal(query)
	}
}

func TestMemorySetValidation(t *testing.T) {
	threeMiBBlock := strings.Replace(sampleMemoryDevice, `<block unit='MiB'>2</block>`, `<block unit='MiB'>3</block>`, 1)
	for _, tc := range []struct {
		name   string
		state  libvirt.DomainState
		xml    string
		target uint64
		kind   controller.Kind
		want   string
	}{
		{"paused", libvirt.DomainPaused, "", testGiB, controller.SetMemory, "cannot set memory while VM is paused"},
		{"misaligned", libvirt.DomainRunning, "", testGiB + 2*testMiB, controller.SetMemory, "not aligned to 134217728"},
		{"capacity", libvirt.DomainRunning, "", 14 * testGiB, controller.SetMemory, "exceeds capacity"},
		{"unsupported block", libvirt.DomainRunning, memoryDomain(threeMiBBlock), testGiB, controller.SetMemory, "block size"},
		{"unsupported operation", libvirt.DomainRunning, "", 0, controller.WakeVM, "unsupported memory operation"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeMemoryAPI{state: tc.state, xml: tc.xml}
			out := fakeMemory(f).Execute(context.Background(), controller.Operation{Kind: tc.kind, MemoryTarget: tc.target})
			if out.Err == nil || !strings.Contains(out.Err.Error(), tc.want) || f.updateCalls != 0 {
				t.Fatal(out, f.updateCalls)
			}
		})
	}
}

func TestMemorySetReclaimZeroBypassesBudget(t *testing.T) {
	f := &fakeMemoryAPI{state: libvirt.DomainRunning}
	m := fakeMemory(f)
	m.meminfo = func() (uint64, error) { return 0, errors.New("must not read during reclaim") }
	out := m.Execute(context.Background(), controller.Operation{Kind: controller.SetMemory, MemoryTarget: 0})
	if out.Err != nil || out.Memory != nil || f.updateCalls != 1 {
		t.Fatal(out, f.updateCalls)
	}
	if !strings.Contains(f.updateXML, `<requested unit="b">0</requested>`) {
		t.Fatal(f.updateXML)
	}
}

func TestMemorySetBudgetRejectsGrowth(t *testing.T) {
	f := &fakeMemoryAPI{state: libvirt.DomainRunning}
	m := fakeMemory(f)
	m.meminfo = func() (uint64, error) { return hostMemoryReserve + testGiB - 1, nil }
	out := m.Execute(context.Background(), controller.Operation{Kind: controller.SetMemory, MemoryTarget: 2 * testGiB})
	if out.Err == nil || !strings.Contains(out.Err.Error(), "insufficient host memory budget") {
		t.Fatal(out)
	}
	if f.updateCalls != 0 {
		t.Fatal(f.updateCalls)
	}
}

func memoryWatchEvent(event controller.Event) string {
	switch e := event.(type) {
	case controller.MemorySourceObserved:
		if e.Err != nil {
			return fmt.Sprintf("ready=%t err=%v", e.Ready, e.Err)
		}
		return fmt.Sprintf("ready=%t", e.Ready)
	case controller.MemoryDeviceObserved:
		if e.Lost {
			return "lost"
		}
		return fmt.Sprintf("event=%s:%d", e.Alias, e.ActualBytes)
	default:
		return fmt.Sprintf("unexpected %T", event)
	}
}

func TestMemoryWatchFiltersAndEmitsExactBytes(t *testing.T) {
	events := make(chan libvirt.DomainEvent, 8)
	events <- libvirt.DomainEvent{
		Event:   "BLOCK_JOB_COMPLETED",
		Details: []byte(`{"alias":"ua-virtiomem0","size":1074266112}`),
	}
	events <- libvirt.DomainEvent{
		Event:   "MEMORY_DEVICE_SIZE_CHANGE",
		Domain:  libvirt.Domain{Name: "win11"},
		Details: []byte(`{"id":"ua-other"}`),
	}
	events <- libvirt.DomainEvent{
		Event:   "MEMORY_DEVICE_SIZE_CHANGE",
		Domain:  libvirt.Domain{Name: "other"},
		Details: []byte(`{"id":"ua-virtiomem0","size":1074266112}`),
	}
	// QMP reports bytes. Keep a deliberately non-grid value untouched.
	events <- libvirt.DomainEvent{
		Event:   "MEMORY_DEVICE_SIZE_CHANGE",
		Domain:  libvirt.Domain{Name: "win11"},
		Details: []byte(`{"id":"ua-virtiomem0","size":1074266112}`),
	}
	close(events)
	m := &MemoryLibvirt{domain: "win11", alias: "ua-virtiomem0"}
	m.initial = newMemorySource(events, context.Background(), nil)
	var got []string
	err := m.watchOnce(context.Background(), func(_ context.Context, event controller.Event) error {
		got = append(got, memoryWatchEvent(event))
		return nil
	})
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal(err)
	}
	want := []string{"ready=true", "event=ua-virtiomem0:1074266112", "ready=false"}
	if len(got) != len(want) {
		t.Fatal(got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatal(got)
		}
	}
}

func TestMemoryWatchInvalidatesWhenSubscriptionContextEnds(t *testing.T) {
	events := make(chan libvirt.DomainEvent)
	close(events)
	eventCtx, stop := context.WithCancel(context.Background())
	stop()
	m := &MemoryLibvirt{domain: "win11", alias: "ua-virtiomem0"}
	m.initial = newMemorySource(events, eventCtx, nil)
	var got []string
	err := m.watchOnce(context.Background(), func(_ context.Context, event controller.Event) error {
		got = append(got, memoryWatchEvent(event))
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	want := []string{"ready=true", "ready=false"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatal(got)
	}
}

func TestMemoryWatchParentCancellationDoesNotInvalidate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan libvirt.DomainEvent)
	m := &MemoryLibvirt{domain: "win11", alias: "ua-virtiomem0"}
	m.initial = newMemorySource(events, context.Background(), func() { close(events) })
	acknowledged := make(chan struct{})
	var got []string
	done := make(chan error, 1)
	go func() {
		done <- m.Watch(ctx, func(_ context.Context, event controller.Event) error {
			got = append(got, memoryWatchEvent(event))
			if source, ok := event.(controller.MemorySourceObserved); ok && source.Ready {
				close(acknowledged)
			}
			return nil
		})
	}()
	select {
	case <-acknowledged:
	case <-time.After(time.Second):
		t.Fatal("watch did not acknowledge its subscription")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("watch did not stop")
	}
	if len(got) != 1 || got[0] != "ready=true" {
		t.Fatal(got)
	}
}

func TestMemoryWatchRejectsBadOrMissingSize(t *testing.T) {
	for _, tc := range []struct {
		name    string
		details string
		want    string
	}{
		{"bad json", `{"id":"ua-virtiomem0",`, "decode MEMORY_DEVICE_SIZE_CHANGE"},
		{"missing size", `{"id":"ua-virtiomem0"}`, "missing size"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := make(chan libvirt.DomainEvent, 1)
			events <- libvirt.DomainEvent{
				Event:   "MEMORY_DEVICE_SIZE_CHANGE",
				Domain:  libvirt.Domain{Name: "win11"},
				Details: []byte(tc.details),
			}
			m := &MemoryLibvirt{domain: "win11", alias: "ua-virtiomem0"}
			m.initial = newMemorySource(events, context.Background(), func() { close(events) })
			var observed []string
			err := m.watchOnce(context.Background(), func(_ context.Context, event controller.Event) error {
				observed = append(observed, memoryWatchEvent(event))
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatal(err)
			}
			var fatal libvirtSourceFatalError
			if errors.As(err, &fatal) {
				t.Fatal("protocol fault must reconnect, not be fatal", err)
			}
			if len(observed) != 2 || observed[0] != "ready=true" ||
				!strings.HasPrefix(observed[1], "ready=false err=") || !strings.Contains(observed[1], tc.want) {
				t.Fatal(observed)
			}
		})
	}
}

func TestMemoryWatchReconnectsAfterMalformedEvent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := make(chan libvirt.DomainEvent, 1)
	first <- libvirt.DomainEvent{
		Event:   "MEMORY_DEVICE_SIZE_CHANGE",
		Domain:  libvirt.Domain{Name: "win11"},
		Details: []byte(`{"id":"ua-virtiomem0",`),
	}
	second := make(chan libvirt.DomainEvent)
	m := &MemoryLibvirt{domain: "win11", alias: "ua-virtiomem0"}
	attempt := 0
	observed := make(chan string, 16)
	m.subscribe = func(context.Context) (*memorySource, error) {
		attempt++
		observed <- "subscribe"
		if attempt == 1 {
			return newMemorySource(first, context.Background(), func() { close(first) }), nil
		}
		return newMemorySource(second, context.Background(), func() { close(second) }), nil
	}
	done := make(chan error, 1)
	go func() {
		done <- m.Watch(ctx, func(_ context.Context, event controller.Event) error {
			observed <- memoryWatchEvent(event)
			return nil
		})
	}()
	next := func() string {
		select {
		case event := <-observed:
			return event
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for reconnecting source")
			return ""
		}
	}
	for _, want := range []string{"subscribe", "ready=true"} {
		if got := next(); got != want {
			t.Fatalf("observed %s, want %s", got, want)
		}
	}
	if got := next(); !strings.HasPrefix(got, "ready=false err=") || !strings.Contains(got, "decode MEMORY_DEVICE_SIZE_CHANGE") {
		t.Fatalf("malformed event was not reported: %s", got)
	}
	for _, want := range []string{"subscribe", "ready=true"} {
		if got := next(); got != want {
			t.Fatalf("observed %s, want %s", got, want)
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("watch did not stop")
	}
}

func TestMemoryWatchReconnectsAfterLoss(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := make(chan libvirt.DomainEvent, 1)
	first <- libvirt.DomainEvent{
		Event:   "MEMORY_DEVICE_SIZE_CHANGE",
		Domain:  libvirt.Domain{Name: "win11"},
		Details: []byte(`{"id":"ua-virtiomem0","size":1074266112}`),
	}
	close(first)
	second := make(chan libvirt.DomainEvent, 1)
	second <- libvirt.DomainEvent{
		Event:   "MEMORY_DEVICE_SIZE_CHANGE",
		Domain:  libvirt.Domain{Name: "win11"},
		Details: []byte(`{"id":"ua-virtiomem0","size":3221225472}`),
	}
	observed := make(chan string, 16)
	m := &MemoryLibvirt{domain: "win11", alias: "ua-virtiomem0"}
	attempt := 0
	m.subscribe = func(context.Context) (*memorySource, error) {
		attempt++
		observed <- "subscribe"
		if attempt == 1 {
			return newMemorySource(first, context.Background(), func() {}), nil
		}
		return newMemorySource(second, context.Background(), func() { close(second) }), nil
	}
	done := make(chan error, 1)
	go func() {
		done <- m.Watch(ctx, func(_ context.Context, event controller.Event) error {
			observed <- memoryWatchEvent(event)
			return nil
		})
	}()
	want := []string{
		"subscribe",
		"ready=true",
		"event=ua-virtiomem0:1074266112",
		"ready=false",
		"subscribe",
		"ready=true",
		"event=ua-virtiomem0:3221225472",
	}
	for _, event := range want {
		select {
		case got := <-observed:
			if got != event {
				t.Fatalf("observed %s, want %s", got, event)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for %s", event)
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("watch did not stop")
	}
}

func TestMemoryCloseDrainsWithoutWatch(t *testing.T) {
	events := make(chan libvirt.DomainEvent)
	go func() { events <- libvirt.DomainEvent{Event: "ignored"}; close(events) }()
	closes := 0
	m := &MemoryLibvirt{initial: newMemorySource(events, context.Background(), func() { closes++ })}
	done := make(chan struct{})
	go func() { m.Close(); m.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close did not drain blocked sender")
	}
	if closes != 1 {
		t.Fatal(closes)
	}
}

func TestMemoryCancelWatchAndCloseTogether(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	eventCtx, stop := context.WithCancel(context.Background())
	defer stop()
	events := make(chan libvirt.DomainEvent)
	observed := make(chan struct{})
	go func() {
		events <- libvirt.DomainEvent{Event: "MEMORY_DEVICE_SIZE_CHANGE", Domain: libvirt.Domain{Name: "win11"}, Details: []byte(`{"id":"ua-virtiomem0","size":1}`)}
		<-eventCtx.Done()
		events <- libvirt.DomainEvent{Event: "ignored"}
		close(events)
	}()
	closes := 0
	m := &MemoryLibvirt{domain: "win11", alias: "ua-virtiomem0"}
	m.initial = newMemorySource(events, eventCtx, func() { closes++; stop() })
	watched := make(chan error, 1)
	go func() {
		watched <- m.Watch(ctx, func(context.Context, controller.Event) error {
			select {
			case <-observed:
			default:
				close(observed)
			}
			return nil
		})
	}()
	select {
	case <-observed:
	case <-time.After(time.Second):
		t.Fatal("Watch did not start")
	}
	cancel()
	closed := make(chan struct{})
	go func() { m.Close(); close(closed) }()
	select {
	case err := <-watched:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Watch did not exit")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close and Watch deadlocked")
	}
	m.Close()
	if closes != 1 {
		t.Fatal(closes)
	}
}
