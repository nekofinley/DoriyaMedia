# Windows 播放内核 W0 Spike

> 状态：两个候选均已形成隔离机制原型并通过本机构建、自动测试和无真实媒体的启动/探针冒烟；Tauri 通过合成双流 `audio-add`、20 次 seek、PTS 诊断和失败清理，WinUI 通过 headless 双源 slave、20 次位置校验 seek 和有界停止；W0 尚未完成。
>
> 截至 2026-11-04，S1-S5 均未运行；30 分钟连续播放、真实样本 20 次随机 seek、音画差、真实 Bilibili 双流和 DPI/多显示器矩阵均无实测结果。

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

当前状态：本机已安装 .NET SDK `8.0.427`，原型位于 `winui-libvlcsharp/`。该项目能从已安装 VLC `3.0.19` 初始化 LibVLCSharp、显示 WinUI video surface、构造音频 slave，并支持 `Referer`/`User-Agent` 白名单 Header；其他 Header 或两流不同 Header 会 fail-closed。headless 合成双源 probe 已用独立 loopback 视频/音频验证 `Opening -> Playing`、`AddSlave`、双轨保持、20 次位置校验 seek、恢复推进和有界停止；但没有真实来源、真实音画同步、真实来源 seek 或 30 分钟结果。`libvlc.dll` 可加载不等于目标样本可解码，也不等于可随产品分发。

## 3. 落盘目录

W0 使用以下隔离目录：

```text
spikes/windows-player/
  README.md                 当前文件
  preflight.ps1             只读环境预检
  tauri-mpv/                Tauri 2 + mpv 机制原型
  winui-libvlcsharp/        WinUI 3 + LibVLCSharp 机制原型
  fixtures/                 合成 S1-S3 样本与 Header/Range loopback fixture
  results/                  脱敏机制记录；2026-11-02 至 2026-11-04 窗口/双流 probe 已记录
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
| WinUI + LibVLCSharp | `READY_FOR_PROTOTYPE`；机制原型已构建并通过 LibVLC probe、app 和双源 playback probe smoke |
| 预检 summary | `READY_FOR_PROTOTYPE W0_NOT_RUN` |
| Java、Gradle、adb、cmake | 已补齐；Java、Gradle、adb 不是 Windows W0 两个候选的直接构建依赖，cmake 只是可选构建工具 |

该快照不能证明以下矩阵已经满足：

- 没有 Windows 11 当前稳定版机器。
- 没有 Intel 显卡目标设备；当前 NVIDIA 与 AMD 适配器来自同一主机，不能替代三台设备。
- 仅在单屏 1920x1080、100% DPI 下完成 Tauri 嵌入视频的全屏/resize smoke；双屏切换、150%/200%、混合 DPI、睡眠恢复和 Wi-Fi/热点矩阵仍未测试。
- `libvlc.dll` 已被 LibVLCSharp probe 加载为 `3.0.19 Vetinari`，但没有证明目标媒体、双流、字幕或图形字幕能力。
- 没有确认 mpv/FFmpeg/VLC/插件及编解码器的分发许可。

### 5.1 机制验证结果

| 候选 | 已验证 | 未验证 |
| --- | --- | --- |
| Tauri 2 + mpv | `pnpm install --frozen-lockfile`、`pnpm build`、`cargo fmt`、`cargo clippy -D warnings`、97 个 Rust tests（另 6 个需真实 mpv/Python/公网的 opt-in 测试）、真实 mpv 空载 IPC 连续 3/3、Header 保护 loopback MP4 + 20 次 seek 连续 3/3、合成 S2 E-AC-3 与 S3 HEVC10/FLAC/ASS codec 断言 + 各 20 次 seek 连续 3/3、独立 loopback video/audio 双流 `audio-add` + 20 次 seek 连续 3/3、缺失外部音频 fail-closed/清理连续 3/3、公网 HTTPS CONNECT 的 mpv PID + 创建时间绑定探针连续 3/3；Tauri debug exe 构建、surface 原生操作主线程化、播放器阻塞 IPC 移入 blocking pool、专用 Win32 视频子窗口 + mpv `--wid`、当前构建 `7be9a184...` 双流 GUI 自动装载、普通/1920x1080 全屏/640x480/1542x931 resize 几何一致、120/120 UI 响应性，以及正常关闭/强制终止零残留 smoke | 真实 S1-S5、真实双流/音画差、真实 Header 来源、30 分钟、真机 seek、150%/200%/双屏 DPI、GPU、网络、许可证 |
| WinUI 3 + LibVLCSharp | .NET restore/build（0 warnings/errors）、91 个 core xUnit tests、独立 headless 双源 playback probe 1/1（聚焦连续 10/10，验证外部 audio slave、双轨保持、20 次位置校验 seek 和恢复推进）、LibVLC 3.0.19 probe（含 audio slave/Header 构造）、Windows App SDK self-contained app smoke、loopback 双源 GUI 机制 smoke | 真实 S1-S5、真实 Bilibili 双流、真实音画差、跨源重定向、30 分钟、真实来源 seek、DPI、GPU、实际 WinUI 合成、网络、许可证 |

两个结果都只证明代码、依赖、窗口/进程生命周期和基础探针可工作，不能替代播放器级验收。Tauri 已隔离 mpv 环境变量、使用 128 位随机命名管道、校验原始进程句柄的 user SID/session、以绝对路径和 SHA-256 固定 sidecar、支持可选的同一句柄 Authenticode leaf signer SHA-256 pin、通过 `PROC_THREAD_ATTRIBUTE_JOB_LIST` 使用 `KILL_ON_JOB_CLOSE` Job Object，将 capability 权限收紧为空，并实现有界 IPC 请求、`start-file`/`file-loaded`/`end-file` 状态、30 秒 `file-loaded` watchdog 和超时强杀清理；mpv 另启用 TLS 校验、禁用脚本/本地自动加载和 yt-dlp、设置网络超时，并配置 stream/demuxer Lavf 协议白名单与 redirect 上限；初始 HTTPS 预检拒绝私网/本机 IP literal 与 `localhost`。双流请求只接受主视频 + 可选外部音频，两路 URL 分别校验、Header 必须完全一致，严格 relay 模式拒绝跨 origin 音频；控制器在 `loadfile` 前先设置 pause，等待主轨后执行 `audio-add <url> select`，确认外部音轨选中后才恢复播放。严格模式还使用无凭据的 `127.0.0.1` `CONNECT` egress guard：仅允许当前播放的精确 HTTPS CONNECT authority，在 Tauri 进程内按播放会话单次解析并固定 DNS、拒绝公私混合结果、只连接固定数字地址，并通过 `GetExtendedTcpTable(TCP_TABLE_OWNER_PID_ALL)` 匹配客户端侧 ESTABLISHED 行，要求连接 owner PID 和进程创建时间均等于已验证 mpv sidecar；监听器使用 `SO_EXCLUSIVEADDRUSE`，DNS 单飞解析的 CONNECT handler 最多等待 5 秒且不持有 relay 全局状态锁（底层 resolver worker 不可取消），新播放会话和 disarm 会主动关闭已登记隧道；每个播放最多接受 512 个已通过 origin 校验的连接，新播放重置预算，并另有 16 个并发连接上限。隧道建立后不检查 TLS SNI、HTTP Host、path/query、method、Header 或响应内容；DNS 不在会话内重解析，严格 DNS/PID/relay 失败也不直连回退。重要残留风险：真实 HTTPS 要求 Lavf 白名单保留 `tcp`，而 `tcp://` 不使用 HTTP proxy，恶意嵌套 playlist 仍可能绕过 CONNECT guard 直接建连，因此 W0 不是进程级 egress fail-closed；正式安全门禁前需要 WFP、AppContainer、进程级防火墙或等价网络层。新增 opt-in 集成测试已让真实 mpv 经完整 Tauri 控制器加载 Header 保护的 loopback 合成 MP4，暂停后执行 20 次相对 seek 并恢复播放，连续 3/3；S2/S3 合成 MKV 另断言 E-AC-3、HEVC Main10、FLAC、`yuv420p10` 和嵌入 ASS 轨道选择，并各执行 20 次位置校验 seek（等待 seek 收敛、验证 `time-pos`，至少 12 个不同落点），连续 3/3；双流测试用两个独立 loopback origin 验证视频轨 + 外部 AAC 轨均选中、20 次 seek 后保持，并记录 0.027-0.031 秒的合成 PTS 诊断样本，连续 3/3；缺失音频路径连续 3/3 fail-closed 且无运行时残留。公网 HTTPS CONNECT 的 owner-PID + 创建时间探针此前连续 3/3；本轮合并 opt-in 运行时因本机直连未建立上游隧道（`raw=1`，`session=0`），不计为双流回归。当前构建 `7be9a184...` 又在自动装载期间完成 120/120 次窗口响应探测（最大 13.62 ms、平均 0.61 ms），正常关闭和强制终止后均无 host/mpv 残留。该结果只证明 Header、IPC、状态机、合成 seek、合成双流、当前构建窗口/生命周期和单-authority egress guard 机制，不是 S1-S5 真机结果。仍需完成真实签名发行物、签名版本清单、生产强制发布者门禁、跨 origin HLS/DASH 重定向/嵌套资源矩阵和真实媒体验证。PID + 创建时间匹配是进程所有权关联而非密码学认证；普通 PID 复用已被缩小，handle 复制、内核失陷和同用户调试/注入被明确置于 W0 威胁模型之外。WinUI 已实现 `Referer`/`User-Agent` media option 与音频 slave，独立 headless 双源 probe 已在 dummy 输出下连续 10/10 通过 20 次位置校验 seek 和双轨保持；诊断中定位到 LibVLC 3.0.19 在清除 pause 后立即高频读取 `State`/`Time` 会触发 D3D11 卸载竞态，probe 现在等待 100 ms 后再轮询，后端也不再从 `Stopped` 回调读取 LibVLC 属性；但 LibVLC 3.0.19 不能安全表达 `Origin`/`Accept`/`Accept-Language` 或两流不同 Header，因此这些场景必须拒绝。该 probe 不证明实际 WinUI swap-chain、音频设备、真实音画差或真实来源。

`fixtures/synthetic/` 中已有 H.264/AAC、H.264/E-AC-3、HEVC Main10/FLAC、HEVC Main10/FLAC/ASS，以及从 S1 派生的纯视频/纯音频六个合成样本，并通过 `ffprobe` 与 mpv 空输出解码检查；真实 S2 E-AC-3 样本仍未提供。Tauri opt-in 集成测试通过完整控制器加载 S2/S3 样本，强制软件解码并断言 E-AC-3/FLAC/HEVC Main 10、精确 `yuv420p`/`yuv420p10`、已解码视频帧、48 kHz 单声道音频、推进的 audio PTS，以及按真实 track ID 选择并在 seek/resume 后保持的嵌入 ASS 轨道。两个样本各执行 20 次位置校验 seek，每次等待 seek 收敛并验证 `time-pos`，至少出现 12 个不同落点，连续 3/3。双流测试另用独立 loopback origin 加载纯视频与纯音频，断言外部 AAC 轨选中并在 20 次 seek 后保持，连续 3/3；缺失外部音频路径也连续 3/3 清理。`fixtures/header-range-server.py` 和 `check-header-range.ps1` 已通过 HEAD 200、100-byte Range 206、416、缺失 Header 403 和 mpv HTTP Header 注入检查。合成样本只验证机制，不关闭 S1-S5，也不证明真实 ASS/PGS 渲染质量或 100 ms 音画差门禁。

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
- 未在真实 S1-S5 样本上执行 30 分钟连续播放、20 次随机 seek、音画差测量或 DPI/多显示器矩阵；合成 loopback 样本的 20 次 seek 与 PTS 样本仅为机制证据。
- 未在 Windows 11、Intel GPU、真实双屏/混合 DPI 或睡眠恢复环境验证。
- Tauri 已改为宿主窗口全屏，并在 Tauri 主窗口内创建专用 Win32 视频子窗口；mpv 通过 `--wid` 嵌入该容器。当前构建 `7be9a184...` 已在合成 S3 媒体上复测普通窗口 Tauri 外框/客户区 1196x859/1180x820（视频容器与 mpv 均为 1117x344）、1920x1080 全屏（均为 1920x1017）、640x480/1542x931 client resize（分别为 601x171/1380x391），并验证父 HWND、`WS_CHILD`、可见性和尺寸一致；自动装载期间 120/120 次窗口响应探测通过，`F11` 切换全屏，`Escape` 仅退出全屏。当前测试机仍只有单块 1920x1080、100% 活动显示器，无法生成双屏、150%/200% 或混合 DPI 证据；CSS 覆盖控件与透明 WebView2 合成也未验证。
- 未验证真实 Bilibili 双流、源 URL 过期、地区错误、单流 403 或 Wi-Fi/热点切换；Tauri 的合成双流只使用独立的本地 HTTP fixture，不能替代 S4/S5。
- Tauri 已有可选的 Authenticode leaf signer SHA-256 pin、Lavf 协议白名单/redirect 上限、30 秒 `file-loaded` watchdog，以及单播放会话、精确 CONNECT authority、DNS pin、mpv PID + 创建时间绑定的 W0 egress guard；但尚无真实签名 mpv 发行物、生产强制发布者门禁、签名版本清单、轮换/吊销策略、产品 `local-relay`、完整跨 origin HLS/DASH 重定向/嵌套资源矩阵和真实媒体验证。未配置 signer pin 时，内容 SHA-256 仍只固定文件、不证明发布者身份。由于 Lavf 白名单必须保留 `tcp`，嵌套 `tcp://` 仍可绕过 HTTP proxy/CONNECT guard，当前 guard 不是进程级 egress fail-closed；正式安全门禁前需要 WFP、AppContainer、进程级防火墙或等价网络层。egress guard 也不防同用户调试器/进程注入，不能作为产品 relay 或 S3 安全门禁证据。
- WinUI 只支持对两路源同时生效的 `Referer`/`User-Agent`；`Origin`、`Accept`、`Accept-Language`、两流不同 Header、跨源重定向和真实 Bilibili 双流均未验收。
- 未完成两个候选的许可证与发布义务复核。

W0 的结论只能在上述证据形成后写入 ADR；当前 ADR 状态保持 `Proposed / 待真机验证`。

## 9. 回滚

W0 回滚方式：删除整个 `spikes/windows-player/`，保留 `docs/adr/0002-windows-player-engine.md` 作为决策记录。不得删除或回滚与本 Spike 无关的工作区改动。
