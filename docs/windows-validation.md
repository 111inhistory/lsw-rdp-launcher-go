# Windows Agent 迁移验证记录

此记录对应内存功能前的历史迁移轮次，其中双端口与 interactive 命令属于旧协议，不能作为当前 protocol 2 / 用户 worker 的验收结论或操作说明。当前会话重构方案与新增实验见 [会话生命周期方案](session-lifecycle-redesign.md) 和 [实机实验记录](session-lifecycle-experiments.md)；新 Agent 的完整 Windows 集成验收仍待完成。后续内存变更记录见 [内存验证记录](memory-validation.md)。

日期：2026-10-19。目标：本机 libvirt 的 `win11`，guest 地址 `192.0.2.14`。

## 环境与操作边界

- Windows 端通过 `ssh win11 '…'` 直接执行命令，使用已有 PowerShell 7.6.6。
- Linux 使用已有 MinGW 工具链交叉构建 GNU Windows exe；测试 exe 在真实 win11 原生运行，不以 Wine 结果代替。
- Windows 侧无需安装新工具。二进制、源码、日志与生成文件均放在 `C:\RdpLauncher`。
- 旧 Rust 仓库、旧 Windows 安装、用户配置、Keyring、PATH 和 RemoteApp 注册表保持不变。
- 测试使用独立端口：后台 49253，交互端 49252。
- backend 通过临时 SYSTEM 计划任务运行，位于 Session 0；interactive 由本次 FreeRDP RemoteApp 启动，实测位于 Session 2。会话号是本次结果，不作为运行保证。

## 结果

| 项目 | 观察结果 |
|---|---|
| Rust 单元测试 | 9 项在 Windows 原生通过，覆盖事件过滤、端口字节序、边界判断、重复状态抑制与停止 |
| 后台独立运行 | 没有 FreeRDP 时，ping/status/watch_status 均可用，role 为 background |
| ETW 启动 | SYSTEM 后台日志显示 source=etw |
| IPv4 SSH 边界 | 建立实际 SSH 连接后约 0.996 秒收到 true；断开后约 1.008 秒收到 false，早于 60 秒兜底 |
| 非零内部变化 | 两条 TCP/22 连接的 1 → 2 → 1 不产生 changed 通知；最后一条关闭后通知 false |
| IPv6 SSH 边界 | 临时任务建立并关闭 `::1:22` 连接，观察到 false → true → false，均在低频兜底前到达 |
| 无应用心跳 | 初始化快照后连接静默 32 秒，随后仍在同一 Go Watch 连接上收到新的 SSH-active 事件 |
| Go 仅后台路径 | 真实 go-libvirt 订阅/查询加新后台，完成 status 请求，FreeRDP=false；移除心跳耦合后本次运行约 0.10 秒完成，不作延迟保证 |
| VM 状态驱动订阅 | Go 测试中暂停关闭订阅，恢复建立新订阅取快照，不等待心跳 |
| 交互端识别 | ping/status 的 role 为 interactive，与后台分开 |
| 应用扫描 | 无图标扫描返回 78 个应用 |
| 用户映射盘 | 返回 B/F/J/M/Y 五个映射盘；没有将用户盘符查询搬入 SYSTEM 后台 |
| 启动成功 | 经交互端运行 cmd，在带空格的测试路径写入预期标记 |
| 启动失败 | 不存在的 exe 返回 ShellExecuteExW / Win32 error 2 |
| 后台职责限制 | 后台拒绝 run 请求，未生成对应副作用文件 |
| 文件打开与实际显示 | open 生成的 `probe document.txt` 成功，Niri 观察到 `probe document.txt - Notepad`；随后仅关闭此测试窗口 |
| 交互端退出 | quit 交互端后，本次 FreeRDP 自行退出，后台仍可独立服务 |
| ETW 收尾 | backend quit 后，所属 ETW session 被释放 |

长连接无应用层心跳、无固定读取超时。TCP keepalive 负责连接级故障检测；它不等价于证明每个业务线程健康。ETW 权限不足时的轮询降级有代码路径，但本轮没有以低权限账户实测。

## 文件与清理

保留以供继续开发：

- `C:\RdpLauncher\bin\remoteapp-launcher.exe`
- `C:\RdpLauncher\bin\agent-tests.exe`
- `C:\RdpLauncher\source\`：本轮 Rust 源码、锁文件、安装脚本、许可证和测试 fixture。
- `C:\RdpLauncher\logs\` 与生成的测试文本。

最终 Windows 二进制与仓库 release 构建的 SHA-256 均为：

```text
8c94551233151cf587c06c0fa3fdfc4c30ce84f299359f839cd06ec3dbfea3a8
```

清理结果：

- 本次交互端、后台和 FreeRDP 均已退出；没有强制结束其他应用。
- 两个临时计划任务和仅允许宿主访问测试端口的临时防火墙规则已删除。
- 本次创建的 ETW session 已释放。
- win11 测试前关闭，为测试启动后保留 Running，供后续开发。
- 启动钩子拉起的旧生命周期 watcher 在测试中暂停，结束时恢复到测试前的 inactive/disabled 状态；未修改其配置，未留下暂停进程。
- 未关闭 VM、修改内存目标、执行旧全生命周期破坏性测试或覆盖旧 Agent 安装。

## 尚未覆盖

- 完整 Go 宿主 CLI、TOML、地址发现、桌面导出与路径模块尚未全部迁移。
- 完整 Go 控制器的真实冷启动、自动暂停与恢复未验收；状态确认顺序和 VM 恢复重订阅已通过模拟测试。
- 应用图标提取沿用旧代码，未在本轮单独做真实图标质量验收。
- Windows Service 或用户常驻进程的最终部署、自启动与权限策略尚未确定。临时 SYSTEM 计划任务只用于验证后台与交互职责独立。
- 意外网络故障后的自动重连仍未实现；不通过定时重试掩盖故障。
