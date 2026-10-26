package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"doriyamedia/internal/config"
	"doriyamedia/internal/models"
)

func TestPlaybackFeatureDisableRevokesOldCapabilities(t *testing.T) {
	f := newPlaybackAPIFixture(t)
	fileID := seedPlaybackAPIFile(t, f.server.Store,
		"https://cdn.example.test/library/fail-closed.mp4", playbackAPITestProbe)
	base := playbackLifecycleFeatures(true, true, true, false, true, true)

	tests := []struct {
		name           string
		transport      string
		disable        func(*config.ClientFeatures)
		disabledStatus int
		disabledCode   V1ErrorCode
		replayedStatus int
		replayedCode   V1ErrorCode
	}{
		{
			name:      "server proxy v1 disabled",
			transport: models.PlaybackTransportServerProxy,
			disable: func(c *config.ClientFeatures) {
				c.V1Enabled = false
			},
			disabledStatus: http.StatusNotFound,
			disabledCode:   V1ErrorFeatureDisabled,
			replayedStatus: http.StatusForbidden,
			replayedCode:   V1ErrorForbidden,
		},
		{
			name:      "server proxy device auth disabled",
			transport: models.PlaybackTransportServerProxy,
			disable: func(c *config.ClientFeatures) {
				c.DeviceAuthEnabled = false
			},
			disabledStatus: http.StatusNotFound,
			disabledCode:   V1ErrorFeatureDisabled,
			replayedStatus: http.StatusForbidden,
			replayedCode:   V1ErrorForbidden,
		},
		{
			name:      "server proxy tickets disabled",
			transport: models.PlaybackTransportServerProxy,
			disable: func(c *config.ClientFeatures) {
				c.PlaybackTicketEnabled = false
			},
			disabledStatus: http.StatusNotFound,
			disabledCode:   V1ErrorFeatureDisabled,
			replayedStatus: http.StatusForbidden,
			replayedCode:   V1ErrorForbidden,
		},
		{
			name:      "server proxy transport disabled",
			transport: models.PlaybackTransportServerProxy,
			disable: func(c *config.ClientFeatures) {
				c.ServerProxyV1Enabled = false
			},
			disabledStatus: http.StatusForbidden,
			disabledCode:   V1ErrorTransportDisabled,
			replayedStatus: http.StatusForbidden,
			replayedCode:   V1ErrorForbidden,
		},
		{
			name:      "server hls v1 disabled",
			transport: models.PlaybackTransportServerHLS,
			disable: func(c *config.ClientFeatures) {
				c.V1Enabled = false
			},
			disabledStatus: http.StatusNotFound,
			disabledCode:   V1ErrorFeatureDisabled,
			replayedStatus: http.StatusForbidden,
			replayedCode:   V1ErrorTransportDisabled,
		},
		{
			name:      "server hls device auth disabled",
			transport: models.PlaybackTransportServerHLS,
			disable: func(c *config.ClientFeatures) {
				c.DeviceAuthEnabled = false
			},
			disabledStatus: http.StatusNotFound,
			disabledCode:   V1ErrorFeatureDisabled,
			replayedStatus: http.StatusForbidden,
			replayedCode:   V1ErrorTransportDisabled,
		},
		{
			name:      "server hls tickets disabled",
			transport: models.PlaybackTransportServerHLS,
			disable: func(c *config.ClientFeatures) {
				c.PlaybackTicketEnabled = false
			},
			disabledStatus: http.StatusNotFound,
			disabledCode:   V1ErrorFeatureDisabled,
			replayedStatus: http.StatusForbidden,
			replayedCode:   V1ErrorTransportDisabled,
		},
		{
			name:      "server hls transport disabled",
			transport: models.PlaybackTransportServerHLS,
			disable: func(c *config.ClientFeatures) {
				c.ServerHLSV1Enabled = false
			},
			disabledStatus: http.StatusForbidden,
			disabledCode:   V1ErrorTransportDisabled,
			replayedStatus: http.StatusForbidden,
			replayedCode:   V1ErrorTransportDisabled,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f.server.Cfg.Client = base
			defer func() { f.server.Cfg.Client = base }()

			ticketID, mediaURL := playbackFailClosedIssue(t, f, fileID, tc.transport)
			if got := playbackFailClosedState(t, f, ticketID, tc.transport); got != models.PlaybackMediaSessionActive {
				t.Fatalf("initial session state = %q, want active", got)
			}

			disabled := base
			tc.disable(&disabled)
			f.server.Cfg.Client = disabled
			response := v1Call(t, http.DefaultClient, http.MethodGet, mediaURL, nil, nil)
			v1AssertError(t, response, tc.disabledStatus, tc.disabledCode)
			if got := playbackFailClosedState(t, f, ticketID, tc.transport); got != models.PlaybackMediaSessionRevoked {
				t.Fatalf("disabled request left session state = %q, want revoked", got)
			}

			f.server.Cfg.Client = base
			replayed := v1Call(t, http.DefaultClient, http.MethodGet, mediaURL, nil, nil)
			v1AssertError(t, replayed, tc.replayedStatus, tc.replayedCode)
		})
	}
}

func TestPlaybackTransportDisableDoesNotRevokeSibling(t *testing.T) {
	f := newPlaybackAPIFixture(t)
	f.server.Cfg.Client = playbackLifecycleFeatures(true, true, true, false, true, true)
	fileID := seedPlaybackAPIFile(t, f.server.Store,
		"https://cdn.example.test/library/sibling.mp4", playbackAPITestProbe)

	proxyTicketID, proxyURL := playbackFailClosedIssue(
		t, f, fileID, models.PlaybackTransportServerProxy)
	hlsTicketID, _ := playbackFailClosedIssue(
		t, f, fileID, models.PlaybackTransportServerHLS)

	f.server.Cfg.Client.ServerProxyV1Enabled = false
	response := v1Call(t, http.DefaultClient, http.MethodGet, proxyURL, nil, nil)
	v1AssertError(t, response, http.StatusForbidden, V1ErrorTransportDisabled)

	if got := playbackFailClosedState(t, f, proxyTicketID, models.PlaybackTransportServerProxy); got != models.PlaybackMediaSessionRevoked {
		t.Errorf("proxy session state = %q, want revoked", got)
	}
	if got := playbackFailClosedState(t, f, hlsTicketID, models.PlaybackTransportServerHLS); got != models.PlaybackMediaSessionActive {
		t.Errorf("sibling HLS session state = %q, want active", got)
	}
}

func TestPlaybackGlobalDisableRevokesBothTransports(t *testing.T) {
	f := newPlaybackAPIFixture(t)
	f.server.Cfg.Client = playbackLifecycleFeatures(true, true, true, false, true, true)
	fileID := seedPlaybackAPIFile(t, f.server.Store,
		"https://cdn.example.test/library/global-disable.mp4", playbackAPITestProbe)

	proxyTicketID, proxyURL := playbackFailClosedIssue(
		t, f, fileID, models.PlaybackTransportServerProxy)
	hlsTicketID, _ := playbackFailClosedIssue(
		t, f, fileID, models.PlaybackTransportServerHLS)

	f.server.Cfg.Client.PlaybackTicketEnabled = false
	response := v1Call(t, http.DefaultClient, http.MethodGet, proxyURL, nil, nil)
	v1AssertError(t, response, http.StatusNotFound, V1ErrorFeatureDisabled)

	if got := playbackFailClosedState(t, f, proxyTicketID, models.PlaybackTransportServerProxy); got != models.PlaybackMediaSessionRevoked {
		t.Errorf("proxy session state = %q, want revoked", got)
	}
	if got := playbackFailClosedState(t, f, hlsTicketID, models.PlaybackTransportServerHLS); got != models.PlaybackMediaSessionRevoked {
		t.Errorf("HLS session state = %q, want revoked", got)
	}
}

func TestV1ServerHLSDisabledRevokeErrorIsRedacted(t *testing.T) {
	const secret = "postgres://db-user:db-password@private.example/media"
	logs := capturePlaybackSecurityLogs(t)
	store := &fakeV1HLSStore{revokeErr: errors.New(secret)}
	h := v1HLSTestHandler(store, &fakeV1HLSEngine{})
	h.cfg.Client.ServerHLSV1Enabled = false
	capability := "ms_" + strings.Repeat("a", 43)
	rec := httptest.NewRecorder()

	h.servePlaylist(rec, v1HLSPlaylistRequest(capability))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if len(store.revokeCalls) != 1 ||
		store.revokeCalls[0] != models.PlaybackTransportServerHLS {
		t.Fatalf("revoke calls = %v, want server-hls", store.revokeCalls)
	}
	if strings.Contains(logs.String(), secret) ||
		strings.Contains(logs.String(), "db-password") {
		t.Fatalf("revoke failure log leaked DB details: %q", logs.String())
	}
}

func playbackFailClosedIssue(
	t *testing.T,
	f playbackAPIFixture,
	fileID int64,
	transport string,
) (string, string) {
	t.Helper()
	create := f.call(t, http.MethodPost, "/api/v1/playback/tickets",
		playbackCreateBody(fileID, f.deviceID, transport))
	if create.Status != http.StatusCreated {
		t.Fatalf("issue %s ticket = %d; body %s", transport, create.Status, create.Body)
	}
	ticketID := v1StringField(t, create.JSON, "ticket_id")
	issued := v1ObjectField(t, create.JSON, "transport")
	if issued["kind"] != transport {
		t.Fatalf("issued transport = %v, want %s", issued["kind"], transport)
	}
	streams, ok := issued["streams"].([]any)
	if !ok || len(streams) != 1 {
		t.Fatalf("issued streams = %#v", issued["streams"])
	}
	stream, ok := streams[0].(map[string]any)
	if !ok {
		t.Fatalf("issued stream = %#v", streams[0])
	}
	rawURL := v1StringField(t, stream, "url")
	return ticketID, playbackLifecycleTestServerURL(t, f, rawURL)
}

func playbackFailClosedState(
	t *testing.T,
	f playbackAPIFixture,
	ticketID, transport string,
) string {
	t.Helper()
	var state string
	if err := f.server.Store.DB.QueryRow(`SELECT state FROM playback_media_sessions
		WHERE ticket_id = ? AND transport = ?`, ticketID, transport).Scan(&state); err != nil {
		t.Fatalf("read %s session state: %v", transport, err)
	}
	return state
}
