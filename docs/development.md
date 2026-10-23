# 开发与测试

## 本地检查

```sh
make build
make check
```

`make build` 生成 `bin/rdp-launcher`，使用 Go 1.27.3。可用 `GO` 和 `GO_GOTOOLCHAIN` 覆盖本地工具链设置。

`make check` 包含：

- Go 全模块 build、vet 和 race 测试，包括内嵌 GUI。
- Windows GNU 目标的 Rust clippy 与测试可执行文件构建。

Linux 交叉构建不会执行 Windows 测试。在 Windows 上可运行：

```powershell
cargo test --manifest-path windows-agent/Cargo.toml --locked
```

GitHub Actions 在 Linux 检查、打包 CLI，在 Windows 原生检查和构建 Agent。CI 不使用生产 token、连接生产 VM 或自动部署。Wine 可补充运行纯单元测试，但不能替代 Windows/RDP 验收。

## 确定性回归

控制器测试使用明确时间和事件序列，不等待真实空闲超时。测试应覆盖：

- FIFO、批次部分完成、取消和确认过期。
- 来源失效与晚到结果、VM 接受与状态确认。
- 当前登录引用、连接基线、跨模式确认和用户上下文。
- lock、空闲计划失效、内存压力与安全收尾。

socket 所有权可重复检查：

```sh
go test -race -count=100 ./internal/host \
  -run '^TestControllerSocketConcurrentStaleRecoveryHasOneReachableOwner$'
```

## 实机测试

**警告：实机测试可能改变 VM、内存设备或登录状态。** 只在专用环境中运行，先检查测试源码的目标、权限和副作用。

未设置实机测试环境变量时，实机测试默认跳过：

- `RDP_LIVE_BACKGROUND`：已运行的测试后台端点。
- `RDP_LIVE_TOKEN_FILE`：宿主上的受限测试 token 文件，不把 token 值放入 argv。
- `RDP_LIVE_LIBVIRT_SOCKET`：测试 libvirt socket，可选。
- `RDP_LIVE_MEMORY=1`：显式允许内存测试。

部分测试固定使用 `win11` domain 或 SSH 别名。运行前应检查并匹配专用测试环境。不要对全部 `TestLive` 直接启用环境变量后运行。

仅后台测试示例：

```sh
RDP_LIVE_BACKGROUND=192.0.2.10:49153 \
RDP_LIVE_TOKEN_FILE=/secure/path/test-token \
  go test -v ./internal/adapter -run '^TestLiveBackgroundOnlySession$'
```

该测试要求 VM 和后台已经运行，不启动 FreeRDP。`TestLiveBackgroundWatchQuiet` 还会建立 SSH 连接验证变化通知。SSH 隧道本身也会使后台报告 SSH 非零，从而阻止空闲注销与挂起；不能把这种阻止记作自动策略失败。

Windows 辅助脚本位于 `windows-agent/tests/`：

- `token-acl.ps1`：临时文件和任务中的 token 权限验证。
- `connecttime-probe.ps1`：只读 x64 SYSTEM WTS 采样；保留原始字节和十进制时间字符串，支持停止标记文件。
- `ipv6-probe.ps1`：IPv6 观测辅助探针。

完整验收还应检查断开与重连、当前桌面执行、目标身份失效、注销超时/非零退出/断线，以及冷启动、Fast Startup 和休眠。自动化检查通过不代表这些场景均已通过。

## 文档与测试数据

Public 文档描述当前功能、接口、操作步骤和可复现的限制。发布说明描述对应 tag，不作为当前版本用法。

本机部署记录、审查对话、临时实验日志和原始用户会话数据放在 Git 忽略的 `.pi/`，不进入 `docs/`。测试示例使用占位账号和文档专用 IP；需要提交 fixture 时使用合成数据，不提交真实 SID、登录引用或凭据。
