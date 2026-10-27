package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"doriyamedia/internal/models"
)

func addPlaybackSubtitle(
	t *testing.T,
	store *Store,
	fileID int64,
	path, format string,
	embedded bool,
	streamIndex int,
) int64 {
	t.Helper()
	if err := store.UpsertSubtitle(&models.Subtitle{
		FileID: fileID, Path: path, Lang: "zh-CN", Title: "Test subtitle",
		Format: format, Embedded: embedded, StreamIdx: streamIndex,
		Convertible: !embedded && format != "hdmv_pgs_subtitle",
	}); err != nil {
		t.Fatalf("insert subtitle: %v", err)
	}
	var id int64
	if err := store.DB.QueryRow(`SELECT id FROM subtitles
		WHERE file_id = ? AND path = ? AND stream_idx = ?`,
		fileID, path, streamIndex).Scan(&id); err != nil {
		t.Fatalf("read subtitle id: %v", err)
	}
	return id
}

func createConsumedPlaybackTicket(
	t *testing.T,
	store *Store,
	fixture playbackTestFixture,
	now time.Time,
	subtitleIDs []int64,
) *models.PlaybackTicket {
	t.Helper()
	ticket, err := store.CreatePlaybackTicket(models.PlaybackTicketRequest{
		UserID: fixture.user.ID, DeviceID: fixture.device.ID, FileID: fixture.fileID,
		CandidateTransports: []string{models.PlaybackTransportDirectHTTP},
		MediaSummary:        []byte(`{}`),
	}, now)
	if err != nil {
		t.Fatalf("create playback ticket: %v", err)
	}
	ready, err := store.FinishPlaybackTicketProbe(
		fixture.bindingWith(ticket.TicketID),
		ticket.Version,
		models.PlaybackTicketProbeResult{
			State:               models.PlaybackTicketReady,
			CandidateTransports: []string{models.PlaybackTransportDirectHTTP},
			MediaSummary:        []byte(`{}`),
			SubtitleIDs:         subtitleIDs,
			VideoIndex:          playbackVideoIndexPtr(0),
		},
		now.Add(time.Second),
	)
	if err != nil {
		t.Fatalf("finish playback probe: %v", err)
	}
	claimed, err := store.ClaimNextPlaybackTransport(
		context.Background(),
		fixture.bindingWith(ticket.TicketID),
		ready.Version,
		now.Add(2*time.Second),
	)
	if err != nil {
		t.Fatalf("claim playback transport: %v", err)
	}
	return claimed
}

func playbackSubtitleTicketBinding(
	ticket *models.PlaybackTicket,
) models.PlaybackTicketBinding {
	return models.PlaybackTicketBinding{
		UserID: ticket.UserID, DeviceID: ticket.DeviceID,
		FileID: ticket.FileID, TicketID: ticket.TicketID,
	}
}

func TestPlaybackSubtitleGrantSnapshotIsFileScopedAndImmutable(t *testing.T) {
	store := openPlaybackTestStore(t)
	now := time.Date(2026, 10, 26, 18, 0, 0, 0, time.UTC)
	owner := addPlaybackTestFixture(t, store, "subtitle-owner", "subtitle-owner", now)
	other := addPlaybackTestFixture(t, store, "subtitle-other", "subtitle-other", now)

	first := addPlaybackSubtitle(t, store, owner.fileID, "/owner/first.srt", "subrip", false, 0)
	second := addPlaybackSubtitle(t, store, owner.fileID, "/owner/second.ass", "ass", false, 0)
	foreign := addPlaybackSubtitle(t, store, other.fileID, "/other/foreign.srt", "subrip", false, 0)

	ticket, err := store.CreatePlaybackTicket(models.PlaybackTicketRequest{
		UserID: owner.user.ID, DeviceID: owner.device.ID, FileID: owner.fileID,
		CandidateTransports: []string{models.PlaybackTransportDirectHTTP},
	}, now)
	if err != nil {
		t.Fatalf("create ticket: %v", err)
	}
	ready, err := store.FinishPlaybackTicketProbe(
		owner.bindingWith(ticket.TicketID),
		ticket.Version,
		models.PlaybackTicketProbeResult{
			State:               models.PlaybackTicketReady,
			CandidateTransports: []string{models.PlaybackTransportDirectHTTP},
			SubtitleIDs:         []int64{second, first, second},
			VideoIndex:          playbackVideoIndexPtr(0),
		},
		now.Add(time.Second),
	)
	if err != nil {
		t.Fatalf("finish ticket with subtitle grants: %v", err)
	}
	if len(ready.SubtitleIDs) != 2 ||
		ready.SubtitleIDs[0] != first || ready.SubtitleIDs[1] != second {
		t.Fatalf("subtitle grants = %v, want [%d %d]", ready.SubtitleIDs, first, second)
	}
	persisted, err := store.PlaybackTicketByID(
		owner.bindingWith(ticket.TicketID), now.Add(2*time.Second))
	if err != nil {
		t.Fatalf("reload ticket: %v", err)
	}
	if len(persisted.SubtitleIDs) != 2 ||
		persisted.SubtitleIDs[0] != first || persisted.SubtitleIDs[1] != second {
		t.Fatalf("persisted subtitle grants = %v", persisted.SubtitleIDs)
	}
	if _, err := store.DB.Exec(`UPDATE playback_tickets
		SET subtitle_ids_json = ? WHERE ticket_id = ?`,
		`[1]`, ticket.TicketID); err == nil {
		t.Fatal("immutable subtitle grant was replaced")
	}

	crossFileTicket, err := store.CreatePlaybackTicket(models.PlaybackTicketRequest{
		UserID: owner.user.ID, DeviceID: owner.device.ID, FileID: owner.fileID,
		CandidateTransports: []string{models.PlaybackTransportDirectHTTP},
	}, now)
	if err != nil {
		t.Fatalf("create cross-file ticket: %v", err)
	}
	if _, err := store.FinishPlaybackTicketProbe(
		owner.bindingWith(crossFileTicket.TicketID),
		crossFileTicket.Version,
		models.PlaybackTicketProbeResult{
			State:               models.PlaybackTicketReady,
			CandidateTransports: []string{models.PlaybackTransportDirectHTTP},
			SubtitleIDs:         []int64{foreign},
			VideoIndex:          playbackVideoIndexPtr(0),
		},
		now.Add(time.Second),
	); !errors.Is(err, ErrInvalidPlaybackTicket) {
		t.Fatalf("cross-file subtitle grant error = %v", err)
	}

	tooMany := make([]int64, models.PlaybackMaxSubtitleGrants+1)
	for i := range tooMany {
		tooMany[i] = int64(i + 1)
	}
	limitTicket, err := store.CreatePlaybackTicket(models.PlaybackTicketRequest{
		UserID: owner.user.ID, DeviceID: owner.device.ID, FileID: owner.fileID,
		CandidateTransports: []string{models.PlaybackTransportDirectHTTP},
	}, now)
	if err != nil {
		t.Fatalf("create limit ticket: %v", err)
	}
	if _, err := store.FinishPlaybackTicketProbe(
		owner.bindingWith(limitTicket.TicketID),
		limitTicket.Version,
		models.PlaybackTicketProbeResult{
			State:               models.PlaybackTicketReady,
			CandidateTransports: []string{models.PlaybackTransportDirectHTTP},
			SubtitleIDs:         tooMany,
			VideoIndex:          playbackVideoIndexPtr(0),
		},
		now.Add(time.Second),
	); !errors.Is(err, ErrInvalidPlaybackTicket) {
		t.Fatalf("oversized subtitle grant error = %v", err)
	}
}

func TestPlaybackSubtitleGrantMigrationIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-subtitle.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	_, err = legacy.Exec(`CREATE TABLE playback_tickets (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		ticket_id TEXT NOT NULL,
		user_id INTEGER NOT NULL,
		device_id INTEGER NOT NULL,
		file_id INTEGER NOT NULL,
		purpose TEXT NOT NULL DEFAULT 'playback',
		state TEXT NOT NULL DEFAULT 'probing',
		candidate_transports TEXT NOT NULL DEFAULT '[]',
		attempted_transports TEXT NOT NULL DEFAULT '[]',
		selected_transport TEXT NOT NULL DEFAULT '',
		media_summary_json TEXT NOT NULL DEFAULT '{}',
		client_capabilities_json TEXT NOT NULL DEFAULT '{}',
		failure_code TEXT NOT NULL DEFAULT '',
		version INTEGER NOT NULL DEFAULT 1,
		created_at TIMESTAMP NOT NULL,
		expires_at TIMESTAMP NOT NULL,
		updated_at TIMESTAMP NOT NULL,
		consumed_at TIMESTAMP,
		revoked_at TIMESTAMP
	)`)
	if err != nil {
		_ = legacy.Close()
		t.Fatalf("create legacy playback ticket table: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatalf("open migrated database: %v", err)
	}
	defer store.DB.Close()
	for attempt := 0; attempt < 2; attempt++ {
		if err := store.migrate(); err != nil {
			t.Fatalf("repeat subtitle migration %d: %v", attempt+1, err)
		}
	}

	if !playbackTableColumns(t, store, "playback_tickets")["subtitle_ids_json"] {
		t.Fatal("subtitle_ids_json migration column is missing")
	}
	var triggers int
	if err := store.DB.QueryRow(`SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'trigger'
			AND name = 'trg_playback_tickets_subtitle_grants_immutable'`).
		Scan(&triggers); err != nil {
		t.Fatalf("inspect subtitle grant trigger: %v", err)
	}
	if triggers != 1 {
		t.Fatalf("subtitle grant trigger count = %d, want 1", triggers)
	}
}

func TestPlaybackSubtitleSessionBindingAuthorizationAndTTL(t *testing.T) {
	store := openPlaybackTestStore(t)
	now := time.Date(2026, 10, 26, 19, 0, 0, 0, time.UTC)
	fixture := addPlaybackTestFixture(t, store, "subtitle-session", "subtitle-session", now)
	granted := addPlaybackSubtitle(
		t, store, fixture.fileID, "/subtitle-session/granted.srt", "subrip", false, 0)
	ungranted := addPlaybackSubtitle(
		t, store, fixture.fileID, "/subtitle-session/ungranted.srt", "subrip", false, 0)
	foreignFixture := addPlaybackTestFixture(
		t, store, "subtitle-session-other", "subtitle-session-other", now)
	foreign := addPlaybackSubtitle(
		t, store, foreignFixture.fileID, "/subtitle-session-other/foreign.srt",
		"subrip", false, 0)
	ticket := createConsumedPlaybackTicket(
		t, store, fixture, now, []int64{granted})

	for _, subtitleID := range []int64{ungranted, foreign} {
		if _, err := store.CreatePlaybackSubtitleSession(
			models.PlaybackSubtitleSessionRequest{
				Ticket:       playbackSubtitleTicketBinding(ticket),
				Transport:    models.PlaybackTransportDirectHTTP,
				SubtitleID:   subtitleID,
				StartSeconds: 12.5,
			},
			now.Add(3*time.Second),
		); !errors.Is(err, ErrInvalidPlaybackSubtitleSession) {
			t.Errorf("subtitle id %d session error = %v", subtitleID, err)
		}
	}

	grant, err := store.CreatePlaybackSubtitleSession(
		models.PlaybackSubtitleSessionRequest{
			Ticket:       playbackSubtitleTicketBinding(ticket),
			Transport:    models.PlaybackTransportDirectHTTP,
			SubtitleID:   granted,
			StartSeconds: 12.5,
		},
		now.Add(3*time.Second),
	)
	if err != nil {
		t.Fatalf("create subtitle session: %v", err)
	}
	if grant.Session.StartSeconds != 12.5 ||
		!grant.Session.AbsoluteExpiresAt.Equal(now.Add(3*time.Second).Add(
			6*time.Hour)) {
		t.Fatalf("subtitle session grant = %+v", grant.Session)
	}
	var storedHash string
	if err := store.DB.QueryRow(`SELECT capability_hash
		FROM playback_subtitle_sessions WHERE id = ?`, grant.Session.ID).
		Scan(&storedHash); err != nil {
		t.Fatalf("read subtitle capability hash: %v", err)
	}
	sum := sha256.Sum256([]byte(grant.Capability))
	if storedHash != hex.EncodeToString(sum[:]) || storedHash == grant.Capability {
		t.Fatalf("stored subtitle capability hash = %q", storedHash)
	}

	access, err := store.AuthorizePlaybackSubtitleSession(
		grant.Capability, false, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("authorize subtitle session: %v", err)
	}
	if access.Session.StartSeconds != 12.5 ||
		access.Session.SubtitleID != granted ||
		access.Session.Transport != models.PlaybackTransportDirectHTTP {
		t.Fatalf("authorized subtitle session = %+v", access.Session)
	}
	if _, err := store.RevokePlaybackSubtitleSession(
		grant.Capability, access.Session.Version-1, now.Add(2*time.Minute)); !errors.Is(
		err, ErrPlaybackSubtitleSessionConflict) {
		t.Fatalf("stale subtitle revoke error = %v", err)
	}
	if _, err := store.RevokePlaybackSubtitleSession(
		grant.Capability, access.Session.Version, now.Add(2*time.Minute)); err != nil {
		t.Fatalf("revoke subtitle session: %v", err)
	}
	if _, err := store.AuthorizePlaybackSubtitleSession(
		grant.Capability, false, now.Add(3*time.Minute)); !errors.Is(
		err, ErrPlaybackSubtitleSessionRevoked) {
		t.Fatalf("revoked subtitle authorization error = %v", err)
	}

	idleGrant, err := store.CreatePlaybackSubtitleSession(
		models.PlaybackSubtitleSessionRequest{
			Ticket:       playbackSubtitleTicketBinding(ticket),
			Transport:    models.PlaybackTransportDirectHTTP,
			SubtitleID:   granted,
			StartSeconds: 0,
		},
		now.Add(3*time.Second),
	)
	if err != nil {
		t.Fatalf("create idle subtitle session: %v", err)
	}
	if _, err := store.AuthorizePlaybackSubtitleSession(
		idleGrant.Capability, false,
		idleGrant.Session.IdleExpiresAt); !errors.Is(err, ErrPlaybackSubtitleSessionExpired) {
		t.Fatalf("idle subtitle expiry error = %v", err)
	}

	absoluteGrant, err := store.CreatePlaybackSubtitleSession(
		models.PlaybackSubtitleSessionRequest{
			Ticket:       playbackSubtitleTicketBinding(ticket),
			Transport:    models.PlaybackTransportDirectHTTP,
			SubtitleID:   granted,
			StartSeconds: 0,
		},
		now.Add(4*time.Second),
	)
	if err != nil {
		t.Fatalf("create absolute subtitle session: %v", err)
	}
	at := now.Add(4 * time.Second).Add(14 * time.Minute)
	for at.Before(absoluteGrant.Session.AbsoluteExpiresAt.Add(-14 * time.Minute)) {
		if _, err := store.AuthorizePlaybackSubtitleSession(
			absoluteGrant.Capability, false, at); err != nil {
			t.Fatalf("refresh absolute subtitle session at %v: %v", at, err)
		}
		at = at.Add(14 * time.Minute)
	}
	if _, err := store.AuthorizePlaybackSubtitleSession(
		absoluteGrant.Capability, false,
		absoluteGrant.Session.AbsoluteExpiresAt); !errors.Is(
		err, ErrPlaybackSubtitleSessionExpired) {
		t.Fatalf("absolute subtitle expiry error = %v", err)
	}
}

func TestPlaybackSubtitleSessionSupportsDirectDASHAndTransportRevocation(t *testing.T) {
	store := openPlaybackTestStore(t)
	now := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	fixture := addPlaybackTestFixture(
		t, store, "subtitle-direct-dash", "subtitle-direct-dash", now)
	subtitleID := addPlaybackSubtitle(
		t, store, fixture.fileID, "/subtitle-direct-dash/caption.srt",
		"subrip", false, 0)
	ticket, err := store.CreatePlaybackTicket(models.PlaybackTicketRequest{
		UserID: fixture.user.ID, DeviceID: fixture.device.ID, FileID: fixture.fileID,
		CandidateTransports: []string{models.PlaybackTransportDirectDASH},
	}, now)
	if err != nil {
		t.Fatalf("create direct-dash ticket: %v", err)
	}
	ready, err := store.FinishPlaybackTicketProbe(
		fixture.bindingWith(ticket.TicketID),
		ticket.Version,
		models.PlaybackTicketProbeResult{
			State:               models.PlaybackTicketReady,
			CandidateTransports: []string{models.PlaybackTransportDirectDASH},
			SubtitleIDs:         []int64{subtitleID},
			VideoIndex:          playbackVideoIndexPtr(0),
		},
		now.Add(time.Second),
	)
	if err != nil {
		t.Fatalf("finish direct-dash ticket probe: %v", err)
	}
	claimed := claimPlaybackTransport(
		t, store, fixture, ready, models.PlaybackTransportDirectDASH, now.Add(2*time.Second))

	grant, err := store.CreatePlaybackSubtitleSession(
		models.PlaybackSubtitleSessionRequest{
			Ticket:       playbackSubtitleTicketBinding(claimed),
			Transport:    models.PlaybackTransportDirectDASH,
			SubtitleID:   subtitleID,
			StartSeconds: 3.5,
		},
		now.Add(3*time.Second),
	)
	if err != nil {
		t.Fatalf("create direct-dash subtitle session: %v", err)
	}
	access, err := store.AuthorizePlaybackSubtitleSession(
		grant.Capability, false, now.Add(4*time.Second))
	if err != nil {
		t.Fatalf("authorize direct-dash subtitle session: %v", err)
	}
	if access.Session.Transport != models.PlaybackTransportDirectDASH ||
		access.Session.StartSeconds != 3.5 {
		t.Fatalf("direct-dash subtitle access = %+v", access.Session)
	}

	revoked, err := store.RevokePlaybackSubtitleSessionsForTransport(
		models.PlaybackTransportDirectDASH, now.Add(5*time.Second))
	if err != nil || revoked != 1 {
		t.Fatalf("revoke direct-dash subtitle sessions = %d, %v; want 1, nil",
			revoked, err)
	}
	if _, err := store.AuthorizePlaybackSubtitleSession(
		grant.Capability, false, now.Add(6*time.Second)); !errors.Is(
		err, ErrPlaybackSubtitleSessionRevoked) {
		t.Fatalf("revoked direct-dash subtitle authorization error = %v", err)
	}

	ticketGrant, err := store.CreatePlaybackSubtitleSession(
		models.PlaybackSubtitleSessionRequest{
			Ticket:       playbackSubtitleTicketBinding(claimed),
			Transport:    models.PlaybackTransportDirectDASH,
			SubtitleID:   subtitleID,
			StartSeconds: 0,
		},
		now.Add(7*time.Second),
	)
	if err != nil {
		t.Fatalf("create second direct-dash subtitle session: %v", err)
	}
	revoked, err = store.RevokePlaybackSubtitleSessionsForTicket(
		playbackSubtitleTicketBinding(claimed),
		models.PlaybackTransportDirectDASH,
		now.Add(8*time.Second),
	)
	if err != nil || revoked != 1 {
		t.Fatalf("ticket-scoped direct-dash subtitle cleanup = %d, %v; want 1, nil",
			revoked, err)
	}
	if _, err := store.AuthorizePlaybackSubtitleSession(
		ticketGrant.Capability, false, now.Add(9*time.Second)); !errors.Is(
		err, ErrPlaybackSubtitleSessionRevoked) {
		t.Fatalf("ticket-cleaned direct-dash subtitle error = %v", err)
	}
}

func TestPlaybackSubtitleSessionInvalidatedAfterTransportFallback(t *testing.T) {
	store := openPlaybackTestStore(t)
	now := time.Date(2026, 10, 27, 10, 30, 0, 0, time.UTC)
	fixture := addPlaybackTestFixture(
		t, store, "subtitle-dash-fallback", "subtitle-dash-fallback", now)
	subtitleID := addPlaybackSubtitle(
		t, store, fixture.fileID, "/subtitle-dash-fallback/caption.srt",
		"subrip", false, 0)
	candidates := []string{
		models.PlaybackTransportDirectDASH,
		models.PlaybackTransportServerHLS,
	}
	ticket, err := store.CreatePlaybackTicket(models.PlaybackTicketRequest{
		UserID: fixture.user.ID, DeviceID: fixture.device.ID, FileID: fixture.fileID,
		CandidateTransports: candidates,
	}, now)
	if err != nil {
		t.Fatalf("create fallback ticket: %v", err)
	}
	ready, err := store.FinishPlaybackTicketProbe(
		fixture.bindingWith(ticket.TicketID),
		ticket.Version,
		models.PlaybackTicketProbeResult{
			State:               models.PlaybackTicketReady,
			CandidateTransports: candidates,
			SubtitleIDs:         []int64{subtitleID},
			VideoIndex:          playbackVideoIndexPtr(0),
		},
		now.Add(time.Second),
	)
	if err != nil {
		t.Fatalf("finish fallback ticket probe: %v", err)
	}
	direct := claimPlaybackTransport(
		t, store, fixture, ready, models.PlaybackTransportDirectDASH,
		now.Add(2*time.Second))
	directGrant, err := store.CreatePlaybackSubtitleSession(
		models.PlaybackSubtitleSessionRequest{
			Ticket:       playbackSubtitleTicketBinding(direct),
			Transport:    models.PlaybackTransportDirectDASH,
			SubtitleID:   subtitleID,
			StartSeconds: 0,
		},
		now.Add(3*time.Second),
	)
	if err != nil {
		t.Fatalf("create direct-dash subtitle session: %v", err)
	}

	if err := store.ReleasePlaybackTransportClaim(
		context.Background(), playbackSubtitleTicketBinding(direct),
		direct.Version, models.PlaybackTransportDirectDASH, direct.ClaimToken,
		now.Add(4*time.Second)); err != nil {
		t.Fatalf("release direct-dash claim: %v", err)
	}
	reloaded, err := store.PlaybackTicketByID(
		playbackSubtitleTicketBinding(direct), now.Add(4*time.Second))
	if err != nil {
		t.Fatalf("reload fallback ticket: %v", err)
	}
	fallback := claimPlaybackTransport(
		t, store, fixture, reloaded, models.PlaybackTransportServerHLS,
		now.Add(5*time.Second))
	if fallback.SelectedTransport != models.PlaybackTransportServerHLS {
		t.Fatalf("fallback transport = %q, want server-hls", fallback.SelectedTransport)
	}

	if _, err := store.AuthorizePlaybackSubtitleSession(
		directGrant.Capability, false, now.Add(6*time.Second)); !errors.Is(
		err, ErrPlaybackSubtitleSessionNotFound) {
		t.Fatalf("stale direct-dash subtitle authorization error = %v", err)
	}
	hlsGrant, err := store.CreatePlaybackSubtitleSession(
		models.PlaybackSubtitleSessionRequest{
			Ticket:       playbackSubtitleTicketBinding(fallback),
			Transport:    models.PlaybackTransportServerHLS,
			SubtitleID:   subtitleID,
			StartSeconds: 0,
		},
		now.Add(6*time.Second),
	)
	if err != nil {
		t.Fatalf("create server-hls subtitle session after fallback: %v", err)
	}
	if _, err := store.AuthorizePlaybackSubtitleSession(
		hlsGrant.Capability, false, now.Add(7*time.Second)); err != nil {
		t.Fatalf("authorize server-hls subtitle session after fallback: %v", err)
	}
}

func TestPlaybackSubtitleSessionFailsClosedOnRevocationAndCascade(t *testing.T) {
	store := openPlaybackTestStore(t)
	now := time.Date(2026, 10, 26, 20, 0, 0, 0, time.UTC)
	fixture := addPlaybackTestFixture(t, store, "subtitle-revoke", "subtitle-revoke", now)
	subtitleID := addPlaybackSubtitle(
		t, store, fixture.fileID, "/subtitle-revoke/caption.srt", "subrip", false, 0)
	ticket := createConsumedPlaybackTicket(
		t, store, fixture, now, []int64{subtitleID})

	createSession := func(start time.Time) *models.PlaybackSubtitleSessionGrant {
		t.Helper()
		grant, err := store.CreatePlaybackSubtitleSession(
			models.PlaybackSubtitleSessionRequest{
				Ticket:       playbackSubtitleTicketBinding(ticket),
				Transport:    models.PlaybackTransportDirectHTTP,
				SubtitleID:   subtitleID,
				StartSeconds: 0,
			},
			start,
		)
		if err != nil {
			t.Fatalf("create subtitle session: %v", err)
		}
		return grant
	}
	sessionState := func(id int64) string {
		t.Helper()
		var state string
		if err := store.DB.QueryRow(`SELECT state
			FROM playback_subtitle_sessions WHERE id = ?`, id).Scan(&state); err != nil {
			t.Fatalf("read subtitle session state: %v", err)
		}
		return state
	}

	directRevoke := createSession(now.Add(3 * time.Second))
	if err := store.RevokeDevice(
		fixture.user.ID, fixture.device.PublicID, now.Add(time.Minute)); err != nil {
		t.Fatalf("revoke playback device: %v", err)
	}
	if state := sessionState(directRevoke.Session.ID); state !=
		models.PlaybackSubtitleSessionRevoked {
		t.Fatalf("device revocation left subtitle session state %q", state)
	}
	if _, err := store.AuthorizePlaybackSubtitleSession(
		directRevoke.Capability, false, now.Add(2*time.Minute)); !errors.Is(
		err, ErrPlaybackSubtitleSessionRevoked) {
		t.Fatalf("device-revoked subtitle session error = %v", err)
	}

	store = openPlaybackTestStore(t)
	fixture = addPlaybackTestFixture(t, store, "all-device-revoke", "all-device-revoke", now)
	subtitleID = addPlaybackSubtitle(
		t, store, fixture.fileID, "/all-device-revoke/caption.srt", "subrip", false, 0)
	ticket = createConsumedPlaybackTicket(
		t, store, fixture, now, []int64{subtitleID})
	allDeviceRevoke := createSession(now.Add(3 * time.Second))
	if _, err := store.RevokeAllDevicesForUser(
		fixture.user.ID, now.Add(time.Minute)); err != nil {
		t.Fatalf("revoke all playback devices: %v", err)
	}
	if state := sessionState(allDeviceRevoke.Session.ID); state !=
		models.PlaybackSubtitleSessionRevoked {
		t.Fatalf("all-device revocation left subtitle session state %q", state)
	}
	if _, err := store.AuthorizePlaybackSubtitleSession(
		allDeviceRevoke.Capability, false, now.Add(2*time.Minute)); !errors.Is(
		err, ErrPlaybackSubtitleSessionRevoked) {
		t.Fatalf("all-device revoked subtitle session error = %v", err)
	}

	store = openPlaybackTestStore(t)
	fixture = addPlaybackTestFixture(t, store, "ticket-revoke", "ticket-revoke", now)
	subtitleID = addPlaybackSubtitle(
		t, store, fixture.fileID, "/ticket-revoke/caption.srt", "subrip", false, 0)
	ticket = createConsumedPlaybackTicket(
		t, store, fixture, now, []int64{subtitleID})
	ticketRevoke := createSession(now.Add(3 * time.Second))
	if _, err := store.RevokePlaybackTicket(
		playbackSubtitleTicketBinding(ticket), ticket.Version, now.Add(time.Minute)); err != nil {
		t.Fatalf("revoke playback ticket: %v", err)
	}
	if state := sessionState(ticketRevoke.Session.ID); state !=
		models.PlaybackSubtitleSessionRevoked {
		t.Fatalf("ticket revocation left subtitle session state %q", state)
	}

	store = openPlaybackTestStore(t)
	fixture = addPlaybackTestFixture(t, store, "user-cascade", "user-cascade", now)
	subtitleID = addPlaybackSubtitle(
		t, store, fixture.fileID, "/user-cascade/caption.srt", "subrip", false, 0)
	ticket = createConsumedPlaybackTicket(
		t, store, fixture, now, []int64{subtitleID})
	userGrant := createSession(now.Add(3 * time.Second))
	if _, err := store.DB.Exec(`DELETE FROM users WHERE id = ?`, fixture.user.ID); err != nil {
		t.Fatalf("delete subtitle session user: %v", err)
	}
	var remaining int
	if err := store.DB.QueryRow(`SELECT COUNT(*)
		FROM playback_subtitle_sessions WHERE id = ?`, userGrant.Session.ID).
		Scan(&remaining); err != nil {
		t.Fatalf("count cascaded subtitle sessions: %v", err)
	}
	if remaining != 0 {
		t.Fatal("subtitle session survived its user deletion")
	}
	if _, err := store.AuthorizePlaybackSubtitleSession(
		userGrant.Capability, false, now.Add(2*time.Minute)); !errors.Is(
		err, ErrPlaybackSubtitleSessionNotFound) {
		t.Fatalf("cascaded subtitle authorization error = %v", err)
	}
}

func TestPlaybackSubtitleSessionsRevokeByTicketAndTransport(t *testing.T) {
	store := openPlaybackTestStore(t)
	now := time.Date(2026, 10, 26, 22, 0, 0, 0, time.UTC)
	fixture := addPlaybackTestFixture(
		t, store, "subtitle-issue-cleanup", "subtitle-issue-cleanup", now)
	subtitleID := addPlaybackSubtitle(
		t, store, fixture.fileID, "/subtitle-issue-cleanup/caption.srt",
		"subrip", false, 0)
	ticket := createConsumedPlaybackTicket(
		t, store, fixture, now, []int64{subtitleID})
	binding := playbackSubtitleTicketBinding(ticket)

	grant, err := store.CreatePlaybackSubtitleSession(
		models.PlaybackSubtitleSessionRequest{
			Ticket:       binding,
			Transport:    models.PlaybackTransportDirectHTTP,
			SubtitleID:   subtitleID,
			StartSeconds: 0,
		},
		now.Add(3*time.Second),
	)
	if err != nil {
		t.Fatalf("create subtitle session: %v", err)
	}
	if _, err := store.RevokePlaybackSubtitleSessionsForTicket(
		binding, "invalid", now.Add(4*time.Second)); !errors.Is(
		err, ErrInvalidPlaybackTransport) {
		t.Fatalf("invalid transport cleanup error = %v", err)
	}
	n, err := store.RevokePlaybackSubtitleSessionsForTicket(
		binding, models.PlaybackTransportDirectHTTP, now.Add(4*time.Second))
	if err != nil || n != 1 {
		t.Fatalf("ticket subtitle cleanup = %d, %v; want 1, nil", n, err)
	}
	if _, err := store.AuthorizePlaybackSubtitleSession(
		grant.Capability, false, now.Add(5*time.Second)); !errors.Is(
		err, ErrPlaybackSubtitleSessionRevoked) {
		t.Fatalf("cleaned subtitle authorization error = %v", err)
	}
	n, err = store.RevokePlaybackSubtitleSessionsForTicket(
		binding, models.PlaybackTransportDirectHTTP, now.Add(6*time.Second))
	if err != nil || n != 0 {
		t.Fatalf("repeated ticket subtitle cleanup = %d, %v", n, err)
	}
}
