package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"doriyamedia/internal/config"
	"doriyamedia/internal/db"
	"doriyamedia/internal/media"
	"doriyamedia/internal/models"
	"doriyamedia/internal/source"
)

const playbackSubtitleProbe = `{
  "format": {"format_name":"matroska,webm","duration":"120.0"},
  "streams": [
    {"index":0,"codec_type":"video","codec_name":"h264","width":1920,"height":1080},
    {"index":1,"codec_type":"audio","codec_name":"aac","channels":2,"tags":{"language":"jpn"}},
    {"index":2,"codec_type":"subtitle","codec_name":"hdmv_pgs_subtitle",
     "tags":{"language":"eng","title":"English PGS"}}
  ]
}`

const playbackSubtitleSRT = `1
00:00:07,000 --> 00:00:09,000
First line

2
00:00:12,000 --> 00:00:14,000
Second line
`

const playbackSubtitleASS = `[Script Info]
ScriptType: v4.00+

[V4+ Styles]
Format: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, OutlineColour, BackColour, Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, Spacing, Angle, BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, MarginV, Encoding
Style: Default,Arial,20,&H00FFFFFF,&H000000FF,&H00000000,&H00000000,0,0,0,0,100,100,0,0,1,1,0,2,10,10,10,1

[Events]
Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text
Dialogue: 0,0:00:10.00,0:00:12.00,Default,,0,0,0,,ASS line
`

func seedPlaybackSubtitleLocalFile(
	t *testing.T,
	store *db.Store,
	probe string,
	subtitleFiles map[string]string,
) (int64, string) {
	t.Helper()
	root := t.TempDir()
	mediaDir := filepath.Join(root, "media")
	if err := os.MkdirAll(mediaDir, 0o755); err != nil {
		t.Fatalf("create subtitle media directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(mediaDir, "episode.mkv"), []byte("video"), 0o644); err != nil {
		t.Fatalf("write playback media: %v", err)
	}
	for name, body := range subtitleFiles {
		if err := os.WriteFile(filepath.Join(mediaDir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write subtitle %s: %v", name, err)
		}
	}
	configJSON, err := json.Marshal(models.SourceConfig{Root: root})
	if err != nil {
		t.Fatalf("marshal local source config: %v", err)
	}
	src := &models.Source{Name: "subtitle-local", Type: "local", Config: string(configJSON)}
	if err := store.CreateSource(src); err != nil {
		t.Fatalf("create local subtitle source: %v", err)
	}
	lib := &models.Library{
		Name: "subtitle-local", Type: "movie", SourceID: src.ID,
		Path: "/media", Providers: "[]",
	}
	if err := store.CreateLibrary(lib); err != nil {
		t.Fatalf("create subtitle library: %v", err)
	}
	item := &models.Item{LibraryID: lib.ID, Type: "movie", Name: "Subtitle fixture"}
	if err := store.InsertItem(item); err != nil {
		t.Fatalf("create subtitle item: %v", err)
	}
	fileID, err := store.UpsertFile(&models.MediaFile{
		ItemID: item.ID, SourceID: src.ID, Path: "/media/episode.mkv",
		Probe: probe,
	})
	if err != nil {
		t.Fatalf("create subtitle media file: %v", err)
	}
	return fileID, root
}

func playbackSubtitleObjects(
	t *testing.T,
	transport map[string]any,
) []map[string]any {
	t.Helper()
	raw, exists := transport["subtitles"]
	if !exists {
		return nil
	}
	values, ok := raw.([]any)
	if !ok {
		t.Fatalf("transport subtitles = %#v", raw)
	}
	out := make([]map[string]any, 0, len(values))
	for _, value := range values {
		subtitle, ok := value.(map[string]any)
		if !ok {
			t.Fatalf("subtitle entry = %#v", value)
		}
		out = append(out, subtitle)
	}
	return out
}

func playbackSubtitleTestURL(
	t *testing.T,
	f playbackAPIFixture,
	rawURL string,
) string {
	t.Helper()
	target, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse subtitle URL: %v", err)
	}
	base, err := url.Parse(f.http.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	target.Scheme = base.Scheme
	target.Host = base.Host
	return target.String()
}

func playbackSubtitleRawRequest(
	t *testing.T,
	method, target string,
) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, target, nil)
	if err != nil {
		t.Fatalf("new subtitle request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s subtitle: %v", method, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read subtitle response: %v", err)
	}
	return resp, body
}

func playbackSubtitleTicketBinding(
	t *testing.T,
	store *db.Store,
	ticketID string,
) models.PlaybackTicketBinding {
	t.Helper()
	binding := models.PlaybackTicketBinding{TicketID: ticketID}
	if err := store.DB.QueryRow(`SELECT user_id, device_id, file_id
		FROM playback_tickets WHERE ticket_id = ?`, ticketID).Scan(
		&binding.UserID, &binding.DeviceID, &binding.FileID,
	); err != nil {
		t.Fatalf("read subtitle ticket binding: %v", err)
	}
	return binding
}

func playbackSubtitleCapabilityFromURL(
	t *testing.T,
	rawURL, marker string,
) string {
	t.Helper()
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse capability URL: %v", err)
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	for i, part := range parts {
		if part == marker && i+1 < len(parts) {
			return strings.TrimSuffix(parts[i+1], ".vtt")
		}
	}
	t.Fatalf("capability marker %q not found in %q", marker, rawURL)
	return ""
}

func hashPlaybackCapabilityForTest(capability string) string {
	sum := sha256.Sum256([]byte(capability))
	return hex.EncodeToString(sum[:])
}

func TestPlaybackSubtitleIssuanceConvertsTextAndKeepsPGSDescriptive(t *testing.T) {
	f := newPlaybackAPIFixture(t)
	f.server.Cfg.Client = config.ClientFeatures{
		V1Enabled:             true,
		DeviceAuthEnabled:     true,
		PlaybackTicketEnabled: true,
		ServerProxyV1Enabled:  true,
	}
	fileID, sourceRoot := seedPlaybackSubtitleLocalFile(t, f.server.Store,
		playbackSubtitleProbe, map[string]string{
			"episode.srt": playbackSubtitleSRT,
			"episode.ass": playbackSubtitleASS,
		})
	create := f.call(t, http.MethodPost, "/api/v1/playback/tickets",
		playbackCreateBody(fileID, f.deviceID, "server-proxy"))
	if create.Status != http.StatusCreated {
		t.Fatalf("create subtitle ticket = %d body %s", create.Status, create.Body)
	}
	ticketID := strings.TrimPrefix(create.Header.Get("Location"),
		"/api/v1/playback/tickets/")
	if !strings.HasPrefix(ticketID, "pt_") {
		t.Fatalf("ticket id = %q", ticketID)
	}
	status := f.call(t, http.MethodGet,
		"/api/v1/playback/tickets/"+ticketID, nil)
	if status.Status != http.StatusOK {
		t.Fatalf("safe ticket status = %d body %s", status.Status, status.Body)
	}
	for _, forbidden := range []string{
		"transport", "subtitles", "url", "media_session", "capability",
	} {
		if _, exists := status.JSON[forbidden]; exists {
			t.Errorf("safe ticket status exposed %q: %s", forbidden, status.Body)
		}
	}
	for _, secret := range []string{"ss_", "ms_", sourceRoot} {
		if bytes.Contains(status.Body, []byte(secret)) {
			t.Errorf("safe ticket status leaked %q: %s", secret, status.Body)
		}
	}
	transport := v1ObjectField(t, create.JSON, "transport")
	subtitles := playbackSubtitleObjects(t, transport)
	if len(subtitles) != 3 {
		t.Fatalf("issued subtitles = %d, want srt+ass+pgs: %#v", len(subtitles), subtitles)
	}

	seenFormats := map[string]bool{}
	textURLs := make([]string, 0, 2)
	for _, subtitle := range subtitles {
		id, _ := subtitle["id"].(string)
		format, _ := subtitle["format"].(string)
		label, _ := subtitle["label"].(string)
		language, _ := subtitle["language"].(string)
		if !strings.HasPrefix(id, "sub_") || label == "" || language == "" {
			t.Errorf("subtitle description is incomplete: %#v", subtitle)
		}
		seenFormats[format] = true
		rawURL, _ := subtitle["url"].(string)
		switch format {
		case "pgs":
			if rawURL != "" {
				t.Errorf("PGS subtitle unexpectedly has URL %q", rawURL)
			}
		case "vtt":
			if !strings.Contains(rawURL,
				"/api/v1/playback/subtitle-sessions/ss_") ||
				!strings.HasSuffix(rawURL, "/subtitle.vtt") ||
				strings.Contains(rawURL, "?") {
				t.Errorf("text subtitle URL = %q", rawURL)
			}
			textURLs = append(textURLs, rawURL)
		default:
			t.Errorf("unexpected subtitle format %q", format)
		}
	}
	if !seenFormats["vtt"] || !seenFormats["pgs"] {
		t.Fatalf("formats = %v", seenFormats)
	}
	if bytes.Contains(create.Body, []byte(sourceRoot)) ||
		bytes.Contains(create.Body, []byte("/media/episode")) {
		t.Fatalf("subtitle issuance leaked a source path: %s", create.Body)
	}

	subtitleRows, err := f.server.Store.SubtitlesByFile(fileID)
	if err != nil {
		t.Fatalf("read subtitle rows: %v", err)
	}
	if len(subtitleRows) != 3 {
		t.Fatalf("snapshotted subtitle rows = %d, want 3", len(subtitleRows))
	}
	var snapshotJSON string
	if err := f.server.Store.DB.QueryRow(`SELECT subtitle_ids_json
		FROM playback_tickets WHERE ticket_id = ?`, ticketID).Scan(&snapshotJSON); err != nil {
		t.Fatalf("read subtitle grant snapshot: %v", err)
	}
	var snapshot []int64
	if err := json.Unmarshal([]byte(snapshotJSON), &snapshot); err != nil {
		t.Fatalf("decode subtitle grant snapshot: %v", err)
	}
	if len(snapshot) != len(subtitleRows) {
		t.Fatalf("subtitle snapshot = %v, rows = %v", snapshot, subtitleRows)
	}

	for i, rawURL := range textURLs {
		target := playbackSubtitleTestURL(t, f, rawURL)
		resp, body := playbackSubtitleRawRequest(t, http.MethodGet, target)
		if resp.StatusCode != http.StatusOK ||
			!bytes.HasPrefix(body, []byte("WEBVTT")) {
			t.Fatalf("text subtitle %d GET = %d %q", i, resp.StatusCode, body)
		}
		if got := resp.Header.Get("Content-Type"); got != "text/vtt; charset=utf-8" {
			t.Errorf("text subtitle Content-Type = %q", got)
		}
		if bytes.Contains(body, []byte(sourceRoot)) ||
			bytes.Contains(body, []byte("source-query-secret")) {
			t.Fatalf("text subtitle leaked source data: %q", body)
		}

		headResp, headBody := playbackSubtitleRawRequest(t, http.MethodHead, target)
		if headResp.StatusCode != http.StatusOK || len(headBody) != 0 {
			t.Fatalf("text subtitle HEAD = %d body %q", headResp.StatusCode, headBody)
		}
		if headResp.ContentLength != int64(len(body)) {
			t.Errorf("HEAD Content-Length = %d, GET body = %d",
				headResp.ContentLength, len(body))
		}

		tampered := target + "?start=99&capability=other&secret=source-query-secret"
		tamperedResp, tamperedBody := playbackSubtitleRawRequest(
			t, http.MethodGet, tampered)
		if tamperedResp.StatusCode != http.StatusBadRequest {
			t.Fatalf("tampered subtitle query = %d %s",
				tamperedResp.StatusCode, tamperedBody)
		}
		if bytes.Contains(tamperedBody, []byte("source-query-secret")) ||
			bytes.Contains(tamperedBody, []byte(sourceRoot)) {
			t.Fatalf("query rejection leaked source data: %s", tamperedBody)
		}
	}

	// The timeline origin is persisted in the subtitle session, not accepted
	// from query parameters. A separately issued session starts at 5 seconds.
	binding := playbackSubtitleTicketBinding(t, f.server.Store, ticketID)
	var srtID int64
	for _, subtitle := range subtitleRows {
		if subtitle.Format == "subrip" {
			srtID = subtitle.ID
			break
		}
	}
	if srtID == 0 {
		t.Fatal("SRT subtitle id was not discovered")
	}
	shifted, err := f.server.Store.CreatePlaybackSubtitleSession(
		models.PlaybackSubtitleSessionRequest{
			Ticket: binding, Transport: models.PlaybackTransportServerProxy,
			SubtitleID: srtID, StartSeconds: 5,
		},
		time.Now().UTC(),
	)
	if err != nil {
		t.Fatalf("create shifted subtitle session: %v", err)
	}
	shiftedURL := v1PlaybackSubtitleURL(
		"https://media.example", "", models.PlaybackTransportServerProxy,
		shifted.Capability)
	shiftedResp, shiftedBody := playbackSubtitleRawRequest(
		t, http.MethodGet, playbackSubtitleTestURL(t, f, shiftedURL))
	if shiftedResp.StatusCode != http.StatusOK {
		t.Fatalf("shifted subtitle = %d %s", shiftedResp.StatusCode, shiftedBody)
	}
	if !bytes.Contains(shiftedBody,
		[]byte("00:00:02.000 --> 00:00:04.000")) {
		t.Fatalf("session start was not applied: %s", shiftedBody)
	}

	forceQuery := httptest.NewRequest(http.MethodGet,
		"/api/v1/playback/subtitle-sessions/"+shifted.Capability+"/subtitle.vtt?", nil)
	forceQuery.SetPathValue("id", shifted.Capability)
	rec := httptest.NewRecorder()
	f.server.handleV1PlaybackSubtitle(rec, forceQuery)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("ForceQuery status = %d body %s", rec.Code, rec.Body.String())
	}
}

func TestPlaybackSubtitleCapabilityFailsClosedAfterDeviceAndTicketRevocation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		revoke func(*testing.T, playbackAPIFixture, string)
	}{
		{
			name: "device",
			revoke: func(t *testing.T, f playbackAPIFixture, _ string) {
				t.Helper()
				response := f.call(t, http.MethodDelete,
					"/api/v1/devices/"+f.deviceID, nil)
				if response.Status != http.StatusOK &&
					response.Status != http.StatusNoContent {
					t.Fatalf("revoke device = %d body %s", response.Status, response.Body)
				}
			},
		},
		{
			name: "ticket",
			revoke: func(t *testing.T, f playbackAPIFixture, ticketID string) {
				t.Helper()
				binding := playbackSubtitleTicketBinding(t, f.server.Store, ticketID)
				ticket, err := f.server.Store.PlaybackTicketByID(
					binding, time.Now().UTC())
				if err != nil {
					t.Fatalf("load ticket for revocation: %v", err)
				}
				if _, err := f.server.Store.RevokePlaybackTicket(
					binding, ticket.Version, time.Now().UTC()); err != nil {
					t.Fatalf("revoke playback ticket: %v", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPlaybackAPIFixture(t)
			f.server.Cfg.Client = config.ClientFeatures{
				V1Enabled:             true,
				DeviceAuthEnabled:     true,
				PlaybackTicketEnabled: true,
				ServerProxyV1Enabled:  true,
			}
			fileID, _ := seedPlaybackSubtitleLocalFile(t, f.server.Store,
				playbackSubtitleProbe, map[string]string{"episode.srt": playbackSubtitleSRT})
			create := f.call(t, http.MethodPost, "/api/v1/playback/tickets",
				playbackCreateBody(fileID, f.deviceID, "server-proxy"))
			if create.Status != http.StatusCreated {
				t.Fatalf("create = %d body %s", create.Status, create.Body)
			}
			ticketID := strings.TrimPrefix(create.Header.Get("Location"),
				"/api/v1/playback/tickets/")
			transport := v1ObjectField(t, create.JSON, "transport")
			subtitles := playbackSubtitleObjects(t, transport)
			var subtitleURL string
			for _, subtitle := range subtitles {
				if subtitle["format"] == "vtt" {
					subtitleURL, _ = subtitle["url"].(string)
					break
				}
			}
			if subtitleURL == "" {
				t.Fatal("issued transport has no text subtitle URL")
			}
			target := playbackSubtitleTestURL(t, f, subtitleURL)

			tc.revoke(t, f, ticketID)
			resp, body := playbackSubtitleRawRequest(t, http.MethodGet, target)
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("revoked subtitle status = %d body %s", resp.StatusCode, body)
			}
			decoded := map[string]any{}
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Fatalf("decode revoked subtitle error: %v", err)
			}
			errorObject := v1ObjectField(t, decoded, "error")
			if errorObject["code"] != string(V1ErrorTransportDisabled) {
				t.Fatalf("revoked subtitle error code = %v", errorObject["code"])
			}
		})
	}
}

func TestPlaybackSubtitleExpiredSessionAndUnknownCapability(t *testing.T) {
	f := newPlaybackAPIFixture(t)
	f.server.Cfg.Client = config.ClientFeatures{
		V1Enabled:             true,
		DeviceAuthEnabled:     true,
		PlaybackTicketEnabled: true,
		ServerProxyV1Enabled:  true,
	}
	fileID, _ := seedPlaybackSubtitleLocalFile(t, f.server.Store,
		playbackSubtitleProbe, map[string]string{"episode.srt": playbackSubtitleSRT})
	create := f.call(t, http.MethodPost, "/api/v1/playback/tickets",
		playbackCreateBody(fileID, f.deviceID, "server-proxy"))
	if create.Status != http.StatusCreated {
		t.Fatalf("create = %d body %s", create.Status, create.Body)
	}
	transport := v1ObjectField(t, create.JSON, "transport")
	var subtitleURL string
	for _, subtitle := range playbackSubtitleObjects(t, transport) {
		if subtitle["format"] == "vtt" {
			subtitleURL, _ = subtitle["url"].(string)
			break
		}
	}
	target := playbackSubtitleTestURL(t, f, subtitleURL)
	capability := playbackSubtitleCapabilityFromURL(t, target, "subtitle-sessions")
	if _, err := f.server.Store.DB.Exec(`UPDATE playback_subtitle_sessions
		SET idle_expires_at = ?, absolute_expires_at = absolute_expires_at,
			updated_at = ?
		WHERE capability_hash = ?`,
		time.Now().UTC().Add(-time.Minute),
		time.Now().UTC().Add(-time.Minute),
		hashPlaybackCapabilityForTest(capability),
	); err != nil {
		t.Fatalf("expire subtitle session: %v", err)
	}
	resp, body := playbackSubtitleRawRequest(t, http.MethodGet, target)
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("expired subtitle status = %d body %s", resp.StatusCode, body)
	}

	unknown := "ss_" + strings.Repeat("z", 43)
	unknownURL := "https://media.example/api/v1/playback/subtitle-sessions/" +
		unknown + "/subtitle.vtt"
	resp, body = playbackSubtitleRawRequest(
		t, http.MethodGet, playbackSubtitleTestURL(t, f, unknownURL))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown subtitle status = %d body %s", resp.StatusCode, body)
	}
}

func TestPlaybackServerHLSSubtitleRenditionRequiresMatchingCapabilities(t *testing.T) {
	f := newPlaybackAPIFixture(t)
	f.server.Cfg.Client = config.ClientFeatures{
		V1Enabled:             true,
		DeviceAuthEnabled:     true,
		PlaybackTicketEnabled: true,
		ServerHLSV1Enabled:    true,
	}
	fileID, _ := seedPlaybackSubtitleLocalFile(t, f.server.Store,
		playbackSubtitleProbe, map[string]string{"episode.srt": playbackSubtitleSRT})
	create := f.call(t, http.MethodPost, "/api/v1/playback/tickets",
		playbackCreateBody(fileID, f.deviceID, "server-hls"))
	if create.Status != http.StatusCreated {
		t.Fatalf("create HLS ticket = %d body %s", create.Status, create.Body)
	}
	transport := v1ObjectField(t, create.JSON, "transport")
	subtitles := playbackSubtitleObjects(t, transport)
	if len(subtitles) != 1 {
		t.Fatalf("HLS subtitle descriptions = %#v, PGS should be omitted", subtitles)
	}
	subtitle := subtitles[0]
	rawSubtitleURL, _ := subtitle["url"].(string)
	if subtitle["format"] != "vtt" ||
		!strings.Contains(rawSubtitleURL,
			"/api/v1/playback/media-sessions/ms_") ||
		!strings.Contains(rawSubtitleURL, "/subtitle-sessions/ss_") {
		t.Fatalf("HLS subtitle rendition = %#v", subtitle)
	}
	target := playbackSubtitleTestURL(t, f, rawSubtitleURL)
	resp, body := playbackSubtitleRawRequest(t, http.MethodGet, target)
	if resp.StatusCode != http.StatusOK || !bytes.HasPrefix(body, []byte("WEBVTT")) {
		t.Fatalf("HLS rendition = %d %q", resp.StatusCode, body)
	}

	headResp, headBody := playbackSubtitleRawRequest(t, http.MethodHead, target)
	if headResp.StatusCode != http.StatusOK || len(headBody) != 0 {
		t.Fatalf("HLS rendition HEAD = %d body %q", headResp.StatusCode, headBody)
	}
	tamperedResp, tamperedBody := playbackSubtitleRawRequest(
		t, http.MethodGet, target+"?burn=1&start=30")
	if tamperedResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("HLS query tamper = %d %s", tamperedResp.StatusCode, tamperedBody)
	}

	second := f.call(t, http.MethodPost, "/api/v1/playback/tickets",
		playbackCreateBody(fileID, f.deviceID, "server-hls"))
	if second.Status != http.StatusCreated {
		t.Fatalf("create second HLS ticket = %d body %s", second.Status, second.Body)
	}
	secondTransport := v1ObjectField(t, second.JSON, "transport")
	secondSubURL, _ := playbackSubtitleObjects(t, secondTransport)[0]["url"].(string)
	firstParts := strings.SplitN(target, "/subtitle-sessions/", 2)
	secondParts := strings.SplitN(playbackSubtitleTestURL(t, f, secondSubURL),
		"/subtitle-sessions/", 2)
	if len(firstParts) != 2 || len(secondParts) != 2 {
		t.Fatalf("unexpected HLS subtitle paths: %q %q", target, secondSubURL)
	}
	crossTicketURL := secondParts[0] + "/subtitle-sessions/" + firstParts[1]
	resp, body = playbackSubtitleRawRequest(t, http.MethodGet, crossTicketURL)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-ticket HLS rendition = %d body %s", resp.StatusCode, body)
	}

	ticketID := strings.TrimPrefix(create.Header.Get("Location"),
		"/api/v1/playback/tickets/")
	binding := playbackSubtitleTicketBinding(t, f.server.Store, ticketID)
	mediaCapability := playbackSubtitleCapabilityFromURL(
		t, target, "media-sessions")
	mediaSession, err := f.server.Store.PlaybackMediaSessionByCapability(
		models.PlaybackMediaSessionBinding{
			Ticket: binding, Transport: models.PlaybackTransportServerHLS,
		},
		mediaCapability, time.Now().UTC())
	if err != nil {
		t.Fatalf("load HLS media session: %v", err)
	}
	if _, err := f.server.Store.RevokePlaybackMediaSession(
		models.PlaybackMediaSessionBinding{
			Ticket: binding, Transport: models.PlaybackTransportServerHLS,
		},
		mediaCapability, mediaSession.Version, time.Now().UTC()); err != nil {
		t.Fatalf("revoke HLS media session: %v", err)
	}
	resp, body = playbackSubtitleRawRequest(t, http.MethodGet, target)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("revoked HLS media rendition = %d body %s", resp.StatusCode, body)
	}
}

func TestPlaybackSubtitleConversionFromDriverSource(t *testing.T) {
	driver := &playbackSubtitleSourceDriver{
		body: playbackSubtitleSRT,
	}
	server := &Server{Engine: &media.Engine{}}
	for _, tc := range []struct {
		name   string
		format string
		body   string
		text   string
	}{
		{name: "srt", format: "subrip", body: playbackSubtitleSRT, text: "First line"},
		{name: "ass", format: "ass", body: playbackSubtitleASS, text: "ASS line"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			driver.body = tc.body
			data, ok := server.loadV1PlaybackSubtitleVTT(
				context.Background(),
				&models.MediaFile{ID: 42, Path: "/media/episode.mkv"},
				driver,
				&models.Subtitle{
					ID: 9, FileID: 42, Path: "driver:" + tc.name,
					Format: tc.format, Convertible: true,
				},
			)
			if !ok || !bytes.HasPrefix(data, []byte("WEBVTT")) ||
				!bytes.Contains(data, []byte(tc.text)) {
				t.Fatalf("%s conversion = %q", tc.name, data)
			}
		})
	}
}

func TestPlaybackSubtitleLanguageAlwaysFitsSchemaBound(t *testing.T) {
	longValidPattern := "zh-" +
		"abcdefgh-abcdefgh-abcdefgh-abcdefgh-abcdefgh"
	if len(longValidPattern) <= 32 ||
		!v1PlaybackSubtitleLanguagePattern.MatchString(longValidPattern) {
		t.Fatalf("test language does not exercise the length boundary: %q", longValidPattern)
	}
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{name: "valid", raw: "zh-Hant", want: "zh-Hant"},
		{name: "too long", raw: longValidPattern, want: "und"},
		{name: "invalid", raw: "not a language", want: "und"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := v1PlaybackSubtitleLanguage(tc.raw)
			if got != tc.want {
				t.Fatalf("v1PlaybackSubtitleLanguage(%q) = %q, want %q",
					tc.raw, got, tc.want)
			}
			if len(got) > 32 {
				t.Fatalf("language %q exceeds the schema limit", got)
			}
		})
	}
}

func TestPlaybackSubtitleCapabilityFailsClosedWhenFeatureDisabled(t *testing.T) {
	for _, tc := range []struct {
		name    string
		disable func(*config.Config)
		status  int
		code    V1ErrorCode
	}{
		{
			name: "playback tickets",
			disable: func(cfg *config.Config) {
				cfg.Client.PlaybackTicketEnabled = false
			},
			status: http.StatusNotFound,
			code:   V1ErrorFeatureDisabled,
		},
		{
			name: "server proxy transport",
			disable: func(cfg *config.Config) {
				cfg.Client.ServerProxyV1Enabled = false
			},
			status: http.StatusForbidden,
			code:   V1ErrorTransportDisabled,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPlaybackAPIFixture(t)
			f.server.Cfg.Client = config.ClientFeatures{
				V1Enabled:             true,
				DeviceAuthEnabled:     true,
				PlaybackTicketEnabled: true,
				ServerProxyV1Enabled:  true,
			}
			fileID, _ := seedPlaybackSubtitleLocalFile(t, f.server.Store,
				playbackSubtitleProbe, map[string]string{"episode.srt": playbackSubtitleSRT})
			create := f.call(t, http.MethodPost, "/api/v1/playback/tickets",
				playbackCreateBody(fileID, f.deviceID, "server-proxy"))
			if create.Status != http.StatusCreated {
				t.Fatalf("create = %d body %s", create.Status, create.Body)
			}
			transport := v1ObjectField(t, create.JSON, "transport")
			var subtitleURL string
			for _, subtitle := range playbackSubtitleObjects(t, transport) {
				if subtitle["format"] == "vtt" {
					subtitleURL, _ = subtitle["url"].(string)
					break
				}
			}
			target := playbackSubtitleTestURL(t, f, subtitleURL)
			capability := playbackSubtitleCapabilityFromURL(
				t, target, "subtitle-sessions")

			tc.disable(f.server.Cfg)
			resp, body := playbackSubtitleRawRequest(t, http.MethodGet, target)
			if resp.StatusCode != tc.status {
				t.Fatalf("disabled subtitle status = %d body %s", resp.StatusCode, body)
			}
			decoded := map[string]any{}
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Fatalf("decode disabled subtitle response: %v", err)
			}
			errorObject := v1ObjectField(t, decoded, "error")
			if errorObject["code"] != string(tc.code) {
				t.Fatalf("disabled subtitle code = %v, want %s",
					errorObject["code"], tc.code)
			}
			var state string
			if err := f.server.Store.DB.QueryRow(`SELECT state
				FROM playback_subtitle_sessions WHERE capability_hash = ?`,
				hashPlaybackCapabilityForTest(capability)).Scan(&state); err != nil {
				t.Fatalf("read disabled subtitle session: %v", err)
			}
			if state != models.PlaybackSubtitleSessionRevoked {
				t.Fatalf("disabled subtitle session state = %q", state)
			}
		})
	}
}

func TestPlaybackSubtitleErrorsAndLogsDoNotExposeSourceSecrets(t *testing.T) {
	f := newPlaybackAPIFixture(t)
	f.server.Cfg.Client = config.ClientFeatures{
		V1Enabled:             true,
		DeviceAuthEnabled:     true,
		PlaybackTicketEnabled: true,
		ServerProxyV1Enabled:  true,
	}
	fileID, sourceRoot := seedPlaybackSubtitleLocalFile(t, f.server.Store,
		playbackSubtitleProbe, map[string]string{"episode.srt": playbackSubtitleSRT})
	create := f.call(t, http.MethodPost, "/api/v1/playback/tickets",
		playbackCreateBody(fileID, f.deviceID, "server-proxy"))
	if create.Status != http.StatusCreated {
		t.Fatalf("create = %d body %s", create.Status, create.Body)
	}
	transport := v1ObjectField(t, create.JSON, "transport")
	var subtitleURL string
	for _, subtitle := range playbackSubtitleObjects(t, transport) {
		if subtitle["format"] == "vtt" {
			subtitleURL, _ = subtitle["url"].(string)
			break
		}
	}
	target := playbackSubtitleTestURL(t, f, subtitleURL)
	capability := playbackSubtitleCapabilityFromURL(t, target, "subtitle-sessions")
	if _, err := f.server.Store.DB.Exec(`UPDATE subtitles
		SET path = ? WHERE file_id = ?`,
		"/media/missing.srt?token=source-query-secret", fileID); err != nil {
		t.Fatalf("make subtitle source unavailable: %v", err)
	}

	var output bytes.Buffer
	previousWriter := log.Writer()
	previousFlags := log.Flags()
	log.SetOutput(&output)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
	})
	resp, body := playbackSubtitleRawRequest(t, http.MethodGet, target)
	logged := output.String()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("unavailable subtitle = %d body %s", resp.StatusCode, body)
	}
	for _, secret := range []string{
		sourceRoot, "source-query-secret", "missing.srt", capability,
	} {
		if strings.Contains(string(body), secret) {
			t.Errorf("subtitle response leaked %q: %s", secret, body)
		}
		if strings.Contains(logged, secret) {
			t.Errorf("subtitle log leaked %q: %s", secret, logged)
		}
	}
	if !strings.Contains(logged, "ss_redacted/subtitle.vtt") {
		t.Fatalf("subtitle log did not retain the redacted route: %s", logged)
	}
}

type playbackSubtitleSourceDriver struct {
	body string
	err  error
}

func (d *playbackSubtitleSourceDriver) List(context.Context, string) ([]source.Entry, error) {
	return nil, nil
}

func (d *playbackSubtitleSourceDriver) DirectURL(
	context.Context,
	string,
) (string, map[string]string, error) {
	return "", nil, source.ErrNoDirect
}

func (d *playbackSubtitleSourceDriver) Open(context.Context, string) (io.ReadCloser, error) {
	if d.err != nil {
		return nil, d.err
	}
	return io.NopCloser(strings.NewReader(d.body)), nil
}

func (d *playbackSubtitleSourceDriver) ProbeURL(
	context.Context,
	string,
) (string, map[string]string, error) {
	return "", nil, source.ErrNoDirect
}

func (d *playbackSubtitleSourceDriver) HasDirect() bool { return false }

func (d *playbackSubtitleSourceDriver) Subtitles(
	context.Context,
	string,
) ([]source.SubtitleTrack, error) {
	return nil, nil
}

func (d *playbackSubtitleSourceDriver) OpenSubtitle(
	context.Context,
	string,
	string,
) (io.ReadCloser, error) {
	if d.err != nil {
		return nil, d.err
	}
	return io.NopCloser(strings.NewReader(d.body)), nil
}

func (d *playbackSubtitleSourceDriver) SubtitlePathPrefix() string {
	return "driver:"
}

func TestPlaybackSubtitleSourceDriverErrorsAreOpaque(t *testing.T) {
	server := &Server{Engine: &media.Engine{}}
	driver := &playbackSubtitleSourceDriver{
		err: errors.New("https://source.example/private.srt?token=source-query-secret"),
	}
	if _, ok := server.loadV1PlaybackSubtitleVTT(
		context.Background(),
		&models.MediaFile{ID: 42},
		driver,
		&models.Subtitle{ID: 9, FileID: 42, Path: "driver:private.srt", Format: "subrip"},
	); ok {
		t.Fatal("source error was treated as a deliverable subtitle")
	}
}
