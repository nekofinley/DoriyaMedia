package media

import (
	"strings"
	"testing"

	"doriyamedia/internal/source"
)

func probeFor(codec, profile, tag, audio, format string) *Probe {
	p := &Probe{}
	p.Format.FormatName = format
	p.Streams = append(p.Streams,
		ProbeStream{Index: 0, CodecType: "video", CodecName: codec, Profile: profile, CodecTagString: tag, Width: 1920, Height: 1080},
		ProbeStream{Index: 1, CodecType: "audio", CodecName: audio},
	)
	return p
}

func decisionProbe(codec, profile, tag string) *Probe {
	return probeFor(codec, profile, tag, "aac", "matroska,webm")
}

func decideWith(codec, profile, tag string, client ClientKind, opts DecideOptions) *Decision {
	return Decide(decisionProbe(codec, profile, tag), "/m/x.mkv", client, opts)
}

// altModes is the fallback chain as plain mode names, for readable assertions.
func altModes(d *Decision) []string {
	out := make([]string, 0, len(d.Alternates))
	for _, a := range d.Alternates {
		out = append(out, a.Mode)
	}
	return out
}

func equalModes(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// Forcing a remux must not copy a video stream the client cannot decode: the
// result plays audio with no picture at all, on every client. With video
// re-encoding withdrawn there is nothing to fall back to, so the decision has to
// say that instead of quietly starting an H.264 encode.
func TestForcedRemuxRefusesWhenVideoCannotBeCopied(t *testing.T) {
	cases := []struct {
		name         string
		codec        string
		profile      string
		tag          string
		client       ClientKind
		appleHEVCCpy bool
		wantMode     string
		wantInCause  string
	}{
		{"AV1 is decodable by nobody", "av1", "", "", ClientSafari, false, "unsupported", "AV1"},
		{"VP9 in HLS is not for Safari", "vp9", "", "", ClientSafari, false, "unsupported", "VP9"},
		{"10-bit H.264 has no hardware path", "h264", "High 10", "avc1", ClientSafari, false, "unsupported", "H264"},
		{"8-bit H.264 copies fine", "h264", "High", "avc1", ClientSafari, false, "remux", "手动指定转封装"},
		// Apple does not decode HEVC carried in MPEG-TS, so copying it there is
		// the "audio but no picture" bug — off by default.
		{"HEVC is not copied for Apple by default", "hevc", "Main 10", "hvc1", ClientSafari, false, "unsupported", "HEVC"},
		{"HEVC is copied for Apple when opted in", "hevc", "Main 10", "hvc1", ClientSafari, true, "remux", "手动指定转封装"},
		{"HEVC is not copied for MSE clients", "hevc", "Main", "hvc1", ClientChrome, true, "unsupported", "HEVC"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := decideWith(c.codec, c.profile, c.tag, c.client, DecideOptions{Force: "remux", AppleHEVCCopy: c.appleHEVCCpy})
			if d.Mode != c.wantMode {
				t.Fatalf("mode = %q, want %q (reason: %s)", d.Mode, c.wantMode, d.Reason)
			}
			if !strings.Contains(d.Reason, c.wantInCause) {
				t.Fatalf("reason %q does not mention %q", d.Reason, c.wantInCause)
			}
			// A playable decision carries a stream; a refusal must not.
			if c.wantMode == "unsupported" && d.Kind != "" {
				t.Fatalf("kind = %q, want empty for a file nothing can play", d.Kind)
			}
			if c.wantMode != "unsupported" && d.Kind != "hls" {
				t.Fatalf("kind = %q, want hls", d.Kind)
			}
		})
	}
}

// The refusal must explain itself, and explain it accurately: the viewer forced
// one thing and got nothing, so the reason has to say why.
func TestForcedRemuxRefusalExplainsWhy(t *testing.T) {
	d := decideWith("av1", "", "", ClientSafari, DecideOptions{Force: "remux"})
	if !strings.Contains(d.Reason, "没有画面") {
		t.Fatalf("reason should explain the symptom, got: %s", d.Reason)
	}
	if !strings.Contains(d.Reason, "转码已禁用") {
		t.Fatalf("reason should say why nothing else is tried, got: %s", d.Reason)
	}

	// Apple can decode HEVC — just not from MPEG-TS — so the message must blame
	// the container, not the codec's decodability.
	hevc := decideWith("hevc", "Main 10", "hvc1", ClientSafari, DecideOptions{Force: "remux"})
	if !strings.Contains(hevc.Reason, "MPEG-TS") {
		t.Fatalf("Apple HEVC refusal should blame the container, got: %s", hevc.Reason)
	}
}

// Transcoding is withdrawn as a feature: a request for it, however it arrives,
// must land on the automatic decision instead of starting an encoder.
func TestForcedTranscodeIsNotHonoured(t *testing.T) {
	for _, codec := range []string{"h264", "hevc", "av1"} {
		d := decideWith(codec, "High", "avc1", ClientSafari, DecideOptions{Force: "transcode"})
		if d.Mode == "transcode" {
			t.Fatalf("codec %s: transcode must not be selectable any more", codec)
		}
	}

	// 8-bit H.264 in Matroska is remuxed; AV1 has nothing left to play with.
	if d := decideWith("h264", "High", "avc1", ClientSafari, DecideOptions{Force: "transcode"}); d.Mode != "remux" {
		t.Fatalf("mode=%q, want remux for 8-bit H.264 (reason: %s)", d.Mode, d.Reason)
	}
	if d := decideWith("av1", "", "", ClientSafari, DecideOptions{Force: "transcode"}); d.Mode != "unsupported" {
		t.Fatalf("mode=%q, want unsupported for AV1 (reason: %s)", d.Mode, d.Reason)
	}
}

// The automatic path already refused to copy undecodable video; this pins that
// down so a future edit cannot reintroduce the same class of bug.
func TestAutoDecisionNeverCopiesUndecodableVideo(t *testing.T) {
	for _, codec := range []string{"av1", "vp9", "mpeg2video"} {
		d := decideWith(codec, "", "", ClientSafari, DecideOptions{})
		if d.Mode != "unsupported" {
			t.Fatalf("codec %s: mode=%q, want unsupported (reason: %s)", codec, d.Mode, d.Reason)
		}
		if !strings.Contains(d.Reason, "转码已禁用") {
			t.Fatalf("codec %s: the refusal should say re-encoding is off, got: %s", codec, d.Reason)
		}
	}
	// H.264 8-bit MKV on Safari is the one case that legitimately copies.
	d := decideWith("h264", "High", "avc1", ClientSafari, DecideOptions{})
	if d.Mode != "remux" {
		t.Fatalf("mode=%q, want remux for 8-bit H.264 (reason: %s)", d.Mode, d.Reason)
	}
}

// Apple + HEVC: the copy is opt-in, and without it the file simply cannot be
// played here — a re-encode of the video is no longer on offer.
func TestAppleHEVCCopyPolicy(t *testing.T) {
	auto := decideWith("hevc", "Main 10", "hvc1", ClientSafari, DecideOptions{})
	if auto.Mode != "unsupported" {
		t.Fatalf("Apple HEVC default mode = %q, want unsupported", auto.Mode)
	}
	if !strings.Contains(auto.Reason, "Apple 不解码 MPEG-TS") {
		t.Fatalf("default reason should explain the container limit, got: %s", auto.Reason)
	}

	optedIn := decideWith("hevc", "Main 10", "hvc1", ClientSafari, DecideOptions{AppleHEVCCopy: true})
	if optedIn.Mode != "remux" {
		t.Fatalf("opted-in Apple HEVC mode = %q, want remux", optedIn.Mode)
	}

	// The flag must never enable HEVC copying for clients that cannot demux it.
	// Checked through a forced remux, because the automatic path legitimately
	// offers Chrome a direct play of Matroska instead.
	chrome := decideWith("hevc", "Main 10", "hvc1", ClientChrome, DecideOptions{Force: "remux", AppleHEVCCopy: true})
	if chrome.Mode != "unsupported" {
		t.Fatalf("Chrome forced-remux mode = %q, want unsupported even with AppleHEVCCopy", chrome.Mode)
	}
}

// A client that reports it can decode HEVC may copy instead of transcoding —
// that is the whole point of asking it. Without the report nothing changes.
func TestClientHEVCReportAllowsCopy(t *testing.T) {
	// Reported capability: copying is allowed for an MSE client.
	reported := decideWith("hevc", "Main 10", "hvc1", ClientChrome,
		DecideOptions{Force: "remux", ClientHEVC: true})
	if reported.Mode != "remux" {
		t.Fatalf("mode=%q, want remux when the client reports HEVC support", reported.Mode)
	}
	if reported.Reason != "手动指定转封装" {
		t.Errorf("reason=%q, want the plain forced-remux reason", reported.Reason)
	}

	// No report: refuse, rather than starting an encoder on a guess.
	silent := decideWith("hevc", "Main 10", "hvc1", ClientChrome, DecideOptions{Force: "remux"})
	if silent.Mode != "unsupported" {
		t.Fatalf("mode=%q, want unsupported without a capability report", silent.Mode)
	}

	// The report must not make undecodable-for-everyone codecs copyable.
	av1 := decideWith("av1", "", "", ClientChrome, DecideOptions{Force: "remux", ClientHEVC: true})
	if av1.Mode != "unsupported" {
		t.Fatalf("AV1 mode=%q: a HEVC report must not affect other codecs", av1.Mode)
	}

	// Apple's behaviour is unchanged by the report: it is governed by its own
	// switch, because the constraint there is the container, not the decoder.
	apple := decideWith("hevc", "Main 10", "hvc1", ClientSafari, DecideOptions{ClientHEVC: true})
	if apple.Mode != "unsupported" {
		t.Fatalf("Apple mode=%q, want unsupported: the report is not Apple's switch", apple.Mode)
	}

	// The automatic path for Chrome offers a direct play of Matroska (the client
	// falls back if the container turns out to be unplayable), so the capability
	// shows up in that branch rather than in the copy branch — see the negative
	// report below, which is what actually rules it out.
	auto := decideWith("hevc", "Main 10", "hvc1", ClientChrome, DecideOptions{ClientHEVC: true})
	if auto.Mode != "proxy" && auto.Mode != "direct" {
		t.Fatalf("automatic mode=%q, want a direct/proxy play for Chrome Matroska", auto.Mode)
	}
}

// A client that says it cannot play HEVC must not be handed HEVC. The codec table
// assumes browsers cope, which is not true on every machine.
func TestClientNoHEVCReportRulesOutDirectAndCopy(t *testing.T) {
	// Without a report, Chrome+Matroska is offered a direct play (the status quo).
	optimistic := decideWith("hevc", "Main 10", "hvc1", ClientChrome, DecideOptions{})
	if optimistic.Mode != "proxy" && optimistic.Mode != "direct" {
		t.Fatalf("baseline mode=%q, want a direct play", optimistic.Mode)
	}

	// With a negative report it cannot be, and the reason says why.
	reported := decideWith("hevc", "Main 10", "hvc1", ClientChrome, DecideOptions{ClientNoHEVC: true})
	if reported.Mode != "unsupported" {
		t.Fatalf("mode=%q, want unsupported when the client reports no HEVC", reported.Mode)
	}
	if !strings.Contains(reported.Reason, "客户端自报不支持 HEVC") {
		t.Errorf("reason should name the self-report, got: %s", reported.Reason)
	}

	// It must not affect codecs the client can play.
	h264 := decideWith("h264", "High", "avc1", ClientChrome, DecideOptions{ClientNoHEVC: true})
	if h264.Mode != "proxy" && h264.Mode != "direct" {
		t.Fatalf("H.264 mode=%q: a HEVC report must not affect other codecs", h264.Mode)
	}

	// Nor somebody else's switch: Apple's copy policy is its own.
	apple := decideWith("hevc", "Main 10", "hvc1", ClientSafari,
		DecideOptions{AppleHEVCCopy: true, ClientNoHEVC: true})
	if apple.Mode != "remux" {
		t.Fatalf("Apple mode=%q: its own opt-in still governs", apple.Mode)
	}
}

// ---------- the fallback chain ----------

// A fallback must be able to work, or it is not a fallback. The chain used to
// offer "direct" for every file the source could 302 — including a Matroska file
// on iOS, which that device can never open — and the client reached it exactly
// when a slow first segment looked like a failure. That is the bug this pins.
func TestFallbackChainNeverOffersAModeTheClientCannotUse(t *testing.T) {
	opts := DecideOptions{AppleHEVCCopy: true, HasDirect: true}
	d := decideWith("hevc", "Main 10", "hvc1", ClientSafari, opts)
	if d.Mode != "remux" {
		t.Fatalf("mode=%q, want remux (reason: %s)", d.Mode, d.Reason)
	}
	for _, a := range d.Alternates {
		if a.Mode == "direct" || a.Mode == "proxy" {
			t.Fatalf("chain offers %q for a file this client cannot play: %v", a.Mode, altModes(d))
		}
	}
	// The copy is the only thing that could play this file, and a re-encode is not
	// a fallback — so there is nothing left to try, which is the honest answer
	// (the player says so instead of switching into a dead end).
	if len(d.Alternates) != 0 {
		t.Fatalf("chain = %v, want nothing", altModes(d))
	}
}

// The order is cost: the same bytes elsewhere, then a copy. A re-encode is not in
// the chain at all any more.
func TestFallbackChainRunsCheapToExpensive(t *testing.T) {
	d := Decide(probeFor("h264", "High", "avc1", "aac", "mov,mp4,m4a,3gp,3g2,mj2"),
		"/m/x.mp4", ClientChrome, DecideOptions{HasDirect: true})
	if d.Mode != "direct" {
		t.Fatalf("mode=%q, want direct (reason: %s)", d.Mode, d.Reason)
	}
	if !equalModes(altModes(d), "proxy", "remux") {
		t.Fatalf("chain = %v, want [proxy remux]", altModes(d))
	}

	// Without a 302-able source the direct play *is* the proxy, so it is the
	// primary and not repeated as a fallback.
	noDirect := Decide(probeFor("h264", "High", "avc1", "aac", "mov,mp4,m4a,3gp,3g2,mj2"),
		"/m/x.mp4", ClientChrome, DecideOptions{})
	if noDirect.Mode != "proxy" {
		t.Fatalf("mode=%q, want proxy", noDirect.Mode)
	}
	if !equalModes(altModes(noDirect), "remux") {
		t.Fatalf("chain = %v, want [remux]", altModes(noDirect))
	}
}

// Whether a browser decodes AC-3/E-AC-3/DTS is a property of the machine (the
// decoders come from the OS), so the client's own answer is the only honest
// source — without it the tables remux files the browser could have played.
func TestClientAudioReportWidensDirectPlay(t *testing.T) {
	eac3 := func(reported map[string]bool) *Decision {
		return Decide(probeFor("h264", "High", "avc1", "eac3", "matroska,webm"),
			"/m/x.mkv", ClientChrome, DecideOptions{HasDirect: true, ClientAudio: reported})
	}

	silent := eac3(nil)
	if silent.Mode != "remux" {
		t.Fatalf("mode=%q, want remux: the table cannot vouch for E-AC-3", silent.Mode)
	}
	reported := eac3(map[string]bool{"eac3": true})
	if reported.Mode != "direct" {
		t.Fatalf("mode=%q, want direct when the client reports E-AC-3 (reason: %s)", reported.Mode, reported.Reason)
	}
	// A report stands for the codec it names and no other.
	if other := eac3(map[string]bool{"ac3": true}); other.Mode != "remux" {
		t.Fatalf("mode=%q: an AC-3 report must not speak for E-AC-3", other.Mode)
	}
	// And it says nothing about video: a codec no browser decodes stays undecodable.
	old := Decide(probeFor("mpeg2video", "", "", "eac3", "matroska,webm"), "/m/x.mkv", ClientChrome,
		DecideOptions{HasDirect: true, ClientAudio: map[string]bool{"eac3": true}})
	if old.Mode != "unsupported" {
		t.Fatalf("mode=%q, want unsupported for MPEG-2 whatever the audio report says", old.Mode)
	}
}

// A source that publishes the picture and the sound as two streams cannot be
// handed to a client at all: whichever single URL it was pointed at carries the
// video only, so "direct" and "proxy" would both play a silent picture. The one
// honest offer is a server-side mux, and the fallback chain must not smuggle the
// single-URL modes back in.
func TestMultiPartSourceOnlyOffersRemux(t *testing.T) {
	probe := partsProbe([]source.Part{
		{URL: "https://cdn.test/video.m4s", Kind: "video", Codec: "h264", Tag: "avc1", Profile: "High", Width: 1920, Height: 1080, Duration: 1426},
		{URL: "https://cdn.test/audio.m4s", Kind: "audio", Codec: "aac", Tag: "mp4a", Duration: 1426},
	})
	if probe.Duration() != 1426 {
		t.Fatalf("duration from the source's own metadata = %v, want 1426", probe.Duration())
	}
	d := Decide(probe, "/热门番剧/x/第1话 [ep1].mp4", ClientSafari, DecideOptions{
		MultiPart: true,
		// The source does have a direct URL (the video part); offering it would be
		// the bug this test exists for.
		HasDirect: true,
	})
	if d.Mode != "remux" || d.Kind != "hls" {
		t.Fatalf("multi-part decision = %s/%s, want remux/hls (%s)", d.Mode, d.Kind, d.Reason)
	}
	for _, m := range altModes(d) {
		if m == "direct" || m == "proxy" {
			t.Fatalf("fallback chain offers %q, which can only play a silent picture: %v", m, altModes(d))
		}
	}
}

// The copy still has to be decodable by the client: a codec this client has no
// path for is refused rather than remuxed into a black picture.
func TestMultiPartRefusesUndecodableCodec(t *testing.T) {
	probe := partsProbe([]source.Part{
		{URL: "https://cdn.test/video.m4s", Kind: "video", Codec: "av1", Tag: "av01", Width: 3840, Height: 2160},
		{URL: "https://cdn.test/audio.m4s", Kind: "audio", Codec: "aac"},
	})
	d := Decide(probe, "/x/第1话 [ep1].mp4", ClientSafari, DecideOptions{MultiPart: true})
	if d.Mode != "unsupported" {
		t.Fatalf("AV1 into Safari decided %s, want unsupported (%s)", d.Mode, d.Reason)
	}
	if !strings.Contains(d.Reason, "DASH") {
		t.Fatalf("reason should name the cause: %q", d.Reason)
	}
}
