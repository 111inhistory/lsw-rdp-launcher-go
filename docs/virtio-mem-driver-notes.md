# virtio-mem 驱动问题与宿主缓解

核查时间：2026-10-19 至 2026-10-20。目标环境为 QEMU 10.2.2、libvirt 12.0.0、Windows viomem 驱动 100.101.104.28500。

驱动文件 SHA-256：

```text
1854f1c559e4a34a100f5f61d4b92a708a628f14e19c960ac0476397ee78a761
```

## 两个问题

上游 `viomem/sys/viomem.c` 仍包含以下行为：

1. `ViomemWorkerThread` 每次配置通知只调用一次 `VirtioMemAddPhysicalMemory`。该函数只消费一个最长空闲段。碎片化位图可能需要多次通知才能完成同一目标。
2. `FindFreeMemoryRangeInBitmap` 对整个设备位图调用 `RtlFindLongestRunClear`，没有限制到 `usable_region_size`。选出的高地址段可能被 QEMU 拒绝；重复同一请求不能改变选块结果。

核查文件：[viomem.c 的固定版本](https://raw.githubusercontent.com/virtio-win/kvm-guest-drivers-windows/34f4a57438f662d0afde43020addc87fd60f5472/viomem/sys/viomem.c)。该提交是文件的最近修改，不是整个仓库的 HEAD。当前安装的驱动没有被替换。

## 纠正旧调查中的假设

QEMU 10.2.2 的 `virtio_mem_set_requested_size` 调用：

```c
virtio_mem_resize_usable_region(vmem, value, false);
```

普通 requested 缩容不会缩小 usable region。`UNPLUG_ALL` 等路径才允许窗口收缩。

因此，“8 GiB 缩到 4 GiB 后，usable 也随之缩小”不是有效复现前提。但越界搜索仍可能发生。例如窗口保留为 8.25 GiB，驱动却选择 `[8 GiB, 10 GiB)`，结束地址仍超出窗口。

## 地址窗口准备，而非全容量实际扩容

当前实现利用 QEMU 10.2.2 的两个独立状态：

- requested setter 扩大 usable region。
- guest PLUG 请求改变 actual，并可能分配宿主物理页。

宿主在已确认 Paused 的 VM 上设置 capacity，再将 requested 设置为向上对齐到 128 MiB 的 actual，最后恢复 VM。setter 的第二次缩小不缩小 usable region；guest 运行后只看到最终目标。

这与在运行中的 Windows 上请求全容量不同。2026-10-20 的测试中，capacity 为 13 GiB，actual 在准备前后都保持 1.125 GiB，没有实际插入 13 GiB。窗口设置与核对耗时约 68.5 ms，不作为时延保证。

适配器处理取消和失败时会尝试恢复安全目标。恢复目标不使用可能遗留的 capacity，控制器可在重启后再次准备。安全目标无法确认时不自动 resume。

该办法要求单一控制器拥有这段暂停窗口。并发的外部 resume、设备重置和其他 QEMU/驱动版本不在本轮验证保证内。

## 有进展才续处理，不做无限重试

在当前固定版本中，相同目标会经过完整调用链：

```text
libvirt qemuDomainChangeMemoryLive
 → qemuDomainChangeMemoryRequestedSize
 → QEMU virtio_mem_set_requested_size
 → virtio_notify_config
```

libvirt 12.0.0 在这条路径中不按 requested 相等去重。QEMU 10.2.2 则明确将 `virtio_notify_config` 放在数值变化判断之外。

所以宿主可以在 actual 已增长、随后静默 1 秒时，重新提交同一目标来推进下一个空闲段。代码要求每次追加通知前有新的实际进展，最多追加 8 次，并保留原有的 30 秒总截止时间。达到 80% 或观测不可用时不追加。

无进展、资源不足或其他内核错误仍会结束本轮自动写入。该办法不能保证一次通知对应一次处理，也不修复所有内核故障。

## 量纲

- 当前 Go 库的通用事件路由没有分发 `DomainEventMemoryDeviceSizeChangeMsg`。
- 实现复用 `SubscribeQEMUEvents`，在独立连接上订阅 `MEMORY_DEVICE_SIZE_CHANGE`。事件的 `id` 对应设备 alias，`size` 单位为 bytes。
- libvirt 的官方高层回调 `virConnectDomainEventMemoryDeviceSizeChangeCallback` 使用 KiB。此前将该回调写成 bytes 的笔记有误。
- 内部比较统一用 bytes。两种事件不能混用单位。

## 源码与接口依据

- [QEMU v10.2.2 virtio-mem.c](https://github.com/qemu/qemu/blob/v10.2.2/hw/virtio/virtio-mem.c)：requested setter、usable region、PLUG 和预分配路径。
- [libvirt v12.0.0 qemu_hotplug.c](https://github.com/libvirt/libvirt/blob/v12.0.0/src/qemu/qemu_hotplug.c)：内存设备 live update。
- [libvirt domain API](https://libvirt.org/html/libvirt-libvirt-domain.html#virConnectDomainEventMemoryDeviceSizeChangeCallback)：高层回调的 KiB 单位。
- [Windows 内存资源通知](https://learn.microsoft.com/en-us/windows/win32/api/memoryapi/nf-memoryapi-creatememoryresourcenotification)：Low/High 行为，没有可配置的固定百分比。
- [MEMORYSTATUSEX](https://learn.microsoft.com/en-us/windows/win32/api/sysinfoapi/ns-sysinfoapi-memorystatusex)：物理总量和可用量。

本轮没有安装、签名或编译新的内核驱动，也没有修改 Secure Boot。若后续修复驱动本身，应分别增加 usable 位图上限和带进展检查的工作循环，并通过 WDK 和真实 guest 验证。
