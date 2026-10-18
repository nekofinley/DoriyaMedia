package media

import (
	"fmt"
	"strings"
)

// ClientKind is the playback capability class of the requesting client.
type ClientKind string

const (
	ClientSafari  ClientKind = "safari"  // iOS/iPadOS/macOS Safari — native AVPlayer, no MSE
	ClientChrome  ClientKind = "chrome"  // Chrome/Edge/Chromium-based + Android — MSE available
	ClientFirefox ClientKind = "firefox" // no HEVC
)

// DetectClient maps a User-Agent to a capability class.
func DetectClient(ua string) ClientKind {
	ua = strings.ToLower(ua)
	isIOS := strings.Contains(ua, "iphone") || strings.Contains(ua, "ipad") || strings.Contains(ua, "ipod") ||
		(strings.Contains(ua, "macintosh") && strings.Contains(ua, "touch"))
	if isIOS {
		return ClientSafari
	}
	isSafariDesktop := strings.Contains(ua, "safari") && !strings.Contains(ua, "chrome") &&
		!strings.Contains(ua, "chromium") && !strings.Contains(ua, "edg") && !strings.Contains(ua, "opr")
	if isSafariDesktop {
		return ClientSafari
	}
	if strings.Contains(ua, "firefox") {
		return ClientFirefox
	}
	return ClientChrome
}

// Decision is the playback plan for one file on one client.
//
// Mode is one of:
//
//	direct      — 302 to the source's own URL; nothing touches this server
//	proxy       — the same bytes streamed through this server
//	remux       — video stream copied into HLS (audio re-encoded when needed)
//	unsupported — nothing here can play it: the video codec has no decoding path
//	              on this client, and re-encoding the video on the fly is
//	              deliberately not offered. A realtime H.264 encode that cannot
//	              keep up is worse than an honest refusal — it burns the CPU and
//	              still leaves the viewer watching a spinner. URL stays empty and
//	              Reason says why, so the player can explain itself.
type Decision struct {
	Mode       string  `json:"mode"` // direct | proxy | remux | unsupported
	Kind       string  `json:"kind"` // mp4 | hls
	URL        string  `json:"url,omitempty"`
	MimeType   string  `json:"mime_type,omitempty"`
	Reason     string  `json:"reason"`
	Duration   float64 `json:"duration,omitempty"`
	VideoCodec string  `json:"video_codec,omitempty"`
	AudioCodec string  `json:"audio_codec,omitempty"`
	VideoIndex int     `json:"video_index"`
	AudioIndex int     `json:"audio_index"`
	Container  string  `json:"container,omitempty"`
	Resolution string  `json:"resolution,omitempty"`

	// Alternates are fallback strategies the client may auto-try when the
	// primary one fails, in the order they should be tried. Only strategies that
	// can work for this client are listed — see fallbackPlan.
	Alternates []DecisionAlt `json:"alternates,omitempty"`
}

// DecisionAlt is one fallback playback strategy. Decide fills in the Mode; the
// API layer adds the URL, because building a signed link is its job, not the
// decision engine's.
type DecisionAlt struct {
	Mode     string `json:"mode"`
	URL      string `json:"url"`
	MimeType string `json:"mime_type,omitempty"`
}

var (
	safariVideo  = map[string]bool{"h264": true, "hevc": true}
	safariAudio  = map[string]bool{"aac": true, "mp3": true, "flac": true, "alac": true}
	chromeVideo  = map[string]bool{"h264": true, "hevc": true, "av1": true, "vp9": true, "vp8": true}
	chromeAudio  = map[string]bool{"aac": true, "mp3": true, "opus": true, "vorbis": true, "flac": true}
	firefoxVideo = map[string]bool{"h264": true, "vp9": true, "vp8": true, "av1": true}
	firefoxAudio = map[string]bool{"aac": true, "mp3": true, "opus": true, "vorbis": true, "flac": true}
)

// mp4-family tokens as reported by ffprobe format_name
var mp4Family = map[string]bool{"mov": true, "mp4": true, "m4a": true, "m4v": true, "mj2": true, "3gp": true, "3g2": true}

func containerTokens(formatName string) []string {
	parts := strings.Split(formatName, ",")
	var out []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func containerMatches(formatName string, allowWebM bool) bool {
	found := false
	for _, t := range containerTokens(formatName) {
		if mp4Family[t] {
			found = true
			break
		}
		if allowWebM && (t == "webm" || t == "matroska") {
			// only plain webm (vp9/av1) counts; matroska alone is not webm
			if t == "webm" {
				found = true
				break
			}
		}
	}
	return found
}

// extOf returns lowercase extension (with dot) of a path/URL.
func extOf(path string) string {
	p := path
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	if i := strings.LastIndex(p, "."); i >= 0 && i > strings.LastIndex(p, "/") {
		return strings.ToLower(p[i:])
	}
	return ""
}

// CanDirectPlay checks whether the probe is playable natively by the client
// straight from a progressive URL (no server-side work).
//
// reportedAudio is what the client said it can decode (ffprobe codec names), or
// nil when it said nothing. A report only ever widens the answer: AC-3/E-AC-3/DTS
// live in the OS on Windows and Android and nowhere on Linux, so which of them a
// browser plays is a property of the machine, not of the browser's name — and the
// audio tables below can only ever guess. Codecs the client did not mention keep
// the table's verdict, so an older client changes nothing.
func CanDirectPlay(probe *Probe, path string, client ClientKind, reportedAudio map[string]bool) (bool, string) {
	if probe == nil || probe.Video() == nil {
		return false, "无法识别视频流"
	}
	ext := extOf(path)

	switch client {
	case ClientSafari:
		if ext != ".mp4" && ext != ".m4v" && ext != ".mov" {
			return false, "Safari 仅支持 MP4/MOV 直连"
		}
		if !containerMatches(probe.Format.FormatName, false) {
			return false, "容器不是 MP4"
		}
	case ClientChrome:
		// Chromium ≥110 demuxes Matroska in <video> (Edge shipped it even
		// earlier). Whether the codec inside actually decodes (HEVC needs an
		// OS/extension decoder) is re-checked client-side — the player falls
		// back to cloud transcode automatically when it can't.
		if !containerMatches(probe.Format.FormatName, true) && ext != ".mkv" {
			return false, "容器不支持直连"
		}
	case ClientFirefox:
		if ext != ".mp4" && ext != ".webm" {
			return false, "Firefox 直连仅支持 MP4/WebM"
		}
	}

	v := probe.Video()
	var videoOK, audioOK bool
	var audioFail string

	// audioPlayable: the codec table's answer, unless the client itself said it
	// can decode this one.
	audioPlayable := func(table map[string]bool, codec string) bool {
		return table[codec] || reportedAudio[codec]
	}

	// 10-bit H.264 (Hi10P) has no consumer hardware decode path — no client
	// can direct-play it; everything else is gated per client below.
	if v != nil && v.CodecName == "h264" && strings.Contains(strings.ToLower(v.Profile), "10") {
		return false, "10bit H.264(Hi10P)没有硬件解码路径"
	}

	switch client {
	case ClientSafari:
		if v.CodecName == "hevc" {
			if p := v.DVProfile(); p == 5 {
				return false, "杜比视界 Profile 5 需转封装"
			}
			if strings.EqualFold(v.CodecTagString, "hev1") {
				return false, "HEVC 编码标签为 hev1,Safari 需要 hvc1"
			}
			videoOK = true
		} else {
			videoOK = safariVideo[v.CodecName]
		}
		if a := firstAudio(probe); a != nil {
			audioOK = audioPlayable(safariAudio, a.CodecName)
			if !audioOK {
				audioFail = "音频 " + strings.ToUpper(a.CodecName) + " iOS 不支持"
			}
		} else {
			audioOK = true // video-only file
		}
	case ClientChrome:
		videoOK = chromeVideo[v.CodecName]
		if a := firstAudio(probe); a != nil {
			audioOK = audioPlayable(chromeAudio, a.CodecName)
			if !audioOK {
				audioFail = "音频 " + strings.ToUpper(a.CodecName) + " 不支持"
			}
		} else {
			audioOK = true
		}
	case ClientFirefox:
		videoOK = firefoxVideo[v.CodecName]
		if a := firstAudio(probe); a != nil {
			audioOK = audioPlayable(firefoxAudio, a.CodecName)
			if !audioOK {
				audioFail = "音频 " + strings.ToUpper(a.CodecName) + " 不支持"
			}
		} else {
			audioOK = true
		}
	}
	if !videoOK {
		return false, fmt.Sprintf("视频编码 %s 不支持直连", strings.ToUpper(v.CodecName))
	}
	if !audioOK {
		return false, audioFail
	}
	return true, ""
}

// hlsVideoCopyOK reports whether the video stream may be *copied* into the
// HLS output and still be decoded by this client's playback stack.
//
// hls.js (Chrome/Edge/Firefox via MSE) cannot demux HEVC from MPEG-TS at all,
// and Hi10P H.264 is undecodable by consumer hardware paths — both need a real
// transcode to H.264.
//
// HEVC for Apple is gated on appleHEVCCopy because Apple's HLS stack does not
// decode HEVC carried in MPEG-TS: the copy plays audio with no picture at all.
// With the session packager on, the copy is fMP4 with an hvc1 sample entry, which
// is what Apple actually wants — and what a browser may support too, which is why
// an MSE client is allowed to copy HEVC when it says its MediaSource handles it
// (caps.HEVC). Without that report the safe answer stays "cannot play here".
func hlsVideoCopyOK(v *ProbeStream, client ClientKind, appleHEVCCopy, clientHEVC bool) bool {
	if v == nil {
		return false
	}
	switch v.CodecName {
	case "h264":
		// "High 10" / "Main 10" profiles are 10-bit — not playable via MSE
		return !strings.Contains(strings.ToLower(v.Profile), "10")
	case "hevc":
		if client == ClientSafari {
			return appleHEVCCopy
		}
		// Other clients only copy HEVC fMP4 if they reported the capability.
		return clientHEVC
	}
	return false
}

// hevcCopyBlockedForApple reports the one case where the video *could* be copied
// but Apple's player will not decode it, so the caller can explain itself
// instead of silently transcoding.
func hevcCopyBlockedForApple(v *ProbeStream, client ClientKind, appleHEVCCopy bool) bool {
	return v != nil && v.CodecName == "hevc" && client == ClientSafari && !appleHEVCCopy
}

// unsupported records a file this client cannot play, carrying the reason the
// player shows the viewer. Kind stays empty: there is no stream to attach.
func unsupported(d *Decision, why string) *Decision {
	d.Mode, d.Kind, d.Reason = "unsupported", "", why
	return d
}

func firstAudio(p *Probe) *ProbeStream {
	streams := p.AudioStreams()
	if len(streams) > 0 {
		return streams[0]
	}
	return nil
}

// DecideOptions carries the inputs to a decision that are not properties of the
// file itself.
type DecideOptions struct {
	// Force is the user's manual override: "" | direct | proxy | remux | transcode.
	Force string
	// HasDirect: the source driver can produce a 302-able URL.
	HasDirect bool
	// DirectHeaders: required headers for the direct link (non-empty means the
	// browser cannot fetch it and we must not 302).
	DirectHeaders map[string]string
	// AppleHEVCCopy allows copying HEVC into MPEG-TS for Apple clients. Off by
	// default: Apple's HLS stack does not decode HEVC from MPEG-TS, so such a
	// stream plays audio with no picture. Turn it on only for a device verified
	// to play HEVC-in-TS.
	AppleHEVCCopy bool
	// ClientHEVC is the client's own report that its playback stack handles HEVC.
	// A browser answers this by asking MediaSource about an hvc1 codec string, so
	// a machine with hardware HEVC support copies instead of transcoding; without
	// the report nothing changes.
	ClientHEVC bool
	// ClientNoHEVC is the opposite report: the client looked and cannot play HEVC.
	// The codec tables assume a browser can, which is wrong on plenty of machines,
	// and handing such a client a stream it cannot decode just looks broken. So a
	// negative report rules out both direct play and copying.
	ClientNoHEVC bool
	// ClientAudio is the set of audio codecs the client reported it can decode
	// (ffprobe names, e.g. "eac3"). It only ever widens direct play; see
	// CanDirectPlay.
	ClientAudio map[string]bool
	// MultiPart marks a file whose media is several upstream streams that have
	// to be muxed together (bilibili DASH: the picture and the sound live at two
	// URLs). Direct play and proxying are both impossible for such a file — the
	// single URL a client could be pointed at carries the video only, so either
	// mode plays a silent picture — which leaves a server-side remux as the one
	// honest offer.
	MultiPart bool
}

// Decide produces the playback plan: what to play first, and which strategies are
// worth falling back to if that fails on this client.
func Decide(probe *Probe, path string, client ClientKind, opts DecideOptions) *Decision {
	d := decide(probe, path, client, opts)
	if probe != nil {
		for _, m := range fallbackPlan(probe, path, client, opts, d) {
			// Only the strategy is decided here; the API layer turns each one
			// into a signed URL.
			d.Alternates = append(d.Alternates, DecisionAlt{Mode: m})
		}
	}
	return d
}

// canDirectPlay is CanDirectPlay plus the client's own report: a client that
// examined itself and found no HEVC decoder must not be handed HEVC just because
// the generic codec table assumes browsers cope.
func canDirectPlay(probe *Probe, path string, client ClientKind, opts DecideOptions) (bool, string) {
	can, why := CanDirectPlay(probe, path, client, opts.ClientAudio)
	if can && opts.ClientNoHEVC {
		if v := probe.Video(); v != nil && v.CodecName == "hevc" {
			return false, "客户端自报不支持 HEVC"
		}
	}
	return can, why
}

// fallbackPlan lists the strategies a client may try after the primary one fails,
// cheapest first, and only those that could actually work for this client:
//
//	direct   the same bytes over the source's own URL  — only if the client can
//	                                                      natively play the file
//	proxy    the same bytes through this server        — same condition; this is a
//	                                                      transport fallback, not a
//	                                                      codec one
//	remux    video stream copied into HLS              — only if the client's own
//	                                                      stack can decode the copy
//
// Re-encoding is absent on purpose: it is not a fallback but a different (and
// expensive) promise, and a server that cannot encode in realtime would only
// make the viewer wait for a stream that never arrives.
//
// The filtering is the point. Offering "direct" for a file the client cannot
// play is not a fallback but a guaranteed dead end, and it is reached precisely
// when the viewer is already waiting: a slow first segment used to look like a
// failure, and the chain answered with a 302 to a Matroska file on iOS — the one
// thing that client can never open.
func fallbackPlan(probe *Probe, path string, client ClientKind, opts DecideOptions, primary *Decision) []string {
	var out []string
	canDirect, _ := canDirectPlay(probe, path, client, opts)
	// A multi-stream source has no single URL to fall back to: offering direct or
	// proxy would only trade one failed attempt for another, both of them silent.
	if canDirect && !opts.MultiPart {
		if opts.HasDirect && len(opts.DirectHeaders) == 0 && primary.Mode != "direct" {
			out = append(out, "direct")
		}
		if primary.Mode != "proxy" {
			out = append(out, "proxy")
		}
	}
	if primary.Mode != "remux" && hlsVideoCopyOK(probe.Video(), client, opts.AppleHEVCCopy, opts.ClientHEVC) {
		out = append(out, "remux")
	}
	return out
}

// decide is the decision proper: the single strategy to start with.
func decide(probe *Probe, path string, client ClientKind, opts DecideOptions) *Decision {
	force := opts.Force
	hasDirect := opts.HasDirect
	directHeaders := opts.DirectHeaders
	d := &Decision{VideoIndex: -1, AudioIndex: -1}
	if probe == nil {
		return unsupported(d, "无法探测媒体信息,无法判断这个文件能不能播")
	}
	v := probe.Video()
	if v != nil {
		d.VideoIndex = v.Index
		d.VideoCodec = v.CodecName
		d.Resolution = fmt.Sprintf("%dx%d", v.Width, v.Height)
	}
	if a := firstAudio(probe); a != nil {
		d.AudioIndex = a.Index
		d.AudioCodec = a.CodecName
	}
	d.Duration = probe.Duration()
	d.Container = probe.Format.FormatName

	if force == "direct" {
		if hasDirect && len(directHeaders) == 0 {
			d.Mode, d.Kind, d.Reason = "direct", "mp4", "手动指定直连"
		} else {
			d.Mode, d.Kind, d.Reason = "proxy", "mp4", "手动指定直连但源不支持,退化为代理"
		}
		return d
	}
	if force == "proxy" {
		d.Mode, d.Kind, d.Reason = "proxy", "mp4", "手动指定代理"
		return d
	}
	// "transcode" is not honoured any more — not even as an override. A client
	// still asking for it (an old bookmark, a stale page) lands on the automatic
	// decision below, which never re-encodes either.
	if force == "remux" {
		// A remux only copies the video stream, which is only useful when this
		// client can actually decode what gets copied. For a codec it cannot
		// (AV1 / VP9 / MPEG-2 / 10-bit H.264 …) the copy would play audio over a
		// black picture — and with video re-encoding off the table, the honest
		// answer is that this file cannot be played here at all.
		if hlsVideoCopyOK(v, client, opts.AppleHEVCCopy, opts.ClientHEVC) {
			d.Mode, d.Kind, d.Reason = "remux", "hls", "手动指定转封装"
			return d
		}
		if hevcCopyBlockedForApple(v, client, opts.AppleHEVCCopy) {
			return unsupported(d, "手动指定转封装,但 Apple 不解码 MPEG-TS 里的 HEVC(只有声音没有画面);转码已禁用,无法播放")
		}
		codec := "该视频编码"
		if v != nil && v.CodecName != "" {
			codec = strings.ToUpper(v.CodecName)
		}
		return unsupported(d, fmt.Sprintf("手动指定转封装,但 %s 无法被本机解码(只有声音没有画面);转码已禁用,无法播放", codec))
	}

	can, why := canDirectPlay(probe, path, client, opts)
	if opts.MultiPart {
		// The source publishes the picture and the sound separately, so there is
		// no URL to play directly and no URL to proxy: both would give the client
		// a video-only stream. Muxing them is the only mode that can work, and
		// the client still has to be able to decode the copy.
		if hlsVideoCopyOK(v, client, opts.AppleHEVCCopy, opts.ClientHEVC) {
			d.Mode, d.Kind = "remux", "hls"
			d.Reason = "源的音视频是两条流(DASH),服务端合流为 HLS(视频流直拷)"
			return d
		}
		if hevcCopyBlockedForApple(v, client, opts.AppleHEVCCopy) {
			return unsupported(d, "源的音视频是两条流(DASH),需服务端合流,但 Apple 不解码 MPEG-TS 里的 HEVC(只有声音没有画面);转码已禁用,无法播放")
		}
		codec := "该视频编码"
		if v != nil && v.CodecName != "" {
			codec = strings.ToUpper(v.CodecName)
		}
		return unsupported(d, fmt.Sprintf("源的音视频是两条流(DASH),需服务端合流,但 %s 无法被本机解码(只有声音没有画面);转码已禁用,无法播放", codec))
	}
	if can {
		if hasDirect && len(directHeaders) == 0 {
			d.Mode, d.Kind, d.Reason = "direct", "mp4", "客户端原生支持,302 直连"
		} else {
			d.Mode, d.Kind, d.Reason = "proxy", "mp4", "客户端原生支持,源不支持 302,走代理"
		}
		return d
	}

	// Video copy is only possible when the client's HLS stack can actually
	// decode the copied stream; without that there is nothing left to offer.
	if hlsVideoCopyOK(v, client, opts.AppleHEVCCopy, opts.ClientHEVC) {
		d.Mode, d.Kind = "remux", "hls"
		d.Reason = why + " → HLS 转封装(视频流直拷)"
		return d
	}
	if hevcCopyBlockedForApple(v, client, opts.AppleHEVCCopy) {
		return unsupported(d, why+" → Apple 不解码 MPEG-TS 里的 HEVC(直拷会只有声音没有画面);转码已禁用,无法播放")
	}
	return unsupported(d, why+" → 需要把视频重新编码才能播,而转码已禁用")
}
