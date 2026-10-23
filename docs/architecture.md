# 架构

## 范围

一个 Linux 控制器管理一个 Windows VM、一个自持 FreeRDP 连接和一个业务 FIFO。控制器可观察同一 Windows 用户的多个登录，但不自动接管外部连接，也不注销该用户的全部登录。

```text
Linux
  CLI（含确认窗） ── Unix socket ── controller
                                      ├─ libvirt：生命周期、内存设备
                                      ├─ Niri：窗口观测
                                      ├─ FreeRDP：自持显示连接
                                      └─ Windows 后台：状态订阅、业务请求

Windows
  backend（SYSTEM / Session 0，单一 TCP 入口）
    ├─ SSH、物理内存、WTS 快照
    ├─ 系统 logoff.exe
    └─ 本机命名管道 ── user-worker（目标登录的用户 token）

  remoteapp-shell：RemoteApp 保活，无请求入口
```

## 模块

| 模块 | 职责 |
|---|---|
| `cmd/rdp-launcher` | CLI、配置入口、确认呈现。 |
| `internal/host` | Unix IPC、配置、Keyring、应用缓存和桌面导出。 |
| `internal/controller` | 单所有者状态机、FIFO、自动策略、过期结果隔离。 |
| `internal/adapter` | libvirt、Niri、FreeRDP 和 Windows 协议 I/O。 |
| `internal/dialog` | 进程内确认窗、系统主题和 DMS 主题。 |
| `windows-agent` | Windows 观测、用户 worker 和精确目标注销。 |

组装入口是 `adapter.Open(ctx, config)` 与 `Session.Run()`。仓库内调用方通过 `Controller.Do` 提交请求，通过 `Snapshot` 读取状态。`internal` 包不是稳定的公共 SDK。

## 主循环与执行器

主循环独占业务状态，不执行阻塞 I/O。消息源发布观测，执行器返回操作结果：

```text
接收消息 → 更新状态 → 选择下一项操作 → 异步执行 → 完成消息
```

VM 查询和变更共用串行控制位置。内存 I/O 可在允许的阶段与用户会话 I/O 并行，但不会并发提交两个 VM 变更。生命周期和来源消息不经过业务 FIFO。

控制器区分命令接受与状态确认。libvirt RPC 成功不等于 VM 已进入目标状态。控制器同时观察命令结果和生命周期事件，确认目标 VM 的实际状态后才继续。确认来源丢失或超时会使状态变为 Unknown，不重放旧命令。

操作携带来源、操作 ID、VM 代次、会话来源代次和空闲计划代次。晚到结果不得恢复已失效的绑定，或替新的空闲计划完成旧步骤。

## 请求、模式与确认

`Request.UserContext` 表示需要用户上下文，`Request.Mode` 表示可选显示模式。两者独立。

| 请求 | 用户上下文 | 模式要求 |
|---|---|---|
| `ping` | 否 | 无。 |
| `apps`、`sync-apps`、`mounts` | 是 | 复用当前绑定；没有显示时默认建立 RemoteApp。 |
| `run`、`open` | 是 | CLI 默认 RemoteApp，桌面冲突时可选择 `current`。 |
| `desktop`、`remoteapp` | 是 | 显式指定目标模式。 |

队头按顺序等待 VM Running、后台就绪及所需的用户绑定。后续请求继续排队。应用派发完成后推进队列，不等待应用退出。

模式冲突仍属于原来的同一请求。控制器通过持续 IPC 连接向发起 CLI 请求一次确认。确认最多等待 60 秒，也受请求总期限约束。`current`、`keep`、`logoff`、`cancel` 是互斥动作，不拆成“重连”和“注销”两个布尔开关。

当前模式为桌面时，即使 UI 计数为零，业务冲突也等待明确选择。空 UI 不是注销授权。确认期间连接、登录或来源改变，会使旧确认失效。

多文件请求使用一个批次。外层携带上下文和模式，批次项只携带业务 payload。批次首次失败后停止，回复保留已完成部分；不自动重放。

## 连接与登录绑定

Windows 登录身份使用完整 `SessionRef`：

- Session ID。
- token AuthenticationId LUID。
- 内核 BootIdentifier。

SID 另行限定配置账号。Session ID 可能复用，用户名也可能在本机和域中重复；二者均不能单独作为登录身份。

连接身份包含自持 FreeRDP 代次、稳定 `client_hostname` 与 WTS ConnectTime。它与登录身份分开：重连可以保留原登录身份，但会产生新的连接观测。

控制器在启动 FreeRDP 前取得新鲜基线。启动后，只接受基线中没有的新登录，或 ConnectTime 已变化的登录。候选还应同时满足 Active、配置账号 SID、稳定 hostname、完整 SessionRef，并且唯一。用户 worker 应返回实际身份并就绪后，控制器才派发业务。

控制器不通过日志解析、进程名或“只有一个 Active”猜测归属。断开后 WTS 客户端字段可能清空，所以控制器保留已确认绑定，不从清空字段重建。

Windows 后台在派发前校验完整 `SessionRef` 与配置的 `SID`，随后以目标登录的 WTS 用户 token 在受限本机命名管道上启动 `user-worker`。`session_status` 返回 worker 的实际 `SessionRef` 与 `SID` 以确认连接绑定，worker 内部不再进行冗余的逐请求身份复核。应用枚举、盘符和 ShellExecute 均在该用户上下文执行，不回退到 SYSTEM。

## 消息源与恢复

- **libvirt**：订阅成功后再查询。控制 RPC 与生命周期订阅使用独立连接。新生命周期事件优先于旧查询结果。
- **Niri**：先建立窗口快照，再按窗口 ID 更新。断线表示 Unknown，不表示窗口为零。
- **Windows 后台**：VM Running 时订阅，暂停或关闭时取消旧订阅。首次发送快照，之后只发送变化。没有应用层心跳或固定读取超时。
- **FreeRDP**：控制器只等待和回收自己创建的子进程。进程存在不等于登录已绑定。

来源分别退避重连。重新订阅后补查快照，不能继续信任断线前缓存。恢复只恢复观测，不重放 `run`、`open`、注销或结果未知的写入。

SSH 状态为零、非零或 Unknown，表示来宾本地 22 端口的已建立 TCP 连接，不等同于认证登录数。ETW 事件触发 TCP 表核对；初始化和 60 秒检查提供兜底。

## 空闲计划

1. RemoteApp 窗口为零后，开始显示断开计时。桌面模式不自动断开。
2. 显示断开、无待办业务、后台有效且 SSH 为零时，开始注销和挂起计划。
3. 若启用注销，只核对最后绑定的登录。控制器先新鲜查询，不用断开前缓存决定。
4. 若同一连接暂时仍为 Active，控制器以 500ms 间隔等待，查询共用 30 秒总预算。客户端或 ConnectTime 改变则保留登录；过渡超时也保留并说明原因。等待期间不抢先挂起。
5. 注销确认后，内存条件允许时立即回收；未启用注销时，在挂起前 30 秒回收。
6. 到达挂起时间后重新检查准入，允许有界回收未完全完成时保留 actual 并挂起。

内存压力、Growing 或内存未知只阻止缩容和挂起，不重置注销时钟。新业务、SSH 活动、来源失效和 lock 会废弃旧计划。已经提交的副作用无法靠取消撤销。

注销由 SYSTEM 启动固定 `%SystemRoot%\System32\logoff.exe`。退出码 0 不等于成功；后台应确认原引用消失。Agent 操作预算为 30 秒，宿主为 35 秒，均受调用者更短期限约束。系统命令只接收 Session ID，因此最后校验与执行之间仍存在无法完全消除的 ID 复用窗口。发现新引用时只报告异常，不追加操作。

## lock 与安全收尾

lock 禁止新的自动断开、注销、缩容、挂起和自动内存初始化暂停周期。显式用户请求仍可执行，无需暂停的压力扩容仍可继续。

lock 不撤销已接受的操作。控制器继续状态确认和必要的安全收尾。若内存初始化 pause 已被接受，控制器完成安全目标准备与 resume，避免把 VM 留在不安全的中间态。

lock 只存在于服务内存，VM 关闭或服务重启后清除。它与 Unix socket 旁的所有权锁文件无关。

## IPC、凭据与边界

宿主用权限 0600 的 Unix socket 提供本地 IPC。控制器在整个运行期持有同目录持久 `.lock` 文件的 flock。未获得所有权时，不删除现存 socket；连接超时也不能当作死 socket。

RDP 密码和 Agent token 分开保存在 Keyring。密码只通过 stdin 交给 FreeRDP。token 附加到 Windows 请求中，不写入命令行、日志或本地控制 IPC。

Windows 只有一个宿主 TCP 入口。用户 worker 使用受限本机管道；RemoteApp shell 不接收业务请求。共享 token 不提供网络加密。协议与权限细节见 [Windows Agent](../windows-agent/README.md)。

WTS 原生查询不能通用地中断。超时会使结果失效，不允许晚到结果恢复旧代次。命名管道取消后应等待内核完成 I/O，再释放缓冲区；因此极端取消清理可能超过调用期限，不能用提前释放内存伪造有界退出。

内存算法、驱动约束和人工恢复见 [memory.md](memory.md)。
