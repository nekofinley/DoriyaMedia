# Android / Windows 客户端开发规划

> 状态：S0 工具/脚本已完成但真实矩阵待执行；S1-S3 已完成；S4 服务端加固主体、专项自动测试、协议契约和 race 门禁已落地，但真实矩阵尚未全部关闭；S5 direct-dash 服务端契约、默认关闭实现及进程级 HTTP E2E 已落地，但真实 Bilibili 双流、客户端本地合流、Windows/Android 真机与网络矩阵尚未验收，因此 S5 未关闭；W0 两个候选和 A0 Media3 机制原型已落地并通过本机构建、启动/探针冒烟，但真实媒体与真机矩阵未验收，因此 W0/A0 未关闭；local-relay 尚未实现
> 基线日期：2026-10-27
> 最近更新：2026-10-28
> 目标：把后续工作拆成可由不同 Agent 独立领取、验证和回滚的任务

## 0. 当前实施进度

截至 2026-10-28，当前分支已落地以下服务端基础和客户端机制原型：

- S0：样本矩阵脚本、dry-run 与报告目录已建立；真机和真实来源矩阵仍需现场执行。
- S1：`/api/v1/meta`、稳定错误模型、OpenAPI/JSON Schema/golden fixtures 与校验脚本已完成。
- S2：设备授权、PKCE、access/refresh 轮换、设备列表/吊销、feature flag fail-closed 已实现；改密/重置与 token 撤销已绑定 `auth_epoch`。
- S3：播放票据、`direct-http`、`server-proxy`、`server-hls`、TTL、refresh、字幕 capability 和私网 SSRF 边界已实现。
- S4：Range/HEAD/`bytes=0-0`/416/If-Range、严格 206 校验、跨源 Header 剥除、源站错误脱敏、阶段超时和固定字段播放遥测已实现并通过本机自动测试；本地强 validator 改为文件系统版本。播放票 TTL、文件代际 CAS、字幕 discovery watchdog、metadata/embedded subtitle 原子提交、audio-only 识别、探测工具快照、source cache invalidation，以及 ticket probe registry/执行并发上限均已接入。probe 失败按真实父 context 区分，单流与 MultiPart 解析锁等待可取消，非协作 MultiPart driver 不再阻塞 ticket probe slot；播放/字幕 cleanup 使用有界 Context。OpenAPI/Schema 已同步 media/HLS 响应矩阵、Range/OWS、206、ETag/If-Range 和公开 failure code，protocolcheck 自测包含 mutation 覆盖。FFmpeg 出站代理已覆盖 DNS/IP 固定、重定向/嵌套资源、CONNECT 凭据隔离和本地 HLS/DASH fail-closed。`LocalDriver` 的 `Open/List/Serve` 已改为句柄操作，v1 本地流也复用安全打开的句柄。
- S4 尚未关闭：仍须用真实 OpenList/Bilibili 来源验证 30 分钟 direct、真实 HLS 解码、20 次随机 seek、网络切换和 Windows/Android 真机矩阵。`go test -race ./...` 已于 2026-10-28 在本机通过。FFmpeg/FFprobe 的普通路径重开、hardlink、根目录父级 ACL 和文件原地改写，以及 `size + mtime` 代际碰撞、subtitle row ID 快照和协作式 driver 取消仍有残余风险，不能宣称 S4 全量验收通过。
- S5：`direct-dash` 服务端契约、默认关闭实现和进程级 HTTP E2E 已落地；只有服务端 flag、设备策略、多流来源资格和客户端 `supports_dash_merge=true` 同时满足时才会签发。E2E 已通过真实 admin API 建源/扫描、能力省略/false/true、全局 flag、设备级禁用与恢复、跨设备隔离、双流签发、签发阶段非法 Header 原子回退和 HLS 关闭 fail-closed 的确定性 fixture 验证。真实 Bilibili 双流、客户端本地合流、Windows/Android 真机与网络矩阵尚未验收，因此 S5 未关闭。`local-relay` 尚未实现，仅保留发现 flag。
- W0：`spikes/windows-player/` 已形成 Tauri 2 + mpv 与 WinUI 3 + LibVLCSharp 两个隔离原型。Tauri 通过 pnpm/Rust 检查、32 个 Rust tests（另 1 个真实 mpv 测试需显式启用）、真实 mpv 空载 JSON IPC 1/1、debug exe 构建和交互会话 GUI 启动/退出 smoke；已实现 mpv 最小环境隔离、128 位随机命名管道、管道服务端 PID 校验、空 Tauri capability、固定 5 秒 IPC 响应期限、`start-file`/`file-loaded`/`end-file` 代次状态机，以及子进程退出、控制超时和失败后的有界清理。完整 DACL/SID/session 对端信任和 Job Object 崩溃联动清理仍未完成。WinUI 通过 0 warning/error 构建、91 个 xUnit tests、LibVLC `3.0.19` probe、Windows App SDK self-contained app smoke，以及带独立 `Referer`/`User-Agent` 校验的合成 loopback 双源 smoke；已支持 `Media.AddSlave` 和 LibVLC 可安全表达的双流共享 Header。两候选都未加载真实媒体，WinUI 尚不支持逐流 Header，因此 W0 未关闭。
- A0：`spikes/android-player/` 已形成 Kotlin + Compose + Media3 机制原型，支持单 URL、白名单 Header、逐流 Header/容器 MIME、双 URL `MergingMediaSource`、同源重定向 Header 保留与跨源 fail-closed、播放意图、控制/seek、lifecycle 暂停与 PlayerView 解绑、脱敏日志。Lint 0 error、10 warnings；24 个 JVM tests 通过；debug 与 test APK 构建通过；API 37 x86_64 ATD 上 `connectedDebugAndroidTest` 4/4 通过，覆盖 Ready、双轨、seek discontinuity、seek 后位置推进、播放/暂停、video surface、逐流输入和生产 MediaSource factory 独立 DataSource，APK 冷启动成功。该 ATD 的 goldfish H.264 解码器存在 Codec2 缺陷，合成 merge 测试使用 `PREFER_SOFTWARE` 仅用于隔离机制；真实 CDN 重定向链、逐流 Range、真实 API 28/31/34 真机、默认硬件解码、真实 Bilibili 双流和长时矩阵仍未执行，因此 A0 未关闭。

当前验证基线：

```powershell
Push-Location .\scripts\protocolcheck
go test -mod=readonly ./... -count=1
Pop-Location
.\scripts\check-protocol.ps1
go test ./...
go vet ./...
.\scripts\device-auth-e2e.ps1
.\scripts\playback-e2e.ps1
```

2026-10-28 本机通过 `go mod verify`、根模块 `go test ./...`、`go vet ./...`、`go test -race ./...`、带 `playbacke2e` tag 的源码编译测试、protocolcheck 自测、协议校验（36 schemas / 49 fixtures / 237 refs）、设备授权 E2E 和播放 E2E；`internal/api` 的 windows/linux/darwin amd64 测试二进制交叉编译通过。最新一次播放 E2E 为 `passed=217 failed=0 skipped=0`：它用 `-tags playbacke2e` 构建服务端，通过真实 admin source/library/scan API 创建确定性多流 fixture，并覆盖 `supports_dash_merge` 省略/false/true、全局 flag、设备级禁用/恢复、跨设备隔离、合法 video/audio 双流、白名单 Header、无 media session、签发阶段非法 Header 原子回退 server-HLS，以及 HLS 关闭时 `unsupported`/refresh `410 ticket_expired`/GET 可查询状态。脚本仍验证固定 `media_response` 字段、HEAD/playlist 零字节计数、ticket ID 与源 URL/Header 不泄漏，用合成 MP4 实际获取并解码 local server-HLS segment；还通过本机标准 `207 Multi-Status` WebDAV fixture 覆盖远程 server-proxy 的 GET/HEAD、Range、416、If-Range、上游忽略 Range、畸形 206、源站 5xx 和敏感响应头剥离。这些 fixture 结果不能替代真实 Bilibili 双流或客户端本地合流；E2E 仍不覆盖 OpenList/Bilibili 真实来源、30 分钟 direct、网络切换或真机播放器，CI 也不运行该脚本。真实 30 分钟 direct、真实来源 HLS 解码、真实 Bilibili 双流和真机矩阵仍无结果。

客户端机制基线同日见 `docs/adr/0002-windows-player-engine.md` 和 `docs/adr/0003-android-player-engine.md`。这些结果只证明原型构建、输入边界、窗口/播放器生命周期和合成合流可运行；不证明真实媒体、默认硬件解码、Bilibili 双流、30 分钟、20 次 seek、音画差、DPI 或网络矩阵通过。

## 1. 结论先行

### 1.1 客户端能让更多视频绕开服务器，但不会自动变成“大多数都能 302”

客户端直连必须同时满足三个条件：

1. 客户端网络能访问源站 URL，且源站接受客户端的出口 IP。
2. 客户端能发送源站要求的请求头；该请求头必须允许下发给客户端。
3. 客户端的解封装、视频、音频和字幕能力能处理该文件。

当前库有 7,405 个文件：

| 来源 | 文件数 | 已知字节 | 说明 |
| --- | ---: | ---: | --- |
| Bilibili | 5,132（69.3%） | 未知 | 当前是音视频分离的 DASH，且需要 Referer/UA |
| OpenList | 2,273（30.7%） | 约 2.30 TB | MKV 1,810 个，约占来源字节 93%；MP4 463 个 |
| 合计 | 7,405 | 至少 2.30 TB | Bilibili 行没有 size 数据 |

预期结论：

| 平台 / 来源 | 客户端直连潜力 | 主要限制 |
| --- | --- | --- |
| Windows + OpenList | 高 | 直链可能绑定出口 IP、会过期；客户端需要能解码对应编码 |
| Android + OpenList | 中到高，机型差异大 | MKV、E-AC3/DTS/FLAC、HEVC 10-bit、ASS/PGS 取决于内核和设备 |
| Windows + Bilibili | 中，未实测 | 双流本地合流、地区、客户端出口 IP、CDN URL 过期 |
| Android + Bilibili | 中到低，未实测 | 同上，另外受 Media3/硬件解码能力限制 |
| WebDAV / 本地目录 | 低 | 主账号凭据不能下发；应继续走服务端代理 |

因此：

- 客户端能明显提高 OpenList 和 Bilibili 的直连机会，尤其是 Windows。
- 不能在没有真机矩阵数据前承诺“大多数视频”或某个直连百分比。
- 只做客户端外壳、继续调用旧 `/api/stream`，并不会带来这些收益。
- 真正的关键改造是新增“播放票据 + 客户端能力 + 安全 Header + 多传输策略”协议。

### 1.2 仓库与分支

采用一个仓库、一个 `main`、短生命周期功能分支：

```text
DoriyaMedia/
  clients/
    android/
    windows/
  protocol/
    openapi.yaml
    schemas/
    fixtures/
  docs/
```

不要长期维护 `android`、`windows` 平台分支。服务端协议、播放决策和两个客户端会持续互相修改，长期分支只会制造协议漂移和合并地狱。

推荐分支和标签：

- 服务端：普通短分支，例如 `codex/protocol-v1-*`；发布标签继续用 `v*`。
- Windows：短分支 `codex/windows-*`；发布标签 `windows-v*`。
- Android：短分支 `codex/android-*`；发布标签 `android-v*`。
- 协议破坏性变更通过 API 版本或能力协商处理，不通过长期分支处理。

只有在以下条件同时成立时才拆多仓库：有独立团队和权限边界、客户端发布节奏完全独立、代码需要闭源或独立授权、共享协议有自动化版本发布机制。当前阶段都不满足。

### 1.3 推荐实施顺序

1. 先做服务端 API v1、设备登录和播放票据。
2. 同时做 Windows 与 Android 的能力 Spike，但不写正式 UI。
3. 用 Windows 做第一条完整产品纵切，因为 mpv 能最快验证 Header、MKV、双流和字幕边界。
4. 协议和失败语义稳定后，再完成 Android 产品线。
5. `direct-dash` 的服务端契约与默认关闭实现现已落地；最后阶段完成真实双流与客户端合流验收、生产启用、本机中继和双内核兼容策略，并完成发布门禁。

这不是“Windows 永远优先”。如果 Android 有明确业务优先级，A0 Spike 仍应与 W0 并行，只是正式产品化应等待同一版协议稳定。

## 2. 目标与非目标

### 2.1 目标

- 让客户端在满足权限和源站要求时直接拉取媒体，播放字节不经过服务器。
- 保持现有 Web 客户端完全兼容，不中断 Safari/Chrome 播放链路。
- 建立版本化、可测试、可灰度和可回滚的原生客户端协议。
- Windows 支持常见 MP4/MKV、多音轨、文本/图形字幕、HLS 与服务端兜底。
- Android 以系统媒体栈为主，提供后台、音频焦点、MediaSession 和 PiP。
- 失败时自动回退到服务端代理或服务端 HLS，而不是黑屏或静默失败。
- 凭据、播放 URL、票据和诊断日志之间保持明确安全边界。

### 2.2 非目标

- 不在客户端实时转码视频。
- 不把 OpenList Token、Bilibili Cookie、WebDAV 主账号 Basic 下发到客户端。
- 不承诺直链可实时吊销；媒体字节离开服务器后，只能依赖短 TTL 和源站过期。
- 不在第一版实现 TV、CarPlay、Android Auto、Apple TV 或其他平台。
- 不在第一版重写现有 Go 播放引擎。
- 不把 `/api` 全面迁到 `/api/v1`；旧 Web 继续使用旧 API。
- 不为了客户端 UI 复用而强行统一 Android 和 Windows 的技术栈。

## 3. Legacy `/api` 实现事实与 v1 边界

以下条目除非明确标注 v1，否则描述 legacy `/api` 现状，不能直接套用到 `/api/v1`。v1 当前可签发 `direct-http / server-proxy / server-hls`，并在默认关闭的 S5 分支中支持 `direct-dash`；没有可用 v1 transport 时进入 `unsupported`。所有 v1 传输都使用固定安全 Header DTO 和票据，server-proxy/server-HLS 另用 media session。

这些事实决定了协议不能只是给旧接口加一个客户端壳：

- 播放决策目前只有 `direct / proxy / remux / unsupported`：
  `internal/media/decision.go:40`
- 只要源要求 Header，当前服务端就禁止直连：
  `internal/media/decision.go:295`
- 多流 DASH 当前强制服务端 HLS：
  `internal/media/decision.go:493`
- `/api/stream` 只有在没有 Header 时才 302：
  `internal/api/play_handlers.go:893`
- 多流 Part 的 Header 当前不会被序列化：
  `internal/source/source.go:53`
- Bilibili 故意报告 `HasDirect=false`，并要求 Referer/UA：
  `internal/source/bilibili.go:1893`
  `internal/source/bilibili.go:249`
- OpenList 能返回 `raw_url` 和要求的 Header：
  `internal/source/openlist.go:183`
- WebDAV 和本地目录不提供直链：
  `internal/source/webdav.go:164`
  `internal/source/local.go:54`
- 旧播放签名只绑定 `fileID + exp`，有效 12 小时，stream/HLS/字幕签名语义可互换：
  `internal/api/play_handlers.go:36`
  `internal/api/server.go:165`
- 登录返回 30 天原始 Bearer Token：
  `internal/api/auth_handlers.go:136`
  `internal/api/user_handlers.go:19`
- 已有播放进度接口：
  `POST /api/progress`
  `GET /api/progress?file_id=`
  `internal/api/server.go:333`
- 当前 legacy Range 已覆盖单段、后缀、416 和 If-Range；v1 仍只接受前向单段 Range 并拒绝后缀 Range：
  `internal/api/proxy_range.go:36`

## 4. 传输模型

### 4.1 四种 v1 传输与未来 local-relay

概念上有五种策略，但当前 contract version 1 只定义四种传输；`local-relay` 尚未实现，也不是当前可签发的 transport。

| `transport` | 当前状态 | 字节路径 | 使用场景 | 是否允许 Header | 必须保留的兜底 |
| --- | --- | --- | --- | --- | --- |
| `direct-http` | v1 已实现 | 客户端 → 源站 | 单 URL、客户端可解码、Header 可安全下发 | 仅白名单 Header | `server-proxy`、启用的 `server-hls` |
| `direct-dash` | v1 additive 已实现，默认关闭 | 客户端分别 → 视频/音频源站，本地合流 | Bilibili 等双流源 | 仅白名单 Header | 启用的 `server-hls` |
| `local-relay` | 未实现，仅保留发现 flag | 客户端播放器 → `127.0.0.1` 中继 → 源站 | 播放内核无法注入 Header，或需要自动刷新 URL | 中继内部注入 | `server-proxy`、启用的 `server-hls` |
| `server-proxy` | v1 已实现 | 客户端 → 服务器 → 源站 | 客户端可解码但源站不允许安全直连 | 不向客户端下发源凭据 | 启用的 `server-hls` |
| `server-hls` | v1 已实现 | 客户端 → 服务器 HLS → ffmpeg 拉源 | 容器/音频/双流/图形字幕不兼容 | 不向客户端下发源凭据 | `unsupported` |

`direct-dash` 只有在 `direct_dash_enabled=true`、设备级策略允许、来源是多流且客户端显式声明 `supports_dash_merge=true` 时才可能签发；省略该能力等价于 false。真实 Bilibili 双流与客户端本地合流尚未验收，不能把该实现状态写成 S5 已关闭。

`local-relay` 尚未开始实现。未来落地时，它不是服务器中转，只监听 loopback，使用随机端口和一次性票据，绝不能绑定 `0.0.0.0`，也不能成为开放代理或 SSRF 跳板。

### 4.2 “像 302”不是继续返回 HTTP 302

原生客户端需要 JSON 票据，因为：

- 客户端必须获得源 URL 和允许的 Header。
- 客户端必须报告自己的容器、视频、音频和字幕能力。
- 服务器必须区分共享、直连、代理、HLS 和合流资源。
- 客户端需要知道 URL 过期时间，以及当前仍可尝试的 `remaining_transports` 顺序。

票据方案的最终流量效果类似 302：媒体字节不经过服务器。但协议不应继续使用浏览器式 302，因为 302 会丢失 Header，也无法表达双流和客户端能力。

### 4.3 失败回退

推荐统一顺序：

```text
direct-http
  -> 重新签发一次票据（源 URL 可能过期或出口变化）
  -> server-proxy
  -> server-hls（仅 server_hls_v1_enabled=true）
  -> unsupported（显示可操作原因）

direct-dash
  -> 重新签发一次票据（源 URL 可能过期或出口变化）
  -> server-hls（仅 server_hls_v1_enabled=true）
  -> unsupported（显示可操作原因）
```

`direct-dash` 不创建 media session，当前不能回退到 `server-proxy`。`local-relay` 是尚未实现的引擎适配层，不是额外内容来源；未来落地后其失败路径才进入 `server-proxy`。

客户端必须避免回退循环：

- 同一次播放会话对同一传输最多重试一次。
- 403/410 优先刷新票据，不立即把错误归为文件损坏。
- 416 属于 Range 语义错误，记录并回退，不继续无限重试。
- 地区、会员或源站策略错误应原样展示脱敏后的原因。

## 5. API v1 与播放票据

### 5.1 API 原则

- 保留 `/api`，Web UI 不改用 v1。
- 新增 `/api/v1`，只服务原生客户端和明确的自动化测试。
- 新增 `protocol/openapi.yaml` 和 JSON Schema。
- 所有响应使用稳定错误码；中文 `message` 只用于展示，不作为程序分支条件。
- `GET` 请求不能触发昂贵探测；播放决策使用 `POST`。
- `GET /api/v1/meta` 返回契约版本、服务端版本、最低客户端版本和 feature flags。

建议端点：

```text
GET    /api/v1/meta
POST   /api/v1/auth/device/authorizations
GET    /api/v1/auth/device/authorizations/{id}
POST   /api/v1/auth/device/token
POST   /api/v1/auth/refresh
POST   /api/v1/auth/logout
GET    /api/v1/devices
DELETE /api/v1/devices/{id}

GET    /api/v1/home
GET    /api/v1/items
GET    /api/v1/items/{id}
POST   /api/v1/progress
GET    /api/v1/progress

POST   /api/v1/playback/tickets
GET    /api/v1/playback/tickets/{id}
POST   /api/v1/playback/tickets/{id}/refresh
```

是否有独立 `/api/v1/library`、`/browse`、`/requests` 端点由 M1 产品范围决定；第一版客户端只需要首页、详情、播放和进度。

### 5.2 设备认证

不采用“在客户端 WebView 内长期保存网页 Cookie”或“客户端直接收集密码”的方案。推荐 OAuth 设备授权风格加 PKCE：

1. 客户端生成一次性 challenge 和设备密钥，向 `POST /api/v1/auth/device/authorizations` 注册。
2. 服务端返回 `device_code`、短 `user_code`、授权 URL 和 10 分钟过期时间。
3. 客户端打开系统浏览器；用户在现有 Web 登录页完成密码登录和 Turnstile。
4. 用户在授权页确认设备名、平台和请求来源。
5. 客户端轮询 `POST /api/v1/auth/device/token`，换取 access token 与 rotating refresh token。
6. access token 建议 15 分钟；refresh token 建议 30 天，轮换后在数据库只保存哈希。
7. 用户可以查看设备列表并单独吊销某台设备。

客户端存储：

- Windows：Rust 层持有令牌，refresh token 使用 DPAPI CurrentUser 保护。
- Android：使用 Android Keystore 包装随机设备密钥，refresh token 加密保存。
- 两者都不得把令牌放进 URL、普通日志、崩溃报告、剪贴板或未加密的 JSON 文件。

Turnstile 不应被原生 UA 绕过。设备授权页复用现有 Turnstile 和登录安全逻辑。

### 5.3 票据请求

建议请求模型：

```json
{
  "file_id": 123,
  "device_id": "dev_...",
  "client": {
    "platform": "windows",
    "version": "0.1.0",
    "engine": "mpv",
    "containers": ["mp4", "mkv", "webm"],
    "video_codecs": ["h264", "hevc", "hevc10"],
    "audio_codecs": ["aac", "eac3", "flac"],
    "subtitle_codecs": ["text", "ass", "pgs"],
    "supports_range": true,
    "supports_headers": true,
    "supports_dash_merge": false
  },
  "preferred_transports": [
    "direct-http",
    "server-proxy",
    "server-hls"
  ]
}
```

客户端能力只作为决策输入，不能被客户端用来突破账号权限。服务端仍执行用户、设备、文件权限和源站安全检查。

当前 contract version 1 的请求模型已包含可选 `supports_dash_merge`，省略等价于 false。仅声明 true 不会让 `direct-dash` 变成可用；还必须同时满足 `direct_dash_enabled`、设备策略和多流来源资格。真实双流与客户端合流尚未验收。

### 5.4 票据响应

建议核心模型：

```json
{
  "ticket_id": "pt_...",
  "state": "consuming",
  "contract_version": 1,
  "expires_at": "2026-10-26T12:05:00Z",
  "transport": {
    "kind": "direct-http",
    "streams": [
      {
        "role": "muxed",
        "url": "https://source.example/file.mp4?signature=...",
        "mime": "video/mp4",
        "headers": {
          "Referer": "https://www.bilibili.com/",
          "User-Agent": "DoriyaMedia/0.1"
        },
        "range": true
      }
    ],
    "subtitles": []
  },
  "media": {
    "container": "mp4",
    "duration": 1440.0,
    "video": {"codec": "hevc", "width": 1920, "height": 1080, "bit_depth": 10},
    "audio": [{"codec": "aac", "channels": 2, "language": "jpn"}]
  },
  "remaining_transports": ["server-proxy", "server-hls"]
}
```

约定：

- 当前状态集合为 `probing / ready / consuming / unsupported / failed / expired / revoked / exhausted`；只有 `consuming` 的一次性签发响应携带 `transport`，`probing` 返回 `202` 且不携带资源描述。
- `direct-dash` 已作为 contract version 1 的 additive transport 进入契约，默认关闭且受 `supports_dash_merge` 等条件限制；`streams` 必须以 `video` 和 `audio` 两个角色表达双流。真实 Bilibili 双流和客户端合流尚未验收，不能把它视为已验收播放路径。
- `server-proxy` 和 `server-hls` 必须有独立 media session，不与 5 分钟决策票共用 TTL。
- media session 建议 15 分钟空闲过期、6 小时绝对上限；HLS playlist 子资源只认同会话。
- 当前 API 不含 `fallback_order`；客户端使用 `remaining_transports` 判断剩余候选顺序。
- `source_expires_at` 目前只是 schema 预留字段，服务端尚未填充；客户端不得依赖它，未知时以 401/403/410 或播放失败为刷新触发器。
- 字幕描述位于 `transport.subtitles`，不会出现在只看状态的 `GET` 响应或顶层 `subtitles` 字段。
- `headers` 只允许 `Referer`、`Origin`、`User-Agent`、`Accept` 等服务端显式列入白名单的值。
- 客户端任何诊断日志只显示源站 host 和 transport，不显示完整 URL、query、Header 或票据。

禁止下发：OpenList Token、Bilibili 账号 Cookie、WebDAV 主账号 Basic、任意源 Header、refresh token。

### 5.5 Feature flags

建议 flags：

```text
client_v1_enabled
device_auth_enabled
playback_ticket_enabled
direct_http_enabled
server_proxy_v1_enabled
server_hls_v1_enabled
direct_dash_enabled
local_relay_enabled
native_player_enabled
libmpv_fallback_enabled
```

全部默认关闭，按用户或设备灰度，并支持单个设备急停。旧 Web 不依赖这些 flags。

## 6. 客户端技术选型

### 6.1 Windows

首选：

```text
Tauri 2
  + 现有 Vue 3 Web UI
  + libmpv sidecar
  + mpv JSON IPC
```

理由：

- mpv 对 MKV、HEVC 10-bit、FLAC、ASS/PGS、多音轨和双 URL 的覆盖最好。
- 可复用现有 Vue 页面和已经存在的视觉体系。
- Rust 层可以安全持有设备令牌，并作为所有 API 请求的统一出口，避免 WebView CORS 和令牌暴露。

保底方案：`WinUI 3 + C# + LibVLCSharp`。只有在以下任一条件出现时切换：

- WebView2 与 mpv 原生窗口在全屏、多显示器、DPI 或层级上无法稳定融合。
- mpv/FFmpeg 的 LGPL/GPL、HEVC/AAC 许可证无法接受。
- Rust sidecar、更新或崩溃隔离成本超过 WinUI 方案。

Windows 第一版建议专注 x64，Windows 11 为主，Windows 10 22H2 做兼容验证；若依赖链在 Win10 上有硬阻塞，应明确降级支持而不是隐藏问题。

### 6.2 Android

首选：

```text
Kotlin + Jetpack Compose + Media3
```

兼容兜底：

```text
libmpv，仅通过 feature flag 和独立播放引擎适配层启用
```

不建议当前用 Flutter 作为 Android 主线。Flutter 能共享 Windows UI，但现有 Windows 侧已经有 Vue，Android 的后台、音频焦点、PiP、MediaSession 和硬件解码仍需要平台原生实现。

媒体栈验收重点是 Media3 在目标真机上的 MKV、HEVC 10-bit、E-AC3/DTS/FLAC、ASS/PGS 和 DASH 合流能力。Media3 不足时先评估 FFmpeg extension，再决定是否启用 libmpv。

### 6.3 共享边界

共享：

- OpenAPI、JSON Schema、错误码、能力枚举和 golden fixtures。
- 视觉 tokens、文案、交互规则、设备授权流程。
- 测试样本描述、播放矩阵和诊断字段命名。

不强行共享：

- Android Compose 与 Windows Vue 的 UI 代码。
- Media3 与 mpv 的播放器状态机实现。
- 平台令牌存储和更新机制。

## 7. 里程碑

| 里程碑 | 结果 | 不包含 |
| --- | --- | --- |
| M0 | 能力/出口测量完成；API v1、票据和设备登录设计冻结第一版 | 产品 UI、正式播放器 |
| M1 | 服务端 v1 可控上线；两个客户端壳能登录、浏览、记录进度 | 原生播放 |
| M2 | 至少一个平台完成 `direct-http / server-proxy / server-hls` 纵切 | `direct-dash`、中继 |
| M3 | 第二平台复用协议完成同一纵切 | 高级字幕/系统集成 |
| M4 | Header 直连、local relay、双流和系统能力完成 | 正式全量发布 |
| M5 | `direct-dash` 通过真机门禁，兼容内核有明确取舍 | 新来源类型 |
| M6 | 签名、更新、灰度和回滚完成 | 下一平台扩张 |

## 8. 任务卡

### S0：能力与出口测量基线

**目标**

- 建立统一的媒体样本矩阵和测量脚本。
- 测出当前库中各类文件在 Windows/Android 上的真实直连、Range、Header、解码和出口 IP 成功率。
- 产出可复核报告，为是否继续以及优先平台提供数据。

**非目标**

- 不修改正式播放决策。
- 不发布客户端。
- 不提交真实账号、Cookie、Token 或会过期的源 URL。

**依赖**

- 无。

**目标目录**

- `scripts/playback-matrix.ps1`
- `protocol/fixtures/media-samples.json`
- `docs/playback-spikes/`

**完成与验证**

- 样本至少包含 S1-S5：
  - S1 OpenList MP4 H.264/AAC，支持 Range。
  - S2 OpenList MKV H.264/EAC3。
  - S3 MKV HEVC 10-bit/FLAC，带 ASS/PGS。
  - S4 真实 Bilibili 视频/音频双 URL。
  - S5 使用 Referer/UA 双流成功样本，以及 Authorization 等非白名单 Header、Range 行为异常和单流失败的负向回退样本。
- 报告记录文件 ID、来源、容器、编码、客户端网络、出口地区、结果和失败阶段，不记录真实凭据。
- 本地命令：

```powershell
Set-Location C:\Users\Administrator\DoriyaMedia
.\scripts\playback-matrix.ps1 -DryRun
go test ./...
git diff --check
```

**设备/网络矩阵**

- Windows 10 22H2、Windows 11 当前稳定版各一台。
- Android 9、12、14 各至少一台真机；优先覆盖 Qualcomm 与 MediaTek。
- 同 LAN、手机热点、不同公网出口、VPN/源站代理出口各测一次。

**回滚**

- 只增加脚本和报告；删除对应目录即可，不影响生产代码。

**日志禁区**

- OpenList Token、Bilibili Cookie、WebDAV Basic、Authorization、refresh/access token、完整源 URL/query/Header 一律不得落盘。

### S1：API v1、OpenAPI 与能力协商

**目标**

- 增加 `/api/v1/meta` 和稳定错误模型。
- 提交第一版 OpenAPI、JSON Schema、golden fixtures 和兼容矩阵。
- 保留旧 `/api` 全部行为。

**非目标**

- 不实现正式票据、设备登录或播放器。
- 不把 Web UI 迁到 v1。

**依赖**

- S0 的能力枚举和错误分类。

**目标目录**

- `protocol/openapi.yaml`
- `protocol/schemas/`
- `protocol/fixtures/`
- `internal/api/`
- `scripts/check-protocol.ps1`

**完成与验证**

- `/api/v1/meta` 返回 `contract_version`、`server_version`、`minimum_client_versions`、`features`。
- 错误码至少覆盖 `client_upgrade_required`、`device_revoked`、`ticket_expired`、`unsupported_media`、`source_unavailable`、`rate_limited`。
- fixtures 覆盖 `meta`、`probing`、`unsupported`、`direct-http`、`server-proxy`、`server-hls`、`direct-dash` 和主要错误；`ticket-direct-dash.json` 证明静态 additive 契约与校验通过，`playback-e2e.ps1` 另行覆盖确定性进程级签发/回退，但两者都不代表真实双流、客户端合流或 S5 验收完成。
- 校验命令：

```powershell
Set-Location C:\Users\Administrator\DoriyaMedia
.\scripts\check-protocol.ps1
gofmt -l .\internal
go vet ./...
go test ./...
git diff --check
```

- 本次改动涉及的 Go 文件必须 `gofmt` clean，schema fixture 校验必须全绿；仓库仍存在未清理的历史格式差异，不能宣称 `gofmt -l .\internal` 全局无输出。

**设备/网络矩阵**

- 纯契约任务不要求真机；OpenAPI lint 在 Linux CI 执行，PowerShell 校验在 Windows 本地执行。

**回滚**

- 关闭 `client_v1_enabled`，停止挂载 `/api/v1` 路由；旧 `/api` 和 Web UI 不受影响。

**日志禁区**

- 禁止把设备令牌、Cookie、完整 Header、源 URL、query signature 写入 fixture 或错误日志。

### S2：设备认证与令牌轮换

**目标**

- 实现系统浏览器授权、PKCE、access/refresh token 轮换、设备列表和单设备吊销。
- 复用现有登录页、Turnstile、账号状态和审计规则。
- refresh token 仅存哈希；用户禁用、改密或吊销后不能继续续期。

**非目标**

- 不让客户端嵌入网页登录并复制 Cookie。
- 不绕过 Turnstile。
- 不改变 Web 现有 30 天 Cookie 登录行为。

**依赖**

- S1。

**目标目录**

- `internal/api/device_handlers.go`
- `internal/auth/`
- `internal/db/`
- `webui/src/views/` 中的设备授权页
- `scripts/device-auth-e2e.ps1`

**完成与验证**

- access token 建议 15 分钟，refresh token 建议 30 天并每次轮换。
- 过期、重放、跨用户、跨设备、已吊销账号和已吊销设备全部拒绝。
- 客户端只拿到 access/refresh token，不拿网页 session token。
- 验证命令：

```powershell
Set-Location C:\Users\Administrator\DoriyaMedia
.\scripts\check-protocol.ps1
go test ./internal/auth ./internal/db ./internal/api
go test ./...
.\scripts\device-auth-e2e.ps1
git diff --check
```

**设备/网络矩阵**

- Windows Chrome/Edge 系统浏览器授权。
- Android Chrome Custom Tabs 授权。
- 授权过程中关闭浏览器、令牌过期、刷新重放、设备吊销各测一次。

**回滚**

- `device_auth_enabled=false` 时隐藏授权页和 v1 认证端点；Web 登录、现有 Bearer 和 Cookie 行为保留。

**日志禁区**

- 禁止记录密码、Turnstile token、device code、user code、access token、refresh token、Cookie 和 Authorization Header。

### S3：播放票据与首批传输

**目标**

- 实现 `POST /api/v1/playback/tickets` 和 ticket status。
- 首批支持 `direct-http`、`server-proxy`、`server-hls`。
- 票据绑定 user/device/file/purpose/transport，明确 TTL、失败原因和 fallback 顺序。
- media session 与 5 分钟决策票分离。

**非目标**

- 不实现 `direct-dash`。
- 不下发源站主凭据。
- 不重用旧 12 小时 `fileID+exp` 万能签名。

**依赖**

- S1、S2。

**目标目录**

- `internal/api/playback_ticket_handlers.go`
- `internal/media/transport.go`
- `internal/db/`
- `internal/api/play_handlers.go`
- `internal/source/source.go`

**完成与验证**

- `Part.Headers` 通过显式安全 DTO 输出，禁止直接序列化任意 map。
- `direct-http` 只在客户端能力和 Header 白名单均通过时签发。
- `server-proxy`、`server-hls` 使用独立 media session，空闲 15 分钟、绝对上限 6 小时。
- 跨文件、跨用户、跨设备、跨用途和过期票据全部拒绝。
- 验证命令：

```powershell
Set-Location C:\Users\Administrator\DoriyaMedia
.\scripts\check-protocol.ps1
go test ./internal/api ./internal/media ./internal/source
go test -race ./internal/api ./internal/media
go test ./...
git diff --check
```

**设备/网络矩阵**

- S3 样本；S1/S2 仅作回归基线。
- 同网出口、手机热点、不同地区出口。
- 源 URL 主动 403、过期和源站 5xx 的故障注入。

**回滚**

- 分别关闭 `direct_http_enabled`、`server_proxy_v1_enabled`、`server_hls_v1_enabled`。
- 任一传输关闭时，ticket 返回剩余可选传输；旧 `/api` 保持原样。

**日志禁区**

- 禁止记录 OpenList Token、Bilibili Cookie、WebDAV Basic、refresh/access token、完整源头 URL/query、原始 Header 和 media session 内部签名。

### S4：Range、Header 与可观测性加固

**目标**

- server-proxy/local/subtitle 支持 GET/HEAD；server-hls playlist/segment 仅 GET，HEAD 是显式 405 例外。统一前向单段 Range、`bytes=0-0`、416 和 If-Range 的 v1 行为。
- 客户端直连前可选执行 `bytes=0-0` 自检。
- 对源站错误 206、忽略 Range、跨域重定向剥除敏感 Header。
- 记录 transport、请求级 fallback、server media bytes 和失败阶段，供本机门禁测试与日志采集使用；当前未接入 metrics 或告警平台。

**非目标**

- v1 不支持多段或后缀 Range；legacy 保留既有后缀 Range 行为。
- 不记录任何媒体内容或敏感请求细节。
- 不引入完整监控平台作为上线前置。

**依赖**

- S3。

**目标目录**

- `internal/api/proxy_range.go`
- `internal/api/proxy_range_test.go`
- `internal/media/`
- `internal/api/`

**完成与验证（本机自动测试）**

- v1 server-proxy、本地媒体和字幕的 `HEAD` 不返回 body，并保留正确的 `Content-Length`/`Content-Range`。
- v1 server-proxy/本地媒体的 `GET Range: bytes=0-0` 返回 206 且恰好一个字节。
- v1 单段 Range 支持闭区间和开放区间；v1 明确拒绝多段和后缀 Range。legacy 继续保留其既有后缀 Range 行为。
- Range 只接受 ASCII 数字和最外层 SP/HTAB OWS：畸形客户端 Range 返回 400，缺失或不满足的范围返回 416；坏 upstream 206、越界 206、畸形上游 `Content-Range` 映射为 502，缺失 416 `Content-Range` 仍按 RFC 走探测回退。
- v1 local 只发送基于 volume/file identity/change time（Linux 为 dev/inode/ctime）的强 ETag，并且 `If-Range` 只接受精确强 ETag；日期、弱 ETag、旧 ETag 或无法取得可靠版本时回退完整 200。server-proxy 把条件交给源站，只能依据源站返回的 validator 拒绝冲突 206；源站返回 206 却省略 validator 时，代理无法独立证明条件匹配。
- v1 HLS playlist/segment 按契约只接受 GET，`HEAD` 返回 405；不能把 server-proxy 的 HEAD 语义外推到 HLS。
- OpenAPI 静态契约已声明 stream GET/HEAD 的 `200/206/400/403/404/408/410/416/429/500/502/504` 与 HLS GET 的 `200/400/403/404/408/410/415/500/502/504`；HEAD 所有响应必须 bodyless，206/416 的 `Content-Range` 必填。Range 外层 SP/HTAB OWS、闭区间/开放区间、拒绝后缀/倒序/多段/重复 Header，以及强 ETag/If-Range 语义均有 protocolcheck mutation test。公开 failure code 使用闭合 schema 和 3 个新增 failed fixtures；未知内部 failed code 收敛为 `source_unavailable`。
- `LocalDriver.Open/List/Serve` 使用 `os.OpenInRoot` 和已打开句柄；v1 本地媒体也复用已打开句柄并对其执行 Range/HEAD。
- 媒体响应遥测使用固定字段：`surface`、`transport`、`asset`、`outcome`、`failure_stage`、`status`、`response_bytes`、`server_media_bytes`、`duration_ms`；不记录票据、URL、query、Header 或本地路径。HEAD 不计响应/媒体字节，playlist/subtitle 不计 `server_media_bytes`。v1/legacy subtitle 均已接入 `media_response` telemetry。
- 有效 TTL：播放决策票绝对 TTL 为 5 分钟，transport claim 为 30 秒；media session 和 subtitle session 均为 15 分钟 idle TTL、6 小时 absolute TTL；ticket transient probe 的成功或普通失败结果缓存 30 秒。最终 ticket CAS 使用取得写锁后重新计算的 effective time，等待写锁不会顺带延长票据有效期。
- 文件代际：数据库 probe 代际目前由 `size + mtime` 构成。代际变化时 `UpsertFile` 清空旧 probe，数据库 trigger `trg_files_generation_clears_subtitles` 删除该文件全部 subtitle 行（包括 external/driver 行）。非空 probe metadata 必须携带 `FileVersion`，metadata 事务内再次核对当前 size/mtime，旧代际不得写入。
- 字幕清理 trigger：同一代际内，后台 probe 只替换并清理本次 probe 发现的 embedded 行，保留 external/driver 行。ticket finalize 对本次候选字幕执行 upsert 并生成 grant ID 快照，但不会删除已消失的外部或 embedded 候选，这是后续待收敛项。
- ticket metadata 原子提交：finalize 在同一个播放写事务内完成 `files.probe`、本次候选字幕 upsert、`subtitle_ids_json` grant 快照和 ticket state/version CAS。事务先锁票并校验 state、version、expiry；撤销若先取得 writer lock，则不会留下 probe 或字幕缓存副作用；ticket CAS 失败时 metadata 一并回滚。
- 后台 probe 原子写入：`doProbe` 使用单事务同时写 `files.probe` 和 embedded subtitle 行，并以开始 probe 时的 `size + mtime` 做 CAS。代际已变化时整体不写，也不更新内存旧 probe；`ErrMediaFileChanged` 不进入 30 秒失败冷却。普通缓存持久化错误只记录脱敏日志，本次已成功的播放 probe 仍保持可用。
- 阶段预算：Create 只读预检 20 秒、后台 probe 总预算 150 秒、finalize 20 秒、失败收敛清理 3 秒；外部字幕 discovery watchdog 为 5 秒。watchdog 到期时省略 external/driver 字幕并继续 finalize，父 context 取消或超时仍是致命错误。非协作 driver 留下的 goroutine 无法强制终止，重复请求可能积累 goroutine/连接。
- audio-only 与 failure code：transient probe 可以返回非空 audio-only probe 加 `errNoVideoStream`，finalize 最终收敛为 `unsupported/unsupported_media`。公开 failed code 包括 `source_unavailable`、`probe_timeout`、`probe_interrupted`、`probe_finalize_failed`；未知内部 failed code 收敛为 `source_unavailable`，避免泄漏内部细节。
- 工具快照：`SetTools` 在 `toolMu` 下原子发布 ffmpeg、ffprobe 和 probe window；`SetBins` 保持兼容并保留当前 window。`currentProber`/`currentFFMpeg` 返回不可变发布快照，probe 热路径不直接读取可变的 `Cfg.Playback`。
- 缓存失效：源配置更新、Bilibili 登录成功和 refresh token 更新成功后调用 `InvalidateSourceCaches(sourceID)`，按 source 清除 resolve/parts 缓存并保留 per-file 解析锁；该函数不清 keyframe cache，也不重建 source list/driver 实例。
- ticket probe 上限：纯 probe registry 以 `fileID + size + mtime` 为键，同 key 单飞，内存条目最多 1024；真正执行 ticket ffprobe 的并发最多 4。registry 满且仍有可淘汰的已完成项时只淘汰最旧一项；全为 in-flight 时调用者等待容量或自身 context 结束，不绕过 registry。调用者自身取消会删除 job 并让 live follower 重新选主；driver 在仍有效的调用者 context 下返回 `Canceled/DeadlineExceeded` 按普通失败缓存 30 秒，不触发热重试。
- Create 在写票前执行只读 `ProbeFor` 预检：请求取消返回 408、deadline 返回 504，且不创建 ticket；非 context 的源 probe 失败仍创建 ticket 并返回 `201 failed/source_unavailable`。`ProbeFor` 成功时同步 finalize；需要 ffprobe 时返回 `202 probing`。
- finalize HTTP/status 语义：普通 finalize 失败返回 500，但已设置的 `Location` 指向终态 ticket；status GET 返回 200，ticket 为 `failed/probe_finalize_failed`。请求取消和超时分别返回 408/504，并尽力将 ticket 收敛为 `probe_interrupted`/`probe_timeout`；若 3 秒收敛本身失败，ticket 可能保留 `probing` 到 TTL。
- 播放写事务由单一 writer 信号量串行化；10 秒预算覆盖信号量、连接池、`BeginTx`、SQLite 锁等待和业务函数。专用连接临时将 `busy_timeout` 设为 0 并做 1-25 ms 指数退避，成功后恢复 `busy_timeout=10000` 再提交；rollback/恢复失败会丢弃连接。deadline/canceled 在 API 层分别映射为 504/408。
- 出站连接设置 TLS handshake 15 秒、ResponseHeader 60 秒、CONNECT 握手 15 秒（或更早的请求 deadline），不设置会截断长媒体流的整请求总超时。
- ticket probe 与字幕发现改为纯读；启动时未过期的遗留 `probing` ticket 收敛为 `failed/probe_interrupted` 并 bump version，已过期的转为 `expired`，使旧 worker 无法完成且恢复幂等。
- `request_issuance_fallback_count` 只表示当前签发调用内已经发生的服务端候选回退；`remaining_transports` 表示当前仍可尝试的候选；`ticket_attempt_ordinal` 与 `last_claimed_transport` 用于识别跨 refresh 的 claim/传输顺序变化。`last_claimed_transport` 证明上一轮 claim 选择，不证明客户端已成功播放；没有客户端失败报告时，不能把“传输序号变化”直接判定为客户端 playback fallback。
- 本机验证命令：

```powershell
Set-Location C:\Users\Administrator\DoriyaMedia
Push-Location .\scripts\protocolcheck
go test -mod=readonly ./... -count=1
go vet -mod=readonly ./...
Pop-Location
.\scripts\check-protocol.ps1
go test ./internal/api ./internal/db ./internal/source ./internal/media -count=1
go vet ./internal/api ./internal/db ./internal/source ./internal/media
go test ./... -count=1
.\scripts\device-auth-e2e.ps1
.\scripts\playback-e2e.ps1
git diff --check
```

`go test -race ./internal/api ./internal/media` 仍是验收命令；本机已安装 GCC，并于 2026-10-28 通过根模块 `go test -race ./...`。

`playback-e2e.ps1` 使用确定性 probe 元数据；本机有 ffmpeg 时会生成合成 MP4，并实际获取 HLS playlist/segment、校验 MPEG-TS 字节后用 ffmpeg 解码，缺少 ffmpeg 时该段计为 1 个 skip。脚本还验证 `media_response` 固定字段、HEAD/playlist 字节口径、HLS capability 的 query 拒绝与同 capability segment URI，以及 ticket ID 不入日志。远程分支使用运行时生成的标准 `207 Multi-Status` WebDAV fixture 和 WebDAV source，覆盖 server-proxy 完整 GET、HEAD、Range 206、416、matching/stale If-Range、上游忽略 Range、畸形 206、源站 5xx，以及 Headers 和 ContentHeaders 中的 Authorization、Cookie、Set-Cookie、WWW-Authenticate、Location、X-Source-* 等 canary 不泄露。direct-dash 分支以 `-tags playbacke2e` 构建服务端，通过真实 admin source/library/scan API 创建确定性多流 source，验证能力与 flag 矩阵、设备级禁用/恢复、跨设备隔离、双流 DTO、白名单 Header、签发阶段非法 Header 原子回退和 HLS 关闭 fail-closed；它不验证真实 Bilibili URL、客户端合流或音画同步。脚本仍不覆盖真实 OpenList/Bilibili 来源、30 分钟持续播放、网络切换或客户端播放器，当前 GitHub Actions 也不运行该脚本，因此不能替代真实媒体验收。

**待真机/真实媒体验收**

- `direct-http` 连续播放 30 分钟时，服务端 media 出站字节为 0；控制 API 和遥测请求不计入。
- 真实来源 HLS playlist/segment 解码、20 次随机 seek、短断网/Wi-Fi 切换和 URL 过期恢复。
- OpenList、Bilibili 及不同出口 IP 的源站 Range/206/416/重定向异常注入。
- Windows 10/11 与 Android 9/12/14 真机矩阵。

**残余限制**

- DB 文件代际仅由 `size + mtime` 构成，同大小且 mtime 未变化或恢复为原值的原地替换仍可绕过；本地 HTTP 的 volume/file identity/change time 强 ETag 不能替代数据库 probe 代际。
- ticket subtitle grant 保存的是可变 subtitle row ID，而不是签发时内容快照；字幕行后续更新没有内容哈希或版本绑定。ticket metadata 对已消失的外部/同图片段候选只 upsert，不清理旧候选。
- 非协作 MultiPart driver 若永久不返回，其调用 goroutine、同 key 解析锁和锁注册表引用会保留，以阻止同 key 并发驱动；调用方和 ticket probe slot 仍可按 context 退出。
- 非协作 subtitle driver 的超时 goroutine 无法强制结束，重复请求可能积累 goroutine/连接；finalize 的 3 秒失败收敛若也失败，ticket 可能停留 `probing` 到 TTL。
- FFmpeg/FFprobe 仍接收普通本地路径并在子进程内重开；hardlink containment、根目录父级 ACL 和文件原地改写无法由当前句柄化完全保证。
- 已验证的是本地打开句柄跨 rename/替换稳定；probe→FFmpeg/HTTP 重开期间的路径替换、hardlink 和父目录 ACL 变化仍未验证。
- 启动收敛假设单个服务进程独占 SQLite 数据库；多实例共享同一数据库时仍需持久化 probe owner/lease，不能用“启动即失败全部 probing”替代代际协议。
- ticket ffprobe 的 4 路限制只覆盖 `ProbeTransient`；legacy `EnsureProbe`、后台 pre-probe 和 FFmpeg 子进程有各自执行路径，不能把该数字理解为全进程所有媒体子进程总上限。
- Playback probe runner 使用包级测试 seam；现有相关测试不得加 `t.Parallel()` 并发覆盖该 seam。
- media/HLS 响应矩阵目前由 OpenAPI + protocolcheck 静态/mutation test 校验，尚未为每个状态码搭建独立 HTTP 集成矩阵；更新后的 GitHub Actions workflow 也尚未在远端实际运行。
- 服务端只保证其代理和 DTO 不把敏感 Header 带到跨源目标；`direct-http` 客户端后续重定向的 Header 剥除必须由原生客户端实现并测试。
- 当前 refresh 没有客户端失败原因上报，服务端只能观察签发顺序，不能单独计算“客户端实际播放失败后发生 fallback”的播放占比；该指标需要后续固定枚举的客户端遥测和稳定 trace 关联。

**回滚**

- 新语义按 transport 级 flag 开启；失败时切回现有 proxy/HLS 处理。

**日志禁区**

- 禁止输出 Authorization、Cookie、Referer 全量、签名 URL、query token、原始 206 响应头和用户密码。

### S5：`direct-dash` 与中继授权边界

**当前状态**

- `direct-dash` 服务端契约、默认关闭实现、协议样例、自动测试和进程级 HTTP E2E 已落地。
- E2E 使用确定性多流 fixture 验证签发、能力/flag/设备策略矩阵、跨设备隔离与恢复、Header 白名单、签发阶段原子回退与 HLS 关闭 fail-closed；它不代表真实 Bilibili 双流或客户端合流通过。
- 真实 Bilibili 双流、客户端本地合流、Windows/Android 真机与网络矩阵均未执行，S5 未关闭。
- `local-relay` 尚未开始实现，仅保留 `local_relay_enabled` 发现 flag。

**目标**

- 完成 `direct-dash` 从真实双流源签发到客户端本地合流的端到端验收，明确 video/audio URL、Header、轨道和同步信息。
- 只在服务端 flag、设备策略、多流来源资格和客户端 `supports_dash_merge=true` 全部满足时签发。
- 保留 `server-hls` 兜底能力，但仅当 `server_hls_v1_enabled=true` 时可用。
- 保留未来 `local-relay` 的短期刷新和凭据隔离边界设计，不让中继读取主账号凭据。

**非目标**

- 不在服务器端做客户端本地合流。
- 不承诺 Bilibili 在所有网络和地区都能直连。
- 不允许中继接受任意目标 URL 或绑定公网地址。
- 不在当前收尾中实现 `local-relay`；它未开始且不属于当前可签发 transport。

**依赖**

- S3、S4；W0/A0 至少一个双流 Spike 已通过。W0/A0 当前已有机制原型，但都尚未完成真实双流、同步、seek 和长时验收，因此 S5 端到端验收仍被阻塞；服务端默认关闭实现可以保留合并，但不能据此宣称 S5 关闭。

**目标目录**

- `internal/api/playback_ticket_handlers.go`
- `internal/media/transport.go`
- `internal/source/bilibili.go`
- `protocol/schemas/playback-ticket.schema.json`

**完成与验证**

- 服务端双流响应包含两个明确角色，不把视频 URL 冒充完整播放源。
- 票据不含 Bilibili Cookie；Header 只含白名单 Referer/UA 等。
- 省略 `supports_dash_merge` 或显式设为 false 时，不得签发 `direct-dash`；为 true 时仍需服务端 flag、设备策略和多流来源资格全部通过。
- 关闭 `direct_dash_enabled` 时，同一文件只能选择仍启用的候选；若 `server_hls_v1_enabled=true` 则回退 `server-hls`，否则继续到其他候选或 `unsupported`。
- 进程级 HTTP E2E 已覆盖以上确定性 fixture 分支；真实 Bilibili 视频/音频双流、客户端本地合流、30 分钟连续播放、20 次随机 seek 和音画同步仍待验收。
- 验证命令：

```powershell
Set-Location C:\Users\Administrator\DoriyaMedia
.\scripts\check-protocol.ps1
go test ./internal/api ./internal/media ./internal/source
go test ./...
git diff --check
```

**设备/网络矩阵**

- 当前均未执行：S5 真实 Bilibili 双流，Windows 与 Android 客户端本地合流，至少 30 分钟和 20 次随机 seek。
- 当前均未执行：大陆、港澳台、海外出口各测一次。
- 当前均未执行：URL 过期、地区失败、单流 403、双流时长不一致各测一次。

**回滚**

- 默认关闭 `direct_dash_enabled`；客户端只能使用其他已启用候选。只有 `server_hls_v1_enabled=true` 时才可使用 server-HLS，否则显示明确的 `unsupported`/重试原因。
- `local_relay_enabled` 继续保持 false；因为 local-relay 尚未实现，关闭它不会改变当前播放路径。

**日志禁区**

- 禁止 Bilibili Cookie、账号信息、完整 CDN URL、签名参数、Referer/UA 以外的 Header、ticket secret 和中继 token。

### W0：Windows 播放内核 Spike

**目标**

- 用最小原型比较 Tauri 2 + mpv 与 WinUI 3 + LibVLCSharp。
- 验证 MKV、HEVC 10-bit、FLAC、ASS/PGS、Range、Header、双 URL 合流、全屏和 DPI。
- 记录选型和许可证结论。

**非目标**

- 不实现正式登录、媒体库和进度 UI。
- 不把原型代码合入产品目录。
- 不在许可证未确认时制作可分发安装包。

**依赖**

- S0；`ticket-direct-dash.json` 双流协议样本已存在。W0 的 Tauri 与 WinUI 机制原型已创建并通过本机构建/冒烟，但真实媒体、逐流 Header、双流同步和稳定性验证尚未开始。

**目标目录**

- `spikes/windows-player/`
- `docs/adr/0002-windows-player-engine.md`

**完成与验证**

- 当前状态：Tauri 已通过构建、32 个 Rust tests（另 1 个真实 mpv 测试需显式启用）、真实 mpv 空载 IPC、debug exe 构建和交互会话 GUI 启动/退出 smoke，并已实现最小环境继承、128 位随机管道名、服务端 PID 校验、空 capability、固定 5 秒 IPC 响应期限、`start-file`/`file-loaded`/`end-file` 代次状态机，以及子进程退出、控制超时和失败后的有界清理；完整 DACL/SID/session 对端信任和 Job Object 崩溃联动清理仍未完成。WinUI 已通过 0 warning/error 构建、91 个 xUnit tests、LibVLC `3.0.19` probe、app smoke 和合成 loopback 双源 smoke，支持 `Media.AddSlave` 与 LibVLC 可安全表达的双流共享 Header。两者均未加载真实媒体，WinUI 尚不支持逐流 Header，因此 W0 未关闭。
- 两个候选方案至少各完成 S1、S2、S3、S4、S5 的可行性测试。
- 通过标准：30 分钟无花屏；20 次 seek 正常；音画差不超过 100 ms；全屏和多显示器正常。
- 许可证结论明确到 mpv、FFmpeg、使用的解码器和 HEVC/AAC 分发义务。
- 验证命令（原型形成后执行）：

```powershell
Set-Location C:\Users\Administrator\DoriyaMedia\spikes\windows-player\tauri-mpv
pnpm install --frozen-lockfile
pnpm build
Set-Location .\src-tauri
cargo fmt --all -- --check
cargo clippy --all-targets --locked -- -D warnings
cargo test --locked

Set-Location C:\Users\Administrator\DoriyaMedia\spikes\windows-player\winui-libvlcsharp
.\verify.ps1

Set-Location C:\Users\Administrator\DoriyaMedia
git diff --check
```

**设备/网络矩阵**

- Windows 10 22H2、Windows 11 当前稳定版。
- Intel 核显、NVIDIA、AMD 至少各一台。
- 单屏/双屏、100%/150%/200% DPI、睡眠恢复、Wi-Fi/热点。

**回滚**

- 删除 `spikes/windows-player/`；ADR 保留为决策记录。

**日志禁区**

- 禁止把测试账号、Token、Cookie、完整测试 URL/query、原始 Header 或媒体内容写入日志/报告。

### W1：Windows 壳、认证与媒体库

**目标**

- 建立 Tauri 2 应用，复用现有 Vue UI 的首页、详情、设置外壳和视觉规范。
- 通过 Rust 命令层访问 v1 API，令牌只驻留 Rust/DPAPI 安全存储。
- 完成设备授权、媒体库浏览、详情和进度同步。

**非目标**

- 不实现原生视频播放。
- 不复制一份长期漂移的 Vue 页面。
- 不给 WebView 开放任意服务器 URL 和任意 Header 请求。

**依赖**

- S1、S2、S3；W0 选型结论。

**目标目录**

- `clients/windows/`
- `clients/windows/src-tauri/`
- `webui/src/platform/`
- `webui/package.json`
- `pnpm-workspace.yaml`（如采用 workspace）

**完成与验证**

- Web 模式继续使用现有 fetch；
- Tauri 模式通过 Rust 命令访问 `/api/v1`，避免跨域和令牌暴露。
- 关闭/重开应用后登录、设备 ID、首页和进度仍然正确。
- 验证命令：

```powershell
Set-Location C:\Users\Administrator\DoriyaMedia
pnpm --dir webui build
Set-Location .\clients\windows
pnpm install --frozen-lockfile
pnpm typecheck
pnpm test
cargo fmt --all -- --check
cargo clippy --workspace --all-targets -- -D warnings
cargo test --workspace
pnpm tauri build --debug
git diff --check
```

**设备/网络矩阵**

- Windows 10/11，WebView2 当前稳定版和系统最低可用版。
- 无网启动、服务端不可达、Token 过期、设备被吊销、进度冲突。

**回滚**

- 关闭 Windows 安装通道；Web UI 路径不依赖 Tauri 改动。
- 新平台适配层保持 feature flag，可回退到纯 Web 播放器。

**日志禁区**

- 禁止 WebView、Rust、安装器或崩溃报告记录密码、Turnstile、access/refresh token、DPAPI blob、Cookie、完整 API URL query 和响应中的签名 URL。

### W2：Windows 基础播放纵切

**目标**

- 把 mpv sidecar 作为独立进程嵌入或覆盖在 Tauri 窗口上。
- 支持 `direct-http`、`server-proxy`、`server-hls`、Range、暂停、seek、音量、全屏和字幕。
- 403/410/过期时刷新 ticket；失败自动进入服务端兜底。

**非目标**

- 不在第一版实现本机中继。
- 不默认启用 libmpv 兼容构建。
- 不在 mpv 崩溃时拖垮整个应用。

**依赖**

- W1、S3、S4。

**目标目录**

- `clients/windows/src-tauri/src/player/`
- `clients/windows/src-tauri/src/mpv/`
- `webui/src/views/Player.vue`
- `webui/src/platform/`

**完成与验证**

- S1、S2、S3、S4 全部可播放；直连 30 分钟时服务端 media 出站接近 0。W2 不负责 `direct-dash` 合流。
- mpv 进程崩溃后应用仍存活，并能提示回退。
- 验证命令：

```powershell
Set-Location C:\Users\Administrator\DoriyaMedia\clients\windows
pnpm typecheck
pnpm test
cargo fmt --all -- --check
cargo clippy --workspace --all-targets -- -D warnings
cargo test --workspace
pnpm tauri build --debug
```

- 手工执行 S1/S2/S3/S4 样本：起播、20 次随机 seek、暂停恢复、字幕切换、全屏和断网恢复；S5 由 W3 验收。

**设备/网络矩阵**

- Windows 10/11；Intel/NVIDIA/AMD。
- LAN、热点、不同出口、VPN、源 URL 到期。
- 单屏/多屏，DPI 100%/150%/200%，睡眠恢复。

**回滚**

- `native_player_enabled=false` 或 `mpv_player_enabled=false` 时回到 Web 播放器。
- 直连失败时强制 server-proxy/server-hls。

**日志禁区**

- 禁止记录源 URL 完整值、query、Header 全量、Cookie、Token、mpv 命令行中的凭据和崩溃转储中的媒体路径。

### W3：Windows Header、中继与双流

**目标**

- 用 mpv Header 注入或 loopback relay 处理 Header/URL 刷新。
- 支持 `direct-dash`：视频主源 + `audio-add` 音频源、轨道发现和字幕。
- 双流失败、音画漂移或地区失败时自动回退 server-hls。

**非目标**

- 不把 local relay 做成公开代理。
- 不自行猜测 CDN URL、地区或账号权限。
- 不在客户端重编码视频。

**依赖**

- W2、S5、S4。

**目标目录**

- `clients/windows/src-tauri/src/relay/`
- `clients/windows/src-tauri/src/player/`
- `clients/windows/src-tauri/src/ticket/`
- `protocol/fixtures/`

**完成与验证**

- 中继只监听 `127.0.0.1`，随机端口，一次性 token；不转发到任意目标。
- Header 注入不把账号 Cookie 写入 mpv 参数或日志。
- 双流 30 分钟、20 次 seek，音画差不超过 100 ms。
- 验证命令：

```powershell
Set-Location C:\Users\Administrator\DoriyaMedia\clients\windows
pnpm test
cargo fmt --all -- --check
cargo clippy --workspace --all-targets -- -D warnings
cargo test --workspace
pnpm tauri build --debug
```

**设备/网络矩阵**

- S3、S4、S5。
- 大陆/港澳台/海外出口。
- Wi-Fi 切热点、CDN URL 过期、单流 403、音频流中断。

**回滚**

- 分别关闭 `local_relay_enabled` 和 `direct_dash_enabled`；恢复 mpv 直接 Header，或仅在 `server_hls_v1_enabled=true` 时使用 server-HLS。local-relay 尚未实现，关闭其 flag 不改变当前路径。
- relay 监听异常时应用立即退出中继并回到服务端兜底。

**日志禁区**

- 禁止中继日志记录 Authorization、Cookie、完整目标 URL、查询签名、Referer/UA 原始值、ticket 和一次性 relay token。

### W4：Windows 系统集成与发布

**目标**

- 完成媒体键、任务栏、睡眠恢复、DPI、字幕菜单、断网恢复、诊断和更新。
- 提供 Authenticode 签名安装包、签名更新清单、降级保护和可回滚版本。
- 产出用户可操作的错误提示，不泄露源站凭据。

**非目标**

- 不做自动后台转码。
- 不支持 Windows ARM64 作为首发硬要求。
- 不默认自动升级到未签名构建。

**依赖**

- W2、W3。

**目标目录**

- `clients/windows/src-tauri/`
- `.github/workflows/client-windows.yml`
- `scripts/`

**完成与验证**

- 媒体键、任务栏控制、睡眠后恢复、DPI 切换、多屏幕全屏、断网恢复通过。
- 安装包、更新清单和校验和均签名；禁止降级到未批准版本。
- 验证命令：

```powershell
Set-Location C:\Users\Administrator\DoriyaMedia\clients\windows
pnpm typecheck
pnpm test
cargo fmt --all -- --check
cargo clippy --workspace --all-targets -- -D warnings
cargo test --workspace
pnpm tauri build
Set-Location C:\Users\Administrator\DoriyaMedia
git diff --check
```

**设备/网络矩阵**

- Windows 10 22H2、Windows 11 当前稳定版。
- x64；NVIDIA/AMD/Intel；单/双屏；100%-200% DPI。
- 正常安装、升级、签名失败、更新中断、回滚和卸载。

**回滚**

- 撤回更新清单并发布上一个签名版本。
- `native_player_enabled`、`direct_http_enabled` 和 `direct_dash_enabled` 可远程急停。

**日志禁区**

- 安装器、更新器、崩溃转储和遥测均不得记录令牌、Cookie、DPAPI blob、完整源 URL/query、原始 Header 或用户媒体绝对路径。

### A0：Android 播放内核 Spike

**目标**

- 用最小应用验证 Kotlin + Compose + Media3 的实际能力。
- 比较 Media3、Media3 + FFmpeg extension、libmpv 在目标真机上的效果。
- 决定第一版是否只发 Media3，还是同时准备 libmpv 兼容 flavor。

**非目标**

- 不实现正式 UI、账号、媒体库或发布。
- 不把 Flutter 作为 Android 主线。
- 不承诺所有 MKV/E-AC3/PGS 在 Android 上可硬解。

**依赖**

- S0；`ticket-direct-dash.json` 双流 fixture 已存在。A0 的 Media3 机制原型已创建并通过 API 37 x86_64 ATD 的合成双流、逐流 DataSource 和重定向策略测试，但真实双流、真机和默认解码路径验证尚未开始。

**目标目录**

- `spikes/android-player/`
- `docs/adr/0003-android-player-engine.md`

**完成与验证**

- 当前状态：单 URL、白名单 Header、逐流 Header/容器 MIME、双 URL `MergingMediaSource`、同源重定向 Header 保留与跨源 fail-closed、播放意图、控制/seek、lifecycle 暂停与 PlayerView 解绑、脱敏日志已实现；lint 0 error/10 warnings、24 个 JVM tests、debug/test APK 构建、API 37 ATD `connectedDebugAndroidTest` 4/4 和冷启动冒烟通过。connected 测试覆盖 Ready、双轨、seek discontinuity、seek 后位置推进、播放/暂停、video surface、逐流输入和生产 MediaSource factory 独立 DataSource。ATD 的 goldfish H.264 解码缺陷使合成测试使用 `PREFER_SOFTWARE`，不能替代真机/默认硬件路径；真实 CDN 重定向链和逐流 Range 尚无端到端结果，真实 API 28/31/34 真机、默认硬件解码、真实 Bilibili 双流和长时矩阵仍未验收。
- S1-S5 在 API 28/31/34 真机上的结果表；当前尚未执行。
- 记录起播、seek、字幕、音画同步、CPU、电量和内存。
- 验证命令：

```powershell
Set-Location C:\Users\Administrator\DoriyaMedia\spikes\android-player
.\gradlew.bat :app:lintDebug :app:testDebugUnitTest :app:assembleDebug :app:assembleDebugAndroidTest
.\gradlew.bat :app:connectedDebugAndroidTest
git diff --check
```

**设备/网络矩阵**

- Android 9/12/14 真机，Qualcomm/MediaTek 至少各一台。
- arm64-v8a 为主，x86_64 模拟器只做 UI/网络冒烟。
- Wi-Fi、5G、热点、VPN、源站不同出口。

**回滚**

- 删除 spike 目录；ADR 保留结论。

**日志禁区**

- 不记录测试账号、Token、Cookie、完整测试 URL/query、Header 和媒体绝对路径。

### A1：Android 壳、认证与媒体库

**目标**

- 建立 Compose、MVVM、OkHttp、Keystore 和基础导航。
- 完成设备授权、首页、详情、季/集导航、进度同步和服务器地址设置。
- 支持明确的网络错误、账号禁用、设备吊销和版本升级提示。

**非目标**

- 不实现原生播放。
- 不收集不必要的设备标识。
- 不把 refresh token 存在普通 SharedPreferences。

**依赖**

- S1、S2、S3。

**目标目录**

- `clients/android/settings.gradle.kts`
- `clients/android/app/`
- `clients/android/core/network/`
- `clients/android/core/security/`
- `clients/android/feature/library/`

**完成与验证**

- 设备授权通过 Chrome Custom Tabs；令牌由 Keystore 保护。
- 登录、首页、详情、进度在进程重启和网络切换后正确。
- 验证命令：

```powershell
Set-Location C:\Users\Administrator\DoriyaMedia\clients\android
.\gradlew.bat :app:lintDebug :app:testDebugUnitTest :app:assembleDebug
.\gradlew.bat :app:connectedDebugAndroidTest
git diff --check
```

**设备/网络矩阵**

- Android 9/12/14 真机；Wi-Fi/5G。
- 首次安装、升级、Token 过期、设备吊销、服务器地址不可达。

**回滚**

- 关闭 v1 feature flag 或回退上一个 APK；Media3/播放器模块不参与该卡。

**日志禁区**

- 禁止记录密码、Turnstile token、access/refresh token、Keystore 明文、Cookie、完整 API URL/query、源站 URL。

### A2：Media3 基础播放

**目标**

- 使用 Media3 播放 `direct-http`、`server-proxy`、`server-hls`。
- 实现自定义 HTTP 数据源 Header 注入、Range、403 刷新、字幕和失败回退。
- 保持服务器直连时媒体出站为 0。

**非目标**

- 不在本卡默认做 `direct-dash` 合流。
- 不默认启用 libmpv。
- 不实现视频重编码。

**依赖**

- A1、S3、S4。

**目标目录**

- `clients/android/core/player/`
- `clients/android/feature/player/`
- `clients/android/core/network/`

**完成与验证**

- S1/S2/S3/S4 起播、暂停、seek、音轨、字幕；A2 不负责 `direct-dash` 合流，S5 由 A3 验收。
- 403 刷新 ticket 后从当前位置继续。
- 验证命令：

```powershell
Set-Location C:\Users\Administrator\DoriyaMedia\clients\android
.\gradlew.bat :app:lintDebug :app:testDebugUnitTest :app:assembleDebug
.\gradlew.bat :app:connectedDebugAndroidTest
git diff --check
```

**设备/网络矩阵**

- Android 9/12/14 真机。
- LAN、5G、热点、源站不同出口。
- 断网恢复、URL 过期、服务端 HLS 回退。

**回滚**

- 关闭 `native_player_enabled`，回到 WebView/Web 播放入口。
- 关闭 `direct_http_enabled`，强制 server-proxy 或 server-hls。

**日志禁区**

- 禁止输出源 URL/query、Cookie、Token、完整 Header、播放器内部请求头和媒体绝对路径。

### A3：Android 双流、兼容内核与后台播放

**目标**

- 使用 Media3 合流 `direct-dash`，验证音画同步和轨道选择。
- 根据 A0 结论接入可选的 libmpv/FFmpeg extension。
- 完成 MediaSession、音频焦点、通知和基础 PiP 体验。

**非目标**

- 不把 libmpv 作为默认主内核。
- 不在客户端做实时视频转码。
- 不把 Bilibili/OpenList 主凭据放进 Android 应用。

**依赖**

- A2、S5、A0。

**目标目录**

- `clients/android/core/player/`
- `clients/android/core/player/mpv/`
- `clients/android/feature/player/`

**完成与验证**

- S3/S4 30 分钟、20 次 seek；音画差不超过 100 ms。
- 后台播放 15 分钟、音频焦点切换、通知控制和 PiP 正常。
- 双流失败自动 server-hls。
- 验证命令：

```powershell
Set-Location C:\Users\Administrator\DoriyaMedia\clients\android
.\gradlew.bat :app:lintDebug :app:testDebugUnitTest :app:assembleDebug
.\gradlew.bat :app:connectedDebugAndroidTest
.\gradlew.bat :app:bundleRelease
git diff --check
```

**设备/网络矩阵**

- Android 9/12/14；Qualcomm/MediaTek。
- Bilibili 大陆/港澳台/海外出口。
- 后台、锁屏、来电、耳机拔出、Wi-Fi/5G 切换、PiP。

**回滚**

- `libmpv_fallback_enabled=false` 时只保留 Media3。
- `direct_dash_enabled=false` 时停止签发 `direct-dash`；仅当 `server_hls_v1_enabled=true` 时强制 server-HLS，否则显示明确的不可用/重试原因。
- 双内核状态异常时使用独立进程/独立播放会话并回到 Media3。

**日志禁区**

- 禁止 Bilibili Cookie、OpenList Token、完整 CDN URL/query、ticket、refresh token、播放器命令行和媒体绝对路径。

### A4：Android 发布与系统门禁

**目标**

- 完成 Android 签名、AAB/APK、灰度、崩溃脱敏、更新说明和设备兼容矩阵。
- 实现后台播放、音频焦点、PiP、字幕菜单、网络切换恢复和存储/电量诊断。
- 发布前完成 Play Integrity、证书固定等可选强化评估，但不把它们当作首版硬依赖。

**非目标**

- 不默认开启 libmpv。
- 不做未授权的崩溃内容上传。
- 不承诺所有 Android OEM 解码器完全一致。

**依赖**

- A2、A3。

**目标目录**

- `clients/android/`
- `.github/workflows/client-android.yml`
- `docs/`

**完成与验证**

- 签名 APK/AAB 可安装、升级、回滚；崩溃日志已脱敏。
- 后台 15 分钟、PiP、通知控制、耳机/音频焦点、Wi-Fi/5G 切换通过。
- 验证命令：

```powershell
Set-Location C:\Users\Administrator\DoriyaMedia\clients\android
.\gradlew.bat :app:lintRelease :app:testReleaseUnitTest :app:bundleRelease :app:assembleRelease
.\gradlew.bat :app:connectedDebugAndroidTest
Set-Location C:\Users\Administrator\DoriyaMedia
git diff --check
```

**设备/网络矩阵**

- Android 9/12/14 真机，另加至少一台主流国产 OEM 设备。
- Wi-Fi、5G、热点、VPN、不同出口；后台/锁屏/PiP/来电/断网恢复。
- arm64-v8a 必须；x86_64 模拟器仅冒烟；是否发布 armeabi-v7a 由包体和市场决定。

**回滚**

- 保留上一个签名 APK/AAB 和服务器兼容 flag。
- 通过服务器 feature flag 急停直连、DASH 或 libmpv，而不要求用户立即卸载。

**日志禁区**

- 禁止记录密码、Turnstile、access/refresh token、Keystore 密文、Cookie、完整 URL/query、Header、媒体路径和可还原用户行为的设备指纹。

### X1：跨客户端 CI、版本与发布

**目标**

- 为服务端、协议、Android、Windows 增加路径过滤 CI。
- 使用不同标签发布不同产物，避免服务端、Android、Windows 版本互相覆盖。
- 产生可验证的校验和、SBOM 和发布说明。

**非目标**

- 不重写现有服务端发布流程。
- 不在 PR 中暴露签名密钥。
- 不让一个客户端的构建失败阻塞无关客户端的紧急服务端修复。

**依赖**

- S1、W1、A1 的最小构建存在。

**目标目录**

- `.github/workflows/ci.yml`
- `.github/workflows/client-windows.yml`
- `.github/workflows/client-android.yml`
- `.github/workflows/release.yml`
- `scripts/`

**完成与验证**

- 服务端仍执行 `go vet ./...` 和 `go test ./...`。
- 协议 job 执行 schema/OpenAPI fixture 校验。
- Windows job 使用 `windows-latest`、固定 Rust/Node/pnpm 版本，执行 cargo/pnpm 测试和构建。
- Android job 使用固定 JDK/Android SDK，执行 lint、unit test 和 assemble。
- 标签规则：
  - 服务端 `v*`
  - Windows `windows-v*`
  - Android `android-v*`
- 本地验证：

```powershell
Set-Location C:\Users\Administrator\DoriyaMedia
.\scripts\check-protocol.ps1
Set-Location .\clients\windows
cargo fmt --all -- --check
cargo clippy --workspace --all-targets -- -D warnings
cargo test --workspace
Set-Location ..\android
.\gradlew.bat :app:lintDebug :app:testDebugUnitTest :app:assembleDebug
Set-Location ..\..
git diff --check
```

**设备/网络矩阵**

- CI：Ubuntu（Go/协议）、Windows（Tauri）、Ubuntu（Android SDK）。
- 发布前真机：Windows 10/11；Android 9/12/14。
- 需要端到端验证至少一次从 tag 到下载、签名校验、安装、升级和回滚。

**回滚**

- 回滚 workflow 版本或关闭签名发布 job；不删除已发布的哈希和 SBOM。
- 客户端更新清单可立即停止分发，但不能依赖它撤销已发出的直链。

**日志禁区**

- CI 日志不得打印签名密钥、部署 Token、源站 Cookie/Token、更新私钥、完整下载 URL query、用户媒体 URL。

## 9. 跨端样本与契约

### 9.1 固定样本

样本只存描述，不存真实凭据：

```json
{
  "id": "S1-openlist-mp4",
  "kind": "openlist",
  "container": "mp4",
  "video": "h264",
  "audio": ["aac"],
  "features": ["range"],
  "source_url": null,
  "secret_ref": "DORIYA_SAMPLE_S1"
}
```

必须覆盖：

- S1：OpenList MP4 H.264/AAC，Range 正常。
- S2：OpenList MKV H.264/EAC3。
- S3：HEVC 10-bit/FLAC，带 ASS/PGS。
- S4：真实 Bilibili 视频/音频 DASH 双流。
- S5：Referer/UA 双流成功样本，以及 Authorization 等非白名单 Header、200/206/416、过期、重定向异常和单流失败的负向回退样本。

真实 URL 放在环境变量或 CI secret 中，测试失败只输出样本 ID 和错误类别。

### 9.2 Golden fixtures

每个 fixture 必须同时用于：

- 服务端 Go handler 测试。
- OpenAPI/JSON Schema 校验。
- Kotlin DTO/解析测试。
- TypeScript/Rust DTO 测试。

至少包含：

- `meta.json`
- `device-auth-start.json`
- `device-auth-token.json`
- `ticket-probing.json`
- `ticket-direct-http.json`
- `ticket-server-proxy.json`
- `ticket-server-hls.json`
- `ticket-direct-dash.json`
- `error-device-revoked.json`
- `error-ticket-expired.json`
- `error-rate-limited.json`

`ticket-direct-dash.json` 已加入并通过静态校验，`playback-e2e.ps1` 也已覆盖确定性双流签发与回退；它们仍不代表真实 Bilibili 双流、客户端本地合流或 S5 验收完成。

## 10. 验收门禁

### 10.1 服务端

- `go vet ./...`、`go test ./...`、关键包 race test 全绿；本机于 2026-10-28 通过 `go test -race ./...`。
- OpenAPI/Schema 和 fixture 校验全绿，包含 media/HLS 精确响应矩阵、HEAD bodyless、Range/OWS、206 `Content-Range`、ETag/If-Range 和公开 failure code 的 mutation test；`check-protocol.ps1` 必须先执行 protocolcheck 自测。
- 票据跨用户、跨设备、跨文件、跨用途、过期重放全部拒绝。
- server-proxy/本地/字幕 HEAD、v1 单段 Range、416、坏 upstream 206 的低层测试全绿；HLS HEAD 的 405 例外符合契约。
- 异步 probe 的撤销后零副作用、metadata+ticket 原子提交、旧 worker 失效和启动收敛幂等测试全绿。
- ticket probe registry 满额等待、单条最旧缓存淘汰、4 路执行上限、driver context 失败 TTL 和调用者取消重新选主测试全绿。
- 旧 `/api` 自动回归测试通过；Web 页面手工播放回归尚未执行。
- direct 播放 30 分钟，服务器 media 出站字节为 0；控制 API 请求不计入。
- 遥测能区分 `response_bytes` 与 `server_media_bytes`，并用 `request_issuance_fallback_count`、`ticket_attempt_ordinal`、`last_claimed_transport` 区分请求内回退和跨 refresh 的 claim/传输变化。
- 协议、设备授权和播放 E2E 分别使用 `check-protocol.ps1`、`device-auth-e2e.ps1`、`playback-e2e.ps1`。

### 10.2 客户端

- Windows：S1-S5 在 Windows 10/11、Intel/NVIDIA/AMD、单/多屏和 DPI 矩阵通过；当前尚未执行。
- Android：S1-S5 在 Android 9/12/14 和 Qualcomm/MediaTek 真机通过；当前尚未执行。
- 真实 Bilibili 双流、客户端本地合流以及 Windows/Android 网络矩阵当前均无结果，因此 S5 门禁未关闭。
- 直连失败在 5 秒内进入服务端兜底，或给出可操作的地区/网络原因。
- 30 分钟连续播放无花屏；20 次随机 seek 成功率 100%；音画差不超过 100 ms。
- 后台、锁屏、音频焦点、PiP（Android）和睡眠恢复、媒体键（Windows）通过。
- 关闭 feature flag 后，客户端不会崩溃，能降级到服务端代理/HLS。

### 10.3 安全

- 客户端包、日志、崩溃报告和 URL 中不存在 OpenList Token、Bilibili Cookie、WebDAV Basic、refresh token。
- local relay 只监听 `127.0.0.1`，随机端口、一次性 token、目标白名单。
- 服务端代理不会把 Authorization/Cookie 带到跨源重定向目标；direct-http 客户端必须实现同等剥除并通过原生测试。
- 直链签发后不可实时吊销的限制在 UI 和文档中明确。

### 10.4 发布

- Windows：Authenticode、更新清单签名、哈希、SBOM、安装/升级/回滚验证。
- Android：release signing、AAB/APK、版本升级/回滚、签名校验。
- 服务端客户端 flag 可按设备急停。
- 发布说明明确“直连是 best-effort，不保证跨网络/跨地区成功”。

## 11. 中止或降级条件

出现以下情况时暂停对应能力，而不是用更多隐藏重试掩盖：

- S0 测出目标平台真实直连收益过低，且服务端带宽节省不足以抵消客户端复杂度。
- OpenList 直链在大多数目标网络因出口 IP 绑定失败，且无法用合法方式解决。
- Bilibili 双流在目标地区/网络无法稳定合流或 seek。
- mpv/FFmpeg 许可证或 HEVC/AAC 分发义务无法接受。
- Media3 在目标 Android 机型上无法稳定处理目标 MKV/音频/字幕，且 libmpv 包体、功耗或许可证不可接受。
- 设备登录无法在保留 Turnstile 的前提下提供可接受体验。
- 票据/中继引入可复现的权限绕过、凭据泄露或开放代理风险。

降级顺序：

```text
direct-http -> server-proxy -> server-hls（仅 server_hls_v1_enabled=true）-> unsupported
direct-dash -> server-hls（仅 server_hls_v1_enabled=true）-> unsupported
```

不是：

```text
无限重试 -> 静默黑屏 -> 把完整凭据交给客户端
```

## 12. 回滚设计

### 12.1 服务端

- 旧 `/api` 永远保留到所有客户端退场。
- 新路由按 `client_v1_enabled`、`playback_ticket_enabled` 和设备级 flag 控制。
- `direct_http_enabled`、`server_proxy_v1_enabled`、`server_hls_v1_enabled`、`direct_dash_enabled` 可以独立关闭。
- ticket/media session 只保存哈希和状态，不依赖客户端缓存才能失效。
- 发生异常时退回旧 Web 播放链路；v1 的 `direct-dash` 仅在 `server_hls_v1_enabled=true` 时回退到 server-HLS，不能回退到 `server-proxy`。

### 12.2 客户端

- 保留原生播放器的 feature flag。
- Windows 可以打开 Web 播放器 fallback。
- Android 可以关闭 libmpv、直连和 DASH；仅当 `server_hls_v1_enabled=true` 时走 Media3 + server-HLS，否则显示明确的不可用/重试原因。
- 安装器/APK 永远允许回到上一个已签名版本；更新清单禁止降级到未知版本。

### 12.3 发布

- 服务端标签：`v*`。
- Windows 标签：`windows-v*`。
- Android 标签：`android-v*`。
- 发布产物附带 SHA256 和 SBOM。
- 先内部设备、再少量用户、最后扩大灰度；每次扩大都要看失败率、fallback 率和 media 出站字节。

## 13. 需要产品最终确认的决策

这些问题不阻塞 S0/S1，但应在 W1/A1 前定稿：

1. Windows 首发最低系统：建议 Windows 11 主支持、Windows 10 22H2 兼容支持或明确不支持。
2. Android 最低版本：建议先按 Android 9/API 28 做能力矩阵，正式 minSdk 由 SDK 和商店要求决定。
3. Android 发布渠道：GitHub/官网 APK、Google Play、国内商店，还是先内部灰度。
4. 是否允许 Windows 分发含 libmpv/FFmpeg 的安装包，以及使用 LGPL-only 还是 GPL 构建。
5. 是否首版支持 WebDAV 直连。建议不支持，继续 server-proxy；主账号 Basic 不下发。
6. 设备数量、并发流、单用户带宽是否需要第一版就配额。
7. 是否允许用户手动导出脱敏诊断包；如果允许，需固定字段白名单，禁止自由文本附带 URL。

## 14. 给后续 Agent 的执行规则

每个 Agent 开始前必须：

1. 读取本文件、`README.md`、`protocol/` 当前版本和相关 Go 测试。
2. 只领取一张任务卡，不同时改协议、两个客户端和发布流程。
3. 先写/更新 contract fixture，再写服务端，再写客户端。
4. 不在代码、fixture、测试输出和提交信息中写入真实凭据或完整源 URL。
5. 不修改旧 `/api` 的既有语义，除非同一张卡明确要求并有关闭 flag。
6. 完成后必须报告：改动文件、执行过的命令、真机/网络结果、失败项、回滚开关和未解决风险。

第一张建议领取的任务卡是 `S0`，第二张是 `S1`。在 S0 的真实数据出来前，不应承诺具体直连比例。
