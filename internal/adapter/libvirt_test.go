package adapter

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/111inhistory/lsw-rdp-launcher-go/internal/controller"
	libvirt "github.com/digitalocean/go-libvirt"
)

type fakeDomain struct {
	calls []string
	state libvirt.DomainState
	err   error
}

func (f *fakeDomain) DomainLookupByName(name string) (libvirt.Domain, error) {
	return libvirt.Domain{Name: name}, nil
}
func (f *fakeDomain) DomainGetState(libvirt.Domain, uint32) (int32, int32, error) {
	return int32(f.state), 0, f.err
}
func (f *fakeDomain) DomainCreate(libvirt.Domain) error {
	f.calls = append(f.calls, "start")
	return f.err
}
func (f *fakeDomain) DomainResume(libvirt.Domain) error {
	f.calls = append(f.calls, "resume")
	return f.err
}
func (f *fakeDomain) DomainPmWakeup(libvirt.Domain, uint32) error {
	f.calls = append(f.calls, "pm-wakeup")
	return f.err
}
func (f *fakeDomain) DomainSuspend(libvirt.Domain) error {
	f.calls = append(f.calls, "suspend")
	return f.err
}
func fakeVM(f *fakeDomain) *Libvirt {
	return &Libvirt{domain: "win11", open: func(context.Context) (domainAPI, func(), error) { return f, func() {}, nil }}
}
func vmWatchEvent(event controller.Event) string {
	switch e := event.(type) {
	case controller.VMSourceObserved:
		return fmt.Sprintf("ready=%t", e.Ready)
	case controller.VMObserved:
		return "vm=" + string(e.State)
	default:
		return fmt.Sprintf("unexpected %T", event)
	}
}
func TestLibvirtMutationsReturnAckNotState(t *testing.T) {
	for _, tc := range []struct {
		kind  controller.Kind
		state controller.VMState
		call  string
	}{
		{controller.WakeVM, controller.VMOff, "start"},
		{controller.WakeVM, controller.VMPaused, "resume"},
		{controller.WakeVM, controller.VMPMSuspended, "pm-wakeup"},
		{controller.SuspendVM, controller.VMRunning, "suspend"},
	} {
		t.Run(tc.call, func(t *testing.T) {
			f := &fakeDomain{}
			out := fakeVM(f).Execute(context.Background(), controller.Operation{Kind: tc.kind, VM: tc.state})
			if out.Err != nil || out.VM != "" || len(f.calls) != 1 || f.calls[0] != tc.call {
				t.Fatal(out, f.calls)
			}
		})
	}
}
func TestLibvirtQueryAndError(t *testing.T) {
	f := &fakeDomain{state: libvirt.DomainPaused}
	out := fakeVM(f).Execute(context.Background(), controller.Operation{Kind: controller.QueryVM})
	if out.VM != controller.VMPaused || out.Err != nil {
		t.Fatal(out)
	}
	f.err = errors.New("denied")
	out = fakeVM(f).Execute(context.Background(), controller.Operation{Kind: controller.WakeVM, VM: controller.VMOff})
	if out.Err != f.err {
		t.Fatal(out)
	}
}
func TestLibvirtLifecycleMappingAndDomainFilter(t *testing.T) {
	events := make(chan libvirt.DomainEventLifecycleMsg, 5)
	events <- libvirt.DomainEventLifecycleMsg{Dom: libvirt.Domain{Name: "other"}, Event: int32(libvirt.DomainEventStarted)}
	for _, event := range []libvirt.DomainEventType{libvirt.DomainEventDefined, libvirt.DomainEventStarted, libvirt.DomainEventSuspended, libvirt.DomainEventStopped} {
		events <- libvirt.DomainEventLifecycleMsg{Dom: libvirt.Domain{Name: "win11"}, Event: int32(event)}
	}
	close(events)
	v := &Libvirt{domain: "win11"}
	v.initial = newLifecycleSource(events, nil)
	var got []string
	err := v.watchOnce(context.Background(), func(_ context.Context, event controller.Event) error {
		got = append(got, vmWatchEvent(event))
		return nil
	})
	want := []string{"ready=true", "vm=running", "vm=paused", "vm=shut off", "ready=false"}
	if err == nil || len(got) != len(want) {
		t.Fatal(got, err)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatal(got)
		}
	}
}
func TestLibvirtWatchCancellationClosesAndDrains(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan libvirt.DomainEventLifecycleMsg)
	observed := make(chan struct{})
	v := &Libvirt{domain: "win11"}
	v.initial = newLifecycleSource(events, func() { close(events) })
	done := make(chan error, 1)
	go func() {
		done <- v.Watch(ctx, func(_ context.Context, event controller.Event) error {
			if source, ok := event.(controller.VMSourceObserved); ok && source.Ready {
				select {
				case <-observed:
				default:
					close(observed)
				}
			}
			return nil
		})
	}()
	select {
	case <-observed:
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
}
func TestLibvirtCloseDrainsWithoutWatch(t *testing.T) {
	events := make(chan libvirt.DomainEventLifecycleMsg)
	go func() {
		events <- libvirt.DomainEventLifecycleMsg{Dom: libvirt.Domain{Name: "win11"}}
		close(events)
	}()
	closes := 0
	v := &Libvirt{initial: newLifecycleSource(events, func() { closes++ })}
	done := make(chan struct{})
	go func() { v.Close(); v.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close did not drain blocked sender")
	}
	if closes != 1 {
		t.Fatal(closes)
	}
}
func TestLibvirtWatchReconnectsAfterAcknowledgedSource(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := make(chan libvirt.DomainEventLifecycleMsg, 1)
	first <- libvirt.DomainEventLifecycleMsg{Dom: libvirt.Domain{Name: "win11"}, Event: int32(libvirt.DomainEventStarted)}
	close(first)
	second := make(chan libvirt.DomainEventLifecycleMsg, 1)
	second <- libvirt.DomainEventLifecycleMsg{Dom: libvirt.Domain{Name: "win11"}, Event: int32(libvirt.DomainEventStopped)}

	observed := make(chan string, 16)
	attempt := 0
	v := &Libvirt{
		domain: "win11",
		subscribe: func(context.Context) (*lifecycleSource, error) {
			attempt++
			observed <- "subscribe"
			if attempt == 1 {
				return newLifecycleSource(first, func() {}), nil
			}
			return newLifecycleSource(second, func() { close(second) }), nil
		},
	}
	done := make(chan error, 1)
	go func() {
		done <- v.Watch(ctx, func(_ context.Context, event controller.Event) error {
			observed <- vmWatchEvent(event)
			return nil
		})
	}()
	want := []string{"subscribe", "ready=true", "vm=running", "ready=false", "subscribe", "ready=true", "vm=shut off"}
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
