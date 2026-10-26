package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"doriyamedia/internal/config"
	"doriyamedia/internal/db"
	"doriyamedia/internal/media"
	"doriyamedia/internal/models"
	"doriyamedia/internal/source"
)

func playbackLifecycleFeatures(
	v1, deviceAuth, playbackTicket, directHTTP, serverProxy, serverHLS bool,
) config.ClientFeatures {
	return config.ClientFeatures{
		V1Enabled:             v1,
		DeviceAuthEnabled:     deviceAuth,
		PlaybackTicketEnabled: playbackTicket,
		DirectHTTPEnabled:     directHTTP,
		ServerProxyV1Enabled:  serverProxy,
		ServerHLSV1Enabled:    serverHLS,
	}
}

func playbackLifecycleAssertRemaining(
	t *testing.T,
	body map[string]any,
	want ...string,
) {
	t.Helper()
	values, ok := body["remaining_transports"].([]any)
	if !ok {
		t.Fatalf("remaining_transports is not an array: %v", body)
	}
	got := make([]string, 0, len(values))
	for _, value := range values {
		transport, ok := value.(string)
		if !ok {
			t.Fatalf("remaining transport is not a string: %#v", value)
		}
		got = append(got, transport)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("remaining transports = %v, want %v", got, want)
	}
}

func playbackLifecycleTicketRow(
	t *testing.T,
	store *db.Store,
	ticketID string,
) struct {
	Binding models.PlaybackTicketBinding
	Version int64
} {
	t.Helper()
	row := struct {
		Binding models.PlaybackTicketBinding
		Version int64
	}{
		Binding: models.PlaybackTicketBinding{TicketID: ticketID},
	}
	if err := store.DB.QueryRow(`
		SELECT user_id, device_id, file_id, version
		FROM playback_tickets WHERE ticket_id = ?`,
		ticketID,
	).Scan(
		&row.Binding.UserID,
		&row.Binding.DeviceID,
		&row.Binding.FileID,
		&row.Version,
	); err != nil {
		t.Fatalf("read playback ticket %q: %v", ticketID, err)
	}
	return row
}

func playbackLifecycleErrorDetails(
	t *testing.T,
	resp v1HTTPResponse,
) map[string]any {
	t.Helper()
	envelope, ok := resp.JSON["error"].(map[string]any)
	if !ok {
		t.Fatalf("response has no error envelope: %s", resp.Body)
	}
	details, ok := envelope["details"].(map[string]any)
	if !ok {
		t.Fatalf("error has no details object: %s", resp.Body)
	}
	return details
}

func playbackLifecycleAssertErrorAction(
	t *testing.T,
	resp v1HTTPResponse,
	action string,
) {
	t.Helper()
	if got := playbackLifecycleErrorDetails(t, resp)["action"]; got != action {
		t.Fatalf("error action = %v, want %q; body %s", got, action, resp.Body)
	}
}

func TestPlaybackStoreErrorMapsContextCancellation(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		status int
		code   V1ErrorCode
		action string
	}{
		{
			name:   "deadline",
			err:    context.DeadlineExceeded,
			status: http.StatusGatewayTimeout,
			code:   V1ErrorInternal,
			action: "retry",
		},
		{
			name:   "canceled",
			err:    context.Canceled,
			status: http.StatusRequestTimeout,
			code:   V1ErrorInvalidRequest,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeV1PlaybackStoreError(rec, tc.err)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d; body %s",
					rec.Code, tc.status, rec.Body.String())
			}
			var envelope v1ErrorEnvelope
			if err := json.NewDecoder(rec.Body).Decode(&envelope); err != nil {
				t.Fatalf("decode playback store error: %v", err)
			}
			if envelope.Error.Code != tc.code {
				t.Fatalf("error code = %q, want %q", envelope.Error.Code, tc.code)
			}
			if tc.action != "" && envelope.Error.Details["action"] != tc.action {
				t.Fatalf("error action = %v, want %q",
					envelope.Error.Details["action"], tc.action)
			}
		})
	}
}

func playbackLifecycleCreateTicket(
	t *testing.T,
	f playbackAPIFixture,
	fileID int64,
) *models.PlaybackTicket {
	t.Helper()
	var userID, deviceID int64
	if err := f.server.Store.DB.QueryRow(
		`SELECT user_id, id FROM devices WHERE public_id = ?`,
		f.deviceID,
	).Scan(&userID, &deviceID); err != nil {
		t.Fatalf("read playback fixture device: %v", err)
	}
	clientJSON, err := json.Marshal(media.NativeClientCapabilities{
		Platform:        "windows",
		Version:         "0.1.0",
		Engine:          "mpv",
		Containers:      []string{"mp4"},
		VideoCodecs:     []string{"h264"},
		AudioCodecs:     []string{"aac"},
		SubtitleCodecs:  []string{"text"},
		SupportsRange:   true,
		SupportsHeaders: true,
	})
	if err != nil {
		t.Fatalf("marshal playback client capabilities: %v", err)
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
		t.Fatalf("create lifecycle playback ticket: %v", err)
	}
	return ticket
}

func playbackLifecycleCreateReadyTicket(
	t *testing.T,
	f playbackAPIFixture,
	fileID int64,
	candidates ...string,
) *models.PlaybackTicket {
	t.Helper()
	ticket := playbackLifecycleCreateTicket(t, f, fileID)
	summary, err := json.Marshal(media.NativeMediaSummary{
		Container: "mp4",
		Duration:  120,
		Video:     &media.NativeVideoSummary{Codec: "h264", Width: 1920, Height: 1080},
		Audio:     []media.NativeAudioSummary{{Codec: "aac", Channels: 2}},
	})
	if err != nil {
		t.Fatalf("marshal playback media summary: %v", err)
	}
	ready, err := f.server.Store.FinishPlaybackTicketProbe(
		playbackBinding(ticket),
		ticket.Version,
		models.PlaybackTicketProbeResult{
			State:               models.PlaybackTicketReady,
			CandidateTransports: candidates,
			MediaSummary:        summary,
			VideoIndex:          intPtr(4),
		},
		time.Now().UTC().Add(time.Second),
	)
	if err != nil {
		t.Fatalf("finish lifecycle playback probe: %v", err)
	}
	return ready
}

func playbackLifecycleMediaSessionVersion(
	t *testing.T,
	store *db.Store,
	ticketID, transport string,
) int64 {
	t.Helper()
	var version int64
	if err := store.DB.QueryRow(`
		SELECT version FROM playback_media_sessions
		WHERE ticket_id = ? AND transport = ?`,
		ticketID,
		transport,
	).Scan(&version); err != nil {
		t.Fatalf("read media session for ticket %q: %v", ticketID, err)
	}
	return version
}

func playbackLifecycleCapability(t *testing.T, rawURL string) string {
	t.Helper()
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse media URL: %v", err)
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) < 2 {
		t.Fatalf("media URL has no capability/asset path: %q", rawURL)
	}
	capability := parts[len(parts)-2]
	if _, ok := v1PlaybackCapability(capability); !ok {
		t.Fatalf("media URL capability = %q", capability)
	}
	return capability
}

func playbackLifecycleTestServerURL(t *testing.T, f playbackAPIFixture, rawURL string) string {
	t.Helper()
	target, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse target URL: %v", err)
	}
	base, err := url.Parse(f.http.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	target.Scheme = base.Scheme
	target.Host = base.Host
	return target.String()
}

func playbackLifecycleDo(
	t *testing.T,
	client *http.Client,
	req *http.Request,
) v1HTTPResponse {
	t.Helper()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s %s response: %v", req.Method, req.URL, err)
	}
	out := v1HTTPResponse{Status: resp.StatusCode, Header: resp.Header.Clone(), Body: raw}
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &out.JSON); err != nil {
			t.Fatalf("decode %s %s response: %v; body %q",
				req.Method, req.URL, err, raw)
		}
	}
	return out
}

func playbackLifecycleChunkedRefresh(
	t *testing.T,
	f playbackAPIFixture,
	path string,
) v1HTTPResponse {
	t.Helper()
	req, err := http.NewRequest(
		http.MethodPost,
		f.http.URL+path,
		strings.NewReader(`{"unexpected":true}`),
	)
	if err != nil {
		t.Fatalf("new chunked refresh request: %v", err)
	}
	req.ContentLength = -1
	req.TransferEncoding = []string{"chunked"}
	req.Header.Set("Authorization", "Bearer "+f.access)
	req.Header.Set("Content-Type", "application/json")
	return playbackLifecycleDo(t, http.DefaultClient, req)
}

func TestPlaybackTicketLifecycleRefreshRejectsProbingReadOnly(t *testing.T) {
	f := newPlaybackAPIFixture(t)
	f.server.Cfg.Client = playbackLifecycleFeatures(
		true, true, true, false, true, false,
	)
	fileID := seedPlaybackAPIFile(
		t, f.server.Store, "https://cdn.example.test/library/video.mp4", "")
	ticket := playbackLifecycleCreateTicket(t, f, fileID)
	location := "/api/v1/playback/tickets/" + ticket.TicketID
	before := playbackLifecycleTicketRow(t, f.server.Store, ticket.TicketID)

	// Clearing the engine makes any accidental source authorization or probe
	// fail observably. Neither refresh nor GET may need source access here.
	f.server.Engine = nil
	refresh := f.call(t, http.MethodPost, location+"/refresh", nil)
	v1AssertError(t, refresh, http.StatusConflict, V1ErrorInvalidRequest)
	playbackLifecycleAssertErrorAction(t, refresh, "status")
	if got := playbackLifecycleErrorDetails(t, refresh)["state"]; got != models.PlaybackTicketProbing {
		t.Fatalf("probing refresh state detail = %v", got)
	}

	polled := f.call(t, http.MethodGet, location, nil)
	if polled.Status != http.StatusOK ||
		polled.JSON["state"] != models.PlaybackTicketProbing {
		t.Fatalf("poll = %d state %v; body %s",
			polled.Status, polled.JSON["state"], polled.Body)
	}
	if _, ok := polled.JSON["transport"]; ok {
		t.Fatal("probing poll exposed a transport")
	}
	after := playbackLifecycleTicketRow(t, f.server.Store, ticket.TicketID)
	if after.Version != before.Version {
		t.Fatalf("refresh advanced probing ticket version %d -> %d",
			before.Version, after.Version)
	}
}

func TestPlaybackTicketLifecycleTerminalRefreshSkipsSource(t *testing.T) {
	for _, tc := range []struct {
		state  string
		status int
		code   V1ErrorCode
		action string
	}{
		{
			state:  models.PlaybackTicketUnsupported,
			status: http.StatusGone,
			code:   V1ErrorTicketExpired,
			action: "request_new_ticket",
		},
		{
			state:  models.PlaybackTicketFailed,
			status: http.StatusGone,
			code:   V1ErrorTicketExpired,
			action: "request_new_ticket",
		},
		{
			state:  models.PlaybackTicketExhausted,
			status: http.StatusGone,
			code:   V1ErrorTicketExpired,
			action: "request_new_ticket",
		},
		{
			state:  models.PlaybackTicketExpired,
			status: http.StatusGone,
			code:   V1ErrorTicketExpired,
			action: "request_new_ticket",
		},
		{
			state:  models.PlaybackTicketRevoked,
			status: http.StatusForbidden,
			code:   V1ErrorForbidden,
			action: "reauthorize",
		},
	} {
		t.Run(tc.state, func(t *testing.T) {
			f := newPlaybackAPIFixture(t)
			f.server.Cfg.Client = playbackLifecycleFeatures(
				true, true, true, false, true, false,
			)
			fileID := seedPlaybackAPIFile(
				t, f.server.Store,
				"https://cdn.example.test/library/video.mp4",
				playbackAPITestProbe,
			)
			ticket := playbackLifecycleCreateReadyTicket(
				t, f, fileID, models.PlaybackTransportServerProxy,
			)
			now := time.Now().UTC()
			var revokedAt any
			if tc.state == models.PlaybackTicketRevoked {
				revokedAt = now
			}
			if _, err := f.server.Store.DB.Exec(`
				UPDATE playback_tickets
				SET state = ?, revoked_at = ?, version = version + 1, updated_at = ?
				WHERE ticket_id = ?`,
				tc.state, revokedAt, now, ticket.TicketID,
			); err != nil {
				t.Fatalf("set ticket terminal state: %v", err)
			}
			before := playbackLifecycleTicketRow(t, f.server.Store, ticket.TicketID)

			// A terminal refresh must return before Store.FileByID, DriverFor, or
			// any probe. Nil makes an accidental source path fail this test.
			f.server.Engine = nil
			resp := f.call(t, http.MethodPost,
				"/api/v1/playback/tickets/"+ticket.TicketID+"/refresh", nil)
			v1AssertError(t, resp, tc.status, tc.code)
			playbackLifecycleAssertErrorAction(t, resp, tc.action)
			details := playbackLifecycleErrorDetails(t, resp)
			if details["state"] != tc.state {
				t.Fatalf("terminal state detail = %v, want %q", details["state"], tc.state)
			}
			if details["ticket_id"] != ticket.TicketID {
				t.Fatalf("terminal ticket_id detail = %v", details["ticket_id"])
			}
			after := playbackLifecycleTicketRow(t, f.server.Store, ticket.TicketID)
			if after.Version != before.Version {
				t.Fatalf("terminal refresh advanced version %d -> %d",
					before.Version, after.Version)
			}
		})
	}
}

func TestPlaybackTicketLifecycleRefreshReadyUsesStoredPlanWithoutProbe(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		http.Error(w, "probe must not run", http.StatusInternalServerError)
	}))
	t.Cleanup(upstream.Close)

	f := newPlaybackAPIFixture(t)
	f.server.Cfg.Client = playbackLifecycleFeatures(
		true, true, true, false, true, false,
	)
	fileID := seedPlaybackAPIFile(
		t, f.server.Store, upstream.URL+"/video.mp4", "",
	)
	ticket := playbackLifecycleCreateReadyTicket(
		t, f, fileID, models.PlaybackTransportServerProxy,
	)
	location := "/api/v1/playback/tickets/" + ticket.TicketID

	engine := f.server.Engine
	f.server.Engine = nil
	ready := f.call(t, http.MethodGet, location, nil)
	f.server.Engine = engine
	if ready.Status != http.StatusOK ||
		ready.JSON["state"] != models.PlaybackTicketReady {
		t.Fatalf("ready status = %d state %v; body %s",
			ready.Status, ready.JSON["state"], ready.Body)
	}
	if _, ok := ready.JSON["transport"]; ok {
		t.Fatal("GET ready ticket reconstructed a transport")
	}
	if got := upstreamCalls.Load(); got != 0 {
		t.Fatalf("GET triggered %d source requests", got)
	}

	refresh := f.call(t, http.MethodPost, location+"/refresh", nil)
	if refresh.Status != http.StatusOK {
		t.Fatalf("ready refresh = %d; body %s", refresh.Status, refresh.Body)
	}
	if refresh.JSON["state"] != models.PlaybackTicketConsuming {
		t.Fatalf("ready refresh state = %v", refresh.JSON["state"])
	}
	transport := v1ObjectField(t, refresh.JSON, "transport")
	if transport["kind"] != models.PlaybackTransportServerProxy {
		t.Fatalf("ready refresh transport = %v", transport)
	}
	playbackLifecycleAssertRemaining(t, refresh.JSON)
	if got := upstreamCalls.Load(); got != 0 {
		t.Fatalf("refresh probed the source %d times", got)
	}

	var attemptedJSON string
	if err := f.server.Store.DB.QueryRow(
		`SELECT attempted_transports FROM playback_tickets WHERE ticket_id = ?`,
		ticket.TicketID,
	).Scan(&attemptedJSON); err != nil {
		t.Fatalf("read attempted transports: %v", err)
	}
	if attemptedJSON != `["server-proxy"]` {
		t.Fatalf("attempted transports = %s", attemptedJSON)
	}

	exhausted := f.call(t, http.MethodPost, location+"/refresh", nil)
	v1AssertError(t, exhausted, http.StatusGone, V1ErrorTicketExpired)
	playbackLifecycleAssertErrorAction(t, exhausted, "request_new_ticket")
	if got := playbackLifecycleErrorDetails(t, exhausted)["state"]; got != models.PlaybackTicketExhausted {
		t.Fatalf("exhausted state detail = %v", got)
	}
	if got := upstreamCalls.Load(); got != 0 {
		t.Fatalf("exhausted refresh probed the source %d times", got)
	}
}

func TestPlaybackTicketLifecycleConcurrentRefreshClaimsOnce(t *testing.T) {
	f := newPlaybackAPIFixture(t)
	f.server.Cfg.Client = playbackLifecycleFeatures(
		true, true, true, true, false, false,
	)
	fileID := seedPlaybackAPIFile(
		t, f.server.Store,
		"https://cdn.example.test/library/video.mp4",
		playbackAPITestProbe,
	)
	ticket := playbackLifecycleCreateReadyTicket(
		t, f, fileID, models.PlaybackTransportDirectHTTP,
	)
	location := "/api/v1/playback/tickets/" + ticket.TicketID + "/refresh"

	type result struct {
		status int
		body   []byte
		err    error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			req, err := http.NewRequest(
				http.MethodPost, f.http.URL+location, nil)
			if err != nil {
				results <- result{err: err}
				return
			}
			req.Header.Set("Authorization", "Bearer "+f.access)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				results <- result{err: err}
				return
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			results <- result{status: resp.StatusCode, body: body, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var issued, rejected int
	for outcome := range results {
		if outcome.err != nil {
			t.Fatalf("concurrent refresh: %v", outcome.err)
		}
		var body map[string]any
		if err := json.Unmarshal(outcome.body, &body); err != nil {
			t.Fatalf("decode concurrent refresh response: %v; body %s",
				err, outcome.body)
		}
		switch outcome.status {
		case http.StatusOK:
			issued++
			if body["state"] != models.PlaybackTicketConsuming {
				t.Fatalf("issued state = %v", body["state"])
			}
			if _, ok := body["transport"].(map[string]any); !ok {
				t.Fatalf("issued response has no transport: %s", outcome.body)
			}
		case http.StatusConflict, http.StatusGone:
			rejected++
		default:
			t.Fatalf("concurrent refresh status = %d; body %s",
				outcome.status, outcome.body)
		}
	}
	if issued != 1 || rejected != 1 {
		t.Fatalf("concurrent refresh issued=%d rejected=%d", issued, rejected)
	}

	var attemptedJSON string
	if err := f.server.Store.DB.QueryRow(
		`SELECT attempted_transports FROM playback_tickets WHERE ticket_id = ?`,
		ticket.TicketID,
	).Scan(&attemptedJSON); err != nil {
		t.Fatalf("read concurrent attempted transports: %v", err)
	}
	if attemptedJSON != `["direct-http"]` {
		t.Fatalf("concurrent attempted transports = %s", attemptedJSON)
	}
}

type playbackLifecycleBlockingDirectDriver struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
	target  string
	headers map[string]string
}

func (d *playbackLifecycleBlockingDirectDriver) List(
	context.Context,
	string,
) ([]source.Entry, error) {
	return nil, errors.New("not implemented")
}

func (d *playbackLifecycleBlockingDirectDriver) DirectURL(
	ctx context.Context,
	_ string,
) (string, map[string]string, error) {
	d.once.Do(func() { close(d.started) })
	select {
	case <-d.release:
		return d.target, d.headers, nil
	case <-ctx.Done():
		return "", nil, ctx.Err()
	}
}

func (d *playbackLifecycleBlockingDirectDriver) Open(
	context.Context,
	string,
) (io.ReadCloser, error) {
	return nil, errors.New("not implemented")
}

func (d *playbackLifecycleBlockingDirectDriver) ProbeURL(
	context.Context,
	string,
) (string, map[string]string, error) {
	return "", nil, errors.New("not implemented")
}

func (d *playbackLifecycleBlockingDirectDriver) HasDirect() bool { return true }

type playbackLifecycleContextIgnoringDirectDriver struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
	target  string
}

func (d *playbackLifecycleContextIgnoringDirectDriver) List(
	context.Context,
	string,
) ([]source.Entry, error) {
	return nil, errors.New("not implemented")
}

func (d *playbackLifecycleContextIgnoringDirectDriver) DirectURL(
	context.Context,
	string,
) (string, map[string]string, error) {
	d.once.Do(func() { close(d.started) })
	<-d.release
	return d.target, nil, nil
}

func (d *playbackLifecycleContextIgnoringDirectDriver) Open(
	context.Context,
	string,
) (io.ReadCloser, error) {
	return nil, errors.New("not implemented")
}

func (d *playbackLifecycleContextIgnoringDirectDriver) ProbeURL(
	context.Context,
	string,
) (string, map[string]string, error) {
	return "", nil, errors.New("not implemented")
}

func (d *playbackLifecycleContextIgnoringDirectDriver) HasDirect() bool { return true }

func TestPlaybackDirectURLHelperBoundsContextIgnoringDriver(t *testing.T) {
	driver := &playbackLifecycleContextIgnoringDirectDriver{
		started: make(chan struct{}),
		release: make(chan struct{}),
		target:  "https://cdn.example.test/direct?signature=HELPER_TIMEOUT_CANARY",
	}
	t.Cleanup(func() { close(driver.release) })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	type result struct {
		target string
		err    error
	}
	resultCh := make(chan result, 1)
	go func() {
		target, _, err := directURLForPlayback(ctx, driver, "video.mp4")
		resultCh <- result{target: target, err: err}
	}()

	select {
	case <-driver.started:
	case <-time.After(time.Second):
		t.Fatal("direct URL helper did not enter the blocking driver")
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("direct URL helper context did not reach its deadline")
	}

	select {
	case got := <-resultCh:
		if !errors.Is(got.err, context.DeadlineExceeded) || got.target != "" {
			t.Fatalf("direct URL helper = target %q err %v, want deadline", got.target, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("direct URL helper remained blocked by a context-ignoring driver")
	}
}

func TestPlaybackProbeFailureFinalizesWithoutRequestContext(t *testing.T) {
	f := newPlaybackAPIFixture(t)
	fileID := seedPlaybackAPIFile(
		t, f.server.Store,
		"https://cdn.example.test/library/video.mp4",
		playbackAPITestProbe,
	)
	ticket := playbackLifecycleCreateTicket(t, f, fileID)

	failed, err := f.server.finishV1PlaybackProbeFailed(
		playbackBinding(ticket), ticket.Version, "source_unavailable")
	if err != nil {
		t.Fatalf("finish failed probe: %v", err)
	}
	if failed == nil || failed.State != models.PlaybackTicketFailed ||
		failed.FailureCode != "source_unavailable" {
		t.Fatalf("failed probe ticket = %+v", failed)
	}
	persisted, err := f.server.Store.PlaybackTicketByID(
		playbackBinding(ticket), time.Now().UTC())
	if err != nil {
		t.Fatalf("load failed probe ticket: %v", err)
	}
	if persisted.State != models.PlaybackTicketFailed {
		t.Fatalf("persisted ticket state = %q, want failed", persisted.State)
	}
}

func TestPlaybackCreateFinalizationFailureUsesOriginalVersion(t *testing.T) {
	f := newPlaybackAPIFixture(t)
	fileID := seedPlaybackAPIFile(
		t, f.server.Store,
		"https://cdn.example.test/library/video.mp4",
		playbackAPITestProbe,
	)
	ticket := playbackLifecycleCreateTicket(t, f, fileID)
	file, drv, err := f.server.v1PlaybackFileAndDriver(fileID)
	if err != nil {
		t.Fatalf("authorize create finalization fixture: %v", err)
	}
	probe := media.ProbeFromCache(file)
	if probe == nil {
		t.Fatal("create finalization fixture has no cached probe")
	}
	client := media.NativeClientCapabilities{
		Platform:    "windows",
		Version:     "0.1.0",
		Engine:      "mpv",
		Containers:  []string{"mp4"},
		VideoCodecs: []string{"h264"},
		AudioCodecs: []string{"aac"},
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	failed, err := f.server.finalizeV1PlaybackTicketCreate(
		cancelled,
		playbackBinding(ticket),
		ticket,
		file,
		drv,
		probe,
		client,
		f.server.Cfg.Client,
	)
	if err != nil {
		t.Fatalf("finalize failed create ticket: %v", err)
	}
	if failed == nil || failed.State != models.PlaybackTicketFailed ||
		failed.FailureCode != "source_unavailable" {
		t.Fatalf("failed create finalization = %+v", failed)
	}
	persisted, err := f.server.Store.PlaybackTicketByID(
		playbackBinding(ticket), time.Now().UTC())
	if err != nil {
		t.Fatalf("load failed create finalization: %v", err)
	}
	if persisted.State != models.PlaybackTicketFailed ||
		persisted.FailureCode != "source_unavailable" {
		t.Fatalf("persisted failed create finalization = %+v", persisted)
	}
}

func TestPlaybackDirectIssuanceRechecksTicketAfterBlockedDirectURL(t *testing.T) {
	f := newPlaybackAPIFixture(t)
	f.server.Cfg.Client = playbackLifecycleFeatures(
		true, true, true, true, false, false,
	)
	fileID := seedPlaybackAPIFile(
		t, f.server.Store,
		"https://cdn.example.test/library/video.mp4",
		playbackAPITestProbe,
	)
	ticket := playbackLifecycleCreateReadyTicket(
		t, f, fileID, models.PlaybackTransportDirectHTTP,
	)
	file, err := f.server.Store.FileByID(fileID)
	if err != nil {
		t.Fatalf("load playback file: %v", err)
	}
	selection := media.NativeTransportSelection{
		Candidates: []media.NativeTransportCandidate{{
			Kind:  media.NativeTransportDirectHTTP,
			Range: true,
		}},
	}
	const directCanary = "https://cdn.example.test/direct?signature=DIRECT_CANARY"
	driver := &playbackLifecycleBlockingDirectDriver{
		started: make(chan struct{}),
		release: make(chan struct{}),
		target:  directCanary,
	}
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/playback/tickets/"+ticket.TicketID+"/refresh",
		nil,
	)
	type result struct {
		issued    *models.PlaybackTicket
		transport *v1PlaybackTransport
		err       error
	}
	resultCh := make(chan result, 1)
	go func() {
		issued, transport, err := f.server.claimV1PlaybackTransport(
			req,
			playbackBinding(ticket),
			ticket,
			file,
			driver,
			selection,
		)
		resultCh <- result{issued: issued, transport: transport, err: err}
	}()

	select {
	case <-driver.started:
	case <-time.After(5 * time.Second):
		t.Fatal("direct URL issuance did not enter the blocked driver")
	}
	if _, err := f.server.Store.UpdateDeviceFeatureOverrides(
		f.deviceID,
		models.DeviceFeatureOverrides{DirectHTTPDisabled: true},
		ticket.UserID,
		time.Now().UTC(),
	); err != nil {
		t.Fatalf("disable direct HTTP during issuance: %v", err)
	}
	close(driver.release)

	var outcome result
	select {
	case outcome = <-resultCh:
	case <-time.After(5 * time.Second):
		t.Fatal("direct URL issuance did not finish")
	}
	if !errors.Is(outcome.err, db.ErrPlaybackTicketRevoked) {
		t.Fatalf("direct issuance error = %v, want revoked ticket", outcome.err)
	}
	if outcome.transport != nil || outcome.issued == nil {
		t.Fatalf("revoked direct issuance returned transport=%+v ticket=%+v",
			outcome.transport, outcome.issued)
	}
	encoded, err := json.Marshal(outcome.transport)
	if err != nil {
		t.Fatalf("marshal rejected transport: %v", err)
	}
	if strings.Contains(string(encoded), directCanary) {
		t.Fatalf("rejected direct issuance leaked the URL: %s", encoded)
	}
	persisted, err := f.server.Store.PlaybackTicketByID(
		playbackBinding(ticket), time.Now().UTC())
	if err != nil {
		t.Fatalf("read revoked ticket: %v", err)
	}
	if persisted.State != models.PlaybackTicketRevoked {
		t.Fatalf("ticket state = %q, want revoked", persisted.State)
	}
}

func TestPlaybackDirectIssuanceTimeoutReleasesClaimWhenDriverIgnoresContext(t *testing.T) {
	f := newPlaybackAPIFixture(t)
	f.server.Cfg.Client = playbackLifecycleFeatures(
		true, true, true, true, false, false,
	)
	fileID := seedPlaybackAPIFile(
		t, f.server.Store,
		"https://cdn.example.test/library/video.mp4",
		playbackAPITestProbe,
	)
	ticket := playbackLifecycleCreateReadyTicket(
		t, f, fileID, models.PlaybackTransportDirectHTTP,
	)
	file, err := f.server.Store.FileByID(fileID)
	if err != nil {
		t.Fatalf("load playback file: %v", err)
	}
	selection := media.NativeTransportSelection{
		Candidates: []media.NativeTransportCandidate{{
			Kind:  media.NativeTransportDirectHTTP,
			Range: true,
		}},
	}
	driver := &playbackLifecycleContextIgnoringDirectDriver{
		started: make(chan struct{}),
		release: make(chan struct{}),
		target:  "https://cdn.example.test/direct?signature=TIMEOUT_CANARY",
	}
	t.Cleanup(func() { close(driver.release) })
	requestCtx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/playback/tickets/"+ticket.TicketID+"/refresh",
		nil,
	).WithContext(requestCtx)
	type result struct {
		issued    *models.PlaybackTicket
		transport *v1PlaybackTransport
		err       error
	}
	resultCh := make(chan result, 1)
	go func() {
		issued, transport, err := f.server.claimV1PlaybackTransport(
			req, playbackBinding(ticket), ticket, file, driver, selection)
		resultCh <- result{issued: issued, transport: transport, err: err}
	}()

	select {
	case <-driver.started:
	case <-time.After(5 * time.Second):
		t.Fatal("direct URL issuance did not enter the context-ignoring driver")
	}
	select {
	case <-requestCtx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("request context did not reach its deadline")
	}

	var outcome result
	select {
	case outcome = <-resultCh:
	case <-time.After(time.Second):
		t.Fatal("timed-out direct URL issuance did not finish")
	}
	if !errors.Is(outcome.err, context.DeadlineExceeded) {
		t.Fatalf("direct issuance error = %v, want context deadline", outcome.err)
	}
	if outcome.transport != nil || outcome.issued == nil {
		t.Fatalf("timed-out direct issuance returned transport=%+v ticket=%+v",
			outcome.transport, outcome.issued)
	}
	if outcome.issued.ClaimToken == "" || outcome.issued.Version <= 0 {
		t.Fatalf("timed-out direct issuance returned no claimed lease: %+v",
			outcome.issued)
	}

	var state, attemptedJSON, claimToken string
	var version int64
	var claimExpires sql.NullTime
	if err := f.server.Store.DB.QueryRow(`
		SELECT state, version, attempted_transports, claim_token, claim_expires_at
		FROM playback_tickets WHERE ticket_id = ?`,
		ticket.TicketID,
	).Scan(&state, &version, &attemptedJSON, &claimToken, &claimExpires); err != nil {
		t.Fatalf("read timed-out playback claim: %v", err)
	}
	if state != models.PlaybackTicketConsuming ||
		version != outcome.issued.Version+1 ||
		attemptedJSON != `["direct-http"]` ||
		claimToken != "" || claimExpires.Valid {
		t.Fatalf("timed-out playback claim = state %q version %d attempted %s token %q expiry %v",
			state, version, attemptedJSON, claimToken, claimExpires)
	}
}

func TestPlaybackTicketLifecycleActiveClaimRejectsConcurrentRefresh(t *testing.T) {
	f := newPlaybackAPIFixture(t)
	f.server.Cfg.Client = playbackLifecycleFeatures(
		true, true, true, true, false, false,
	)
	fileID := seedPlaybackAPIFile(
		t, f.server.Store,
		"https://cdn.example.test/library/video.mp4",
		playbackAPITestProbe,
	)
	ticket := playbackLifecycleCreateReadyTicket(
		t, f, fileID, models.PlaybackTransportDirectHTTP,
	)
	file, err := f.server.Store.FileByID(fileID)
	if err != nil {
		t.Fatalf("load playback file: %v", err)
	}
	selection := media.NativeTransportSelection{
		Candidates: []media.NativeTransportCandidate{{
			Kind:  media.NativeTransportDirectHTTP,
			Range: true,
		}},
	}
	driver := &playbackLifecycleBlockingDirectDriver{
		started: make(chan struct{}),
		release: make(chan struct{}),
		target:  "https://cdn.example.test/direct?signature=LEASE_CANARY",
	}
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/playback/tickets/"+ticket.TicketID+"/refresh",
		nil,
	)
	type result struct {
		issued    *models.PlaybackTicket
		transport *v1PlaybackTransport
		err       error
	}
	resultCh := make(chan result, 1)
	go func() {
		issued, transport, err := f.server.claimV1PlaybackTransport(
			req, playbackBinding(ticket), ticket, file, driver, selection)
		resultCh <- result{issued: issued, transport: transport, err: err}
	}()

	select {
	case <-driver.started:
	case <-time.After(5 * time.Second):
		t.Fatal("direct URL issuance did not enter the blocked driver")
	}

	conflict := f.call(t, http.MethodPost,
		"/api/v1/playback/tickets/"+ticket.TicketID+"/refresh", nil)
	v1AssertError(t, conflict, http.StatusConflict, V1ErrorInvalidRequest)
	playbackLifecycleAssertErrorAction(t, conflict, "retry")

	close(driver.release)
	var outcome result
	select {
	case outcome = <-resultCh:
	case <-time.After(5 * time.Second):
		t.Fatal("direct URL issuance did not finish")
	}
	if outcome.err != nil || outcome.transport == nil || outcome.issued == nil {
		t.Fatalf("blocked direct issuance = ticket %+v transport %+v err %v",
			outcome.issued, outcome.transport, outcome.err)
	}
	if outcome.transport.Kind != models.PlaybackTransportDirectHTTP {
		t.Fatalf("blocked direct transport = %q", outcome.transport.Kind)
	}

	var state, attemptedJSON, claimToken string
	var claimExpires sql.NullTime
	if err := f.server.Store.DB.QueryRow(`
		SELECT state, attempted_transports, claim_token, claim_expires_at
		FROM playback_tickets WHERE ticket_id = ?`,
		ticket.TicketID,
	).Scan(&state, &attemptedJSON, &claimToken, &claimExpires); err != nil {
		t.Fatalf("read finalized playback claim: %v", err)
	}
	if state != models.PlaybackTicketConsuming ||
		attemptedJSON != `["direct-http"]` || claimToken != "" ||
		claimExpires.Valid {
		t.Fatalf("final playback claim = state %q attempted %s token %q expiry %v",
			state, attemptedJSON, claimToken, claimExpires)
	}
}

type playbackLifecycleFailedDirectDriver struct{}

func (playbackLifecycleFailedDirectDriver) List(context.Context, string) ([]source.Entry, error) {
	return nil, errors.New("not implemented")
}

func (playbackLifecycleFailedDirectDriver) DirectURL(context.Context, string) (string, map[string]string, error) {
	return "", nil, errors.New("source refused to mint a direct URL")
}

func (playbackLifecycleFailedDirectDriver) Open(context.Context, string) (io.ReadCloser, error) {
	return nil, errors.New("not implemented")
}

func (playbackLifecycleFailedDirectDriver) ProbeURL(context.Context, string) (string, map[string]string, error) {
	return "", nil, errors.New("not implemented")
}

func (playbackLifecycleFailedDirectDriver) HasDirect() bool { return true }

func TestPlaybackTicketLifecycleDirectIssuanceFallsBackAtomically(t *testing.T) {
	f := newPlaybackAPIFixture(t)
	f.server.Cfg.Client = playbackLifecycleFeatures(
		true, true, true, true, true, false,
	)
	fileID := seedPlaybackAPIFile(
		t, f.server.Store,
		"https://cdn.example.test/library/video.mp4",
		playbackAPITestProbe,
	)

	var userID, deviceID int64
	if err := f.server.Store.DB.QueryRow(
		`SELECT user_id, id FROM devices WHERE public_id = ?`, f.deviceID,
	).Scan(&userID, &deviceID); err != nil {
		t.Fatalf("read fixture device: %v", err)
	}
	now := time.Now().UTC()
	created, err := f.server.Store.CreatePlaybackTicket(models.PlaybackTicketRequest{
		UserID:              userID,
		DeviceID:            deviceID,
		FileID:              fileID,
		Purpose:             models.PlaybackPurposePlayback,
		CandidateTransports: []string{},
		MediaSummary:        json.RawMessage(`{}`),
		ClientCapabilities:  json.RawMessage(`{}`),
	}, now)
	if err != nil {
		t.Fatalf("create ticket: %v", err)
	}
	binding := playbackBinding(created)
	ready, err := f.server.Store.FinishPlaybackTicketProbe(
		binding,
		created.Version,
		models.PlaybackTicketProbeResult{
			State: models.PlaybackTicketReady,
			CandidateTransports: []string{
				models.PlaybackTransportDirectHTTP,
				models.PlaybackTransportServerProxy,
			},
			MediaSummary: json.RawMessage(
				`{"container":"mp4","duration":120,"video":{"codec":"h264"},"audio":[]}`,
			),
			VideoIndex: intPtr(0),
		},
		now.Add(time.Second),
	)
	if err != nil {
		t.Fatalf("finish ticket probe: %v", err)
	}
	file, err := f.server.Store.FileByID(fileID)
	if err != nil {
		t.Fatalf("load playback file: %v", err)
	}
	selection := media.NativeTransportSelection{
		Candidates: []media.NativeTransportCandidate{
			{Kind: media.NativeTransportDirectHTTP},
			{Kind: media.NativeTransportServerProxy},
		},
	}
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/playback/tickets/"+created.TicketID+"/refresh", nil)

	issued, transport, err := f.server.claimV1PlaybackTransport(
		req,
		binding,
		ready,
		file,
		playbackLifecycleFailedDirectDriver{},
		selection,
	)
	if err != nil {
		t.Fatalf("claim fallback transport: %v", err)
	}
	if transport == nil ||
		transport.Kind != models.PlaybackTransportServerProxy ||
		transport.requestFallbackCount != 1 {
		t.Fatalf("issued transport = %#v, want server-proxy with one current-request fallback",
			transport)
	}
	if issued.SelectedTransport != models.PlaybackTransportServerProxy ||
		len(issued.AttemptedTransports) != 2 ||
		issued.AttemptedTransports[0] != models.PlaybackTransportDirectHTTP ||
		issued.AttemptedTransports[1] != models.PlaybackTransportServerProxy {
		t.Fatalf("issued ticket = %#v", issued)
	}
}

func TestPlaybackTicketLifecycleFailedIssuanceLeavesNoActiveClaim(t *testing.T) {
	f := newPlaybackAPIFixture(t)
	f.server.Cfg.Client = playbackLifecycleFeatures(
		true, true, true, true, false, false,
	)
	fileID := seedPlaybackAPIFile(
		t, f.server.Store,
		"https://cdn.example.test/library/video.mp4",
		playbackAPITestProbe,
	)
	ticket := playbackLifecycleCreateReadyTicket(
		t, f, fileID, models.PlaybackTransportDirectHTTP,
	)
	file, err := f.server.Store.FileByID(fileID)
	if err != nil {
		t.Fatalf("load playback file: %v", err)
	}
	selection := media.NativeTransportSelection{
		Candidates: []media.NativeTransportCandidate{{
			Kind: media.NativeTransportDirectHTTP,
		}},
	}
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/playback/tickets/"+ticket.TicketID+"/refresh",
		nil,
	)

	issued, transport, err := f.server.claimV1PlaybackTransport(
		req,
		playbackBinding(ticket),
		ticket,
		file,
		playbackLifecycleFailedDirectDriver{},
		selection,
	)
	if !errors.Is(err, db.ErrPlaybackTicketExhausted) {
		t.Fatalf("failed issuance error = %v, want exhausted ticket", err)
	}
	if transport != nil || issued == nil {
		t.Fatalf("failed issuance returned transport=%+v ticket=%+v", transport, issued)
	}

	var state, attemptedJSON, claimToken string
	var claimExpires sql.NullTime
	if err := f.server.Store.DB.QueryRow(`
		SELECT state, attempted_transports, claim_token, claim_expires_at
		FROM playback_tickets WHERE ticket_id = ?`,
		ticket.TicketID,
	).Scan(&state, &attemptedJSON, &claimToken, &claimExpires); err != nil {
		t.Fatalf("read failed issuance claim: %v", err)
	}
	if state != models.PlaybackTicketExhausted ||
		attemptedJSON != `["direct-http"]` || claimToken != "" ||
		claimExpires.Valid {
		t.Fatalf("failed issuance claim = state %q attempted %s token %q expiry %v",
			state, attemptedJSON, claimToken, claimExpires)
	}
}

func TestPlaybackTicketLifecycleResponseWriteFailureRevokesOnlyFailedIssuance(t *testing.T) {
	f := newPlaybackAPIFixture(t)
	f.server.Cfg.Client = config.ClientFeatures{
		V1Enabled:             true,
		DeviceAuthEnabled:     true,
		PlaybackTicketEnabled: true,
		ServerProxyV1Enabled:  true,
	}
	fileID, _ := seedPlaybackSubtitleLocalFile(
		t, f.server.Store, playbackSubtitleProbe,
		map[string]string{"episode.srt": playbackSubtitleSRT},
	)
	create := f.call(
		t, http.MethodPost, "/api/v1/playback/tickets",
		playbackCreateBody(fileID, f.deviceID, models.PlaybackTransportServerProxy),
	)
	if create.Status != http.StatusCreated {
		t.Fatalf("create playback ticket = %d body %s", create.Status, create.Body)
	}
	ticketID := strings.TrimPrefix(
		create.Header.Get("Location"), "/api/v1/playback/tickets/",
	)
	binding := playbackSubtitleTicketBinding(t, f.server.Store, ticketID)
	ticket, err := f.server.Store.PlaybackTicketByID(binding, time.Now().UTC())
	if err != nil {
		t.Fatalf("load issued playback ticket: %v", err)
	}

	var mediaSessions, subtitleSessions int
	if err := f.server.Store.DB.QueryRow(`
		SELECT COUNT(*) FROM playback_media_sessions
		WHERE ticket_id = ? AND state = ?`,
		ticketID, models.PlaybackMediaSessionActive,
	).Scan(&mediaSessions); err != nil {
		t.Fatalf("count issued media sessions: %v", err)
	}
	if err := f.server.Store.DB.QueryRow(`
		SELECT COUNT(*) FROM playback_subtitle_sessions
		WHERE ticket_id = ? AND transport = ? AND state = ?`,
		ticketID, models.PlaybackTransportServerProxy,
		models.PlaybackSubtitleSessionActive,
	).Scan(&subtitleSessions); err != nil {
		t.Fatalf("count issued subtitle sessions: %v", err)
	}
	if mediaSessions != 1 || subtitleSessions != 1 {
		t.Fatalf("issued sessions = media %d subtitle %d, want 1 and 1",
			mediaSessions, subtitleSessions)
	}

	writer := &playbackTelemetryFailingWriter{
		writeErr: errors.New("client disconnected"),
	}
	f.server.writeV1PlaybackIssuance(
		writer, http.StatusCreated, ticket,
		&v1PlaybackTransport{Kind: models.PlaybackTransportServerProxy},
		f.server.Cfg.Client,
	)

	var state, selected string
	if err := f.server.Store.DB.QueryRow(
		`SELECT state, selected_transport FROM playback_tickets WHERE ticket_id = ?`, ticketID,
	).Scan(&state, &selected); err != nil {
		t.Fatalf("read ticket after response failure: %v", err)
	}
	if state != models.PlaybackTicketConsuming ||
		selected != models.PlaybackTransportServerProxy {
		t.Fatalf("ticket after response failure = state %q transport %q, want consuming/server-proxy",
			state, selected)
	}
	var mediaState, subtitleState string
	if err := f.server.Store.DB.QueryRow(`
		SELECT state FROM playback_media_sessions WHERE ticket_id = ?`,
		ticketID,
	).Scan(&mediaState); err != nil {
		t.Fatalf("read media session after response failure: %v", err)
	}
	if err := f.server.Store.DB.QueryRow(`
		SELECT state FROM playback_subtitle_sessions WHERE ticket_id = ?`,
		ticketID,
	).Scan(&subtitleState); err != nil {
		t.Fatalf("read subtitle session after response failure: %v", err)
	}
	if mediaState != models.PlaybackMediaSessionRevoked ||
		subtitleState != models.PlaybackSubtitleSessionRevoked {
		t.Fatalf("session states after response failure = media %q subtitle %q",
			mediaState, subtitleState)
	}
}

func TestPlaybackTicketLifecycleWriteFailureKeepsConcurrentFallback(t *testing.T) {
	f := newPlaybackAPIFixture(t)
	f.server.Cfg.Client = config.ClientFeatures{
		V1Enabled:             true,
		DeviceAuthEnabled:     true,
		PlaybackTicketEnabled: true,
		ServerProxyV1Enabled:  true,
		ServerHLSV1Enabled:    true,
	}
	fileID, _ := seedPlaybackSubtitleLocalFile(
		t, f.server.Store, playbackSubtitleProbe,
		map[string]string{"episode.srt": playbackSubtitleSRT},
	)
	create := f.call(
		t, http.MethodPost, "/api/v1/playback/tickets",
		playbackCreateBody(fileID, f.deviceID,
			models.PlaybackTransportServerProxy,
			models.PlaybackTransportServerHLS,
		),
	)
	if create.Status != http.StatusCreated {
		t.Fatalf("create playback ticket = %d body %s", create.Status, create.Body)
	}
	location := create.Header.Get("Location")
	ticketID := strings.TrimPrefix(
		location, "/api/v1/playback/tickets/",
	)
	binding := playbackSubtitleTicketBinding(t, f.server.Store, ticketID)
	firstIssuance, err := f.server.Store.PlaybackTicketByID(binding, time.Now().UTC())
	if err != nil {
		t.Fatalf("load first issuance: %v", err)
	}

	refresh := f.call(t, http.MethodPost, location+"/refresh", nil)
	if refresh.Status != http.StatusOK {
		t.Fatalf("fallback refresh = %d body %s", refresh.Status, refresh.Body)
	}
	transport := v1ObjectField(t, refresh.JSON, "transport")
	if transport["kind"] != models.PlaybackTransportServerHLS {
		t.Fatalf("fallback transport = %v", transport)
	}

	writer := &playbackTelemetryFailingWriter{
		writeErr: errors.New("client disconnected"),
	}
	f.server.writeV1PlaybackIssuance(
		writer, http.StatusCreated, firstIssuance,
		&v1PlaybackTransport{Kind: models.PlaybackTransportServerProxy},
		f.server.Cfg.Client,
	)

	for _, tc := range []struct {
		name   string
		table  string
		states map[string]string
	}{
		{
			name:  "media sessions",
			table: "playback_media_sessions",
			states: map[string]string{
				models.PlaybackTransportServerProxy: models.PlaybackMediaSessionRevoked,
				models.PlaybackTransportServerHLS:   models.PlaybackMediaSessionActive,
			},
		},
		{
			name:  "subtitle sessions",
			table: "playback_subtitle_sessions",
			states: map[string]string{
				models.PlaybackTransportServerProxy: models.PlaybackSubtitleSessionRevoked,
				models.PlaybackTransportServerHLS:   models.PlaybackSubtitleSessionActive,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for transportKind, want := range tc.states {
				var got string
				query := `SELECT state FROM ` + tc.table +
					` WHERE ticket_id = ? AND transport = ?`
				if err := f.server.Store.DB.QueryRow(
					query, ticketID, transportKind).Scan(&got); err != nil {
					t.Fatalf("read %s state for %s: %v",
						tc.name, transportKind, err)
				}
				if got != want {
					t.Errorf("%s state for %s = %q, want %q",
						tc.name, transportKind, got, want)
				}
			}
		})
	}

	var state, selected string
	if err := f.server.Store.DB.QueryRow(
		`SELECT state, selected_transport FROM playback_tickets WHERE ticket_id = ?`,
		ticketID,
	).Scan(&state, &selected); err != nil {
		t.Fatalf("read ticket after first issuance write failure: %v", err)
	}
	if state != models.PlaybackTicketConsuming ||
		selected != models.PlaybackTransportServerHLS {
		t.Fatalf("ticket after first issuance write failure = state %q transport %q",
			state, selected)
	}
}

func TestPlaybackTicketLifecycleFeatureFlags(t *testing.T) {
	f := newPlaybackAPIFixture(t)
	fileID := seedPlaybackAPIFile(
		t, f.server.Store,
		"https://cdn.example.test/library/video.mp4",
		playbackAPITestProbe,
	)
	createBody := playbackCreateBody(
		fileID, f.deviceID, "direct-http", "server-proxy", "server-hls",
	)

	for _, tc := range []struct {
		name string
		cfg  config.ClientFeatures
	}{
		{
			name: "v1 disabled",
			cfg:  playbackLifecycleFeatures(false, true, true, true, true, true),
		},
		{
			name: "device auth disabled",
			cfg:  playbackLifecycleFeatures(true, false, true, true, true, true),
		},
		{
			name: "playback tickets disabled",
			cfg:  playbackLifecycleFeatures(true, true, false, true, true, true),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.server.Cfg.Client = tc.cfg
			resp := f.call(t, http.MethodPost, "/api/v1/playback/tickets", createBody)
			v1AssertError(t, resp, http.StatusNotFound, V1ErrorFeatureDisabled)
		})
	}

	t.Run("disabled direct omits direct and keeps fallback order", func(t *testing.T) {
		f.server.Cfg.Client = playbackLifecycleFeatures(
			true, true, true, false, true, true,
		)
		create := f.call(t, http.MethodPost, "/api/v1/playback/tickets", createBody)
		if create.Status != http.StatusCreated {
			t.Fatalf("create = %d; body %s", create.Status, create.Body)
		}
		transport := v1ObjectField(t, create.JSON, "transport")
		if transport["kind"] != models.PlaybackTransportServerProxy {
			t.Fatalf("transport = %v", transport)
		}
		playbackLifecycleAssertRemaining(
			t, create.JSON, models.PlaybackTransportServerHLS,
		)

		// Disabling every server transport after the ticket was prepared must
		// consume the untried candidates and finish as exhausted, never issue a
		// disabled transport.
		f.server.Cfg.Client = playbackLifecycleFeatures(
			true, true, true, false, false, false,
		)
		refresh := f.call(t, http.MethodPost,
			create.Header.Get("Location")+"/refresh", nil)
		v1AssertError(t, refresh, http.StatusGone, V1ErrorTicketExpired)
		playbackLifecycleAssertErrorAction(t, refresh, "request_new_ticket")
		if got := playbackLifecycleErrorDetails(t, refresh)["state"]; got != models.PlaybackTicketExhausted {
			t.Fatalf("terminal state detail = %v", got)
		}

		allOff := f.call(t, http.MethodPost, "/api/v1/playback/tickets", createBody)
		if allOff.Status != http.StatusCreated {
			t.Fatalf("all-off create = %d; body %s", allOff.Status, allOff.Body)
		}
		if allOff.JSON["state"] != models.PlaybackTicketUnsupported {
			t.Fatalf("all-off state = %v", allOff.JSON["state"])
		}
		if message, _ := allOff.JSON["message"].(string); message == "" {
			t.Fatalf("all-off response has no stable message: %v", allOff.JSON)
		}
		if code := allOff.JSON["failure_code"]; code != "unsupported_media" {
			t.Fatalf("all-off failure_code = %v, want unsupported_media", code)
		}
		if _, ok := allOff.JSON["transport"]; ok {
			t.Fatal("unsupported ticket exposed a transport")
		}
		playbackLifecycleAssertRemaining(t, allOff.JSON)

		again := f.call(t, http.MethodPost,
			allOff.Header.Get("Location")+"/refresh", nil)
		v1AssertError(t, again, http.StatusGone, V1ErrorTicketExpired)
		playbackLifecycleAssertErrorAction(t, again, "request_new_ticket")
		if got := playbackLifecycleErrorDetails(t, again)["state"]; got != models.PlaybackTicketUnsupported {
			t.Fatalf("terminal state detail = %v", got)
		}
	})
}

func TestPlaybackTicketStatusRemainingTransportsUsesLiveFlags(t *testing.T) {
	f := newPlaybackAPIFixture(t)
	f.server.Cfg.Client = playbackLifecycleFeatures(
		true, true, true, true, true, true,
	)
	fileID := seedPlaybackAPIFile(
		t, f.server.Store,
		"https://cdn.example.test/library/video.mp4",
		playbackAPITestProbe,
	)
	ticket := playbackLifecycleCreateReadyTicket(
		t, f, fileID,
		models.PlaybackTransportDirectHTTP,
		models.PlaybackTransportServerProxy,
		models.PlaybackTransportServerHLS,
	)
	location := "/api/v1/playback/tickets/" + ticket.TicketID

	// Status must remain source-free while it applies live transport flags.
	f.server.Engine = nil
	allEnabled := f.call(t, http.MethodGet, location, nil)
	if allEnabled.Status != http.StatusOK {
		t.Fatalf("all-enabled status = %d; body %s",
			allEnabled.Status, allEnabled.Body)
	}
	playbackLifecycleAssertRemaining(
		t, allEnabled.JSON,
		models.PlaybackTransportDirectHTTP,
		models.PlaybackTransportServerProxy,
		models.PlaybackTransportServerHLS,
	)

	f.server.Cfg.Client = playbackLifecycleFeatures(
		true, true, true, false, true, true,
	)
	directDisabled := f.call(t, http.MethodGet, location, nil)
	if directDisabled.Status != http.StatusOK {
		t.Fatalf("direct-disabled status = %d; body %s",
			directDisabled.Status, directDisabled.Body)
	}
	playbackLifecycleAssertRemaining(
		t, directDisabled.JSON,
		models.PlaybackTransportServerProxy,
		models.PlaybackTransportServerHLS,
	)

	f.server.Cfg.Client = playbackLifecycleFeatures(
		true, true, true, false, false, false,
	)
	noneEnabled := f.call(t, http.MethodGet, location, nil)
	v1AssertError(t, noneEnabled, http.StatusGone, V1ErrorTicketExpired)
	playbackLifecycleAssertErrorAction(t, noneEnabled, "request_new_ticket")
	if got := playbackLifecycleErrorDetails(t, noneEnabled)["state"]; got != models.PlaybackTicketExhausted {
		t.Fatalf("empty effective plan state = %v, want exhausted", got)
	}
	if _, ok := noneEnabled.JSON["remaining_transports"]; ok {
		t.Fatalf("terminal status exposed a ready+empty projection: %s", noneEnabled.Body)
	}
}

func TestPlaybackTicketRefreshUsesFrozenVideoIndexWithoutProbe(t *testing.T) {
	f := newPlaybackAPIFixture(t)
	f.server.Cfg.Client = playbackLifecycleFeatures(
		true, true, true, false, false, true,
	)
	fileID := seedPlaybackAPIFile(
		t, f.server.Store,
		"https://cdn.example.test/library/refresh-no-probe.mkv",
		"",
	)
	ticket := playbackLifecycleCreateReadyTicket(
		t, f, fileID, models.PlaybackTransportServerHLS,
	)
	if ticket.VideoIndex != 4 {
		t.Fatalf("ready ticket video index = %d, want 4", ticket.VideoIndex)
	}

	// Refresh must use the ticket's frozen probe result. DriverFor handles URL
	// sources without the store, while any accidental probe would dereference
	// this deliberately removed store and fail the test.
	f.server.Engine.Store = nil
	refresh := f.call(t, http.MethodPost,
		"/api/v1/playback/tickets/"+ticket.TicketID+"/refresh", nil)
	if refresh.Status != http.StatusOK {
		t.Fatalf("refresh = %d; body %s", refresh.Status, refresh.Body)
	}
	transport := v1ObjectField(t, refresh.JSON, "transport")
	if transport["kind"] != models.PlaybackTransportServerHLS {
		t.Fatalf("refresh transport = %v, want server-hls", transport)
	}

	var sessionVideoIndex int
	if err := f.server.Store.DB.QueryRow(`
		SELECT video_index FROM playback_media_sessions
		WHERE ticket_id = ? AND transport = ?`,
		ticket.TicketID, models.PlaybackTransportServerHLS,
	).Scan(&sessionVideoIndex); err != nil {
		t.Fatalf("read refreshed HLS session video index: %v", err)
	}
	if sessionVideoIndex != 4 {
		t.Fatalf("refreshed HLS session video index = %d, want 4", sessionVideoIndex)
	}
	if bytes.Contains(refresh.Body, []byte("video_index")) {
		t.Fatalf("refresh response exposed the internal video index: %s", refresh.Body)
	}
}

func TestPlaybackTicketStatusUsesClosedFailureCodes(t *testing.T) {
	s := &Server{}
	for _, tc := range []struct {
		state string
		code  string
		want  string
	}{
		{models.PlaybackTicketUnsupported, "internal_detail_canary", "unsupported_media"},
		{models.PlaybackTicketFailed, "source_token_canary", "source_unavailable"},
	} {
		got := s.v1PlaybackTicketStatus(&models.PlaybackTicket{
			TicketID:    "pt_failure_code_test",
			State:       tc.state,
			ExpiresAt:   time.Now().UTC().Add(time.Minute),
			FailureCode: tc.code,
		}, config.ClientFeatures{})
		if got.FailureCode != tc.want {
			t.Errorf("%s failure code = %q, want %q", tc.state, got.FailureCode, tc.want)
		}
		if strings.Contains(got.FailureCode, "canary") {
			t.Errorf("%s leaked raw failure code %q", tc.state, got.FailureCode)
		}
	}
}

func TestPlaybackTicketLifecycleIsolation(t *testing.T) {
	f := newPlaybackAPIFixture(t)
	f.server.Cfg.Client = playbackLifecycleFeatures(
		true, true, true, true, false, false,
	)
	fileID := seedPlaybackAPIFile(
		t, f.server.Store,
		"https://cdn.example.test/library/video.mp4",
		playbackAPITestProbe,
	)
	create := f.call(t, http.MethodPost, "/api/v1/playback/tickets",
		playbackCreateBody(fileID, f.deviceID, "direct-http"))
	if create.Status != http.StatusCreated {
		t.Fatalf("create = %d; body %s", create.Status, create.Body)
	}
	location := create.Header.Get("Location")
	original := f.call(t, http.MethodGet, location, nil)
	if original.Status != http.StatusOK {
		t.Fatalf("original lookup = %d; body %s", original.Status, original.Body)
	}

	admin, _ := v1Login(t, f.http.URL, "admin", "admin-pass")
	_, secondPair := v1IssueDevice(t, f.http.URL, admin, "Playback Second Device")
	secondAccess := v1StringField(t, secondPair, "access_token")
	crossDevice := v1Call(t, http.DefaultClient, http.MethodGet,
		f.http.URL+location, nil, map[string]string{
			"Authorization": "Bearer " + secondAccess,
		})
	v1AssertError(t, crossDevice, http.StatusNotFound, V1ErrorTicketNotFound)

	viewerID := v1CreateViewer(
		t, f.http.URL, admin, "playback-viewer", "viewer-pass",
	)
	if viewerID <= 0 {
		t.Fatalf("viewer id = %d", viewerID)
	}
	viewer, _ := v1Login(t, f.http.URL, "playback-viewer", "viewer-pass")
	_, viewerPair := v1IssueDevice(t, f.http.URL, viewer, "Playback Viewer Device")
	viewerAccess := v1StringField(t, viewerPair, "access_token")
	crossUser := v1Call(t, http.DefaultClient, http.MethodGet,
		f.http.URL+location, nil, map[string]string{
			"Authorization": "Bearer " + viewerAccess,
		})
	v1AssertError(t, crossUser, http.StatusNotFound, V1ErrorTicketNotFound)
}

func TestPlaybackTicketLifecycleExpiryAndRevocation(t *testing.T) {
	f := newPlaybackAPIFixture(t)
	f.server.Cfg.Client = playbackLifecycleFeatures(
		true, true, true, true, false, false,
	)
	fileID := seedPlaybackAPIFile(
		t, f.server.Store,
		"https://cdn.example.test/library/video.mp4",
		playbackAPITestProbe,
	)

	expiredCreate := f.call(t, http.MethodPost, "/api/v1/playback/tickets",
		playbackCreateBody(fileID, f.deviceID, "direct-http"))
	if expiredCreate.Status != http.StatusCreated {
		t.Fatalf("expired setup = %d; body %s",
			expiredCreate.Status, expiredCreate.Body)
	}
	expiredLocation := expiredCreate.Header.Get("Location")
	expireAt := time.Now().UTC().Add(models.PlaybackTicketTTL + time.Minute)
	expiredCount, err := f.server.Store.ExpirePlaybackTickets(expireAt)
	if err != nil || expiredCount != 1 {
		t.Fatalf("expire tickets = %d, %v; want 1", expiredCount, err)
	}
	expiredStatus := f.call(t, http.MethodGet, expiredLocation, nil)
	if expiredStatus.Status != http.StatusOK ||
		expiredStatus.JSON["state"] != models.PlaybackTicketExpired {
		t.Fatalf("expired status = %d state %v; body %s",
			expiredStatus.Status, expiredStatus.JSON["state"], expiredStatus.Body)
	}
	expiredRefresh := f.call(t, http.MethodPost, expiredLocation+"/refresh", nil)
	v1AssertError(t, expiredRefresh, http.StatusGone, V1ErrorTicketExpired)
	playbackLifecycleAssertErrorAction(t, expiredRefresh, "request_new_ticket")

	revokedCreate := f.call(t, http.MethodPost, "/api/v1/playback/tickets",
		playbackCreateBody(fileID, f.deviceID, "direct-http"))
	if revokedCreate.Status != http.StatusCreated {
		t.Fatalf("revoked setup = %d; body %s",
			revokedCreate.Status, revokedCreate.Body)
	}
	revokedLocation := revokedCreate.Header.Get("Location")
	revokedTicketID := v1StringField(t, revokedCreate.JSON, "ticket_id")
	revokedRow := playbackLifecycleTicketRow(
		t, f.server.Store, revokedTicketID,
	)
	if _, err := f.server.Store.RevokePlaybackTicket(
		revokedRow.Binding,
		revokedRow.Version,
		time.Now().UTC(),
	); err != nil {
		t.Fatalf("revoke ticket: %v", err)
	}
	revokedStatus := f.call(t, http.MethodGet, revokedLocation, nil)
	if revokedStatus.Status != http.StatusOK ||
		revokedStatus.JSON["state"] != models.PlaybackTicketRevoked {
		t.Fatalf("revoked status = %d state %v; body %s",
			revokedStatus.Status, revokedStatus.JSON["state"], revokedStatus.Body)
	}
	revokedRefresh := f.call(t, http.MethodPost, revokedLocation+"/refresh", nil)
	v1AssertError(t, revokedRefresh, http.StatusForbidden, V1ErrorForbidden)
	playbackLifecycleAssertErrorAction(t, revokedRefresh, "reauthorize")

	f.server.Cfg.Client = playbackLifecycleFeatures(
		true, true, true, false, true, false,
	)
	sessionCreate := f.call(t, http.MethodPost, "/api/v1/playback/tickets",
		playbackCreateBody(fileID, f.deviceID, "server-proxy"))
	if sessionCreate.Status != http.StatusCreated {
		t.Fatalf("session setup = %d; body %s",
			sessionCreate.Status, sessionCreate.Body)
	}
	sessionTicketID := v1StringField(t, sessionCreate.JSON, "ticket_id")
	sessionTransport := v1ObjectField(t, sessionCreate.JSON, "transport")
	sessionStreams := sessionTransport["streams"].([]any)
	if len(sessionStreams) != 1 {
		t.Fatalf("session streams = %#v", sessionStreams)
	}
	sessionStream := sessionStreams[0].(map[string]any)
	mediaURL := v1StringField(t, sessionStream, "url")
	capability := playbackLifecycleCapability(t, mediaURL)
	sessionRow := playbackLifecycleTicketRow(
		t, f.server.Store, sessionTicketID,
	)
	sessionVersion := playbackLifecycleMediaSessionVersion(
		t, f.server.Store, sessionTicketID, models.PlaybackTransportServerProxy,
	)
	if _, err := f.server.Store.RevokePlaybackMediaSession(
		models.PlaybackMediaSessionBinding{
			Ticket:    sessionRow.Binding,
			Transport: models.PlaybackTransportServerProxy,
		},
		capability,
		sessionVersion,
		time.Now().UTC(),
	); err != nil {
		t.Fatalf("revoke media session: %v", err)
	}

	revokedMedia := v1Call(
		t,
		http.DefaultClient,
		http.MethodGet,
		playbackLifecycleTestServerURL(t, f, mediaURL),
		nil,
		nil,
	)
	v1AssertError(t, revokedMedia, http.StatusForbidden, V1ErrorForbidden)
}

func TestPlaybackTicketLifecycleRefreshBodiesAndHeadErrors(t *testing.T) {
	f := newPlaybackAPIFixture(t)
	f.server.Cfg.Client = playbackLifecycleFeatures(
		true, true, true, false, true, false,
	)
	refreshPath := "/api/v1/playback/tickets/pt_" +
		strings.Repeat("a", 43) + "/refresh"

	withLength := f.call(
		t, http.MethodPost, refreshPath, []byte(`{"unexpected":true}`),
	)
	v1AssertError(t, withLength, http.StatusBadRequest, V1ErrorInvalidRequest)

	chunked := playbackLifecycleChunkedRefresh(t, f, refreshPath)
	v1AssertError(t, chunked, http.StatusBadRequest, V1ErrorInvalidRequest)

	capability := "ms_" + strings.Repeat("a", 43)
	for _, tc := range []struct {
		name            string
		path            string
		playbackEnabled bool
		status          int
	}{
		{
			name:            "feature disabled",
			path:            "/api/v1/playback/media-sessions/" + capability + "/stream",
			playbackEnabled: false,
			status:          http.StatusNotFound,
		},
		{
			name:            "query rejected",
			path:            "/api/v1/playback/media-sessions/" + capability + "/stream?forged=1",
			playbackEnabled: true,
			status:          http.StatusBadRequest,
		},
		{
			name:            "invalid capability",
			path:            "/api/v1/playback/media-sessions/ms_short/stream",
			playbackEnabled: true,
			status:          http.StatusNotFound,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.server.Cfg.Client = playbackLifecycleFeatures(
				true, true, tc.playbackEnabled, false, true, false,
			)
			req, err := http.NewRequest(
				http.MethodHead, f.http.URL+tc.path, nil,
			)
			if err != nil {
				t.Fatalf("new HEAD request: %v", err)
			}
			resp := playbackLifecycleDo(t, http.DefaultClient, req)
			if resp.Status != tc.status {
				t.Fatalf("HEAD status = %d, want %d; body %q",
					resp.Status, tc.status, resp.Body)
			}
			if len(resp.Body) != 0 {
				t.Fatalf("HEAD error body = %q, want empty", resp.Body)
			}
			if got := resp.Header.Get("Cache-Control"); got != "no-store" {
				t.Errorf("HEAD Cache-Control = %q, want no-store", got)
			}
		})
	}
}
