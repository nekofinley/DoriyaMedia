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

截至 2026-11-01，两个隔离原型均已创建并通过以下本机验证：

| 候选 | 已通过 | 证据边界 |
| --- | --- | --- |
| Tauri 2 + mpv | pnpm frozen install/build；cargo fmt；clippy `-D warnings`；75 个 Rust tests 通过（另 3 个真实 mpv/Python/公网 opt-in 测试需显式启用）；真实 mpv 空载 JSON IPC 连续 3/3；Header 保护 loopback 合成 MP4 + 20 次 seek 连续 3/3；公网 HTTPS CONNECT 的 mpv owner-PID + 创建时间探针连续 3/3；Tauri debug exe 构建及此前交互会话 GUI 启动/退出 smoke | 只证明宿主、受限制 pipe、sidecar、IPC、输入边界、异步装载状态、Header 注入、seek 和单-origin egress guard 机制可运行，未加载真实媒体 |
| WinUI 3 + LibVLCSharp | .NET restore/build 0 warnings/0 errors；91 个 xUnit tests；LibVLC 3.0.19 probe（含 audio slave/Header 构造）；Windows App SDK self-contained app smoke；两个 loopback 合成源的 GUI `Opening -> Playing` smoke | 只证明窗口、LibVLC 初始化、白名单 Header 和本地双源机制可运行；不是真实 S1-S5、音画同步或长时结果 |

当前实现边界：

- Tauri 和 WinUI 都接受 HTTPS；plain HTTP 只允许显式启用的精确 loopback 测试模式。Tauri 的 HTTPS 预检拒绝 loopback/私网/链路本地/文档/CGNAT/组播/选定特殊用途 IP literal 和 `localhost` 主机名；严格模式下再由精确 CONNECT authority 的 W0 egress guard 处理域名解析、公网地址过滤和 DNS pin。
- Tauri 已实现 Header 白名单和双流的命令基础；mpv 子进程使用清空后的最小环境，Tauri capability 权限为空。sidecar 必须通过绝对 `DORIYA_W0_MPV_PATH` 和必填 `DORIYA_W0_MPV_SHA256` 校验：宿主以禁止写/删除共享的方式打开文件，完成 SHA-256 比对并保持句柄锁定，直到 `CreateProcessW` 接受映像。可选 `DORIYA_W0_MPV_SIGNER_SHA256` 会在同一验证句柄上执行离线 Authenticode 校验，并要求 leaf signer 的 SHA-256 thumbprint 精确匹配；未配置时仅适用于本机未签名开发 mpv，不等于生产发布者门禁或签名版本清单验证。宿主先创建 128 位 CSPRNG 命名、仅当前用户单一 ACE、拒绝远程客户端的单实例 pipe，并预连接 client 端；`STARTUPINFOEX` 只允许 client stdin 与 NUL 输出句柄继承，参数使用 `fd://0`。connect、进程创建和首次 IPC acknowledgement 共享一个有界的 15 秒启动期限；自动启动 sidecar 的 `open`/`control` 使用 15 秒启动加 5 秒操作的单一绝对期限，已有 runtime 的请求仍共享 5 秒运行期限；connect 进入 pending 后的失败、超时和 read/write 等待失败都会先 `CancelIoEx` 并阻塞 `GetOverlappedResult` 回收，再释放 `OVERLAPPED`。子进程通过 `PROC_THREAD_ATTRIBUTE_JOB_LIST` 在创建时原子加入 `KILL_ON_JOB_CLOSE` Job Object，并用原始进程句柄校验 user SID/session，避免 PID 复用竞态；任意校验失败均终止 sidecar。mpv 固定使用 `--tls-verify=yes`、`--ytdl=no`、`--network-timeout=15`，关闭脚本/本地自动加载，并通过 `--stream-lavf-o-append`、`--demuxer-lavf-o-append` 设置 Lavf 协议白名单和 `max_redirects=3`；该 append 形式已与真实 mpv `fd://0` IPC 联合冒烟。IPC 在 worker 中执行，`start-file`/`file-loaded`/`end-file` 驱动状态；`status` 轮询对未进入 `file-loaded` 的打开操作实施 30 秒期限，超时会强制清理 sidecar。opt-in 集成测试已通过完整控制器加载 Header 保护的 loopback 合成 MP4，并完成暂停、20 次相对 seek 和恢复播放 3/3；真实媒体 Header/双流、长时和跨 origin HLS/DASH 嵌套资源矩阵尚未验证。
- Tauri 严格模式另有一个仅用于 W0 的 egress guard，不是产品 `local-relay`：只绑定 `127.0.0.1:0` 并使用 `SO_EXCLUSIVEADDRUSE`，通过无凭据代理 URL 和受保护 JSON IPC 的 `http-proxy` 属性交给 mpv；只接受 HTTP/1.1 `CONNECT` 和当前播放的精确 HTTPS CONNECT authority（host + port）。每个播放会话在宿主内以 single-flight 方式解析 DNS（不持有 relay 全局状态锁，CONNECT handler 最多等待 5 秒；底层不可取消的 resolver worker 可能更晚退出）、拒绝公私混合答案、缓存 pin，只连接 pin 的数字 `SocketAddr`，并验证 `peer_addr()`；连接、握手和写操作有界，会话 CONNECT 数量和并发受限。`GetExtendedTcpTable(TCP_TABLE_OWNER_PID_ALL)` 用于匹配客户端侧 ESTABLISHED loopback 行，owner PID 和进程创建时间必须等于已校验 mpv sidecar；查询失败、PID/创建时间不同或竞态均 fail-closed。新播放和 disarm 会轮换 generation 并主动关闭旧隧道；`stop()` 的 2 秒等待不能取消已开始的不可取消 DNS/connect，相关 handler 可能短暂存活，但后续 generation/shutdown 检查会阻止持续隧道。FFmpeg 会把 `Host` 写成代理 loopback authority，解析器只接受该精确代理 authority 或精确 CONNECT target。真实 mpv 公网 HTTPS 探针连续 3/3 观察到成功 CONNECT 的 owner PID 与创建时间均匹配 sidecar。建立隧道后不检查 TLS SNI、HTTP Host、path/query、method、Header 或响应内容；DNS 在一个播放会话内不会重解析，CDN/DNS 轮换或 URL 刷新需要新播放；严格 DNS/PID/relay 失败不会回退直连；debug-only 的 loopback HTTP fixture 开关在 release 构建中被忽略。Open P1 residual risk：真实 HTTPS 要求 Lavf 白名单保留 `tcp`，而 `tcp://` 不经过 HTTP proxy，恶意嵌套 playlist 仍可能绕过 CONNECT guard 直接建连，因此该 guard 不是进程级 egress fail-closed，正式安全门禁前需要 WFP、AppContainer、进程级防火墙或等价网络层。PID + 创建时间匹配是所有权关联而非密码学认证；普通 PID 复用已被缩小，handle 复制、内核失陷与同用户调试/注入不在 W0 威胁模型内。该机制不支持跨 origin redirect、HLS/DASH 子 origin、逐流 origin 或自动 URL 刷新，不能作为产品 local-relay 或 S3 安全门禁证据。
- WinUI 已通过 `Media.AddSlave` 实现音频 slave，并支持 `Referer`/`User-Agent` media option。VLC 3.0.19 没有安全的 `Origin`/`Accept`/`Accept-Language` 注入路径，media option 也不能表达两流不同 Header，因此这些场景 fail-closed；不能据此认为 S4/S5 已通过。
- WinUI 目录另有独立的 loopback relay 机制探针：仅绑定 `127.0.0.1` 随机端口，使用 256-bit 一次性 token、逐流 Header、精确 origin allowlist、公网 HTTPS-only、GET/HEAD/Range/If-Range、响应头 allowlist、3xx fail-closed、精确 IANA RIR 全球单播 CIDR 前缀 allowlist、私网/转换 IPv6 connect-time 拒绝、响应头阶段超时与响应体空闲超时分离；响应已开始后发生超时或流错误会主动重置下游连接。Release build 0 warning/0 error，独立 xUnit 94/94。该探针尚未接入 WinUI/LibVLC，没有真实播放、刷新、产品票据或设备策略证据。
- Tauri 的 DACL、受限继承 handle、SID/session、内容哈希钉扎、可选 Authenticode leaf signer pin、mpv 网络选项、30 秒 `file-loaded` watchdog、创建时 Job Object 和单-origin egress guard 机制已落地，但仍需真实签名发行物、生产强制发布者门禁、签名版本清单、轮换/吊销策略、真实媒体 Header/双流、Host 崩溃联动实机验证和完整跨 origin HLS/DASH 重定向/嵌套资源策略。pending overlapped 读的取消后回收仍会阻塞等待驱动完成；WinUI 仍需验证真实来源、跨源重定向、音画同步和长时行为。
- API 37 Android ATD 的 Media3 机制结果与本文无关，不能替代任何 Windows 候选。

这些结果只把 W0 从“未开始”推进到“机制原型可构建、可启动、可探针”；S1-S5、稳定性、音画差、DPI/多显示器、Windows 11、Intel GPU、真实网络矩阵和许可证门禁仍未执行。

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
- Tauri 的 mpv IPC 管道不得重新退化为把 pipe 路径交给 mpv、允许第二个 client 或使用默认宽松 DACL；`fd://0` 继承路径必须由真实 mpv smoke 持续覆盖。

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

- 两个原型已创建并完成机制级构建/启动验证，但 S1-S5、30 分钟、真实样本上的 20 次随机 seek、音画差和 DPI 矩阵均未执行。
- 本机已配置 mpv `v0.41.0` 和 .NET SDK `8.0.427`；真实 Bilibili/OpenList 样本和发布许可仍未解决。
- S2 的真实 E-AC3 样本未解决，不能用其他编码替代。
- Windows 11、Intel GPU、真实双屏/DPI、睡眠恢复和 Wi-Fi/热点矩阵未覆盖。
- mpv/FFmpeg 与 LibVLC/VLC 的准确发行物、构建参数、解码器和分发模型未冻结。
- 最终选择、许可证结论和可分发性均待真机测试、依赖审计及法务/发布前复核。
- Tauri 的 sidecar 内容哈希钉扎、可选 Authenticode leaf signer pin、Lavf 协议白名单/redirect 上限、30 秒 `file-loaded` watchdog 和单-origin egress guard 已完成，但真实签名发行物、生产强制发布者门禁、签名版本清单、完整跨 origin HLS/DASH 重定向/嵌套资源策略、真实媒体和 Host 崩溃联动实机验证仍未完成；egress guard 不是产品 `local-relay`，也不防同用户调试/注入；由于 Lavf 白名单必须保留 `tcp`，嵌套 `tcp://` 仍可绕过 HTTP proxy/CONNECT guard，当前不是进程级 egress fail-closed，正式安全门禁前需要 WFP、AppContainer、进程级防火墙或等价网络层。WinUI 的真实来源、跨源重定向、音画同步和长时行为仍未完成。两者均不能据此进入正式产品集成。
