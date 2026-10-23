# RDP Launcher Go

事件驱动的 Linux 宿主控制器，配合 Rust Windows Agent。架构见 [docs/architecture.md](docs/architecture.md)。

## 构建与版本

项目统一使用 Go **1.27.3**。`make check` 与 `make build` 显式选择该工具链，即使本机设置了 `GOTOOLCHAIN=local`；不替换系统全局 Go。GUI 已直接内嵌至主模块，`make build` 生成单一 CLI 二进制 `bin/rdp-launcher`（不再构建或依赖独立的 `rdp-launcher-dialog` 二进制或子模块），在精确 release tag 上自动嵌入版本，其他提交显示 `dev`。例如检出 `v0.1.0` 后执行 `make build`，CLI 显示 `v0.1.0`，同时保留提交号及脏工作区标记。

1.27.3 包含 [GO-2026-6809 / CVE-2026-111943](https://pkg.go.dev/vuln/GO-2026-6809) 的 `crypto/x509` 中间 CA 拒绝信任修复；另一受支持分支的修复版本为 1.26.10。参见 [Go 官方发布记录](https://go.dev/doc/devel/release#go1.27.3)。这不是系统 CA 包升级，也不会替代外部 FreeRDP 的 TLS／证书策略。

## GitHub Actions

`.github/workflows/ci.yml` 在 push、PR、`v*` tag 和手动运行时检查代码。Linux 检查主模块（内嵌 GUI）并打包单一 CLI 二进制；Windows 原生运行 agent 的 clippy、单测和 release 构建。Actions artifact 保留 14 天，CI 不自动部署、不写入 GitHub Release，也不使用生产 token 或 VM。宿主与来宾更新仅支持手动配套升级，不设自动部署或回滚流程。

手动运行可填写 `ref`（例如 `v0.1.0`），用于验证早于工作流文件的已有 tag。Windows runner 单测不等于 RemoteApp／桌面切换或完整冷启动验收。

## 当前实现

- `internal/controller/`：主循环、FIFO、统一连接与登录绑定状态、自动操作统一准入、VM 控制确认、空闲 deadline、内存态 lock/unlock。
- `internal/adapter/`：复用 `digitalocean/go-libvirt`，接入 Niri、Windows Agent 与 FreeRDP；复用 `errgroup` 管理来源。
- `windows-agent/`：单一后台入口（Session 0 SYSTEM，默认端口 49153），通过受限本机 IPC 路由至按需启动的用户会话 worker；RemoteApp 保活作为内部 `remoteapp-shell` 角色运行（无 TCP 监听）；SSH 使用 `ferrisetw`，内存每秒本地采样，变化时通知。
- 自动内存：宿主双阈值默认 95%/80%，最终目标向上对齐 128 MiB；空闲注销启用时在确认注销后立即回收内存，未启用时维持挂起前回收。配置与驱动缓解见 [docs/memory.md](docs/memory.md)。

### 生命周期、锁定与协议升级

- **空闲超时与注销**：新配置 `lifecycle.idle_logoff_timeout`（单位秒，**默认 -1 表示禁用**），与日常空闲仅断开连接的现状一致。设置为 `0` 表示显示断开后立即尝试注销；启用时为非负值，与挂起计时起点一致（显示断开 `!Display` 后起算）。挂起启用时要求 `0 <= idle_logoff_timeout <= vm_suspend_timeout`（挂起禁用时不设此上界）。注销不依赖是否启用自动断开，手动关闭桌面或 RemoteApp 连接同样适用。启用注销时，内存回收移至确认注销完成后立即执行。
- **删除空会话自动注销**：彻底移除旧切换流程中因窗口和托盘计数为零而触发的 `require_empty` 自动注销逻辑；空登录默认保留，注销操作必须由用户在确认流程中明确授权。
- **统一 lock 语义**：lock 统一禁止所有自动破坏性操作（自动断开连接、自动注销图形登录、主动内存缩容及自动挂起 VM），但显式用户请求仍正常执行。lock 仅驻留在控制器服务内存中，不持久化、不创建锁文件；VM 关机清除 lock，服务重启同样恢复为 unlocked。
- **协议升级与强校验**：宿主与 Windows Agent 通信采用协议版本 2（`protocol: 2`）。新旧协议版本不匹配时立即拒绝并提示配套升级，不静默忽略新字段，不进行旧端点兜底兼容。更新采取手动配套升级。

后台状态使用长连接：首次快照，之后仅推送状态变化。没有应用层心跳和固定读取超时。VM Running 时重新订阅，暂停/关闭时结束旧订阅。libvirt、内存、Niri 与后台来源断线后各自退避重连；订阅被确认后重新查询 VM/内存或取得新快照，断线期间观测视为失效。VM 命令已接受但确认来源丢失时立即释放控制槽，并向关联调用者报告结果未知；补查失败则重建该次来源，按相同退避继续恢复，不等待新 CLI 请求。恢复只覆盖观测，不自动重放已发出且结果未知的 run/open 等业务请求。SSH 只区分零/非零/Unknown，60 秒检查为 Windows 侧的低频兜底。

## CLI

```sh
GOTOOLCHAIN=go1.27.3 go build -o rdp-launcher ./cmd/rdp-launcher
mkdir -p ~/.config/rdp-launcher-go
cp -n config.example.toml ~/.config/rdp-launcher-go/config.toml
# 修改 guest IP、用户、程序位置等配置；Windows 后台必须已启动。
./rdp-launcher auth check       # RDP 密码；没有时用 auth set。
./rdp-launcher auth agent check # 共享 agent token；没有时用 auth agent set。
./rdp-launcher start            # 用户级 systemd 后台启动，就绪后返回终端。
```

```sh
./rdp-launcher status           # 本地快照，不唤醒 VM。
./rdp-launcher ping             # 仅后台请求，不启动 FreeRDP。
./rdp-launcher lock
./rdp-launcher unlock
./rdp-launcher run WINWORD.EXE
./rdp-launcher open '/data/shared/report.docx'
./rdp-launcher open --app WINWORD.EXE 'J:\report.docx'
./rdp-launcher mounts           # 交互用户的映射盘。
./rdp-launcher sync-apps        # 获取应用、图标并生成 .desktop。
./rdp-launcher list-apps        # 读取本地缓存。
./rdp-launcher path '/data/shared/report.docx'
./rdp-launcher shutdown
```

全局 `--config`、`--socket`、`--timeout` 放在命令前；子命令参数放在文件或应用名前。`--timeout 0` 不设客户端时限。`--help` 列出入口。`watcher` 是 `serve` 的别名。

`rdp-launcher version` 或 `rdp-launcher --version` 显示该 CLI 二进制的版本及 commit hash，不读取配置、不连接控制器，也不唤醒 VM。版本默认显示 `dev`，可在构建时指定；不显示带日期的自动伪版本。commit 来自 Go 嵌入的 VCS 信息，取前 8 位，未提交修改标记为 `-dirty`；没有 VCS 信息时显示 `unknown`。发布构建可用 `-ldflags '-X main.version=v0.1.0'` 指定版本；源码包构建还可用 `-X main.commit=<hash>` 指定 commit；显式指定时忽略本地 VCS 的 `dirty` 状态。此命令不代表当前常驻控制器的构建版本。

`rdp-launcher start` 通过用户级 systemd 后台启动控制器，就绪后返回终端，不需要保持 shell。已有控制器（包括手动 `serve`／`watcher`）运行时直接返回，不重启或断开连接。首次启动会创建用户单元，并通过专用 ExecStart drop-in 固定当前可执行文件、配置和 socket；保留已有单元的其他设置，不自动开启登录自启。`serve`／`watcher` 保留作前台调试。`status` 仅显示可读纯文本状态，覆盖 lock、当前连接与 Windows 登录绑定状态、会话快照新鲜度、当前阶段、最近错误、阻塞原因及保留登录数；不接受 `--json`。`help`／`--help` 也显示版本。这些本地状态查询不会唤醒 VM。

控制器在用户运行目录创建权限 0600 的 Unix socket，并在整个 `serve` 生命周期持有同目录 `*.lock` 文件的 `Flock(LOCK_EX|LOCK_NB)` 独占所有权；不写 PID 文件。该 lock 只区分 Linux 控制器所有者，不是 Windows 会话锁；多个 CLI 共用同一个控制器和 FIFO。客户端退出会取消其未完成请求，不重放 run/open。Keyring 使用 `secret-tool`，凭据不写入 TOML，不进入 CLI argv 或 Linux 控制 socket；agent token 仅附加在受控网络的 Windows 请求 envelope 中。

### Windows Agent 共享 token 鉴权与单一后台入口

宿主仅连接 Windows Agent 的单一后台端点（Session 0 SYSTEM，默认端口 `49153`），对**所有**动作校验共享 token（唯一例外是下文明确记录的 loopback local-only 模式），包括 ping、status、watch_status、quit、run、open、应用扫描和会话管理。彻底废弃全局交互 TCP 端点（原 `agent_port` 49152 移除，配置文件中若有残留配置会给出明确迁移错误）。这里只做身份校验，不提供传输加密；明文 TCP 仍只适用于受控网络或受防火墙限制的主机链路，不能把 token 当作公网安全方案。

- 宿主从 Keyring 读取 token：服务名默认是 `<server.service>-agent`（默认 `rdp-bridge-agent`），用户名固定为 `agent`，与 RDP 密码条目分开。可用 `[server].agent_service` 覆盖。
- Windows 后台端只用 `--auth-token-file PATH` 指向 token 文件，启动时读取一次；token 本身不进入 argv 或日志。后台服务必须显式带该参数。
- 用户会话动作（启动应用、打开文件、枚举应用、映射盘）由后台服务在收到请求后，核对配置的账户 SID 与 SessionRef，通过本机受限 IPC 路由至按需启动的用户会话 worker 执行。RemoteApp shell 仅作为连接保活运行（`remoteapp-shell` 角色），不监听 TCP。
- FreeRDP 客户端主机名通过 `[freerdp].client_hostname` 配置（默认 `rdp-launcher`，<=15 字符 ASCII），保持 FreeRDP 客户端 NetBIOS 名称稳定，以保证会话可靠复用与重连关联。
- 非 loopback 监听缺少 token 文件时直接拒绝启动，不会静默退回无鉴权。仅 loopback 且未提供 token 文件时保留**显式 local-only 模式**，只适合本机测试。
- 请求 JSON 使用顶层 `token` 字段；校验失败返回 `authentication required`。当前主程序会以固定动作名记录请求，不再打印原始请求内容。

配置并检查：

```sh
# 使用 secret-tool 的隐藏输入提示；输入双方共享的足够长随机 token。
./rdp-launcher auth agent set
./rdp-launcher auth agent check
./rdp-launcher auth agent clear
```

在 Windows 上创建同一 token 的单行文件（路径写入 `[server].agent_token_file`）。文件末尾允许一个 LF 或 CRLF 行结束符；其余 CR/LF 会拒绝，空格/制表符保留为 token 内容。该文件的 ACL 必须同时允许 SYSTEM（后台服务）和配置的交互用户，可另允许 Administrators（受信机器管理员例外；管理员可绕过 ACL，不代表普通用户权限）；不得允许 Users、Authenticated Users、Everyone 或其他本地用户读取。`--session-user` 只是会话管理目标，不能代替 ACL 的交互用户配置。`windows-agent/install.ps1 -InteractiveUser <Windows-user>` 可用隐藏提示或 `-TokenSource` 文件提供 token、设置受保护 ACL 并拒绝过宽 trustee；安装后可用 `-VerifyOnly` 复查。ACL 的验收点是该脚本：Rust agent 启动时只校验 token 值，不重新解析 ACL；管理员或所有者变更 ACL 后应重跑验证。已在 Win11 的临时 fixture 上实测指定用户账户与 SYSTEM 均能读取相同 token，正确 ACL 通过校验，宽泛 Users 授权及 DENY ACE 被拒绝。复现命令见 `windows-agent/tests/token-acl.ps1`；测试不修改生产 token、现有 agent 或服务。部署到其他账户／路径后仍应运行 `-VerifyOnly`。

`secret-tool lookup` 经管道输出原始字节，不会附加换行；若手工写入 Keyring 时用了 `echo`，请改用 `printf %s` 或重新用 `auth agent set` 设置，否则含 CR/LF 的条目会被明确拒绝而不是被静默裁剪。

`sync-apps` 需要 `desktop-file-utils`（提供 `desktop-file-edit`）；桌面文件的键值序列化交给其 GLib 实现，不复用 shell 或 INI 转义。`[freerdp].app_id` 指定 Niri 观察的客户端标识，默认是 `com.freerdp.client.sdl3`；其他 FreeRDP 客户端应填自己的实际 App ID。该配置只影响观察，不改客户端的窗口角色。可选 `[freerdp.env]` 表用于给 FreeRDP 子进程设置字符串环境变量，例如 `SDL_RENDER_DRIVER = "opengl"`。优先级为继承环境 < `video_driver` < 显式 env；空字符串保留，不做 shell 展开，不修改控制器环境。默认仍过滤继承的代理变量，显式 env 可主动设置代理。环境变量不适合存放凭据（配置及 `show-config` 可见）；RDP 密码仍只经 stdin 传入。修改配置后需重启控制器，下一次启动 FreeRDP 时使用新值。

内置“Windows 桌面”入口由 `rdp-launcher install-shortcuts` 安装，并在 `sync-apps` 时自动维护。该命令只使用本地配置和缓存，不连接控制器、不唤醒 VM；同时重建已缓存的应用入口。固定入口为 `$XDG_DATA_HOME/applications/rdp-launcher-go.desktop`（默认 `~/.local/share/applications/`），执行 `rdp-launcher desktop`，复用安全模式切换，不直接调用 FreeRDP。它不作为远端应用写入缓存，也不受远端应用过滤影响。

应用缓存位于 `$XDG_CACHE_HOME/rdp-launcher-go`，桌面入口位于 `$XDG_DATA_HOME/applications`；未设 XDG 变量时遵循用户目录默认值。同步持有缓存目录的 `.sync.lock`，先准备图标及桌面文件，成功发布后最后提交原始缓存；正常发布失败会尝试回滚并报告回滚失败。持有同步锁时也清理崩溃残留的 `.rdp-launcher-go-stage-*.desktop`（序列化工具要求此后缀）。只清理自身 `rdp-launcher-go-` 桌面文件及未引用 PNG，不宣称跨目录的崩溃事务原子性。支持配置中的 MIME/分类 overrides。Linux 文件通过显式 `[mounts]` 转成 Windows 路径；`mounts` 只查询，不创建共享或修改映射。

### 启动、模式切换与确认流程

```sh
rdp-launcher desktop     # 连接完整桌面（必要时提示切换）。
rdp-launcher remoteapp   # 连接 RemoteApp（必要时提示切换）。
```

桌面模式与 RemoteApp 共用一个 FreeRDP 客户端和 Keyring。同一控制器单所有者只管理一个 FreeRDP 实例，按 Pro 的单活跃交互式会话模型处理。业务启动（`run`/`open`）与显式连接（`desktop`/`remoteapp`）共用同一个 FIFO 执行流程，保留原始 payload。

当目标模式与当前状态存在冲突或需要用户选择时，控制器在同一个持续请求 IPC 连接中向 CLI 发送确认提示，提供互斥的四个动作选项：
1. **在当前桌面执行**（`current`）：直接在已绑定的当前桌面登录中执行，不停止 FreeRDP，不注销。
2. **保留并切换**（`keep`）：断开旧连接，保留 Windows 端的旧登录，连接目标模式并重新绑定后执行。
3. **注销并切换**（`logoff`）：注销授权的具体旧登录并确认其结束，再建立新连接执行。
4. **取消**（`cancel`）：放弃本次操作，不更动原连接。

CLI 全局支持 `--on-conflict=current|keep|logoff|cancel`。确认呈现方式遵循一致模型：
- **Wayland 环境**：CLI 直接调起进程内嵌的 MyGo 原生确认窗（单二进制直接承载，无需单独 dialog 二进制或 `[ui].dialog_bin` 配置）。
- **无图形但有 TTY**：在终端提供相同的动作选择与警告提示。
- **无 Wayland 且无 TTY**：若未显式指定 `--on-conflict`，stderr 输出明确选项要求并以非零码退出。

确认等待上限为 60 秒，并受请求总时限约束；超时或取消均释放队头，保持原连接不变。

注销操作由 Windows Agent 在 Session 0 的 SYSTEM 上下文中通过系统 `%SystemRoot%\System32\logoff.exe <Session ID>` 独立子进程执行，执行前重新校验 SessionRef，并在有界时限内轮询确认原 SessionRef 完全消失，不再使用旧的不稳定进程内注销。

确认窗采用紧凑布局，默认跟随系统明暗、强调色及字体。检测到 DMS 时，直接读取 `$XDG_CONFIG_HOME/DankMaterialShell/settings.json` 的 `popupTransparency`（浮窗不同步时使用 `floatingWindowTransparency`）、`cornerRadius`、`fontFamily`、`fontScale` 和 `fontWeight`，并复用 `$XDG_CACHE_HOME/DankMaterialShell/dms-colors.json` 当前 Matugen 配色，无需另建模板。按钮高度／水平内边距采用 DMS 的 40／16，间距采用其 12／24 标尺，并随字体缩放调整布局。未设置 XDG 时使用标准用户目录。每次打开重新读取，缺失或损坏时回退系统主题；只有配色缓存、没有有效 settings 时仅应用配色，保留系统字体和尺寸指标。DMS 模式按实际设置窗口分层：外层使用浮窗 surface/alpha，内容置于独立实色卡片，文字及按钮保持不透明；非 DMS 桌面保留系统主题回退；系统要求减少透明时使用实色面板，高对比度时保留系统主题。正文使用滚动兜底，按钮保持在独立底栏；取消初始获焦时显示描边，注销提示使用 warning 琥珀色，并按实色卡片背景调整至至少 4.5:1 的文字对比度；注销按钮仍保留危险色。niri 26.04+ 可使用 [`contrib/niri-rdp-launcher.kdl`](contrib/niri-rdp-launcher.kdl)：复制到 `~/.config/niri/rdp-launcher.kdl`，在主配置加入 `include optional=true "rdp-launcher.kdl"`，然后运行 `niri validate`。规则匹配确认窗（`app-id="^rdp-launcher$"`），保留 xray、默认关闭强制 blur，继承桌面／DMS 的窗口外圆角，不另行覆盖，并避免焦点边框实色背景盖住效果。

### 单一 CLI 二进制构建与配套更新

主服务与 GUI 完整合并为单一 Go CLI 二进制 `bin/rdp-launcher`，不再维护或依赖独立的 `rdp-launcher-dialog` 二进制或嵌套模块：

```sh
GOTOOLCHAIN=go1.27.3 go build -o bin/rdp-launcher ./cmd/rdp-launcher
```

只需将 `bin/rdp-launcher` 放入 PATH 或 systemd 服务单元中，无需配置 `[ui].dialog_bin`。

**手动配套升级**：宿主控制器与 Windows Agent 均采用协议版本 2（`protocol: 2`）。新旧协议不匹配时会立即拒绝并明确提示配套升级（`protocol mismatch: Windows agent and host must be upgraded together`）。更新必须同时部署宿主二进制与 Windows Agent 二进制，不设自动部署或回滚脚本，不保留旧版本协议兼容。

### 应用过滤（宿主侧）

在 Linux 配置文件中添加：

```toml
[filters]
exclude_apps = ["documentation-for-desktop-apps", "Sample Desktop Apps"]
exclude_dirs = ['C:\Program Files (x86)\Windows Kits\10\Shortcuts']
```

- `exclude_apps` 对 ID、名称、远端目标路径或快捷方式路径精确匹配，不做子串或通配符匹配。
- `exclude_dirs` 排除来源快捷方式或目标位于该目录及子目录的条目；按 Windows 路径比较，忽略大小写，接受两种斜杠，不误匹配同名前缀的其他目录。
- 路径填远端的完整路径，不填 Linux 挂载路径，也不在宿主展开 Windows 环境变量。UWP 没有快捷方式路径时，按 ID、名称或 AUMID 排除，不猜安装目录。
- Rust 只提供信息；规则、匹配和发布决定均在宿主。远端仍遍历扫描目录，这是扫描结果过滤，不是远端遍历剪枝。
- 过滤统一应用于合并后的应用目录，包括手动条目；MIME／分类 overrides 不会让排除条目重新出现。`apps`、`list-apps` 和 `sync-apps` 使用相同规则。
- 原始发现缓存保留，修改过滤规则后无需重新扫描即可查看过滤后的目录；运行 `sync-apps` 更新启动器入口并删除自有命名空间中的过期文件。旧 `remoteapp-` 命名空间不在自动清理范围内。
- 来源目录匹配需要新版 agent 提供 `shortcut_path`；旧缓存仍可按现有 ID、名称及目标路径过滤。过滤用于应用目录，不是禁止显式执行远端程序的权限规则。

### 手动添加应用

扫描未发现的系统程序或便携程序，直接加入配置：

```toml
[[apps]]
id = "explorer"
name = "文件资源管理器"
target = 'C:\Windows\explorer.exe'
# icon = "/absolute/path/to/windows-explorer.png"
# arguments = '/separate'
# working_dir = 'C:\Windows'
```

`id`、`name`、`target` 必填。`icon` 可指定图标文件绝对路径或图标主题名；应使用对应 Windows 应用的真实图标，不要用 Linux 同类应用的图标代替。不配置时不指定图标。

保存后即可运行 `rdp-launcher run explorer` 或 `rdp-launcher open --app explorer 'J:\目录'`，无需同步或重启控制器。`list-apps` 合并手动配置和本地缓存，不唤醒 VM；`apps` 合并手动配置和实时扫描结果。相同 ID 以手动配置为准。

运行 `sync-apps` 时也会生成这些应用的桌面入口。缓存只保存扫描结果；删除手动配置后，不会从缓存恢复该手动项。再同步一次可清除旧桌面入口。

## 作为库接入

通过 `adapter.Open(ctx, config)` 注册 libvirt 事件，再启动 `session.Run()`。请求入口调用 `session.Controller.Do(ctx, request)`。

- libvirt 命令成功仅表示接受；生命周期事件确认目标状态后主循环才推进。
- 显式提供 libvirt socket/URI/domain、后台 endpoint、FreeRDP 参数、密码读取函数和 `AgentToken` provider。`AgentToken` 为 nil 仅用于既有 mock 或显式 loopback local-only 场景；生产配置应提供从 Keyring 读取的 provider。
- 宿主与 Windows Agent 之间仅通过单一后台 endpoint 通信，由后台服务路由至受管会话 worker。
- `NiriSocket` 留空时窗口状态为 Unknown。
- 凭据经 stdin 交给 FreeRDP，Keyring 读取由调用方接入。

配置格式见 [config.example.toml](config.example.toml)。guest 地址显式配置；初次连接会等待后台启动。已建立来源的意外断线由该来源自行退避重连，并在订阅确认后重新查询/取快照；这是观测恢复，不是业务请求重试框架，run/open、注销和内存写入不会因断线自动重放。Windows 安装和协议说明见 [windows-agent/README.md](windows-agent/README.md)。

## 验证

统一入口为 `make check`，覆盖主 Go 模块（内嵌 GUI）及 Windows Rust 目标。Windows 测试在此入口中仅交叉构建，不自动运行 guest 或注销会话。socket 所有权并发竞争可单独重复 100 次：`go test -race -count=100 ./internal/host -run '^TestControllerSocketConcurrentStaleRecoveryHasOneReachableOwner$'`。修复取舍及验收边界见 [审查处理决定](docs/review-decisions.md)。

```sh
go test ./...
go test -race ./...
go vet ./...
```

Windows 测试需要 Windows 目标；本轮交叉构建测试 exe 后在 win11 原生执行。

真实环境测试默认跳过。已有后台与 VM 运行时，可以显式执行只读仅后台测试：

```sh
RDP_LIVE_BACKGROUND=192.0.2.14:49253 \
  go test -v ./internal/adapter -run '^TestLiveBackgroundOnlySession$'
```

同文件中的 `TestLiveBackgroundWatchQuiet` 会等待 32 秒，再通过 `ssh win11 'Start-Sleep -Seconds 2'` 产生状态变化，以验证同一安静连接仍可收消息。只在专用测试环境显式运行。

迁移验证见 [docs/windows-validation.md](docs/windows-validation.md)，内存与真实暂停恢复验证见 [docs/memory-validation.md](docs/memory-validation.md)。
