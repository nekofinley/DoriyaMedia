# Windows 播放内核 W0 Spike

> 状态：两个候选均已形成隔离机制原型并通过本机构建、自动测试和无真实媒体的启动/探针冒烟；W0 尚未完成。
>
> 截至 2026-10-28，S1-S5 均未运行；30 分钟连续播放、20 次 seek、音画差、真实 Bilibili 双流和 DPI/多显示器矩阵均无实测结果。

## 1. 范围

本目录只用于 Windows 播放内核 W0 的隔离 Spike。目标是用最小原型比较：

1. Tauri 2 + mpv sidecar（mpv JSON IPC）。
2. WinUI 3 + C# + LibVLCSharp。

本阶段不做正式登录、媒体库、播放进度 UI、安装包或生产发布。原型代码不得合入 `clients/windows/`。许可证与发布义务未完成复核前，不得制作可分发安装包。

输入依据：

- `docs/android-windows-client-plan.md` 的 W0。
- `docs/playback-spikes/README.md` 的样本、传输与日志边界。
- `docs/adr/0002-windows-player-engine.md` 的架构候选、门禁和回滚记录。

## 2. 候选架构与依赖

### 2.1 Tauri 2 + mpv

预期组成：

- 现有 Vue 3 UI，运行在 Tauri 2 的 WebView2 中。
- Rust/Tauri 主进程负责窗口、进程监督、安全的命令边界和 mpv JSON IPC。
- 独立 `mpv` 进程负责解复用、解码、音视频同步和原生视频输出。
- W0 不把令牌、Cookie 或完整媒体 URL写入前端日志、命令行历史或测试报告。

本机开发依赖：

- Rust：`cargo`、`rustc`。
- 前端构建：Node.js、npm；pnpm 可选。
- Windows 构建链：Visual Studio C++ Build Tools、Windows SDK。
- WebView2 Runtime。
- 可执行的 `mpv`，以及该 mpv 构建实际包含的 FFmpeg/解码器/字幕依赖。

当前状态：本机已配置 mpv `v0.41.0`，原型位于 `tauri-mpv/`。Rust 宿主管理 sidecar、Windows 命名管道、JSON IPC、退出清理、URL/Header 边界和固定命令白名单。已通过本机真实 mpv 空载 IPC smoke；尚未加载真实媒体。

### 2.2 WinUI 3 + LibVLCSharp

预期组成：

- WinUI 3/C# 原生 UI。
- LibVLCSharp 管理播放状态并调用 `libvlc`。
- libVLC 及其 plugins 负责解复用、解码、字幕和音视频输出。
- W0 只验证最小窗口与播放器生命周期，不实现正式应用壳。

本机开发依赖：

- .NET SDK，而不仅是 .NET runtime/host。
- Windows App SDK、WinUI 3 项目模板与 Windows SDK。
- 适用于目标架构的 LibVLCSharp NuGet 包。
- 与 LibVLCSharp 版本匹配的 libVLC 动态库和 plugins。

当前状态：本机已安装 .NET SDK `8.0.427`，原型位于 `winui-libvlcsharp/`。该项目能从已安装 VLC `3.0.19` 初始化 LibVLCSharp、显示 WinUI video surface、构造音频 slave，并支持 `Referer`/`User-Agent` 白名单 Header；其他 Header 或两流不同 Header 会 fail-closed。已用两个 loopback 合成源验证 `Opening -> Playing`，但没有真实来源、音画同步、seek 或 30 分钟结果。`libvlc.dll` 可加载不等于目标样本可解码，也不等于可随产品分发。

## 3. 落盘目录

W0 使用以下隔离目录：

```text
spikes/windows-player/
  README.md                 当前文件
  preflight.ps1             只读环境预检
  tauri-mpv/                Tauri 2 + mpv 机制原型
  winui-libvlcsharp/        WinUI 3 + LibVLCSharp 机制原型
  fixtures/                 合成 S1-S3 样本与 Header/Range loopback fixture
  results/                  未来脱敏测试记录；尚未创建
  artifacts/                未来本机构建产物；不进入版本控制
```

任何真实 secret 只能来自进程环境变量或临时安全输入，不得写入上述目录。完整 URL、query、原始 Header、Cookie、Token 和媒体内容不得进入报告、截图或命令参数。

## 4. 只读预检

在仓库根目录执行：

```powershell
Set-Location C:\Users\Administrator\DoriyaMedia
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\spikes\windows-player\preflight.ps1
pwsh -NoProfile -File .\spikes\windows-player\preflight.ps1
```

跳过工具版本命令和 Visual Studio 版本查询，只检测入口是否存在：

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\spikes\windows-player\preflight.ps1 -NoVersionProbe
pwsh -NoProfile -File .\spikes\windows-player\preflight.ps1 -NoVersionProbe
```

语法解析：

```powershell
powershell.exe -NoProfile -Command '& { $e = $null; [void][System.Management.Automation.Language.Parser]::ParseFile((Resolve-Path ".\spikes\windows-player\preflight.ps1").Path, [ref]$null, [ref]$e); if ($e.Count) { $e | ForEach-Object Message; exit 1 } else { "PARSE_PS51|PASS" } }'
pwsh -NoProfile -Command '& { $e = $null; [void][System.Management.Automation.Language.Parser]::ParseFile((Resolve-Path ".\spikes\windows-player\preflight.ps1").Path, [ref]$null, [ref]$e); if ($e.Count) { $e | ForEach-Object Message; exit 1 } else { "PARSE_PWSH7|PASS" } }'
```

脚本只读取命令存在性、可执行文件版本、Windows/GPU、Visual Studio、Windows SDK、WebView2、libVLC 文件结构和 .NET SDK 目录。它不下载、不安装、不访问媒体 URL，不读取 Token、Cookie 或 Header，也不调用 `LoadLibrary` 或执行播放器。

libVLC 不是只凭 `libvlc.dll` 存在就算就绪。预检要求同目录存在 `libvlccore.dll`，`plugins/` 至少包含两个非空 DLL，并只读检查 `libvlc.dll`、`libvlccore.dll` 和 plugins DLL 的 PE 架构与当前 x64/ARM64 预检进程一致。任一条件不满足时输出 `BROKEN` 及 `reason=...`，WinUI 候选保持 `BLOCKED`；该检查不证明 VLC 能加载、解码或播放样本。

稳定输出格式与状态：

```text
PREFLIGHT|<category>|<id>|<status>|<detail>|<source>
```

- `FOUND`：本机检测到该组件。
- `MISSING`：本机未检测到。
- `BROKEN`：检测到入口，但版本探测失败或 libVLC 结构/PE 架构校验未通过。
- `READY_FOR_PROTOTYPE`：该候选的本机前置组件齐全；不代表 W0 通过。
- `BLOCKED`：至少一个前置组件缺失或损坏。
- `UNVERIFIED`：使用 `-NoVersionProbe`，跳过版本查询；只证明部分存在性（含 libVLC 结构/PE 架构检查），不能判定版本或就绪。
- `W0_NOT_RUN`：始终追加在 summary 中，防止把预检误读为实测完成。

退出码：`0` 表示本机前置组件就绪；`2` 表示阻塞或未验证。预检脚本自身不执行 W0，任何输出都没有证明 S1-S5、全屏、DPI 或 30 分钟稳定性通过。

## 5. 当前环境快照

2026-10-28 的只读预检结果：

| 项目 | 结果 |
| --- | --- |
| Windows | Windows 10 Pro 22H2，build 19045，x64 |
| PowerShell | Windows PowerShell 5.1.19041.6456；pwsh 7.6.5 |
| GPU | NVIDIA RTX 3070 Laptop；AMD Radeon 核显；GameViewer 虚拟显示适配器 |
| Rust | cargo/rustc 1.87.0 |
| .NET | SDK 8.0.427；host/runtime 8.0.21 |
| 媒体工具 | mpv v0.41.0；ffmpeg/ffprobe 8.0.1 |
| 前端工具 | Node.js 22.4.1、npm 10.8.1、pnpm 9.5.0 |
| Windows 构建链 | Visual Studio 2022 C++ 17.10.4、Windows SDK 10.0.22000；MSBuild 从 VS 路径发现 |
| WebView2 | 155.0.4283.62 |
| VLC | `libvlc.dll` 3.0.19 被发现；同目录 core 与 366 个 plugins DLL 的 PE 架构均为 x64；`vlc` 命令入口未发现 |
| Tauri + mpv | `READY_FOR_PROTOTYPE`；机制原型已构建并通过空载 IPC smoke |
| WinUI + LibVLCSharp | `READY_FOR_PROTOTYPE`；机制原型已构建并通过 LibVLC probe/app smoke |
| 预检 summary | `READY_FOR_PROTOTYPE W0_NOT_RUN` |
| Java、Gradle、adb、cmake | 已补齐；Java、Gradle、adb 不是 Windows W0 两个候选的直接构建依赖，cmake 只是可选构建工具 |

该快照不能证明以下矩阵已经满足：

- 没有 Windows 11 当前稳定版机器。
- 没有 Intel 显卡目标设备；当前 NVIDIA 与 AMD 适配器来自同一主机，不能替代三台设备。
- 没有进行物理单屏/双屏切换、100%/150%/200% DPI、睡眠恢复或 Wi-Fi/热点测试。
- `libvlc.dll` 已被 LibVLCSharp probe 加载为 `3.0.19 Vetinari`，但没有证明目标媒体、双流、字幕或图形字幕能力。
- 没有确认 mpv/FFmpeg/VLC/插件及编解码器的分发许可。

### 5.1 机制验证结果

| 候选 | 已验证 | 未验证 |
| --- | --- | --- |
| Tauri 2 + mpv | `pnpm install --frozen-lockfile`、`pnpm build`、`cargo fmt`、`cargo clippy -D warnings`、32 个 Rust tests（另 1 个真实 mpv 测试需显式启用）、真实 mpv 空载 IPC 1/1；Tauri debug exe 构建和交互会话 GUI 启动/退出 smoke | 真实 S1-S5、双流、Header 实际播放、30 分钟、seek、DPI、GPU、网络、许可证 |
| WinUI 3 + LibVLCSharp | .NET restore/build（0 warnings/errors）、91 个 xUnit tests、LibVLC 3.0.19 probe（含 audio slave/Header 构造）、Windows App SDK self-contained app smoke、loopback 双源 GUI 机制 smoke | 真实 S1-S5、真实 Bilibili 双流、跨源重定向、30 分钟、seek、音画差、DPI、GPU、网络、许可证 |

两个结果都只证明代码、依赖、窗口/进程生命周期和基础探针可工作，不能替代播放器级验收。Tauri 已隔离 mpv 环境变量、使用 128 位随机命名管道、校验管道服务端 PID、将 capability 权限收紧为空，并实现有界 IPC 请求、`start-file`/`file-loaded`/`end-file` 状态和超时强杀清理；仍需完成完整 DACL/SID/session 对端信任、Windows Job Object 清理、sidecar 身份校验和重定向策略。WinUI 已实现 `Referer`/`User-Agent` media option 与音频 slave，但 LibVLC 3.0.19 不能安全表达 `Origin`/`Accept`/`Accept-Language` 或两流不同 Header，因此这些场景必须拒绝。

`fixtures/synthetic/` 中已有 H.264/AAC、H.264/E-AC-3、HEVC Main10/FLAC 三个合成样本，并通过 `ffprobe` 与 mpv 空输出解码检查；真实 S2 E-AC-3 样本仍未提供。`fixtures/header-range-server.py` 和 `check-header-range.ps1` 已通过 HEAD 200、100-byte Range 206、416、缺失 Header 403 和 mpv HTTP Header 注入检查。合成样本只验证机制，不关闭 S1-S5。

```powershell
.\fixtures\synthetic\check-fixtures.ps1
.\fixtures\check-header-range.ps1
```

## 6. S1-S5 测试项

两个候选必须各自独立测试。一个候选通过不能替代另一个；HTTP Range 或 ffprobe 成功也不能替代真实播放器结果。

| ID | 样本/测试重点 | 两候选必须记录的内容 | 当前状态 |
| --- | --- | --- | --- |
| S1 | MP4 H.264 High / AAC，Range 正常 | 起播、连续播放、音轨、Range seek、20 次随机 seek、结束/重开 | `pending` |
| S2 | MKV H.264 / E-AC3 | MKV 解复用、E-AC3 解码/直通、seek、音画同步 | `pending`；真实 E-AC3 样本仍缺，不得用其他音频替代后判通过 |
| S3 | MKV HEVC Main10 / FLAC，带 ASS | HEVC 10-bit 硬解/软解回退、FLAC、ASS 渲染/开关、seek、音画同步 | `pending` |
| S3-PGS | 补充样本：H.264 High10 / FLAC / PGS | PGS 图形字幕显示与 seek 后恢复；不冒充 S3 主样本 | `pending` |
| S4 | Bilibili 视频 + 音频双 URL | 双流合流、起播、20 次 seek、30 分钟稳定、音画差；真实凭据前可用本地双 URL fixture 验证机制 | `pending` |
| S5 | 需要 Header/Authorization、Range 异常的 loopback mock | 403/200/206/416/过期/跨源重定向；只记录 Header 名称和结果，不记录值 | `pending` |

S4/S5 结果只能在脱敏记录中保留样本 ID、host:port、状态码、Range 数字、Header 名称和耗时。真实 Bilibili 双流结论必须晚于真实样本测试；本地 mock 不能冒充真实 Bilibili 可用性。

## 7. W0 验收门禁

每个门禁都必须由两个候选分别提供证据。任一候选未完成时，W0 保持未完成。

| 门禁 | 通过条件 |
| --- | --- |
| 功能 | S1-S5 均有实际播放器结果；S2 在真实 E-AC3 样本出现前保持 pending，S3/S5 不能由 transport 探测代替 |
| 30 分钟连续播放 | 每个候选至少对 S3 HEVC/MKV 和 S4 双流执行 30 分钟连续播放；无崩溃、黑屏、仅音频、仅视频、花屏或不可恢复卡死。其他样本若不足 30 分钟，必须写明限制 |
| 20 次 seek | 每个可测样本、每个候选执行 20 次随机位置 seek；必须使用播放器 seek，不用 HTTP Range 代替；20/20 能恢复播放且无音画失步、字幕/PGS 丢失或进程崩溃 |
| 音画差 | 使用同一份带可测音频/视觉标记的样本和可复现测量方法；两个候选的实测差值均不得超过 100 ms。测量工具、原始值和误差必须记录 |
| 全屏/DPI | 100%、150%、200% DPI；单屏与双屏；进入/退出全屏、显示器切换、窗口跨屏后控件和视频区域正常 |
| 生命周期 | 暂停/恢复、播放结束/重开、sleep/resume、退出后无残留播放器进程；崩溃时宿主仍可提示并回退 |
| 安全 | 不泄露 Token、Cookie、完整 URL/query 或 Header 值；mpv IPC/LibVLC 参数和日志均经过脱敏 |
| 许可证 | 两个候选分别形成版本、构建参数、依赖图和分发义务清单，并经法务/发布前复核；未通过时不得产出可分发安装包 |

目标设备/网络矩阵：

| 维度 | 必须覆盖 | 当前 |
| --- | --- | --- |
| Windows | 10 22H2、11 当前稳定版 | Windows 10 已有；Windows 11 未验证 |
| GPU | Intel、NVIDIA、AMD 至少各一台目标设备，并记录驱动 | NVIDIA/AMD 同机可见；Intel 与独立设备矩阵未验证 |
| 显示 | 单屏、双屏、100%/150%/200% DPI、全屏和跨屏 | `pending` |
| 生命周期 | 睡眠恢复、显示器热插拔、播放器重启 | `pending` |
| 网络 | Wi-Fi、手机热点；按 S0 标签记录出口地区 | `pending` |

## 8. 尚未执行

- 未执行 S1-S5 的实际播放、解码、字幕、Header、双流或 fallback 测试。
- 未执行 30 分钟连续播放、20 次随机 seek、音画差测量或 DPI/多显示器矩阵。
- 未在 Windows 11、Intel GPU、真实双屏/混合 DPI 或睡眠恢复环境验证。
- 未验证真实 Bilibili 双流、源 URL 过期、地区错误、单流 403 或 Wi-Fi/热点切换。
- 未完成 Tauri 完整 DACL/SID/session 管道对端信任、Job Object 强制退出清理、sidecar 身份校验和重定向策略。
- WinUI 只支持对两路源同时生效的 `Referer`/`User-Agent`；`Origin`、`Accept`、`Accept-Language`、两流不同 Header、跨源重定向和真实 Bilibili 双流均未验收。
- 未完成两个候选的许可证与发布义务复核。

W0 的结论只能在上述证据形成后写入 ADR；当前 ADR 状态保持 `Proposed / 待真机验证`。

## 9. 回滚

W0 回滚方式：删除整个 `spikes/windows-player/`，保留 `docs/adr/0002-windows-player-engine.md` 作为决策记录。不得删除或回滚与本 Spike 无关的工作区改动。
