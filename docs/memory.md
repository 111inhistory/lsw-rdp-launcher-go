# 自动内存控制

内存策略位于 Linux 宿主。Windows Agent 只采集和通知，不决定容量。

## 启用与配置

当前入口是 Go 配置。项目尚未接入 TOML 和 CLI。

```go
cfg.Policy.Memory = &controller.MemoryPolicy{
    StartPercent: 95,
    StopPercent:  80,
}
cfg.Policy.VMSuspend = 5 * time.Minute // 已有的空闲暂停配置。
// cfg.MemoryAlias 默认为 ua-virtiomem0。
```

`Memory` 为 nil 时关闭自动内存控制。两个百分比为零时采用 95/80。配置应满足 `0 < stop < start <= 100`。

以下值固定，不增加配置项：

- 扩容目标对齐粒度：128 MiB。
- 回收提前量：30 秒。
- 单次扩容总截止时间：30 秒。
- 驱动续处理：静默 1 秒后检查，最多追加 8 次通知。
- 宿主预算：增长后至少保留 1 GiB `MemAvailable`。预算检查不是内存预留。

## 双阈值

占用率指 Windows 可见物理内存，不是 commit 使用率：

```text
used = total_bytes - available_bytes
usage = used / total_bytes
```

宿主在占用率达到 95% 时启动扩容周期。在占用率降到 80% 前，宿主继续评估扩容。处于 80% 和 95% 之间时，宿主保留原来的周期状态。

宿主根据已用物理内存估算目标：

```text
wanted_guest_total = ceil(used * 100 / stop_percent)
extra = max(wanted_guest_total - guest_total, 0)
raw_device_target = current_actual + extra
device_target = ceil(raw_device_target / 128MiB) * 128MiB
```

宿主将目标限制在设备容量内。设备容量和 block 从 live XML 读取；当前实现要求 block 可整除 128 MiB。

对齐的是最终目标，不是增量。例如 actual 为 386 MiB 时，不能通过加 128 MiB 得到 514 MiB。Windows 可见物理内存还受硬件保留等因素影响，不保证自身是 128 MiB 的整数倍。

设置成功仅表示请求被接受。宿主分别观察 QEMU actual 和 Windows 可见内存。Windows 总量没有变化且占用率仍高时，宿主不会用同一份旧数据连续增加新目标。

达到 80% 后，宿主停止追加扩容请求，不反向缩容。已被驱动接受的目标仍可能继续执行。达到设备容量或宿主预算上限时，宿主不越界增长。

## Agent 通知

后台协议在原有 SSH 状态外增加 `memory`：

```json
{"status":"ok","role":"background","kind":"snapshot","ssh_active":false,"memory":{"total_bytes":4294967296,"available_bytes":1073741824},"source":"etw"}
```

- `total_bytes` 和 `available_bytes` 来自 `GlobalMemoryStatusEx`。
- `memory: null` 表示观测不可用，不表示零内存或零占用。启用策略时，宿主等待观测恢复，不回收、扩容或自动暂停。
- `source: etw` 只描述 SSH 来源，不表示内存百分比来自 ETW。

Agent 每秒本地采样一次，不再叠加 Windows Low/High 事件路径。采样直接放在后台已有的 Tokio 事件循环中，不另建线程或停止协议，也不维护 Win32 通知句柄、共享样本锁或备用路径。VM 恢复后跳过错过的 tick，不补发采样。

当占用率的整数上下界、物理总量或观测可用性变化时，Agent 发布通知。只比较一个整数边界会漏掉恰好到达 95% 或 80% 的情况，所以 Agent 同时比较 floor 和 ceil。Agent 不保存策略阈值。

可用字节数在同一个百分比区间内波动时，Agent 不逐次推送。长连接仍是首次快照和变化通知，没有应用层心跳。

## suspend 前回收

假设空闲暂停时间为 300 秒：

```text
0 秒：开始空闲计时。
270 秒：请求设备动态目标 0，保持 VM 运行。
300 秒：重新检查空闲条件，然后 suspend。
```

空闲暂停时间不足 30 秒时，宿主立即开始回收，不额外延长暂停计时。

请求动态目标 0 不会移除启动基础内存。驱动也不保证 actual 能回到 0。到期时允许保留未回收的内存并暂停，状态继续展示 actual，不宣称已经全部回收。

新请求、SSH 活动、lock 或其他空闲条件变化会取消暂停。取消不能撤销已经接受的内存请求，也不自动恢复一个固定工作档位。后续扩容仍按物理占用率决定。

## 本轮减法

目标是减少需要理解和维护的机制，不是节省几个 CPU cycle。

| 概念 | 决定 | 保留或删除的理由 |
|---|---|---|
| 原生 Low/High 与周期采样双轨 | Replace | 统一为 1 秒采样；不再报告原生 low，也不再提前唤醒。业务仍以宿主的 95%/80% 为准。 |
| MemoryLibvirt 两层 Once | Merge | 所有关闭都经过同一 Close，同时完成关闭和 drain。 |
| Agent 请求多层超时 | Merge | Call 负责一个总时限；保留 AfterFunc 中断已建立连接的 I/O。Session 不再给 Dispatch 叠加时限。 |
| 简单算术与分支的微测试 | Delete | 不重复手算百分比；保留取消、并发、真实采样通知和暂停恢复的行为验证。 |
| 驱动窗口准备、生命周期确认和真实容量检查 | Keep | 删除会破坏当前驱动上的扩缩容或状态确认。 |

本轮不改 SSH ETW、Niri、业务 FIFO、意外断线恢复策略或 XML 表达。没有新增配置项。

## 当前 Windows 驱动的缓解

当前 `viomem.sys` 存在位图越界搜索和单次只处理一个空闲段的问题。宿主采用两个针对性步骤。

### 扩大允许地址窗口

宿主只在 VM 已确认 Paused 时依次设置：

1. 将 requested 设为设备 capacity，扩大 QEMU usable region。
2. 将 requested 设为向上对齐的 actual。
3. 确认安全目标设置成功，再 resume。

QEMU 10.2.2 在 setter 中改变允许地址窗口。实际插入内存仍需要 guest 发出 PLUG 请求。暂停的 vCPU 不会因为临时的 capacity 目标执行全容量扩容。

收到有效后台观测后，控制器先确认暂停、设置窗口、确认恢复，再派发首个业务。正常恢复时也刷新窗口。初始化会短暂停顿 RDP/SSH，不是每次扩容都暂停。lock 只阻止空闲暂停，不阻止这次准备。整个流程复用现有 VM 生命周期确认。

UI 启动和远端请求可以与内存 I/O 并行，避免慢请求堵住压力处理。VM 控制仍共用一个串行位置，不并发提交两个 VM 变更。退出时，控制器等待已取消操作的有界清理完成，避免退出后仍有写入。

最终恢复目标取自 actual，而不是之前可能遗留的 capacity。因此控制器重启后可以重新执行准备。若请求中断，适配器用独立的 5 秒 context 尝试恢复安全目标。安全目标无法恢复时，宿主不自动 resume，返回错误并保留 Paused。下一次经控制器恢复时会重新准备目标。

**注意：不要在这两个写入步骤之间用另一个控制器或 virsh resume。** 内存控制要求单一写者。初始化期间退出控制器可能保留 Paused，应通过控制器重新请求恢复。

<a id="recovery"></a>
### 准备失败后的人工恢复

先运行 `rdp-launcher status`，检查 `VM`、`LastError` 和 `Memory.LastError`。无法确认安全目标时，错误会明确提示 VM 保持 Paused，并指向本节。`Memory.LastError` 非空也可能只表示自动扩缩容被停止，不能一律理解为可以绕过准备。

1. 先排除具体故障：libvirt 连接／权限、配置的设备 alias、设备容量／块大小、宿主内存余量。来源临时断线会自动重连；写入或安全错误不会因重连而消失。
2. 修复原因后，可以再次通过控制器请求 `rdp-launcher ping`。暂停状态下会重新读取设备并准备安全目标。成功唤醒不代表此前的驱动错误已消失；自动扩缩容若仍被错误锁止，应在修复后重启控制器重新评估。
3. 若准备始终失败，不要循环执行 `virsh resume`。下面是明确停用自动内存控制后的人工降级流程，不是跳过安全校验的 force-resume。

人工降级需要操作者有 libvirt 权限，并了解当前域与 alias。以下使用 `qemu:///system`、域 `win11`、alias `ua-virtiomem0`；先按实际配置替换。停止服务会关闭它持有的 FreeRDP 客户端，但不注销 Windows 用户会话。

```sh
systemctl --user stop rdp-launcher.service
virsh -c qemu:///system domstate win11
virsh -c qemu:///system dumpxml win11 --xpath "/domain/devices/memory[@model='virtio-mem'][alias/@name='ua-virtiomem0']"
```

必须确认域仍为 Paused，且没有其他内存写者。读取该设备的 `target/current`，确认单位；不能用旧 `requested`、设备 `size` 或缓存值代替实际 `current`。缺失或无法解释这个值时应停止，不猜测为零。

若 XML 中 `current` 的单位为 KiB，手工填写其数值后执行：

```sh
actual_kib=... # 填上一步 current 的 KiB 数值，不填 capacity 或 requested
virsh -c qemu:///system update-memory-device win11 --alias ua-virtiomem0 --requested-size "$actual_kib" --live
virsh -c qemu:///system dumpxml win11 --xpath "/domain/devices/memory[@model='virtio-mem'][alias/@name='ua-virtiomem0']"
```

第二次读取必须确认 `requested` 与 `current` 在相同单位下相等，且域仍暂停；写入失败或数值不符就停止。这个目标不要求分配新的动态内存，也没有扩展可用窗口。

在宿主配置中移除整个 `[memory]` 段以停用自动内存控制（把百分比填零不会停用）。完成上述核对后，才可恢复并重新启动控制器：

```sh
virsh -c qemu:///system resume win11
systemctl --user start rdp-launcher.service
rdp-launcher status
```

此时 VM 使用已确认的现有内存，自动扩缩容关闭。不要在问题未定位时直接重新启用 `[memory]`；重新启用会再次执行窗口准备。

### 续处理同一扩容目标

如果 actual 有增长但尚未达到目标，随后静默 1 秒，宿主可再次提交同一目标来唤醒旧驱动。

每次续处理都要求实际增长超过上一次通知时的 actual。没有进展时不重复提交。最多追加 8 次，且不延长原来的 30 秒截止时间。达到 80% 后不再续处理。

到期仍未满足目标时，宿主记录 target、actual 和 Windows 总量，并停止本轮自动写入。暂停/恢复不被视为修复了未知驱动故障。冷启动或重建控制器后重新评估。

这些步骤不是驱动补丁，也不保证恢复所有内核故障。源码依据见 [驱动核查](virtio-mem-driver-notes.md)，实测范围见 [内存验证记录](memory-validation.md)。
