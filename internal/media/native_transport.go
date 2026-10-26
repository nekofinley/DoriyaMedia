package media

import (
	"net/url"
	"strconv"
	"strings"

	"doriyamedia/internal/source"
)

// NativeTransportKind is the transport vocabulary shared by S3 tickets and
// native clients. Direct DASH and the local relay are deliberately absent: S3
// must never offer a transport that it cannot authorize end to end.
type NativeTransportKind string

const (
	NativeTransportDirectHTTP  NativeTransportKind = "direct-http"
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

// NativeTransportFeatureFlags contains only the S3 transport switches. Direct
// DASH and local relay are intentionally not representable here.
type NativeTransportFeatureFlags struct {
	DirectHTTPEnabled    bool `json:"direct_http_enabled"`
	ServerProxyV1Enabled bool `json:"server_proxy_v1_enabled"`
	ServerHLSV1Enabled   bool `json:"server_hls_v1_enabled"`
}

// NativeTransportInput contains already-resolved source facts. SourceHeaders is
// used only to decide whether direct-http may emit the fixed safe DTO; it is
// never copied into server-proxy or server-hls candidates.
type NativeTransportInput struct {
	Probe         *Probe
	SourceURL     string
	SourceHeaders map[string]string
	MultiPart     bool
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
	Codec  string `json:"codec"`
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
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
		summary.Video = &NativeVideoSummary{
			Codec:  video.CodecName,
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

// SelectNativeTransports evaluates the S3 transport set as a pure function.
// Client preferences only reorder already-eligible transports; they cannot add
// direct-dash, local-relay, or a transport disabled by feature flags.
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
			!input.Features.ServerProxyV1Enabled &&
			!input.Features.ServerHLSV1Enabled:
			reason = "all S3 native transports are disabled"
		case input.MultiPart && !input.Features.ServerHLSV1Enabled:
			reason = "multi-part media requires server-hls, which is disabled"
		case !nativePlaybackOK && !input.Features.ServerHLSV1Enabled:
			reason = "client cannot play the source container or audio set, and server-hls is disabled"
		}
		return unsupportedNativeSelection(selection, reason)
	}

	selection.Candidates = candidates
	return selection
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
