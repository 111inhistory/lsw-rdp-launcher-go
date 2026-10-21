# 内存功能验证记录

日期：2026-10-19 至 2026-10-20。环境：win11、QEMU 10.2.2、libvirt 12.0.0、viomem 100.101.104.28500。

## 本轮减法后复验

- 内存模块仅保留 Win32 读取、变化判断和数据结构；采样并入后台现有 Tokio 循环。删除独立线程、通知句柄、共享样本锁、停止协议和算术微测试。
- `go test -race ./... -count=1`、`go vet ./...`、Windows 交叉构建及原生测试通过。
- 真实 watch_status 首帧返回两个内存字段，不再带 low。
- 真实控制器约 5.30 秒提交回收、35.30 秒确认 Paused；保留 30 秒窗口。恢复后重新订阅，观察到 Windows 物理内存变化；随后小幅增长也通过验证。
- 最终 VM 为 Running，requested 恢复到 1 GiB；actual 保留 1.25 GiB，属于部分回收，没有报告为完全恢复。
- quit 后确认开发后台退出，所属 ETW、临时任务及防火墙规则已清理。未终止用户进程。
- 当前开发二进制 SHA-256：`f0df2446bd5425f5f74fce789bc3e72edfeefffeef4d9ad8cc6887280435f698`。

## 减法前的验证结果

以下保留原轮次记录。当前实现已删去 Low/High 路径和 low 字段，改为单一采样；微测试不再保留。当前设计见 [内存控制](memory.md)。

- Go 全套测试通过，含 50 轮竞态检查和 `go vet`。
- Windows 原生测试通过，不以 Wine 结果代替。
- 收尾时删除不现实容量的 uint64 溢出检查、u128 百分比运算和对应极值用例，再次通过 Go 竞态检查和 Windows 原生验证。
- 该轮开发二进制 SHA-256：`5c2cbe4e292fd9ea474932bb4464091d29a4b89b47e940dbaadee8615714693f`。
- Agent 返回真实物理总量、可用量和原生 low 状态；不保存 95%/80%。
- 95% 启动、80% 停止、非网格 actual 的最终对齐、事件先于 RPC 返回、Unknown、容量与宿主预算边界均有测试。
- 驱动续处理要求新进展、保留原截止时间、最多 8 次；无进展不重发。该碎片化序列通过模拟测试，未对真实 guest 做全容量压力实验。
- 慢 UI/RPC 不阻塞内存 I/O；内存操作完成不取消 UI 操作。控制器退出等待操作清理。并发与清理测试通过。

## 真实内存与生命周期

### 小幅增长

动态目标从 1 GiB 调整到 1.125 GiB。QMP actual 到达 1,207,959,552 bytes。Windows 可见物理总量从 4,204,240,896 增加到 4,338,458,624 bytes，差值恰为 128 MiB。

恢复原 requested=1 GiB 后，actual 未完全缩回，保留 1.125 GiB。没有强制关机、重启或终止业务进程来回收。

### 暂停阶段的窗口准备

在已确认 Paused 的 VM 上设置 capacity，再设置向上对齐的 actual。capacity 为 13 GiB，actual 在准备前后均为 1.125 GiB，没有实际插入 13 GiB。该次窗口设置与核对耗时约 68.5 ms，不作为时延保证。

### 真实控制器循环

本轮复跑 `TestLiveMemoryReclaimSuspendResume`：

- 配置空闲 35 秒后暂停。
- 约 5.060 秒接受回收请求。
- 约 35.061 秒确认 Paused，即保留约 30 秒回收窗口。
- 暂停时剩余 actual=1,088,421,888 bytes，并非 128 MiB 网格值。
- 恢复前准备窗口，将目标向上对齐到 1,207,959,552 bytes。
- 恢复后重新订阅，后台、Windows 内存与 QEMU 设备观测可用，没有启动 FreeRDP。
- 启用策略时的窗口初始化、业务派发和两类观测组合也通过真实 Session 验证。

恢复与清理最终确认 VM 为 Running，原 requested=1,073,741,824 bytes 已恢复。actual 保留为 1,207,959,552 bytes。requested 与 actual 的差值没有被报告为回收完成。

## 边界与未覆盖

没有分配测试内存将 Windows 压到 95%，也没有制造长时间全容量碎片化压力。百分比状态机和续处理通过模拟测试；真实 VM 验证覆盖小幅增长、partial reclaim、窗口准备、暂停、恢复与重新订阅。

没有安装或修改内核驱动、Secure Boot、inactive XML、旧 Agent 安装、用户配置、Keyring 或 PATH。代码对当前固定版本提供宿主缓解，不宣称修复所有内核故障。

测试使用 `C:\\RdpLauncher`、独立端口 49253 和临时 SYSTEM 任务。收尾时退出测试后台，删除本轮临时任务与防火墙规则，检查所属 ETW session。保留开发二进制、源码和日志。VM 保持 Running，旧生命周期服务保持测试前的 inactive/disabled。

## 重跑

以下测试会调整 live 内存或暂停 VM，只在测试环境显式运行：

```sh
RDP_LIVE_MEMORY=1 RDP_LIVE_BACKGROUND=192.0.2.14:49253 \
  go test -v ./internal/adapter \
  -run '^TestLiveMemory(ReclaimSuspendResume|SessionObservations)$' -count=1 -timeout=100s
```

运行前应启动新版测试后台。默认 `go test ./...` 跳过所有真实环境测试。
