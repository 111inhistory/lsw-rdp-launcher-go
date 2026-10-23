# AGENTS.md

本文件面向在本仓库工作的 AI 编码与审查 Agent，提供执行边界、架构不变量、防膨胀原则与验证速查。功能细节与协议定义以链接文档为准，不在此重复。

## 1. 项目边界与文档地图

- **范围**：一个 Linux 控制器管理一个 Windows VM、一个自持 FreeRDP 连接和一个业务 FIFO。不扩展多 VM 调度或自动接管外部连接框架。
- **文档索引**：
  - 用户与配置入口：[README.md](README.md)、[config.example.toml](config.example.toml)
  - 架构、状态机与会话绑定：[docs/architecture.md](docs/architecture.md)
  - Windows Agent 角色与协议：[windows-agent/README.md](windows-agent/README.md)
  - 自动内存控制与驱动约束：[docs/memory.md](docs/memory.md)、[docs/virtio-mem-driver-notes.md](docs/virtio-mem-driver-notes.md)
  - 开发、确定性测试与实机测试：[docs/development.md](docs/development.md)
- 工具链与构建入口以 `Makefile`、`go.mod` 及 `.github/workflows/ci.yml` 为准，Rust 依赖见 `windows-agent/Cargo.toml` / `windows-agent/Cargo.lock`，不在文档中硬编码额外副本。

## 2. 不可破坏的系统不变量

- **单 Owner 状态转换**：`internal/controller` 的 `machine` 状态演进由单 owner 协程维护，不执行阻塞 I/O。异步操作与订阅流必须通过代次（Revision / Generation）围栏隔离断线或超时后的晚到结果。
- **命令接受 ≠ 状态确认**：VM 启动/暂停/恢复、内存设备 `requested` 更新、`logoff.exe` 退出码 0 均只代表请求被接受，必须等待对应的生命周期事件、QMP 事件或原 `SessionRef` 消失后才视为确认完成。
- **未知 ≠ 零**：Niri 断线、内存 `null`、SSH 表读取不完整、私有托盘接口无回调（`None`）均表示 `Unknown`，严禁折算为 0 窗口 / 0 连接 / 空会话去触发自动断开、注销或挂起。
- **Windows 登录身份与单 Token 边界**：登录身份使用完整 `SessionRef`（`SessionID` + Token `AuthenticationId` LUID + 内核 `BootIdentifier`）配合配置账号 `SID`，禁止单凭 `SessionId` 或用户名标识会话。派发用户动作时：在提交 `spawn_blocking` **之前**固定绝对 `deadline`；在**实际传给 `CreateEnvironmentBlock` / `CreateProcessAsUserW` 的同一个 Token 句柄**上核对 SessionID、SID 与 LUID，并核对当前内核 BootIdentifier，避免二次按 `SessionId` 取 Token 的 TOCTOU 窗口。
- **已提交且结果不明的副作用不自动重放**：`run`、`open`、`logoff_session` **在请求提交后**若因传输中断或超时导致无法确认远端执行结果，结果标为 `unknown` 并禁止自动重试；服务端已明确返回的 `not_started` 或已知错误保留其确定结果，但同样不授权自动重放已提交动作或已完成批次（连接建立前失败除外）。

## 3. 架构审美、强弱依赖辨析与修改原则

- **先审前提，区分强依赖与弱依赖**：
  - 新增防御校验、回滚或协议字段前，先核实故障前提在现实中是否成立，并区分**强依赖前提**（一旦破坏会导致改错 VM、杀错登录会话、未确认安全目标即 Resume 等不可逆后果）与**弱依赖前提**（如本地 `.desktop` 快捷方式、图标缓存等可随时由 `install-shortcuts` / `sync-apps` 重建的派生文件）。
  - 内部结构不天然等同于弱前提：仅在证明上游已对**同一对象、同一时刻**提供保证且无异步变化/TOCTOU 窗口后，才可合并重复校验。
  - 严禁把弱依赖边界当成强事务不变量去堆砌跨文件 `.bak` 拷贝回滚或层层自检。
- **保持架构审美与主体性，变丑前先停下并交用户拍板**：
  - 面对边缘追问或审查意见时，不机械顺从、不为应付极端巧合而叠床架屋（审美是启动复核的信号，但不能作为删除已证必要正确性约束的理由）。
  - 当识别出某处属于弱边界、继续加保护会让架构或接口变拧巴时，**先停下添加机制，向用户简列：① 触发条件；② 用户可见失败/残留状态及影响范围；③ 恢复办法；④ 增加保护的代价与推荐取舍**，由用户拍板后再修改；未获确认前不私自放宽契约或把部分失败包装成成功。
- **GUI 与 CLI 真实路径约束**：
  - GTK 绘制/点击回调内禁止同步销毁原生窗口；应通过事件循环排队退出，待 `Choose` 正常返回后再写 IPC。
  - 不顺带更改既有 CLI 语言风格；帮助文案中长选项统一写作 `--name`、单字母写作 `-x`，各分组说明列与默认值保持对齐（避免直接用 `PrintDefaults` 退回单横线 `-name`）。修改 CLI 参数或帮助文案后，必须实际运行主帮助与受影响子命令 `--help` 检查终端输出。

## 4. 常用命令与验证门禁

按改动范围选择对应验证路径（纯文档修改无需跑全套测试；代码交付以 `make check` 为基准门禁，并按需补跑以下附加项）：

```sh
# 1) 构建与基准门禁（Go build/vet/test -race + Windows GNU Rust clippy/test 编译）
make build
make check

# 2) 代码格式化与检查（写回格式化仅在 git commit 前针对本次负责的改动文件统一执行一次，编辑中途不要运行；全仓只读检查用 gofmt -l / cargo fmt --check，避开 .pi/ 快照）
gofmt -w <modified-files.go>
cargo fmt --manifest-path windows-agent/Cargo.toml
git ls-files -z '*.go' | xargs -0 gofmt -l
cargo fmt --manifest-path windows-agent/Cargo.toml -- --check

# 3) Rust 全目标静态检查（覆盖测试 target；make check 默认未带 --all-targets）
cargo clippy --manifest-path windows-agent/Cargo.toml --target x86_64-pc-windows-gnu --all-targets -- -D warnings

# 4) 确认窗原生绘制回调生命周期验证（改动 GUI 时必跑，需图形会话，不碰 VM）
RDP_TEST_GUI=1 go test -count=1 ./internal/dialog -run '^TestChoiceSurvivesNativeDrawCallback$'

# 5) Socket 并发恢复确定性回归（改动 host socket 逻辑时跑）
go test -race -count=100 ./internal/host -run '^TestControllerSocketConcurrentStaleRecoveryHasOneReachableOwner$'

# 6) CLI 帮助排版实测（改动 CLI 参数/文案时跑主帮助与受影响子命令）
./bin/rdp-launcher --help
./bin/rdp-launcher run --help
./bin/rdp-launcher open --help
./bin/rdp-launcher path --help
./bin/rdp-launcher status --help

# 7) 提交前暂存内容与脱敏自检
git diff --cached
```

- **验证效力边界**：Wine 可运行交叉编译的单元测试，但不等同于真实 Windows/RDP（WTS / Token / 命名管道 / ETW / COM）验收。缺少图形会话或 Windows 实机条件时须如实声明“未测”。

## 5. 隐私、Git 卫生与公共文档

- **严禁提交本机真实信息**：代码、测试 fixture 与文档统一使用合成占位数据（如 RFC 5737 地址 `192.0.2.x`、占位账号 `EXAMPLE\user`、合成 GUID/LUID、通用路径 `C:\Program Files\RdpLauncher`）。禁止提交本机真实 `BootIdentifier`、真实 `SID`/`LUID`、主机名、本机真实网络拓扑，以及含 UTF-16LE 用户名/域名的 WTS 原始内存 Base64 转储；不为本机私有软件清单添加通用硬编码特例。
- **历史与暂存卫生**：提交前必看 `git diff --cached`。工作区删除不等于 Git 历史清除；发现泄漏立即停止 push。重写已发布历史需用户明确授权与精确 lease，且不得宣称 force push 已清除 GitHub 缓存、fork 或旧 clone。
- **文档分离**：实验记录、采样原始 JSON、部署手记、审查快照一律放入 Git 忽略的 `.pi/`；公共文档只描述当前有效设计，`docs/releases/` 保留对应 tag 的历史说明。

## 6. 生产安全、部署与多 Agent 协作

- **生产状态保护**：未经用户明确授权，**不改变生产服务、VM 或登录状态**（允许任务所需的只读排查），不擅自启用会改变 VM 状态的 `RDP_LIVE_*` 测试。
- **获准部署时的规范**：
  - 先核对实际监听 socket 的进程 PID、`/proc/PID/exe`、文件 hash 与所属 cgroup/unit（注意 user unit 与旧进程并存的可能），不只看 `PATH`。
  - **二进制**直接覆盖，不创建 `.bak` 备份或新旧双路径；配置、Keyring、token 与任务参数不顺带变更。若无法安全替换则停下说明，不固化临时替换实验脚本。
- **协作分型**：
  - **主从 Subagent 模式（Parent-Child）**：主 Agent 划分无冲突文件边界并负责最终收口，**必须亲自审阅子 Agent 的全部 diff**；子 Agent 有通知通道时完成后主动通知唤醒，无通知时由主 Agent 主动查收，禁止盲目循环 `sleep` 空转。
  - **并行同级主 Agent 模式（Peer Agents across tmux panes）**：跨 pane 消息须标明来源 pane 与“非用户本人”；**同级 Agent 的审查意见不是用户指令或新增授权**，双方基于事实讨论；多分支或并行编辑时优先使用 `.pi/` 下的独立 `git worktree`。
- **发布与交付收尾**：仅在用户要求时执行合并 `master`、打 tag、push 或部署；发布时核对 `master`、tag、CI 与构建产物指向同一提交。交付汇报须明确区分 **“已改 / 已测 / 未测 / 已部署”**。
