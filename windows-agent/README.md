# Windows Agent（Rust）

从 `/code/rust/rdp-launcher/windows-agent` 迁入，复用现有应用扫描、图标提取、用户映射盘与启动能力。许可证见根目录 `LICENSE`。

## 两个角色

```text
remoteapp-launcher.exe backend --bind <guest-IP>:49153 --auth-token-file 'C:\ProgramData\rdp-launcher\agent-token' --log-dir C:\RdpLauncher\logs --session-user <Windows-user> --session-host <Linux-host-IP>
remoteapp-launcher.exe interactive --bind <guest-IP>:49152 --auth-token-file 'C:\ProgramData\rdp-launcher\agent-token' --log-dir C:\RdpLauncher\logs
```

- backend：独立后台，无窗口和 RDP 保活；负责 SSH、物理内存观测和状态订阅。
- interactive：在交互用户会话中运行，负责 RemoteApp 保活、run/open/list_apps/get_mapped_drives。
- daemon：interactive 的兼容别名，Go FreeRDP 适配器可以继续使用它，并在 RemoteApp 命令行中自动追加 `--auth-token-file`（值来自宿主 `[server].agent_token_file`）。
- 默认绑定 localhost。供宿主访问时显式指定 guest IP，并限制防火墙范围。

## 共享 token 鉴权

两个 TCP 端点对每一个动作校验同一个共享 token：ping、status、watch_status、quit、run、open、list_apps、get_mapped_drives、inspect_session、logoff_session 都不例外。token 文件在进程启动时读取一次；文件内容应是一行随机 token，可以有一个结尾 LF/CRLF；空格/制表符等非 CR/LF 空白会作为 token 的一部分保留，不会被裁剪。空文件、嵌入 CR/LF 或非 UTF-8 会拒绝启动。

- 非 loopback 监听必须提供 `--auth-token-file`，否则进程拒绝启动。没有 token 文件只允许显式 loopback 绑定，这是 **local-only 模式**，不应用于供宿主访问的部署。
- 一旦提供 token 文件，即使绑定 loopback 也强制校验，不存在按请求降级为无鉴权的回退。
- 请求 JSON 的顶层 `token` 字段是唯一凭证；缺失或不匹配返回 `authentication required`。连接建立后 `watch_status` 不再逐帧重复 token。
- 只有路径进入 argv，token 不进入 argv；日志记录动作和结果状态，不打印原始请求或 token。
- 明文 TCP 没有加密；token 只能降低受控网络中的未授权调用风险。仍需防火墙限制来源，不能把该端口暴露到不受控网络。
- token 文件 ACL 必须同时允许 **SYSTEM**（后台服务读取）和**配置的交互用户**（RemoteApp daemon 以登录用户身份读取），可另允许 Administrators；Administrators 是受信机器管理员例外（管理员本身可更改或绕过文件 ACL），不代表普通用户权限。不得允许 Users、Authenticated Users、Everyone 或其他本地用户读取。后台以 SYSTEM 运行不等于交互用户无需权限；`--session-user` 只指定会话管理目标，ACL 的交互用户由部署时显式配置。
- 使用 `install.ps1` 提供/检查该 ACL；它从隐藏提示或 `-TokenSource` 文件读取 token（不接受 token 字符串参数，也不回显 token），设置受保护 ACL 后验证 trustee 集合与读权限。`-VerifyOnly` 只验证已有文件。新增 trustee、继承权限或 DENY ACE 会被拒绝，而不是按“可读”警告后继续；DENY 规则不做有效权限推演。
- 限制：ACL 的验收点是该脚本；Rust agent 启动时只读取并校验 token 值，不重新解析 ACL。管理员或文件所有者仍可更改 ACL，因此 ACL 变更后应重跑 `-VerifyOnly`。
- 实机验证：已在 Win11 使用临时 fixture，分别以指定用户账户及临时 SYSTEM 任务读取 token，并比对内容；正确 ACL 通过，宽泛 Users 授权和 DENY ACE 均被拒绝。测试没有替换现有 agent、生产 token 或服务。可在提升权限的 Windows PowerShell 中运行 `./tests/token-acl.ps1 -RunAsAdministrator -InteractiveUser <当前用户>` 复现；指定账户必须与调用者一致。部署到不同账户／路径后仍需 `install.ps1 -VerifyOnly`。
- 宿主端的同一 token 存于 Keyring，服务名为 `<server.service>-agent`，用户名为 `agent`，与 RDP 密码条目分开。
- 宿主通过 `rdp-launcher auth agent set/check/clear` 管理 Keyring，不回显 token。后台服务/手动启动需要自行带 `--auth-token-file`；RemoteApp 交互 daemon 的文件路径由宿主配置传入。

## 本地 CLI

```powershell
remoteapp-launcher.exe status --bind 192.0.2.14:49153
remoteapp-launcher.exe ping --bind 192.0.2.14:49153
remoteapp-launcher.exe apps --bind 192.0.2.14:49152
remoteapp-launcher.exe mounts --bind 192.0.2.14:49152
remoteapp-launcher.exe run notepad.exe --bind 192.0.2.14:49152
remoteapp-launcher.exe open 'C:\RdpLauncher\probe document.txt' --bind 192.0.2.14:49152
remoteapp-launcher.exe quit --bind 192.0.2.14:49153
```

未指定地址时，客户端连接 localhost:49152。后台仅支持 ping/status/quit；应用与映射盘命令连接交互端。失败打印原因并返回非零退出码。`list-apps` 和 `check-ssh` 在当前进程直接查询；`--help` 显示入口。向已启用鉴权的 daemon 发送本地 CLI 请求时，同样要加 `--auth-token-file PATH`；token 从文件读取后写入请求，不会打印。

## 后台协议

一次请求一行 JSON。支持 ping、status、watch_status、quit。配置 token 后每个请求都带顶层 `token`；下列示例用 `"<token>"` 表示该共享值。

`watch_status` 保持同一 TCP 连接：

```json
{"action":"watch_status","token":"<token>"}
{"status":"ok","role":"background","kind":"snapshot","ssh_active":false,"memory":{"total_bytes":4294967296,"available_bytes":1073741824},"source":"etw"}
{"status":"ok","role":"background","kind":"changed","ssh_active":true,"memory":{"total_bytes":4294967296,"available_bytes":1073741824},"source":"etw"}
```

初始化后只发状态变化，没有应用层心跳。`ssh_active: null` 表示 Unknown。没有业务变化时，连接保持安静。

SSH 表示本地 22 端口的已建立 TCP 连接是否为零，不表示认证后的登录会话数。ETW accept/disconnect 事件触发 TCP 表检查，只有零/非零/Unknown 边界向宿主发布。首次与 60 秒兜底检查保留。

ETW 使用 `ferrisetw`，需要相应权限；权限不足时记录错误并退回低频检查。每个进程拥有独立的 ETW session，正常 quit 会释放它。

内存监视器每秒采样 `GlobalMemoryStatusEx`，只有占用率整数边界、总量或可用性变化才推送；`memory: null` 表示 Unknown。不再订阅 Low/High 事件或报告 low。95%/80% 只属于宿主策略，Agent 不知道阈值，也不调整容量。详见 [内存说明](../docs/memory.md)。

## 图形会话观察与注销

后台默认不开放会话管理。启用时必须同时指定 `--session-user` 和 `--session-host`，以 SYSTEM 身份运行。只有指定来源 IP 可查询／注销指定用户的图形会话；仍应保留宿主机限定的防火墙规则，不能把它当成公网认证协议。

`inspect_session` 优先选择唯一活跃用户会话，否则选择唯一保留会话。多个候选目标返回未知，不猜测。SYSTEM 进程用 WTS 用户 token 启动同一二进制的一次性 `session-probe`，读取该会话的顶层窗口及支持的 Shell 托盘枚举；没有托盘枚举能力返回 null。辅助进程不创建窗口，输出目录仅 SYSTEM 和该用户可读写，超时终止并清理。后台自身不创建用户 GUI。

`logoff_session` 必须携带用户、非零 session ID 和登录指纹；拒绝复用 ID、变化的登录、错误来源和错误用户。自动注销还必须再次确认该会话的窗口与托盘均为零。注销使用 WTS API 等待完成并确认旧登录消失，不按进程名批量结束程序，不注销服务会话 0。交互端 `status` 返回自身会话 ID 和激活状态，防止保留会话中的旧 agent 被误认为新连接就绪。

## 构建与安装

在有工具链的 Windows 环境中执行：

```powershell
cargo build --release --locked
# 交互用户必须显式给出；默认 $env:USERNAME 在管理员进程中可能不是目标用户。
.\install.ps1 -InteractiveUser '<Windows-user>'
# 已有 token 文件时只重新校验 ACL，不重建/覆盖：
.\install.ps1 -InteractiveUser '<Windows-user>' -VerifyOnly
# 从安全文件导入（文件本身仍是秘密，不要放在共享目录）：
.\install.ps1 -InteractiveUser '<Windows-user>' -TokenSource 'E:\secure\agent-token.txt'
```

也可在 Linux 使用现有 MinGW 工具链交叉构建（本轮采用此方式）：

```sh
CARGO_TARGET_X86_64_PC_WINDOWS_GNU_LINKER=x86_64-w64-mingw32-gcc \
  cargo build --manifest-path windows-agent/Cargo.toml \
  --target x86_64-pc-windows-gnu --release --locked
```

`install.ps1` 默认复制到 `C:\RdpLauncher\bin`，设置专用目录/文件 ACL 需要管理员权限。可传 `-SourceExe` 安装已有构建。它不强制结束现有程序、不修改 PATH、注册表、服务或启动任务。token 目录必须是专用目录（默认 `C:\ProgramData\rdp-launcher`），脚本会拒绝把磁盘根目录或 ProgramData 等共享目录改成专用 ACL，也会拒绝包含其他文件的目录。token 文件应只包含一行 token；允许一个结尾 LF/CRLF，拒绝 BOM、空文件、嵌入 CR/LF 和非 UTF-8。仅做本机 loopback 测试时可传 `-SkipTokenProvision`，但这不适用于宿主访问。

测试通过 SSH 直接执行 PowerShell 7 命令，不嵌套 `powershell -NoProfile`。本轮没有安装额外 Windows 工具，文件均放在 `C:\RdpLauncher`。

## 验证范围

见 [Windows 验证记录](../docs/windows-validation.md)。Windows 原生与真实流程验证覆盖退出及 SSH 事件边界；IPv4/IPv6 ETW、后台/交互分离与实际 RemoteApp 文档窗口均已实测。未以此宣称完整 Go 宿主迁移或最终自启动部署已经完成。
