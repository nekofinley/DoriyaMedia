package media

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func nativeTestProbe(format, videoCodec string, audio ...ProbeStream) *Probe {
	p := &Probe{Format: ProbeFormat{
		FormatName: format,
		Duration:   json.Number("123.5"),
	}}
	p.Streams = append(p.Streams, ProbeStream{
		Index:          0,
		CodecType:      "video",
		CodecName:      videoCodec,
		CodecTagString: "avc1",
		Profile:        "High",
		Width:          1920,
		Height:         1080,
	})
	for i := range audio {
		audio[i].Index = i + 1
		audio[i].CodecType = "audio"
		p.Streams = append(p.Streams, audio[i])
	}
	return p
}

func nativeTestCapabilities() NativeClientCapabilities {
	return NativeClientCapabilities{
		Platform:    "windows",
		Version:     "0.1.0",
		Engine:      "mpv",
		Containers:  []string{"mp4", "mkv", "webm"},
		VideoCodecs: []string{"h264", "hevc", "av1"},
		AudioCodecs: []string{"aac", "opus", "flac"},
		SubtitleCodecs: []string{
			"text", "ass", "pgs",
		},
		SupportsRange:     true,
		SupportsHeaders:   true,
		SupportsDASHMerge: false,
		PreferredTransports: []NativeTransportKind{
			NativeTransportDirectHTTP,
			NativeTransportServerProxy,
			NativeTransportServerHLS,
		},
	}
}

func nativeTestFeatures() NativeTransportFeatureFlags {
	return NativeTransportFeatureFlags{
		DirectHTTPEnabled:    true,
		ServerProxyV1Enabled: true,
		ServerHLSV1Enabled:   true,
	}
}

func nativeTestInput() NativeTransportInput {
	return NativeTransportInput{
		Probe:         nativeTestProbe("mov,mp4,m4a,3gp,3g2,mj2", "h264", ProbeStream{CodecName: "aac", Channels: 2}),
		SourceURL:     "https://cdn.example.com/video.mp4?token=one",
		SourceHeaders: map[string]string{"Accept": "video/mp4"},
		Client:        nativeTestCapabilities(),
		Features:      nativeTestFeatures(),
		MultiPart:     false,
	}
}

func TestSummarizeNativeMedia(t *testing.T) {
	probe := nativeTestProbe("matroska,webm", "hevc",
		ProbeStream{CodecName: "aac", Channels: 2, Tags: map[string]string{"language": "jpn"}},
		ProbeStream{CodecName: "flac", Channels: 6, Tags: map[string]string{"language": "eng"}},
	)
	got := SummarizeNativeMedia(probe)
	if got.Container != "mkv" {
		t.Errorf("container = %q, want mkv", got.Container)
	}
	if got.Duration != 123.5 {
		t.Errorf("duration = %v, want 123.5", got.Duration)
	}
	if got.Video == nil || got.Video.Codec != "hevc" || got.Video.Width != 1920 || got.Video.Height != 1080 {
		t.Errorf("video = %#v, want hevc 1920x1080", got.Video)
	}
	wantAudio := []NativeAudioSummary{
		{Codec: "aac", Channels: 2, Language: "jpn"},
		{Codec: "flac", Channels: 6, Language: "eng"},
	}
	if !reflect.DeepEqual(got.Audio, wantAudio) {
		t.Errorf("audio = %#v, want %#v", got.Audio, wantAudio)
	}
}

func TestSummarizeNativeMediaAllowsNoAudio(t *testing.T) {
	got := SummarizeNativeMedia(nativeTestProbe("mov,mp4,m4a,3gp,3g2,mj2", "h264"))
	if got.Audio == nil {
		t.Fatal("audio is nil; a video-only file must serialize as []")
	}
	if len(got.Audio) != 0 {
		t.Fatalf("audio = %#v, want empty", got.Audio)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if !strings.Contains(string(encoded), `"audio":[]`) {
		t.Fatalf("video-only summary JSON = %s, want audio:[]", encoded)
	}

	zero := SummarizeNativeMedia(nil)
	if zero.Audio == nil || zero.Video != nil {
		t.Fatalf("nil probe summary = %#v, want nil video and non-nil empty audio", zero)
	}
}

func TestSelectNativeTransportsOffersAllEnabledS3Candidates(t *testing.T) {
	selection := SelectNativeTransports(nativeTestInput())
	want := []NativeTransportKind{
		NativeTransportDirectHTTP,
		NativeTransportServerProxy,
		NativeTransportServerHLS,
	}
	if got := selection.Kinds(); !reflect.DeepEqual(got, want) {
		t.Fatalf("kinds = %v, want %v (%s)", got, want, selection.UnsupportedReason)
	}
	if selection.UnsupportedReason != "" {
		t.Fatalf("unsupported reason = %q, want empty", selection.UnsupportedReason)
	}
	direct := selection.Candidates[0]
	if direct.Headers == nil || direct.Headers.Accept != "video/mp4" {
		t.Fatalf("direct headers = %#v, want validated header DTO", direct.Headers)
	}
	if !direct.Range {
		t.Fatal("direct range = false, want client capability")
	}
	for _, candidate := range selection.Candidates[1:] {
		if candidate.Headers != nil {
			t.Errorf("%s exposed source headers: %#v", candidate.Kind, candidate.Headers)
		}
	}
}

func TestSelectNativeTransportsClientPreferenceOnlyReordersEligible(t *testing.T) {
	input := nativeTestInput()
	input.Client.PreferredTransports = []NativeTransportKind{
		NativeTransportServerHLS,
		NativeTransportServerProxy,
		NativeTransportServerHLS,
	}
	got := SelectNativeTransports(input).Kinds()
	want := []NativeTransportKind{
		NativeTransportServerHLS,
		NativeTransportServerProxy,
		NativeTransportDirectHTTP, // unmentioned eligible transports remain, in default order
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("kinds = %v, want %v", got, want)
	}

	input.Client.PreferredTransports = []NativeTransportKind{
		NativeTransportServerHLS,
		NativeTransportDirectHTTP,
		"direct-dash",
		"local-relay",
		"made-up",
	}
	input.SourceHeaders = map[string]string{"Authorization": "Bearer source-secret"}
	got = SelectNativeTransports(input).Kinds()
	want = []NativeTransportKind{
		NativeTransportServerHLS,
		NativeTransportServerProxy,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("kinds with ineligible direct = %v, want %v", got, want)
	}
}

func TestSelectNativeTransportsRejectsUnsafeDirectHeadersButKeepsServerFallbacks(t *testing.T) {
	for _, name := range []string{"Cookie", "Authorization", "X-OpenList-Token", "Proxy-Authorization"} {
		t.Run(name, func(t *testing.T) {
			input := nativeTestInput()
			input.SourceHeaders = map[string]string{name: "source-secret-value"}
			selection := SelectNativeTransports(input)
			want := []NativeTransportKind{
				NativeTransportServerProxy,
				NativeTransportServerHLS,
			}
			if got := selection.Kinds(); !reflect.DeepEqual(got, want) {
				t.Fatalf("kinds = %v, want %v", got, want)
			}
			for _, candidate := range selection.Candidates {
				if candidate.Headers != nil {
					t.Fatalf("%s exposed an unsafe source header set", candidate.Kind)
				}
			}
		})
	}
}

func TestSelectNativeTransportsRejectsUnsafeHeaderValuesForDirect(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
	}{
		{"origin path", map[string]string{"Origin": "https://example.com/path"}},
		{"CRLF injection", map[string]string{"User-Agent": "player\r\nAuthorization: Bearer source-secret"}},
		{"oversized", map[string]string{"Accept": strings.Repeat("a", 513)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := nativeTestInput()
			input.SourceHeaders = tc.headers
			want := []NativeTransportKind{
				NativeTransportServerProxy,
				NativeTransportServerHLS,
			}
			if got := SelectNativeTransports(input).Kinds(); !reflect.DeepEqual(got, want) {
				t.Fatalf("kinds = %v, want %v", got, want)
			}
		})
	}
}

func TestSelectNativeTransportsRequiresHeaderCapabilityOnlyForDirect(t *testing.T) {
	input := nativeTestInput()
	input.Client.SupportsHeaders = false
	want := []NativeTransportKind{
		NativeTransportServerProxy,
		NativeTransportServerHLS,
	}
	if got := SelectNativeTransports(input).Kinds(); !reflect.DeepEqual(got, want) {
		t.Fatalf("kinds with required headers = %v, want %v", got, want)
	}

	input.SourceHeaders = nil
	want = []NativeTransportKind{
		NativeTransportDirectHTTP,
		NativeTransportServerProxy,
		NativeTransportServerHLS,
	}
	if got := SelectNativeTransports(input).Kinds(); !reflect.DeepEqual(got, want) {
		t.Fatalf("kinds without headers = %v, want %v", got, want)
	}
}

func TestSelectNativeTransportsDirectURLPolicy(t *testing.T) {
	invalid := []string{
		"",
		"http://cdn.example.com/video.mp4",
		"//cdn.example.com/video.mp4",
		"/video.mp4",
		"https://user:pass@cdn.example.com/video.mp4",
		"https://cdn.example.com/video.mp4#fragment",
		"https://cdn.example.com/video.mp4#",
		"https://cdn.example.com/video .mp4",
		"https:///video.mp4",
		"https://cdn.example.com:bad/video.mp4",
		"https://cdn.example.com:/video.mp4",
		"https://cdn.example.com:65536/video.mp4",
		"https://cdn.example.com\\video.mp4",
	}
	for _, sourceURL := range invalid {
		t.Run(sourceURL, func(t *testing.T) {
			input := nativeTestInput()
			input.SourceURL = sourceURL
			want := []NativeTransportKind{
				NativeTransportServerProxy,
				NativeTransportServerHLS,
			}
			if got := SelectNativeTransports(input).Kinds(); !reflect.DeepEqual(got, want) {
				t.Fatalf("kinds for %q = %v, want %v", sourceURL, got, want)
			}
			if ValidNativeDirectURL(sourceURL) {
				t.Errorf("ValidNativeDirectURL(%q) = true", sourceURL)
			}
		})
	}

	for _, sourceURL := range []string{
		"https://cdn.example.com/video.mp4",
		"HTTPS://cdn.example.com/video.mp4?signature=abc",
		"https://cdn.example.com:8443/video.mp4",
	} {
		input := nativeTestInput()
		input.SourceURL = sourceURL
		if !ValidNativeDirectURL(sourceURL) {
			t.Errorf("ValidNativeDirectURL(%q) = false", sourceURL)
		}
		if got := SelectNativeTransports(input).Kinds()[0]; got != NativeTransportDirectHTTP {
			t.Errorf("first kind for %q = %s, want direct-http", sourceURL, got)
		}
	}
}

func TestSelectNativeTransportsFeatureFlagsFailClosed(t *testing.T) {
	cases := []struct {
		name     string
		features NativeTransportFeatureFlags
		want     []NativeTransportKind
	}{
		{
			name:     "all off",
			features: NativeTransportFeatureFlags{},
			want:     []NativeTransportKind{NativeTransportUnsupported},
		},
		{
			name:     "direct only",
			features: NativeTransportFeatureFlags{DirectHTTPEnabled: true},
			want:     []NativeTransportKind{NativeTransportDirectHTTP},
		},
		{
			name:     "proxy only",
			features: NativeTransportFeatureFlags{ServerProxyV1Enabled: true},
			want:     []NativeTransportKind{NativeTransportServerProxy},
		},
		{
			name:     "hls only",
			features: NativeTransportFeatureFlags{ServerHLSV1Enabled: true},
			want:     []NativeTransportKind{NativeTransportServerHLS},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := nativeTestInput()
			input.Features = tc.features
			selection := SelectNativeTransports(input)
			if got := selection.Kinds(); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("kinds = %v, want %v (%s)", got, tc.want, selection.UnsupportedReason)
			}
			if tc.want[0] == NativeTransportUnsupported && selection.UnsupportedReason == "" {
				t.Fatal("unsupported selection has no reason")
			}
		})
	}
}

func TestSelectNativeTransportsCapabilityMatrix(t *testing.T) {
	t.Run("container requires HLS", func(t *testing.T) {
		input := nativeTestInput()
		input.Probe = nativeTestProbe("mpegts", "h264", ProbeStream{CodecName: "aac", Channels: 2})
		want := []NativeTransportKind{NativeTransportServerHLS}
		if got := SelectNativeTransports(input).Kinds(); !reflect.DeepEqual(got, want) {
			t.Fatalf("kinds = %v, want %v", got, want)
		}
	})

	t.Run("unsupported audio requires HLS", func(t *testing.T) {
		input := nativeTestInput()
		input.Probe = nativeTestProbe("mov,mp4,m4a,3gp,3g2,mj2", "h264",
			ProbeStream{CodecName: "dts", Channels: 6})
		want := []NativeTransportKind{NativeTransportServerHLS}
		if got := SelectNativeTransports(input).Kinds(); !reflect.DeepEqual(got, want) {
			t.Fatalf("kinds = %v, want %v", got, want)
		}
	})

	t.Run("every audio track must be directly decodable", func(t *testing.T) {
		input := nativeTestInput()
		input.Probe = nativeTestProbe("mov,mp4,m4a,3gp,3g2,mj2", "h264",
			ProbeStream{CodecName: "aac", Channels: 2},
			ProbeStream{CodecName: "dts", Channels: 6})
		want := []NativeTransportKind{NativeTransportServerHLS}
		if got := SelectNativeTransports(input).Kinds(); !reflect.DeepEqual(got, want) {
			t.Fatalf("kinds = %v, want %v", got, want)
		}
	})

	t.Run("video codec is mandatory for HLS too", func(t *testing.T) {
		input := nativeTestInput()
		input.Probe = nativeTestProbe("mpegts", "mpeg2video", ProbeStream{CodecName: "aac", Channels: 2})
		selection := SelectNativeTransports(input)
		if got := selection.Kinds(); !reflect.DeepEqual(got, []NativeTransportKind{NativeTransportUnsupported}) {
			t.Fatalf("kinds = %v, want unsupported", got)
		}
		if !strings.Contains(selection.UnsupportedReason, "video") {
			t.Fatalf("reason = %q, want video-codec cause", selection.UnsupportedReason)
		}
	})

	t.Run("probe without video is unsupported", func(t *testing.T) {
		input := nativeTestInput()
		input.Probe = &Probe{Format: ProbeFormat{FormatName: "mp3"}}
		selection := SelectNativeTransports(input)
		if got := selection.Kinds(); !reflect.DeepEqual(got, []NativeTransportKind{NativeTransportUnsupported}) {
			t.Fatalf("kinds = %v, want unsupported", got)
		}
	})

	t.Run("video-only direct playback", func(t *testing.T) {
		input := nativeTestInput()
		input.Probe = nativeTestProbe("mov,mp4,m4a,3gp,3g2,mj2", "h264")
		want := []NativeTransportKind{
			NativeTransportDirectHTTP,
			NativeTransportServerProxy,
			NativeTransportServerHLS,
		}
		if got := SelectNativeTransports(input).Kinds(); !reflect.DeepEqual(got, want) {
			t.Fatalf("kinds = %v, want %v", got, want)
		}
	})
}

func TestSelectNativeTransportsMultipartIsHLSOnly(t *testing.T) {
	input := nativeTestInput()
	input.MultiPart = true
	input.Client.SupportsDASHMerge = true
	input.Client.PreferredTransports = []NativeTransportKind{
		NativeTransportDirectHTTP,
		NativeTransportServerProxy,
		"direct-dash",
		"local-relay",
		NativeTransportServerHLS,
	}
	selection := SelectNativeTransports(input)
	want := []NativeTransportKind{NativeTransportServerHLS}
	if got := selection.Kinds(); !reflect.DeepEqual(got, want) {
		t.Fatalf("kinds = %v, want %v", got, want)
	}
	if selection.Candidates[0].Headers != nil {
		t.Fatal("server-hls multipart candidate exposed source headers")
	}
}

func TestSelectNativeTransportsMultipartFailsClosedWithoutHLS(t *testing.T) {
	input := nativeTestInput()
	input.MultiPart = true
	input.Features.ServerHLSV1Enabled = false
	selection := SelectNativeTransports(input)
	if got := selection.Kinds(); !reflect.DeepEqual(got, []NativeTransportKind{NativeTransportUnsupported}) {
		t.Fatalf("kinds = %v, want unsupported", got)
	}
	if !strings.Contains(selection.UnsupportedReason, "multi-part") {
		t.Fatalf("reason = %q, want multi-part HLS requirement", selection.UnsupportedReason)
	}
}

func TestSelectNativeTransportsContainerAndCodecAliases(t *testing.T) {
	input := nativeTestInput()
	input.Probe = nativeTestProbe("matroska,webm", "hevc",
		ProbeStream{CodecName: "eac3", Channels: 6})
	input.Client = NativeClientCapabilities{
		Containers:      []string{"mkv"},
		VideoCodecs:     []string{"h265"},
		AudioCodecs:     []string{"ec-3"},
		SupportsHeaders: true,
		SupportsRange:   true,
	}
	input.SourceHeaders = map[string]string{"accept": "video/mp4"}
	selection := SelectNativeTransports(input)
	want := []NativeTransportKind{
		NativeTransportDirectHTTP,
		NativeTransportServerProxy,
		NativeTransportServerHLS,
	}
	if got := selection.Kinds(); !reflect.DeepEqual(got, want) {
		t.Fatalf("kinds = %v, want %v", got, want)
	}
}

func TestSelectNativeTransportsRangeIsReportedNotInvented(t *testing.T) {
	input := nativeTestInput()
	input.Client.SupportsRange = false
	selection := SelectNativeTransports(input)
	if len(selection.Candidates) == 0 {
		t.Fatal("no candidates")
	}
	for _, candidate := range selection.Candidates {
		if candidate.Range {
			t.Errorf("%s range = true, want reported false", candidate.Kind)
		}
	}
}

func TestNativeTransportSelectionJSONDoesNotLeakSourceURLOrRejectedHeaders(t *testing.T) {
	input := nativeTestInput()
	input.SourceURL = "https://cdn.example.com/video.mp4?signature=source-url-secret"
	encoded, err := json.Marshal(SelectNativeTransports(input))
	if err != nil {
		t.Fatalf("json.Marshal direct: %v", err)
	}
	if strings.Contains(string(encoded), "source-url-secret") {
		t.Fatalf("selection JSON leaked the source URL: %s", encoded)
	}

	input.SourceHeaders = map[string]string{
		"Cookie":        "session=header-secret",
		"Authorization": "Bearer header-secret",
	}
	encoded, err = json.Marshal(SelectNativeTransports(input))
	if err != nil {
		t.Fatalf("json.Marshal rejected headers: %v", err)
	}
	if strings.Contains(string(encoded), "header-secret") || strings.Contains(string(encoded), "Cookie") {
		t.Fatalf("selection JSON leaked a rejected header: %s", encoded)
	}
}
