# virtio-mem 驱动行为与宿主协同参考

本文档总结 Windows virtio-mem 驱动（`viomem.c`）在动态热插拔中的已知行为，以及宿主通过 QEMU/libvirt 进行地址窗口准备与进展受控通知的实现机制。

环境基线：QEMU 10.2+、libvirt 12.0+ 与 Windows `viomem` 驱动（`virtio-win/kvm-guest-drivers-windows`）。

## 上游 viomem.c 的两项驱动行为

上游 `viomem/sys/viomem.c` 包含以下两项关键行为特征：

1. **单次通知仅消费单个空闲段**：`ViomemWorkerThread` 每次接收到配置变更通知（config notification）时，仅调用一次 `VirtioMemAddPhysicalMemory`，而该函数只查找并消费当前最长的一个连续空闲段。当来宾物理内存位图处于碎片化状态时，单次通知无法完成跨多段的扩容，需要多次通知才能推进至目标大小。
2. **位图搜索未受 usable_region_size 约束**：`FindFreeMemoryRangeInBitmap` 对整个设备位图调用 `RtlFindLongestRunClear`，未将搜索范围截断在当前的 `usable_region_size` 之内。当选中的高地址段超出可用区域时，QEMU 会直接拒绝 PLUG 请求；驱动不会回退选择窗口内的小空闲块，重复提交相同请求也无法改变选块结果。

## QEMU 10.2 usable_region_size 扩展与缩容机制

在 QEMU 10.2+ 中，`virtio_mem_set_requested_size` 内部调用：

```c
virtio_mem_resize_usable_region(vmem, value, false);
```

第三个参数指示是否允许收缩可用区域。在常规的 `requested-size` 调整路径中，该参数固定为 `false`（仅在 `UNPLUG_ALL` 等设备重置路径下才允许收缩）。

因此，常规的 requested 目标缩容**不会**缩小 `usable_region_size`。可用区域一旦被推高，只要虚拟机未进行设备级重置，窗口就维持在历史最高位。

尽管常规缩容不收缩可用区域，但若可用区域尚未扩展到设备全容量，驱动仍可能在全设备位图中选中超出当前窗口的高地址块（例如可用窗口为 8.25 GiB，驱动选定 `[8 GiB, 10 GiB)`，结束地址超出窗口），导致 QEMU 拒绝。

## 暂停窗口准备机制

QEMU virtio-mem 具有两项解耦的状态：

- `requested-size` 的设置会扩展 `usable_region_size`。
- 来宾内部发起的 PLUG 请求才会改变 `actual` 并分配宿主物理内存页。

宿主利用此特性，在虚拟机处于已确认的 `Paused` 状态下完成可用区域扩展：

1. **暂停虚拟机**：确保无来宾代码运行。
2. **设置全容量 requested**：将 `requested` 设置为设备 `capacity`，触发 QEMU 立即将 `usable_region_size` 扩大到整个设备容量。
3. **设置实际目标 requested**：将 `requested` 调回与当前 `actual` 向上对齐到 128 MiB 的目标容量。由于常规缩容不收缩 `usable_region_size`，可用区域保持全容量覆盖。
4. **恢复虚拟机**：安全目标确认后恢复虚拟机运行。来宾代码开始执行时仅看到最终目标尺寸，但此时可用区域已覆盖全设备，消除了驱动选块超出窗口被拒的可能，且准备过程不会在宿主实际插入多余的物理内存页。

若准备失败或被取消，控制器会尝试恢复安全目标；在安全目标未获得明确确认前，不自动执行 Resume。该过程要求单一控制器独占暂停窗口。

## 进展受控的追加通知机制

在动态扩容遇到碎片化位图时，驱动单次通知仅消费一段。相同目标可经由完整调用链触发再次通知：

```text
libvirt qemuDomainChangeMemoryLive
 → qemuDomainChangeMemoryRequestedSize
 → QEMU virtio_mem_set_requested_size
 → virtio_notify_config
```

libvirt 在该路径中不对相同的 requested 目标做去重过滤；QEMU 也将 `virtio_notify_config` 置于数值变化判断之外。因此，重新提交相同目标仍会触发配置变更中断。

为避免无界重试与盲目轮询，宿主实施**进展受控（progress-gated）**机制：

- 仅在观测到 `actual` 已经增长且随后静默（如 1 秒内无新事件）时，才重新提交相同目标以推进下一段。
- 每次追加通知严格以“前一次产生了实际进展”为准入条件。
- 限制追加通知最多 8 次，且受 30 秒总操作期限约束。
- 达到 80% 目标阈值或观测数据不可用时不进行追加。
- 若无新进展或来宾内核报告错误，立即终止自动重通知，不进行无限制重试。

## 事件量纲与单位区分

不同来源的内存事件量纲不同，宿主接入时进行严格区分与归一化：

- **QEMU Monitor 事件（`MEMORY_DEVICE_SIZE_CHANGE`）**：通过 QEMU 事件连接订阅，事件中的 `id` 为设备别名（alias），`size` 单位为 **bytes**。
- **libvirt 官方事件（`virConnectDomainEventMemoryDeviceSizeChangeCallback`）**：高层回调传递的内存尺寸单位为 **KiB**。
- **宿主内部处理**：控制器与适配器内部统一转换为 **bytes** 进行计算和比较，禁止混用两种来源的原始数值。

## 源码与接口依据

- [QEMU virtio-mem.c](https://github.com/qemu/qemu/blob/v10.2.2/hw/virtio/virtio-mem.c)：`virtio_mem_set_requested_size`、`usable_region_size` 扩展规则、PLUG 请求与预分配。
- [libvirt qemu_hotplug.c](https://github.com/libvirt/libvirt/blob/v12.0.0/src/qemu/qemu_hotplug.c)：内存设备 live update。
- [libvirt Domain Events API](https://libvirt.org/html/libvirt-libvirt-domain.html#virConnectDomainEventMemoryDeviceSizeChangeCallback)：高层回调及其 KiB 单位。
- [上游 viomem.c 源码](https://raw.githubusercontent.com/virtio-win/kvm-guest-drivers-windows/34f4a57438f662d0afde43020addc87fd60f5472/viomem/sys/viomem.c)：Windows virtio-mem 驱动主循环与位图分配实现。
- [Windows 内存资源通知 (CreateMemoryResourceNotification)](https://learn.microsoft.com/en-us/windows/win32/api/memoryapi/nf-memoryapi-creatememoryresourcenotification)：高低水位通知行为。
- [MEMORYSTATUSEX 结构体](https://learn.microsoft.com/en-us/windows/win32/api/sysinfoapi/ns-sysinfoapi-memorystatusex)：来宾物理内存总量与可用量。
