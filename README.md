# RDP Launcher Go

事件驱动的 Linux 宿主控制器，配合 Rust Windows Agent。架构见 [docs/architecture.md](docs/architecture.md)。

## 当前实现

- `internal/controller/`：主循环、FIFO、后台/交互两种就绪条件、VM 控制确认、空闲 deadline、lock/unlock。
- `internal/adapter/`：复用 `digitalocean/go-libvirt`，接入 Niri、Windows Agent 与 FreeRDP；复用 `errgroup` 管理来源。
- `windows-agent/`：后台与交互端两个入口，复用旧 Rust 能力；SSH 使用 `ferrisetw`，内存只做每秒本地采样，变化时通知。
- 自动内存：宿主双阈值默认 95%/80%，最终目标向上对齐 128 MiB；suspend 前 30 秒回收。配置与驱动缓解见 [docs/memory.md](docs/memory.md)。

后台状态使用长连接：首次快照，之后仅推送状态变化。没有应用层心跳和固定读取超时。VM Running 时重新订阅，暂停/关闭时结束旧订阅。libvirt、内存、Niri 与后台来源断线后各自退避重连；订阅被确认后重新查询 VM/内存或取得新快照，断线期间观测视为失效。VM 命令已接受但确认来源丢失时立即释放控制槽，并向关联调用者报告结果未知；补查失败则重建该次来源，按相同退避继续恢复，不等待新 CLI 请求。恢复只覆盖观测，不自动重放已发出且结果未知的 run/open 等业务请求。SSH 只区分零/非零/Unknown，60 秒检查为 Windows 侧的低频兜底。

## CLI

```sh
go build -o rdp-launcher ./cmd/rdp-launcher
mkdir -p ~/.config/rdp-launcher-go
cp -n config.example.toml ~/.config/rdp-launcher-go/config.toml
# 修改 guest IP、用户、程序位置等配置；Windows 后台必须已启动。
./rdp-launcher auth check       # RDP 密码；没有时用 auth set。
./rdp-launcher auth agent check # 共享 agent token；没有时用 auth agent set。
./rdp-launcher serve            # 前台常驻；其他终端通过同一个 Unix socket 提交请求。
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

控制器在用户运行目录创建权限 0600 的 Unix socket，并在整个 `serve` 生命周期持有同目录 `*.lock` 文件的 `Flock(LOCK_EX|LOCK_NB)` 独占所有权；不写 PID 文件。该 lock 只区分 Linux 控制器所有者，不是 Windows 会话锁；多个 CLI 共用同一个控制器和 FIFO。客户端退出会取消其未完成请求，不重放 run/open。Keyring 使用 `secret-tool`，凭据不写入 TOML，不进入 CLI argv 或 Linux 控制 socket；agent token 仅附加在受控网络的 Windows 请求 envelope 中。

### Windows Agent 共享 token 鉴权

两个 Windows TCP 端点（后台与交互）对**所有**动作校验共享 token（唯一例外是下文明确记录的 loopback local-only 模式），包括 ping、status、watch_status、quit、run、open、应用扫描和会话管理。这里只做身份校验，不提供传输加密；明文 TCP 仍只适用于受控网络或受防火墙限制的主机链路，不能把 token 当作公网安全方案。

- 宿主从 Keyring 读取 token：服务名默认是 `<server.service>-agent`（默认 `rdp-bridge-agent`），用户名固定为 `agent`，与 RDP 密码条目分开。可用 `[server].agent_service` 覆盖。
- Windows 端只用 `--auth-token-file PATH` 指向 token 文件，启动时读取一次；token 本身不进入 argv 或日志。宿主启动交互 daemon 时会通过 `[server].agent_token_file` 把 Windows 路径加入 RemoteApp 命令行；后台服务/手动启动必须显式带同一个参数。RemoteApp 的 token 文件路径不支持空白字符（含空格、制表符及 Unicode 空白）、逗号或双引号；请使用默认的 `C:\ProgramData\rdp-launcher\agent-token` 路径，避免嵌套引号解析歧义。
- 非 loopback 监听缺少 token 文件时直接拒绝启动，不会静默退回无鉴权。仅 loopback 且未提供 token 文件时保留**显式 local-only 模式**，只适合本机测试。
- 请求 JSON 使用顶层 `token` 字段；校验失败返回 `authentication required`。当前主程序会以固定动作名记录请求，不再打印原始请求内容。

配置并检查：

```sh
# 使用 secret-tool 的隐藏输入提示；输入双方共享的足够长随机 token。
./rdp-launcher auth agent set
./rdp-launcher auth agent check
./rdp-launcher auth agent clear
```

在 Windows 上创建同一 token 的单行文件（路径写入 `[server].agent_token_file`）。文件末尾允许一个 LF 或 CRLF 行结束符；其余 CR/LF 会拒绝，空格/制表符保留为 token 内容。该文件的 ACL 必须同时允许 SYSTEM（后台服务）和配置的交互用户（RemoteApp daemon），可另允许 Administrators（受信机器管理员例外；管理员可绕过 ACL，不代表普通用户权限）；不得允许 Users、Authenticated Users、Everyone 或其他本地用户读取。`--session-user` 只是会话管理目标，不能代替 ACL 的交互用户配置。`windows-agent/install.ps1 -InteractiveUser <Windows-user>` 可用隐藏提示或 `-TokenSource` 文件提供 token、设置受保护 ACL 并拒绝过宽 trustee；安装后可用 `-VerifyOnly` 复查。ACL 的验收点是该脚本：Rust agent 启动时只校验 token 值，不重新解析 ACL；管理员或所有者变更 ACL 后应重跑验证。已在 Win11 的临时 fixture 上实测指定用户账户与 SYSTEM 均能读取相同 token，正确 ACL 通过校验，宽泛 Users 授权及 DENY ACE 被拒绝。复现命令见 `windows-agent/tests/token-acl.ps1`；测试不修改生产 token、现有 agent 或服务。部署到其他账户／路径后仍应运行 `-VerifyOnly`。

`secret-tool lookup` 经管道输出原始字节，不会附加换行；若手工写入 Keyring 时用了 `echo`，请改用 `printf %s` 或重新用 `auth agent set` 设置，否则含 CR/LF 的条目会被明确拒绝而不是被静默裁剪。

`sync-apps` 需要 `desktop-file-utils`（提供 `desktop-file-edit`）；桌面文件的键值序列化交给其 GLib 实现，不复用 shell 或 INI 转义。`[freerdp].app_id` 指定 Niri 观察的客户端标识，默认是 `com.freerdp.client.sdl3`；其他 FreeRDP 客户端应填自己的实际 App ID。该配置只影响观察，不改客户端的窗口角色。

应用缓存位于 `$XDG_CACHE_HOME/rdp-launcher-go`，桌面入口位于 `$XDG_DATA_HOME/applications`；未设 XDG 变量时遵循用户目录默认值。同步持有缓存目录的 `.sync.lock`，先准备图标及桌面文件，成功发布后最后提交原始缓存；正常发布失败会尝试回滚并报告回滚失败。持有同步锁时也清理崩溃残留的 `.rdp-launcher-go-stage-*.desktop`（序列化工具要求此后缀）。只清理自身 `rdp-launcher-go-` 桌面文件及未引用 PNG，不宣称跨目录的崩溃事务原子性。支持配置中的 MIME/分类 overrides。Linux 文件通过显式 `[mounts]` 转成 Windows 路径；`mounts` 只查询，不创建共享或修改映射。

### 全屏桌面模式

```sh
rdp-launcher desktop     # 检查旧图形会话，再连接完整桌面。
rdp-launcher remoteapp   # 检查旧图形会话，再连接 RemoteApp。
```

桌面模式使用同一个 FreeRDP 和 Keyring，参数为 `/f +dynamic-resolution`，不传 `/app:`，不等待交互 agent。成功响应只表示客户端进程已启动，不保证远端认证或首帧已完成。`Ctrl+Alt+Enter` 切换全屏；关闭桌面客户端窗口可断开连接。

同一控制器只拥有一个 FreeRDP 进程；按 Pro 的单活跃交互式会话模型处理，不做多用户 RDS 调度。切换与普通请求共用 FIFO。没有用户窗口、也没有用户托盘时，自动注销明确识别的旧图形会话，再连接新模式。有内容或状态未知时，MyGo 原生窗口询问“保留并切换／注销并切换／取消”。取消、Esc 和关闭确认窗均不改变旧连接；Enter 默认取消。确认窗未得到选择前不先断开连接。原来的 `--replace` 已移除。

确认窗采用紧凑布局，默认跟随系统明暗、强调色及字体。检测到 DMS 时，直接读取 `$XDG_CONFIG_HOME/DankMaterialShell/settings.json` 的 `popupTransparency`（值表示不透明度）、`cornerRadius`、`fontFamily`、`fontScale` 和 `fontWeight`，并复用 `$XDG_CACHE_HOME/DankMaterialShell/dms-colors.json` 当前 Matugen 配色，无需另建模板。按钮高度／水平内边距采用 DMS 的 40／16，间距采用其 12／24 标尺，并随字体缩放调整布局。未设置 XDG 时使用标准用户目录。每次打开重新读取，缺失或损坏时回退系统主题。只对面板背景做半透明，不降低文字和按钮的不透明度；系统要求减少透明时使用实色面板，高对比度时保留系统主题。niri 26.04+ 可使用 [`contrib/niri-rdp-launcher.kdl`](contrib/niri-rdp-launcher.kdl)：复制到 `~/.config/niri/rdp-launcher.kdl`，在主配置加入 `include optional=true "rdp-launcher.kdl"`，然后运行 `niri validate`。规则仅匹配确认窗，启用 blur/xray，继承桌面／DMS 的窗口外圆角，不另行覆盖，并避免焦点边框实色背景盖住效果；xray 透出模糊壁纸，不透出底下窗口的文字。其他合成器的模糊支持由其自身配置决定。

Windows Session ID 只用于定位目标；后台同时校验用户与登录指纹，拒绝 session 0、已变化的登录和不明确的目标。自动注销前再次确认窗口和托盘均为零；未知不等于零。Shell 托盘枚举是可选能力，RemoteApp 壳不能可靠枚举时会显示“未知”并要求确认，不会假定没有托盘。不存在可唯一确定的用户会话时，“注销并切换”不可用。图形会话中的无窗口应用也会随注销结束；SSH／服务会话独立观察，不作为本操作的注销目标。

桌面连接存在时不会因 RemoteApp 窗口计数为零而自动断开或暂停 VM。退出后仍使用原空闲策略。`status` 的 `DisplayMode` 显示 `desktop` 或 `remoteapp`。RemoteApp 就绪需要交互 agent 所在会话已激活；旧的断开会话里仍存活的 agent 不算新连接就绪。

`run/open/apps/sync-apps/mounts` 仍使用 RemoteApp 和交互 agent；桌面连接存在时会明确报模式冲突。先用 `remoteapp` 切换，再执行这些命令。后台请求仍可运行。

### 原生确认窗构建与部署

主服务仍是纯 Go CLI；GUI 是按需启动的短生命周期进程，不是新常驻服务或 WebView。GUI 仅收观察信息、返回选择，不接收密码或执行注销。MyGo 所需 Go 1.27.1 放在独立模块，不升级主服务的 Go 要求：

```sh
go build -o bin/rdp-launcher ./cmd/rdp-launcher
(cd cmd/rdp-launcher-dialog && GOTOOLCHAIN=go1.27.1 go build -o ../../bin/rdp-launcher-dialog .)
(cd cmd/rdp-launcher-dialog && GOTOOLCHAIN=go1.27.1 go test ./...)
```

将两个 Linux 二进制放入服务可用的 PATH，或配置 `[ui].dialog_bin` 为绝对路径。需要 GTK3 运行库及用户图形会话环境。同步更新 Windows backend 与 interactive agent；后台以 SYSTEM 运行，并指定 `--session-user <Windows-user> --session-host <Linux-host-IP>`。仅允许指定来源主机管理指定用户，默认关闭会话管理。新 CLI 使用独立的 `switch-display` IPC 动作，旧服务会拒绝它，不会静默降级为直接断开。不要只更新 CLI。

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
- 显式提供 libvirt socket/URI/domain、后台 endpoint、交互 endpoint、FreeRDP 参数、密码读取函数和 `AgentToken` provider。`AgentToken` 为 nil 仅用于既有 mock 或显式 loopback local-only 场景；生产配置应提供从 Keyring 读取的 provider。
- 后台与交互 endpoint 分开配置。仅后台请求可以不配置交互端，且不会启动 FreeRDP。
- `NiriSocket` 留空时窗口状态为 Unknown。
- 凭据经 stdin 交给 FreeRDP，Keyring 读取由调用方接入。

配置格式见 [config.example.toml](config.example.toml)。guest 地址显式配置；初次连接会等待后台启动。已建立来源的意外断线由该来源自行退避重连，并在订阅确认后重新查询/取快照；这是观测恢复，不是业务请求重试框架，run/open、注销和内存写入不会因断线自动重放。Windows 安装和协议说明见 [windows-agent/README.md](windows-agent/README.md)。

## 验证

统一入口为 `make check`，覆盖主 Go 模块、独立 GUI 模块及 Windows Rust 目标。GUI 使用 `GUI_GOTOOLCHAIN`（默认 `go1.27.1`）；仅运行根目录 `go test ./...` 不覆盖 GUI。Windows 测试在此入口中仅交叉构建，不自动运行 guest 或注销会话。socket 所有权并发竞争可单独重复 100 次：`go test -race -count=100 ./internal/host -run '^TestControllerSocketConcurrentStaleRecoveryHasOneReachableOwner$'`。修复取舍及验收边界见 [审查处理决定](docs/review-decisions.md)。

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
