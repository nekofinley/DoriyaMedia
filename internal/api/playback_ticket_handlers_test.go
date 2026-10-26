package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"doriyamedia/internal/config"
	"doriyamedia/internal/db"
	"doriyamedia/internal/media"
	"doriyamedia/internal/models"
)

const playbackAPITestProbe = `{
  "format": {"format_name":"mov,mp4,m4a,3gp,3g2,mj2","duration":"120.0"},
  "streams": [
    {"index":0,"codec_type":"video","codec_name":"h264","width":1920,"height":1080},
    {"index":1,"codec_type":"audio","codec_name":"aac","channels":2,"tags":{"language":"jpn"}}
  ]
}`

type playbackAPIFixture struct {
	server   *Server
	http     *httptest.Server
	access   string
	deviceID string
}

func newPlaybackAPIFixture(t *testing.T) playbackAPIFixture {
	t.Helper()
	srv, ts := newTestServer(t)
	srv.Cfg.Client = config.ClientFeatures{
		V1Enabled:         true,
		DeviceAuthEnabled: true,
	}
	srv.Cfg.Users.BaseURL = "https://media.example"
	srv.Engine = media.NewEngine(srv.Store, srv.Cfg)

	admin, _ := v1SetupAdmin(t, ts.URL)
	_, token := v1IssueDevice(t, ts.URL, admin, "Playback Windows")
	access := v1StringField(t, token, "access_token")
	device := v1ObjectField(t, token, "device")
	deviceID := v1StringField(t, device, "id")
	return playbackAPIFixture{
		server: srv, http: ts, access: access, deviceID: deviceID,
	}
}

func (f playbackAPIFixture) call(
	t *testing.T,
	method, path string,
	body any,
) v1HTTPResponse {
	t.Helper()
	return v1Call(t, http.DefaultClient, method, f.http.URL+path, body, map[string]string{
		"Authorization": "Bearer " + f.access,
	})
}

func seedPlaybackAPIFile(t *testing.T, store *db.Store, mediaPath, probe string) int64 {
	t.Helper()
	return seedPlaybackAPIFileWithSource(t, store, &models.Source{
		Name: "playback-api", Type: "url", Config: "{}",
	}, mediaPath, probe)
}

func seedPlaybackAPIFileWithSource(
	t *testing.T,
	store *db.Store,
	src *models.Source,
	mediaPath, probe string,
) int64 {
	t.Helper()
	if err := store.CreateSource(src); err != nil {
		t.Fatalf("create source: %v", err)
	}
	lib := &models.Library{
		Name: "playback-api", Type: "movie", SourceID: src.ID, Path: "/", Providers: "[]",
	}
	if err := store.CreateLibrary(lib); err != nil {
		t.Fatalf("create library: %v", err)
	}
	item := &models.Item{LibraryID: lib.ID, Type: "movie", Name: "Playback API"}
	if err := store.InsertItem(item); err != nil {
		t.Fatalf("insert item: %v", err)
	}
	fileID, err := store.UpsertFile(&models.MediaFile{
		ItemID: item.ID, SourceID: src.ID, Path: mediaPath, Probe: probe,
	})
	if err != nil {
		t.Fatalf("insert file: %v", err)
	}
	return fileID
}

func playbackCreateBody(fileID int64, deviceID string, transports ...string) map[string]any {
	return map[string]any{
		"file_id":   fileID,
		"device_id": deviceID,
		"client": map[string]any{
			"platform":         "windows",
			"version":          "0.1.0",
			"engine":           "mpv",
			"containers":       []string{"mp4", "mkv"},
			"video_codecs":     []string{"h264", "hevc"},
			"audio_codecs":     []string{"aac", "flac"},
			"subtitle_codecs":  []string{"text", "ass"},
			"supports_range":   true,
			"supports_headers": true,
		},
		"preferred_transports": transports,
	}
}

func TestPlaybackTicketDirectThenServerFallbacksAndSafeStatus(t *testing.T) {
	f := newPlaybackAPIFixture(t)
	logs := capturePlaybackSecurityLogs(t)
	f.server.Cfg.Client = config.ClientFeatures{
		V1Enabled:             true,
		DeviceAuthEnabled:     true,
		PlaybackTicketEnabled: true,
		DirectHTTPEnabled:     true,
		ServerProxyV1Enabled:  true,
		ServerHLSV1Enabled:    true,
	}
	fileID := seedPlaybackAPIFile(
		t, f.server.Store,
		"https://cdn.example.test/library/video.mp4?signature=source-secret",
		playbackAPITestProbe,
	)
	create := f.call(t, http.MethodPost, "/api/v1/playback/tickets",
		playbackCreateBody(fileID, f.deviceID,
			"direct-http", "server-proxy", "server-hls"))
	if create.Status != http.StatusCreated {
		t.Fatalf("create status = %d, want 201; body %s", create.Status, create.Body)
	}
	if got := create.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("create Cache-Control = %q", got)
	}
	location := create.Header.Get("Location")
	if !strings.HasPrefix(location, "/api/v1/playback/tickets/pt_") {
		t.Fatalf("Location = %q", location)
	}
	if create.JSON["state"] != models.PlaybackTicketConsuming {
		t.Fatalf("create state = %v", create.JSON["state"])
	}
	transport := v1ObjectField(t, create.JSON, "transport")
	if transport["kind"] != models.PlaybackTransportDirectHTTP {
		t.Fatalf("transport = %v", transport)
	}
	if _, ok := transport["media_session"]; ok {
		t.Fatal("direct transport unexpectedly contains a media session")
	}
	streams, ok := transport["streams"].([]any)
	if !ok || len(streams) != 1 {
		t.Fatalf("streams = %#v", transport["streams"])
	}
	stream, _ := streams[0].(map[string]any)
	if stream["url"] != "https://cdn.example.test/library/video.mp4?signature=source-secret" {
		t.Fatalf("direct stream = %v", stream)
	}
	if remaining := create.JSON["remaining_transports"].([]any); len(remaining) != 2 ||
		remaining[0] != models.PlaybackTransportServerProxy ||
		remaining[1] != models.PlaybackTransportServerHLS {
		t.Fatalf("remaining transports = %v", remaining)
	}

	engine := f.server.Engine
	f.server.Engine = nil
	status := f.call(t, http.MethodGet, location, nil)
	f.server.Engine = engine
	if status.Status != http.StatusOK || status.JSON["state"] != models.PlaybackTicketConsuming {
		t.Fatalf("status = %d body %s", status.Status, status.Body)
	}
	if _, ok := status.JSON["transport"]; ok {
		t.Fatal("GET status exposed a one-time transport")
	}
	if bytes.Contains(status.Body, []byte("source-secret")) ||
		bytes.Contains(status.Body, []byte("media-sessions/ms_")) ||
		bytes.Contains(status.Body, []byte("video_index")) {
		t.Fatalf("GET status leaked issuance data: %s", status.Body)
	}

	refreshProxy := f.call(t, http.MethodPost, location+"/refresh", nil)
	if refreshProxy.Status != http.StatusOK {
		t.Fatalf("proxy refresh = %d body %s", refreshProxy.Status, refreshProxy.Body)
	}
	proxyTransport := v1ObjectField(t, refreshProxy.JSON, "transport")
	if proxyTransport["kind"] != models.PlaybackTransportServerProxy {
		t.Fatalf("proxy transport = %v", proxyTransport)
	}
	proxyStream := proxyTransport["streams"].([]any)[0].(map[string]any)
	proxyURL, _ := proxyStream["url"].(string)
	if !strings.HasPrefix(proxyURL, "https://media.example/api/v1/playback/media-sessions/ms_") ||
		!strings.HasSuffix(proxyURL, "/stream") {
		t.Fatalf("proxy URL = %q", proxyURL)
	}
	if bytes.Contains(refreshProxy.Body, []byte("source-secret")) ||
		bytes.Contains(refreshProxy.Body, []byte("cdn.example.test")) {
		t.Fatalf("server-proxy issuance leaked source data: %s", refreshProxy.Body)
	}

	refreshHLS := f.call(t, http.MethodPost, location+"/refresh", nil)
	if refreshHLS.Status != http.StatusOK {
		t.Fatalf("hls refresh = %d body %s", refreshHLS.Status, refreshHLS.Body)
	}
	hlsTransport := v1ObjectField(t, refreshHLS.JSON, "transport")
	if hlsTransport["kind"] != models.PlaybackTransportServerHLS {
		t.Fatalf("hls transport = %v", hlsTransport)
	}
	hlsStream := hlsTransport["streams"].([]any)[0].(map[string]any)
	if !strings.HasSuffix(hlsStream["url"].(string), "/index.m3u8") {
		t.Fatalf("hls stream = %v", hlsStream)
	}

	exhausted := f.call(t, http.MethodPost, location+"/refresh", nil)
	v1AssertError(t, exhausted, http.StatusGone, V1ErrorTicketExpired)
	playbackLifecycleAssertErrorAction(t, exhausted, "request_new_ticket")

	output := logs.String()
	for _, want := range []string{
		"event=transport_issued transport=direct-http",
		"request_issuance_fallback_count=0",
		"ticket_attempt_ordinal=0 last_claimed_transport=none",
		"event=transport_issued transport=server-proxy",
		"ticket_attempt_ordinal=1 last_claimed_transport=direct-http",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("playback issuance telemetry omits %q: %q", want, output)
		}
	}
}

func TestPlaybackServerProxyStreamUsesCapabilityOnly(t *testing.T) {
	payload := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		http.ServeContent(w, r, "clip.mp4", time.Unix(1_700_000_000, 0), bytes.NewReader(payload))
	}))
	t.Cleanup(upstream.Close)

	f := newPlaybackAPIFixture(t)
	f.server.Cfg.Client = config.ClientFeatures{
		V1Enabled:             true,
		DeviceAuthEnabled:     true,
		PlaybackTicketEnabled: true,
		ServerProxyV1Enabled:  true,
	}
	sourceConfig, err := json.Marshal(models.SourceConfig{WebDAVURL: upstream.URL})
	if err != nil {
		t.Fatalf("marshal source config: %v", err)
	}
	// A relative path backed by a configured WebDAV origin exercises the
	// administrator-configured private-source exception without granting one to
	// an arbitrary URLDriver.
	fileID := seedPlaybackAPIFileWithSource(t, f.server.Store,
		&models.Source{Name: "playback-api-private", Type: "webdav", Config: string(sourceConfig)},
		"/clip.mp4", playbackAPITestProbe)
	create := f.call(t, http.MethodPost, "/api/v1/playback/tickets",
		playbackCreateBody(fileID, f.deviceID, "server-proxy"))
	if create.Status != http.StatusCreated {
		t.Fatalf("create = %d body %s", create.Status, create.Body)
	}
	transport := v1ObjectField(t, create.JSON, "transport")
	stream := transport["streams"].([]any)[0].(map[string]any)
	mediaURL := stream["url"].(string)
	// The fixture advertises the production HTTPS origin, while the test
	// listener is local HTTP. Keep the capability-addressed path intact.
	mediaURL = f.http.URL + strings.TrimPrefix(mediaURL, "https://media.example")

	req, err := http.NewRequest(http.MethodGet, mediaURL, nil)
	if err != nil {
		t.Fatalf("new media request: %v", err)
	}
	req.Header.Set("Range", "bytes=2-5")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("media GET: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusPartialContent || string(raw) != "2345" {
		t.Fatalf("media response = %d %q", resp.StatusCode, raw)
	}
	if got := resp.Header.Get("Content-Range"); got != "bytes 2-5/36" {
		t.Errorf("Content-Range = %q", got)
	}
	if got := resp.Header.Get("Cache-Control"); got != v1ProxyCacheControl {
		t.Errorf("Cache-Control = %q", got)
	}
	if bytes.Contains(raw, []byte("upstream-secret")) {
		t.Fatal("proxy response leaked the source URL")
	}

	badQuery := v1Call(t, http.DefaultClient, http.MethodGet, mediaURL+"?signature=forged", nil, nil)
	v1AssertError(t, badQuery, http.StatusBadRequest, V1ErrorInvalidRequest)
}

func TestPlaybackTicketRejectsDeviceMismatchAndUnknownSourceFields(t *testing.T) {
	f := newPlaybackAPIFixture(t)
	f.server.Cfg.Client = config.ClientFeatures{
		V1Enabled:             true,
		DeviceAuthEnabled:     true,
		PlaybackTicketEnabled: true,
		DirectHTTPEnabled:     true,
	}
	fileID := seedPlaybackAPIFile(t, f.server.Store,
		"https://cdn.example.test/video.mp4", playbackAPITestProbe)

	body := playbackCreateBody(fileID, "dev_someone_else_000001", "direct-http")
	resp := f.call(t, http.MethodPost, "/api/v1/playback/tickets", body)
	v1AssertError(t, resp, http.StatusForbidden, V1ErrorForbidden)

	body = playbackCreateBody(fileID, f.deviceID, "direct-http")
	body["source_url"] = "https://attacker.example/secret"
	resp = f.call(t, http.MethodPost, "/api/v1/playback/tickets", body)
	v1AssertError(t, resp, http.StatusBadRequest, V1ErrorInvalidRequest)

	body = playbackCreateBody(fileID, f.deviceID, "direct-http")
	body["features"] = map[string]any{"client_v1_enabled": true}
	resp = f.call(t, http.MethodPost, "/api/v1/playback/tickets", body)
	v1AssertError(t, resp, http.StatusBadRequest, V1ErrorInvalidRequest)
}

func TestLogMiddlewareRedactsPlaybackMediaCapability(t *testing.T) {
	capability := "ms_" + strings.Repeat("a", 43)
	var output bytes.Buffer
	previousWriter := log.Writer()
	previousFlags := log.Flags()
	log.SetOutput(&output)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
	})

	handler := logMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/playback/media-sessions/"+capability+"/stream", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if strings.Contains(output.String(), capability) {
		t.Fatalf("log contains media capability: %s", output.String())
	}
	if !strings.Contains(output.String(), "ms_redacted/stream") {
		t.Fatalf("log did not preserve the redacted route: %s", output.String())
	}
}

func TestLogMiddlewareRedactsPlaybackTicketID(t *testing.T) {
	ticketID := "pt_" + strings.Repeat("b", 43)
	var output bytes.Buffer
	previousWriter := log.Writer()
	previousFlags := log.Flags()
	log.SetOutput(&output)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
	})

	handler := logMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, path := range []string{
		"/api/v1/playback/tickets/" + ticketID,
		"/api/v1/playback/tickets/" + ticketID + "/refresh",
	} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
	}

	if strings.Contains(output.String(), ticketID) {
		t.Fatalf("log contains playback ticket id: %s", output.String())
	}
	for _, route := range []string{"pt_redacted", "pt_redacted/refresh"} {
		if !strings.Contains(output.String(), route) {
			t.Errorf("log did not preserve redacted route %q: %s", route, output.String())
		}
	}
}

func TestPlaybackClientCapabilitiesRejectMalformedPayload(t *testing.T) {
	var client v1PlaybackClientCapabilities
	if err := json.Unmarshal([]byte(`{
		"platform":"windows","version":"0.1.0","engine":"mpv",
		"containers":["mp4"],"video_codecs":["h264"],"audio_codecs":["aac"],
		"subtitle_codecs":["text"],"supports_range":true,"supports_headers":true
	}`), &client); err != nil {
		t.Fatalf("decode client: %v", err)
	}
	client.Containers = append(client.Containers, "mp4")
	if _, err := normalizeV1PlaybackClient(client); err == nil {
		t.Fatal("duplicate container was accepted")
	}
}
