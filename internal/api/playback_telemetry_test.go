package api

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"doriyamedia/internal/config"
	"doriyamedia/internal/models"
)

func TestPlaybackTelemetryRecordsFixedMediaDimensions(t *testing.T) {
	const (
		queryCanary  = "telemetry-query-canary-8b15"
		headerCanary = "telemetry-header-canary-3a72"
	)
	logs := capturePlaybackSecurityLogs(t)
	v1PlaybackTelemetryRecorder.reset()
	t.Cleanup(v1PlaybackTelemetryRecorder.reset)

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/playback/media-sessions/ms_test/stream?token="+queryCanary, nil)
	req.Header.Set("Authorization", "Bearer "+headerCanary)
	rec := httptest.NewRecorder()
	telemetry := newV1PlaybackTelemetry(rec, "server-proxy", req)
	telemetry.Header().Set("Content-Length", "5")
	telemetry.WriteHeader(http.StatusPartialContent)
	if _, err := telemetry.Write([]byte("media")); err != nil {
		t.Fatalf("write telemetry body: %v", err)
	}
	telemetry.finish()

	snapshot := v1PlaybackTelemetryRecorder.snapshot()
	if len(snapshot) != 1 {
		t.Fatalf("telemetry series = %d, want 1", len(snapshot))
	}
	got := snapshot[0]
	if got.Key.Surface != "v1" ||
		got.Key.Transport != "server-proxy" ||
		got.Key.Asset != playbackAssetStream ||
		got.Key.Outcome != "success" ||
		got.Key.FailureStage != "complete" ||
		got.Value.Count != 1 ||
		got.Value.ResponseBytes != 5 ||
		got.Value.ServerMediaBytes != 5 {
		t.Fatalf("telemetry = %+v, want one successful five-byte server-proxy response", got)
	}

	output := logs.String()
	for _, want := range []string{
		"event=media_response",
		"surface=v1",
		"transport=server-proxy",
		"asset=stream",
		"outcome=success",
		"failure_stage=complete",
		"status=206",
		"response_bytes=5",
		"server_media_bytes=5",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("telemetry log omits %q: %q", want, output)
		}
	}
	for _, secret := range []string{
		queryCanary, headerCanary, "ms_test", "?token=", "Authorization",
	} {
		if strings.Contains(output, secret) {
			t.Errorf("telemetry log leaked %q: %q", secret, output)
		}
	}
}

func TestPlaybackTelemetryHEADOverRealServerDoesNotCountBodyBytes(t *testing.T) {
	v1PlaybackTelemetryRecorder.reset()
	t.Cleanup(v1PlaybackTelemetryRecorder.reset)

	finished := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(finished)
		telemetry := newV1PlaybackTelemetry(w, "server-proxy", r)
		defer telemetry.finish()
		telemetry.Header().Set("Content-Length", "5")
		telemetry.WriteHeader(http.StatusOK)
		_, _ = telemetry.Write([]byte("media"))
	}))
	t.Cleanup(server.Close)

	req, err := http.NewRequest(http.MethodHead, server.URL+"/media", nil)
	if err != nil {
		t.Fatalf("new HEAD request: %v", err)
	}
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("HEAD request: %v", err)
	}
	_ = resp.Body.Close()
	<-finished

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD status = %d, want 200", resp.StatusCode)
	}
	snapshot := v1PlaybackTelemetryRecorder.snapshot()
	if len(snapshot) != 1 {
		t.Fatalf("telemetry series = %d, want 1", len(snapshot))
	}
	got := snapshot[0]
	if got.Key.Outcome != "success" ||
		got.Key.FailureStage != playbackFailureStageComplete ||
		got.Value.ResponseBytes != 0 ||
		got.Value.ServerMediaBytes != 0 {
		t.Fatalf("telemetry = %+v, want successful HEAD with zero body bytes", got)
	}
}

func TestPlaybackTransportIssuedLogsRemainingTransports(t *testing.T) {
	logs := capturePlaybackSecurityLogs(t)
	logV1PlaybackTransportIssued("direct-http", 0, 2, 0, "")

	output := logs.String()
	for _, want := range []string{
		"transport=direct-http",
		"request_issuance_fallback_count=0",
		"remaining_transports=2",
		"ticket_attempt_ordinal=0",
		"last_claimed_transport=none",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("transport issuance log omits %q: %q", want, output)
		}
	}
	if strings.Contains(output, "server_media_bytes=") {
		t.Fatalf("transport issuance log has the wrong fields: %q", output)
	}
}

func TestPlaybackTransportIssuedLogsActualFallbackCount(t *testing.T) {
	logs := capturePlaybackSecurityLogs(t)
	logV1PlaybackTransportIssued("server-proxy", 1, 1, 0, "")

	output := logs.String()
	for _, want := range []string{
		"event=transport_issued",
		"transport=server-proxy",
		"request_issuance_fallback_count=1",
		"remaining_transports=1",
		"ticket_attempt_ordinal=0",
		"last_claimed_transport=none",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("transport issuance log omits %q: %q", want, output)
		}
	}
}

func TestPlaybackIssuanceUsesCurrentRequestFallbackCount(t *testing.T) {
	logs := capturePlaybackSecurityLogs(t)
	server := &Server{}
	ticket := &models.PlaybackTicket{
		State:               models.PlaybackTicketConsuming,
		CandidateTransports: []string{"direct-http", "server-proxy"},
		AttemptedTransports: []string{"direct-http", "server-proxy"},
	}
	transport := &v1PlaybackTransport{
		Kind:                 models.PlaybackTransportServerProxy,
		requestFallbackCount: 0,
		ticketAttemptOrdinal: 1,
		lastClaimedTransport: models.PlaybackTransportDirectHTTP,
	}
	rec := httptest.NewRecorder()

	server.writeV1PlaybackIssuance(
		rec, http.StatusOK, ticket, transport, config.ClientFeatures{})

	output := logs.String()
	for _, want := range []string{
		"request_issuance_fallback_count=0",
		"ticket_attempt_ordinal=1",
		"last_claimed_transport=direct-http",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("current-request fallback telemetry omits %q: %q", want, output)
		}
	}
}

func TestLegacyPlaybackTelemetryUsesFixedSurfaceAndNoRequestIdentifiers(t *testing.T) {
	const queryCanary = "legacy-telemetry-query-canary"
	logs := capturePlaybackSecurityLogs(t)
	v1PlaybackTelemetryRecorder.reset()
	t.Cleanup(v1PlaybackTelemetryRecorder.reset)

	req := httptest.NewRequest(http.MethodGet,
		"/api/stream/41?token="+queryCanary, nil)
	req.Header.Set("Authorization", "Bearer "+queryCanary)
	rec := httptest.NewRecorder()
	telemetry := newPlaybackTelemetry(rec, "legacy-stream", "legacy", req)
	telemetry.Header().Set("Content-Length", "4")
	telemetry.WriteHeader(http.StatusOK)
	if _, err := telemetry.Write([]byte("data")); err != nil {
		t.Fatalf("write telemetry body: %v", err)
	}
	telemetry.finish()

	output := logs.String()
	for _, want := range []string{
		"surface=legacy", "transport=legacy-stream",
		"asset=stream",
		"outcome=success", "failure_stage=complete",
		"status=200", "response_bytes=4", "server_media_bytes=4",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("legacy telemetry log omits %q: %q", want, output)
		}
	}
	for _, secret := range []string{queryCanary, "/api/stream/41", "Authorization"} {
		if strings.Contains(output, secret) {
			t.Errorf("legacy telemetry log leaked %q: %q", secret, output)
		}
	}
}

func TestPlaybackHLSPlaylistTelemetryDoesNotCountAsServerMediaBytes(t *testing.T) {
	logs := capturePlaybackSecurityLogs(t)
	v1PlaybackTelemetryRecorder.reset()
	t.Cleanup(v1PlaybackTelemetryRecorder.reset)

	capability := "ms_" + strings.Repeat("a", 22)
	store := &fakeV1HLSStore{
		access: v1HLSAccess("remux"),
		file:   &models.MediaFile{ID: 42, Path: "https://source.example/movie.mkv"},
	}
	engine := &fakeV1HLSEngine{probe: v1HLSProbe(30, 2)}
	h := v1HLSTestHandler(store, engine)
	rec := httptest.NewRecorder()

	h.servePlaylist(rec, v1HLSPlaylistRequest(capability))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	snapshot := v1PlaybackTelemetryRecorder.snapshot()
	if len(snapshot) != 1 {
		t.Fatalf("telemetry series = %d, want 1", len(snapshot))
	}
	got := snapshot[0]
	if got.Key.Surface != "v1" ||
		got.Key.Transport != models.PlaybackTransportServerHLS ||
		got.Key.Asset != playbackAssetPlaylist ||
		got.Key.Outcome != "success" ||
		got.Key.FailureStage != "complete" ||
		got.Value.ResponseBytes != int64(rec.Body.Len()) ||
		got.Value.ServerMediaBytes != 0 {
		t.Fatalf("telemetry = %+v, want one successful HLS playlist response", got)
	}
	output := logs.String()
	for _, want := range []string{
		"surface=v1", "transport=server-hls", "asset=playlist",
		"outcome=success", "failure_stage=complete",
		"response_bytes=", "server_media_bytes=0",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("HLS playlist telemetry omits %q: %q", want, output)
		}
	}
	for _, secret := range []string{capability, "source.example", "movie.mkv"} {
		if strings.Contains(output, secret) {
			t.Errorf("HLS playlist telemetry leaked %q: %q", secret, output)
		}
	}
}

func TestPlaybackHLSPlaylistAndSegmentUseSeparateTelemetrySeries(t *testing.T) {
	logs := capturePlaybackSecurityLogs(t)
	v1PlaybackTelemetryRecorder.reset()
	t.Cleanup(v1PlaybackTelemetryRecorder.reset)

	capability := "ms_" + strings.Repeat("b", 22)
	store := &fakeV1HLSStore{
		access: v1HLSAccess("remux"),
		file:   &models.MediaFile{ID: 43, Path: "https://source.example/private/movie.mkv"},
	}
	engine := &fakeV1HLSEngine{
		probe:       v1HLSProbe(30, 5),
		segmentBody: []byte("segment-media"),
	}
	h := v1HLSTestHandler(store, engine)

	playlist := httptest.NewRecorder()
	h.servePlaylist(playlist, v1HLSPlaylistRequest(capability))
	segment := httptest.NewRecorder()
	h.serveSegment(segment, v1HLSSegmentRequest(capability, "0.ts"))

	if playlist.Code != http.StatusOK || segment.Code != http.StatusOK {
		t.Fatalf("statuses = playlist %d, segment %d", playlist.Code, segment.Code)
	}
	snapshot := v1PlaybackTelemetryRecorder.snapshot()
	if len(snapshot) != 2 {
		t.Fatalf("telemetry series = %d, want separate playlist and segment series", len(snapshot))
	}
	byAsset := map[string]v1PlaybackTelemetryValue{}
	for _, series := range snapshot {
		byAsset[series.Key.Asset] = series.Value
	}
	playlistValue, ok := byAsset[playbackAssetPlaylist]
	if !ok || playlistValue.ResponseBytes != int64(playlist.Body.Len()) ||
		playlistValue.ServerMediaBytes != 0 {
		t.Fatalf("playlist telemetry = %+v, ok=%v", playlistValue, ok)
	}
	segmentValue, ok := byAsset[playbackAssetSegment]
	if !ok || segmentValue.ResponseBytes != int64(segment.Body.Len()) ||
		segmentValue.ServerMediaBytes != int64(segment.Body.Len()) {
		t.Fatalf("segment telemetry = %+v, ok=%v", segmentValue, ok)
	}
	output := logs.String()
	for _, want := range []string{"asset=playlist", "asset=segment"} {
		if !strings.Contains(output, want) {
			t.Errorf("HLS telemetry log omits %q: %q", want, output)
		}
	}
	for _, secret := range []string{capability, "source.example", "movie.mkv"} {
		if strings.Contains(output, secret) {
			t.Errorf("HLS telemetry leaked %q: %q", secret, output)
		}
	}
}

type playbackTelemetryFailingWriter struct {
	header    http.Header
	status    int
	writeN    int
	writeErr  error
	writeBody bool
}

func (w *playbackTelemetryFailingWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *playbackTelemetryFailingWriter) WriteHeader(status int) {
	w.status = status
}

func (w *playbackTelemetryFailingWriter) Write(p []byte) (int, error) {
	w.writeBody = true
	n := w.writeN
	if n > len(p) {
		n = len(p)
	}
	return n, w.writeErr
}

func TestWriteJSONResponseRejectsShortWrite(t *testing.T) {
	writer := &playbackTelemetryFailingWriter{writeN: 3}
	err := writeJSONResponse(
		writer, http.StatusOK, map[string]string{"state": "ready"},
	)
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("writeJSONResponse error = %v, want io.ErrShortWrite", err)
	}
}

func TestPlaybackTelemetryMarksWriterErrorsAndShortWritesAborted(t *testing.T) {
	tests := []struct {
		name     string
		writeN   int
		writeErr error
		want     int64
	}{
		{name: "writer error", writeN: 5, writeErr: errors.New("client disconnected"), want: 5},
		{name: "short write", writeN: 3, want: 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v1PlaybackTelemetryRecorder.reset()
			t.Cleanup(v1PlaybackTelemetryRecorder.reset)
			writer := &playbackTelemetryFailingWriter{
				writeN: tc.writeN, writeErr: tc.writeErr,
			}
			req := httptest.NewRequest(http.MethodGet, "/media", nil)
			telemetry := newV1PlaybackTelemetry(writer, "server-proxy", req)
			telemetry.Header().Set("Content-Length", "5")
			telemetry.WriteHeader(http.StatusOK)
			if _, err := telemetry.Write([]byte("media")); err == nil && tc.writeErr != nil {
				t.Fatal("writer error was not returned")
			}
			telemetry.finish()

			snapshot := v1PlaybackTelemetryRecorder.snapshot()
			if len(snapshot) != 1 {
				t.Fatalf("telemetry series = %d, want 1", len(snapshot))
			}
			got := snapshot[0]
			if got.Key.Outcome != "aborted" ||
				got.Key.FailureStage != "body" ||
				got.Value.ServerMediaBytes != tc.want {
				t.Fatalf("telemetry = %+v, want aborted/body with %d bytes", got, tc.want)
			}
		})
	}
}

func TestPlaybackTelemetryMarksTruncatedAndFailedResponses(t *testing.T) {
	tests := []struct {
		name          string
		status        int
		contentLength string
		body          string
		outcome       string
		stage         string
		mediaBytes    int64
	}{
		{
			name: "truncated body", status: http.StatusOK, contentLength: "9",
			body: "short", outcome: "aborted", stage: "body", mediaBytes: 5,
		},
		{
			name: "source failure", status: http.StatusBadGateway,
			body: `{"error":{}}`, outcome: "error", stage: "source", mediaBytes: 0,
		},
		{
			name: "authorization failure", status: http.StatusForbidden,
			body: `{"error":{}}`, outcome: "error", stage: "authorization", mediaBytes: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v1PlaybackTelemetryRecorder.reset()
			t.Cleanup(v1PlaybackTelemetryRecorder.reset)
			req := httptest.NewRequest(http.MethodGet, "/media", nil)
			rec := httptest.NewRecorder()
			telemetry := newV1PlaybackTelemetry(rec, "server-hls", req)
			if tc.contentLength != "" {
				telemetry.Header().Set("Content-Length", tc.contentLength)
			}
			telemetry.WriteHeader(tc.status)
			if _, err := telemetry.Write([]byte(tc.body)); err != nil {
				t.Fatalf("write telemetry body: %v", err)
			}
			telemetry.finish()

			snapshot := v1PlaybackTelemetryRecorder.snapshot()
			if len(snapshot) != 1 {
				t.Fatalf("telemetry series = %d, want 1", len(snapshot))
			}
			got := snapshot[0]
			if got.Key.Outcome != tc.outcome ||
				got.Key.FailureStage != tc.stage ||
				got.Value.ServerMediaBytes != tc.mediaBytes {
				t.Fatalf("telemetry = %+v, want outcome=%s stage=%s bytes=%d",
					got, tc.outcome, tc.stage, tc.mediaBytes)
			}
		})
	}
}

func TestPlaybackFailureStageMatrix(t *testing.T) {
	tests := []struct {
		name   string
		status int
		code   V1ErrorCode
		want   string
	}{
		{name: "bad request", status: http.StatusBadRequest, want: playbackFailureStageRequest},
		{name: "method not allowed", status: http.StatusMethodNotAllowed, want: playbackFailureStageRequest},
		{name: "range unsatisfiable", status: http.StatusRequestedRangeNotSatisfiable, want: playbackFailureStageRequest},
		{
			name: "invalid request 404", status: http.StatusNotFound,
			code: V1ErrorInvalidRequest, want: playbackFailureStageRequest,
		},
		{
			name: "feature disabled 404", status: http.StatusNotFound,
			code: V1ErrorFeatureDisabled, want: playbackFailureStageAuthorization,
		},
		{name: "unauthenticated", status: http.StatusUnauthorized, want: playbackFailureStageAuthorization},
		{name: "forbidden", status: http.StatusForbidden, want: playbackFailureStageAuthorization},
		{
			name: "session not found", status: http.StatusNotFound,
			code: V1ErrorMediaSessionNotFound, want: playbackFailureStageSession,
		},
		{name: "conflict", status: http.StatusConflict, want: playbackFailureStageSession},
		{name: "gone", status: http.StatusGone, want: playbackFailureStageSession},
		{name: "rate limited", status: http.StatusTooManyRequests, want: playbackFailureStageRateLimit},
		{name: "unsupported media", status: http.StatusUnsupportedMediaType, want: playbackFailureStageSource},
		{name: "bad gateway", status: http.StatusBadGateway, want: playbackFailureStageSource},
		{name: "unavailable", status: http.StatusServiceUnavailable, want: playbackFailureStageSource},
		{name: "gateway timeout", status: http.StatusGatewayTimeout, want: playbackFailureStageSource},
		{name: "internal", status: http.StatusInternalServerError, want: playbackFailureStageInternal},
		{name: "other 5xx", status: 599, want: playbackFailureStageInternal},
		{name: "other response", status: http.StatusTeapot, want: playbackFailureStageResponse},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := v1PlaybackFailureStage(tc.status)
			if tc.code != "" {
				got = v1PlaybackFailureStageForError(tc.status, tc.code)
			}
			if got != tc.want {
				t.Fatalf("failure stage = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPlaybackTelemetryDimensionsUseIndependentFixedEnums(t *testing.T) {
	if got := safePlaybackTelemetryTransport("direct-dash"); got != "direct-dash" {
		t.Fatalf("transport sanitizer rejected direct-dash: %q", got)
	}
	if got := safePlaybackTelemetryTransport("success"); got != "unknown" {
		t.Fatalf("transport sanitizer accepted an outcome value: %q", got)
	}
	if got := safePlaybackTelemetryOutcome("server-proxy"); got != "unknown" {
		t.Fatalf("outcome sanitizer accepted a transport value: %q", got)
	}
	if got := safePlaybackTelemetryFailureStage("direct-http"); got != "unknown" {
		t.Fatalf("failure-stage sanitizer accepted a transport value: %q", got)
	}
	if got := safePlaybackTelemetryAsset("other"); got != playbackAssetStream {
		t.Fatalf("asset sanitizer = %q, want default %q", got, playbackAssetStream)
	}
}

func TestV1HLSErrorAppliesFixedAmbiguousStatusOverrides(t *testing.T) {
	tests := []struct {
		code V1ErrorCode
		want string
	}{
		{code: V1ErrorInvalidRequest, want: playbackFailureStageRequest},
		{code: V1ErrorMediaSessionNotFound, want: playbackFailureStageSession},
		{code: V1ErrorFeatureDisabled, want: playbackFailureStageAuthorization},
	}
	for _, tc := range tests {
		t.Run(string(tc.code), func(t *testing.T) {
			v1PlaybackTelemetryRecorder.reset()
			t.Cleanup(v1PlaybackTelemetryRecorder.reset)

			rec := httptest.NewRecorder()
			telemetry := newV1PlaybackTelemetry(rec, models.PlaybackTransportServerHLS, nil)
			telemetry.setAsset(playbackAssetPlaylist)
			writeV1HLSError(telemetry, http.StatusNotFound, tc.code, "fixed", nil)
			telemetry.finish()

			snapshot := v1PlaybackTelemetryRecorder.snapshot()
			if len(snapshot) != 1 {
				t.Fatalf("telemetry series = %d, want 1", len(snapshot))
			}
			got := snapshot[0]
			if got.Key.FailureStage != tc.want ||
				got.Key.Outcome != "error" ||
				got.Key.Asset != playbackAssetPlaylist ||
				got.Value.ServerMediaBytes != 0 {
				t.Fatalf("telemetry = %+v, want 404 stage %q", got, tc.want)
			}
		})
	}
}

func TestV1PlaybackErrorAppliesFixedAmbiguousStatusOverrides(t *testing.T) {
	tests := []struct {
		code V1ErrorCode
		want string
	}{
		{code: V1ErrorInvalidRequest, want: playbackFailureStageRequest},
		{code: V1ErrorMediaSessionNotFound, want: playbackFailureStageSession},
		{code: V1ErrorFeatureDisabled, want: playbackFailureStageAuthorization},
	}
	for _, tc := range tests {
		t.Run(string(tc.code), func(t *testing.T) {
			v1PlaybackTelemetryRecorder.reset()
			t.Cleanup(v1PlaybackTelemetryRecorder.reset)

			rec := httptest.NewRecorder()
			telemetry := newV1PlaybackTelemetry(
				rec, models.PlaybackTransportServerProxy, nil)
			writeV1PlaybackError(telemetry, http.StatusNotFound, tc.code, "fixed", nil)
			telemetry.finish()

			snapshot := v1PlaybackTelemetryRecorder.snapshot()
			if len(snapshot) != 1 {
				t.Fatalf("telemetry series = %d, want 1", len(snapshot))
			}
			got := snapshot[0]
			if got.Key.FailureStage != tc.want ||
				got.Key.Outcome != "error" ||
				got.Value.ServerMediaBytes != 0 {
				t.Fatalf("telemetry = %+v, want 404 stage %q", got, tc.want)
			}
		})
	}
}

func TestPlaybackHEADWrapperPreservesFailureStageTelemetry(t *testing.T) {
	v1PlaybackTelemetryRecorder.reset()
	t.Cleanup(v1PlaybackTelemetryRecorder.reset)

	req := httptest.NewRequest(http.MethodHead,
		"/api/v1/playback/media-sessions/ms_test/stream", nil)
	rec := httptest.NewRecorder()
	telemetry := newV1PlaybackTelemetry(
		rec, models.PlaybackTransportServerProxy, req)
	head := v1ProxyHeadWriter{ResponseWriter: telemetry}
	writeV1PlaybackError(head, http.StatusNotFound, V1ErrorFeatureDisabled, "fixed", nil)
	telemetry.finish()

	snapshot := v1PlaybackTelemetryRecorder.snapshot()
	if len(snapshot) != 1 {
		t.Fatalf("telemetry series = %d, want 1", len(snapshot))
	}
	got := snapshot[0]
	if got.Key.FailureStage != playbackFailureStageAuthorization ||
		got.Key.Outcome != "error" ||
		got.Value.ResponseBytes != 0 ||
		got.Value.ServerMediaBytes != 0 {
		t.Fatalf("HEAD telemetry = %+v, want authorization error with zero bytes", got)
	}
}
