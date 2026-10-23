# 自动内存控制

Linux 宿主决定内存容量。Windows Agent 只报告物理总量和可用量，不保存阈值，也不写入内存设备。

## 配置

在宿主 TOML 中添加：

```toml
[memory]
start_percent = 95
stop_percent = 80
```

配置应满足 `0 < stop < start <= 100`。两个值都为零时使用 95/80；**移除整个 `[memory]` 段才会关闭自动内存控制**。设备 alias 使用 `[lifecycle].virtio_mem_alias`，默认 `ua-virtiomem0`。

库内配置对应 `Options.Memory`，值为 nil 时关闭。设备容量和 block 从 live XML 读取；block 应能整除 128 MiB。

固定参数如下：

| 参数 | 值 |
|---|---|
| 目标对齐粒度 | 128 MiB |
| 挂起前回收提前量 | 30 秒 |
| 单轮扩容期限 | 30 秒 |
| 有进展后的续处理等待 | 1 秒 |
| 追加通知上限 | 8 次，不延长总期限 |
| 宿主预算 | 扩容后至少保留 1 GiB MemAvailable |

宿主预算检查不预留内存，其他进程仍可同时消耗宿主内存。

## 双阈值扩容

占用率使用 Windows 物理内存，不是 commit 使用率：

```text
used = total_bytes - available_bytes
usage = used / total_bytes
```

达到 start 阈值后开始扩容。降到 stop 阈值后停止追加请求。在两个阈值之间，控制器保持原来的扩容周期状态，不反向缩容。

```text
wanted_guest_total = ceil(used * 100 / stop_percent)
extra = max(wanted_guest_total - guest_total, 0)
raw_target = current_actual + extra
device_target = ceil(raw_target / 128 MiB) * 128 MiB
```

最终目标受设备容量和宿主预算限制。对齐的是最终目标，不是增量。Windows 硬件保留可能使可见总量不按 128 MiB 对齐。

写入成功只代表请求被接受。控制器分别观察设备 actual 和 Windows 总量；旧观测不能触发连续追加。若 actual 有新增长、随后静默 1 秒，可重新提交同一目标推进驱动，最多追加 8 次。无进展时不重试。

到期仍未收敛时，控制器记录 target、actual 和 Windows 总量并停止本轮写入。暂停或恢复不清除未知驱动故障。来源重连只清除连接丢失错误，不清除协议、写入或恢复安全错误。

## 内存观测

Agent 每秒用 `GlobalMemoryStatusEx` 采样。首次发送快照，之后在占用率整数边界、物理总量或可用性变化时通知。Agent 同时比较占用率的 floor 和 ceil，以保留阈值边界。

`memory:null` 表示 Unknown，不是零占用。启用策略时，未知内存阻止扩容、缩容和自动挂起，但不重置空闲注销时钟。SSH 的 ETW 来源与内存采样无关。

## 空闲回收

空闲注销默认关闭。关闭时，若挂起超时为 300 秒，控制器在第 270 秒请求动态目标 0，第 300 秒复查条件后挂起。超时不足 30 秒时立即开始回收，不额外延长原挂起时间。

启用空闲注销时，控制器不在注销未决期间提前缩容。确认登录结束后，控制器尽快回收。若仍在扩容、处于压力状态或内存未知，待回收标记保留到条件安全时。

目标 0 只回收动态设备内存，不移除基础内存。驱动不保证 actual 回到 0。回收等待有界，到期允许保留未回收内存并挂起；状态继续展示实际值。

新业务、SSH 活动、来源失效或 lock 会取消空闲计划。取消不能撤销已接受的内存请求，也不会恢复固定工作档位。后续扩容仍由占用率决定。

## 暂停窗口准备

部分 Windows virtio-mem 驱动需要扩大 QEMU 允许的地址窗口，详见 [驱动约束](virtio-mem-driver-notes.md)。控制器在已确认 Paused 的 VM 上按顺序执行：

1. 将 requested 设为设备 capacity，扩大 usable region。
2. 将 requested 设为向上对齐的 actual。
3. 确认安全目标后恢复 VM。

暂停的 vCPU 不执行 PLUG，所以临时 capacity 目标不等于实际插入全容量。最终恢复目标取自 actual，而不是可能遗留的 requested。

自动初始化会短暂停顿 RDP/SSH。lock 禁止开启新的自动初始化暂停周期；显式用户请求仍可准备。已接受的 pause 应完成必要的安全收尾和恢复，不能因 lock 把 VM 留在中间态。

VM 变更保持串行。允许阶段中的内存 I/O 可与用户会话 I/O 并行。发生取消或失败时，适配器使用独立 5 秒期限尝试恢复安全目标。无法确认时保持 Paused 并报告错误。

**警告：不要在准备期间用另一个控制器或 virsh 恢复 VM。** 控制器要求该暂停窗口只有一个写者，否则 guest 可能看到临时容量目标。

<a id="recovery"></a>
## 准备失败后的人工恢复

**警告：不要循环执行 `virsh resume` 绕过安全检查。** 无法确认安全目标时，应保持 VM 暂停。

1. 查看 `rdp-launcher status` 中的 VM、最近错误和内存错误。
2. 排除 libvirt 权限、设备 alias、容量、block 或宿主余量问题。
3. 修复后通过 `rdp-launcher ping` 请求恢复。控制器会重新读取并准备安全目标。

成功恢复不代表先前驱动错误已清除。若自动写入仍因错误停止，应在修复后重启控制器评估。

若仍无法准备，可停用自动内存控制并人工降级。以下变量应替换为实际配置：

```sh
VM_NAME=windows-vm
MEMORY_ALIAS=ua-virtiomem0
systemctl --user stop rdp-launcher.service
virsh -c qemu:///system domstate "$VM_NAME"
virsh -c qemu:///system dumpxml "$VM_NAME" \
  --xpath "/domain/devices/memory[@model='virtio-mem'][alias/@name='$MEMORY_ALIAS']"
```

停止服务会断开它持有的 FreeRDP，不注销 Windows 登录。确认 VM 仍为 Paused，且没有其他内存写者。读取设备 `target/current` 及单位；不能使用缓存、capacity 或旧 requested。值缺失或无法解释时停止。

如果 current 的单位为 KiB，手工填写数值并执行：

```sh
actual_kib=... # 填入刚读取的 current，单位 KiB。
virsh -c qemu:///system update-memory-device "$VM_NAME" \
  --alias "$MEMORY_ALIAS" --requested-size "$actual_kib" --live
virsh -c qemu:///system dumpxml "$VM_NAME" \
  --xpath "/domain/devices/memory[@model='virtio-mem'][alias/@name='$MEMORY_ALIAS']"
```

确认 requested 与 current 在相同单位下相等，且 VM 仍暂停。写入失败或值不符时停止。这个目标不会要求新的动态内存。

移除宿主配置的整个 `[memory]` 段，再执行：

```sh
virsh -c qemu:///system resume "$VM_NAME"
systemctl --user start rdp-launcher.service
rdp-launcher status
```

VM 此时使用已确认的现有内存。故障未定位前，不应重新启用自动内存控制。
