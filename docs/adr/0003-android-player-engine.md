# ADR 0003: Android 播放内核选型

- 状态：Proposed / 待真机验证
- 日期：2026-10-28
- 范围：Android A0 播放内核 Spike
- 决策性质：架构候选、验证门禁与回滚边界；本文不表示 A0 或 S5 已通过

## 1. 决策摘要

A0 同时保留三个候选方向，但当前只实现了第一个方向的机制原型：

1. Kotlin + Compose + AndroidX Media3 / ExoPlayer。
2. Media3 + FFmpeg extension。
3. libmpv 兼容内核。

第一版产品默认优先使用 Media3，因为它使用 Android 系统媒体栈、包体和生命周期成本最低，也最容易接入 MediaSession、音频焦点和 PiP。这个偏好不是最终选型结论。只有 Media3 在 API 28/31/34 真机和 Qualcomm/MediaTek 组合上通过 A0 门禁后，才能决定只发 Media3。

若 Media3 在目标设备上对 MKV、E-AC3/DTS/FLAC、HEVC 10-bit、ASS/PGS 或 `direct-dash` 合流存在稳定、可复现的能力缺口，再以同一设备矩阵验证 Media3 + FFmpeg extension。只有前两者都无法满足产品最低覆盖，且包体、功耗、崩溃隔离和许可证义务均可接受时，才评估 libmpv。不得用 Android x86_64 模拟器的单一结果替代真机或提前宣布赢家。

## 2. 背景与驱动因素

A0 的目标是用最小应用验证真实播放器能力，而不是先搭建正式 UI：

- 单 URL HTTPS 播放和白名单 Header。
- Bilibili 式视频/音频双 URL 本地合流。
- MKV、H.264、HEVC 10-bit、FLAC、E-AC3、ASS/PGS 等真实样本。
- 起播、暂停、seek、音画同步、字幕、错误恢复和长时稳定性。
- Wi-Fi、5G、热点、VPN 和不同源站出口。
- CPU、内存、电量和热行为。
- 后台、音频焦点、MediaSession、PiP 以及系统媒体通知的后续边界。

第一版非目标：

- 在 Android 客户端实时转码视频。
- 把 OpenList Token、Bilibili Cookie、WebDAV 主账号或长期令牌交给播放器。
- 承诺所有 OEM 解码器对所有 MKV 音频和图形字幕都一致。
- 在没有真实设备证据时承诺“大多数文件”可直连或本地合流。

## 3. 候选架构

### 3.1 候选 A：Media3 / ExoPlayer

```text
Compose UI
  -> PlaybackViewModel
  -> ExoPlayer
  -> Android MediaExtractor / MediaCodec
  -> system audio/video output

video URL + audio URL
  -> ProgressiveMediaSource x2
  -> MergingMediaSource
```

职责边界：

- Compose 只处理输入、控制和状态展示。
- ViewModel 负责输入校验、播放器生命周期和安全日志。
- Media3 负责容器解析、轨道选择、解码和音视频同步。
- 白名单 Header 只通过受控 `DataSource.Factory` 注入。

优点：

- 系统媒体栈、安装包体小、后台和媒体会话生态完整。
- 官方支持 `MergingMediaSource`、HLS、字幕和轨道选择。
- 与 Android 音频焦点、MediaSession、PiP 的产品路径一致。

主要风险：

- 设备 OEM 的 MediaCodec 对 MKV、DTS/E-AC3、HEVC 10-bit、ASS/PGS 支持差异大。
- 软件/硬件解码器选择、首帧时间和 seek 行为可能因设备而异。
- 某些模拟器 ATD 镜像自带解码器缺陷，不能代表真机。
- 本地双流合流只能解决容器和轨道组合，不能弥补设备缺少解码器。

### 3.2 候选 B：Media3 + FFmpeg extension

在 Media3 渲染链中增加 FFmpeg audio/video extension，用于补充系统解码器缺口。

它只在真实设备样本证明 Media3 的系统解码能力不足时进入 A0 的下一阶段。必须验证：

- 目标 ABI、包体和 native 构建可复现性。
- H.264/HEVC/FLAC/E-AC3 等实际解码覆盖，不能只证明 extension 能加载。
- 硬件解码回退边界、功耗、内存、首帧和 seek。
- FFmpeg 构建参数、LGPL/GPL 组件、专利/技术许可和再分发义务。

当前没有构建或运行该候选，不能把 Media3 的测试结果外推到 FFmpeg extension。

### 3.3 候选 C：libmpv 兼容内核

libmpv 是最后的兼容性方案，不是默认方案：

- 独立或嵌入播放器与 Android 生命周期、Surface、音频焦点和 MediaSession 的整合成本最高。
- native 包体、ABI、线程、崩溃隔离和 OEM 兼容测试显著增加。
- mpv、FFmpeg、libass、字体、解码器和插件均需单独完成许可证与分发复核。
- 只有在 Media3 和 Media3 + FFmpeg 都无法满足经过确认的产品最低覆盖时才考虑。

当前没有 libmpv 构建、JNI 集成或真机结果。

## 4. 当前 A0 机制证据

### 4.1 已实现

`spikes/android-player/` 是隔离原型，不进入产品目录：

- Kotlin + Jetpack Compose，`minSdk=28`，`targetSdk=37`，`compileSdk=37`。
- 单 URL 播放，HTTPS-only；HTTP 仅保留给显式本机测试的协议边界讨论，当前 UI 不接受普通 HTTP 媒体 URL。
- Header 白名单：`Referer`、`Origin`、`User-Agent`、`Accept`、`Accept-Language`。
- 视频和音频分别保存 URL、Header 和容器 MIME，各自创建独立的 `OkHttpDataSource` 与 `ProgressiveMediaSource`，再由 `MergingMediaSource` 本地合流。
- 每次播放请求按流建立 OkHttp client；同源重定向保留白名单 Header，scheme/host/port 任一变化的重定向在目标收到请求前 fail-closed，同时禁用 TLS 重定向、Cookie 和连接失败自动重试。
- 播放意图 `playWhenReady`、播放、暂停、seek、位置/时长和脱敏后的 Media3 错误码；准备失败会停止并清空旧 source。
- 页面离开时暂停，`PlayerView` 释放时解绑播放器，进度轮询受 lifecycle 约束。
- URL、Header 名称和错误码的脱敏日志测试。
- JVM 输入/Header/日志/状态展示测试，以及带合成 H.264/AAC 素材的 instrumented merge、seek 后位置推进、video/audio 轨道和 surface 可见性测试。

### 4.2 2026-10-28 本机验证

| 检查 | 结果 |
| --- | --- |
| `:app:lintDebug` | 通过，0 error，10 个依赖版本提示 |
| `:app:testDebugUnitTest` | 通过，24 tests，0 failures |
| `:app:assembleDebug` | 通过，生成 debug APK |
| `:app:assembleDebugAndroidTest` | 通过，测试 APK 可编译 |
| API 37 x86_64 ATD `connectedDebugAndroidTest` | 通过，4/4 tests；覆盖 Ready、双轨、seek discontinuity、seek 后位置推进、播放/暂停、video surface 可见性、逐流 Header/MIME 输入和生产 `PlaybackMediaSourceFactory` 独立创建两路 DataSource |
| APK 安装与 `MainActivity` 冷启动 | 通过，Activity 为 topResumed，UI 文本可见，未见应用级 Runtime error |

API 37 ATD 的系统 `c2.goldfish.h264.decoder` 在当前主机上出现 Codec2 buffer/SELinux 错误，导致合成 H.264 素材无法按默认硬件解码路径播放。instrumented 合流测试显式使用 `MediaCodecSelector.PREFER_SOFTWARE`，目的是隔离并验证 `MergingMediaSource` 机制，不是产品默认路径，也不是真机解码结论。该 workaround 必须保留在测试说明中，不能写成“模拟器通过了目标 H.264 能力”。

### 4.3 这些结果证明什么

- Media3 双源合流 API、assets 读取、播放器状态、seek、seek 后位置推进、播放/暂停、video surface 和测试 APK 生命周期可工作。
- Header 和 URL 校验/脱敏逻辑有 JVM 层证据。
- 视频和音频的 Header、容器 MIME 与 DataSource client 已按流独立建模，生产 `PlaybackMediaSourceFactory` 的独立 DataSource 创建有 instrumented 证据。
- 同源重定向保留白名单 Header、跨源重定向在目标收到请求前拒绝，已由 JVM loopback fixture 覆盖。
- Android 构建、lint、APK 安装和无窗口 ATD 启动链路可工作。

这些结果不证明：

- 真实 Bilibili 视频/音频双流可以合流、同步或长时间保持。
- 真实 MKV、HEVC 10-bit、FLAC、E-AC3、ASS/PGS、多音轨或 HLS 回退可用。
- 任何目标 API 28/31/34 真机、Qualcomm/MediaTek 设备或 arm64 设备的解码能力。
- 默认硬件解码路径、功耗、电量、热行为或后台/PiP/MediaSession 已满足产品要求。
- 真实 Bilibili/CDN 多跳重定向、DNS 变化和弱网重试可用；fixture 只证明当前拒绝策略。
- 生产 `PlaybackViewModel` 的完整网络、错误和 lifecycle 合流路径已被端到端覆盖；当前合流播放测试独立构造了 Media3 source。
- 合流路径的逐流 Range 可用；当前只接受 MP4/WebM progressive，Range 委托 Media3/OkHttp，尚无两路独立 Range 的端到端测试。

## 5. A0 验收门禁

### 5.1 功能矩阵

每个候选必须在同一组目标设备上分别记录：

| ID | 场景 | 通过条件 |
| --- | --- | --- |
| S1 | MP4 H.264 + AAC，Range 正常 | 起播、连续播放、音轨、20 次随机 seek、结束/重开 |
| S2 | MKV H.264 + E-AC3 | 解复用、E-AC3 解码/直通、seek、音画同步；真实样本缺口不得替代 |
| S3 | MKV HEVC Main10 + FLAC + ASS | HEVC 10-bit 解码/回退、FLAC、ASS 显示与 seek 恢复 |
| S3-PGS | H.264 High10 + FLAC + PGS | PGS 图形字幕显示、seek 后恢复；不冒充 S3 |
| S4 | 视频 + 音频双 URL | 本地合流、20 次 seek、30 分钟稳定性、音画差 <= 100 ms |
| S5 | Header/Range/过期/重定向场景 | 403/200/206/416、URL 过期、单流失败和受控回退；日志不得泄露值 |

### 5.2 设备和运行矩阵

- Android 9/API 28、Android 12/API 31、Android 14/API 34 真机。
- Qualcomm 和 MediaTek 至少各一台；arm64-v8a 为主。
- x86_64 ATD 只做 UI、网络和 API 机制冒烟，不能替代 arm64 真机。
- Wi-Fi、5G、热点、VPN 和不同源站出口。
- 至少一台主流国产 OEM 设备。
- 30 分钟连续播放、20 次 seek、音画差、CPU、内存、电量和热记录。
- 后台、锁屏、音频焦点、MediaSession、PiP 和进程被回收后的恢复。

### 5.3 选型门禁

| 门禁 | 通过条件 | 失败处理 |
| --- | --- | --- |
| G0 证据 | 所有结果有设备、API、ABI、素材、工具和原始测量记录，且无 secret/完整 URL/Header 值 | 不能形成选型结论 |
| G1 覆盖 | Media3 在目标矩阵达到产品最低覆盖 | 进入 Media3 + FFmpeg extension |
| G2 合流 | 真实 Bilibili 双流可起播、seek、30 分钟稳定、音画差 <= 100 ms | 保留 server-HLS 回退，不打开 direct-dash |
| G3 安全 | 白名单 Header、HTTPS/ticket 边界、日志脱敏和错误净化为 fail-closed | 候选不通过 |
| G4 性能 | 首帧、seek、CPU、内存、电量和热在预算内 | 候选不通过或降级支持 |
| G5 生命周期 | 后台、焦点、MediaSession、PiP、进程恢复无不可恢复状态 | 修复后重测 |
| G6 许可证/发布 | Media3、FFmpeg extension、libmpv、libass、字体、解码器和所有 native 分发件完成复核 | 禁止发布对应二进制 |

## 6. 安全边界

- 客户端只接收短期 ticket 能力和白名单 Header；不得接收 OpenList Token、Bilibili Cookie、WebDAV 主账号或长期 refresh token。
- 媒体 URL 的完整 query、签名字段和 Header 值在日志、崩溃报告、测试报告和 UI 诊断中必须脱敏或省略。
- Header 名称、重复项、大小和值类型必须 fail-closed；不能把用户输入直接交给 `DefaultHttpDataSource`。
- URL 必须限制 scheme、userinfo、fragment、控制字符和长度；生产路径优先使用 HTTPS 和服务端 ticket 绑定的目标。
- 重定向、跨源 Header、私网/loopback 目标和过期 URL 必须有独立测试，不能只验证首跳。
- 同源重定向会保留白名单 Header；scheme、host 或 port 任一变化的重定向必须在目标收到请求前 fail-closed，并由真实 CDN 链路继续验证。
- 本地合流不得把音视频 URL 或 Header 写入持久化存储；临时能力只在需要时驻留内存。
- 播放器失败只显示稳定错误码和可操作原因，不把 Media3 底层异常原文、源站响应或签名 URL 回显给用户。

真实 Bilibili 测试仍必须由服务端签发能力；本地 fixture 不能证明 Bilibili 可用性，也不能证明服务端 ticket/设备策略已正确集成到客户端。

## 7. 许可证与发布边界

- Media3/ExoPlayer 的 AndroidX 许可证和传递依赖必须在具体版本上审计。
- FFmpeg extension 的构建参数、启用的库、LGPL/GPL 边界、源码提供和 relinking 义务必须按实际二进制复核。
- libmpv、FFmpeg、libass、字体、字幕渲染器和插件不能只看主库许可证。
- HEVC、AAC、E-AC3、DTS 等编解码器可能涉及独立专利/技术许可；开源许可证不自动解决这些义务。
- 合成测试素材可以纳入隔离测试资产；真实 Bilibili/OpenList 媒体和凭据不得进入提交或发布包。

许可证条目未关闭前，只能进行本机开发验证，不得产出可分发 Android 安装包或宣称兼容所有目标设备。

## 8. 选型规则

1. Media3 在 G0-G6 全部通过：第一版优先只发 Media3。
2. Media3 仅因明确的解码缺口未通过：在相同设备矩阵上验证 Media3 + FFmpeg extension；不得用 extension 的桌面构建替代。
3. Media3 与 FFmpeg extension 都无法覆盖最低产品范围：才评估 libmpv，且必须同时接受包体、功耗、崩溃隔离和许可证成本。
4. 只有 x86_64 模拟器结果：A0 保持开放，不能选定候选。
5. 真实 Bilibili 双流未通过：`direct-dash` 默认保持关闭，Android 只可使用服务端允许的 `server-hls` 或其他兜底。
6. 任一门禁缺少证据：ADR 保持 `Proposed / 待真机验证`，不得写成 Media3 已胜出或 A0 已关闭。

## 9. 回滚

- 删除 `spikes/android-player/`，保留本 ADR 作为实验记录。
- 产品集成必须通过独立 `PlaybackEngine` 适配层和默认关闭的 feature flag 控制。
- Media3 失败时回退服务端 HLS/仅可用的传输；不得静默启用 libmpv 或把失败伪装成成功。
- 关闭 `direct_dash_enabled` 和客户端 `supports_dash_merge` 能力即可停止本地双流路径；`local-relay` 未实现前不得作为回滚依赖。
- 不删除或回滚与 A0 无关的工作区改动。
