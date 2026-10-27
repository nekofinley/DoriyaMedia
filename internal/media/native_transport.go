package media

import (
	"math"
	"net/url"
	"strconv"
	"strings"

	"doriyamedia/internal/source"
)

// NativeTransportKind is the transport vocabulary shared by playback tickets
// and native clients. The local relay remains deliberately absent.
type NativeTransportKind string

const (
	NativeTransportDirectHTTP  NativeTransportKind = "direct-http"
	NativeTransportDirectDASH  NativeTransportKind = "direct-dash"
	NativeTransportServerProxy NativeTransportKind = "server-proxy"
	NativeTransportServerHLS   NativeTransportKind = "server-hls"
	NativeTransportUnsupported NativeTransportKind = "unsupported"
)

// NativeClientCapabilities is the client-owned part of a native playback
// decision. It is an input to authorization, never a way to widen permissions.
type NativeClientCapabilities struct {
	Platform            string                `json:"platform"`
	Version             string                `json:"version"`
	Engine              string                `json:"engine"`
	Containers          []string              `json:"containers"`
	VideoCodecs         []string              `json:"video_codecs"`
	AudioCodecs         []string              `json:"audio_codecs"`
	SubtitleCodecs      []string              `json:"subtitle_codecs"`
	SupportsRange       bool                  `json:"supports_range"`
	SupportsHeaders     bool                  `json:"supports_headers"`
	SupportsDASHMerge   bool                  `json:"supports_dash_merge"`
	PreferredTransports []NativeTransportKind `json:"preferred_transports"`
}

// NativeTransportFeatureFlags contains independently switchable transport
// gates. Direct DASH defaults off and the local relay is not representable.
type NativeTransportFeatureFlags struct {
	DirectHTTPEnabled    bool `json:"direct_http_enabled"`
	DirectDASHEnabled    bool `json:"direct_dash_enabled"`
	ServerProxyV1Enabled bool `json:"server_proxy_v1_enabled"`
	ServerHLSV1Enabled   bool `json:"server_hls_v1_enabled"`
}

// NativeTransportInput contains already-resolved source facts. SourceHeaders is
// used only for direct-http eligibility; Parts is used only for direct-dash.
// Neither is copied into server-proxy or server-hls candidates.
type NativeTransportInput struct {
	Probe         *Probe
	SourceURL     string
	SourceHeaders map[string]string
	MultiPart     bool
	Parts         []source.Part
	Client        NativeClientCapabilities
	Features      NativeTransportFeatureFlags
}

// NativeTransportCandidate is one eligible transport. Headers is non-nil only
// for direct-http and always contains a validated source.StreamHeaders DTO.
type NativeTransportCandidate struct {
	Kind    NativeTransportKind   `json:"kind"`
	Headers *source.StreamHeaders `json:"headers,omitempty"`
	Range   bool                  `json:"range"`
}

// NativeDirectDASHStream is an internally validated pair member. It contains
// source URLs and headers, so callers must never include it in ticket status or
// candidate-plan JSON.
type NativeDirectDASHStream struct {
	Role    string
	URL     string
	MIME    string
	Headers source.StreamHeaders
}

// NativeTransportSelection is ordered from the preferred candidate to the last
// fallback. When no transport is eligible, Candidates contains exactly one
// unsupported candidate and UnsupportedReason explains the terminal result.
type NativeTransportSelection struct {
	Media             NativeMediaSummary         `json:"media"`
	Candidates        []NativeTransportCandidate `json:"candidates"`
	UnsupportedReason string                     `json:"unsupported_reason,omitempty"`
}

// NativeVideoSummary is the video part of a ticket's media summary.
type NativeVideoSummary struct {
	Codec    string `json:"codec"`
	BitDepth int    `json:"bit_depth,omitempty"`
	HDR      bool   `json:"hdr,omitempty"`
	Width    int    `json:"width,omitempty"`
	Height   int    `json:"height,omitempty"`
}

// NativeAudioSummary is one audio track. Audio is allowed to be an empty list.
type NativeAudioSummary struct {
	Codec    string `json:"codec"`
	Channels int    `json:"channels"`
	Language string `json:"language,omitempty"`
}

// NativeMediaSummary is a compact, client-safe description of the source. It
// contains no source URL, headers, path, title, or credentials.
type NativeMediaSummary struct {
	Container string               `json:"container"`
	Duration  float64              `json:"duration"`
	Video     *NativeVideoSummary  `json:"video,omitempty"`
	Audio     []NativeAudioSummary `json:"audio"`
}

var defaultNativeTransportOrder = [...]NativeTransportKind{
	NativeTransportDirectHTTP,
	NativeTransportDirectDASH,
	NativeTransportServerProxy,
	NativeTransportServerHLS,
}

// SummarizeNativeMedia builds the ticket media summary from a probe. Audio is
// always non-nil so a video-only file serializes as an empty array, not null.
func SummarizeNativeMedia(probe *Probe) NativeMediaSummary {
	summary := NativeMediaSummary{Audio: []NativeAudioSummary{}}
	if probe == nil {
		return summary
	}
	summary.Container = nativeContainerSummary(probe.Format.FormatName)
	summary.Duration = probe.Duration()
	if video := probe.Video(); video != nil {
		bitDepth := video.BitDepth
		if bitDepth == 0 {
			bitDepth = nativeProfileBitDepth(video.Profile)
		}
		summary.Video = &NativeVideoSummary{
			Codec:    video.CodecName,
			BitDepth: bitDepth,
			HDR: video.HDR || video.DVProfile() > 0 ||
				nativeProfileHDR(video.Profile, video.CodecTagString),
			Width:  video.Width,
			Height: video.Height,
		}
	}
	for _, audio := range probe.AudioStreams() {
		channels := audio.Channels
		if channels < 1 {
			// The contract requires a positive channel count. Some declared
			// multi-part streams omit it; mono is the conservative fallback and
			// still tells clients this audio is not surround.
			channels = 1
		}
		summary.Audio = append(summary.Audio, NativeAudioSummary{
			Codec:    audio.CodecName,
			Channels: channels,
			Language: audio.Language(),
		})
	}
	return summary
}

func nativeContainerSummary(formatName string) string {
	tokens := containerTokens(formatName)
	for _, token := range tokens {
		if mp4Family[token] {
			return "mp4"
		}
	}
	for _, token := range tokens {
		switch token {
		case "matroska":
			return "mkv"
		case "mpegts", "m2ts":
			return "mpegts"
		}
	}
	if len(tokens) == 0 {
		return ""
	}
	return normalizeNativeContainer(tokens[0])
}

// SelectNativeTransports evaluates the ticket transport set as a pure function.
// Client preferences only reorder already-eligible transports; they cannot add
// local-relay or a transport disabled by feature flags.
func SelectNativeTransports(input NativeTransportInput) NativeTransportSelection {
	selection := NativeTransportSelection{
		Media:      SummarizeNativeMedia(input.Probe),
		Candidates: []NativeTransportCandidate{},
	}
	if input.Probe == nil || input.Probe.Video() == nil {
		return unsupportedNativeSelection(selection, "media has no probeable video stream")
	}
	if !nativeVideoSupported(input.Probe, input.Client) {
		return unsupportedNativeSelection(selection, "client cannot decode the source video codec")
	}

	nativePlaybackOK := nativeContainerSupported(input.Probe, input.Client) &&
		nativeAudioSetSupported(input.Probe, input.Client)
	eligible := make(map[NativeTransportKind]NativeTransportCandidate, len(defaultNativeTransportOrder))

	var directHeaders *source.StreamHeaders
	if !input.MultiPart &&
		input.Features.DirectHTTPEnabled &&
		nativePlaybackOK &&
		ValidNativeDirectURL(input.SourceURL) {
		headers, err := source.NewStreamHeadersForURL(input.SourceURL, input.SourceHeaders)
		if err == nil && (headers.Empty() || input.Client.SupportsHeaders) {
			directHeaders = &headers
			eligible[NativeTransportDirectHTTP] = NativeTransportCandidate{
				Kind:    NativeTransportDirectHTTP,
				Headers: directHeaders,
				Range:   input.Client.SupportsRange,
			}
		}
	}

	if !input.MultiPart && input.Features.ServerProxyV1Enabled && nativePlaybackOK {
		eligible[NativeTransportServerProxy] = NativeTransportCandidate{
			Kind:  NativeTransportServerProxy,
			Range: input.Client.SupportsRange,
		}
	}

	if input.MultiPart && input.Features.DirectDASHEnabled && nativePlaybackOK {
		if _, ok := ResolveNativeDirectDASHStreams(input.Parts, input.Client); ok {
			eligible[NativeTransportDirectDASH] = NativeTransportCandidate{
				Kind: NativeTransportDirectDASH,
				// Source byte-range support is not probed for either part. Never
				// promise seeking until the two upstream links have been tested.
				Range: false,
			}
		}
	}

	// HLS can repair an incompatible source container or audio track, but it
	// copies video. A source-only audio set or unsafe Header is therefore not a
	// reason to hide HLS, while an undecodable video still is.
	if input.Features.ServerHLSV1Enabled {
		eligible[NativeTransportServerHLS] = NativeTransportCandidate{
			Kind:  NativeTransportServerHLS,
			Range: input.Client.SupportsRange,
		}
	}

	candidates := orderNativeTransportCandidates(eligible, input.Client.PreferredTransports)
	if len(candidates) == 0 {
		reason := "no eligible native transport is enabled"
		switch {
		case !input.Features.DirectHTTPEnabled &&
			!input.Features.DirectDASHEnabled &&
			!input.Features.ServerProxyV1Enabled &&
			!input.Features.ServerHLSV1Enabled:
			reason = "all native transports are disabled"
		case input.MultiPart && !input.Features.ServerHLSV1Enabled:
			reason = "multi-part media has no eligible direct-dash or server-hls transport"
		case !nativePlaybackOK && !input.Features.ServerHLSV1Enabled:
			reason = "client cannot play the source container or audio set, and server-hls is disabled"
		}
		return unsupportedNativeSelection(selection, reason)
	}

	selection.Candidates = candidates
	return selection
}

// ResolveNativeDirectDASHStreams validates and returns exactly one video and
// one audio stream. Any unsafe URL or header, unsupported codec, malformed
// role, unknown MIME, or timeline mismatch rejects the entire candidate.
func ResolveNativeDirectDASHStreams(
	parts []source.Part,
	caps NativeClientCapabilities,
) ([]NativeDirectDASHStream, bool) {
	if !caps.SupportsDASHMerge || len(parts) != 2 {
		return nil, false
	}
	var video, audio *source.Part
	for i := range parts {
		switch role := strings.ToLower(strings.TrimSpace(parts[i].Kind)); role {
		case "video":
			if video != nil {
				return nil, false
			}
			video = &parts[i]
		case "audio":
			if audio != nil {
				return nil, false
			}
			audio = &parts[i]
		default:
			return nil, false
		}
	}
	if video == nil || audio == nil || video.URL == audio.URL ||
		!validNativeDASHDuration(video.Duration) ||
		!validNativeDASHDuration(audio.Duration) ||
		math.Abs(video.Duration-audio.Duration) > 0.25 {
		return nil, false
	}
	clientVideo := nativeCodecSet(caps.VideoCodecs)
	clientAudio := nativeCodecSet(caps.AudioCodecs)
	if !clientVideo[normalizeNativeCodec(video.Codec)] ||
		!clientAudio[normalizeNativeCodec(audio.Codec)] {
		return nil, false
	}
	if !nativeDASHVideoProfileSupported(*video, caps) {
		return nil, false
	}

	streams := make([]NativeDirectDASHStream, 0, 2)
	for _, part := range []*source.Part{video, audio} {
		role := strings.ToLower(strings.TrimSpace(part.Kind))
		if !ValidNativeDirectURL(part.URL) ||
			!validNativeDASHPartMIME(role, part.MIME) {
			return nil, false
		}
		headers, err := source.NewStreamHeadersForURL(part.URL, part.Headers)
		if err != nil || (!headers.Empty() && !caps.SupportsHeaders) {
			return nil, false
		}
		streams = append(streams, NativeDirectDASHStream{
			Role: role, URL: part.URL,
			MIME:    strings.ToLower(strings.TrimSpace(part.MIME)),
			Headers: headers,
		})
	}
	return streams, true
}

func validNativeDASHDuration(value float64) bool {
	return value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

// nativeProfileBitDepth recognizes the profile labels ffprobe and the
// Bilibili codec mapper use. An unknown profile stays at zero rather than
// pretending an unverified stream is 8-bit.
func nativeProfileBitDepth(profile string) int {
	p := strings.ToLower(strings.TrimSpace(profile))
	switch {
	case strings.Contains(p, "12"):
		return 12
	case strings.Contains(p, "10"):
		return 10
	case p == "baseline", p == "main", p == "high",
		p == "extended", p == "main still picture":
		return 8
	default:
		return 0
	}
}

func nativeProfileHDR(profile, tag string) bool {
	p := strings.ToLower(strings.TrimSpace(profile))
	t := strings.ToLower(strings.TrimSpace(tag))
	return strings.Contains(p, "hdr") ||
		strings.Contains(p, "dolby") ||
		p == "dv" || p == "dovi" ||
		strings.HasPrefix(t, "dv")
}

func nativeDASHVideoProfileSupported(part source.Part, caps NativeClientCapabilities) bool {
	bitDepth := part.BitDepth
	if bitDepth == 0 {
		bitDepth = nativeProfileBitDepth(part.Profile)
	}
	hdr := part.HDR || nativeProfileHDR(part.Profile, part.Tag)
	if bitDepth <= 8 && !hdr {
		return true
	}
	// The first direct-dash contract has no separate bit-depth/HDR capability.
	// Only a client that explicitly advertises the profile may receive it;
	// generic h264/hevc is not enough for High 10, Main 10, HDR, or Dolby.
	family := normalizeNativeCodec(part.Codec)
	for _, raw := range caps.VideoCodecs {
		capability := strings.ToLower(strings.TrimSpace(raw))
		if hdr {
			if nativeProfileHDR(capability, "") {
				return true
			}
			continue
		}
		capFamily := normalizeNativeCodec(capability)
		if bitDepth > 8 &&
			(capFamily == family || strings.HasPrefix(capFamily, family)) &&
			nativeProfileBitDepth(capability) == bitDepth {
			return true
		}
	}
	return false
}

func validNativeDASHPartMIME(role, value string) bool {
	switch role {
	case "video":
		return strings.EqualFold(strings.TrimSpace(value), "video/mp4")
	case "audio":
		return strings.EqualFold(strings.TrimSpace(value), "audio/mp4")
	default:
		return false
	}
}

// Kinds returns the candidate kinds in selection order.
func (s NativeTransportSelection) Kinds() []NativeTransportKind {
	out := make([]NativeTransportKind, 0, len(s.Candidates))
	for _, candidate := range s.Candidates {
		out = append(out, candidate.Kind)
	}
	return out
}

func unsupportedNativeSelection(selection NativeTransportSelection, reason string) NativeTransportSelection {
	selection.Candidates = []NativeTransportCandidate{{Kind: NativeTransportUnsupported}}
	selection.UnsupportedReason = reason
	return selection
}

func orderNativeTransportCandidates(
	eligible map[NativeTransportKind]NativeTransportCandidate,
	preferred []NativeTransportKind,
) []NativeTransportCandidate {
	out := make([]NativeTransportCandidate, 0, len(eligible))
	added := make(map[NativeTransportKind]bool, len(eligible))
	for _, kind := range preferred {
		candidate, ok := eligible[kind]
		if !ok || added[kind] {
			continue
		}
		out = append(out, candidate)
		added[kind] = true
	}
	for _, kind := range defaultNativeTransportOrder {
		candidate, ok := eligible[kind]
		if !ok || added[kind] {
			continue
		}
		out = append(out, candidate)
		added[kind] = true
	}
	return out
}

func nativeContainerSupported(probe *Probe, caps NativeClientCapabilities) bool {
	clientContainers := make(map[string]bool, len(caps.Containers))
	for _, container := range caps.Containers {
		if normalized := normalizeNativeContainer(container); normalized != "" {
			clientContainers[normalized] = true
		}
	}
	if len(clientContainers) == 0 {
		return false
	}
	for _, token := range containerTokens(probe.Format.FormatName) {
		if clientContainers[normalizeNativeContainer(token)] {
			return true
		}
	}
	return false
}

func nativeVideoSupported(probe *Probe, caps NativeClientCapabilities) bool {
	video := probe.Video()
	if video == nil || video.CodecName == "" {
		return false
	}
	return nativeCodecSet(caps.VideoCodecs)[normalizeNativeCodec(video.CodecName)]
}

// Direct and proxy preserve every track in the source container. Without a
// selected-audio field, every audio codec must therefore be decodable; HLS can
// later choose or normalize a track without exposing source credentials.
func nativeAudioSetSupported(probe *Probe, caps NativeClientCapabilities) bool {
	audio := probe.AudioStreams()
	if len(audio) == 0 {
		return true
	}
	clientAudio := nativeCodecSet(caps.AudioCodecs)
	for _, stream := range audio {
		if stream.CodecName == "" || !clientAudio[normalizeNativeCodec(stream.CodecName)] {
			return false
		}
	}
	return true
}

func nativeCodecSet(codecs []string) map[string]bool {
	set := make(map[string]bool, len(codecs))
	for _, codec := range codecs {
		if normalized := normalizeNativeCodec(codec); normalized != "" {
			set[normalized] = true
		}
	}
	return set
}

func normalizeNativeContainer(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "matroska":
		return "mkv"
	case "mpegts", "m2ts", "ts":
		return "mpegts"
	default:
		return strings.ToLower(strings.TrimSpace(value))
	}
}

func normalizeNativeCodec(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "avc", "avc1", "h.264":
		return "h264"
	case "h26410", "h26410bit", "hi10p":
		return "h264"
	case "h265", "h.265", "hev1", "hvc1", "hevc10", "hevc10bit":
		return "hevc"
	case "av01":
		return "av1"
	case "vp09":
		return "vp9"
	case "mp4a":
		return "aac"
	case "ac-3":
		return "ac3"
	case "ec-3":
		return "eac3"
	default:
		return strings.ToLower(strings.TrimSpace(value))
	}
}

// ValidNativeDirectURL is the single policy for a direct-http URL. Both
// eligibility and issuance use it so a URL that rotates between those two
// steps cannot become eligible through a looser second check.
func ValidNativeDirectURL(raw string) bool {
	if raw == "" || len(raw) > 4096 || strings.TrimSpace(raw) != raw {
		return false
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] <= 0x20 || raw[i] == 0x7f {
			return false
		}
	}
	if strings.ContainsAny(raw, "#\\") {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || !strings.EqualFold(u.Scheme, "https") {
		return false
	}
	if u.Opaque != "" || u.User != nil || u.Fragment != "" || !validNativeURLAuthority(u) {
		return false
	}
	return true
}

func validNativeURLAuthority(u *url.URL) bool {
	if u == nil || u.Host == "" || u.Hostname() == "" || strings.HasSuffix(u.Host, ":") {
		return false
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return false
		}
	}
	return true
}
