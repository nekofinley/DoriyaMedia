# 任务：让 B 站番剧在打包进行中就能起播与 seek（去掉“等整集打包完”）

## 背景

DoriyaMedia（Go 后端 + Vue 3 前端）播放 B 站番剧时，音视频是两条独立 DASH 流，
服务端用一个长驻 ffmpeg 把两路合流成 fMP4 HLS，产物落在
`<data>/cache/hls/<会话目录>/`，播单是 `#EXT-X-PLAYLIST-TYPE:EVENT`，
打包结束时才追加 `#EXT-X-ENDLIST`。

当前实现（已合入工作区）采用了一个保守策略：**续播/切清晰度时先不挂载 `<video>`，
等服务端把整个会话打包完（等到 `ENDLIST`），再挂载并 seek 到目标位置。**

这个策略当时的理由是「hls.js 会把还在增长的播单当直播、把播放头钉在直播边缘」。
本任务的第一步已经用实验推翻了它——见下节：hls.js 是认 `startPosition` 的，只要传
相对位置。所以保守策略可以拆掉，代价比原先估计的小。

后果（本任务要消除的）：
- 从片中续播要等“从关键帧到片尾”全部打包完（实测 297s 的集从 2:00 续播约等 10s，
  从片头附近续播几乎等于等整集）。
- 打包完成到真正出画面之间还有约 7 秒 `readyState=1` 的空窗（hls.js 解析 VOD 播单 +
  拉首片）。
- 播放中向前 seek 超出已打包窗口时，同样要等。

## 已经查清的事实（不要重复验证，直接采信）

以下结论由 `.tmp/seeklab/probe.mjs` 实测得出（headless Chrome + hls.js 1.7.3，
本地 fixture：EVENT 播单无 ENDLIST、中途起播的会话）：

| 场景 | `startPosition` | 首帧 `currentTime` | 结论 |
| --- | --- | --- | --- |
| EVENT 播单 | `8`（相对流内位置） | `8` | ✅ 生效 |
| EVENT 播单 | 不指定 | `28`（贴直播边缘） | ❌ 默认行为不可用 |
| EVENT 播单，中途起播 | `10`（相对） | `10` | ✅ 生效 |
| EVENT 播单，中途起播 | `120`（绝对文件时间） | `0.21` | ❌ 回落到开头 |
| VOD 播单 | `8` | `8` | ✅ 生效 |

**核心结论：hls.js 在 EVENT（无 ENDLIST）播单下完全支持 `startPosition`，
只要传的是「相对当前会话起点」的位置，而不是绝对文件时间。**

因此不需要改服务端播单类型、不需要在打包中途改写 `#EXT-X-PLAYLIST-TYPE`，
也不需要 `#EXT-X-DISCONTINUITY`。任务范围比想象小。

要复跑这组实验（`hls.min.js` 与两套 fixture 都已在目录里，无需重新生成）：

```powershell
cd .tmp\seeklab
node probe.mjs
```

脚本会起一个本地静态服务 + headless Chrome，依次跑 5 个场景并打印首帧
`currentTime`。`media/` 是从 0 开始的 40 秒源，`media2/` 是从 20 秒开始打包的
（模拟「会话从关键帧起播」）。

### 另一个必须知道的事实

会话从文件中途起播时，ffmpeg 输出仍是 `seg00000.m4s` 起编号、`MEDIA-SEQUENCE:0`，
元素时钟从 0 开始。即：

```
元素 currentTime = 文件时间 − stream_start
```

`/api/play` 已返回 `stream_start`（会话起点，DASH 时是「≤ 续播点的视频关键帧」）。

## 现状代码位置

- `internal/api/play_handlers.go`
  - `playFile`：算 `resumeFrom`（观众位置）与 `packFrom`（打包起点，关键帧），
    返回 `stream_start` / `resume_at` / `can_start_at`。
  - `serveSessionHLS`：multi 源时 `sessionStart = AlignedStart(...)`，分片从 0 编号。
  - 分片请求分支：multi 时按 `keyStart(startSeg)` 找会话，找不到就 `ensureAt` 重开。
- `internal/media/session.go`：`Session`、`EnsureSession`、`ShiftSegment`、
  `MarkInitServed`/`WaitInitServed`、`looksComplete`（要求 `ENDLIST` 才算完整缓存）。
- `internal/media/keyframe.go`：`AlignedStart`（返回 `(位置, ok)`，失败时调用方从 0 开始）。
- `webui/src/views/Player.vue`
  - `loadDecision`：续播时把 `switchAt` 设为 `resume_at`，然后 `await warmSession(...)`。
  - `warmSession`：轮询 `/api/play/{id}/status?variant=...`，**等 `complete && generated >= need`**。
  - `startCand`：`hlsConfig.startPosition = switchAt - streamStart`（**这一句已经是对的**）。
  - `maybeResume`：目标可达后 `v.currentTime = target - streamStart`。
  - `streamGoal` / `streamPct` / `loadingText`：进度条与文案，均以「相对流内」计。

## 要做的事

### 1. 让 `warmSession` 只等到「目标已打包」，不等整集

`webui/src/views/Player.vue` 约 690-720 行，当前条件：

```js
const need = Math.max(0, Number(target) - streamStart.value)
if (st.stream.complete && generated >= need) {
  packaged.value = true
  return
}
```

改成「`generated` 覆盖 `need` 加上少量余量即可返回，`complete` 时也返回（整集已完成的
快路径保留）」：

```js
const need = Math.max(0, Number(target) - streamStart.value)
// 余量：2~3 个分片，保证 startPosition 指向的分片确实已落盘。
// segLen 可从 play 决策里带，或按 4s 估。
const margin = 12
if (st.stream.complete || generated >= need + margin) {
  packaged.value = st.stream.complete
  return
}
```

注意 `packaged` 这个 ref 的语义是「服务端已完工」，只在 `complete` 时置真；
`loadingText` 用它决定显示「正在重新封装…」还是「正在加载画面…」。别在只是
「够用了」的情况下把它置真。

**验收**：续播 2:00（`stream_start=110`）时，`warmSession` 在 `generated` 达到
约 20 秒就返回，而不是等到 190 秒。

### 2. 让「挂载时机」不再依赖 ENDLIST，并保证 startPosition 生效

当前 `startCand` 已设 `hlsConfig.startPosition = switchAt - streamStart`，这条正确，
但需要确认在「播单仍是 EVENT」时不被后续逻辑覆盖：

- `onLoadedMetadata` 里 `switchTo > 0` 分支会 `v.pause()` 并 `return`，把跳转交给
  `maybeResume`。**这段在新方案下应该改掉**：既然 `startPosition` 已经把播放头放到
  了目标位置，就不该再暂停等 `complete`。
- `maybeResume` 里有一条「非 switching 且是 stream_variant 时必须 `st.complete`」的
  限制（`if (!switching && play.value?.stream_variant) { if (!st || !st.complete) return }`）。
  这条在新方案下也要放开：目标分片已存在即可 seek。

建议做法：进入 `switchAt` 路径时，让 `startPosition` 负责定位；`onLoadedMetadata`
不再暂停，只在「实际 `currentTime` 明显偏离目标」时用 `maybeResume` 纠正一次。

⚠️ 改动时注意两个已有的守卫，别把它们弄坏：

- `maybeResume` 开头有一段「目标已被播放头越过 → 视为过期并清空」的逻辑
  （`if (!switching && target <= v.currentTime + streamStart.value + 0.5) { resumeAt.value = 0; return }`）。
  它修的是「暂停后再播放跳回旧位置」的 bug。放开 `complete` 限制时不要顺手删掉它。
- `saveProgress` 里 `parked = v.paused || v.readyState < 2` 决定是否用等待目标当进度。
  它修的是「把等待目标当实际位置存进进度」的 bug。同样别删。

### 3. 播放中向前 seek 超出已打包窗口

`serveSessionHLS` 的分片分支（`play_handlers.go` 约 1460-1480 行）目前是：找不到能服务该
分片的会话就 `ensureAt(inputs, idx)` 重开会话。但 multi 分支里 `ensureAt` **忽略传入的
`startSeg`**，固定用外层闭包算好的 `sessionStart`：

```go
ensureAt := func(in []media.Input, startSeg int) (*media.Session, error) {
    o := optsFor(in)
    o.StartSegment = startSeg
    o.StartSeconds = float64(startSeg) * segLen
    if multi {
        o.StartSegment = 0
        o.StartSeconds = sessionStart   // ← 永远是本次请求 start 参数算出来的那个
        ...
    }
}
```

所以向前 seek 到窗口外时，重开的会话仍从**最初那个位置**开始打包，而不是 seek 目标。

需要：
- 从分片索引反推文件时间：`fileTime := idx*segLen + sessionStart`（`sessionStart` 是本次
  请求 `start` 参数对齐后的关键帧；分片从 0 编号）。
- 用这个时间重新 `AlignedStart` 并对齐到关键帧，再 `EnsureSession`，同时让
  `Session.ShiftSegment` 用新的 `StartSeconds`。
- 会话键要能区分不同起点（当前 multi 用 `keyStart()` 把 `sessionStart` 编成毫秒，
  已经有这个机制，复用它即可）。
- 前端：`can_start_at` 已为 `true`；`seekTo`/`endScrub` 里已有「超出可达范围且
  `can_start_at` 就重新 load」的逻辑，但**重新 load 后 `stream_start` 会变**，必须更新
  `streamStart.value` 为新会话的起点，否则进度条与字幕会漂。

**这一项如果评估后风险过大，可以单独拆出去，只做 1 和 2**——1+2 已经能解决
「续播等整集」和「加载空窗」，向前 seek 属于锦上添花。

## 硬性约束（不要违反）

1. **绝不能从非关键帧起播。** DASH 画面和声音是两条独立输入，`-c:v copy` 只能从关键帧
   起播，声音却从指定位置开始。实测（5s GOP）：请求 21s 起播，画面在 20.08s、声音在
   21.07s，**永久错位约 1 秒**。`AlignedStart` 的失败语义必须保持：拿不到关键帧就从 0
   开始，不许猜。
2. **`stream_start` 必须与实际打包起点一致。** `/api/play` 报的值和
   `serveSessionHLS` 实际用的 `sessionStart` 必须相等，否则音画错位 + 位置漂移。
   两者都走 `AlignedStart`（有缓存，键是 `文件ID|位置`）。
3. **元素时钟 = 文件时间 − `stream_start`。** 所有 `startPosition`、`currentTime`、
   进度保存都必须按这个换算。历史上这里出过 bug：进度被存成相对时间，导致续播倒退。
4. **不要回到「等 ENDLIST」的保守方案**——那正是本任务要移除的。
5. 分片响应顺序：`init.mp4` 必须先于媒体分片到达浏览器（`Session.WaitInitServed`
   已实现，不要移除）。

## 验收标准

在隔离环境里用 headless Chrome 驱动真实播放页。仓库 `.tmp/resume-repro/probe.mjs`
有现成的探针，它会记录 `currentTime` / `paused` / `readyState` / 加载文案 / HLS 网络
时间线，可直接复用：

```powershell
# 需要一份数据库副本 + 登录 token（.tmp/resume-repro/mint 可签发）
cd .tmp\resume-repro
node probe.mjs <fileId> <观察秒数> <暂停秒> <暂停后观察秒> <清晰度> none
```

`mint` 的用法：`go run ./.tmp/resume-repro/mint <db路径>`，输出一个 64 字符 token，
写进 `.tmp/resume-repro/token.txt`。

1. **续播 2:00**（`stream_start=110`，文件时长 297s）：
   - 从进入页面到首帧 `currentTime > 0` 的墙钟时间 **≤ 5 秒**
     （当前约 10 秒；用探针时间戳算，不用肉眼估）。
   - 首帧 `currentTime` 落在 10±1（即文件时间 120±1），**不是 0**。
   - 全程无 `DEMUXER_ERROR_COULD_NOT_PARSE`。
2. **播放中暂停再播放**：暂停在任意位置，4 秒后播放，`currentTime` 连续前进，
   不回跳（这是已修好的回归项，不要弄坏）。
3. **向前 seek 到未打包区域**（如果做了第 3 项）：能起播且落在目标附近，无音画错位。
4. **音画同步**：从关键帧起播的会话，取首片 `init.mp4 + seg00000.m4s` 拼接后
   `ffprobe -show_entries stream=start_time`，视频与音频相差 < 0.2s。
5. `go test ./...` 全绿；`gofmt -l` 对改动文件无输出；`go vet` 干净。

## 需要补的测试

- Go：
  - `AlignedStart` 的失败路径（探测失败 → 返回 `ok=false` → 调用方从 0 开始）。
  - 「`/api/play` 与 `serveSessionHLS` 对同一位置算出相同 `sessionStart`」——
    这是第 2 条硬性约束，之前就出过不一致的 bug。
  - 仓库已有 `internal/media/align_test.go`、`keyframe_test.go` 可参考，
    后者用真实 ffmpeg 合成双流源验证音画同步。
- 前端：无测试框架，靠上面的 headless 验收脚本。

## 环境与工具

- Go 1.26、ffmpeg 8.0.1、Node 22、Chrome 均已安装。
- 前端构建：`cd webui && pnpm run build`（产物输出到 `internal/web/dist`，
  **该目录随仓库提交**）。若 esbuild 报 `Access is denied`，把 `TEMP`/`TMP` 指到
  `.tmp/esbuild-tmp` 再构建。
- 后端构建：`go build -ldflags "-s -w" -o doriyamedia.exe ./cmd/doriyamedia`。
- 构建后需要重启服务才能生效；前端产物嵌入二进制，只重建前端不重建二进制无效。

## 相关文档

`docs/ios-safari-playback-redesign.md` 的 §12（会话起点、时间轴、tfdt 平移）记录了
这套机制的设计与踩过的坑，动手前值得读。特别是 §12.7：**hls.js 会把会话流重新对齐到
0，丢弃分片里携带的绝对时间戳**——这正是「元素时钟 = 文件时间 − stream_start」的由来。

## 完成情况与实测（2026-10-24）

### 做了什么

1. **`warmSession` 只等到「目标已打包」**（`webui/src/views/Player.vue`）：条件从
   「`complete && generated >= need`」改为「`complete`（快路径）或 `generated >=
   need + 一个分片`」。分片长度不再写死：`warmAhead()` 用 `generated_seconds /
   segments_ready` 量出真实长度（ffmpeg 按关键帧切，配 4s 实际是 5s）。
   `packaged` 仍只在 `complete` 时置真。

2. **挂载时机不再依赖 ENDLIST**：`onLoadedMetadata` 的 `switchTo > 0` 分支不再
   `pause()` + `return`，而是直接放行——`startPosition` 已经把播放头放到目标位置。
   `maybeResume` 去掉「非切换时必须 `complete`」的闸门，改成只看 `reachable()`
   （已缓冲 + 服务端已生产）。两条既有守卫保持原样：目标被越过时清空 `resumeAt`、
   `saveProgress` 的 `parked` 判定。
   顺带修掉三个连带问题：
   - `holdForSession()` 语义改为「只为『挂载后才拿到的保存进度』压画面」，否则
     `onPlaying`/`onCanPlay` 会把刚起播的画面暂停；
   - 暂停时切清晰度不再被 `startCand` 的 `v.play()` 打破；
   - `onEnded`（追上打包边缘）改用 `switchAt` 而不是 `resumeAt` 重新挂载——后者会
     在 `attachSource` 里被清掉，画面于是从流的开头重放。

3. **向前 seek 超出窗口**：走客户端重载（`endScrub` 已有，`seekBy` 补上），
   `/api/play?at=` 会重新算 `stream_start`，`loadDecision` 更新 `streamStart`。
   没有做「服务端按分片序号重开一个不同起点的会话」：那会让 hls.js 手里的分片序号
   与新的会话起点错位，且客户端的时间轴（`stream_start`）无从得知变化，收益不抵风险。

### 两个实测暴露出来的真问题（都已修）

- **分片 URL 丢掉了会话身份**：`RewriteSessionPlaylist` 重写播单时只带 `sig&m&q`，
  `init.mp4` 与 `segNNNNN.m4s` 上**没有 `start`**——于是分片请求按 `startSec=0`
  找会话，被「跟随最近使用的会话」交给了上一次播放的会话（实测：播单来自 s110000，
  分片来自 s0，元素于是从文件开头播）。修法：播单里每个 URL 都带 `&ss=<会话起点>`，
  分片/init 请求据此直接定位会话（不再为每个分片重新探测关键帧）。
  同时把 `/api/play` 与 `serveSessionHLS` 的起点计算合并成
  `Engine.PackStart`（+ `PackStartFrom`），失败也进缓存（1 分钟），两条路径必然同答。
- **Chromium 声称能原生播 HLS**（`canPlayType(...) === 'maybe'`），于是播放器跳过了
  hls.js，`startPosition` 根本没生效；而 Chromium 的原生 HLS 播放器在播单还在增长时
  `seekable` 是空的，只能从会话起点开始播。修法：`nativeHlsPreferred` 只在 WebKit
  （iOS/Safari，无 MSE）上走原生，有 MediaSource 的浏览器一律用 hls.js。

### 实测（headless Chrome 驱动真实播放页，file 32368，`stream_start=110`，目标文件时间 120）

| 场景 | 结果 |
| --- | --- |
| 冷启动续播 2:00（会话刚开始打包，EVENT 播单） | 首帧 `currentTime = 10.00` → 文件 120.0；无 `DEMUXER_ERROR` |
| 同一场景，关键帧已在缓存里（第二次进同一集） | 进入页面到首帧 **3.8s**（加载文案 3.3s 变成「正在加载画面…」，3.8s 首帧落在 10） |
| 关键帧需要现测（缓存过期 / 首次进入） | 再 +3~4s（那一次是 CDN 关键帧探测的耗时，已与其余决策并行）；极端抖动时 `/api/play` 本身要 17~60s |
| 旧行为对比 | 等待部分是「从关键帧打包到片尾」（110→235，约 125s 内容），现在是「到目标后一片」（15s 内容） |
| 暂停 4s 再播放 | `15.31 → 15.41 → 16.43 …` 连续前进，不回跳 |
| 追上打包边缘（打包器还没写到） | hls.js 自己等下一批分片（`rs=2`，文案「正在生成分片 · 就绪 65s · 11.2× 实时」），`dur` 从 20.2 长到 80，画面继续 |
| 音画同步（`init+seg00000`） | video `start_time=0.120`，audio `start_time=0.120`，差 0 < 0.2s |
| 进度保存 | 存的是文件时间（`position=135.68` = 110 + 25.68） |

`go test ./...` 全绿，`go vet ./...` 干净，改动文件 `gofmt -l` 无输出。

### 新增测试

- `internal/media/packstart_test.go`：
  - `TestPackStartAlignsToTheKeyframe`：双流源下起点必须落在关键帧上；
  - `TestPackStartFailsToZeroAndKeepsThatAnswer`：探测失败 → 0（不是请求位置），且
    紧随其后的第二个调用者读到的是同一个 0（否则玩家被告诉的 `stream_start` 与实际
    打包起点不同）；
  - `TestPackStartAgreesAcrossResolutions`：两次独立解析（中间丢掉 parts 缓存）必须
    算出同一个起点——第 2 条硬性约束。
- `internal/media/session_test.go`：
  `TestSessionForStreamAtHonoursTheRequestedStart` —— 播单必须由「起点等于这个 URL
  起点」的会话回答，而不是最近用过的那个。

### 环境备注（不是代码问题）

- 本次实测期间 bilibili 的 4K CDN 与 `socks5://127.0.0.1:3080` 反复抖动：
  `ffprobe` / `ffmpeg` 会以 `exit status 1 ()`（空 stderr，实际是 30s 超时被杀）
  失败，个别 `/api/play` 因此要 17~60s 才回（关键帧探测的 30s×2 重试），
  4K 首片偶发 2~5s。上面的数字是在这种环境里测的，网络正常时会明显更好。
- 一个**已知但未修**的旧缺口：打包器在播放途中死掉（源被拒、CDN 断流）时，服务端
  不会自动重开，客户端只能看到 `packager_running=false` 并停在缓冲上。它不属于本任务
  （本次改动让「追上打包边缘」变得常见，但那条路走的是重挂载，不受影响）。

