# suggestion.md 当前核对

核对日期：2026-10-20。依据为当前代码、既定就绪契约和测试，不按原文严重程度直接修改。

首次核对只更新记录。后续用户要求做减法，11/12/15 已落实如下；其他项仍是核对结论。

## 逐项结论

| Issue | 当前结论 | 依据与处理建议 |
|---|---|---|
| 01 | 不是现有契约下的缺陷 | 交互请求仍依赖后台，但这是两种模式共用的前置条件。`docs/architecture.md:113` 已明确，`TestLoopInteractiveWaitsForBackendAfterDisplay` 也明确测试该行为。内存启用后还先等待后台观测和窗口准备，原建议只改最后的 Dispatch 条件已不完整。若要独立交互，应先改变契约。 |
| 02 | 保留 TODO，修正描述 | `autoFailed` 仍阻止自动动作，但新请求和 VM 状态变化会复位，不是只能重启进程解除。无人干预时的恢复仍缺失；是否增加冷却恢复属于部署策略，不是当前回归缺陷。 |
| 03 | 问题仍成立，术语有误 | `Display.Stop` 超时仍只返回错误，不强杀；忽略 SIGTERM 的自有进程可能留下。它是仍存活的挂起进程，不是已退出但未回收的 zombie。启动失败清理和 Session 结束已有 Kill 路径，不能概括为整个进程生命周期都没有清理。 |
| 04 | 行为仍存在，修复前提不成立 | `LastError` 的确同时禁止后续增长与回收。但该错误还包括权限、设备、事件源、恢复安全目标失败等情况，不能统一视为仅 PLUG 失败。UNPLUG 也可能失败或只能部分回收。应先区分可继续回收的已知错误与未知设备状态，再决定是否放行，不直接增加一个通用 GrowthFrozen。 |
| 05 | 未证实为正常时序缺陷 | 取消时确实没有清空 `reclaimedFor`，但新周期会重新计算 deadline，旧时间不会阻止新的 deadline。原文依赖“新 deadline 恰好相同”，未给出当前固定超时、单调时钟下的实际路径。现有取消测试已覆盖取消暂停。应补具体重复周期复现，再决定是否改字段。 |
| 06 | 已被新实现替代，不采用原方案 | 当前已在 Paused 阶段扩大 usable window，再设置向上对齐的 actual 后恢复；不是在运行中的 Windows 上实际插满容量再回收。窗口准备已有真实验证，无需再加 WarmupUsableRegion 配置项。UNPLUG_ALL 等路径可缩小窗口，所以“永久 100% 免疫”也不是成立的保证。 |
| 07 | 部分已解决，保留诊断改进 | 当前已有 QEMU actual 与 Windows 总量两类确认、截止时间和相关测试，超时也输出 target/actual/guest_total。还能改进等待原因的显示，但不必为显示诊断增加四个持久阶段。确认可乱序到达，且达到 80% 后允许停止追加请求，不应强制改成严格先 QEMU 后 guest 的流水线。 |
| 08 | 精简方向可保留，替换方案不完整 | 自定义 XML 树仍在。但 libvirt live update 会比较原设备的 size、block、NUMA、address、access 等字段，不能只提交 requested 后丢弃其余定义。可改成保留字段的结构化方案；当前 XML 保留测试不能删除。原文将解析、字段读取、单位转换等都归为近 300 行解析器，也不准确。 |
| 09 | 低优先级整理，不是已证实性能问题 | `chan any` 和匿名唤醒仍在。内部通道还传 job、结果和锁请求，不只传公开 Event。改为具名唤醒可提高可读性；改成 chan Event 则需同步统一内部消息接口。“泛型逃逸”不是此处准确术语，也没有性能证据。 |
| 10 | 行数与“非自动化”描述过时 | Live 测试现已增加真实回收/暂停/恢复、窗口准备和清理用例。它们有断言、超时和清理，是显式启用的自动化集成测试，不是因为默认 Skip 就变成手工脚本。目录或 build tag 可另行整理。纯状态机与实际事件循环测试覆盖不同层，不应按断言相似直接合并。 |
| 11 | 已合并预算，保留 I/O 取消 | 当前已由 Call 统一请求总时限，删除 Dispatch 外层时限、Call 的独立 Dialer 时限和 SetDeadline；AfterFunc 保留。Dialer 的连接预算、总请求预算和取消读取不是同一作用。list_apps 读取图标、VM 暂停等情况也不符合“所有 RPC 都是微秒级”的前提。普通 net.Conn 的 Read/Write 不会自动响应 ctx 取消。只保留 WithTimeout 和 defer Close，可能卡在 Decode，永远到不了 defer。deadline 与 AfterFunc 可以择一承担部分责任，但删除后必须保留实际的 I/O 取消路径。 |
| 12 | 已合并 | 当前只由 Close 的一处 Once 负责关闭与 drain，Watch 也经过该入口。“未启动 Watch 也能 drain”和并发关闭的保证仍保留。 |
| 13 | 调用点仍在，性能结论缺证据 | 循环中确实多次检查生命周期通道，但这是有意优先处理已排队生命周期，避免旧 RPC 结果先推进。调用 len/非阻塞取消息不等于每次查询 libvirt。若批次有 n 条普通消息，调用点为 n+2，不是全局最多 12 次。合并通道需重新证明时序，revision 不能替代尚未应用的消息。 |
| 14 | 未实现恢复，但“崩溃”描述失准 | 意外后台断线仍使 Session.Run 返回错误，Session 内组件随之取消，不会取消调用者的父 context。这是已记录的 fail-fast 边界，不是 panic 或直接杀 Linux 进程；调用者决定是否重建 Session。VM 暂停/恢复导致的正常退订与重新订阅已实现。无人值守部署仍需处理意外断线，但不能自动重放结果未知的 run/open。 |
| 15 | 已替换为单一采样 | 删除 Win32 Low/High 句柄、共享样本锁、备用等待路径和 low 字段。保持固定 1 秒采样，不新增高水位判断或自适应时限。代价是不再提供原生 low 诊断及提前唤醒。简单算术微测试也已删除，保留跨组件行为验证。 |

## 需要修正的两条总论

### 连接隔离

当前 SDK 和“取消时关闭连接”的实现下，控制连接与事件连接分离有必要。原因是关闭控制连接不应断开订阅，不是只有注册事件后才启动读循环。

关闭 socket 只能结束本地等待，不能撤销服务端已经接受的操作。因此仍不应盲目重放 run/open。

### 扩容停滞

原文“停滞后绝不再次设置同一目标”的概括已过时。当前允许针对旧驱动的有界续处理：先观察到实际进展，再在静默 1 秒后通知一次；最多追加 8 次，不延长 30 秒总截止时间。无新进展时不追加。

30 秒是单次目标的总截止时间，不是从最后一次进展重新计时。续处理不延长截止时间。

这与无条件重试不同，代码见 `internal/controller/memory.go:260` 和 `TestMemoryDrainRequiresActualProgressAndKeepsOriginalDeadline`。

## 当前定位

- 03：`internal/adapter/freerdp.go:137`，Stop 的 ctx.Done 分支。
- 04/05：`internal/controller/memory.go:296`、`:312`，回收与取消。
- 06：`internal/adapter/memory_prepare.go:19`，暂停阶段的窗口准备。
- 07：`internal/controller/memory.go:131`、`:157`、`:226`，两类进度与超时。
- 08：`internal/adapter/memory_libvirt.go:283` 起，XML 树与字段处理。
- 11：`internal/adapter/agent.go:52`、`internal/adapter/session.go:58`。
- 12：`internal/adapter/memory_libvirt.go:49`、`:87`、`:230`。
- 13：`internal/controller/controller.go:185` 起。
- 14：`internal/adapter/session.go:104` 起，errgroup 与后台订阅。
- 15：`windows-agent/src/memory.rs` 只保留读取与判变；采样位于 `backend.rs` 的 serve 循环。

原文件中的行号应视为历史定位，不能继续作为当前准确位置。

## 后续顺序

1. 优先补上自有 FreeRDP 进程停止超时的清理与测试（03）。
2. 正式部署前确定意外断线的恢复边界（14，与 02 合并规划）。
3. 判断哪些明确的增长失败允许继续回收（04）。
4. 关闭入口、请求预算和内存采样已精简（12/11/15）；XML 表达未改。
5. 01/05/06/07/09/10/13 不按原建议直接重构。

本轮运行 `go test -race ./...` 和 `go vet ./...`，均通过。未新增真实 VM 实验；没有把已有测试通过当作 03 已被覆盖或修复。
