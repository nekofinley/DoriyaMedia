# ADR 0001: 原生客户端认证与播放票据安全边界

- 状态：Proposed
- 日期：2026-10-26
- 范围：Android / Windows 客户端规划中的 S2、S3，以及 S1 的契约依赖
- 结论性质：实现边界与安全审计；本文不表示相关能力已经落地

## 1. 决策摘要

1. Web 登录 session 与原生客户端 token 必须是两套独立凭据、两套独立存储、两套独立中间件。原生客户端不得获得或复用 Web Cookie session。
2. S2 采用设备授权 + 强制 PKCE S256 + 短寿命 access token + 单次轮换 refresh token。服务端只保存高熵 token 的哈希；refresh token 重放会使整个 token family 和对应设备失效。
3. 设备身份由服务端签发，并由 access token 绑定。请求体里的 `device_id` 只能用于一致性校验，不能作为身份来源。
4. S3 的 5 分钟播放票据只是一次播放决策记录，不是媒体会话，也不是万能文件签名。server-proxy / server-hls 必须另有 15 分钟空闲、6 小时绝对上限的授权媒体会话。
5. 传输由服务端依据用户、设备、文件、源能力、客户端能力和 feature flag 决定。客户端只能表达偏好，不能强制越过不可用或未授权传输。
6. `direct-http` 只能返回白名单 header。需要 Cookie、Authorization、WebDAV Basic、OpenList Token 或任意非白名单 header 的源不得签发 direct-http，必须回落到 server-proxy / server-hls。
7. 现有 `/api` 的 Cookie session、12 小时 `fileID+exp` 签名和 HLS 参数语义保持不变；v1 不得接受或签发这些旧签名作为客户端授权。
8. local relay 不属于 S3 首批传输的最低交付。其安全边界未实现并测试前，`local_relay_enabled` 必须保持关闭。

## 2. 审计快照

本节只记录审计时可见事实。S1 服务端工作当时仍在并发进行，因此涉及 v1 的部分以“进行中、未冻结”为准，不视为已完成契约。

### 2.1 认证、session 与数据库

| 现状事实 | 证据 | 安全含义 / 风险 |
| --- | --- | --- |
| Web 登录成功后签发 32 字节随机 token，有效期 30 天。 | `internal/api/user_handlers.go` 的 `sessionTTL`；`internal/api/auth_handlers.go` 的 `startSession`；`internal/auth/auth.go` 的 `NewToken`。 | token 是长期 bearer；与原生客户端的 15 分钟 access token 不是同一安全等级。 |
| session token 同时写入 HttpOnly Cookie 并出现在登录 JSON 响应中。 | `internal/api/auth_handlers.go` 的 `startSession`。 | Cookie 设置了 `Path=/`、`HttpOnly`、`SameSite=Lax` 和 `Expires`，但没有 `Secure`。登录响应中的 token 也扩大了暴露面。 |
| Cookie session 和 `Authorization: Bearer` 共用同一读取函数。 | `internal/api/server.go` 的 `sessionToken`。 | 无法区分浏览器 session、原生 access token 或未来 refresh token；S2 若复用会造成 token 类型混淆。 |
| `currentUser` 每次请求查询 session 表、检查过期时间，并重新检查账号是否仍可登录。 | `internal/api/server.go` 的 `currentUser`、`requireAuth`、`requireAdmin`。 | 这是立即撤销的良好基础，但只适用于 Web session；没有 device、scope、purpose、auth epoch 概念。 |
| `sessions` 表以原始 token 作为主键，只有 `token`、`user_id`、`expires_at` 三列。 | `internal/db/db.go` 的 `migrate`、`CreateSession`、`SessionByToken`、`DeleteSession`。 | 数据库泄露即可直接接管 Web session；表里也没有设备、最近使用、创建时间或撤销原因。 |
| 用户删除会显式删除 session 和进度，`sessions` 同时有用户外键级联。 | `internal/db/db.go` 的 `DeleteUser`。 | 新增 device/token/ticket 表必须同样覆盖删除与级联，不能只依赖应用层清理。 |
| 改密、管理员改密、邮件重置、禁用账号和管理员批量踢下线都会让 Web session 失效；改密时会保留当前 Web session。 | `internal/api/user_handlers.go` 的 `handleChangePassword`、`handleSetUserPassword`、`handleRevokeUserSessions`；`internal/api/mail_handlers.go` 的 `handleResetPassword`。 | 这些路径目前只处理 `sessions`。S2 必须统一接入 refresh token family、access token 和设备状态撤销，否则原生凭据会绕过 Web 撤销。 |
| 登录在核对密码前校验 Turnstile，并用按客户端 IP 的内存限流器限制尝试。 | `internal/api/auth_handlers.go` 的 `handleLogin`；`internal/api/turnstile.go` 的 `verifyTurnstile`；`internal/api/ratelimit.go` 的 `attemptLimiter`。 | 浏览器登录安全逻辑可复用；但限流器是进程内状态，重启会清零，设备轮询还需要独立限流。 |
| 登录会区分账号待审核、已禁用和邮箱未验证，并在密钥正确后返回具体原因。 | `internal/api/auth_handlers.go` 的 `handleLogin`。 | 设备授权页应复用同一账号门禁语义，不能仅凭“浏览器里有 Cookie”批准设备。 |
| `auth_tokens` 保存邮件验证/重置 token，明文、每种类型每用户保留一个，使用后删除。 | `internal/db/db.go` 的 `CreateAuthToken`、`AuthTokenUserID`、`DeleteAuthToken`。 | 这是一次性邮件 token 的实现，不是 refresh token；不应直接搬用，否则缺少 family、设备绑定和重放检测。 |
| 数据库为 SQLite WAL，最多 4 个连接。 | `internal/db/db.go` 的 `Open`。 | token 轮换和票据认领必须使用短事务与条件更新；不得在持有事务时等待探测、源站或 ffmpeg。 |
| 当前没有 devices、device authorizations、access/refresh token、playback ticket 或授权媒体会话表。 | `internal/db/db.go` 的 `migrate` 中现有表清单。 | S2/S3 数据能力均为待实现，不能把现有 Web session 当作已完成设备认证。 |

#### 审计时进行中的 S1 事实

- `internal/api/v1_meta.go` 的 `handleV1Meta` 已注册公开的 `GET /api/v1/meta`，当前所有 feature flags 都返回关闭。
- `internal/api/v1_errors.go` 的 `V1ErrorCode` 已建立稳定错误码外壳，沿用 `{ "error": { "code", "message", "details" } }`，并保持旧 `/api` 的单字符串 `error` 外壳不变。
- 审计时尚未看到 `protocol/openapi.yaml` 或 JSON Schema 冻结文件；`v1ContractVersion` 仍为 1。
- 以上是并行工作中的现场状态，不是 S2/S3 可以依赖的最终契约。S1 完成后才能冻结字段、错误码和 feature flag 语义。

### 2.2 播放签名、stream、HLS 与字幕

| 现状事实 | 证据 | 安全含义 / 风险 |
| --- | --- | --- |
| 签名 URL 的有效期是 12 小时，签名载荷只有 `fileID` 和过期时间。 | `internal/api/play_handlers.go` 的 `sigTTL`；`internal/api/server.go` 的 `signPayload`、`signedQuery`、`sigOK`。 | HMAC 本身可用，但授权粒度过粗：没有 user、device、purpose、transport、nonce 或 session。任何拿到某文件签名的人可在有效期内访问该文件的多个媒体入口。 |
| 播放资源接受 Web session 或文件级签名。 | `internal/api/server.go` 的 `allowPlayerAuth`；`/api/stream`、`/api/hls`、`/api/subs`、`/api/danmaku` 路由。 | 签名是 bearer 能力，不绑定调用者；不能作为 v1 播放票据实现。 |
| `/api/play` 生成 direct/proxy 的 `/api/stream` URL 和 HLS/subtitle/danmaku 的本地签名 URL。 | `internal/api/play_handlers.go` 的 `playFile`。 | 当前 `Decision.URL` 由 API 层填充，decision 引擎本身不产生 URL；“决定”和“授权”没有分离。 |
| `/api/stream` 在源可直连且不需要 header 时返回 302；有必需 header 时改为服务端代理。 | `internal/api/play_handlers.go` 的 `handleStream`；`internal/api/proxy_range.go` 的 `proxyRemoteVia`。 | 当前 Web 路径不会把源 header 下发给浏览器，这是正确的边界；v1 direct-http 若直接下发源 URL 和 header，需要全新白名单与绑定。 |
| 代理保留单段 Range、HEAD 降级和上游分段修复语义。 | `internal/api/proxy_range.go`；`internal/api/proxy_range_test.go`。 | Range 行为可复用，但需要新的媒体会话授权包装。 |
| HLS URL 携带 `m`、`a`、`burn`、`q`、`start`、`ss`、`is` 等播放参数，但签名只覆盖 `fileID+exp`。 | `internal/api/play_handlers.go` 的 `playFile`、`handleHLS`、`serveSessionHLS`。 | 参数没有签名绑定，持有文件签名的人可以修改模式、音轨、质量、起点、字幕烧录等语义；S3 必须把归一化参数写入媒体会话，而不是继续信任可变 query。 |
| HLS playlist、init、segment 和字幕子资源继续使用同一个文件级签名。 | `internal/api/play_handlers.go` 的 `serveSessionHLS`、`serveHLSSubtitles`；`handleSubtitle`。 | 子资源没有独立 session/purpose 边界；S3 的 HLS 子资源必须只认同一媒体会话。 |
| 当前 HLS 打包 session 是内部工作缓存，不是授权 session。 | `internal/media/session.go` 的 `sessionIdleTTL`、`SessionKey`、`gcSessions`；`internal/api/play_handlers.go` 的 `serveSessionHLS`。 | 打包 session 默认 90 秒空闲回收，key 只含文件与播放参数，不含 user/device；绝不能把该 key 当作 bearer 凭据，也不能宣称已有 15 分钟/6 小时媒体 session。 |
| `/api/play/{id}/status` 按文件与 variant 查询探测/打包进度。 | `internal/api/play_handlers.go` 的 `handlePlayStatus`、`streamStatusFor`。 | 状态是文件级，不是 ticket 级；S3 的 ticket status 必须独立绑定 ticket 与设备。 |
| HLS 字幕通过 master/rendition playlist 提供，最终指向 `/api/subs`；文件级签名允许访问该文件任意可转换字幕。 | `internal/api/play_handlers.go` 的 `serveHLSSubtitles`、`handleSubtitle`。 | 当前没有“本 ticket 允许哪些字幕/声道/起止范围”的绑定；S3 需要显式 subtitle grants。 |
| 日志中间件只记录方法、路径和耗时，不记录 query。 | `internal/api/server.go` 的 `logMiddleware`。 | 这是有利现状，但不足以作为脱敏保证；路径中若放 media session 能力也会泄露。 |
| 代理 Range 异常会记录完整上游 target。 | `internal/api/proxy_range.go` 中 `proxyRemoteVia` 的两处 `log.Printf`。 | target 可能含源站签名 query，是明确的日志泄露风险；v1 上线前必须有脱敏日志或删除该日志。 |
| 多个 handler 会把底层错误文本直接返回或写入日志。 | `internal/api/play_handlers.go` 的 `fail` 调用和 HLS 日志；`internal/source` 各 driver 的错误文本。 | v1 必须映射到稳定错误码并返回净化的 message；不能把源站响应、URL、header 或凭据透传。 |

### 2.3 源 header、多流与凭据边界

| 现状事实 | 证据 | 安全含义 / 风险 |
| --- | --- | --- |
| `source.Driver` 的 `DirectURL` 与 `ProbeURL` 都返回 URL 和必需 header；header 是任意 map。 | `internal/source/source.go` 的 `Driver`。 | 当前接口没有安全 header 类型或来源标记；S3 不能直接序列化该 map。 |
| `source.Part.Headers` 已标记 `json:"-"`，但 `Part.URL` 仍可被序列化。 | `internal/source/source.go` 的 `Part`。 | 防止 header 意外 JSON 化是好的现有约束，但不是可下发的安全 DTO；任何 `Part` 整体输出仍会泄露源 URL。 |
| 多流源通过 `MultiPart.Parts` 暴露 video/audio 两条流，并允许每条流各自带 header。 | `internal/source/source.go` 的 `MultiPart`、`Part`；`internal/source/bilibili.go` 的 `Parts`。 | S3 不实现 direct-dash；多流只能走 server-hls。S5 才可考虑直接下发两条流。 |
| Bilibili 的 CDN stream header 只包含 Referer 和 User-Agent，不含账号 Cookie；其 `HasDirect` 为 false。 | `internal/source/bilibili.go` 的 `headers`、`streamHeaders`、`HasDirect`、`Parts`。 | 这是正确的凭据分离：账号 Cookie 只用于 API，不发送到第三方 CDN。S3 可把 Referer/UA 视为候选白名单，但仍需再次校验。 |
| Bilibili 的账号 Cookie 与 refresh token 保存在源配置中。 | `internal/models/models.go` 的 `SourceConfig`；`internal/api/bili_account_handlers.go`。 | 它们是源站主凭据，绝不能出现在 ticket、client logs、diagnostics 或 media response 中。 |
| OpenList 的 `fs/get` 返回任意 `header` map，驱动会把它用于直链或探测。 | `internal/source/openlist.go` 的 `parseAlistHeader`、`resolve`。 | 上游可以返回 Authorization、Cookie 或自定义凭据 header。direct-http 必须逐项白名单；任何非白名单项都使 direct 不合格。 |
| WebDAV 通过服务端 `Authorization: Basic` 拉流，`HasDirect` 为 false。 | `internal/source/webdav.go` 的 `auth`、`ProbeURL`、`HasDirect`。 | Basic 凭据只能留在服务端；server-proxy/server-hls 可以使用，ticket 不得下发。 |
| 裸 URL 源的 `HasDirect` 为 true，直接返回路径；没有在 driver 内强制 scheme/host 策略。 | `internal/source/url.go` 的 `URLDriver`。 | v1 必须在授权/签票层重新规范化并限制为 HTTP(S)、禁止 userinfo 和客户端指定 target；不得把客户端输入直接交给服务端拉流。 |
| 源凭据以原样 JSON 存入 `sources.config`；普通用户的 API 输出会掩码，管理员可读明文。 | `internal/models/models.go` 的 `SourceConfig`；`internal/db/db.go` 的 `CreateSource`；`internal/api/library_handlers.go` 的 `maskSourceConfig`。 | 当前没有源凭据的静态加密；这是独立于 S2/S3 的既有风险。S2/S3 只能在边界上保证不扩大泄露，不能宣称凭据已加密。 |
| 共享 `http.Client` 未设置自定义 `CheckRedirect`。 | `internal/source/source.go` 的 `httpClient`；`internal/api/play_handlers.go` 的 `proxyClient`。 | Go 默认策略会保护部分敏感 header，但不会替项目定义完整的同源策略；自定义 header 仍可能跨域跟随。v1 必须使用显式重定向策略。 |

### 2.4 direct / proxy / remux / unsupported 决策

| 决策 | 当前实现事实 | 证据 | 对 S3 的约束 |
| --- | --- | --- | --- |
| `direct` | 源 `HasDirect` 为 true，且 `DirectHeaders` 为空时选择；否则退化为 proxy。 | `internal/media/decision.go` 的 `decide`、`canDirectPlay`。 | 只可映射为 direct-http；源 URL 必须来自服务端解析，header 必须走安全 DTO。当前 Web 的 `direct` URL 实际是 `/api/stream` 302，不是 v1 的源直链。 |
| `proxy` | 单 URL 源可播但不可 302 时选择；支持 Range。 | `internal/media/decision.go` 的 `decide`；`internal/api/proxy_range.go`。 | 映射为 server-proxy，必须包在授权媒体会话下。 |
| `remux` | 视频流复制到 HLS，必要时仅重编码音频。 | `internal/media/decision.go` 的 `hlsVideoCopyOK`、`decide`；`internal/media/session.go`。 | 映射为 server-hls；不能复用 12 小时 HLS 签名。 |
| `unsupported` | 探测缺失或客户端无法解码可复制的视频编码时，`URL` 为空，只返回原因。 | `internal/media/decision.go` 的 `unsupported`、`decide`。 | 映射为 ticket 的 `unsupported`，不得返回 URL，也不得伪造 fallback。 |
| fallback | 按 direct、proxy、remux 的可用性和成本顺序给出 alternates；不含转码。 | `internal/media/decision.go` 的 `fallbackPlan`；`internal/media/decision_test.go`。 | v1 可复用 eligibility 逻辑，但 fallback 的签发/推进必须由 ticket 状态机控制。 |
| multipart | 音视频双流只能 server-side mux；direct/proxy 会得到无声画面。 | `internal/media/decision.go` 的 `MultiPart` 分支；`internal/media/decision_test.go` 的 `TestMultiPartSourceOnlyOffersRemux`。 | S3 对 `source.MultiPart` 只能签发 server-hls；direct-http 与 server-proxy 必须排除。 |

以下现有实现不能直接充当 v1 契约：

- `Decision.Kind` 对 direct/proxy 固定写 `mp4`，API 也固定写 `video/mp4`；MKV 等容器直连时这个 MIME/kind 不可信。S3 必须从探测结果和实际传输生成契约字段。
- `Decision.URL` 是本地 `/api` 签名 URL；S3 必须另建 v1 ticket/media session，不能序列化该结构冒充完整 ticket。
- 能力来自 query 参数或 UA（`clientKind`、`caps`、`reportedAudio`）；S3 应使用规范化能力枚举，UA 最多作为旧入口的兼容输入。
- `/api/play` 只要求登录和文件存在，当前没有按用户/媒体库分配的 ACL；README 也明确所有用户共享媒体库。S3 必须把文件授权集中到一个函数，不能把“文件存在”写成“用户有权播放”。
- `burn` 与 `force=byterange` 会改变成本/语义，且不是自动决策的一部分。S3 首批不应允许客户端通过任意 query 强制这些路径；若产品需要，应另立字段、配额和安全测试。

## 3. S2：设备认证、PKCE 与 refresh token

### 3.1 可复用与不可复用的现有能力

| 能力 | 结论 | 边界 |
| --- | --- | --- |
| Turnstile 校验 | 可复用 | 只在系统浏览器中的现有登录页/授权登录过程使用。原生客户端不能提交绕过 Turnstile 的登录，也不能把 Turnstile token 当设备凭据。 |
| 登录限流思路 | 可复用 | 设备授权轮询、user code 尝试、token exchange 都需要独立限流；现有进程内限流器只能作为基础实现。 |
| 账号状态、邮箱门禁、角色检查 | 可复用并集中 | 授权页和 v1 请求应调用同一套用户状态判断；不要让设备 token 跳过禁用/待审核/邮箱要求。 |
| 密码校验与 PBKDF2 | 可复用 | 仅由浏览器登录使用；设备客户端不收集密码。本 ADR 不改变密码哈希参数。 |
| `auth.NewToken` 的密码学随机源 | 可作为底层随机源 | 需要新的 opaque token、PKCE challenge、哈希和比较封装；不要直接把现有 Web session token 当 access/refresh token。 |
| 数据库迁移、WAL 和事务模式 | 可复用 | 新表必须增量迁移并保留旧 session；网络/探测/ffmpeg 不得在事务中运行。 |
| 现有 `currentUser` / `requireAuth` | 不可直接用于 v1 | 它只认 Web session 和旧 Bearer。v1 需要独立 access token 中间件，避免 token 类型混淆。 |
| `startSession` / `/api/auth/login` | 不可复用为设备 token 响应 | 它会创建 30 天 Web session、设置 Cookie 并返回同值 token；原生客户端不得拿到 Web session。 |
| `sessions` 表 | 不可复用为 refresh token 表 | 没有设备、family、轮换、重放、scope 和 auth epoch。 |
| `auth_tokens` 表 | 不可复用为 refresh token 表 | 邮件验证/重置 token 不是长期可轮换凭据，也不能承担设备撤销和 family 重放检测。 |
| 12 小时文件签名 | 不可复用为 S2/S3 授权 | 它不绑定用户、设备、purpose 或 nonce。 |

### 3.2 推荐数据模型

表名为建议，不是已实现事实。所有 token、device code、user code 和内部 capability 都不得明文落库。

#### `users` 增补

- `auth_epoch`：单调递增整数。改密、密码重置、管理员改密和全局撤销时递增；access/refresh token 记录签发时的 epoch，校验时不匹配即拒绝。当前实现为 `users.auth_epoch` 加两张 device token 表的 epoch 列；epoch 不进入 token 响应或 JSON Schema，因为客户端持有的 token 始终是不透明字符串。
- 旧 Web session 行为不变；`auth_epoch` 只约束新设备 token，避免一次改密还保留可续期的原生凭据。

#### `devices`

- `id`：服务端生成的高熵公开标识；不是 bearer token，不能由客户端自选。
- `user_id`：设备所有者。
- `display_name`、`platform`、`app_version`：用于设备列表；显示名做长度、控制字符和 HTML 安全处理。
- `public_key_fingerprint`：可选；若客户端使用安装密钥，只存公钥指纹。S2 不以它替代 TLS/PKCE，也不宣称具备 DPoP 级发送者约束。
- `created_at`、`last_seen_at`、`last_seen_ip_hash`：最近使用只做节流更新；IP 只存带服务端密钥的不可逆指纹，不存原始 IP。
- `revoked_at`、`revoke_reason`、`status`：单设备撤销和审计。
- 设备删除/用户删除走外键级联；被撤销设备保留到审计保留期结束。

#### `device_authorizations`

- `id`：浏览器授权页定位标识，不是设备 token。
- `device_code_hash`：高熵 device code 的哈希，唯一索引。
- `user_code_hash`：短 user code 的带密钥哈希；不能只存普通 SHA-256，因为短码可离线枚举。
- `code_challenge`、`code_challenge_method`：S2 只接受 S256，拒绝 plain。
- `client_name`、`platform`、`app_version`、可选安装公钥指纹：仅用于展示与审计，不能作为授权依据。
- `state`：`pending`、`approved`、`denied`、`consumed`、`expired`。
- `user_id`：只有批准后写入。
- `created_at`、`expires_at`、`approved_at`、`denied_at`、`consumed_at`。
- `interval_seconds`、`last_polled_at`、`poll_count`、`failed_attempts`。
- `request_ip_hash`：用于风险审计，不用于永久锁定设备；移动网络 IP 会变化。

#### `device_token_families`

- `id`：refresh token 轮换 family。
- `user_id`、`device_id`、`created_at`。
- `absolute_expires_at`：建议 180 天，强制周期性重新授权。
- `revoked_at`、`revoke_reason`、`reuse_detected_at`。

#### `device_access_tokens`

- `token_hash`：唯一索引。
- `family_id`、`user_id`、`device_id`、`auth_epoch`。
- `scopes`：排序后的规范 scope 列表。
- `issued_at`、`expires_at`、`revoked_at`、`last_used_at`。

#### `device_refresh_tokens`

- `token_hash`：唯一索引。
- `family_id`、`user_id`、`device_id`、`auth_epoch`。
- `parent_id` / `replaced_by_id`：保留轮换链，便于检测重放。
- `issued_at`、`expires_at`、`used_at`、`revoked_at`、`reuse_detected_at`。

建议索引：

- 所有 token hash 唯一索引。
- `devices(user_id, revoked_at)`。
- `device_authorizations(user_code_hash, state, expires_at)`。
- `device_access_tokens(device_id, revoked_at)`、`expires_at`。
- `device_refresh_tokens(family_id)`、`device_id`、`expires_at`。

保留策略：撤销记录和轮换链至少保留 30 天安全审计期；过期 token hash 可在审计期后清理。清理任务不得阻塞登录、刷新或播放请求。

### 3.3 设备授权与 PKCE 生命周期

#### 授权请求

`POST /api/v1/auth/device/authorizations` 建议接收：

- 设备展示名、平台枚举、客户端版本。
- 请求 scope 列表；服务端取允许集合与最小权限集合的交集。
- PKCE `code_challenge` 与 `code_challenge_method=S256`。
- 可选的安装公钥指纹。

服务端生成：

- 高熵 device code，有效期 10 分钟。
- 短 user code，建议 8-10 个无歧义字符，有效期同为 10 分钟。
- 轮询间隔，建议 5 秒。
- 系统浏览器授权入口；该入口不得包含 access/refresh token。

服务端响应只返回 device code、user code、授权入口、过期时间和轮询间隔。device code 和 user code 都不写入 URL query、普通日志、崩溃报告或剪贴板。

#### 浏览器批准

1. 客户端用系统浏览器打开授权入口，不嵌入长期 WebView。
2. 浏览器若未登录，走现有 Web 登录；Turnstile、登录限流、账号状态、邮箱门禁和密码校验都沿用现有逻辑。
3. 登录后进入授权页。授权页展示设备名、平台、请求 scope 和发起信息，用户必须明确确认；打开链接本身不能自动批准。
4. 批准接口必须是 POST，并同时校验：
   - 有效 Web session；
   - 同源 `Origin` / Fetch Metadata 或等价 CSRF 防护；
   - 一次性 CSRF token；
   - user code 状态、过期时间和尝试次数；
   - 账号仍允许登录。
5. 批准只把 `user_id` 与 authorization 绑定，不向浏览器签发设备 token。

现有 `SameSite=Lax` 只是一种缓解，不是新授权页的完整 CSRF 防线。审计时未看到应用级 CSRF token 机制，因此新授权页必须自行实现或先提交通用 CSRF 方案。

#### Token exchange

`POST /api/v1/auth/device/token` 同时提交 device code 与 PKCE verifier：

- 先用带密钥哈希定位授权记录。
- 再校验 S256 challenge；verifier 缺失、格式错误或不匹配统一返回 `invalid_grant`。
- 未批准返回 `authorization_pending`；轮询过快返回 `slow_down`；拒绝、过期、已消费分别返回稳定错误码。
- 成功时一次性原子消费 authorization，创建 device、token family、access token 和 refresh token。
- device code 在成功后立即失效；使用同一 device code 或同一 verifier 再换 token 必须失败。

建议 token 生命周期：

- access token：15 分钟。
- refresh token：30 天。
- token family 绝对上限：建议 180 天，到期后必须重新做设备授权。
- 每次刷新生成新的 access token 和 refresh token；旧 refresh token 立即标记已使用，旧 access token 在同一事务中撤销。

access/refresh token 使用至少 256 位随机熵、opaque 字符串，不使用 JWT 自包含撤销。服务端保存哈希并在每个受保护请求查表，这样设备撤销和账号禁用能立即生效。

#### 客户端密钥与存储

- PKCE verifier 是一次性授权材料，只在客户端内存和交换请求中存在，不写入普通文件。
- Windows 使用 DPAPI CurrentUser 保护 refresh token；access token 尽量只放内存。
- Android 使用 Android Keystore 包装随机设备密钥并保护 refresh token。
- token 不进入 URL、剪贴板、普通日志、崩溃报告或未加密 JSON。
- 若实现安装公钥，S2 仍按 bearer token 处理；没有完整 proof-of-possession 协议前，不得宣传为“不可复制的 token”。

### 3.4 刷新、撤销与账号生命周期

#### 刷新轮换

刷新必须在一个短 SQLite 事务内完成：

1. 按 token hash 查找 refresh token。
2. 条件更新：仅当未使用、未撤销、未过期且账号/设备状态有效时，把旧 token 标记为已使用。
3. 若 `RowsAffected` 不是 1，进入重放/撤销判断；不能继续签发。
4. 创建新的 access/refresh token 和 `replaced_by`，撤销该 family 的旧 access token。
5. 提交后再返回新 token；不得在事务内做网络请求。

严格策略：

- refresh token 不允许多次使用，也不提供“重放宽限期”。
- 客户端必须对刷新做单飞（single-flight）和串行化；并发两个刷新只能一个成功。
- 旧 refresh token 在未过期时再次出现，视为重放：撤销整个 family、该设备全部 access/refresh token，并把设备标记为需要重新授权。
- 对外统一返回 `invalid_grant` 和需要重新认证的提示；内部审计事件记录“重放检测”，但不向攻击者披露检测细节。
- 因响应丢失导致客户端没收到新 token 时，客户端应按“需要重新授权”处理，不得无限重试旧 token。这是严格轮换的安全代价，需产品接受。

#### 请求校验

每个 v1 access token 请求至少校验：

- token hash 存在、未过期、未撤销；
- token 类型是 access token，不是 Web session 或 refresh token；
- token 绑定 user/device 与目标资源一致；
- device 未撤销；
- 用户状态仍允许访问；
- token 的 `auth_epoch` 等于用户当前值；
- scope 覆盖路由要求；
- 账号角色仍允许该路由（设备 token 不能自动获得管理员权限）。

`last_seen_at` 更新应节流，避免每个 HLS 分片都写一次数据库。

#### 撤销路径

| 事件 | 必须撤销 | 备注 |
| --- | --- | --- |
| 客户端 logout | 当前设备 family 的 access/refresh token | 可保留 device 行并显示“已退出”；再次使用需授权。 |
| 用户撤销当前账号某台设备 | 该设备全部 family 与 token | 下一请求返回 `device_revoked`；设备列表保留审计。 |
| 修改密码 | 该用户所有 device token；递增 `auth_epoch` | 现有 Web session 的“保留当前 session”规则不变。 |
| 邮件重置密码 | 同上 | 现有重置路径已撤销 Web session，需增加 device 撤销。 |
| 管理员改密 | 同上 | 管理员改自己的密码时，Web session 仍按现有规则；设备 token 全部失效。 |
| 账号禁用 | 该用户所有 device token；设备状态失效 | 现有状态检查继续作为第二道防线。 |
| 管理员“全部踢下线” | 所有 Web session 与 device token | 复用同一撤销服务，避免遗漏。 |
| 删除账号 | 级联删除/撤销所有设备与 token | `devices`、families、tokens 必须有用户外键与明确清理。 |
| 重放检测 | 整个 family 与该设备 | 记录内部审计，不保存原始 token。 |

### 3.5 建议 scope

首批客户端建议只申请：

- `library.read`：首页、条目、详情。
- `playback`：创建/刷新播放票据。
- `progress.write`：写入观看进度。
- `devices.manage`：列出/撤销自己的设备；是否由 access token 承担由 S1/S2 契约确认。

`devices.manage` 不能越过管理员角色限制。需要管理其他用户、修改配置或查看源凭据的路由永远不接受设备 token。

### 3.6 稳定错误码与 HTTP 映射

继续使用 S1 的 `{ "error": { "code", "message", "details" } }`。以下 P0 表中，“S1 现有”表示审计时已经存在的错误码；“需 S1 扩展”必须进入 OpenAPI/JSON Schema 后才能实现。

| 条件 | HTTP | 公开 code | 状态来源 | 客户端动作 |
| --- | --- | --- | --- | --- |
| JSON/字段/枚举/PKCE 格式错误 | 400 | `invalid_request` | S1 现有 | 修正请求，不重试同一 payload。 |
| 设备授权仍在等待用户 | 400 | `authorization_pending` | 需 S1 扩展 | 按 interval 继续轮询。 |
| 轮询快于服务端 interval | 400 | `slow_down` | 需 S1 扩展 | 增大 interval 后重试。 |
| 用户拒绝设备 | 400 | `access_denied` | 需 S1 扩展 | 停止轮询并展示拒绝。 |
| device code / user code 过期 | 400 | `expired_token` | 需 S1 扩展 | 重新发起设备授权。 |
| device code、verifier、已消费授权或跨设备交换无效 | 400 | `invalid_grant` | 需 S1 扩展 | 重新发起授权；不要区分内部原因。 |
| refresh 无效/过期/已撤销/重放 | 400 | `invalid_grant` | 需 S1 扩展 | 清理本地 token 并重新授权；内部记录重放。 |
| access token 缺失、无效或过期 | 401 | `unauthenticated` | S1 现有 | 先尝试一次刷新，失败则重新授权。 |
| 设备已撤销 | 401 | `device_revoked` | S1 现有 | 清除 token，展示设备被撤销。 |
| 账号禁用 | 403 | `account_disabled` | 需 S1 扩展 | 停止重试，提示管理员状态。 |
| 邮箱验证未完成且策略要求 | 403 | `email_verification_required` | 需 S1 扩展 | 引导用户在浏览器完成验证。 |
| scope 不足 | 403 | `insufficient_scope` | 需 S1 扩展 | 不自动提升权限；重新授权并展示 scope。 |
| 设备/票据不属于当前主体 | 404 | `not_found` | 需 S1 扩展 | 不暴露资源存在性，创建新票据/刷新设备列表。 |
| 传输被关闭 | 403 | `transport_disabled` | 需 S1 扩展 | 用新 ticket 获取剩余传输。 |
| ticket 过期 | 410 | `ticket_expired` | S1 现有 | 新建 ticket。 |
| media session 过期 | 410 | `media_session_expired` | 需 S1 扩展 | 新建 ticket/session，并从当前位置恢复。 |
| 媒体编码不可播 | 415 | `unsupported_media` | S1 现有 | 展示原因，不盲目切换传输。 |
| 源站不可用/源链接过期/5xx | 502 | `source_unavailable` | S1 现有 | 依据 ticket 的 fallback 规则推进或重试。 |
| 客户端取消播放控制请求 | 408 | `invalid_request` | S4 新增 | 停止本次请求；不要在客户端循环重试。 |
| 服务端签发/存储超时 | 504 | `internal_error` | S4 新增 | 可有限重试；源站超时仍由具体源站错误映射处理。 |
| 限流 | 429 | `rate_limited` | S1 现有 | 遵守 `Retry-After`，指数退避。 |
| 客户端版本过低 | 426 | `client_upgrade_required` | S1 现有 | 停止调用并提示升级。 |
| 未预期内部错误 | 500 | `internal_error` | S1 现有 | 可有限重试，不解析中文 message。 |

错误 `details` 只允许稳定、非敏感字段，例如 `action`、`state`、`ticket_id`、`retry_after_seconds`。禁止包含 device code、user code、token、Cookie、Authorization、源 URL/query、原始 header 或尚未实际填充的预留字段。

## 4. S3：播放票据与媒体会话

### 4.1 票据字段

播放票据是服务端状态记录，不是 `media.Decision` 的 JSON 化。建议字段如下：

| 字段 | 语义 | 存储/输出边界 |
| --- | --- | --- |
| `ticket_id` | 高熵、公开定位标识。 | 单独持有不等于可播放；所有操作仍需 access token 与设备绑定。 |
| `contract_version` | 创建时的 v1 契约版本。 | 输出；用于拒绝旧客户端误解字段。 |
| `state` | 票据状态。 | 输出；状态机见下节。 |
| `purpose` | S3 固定为 `playback`。 | 绑定；不得用播放票访问下载、管理或其他用途。 |
| `user_id` / `device_id` | 服务器从 access token 取得的主体。 | 不信任请求体；请求体声明不一致直接拒绝。 |
| `file_id` | 授权后的文件。 | 绑定；必须与媒体会话和子资源一致。 |
| `transport` | 当前 `direct-http`、`server-proxy` 或 `server-hls`。 | 当前传输，不能由客户端任意切换。 |
| `candidate_transports` / `attempted_transports` | 探测后冻结的有序候选集合与实际尝试历史。 | 数据库内部字段；响应只公开按实时 flag 过滤后的 `remaining_transports`，当前契约没有 `fallback_order`。 |
| `ticket_nonce_hash` | 每个 ticket 独立的高熵 nonce 哈希。 | 原始 nonce 只用于派生本次媒体 capability；不入日志、不落库明文。 |
| `capability_hash` | 归一化后的客户端能力快照哈希。 | 仅用于一致性/审计，授权仍以服务端判断为准。 |
| `header_profile_hash` | 白名单 header 的规范化哈希。 | 不记录 header 值；用于检测换源/票据替换。 |
| `feature_snapshot` | 创建时相关的全局/设备级 flag。 | 激活时仍实时复核；flag 关闭时 fail closed。 |
| `source_origin` / `source_url_hash` | 源 origin 与 URL 指纹。 | 可记录 origin/host 供诊断；完整 URL 只短暂在内存。 |
| `source_expires_at` | direct-http 的预留字段，用于未来在源明确给出过期时间时输出。 | 当前 S3 服务端不填充；客户端不得依赖，未知时以源错误或播放失败触发 refresh/新 ticket。 |
| `media_session_id_hash` | 关联媒体会话。 | 原始 session capability 不写入日志。 |
| `created_at`、`expires_at`、`consumed_at`、`revoked_at` | 生命周期。 | ticket 创建后 5 分钟绝对过期，不滑动续期。 |
| `failure_code` | 安全净化后的失败原因。 | 不得包含源站响应或原始 URL。 |

数据库只保存 ticket 元数据、nonce/hash、URL 指纹和状态，不保存可直接使用的源 URL、header、access/refresh token 或媒体 session 原文。direct-http 的完整源 URL 仅在本次响应前短暂存在于服务端内存；服务重启后未释放的 ticket 只能转为失败/过期，客户端应新建 ticket。

### 4.2 状态机与 TTL

| 状态 | 含义 | 允许转移 |
| --- | --- | --- |
| `probing` | 已创建 ticket，正在探测/解析源。 | `ready`、`unsupported`、`failed`、`expired`、`revoked`。 |
| `ready` | 已冻结媒体摘要与服务端过滤后的候选传输计划；尚未 claim 或释放任何 transport description。 | `consuming`、`exhausted`、`failed`、`expired`、`revoked`。 |
| `consuming` | server-proxy/server-hls 已激活；或 direct-http 描述已释放。 | `consuming`（服务端推进 fallback）、`exhausted`、`expired`、`revoked`。 |
| `unsupported` | 该客户端能力下无可播放路径。 | 终止；需要新 ticket。 |
| `failed` | 源解析、探测或签发失败。 | 终止；重试必须新建 ticket。 |
| `exhausted` | 所有允许的传输都已尝试/释放。 | 终止；需要新 ticket。 |
| `expired` | ticket 自身 5 分钟 TTL 到期。 | 终止；媒体 session 仍按自己的 TTL 独立处理。 |
| `revoked` | 用户、设备、管理员或 feature flag 使 ticket 失效。 | 终止。 |

规则：

- ticket TTL 从创建时刻起 5 分钟绝对上限；`GET`、`POST refresh` 和进度查询都不延长。
- `POST /api/v1/playback/tickets` 会触发探测；`GET /api/v1/playback/tickets/{id}` 只读状态，不得触发昂贵探测。
- `POST /api/v1/playback/tickets/{id}/refresh` 必须先读取 ticket 并完成纯本地状态判定，之后才可授权文件或访问 Driver/Engine/源。`unsupported`、`failed`、`exhausted`、`expired`、`revoked` 全部是终态，不能被 refresh 恢复、探测或推进。
- 仅 `ready`、`consuming` 允许 refresh claim 下一个 transport。`probing` 不允许用 refresh 代替探测，固定返回 `409 invalid_request`、`details.action=status`；客户端继续只读 `GET`，直到状态变为 `ready`。`GET` 永不重建或补发 transport。
- refresh 对 `unsupported`、`failed`、`exhausted`、`expired` 固定返回 `410 ticket_expired`、`details.action=request_new_ticket`；对 `revoked` 固定返回 `403 forbidden`、`details.action=reauthorize`。这些拒绝必须在任何文件授权、Driver/Engine 调用或源访问之前完成。
- 进行中的探测必须有上限；现有 `playFile` 使用 150 秒 context，可作为兼容输入，但 v1 不能让一个 ticket 无限停在 `probing`。
- ticket 的 `expired` 不等于媒体 session 过期。媒体 session 在 ticket 过期后仍可在自身 15 分钟空闲/6 小时绝对上限内工作；客户端不需要用 ticket 签每个 HLS 分片。
- ticket `ready`/`consuming` 不保证源 URL 一直有效。已知 `source_expires_at` 到期（当前 S3 通常未知）、源 403 或源 URL 轮换时，客户端调用 ticket refresh 或新建 ticket。
- ticket 不允许从终态恢复。create 仍可合法返回 `201` 的 `unsupported`/`failed` 终态 issuance；refresh 的终态则使用上述稳定错误 envelope，不能把终态 issuance 当作可继续刷新。

### 4.3 绑定、一次性与重放规则

以下绑定全部必须匹配，任何一项不同即拒绝：

- access token 的 user/device；
- ticket 的 user/device；
- 目标 file；
- purpose=`playback`；
- 当前 transport 属于 ticket 冻结的 `candidate_transports`；
- ticket nonce/capability；
- 媒体资源路径、HLS 子资源类型和会话参数；
- 账号状态、设备状态和 `auth_epoch`。

一次性语义必须精确表述：

- ticket 状态读取是幂等、非消费操作。
- 每次成功的 refresh 只返回一个 transport description，并以原子条件更新推进 ticket 状态。refresh 不重新探测源；它只能使用 create 时冻结的 media summary、transport candidate 顺序和字幕 ID 快照，并实时复核 feature flag。
- 一个 transport candidate 最多被尝试一次。若当前候选在签发时不可用，服务端可在同一 refresh 内按已存储的 fallback 顺序继续 claim，但最终仍只能返回一个成功 description；不得重新探测来扩大候选集合。
- 并发 refresh 必须依赖数据库条件更新/compare-and-swap，恰好一个请求能 claim；输掉 claim 的请求返回稳定的 `409 invalid_request`、`details.action=retry`，若期间已进入终态则返回对应终态错误。客户端必须对同一 ticket 串行 refresh（single-flight）。
- direct-http 传输在描述释放时即视为尝试；服务端此后不再重复下发同一源 URL。direct URL 本身是不可撤销的 bearer，服务端无法检测或阻止持有者后续重复请求。
- server-proxy/server-hls 的媒体 session capability 一经激活，可在 TTL 内重复用于 Range、init、playlist 和 segment；这不是“每请求一次性”，否则播放器无法工作。
- 每个 transport 只能由服务端推进一次；重放已消费的 ticket/transport 不得重新签发 URL。
- `GET` 永不 claim、探测、重建或补发 transport。服务端不保存明文 direct URL、media capability 或已发放 description；进程重启后不能凭 ticket 元数据恢复旧 description，只能按未尝试候选签发新 transport 或进入终态。
- 媒体 session capability 不能移到另一个 file、transport、purpose、device 或 ticket；跨资源使用必须拒绝。
- 被撤销设备或账号的后续票据/媒体请求必须立即失败；已有 direct-http URL 仍可能工作到源自己的过期时间，必须在 UI 和文档中明确这一限制。

### 4.4 请求与响应边界

`POST /api/v1/playback/tickets` 的请求至少包含：

- file 标识；
- 规范化客户端能力：platform、version、engine、container、video codec、audio codec、subtitle codec、Range/header/DASH 支持；
- 传输偏好顺序。

服务端必须重新校验/规范化：

- access token 的 user/device，并与请求体 `device_id` 一致；
- file 对应用户的可见性；
- 账号/设备状态；
- feature flags，包括设备级 override；
- 源类型、多流能力、header 需求、源 URL 是否可签发；
- 媒体探测与传输 eligibility。

响应至少包含：

- `ticket_id`、`contract_version`、`state`、`expires_at`；
- 当前 transport（仅 `consuming` 一次性签发响应）和 `remaining_transports`；
- 预留的可选 `source_expires_at`；当前 S3 不填充，客户端不得依赖；
- 媒体摘要：容器、时长、视频/音频编码、分辨率、声道/语言；
- 当前 transport 的资源描述；
- 可用字幕描述与默认项；
- 净化后的 reason/failure code。

`direct-http` 的 `streams` 角色在 S3 只能是 `muxed`；多流 video/audio 成对属于 S5 direct-dash，当前 S3 契约和 fixture 都不包含 `direct-dash`。`server-proxy` 和 `server-hls` 不得包含源 URL 或源 header。

### 4.5 transport 映射

| 现有 `media.Decide` | v1 transport | 条件 |
| --- | --- | --- |
| `direct` | `direct-http` | 源 `HasDirect`、URL 可规范化、无必需 header 或全部必需 header 命中白名单且客户端声明支持 header。 |
| `proxy` | `server-proxy` | 源可服务端读取；有 Range 能力或能由服务端修正 Range。 |
| `remux` | `server-hls` | 客户端可解码复制后的视频；会话/包管理器可用。 |
| `unsupported` | 无 | 返回 `unsupported_media`/ticket `unsupported`，不签发 URL。 |
| MultiPart | `server-hls` | 仅 server-side mux；S3 不签发 direct-dash，也不签 server-proxy 单流。 |

客户端传输偏好只影响有资格传输的排序；不能：

- 把 `direct-http` 强加给有 Cookie/Authorization/非白名单 header 的源；
- 把 multipart 强制为 direct/proxy；
- 打开已关闭的 feature flag；
- 绕过用户/设备/文件检查；
- 强制 `burn` 或 `byterange`。

### 4.6 授权媒体会话

媒体会话是 S3 的核心授权对象，与 `internal/media/session.go` 的打包 session 分离：

- 授权媒体会话绑定 `ticket_id`、user、device、file、purpose、transport、nonce/capability hash 和资源参数。
- server-proxy 的空闲 TTL 建议 15 分钟，绝对上限 6 小时；HLS playlist/init/segment 继续请求会刷新空闲时间，但不能超过绝对上限。
- 媒体会话 token 只出现在 v1 媒体 URL 的必要 capability 位置；服务端只保存 hash，日志必须脱敏。
- HLS 子资源只认同一个媒体会话；不能用文件级签名、另一个会话或猜测的 segment 路径访问。
- 归一化后的 `mode`、音轨序号、video index、质量、起点/keyframe、字幕 ID、是否 burn 等必须写入会话记录；请求 query 变化必须与记录匹配。
- 会话撤销/关闭时，后续 playlist、init、segment、subtitle、danmaku 请求全部 fail closed。
- 现有 `media.Engine` 可以内部去重/复用打包工作，但 `media.SessionKey` 不是授权凭据；任何去重都必须保留 v1 授权包装。

建议 v1 媒体路径与旧 `/api` 物理隔离，例如 `/api/v1/playback/media/...` 或独立的 v1 sessions 路由。旧 `/api/stream`、`/api/hls` 继续使用旧 session/签名，不因 v1 改动而改变语义。

### 4.7 字幕、弹幕与烧录

- `subtitles` 中每个条目必须带稳定 ID、语言、标签、格式、默认标记和 delivery 类型。
- 文本字幕 URL 必须绑定当前媒体会话或单独的短寿命 subtitle capability；不得复用旧的 12 小时文件签名。
- URL 只能在 `subtitle.FileID == ticket.file_id` 且字幕 ID 在 ticket grant 中时签发。
- HLS master/rendition 子 playlist 必须继续携带同一媒体会话能力；不能把任意字幕 ID 改成 ticket 中未授权的内容。
- 图形字幕只能通过显式 burn 选项进入，且 burn 是重编码成本路径；S3 首批默认不开放任意 `burn` query，若开放必须单独配额、时长上限和测试。
- 弹幕如果进入 v1，必须使用独立的 ticket/media-session grant；不得复用 `/api/danmaku` 的全局文件签名。

### 4.8 title、源 URL 与日志脱敏

| 数据 | API 响应 | 数据库 | 日志/指标/崩溃报告 |
| --- | --- | --- | --- |
| 标题、剧集名 | 可返回给已授权用户 | 可保留现有媒体库字段 | 默认不记录；不放进指标 label。 |
| ticket ID / 会话 ID | 客户端可见 | 可记录公开定位标识 | 默认不记录；如需排障只记录带密钥的短指纹。 |
| 源完整 URL/query | 仅 direct-http 响应按设计返回一次 | 不保存明文，只保存 origin/hash | 禁止；只能记录 host、transport、file ID（非标题）。 |
| Referer/User-Agent 等白名单值 | direct-http 必要时返回 | 只保存规范化 profile hash | 禁止记录原值。 |
| Cookie / Authorization / WebDAV Basic / OpenList Token / Bili Cookie | 永不返回 | 不进入 ticket/session；源配置现状另见审计节 | 永不记录。 |
| access/refresh/device code/user code/device code verifier | 仅对应的授权响应返回 | 只保存 hash | 永不记录。 |
| 媒体 session 内部 capability | 媒体 URL 中可存在，作为 bearer | 只保存 hash | 永不记录；访问日志也必须遮盖。 |
| Turnstile token | 只提交给现有校验 | 不保存 | 永不记录。 |

现有 `logMiddleware` 只记录 path，不记录 query，这不能替代脱敏：

- v1 media capability 不能裸落在未脱敏的 path；
- `proxyRemoteVia` 当前记录完整 target，S3 前必须改为记录 host/transport/状态或删除；
- 任何新 v1 logger 必须是结构化、字段白名单的，不得把 error 直接拼进日志；
- 客户端诊断最多显示源 host、transport、HTTP 状态和字节统计，不显示完整 URL、path+query、header、票据或 token。

## 5. Header 白名单与跨域重定向

### 5.1 direct-http 可下发 header

S3 只能使用显式 DTO；禁止遍历 `Part.Headers` 或 `DirectURL` 返回的 map 直接序列化。

| header | 是否允许 | 约束 |
| --- | --- | --- |
| `Referer` | 有条件允许 | 只能是源 provider 的规范化 origin/固定 referer；不得带 userinfo、片段或未审查 query；不得含控制字符。 |
| `Origin` | 有条件允许 | 必须与允许的源 origin 完全匹配；不能由客户端填写任意值。 |
| `User-Agent` | 有条件允许 | 使用受控 profile；限制长度并拒绝 CR/LF；不包含账号信息。 |
| `Accept` | 有条件允许 | 归一化为媒体 MIME 子集；不影响授权。 |
| `Accept-Language` | 可选 | 仅当源确实需要；限制长度和字符集。 |
| 其他 header | 禁止 | 包括 Authorization、Proxy-Authorization、Cookie、Set-Cookie、Host、Connection、Transfer-Encoding、Content-*、任意 X-* 和 OpenList 自定义 header。 |

补充规则：

- header 数量、名称长度和值长度设置上限；重复/冲突项拒绝。
- 任何必需 header 未命中白名单，direct-http 不合格，转为 server-proxy/server-hls；不能“先发出去让客户端试试”。
- `supports_headers` 只是客户端能力输入；空值表示客户端不确定，服务端应保守。
- header profile 要绑定 ticket；刷新 ticket 时重新读取源并重新白名单，不复用旧任意 map。
- `Cookie` 和 `Authorization` 永远不是 direct-http header，即使源当前看起来是内部源也不例外。

### 5.2 重定向策略

服务端拉流不得依赖 Go 默认 `http.Client` 重定向行为作为安全边界。

建议策略：

- 每个 v1 源请求使用独立 client 和 `CheckRedirect`。
- 只允许 HTTP(S)；禁止 HTTPS 降级到 HTTP，除非源策略显式配置且经过安全评审。
- 重定向次数设为小上限并超时。
- 同 origin 重定向只保留该 transport 的白名单 header。
- 跨 origin 重定向默认剥掉 Authorization、Cookie、Proxy-Authorization、所有 X-* 和所有未知 header；只有源策略明确允许的目标 origin 才能保留 Referer/UA 等安全 header。
- 若跨域目标需要凭据，重新解析源或走 server-proxy 的受控路径，不把凭据自动跟随过去。
- 响应最终 URL 不写日志；只记录初始 host、最终 host、状态码和 transport。

客户端 direct-http 的跨域行为不能完全由服务端控制。因此白名单本身必须保证“即使跨域也不携带凭据”：只允许无秘密的 Referer/Origin/UA/Accept 等，绝不把 Cookie、Authorization 或自定义 secret header 交给 direct-http 客户端。

### 5.3 现有源适配结论

- Bilibili：`streamHeaders` 的 Referer/UA 可作为候选白名单；账号 Cookie 不进入 stream header。当前 `HasDirect=false`，S3 不应擅自打开直连。
- OpenList：`parseAlistHeader` 可能返回任意 header；默认应只允许安全子集，其他一律 server-proxy。若 OpenList 自身 `/p` 链接无需 header，可按白名单直连。
- WebDAV：Basic 只在服务端使用，direct-http 永远不合格。
- URL 源：必须重新校验 scheme/host、禁止 URL userinfo 和客户端 target；不得把任意 `.strm` 内容作为可信直链无条件透传。
- 多流：每个 `Part` 的 header 分别评估；S3 的 server-hls 在服务端应用，direct-dash 留给 S5。

## 6. local relay 安全要求

local relay 解决的是本机播放器无法设置某些请求 header 或需要统一中转的兼容问题，不是把源站凭据搬到客户端。S3 首批不要求它；在以下要求全部实现并有自动测试前，`local_relay_enabled` 必须保持 false。

### 6.1 监听与寻址

- 只监听 IPv4 loopback `127.0.0.1`，不得监听 `0.0.0.0`、LAN 地址或 IPv6 通配地址。
- 使用端口 0 由内核分配随机空闲端口；不得固定常用端口。
- 校验 `Host` 为本次 relay 的 loopback 地址和端口，拒绝 DNS rebinding 风格的 Host。
- relay 进程退出、播放结束或 ticket/session 过期时立即关闭 listener，并清除目标与能力材料。

### 6.2 一次性 bootstrap token

- relay 启动时生成独立高熵 capability，不使用 access/refresh token、Cookie、Authorization、源站凭据或可预测 ID。
- bootstrap capability 一次性绑定本轮 relay；首次成功绑定后再次使用必须失败或被明确拒绝。
- 若播放器不能设置 header，capability 可以出现在 loopback URL 中，但必须在 app 日志、代理日志和崩溃报告中脱敏；更推荐通过本机 IPC 在启动播放器前注入。
- capability 只允许一个目标/transport/purpose，不能用于其他 relay、其他文件或任意代理。
- Range、HLS 子资源等后续请求由同一内存 session 授权，不等于重复使用 bootstrap capability；session 必须有空闲和绝对 TTL。

### 6.3 目标白名单

- 目标 URL 由服务端 ticket 预绑定，客户端只能选 ticket 中的 target ID；禁止请求参数携带任意目标 URL、任意 Host 或 CONNECT 目标。
- 精确允许 scheme、host、port，必要时允许 path/query hash；默认只允许 HTTPS，HTTP 仅在明确源策略下允许。
- 重定向目标必须预先列入 ticket 允许 origin，或 relay 拒绝；不得在运行时“跟随到哪算哪”。
- 拒绝 loopback、私有网段、link-local、metadata 地址和 DNS rebinding 目标，除非该源明确是受信任的本地 relay 场景并经安全评审。
- 不提供通用 HTTP proxy/CONNECT 语义；只服务 GET/HEAD/Range 等媒体所需方法。
- 入站 Cookie/Authorization/Proxy-Authorization 全部剥离；只注入 ticket 白名单 header，绝不转发源站主凭据。
- 响应只回传媒体所需 header；不得把上游 Set-Cookie 或凭据类 header 反射给播放器。
- 限制并发、请求体、响应大小和超时；记录 host/transport/字节统计，不记录完整 URL/query/header/token。

## 7. S1 / S2 / S3 拆分与依赖

### 7.1 S1 完成前的门禁

S1 冻结之前，S2/S3 可以实现内部原型、迁移草案和本地测试，但不得合并暴露新端点。S1 必须提供并冻结：

- `/api/v1/meta` 的 contract version、最低客户端版本和全部 feature flags；
- OpenAPI/JSON Schema 与 golden fixtures；
- 稳定错误 envelope 和错误码表；
- 能力枚举、传输枚举、ticket/subtitle 字段；
- 运行时 feature flag 读取方式与设备级 override；
- v1 与旧 `/api` 的路由隔离规则。

审计时的 S1 现场只有部分 `v1_meta`/`v1_errors` 工作；`protocol/` 尚未冻结。不能把“所有 flag 当前为 false 的测试”当成 S2/S3 已完成，也不能在 S1 golden 测试里偷偷改语义而不做契约评审。

### 7.2 S2 可执行拆分

| 子卡 | 内容 | 前置 | 优先级 |
| --- | --- | --- | --- |
| S2.1 | 增量 schema、`auth_epoch`、token hash 工具、统一撤销服务、迁移测试。 | S1 error/route freeze 可并行做内部草案。 | P0 |
| S2.2 | device authorization、user code、PKCE S256、系统浏览器批准页、Turnstile/CSRF/限流。 | S1 + S2.1。 | P0 |
| S2.3 | token exchange、15 分钟 access、30 天 refresh、family 轮换/重放撤销。 | S2.2。 | P0 |
| S2.4 | v1 access 中间件、scope/role/device/epoch 校验、token 类型隔离。 | S2.3。 | P0 |
| S2.5 | 设备列表、单设备撤销、logout、所有改密/禁用/管理员撤销路径接入。 | S2.1、S2.4。 | P0 |
| S2.6 | Windows/Android 系统浏览器真机 e2e 与安全日志扫描。 | S2.2-S2.5。 | P1（发布前必须） |

先做 S2.1 和错误契约，再做 S2.2/2.3，不要把设备授权页做成另一个 Web 登录 cookie 入口。

### 7.3 S3 可执行拆分

| 子卡 | 内容 | 前置 | 优先级 |
| --- | --- | --- | --- |
| S3.1 | ticket/media session schema、状态机、原子 claim、TTL 清理。 | S1 + S2 完成。 | P0 |
| S3.2 | normalized capabilities + `media.Decide` adapter + transport eligibility；不序列化内部 Decision。 | S3.1。 | P0 |
| S3.3 | direct-http 安全 header DTO、URL 规范化/重定向策略、secret-header 回落。 | S3.2。 | P0 |
| S3.4 | server-proxy 媒体会话、Range/HEAD、会话撤销。 | S3.1。 | P0 |
| S3.5 | server-hls 媒体会话、playlist/init/segment、字幕授权、参数不可变。 | S3.1、S3.2。 | P0 |
| S3.6 | ticket status/refresh、fallback 推进、feature flag 关闭、错误映射。 | S3.3-S3.5。 | P0 |
| S3.7 | 脱敏日志、日志 canary 扫描、race/e2e、网络矩阵。 | S3.3-S3.6。 | P0（上线前） |
| S3.8 | local relay。 | S3 安全门禁完成，且作为独立卡。 | P1，不阻塞 S3 首批 |

S3 不得先把旧 `/api` 的签名 URL 包一层新路径；必须从 ticket 和媒体会话授权链开始实现。

### 7.4 必须先补的测试

#### S2 合并前

1. 旧数据库增量迁移：旧 users/sessions 保留，新增表为空，外键/索引存在；重复迁移幂等。
2. 新 token 不以明文出现在数据库、日志、错误响应和 crash/diagnostic capture 中；只存 hash。
3. PKCE 只接受 S256；verifier 错误、缺失、跨 authorization、重复使用全部失败。
4. device authorization 状态：pending、approved、denied、consumed、expired 的合法/非法转移。
5. user code 枚举/爆破限流；过期和已消费码不再可用；浏览器批准页 CSRF/Origin 测试。
6. Turnstile 只在浏览器登录路径校验；原生 UA 不能绕过；Turnstile 网络失败不会 fail open。
7. access token 15 分钟边界；refresh 30 天边界；family 绝对上限；旧 access token 在刷新后失效。
8. 两个并发 refresh 只有一个成功；旧 refresh 重放撤销 family/设备；响应丢失的客户端进入重新授权而非无限重试。
9. 账号禁用、改密、邮件重置、管理员改密、管理员批量踢下线、删除账号分别使 refresh 无法续期并使 access 失效。
10. Web session token、access token、refresh token、device code、PKCE verifier 五种类型互相不能冒用；旧 `/api` 不接受 access token，v1 不接受 Web session。
11. scope 与 role 边界：普通设备 token 不能访问管理员路由；设备只能列出/撤销自己的设备；跨用户 device/ticket 返回不可枚举错误。
12. 设备列表不返回 token、原始 IP、PKCE verifier 或源凭据。

#### S3 合并前

1. binding 矩阵：user、device、file、purpose、transport、nonce、header profile 任意一项不匹配都拒绝。
2. ticket TTL：探测中、ready、consuming、expired 边界；`GET` 不触发探测；过期 ticket 返回稳定错误。
3. media session：15 分钟空闲过期、6 小时绝对上限；ticket 过期不等于 media session 过期；HLS 子资源只认同会话。
4. direct-http：无 header、白名单 header、非白名单 header、Cookie/Authorization/WebDAV/OpenList secret header 的资格矩阵。
5. `Part.Headers`、`Part.URL` 和 `media.Decision` 永不被直接 JSON 序列化；响应中不存在源凭据或未白名单 header。
6. 跨域重定向：Authorization/Cookie/未知 header 不跟随；安全 header 只在允许 origin 保留；HTTPS 降级被拒绝。
7. HLS query 篡改：改音频、质量、起点、mode、burn、字幕 ID 不能复用已绑定会话。
8. multipart 源只产生 server-hls；不会因为 `HasDirect` 为 true 而错误签发 direct/proxy。
9. fallback：direct->proxy->hls 的顺序、关闭 flag、源 403/过期/5xx、解码错误、认证错误分别走正确动作；不能通过客户端偏好越过 flag。
10. ticket/transport 重放：已消费 transport 不再签发；未知/跨主体 ticket 不泄露存在性；feature flag 关闭立即 fail closed。
11. Range/HEAD 与 HLS init/segment 不因媒体会话授权而被破坏；保留现有 `proxy_range_test.go` 的语义。
12. 日志/响应 canary：测试生成的 URL query、header、token、Cookie、票据和标题哨兵不得出现在日志、指标 label 或错误 message 中。
13. `go test -race ./internal/api ./internal/media`，覆盖 ticket claim、刷新并发、会话关闭和 fallback。
14. local relay 若启用，额外测试 loopback/随机端口、Host、一次性 bootstrap、目标白名单、拒绝 CONNECT/任意 target、跨域重定向、Cookie/Authorization 剥离和日志脱敏。

#### 可复用并有参考价值的现有测试

- `internal/api/mail_flow_test.go`：Turnstile、密码重置和撤销语义。
- `internal/api/users_test.go`：登录限流与账号状态。
- `internal/api/proxy_range_test.go`：Range/HEAD 兼容性。
- `internal/media/decision_test.go`：fallback、multipart 和 unsupported 的保守行为。
- `internal/netproxy/netproxy_test.go`：loopback bridge 的随机端口与“不成为开放代理”思路；它不等于 playback local relay，不能直接当作后者验收。
- `internal/api/v1_errors_test.go`、`internal/api/v1_meta_test.go`：S1 的错误外壳/meta 现场测试；S2/S3 只能在契约冻结后扩展。

## 8. Feature flag 与回滚

### 8.1 默认与生效

- `client_v1_enabled`、`device_auth_enabled`、`playback_ticket_enabled`、三种首批 transport 和 `local_relay_enabled` 默认全部关闭。
- flag 必须在服务端评估；`GET /api/v1/meta` 只做发现，客户端提交的 flag 值不产生权限。
- 设备级 override 必须能在全局关闭之外单独急停一台设备。
- 每次创建/刷新/激活 ticket 都要实时复核 flag，不能只依赖创建时快照。

### 8.2 关闭语义

| 关闭项 | 行为 |
| --- | --- |
| `device_auth_enabled=false` | 不再创建 device authorization 或 exchange/refresh；现有 access token 最多按 15 分钟自然过期。需要立即停用时同时撤销设备/账号 token；Web 登录和旧 `/api` 保持原样。 |
| `playback_ticket_enabled=false` | 不再创建/刷新 ticket；已有 ticket fail closed。媒体 session 的后续请求由独立开关/撤销决定，但不得继续扩大权限。 |
| `direct_http_enabled=false` | 不再签发 direct-http；已经交付给客户端的源直链无法实时撤销，必须在 UI/文档声明。 |
| `server_proxy_v1_enabled=false` | 不再创建 server-proxy；已有会话在下一请求拒绝并进入清理。 |
| `server_hls_v1_enabled=false` | 不再创建 server-hls；已有 playlist/segment 请求 fail closed，客户端通过新 ticket 选择剩余传输。 |
| `local_relay_enabled=false` | 不创建 relay listener；已有 listener 关闭端口并清空内存目标/capability。 |

### 8.3 回滚边界

- 旧 `/api`、Web 30 天 Cookie 登录和 12 小时签名在 S2/S3 回滚中保持原状。
- 新表只做增量迁移；关闭 flag 不删除审计数据或用户进度。
- 不自动把旧 session 转成 device token，也不自动把 device token 转成 Web session。
- 发生认证异常时，禁用 `device_auth_enabled` 并清空/撤销受影响设备；发生票据异常时，关闭对应 transport，必要时拒绝全部 v1 media session。
- 回滚报告必须列出：受影响设备、已撤销 token family、已关闭 transport、无法撤销的 direct URL 及其源过期时间。

## 9. 安全验收门禁

S2/S3 合并前至少满足：

- 非 loopback 的认证/播放票据 endpoint 只在 HTTPS 或受信任 TLS 反向代理后启用；本机 loopback 开发模式可显式豁免。当前二进制默认明文 HTTP，不能默默把 bearer token 暴露在 LAN。
- 所有 token、device code、user code、PKCE verifier、源凭据、完整源 URL/query、header 值和媒体 capability 有自动日志脱敏测试。
- 不存在 token 类型混淆；Web session 与 device token 的测试互相拒绝。
- 所有撤销入口都同时处理 Web session 与 device token，或有统一的撤销服务保证。
- direct-http 不携带任何非白名单 header；多流不进入 direct/proxy。
- ticket/media session 的 TTL、绑定、一次性/重放和 fallback 行为有并发测试。
- `go test ./...` 通过；涉及并发的包通过 `-race`；协议 fixture 与 OpenAPI 校验通过。
- 真机矩阵至少覆盖 Windows Chrome/Edge 系统浏览器授权、Android Chrome Custom Tabs 授权、授权中断/过期、refresh 重放、设备撤销、源 403/过期/5xx、ticket 关闭 flag 和 media session 过期。

## 10. 未决事项

1. TLS：自托管用户是否强制 HTTPS，还是允许仅在显式“不安全开发模式”下启用设备认证？建议生产默认强制。
2. refresh family 的绝对上限是否接受 180 天，还是产品要求更短/更长；30 天 token TTL 不变。
3. S1 是否接受本文列出的新错误码；若不接受，必须给出等价的可分支映射，不能靠中文 message。
4. 当前所有登录用户共享媒体库；是否在 S3 前引入 per-user/per-library ACL，还是明确 v1 首版沿用共享策略。无论选择哪种，ticket 都不得把“文件存在”当成“用户有权”。
5. local relay 放在 Windows/Android 客户端进程还是独立 sidecar；无论位置如何，第 6 节边界不变。
6. ticket 是否支持 burn/byterange；建议 S3 首批不支持任意客户端强制，另行卡片评估。
7. `devices.manage` 是否由 access token 使用，还是设备管理只允许 Web session；如果要让原生客户端管理设备，需在 S1 scope 中冻结。

## 11. 结论

S2/S3 的主要风险不是 HMAC、PBKDF2 或 Go HTTP 实现本身，而是把“一个长期文件签名/一个 Web Cookie”误扩展成设备、用户、传输和媒体会话的全能授权。正确顺序是先冻结 v1 契约，再建立独立设备 token 与授权媒体会话，最后才允许 direct-http 和 local relay 接触源 URL/header。任何无法实时撤销的 direct URL 都必须如实展示其剩余有效期和限制，不能用“已有签名”宣称已经完成吊销。
