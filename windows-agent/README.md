# Windows Agent

Rust Agent 为 Linux 控制器提供状态观测和用户会话执行能力。宿主只连接一个后台端点，默认端口为 `49153`。当前协议为 2，宿主与 Agent 应配套更新。

## 进程角色

| 角色 | 运行位置与职责 |
|---|---|
| `backend` | SYSTEM / Session 0。观测 SSH、内存和 WTS；路由用户请求；注销指定登录。 |
| `remoteapp-shell` | 登录用户会话。维持 RemoteApp 连接和托盘保活，无 TCP 监听，不读取 token。 |
| `user-worker` | 后台按需启动的内部角色。使用目标登录的用户 token，经本机管道执行一次请求后退出。 |

不要手动启动 worker，也不要把用户应用直接放到 SYSTEM 中执行。旧 `interactive` / `daemon` 入口不再提供。

## 构建与安装

在 Windows 上构建：

```powershell
cargo build --release --locked
```

以管理员身份安装，并明确指定交互用户：

```powershell
.\install.ps1 -InstallDir 'C:\Program Files\RdpLauncher' `
  -InteractiveUser 'EXAMPLE\user'
```

脚本复制可执行文件并设置 token 文件权限，不注册服务、启动任务、PATH 或 RemoteApp 项。宿主 `[remoteapp].default_app` 应指向实际安装的同一版程序。

使用 SYSTEM 服务或启动任务运行后台。以下示例地址应替换为实际 IP：

```powershell
& 'C:\Program Files\RdpLauncher\remoteapp-launcher.exe' backend `
  --bind 192.0.2.10:49153 `
  --auth-token-file 'C:\ProgramData\rdp-launcher\agent-token' `
  --session-user 'EXAMPLE\user' --session-host 192.0.2.1
```

`--session-user` 启动时解析为 SID。`--session-host` 限制管理请求的 TCP 对端地址，不是 RDP 客户端 hostname。SSH 转发会改变后台看到的 TCP 对端，配置时应使用实际对端。

RemoteApp 由宿主以 `remoteapp-shell` 参数启动，不需要独立网络端口。

Linux 交叉构建需已有 MinGW 工具链：

```sh
CARGO_TARGET_X86_64_PC_WINDOWS_GNU_LINKER=x86_64-w64-mingw32-gcc \
  cargo build --manifest-path windows-agent/Cargo.toml \
  --target x86_64-pc-windows-gnu --release --locked
```

## Token 与网络

**警告：不要向不可信网络开放后台端口。** TCP 没有传输加密；token 不是公网安全方案。

- 非 loopback 监听要求 `--auth-token-file`。只有显式 loopback 绑定可在未提供文件时使用 local-only 模式。
- 后台启动时读取一次 token。文件可带一个结尾 LF 或 CRLF；空 token、嵌入换行、BOM 等非法内容会被拒绝。空格和制表符不被裁剪。
- 每个请求使用顶层 `token` 字段。`watch_status` 在建立连接时校验一次。token 不进入 argv 或日志。
- token 文件只允许 SYSTEM、配置的交互用户及可选受信 Administrators 访问。不要授予 Users、Everyone 或 Authenticated Users。
- worker 不读取 token。后台与 worker 使用只允许 SYSTEM 和配置账号访问的本机命名管道，不接受远程管道连接。

已有文件可只验证权限：

```powershell
.\install.ps1 -InteractiveUser 'EXAMPLE\user' -VerifyOnly
```

也可用 `-TokenSource` 从安全文件导入，或用 `-SourceExe` 指定构建产物。token 目录应为专用目录。权限验证由安装脚本负责，后台不会重新解析 ACL。

## 协议

请求和响应均为一行 JSON。每个请求应带 `protocol:2`。版本缺失或不符时，后台返回明确的配套升级错误，不回退旧协议。

```json
{"action":"ping","protocol":2,"token":"<shared-token>"}
```

响应带 `protocol:2` 和 `role:"background"`。`session_status` 的成功响应使用 `role:"user"`，用于证明目标 worker 的身份。

| 动作 | 额外字段 | 含义 |
|---|---|---|
| `ping`、`status` | 无 | 健康响应或当前观测。 |
| `watch_status` | 无 | 首次快照及后续变化。 |
| `inspect_sessions` | 可选 `user` | 配置账号的完整登录快照。 |
| `session_status` | `session_ref` | 验证目标 worker 身份和活动状态。 |
| `run` | `session_ref`、`target`；可选 `params`、`working_dir` | 用户上下文 ShellExecute。 |
| `open` | `session_ref`、`file` | 在目标用户登录中打开文件。 |
| `list_apps` | `session_ref`；可选 `with_icons` | 用户应用目录。 |
| `get_mapped_drives` | `session_ref` | 该登录的映射盘。 |
| `logoff_session` | `user`、`session_ref` | 注销明确识别的登录。 |
| `quit` | 无 | 停止后台。 |

### 登录身份与快照

`session_ref` 不是裸 Session ID。以下仅为格式示例，实际请求应使用后台返回的值：

```json
{
  "session_id": 7,
  "authentication_id": "000000000000ABCD",
  "boot_identifier": "11111111-2222-4333-8444-555555555555"
}
```

AuthenticationId 是 token LUID 的 16 位大写十六进制值。BootIdentifier 来自内核 `SystemBootEnvironmentInformation`。SID 另行限定配置账号，不能用用户名或 Session ID 替代完整身份。

`status` / `watch_status` 示例：

```json
{
  "status":"ok","protocol":2,"role":"background","kind":"snapshot",
  "ssh_active":false,
  "memory":{"total_bytes":4294967296,"available_bytes":1073741824},
  "source":"etw",
  "snapshot":{"revision":1,"fresh":true,"sessions":[]}
}
```

快照登录项包含 `reference`、SID、用户、域、WTS 状态、客户端字段、LogonTime 和 ConnectTime。WTS 状态 `0` 为 Active，`4` 为 Disconnected。时间值是原始 FILETIME；消费者应保留 64 位精度。

`revision` 随可见内容变化。`fresh:false` 的快照只供展示，不能用于派发、切换或注销。窗口和托盘计数是可选观测，不是注销授权。字段未知时不当作零。

WTS 通知只触发异步刷新，不在回调内查询。SSH 和内存状态使用同一长连接：首次 `kind:"snapshot"`，之后变化时发送 `kind:"changed"`。没有应用层心跳。

### 用户执行

后台在派发前核对完整 SessionRef 和配置 SID。随后按需使用 WTS 用户 token、用户环境及 `winsta0\default` 桌面创建 worker。worker 再核对自身实际 Session ID、AuthenticationId、BootIdentifier 和 SID。

`session_status` 返回 worker 的实际引用，而不是回显请求。应用、文件、快捷方式和映射盘请求均在目标用户上下文执行；工作目录不继承 SYSTEM cwd。不同登录的 `net use` 和重定向盘可能不同。

### 注销

**警告：注销会结束目标登录中的应用。** 调用方应先获得用户授权，或满足已配置的空闲策略。

后台紧接着复核身份，再调用固定 `%SystemRoot%\System32\logoff.exe`，以独立十进制参数传入 Session ID，不经过 shell。

操作共用 30 秒预算。退出码 0 不代表成功；后台确认原 SessionRef 消失后才返回成功。错误返回 `outcome`：`not_started`、`failed`、`unknown` 或 `anomaly`。发现 ID 已对应新引用时只报告异常，不追加操作。

后台不自动重试，也不把终止子进程当作撤销注销。原生 WTS 查询不能通用地中断，查询或管道取消清理可能超过调用期限。最后一次身份校验与系统命令之间仍存在 ID 复用窗口。

## 本地诊断与观测

```powershell
remoteapp-launcher.exe ping --bind 192.0.2.10:49153 --auth-token-file PATH
remoteapp-launcher.exe status --bind 192.0.2.10:49153 --auth-token-file PATH
remoteapp-launcher.exe inspect-sessions --bind 192.0.2.10:49153 --auth-token-file PATH
remoteapp-launcher.exe list-apps --with-icons
remoteapp-launcher.exe check-ssh
```

`list-apps` 和 `check-ssh` 在当前进程直接查询。不要把 SYSTEM 下的本地应用列表当作目标用户目录。业务执行由宿主通过后台路由。

SSH 状态表示本地 22 端口已建立 TCP 连接的零/非零边界。ETW 事件触发 TCP 表核对，初始化和 60 秒检查提供兜底；不可用时为 Unknown。它不统计认证登录数。

内存每秒采样 `GlobalMemoryStatusEx`，只在百分比边界、总量或可用性变化时通知。阈值和容量策略位于宿主，详见 [内存控制](../docs/memory.md)。构建检查、实机测试入口与风险见 [开发与测试](../docs/development.md)。
