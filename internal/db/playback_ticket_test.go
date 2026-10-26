package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"doriyamedia/internal/models"
)

type playbackTestFixture struct {
	store   *Store
	user    *models.User
	device  *models.Device
	fileID  int64
	binding models.PlaybackTicketBinding
}

func openPlaybackTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "playback.db"))
	if err != nil {
		t.Fatalf("open playback store: %v", err)
	}
	t.Cleanup(func() { _ = store.DB.Close() })
	return store
}

func playbackVideoIndexPtr(index int) *int {
	return &index
}

func TestPlaybackMigrationAndIdempotency(t *testing.T) {
	path := filepath.Join(t.TempDir(), "playback.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open playback store: %v", err)
	}
	for _, table := range []string{
		"playback_tickets", "playback_media_sessions",
	} {
		var count int
		if err := store.DB.QueryRow(`SELECT COUNT(*) FROM sqlite_master
			WHERE type = 'table' AND name = ?`, table).Scan(&count); err != nil {
			t.Fatalf("inspect table %s: %v", table, err)
		}
		if count != 1 {
			t.Errorf("table %s was not created", table)
		}
	}
	for _, object := range []struct {
		kind string
		name string
	}{
		{"index", "idx_playback_tickets_public_id"},
		{"index", "idx_playback_media_sessions_capability"},
		{"index", "idx_playback_media_sessions_active_transport"},
		{"trigger", "trg_playback_tickets_binding_immutable"},
		{"trigger", "trg_playback_media_sessions_binding_insert"},
		{"trigger", "trg_playback_media_sessions_params_immutable"},
	} {
		var count int
		if err := store.DB.QueryRow(`SELECT COUNT(*) FROM sqlite_master
			WHERE type = ? AND name = ?`, object.kind, object.name).Scan(&count); err != nil {
			t.Fatalf("inspect %s %s: %v", object.kind, object.name, err)
		}
		if count != 1 {
			t.Errorf("%s %s was not created", object.kind, object.name)
		}
	}

	ticketFKs := playbackForeignKeyTables(t, store, "playback_tickets")
	for _, table := range []string{"users", "devices", "files"} {
		if !ticketFKs[table] {
			t.Errorf("playback_tickets has no foreign key to %s", table)
		}
	}
	sessionFKs := playbackForeignKeyTables(t, store, "playback_media_sessions")
	for _, table := range []string{
		"playback_tickets", "users", "devices", "files",
	} {
		if !sessionFKs[table] {
			t.Errorf("playback_media_sessions has no foreign key to %s", table)
		}
	}

	ticketColumns := playbackTableColumns(t, store, "playback_tickets")
	for _, forbidden := range []string{
		"source_url", "source_headers", "headers_json", "capability",
	} {
		if ticketColumns[forbidden] {
			t.Errorf("playback_tickets stores forbidden column %s", forbidden)
		}
	}
	sessionColumns := playbackTableColumns(t, store, "playback_media_sessions")
	for _, forbidden := range []string{
		"source_url", "source_headers", "headers_json", "capability", "token",
	} {
		if sessionColumns[forbidden] {
			t.Errorf("playback_media_sessions stores forbidden column %s", forbidden)
		}
	}
	if !sessionColumns["capability_hash"] {
		t.Error("playback_media_sessions is missing capability_hash")
	}
	if !ticketColumns["claim_token"] || !ticketColumns["claim_expires_at"] {
		t.Error("playback_tickets is missing internal transport claim columns")
	}

	if err := store.migrate(); err != nil {
		t.Fatalf("repeat migration: %v", err)
	}
	fixture := addPlaybackTestFixture(t, store, "migration", "migration", time.Now().UTC())
	ticket, err := store.CreatePlaybackTicket(models.PlaybackTicketRequest{
		UserID: fixture.user.ID, DeviceID: fixture.device.ID, FileID: fixture.fileID,
		CandidateTransports: []string{models.PlaybackTransportServerHLS},
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("create ticket before reopen: %v", err)
	}
	if err := store.DB.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen migrated store: %v", err)
	}
	defer reopened.DB.Close()
	var count int
	if err := reopened.DB.QueryRow(`SELECT COUNT(*) FROM playback_tickets
		WHERE ticket_id = ?`, ticket.TicketID).Scan(&count); err != nil {
		t.Fatalf("read ticket after reopen: %v", err)
	}
	if count != 1 {
		t.Errorf("ticket did not survive idempotent reopen")
	}
}

func TestPlaybackVideoIndexMigrationIsIdempotentAndFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-video-index.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	for _, stmt := range []string{
		`CREATE TABLE playback_tickets (
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
		)`,
		`CREATE TABLE playback_media_sessions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			capability_hash TEXT NOT NULL,
			ticket_id TEXT NOT NULL,
			user_id INTEGER NOT NULL,
			device_id INTEGER NOT NULL,
			file_id INTEGER NOT NULL,
			purpose TEXT NOT NULL DEFAULT 'playback',
			transport TEXT NOT NULL,
			state TEXT NOT NULL DEFAULT 'active',
			mode TEXT NOT NULL,
			audio_ordinal INTEGER NOT NULL,
			start_seconds REAL NOT NULL,
			quality TEXT,
			transcode_height INTEGER,
			version INTEGER NOT NULL DEFAULT 1,
			created_at TIMESTAMP NOT NULL,
			last_used_at TIMESTAMP,
			idle_expires_at TIMESTAMP NOT NULL,
			absolute_expires_at TIMESTAMP NOT NULL,
			updated_at TIMESTAMP NOT NULL,
			revoked_at TIMESTAMP
		)`,
	} {
		if _, err := legacy.Exec(stmt); err != nil {
			_ = legacy.Close()
			t.Fatalf("create legacy playback schema: %v", err)
		}
	}
	now := time.Date(2026, 10, 26, 20, 0, 0, 0, time.UTC)
	_, err = legacy.Exec(`INSERT INTO playback_tickets
		(ticket_id, user_id, device_id, file_id, purpose, state,
		 candidate_transports, attempted_transports, selected_transport,
		 media_summary_json, client_capabilities_json, failure_code, version,
		 created_at, expires_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		"pt_legacy_video_index", 1, 2, 3, models.PlaybackPurposePlayback,
		models.PlaybackTicketReady, `["server-hls"]`, `["server-hls"]`,
		models.PlaybackTransportServerHLS, `{}`, `{}`, "", 2,
		now, now.Add(time.Hour), now)
	if err != nil {
		_ = legacy.Close()
		t.Fatalf("insert legacy ticket: %v", err)
	}
	_, err = legacy.Exec(`INSERT INTO playback_media_sessions
		(capability_hash, ticket_id, user_id, device_id, file_id, purpose,
		 transport, state, mode, audio_ordinal, start_seconds, version,
		 created_at, idle_expires_at, absolute_expires_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		"legacy-capability-hash", "pt_legacy_video_index", 1, 2, 3,
		models.PlaybackPurposePlayback, models.PlaybackTransportServerHLS,
		models.PlaybackMediaSessionActive, "remux", 0, 0, 1, now,
		now.Add(time.Hour), now.Add(time.Hour), now)
	if err != nil {
		_ = legacy.Close()
		t.Fatalf("insert legacy media session: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatalf("open migrated playback database: %v", err)
	}
	defer store.DB.Close()
	for attempt := 0; attempt < 2; attempt++ {
		if err := store.migrate(); err != nil {
			t.Fatalf("repeat playback video-index migration %d: %v", attempt+1, err)
		}
	}

	if !playbackTableColumns(t, store, "playback_tickets")["video_index"] {
		t.Fatal("playback_tickets.video_index migration column is missing")
	}
	if !playbackTableColumns(t, store, "playback_tickets")["claim_token"] ||
		!playbackTableColumns(t, store, "playback_tickets")["claim_expires_at"] {
		t.Fatal("playback_tickets claim lease migration columns are missing")
	}
	if !playbackTableColumns(t, store, "playback_media_sessions")["video_index"] {
		t.Fatal("playback_media_sessions.video_index migration column is missing")
	}
	var ticketVideoIndex, sessionVideoIndex int
	var legacyClaimToken string
	var legacyClaimExpiry sql.NullTime
	if err := store.DB.QueryRow(`SELECT video_index FROM playback_tickets
		WHERE ticket_id = ?`, "pt_legacy_video_index").Scan(&ticketVideoIndex); err != nil {
		t.Fatalf("read legacy ticket video index: %v", err)
	}
	if err := store.DB.QueryRow(`SELECT claim_token, claim_expires_at
		FROM playback_tickets WHERE ticket_id = ?`,
		"pt_legacy_video_index").Scan(&legacyClaimToken, &legacyClaimExpiry); err != nil {
		t.Fatalf("read legacy ticket claim lease: %v", err)
	}
	if err := store.DB.QueryRow(`SELECT video_index FROM playback_media_sessions
		WHERE ticket_id = ?`, "pt_legacy_video_index").Scan(&sessionVideoIndex); err != nil {
		t.Fatalf("read legacy media session video index: %v", err)
	}
	if ticketVideoIndex != models.PlaybackVideoIndexUnknown ||
		sessionVideoIndex != models.PlaybackVideoIndexUnknown {
		t.Fatalf("legacy video indexes = ticket %d, session %d; want both %d",
			ticketVideoIndex, sessionVideoIndex, models.PlaybackVideoIndexUnknown)
	}
	if legacyClaimToken != "" || legacyClaimExpiry.Valid {
		t.Fatalf("legacy claim lease = %q, %v; want empty",
			legacyClaimToken, legacyClaimExpiry)
	}
	if _, err := store.DB.Exec(`UPDATE playback_media_sessions
		SET video_index = 7 WHERE ticket_id = ?`, "pt_legacy_video_index"); err == nil {
		t.Fatal("legacy media session video index was mutable")
	}
	if _, err := store.DB.Exec(`UPDATE playback_tickets
		SET video_index = 7 WHERE ticket_id = ?`, "pt_legacy_video_index"); err == nil {
		t.Fatal("ready legacy ticket video index was mutable")
	}
}

func TestPlaybackTicketCASAndClaimOrder(t *testing.T) {
	store := openPlaybackTestStore(t)
	now := time.Date(2026, 10, 26, 15, 0, 0, 0, time.UTC)
	owner := addPlaybackTestFixture(t, store, "owner", "owner", now)
	other := addPlaybackTestFixture(t, store, "other", "other", now)

	summary := json.RawMessage(`{"container":"mkv","duration_seconds":120,"video":{"codec":"h264"}}`)
	ticket, err := store.CreatePlaybackTicket(models.PlaybackTicketRequest{
		UserID: owner.user.ID, DeviceID: owner.device.ID, FileID: owner.fileID,
		Purpose: models.PlaybackPurposePlayback,
		CandidateTransports: []string{
			models.PlaybackTransportDirectHTTP,
			models.PlaybackTransportServerProxy,
			models.PlaybackTransportServerHLS,
		},
		MediaSummary: summary,
	}, now)
	if err != nil {
		t.Fatalf("create ticket: %v", err)
	}
	if !strings.HasPrefix(ticket.TicketID, "pt_") {
		t.Errorf("ticket id = %q", ticket.TicketID)
	}
	if ticket.State != models.PlaybackTicketProbing || ticket.Version != 1 {
		t.Fatalf("new ticket = state %q version %d", ticket.State, ticket.Version)
	}
	if !ticket.ExpiresAt.Equal(now.Add(models.PlaybackTicketTTL)) {
		t.Errorf("ticket expiry = %v, want %v", ticket.ExpiresAt, now.Add(models.PlaybackTicketTTL))
	}
	if !ticket.CreatedAt.Equal(now) || ticket.UserID != owner.user.ID ||
		ticket.DeviceID != owner.device.ID || ticket.FileID != owner.fileID ||
		ticket.Purpose != models.PlaybackPurposePlayback {
		t.Fatalf("ticket binding = %+v", ticket)
	}

	wrongBindings := []models.PlaybackTicketBinding{
		{UserID: other.user.ID, DeviceID: owner.device.ID, FileID: owner.fileID, TicketID: ticket.TicketID},
		{UserID: owner.user.ID, DeviceID: other.device.ID, FileID: owner.fileID, TicketID: ticket.TicketID},
		{UserID: owner.user.ID, DeviceID: owner.device.ID, FileID: other.fileID, TicketID: ticket.TicketID},
	}
	for _, binding := range wrongBindings {
		if _, err := store.PlaybackTicketByID(binding, now); !errors.Is(err, ErrPlaybackTicketNotFound) {
			t.Errorf("cross binding %+v error = %v", binding, err)
		}
		if _, err := store.ClaimNextPlaybackTransport(
			context.Background(), binding, ticket.Version, now,
		); !errors.Is(err, ErrPlaybackTicketNotFound) {
			t.Errorf("cross claim %+v error = %v", binding, err)
		}
	}
	if _, err := store.FinishPlaybackTicketProbe(owner.bindingWith(ticket.TicketID), 99,
		models.PlaybackTicketProbeResult{
			State:               models.PlaybackTicketReady,
			CandidateTransports: []string{models.PlaybackTransportServerHLS},
			VideoIndex:          playbackVideoIndexPtr(0),
		}, now.Add(time.Second)); !errors.Is(err, ErrPlaybackTicketConflict) {
		t.Fatalf("wrong-version probe error = %v", err)
	}

	ready, err := store.FinishPlaybackTicketProbe(owner.bindingWith(ticket.TicketID), ticket.Version,
		models.PlaybackTicketProbeResult{
			State: models.PlaybackTicketReady,
			CandidateTransports: []string{
				models.PlaybackTransportDirectHTTP,
				models.PlaybackTransportServerProxy,
				models.PlaybackTransportServerHLS,
			},
			MediaSummary: summary,
			VideoIndex:   playbackVideoIndexPtr(0),
		}, now.Add(time.Second))
	if err != nil {
		t.Fatalf("finish probe: %v", err)
	}
	if ready.State != models.PlaybackTicketReady || ready.Version != 2 ||
		len(ready.CandidateTransports) != 3 || len(ready.AttemptedTransports) != 0 {
		t.Fatalf("ready ticket = %+v", ready)
	}
	if string(ready.MediaSummary) != string(summary) {
		t.Errorf("ready media summary = %s, want %s", ready.MediaSummary, summary)
	}
	if _, err := store.FinishPlaybackTicketProbe(owner.bindingWith(ticket.TicketID), 1,
		models.PlaybackTicketProbeResult{
			State:               models.PlaybackTicketReady,
			CandidateTransports: []string{models.PlaybackTransportServerHLS},
			VideoIndex:          playbackVideoIndexPtr(0),
		}, now.Add(2*time.Second)); !errors.Is(err, ErrPlaybackTicketConflict) {
		t.Fatalf("stale probe error = %v", err)
	}

	current, err := store.ClaimNextPlaybackTransport(
		context.Background(), owner.bindingWith(ticket.TicketID),
		ready.Version, now.Add(2*time.Second))
	if err != nil {
		t.Fatalf("claim direct: %v", err)
	}
	if current.SelectedTransport != models.PlaybackTransportDirectHTTP ||
		current.State != models.PlaybackTicketConsuming ||
		len(current.AttemptedTransports) != 1 || current.Version != 3 ||
		current.ConsumedAt == nil {
		t.Fatalf("first claim = %+v", current)
	}
	persisted, err := store.PlaybackTicketByID(owner.bindingWith(ticket.TicketID), now.Add(2*time.Second))
	if err != nil || persisted.SelectedTransport != models.PlaybackTransportDirectHTTP ||
		len(persisted.AttemptedTransports) != 1 ||
		persisted.AttemptedTransports[0] != models.PlaybackTransportDirectHTTP {
		t.Fatalf("persisted claim = %+v, err %v", persisted, err)
	}
	if _, err := store.ClaimNextPlaybackTransport(
		context.Background(), owner.bindingWith(ticket.TicketID),
		ready.Version, now.Add(3*time.Second)); !errors.Is(err, ErrPlaybackTicketConflict) {
		t.Fatalf("stale claim error = %v", err)
	}
	if err := store.ValidatePlaybackTransportIssuance(
		context.Background(), owner.bindingWith(ticket.TicketID), current.Version,
		current.SelectedTransport, current.ClaimToken, now.Add(3*time.Second)); err != nil {
		t.Fatalf("finalize direct claim: %v", err)
	}
	current, err = store.PlaybackTicketByID(
		owner.bindingWith(ticket.TicketID), now.Add(3*time.Second))
	if err != nil {
		t.Fatalf("reload finalized direct claim: %v", err)
	}

	current, err = store.ClaimNextPlaybackTransport(
		context.Background(), owner.bindingWith(ticket.TicketID),
		current.Version, now.Add(3*time.Second))
	if err != nil || current.SelectedTransport != models.PlaybackTransportServerProxy {
		t.Fatalf("claim proxy = %+v, err %v", current, err)
	}
	if err := store.ValidatePlaybackTransportIssuance(
		context.Background(), owner.bindingWith(ticket.TicketID), current.Version,
		current.SelectedTransport, current.ClaimToken, now.Add(4*time.Second)); err != nil {
		t.Fatalf("finalize proxy claim: %v", err)
	}
	current, err = store.PlaybackTicketByID(
		owner.bindingWith(ticket.TicketID), now.Add(4*time.Second))
	if err != nil {
		t.Fatalf("reload finalized proxy claim: %v", err)
	}
	current, err = store.ClaimNextPlaybackTransport(
		context.Background(), owner.bindingWith(ticket.TicketID),
		current.Version, now.Add(4*time.Second))
	if err != nil || current.SelectedTransport != models.PlaybackTransportServerHLS {
		t.Fatalf("claim hls = %+v, err %v", current, err)
	}
	if err := store.ValidatePlaybackTransportIssuance(
		context.Background(), owner.bindingWith(ticket.TicketID), current.Version,
		current.SelectedTransport, current.ClaimToken, now.Add(5*time.Second)); err != nil {
		t.Fatalf("finalize hls claim: %v", err)
	}
	current, err = store.PlaybackTicketByID(
		owner.bindingWith(ticket.TicketID), now.Add(5*time.Second))
	if err != nil {
		t.Fatalf("reload finalized hls claim: %v", err)
	}
	exhausted, err := store.ClaimNextPlaybackTransport(
		context.Background(), owner.bindingWith(ticket.TicketID),
		current.Version, now.Add(5*time.Second))
	if !errors.Is(err, ErrPlaybackTicketExhausted) || exhausted == nil ||
		exhausted.State != models.PlaybackTicketExhausted ||
		len(exhausted.AttemptedTransports) != 3 {
		t.Fatalf("exhaust ticket = %+v, err %v", exhausted, err)
	}

	for _, sensitive := range []json.RawMessage{
		json.RawMessage(`{"source_url":"https://example.test/secret"}`),
		json.RawMessage(`{"headers":{"Authorization":"secret"}}`),
		json.RawMessage(`{"nested":{"access_token":"secret"}}`),
		json.RawMessage(`{"poster":"https://example.test/secret.jpg"}`),
	} {
		if _, err := store.CreatePlaybackTicket(models.PlaybackTicketRequest{
			UserID: owner.user.ID, DeviceID: owner.device.ID, FileID: owner.fileID,
			MediaSummary: sensitive,
		}, now); !errors.Is(err, ErrInvalidPlaybackTicket) {
			t.Errorf("sensitive summary %s error = %v", sensitive, err)
		}
	}
	if _, err := store.CreatePlaybackTicket(models.PlaybackTicketRequest{
		UserID: owner.user.ID, DeviceID: owner.device.ID, FileID: owner.fileID,
		CandidateTransports: []string{
			models.PlaybackTransportServerProxy,
			models.PlaybackTransportServerProxy,
		},
	}, now); !errors.Is(err, ErrInvalidPlaybackTransport) {
		t.Errorf("duplicate transport error = %v", err)
	}

	unsupported, err := store.CreatePlaybackTicket(models.PlaybackTicketRequest{
		UserID: owner.user.ID, DeviceID: owner.device.ID, FileID: owner.fileID,
	}, now)
	if err != nil {
		t.Fatalf("create unsupported ticket: %v", err)
	}
	unsupported, err = store.FinishPlaybackTicketProbe(owner.bindingWith(unsupported.TicketID),
		unsupported.Version, models.PlaybackTicketProbeResult{
			State:       models.PlaybackTicketUnsupported,
			FailureCode: "no_playable_transport",
		}, now.Add(time.Second))
	if err != nil || unsupported.State != models.PlaybackTicketUnsupported ||
		len(unsupported.CandidateTransports) != 0 ||
		unsupported.FailureCode != "no_playable_transport" {
		t.Fatalf("unsupported ticket = %+v, err %v", unsupported, err)
	}
	failed, err := store.CreatePlaybackTicket(models.PlaybackTicketRequest{
		UserID: owner.user.ID, DeviceID: owner.device.ID, FileID: owner.fileID,
	}, now)
	if err != nil {
		t.Fatalf("create failed ticket: %v", err)
	}
	failed, err = store.FinishPlaybackTicketProbe(owner.bindingWith(failed.TicketID),
		failed.Version, models.PlaybackTicketProbeResult{
			State:               models.PlaybackTicketFailed,
			CandidateTransports: []string{models.PlaybackTransportServerHLS},
			FailureCode:         "source_unavailable",
		}, now.Add(time.Second))
	if err != nil || failed.State != models.PlaybackTicketFailed {
		t.Fatalf("failed ticket = %+v, err %v", failed, err)
	}
}

func TestPlaybackClaimRechecksSelectedTransportOverride(t *testing.T) {
	store := openPlaybackTestStore(t)
	now := time.Date(2026, 10, 26, 15, 30, 0, 0, time.UTC)
	fixture := addPlaybackTestFixture(t, store, "claim-override", "claim-override", now)
	if _, err := store.UpdateDeviceFeatureOverrides(
		fixture.device.PublicID,
		models.DeviceFeatureOverrides{DirectHTTPDisabled: true},
		fixture.user.ID,
		now,
	); err != nil {
		t.Fatalf("disable direct HTTP override: %v", err)
	}

	for _, tc := range []struct {
		name      string
		transport string
		wantErr   error
	}{
		{
			name:      "denied transport fails closed",
			transport: models.PlaybackTransportDirectHTTP,
			wantErr:   ErrDevicePlaybackDisabled,
		},
		{
			name:      "allowed sibling remains claimable",
			transport: models.PlaybackTransportServerProxy,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ticket, err := store.CreatePlaybackTicket(models.PlaybackTicketRequest{
				UserID: fixture.user.ID, DeviceID: fixture.device.ID, FileID: fixture.fileID,
				CandidateTransports: []string{tc.transport},
				MediaSummary:        json.RawMessage(`{}`),
			}, now)
			if err != nil {
				t.Fatalf("create ticket: %v", err)
			}
			ready, err := store.FinishPlaybackTicketProbe(
				fixture.bindingWith(ticket.TicketID),
				ticket.Version,
				models.PlaybackTicketProbeResult{
					State:               models.PlaybackTicketReady,
					CandidateTransports: []string{tc.transport},
					MediaSummary:        json.RawMessage(`{}`),
					VideoIndex:          playbackVideoIndexPtr(0),
				},
				now.Add(time.Second),
			)
			if err != nil {
				t.Fatalf("finish probe: %v", err)
			}

			claimed, err := store.ClaimNextPlaybackTransport(
				context.Background(),
				fixture.bindingWith(ticket.TicketID),
				ready.Version,
				now.Add(2*time.Second),
			)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("claim error = %v, want %v", err, tc.wantErr)
				}
				if claimed != nil {
					t.Fatalf("denied claim returned ticket: %+v", claimed)
				}
				persisted, readErr := store.PlaybackTicketByID(
					fixture.bindingWith(ticket.TicketID), now.Add(2*time.Second))
				if readErr != nil {
					t.Fatalf("read denied ticket: %v", readErr)
				}
				if persisted.State != models.PlaybackTicketReady ||
					persisted.Version != ready.Version ||
					len(persisted.AttemptedTransports) != 0 {
					t.Fatalf("denied claim mutated ticket: %+v", persisted)
				}
				return
			}
			if err != nil {
				t.Fatalf("claim allowed transport: %v", err)
			}
			if claimed == nil || claimed.SelectedTransport != tc.transport ||
				claimed.State != models.PlaybackTicketConsuming {
				t.Fatalf("claimed ticket = %+v", claimed)
			}
		})
	}
}

func TestPlaybackTicketExpiryAndRevocation(t *testing.T) {
	store := openPlaybackTestStore(t)
	now := time.Date(2026, 10, 26, 16, 0, 0, 0, time.UTC)
	fixture := addPlaybackTestFixture(t, store, "ttl", "ttl", now)
	ticket, err := store.CreatePlaybackTicket(models.PlaybackTicketRequest{
		UserID: fixture.user.ID, DeviceID: fixture.device.ID, FileID: fixture.fileID,
	}, now)
	if err != nil {
		t.Fatalf("create ticket: %v", err)
	}
	expired, err := store.PlaybackTicketByID(fixture.bindingWith(ticket.TicketID), now.Add(models.PlaybackTicketTTL))
	if err != nil {
		t.Fatalf("read expired ticket: %v", err)
	}
	if expired.State != models.PlaybackTicketExpired || expired.Version != 2 {
		t.Fatalf("expired ticket = %+v", expired)
	}
	if _, err := store.FinishPlaybackTicketProbe(fixture.bindingWith(ticket.TicketID), expired.Version,
		models.PlaybackTicketProbeResult{
			State:               models.PlaybackTicketReady,
			CandidateTransports: []string{models.PlaybackTransportServerProxy},
			VideoIndex:          playbackVideoIndexPtr(0),
		}, now.Add(models.PlaybackTicketTTL)); !errors.Is(err, ErrPlaybackTicketExpired) {
		t.Fatalf("probe expired ticket error = %v", err)
	}

	bulk, err := store.CreatePlaybackTicket(models.PlaybackTicketRequest{
		UserID: fixture.user.ID, DeviceID: fixture.device.ID, FileID: fixture.fileID,
	}, now)
	if err != nil {
		t.Fatalf("create bulk ticket: %v", err)
	}
	n, err := store.ExpirePlaybackTickets(now.Add(models.PlaybackTicketTTL))
	if err != nil || n != 1 {
		t.Fatalf("bulk expiry = %d, err %v", n, err)
	}
	bulk, err = store.PlaybackTicketByID(fixture.bindingWith(bulk.TicketID), now.Add(models.PlaybackTicketTTL))
	if err != nil || bulk.State != models.PlaybackTicketExpired {
		t.Fatalf("bulk ticket = %+v, err %v", bulk, err)
	}

	ready := createReadyPlaybackTicket(t, store, fixture, now.Add(time.Minute),
		[]string{models.PlaybackTransportServerProxy}, `{"container":"mp4"}`)
	claimed := claimPlaybackTransport(t, store, fixture, ready, models.PlaybackTransportServerProxy, now.Add(2*time.Minute))
	grant := createPlaybackMediaSession(t, store, fixture, claimed, models.PlaybackTransportServerProxy, now.Add(3*time.Minute))
	revoked, err := store.RevokePlaybackTicket(fixture.bindingWith(claimed.TicketID), claimed.Version, now.Add(4*time.Minute))
	if err != nil {
		t.Fatalf("revoke ticket: %v", err)
	}
	if revoked.State != models.PlaybackTicketRevoked || revoked.RevokedAt == nil {
		t.Fatalf("revoked ticket = %+v", revoked)
	}
	session, err := store.PlaybackMediaSessionByCapability(
		fixture.sessionBinding(claimed.TicketID, models.PlaybackTransportServerProxy),
		grant.Capability, now.Add(4*time.Minute))
	if err != nil || session.State != models.PlaybackMediaSessionRevoked {
		t.Fatalf("session after ticket revoke = %+v, err %v", session, err)
	}
	if _, err := store.RevokePlaybackTicket(fixture.bindingWith(claimed.TicketID), revoked.Version,
		now.Add(5*time.Minute)); !errors.Is(err, ErrPlaybackTicketRevoked) {
		t.Fatalf("second ticket revoke error = %v", err)
	}
}

func TestPlaybackClaimNextIsAtomic(t *testing.T) {
	store := openPlaybackTestStore(t)
	now := time.Date(2026, 10, 26, 16, 30, 0, 0, time.UTC)
	fixture := addPlaybackTestFixture(t, store, "claim-race", "claim-race", now)
	ticket := createReadyPlaybackTicket(t, store, fixture, now,
		[]string{models.PlaybackTransportServerProxy, models.PlaybackTransportServerHLS}, `{}`)

	type result struct {
		ticket *models.PlaybackTicket
		err    error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for range 2 {
		go func() {
			<-start
			claimed, err := store.ClaimNextPlaybackTransport(
				context.Background(),
				fixture.bindingWith(ticket.TicketID), ticket.Version, now.Add(time.Second))
			results <- result{ticket: claimed, err: err}
		}()
	}
	close(start)

	var successes, conflicts int
	for range 2 {
		outcome := <-results
		switch {
		case outcome.err == nil:
			successes++
			if outcome.ticket == nil ||
				outcome.ticket.SelectedTransport != models.PlaybackTransportServerProxy ||
				len(outcome.ticket.AttemptedTransports) != 1 {
				t.Errorf("winning claim = %+v", outcome.ticket)
			}
		case errors.Is(outcome.err, ErrPlaybackTicketConflict):
			conflicts++
		default:
			t.Errorf("concurrent claim error = %v", outcome.err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent claims = %d successes, %d conflicts", successes, conflicts)
	}
}

func TestPlaybackTransportClaimLeaseLifecycle(t *testing.T) {
	store := openPlaybackTestStore(t)
	now := time.Date(2026, 10, 26, 16, 45, 0, 0, time.UTC)
	fixture := addPlaybackTestFixture(t, store, "claim-lease", "claim-lease", now)
	ready := createReadyPlaybackTicket(t, store, fixture, now,
		[]string{
			models.PlaybackTransportDirectHTTP,
			models.PlaybackTransportServerProxy,
		}, `{}`)
	binding := fixture.bindingWith(ready.TicketID)

	claimed := claimPlaybackTransport(
		t, store, fixture, ready, models.PlaybackTransportDirectHTTP, now)
	if claimed.ClaimToken == "" || claimed.ClaimExpiresAt == nil ||
		!claimed.ClaimExpiresAt.After(now) {
		t.Fatalf("claim lease = token %q expiry %v", claimed.ClaimToken, claimed.ClaimExpiresAt)
	}
	if _, err := store.ClaimNextPlaybackTransport(
		context.Background(), binding, claimed.Version,
		now.Add(time.Second)); !errors.Is(
		err, ErrPlaybackTicketConflict) {
		t.Fatalf("second claim during active lease error = %v", err)
	}
	if err := store.ValidatePlaybackTransportIssuance(
		context.Background(), binding, claimed.Version, claimed.SelectedTransport,
		"pc_wrong", now.Add(time.Second)); !errors.Is(
		err, ErrPlaybackTicketConflict) {
		t.Fatalf("wrong-token finalize error = %v", err)
	}
	if err := store.ReleasePlaybackTransportClaim(
		context.Background(), binding, claimed.Version, claimed.SelectedTransport,
		claimed.ClaimToken, now.Add(time.Second)); err != nil {
		t.Fatalf("release active claim: %v", err)
	}

	released, err := store.PlaybackTicketByID(binding, now.Add(time.Second))
	if err != nil {
		t.Fatalf("reload released claim: %v", err)
	}
	if released.ClaimToken != "" || released.ClaimExpiresAt != nil ||
		released.Version != claimed.Version+1 {
		t.Fatalf("released ticket = %+v", released)
	}
	proxy := claimPlaybackTransport(
		t, store, fixture, released, models.PlaybackTransportServerProxy,
		now.Add(2*time.Second))
	if err := store.ValidatePlaybackTransportIssuance(
		context.Background(), binding, proxy.Version, proxy.SelectedTransport,
		proxy.ClaimToken, now.Add(3*time.Second)); err != nil {
		t.Fatalf("finalize proxy claim: %v", err)
	}
	finalized, err := store.PlaybackTicketByID(binding, now.Add(3*time.Second))
	if err != nil {
		t.Fatalf("reload finalized proxy claim: %v", err)
	}
	if finalized.ClaimToken != "" || finalized.ClaimExpiresAt != nil {
		t.Fatalf("finalized claim was not cleared: %+v", finalized)
	}
}

type playbackTicketMutationSnapshot struct {
	version             int64
	state               string
	attemptedTransports string
	selectedTransport   string
	claimToken          string
	claimExpiresAt      sql.NullTime
	updatedAt           time.Time
	mediaSessions       int
}

func readPlaybackTicketMutationSnapshot(
	t *testing.T,
	store *Store,
	binding models.PlaybackTicketBinding,
) playbackTicketMutationSnapshot {
	t.Helper()
	var snapshot playbackTicketMutationSnapshot
	if err := store.DB.QueryRow(`SELECT version, state, attempted_transports,
		selected_transport, claim_token, claim_expires_at, updated_at
		FROM playback_tickets
		WHERE ticket_id = ? AND user_id = ? AND device_id = ? AND file_id = ?`,
		binding.TicketID, binding.UserID, binding.DeviceID, binding.FileID,
	).Scan(
		&snapshot.version,
		&snapshot.state,
		&snapshot.attemptedTransports,
		&snapshot.selectedTransport,
		&snapshot.claimToken,
		&snapshot.claimExpiresAt,
		&snapshot.updatedAt,
	); err != nil {
		t.Fatalf("read playback ticket mutation snapshot: %v", err)
	}
	if err := store.DB.QueryRow(`SELECT COUNT(*) FROM playback_media_sessions
		WHERE ticket_id = ?`, binding.TicketID).Scan(&snapshot.mediaSessions); err != nil {
		t.Fatalf("count playback media sessions: %v", err)
	}
	return snapshot
}

func TestPlaybackTicketMutationsFailClosedWithCanceledContext(t *testing.T) {
	now := time.Date(2026, 10, 26, 16, 47, 0, 0, time.UTC)
	newReadyFixture := func(t *testing.T) (
		*Store,
		playbackTestFixture,
		*models.PlaybackTicket,
		models.PlaybackTicketBinding,
	) {
		t.Helper()
		store := openPlaybackTestStore(t)
		fixture := addPlaybackTestFixture(t, store, "canceled-context", "canceled-context", now)
		ready := createReadyPlaybackTicket(t, store, fixture, now,
			[]string{models.PlaybackTransportServerProxy}, `{}`)
		return store, fixture, ready, fixture.bindingWith(ready.TicketID)
	}
	newClaimedFixture := func(t *testing.T) (
		*Store,
		models.PlaybackTicketBinding,
		*models.PlaybackTicket,
	) {
		t.Helper()
		store, fixture, ready, binding := newReadyFixture(t)
		claimed := claimPlaybackTransport(
			t, store, fixture, ready, models.PlaybackTransportServerProxy,
			now.Add(time.Second))
		return store, binding, claimed
	}
	canceledContext := func() context.Context {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return ctx
	}
	assertUnchanged := func(
		t *testing.T,
		before, after playbackTicketMutationSnapshot,
	) {
		t.Helper()
		if after != before {
			t.Fatalf("canceled mutation changed ticket\nbefore: %+v\nafter:  %+v",
				before, after)
		}
	}

	t.Run("claim", func(t *testing.T) {
		store, _, ready, binding := newReadyFixture(t)
		before := readPlaybackTicketMutationSnapshot(t, store, binding)
		claimed, err := store.ClaimNextPlaybackTransport(
			canceledContext(), binding, ready.Version, now.Add(time.Second))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled claim error = %v, want context.Canceled", err)
		}
		if claimed != nil {
			t.Fatalf("canceled claim returned ticket: %+v", claimed)
		}
		assertUnchanged(t, before, readPlaybackTicketMutationSnapshot(t, store, binding))
	})

	t.Run("media session", func(t *testing.T) {
		store, binding, claimed := newClaimedFixture(t)
		before := readPlaybackTicketMutationSnapshot(t, store, binding)
		if before.mediaSessions != 0 {
			t.Fatalf("fixture has %d media sessions before canceled create", before.mediaSessions)
		}
		grant, err := store.CreatePlaybackMediaSession(
			canceledContext(),
			models.PlaybackMediaSessionRequest{
				Ticket:       binding,
				Transport:    claimed.SelectedTransport,
				Mode:         "remux",
				AudioOrdinal: 0,
				StartSeconds: 0,
			},
			now.Add(2*time.Second),
		)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled media session error = %v, want context.Canceled", err)
		}
		if grant != nil {
			t.Fatalf("canceled media session returned grant: %+v", grant)
		}
		assertUnchanged(t, before, readPlaybackTicketMutationSnapshot(t, store, binding))
	})

	t.Run("validate issuance", func(t *testing.T) {
		store, binding, claimed := newClaimedFixture(t)
		before := readPlaybackTicketMutationSnapshot(t, store, binding)
		err := store.ValidatePlaybackTransportIssuance(
			canceledContext(), binding, claimed.Version,
			claimed.SelectedTransport, claimed.ClaimToken, now.Add(2*time.Second))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled validation error = %v, want context.Canceled", err)
		}
		assertUnchanged(t, before, readPlaybackTicketMutationSnapshot(t, store, binding))
	})

	t.Run("release claim", func(t *testing.T) {
		store, binding, claimed := newClaimedFixture(t)
		before := readPlaybackTicketMutationSnapshot(t, store, binding)
		err := store.ReleasePlaybackTransportClaim(
			canceledContext(), binding, claimed.Version,
			claimed.SelectedTransport, claimed.ClaimToken, now.Add(2*time.Second))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled release error = %v, want context.Canceled", err)
		}
		assertUnchanged(t, before, readPlaybackTicketMutationSnapshot(t, store, binding))
	})
}

func TestPlaybackTransportClaimLeaseExpires(t *testing.T) {
	store := openPlaybackTestStore(t)
	now := time.Date(2026, 10, 26, 16, 50, 0, 0, time.UTC)
	fixture := addPlaybackTestFixture(t, store, "claim-expiry", "claim-expiry", now)
	ready := createReadyPlaybackTicket(t, store, fixture, now,
		[]string{
			models.PlaybackTransportDirectHTTP,
			models.PlaybackTransportServerProxy,
		}, `{}`)
	binding := fixture.bindingWith(ready.TicketID)
	claimed := claimPlaybackTransport(
		t, store, fixture, ready, models.PlaybackTransportDirectHTTP, now)

	expiredAt := claimed.ClaimExpiresAt.Add(time.Second)
	proxy, err := store.ClaimNextPlaybackTransport(
		context.Background(), binding, claimed.Version, expiredAt)
	if err != nil {
		t.Fatalf("claim after lease expiry: %v", err)
	}
	if proxy.SelectedTransport != models.PlaybackTransportServerProxy ||
		proxy.ClaimToken == "" || proxy.ClaimToken == claimed.ClaimToken {
		t.Fatalf("claim after expiry = %+v", proxy)
	}
	if err := store.ValidatePlaybackTransportIssuance(
		context.Background(), binding, claimed.Version, claimed.SelectedTransport,
		claimed.ClaimToken, expiredAt); !errors.Is(
		err, ErrPlaybackTicketConflict) {
		t.Fatalf("stale-token finalize error = %v", err)
	}
}

func TestPlaybackMediaSessionHashTTLAndImmutability(t *testing.T) {
	store := openPlaybackTestStore(t)
	now := time.Date(2026, 10, 26, 17, 0, 0, 0, time.UTC)
	fixture := addPlaybackTestFixture(t, store, "session", "session", now)
	other := addPlaybackTestFixture(t, store, "session-other", "session-other", now)
	ready := createReadyPlaybackTicket(t, store, fixture, now,
		[]string{models.PlaybackTransportServerProxy, models.PlaybackTransportServerHLS},
		`{"container":"mkv","duration_seconds":300}`)
	claimed := claimPlaybackTransport(t, store, fixture, ready, models.PlaybackTransportServerProxy, now.Add(time.Second))

	quality := "1080p"
	height := 1080
	if _, err := store.CreatePlaybackMediaSession(
		context.Background(),
		models.PlaybackMediaSessionRequest{
			Ticket: fixture.bindingWith(claimed.TicketID), Transport: models.PlaybackTransportServerHLS,
			Mode: "remux",
		},
		now.Add(2*time.Second),
	); !errors.Is(err, ErrInvalidPlaybackMediaSession) {
		t.Errorf("unclaimed transport session error = %v", err)
	}
	grant, err := store.CreatePlaybackMediaSession(
		context.Background(),
		models.PlaybackMediaSessionRequest{
			Ticket:          fixture.bindingWith(claimed.TicketID),
			Transport:       models.PlaybackTransportServerProxy,
			Mode:            "remux",
			AudioOrdinal:    2,
			StartSeconds:    42.5,
			Quality:         &quality,
			TranscodeHeight: &height,
		},
		now.Add(2*time.Second),
	)
	if err != nil {
		t.Fatalf("create media session: %v", err)
	}
	if !strings.HasPrefix(grant.Capability, "ms_") || grant.Session == nil {
		t.Fatalf("media grant = %+v", grant)
	}
	session := grant.Session
	if session.State != models.PlaybackMediaSessionActive || session.Version != 1 ||
		session.Mode != "remux" || session.AudioOrdinal != 2 ||
		session.VideoIndex != 4 ||
		session.StartSeconds != 42.5 || session.Quality == nil ||
		*session.Quality != "1080p" || session.TranscodeHeight == nil ||
		*session.TranscodeHeight != 1080 {
		t.Fatalf("session parameters = %+v", session)
	}
	if !session.IdleExpiresAt.Equal(now.Add(2 * time.Second).Add(models.PlaybackMediaSessionIdleTTL)) {
		t.Errorf("idle expiry = %v", session.IdleExpiresAt)
	}
	if !session.AbsoluteExpiresAt.Equal(now.Add(2 * time.Second).Add(models.PlaybackMediaSessionAbsoluteTTL)) {
		t.Errorf("absolute expiry = %v", session.AbsoluteExpiresAt)
	}

	var capabilityHash string
	if err := store.DB.QueryRow(`SELECT capability_hash FROM playback_media_sessions
		WHERE ticket_id = ? AND transport = ?`, claimed.TicketID,
		models.PlaybackTransportServerProxy).Scan(&capabilityHash); err != nil {
		t.Fatalf("read stored capability hash: %v", err)
	}
	sum := sha256.Sum256([]byte(grant.Capability))
	if capabilityHash != hex.EncodeToString(sum[:]) || capabilityHash == grant.Capability {
		t.Fatalf("stored capability hash = %q", capabilityHash)
	}
	if _, err := store.CreatePlaybackMediaSession(
		context.Background(),
		models.PlaybackMediaSessionRequest{
			Ticket: fixture.bindingWith(claimed.TicketID), Transport: models.PlaybackTransportServerProxy,
			Mode: "remux",
		},
		now.Add(3*time.Second),
	); !errors.Is(err, ErrPlaybackMediaSessionConflict) {
		t.Errorf("duplicate active media session error = %v", err)
	}

	binding := fixture.sessionBinding(claimed.TicketID, models.PlaybackTransportServerProxy)
	read, err := store.PlaybackMediaSessionByCapability(binding, grant.Capability, now.Add(3*time.Minute))
	if err != nil || read.ID != session.ID {
		t.Fatalf("read media session = %+v, err %v", read, err)
	}
	if read.Mode != "remux" || read.AudioOrdinal != 2 || read.StartSeconds != 42.5 ||
		read.VideoIndex != 4 ||
		read.Quality == nil || *read.Quality != "1080p" ||
		read.TranscodeHeight == nil || *read.TranscodeHeight != 1080 {
		t.Fatalf("read media session parameters = %+v", read)
	}
	wrongBindings := []models.PlaybackMediaSessionBinding{
		{Ticket: other.bindingWith(claimed.TicketID), Transport: models.PlaybackTransportServerProxy},
		{Ticket: models.PlaybackTicketBinding{UserID: fixture.user.ID, DeviceID: other.device.ID, FileID: fixture.fileID, TicketID: claimed.TicketID}, Transport: models.PlaybackTransportServerProxy},
		{Ticket: models.PlaybackTicketBinding{UserID: fixture.user.ID, DeviceID: fixture.device.ID, FileID: other.fileID, TicketID: claimed.TicketID}, Transport: models.PlaybackTransportServerProxy},
		{Ticket: fixture.bindingWith("pt_wrong"), Transport: models.PlaybackTransportServerProxy},
		{Ticket: fixture.bindingWith(claimed.TicketID), Transport: models.PlaybackTransportServerHLS},
	}
	for _, wrong := range wrongBindings {
		if _, err := store.PlaybackMediaSessionByCapability(wrong, grant.Capability,
			now.Add(3*time.Minute)); !errors.Is(err, ErrPlaybackMediaSessionNotFound) {
			t.Errorf("cross session binding %+v error = %v", wrong, err)
		}
	}
	if _, err := store.PlaybackMediaSessionByCapability(binding, "ms_wrong", now.Add(3*time.Minute)); !errors.Is(err, ErrPlaybackMediaSessionNotFound) {
		t.Errorf("wrong capability error = %v", err)
	}

	touched, err := store.TouchPlaybackMediaSession(binding, grant.Capability, now.Add(10*time.Minute))
	if err != nil {
		t.Fatalf("touch session: %v", err)
	}
	if touched.Version != 2 || touched.LastUsedAt == nil ||
		!touched.LastUsedAt.Equal(now.Add(10*time.Minute)) ||
		!touched.IdleExpiresAt.Equal(now.Add(10*time.Minute).Add(models.PlaybackMediaSessionIdleTTL)) ||
		!touched.AbsoluteExpiresAt.Equal(session.AbsoluteExpiresAt) {
		t.Fatalf("touched session = %+v", touched)
	}

	if _, err := store.DB.Exec(`UPDATE playback_media_sessions SET mode = 'transcode' WHERE id = ?`,
		session.ID); err == nil {
		t.Error("immutable mode update unexpectedly succeeded")
	}
	if _, err := store.DB.Exec(`UPDATE playback_media_sessions SET video_index = 7 WHERE id = ?`,
		session.ID); err == nil {
		t.Error("immutable video index update unexpectedly succeeded")
	}
	if _, err := store.DB.Exec(`UPDATE playback_media_sessions SET ticket_id = ? WHERE id = ?`,
		"pt_other", session.ID); err == nil {
		t.Error("immutable session binding update unexpectedly succeeded")
	}
	if _, err := store.DB.Exec(`UPDATE playback_tickets SET user_id = ? WHERE id = ?`,
		other.user.ID, claimed.ID); err == nil {
		t.Error("immutable ticket binding update unexpectedly succeeded")
	}
	if _, err := store.DB.Exec(`UPDATE playback_tickets SET expires_at = ? WHERE id = ?`,
		claimed.ExpiresAt.Add(time.Hour), claimed.ID); err == nil {
		t.Error("immutable ticket expiry update unexpectedly succeeded")
	}
	if _, err := store.DB.Exec(`UPDATE playback_media_sessions SET absolute_expires_at = ? WHERE id = ?`,
		session.AbsoluteExpiresAt.Add(time.Hour), session.ID); err == nil {
		t.Error("immutable session expiry update unexpectedly succeeded")
	}

	idleExpiredAt := touched.IdleExpiresAt
	expired, err := store.PlaybackMediaSessionByCapability(binding, grant.Capability, idleExpiredAt)
	if err != nil || expired.State != models.PlaybackMediaSessionExpired {
		t.Fatalf("idle-expired session = %+v, err %v", expired, err)
	}
	if _, err := store.TouchPlaybackMediaSession(binding, grant.Capability, idleExpiredAt); !errors.Is(err, ErrPlaybackMediaSessionExpired) {
		t.Errorf("touch idle-expired session error = %v", err)
	}

	t.Run("absolute expiry caps idle and cannot slide", func(t *testing.T) {
		fixture := addPlaybackTestFixture(t, store, "absolute", "absolute", now)
		ready := createReadyPlaybackTicket(t, store, fixture, now,
			[]string{models.PlaybackTransportServerHLS}, `{}`)
		claimed := claimPlaybackTransport(t, store, fixture, ready, models.PlaybackTransportServerHLS, now)
		grant := createPlaybackMediaSession(t, store, fixture, claimed, models.PlaybackTransportServerHLS, now)
		if _, err := store.DB.Exec(`UPDATE playback_media_sessions
			SET idle_expires_at = ?, version = version + 1 WHERE id = ?`,
			grant.Session.AbsoluteExpiresAt, grant.Session.ID); err != nil {
			t.Fatalf("extend test idle window: %v", err)
		}
		readyForAbsolute := *grant.Session
		readyForAbsolute.Version = 2
		touched, err := store.TouchPlaybackMediaSession(
			fixture.sessionBinding(claimed.TicketID, models.PlaybackTransportServerHLS),
			grant.Capability, now.Add(models.PlaybackMediaSessionAbsoluteTTL-30*time.Second))
		if err != nil {
			t.Fatalf("touch near absolute expiry: %v", err)
		}
		if !touched.IdleExpiresAt.Equal(touched.AbsoluteExpiresAt) {
			t.Errorf("idle expiry %v did not cap at absolute %v",
				touched.IdleExpiresAt, touched.AbsoluteExpiresAt)
		}
		if _, err := store.TouchPlaybackMediaSession(
			fixture.sessionBinding(claimed.TicketID, models.PlaybackTransportServerHLS),
			grant.Capability, touched.AbsoluteExpiresAt); !errors.Is(err, ErrPlaybackMediaSessionExpired) {
			t.Errorf("touch at absolute expiry error = %v", err)
		}
	})
}

func TestRevokePlaybackMediaSessionsForTransportScopesActiveSessions(t *testing.T) {
	store := openPlaybackTestStore(t)
	now := time.Date(2026, 10, 26, 17, 30, 0, 0, time.UTC)
	fixture := addPlaybackTestFixture(t, store, "transport-revoke", "transport-revoke", now)

	proxyReady := createReadyPlaybackTicket(t, store, fixture, now,
		[]string{models.PlaybackTransportServerProxy}, `{}`)
	proxyClaimed := claimPlaybackTransport(
		t, store, fixture, proxyReady, models.PlaybackTransportServerProxy, now.Add(time.Second))
	proxyGrant := createPlaybackMediaSession(
		t, store, fixture, proxyClaimed, models.PlaybackTransportServerProxy, now.Add(2*time.Second))

	hlsReady := createReadyPlaybackTicket(t, store, fixture, now.Add(3*time.Second),
		[]string{models.PlaybackTransportServerHLS}, `{}`)
	hlsClaimed := claimPlaybackTransport(
		t, store, fixture, hlsReady, models.PlaybackTransportServerHLS, now.Add(4*time.Second))
	hlsGrant := createPlaybackMediaSession(
		t, store, fixture, hlsClaimed, models.PlaybackTransportServerHLS, now.Add(5*time.Second))

	if _, err := store.RevokePlaybackMediaSessionsForTransport(
		models.PlaybackTransportDirectHTTP, now.Add(6*time.Second)); !errors.Is(err, ErrInvalidPlaybackTransport) {
		t.Fatalf("direct transport revoke error = %v", err)
	}
	revoked, err := store.RevokePlaybackMediaSessionsForTransport(
		models.PlaybackTransportServerProxy, now.Add(7*time.Second))
	if err != nil || revoked != 1 {
		t.Fatalf("revoke proxy sessions = %d, %v; want 1", revoked, err)
	}
	if again, err := store.RevokePlaybackMediaSessionsForTransport(
		models.PlaybackTransportServerProxy, now.Add(8*time.Second)); err != nil || again != 0 {
		t.Fatalf("repeat proxy revoke = %d, %v; want 0", again, err)
	}

	var proxyState, hlsState string
	if err := store.DB.QueryRow(`SELECT state FROM playback_media_sessions
		WHERE ticket_id = ? AND transport = ?`,
		proxyClaimed.TicketID, models.PlaybackTransportServerProxy).Scan(&proxyState); err != nil {
		t.Fatalf("read proxy state: %v", err)
	}
	if err := store.DB.QueryRow(`SELECT state FROM playback_media_sessions
		WHERE ticket_id = ? AND transport = ?`,
		hlsClaimed.TicketID, models.PlaybackTransportServerHLS).Scan(&hlsState); err != nil {
		t.Fatalf("read HLS state: %v", err)
	}
	if proxyState != models.PlaybackMediaSessionRevoked {
		t.Errorf("proxy state = %q, want revoked", proxyState)
	}
	if hlsState != models.PlaybackMediaSessionActive {
		t.Errorf("sibling HLS state = %q, want active", hlsState)
	}

	if _, err := store.AuthorizePlaybackMediaSession(
		proxyGrant.Capability, false, now.Add(9*time.Second)); !errors.Is(err, ErrPlaybackMediaSessionRevoked) {
		t.Errorf("revoked proxy capability authorization error = %v", err)
	}
	if _, err := store.AuthorizePlaybackMediaSession(
		hlsGrant.Capability, false, now.Add(9*time.Second)); err != nil {
		t.Errorf("sibling HLS capability authorization failed: %v", err)
	}
}

func TestPlaybackMediaSessionRevocationAndCascades(t *testing.T) {
	t.Run("revoke session cas", func(t *testing.T) {
		store := openPlaybackTestStore(t)
		now := time.Date(2026, 10, 26, 18, 0, 0, 0, time.UTC)
		fixture := addPlaybackTestFixture(t, store, "revoke", "revoke", now)
		ready := createReadyPlaybackTicket(t, store, fixture, now,
			[]string{models.PlaybackTransportServerProxy}, `{}`)
		claimed := claimPlaybackTransport(t, store, fixture, ready, models.PlaybackTransportServerProxy, now)
		grant := createPlaybackMediaSession(t, store, fixture, claimed, models.PlaybackTransportServerProxy, now)
		binding := fixture.sessionBinding(claimed.TicketID, models.PlaybackTransportServerProxy)
		revoked, err := store.RevokePlaybackMediaSession(binding, grant.Capability, 1, now.Add(time.Minute))
		if err != nil {
			t.Fatalf("revoke media session: %v", err)
		}
		if revoked.State != models.PlaybackMediaSessionRevoked || revoked.RevokedAt == nil ||
			revoked.Version != 2 {
			t.Fatalf("revoked session = %+v", revoked)
		}
		read, err := store.PlaybackMediaSessionByCapability(binding, grant.Capability, now.Add(2*time.Minute))
		if err != nil || read.State != models.PlaybackMediaSessionRevoked {
			t.Fatalf("read revoked session = %+v, err %v", read, err)
		}
		if _, err := store.TouchPlaybackMediaSession(binding, grant.Capability,
			now.Add(2*time.Minute)); !errors.Is(err, ErrPlaybackMediaSessionRevoked) {
			t.Errorf("touch revoked session error = %v", err)
		}
		if _, err := store.RevokePlaybackMediaSession(binding, grant.Capability, revoked.Version,
			now.Add(3*time.Minute)); !errors.Is(err, ErrPlaybackMediaSessionRevoked) {
			t.Errorf("second media revoke error = %v", err)
		}
	})

	for _, tc := range []struct {
		name   string
		delete func(t *testing.T, store *Store, fixture playbackTestFixture)
	}{
		{
			name: "user deletion",
			delete: func(t *testing.T, store *Store, fixture playbackTestFixture) {
				t.Helper()
				if err := store.DeleteUser(fixture.user.ID); err != nil {
					t.Fatalf("delete user: %v", err)
				}
			},
		},
		{
			name: "device deletion",
			delete: func(t *testing.T, store *Store, fixture playbackTestFixture) {
				t.Helper()
				if _, err := store.DB.Exec(`DELETE FROM devices WHERE id = ?`, fixture.device.ID); err != nil {
					t.Fatalf("delete device: %v", err)
				}
			},
		},
		{
			name: "file deletion",
			delete: func(t *testing.T, store *Store, fixture playbackTestFixture) {
				t.Helper()
				if _, err := store.DB.Exec(`DELETE FROM files WHERE id = ?`, fixture.fileID); err != nil {
					t.Fatalf("delete file: %v", err)
				}
			},
		},
	} {
		t.Run(tc.name+" cascades", func(t *testing.T) {
			store := openPlaybackTestStore(t)
			now := time.Date(2026, 10, 26, 19, 0, 0, 0, time.UTC)
			fixture := addPlaybackTestFixture(t, store, tc.name, strings.ReplaceAll(tc.name, " ", "-"), now)
			ready := createReadyPlaybackTicket(t, store, fixture, now,
				[]string{models.PlaybackTransportServerHLS}, `{}`)
			claimed := claimPlaybackTransport(t, store, fixture, ready, models.PlaybackTransportServerHLS, now)
			_ = createPlaybackMediaSession(t, store, fixture, claimed, models.PlaybackTransportServerHLS, now)

			tc.delete(t, store, fixture)
			for _, query := range []string{
				`SELECT COUNT(*) FROM playback_tickets WHERE ticket_id = ?`,
				`SELECT COUNT(*) FROM playback_media_sessions WHERE ticket_id = ?`,
			} {
				var count int
				if err := store.DB.QueryRow(query, claimed.TicketID).Scan(&count); err != nil {
					t.Fatalf("cascade count: %v", err)
				}
				if count != 0 {
					t.Errorf("%s left %d rows", query, count)
				}
			}
		})
	}
}

func TestDeviceRevocationRevokesPlayback(t *testing.T) {
	t.Run("single device leaves sibling playback active", func(t *testing.T) {
		store := openPlaybackTestStore(t)
		now := time.Date(2026, 10, 26, 20, 0, 0, 0, time.UTC)
		target := addPlaybackTestFixture(t, store, "single-target", "single-target", now)
		siblingDevice := addPlaybackTestDeviceForUser(
			t, store, target.user.ID, "single-sibling", "single-sibling", now)
		sibling := playbackTestFixture{
			store: store, user: target.user, device: siblingDevice,
			fileID: target.fileID,
		}
		sibling.binding = sibling.bindingWith("")

		probing, err := store.CreatePlaybackTicket(models.PlaybackTicketRequest{
			UserID: target.user.ID, DeviceID: target.device.ID, FileID: target.fileID,
			CandidateTransports: []string{models.PlaybackTransportServerProxy},
		}, now)
		if err != nil {
			t.Fatalf("create probing ticket: %v", err)
		}
		ready := createReadyPlaybackTicket(t, store, target, now,
			[]string{models.PlaybackTransportServerProxy}, `{}`)
		consuming, grant := createActivePlaybackCapability(t, store, target, now)
		terminal, err := store.CreatePlaybackTicket(models.PlaybackTicketRequest{
			UserID: target.user.ID, DeviceID: target.device.ID, FileID: target.fileID,
		}, now)
		if err != nil {
			t.Fatalf("create terminal ticket: %v", err)
		}
		terminal, err = store.FinishPlaybackTicketProbe(
			target.bindingWith(terminal.TicketID), terminal.Version,
			models.PlaybackTicketProbeResult{
				State:       models.PlaybackTicketUnsupported,
				FailureCode: "no_playable_transport",
			}, now)
		if err != nil {
			t.Fatalf("finish terminal ticket probe: %v", err)
		}

		siblingTicket, siblingGrant := createActivePlaybackCapability(t, store, sibling, now)
		revokeAt := now.Add(time.Minute)
		if err := store.RevokeDevice(target.user.ID, target.device.PublicID, revokeAt); err != nil {
			t.Fatalf("revoke target device: %v", err)
		}

		for _, tc := range []struct {
			name   string
			ticket *models.PlaybackTicket
		}{
			{name: "probing", ticket: probing},
			{name: "ready", ticket: ready},
			{name: "consuming", ticket: consuming},
		} {
			t.Run(tc.name+" ticket revoked", func(t *testing.T) {
				revoked := playbackTicketByID(t, store, target.bindingWith(tc.ticket.TicketID), revokeAt)
				if revoked.State != models.PlaybackTicketRevoked ||
					revoked.RevokedAt == nil || !revoked.RevokedAt.Equal(revokeAt) ||
					!revoked.UpdatedAt.Equal(revokeAt) ||
					revoked.Version != tc.ticket.Version+1 {
					t.Fatalf("revoked ticket = %+v", revoked)
				}
			})
		}

		terminalAfter := playbackTicketByID(t, store, target.bindingWith(terminal.TicketID), revokeAt)
		if terminalAfter.State != models.PlaybackTicketUnsupported ||
			terminalAfter.Version != terminal.Version ||
			terminalAfter.RevokedAt != nil {
			t.Fatalf("terminal ticket changed: %+v", terminalAfter)
		}

		targetSession := playbackMediaSessionByCapability(
			t, store, target.sessionBinding(consuming.TicketID, models.PlaybackTransportServerProxy),
			grant.Capability, revokeAt)
		if targetSession.State != models.PlaybackMediaSessionRevoked ||
			targetSession.RevokedAt == nil || !targetSession.RevokedAt.Equal(revokeAt) ||
			!targetSession.UpdatedAt.Equal(revokeAt) ||
			targetSession.Version != grant.Session.Version+1 {
			t.Fatalf("target media session = %+v", targetSession)
		}
		if _, err := store.AuthorizePlaybackMediaSession(
			grant.Capability, false, revokeAt); !errors.Is(err, ErrPlaybackMediaSessionRevoked) {
			t.Fatalf("authorize revoked target session error = %v", err)
		}

		siblingTicketAfter := playbackTicketByID(
			t, store, sibling.bindingWith(siblingTicket.TicketID), revokeAt)
		if siblingTicketAfter.State != models.PlaybackTicketConsuming ||
			siblingTicketAfter.Version != siblingTicket.Version ||
			siblingTicketAfter.RevokedAt != nil {
			t.Fatalf("sibling ticket changed: %+v", siblingTicketAfter)
		}
		siblingSession := playbackMediaSessionByCapability(
			t, store, sibling.sessionBinding(siblingTicket.TicketID, models.PlaybackTransportServerProxy),
			siblingGrant.Capability, revokeAt)
		if siblingSession.State != models.PlaybackMediaSessionActive ||
			siblingSession.Version != siblingGrant.Session.Version ||
			siblingSession.RevokedAt != nil {
			t.Fatalf("sibling media session changed: %+v", siblingSession)
		}
		if _, err := store.AuthorizePlaybackMediaSession(
			siblingGrant.Capability, false, revokeAt); err != nil {
			t.Fatalf("authorize sibling playback: %v", err)
		}
	})

	t.Run("all devices revoke every user playback capability", func(t *testing.T) {
		store := openPlaybackTestStore(t)
		now := time.Date(2026, 10, 26, 21, 0, 0, 0, time.UTC)
		first := addPlaybackTestFixture(t, store, "all-first", "all-first", now)
		secondDevice := addPlaybackTestDeviceForUser(
			t, store, first.user.ID, "all-second", "all-second", now)
		second := playbackTestFixture{
			store: store, user: first.user, device: secondDevice,
			fileID: first.fileID,
		}
		second.binding = second.bindingWith("")
		otherUser := addPlaybackTestFixture(t, store, "all-other", "all-other", now)

		firstTicket, firstGrant := createActivePlaybackCapability(t, store, first, now)
		secondTicket, secondGrant := createActivePlaybackCapability(t, store, second, now)
		otherTicket, otherGrant := createActivePlaybackCapability(t, store, otherUser, now)

		revokeAt := now.Add(time.Minute)
		revokedDevices, err := store.RevokeAllDevicesForUser(first.user.ID, revokeAt)
		if err != nil {
			t.Fatalf("revoke all user devices: %v", err)
		}
		if revokedDevices != 2 {
			t.Fatalf("newly revoked devices = %d, want 2", revokedDevices)
		}

		for _, tc := range []struct {
			name    string
			fixture playbackTestFixture
			ticket  *models.PlaybackTicket
			grant   *models.PlaybackMediaSessionGrant
		}{
			{name: "first", fixture: first, ticket: firstTicket, grant: firstGrant},
			{name: "second", fixture: second, ticket: secondTicket, grant: secondGrant},
		} {
			t.Run(tc.name+" device revoked", func(t *testing.T) {
				ticket := playbackTicketByID(
					t, store, tc.fixture.bindingWith(tc.ticket.TicketID), revokeAt)
				if ticket.State != models.PlaybackTicketRevoked ||
					ticket.RevokedAt == nil || !ticket.RevokedAt.Equal(revokeAt) ||
					!ticket.UpdatedAt.Equal(revokeAt) ||
					ticket.Version != tc.ticket.Version+1 {
					t.Fatalf("revoked ticket = %+v", ticket)
				}
				session := playbackMediaSessionByCapability(
					t, store,
					tc.fixture.sessionBinding(tc.ticket.TicketID, models.PlaybackTransportServerProxy),
					tc.grant.Capability, revokeAt)
				if session.State != models.PlaybackMediaSessionRevoked ||
					session.RevokedAt == nil || !session.RevokedAt.Equal(revokeAt) ||
					!session.UpdatedAt.Equal(revokeAt) ||
					session.Version != tc.grant.Session.Version+1 {
					t.Fatalf("revoked media session = %+v", session)
				}
				if _, err := store.AuthorizePlaybackMediaSession(
					tc.grant.Capability, false, revokeAt); !errors.Is(err, ErrPlaybackMediaSessionRevoked) {
					t.Fatalf("authorize revoked session error = %v", err)
				}
			})
		}

		otherTicketAfter := playbackTicketByID(
			t, store, otherUser.bindingWith(otherTicket.TicketID), revokeAt)
		if otherTicketAfter.State != models.PlaybackTicketConsuming ||
			otherTicketAfter.Version != otherTicket.Version ||
			otherTicketAfter.RevokedAt != nil {
			t.Fatalf("other user's ticket changed: %+v", otherTicketAfter)
		}
		otherSession := playbackMediaSessionByCapability(
			t, store,
			otherUser.sessionBinding(otherTicket.TicketID, models.PlaybackTransportServerProxy),
			otherGrant.Capability, revokeAt)
		if otherSession.State != models.PlaybackMediaSessionActive ||
			otherSession.Version != otherGrant.Session.Version ||
			otherSession.RevokedAt != nil {
			t.Fatalf("other user's media session changed: %+v", otherSession)
		}
		if _, err := store.AuthorizePlaybackMediaSession(
			otherGrant.Capability, false, revokeAt); err != nil {
			t.Fatalf("authorize other user's playback: %v", err)
		}
	})
}

func createActivePlaybackCapability(
	t *testing.T,
	store *Store,
	fixture playbackTestFixture,
	now time.Time,
) (*models.PlaybackTicket, *models.PlaybackMediaSessionGrant) {
	t.Helper()
	ready := createReadyPlaybackTicket(t, store, fixture, now,
		[]string{models.PlaybackTransportServerProxy}, `{}`)
	claimed := claimPlaybackTransport(
		t, store, fixture, ready, models.PlaybackTransportServerProxy, now)
	grant := createPlaybackMediaSession(
		t, store, fixture, claimed, models.PlaybackTransportServerProxy, now)
	return claimed, grant
}

func playbackTicketByID(
	t *testing.T,
	store *Store,
	binding models.PlaybackTicketBinding,
	now time.Time,
) *models.PlaybackTicket {
	t.Helper()
	ticket, err := store.PlaybackTicketByID(binding, now)
	if err != nil {
		t.Fatalf("read playback ticket %s: %v", binding.TicketID, err)
	}
	return ticket
}

func playbackMediaSessionByCapability(
	t *testing.T,
	store *Store,
	binding models.PlaybackMediaSessionBinding,
	capability string,
	now time.Time,
) *models.PlaybackMediaSession {
	t.Helper()
	session, err := store.PlaybackMediaSessionByCapability(binding, capability, now)
	if err != nil {
		t.Fatalf("read playback media session: %v", err)
	}
	return session
}

func addPlaybackTestFixture(
	t *testing.T,
	store *Store,
	name, slug string,
	now time.Time,
) playbackTestFixture {
	t.Helper()
	user := &models.User{
		Name: name, PassHash: "hash", Salt: "salt",
		Role: models.RoleUser, Status: models.StatusActive,
	}
	if err := store.CreateUser(user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	device := addPlaybackTestDeviceForUser(t, store, user.ID, name, slug, now)
	source := &models.Source{Name: name + " source", Type: "local", Config: "{}"}
	if err := store.CreateSource(source); err != nil {
		t.Fatalf("create source: %v", err)
	}
	library := &models.Library{
		Name: name + " library", Type: "movie", SourceID: source.ID,
		Path: "/" + slug, Providers: `["nfo"]`,
	}
	if err := store.CreateLibrary(library); err != nil {
		t.Fatalf("create library: %v", err)
	}
	item := &models.Item{
		LibraryID: library.ID, Type: "movie", Name: name + " movie",
	}
	if err := store.InsertItem(item); err != nil {
		t.Fatalf("create item: %v", err)
	}
	fileID, err := store.UpsertFile(&models.MediaFile{
		ItemID: item.ID, SourceID: source.ID,
		Path: fmt.Sprintf("/%s/movie.mkv", slug), Size: 1024, MTime: now.Unix(),
	})
	if err != nil {
		t.Fatalf("create file: %v", err)
	}
	fixture := playbackTestFixture{
		store: store, user: user, device: device, fileID: fileID,
	}
	fixture.binding = fixture.bindingWith("")
	return fixture
}

func addPlaybackTestDeviceForUser(
	t *testing.T,
	store *Store,
	userID int64,
	name, slug string,
	now time.Time,
) *models.Device {
	t.Helper()
	devicePublicID := "dev_test_" + slug
	res, err := store.DB.Exec(`INSERT INTO devices
		(public_id, user_id, name, platform, client_version, created_at, last_seen_at)
		VALUES (?,?,?,?,?,?,?)`,
		devicePublicID, userID, name+" client", "windows", "0.0.0-test", now, now)
	if err != nil {
		t.Fatalf("create device: %v", err)
	}
	deviceID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("read device id: %v", err)
	}
	return &models.Device{
		ID: deviceID, PublicID: devicePublicID, UserID: userID,
		Name: name + " client", Platform: "windows", ClientVersion: "0.0.0-test",
		CreatedAt: now, LastSeenAt: now,
	}
}

func (f playbackTestFixture) bindingWith(ticketID string) models.PlaybackTicketBinding {
	return models.PlaybackTicketBinding{
		UserID: f.user.ID, DeviceID: f.device.ID, FileID: f.fileID,
		TicketID: ticketID,
	}
}

func (f playbackTestFixture) sessionBinding(ticketID, transport string) models.PlaybackMediaSessionBinding {
	return models.PlaybackMediaSessionBinding{
		Ticket: f.bindingWith(ticketID), Transport: transport,
	}
}

func createReadyPlaybackTicket(
	t *testing.T,
	store *Store,
	fixture playbackTestFixture,
	now time.Time,
	transports []string,
	summary string,
) *models.PlaybackTicket {
	t.Helper()
	ticket, err := store.CreatePlaybackTicket(models.PlaybackTicketRequest{
		UserID: fixture.user.ID, DeviceID: fixture.device.ID, FileID: fixture.fileID,
		CandidateTransports: transports,
		MediaSummary:        json.RawMessage(summary),
	}, now)
	if err != nil {
		t.Fatalf("create ticket: %v", err)
	}
	ready, err := store.FinishPlaybackTicketProbe(fixture.bindingWith(ticket.TicketID), ticket.Version,
		models.PlaybackTicketProbeResult{
			State: models.PlaybackTicketReady, CandidateTransports: transports,
			MediaSummary: json.RawMessage(summary),
			VideoIndex:   playbackVideoIndexPtr(4),
		}, now)
	if err != nil {
		t.Fatalf("finish ticket probe: %v", err)
	}
	return ready
}

func claimPlaybackTransport(
	t *testing.T,
	store *Store,
	fixture playbackTestFixture,
	ticket *models.PlaybackTicket,
	transport string,
	now time.Time,
) *models.PlaybackTicket {
	t.Helper()
	claimed, err := store.ClaimNextPlaybackTransport(
		context.Background(), fixture.bindingWith(ticket.TicketID), ticket.Version, now)
	if err != nil {
		t.Fatalf("claim %s: %v", transport, err)
	}
	if claimed.SelectedTransport != transport {
		t.Fatalf("claimed %q, want %q", claimed.SelectedTransport, transport)
	}
	return claimed
}

func createPlaybackMediaSession(
	t *testing.T,
	store *Store,
	fixture playbackTestFixture,
	ticket *models.PlaybackTicket,
	transport string,
	now time.Time,
) *models.PlaybackMediaSessionGrant {
	t.Helper()
	grant, err := store.CreatePlaybackMediaSession(
		context.Background(),
		models.PlaybackMediaSessionRequest{
			Ticket:       fixture.bindingWith(ticket.TicketID),
			Transport:    transport,
			Mode:         "remux",
			AudioOrdinal: 0,
			StartSeconds: 0,
		},
		now,
	)
	if err != nil {
		t.Fatalf("create playback media session: %v", err)
	}
	return grant
}

func playbackForeignKeyTables(t *testing.T, store *Store, table string) map[string]bool {
	t.Helper()
	rows, err := store.DB.Query(`PRAGMA foreign_key_list(` + table + `)`)
	if err != nil {
		t.Fatalf("foreign keys for %s: %v", table, err)
	}
	defer rows.Close()
	out := make(map[string]bool)
	for rows.Next() {
		var (
			id, seq  int
			parent   string
			from, to string
			onUpdate string
			onDelete string
			match    string
		)
		if err := rows.Scan(&id, &seq, &parent, &from, &to, &onUpdate, &onDelete, &match); err != nil {
			t.Fatalf("scan foreign key for %s: %v", table, err)
		}
		out[parent] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("foreign key rows for %s: %v", table, err)
	}
	return out
}

func playbackTableColumns(t *testing.T, store *Store, table string) map[string]bool {
	t.Helper()
	rows, err := store.DB.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		t.Fatalf("columns for %s: %v", table, err)
	}
	defer rows.Close()
	out := make(map[string]bool)
	for rows.Next() {
		var (
			cid          int
			name         string
			columnType   string
			notNull      int
			defaultValue any
			pk           int
		)
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &pk); err != nil {
			t.Fatalf("scan column for %s: %v", table, err)
		}
		out[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("column rows for %s: %v", table, err)
	}
	return out
}
