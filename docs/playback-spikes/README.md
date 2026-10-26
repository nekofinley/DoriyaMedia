# Playback Spikes S0：能力与出口测量基线

本目录只服务于 S0，不改变正式播放决策，也不包含客户端实现。目标是用同一份样本清单和同一套脚本，记录 Windows/Android 在真实文件、真实网络上的传输、解码、seek 和 fallback 表现。

在真实客户端和设备数据出来之前，不得把本目录的 HTTP 探测结果描述成“可播放比例”或“直连成功率”。

## 文件

| 文件 | 用途 |
| --- | --- |
| `scripts/playback-matrix.ps1` | 清单校验、DryRun 矩阵、实际探测、脱敏 JSON/Markdown 报告 |
| `scripts/playback-matrix-mock.ps1` | S5 专用 loopback mock，仅 S0 脚本使用 |
| `protocol/fixtures/media-samples.json` | S1-S5 样本定义、设备矩阵、网络矩阵 |
| `docs/playback-spikes/RESULTS-TEMPLATE.md` | 真机结果填写模板 |
| `docs/playback-spikes/results/` | 实际运行生成的报告目录（默认位置） |

## 固定样本

| ID | 内容 | 当前状态 |
| --- | --- | --- |
| S1 | OpenList MP4 H.264 High / AAC，Range 正常 | 需要候选 `file_id` 和临时 secret |
| S2 | OpenList MKV H.264 / E-AC3 | **pending：当前本地库未找到可确认的 E-AC3 候选，不能用其他音频编码代替** |
| S3 | OpenList MKV HEVC Main10 / FLAC，带 ASS | 需要候选 `file_id` 和临时 secret |
| S3-PGS | 补充样本：MKV H.264 High10 / FLAC / PGS | 用于覆盖图形字幕路径，不冒充 S3 主样本 |
| S4 | Bilibili 视频 + 音频 DASH 双 URL | 需要双流 secret JSON；不得把视频 URL 冒充完整播放源 |
| S5 | 需要 Referer/Authorization、Range 异常的 mock | 脚本自动启动本地 loopback mock，不需要外部 secret |

样本清单的 `source_url` 必须保持 `null`。真实 URL 只允许从进程环境变量读取，不写进 JSON、Markdown、脚本、命令历史或提交。

## DryRun

```powershell
Set-Location C:\Users\Administrator\DoriyaMedia
.\scripts\playback-matrix.ps1 -DryRun
```

DryRun 会：

- 校验 S1-S5 是否存在，字段、编码、设备、网络和 secret 环境变量名是否合法；
- 拒绝非 `null` 的 `source_url`；
- 输出样本 × 设备矩阵、设备/网络清单、样本 × 网络探测计划；
- 显示 file ID 和 secret 是 `missing`、`parameter`、`environment` 还是 `configured`，但不回显其值；
- 不访问 DNS、TCP、HTTP，不启动 mock，不写报告。

Windows PowerShell 5.1 和 PowerShell 7 都应通过：

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\scripts\playback-matrix.ps1 -DryRun
pwsh -NoProfile -File .\scripts\playback-matrix.ps1 -DryRun
```

## 候选 file_id

提交的 manifest 中所有 `file_id` 都是 `null`。运行时可通过参数或环境变量注入，优先级为：

1. `-FileIds 'S1=<file_id>;S3=<file_id>;S4=<file_id>'`
2. manifest 中的 `file_id_env` 指向的环境变量
3. 用户自行填写的 manifest `file_id`

环境变量名示例（实际名字以 manifest 为准）：

```text
DORIYA_SAMPLE_S1_FILE_ID
DORIYA_SAMPLE_S2_FILE_ID
DORIYA_SAMPLE_S3_FILE_ID
DORIYA_SAMPLE_S3_PGS_FILE_ID
DORIYA_SAMPLE_S4_FILE_ID
```

没有 `file_id` 时脚本不会崩：transport 探测仍可执行，但 `ticket` 阶段会明确标为 `file_id_missing`，报告中的 `file_id` 为 `null`。

## Secret

S1-S4 的 URL 只从 `secret_ref` 指定的进程环境变量读取。示例只展示占位符，不要在文件中填真实值：

```powershell
$env:DORIYA_SAMPLE_S1_URL = '<temporary source URL>'
$env:DORIYA_SAMPLE_S1_FILE_ID = '<file id>'

.\scripts\playback-matrix.ps1 `
  -Samples S1 `
  -NetworkLabel same-LAN `
  -EgressRegion '<region label>'
```

secret 缺失时，对应样本会生成 `skipped` 报告，`skip_reason=secret_missing`，不会发起外部请求。

S3-PGS 使用 `DORIYA_SAMPLE_S3_PGS_URL` / `DORIYA_SAMPLE_S3_PGS_FILE_ID`。

### S4 双流 JSON

`DORIYA_SAMPLE_S4_SOURCES_JSON` 可以是以下形状（全部为占位符）：

```json
{
  "video": {
    "url": "<signed video URL>",
    "headers": {
      "Referer": "<allowed referer>",
      "User-Agent": "<client UA>"
    }
  },
  "audio": {
    "url": "<signed audio URL>",
    "headers": {
      "Referer": "<allowed referer>",
      "User-Agent": "<client UA>"
    }
  }
}
```

也支持 `streams` 数组或 `video_url` / `audio_url`，但必须同时存在 video 和 audio 角色。`DORIYA_SAMPLE_S4_HEADERS_JSON` 可以提供公共 Header；角色内 Header 会覆盖公共值。

脚本只记录 Header 名称，不记录值。跨源重定向时，脚本会丢弃全部源站 Header，避免 Authorization/Cookie/Referer 泄露到第三方；如果 CDN 要求这些 Header，结果会如实失败，而不是绕过安全边界。

## S5 本地 mock

S5 不访问外部网络。脚本会在两个随机 `127.0.0.1` 端口上启动临时 mock，并在结束时停止：

- 无 Header：`403`
- 正确 Referer + Authorization：`200`
- 正常 Range：`206`
- 非法 Range：`416`
- 过期资源：`410`
- 跨源重定向：目标端检查 Authorization/Cookie/Referer/Origin 均未转发

单独运行：

```powershell
.\scripts\playback-matrix.ps1 `
  -Samples S5 `
  -NetworkLabel same-LAN `
  -EgressRegion '<region label>' `
  -OutputDirectory .\.tmp\playback-matrix-s5
```

S5 只能证明传输/Header/Range 语义。`decode`、真实播放器 seek 和正式 fallback 仍然是 `pending`。

## 实际模式

```powershell
.\scripts\playback-matrix.ps1 `
  -Samples S1,S3,S3-PGS,S4 `
  -FileIds 'S1=<id>;S3=<id>;S3-PGS=<id>;S4=<id>' `
  -NetworkLabel same-LAN `
  -EgressRegion '<region label>'
```

默认输出到 `docs/playback-spikes/results/`，同时生成：

- `playback-matrix-<UTC>-<pid>.json`：完整脱敏结果，包含每个样本/设备/网络 cell；
- `playback-matrix-<UTC>-<pid>.md`：可人工复核的摘要和阶段表。

网络/地区标签也可以通过环境变量提供：

```text
DORIYA_MATRIX_NETWORK_LABEL
DORIYA_MATRIX_EGRESS_REGION
```

这两个标签是测试者填写的条件标签，脚本不会主动查询公网 IP 或推断地区。

报告字段包括：

- 样本 ID、可选 file ID、来源 kind、transport、host、port；
- 容器、视频/音频编码、features、字幕；
- 客户端平台/版本矩阵、网络标签、出口地区标签；
- `ticket`、`direct_dns`、`connect`、`http`、`range`、`header`、`decode`、`seek`、`fallback` 各阶段状态、耗时和失败类别；
- 每个样本/设备/网络 cell 的 `pending` 客户端记录。

## 阶段含义

| 阶段 | 本脚本实际测什么 | 是否等于客户端结论 |
| --- | --- | --- |
| `ticket` | 记录 file ID；S0 尚无 v1 ticket API，标 pending | 否 |
| `direct_dns` | 解析源站 host，仅记录地址数量/范围类别 | 否 |
| `connect` | 到源站 host/port 的 TCP 连接 | 否 |
| `http` | HEAD；HEAD 不被支持时用 `GET bytes=0-0` | 否 |
| `range` | `GET` 指定小范围，检查 `206` 和 `Content-Range` | 否 |
| `header` | 带配置 Header 的请求是否成功；跨源重定向是否剥离 Header | 否 |
| `decode` | 需要 W0/A0 真机或客户端 runner，脚本固定 pending | 是，必须真机 |
| `seek` | HTTP 中点 Range，仅记 `probe_kind=http-range` | 否 |
| `fallback` | 需要 v1 server-proxy/HLS 或客户端 runner，S0 固定 pending | 是，需后续实现 |

状态只用 `pass`、`fail`、`skip`、`pending`、`not_applicable`。HTTP 级 seek 通过不得填写为“播放器 seek 通过”。

## 设备/网络矩阵

设备计划：

| 平台 | 版本 | 要求 |
| --- | --- | --- |
| Windows | 10 22H2 | 一台；记录 GPU/驱动 |
| Windows | 11 当前稳定版 | 一台；记录 GPU/驱动 |
| Android | 9 | 至少一台真机 |
| Android | 12 | 至少一台真机 |
| Android | 14 | 至少一台真机 |

Android 优先覆盖 Qualcomm 与 MediaTek、arm64-v8a；模拟器只能用于 UI/网络冒烟，不能填 decode 结论。

网络条件：

| 标签 | 说明 |
| --- | --- |
| `same-LAN` | 同 LAN / 同公网出口（适用时） |
| `phone-hotspot` | 手机热点 |
| `alternate-public-egress` | 与服务器不同的公网出口 |
| `VPN-or-source-proxy` | VPN 或源站代理出口 |

每个设备/网络组合都需要单独记录 `network_label` 和 `egress_region_label`。一次主机探测不能替代四种网络条件下的真机结果。

## 日志禁区

以下内容不得进入控制台、报告、截图、命令参数或提交：

- OpenList Token、Bilibili Cookie、WebDAV Basic 凭据；
- Authorization、Cookie、Referer/UA 的值；
- ticket、access/refresh token、一次性 mock token；
- 完整源 URL、path、query、fragment；
- 原始响应 Header 集合。

允许记录：样本 ID、可选 file ID、kind、transport、host、port、容器/编码、Header 名称、网络/地区标签、HTTP 状态、Range 数字、阶段耗时和失败类别。

## 结果流程

1. 先运行 DryRun，确认样本、设备和 secret/file ID 状态。
2. 在目标网络下运行主机 transport 探测。
3. 在每台 Windows/Android 真机上执行客户端播放，填写 `RESULTS-TEMPLATE.md`。
4. 对 S1-S5 分别记录 decode、真实 seek、字幕、fallback；没有结论就写 `pending`。
5. 汇总后才能讨论直连收益，且必须注明网络、地区、设备、版本和样本缺口。

S2 的 E-AC3 缺口在找到真实样本前始终是 unresolved，不允许用 AAC/FLAC 样本替代后宣称 S2 通过。
