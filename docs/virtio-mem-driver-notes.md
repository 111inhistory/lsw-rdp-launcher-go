# virtio-mem 驱动约束与宿主缓解

以下机制针对 QEMU 10.2.2、libvirt 12.0.0 与 Windows viomem 100.101.104.28500 的组合。其他版本应重新核对，不保证同样适用。

## 驱动的两个约束

固定版本的 [viomem.c](https://github.com/virtio-win/kvm-guest-drivers-windows/blob/34f4a57438f662d0afde43020addc87fd60f5472/viomem/sys/viomem.c) 包含以下行为：

1. `ViomemWorkerThread` 每次配置通知只调用一次 `VirtioMemAddPhysicalMemory`。后者只消费一个最长空闲段；碎片化位图可能需要多次通知才能完成同一目标。
2. `FindFreeMemoryRangeInBitmap` 对整个设备位图调用 `RtlFindLongestRunClear`，没有限制到 `usable_region_size`。选出的高地址段可能被 QEMU 拒绝。

因此，重复无进展的同一请求既不能保证完成目标，也不能改变错误选块。宿主采用地址窗口准备与有进展才续处理两项缓解，不修改内核驱动。

## requested、usable 与 actual

QEMU 10.2.2 的 requested setter 调用：

```c
virtio_mem_resize_usable_region(vmem, value, false);
```

普通 requested 缩容不会缩小 usable region。`UNPLUG_ALL` 等路径才允许窗口收缩。不能以“requested 从大变小”直接推断地址窗口同步缩小。

两个状态变化不同：

- requested setter 扩大 usable region。
- guest PLUG 请求改变 actual，并可能分配宿主物理页。

宿主在已确认 Paused 的 VM 上先请求 capacity，再将 requested 设置为向上对齐的 actual，最后恢复 VM。暂停的 vCPU 不执行 PLUG，guest 恢复后只看到安全的最终目标。这不等于在运行中的 Windows 上实际插入全容量。

这个暂停窗口应只有一个写者。外部并发恢复、设备重置或其他 QEMU/驱动版本可能破坏前提。失败时的恢复流程见 [memory.md#recovery](memory.md#recovery)。

## 有进展才续处理

在上述固定版本中，相同目标经过以下路径：

```text
libvirt qemuDomainChangeMemoryLive
 → qemuDomainChangeMemoryRequestedSize
 → QEMU virtio_mem_set_requested_size
 → virtio_notify_config
```

libvirt 12.0.0 不在这条路径中按 requested 相等去重。QEMU 10.2.2 将通知放在数值变化判断之外。

宿主只在 actual 已增长、随后静默 1 秒时追加同目标通知。每次追加前应有新的进展，最多 8 次，不延长原 30 秒期限。达到 stop 阈值或观测不可用时不追加。

这不是无限重试，也不保证一次通知对应一次处理。资源不足或其他内核故障仍会结束本轮自动写入。

## 事件与量纲

控制器使用独立连接订阅 QEMU `MEMORY_DEVICE_SIZE_CHANGE`：`id` 对应设备 alias，`size` 单位为 bytes。

libvirt 高层 `virConnectDomainEventMemoryDeviceSizeChangeCallback` 使用 KiB。两种接口不能混用单位；内部比较统一为 bytes。

## 接口依据

- [QEMU v10.2.2 virtio-mem.c](https://github.com/qemu/qemu/blob/v10.2.2/hw/virtio/virtio-mem.c)：requested setter、usable region 与 PLUG。
- [libvirt v12.0.0 qemu_hotplug.c](https://github.com/libvirt/libvirt/blob/v12.0.0/src/qemu/qemu_hotplug.c)：内存设备 live update。
- [libvirt 内存设备事件](https://libvirt.org/html/libvirt-libvirt-domain.html#virConnectDomainEventMemoryDeviceSizeChangeCallback)：高层回调的 KiB 单位。
- [MEMORYSTATUSEX](https://learn.microsoft.com/en-us/windows/win32/api/sysinfoapi/ns-sysinfoapi-memorystatusex)：Windows 物理总量与可用量。

调整驱动本身仍需 WDK、签名及独立实机验证。宿主缓解不修复所有内核故障，也不改变 Secure Boot。
