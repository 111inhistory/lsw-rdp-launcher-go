package adapter

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/111inhistory/lsw-rdp-launcher-go/internal/controller"
	libvirt "github.com/digitalocean/go-libvirt"
)

type preparationAPI struct {
	fakeMemoryAPI
	targets []uint64
	fail    map[int]error
	cancel  context.CancelFunc
}

func (f *preparationAPI) DomainUpdateDeviceFlags(_ libvirt.Domain, raw string, flags libvirt.DomainDeviceModifyFlags) error {
	node, err := parseTestMemoryXML(raw)
	if err != nil {
		return err
	}
	target, err := xmlBytes(node.FindElement("target/requested"))
	if err != nil {
		return err
	}
	f.targets = append(f.targets, target)
	if flags != libvirt.DomainDeviceModifyLive {
		return errors.New("notlive")
	}
	if f.cancel != nil && len(f.targets) == 2 {
		f.cancel()
	}
	return f.fail[len(f.targets)]
}
func preparationMemory(f *preparationAPI) *MemoryLibvirt {
	base := fakeMemory(&f.fakeMemoryAPI)
	base.open = func(ctx context.Context) (memoryAPI, func(), error) {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		return f, func() {}, nil
	}
	return base
}
func TestMemoryPrepareOnlyWhilePausedAndRoundsFinalActual(t *testing.T) {
	f := &preparationAPI{fakeMemoryAPI: fakeMemoryAPI{state: libvirt.DomainPaused, xml: memoryDomain(strings.Replace(sampleMemoryDevice, `<current unit='GiB'>1</current>`, `<current unit='MiB'>386</current>`, 1))}}
	out := preparationMemory(f).Execute(context.Background(), controller.Operation{Kind: controller.PrepareMemory})
	if out.Err != nil || !out.ResumeSafe || !out.MemoryPrepared || out.Memory.ActualBytes != 386*testMiB || len(f.targets) != 2 || f.targets[0] != 13*testGiB || f.targets[1] != 512*testMiB {
		t.Fatal(out, f.targets)
	}
	f.state = libvirt.DomainRunning
	f.targets = nil
	out = preparationMemory(f).Execute(context.Background(), controller.Operation{Kind: controller.PrepareMemory})
	if out.Err == nil || len(f.targets) != 0 {
		t.Fatal(out, f.targets)
	}
}
func TestMemoryPrepareCrashRecoveryDoesNotRestoreCapacityAsWorkingTarget(t *testing.T) {
	f := &preparationAPI{fakeMemoryAPI: fakeMemoryAPI{state: libvirt.DomainPaused, xml: memoryDomain(strings.Replace(sampleMemoryDevice, `<requested unit='GiB'>1</requested>`, `<requested unit='GiB'>13</requested>`, 1))}}
	out := preparationMemory(f).Execute(context.Background(), controller.Operation{Kind: controller.PrepareMemory})
	if out.Err != nil || f.targets[len(f.targets)-1] != testGiB {
		t.Fatal(out, f.targets)
	}
}
func TestMemoryPrepareFailedWritesRestoreOnFreshConnection(t *testing.T) {
	for _, failed := range []int{1, 2} {
		f := &preparationAPI{fakeMemoryAPI: fakeMemoryAPI{state: libvirt.DomainPaused}, fail: map[int]error{failed: errors.New("RPC lost")}}
		out := preparationMemory(f).Execute(context.Background(), controller.Operation{Kind: controller.PrepareMemory})
		if out.Err == nil || !out.ResumeSafe || out.MemoryPrepared || f.targets[len(f.targets)-1] != testGiB {
			t.Fatal(failed, out, f.targets)
		}
	}
}
func TestMemoryPrepareCancellationStillRestoresSafeTarget(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &preparationAPI{fakeMemoryAPI: fakeMemoryAPI{state: libvirt.DomainPaused}, cancel: cancel, fail: map[int]error{2: context.Canceled}}
	out := preparationMemory(f).Execute(ctx, controller.Operation{Kind: controller.PrepareMemory})
	if !errors.Is(out.Err, context.Canceled) || !out.ResumeSafe || len(f.targets) != 3 || f.targets[2] != testGiB {
		t.Fatal(out, f.targets)
	}
}
func TestMemoryPrepareUnsafeRestorationIsNotResumed(t *testing.T) {
	f := &preparationAPI{fakeMemoryAPI: fakeMemoryAPI{state: libvirt.DomainPaused}, fail: map[int]error{2: errors.New("writefailed"), 3: errors.New("cleanupfailed")}}
	out := preparationMemory(f).Execute(context.Background(), controller.Operation{Kind: controller.PrepareMemory})
	if out.Err == nil || out.ResumeSafe || out.MemoryPrepared || !strings.Contains(out.Err.Error(), "leave VM paused") {
		t.Fatal(out)
	}
}
