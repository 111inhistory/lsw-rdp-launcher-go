# Windows 断开时 ConnectTime 实测

日期：2026-10-23。代码基线：`f882b3d`（业务实现 `6e3493a`）。测试对象为本机 `win11`，不是对全部 Windows/RDS 策略的保证。

## 结论

**本机以下三种断开都没有改写 ConnectTime；重新连接会改写它，但不改变原登录的 SessionRef。** 因此本轮没有发现空闲注销把正常断开误判为外部重连的问题，无需修改该判断。

| 场景 | Session / AuthenticationId | Active 的 ConnectTime（原始 FILETIME） | Disconnected 的 ConnectTime | 结果 |
|---|---|---|---|---|
| RD 手动关闭窗口 | 2 / `000000000008D432` | `134372011363270527` | `134372011363270527` | 不变 |
| RemoteApp 手动结束自持客户端 | 4 / `00000000001FF025` | `134372016650402602` | `134372016650402602` | 不变 |
| 新控制器空闲自动断开 RemoteApp | 4 / `00000000001FF025` | `134372016785272070` | `134372016785272070` | 不变 |

正向对照：RD 同登录重连后 ConnectTime 从 `134372011363270527` 变为 `134372011771837977`，随后再次手动断开仍保持新值。RemoteApp 重连也由表中手动场景的值变为自动场景的值。两种重连的 SID、AuthenticationId、BootIdentifier、LogonTime 均保持原登录身份。

## 方法与证据

- 运行只读 x64 SYSTEM 探针，约每 100ms 采样一次；同时读取 WTS 状态、WTSINFOEX 原始字节、SID、token AuthenticationId 和内核 BootIdentifier。FILETIME 存成十进制字符串，不经 JavaScript 浮点数转换。
- 全程得到 9003 帧完整快照，6306 条带原始 INFOEX 的登录行均通过独立字节解码校验；无采样错误。四段断开（含 RD 重连后再次关闭）的连续区间分别有 64、37、89、124 帧，不仅比较首尾值。每段均实际观察到 1～2 帧 ClientName 已清空、状态仍为 Active，随后转为 Disconnected；这支持当前实现对同连接过渡态的有界等待，而非一次永久跳过。
- 独立解码 WTSINFOEX 原始缓冲区：Level 在偏移 0，Data 在 8，LogonTime / ConnectTime / DisconnectTime 分别在外层偏移 168 / 176 / 184。结构尺寸与偏移先断言；每帧保留 base64 原字节供复核。
- RD 使用本轮直接启动的 FreeRDP，按其准确 PID 找到 Niri 窗口再发送关闭操作。RemoteApp 手动场景向本轮自持 PID 发送 SIGTERM，不执行注销；自动场景没有发送手动终止信号，而由新控制器的真实 `SourceIdle / StopDisplay` 执行。
- 自动场景的隔离测试策略为：显示空闲 12 秒、VM 挂起禁用、空闲注销禁用、内存控制禁用。控制器日志在 `12:01:32` 记录 `kind=stop-display source=idle session=4`；随后独立探针确认 Disconnected。不是用脚本定时杀进程冒充自动断开。
- 只读探针源码：[windows-agent/tests/connecttime-probe.ps1](../windows-agent/tests/connecttime-probe.ps1)，要求 x64 SYSTEM，采样最长 1200 秒，支持停止标记文件，不提供命令执行接口。
- 已提交的原始证据子集和逐场景统计：[fixtures/connecttime-20261023.json](fixtures/connecttime-20261023.json)。完整原始数据保存在本机 `.pi/live-connecttime-20261023/samples.jsonl` 和来宾 `C:\ProgramData\lsw-connecttime-20261023\samples.jsonl`，不再只依赖 `/tmp`。完整文件 SHA256：`aa11aa02097e8d5911e771e52ce9cb495af2ff2cb46c69488f9eb6bea5e8adec`。
- 来宾时钟只用于描述，顺序与间隔依赖探针 Stopwatch；不将两端墙钟直接相减。

## 隔离环境中的附加验证

没有替换生产 Agent、注册表 RemoteApp 项或服务配置，没有改变防火墙/ACL。

新 Windows Agent 从临时目录运行，监听来宾 loopback `49253`，宿主经临时 SSH 转发访问；临时 SYSTEM 任务仅运行本轮探针及固定 Agent。测试 Agent 的 `--session-host` 因此使用实际 TCP 对端 `127.0.0.1`，不是 RDP 客户端地址。原生产后台始终是 PID 5568，未被替换。

- 新 Agent 的完整快照、用户 worker 就绪、RemoteApp 绑定与同登录重连成功。
- RemoteApp 和 RD 桌面内，`list_apps` / `get_mapped_drives` 使用 `UserContext=true, Mode=""`，在同一绑定引用派发，没有确认或断开操作。
- 桌面绑定 Session 3 / `000000000027E193` 中，Win32 业务启动成功。实际子进程报告用户 SID（非 SYSTEM）、Session 3、`USERPROFILE=C:\Users\rdp-user`、cwd 为明确传入的用户目录。
- 经**新 Agent 的 logoff_session 接口**，SYSTEM `logoff.exe` 分别确认结束本轮的 Session 3 和 4。删除 3 后，4 仍保留；随后仅按 4 的已核对引用清理它。两次返回 `session logoff confirmed`，后续新鲜查询均确认原引用消失，后台没有崩溃。
- 曾试图在 SSH 隧道保持连接时观察零延迟自动注销：控制器正确报告 `SSH=nonzero`，不启动注销计时。该尝试不是代码故障，也**不能记为空闲自动注销通过**；上述注销成功属于显式、精确目标的 Agent 接口集成验证。
- 实测额外发现 IPv4 ClientAddress 读取偏移错误（显示为 `0.0.192.168`）。已单独修正 AF_INET 使用 WTS Address 的第 2～5 字节，补真实原始布局单测；IPv6 未实测，不据此推断其布局。采集时测试 Agent SHA256 为 `989155e492975a9ec7d850434d20db1f5d678a0c01536663c5a81332d720b65a`，早于此地址显示修正；修正不涉及本轮验证的连接身份/时间判断。

## 现场问题、收尾与未覆盖项

原驻留服务启动时未继承 `WAYLAND_DISPLAY` / `NIRI_SOCKET`。直接让它启动 RemoteApp 会在客户端启动阶段失败；这不是 WTS 断开样本。本轮没有重启生产服务，使用当前图形环境中的隔离控制器完成验证。该环境问题仍需在实际部署时处理，原服务可能仍显示这一次最近请求错误。

测试前 VM 关闭且没有图形用户登录。本轮临时开机，清理的仅为自己创建并校验过身份的登录。全部临时任务、隧道、测试后台及 FreeRDP 已结束，原控制器 lock 恢复未锁定，VM 通过正常 shutdown 恢复关闭。探针与数据文件保留供审查，没有强制断电或注销测试前的登录。

本轮开机后的 BootIdentifier 与先前记录相同；没有证明这是新的 Windows 内核冷启动，也没有据此完成冷启动 / Fast Startup / 休眠身份验收。其余失败注入、真实 ID 复用、原旧 RPC 崩溃根因、完整旧 hostname 迁移及 GUI 端到端验收仍不在本结论之内。
