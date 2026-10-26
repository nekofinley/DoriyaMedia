package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"doriyamedia/internal/config"
	"doriyamedia/internal/db"
	"doriyamedia/internal/media"
	"doriyamedia/internal/models"
	"doriyamedia/internal/source"
)

type fakeV1HLSStore struct {
	access       *db.PlaybackMediaSessionAccess
	authorizeErr []error
	authorizeLog []fakeV1HLSAuthorizeCall
	file         *models.MediaFile
	fileErr      error
	revokeErr    error
	revokeCalls  []string
}

type fakeV1HLSAuthorizeCall struct {
	capability               string
	requireEmailVerification bool
	now                      time.Time
}

func (s *fakeV1HLSStore) AuthorizePlaybackMediaSessionContext(
	ctx context.Context,
	capability string,
	requireEmailVerification bool,
	now time.Time,
) (*db.PlaybackMediaSessionAccess, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.authorizeLog = append(s.authorizeLog, fakeV1HLSAuthorizeCall{
		capability: capability, requireEmailVerification: requireEmailVerification, now: now,
	})
	if len(s.authorizeErr) > 0 {
		err := s.authorizeErr[0]
		s.authorizeErr = s.authorizeErr[1:]
		if err != nil {
			return nil, err
		}
	}
	return s.access, nil
}

func (s *fakeV1HLSStore) FileByID(id int64) (*models.MediaFile, error) {
	if s.fileErr != nil {
		return nil, s.fileErr
	}
	if s.file == nil {
		return &models.MediaFile{ID: id, Path: "https://source.example/private.mkv?token=secret"}, nil
	}
	return s.file, nil
}

func (s *fakeV1HLSStore) RevokePlaybackMediaSessionsForTransport(
	transport string,
	_ time.Time,
) (int64, error) {
	s.revokeCalls = append(s.revokeCalls, transport)
	return 0, s.revokeErr
}

type fakeV1HLSEngine struct {
	driver      source.Driver
	driverErr   error
	probe       *media.Probe
	probeErr    error
	segmentBody []byte
	segmentErr  error

	ensureProbeCalls int
	playlistCalls    int
	playlistBase     string
	playlistSuffix   string
	playlistDuration float64
	playlistSegLen   float64

	segmentCalls           int
	segmentStart           float64
	segmentLength          float64
	segmentVideoMode       string
	segmentAudioMode       string
	segmentAudioOrdinal    int
	segmentVideoIndex      int
	segmentTranscodeHeight int
}

func (e *fakeV1HLSEngine) DriverFor(*models.MediaFile) (source.Driver, error) {
	return e.driver, e.driverErr
}

func (e *fakeV1HLSEngine) EnsureProbe(context.Context, *models.MediaFile, source.Driver) (*media.Probe, error) {
	e.ensureProbeCalls++
	return e.probe, e.probeErr
}

func (e *fakeV1HLSEngine) Playlist(basePath, suffix string, duration, segLen float64) string {
	e.playlistCalls++
	e.playlistBase = basePath
	e.playlistSuffix = suffix
	e.playlistDuration = duration
	e.playlistSegLen = segLen
	return (&media.Engine{}).Playlist(basePath, suffix, duration, segLen)
}

func (e *fakeV1HLSEngine) Segment(
	_ context.Context,
	_ *models.MediaFile,
	_ source.Driver,
	start, length float64,
	videoMode, audioMode string,
	audioOrdinal, videoIndex, transcodeHeight int,
	out io.Writer,
) (int64, error) {
	e.segmentCalls++
	e.segmentStart = start
	e.segmentLength = length
	e.segmentVideoMode = videoMode
	e.segmentAudioMode = audioMode
	e.segmentAudioOrdinal = audioOrdinal
	e.segmentVideoIndex = videoIndex
	e.segmentTranscodeHeight = transcodeHeight
	if e.segmentErr != nil {
		return 0, e.segmentErr
	}
	if len(e.segmentBody) == 0 {
		e.segmentBody = []byte("mpeg-ts")
	}
	n, err := out.Write(e.segmentBody)
	return int64(n), err
}

func TestV1HLSCapabilityParsing(t *testing.T) {
	validMin := "ms_" + strings.Repeat("a", 22)
	validMax := "ms_" + strings.Repeat("Z9_-", 24)
	tests := []struct {
		name string
		raw  string
		want bool
	}{
		{name: "minimum length", raw: validMin, want: true},
		{name: "maximum length", raw: validMax, want: true},
		{name: "missing prefix", raw: "pt_" + strings.Repeat("a", 22)},
		{name: "too short", raw: "ms_" + strings.Repeat("a", 21)},
		{name: "too long", raw: "ms_" + strings.Repeat("a", 97)},
		{name: "path separator", raw: "ms_" + strings.Repeat("a", 21) + "/"},
		{name: "dot", raw: "ms_" + strings.Repeat("a", 21) + "."},
		{name: "space", raw: "ms_" + strings.Repeat("a", 21) + " "},
		{name: "empty", raw: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.SetPathValue("id", tc.raw)
			got, ok := v1HLSCapabilityFromPath(req)
			if ok != tc.want {
				t.Fatalf("ok = %v, want %v", ok, tc.want)
			}
			if tc.want && got != tc.raw {
				t.Fatalf("capability = %q, want %q", got, tc.raw)
			}
			if !tc.want && got != "" {
				t.Fatalf("invalid capability returned %q", got)
			}
		})
	}
}

func TestV1HLSSegmentParsing(t *testing.T) {
	tests := []struct {
		raw   string
		index int
		ok    bool
	}{
		{raw: "0.ts", index: 0, ok: true},
		{raw: "000.ts", index: 0, ok: true},
		{raw: "42.ts", index: 42, ok: true},
		{raw: "", ok: false},
		{raw: ".ts", ok: false},
		{raw: "-1.ts", ok: false},
		{raw: "+1.ts", ok: false},
		{raw: "1", ok: false},
		{raw: "1.m3u8", ok: false},
		{raw: "../1.ts", ok: false},
		{raw: "1.ts/2", ok: false},
		{raw: "9223372036854775808.ts", ok: false},
	}
	for _, tc := range tests {
		t.Run(tc.raw, func(t *testing.T) {
			got, ok := v1HLSSegmentIndex(tc.raw)
			if ok != tc.ok || got != tc.index {
				t.Fatalf("v1HLSSegmentIndex(%q) = (%d, %v), want (%d, %v)",
					tc.raw, got, ok, tc.index, tc.ok)
			}
		})
	}
}

func TestV1HLSSegmentStart(t *testing.T) {
	if got, ok := v1HLSSegmentStart(10, 2); !ok || got != 18 {
		t.Fatalf("start = (%v, %v), want (18, true)", got, ok)
	}
	if _, ok := v1HLSSegmentStart(-1, 0); ok {
		t.Fatal("negative session start was accepted")
	}
	if _, ok := v1HLSSegmentStart(0, -1); ok {
		t.Fatal("negative segment index was accepted")
	}
	if _, ok := v1HLSSegmentStart(math.MaxFloat64, 1); ok {
		t.Fatal("overflowing segment start was accepted")
	}
	if _, ok := v1HLSSegmentStart(math.NaN(), 0); ok {
		t.Fatal("NaN session start was accepted")
	}
}

func TestV1ServerHLSFeatureGate(t *testing.T) {
	capability := "ms_" + strings.Repeat("a", 22)
	tests := []struct {
		name   string
		cfg    *config.Config
		status int
		code   V1ErrorCode
	}{
		{
			name: "v1 disabled",
			cfg: &config.Config{Client: config.ClientFeatures{
				PlaybackTicketEnabled: true, ServerHLSV1Enabled: true,
			}},
			status: http.StatusNotFound,
			code:   V1ErrorFeatureDisabled,
		},
		{
			name: "playback tickets disabled",
			cfg: &config.Config{Client: config.ClientFeatures{
				V1Enabled: true, ServerHLSV1Enabled: true,
			}},
			status: http.StatusNotFound,
			code:   V1ErrorFeatureDisabled,
		},
		{
			name: "server hls disabled",
			cfg: &config.Config{Client: config.ClientFeatures{
				V1Enabled: true, DeviceAuthEnabled: true, PlaybackTicketEnabled: true,
			}},
			status: http.StatusForbidden,
			code:   V1ErrorTransportDisabled,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := &Server{Cfg: tc.cfg}
			req := httptest.NewRequest(http.MethodGet, "/api/v1/playback/media-sessions/"+capability+"/index.m3u8", nil)
			req.SetPathValue("id", capability)
			rec := httptest.NewRecorder()

			server.handleV1ServerHLSPlaylist(rec, req)

			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tc.status, rec.Body.String())
			}
			if got := decodeV1HLSError(t, rec); got != tc.code {
				t.Fatalf("error code = %q, want %q", got, tc.code)
			}
			assertV1HLSCommonResponseHeaders(t, rec)
		})
	}
}

func TestV1ServerHLSRoutesAreMounted(t *testing.T) {
	capability := "ms_" + strings.Repeat("a", 22)
	server := &Server{Cfg: v1HLSTestConfig()}
	handler := server.Handler()

	for _, tc := range []struct {
		name   string
		method string
		path   string
		status int
	}{
		{
			name:   "playlist reaches handler",
			method: http.MethodGet,
			path:   "/api/v1/playback/media-sessions/" + capability + "/index.m3u8",
			status: http.StatusInternalServerError,
		},
		{
			name:   "playlist head is rejected by HLS handler",
			method: http.MethodHead,
			path:   "/api/v1/playback/media-sessions/" + capability + "/index.m3u8",
			status: http.StatusMethodNotAllowed,
		},
		{
			name:   "segment reaches handler",
			method: http.MethodGet,
			path:   "/api/v1/playback/media-sessions/" + capability + "/0.ts",
			status: http.StatusInternalServerError,
		},
		{
			name:   "segment head is rejected by HLS handler",
			method: http.MethodHead,
			path:   "/api/v1/playback/media-sessions/" + capability + "/0.ts",
			status: http.StatusMethodNotAllowed,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tc.status, rec.Body.String())
			}
			if tc.method == http.MethodHead {
				if got := rec.Header().Get("Allow"); got != http.MethodGet {
					t.Fatalf("Allow = %q, want GET", got)
				}
				if got := decodeV1HLSError(t, rec); got != V1ErrorInvalidRequest {
					t.Fatalf("error code = %q, want %q", got, V1ErrorInvalidRequest)
				}
				assertV1HLSCommonResponseHeaders(t, rec)
			}
		})
	}
}

func TestV1ServerHLSHeadHasNoNetworkBody(t *testing.T) {
	capability := "ms_" + strings.Repeat("a", 22)
	server := &Server{Cfg: v1HLSTestConfig()}
	ts := httptest.NewServer(server.Handler())
	t.Cleanup(ts.Close)

	for _, path := range []string{
		"/api/v1/playback/media-sessions/" + capability + "/index.m3u8",
		"/api/v1/playback/media-sessions/" + capability + "/0.ts",
	} {
		t.Run(path, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodHead, ts.URL+path, nil)
			if err != nil {
				t.Fatalf("new HLS HEAD request: %v", err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("HLS HEAD request: %v", err)
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read HLS HEAD body: %v", err)
			}
			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Fatalf("status = %d, want 405", resp.StatusCode)
			}
			if got := resp.Header.Get("Allow"); got != http.MethodGet {
				t.Fatalf("Allow = %q, want GET", got)
			}
			if len(body) != 0 {
				t.Fatalf("HLS HEAD returned %d body bytes", len(body))
			}
		})
	}
}

func TestV1ServerHLSRejectsQueryBeforeAuthorization(t *testing.T) {
	capability := "ms_" + strings.Repeat("a", 22)
	for _, target := range []string{
		"/api/v1/playback/media-sessions/" + capability + "/index.m3u8?capability=other",
		"/api/v1/playback/media-sessions/" + capability + "/index.m3u8?",
	} {
		t.Run(target, func(t *testing.T) {
			store := &fakeV1HLSStore{access: v1HLSAccess("remux")}
			engine := &fakeV1HLSEngine{probe: v1HLSProbe(30, 2)}
			h := v1HLSTestHandler(store, engine)
			req := httptest.NewRequest(http.MethodGet, target, nil)
			req.SetPathValue("id", capability)
			rec := httptest.NewRecorder()

			h.servePlaylist(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
			if got := decodeV1HLSError(t, rec); got != V1ErrorInvalidRequest {
				t.Fatalf("error code = %q, want %q", got, V1ErrorInvalidRequest)
			}
			if len(store.authorizeLog) != 0 {
				t.Fatalf("query request authorized %d time(s)", len(store.authorizeLog))
			}
		})
	}
}

func TestV1ServerHLSPlaylistShapeAndAuthorization(t *testing.T) {
	capability := "ms_" + strings.Repeat("a", 22)
	fixedNow := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	store := &fakeV1HLSStore{
		access: v1HLSAccess("remux"),
		file: &models.MediaFile{
			ID:   42,
			Path: "https://source.example/private/movie.mkv?token=must-not-leak",
		},
	}
	engine := &fakeV1HLSEngine{probe: v1HLSProbe(20, 3)}
	h := v1HLSTestHandler(store, engine)
	h.now = func() time.Time { return fixedNow }
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/playback/media-sessions/"+capability+"/index.m3u8", nil)
	req.SetPathValue("id", capability)
	rec := httptest.NewRecorder()

	h.servePlaylist(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	assertV1HLSCommonResponseHeaders(t, rec)
	if got := rec.Header().Get("Content-Type"); got != v1HLSPlaylistContentType {
		t.Errorf("Content-Type = %q, want %q", got, v1HLSPlaylistContentType)
	}
	if len(store.authorizeLog) != 1 {
		t.Fatalf("authorize calls = %d, want 1", len(store.authorizeLog))
	}
	auth := store.authorizeLog[0]
	if auth.capability != capability || !auth.requireEmailVerification || !auth.now.Equal(fixedNow) {
		t.Fatalf("authorize call = %+v", auth)
	}
	if engine.playlistCalls != 1 {
		t.Fatalf("playlist calls = %d, want 1", engine.playlistCalls)
	}
	if engine.playlistBase != "/api/v1/playback/media-sessions/"+capability {
		t.Errorf("playlist base = %q", engine.playlistBase)
	}
	if engine.playlistSuffix != ".ts" {
		t.Errorf("playlist suffix = %q, want .ts", engine.playlistSuffix)
	}
	if engine.playlistDuration != 10 || engine.playlistSegLen != v1HLSSegmentSeconds {
		t.Errorf("playlist duration/segLen = (%v, %v), want (10, 4)",
			engine.playlistDuration, engine.playlistSegLen)
	}

	lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
	var uris []string
	for _, line := range lines {
		if line != "" && !strings.HasPrefix(line, "#") {
			uris = append(uris, line)
		}
	}
	wantURIs := []string{
		"/api/v1/playback/media-sessions/" + capability + "/0.ts",
		"/api/v1/playback/media-sessions/" + capability + "/1.ts",
		"/api/v1/playback/media-sessions/" + capability + "/2.ts",
	}
	if strings.Join(uris, "\n") != strings.Join(wantURIs, "\n") {
		t.Fatalf("playlist URIs:\n%s\nwant:\n%s", strings.Join(uris, "\n"), strings.Join(wantURIs, "\n"))
	}
	for _, forbidden := range []string{"source.example", "must-not-leak", "/private/", "?", "token="} {
		if strings.Contains(rec.Body.String(), forbidden) {
			t.Errorf("playlist leaked %q:\n%s", forbidden, rec.Body.String())
		}
	}
}

func TestV1ServerHLSRejectsUnsafePathAndSegment(t *testing.T) {
	t.Run("invalid capability", func(t *testing.T) {
		store := &fakeV1HLSStore{access: v1HLSAccess("remux")}
		engine := &fakeV1HLSEngine{probe: v1HLSProbe(30, 2)}
		h := v1HLSTestHandler(store, engine)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/playback/media-sessions/ms_short/index.m3u8", nil)
		req.SetPathValue("id", "ms_short")
		rec := httptest.NewRecorder()

		h.servePlaylist(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
		}
		if len(store.authorizeLog) != 0 {
			t.Fatal("invalid path capability triggered authorization")
		}
	})

	t.Run("invalid segment", func(t *testing.T) {
		capability := "ms_" + strings.Repeat("a", 22)
		store := &fakeV1HLSStore{access: v1HLSAccess("remux")}
		engine := &fakeV1HLSEngine{probe: v1HLSProbe(30, 2)}
		h := v1HLSTestHandler(store, engine)
		req := httptest.NewRequest(http.MethodGet,
			"/api/v1/playback/media-sessions/"+capability+"/../index.ts", nil)
		req.SetPathValue("id", capability)
		req.SetPathValue("segment", "../index.ts")
		rec := httptest.NewRecorder()

		h.serveSegment(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
		if len(store.authorizeLog) != 0 {
			t.Fatal("invalid segment triggered authorization")
		}
	})
}

func TestV1ServerHLSSegmentModesAndArguments(t *testing.T) {
	tests := []struct {
		name       string
		mode       string
		height     *int
		wantVideo  string
		wantAudio  string
		wantHeight int
	}{
		{name: "remux", mode: "remux", wantVideo: "copy", wantAudio: "copy", wantHeight: 1080},
		{name: "remux aac", mode: "remux-aac", wantVideo: "copy", wantAudio: "aac", wantHeight: 1080},
		{name: "transcode", mode: "transcode", height: intPtr(720), wantVideo: "transcode", wantAudio: "aac", wantHeight: 720},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			capability := "ms_" + strings.Repeat("a", 22)
			access := v1HLSAccess(tc.mode)
			access.Session.TranscodeHeight = tc.height
			store := &fakeV1HLSStore{access: access}
			engine := &fakeV1HLSEngine{probe: v1HLSProbe(60, 5), segmentBody: []byte("mpeg-ts-body")}
			h := v1HLSTestHandler(store, engine)
			req := v1HLSSegmentRequest(capability, "2.ts")
			rec := httptest.NewRecorder()

			h.serveSegment(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			assertV1HLSCommonResponseHeaders(t, rec)
			if got := rec.Header().Get("Content-Type"); got != v1HLSSegmentContentType {
				t.Errorf("Content-Type = %q, want %q", got, v1HLSSegmentContentType)
			}
			if rec.Body.String() != "mpeg-ts-body" {
				t.Errorf("body = %q", rec.Body.String())
			}
			if len(store.authorizeLog) != 1 {
				t.Fatalf("authorize calls = %d, want 1", len(store.authorizeLog))
			}
			if engine.segmentCalls != 1 {
				t.Fatalf("segment calls = %d, want 1", engine.segmentCalls)
			}
			if engine.segmentStart != 18 || engine.segmentLength != 4 {
				t.Errorf("segment start/length = (%v, %v), want (18, 4)",
					engine.segmentStart, engine.segmentLength)
			}
			if engine.segmentVideoMode != tc.wantVideo || engine.segmentAudioMode != tc.wantAudio {
				t.Errorf("modes = (%q, %q), want (%q, %q)",
					engine.segmentVideoMode, engine.segmentAudioMode, tc.wantVideo, tc.wantAudio)
			}
			if engine.segmentAudioOrdinal != access.Session.AudioOrdinal {
				t.Errorf("audio ordinal = %d, want %d", engine.segmentAudioOrdinal, access.Session.AudioOrdinal)
			}
			if engine.segmentVideoIndex != 5 {
				t.Errorf("video index = %d, want 5", engine.segmentVideoIndex)
			}
			if engine.segmentTranscodeHeight != tc.wantHeight {
				t.Errorf("transcode height = %d, want %d", engine.segmentTranscodeHeight, tc.wantHeight)
			}
		})
	}
}

func TestV1ServerHLSRejectsLegacySessionBeforeProbe(t *testing.T) {
	capability := "ms_" + strings.Repeat("a", 22)
	access := v1HLSAccess("remux")
	access.Session.VideoIndex = models.PlaybackVideoIndexUnknown
	store := &fakeV1HLSStore{access: access}
	engine := &fakeV1HLSEngine{probe: v1HLSProbe(60, 5)}
	h := v1HLSTestHandler(store, engine)

	for _, tc := range []struct {
		name string
		run  func(*httptest.ResponseRecorder)
	}{
		{
			name: "playlist",
			run: func(rec *httptest.ResponseRecorder) {
				h.servePlaylist(rec, v1HLSPlaylistRequest(capability))
			},
		},
		{
			name: "segment",
			run: func(rec *httptest.ResponseRecorder) {
				h.serveSegment(rec, v1HLSSegmentRequest(capability, "0.ts"))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tc.run(rec)
			if rec.Code != http.StatusGone {
				t.Fatalf("status = %d, want 410; body=%s", rec.Code, rec.Body.String())
			}
			if got := decodeV1HLSError(t, rec); got != V1ErrorMediaSessionExpired {
				t.Fatalf("error code = %q, want %q", got, V1ErrorMediaSessionExpired)
			}
		})
	}
	if engine.ensureProbeCalls != 0 || engine.segmentCalls != 0 {
		t.Fatalf("legacy HLS session reached probe/segment: probe=%d segment=%d",
			engine.ensureProbeCalls, engine.segmentCalls)
	}
}

func TestV1ServerHLSSegmentUsesProbeFrozenIndexAfterReorder(t *testing.T) {
	f := newPlaybackAPIFixture(t)
	f.server.Cfg.Client = config.ClientFeatures{
		V1Enabled:             true,
		DeviceAuthEnabled:     true,
		PlaybackTicketEnabled: true,
		ServerHLSV1Enabled:    true,
	}
	fileID := seedPlaybackAPIFile(
		t, f.server.Store,
		"https://cdn.example.test/library/two-video-streams.mkv",
		"",
	)
	file, err := f.server.Store.FileByID(fileID)
	if err != nil {
		t.Fatalf("load HLS test file: %v", err)
	}
	driver, err := f.server.Engine.DriverFor(file)
	if err != nil {
		t.Fatalf("load HLS test driver: %v", err)
	}
	var userID, deviceID int64
	if err := f.server.Store.DB.QueryRow(
		`SELECT user_id, id FROM devices WHERE public_id = ?`, f.deviceID,
	).Scan(&userID, &deviceID); err != nil {
		t.Fatalf("read HLS fixture device: %v", err)
	}
	client := media.NativeClientCapabilities{
		Platform:      "windows",
		Version:       "0.1.0",
		Engine:        "mpv",
		Containers:    []string{"mkv"},
		VideoCodecs:   []string{"h264"},
		AudioCodecs:   []string{"aac"},
		SupportsRange: true,
	}
	clientJSON, err := json.Marshal(client)
	if err != nil {
		t.Fatalf("marshal HLS client capabilities: %v", err)
	}
	ticket, err := f.server.Store.CreatePlaybackTicket(models.PlaybackTicketRequest{
		UserID:              userID,
		DeviceID:            deviceID,
		FileID:              fileID,
		Purpose:             models.PlaybackPurposePlayback,
		CandidateTransports: []string{},
		MediaSummary:        json.RawMessage(`{}`),
		ClientCapabilities:  clientJSON,
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("create HLS playback ticket: %v", err)
	}
	binding := playbackBinding(ticket)

	// The initial probe has two video streams. The first (index 2) is the one
	// selected and frozen on the ticket; index 7 is never authorized implicitly.
	initialProbe := &media.Probe{
		Streams: []media.ProbeStream{
			{Index: 2, CodecType: "video", CodecName: "h264", Width: 1920, Height: 1080},
			{Index: 7, CodecType: "video", CodecName: "h264", Width: 1280, Height: 720},
			{Index: 3, CodecType: "audio", CodecName: "aac", Channels: 2},
		},
		Format: media.ProbeFormat{
			FormatName: "matroska,webm",
			Duration:   json.Number("60"),
		},
	}
	features := config.ClientFeatures{ServerHLSV1Enabled: true}
	ready, err := f.server.finishV1PlaybackTicketWithProbe(
		context.Background(), binding, ticket.Version, file, driver,
		initialProbe, client, features,
	)
	if err != nil {
		t.Fatalf("finish HLS playback probe: %v", err)
	}
	if ready.VideoIndex != 2 {
		t.Fatalf("ticket video index = %d, want 2", ready.VideoIndex)
	}
	claimed, err := f.server.Store.ClaimNextPlaybackTransport(
		context.Background(), binding, ready.Version, time.Now().UTC())
	if err != nil {
		t.Fatalf("claim HLS transport: %v", err)
	}
	if claimed.SelectedTransport != models.PlaybackTransportServerHLS {
		t.Fatalf("claimed transport = %q, want server-hls", claimed.SelectedTransport)
	}
	req := httptest.NewRequest(http.MethodPost, "/refresh", nil)
	transport, err := f.server.issueV1PlaybackTransport(
		req, binding, claimed, file, driver,
		media.NativeTransportSelection{
			Candidates: []media.NativeTransportCandidate{{
				Kind: media.NativeTransportServerHLS,
			}},
		},
	)
	if err != nil {
		t.Fatalf("issue HLS transport: %v", err)
	}
	capability := playbackLifecycleCapability(t, transport.Streams[0].URL)
	var persistedIndex int
	if err := f.server.Store.DB.QueryRow(`
		SELECT video_index FROM playback_media_sessions
		WHERE ticket_id = ? AND transport = ?`,
		ticket.TicketID, models.PlaybackTransportServerHLS,
	).Scan(&persistedIndex); err != nil {
		t.Fatalf("read frozen HLS session index: %v", err)
	}
	if persistedIndex != 2 {
		t.Fatalf("persisted session video index = %d, want 2", persistedIndex)
	}

	// A later probe with the streams reordered must not silently switch the
	// media session to the first video stream currently visible to ffprobe.
	reorderedProbe := &media.Probe{
		Streams: []media.ProbeStream{
			{Index: 7, CodecType: "video", CodecName: "h264", Width: 1280, Height: 720},
			{Index: 2, CodecType: "video", CodecName: "h264", Width: 1920, Height: 1080},
			{Index: 3, CodecType: "audio", CodecName: "aac", Channels: 2},
		},
		Format: initialProbe.Format,
	}
	engine := &fakeV1HLSEngine{
		probe:       reorderedProbe,
		segmentBody: []byte("frozen-video-segment"),
	}
	h := v1HLSTestHandler(f.server.Store, engine)
	cfg := v1HLSTestConfig()
	cfg.Users.RequireEmail = false
	h.cfg = cfg
	h.now = time.Now

	playlist := httptest.NewRecorder()
	h.servePlaylist(playlist, v1HLSPlaylistRequest(capability))
	if playlist.Code != http.StatusOK {
		t.Fatalf("playlist status = %d; body=%s", playlist.Code, playlist.Body.String())
	}
	segment := httptest.NewRecorder()
	h.serveSegment(segment, v1HLSSegmentRequest(capability, "0.ts"))
	if segment.Code != http.StatusOK {
		t.Fatalf("segment status = %d; body=%s", segment.Code, segment.Body.String())
	}
	if engine.segmentVideoIndex != 2 {
		t.Fatalf("segment video index = %d, want frozen index 2",
			engine.segmentVideoIndex)
	}
}

func TestV1ServerHLSSegmentUnknownModeFailsClosed(t *testing.T) {
	capability := "ms_" + strings.Repeat("a", 22)
	store := &fakeV1HLSStore{access: v1HLSAccess("unknown")}
	engine := &fakeV1HLSEngine{probe: v1HLSProbe(60, 2)}
	h := v1HLSTestHandler(store, engine)
	rec := httptest.NewRecorder()

	h.serveSegment(rec, v1HLSSegmentRequest(capability, "0.ts"))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if got := decodeV1HLSError(t, rec); got != V1ErrorTransportDisabled {
		t.Fatalf("error code = %q, want %q", got, V1ErrorTransportDisabled)
	}
	if engine.segmentCalls != 0 {
		t.Fatal("unknown mode reached the engine")
	}
}

func TestV1ServerHLSSegmentBeyondDurationIsInvalid(t *testing.T) {
	capability := "ms_" + strings.Repeat("a", 22)
	access := v1HLSAccess("remux")
	access.Session.StartSeconds = 59
	store := &fakeV1HLSStore{access: access}
	engine := &fakeV1HLSEngine{probe: v1HLSProbe(60, 2)}
	h := v1HLSTestHandler(store, engine)
	rec := httptest.NewRecorder()

	h.serveSegment(rec, v1HLSSegmentRequest(capability, "1.ts"))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if got := decodeV1HLSError(t, rec); got != V1ErrorInvalidRequest {
		t.Fatalf("error code = %q, want %q", got, V1ErrorInvalidRequest)
	}
	if engine.segmentCalls != 0 {
		t.Fatal("out-of-range segment reached the engine")
	}
}

func TestV1ServerHLSAuthorizationErrors(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		status int
		code   V1ErrorCode
	}{
		{name: "not found", err: db.ErrPlaybackMediaSessionNotFound, status: http.StatusNotFound, code: V1ErrorMediaSessionNotFound},
		{name: "expired", err: db.ErrPlaybackMediaSessionExpired, status: http.StatusGone, code: V1ErrorMediaSessionExpired},
		{name: "revoked", err: db.ErrPlaybackMediaSessionRevoked, status: http.StatusForbidden, code: V1ErrorTransportDisabled},
		{name: "ticket expired", err: db.ErrPlaybackTicketExpired, status: http.StatusGone, code: V1ErrorTicketExpired},
		{name: "internal", err: errors.New("database detail must not leak"), status: http.StatusInternalServerError, code: V1ErrorInternal},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			capability := "ms_" + strings.Repeat("a", 22)
			store := &fakeV1HLSStore{authorizeErr: []error{tc.err}}
			engine := &fakeV1HLSEngine{probe: v1HLSProbe(30, 2)}
			h := v1HLSTestHandler(store, engine)
			rec := httptest.NewRecorder()

			h.servePlaylist(rec, v1HLSPlaylistRequest(capability))

			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tc.status, rec.Body.String())
			}
			if got := decodeV1HLSError(t, rec); got != tc.code {
				t.Fatalf("error code = %q, want %q", got, tc.code)
			}
			if strings.Contains(rec.Body.String(), "database detail") {
				t.Fatalf("error body leaked the internal error: %s", rec.Body.String())
			}
		})
	}
}

func TestV1ServerHLSRetriesAuthorizationConflicts(t *testing.T) {
	capability := "ms_" + strings.Repeat("a", 22)
	store := &fakeV1HLSStore{
		access: v1HLSAccess("remux"),
		authorizeErr: []error{
			db.ErrPlaybackMediaSessionConflict,
			db.ErrPlaybackMediaSessionConflict,
		},
	}
	engine := &fakeV1HLSEngine{probe: v1HLSProbe(30, 2)}
	h := v1HLSTestHandler(store, engine)
	h.authorizeBackoff = 0
	rec := httptest.NewRecorder()

	h.servePlaylist(rec, v1HLSPlaylistRequest(capability))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(store.authorizeLog) != 3 {
		t.Fatalf("authorize calls = %d, want 3", len(store.authorizeLog))
	}
	for _, call := range store.authorizeLog {
		if call.capability != capability {
			t.Fatalf("authorize capability = %q, want %q", call.capability, capability)
		}
	}
}

func TestV1ServerHLSAuthorizationRetryIsBounded(t *testing.T) {
	capability := "ms_" + strings.Repeat("a", 22)
	store := &fakeV1HLSStore{authorizeErr: []error{
		db.ErrPlaybackMediaSessionConflict,
		db.ErrPlaybackMediaSessionConflict,
		db.ErrPlaybackMediaSessionConflict,
		db.ErrPlaybackMediaSessionConflict,
	}}
	engine := &fakeV1HLSEngine{probe: v1HLSProbe(30, 2)}
	h := v1HLSTestHandler(store, engine)
	h.authorizeBackoff = 0
	rec := httptest.NewRecorder()

	h.servePlaylist(rec, v1HLSPlaylistRequest(capability))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
	if len(store.authorizeLog) != 3 {
		t.Fatalf("authorize calls = %d, want 3", len(store.authorizeLog))
	}
}

func TestV1ServerHLSDoesNotLogRequestSecrets(t *testing.T) {
	var logs bytes.Buffer
	oldWriter, oldFlags, oldPrefix := log.Writer(), log.Flags(), log.Prefix()
	log.SetOutput(&logs)
	log.SetFlags(0)
	log.SetPrefix("")
	t.Cleanup(func() {
		log.SetOutput(oldWriter)
		log.SetFlags(oldFlags)
		log.SetPrefix(oldPrefix)
	})

	capability := "ms_" + strings.Repeat("a", 22)
	store := &fakeV1HLSStore{
		authorizeErr: []error{errors.New("source https://private.example/path?token=query-secret")},
	}
	engine := &fakeV1HLSEngine{probe: v1HLSProbe(30, 2)}
	h := v1HLSTestHandler(store, engine)
	rec := httptest.NewRecorder()

	h.servePlaylist(rec, v1HLSPlaylistRequest(capability))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	for _, secret := range []string{capability, "private.example", "query-secret", "token="} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("logs leaked %q: %q", secret, logs.String())
		}
	}
}

func v1HLSTestHandler(store v1PlaybackHLSStore, engine v1PlaybackHLSEngine) *v1ServerHLS {
	return &v1ServerHLS{
		store:             store,
		engine:            engine,
		cfg:               v1HLSTestConfig(),
		now:               func() time.Time { return time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC) },
		authorizeAttempts: 3,
		authorizeBackoff:  0,
	}
}

func v1HLSTestConfig() *config.Config {
	return &config.Config{
		Client: config.ClientFeatures{
			V1Enabled:             true,
			DeviceAuthEnabled:     true,
			PlaybackTicketEnabled: true,
			ServerHLSV1Enabled:    true,
		},
		Users: config.Users{RequireEmail: true},
		SMTP:  config.SMTP{Host: "smtp.example.test"},
	}
}

func v1HLSAccess(mode string) *db.PlaybackMediaSessionAccess {
	height := 720
	return &db.PlaybackMediaSessionAccess{Session: &models.PlaybackMediaSession{
		FileID:          42,
		Transport:       models.PlaybackTransportServerHLS,
		State:           models.PlaybackMediaSessionActive,
		Mode:            mode,
		AudioOrdinal:    1,
		VideoIndex:      5,
		StartSeconds:    10,
		TranscodeHeight: &height,
	}}
}

func v1HLSProbe(duration float64, videoIndex int) *media.Probe {
	return &media.Probe{
		Streams: []media.ProbeStream{
			{Index: videoIndex, CodecType: "video", CodecName: "h264"},
			{Index: videoIndex + 1, CodecType: "audio", CodecName: "aac"},
		},
		Format: media.ProbeFormat{Duration: json.Number(strconv.FormatFloat(duration, 'f', -1, 64))},
	}
}

func v1HLSPlaylistRequest(capability string) *http.Request {
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/playback/media-sessions/"+capability+"/index.m3u8", nil)
	req.SetPathValue("id", capability)
	return req
}

func v1HLSSegmentRequest(capability, segment string) *http.Request {
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/playback/media-sessions/"+capability+"/"+segment, nil)
	req.SetPathValue("id", capability)
	req.SetPathValue("segment", segment)
	return req
}

func decodeV1HLSError(t *testing.T, rec *httptest.ResponseRecorder) V1ErrorCode {
	t.Helper()
	var envelope v1ErrorEnvelope
	if err := json.NewDecoder(rec.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode v1 error: %v; body=%q", err, rec.Body.String())
	}
	return envelope.Error.Code
}

func assertV1HLSCommonResponseHeaders(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if got := rec.Header().Get("Cache-Control"); got != "private, no-store" {
		t.Errorf("Cache-Control = %q, want private, no-store", got)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
}

func intPtr(v int) *int { return &v }
