# Windows Agent（Rust）

从 `/code/rust/rdp-launcher/windows-agent` 迁入，复用现有应用扫描、图标提取、用户映射盘与启动能力。许可证见根目录 `LICENSE`。

本版按 `docs/session-lifecycle-redesign.md` 改为 **protocol 2**：宿主只连接一个后台入口，用户动作按 `session_ref` 路由到按需创建的用户 worker。旧的全局交互 TCP 端点、`inspect_session` 单会话猜测和登录指纹路由已删除，不提供新旧协议并行兼容。

## 角色

```text
remoteapp-launcher.exe backend --bind <guest-IP>:49153 \
  --auth-token-file 'C:\ProgramData\rdp-launcher\agent-token' \
  --log-dir C:\RdpLauncher\logs \
  --session-user <Windows-user> --session-host <Linux-host-IP>

remoteapp-launcher.exe remoteapp-shell --log-dir C:\RdpLauncher\logs
```

- **backend**：唯一的宿主 TCP 入口，无用户 GUI；负责 SSH、物理内存观测、WTS 会话快照、SYSTEM 注销以及把用户动作路由到用户 worker。
- **remoteapp-shell**：仅在 RemoteApp 路径需要，在登录用户会话内维持 RDP 执行状态和托盘保活。**没有 TCP 监听、没有请求端点、不接收 token。**
- **user-worker**：内部角色，由 backend 按需以 WTS 用户 token 启动，每个请求一个短生命周期进程，经本机命名管道收发一次 JSON，然后退出。它不接受宿主直接连接，不应手动启动。
- 没有 `interactive`/`daemon` 兼容别名。

## protocol 2

每个 host→backend 请求都必须带 `"protocol":2`。缺失或版本不同会立即返回：

```json
{"status":"error","protocol":2,"role":"background","message":"protocol mismatch: Windows agent and host must be upgraded together (agent protocol 2)"}
```

所有 backend 响应都带 `protocol:2` 和 `role:"background"`；`session_status` 是例外，它证明用户 worker 的就绪状态，返回 `role:"user"`。请求业务字段使用 snake_case。

`session_ref` 是登录实例身份，不是 Session ID 的别名：

```json
{
  "session_id": 4,
  "authentication_id": "0000000003C517EA",
  "boot_identifier": "11111111-2222-4333-8444-555555555555"
}
```

- `authentication_id` 是 token AuthenticationId LUID 的 16 位大写十六进制。
- `boot_identifier` 是 `SystemBootEnvironmentInformation` 的规范 GUID。
- 账号归属另由 SID 校验。backend 启动时把 `--session-user` 解析为 SID，后续只接受该 SID 的登录；不按显示名猜测。

## 共享 token 鉴权

backend 对每个 host 请求校验共享 token：`ping`、`status`、`watch_status`、`quit`、`inspect_sessions`、`logoff_session`、`session_status`、`run`、`open`、`list_apps`、`get_mapped_drives` 都不例外。

- token 文件在 backend 启动时读取一次；内容是一行 token，可有一个结尾 LF/CRLF；空文件、嵌入 CR/LF、BOM 或其他格式错误会拒绝启动。
- 非 loopback 监听必须提供 `--auth-token-file`；没有 token 只允许显式 loopback 绑定（local-only 模式）。
- 请求 JSON 的顶层 `token` 字段是唯一凭证；缺失或不匹配返回 `authentication required`。`watch_status` 只在建立连接时校验一次。
- token 不进入 argv 或日志；日志只记录动作名和结果。
- 明文 TCP 不加密。token 只降低受控网络中的未授权调用风险，仍需防火墙限制来源。
- user worker 不读取共享 token，也不开放网络端点；backend 通过只允许 SYSTEM 和配置账号的命名管道（`PIPE_REJECT_REMOTE_CLIENTS`）与它通信。
- `install.ps1` 的现有 ACL 契约保持不变：SYSTEM 和配置交互用户可读，Administrators 可例外。当前只有 backend 读取 token；保留该 ACL 是为了兼容已有部署和安装脚本，不是新增权限。

## 后台协议

一次请求一行 JSON，响应一行 JSON。请求示例：

```json
{"action":"ping","protocol":2,"token":"<token>"}
```

`status` 返回 SSH、内存和最近快照；`watch_status` 在同一连接上先发一帧 `kind:"snapshot"`，之后只在 SSH、内存或会话快照变化时发 `kind:"changed"`：

```json
{
  "status":"ok","protocol":2,"role":"background","kind":"snapshot",
  "ssh_active":false,
  "memory":{"total_bytes":4294967296,"available_bytes":1073741824},
  "source":"etw",
  "snapshot":{"revision":3,"fresh":true,"sessions":[]}
}
```

### inspect_sessions

```json
{"action":"inspect_sessions","protocol":2,"token":"<token>","user":"<Windows-user>"}
```

`user` 可省略（backend 已绑定配置账号）；若提供必须与配置用户一致。响应：

```json
{
  "status":"ok","protocol":2,"role":"background",
  "snapshot":{
    "revision":7,
    "fresh":true,
    "sessions":[{
      "reference":{"session_id":4,"authentication_id":"0000000003C517EA","boot_identifier":"11111111-2222-4333-8444-555555555555"},
      "sid":"S-1-5-21-...","user":"user","domain":"MACHINE",
      "state":0,"client_name":"lswD2210a001","client_address":"192.0.2.10",
      "logon_time":638000000000000000,"connect_time":638000000000000000,
      "windows":1,"tray_icons":2
    }]
  }
}
```

- `state` 使用 WTS 原值：`0` Active，`4` Disconnected；其他状态也可能出现。
- 只列出配置 SID 的用户登录，排除 Session 0。允许同一用户保留多个登录。
- WTS 事件回调只触发刷新，不在回调内查询。刷新串行、有超时；超时或查询失败会把旧快照标为 `fresh:false`。`fresh:false` 只供展示，不得用于派发、切换或注销。
- `windows`/`tray_icons` 是可选活动观测，由短生命周期用户 worker 尽力采样；失败时为 null，不影响 `fresh`。它们不再是注销授权依据。
- 身份字段不完整（例如暂取不到 AuthenticationId）时仍可用于展示，但 `fresh:false` 且 `note` 说明不可派发。

### 用户动作

`run`、`open`、`list_apps`、`get_mapped_drives` 必须带完整 `session_ref`：

```json
{"action":"run","protocol":2,"token":"<token>","session_ref":{...},"target":"notepad.exe","params":null,"working_dir":null}
```

backend 在派发前重新核对启动标识、Session ID、配置 SID 和 AuthenticationId；任何不一致都拒绝。通过后按需启动本机 user worker，使用 WTS token、用户环境、用户配置文件和 `winsta0\default` 桌面执行原有 ShellExecute／应用目录／映射盘逻辑；不会回退到 SYSTEM 桌面执行。进程工作目录为用户 profile，不继承 SYSTEM cwd。

### session_status

`session_status` 用于证明目标登录的 worker 已就绪：

```json
{"action":"session_status","protocol":2,"token":"<token>","session_ref":{...}}
```

成功响应：

```json
{
  "status":"ok","protocol":2,"role":"user",
  "session_ref":{...},
  "session_active":true,
  "activity":{"reference":{...},"sid":"S-1-5-21-...","user":"MACHINE\\user","session_id":4,"windows":1,"tray_icons":2}
}
```

worker 在执行任何业务 action 前，用自身的 `ProcessIdToSessionId`、token AuthenticationId、boot identifier 和 SID 核对 backend 随请求下发的预期 SessionRef；不匹配则拒绝执行。`session_status` 返回 worker 实际 SessionRef，backend 与请求的完整 SessionRef 对比，而不只是比较 session/SID 和回显请求值。UI 观察是非权威数据，可以不带预期 SessionRef。Disconnected 登录会返回 `session_active:false`，但仍可证明 worker 位于该登录。

### logoff_session

```json
{"action":"logoff_session","protocol":2,"token":"<token>","user":"<Windows-user>","session_ref":{...}}
```

- 执行前紧接着重新核对完整 SessionRef 和配置 SID；不匹配则不启动命令。若这次原生查询返回时操作期限已过，也返回 `not_started`，不会在过期后启动命令。
- 只运行固定路径 `%SystemRoot%\System32\logoff.exe`，Session ID 作为独立十进制参数，不经过 shell，不扩展为通用命令执行。
- 子进程继承 Agent 的 SYSTEM / Session 0 上下文；命令等待上限 30 秒，并受同一操作剩余期限约束。
- 退出码 0 不等于成功：在剩余期限内轮询，确认原 SessionRef 消失后才返回成功。注销异步结束时也等待确认。
- 非零退出、超时、查询失败或断线返回 `status:"error"`，并带 `outcome`：`not_started`、`failed`、`unknown` 或 `anomaly`。
- 若命令启动后发现同一 Session ID 已对应新的 SessionRef，返回 `outcome:"anomaly"`，不追加任何操作。最后一个校验与命令执行之间的 ID 复用窗口按设计接受，无法完全消除。
- 不复用旧 `WTSLogoffSession` 进程内路径，不自动重试，不把杀掉子进程宣称为撤销已提交的注销。原生 WTS 确认查询无法取消；若查询本身阻塞超过期限，函数可能晚于期限返回，但仍只会在实际观察到原登录消失时报告成功，否则报告未知。

## 本地 CLI

```powershell
remoteapp-launcher.exe status --bind 192.0.2.14:49153 --auth-token-file PATH
remoteapp-launcher.exe ping --bind 192.0.2.14:49153 --auth-token-file PATH
remoteapp-launcher.exe inspect-sessions --bind 192.0.2.14:49153 --auth-token-file PATH
remoteapp-launcher.exe quit --bind 192.0.2.14:49153 --auth-token-file PATH
remoteapp-launcher.exe list-apps [--with-icons]
remoteapp-launcher.exe check-ssh
```

本地 CLI 只连接 backend，默认端口 `49153`，并同样发送 `protocol:2`。用户动作由宿主控制器发起；agent CLI 不再提供直连交互端的 `apps/mounts/run/open`。`list-apps` 和 `check-ssh` 在当前进程直接查询。向启用鉴权的 backend 发请求时需带 `--auth-token-file PATH`，token 从文件读取后写入请求，不打印。

## SSH 与内存

SSH 观测语义不变：只报告本地 22 端口已建立 TCP 连接是否为零，不表示认证后的登录数；`ssh_active:null` 为 Unknown。ETW accept/disconnect 只触发 TCP 表检查，首次与 60 秒兜底检查保留；权限不足时退回低频检查。

内存监视器每秒采样 `GlobalMemoryStatusEx`，只有占用率整数边界、总量或可用性变化才推送；`memory:null` 为 Unknown。95%/80% 属于宿主策略，Agent 不知道阈值，也不调整容量。详见 [内存说明](../docs/memory.md)。

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

也可在 Linux 使用现有 MinGW 工具链交叉构建：

```sh
CARGO_TARGET_X86_64_PC_WINDOWS_GNU_LINKER=x86_64-w64-mingw32-gcc \
  cargo build --manifest-path windows-agent/Cargo.toml \
  --target x86_64-pc-windows-gnu --release --locked
```

本地静态检查：

```sh
cargo fmt --manifest-path windows-agent/Cargo.toml --check
CARGO_TARGET_X86_64_PC_WINDOWS_GNU_LINKER=x86_64-w64-mingw32-gcc \
  cargo clippy --manifest-path windows-agent/Cargo.toml --target x86_64-pc-windows-gnu --all-targets
CARGO_TARGET_X86_64_PC_WINDOWS_GNU_LINKER=x86_64-w64-mingw32-gcc \
  cargo test --manifest-path windows-agent/Cargo.toml --target x86_64-pc-windows-gnu --no-run
```

`install.ps1` 默认复制到 `C:\RdpLauncher\bin`，设置专用目录/文件 ACL 需要管理员权限。可传 `-SourceExe` 安装已有构建。它不强制结束现有程序、不修改 PATH、注册表、服务或启动任务。token 目录必须是专用目录（默认 `C:\ProgramData\rdp-launcher`），脚本会拒绝把磁盘根目录或 ProgramData 等共享目录改成专用 ACL，也会拒绝包含其他文件的目录。

## 验证范围

历史记录见 [Windows 验证记录](../docs/windows-validation.md)，新实现验证见 [2026-10-23 隔离实测](../docs/connecttime-validation.md)。协议 2 快照、本机管道与用户 worker 就绪、RD/RemoteApp 绑定、用户桌面查询与 Win32 执行、SYSTEM `logoff.exe` 精确目标注销确认已在真实 Windows 隔离环境执行，**未替换生产安装**。仍需验收 SessionRef 失效与 ID 复用、注销超时/非零退出/断线/异步确认、WTS 查询超时后的 stale 行为、无 SSH 阻挡的空闲自动注销及完整 CLI/GUI 流程。旧 `WTSLogoffSession` 的 `0xC0000005` 根因仍独立核实，本实现不依赖该路径。
