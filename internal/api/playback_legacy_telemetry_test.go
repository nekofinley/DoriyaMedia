package api

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"doriyamedia/internal/media"
	"doriyamedia/internal/models"
)

func TestLegacyHLSPlaylistTelemetrySuccess(t *testing.T) {
	logs := capturePlaybackSecurityLogs(t)
	v1PlaybackTelemetryRecorder.reset()
	t.Cleanup(v1PlaybackTelemetryRecorder.reset)

	server, fileID := legacyHLSTelemetryFixture(t)
	rec := httptest.NewRecorder()
	server.handleHLS(rec, legacyHLSTelemetryRequest(fileID, "index.m3u8"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	snapshot := v1PlaybackTelemetryRecorder.snapshot()
	if len(snapshot) != 1 {
		t.Fatalf("telemetry series = %d, want 1", len(snapshot))
	}
	got := snapshot[0]
	if got.Key.Surface != "legacy" ||
		got.Key.Transport != models.PlaybackTransportServerHLS ||
		got.Key.Asset != playbackAssetPlaylist ||
		got.Key.Outcome != "success" ||
		got.Key.FailureStage != playbackFailureStageComplete ||
		got.Value.ResponseBytes != int64(rec.Body.Len()) ||
		got.Value.ServerMediaBytes != 0 {
		t.Fatalf("telemetry = %+v, want successful legacy HLS playlist", got)
	}

	output := mediaResponseLogs(logs.String())
	for _, want := range []string{
		"surface=legacy",
		"transport=server-hls",
		"asset=playlist",
		"outcome=success",
		"failure_stage=complete",
		"response_bytes=",
		"server_media_bytes=0",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("legacy HLS telemetry omits %q: %q", want, output)
		}
	}
	for _, secret := range []string{
		fmt.Sprintf("/api/hls/%d", fileID),
		"source.example",
		"movie.mkv",
		"query-canary",
		"Authorization",
	} {
		if strings.Contains(output, secret) {
			t.Errorf("legacy HLS telemetry leaked %q: %q", secret, output)
		}
	}
}

func TestLegacyHLSSegmentFailureTelemetryIsSourceFailure(t *testing.T) {
	const (
		queryCanary  = "legacy-hls-502-query-canary"
		headerCanary = "legacy-hls-502-header-canary"
	)
	logs := capturePlaybackSecurityLogs(t)
	v1PlaybackTelemetryRecorder.reset()
	t.Cleanup(v1PlaybackTelemetryRecorder.reset)

	server, fileID := legacyHLSTelemetryFixture(t)
	rec := httptest.NewRecorder()
	req := legacyHLSTelemetryRequest(fileID, "0.ts")
	req.URL.RawQuery = queryCanary
	req.Header.Set("Authorization", "Bearer "+headerCanary)
	server.handleHLS(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%q", rec.Code, rec.Body.String())
	}
	snapshot := v1PlaybackTelemetryRecorder.snapshot()
	if len(snapshot) != 1 {
		t.Fatalf("telemetry series = %d, want 1", len(snapshot))
	}
	got := snapshot[0]
	if got.Key.Surface != "legacy" ||
		got.Key.Transport != models.PlaybackTransportServerHLS ||
		got.Key.Asset != playbackAssetSegment ||
		got.Key.Outcome != "error" ||
		got.Key.FailureStage != playbackFailureStageSource ||
		got.Value.ResponseBytes != int64(rec.Body.Len()) ||
		got.Value.ServerMediaBytes != 0 {
		t.Fatalf("telemetry = %+v, want legacy HLS segment source failure", got)
	}

	output := logs.String()
	for _, want := range []string{
		"surface=legacy",
		"transport=server-hls",
		"asset=segment",
		"outcome=error",
		"failure_stage=source",
		"status=502",
		"server_media_bytes=0",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("legacy HLS 502 telemetry omits %q: %q", want, output)
		}
	}
	for _, secret := range []string{
		queryCanary,
		headerCanary,
		"movie-token-secret",
		"source.example",
		"movie.mkv",
	} {
		if strings.Contains(output, secret) {
			t.Errorf("legacy HLS 502 logs leaked %q: %q", secret, output)
		}
	}
}

func TestLegacyHLSProbeFailureTelemetryIsSourceFailure(t *testing.T) {
	logs := capturePlaybackSecurityLogs(t)
	v1PlaybackTelemetryRecorder.reset()
	t.Cleanup(v1PlaybackTelemetryRecorder.reset)

	server, fileID := legacyHLSTelemetryFixtureWithProbe(t, "")
	rec := httptest.NewRecorder()
	server.handleHLS(rec, legacyHLSTelemetryRequest(fileID, "index.m3u8"))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%q", rec.Code, rec.Body.String())
	}
	snapshot := v1PlaybackTelemetryRecorder.snapshot()
	if len(snapshot) != 1 {
		t.Fatalf("telemetry series = %d, want 1", len(snapshot))
	}
	got := snapshot[0]
	if got.Key.Asset != playbackAssetPlaylist ||
		got.Key.Outcome != "error" ||
		got.Key.FailureStage != playbackFailureStageSource ||
		got.Value.ServerMediaBytes != 0 {
		t.Fatalf("telemetry = %+v, want legacy HLS probe source failure", got)
	}

	output := mediaResponseLogs(logs.String())
	for _, want := range []string{
		"asset=playlist",
		"failure_stage=source",
		"status=500",
		"server_media_bytes=0",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("legacy HLS probe telemetry omits %q: %q", want, output)
		}
	}
}

func TestLegacyHLSUnknownResourceTelemetryIsRequestFailure(t *testing.T) {
	v1PlaybackTelemetryRecorder.reset()
	t.Cleanup(v1PlaybackTelemetryRecorder.reset)

	server, fileID := legacyHLSTelemetryFixture(t)
	rec := httptest.NewRecorder()
	server.handleHLS(rec, legacyHLSTelemetryRequest(fileID, "not-a-playlist"))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%q", rec.Code, rec.Body.String())
	}
	snapshot := v1PlaybackTelemetryRecorder.snapshot()
	if len(snapshot) != 1 {
		t.Fatalf("telemetry series = %d, want 1", len(snapshot))
	}
	got := snapshot[0]
	if got.Key.FailureStage != playbackFailureStageRequest ||
		got.Key.Outcome != "error" ||
		got.Value.ServerMediaBytes != 0 {
		t.Fatalf("telemetry = %+v, want legacy HLS request failure", got)
	}
}

func TestLegacyHLSPlaylistAliasesUsePlaylistAsset(t *testing.T) {
	for _, rest := range []string{
		"index",
		"playlist",
		"index.ts",
		"playlist.foo",
	} {
		t.Run(rest, func(t *testing.T) {
			v1PlaybackTelemetryRecorder.reset()
			t.Cleanup(v1PlaybackTelemetryRecorder.reset)

			server, fileID := legacyHLSTelemetryFixture(t)
			rec := httptest.NewRecorder()
			server.handleHLS(rec, legacyHLSTelemetryRequest(fileID, rest))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
			}
			snapshot := v1PlaybackTelemetryRecorder.snapshot()
			if len(snapshot) != 1 {
				t.Fatalf("telemetry series = %d, want 1", len(snapshot))
			}
			got := snapshot[0]
			if got.Key.Asset != playbackAssetPlaylist ||
				got.Value.ResponseBytes != int64(rec.Body.Len()) ||
				got.Value.ServerMediaBytes != 0 {
				t.Fatalf("telemetry = %+v, want playlist alias without server media bytes", got)
			}
		})
	}
}

func TestLegacyHLSPlaylistShortWriteTelemetryIsBodyFailure(t *testing.T) {
	const (
		queryCanary  = "legacy-hls-short-query-canary"
		headerCanary = "legacy-hls-short-header-canary"
	)
	logs := capturePlaybackSecurityLogs(t)
	v1PlaybackTelemetryRecorder.reset()
	t.Cleanup(v1PlaybackTelemetryRecorder.reset)

	server, fileID := legacyHLSTelemetryFixture(t)
	writer := &playbackTelemetryFailingWriter{writeN: 3}
	req := legacyHLSTelemetryRequest(fileID, "index.m3u8")
	req.URL.RawQuery = queryCanary
	req.Header.Set("Authorization", "Bearer "+headerCanary)
	server.handleHLS(writer, req)

	snapshot := v1PlaybackTelemetryRecorder.snapshot()
	if len(snapshot) != 1 {
		t.Fatalf("telemetry series = %d, want 1", len(snapshot))
	}
	got := snapshot[0]
	if got.Key.Surface != "legacy" ||
		got.Key.Asset != playbackAssetPlaylist ||
		got.Key.Outcome != "aborted" ||
		got.Key.FailureStage != playbackFailureStageBody ||
		got.Value.ResponseBytes != 3 ||
		got.Value.ServerMediaBytes != 0 {
		t.Fatalf("telemetry = %+v, want legacy HLS playlist body failure", got)
	}

	output := logs.String()
	for _, want := range []string{
		"asset=playlist",
		"outcome=aborted",
		"failure_stage=body",
		"response_bytes=3",
		"server_media_bytes=0",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("legacy HLS short-write telemetry omits %q: %q", want, output)
		}
	}
	for _, secret := range []string{
		queryCanary,
		headerCanary,
		"movie-token-secret",
		"source.example",
		"movie.mkv",
	} {
		if strings.Contains(output, secret) {
			t.Errorf("legacy HLS short-write logs leaked %q: %q", secret, output)
		}
	}
}

func TestLegacyHLSAssetClassification(t *testing.T) {
	tests := []struct {
		rest string
		want string
	}{
		{rest: "", want: playbackAssetPlaylist},
		{rest: "index.m3u8", want: playbackAssetPlaylist},
		{rest: "index", want: playbackAssetPlaylist},
		{rest: "index-extra", want: playbackAssetPlaylist},
		{rest: "index.ts", want: playbackAssetPlaylist},
		{rest: "playlist", want: playbackAssetPlaylist},
		{rest: "playlist-extra", want: playbackAssetPlaylist},
		{rest: "playlist.foo", want: playbackAssetPlaylist},
		{rest: "master.m3u8", want: playbackAssetSubtitle},
		{rest: "byterange.m3u8", want: playbackAssetPlaylist},
		{rest: "subs/4.m3u8", want: playbackAssetSubtitle},
		{rest: "subs/0.ts", want: playbackAssetSubtitle},
		{rest: "0.ts", want: playbackAssetSegment},
		{rest: "seg00001.m4s", want: playbackAssetSegment},
		{rest: "init.mp4", want: playbackAssetSegment},
		{rest: "other.m3u8", want: playbackAssetStream},
		{rest: "video.mp4", want: playbackAssetStream},
		{rest: "unknown", want: playbackAssetStream},
	}
	for _, tc := range tests {
		t.Run(tc.rest, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/hls/1/"+tc.rest, nil)
			req.SetPathValue("rest", tc.rest)
			if got := legacyHLSAsset(req); got != tc.want {
				t.Fatalf("asset = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLegacySubtitleVTTTelemetryGetAndHead(t *testing.T) {
	logs := capturePlaybackSecurityLogs(t)
	v1PlaybackTelemetryRecorder.reset()
	t.Cleanup(v1PlaybackTelemetryRecorder.reset)

	server, httpServer, fileID, subID := legacySubtitleTelemetryFixture(t)
	target := fmt.Sprintf(
		"%s/api/subs/%d/%d.vtt%s&telemetry-query-canary=subtitle-get",
		httpServer.URL,
		fileID,
		subID,
		signedQuery(server.Cfg.Secret, fileID, sigTTL),
	)

	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatalf("new subtitle GET request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer subtitle-get-header-canary")
	resp, err := httpServer.Client().Do(req)
	if err != nil {
		t.Fatalf("subtitle GET: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read subtitle GET body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("subtitle GET status = %d, want 200; body=%q", resp.StatusCode, body)
	}
	waitForPlaybackTelemetryCount(t, 1)

	snapshot := v1PlaybackTelemetryRecorder.snapshot()
	if len(snapshot) != 1 {
		t.Fatalf("telemetry series = %d, want 1", len(snapshot))
	}
	got := snapshot[0]
	if got.Key.Surface != "legacy" ||
		got.Key.Transport != models.PlaybackTransportServerHLS ||
		got.Key.Asset != playbackAssetSubtitle ||
		got.Key.Outcome != "success" ||
		got.Key.FailureStage != playbackFailureStageComplete ||
		got.Value.ResponseBytes != int64(len(body)) ||
		got.Value.ServerMediaBytes != 0 {
		t.Fatalf("GET telemetry = %+v, want successful legacy subtitle response", got)
	}

	v1PlaybackTelemetryRecorder.reset()
	req, err = http.NewRequest(http.MethodHead, target, nil)
	if err != nil {
		t.Fatalf("new subtitle HEAD request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer subtitle-head-header-canary")
	resp, err = httpServer.Client().Do(req)
	if err != nil {
		t.Fatalf("subtitle HEAD: %v", err)
	}
	headBody, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read subtitle HEAD body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("subtitle HEAD status = %d, want 200", resp.StatusCode)
	}
	if len(headBody) != 0 {
		t.Fatalf("subtitle HEAD body length = %d, want 0", len(headBody))
	}
	waitForPlaybackTelemetryCount(t, 1)

	snapshot = v1PlaybackTelemetryRecorder.snapshot()
	if len(snapshot) != 1 {
		t.Fatalf("telemetry series = %d, want 1 after HEAD", len(snapshot))
	}
	got = snapshot[0]
	if got.Key.Surface != "legacy" ||
		got.Key.Asset != playbackAssetSubtitle ||
		got.Key.Outcome != "success" ||
		got.Key.FailureStage != playbackFailureStageComplete ||
		got.Value.ResponseBytes != 0 ||
		got.Value.ServerMediaBytes != 0 {
		t.Fatalf("HEAD telemetry = %+v, want successful subtitle HEAD with zero bytes", got)
	}

	output := logs.String()
	for _, secret := range []string{
		"telemetry-query-canary",
		"subtitle-get-header-canary",
		"subtitle-head-header-canary",
		"subtitle-path-canary",
	} {
		if strings.Contains(output, secret) {
			t.Errorf("subtitle GET/HEAD logs leaked %q: %q", secret, output)
		}
	}
}

func TestLegacySubtitleVTTShortWriteTelemetryIsBodyFailure(t *testing.T) {
	const (
		queryCanary  = "subtitle-short-query-canary"
		headerCanary = "subtitle-short-header-canary"
	)
	logs := capturePlaybackSecurityLogs(t)
	v1PlaybackTelemetryRecorder.reset()
	t.Cleanup(v1PlaybackTelemetryRecorder.reset)

	server, _, fileID, subID := legacySubtitleTelemetryFixture(t)
	req := legacySubtitleTelemetryRequest(fileID, subID, http.MethodGet)
	req.URL.RawQuery = queryCanary
	req.Header.Set("Authorization", "Bearer "+headerCanary)
	writer := &playbackTelemetryFailingWriter{writeN: 3}
	server.handleSubtitle(writer, req)

	snapshot := v1PlaybackTelemetryRecorder.snapshot()
	if len(snapshot) != 1 {
		t.Fatalf("telemetry series = %d, want 1", len(snapshot))
	}
	got := snapshot[0]
	if got.Key.Surface != "legacy" ||
		got.Key.Asset != playbackAssetSubtitle ||
		got.Key.Outcome != "aborted" ||
		got.Key.FailureStage != playbackFailureStageBody ||
		got.Value.ResponseBytes != 3 ||
		got.Value.ServerMediaBytes != 0 {
		t.Fatalf("telemetry = %+v, want legacy subtitle body failure", got)
	}

	output := logs.String()
	for _, want := range []string{
		"asset=subtitle",
		"outcome=aborted",
		"failure_stage=body",
		"response_bytes=3",
		"server_media_bytes=0",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("subtitle short-write telemetry omits %q: %q", want, output)
		}
	}
	for _, secret := range []string{
		queryCanary,
		headerCanary,
		"subtitle-path-canary",
	} {
		if strings.Contains(output, secret) {
			t.Errorf("subtitle short-write logs leaked %q: %q", secret, output)
		}
	}
}

func TestLegacySubtitleVTT502TelemetryIsSourceFailure(t *testing.T) {
	const (
		queryCanary  = "subtitle-502-query-canary"
		headerCanary = "subtitle-502-header-canary"
		pathCanary   = "subtitle-502-path-canary"
	)
	logs := capturePlaybackSecurityLogs(t)
	v1PlaybackTelemetryRecorder.reset()
	t.Cleanup(v1PlaybackTelemetryRecorder.reset)

	server, _, fileID, _ := legacySubtitleTelemetryFixture(t)
	if err := server.Store.UpsertSubtitle(&models.Subtitle{
		FileID:      fileID,
		Path:        "/media/" + pathCanary + ".srt",
		Lang:        "en",
		Title:       "missing",
		Format:      "srt",
		Convertible: true,
	}); err != nil {
		t.Fatalf("insert missing subtitle row: %v", err)
	}
	subs, err := server.Store.SubtitlesByFile(fileID)
	if err != nil {
		t.Fatalf("list subtitle rows: %v", err)
	}
	var missingSubID int64
	for _, sub := range subs {
		if sub.Path == "/media/"+pathCanary+".srt" {
			missingSubID = sub.ID
			break
		}
	}
	if missingSubID == 0 {
		t.Fatal("missing subtitle row was not created")
	}

	req := legacySubtitleTelemetryRequest(fileID, missingSubID, http.MethodGet)
	req.URL.RawQuery = queryCanary
	req.Header.Set("Authorization", "Bearer "+headerCanary)
	rec := httptest.NewRecorder()
	server.handleSubtitle(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%q", rec.Code, rec.Body.String())
	}
	snapshot := v1PlaybackTelemetryRecorder.snapshot()
	if len(snapshot) != 1 {
		t.Fatalf("telemetry series = %d, want 1", len(snapshot))
	}
	got := snapshot[0]
	if got.Key.Surface != "legacy" ||
		got.Key.Asset != playbackAssetSubtitle ||
		got.Key.Outcome != "error" ||
		got.Key.FailureStage != playbackFailureStageSource ||
		got.Value.ResponseBytes != int64(rec.Body.Len()) ||
		got.Value.ServerMediaBytes != 0 {
		t.Fatalf("telemetry = %+v, want legacy subtitle source failure", got)
	}

	output := logs.String()
	for _, want := range []string{
		"asset=subtitle",
		"outcome=error",
		"failure_stage=source",
		"status=502",
		"server_media_bytes=0",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("subtitle 502 telemetry omits %q: %q", want, output)
		}
	}
	for _, secret := range []string{queryCanary, headerCanary, pathCanary} {
		if strings.Contains(output, secret) {
			t.Errorf("subtitle 502 logs leaked %q: %q", secret, output)
		}
	}
}

func TestLegacyStreamDirectRedirectTelemetryServesNoMediaBytes(t *testing.T) {
	const (
		directCanary = "legacy-direct-url-canary"
		queryCanary  = "legacy-direct-query-canary"
		headerCanary = "legacy-direct-header-canary"
	)
	logs := capturePlaybackSecurityLogs(t)
	v1PlaybackTelemetryRecorder.reset()
	t.Cleanup(v1PlaybackTelemetryRecorder.reset)

	server, store := newLegacyHandlerSecurityServer(t)
	file := seedLegacyHandlerFile(t, store, "url", "{}",
		"https://media.example.test/private/movie.mp4?token="+directCanary)
	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/api/stream/%d?%s", file.ID, queryCanary), nil)
	req.SetPathValue("id", strconv.FormatInt(file.ID, 10))
	req.Header.Set("Authorization", "Bearer "+headerCanary)
	rec := httptest.NewRecorder()
	server.handleStream(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body=%q", rec.Code, rec.Body.String())
	}
	if location := rec.Header().Get("Location"); !strings.Contains(location, directCanary) {
		t.Fatalf("Location = %q, want direct source URL", location)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("redirect body length = %d, want 0", rec.Body.Len())
	}
	snapshot := v1PlaybackTelemetryRecorder.snapshot()
	if len(snapshot) != 1 {
		t.Fatalf("telemetry series = %d, want 1", len(snapshot))
	}
	got := snapshot[0]
	if got.Key.Surface != "legacy" ||
		got.Key.Transport != "legacy-stream" ||
		got.Key.Asset != playbackAssetStream ||
		got.Key.Outcome != "success" ||
		got.Key.FailureStage != playbackFailureStageComplete ||
		got.Value.ResponseBytes != 0 ||
		got.Value.ServerMediaBytes != 0 {
		t.Fatalf("telemetry = %+v, want a byte-free direct redirect", got)
	}

	output := logs.String()
	for _, secret := range []string{directCanary, queryCanary, headerCanary} {
		if strings.Contains(output, secret) {
			t.Errorf("legacy stream redirect logs leaked %q: %q", secret, output)
		}
	}
}

func TestLegacyStreamBadIDTelemetryIsRequestFailure(t *testing.T) {
	v1PlaybackTelemetryRecorder.reset()
	t.Cleanup(v1PlaybackTelemetryRecorder.reset)

	server, _ := newLegacyHandlerSecurityServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/stream/not-an-id", nil)
	req.SetPathValue("id", "not-an-id")
	rec := httptest.NewRecorder()
	server.handleStream(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	assertLegacyStreamTelemetry(t, "error", playbackFailureStageRequest)
}

func TestLegacyStreamFileNotFoundTelemetryIsSessionFailure(t *testing.T) {
	v1PlaybackTelemetryRecorder.reset()
	t.Cleanup(v1PlaybackTelemetryRecorder.reset)

	server, _ := newLegacyHandlerSecurityServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/stream/999999", nil)
	req.SetPathValue("id", "999999")
	rec := httptest.NewRecorder()
	server.handleStream(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	assertLegacyStreamTelemetry(t, "error", playbackFailureStageSession)
}

func TestLegacyStreamDriverFailureTelemetryIsInternal(t *testing.T) {
	const (
		pathCanary   = "legacy-stream-driver-path-canary"
		queryCanary  = "legacy-stream-driver-query-canary"
		headerCanary = "legacy-stream-driver-header-canary"
	)
	logs := capturePlaybackSecurityLogs(t)
	v1PlaybackTelemetryRecorder.reset()
	t.Cleanup(v1PlaybackTelemetryRecorder.reset)

	server, store := newLegacyHandlerSecurityServer(t)
	file := seedLegacyHandlerFile(t, store, "unsupported",
		"{}", "/private/"+pathCanary+".mkv")
	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/api/stream/%d?%s", file.ID, queryCanary), nil)
	req.SetPathValue("id", strconv.FormatInt(file.ID, 10))
	req.Header.Set("Authorization", "Bearer "+headerCanary)
	rec := httptest.NewRecorder()
	server.handleStream(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%q", rec.Code, rec.Body.String())
	}
	assertLegacyStreamTelemetry(t, "error", playbackFailureStageInternal)

	output := logs.String()
	for _, secret := range []string{pathCanary, queryCanary, headerCanary} {
		if strings.Contains(output, secret) {
			t.Errorf("legacy stream driver failure logs leaked %q: %q", secret, output)
		}
	}
}

func legacySubtitleTelemetryFixture(t *testing.T) (*Server, *httptest.Server, int64, int64) {
	t.Helper()
	server, httpServer := newTestServer(t)
	server.Engine = media.NewEngine(server.Store, server.Cfg)
	fileID, _ := seedPlaybackSubtitleLocalFile(t, server.Store, playbackAPITestProbe,
		map[string]string{
			"subtitle-path-canary.vtt": "WEBVTT\n\n00:00:01.000 --> 00:00:02.000\nsubtitle test\n",
		})
	if err := server.Store.UpsertSubtitle(&models.Subtitle{
		FileID:      fileID,
		Path:        "/media/subtitle-path-canary.vtt",
		Lang:        "en",
		Title:       "English",
		Format:      "webvtt",
		Convertible: true,
	}); err != nil {
		t.Fatalf("insert subtitle row: %v", err)
	}
	subs, err := server.Store.SubtitlesByFile(fileID)
	if err != nil {
		t.Fatalf("list subtitle rows: %v", err)
	}
	if len(subs) != 1 {
		t.Fatalf("subtitle rows = %d, want 1", len(subs))
	}
	return server, httpServer, fileID, subs[0].ID
}

func legacySubtitleTelemetryRequest(fileID, subID int64, method string) *http.Request {
	req := httptest.NewRequest(
		method,
		fmt.Sprintf("/api/subs/%d/%d.vtt", fileID, subID),
		nil,
	)
	req.SetPathValue("id", strconv.FormatInt(fileID, 10))
	req.SetPathValue("sub", strconv.FormatInt(subID, 10)+".vtt")
	return req
}

func assertLegacyStreamTelemetry(t *testing.T, outcome, stage string) {
	t.Helper()
	snapshot := v1PlaybackTelemetryRecorder.snapshot()
	if len(snapshot) != 1 {
		t.Fatalf("telemetry series = %d, want 1", len(snapshot))
	}
	got := snapshot[0]
	if got.Key.Surface != "legacy" ||
		got.Key.Transport != "legacy-stream" ||
		got.Key.Asset != playbackAssetStream ||
		got.Key.Outcome != outcome ||
		got.Key.FailureStage != stage ||
		got.Value.ServerMediaBytes != 0 {
		t.Fatalf("telemetry = %+v, want legacy-stream %s/%s", got, outcome, stage)
	}
}

func waitForPlaybackTelemetryCount(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if len(v1PlaybackTelemetryRecorder.snapshot()) >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d playback telemetry series", want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func legacyHLSTelemetryFixture(t *testing.T) (*Server, int64) {
	t.Helper()
	return legacyHLSTelemetryFixtureWithProbe(t, playbackAPITestProbe)
}

func legacyHLSTelemetryFixtureWithProbe(t *testing.T, probe string) (*Server, int64) {
	t.Helper()
	server, _ := newTestServer(t)
	tools := t.TempDir()
	server.Cfg.FFmpeg = filepath.Join(tools, "missing-ffmpeg")
	server.Cfg.FFProbe = filepath.Join(tools, "missing-ffprobe")
	server.Cfg.Playback.HLSSessionPackager = false
	server.Engine = media.NewEngine(server.Store, server.Cfg)
	fileID := seedPlaybackAPIFile(
		t,
		server.Store,
		"https://source.example/private/movie.mkv?token=movie-token-secret",
		probe,
	)
	return server, fileID
}

func legacyHLSTelemetryRequest(fileID int64, rest string) *http.Request {
	req := httptest.NewRequest(
		http.MethodGet,
		fmt.Sprintf("/api/hls/%d/%s?token=query-canary", fileID, rest),
		nil,
	)
	req.SetPathValue("id", strconv.FormatInt(fileID, 10))
	req.SetPathValue("rest", rest)
	return req
}

func mediaResponseLogs(output string) string {
	var lines []string
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, "event=media_response") {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}
