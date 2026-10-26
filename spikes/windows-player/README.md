# Windows 播放内核 W0 可行性预检

> 状态：仅完成环境与可行性文档预检，W0 尚未完成。
>
> 截至 2026-10-27，两个候选原型均未创建或构建，S1-S5 均未运行；30 分钟连续播放、20 次 seek、音画差和 DPI 矩阵均无实测结果。

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

当前阻断项：`mpv` 不在 `PATH`，未在本机标准位置发现可执行文件。只看到 `ffmpeg` 不代表已有 mpv，也不能把系统 ffmpeg 的存在当作 mpv 的解码器或发布许可已经确认。

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

当前阻断项：`dotnet` host 存在，但 `.NET SDK` 缺失。`C:\Program Files\VideoLAN\VLC\libvlc.dll` 仅说明本机有可检测到的 VLC 运行时文件；`vlc` 命令入口未发现，且该 DLL 尚未证明能被 LibVLCSharp 加载、解码目标样本，也不能据此认定可随产品分发。

## 3. 落盘目录

W0 开始后使用以下隔离目录：

```text
spikes/windows-player/
  README.md                 当前文件
  preflight.ps1             只读环境预检
  tauri-mpv/                未来原型；尚未创建
  winui-libvlcsharp/        未来原型；尚未创建
  fixtures/                 未来本地测试素材/脱敏 fixture；尚未创建
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

退出码：`0` 表示本机前置组件就绪但未执行 W0；`2` 表示阻塞或未验证。任何输出都没有证明 S1-S5、全屏、DPI 或 30 分钟稳定性通过。

## 5. 当前环境快照

2026-10-27 的只读预检结果：

| 项目 | 结果 |
| --- | --- |
| Windows | Windows 10 Pro 22H2，build 19045，x64 |
| PowerShell | Windows PowerShell 5.1.19041.6456；pwsh 7.6.5 |
| GPU | NVIDIA RTX 3070 Laptop；AMD Radeon 核显；GameViewer 虚拟显示适配器 |
| Rust | cargo/rustc 1.87.0 |
| .NET | host/runtime 8.0.21；SDK 目录不存在 |
| 媒体工具 | ffmpeg/ffprobe 8.0.1 |
| 前端工具 | Node.js 22.4.1、npm 10.8.1、pnpm 9.5.0 |
| Windows 构建链 | Visual Studio 2022 C++ 17.10.4、Windows SDK 10.0.22000；MSBuild 从 VS 路径发现 |
| WebView2 | 155.0.4283.62 |
| VLC | `libvlc.dll` 3.0.19 被发现；同目录 core 与 366 个 plugins DLL 的 PE 架构均为 x64；`vlc` 命令入口未发现 |
| Tauri + mpv | `BLOCKED`：缺 `mpv` |
| WinUI + LibVLCSharp | `BLOCKED`：缺 `.NET SDK` |
| 预检 summary | `BLOCKED missing=dotnet-sdk,mpv W0_NOT_RUN` |
| Java、Gradle、adb、cmake | 未检测到；Java、Gradle、adb 不是 Windows W0 两个候选的直接构建依赖，cmake 只是可选构建工具 |

该快照不能证明以下矩阵已经满足：

- 没有 Windows 11 当前稳定版机器。
- 没有 Intel 显卡目标设备；当前 NVIDIA 与 AMD 适配器来自同一主机，不能替代三台设备。
- 没有进行物理单屏/双屏切换、100%/150%/200% DPI、睡眠恢复或 Wi-Fi/热点测试。
- 没有证明现有 `ffmpeg` 或 `libvlc.dll` 可被任一原型加载，也没有确认其分发许可。

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

- 未创建 Tauri 或 WinUI 原型工程。
- 未安装 `mpv` 或 `.NET SDK`，也未安装任何 NuGet、crate 或大型依赖。
- 未运行 `cargo build/test`、`dotnet build/test` 或任何不存在的项目命令。
- 未执行 S1-S5 的实际播放、解码、字幕、Header、双流或 fallback 测试。
- 未执行 30 分钟连续播放、20 次随机 seek、音画差测量或 DPI/多显示器矩阵。
- 未完成两个候选的许可证与发布义务复核。

W0 的结论只能在上述证据形成后写入 ADR；当前 ADR 状态保持 `Proposed / 待真机验证`。

## 9. 回滚

W0 回滚方式：删除整个 `spikes/windows-player/`，保留 `docs/adr/0002-windows-player-engine.md` 作为决策记录。不得删除或回滚与本 Spike 无关的工作区改动。
