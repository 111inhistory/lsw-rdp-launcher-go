# RDP Launcher Go

Linux 上的 Windows VM 启动器。Go 控制器通过 libvirt 管理 VM，用 FreeRDP 连接桌面或 RemoteApp。Rust Windows Agent 在指定用户的登录中启动应用、打开文件和查询映射盘。

一个控制器管理一个 VM 和一个自持 FreeRDP 连接。多个 CLI 共用业务队列；状态查询不会唤醒 VM。

## 依赖与构建

- Linux：libvirt、Niri、FreeRDP、`secret-tool`。
- 应用入口同步：`desktop-file-utils`。
- 构建：Go 1.27.3；Windows Agent 另需 Rust 工具链。

```sh
make build
pkexec install -m 755 bin/rdp-launcher /usr/local/bin/rdp-launcher
```

产物是单一 CLI，确认窗已内嵌，无需独立 dialog 程序。`make build` 和 `make check` 显式选择项目工具链，不替换系统 Go。在精确 release tag 上构建时，CLI 显示该 tag；其他构建显示 `dev` 和提交号。

系统级安装二进制不代表以 root 运行控制器。控制器需要桌面用户的 Keyring、Wayland 和 Niri 环境，应以该用户身份运行。

## 初始配置

1. 按 [Windows Agent 说明](windows-agent/README.md) 安装后台和 RemoteApp 程序。
2. 创建宿主配置，并修改 VM、Windows 用户、IP 和程序路径：

   ```sh
   mkdir -p ~/.config/rdp-launcher-go
   cp -n config.example.toml ~/.config/rdp-launcher-go/config.toml
   ```

3. 设置 RDP 密码和双方共享的 Agent token：

   ```sh
   rdp-launcher auth set
   rdp-launcher auth agent set
   ```

4. 启动控制器：

   ```sh
   rdp-launcher start
   rdp-launcher status
   ```

`start` 通过用户级 systemd 启动服务，不要求终端常驻，也不会重启已有控制器。需要随图形会话启动时，可使用 [服务单元](contrib/rdp-launcher.service)。Niri 会话应在控制器启动前完成环境导入。`serve` 是前台调试入口，`watcher` 是其别名。

配置项见 [config.example.toml](config.example.toml)。RDP 密码和 token 保存在 Keyring，不写入 TOML。RDP 密码通过 stdin 传给 FreeRDP，不进入 argv。

**警告：不要将 Windows Agent 端口暴露到不可信网络。** 共享 token 不提供传输加密。非 loopback 监听要求 token 文件；文件权限和来源限制见 [Agent 说明](windows-agent/README.md)。

## 常用命令

```sh
rdp-launcher desktop                     # 完整桌面。
rdp-launcher remoteapp                   # RemoteApp 连接。
rdp-launcher run WINWORD.EXE
rdp-launcher open '/data/shared/report.docx'
rdp-launcher open --app WINWORD.EXE 'J:\report.docx'
rdp-launcher apps                        # 实时应用目录。
rdp-launcher mounts                      # 当前绑定登录的映射盘。
rdp-launcher sync-apps                   # 同步应用、图标和桌面入口。
rdp-launcher list-apps                   # 本地应用缓存，不唤醒 VM。
rdp-launcher install-shortcuts           # 从本地配置和缓存重建入口。
rdp-launcher path '/data/shared/report.docx'
rdp-launcher ping                        # 仅后台请求，不启动 FreeRDP。
rdp-launcher status                      # 本地可读状态，不唤醒 VM。
rdp-launcher lock
rdp-launcher unlock
rdp-launcher shutdown                    # 停止控制器，不注销 Windows 登录。
```

全局参数 `--config`、`--socket`、`--timeout`、`--on-conflict` 放在命令前。子命令参数放在文件或应用名前。`--timeout 0` 关闭客户端总时限。`version` 显示 CLI 版本，不读取配置或连接控制器。`status` 不提供 JSON 输出。

### 桌面与 RemoteApp

`apps`、`sync-apps`、`mounts` 只要求用户上下文，不指定显示模式。有连接时，控制器在当前绑定的登录中执行这些请求，不弹确认或切换模式；无连接时，控制器先建立默认 RemoteApp 连接。控制器不会挑选一个保留的断开登录来执行查询。

`run`、`open` 默认偏向 RemoteApp。在桌面模式中执行时，控制器等待明确选择，即使桌面没有应用窗口。可用动作如下：

| 动作 | 行为 |
|---|---|
| `current` | 在当前已绑定桌面执行，不断开连接。 |
| `keep` | 断开旧连接，保留登录，再连接目标模式。 |
| `logoff` | 注销明确识别的旧登录，确认结束后再连接。 |
| `cancel` | 取消请求，保留原连接。 |

`current` 仅在当前桌面可执行该业务时提供。跨模式切换不保证沿用同一个 Windows 登录。窗口或托盘计数为零不授权注销。

Wayland 下使用内嵌确认窗，无图形但有 TTY 时使用终端选择。无 Wayland 且无 TTY 时，应显式指定动作，例如：

```sh
rdp-launcher --on-conflict=current open 'J:\report.docx'
```

确认最多等待 60 秒，并受请求总时限约束。取消、超时或目标登录改变时，控制器不会继续旧转场。多文件打开共用一次确认，已执行部分不会重放。

确认窗跟随系统主题；DMS 运行时读取其主题设置和配色。Niri 窗口规则见 [contrib/niri-rdp-launcher.kdl](contrib/niri-rdp-launcher.kdl)。

### 空闲与 lock

| 配置 | 默认值 | 含义 |
|---|---|---|
| `idle_disconnect_timeout` | 30 秒 | 无 RemoteApp 窗口后断开自持连接；0 禁用。桌面模式不自动断开。 |
| `idle_logoff_timeout` | -1 | 显示断开后注销最后绑定的登录；-1 禁用，0 立即尝试。 |
| `vm_suspend_timeout` | 300 秒 | 显示断开且空闲条件成立后挂起 VM；0 禁用。 |

**警告：启用空闲注销会结束目标登录中的应用。** 保存工作后再启用。挂起启用时，注销超时不能大于挂起超时。控制器只注销最后绑定、已确认断开的登录，不扫除同名用户的所有登录。

SSH 非零或未知、后台失效、lock 和新请求会中止空闲计划。内存压力或扩容只阻止缩容和挂起，不重置注销计时。启用注销时，内存回收在确认注销后开始；禁用时，保留挂起前回收。详见 [内存控制](docs/memory.md)。

`lock` 禁止自动断开、注销、缩容和挂起，但不禁止显式用户请求或无需暂停的保护性扩容。lock 只存在于服务内存；VM 关闭或服务重启后清除。

### 应用目录与路径

- `[mounts]` 只声明现有 Windows 盘符与宿主目录的对应关系，不创建共享或映射盘。
- `[[apps]]` 可添加未被扫描到的应用。`id`、`name`、`target` 必填；可设置 `arguments`、`working_dir`、`icon`。无需先同步即可运行。
- `[filters]` 按 ID、名称、目标或快捷方式路径精确排除应用；目录规则使用 Windows 路径并包含子目录。过滤不是执行权限限制。
- `[[overrides]]` 设置 MIME 和分类，不会恢复已排除的应用。
- `list-apps` 合并本地缓存与手动配置；相同 ID 以手动配置为准。图标以 `icon` 输出本地缓存 PNG 路径或原有配置值，不输出 `icon_base64`，也不检查或重建图标文件。
- `sync-apps` 只维护自身 `rdp-launcher-go-` 命名空间的入口和图标，不删除其他应用的文件。

缓存位于 `$XDG_CACHE_HOME/rdp-launcher-go`，桌面入口位于 `$XDG_DATA_HOME/applications`。未设置 XDG 变量时使用用户目录默认值。`install-shortcuts` 不连接 VM，也会重建缓存应用入口。

`[freerdp.env]` 仅影响 FreeRDP 子进程，优先级为继承环境 < `video_driver` < 显式 env。不要在环境变量中存放凭据。`app_id` 应与所用 FreeRDP 客户端在 Niri 中的实际标识一致；未知窗口状态不会被当作空闲。

## 升级

宿主和 Windows Agent 应配套更新。当前 Agent 协议为 2，不保留旧全局交互端点或双版本路由。

- 删除旧配置 `server.agent_port`、`ui.dialog_bin` 和 `lifecycle.reclaim_virtio_mem`。内存策略由 `[memory]` 启用。
- Windows RemoteApp 程序应更新为同一版 Agent；宿主使用其 `remoteapp-shell` 角色。
- 保持 `client_hostname` 稳定。旧版本未显式设置该值时，应核对旧连接实际使用的名称。不要直接假定它等于 Linux 主机名。
- 如果 Windows 保留旧登录，应先保存工作并正常注销，或继续使用旧连接的有效 hostname。不要通过换随机 hostname 或自动注销所有登录来掩盖绑定失败。

## 开发与文档

```sh
make check
```

该命令运行 Go build、vet、race 测试和 Windows Rust 目标的静态检查、测试构建；Linux 交叉构建不会执行 Windows 测试。GitHub Actions 在 Windows runner 原生运行 Rust 测试。CI 不连接生产 VM 或自动部署。

- [架构与会话绑定](docs/architecture.md)
- [Windows Agent 安装与协议](windows-agent/README.md)
- [自动内存控制与恢复](docs/memory.md)
- [virtio-mem 驱动约束](docs/virtio-mem-driver-notes.md)
- [开发与测试](docs/development.md)
- [v0.2.0 发布说明](docs/releases/v0.2.0.md)
- [v0.1.0 历史版本说明](docs/releases/v0.1.0.md)
