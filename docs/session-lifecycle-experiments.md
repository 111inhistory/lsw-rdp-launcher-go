# 会话生命周期重构：实机实验记录

日期：2026-10-22。身份：**lsw-rdp-launcher-go 实验 Agent**。

状态：只有探针和实验结果，**未实现或部署重构**，也未将审查意见自动合并进提案。本文给 `session-lifecycle-redesign.md` 的后续修订提供事实依据。

## 1. 范围与现场

- 使用当前 win11、已部署 Windows agent 和配置中的 FreeRDP 二进制。
- 实验开始时 VM Paused，控制器无显示连接，未锁定；唤醒后没有已登录的用户图形会话。
- 临时 lock 避免控制器把不归它持有的实验 FreeRDP 当成空闲而暂停 VM；没有启动第二个控制器。
- 用单独脚本直接启动 FreeRDP，并仅按本轮记录的 PID 结束它。密码只从现有 Keyring 读取后经 stdin 传入，没有写入实验文件或 argv。
- 所有 Windows 探针放在 `C:\ProgramData\lsw-session-probe-20261022-2040`，不替换生产二进制。临时 SYSTEM 任务仅运行本轮固定探针。
- 原始数据与探针：`/tmp/lsw-session-experiments-20261022/`。主要记录为 `snapshots.jsonl`、`wts-events.jsonl`、`interactive-status.jsonl`、`worker-*.json` 和 FreeRDP 日志。

## 2. 已验证结果

| 项目 | 实际结果 | 对方案的影响 |
|---|---|---|
| RD 的客户端标识 | `/client-hostname:lswD2210a001` 能在 Active 登录的 WTSClientName 中完整读到 | 这种短标识可用于当前连接关联，但不是认证或登录身份 |
| RD 登录 | 首次建立 Session 4，能读取用户、域、客户端地址、协议、LogonTime、ConnectTime | 不必通过进程名猜“哪个登录是自己的” |
| 断开后的客户端属性 | Session 4 保留登录时间，但 ClientName 为空，地址为零，Protocol 返回 0 | **不能在断开后从这些 WTS 字段重新构造连接归属**；需要保留已确认的登录绑定 |
| 同标识 RD 重连 | 保持原标识后重新连入 Session 4，LogonTime 不变，ConnectTime 改变 | 登录实例与连接实例确实需要分开 |
| 改标识 RD 重连 | 两次换成新标识均尝试建立 Session 5，最终连接失败；恢复原标识后立即重连 Session 4 成功 | **不能直接采用“每个 FreeRDP 进程生成新 hostname 代次”**。当前机器上它影响了会话复用行为 |
| RD → RemoteApp | 使用同一个客户端标识，RemoteApp 建立了新的 Session 6；Session 4 留在 Disconnected | 同一标识不保证跨模式复用同一个登录；保留切换必须重新确认目标绑定 |
| RemoteApp 重连 | 使用原标识重新连接 Session 6，LogonTime、AuthenticationId 不变 | 可保留该登录的稳定引用，但连接状态必须更新 |
| RemoteApp → RD | 使用相同标识重新连回 Session 4；Session 6 变为 Disconnected | 当前 Windows 的 RD 与 RemoteApp 登录可以同时保留，不能当成唯一一个“用户会话” |
| 旧交互端口 | RD/Session 4 Active 时，49152 仍由 RemoteApp/Session 6 的旧 agent 回答 `session_active=false` | 固定端口只说明某个 agent 存活，不能代表“当前桌面” |

以上只证明当前固定环境的行为，不声称所有 Windows/RDS 策略相同。此次标识长度均低于 15 字符，未测试协议最大长度或截断规则。

### 改标识重连失败的边界

Windows LocalSessionManager 记录了 Session 5 从 `CsrConnected` 转换时错误 `0x80004005`，并在约 60 秒后断开。现有记录证明“不同 tag 标识 + 保留旧登录导致会话复用失败”（different tag + retained old login failure）的对照，**尚未证明内部根因只是 hostname，也未排除本机多会话策略/第三方改动的影响**。

更重要的是，**本次实验并不等同于从旧版 FreeRDP 实际生效的默认/参数名称到新版固定默认（`rdp-launcher`）的完整升级路径测试**。确切的旧默认向新默认升级验收仍标记为 pending（待真机验证）。因此在真实升级前，用户应保存工作并通过 Windows 正常注销旧登录，或者把 `client_hostname` 显式设为旧连接实际生效的名称，不能未经核对就当作 `os.Hostname()`；不要为了迁移自动调用旧故障 RPC。当前真实业务 VM 处于关机（shut off）状态，不进行未经协调的开机、测试或后台自动注销。

因此，后续设计应优先验证一个稳定的客户端标识，连接代次保留在宿主；不得把未经验证的新随机 hostname 直接部署为连接实例 ID。

## 3. 用户 worker：实际执行位置与环境

通过临时 SYSTEM 任务执行以下固定流程：

```text
确认目标 Session ID + LogonTime + Active + ClientName
→ WTSQueryUserToken
→ CreateEnvironmentBlock
→ CreateProcessAsUser（winsta0\default）
→ 用户上下文探针及 ShellExecuteEx
```

没有提供通用 SYSTEM 命令执行服务。此实验也不决定正式 worker 的 IPC/安全扩展范围。

### RD / Session 4

- worker 身份为配置用户，SID 与该用户一致，进程所在 Session 为 4。
- `USERPROFILE`、`APPDATA`、`LOCALAPPDATA`、`TEMP` 等是用户目录，不是 SYSTEM profile。
- `F:`、`J:` 等当前已有映射可见。
- ShellExecuteEx 启动的 Win32 子进程也位于 Session 4，子进程 whoami 返回相同用户 SID。
- 按现有 UWP 路径调用 `explorer.exe shell:AppsFolder\Microsoft.WindowsCalculator_8wekyb3d8bbwe!App` 后，实际 `CalculatorApp.exe` 进程位于 Session 4。
- 这时 Session 6 的旧 RemoteApp agent 仍存在；worker 没有误用它。

### RemoteApp / Session 6

- 同样的固定 worker 路径在 Session 6 执行成功，用户目录和现有映射可见。
- ShellExecuteEx 的 Win32 子进程位于 Session 6。
- 同模式重连前后，AuthenticationId 保持不变。

### 没有证明的范围

- 映射盘结果只覆盖这台 VM 当前已有的映射；没有据此证明任意登录专属映射、提权 token、DDE、文件关联或所有 UWP 应用都兼容。
- 此实验使用已经完成登录、配置文件已加载的用户。未测试尚未加载 profile 的冷启动或其他用户。
- worker 的 `SESSIONNAME` 环境变量为空；**不能依赖它判断工作位置**。实际 Session 由 ProcessIdToSessionId 确认。
- 实验工作目录是明确传入的探针目录；正式执行的默认 cwd 仍需沿用业务工作目录规则，不能继承 SYSTEM cwd。

## 4. 登录实例与启动实例身份

本轮实测：

- Session 4 的 AuthenticationId 为 `0000000003C517EA`，同模式重连后不变。
- Session 6 的 AuthenticationId 为 `0000000003D481ED`，同模式重连后不变，且不同于 Session 4。
- WTSINFOEX LogonTime 在断开、重连和来宾校时前后保持原值；ConnectTime 会变化。
- `NtQuerySystemInformation(SystemBootEnvironmentInformation=90)` 在此 Windows 上可读取 BootIdentifier GUID；重复读取一致，返回长度 32。

这支持采用 **Session ID + 登录实例标识 + 启动实例标识**，而非只用 Session ID 或当前 ClientName。

BootIdentifier 在本次记录中为 `11111111-2222-4333-8444-555555555555`。**没有为验证 GUID 变化而重启来宾，也未做休眠/Fast Startup 场景；这些不能记作已验收。**

来宾在恢复后一度比宿主慢约 32 分钟，之后发生校时。不要把宿主日志时间和来宾墙钟直接拼接，也不要优先用会随校时推算的 LastBootUpTime 当唯一启动身份。实验通知另存 Stopwatch 计数。

## 5. WTS 事件与查询时机

临时隐藏窗口订阅 `WTSRegisterSessionNotification(..., NOTIFY_FOR_ALL_SESSIONS)`，实际收到：

- Session create（10）；
- remote connect（3）；
- session logon（5）；
- desktop ready（15）；
- remote disconnect（4）；
- lock（7）及会话终止相关通知。

首次 remote connect 到达时，快照曾仍没有用户名、没有有效 LogonTime；稍后的 logon 事件才具有可绑定的完整登录信息。**connect 通知本身不等于应用可派发。**

失败连接阶段，完整 WTS 快照曾超过 15 秒。后续探针改为通知回调只记录事件、不在回调内完整查询，并对未完成登录的过渡会话减少字段查询，但换标识失败仍会发生。因此：

- 事件回调应只触发后台刷新，不承担完整同步查询；
- WTS 查询必须有有界执行/失效处理，不能阻塞控制器或状态推送；
- 不能据本次超时把每个 WTS API 都宣称为可靠有界，也不能断言回调查询就是连接失败根因。

## 6. 发现的现有后台异常

清理实验登录时，向当前生产后台发送一次带已记录 Session 4 指纹的 `logoff_session` 请求：

- 宿主连接 reset；后台原进程 PID 2484 消失。
- 原 Windows 启动任务 LastTaskResult 为 `3221225477`（`0xC0000005`）。
- 两个实验登录当时仍存在，不能当作注销已经成功。
- 此次没有找到可直接解释根因的应用错误事件或 dump。

**这是一次实测异常及时间关联，不是已完成根因定位。** 未再次向生产后台重复触发。后续注销接口应在隔离测试后台/临时测试登录中验证，不能因为旧代码已有指纹检查就把它列为实机可靠。

### 6.1 注销源码路径与可绕过部分（仅静态核对）

`internal/controller/switch.go` 的空会话切换和显式“注销并切换”最终共用 `internal/adapter/session.go` → 后台 `logoff_session`。后台在 `windows-agent/src/backend.rs` 完成请求/访问校验后，通过 `spawn_blocking` 调用 `windows-agent/src/session.rs:logoff`：

```text
拒绝 Session 0/空指纹
→ WTSQuerySessionInformationW 读取目标并核对用户/指纹
→ require_empty=true 时运行用户 UI probe，要求 windows=0 且 tray=0
→ 再读目标指纹
→ WTSLogoffSession(NULL, id, wait=1)
→ 枚举/读取用户登录，确认原指纹不再存在
→ 返回成功
```

**原始崩溃请求的 require_empty=false，已经跳过 UI probe。** 因此改成 false 或跳过空窗口检查不是针对这次异常的有效绕过证据。没有阶段日志/dump，尚不能区分崩溃发生在原生查询、注销调用还是注销后的确认，不能直接断言 WTSLogoffSession 本身有缺陷。普通 Rust Result/`spawn_blocking` 也不能作为进程级访问冲突的隔离边界。

实际部署 EXE 与本地 Windows 构建产物 SHA256 一致：`0ebf788351e7ea07837a99be1cdb53fc313e81969ce5c38be8c552ae8018e9bb`。

可区分两种绕过：

- 不需要注销的业务选择保留/断开，完全不调用该 RPC；但这不会释放保留登录内的应用内存。
- 需要真正注销时，先用独立探针核对目标登录，再通过 Windows 自带 `logoff <id>` 命令执行，绕过当前 Rust RPC 的整个执行路径。本轮 4、6 的清理已成功；这只是受控实验中的替代路径，不是已实现的生产修复，也没有证明原生系统内部完全不使用 WTS。`tsdiscon` 仅断开，不能充当注销。

用户已将崩溃根因核实另行安排；本 Agent 不继续生产复现、不禁用现有入口。

按原有 `RdpLauncherBackend` 任务恢复了后台，之后只对本轮创建、且 Session ID/LogonTime/用户仍匹配的 4、6 使用已有 logoff 命令清理；没有注销实验前的登录（实验前没有用户图形登录）。

恢复后另遇到后台本机 ping 成功、宿主 SYN 不通：原 `RdpLauncher-Host` 规则内容正确，但重新设置同一个 ApplicationFilter.Program 值后访问恢复。**没有添加规则、扩大端口/地址范围或关闭防火墙。** 此现象发生在前面 ReFS 脱机/上线之后，可能与程序路径规则重解析有关，尚未证明因果。原始 program 值已保存；它不是本次重构要新增的安全机制。

### 6.2 后续验证：SYSTEM / Session 0 执行系统 logoff.exe

按用户后续“试试”的授权，用独立临时 SYSTEM 任务执行 `C:\Windows\System32\logoff.exe <id>`，不调用旧注销 RPC，不改生产 Agent。

- SSH 基线：Session 0，配置用户身份，Administrator=true。
- 实验执行者：Session 0，`NT AUTHORITY\SYSTEM`，SID `S-1-5-18`，Administrator=true；保存了默认权限列表，没有额外启用权限。
- 只新建本轮自己的登录，调用前核对 ID、LogonTime、用户、域、SID 及 Active/Disconnected 状态，使用固定系统程序和独立数字参数。

| 目标 | 注销前 | logoff.exe | 注销后确认 |
|---|---|---|---|
| 新建 RD / Session 5 | Active，客户端标识匹配 | 退出码 0，682ms | 原登录身份消失，随后 Session 行也消失 |
| 新建 RemoteApp / Session 7 | 已断开，保留登录时间 | 退出码 0，437ms | 原登录身份消失，随后 Session 行也消失 |
| 已注销的旧目标 / Session 7 | 已不存在 | 校验拒绝，未启动命令 | 没有新的注销副作用 |

两次成功后，生产后台均仍为 PID 4724，最终原有 ping 返回 pong；没有后台崩溃。实验登录已结束、FreeRDP 已关闭、临时任务已删除、lock 恢复 false。

**结论：SYSTEM / Session 0 有能力通过系统 logoff.exe 注销指定的 Active 和 Disconnected 用户登录；无需用户会话 worker，也无需经 SSH 执行这一业务操作。** 据此将重构方案更新为 Agent 内部采用系统命令执行注销（见提案 §5.1），不再依赖旧进程内直接注销路径，也不要求先定位旧根因才能实现替代策略。尚未将其集成到 Agent，没有据此宣称旧 RPC 已修复；仍须验收调用前紧接着核对身份、超时结果未知处理和剩余期限内轮询确认。系统命令仅接受 Session ID，最后校验到执行之间的 ID 复用竞态并未由本实验消除；这是方案明确接受的边界。事后发现新的 SessionRef 时报告异常，不追加操作。

结果及固定探针：`/tmp/lsw-system-logoff-20261022/`（`system-active.json`、`system-disconnected.json`、`system-stale-target.json`、`system-logoff.ps1`）。来宾墙钟仍与宿主有偏差，耗时使用 Stopwatch，不以两端日志墙钟相减。

## 7. GUI 合并的无界面路径与构建

在 `/tmp` 中创建独立最小 Go 程序，实际链接当前 `github.com/egoist/mygo v0.5.5`，保留 GUI 调用分支但执行 noop 分支：

- 取消 WAYLAND_DISPLAY、DISPLAY、XDG_RUNTIME_DIR 后运行成功，输出 `HEADLESS_PATH_OK`。
- `/proc/self/maps` 未出现探针筛选的 Wayland/EGL/X11 库。
- 相同程序以 `GOOS=windows GOARCH=amd64 CGO_ENABLED=0` 交叉构建成功。
- 当前完整 `cmd/rdp-launcher-dialog` 同样 Windows 交叉构建成功。

说明当前依赖下“链接 GUI 但服务/CLI 无界面路径不启动 UI”具有可行性。不是对未来完整合并补丁的验收，也没有在 Windows 执行该 GUI。

另核对当前 `.github/workflows/ci.yml`：`make check-go`、`make check-gui` 在 Linux job；Windows job 是 Rust agent 的原生检查，不是 Windows 原生 Go 检查。跨平台构建仍应明确维护，但不要据现有 CI 声称 Windows Go 已在原生测试。

## 8. 收尾与未通过项

已完成：

- 所有本轮拥有的 FreeRDP 进程已结束。
- 实验登录 4、6 已清理；临时 SYSTEM 任务均删除。
- 原有后台恢复运行，宿主最终 `BackendReady=true`、`GuestKnown=true`，lock 恢复为实验前的 false。
- VM 实验中被唤醒，收尾恢复正常自动管理，而不是强行重新暂停。
- 未部署新 Agent、未改生产 TOML、未更换驱动、未重启 VM。保留探针和结果文件便于复核。

仍需后续明确验证：

1. 自持连接单纯断开时的 ConnectTime 不变性：覆盖 RD 手动关闭、RemoteApp 自动/手动断开，保存断开前绑定值、过渡态和 Disconnected 的原始快照及 SessionRef；确认当前“ConnectTime 改变即另一连接重连”的跳过条件不会误判。此项待新一轮实机验收，不因历史文字记录或代码审查通过而视为完成。
2. 换客户端标识时本机新登录失败的根因，及从旧版实际生效名称向新版默认（`rdp-launcher`）升级迁移的完整实体验收（标记为 pending）；升级前用户必须显式注销旧会话或在配置中指定旧生效主机名。当前真实业务 VM 保持关机状态，不进行意外的开机或测试。
3. 来宾冷启动后的 BootIdentifier 变化；不通过重启当前业务 VM 强行补测试。
4. SYSTEM 系统命令方案的 Agent 内集成验收，包括前后查询、异常退出、超时、断线与目标复用；旧 `logoff_session` 的 `0xC0000005` 根因另行核实，不再作为采用替代策略的前置必解项。
5. 任意文件关联、DDE、其他 UWP、不同用户 profile/映射环境。
6. 完整新 IPC、CLI 内 GUI/TTY/stderr 回退及 lock 并发准入的端到端验收；现有分支实现及单元测试通过，不等于新一轮 Windows/RDP 实机验收通过。
