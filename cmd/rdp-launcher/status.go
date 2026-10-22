package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/111inhistory/lsw-rdp-launcher-go/internal/controller"
)

type statusView struct {
	VM, SSH, DisplayMode, Operation, AwaitingVM, LastError string
	BackendReady, Display, Locked                          bool
	Windows, Queued                                        int
	DisplayDeadline, SuspendDeadline                       time.Time
	Memory                                                 struct {
		Guest                                                                             controller.GuestMemory
		Device                                                                            controller.MemoryDevice
		GuestKnown, DeviceKnown, Growing, Awaiting, Reclaiming, Preparing, WindowPrepared bool
		Operation, LastError                                                              string
		TargetBytes                                                                       uint64
	}
}

func printStatus(out io.Writer, data json.RawMessage, asJSON bool) error {
	if asJSON {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			return err
		}
		if fields == nil {
			return fmt.Errorf("controller returned an empty status")
		}
		fields["Version"], _ = json.Marshal(versionString())
		encoder := json.NewEncoder(out)
		encoder.SetIndent("", "  ")
		return encoder.Encode(fields)
	}
	var s statusView
	s.Windows = -1
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	var text strings.Builder
	fmt.Fprintln(&text, versionString())
	yesNo := func(value bool, yes, no string) string {
		if value {
			return yes
		}
		return no
	}
	fmt.Fprintf(&text, "虚拟机：%s\n后台：%s\n", s.VM, yesNo(s.BackendReady, "就绪", "未就绪"))
	display := "未连接"
	if s.Display {
		display = "已连接"
		if s.DisplayMode != "" {
			display += "（" + s.DisplayMode + "）"
		}
	}
	windows := "未知"
	if s.Windows >= 0 {
		windows = fmt.Sprint(s.Windows)
	}
	fmt.Fprintf(&text, "显示：%s\n窗口：%s\nSSH：%s\n自动暂停：%s\n队列：%d\n", display, windows, s.SSH, yesNo(s.Locked, "禁用（已锁定）", "启用"), s.Queued)
	if s.Operation != "" {
		fmt.Fprintf(&text, "当前操作：%s\n", s.Operation)
	}
	if s.AwaitingVM != "" {
		fmt.Fprintf(&text, "等待虚拟机：%s\n", s.AwaitingVM)
	}
	if s.Memory.GuestKnown {
		fmt.Fprintf(&text, "内存：总量 %.2f GiB，可用 %.2f GiB\n", float64(s.Memory.Guest.TotalBytes)/(1<<30), float64(s.Memory.Guest.AvailableBytes)/(1<<30))
	} else {
		fmt.Fprintln(&text, "内存：未知")
	}
	if s.Memory.DeviceKnown {
		fmt.Fprintf(&text, "virtio-mem：已插入 %.2f GiB，请求 %.2f GiB，容量 %.2f GiB\n", float64(s.Memory.Device.ActualBytes)/(1<<30), float64(s.Memory.Device.RequestedBytes)/(1<<30), float64(s.Memory.Device.CapacityBytes)/(1<<30))
	}
	var memory []string
	for _, item := range []struct {
		active bool
		name   string
	}{{s.Memory.Growing, "扩容"}, {s.Memory.Awaiting, "等待确认"}, {s.Memory.Reclaiming, "回收"}, {s.Memory.Preparing, "安全准备"}} {
		if item.active {
			memory = append(memory, item.name)
		}
	}
	if s.Memory.Operation != "" {
		memory = append(memory, s.Memory.Operation)
	}
	if len(memory) > 0 {
		fmt.Fprintf(&text, "内存操作：%s\n", strings.Join(memory, " / "))
	}
	for _, item := range []struct {
		name string
		at   time.Time
	}{{"空闲断开", s.DisplayDeadline}, {"自动暂停", s.SuspendDeadline}} {
		if !item.at.IsZero() {
			fmt.Fprintf(&text, "%s时间：%s\n", item.name, item.at.Local().Format(time.RFC3339))
		}
	}
	if s.LastError != "" {
		fmt.Fprintf(&text, "错误：%s\n", s.LastError)
	}
	if s.Memory.LastError != "" {
		fmt.Fprintf(&text, "内存错误：%s\n", s.Memory.LastError)
	}
	_, err := io.WriteString(out, text.String())
	return err
}
