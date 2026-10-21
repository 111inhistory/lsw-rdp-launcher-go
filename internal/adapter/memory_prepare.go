package adapter

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/111inhistory/lsw-rdp-launcher-go/internal/controller"
	libvirt "github.com/digitalocean/go-libvirt"
)

// WORKAROUND (QEMU 10.2 / Windows virtio-mem): prepareResume widens
// usable_region_size without asking a running guest to
// plug the whole device. QEMU10.2's requested-size setter changes this window
// immediately; only a guest PLUG request allocates/onlines RAM. The VM must
// remain paused under this controller's exclusive ownership during both writes.
// Final requested is rounded actual, NOT the temporary capacity or an old
// requested value: repeating preparation after a controller crash is safe.
// Retire this sequence only after live suspend/resume tests show the usable
// window stays wide on the supported guest/QEMU combination.
func (m *MemoryLibvirt) prepareResume(ctx context.Context, api memoryAPI, dom libvirt.Domain) (out controller.Outcome) {
	state, _, err := api.DomainGetState(dom, 0)
	if err != nil {
		return controller.Outcome{Err: err}
	}
	if vmState(libvirt.DomainState(state)) != controller.VMPaused {
		return controller.Outcome{Err: fmt.Errorf("memory window preparation requires Paused VM")}
	}
	device, node, err := m.readDevice(api, dom)
	if err != nil {
		return controller.Outcome{Err: err}
	}
	if device.BlockBytes == 0 || device.BlockBytes > controller.MemoryQuantum || controller.MemoryQuantum%device.BlockBytes != 0 || device.ActualBytes > device.CapacityBytes {
		return controller.Outcome{Err: fmt.Errorf("unsupported memory device for window preparation")}
	}
	target := (device.ActualBytes + controller.MemoryQuantum - 1) / controller.MemoryQuantum * controller.MemoryQuantum
	if target > device.CapacityBytes {
		return controller.Outcome{Err: fmt.Errorf("device actual cannot be rounded within capacity")}
	}
	if target > device.ActualBytes {
		if err := m.checkBudget(target - device.ActualBytes); err != nil {
			return controller.Outcome{Err: err}
		}
	}
	// Any failed RPC can have reached QEMU. Restore on a fresh bounded connection,
	// even when the original operation context has already been canceled.
	defer func() {
		if out.Err == nil || out.ResumeSafe {
			return
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := m.restoreMemoryTarget(cleanup, target); err != nil {
			out.Err = fmt.Errorf("%w; restoring safe memory target failed: %v; leave VM paused", out.Err, err)
			log.Printf("memory preparation: %v", out.Err)
		} else {
			out.ResumeSafe = true
		}
	}()
	fullXML, err := updateMemoryXML(node, device.CapacityBytes)
	if err != nil {
		return controller.Outcome{Err: err}
	}
	if err := api.DomainUpdateDeviceFlags(dom, fullXML, libvirt.DomainDeviceModifyLive); err != nil {
		return controller.Outcome{Err: err}
	}
	finalXML, err := updateMemoryXML(node, target)
	if err != nil {
		return controller.Outcome{Err: err}
	}
	if err := api.DomainUpdateDeviceFlags(dom, finalXML, libvirt.DomainDeviceModifyLive); err != nil {
		return controller.Outcome{Err: err}
	}
	out.ResumeSafe = true
	state, _, err = api.DomainGetState(dom, 0)
	if err != nil {
		out.Err = err
		return
	}
	if vmState(libvirt.DomainState(state)) != controller.VMPaused {
		out.Err = fmt.Errorf("VM changed during memory preparation")
		return
	}
	device.RequestedBytes = target
	out.Memory = &device
	out.MemoryPrepared = true
	return
}
func (m *MemoryLibvirt) restoreMemoryTarget(ctx context.Context, target uint64) error {
	api, close, err := m.open(ctx)
	if err != nil {
		return err
	}
	defer close()
	dom, err := api.DomainLookupByName(m.domain)
	if err != nil {
		return err
	}
	_, node, err := m.readDevice(api, dom)
	if err != nil {
		return err
	}
	raw, err := updateMemoryXML(node, target)
	if err != nil {
		return err
	}
	return api.DomainUpdateDeviceFlags(dom, raw, libvirt.DomainDeviceModifyLive)
}
