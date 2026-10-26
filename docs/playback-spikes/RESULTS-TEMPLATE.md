# S0 真机与网络结果模板

> 只填脱敏结果。不要粘贴源 URL、path、query、Header 值、Cookie、Token 或 ticket。

## 运行信息

| 字段 | 填写 |
| --- | --- |
| 执行日期 | `<YYYY-MM-DD>` |
| 执行人 | `<name>` |
| 主机探测报告 | `docs/playback-spikes/results/<report>.json` |
| 客户端版本 | Windows `<version>` / Android `<version>` |
| 网络标签 | `same-LAN` / `phone-hotspot` / `alternate-public-egress` / `VPN-or-source-proxy` |
| 出口地区标签 | `<region label>` |
| 服务端版本 | `<version/commit>` |
| 备注 | `<notes>` |

## 样本结果

file ID 只从运行时参数/环境变量填写；空白表示未提供。S2 在没有真实 E-AC3 样本时必须保持 `pending`。

| ID | file ID | 来源 host（不含 path） | 容器 | 视频 | 音频 | 字幕 | 网络 | 出口地区 | 平台/版本 | 总状态 | 备注 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| S1 | `<id or ->` | `<host:port>` | mp4 | h264 High | aac | - | `<label>` | `<label>` | `<platform/version>` | pending | |
| S2 | `<id or ->` | `<host:port>` | mkv | h264 | eac3 | - | `<label>` | `<label>` | `<platform/version>` | **pending** | 真实 E-AC3 样本缺失时不得填 pass |
| S3 | `<id or ->` | `<host:port>` | mkv | hevc Main10 | flac | ass | `<label>` | `<label>` | `<platform/version>` | pending | |
| S3-PGS | `<id or ->` | `<host:port>` | mkv | h264 High10 | flac | pgs | `<label>` | `<label>` | `<platform/version>` | pending | 图形字幕补充样本 |
| S4 | `<id or ->` | `<host:port>` | dash | h264 | aac | - | `<label>` | `<label>` | `<platform/version>` | pending | 视频/音频双 URL |
| S5 | `-` | `127.0.0.1` | mp4 | h264 | aac | - | `<label>` | `<label>` | `<platform/version>` | pending | loopback mock |

## 阶段结果

每个样本/客户端填一组。`seek` 必须区分 HTTP Range 和真实播放器 seek；`decode` 必须是实际客户端解码。

| 样本 | 客户端/引擎 | ticket | direct_dns | connect | http | range | header | decode | seek（player） | fallback | 失败类别 | 证据/备注 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| S1 | `<Windows/mpv或Android/Media3>` | pending | pending | pending | pending | pending | pending | pending | pending | pending | - | |
| S2 | `<client>` | pending | pending | pending | pending | pending | pending | pending | pending | pending | - | E-AC3 sample gap |
| S3 | `<client>` | pending | pending | pending | pending | pending | pending | pending | pending | pending | - | ASS/HEVC10/FLAC |
| S3-PGS | `<client>` | pending | pending | pending | pending | pending | pending | pending | pending | pending | - | PGS |
| S4 | `<client>` | pending | pending | pending | pending | pending | pending | pending | pending | pending | - | 双流合流 |
| S5 | `<client>` | pending | pending | pending | pending | pending | pending | pending | pending | pending | - | mock |

状态取值：`pass` / `fail` / `skip` / `pending` / `not_applicable`。

`header` 只记录 Header 名称和结果，不记录值。跨源重定向如果要求转发源站 Authorization/Cookie/Referer，应如实记 fail 或 blocked，不得降低安全策略。

## 设备矩阵

| 设备 ID | 平台/版本 | 型号 | SoC/CPU | GPU/驱动 | 解码器/播放引擎 | 屏幕/DPI | 结果 | 备注 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `<device-id>` | Windows 10 22H2 | `<model>` | `<cpu>` | `<gpu/driver>` | `<mpv/VLC等>` | `<screens/dpi>` | pending | |
| `<device-id>` | Windows 11 current | `<model>` | `<cpu>` | `<gpu/driver>` | `<mpv/VLC等>` | `<screens/dpi>` | pending | |
| `<device-id>` | Android 9 | `<model>` | `<Qualcomm/MediaTek>` | `<gpu>` | `<Media3/libmpv>` | `<dpi>` | pending | arm64-v8a |
| `<device-id>` | Android 12 | `<model>` | `<Qualcomm/MediaTek>` | `<gpu>` | `<Media3/libmpv>` | `<dpi>` | pending | arm64-v8a |
| `<device-id>` | Android 14 | `<model>` | `<Qualcomm/MediaTek>` | `<gpu>` | `<Media3/libmpv>` | `<dpi>` | pending | arm64-v8a |

## 网络矩阵

| 网络标签 | 出口地区标签 | 运营商/线路 | 时间 | S1 | S2 | S3 | S3-PGS | S4 | S5 | 备注 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| same-LAN | `<label>` | `<isp>` | `<time>` | pending | pending | pending | pending | pending | pending | |
| phone-hotspot | `<label>` | `<carrier>` | `<time>` | pending | pending | pending | pending | pending | pending | |
| alternate-public-egress | `<label>` | `<isp>` | `<time>` | pending | pending | pending | pending | pending | pending | |
| VPN-or-source-proxy | `<label>` | `<provider>` | `<time>` | pending | pending | pending | pending | pending | pending | |

## 播放质量

| 样本 | 起播耗时 | 30 分钟稳定 | 20 次 seek 成功数 | 音画差 | CPU/GPU | 内存 | 备注 |
| --- | ---: | --- | ---: | --- | --- | --- | --- |
| S1 | `<ms>` | pending | `<n>/20` | `<ms>` | `<value>` | `<value>` | |
| S2 | `<ms>` | pending | `<n>/20` | `<ms>` | `<value>` | `<value>` | sample gap |
| S3 | `<ms>` | pending | `<n>/20` | `<ms>` | `<value>` | `<value>` | |
| S3-PGS | `<ms>` | pending | `<n>/20` | `<ms>` | `<value>` | `<value>` | |
| S4 | `<ms>` | pending | `<n>/20` | `<ms>` | `<value>` | `<value>` | |
| S5 | `<ms>` | pending | `<n>/20` | `<ms>` | `<value>` | `<value>` | loopback mock |

## 未解决项

- [ ] S2 找到真实 MKV H.264 / E-AC3 样本并完成测试。
- [ ] Windows 10/11 与 Android 9/12/14 真机 decode 结果。
- [ ] 真实播放器 20 次随机 seek。
- [ ] 四种网络/出口地区分别记录。
- [ ] v1 ticket 与 server-proxy/server-HLS fallback 可测后补测。
- [ ] `<追加项>`。

## 结论

只写有证据支持的结论：

- 传输层：`<summary>`
- 客户端解码：`<summary or pending>`
- Header/重定向安全：`<summary>`
- Range/seek：`<summary>`
- fallback：`<summary or pending>`
- 是否足以支持继续 Windows/Android 客户端投入：`<yes/no/pending + reason>`
