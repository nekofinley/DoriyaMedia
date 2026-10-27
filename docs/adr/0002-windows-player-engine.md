# ADR 0002: Windows 播放内核选型

- 状态：Proposed / 待真机验证
- 日期：2026-10-28
- 范围：Windows W0 播放内核 Spike
- 决策性质：架构提案与门禁，不是最终选型；机制原型已完成本机验证，但真实播放与发布门禁未完成

## 1. 决策摘要

W0 同时验证两个候选：

1. Tauri 2 + 现有 Vue UI + mpv sidecar（JSON IPC）。
2. WinUI 3 + C# + LibVLCSharp。

当前不宣布赢家。若两个候选均通过功能、稳定性、窗口/DPI、安全和许可证门禁，按计划中的当前偏好优先 Tauri，以复用现有 Vue UI 和 Rust 安全边界。若 Tauri/mpv 在全屏、多显示器、DPI、原生窗口融合、崩溃隔离、更新成本或许可证上出现硬失败，则切换到已通过同等门禁的 WinUI 方案。任一门禁未完成时保持 `Proposed`，不能以环境预检替代结论。

## 2. 背景与驱动因素

W0 的目标是用最小原型验证 MKV、HEVC 10-bit、FLAC、ASS/PGS、Range、受限 Header、双 URL 合流、全屏和 DPI，并形成许可证结论。正式客户端暂不实现登录、媒体库和进度 UI。

主要驱动因素：

- 播放覆盖：MKV、HEVC 10-bit、FLAC、E-AC3、ASS/PGS、多音轨和双流。
- UI 复用：Tauri 可继续使用现有 Vue 3 页面和视觉规范。
- 安全边界：设备令牌、ticket、源 Header 和播放 URL 只能由受控宿主层处理。
- 原生窗口：全屏、无边框、多显示器、DPI、焦点和层级必须稳定。
- 稳定性：播放器崩溃不能拖垮整个客户端，且必须可回退。
- 构建与更新：sidecar、原生 DLL、运行时、签名和更新体积可运维。
- 许可证与分发：软件著作权许可、编解码器技术许可和发布义务必须能通过复核。

## 3. 候选架构

### 3.1 候选 A：Tauri 2 + mpv

```text
Vue 3 UI
  -> Tauri 2 WebView2
  -> Rust host / command boundary
  -> supervised mpv process
  -> named pipe + JSON IPC
  -> mpv native video surface + audio output
```

职责边界：

- Vue 只处理 UI 和播放器控制意图，不持有设备 refresh token。
- Rust 层负责 API、短期票据、进程生命周期、IPC 白名单和日志脱敏。
- W0 优先验证独立 mpv sidecar 与 JSON IPC；libmpv 嵌入不是默认落地方式。
- 媒体 URL、Header 和临时凭据不得出现在命令行、前端 store、崩溃转储或普通日志中。

主要风险：

- WebView2 与 mpv 原生视频窗口在全屏、多显示器、DPI 和层级上的组合。
- 独立进程 IPC 的竞态、超时、崩溃恢复和残留进程。
- mpv、FFmpeg、libass、解码器及其插件的构建和分发义务。
- sidecar 版本、更新、签名、杀毒软件误报和安装体积。

### 3.2 候选 B：WinUI 3 + LibVLCSharp

```text
WinUI 3 C# UI
  -> player service / adapter
  -> LibVLCSharp
  -> libvlc + plugins
  -> Windows audio/video output
```

职责边界：

- WinUI 3 负责窗口和播放器表面。
- LibVLCSharp/libVLC 负责解复用、解码、字幕和输出。
- C# 宿主仍不得把 Token、Cookie、完整 URL 或原始 Header写入日志或 UI 诊断。

主要风险：

- `.NET SDK`、Windows App SDK、LibVLCSharp NuGet 与 native libVLC/plugins 的版本配套。
- libVLC 插件发现、32/64 位匹配、打包和升级。
- WinUI 窗口、全屏、DPI、多显示器和窗口生命周期的验证成本。
- LibVLCSharp、libVLC、VLC modules 和编解码器的许可证与分发义务。

## 4. 评估证据与验收门禁

### 4.1 当前机制证据

截至 2026-10-28，两个隔离原型均已创建并通过以下本机验证：

| 候选 | 已通过 | 证据边界 |
| --- | --- | --- |
| Tauri 2 + mpv | pnpm frozen install/build；cargo fmt；clippy `-D warnings`；32 个 Rust tests 通过（另 1 个真实 mpv 测试需显式启用）；真实 mpv 空载 JSON IPC 1/1；Tauri debug exe 构建及交互会话 GUI 启动/退出 smoke | 只证明宿主、sidecar、IPC、输入边界、异步装载状态和退出清理可运行，未加载真实媒体 |
| WinUI 3 + LibVLCSharp | .NET restore/build 0 warnings/0 errors；91 个 xUnit tests；LibVLC 3.0.19 probe（含 audio slave/Header 构造）；Windows App SDK self-contained app smoke；两个 loopback 合成源的 GUI `Opening -> Playing` smoke | 只证明窗口、LibVLC 初始化、白名单 Header 和本地双源机制可运行；不是真实 S1-S5、音画同步或长时结果 |

当前实现边界：

- Tauri 和 WinUI 都接受 HTTPS；plain HTTP 只允许显式启用的精确 loopback 测试模式。
- Tauri 已实现 Header 白名单和双流的命令基础；mpv 子进程使用清空后的最小环境，命名管道由 128 位 CSPRNG 生成并校验服务端 PID，Tauri capability 权限为空。IPC 请求在 worker 中执行并受固定 5 秒期限约束，`start-file`/`file-loaded`/`end-file` 驱动状态，超时会强制清理 sidecar。真实媒体 Header/双流尚未验证。
- WinUI 已通过 `Media.AddSlave` 实现音频 slave，并支持 `Referer`/`User-Agent` media option。VLC 3.0.19 没有安全的 `Origin`/`Accept`/`Accept-Language` 注入路径，media option 也不能表达两流不同 Header，因此这些场景 fail-closed；不能据此认为 S4/S5 已通过。
- Tauri 仍需完成完整 DACL/SID/session 对端信任、Windows Job Object 崩溃清理、sidecar 可执行文件身份校验和重定向策略。WinUI 仍需验证真实来源、跨源重定向、音画同步和长时行为。
- API 37 Android ATD 的 Media3 机制结果与本文无关，不能替代任何 Windows 候选。

这些结果只把 W0 从“未开始”推进到“机制原型可构建、可启动、可探针”；S1-S5、稳定性、音画差、DPI/多显示器、Windows 11、Intel GPU、网络和许可证门禁仍未执行。

### 4.2 证据要求

每个候选分别提供：

- 工具和运行时版本、Windows/驱动、屏幕/DPI、解码器和播放器版本。
- S1-S5 的实际播放结果；transport 探测、HTTP Range 或 ffprobe 不能替代播放器结果。
- 30 分钟连续播放记录。
- 每个可测样本 20 次播放器随机 seek 记录。
- 使用同一测试素材和测量方法的音画差原始值。
- 全屏、单双屏、100%/150%/200% DPI、睡眠恢复、Wi-Fi/热点记录。
- 脱敏日志、崩溃/恢复证据、CPU/GPU/内存观察值。
- 依赖图、构建参数、许可证文本、SBOM/第三方声明和分发模型。

### 4.3 硬门禁

| 门禁 | 通过条件 | 失败处理 |
| --- | --- | --- |
| G0 证据完整性 | 两个候选均有可复现实验记录，且无 secret、完整 URL/query 或 Header 值泄漏 | W0 不能形成选型结论 |
| G1 功能 | S1-S5 分别有真实播放器结果；S2 的真实 E-AC3 缺口不得替代；S3 覆盖 HEVC 10-bit/FLAC/ASS，S3-PGS 另记 | 候选不通过 |
| G2 稳定性 | 30 分钟连续播放无花屏/崩溃/黑屏/仅单轨/不可恢复卡死；每个可测样本 20/20 次 seek 可恢复 | 候选不通过 |
| G3 音画同步 | 实测音画差不超过 100 ms，记录测量方法和原始值 | 候选不通过 |
| G4 窗口/DPI | 全屏、单屏/双屏、100%/150%/200% DPI、显示器切换和 sleep/resume 正常 | Tauri 若原生窗口融合失败，进入 WinUI 切换门禁 |
| G5 安全/运维 | 凭据不进入 WebView/IPC/命令行/日志；播放器崩溃可隔离；退出无残留进程；构建和更新可复现 | 候选不通过或需修复后重测 |
| G6 许可证/发布 | mpv、FFmpeg、LibVLCSharp、libVLC/VLC、解码器和所有再分发组件完成发布前复核，义务可履行 | 禁止制作可分发安装包；必要时更换构建或候选 |

### 4.4 选择规则

1. 两个候选都通过 G0-G6：优先 Tauri 2 + mpv，因其复用 Vue UI 且符合当前规划偏好。
2. Tauri 通过 G1-G3/G5，但 G4 的原生窗口融合无法稳定解决：若 WinUI 通过全部门禁，选 WinUI。
3. Tauri 因 mpv/FFmpeg/解码器的许可证或发布义务不可接受：若 WinUI 的完整依赖同样通过 G6，选 WinUI。
4. Tauri 因崩溃隔离、sidecar 更新或许可证成本超过 WinUI：只有在 WinUI 通过同等门禁后才能切换。
5. 只有一个候选完成：不得宣布该候选胜出；状态保持 `Proposed / 待真机验证`。
6. 两个候选都失败：暂停正式播放器集成，保留 Web/WebView 播放与服务端 HLS 回退，重新评估内核或分发模型。

G1-G5 的功能结论和 G6 的许可证结论必须分开记录。功能通过不能替代许可证通过；本 ADR 不把“代码可运行”写成“可分发”。

## 5. 许可证与发布前置问题

本节只列复核项，不给出法律结论，也不批准任何二进制分发。最终义务取决于准确版本、构建选项、链接方式、插件集合、分发地区、商业模型和适用法律，必须由法务/发布负责人结合具体产物复核。

### 5.1 mpv

- 锁定 mpv 版本、上游源码、实际构建选项和二进制来源。
- 核对 mpv 本身的许可证、随附 LICENSE/COPYING、是否含不同许可的模块，以及 sidecar 再分发方式。
- 不得仅凭“动态加载”或“独立进程”推断没有 GPL/LGPL 或其他义务。
- 若分发修改版或随包提供二进制，需确认相应源码、修改说明、许可证文本和 relinking/替换权利等义务。

### 5.2 FFmpeg 与解码器

- 锁定 FFmpeg 版本、源码、`configure` 参数、启用/禁用的组件和外部库。
- 上游默认许可证与启用 GPL/nonfree 组件后的实际许可证可能不同；必须以实际构建为准。
- 当前机器上存在的 `ffmpeg 8.0.1-full_build` 只是开发环境事实，不是可再分发的批准，也不应默认打包。
- 对实际使用的解码器逐个核对：软件解码或硬件解码、FFmpeg 内置或外部库、插件许可证、专利/技术许可和再分发条件。
- 若采用 LGPL 组件，需确认动态/静态链接、通知、许可证副本、源码提供和可替换/重链接等义务；不得预先假设某种链接方式自动满足要求。

### 5.3 LibVLCSharp、libVLC、VLC 与 plugins

- 分别核对 LibVLCSharp NuGet、libVLC 动态库、VLC modules/plugins、第三方库和字体/字幕依赖的许可证；不能只看 `libvlc.dll` 一个文件。
- 确认目标 libVLC 版本的官方发布物、构建配置、插件集合和再分发条款。
- 确认应用、库和插件的许可证边界；不得假设 `libvlc.dll` 的存在意味着整个播放器包只受单一宽松许可证约束。
- 记录 NuGet 依赖图、native 文件来源、许可证副本和更新渠道。

### 5.4 HEVC、AAC 与其他编解码器

- HEVC、AAC 及测试素材中的其他编解码器可能涉及独立于开源软件许可证的专利/技术许可。开源许可证不能自动解决这些义务。
- 复核范围至少包括：使用的解码器实现、软件/硬件路径、是否随应用分发、是否由操作系统提供、分发地区、用户类型、商业/免费模式和任何许可池或独立许可人的现行条款。
- 不得在本 ADR 中预先断言某一许可池覆盖范围、费率、地域适用性或产品分类；这些必须按发布时点核实。
- 测试素材也有内容版权和分发限制；真实 Bilibili/OpenList 媒体不得进入提交或发布包。

### 5.5 发布前交付物

在 G6 关闭前至少需要：

- 完整 SBOM 和依赖图，含 native、managed、插件和运行时分发文件。
- 每个组件的版本、来源、哈希、许可证标识和许可证文本副本。
- 实际构建参数、源码/许可链接方式和所有补丁说明。
- `THIRD_PARTY_NOTICES`、所需源码提供/书面要约，以及用户可见的许可信息。
- 安装包/便携包/自动更新/源码发布各渠道的复核记录。
- 法务/发布负责人对具体二进制和分发模型的书面结论。

缺少上述任一项时，W0 只能进行本机开发验证，不得生成或分发正式安装包。

## 6. 安全与回滚

### 6.1 安全边界

- 设备 token、Cookie、ticket secret、签名 URL/query 和 Header 值不得进入前端、mpv 命令行、LibVLC 日志、崩溃转储或测试报告。
- 只允许白名单 Header 名称和值进入播放器边界；完整记录仍须脱敏。
- 播放器进程只能接收最小短期能力，不得获得长期账号凭据。
- 所有诊断输出在写入磁盘前经过字段白名单和脱敏。

### 6.2 W0 回滚

- 未选型或任一硬门禁失败：删除 `spikes/windows-player/`，不合并原型代码。
- 保留本 ADR，将状态更新为对应结论或 `Superseded`，保留证据摘要和失败原因。
- 不删除或回滚与本 Spike 无关的工作区改动。

### 6.3 产品集成回滚

- 正式集成必须位于独立播放引擎适配层之后，并由默认关闭的 feature flag 控制。
- 可回退到 `native_player_enabled=false`、`mpv_player_enabled=false` 和服务端 HLS/Web 播放器路径。
- 急停时终止 sidecar/player 进程、关闭新票据签发，并保留普通 Web 播放。
- 许可证无法通过时不发布含对应引擎/二进制的安装包，而不是用隐藏开关掩盖分发风险。

## 7. 待决事项

- 两个原型已创建并完成机制级构建/启动验证，但 S1-S5、30 分钟、seek、音画差和 DPI 矩阵均未执行。
- 本机已配置 mpv `v0.41.0` 和 .NET SDK `8.0.427`；真实 Bilibili/OpenList 样本和发布许可仍未解决。
- S2 的真实 E-AC3 样本未解决，不能用其他编码替代。
- Windows 11、Intel GPU、真实双屏/DPI、睡眠恢复和 Wi-Fi/热点矩阵未覆盖。
- mpv/FFmpeg 与 LibVLC/VLC 的准确发行物、构建参数、解码器和分发模型未冻结。
- 最终选择、许可证结论和可分发性均待真机测试、依赖审计及法务/发布前复核。
- Tauri 的完整 DACL/SID/session 对端信任、Job Object/崩溃清理、sidecar 身份校验和重定向策略仍未完成；WinUI 的真实来源、跨源重定向、音画同步和长时行为仍未完成。两者均不能据此进入正式产品集成。
