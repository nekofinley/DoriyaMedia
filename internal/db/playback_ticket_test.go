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
	"sync/atomic"
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

func TestPlaybackDirectDASHTransportCheckMigrationIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "direct-dash-migration.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open playback store: %v", err)
	}
	now := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	fixture := addPlaybackTestFixture(
		t, store, "dash-migration-user", "dash-migration-device", now)
	subtitleID := addPlaybackSubtitle(
		t, store, fixture.fileID, "/dash-migration/caption.srt", "subrip", false, 0)
	transports := []string{
		models.PlaybackTransportDirectHTTP,
		models.PlaybackTransportServerProxy,
		models.PlaybackTransportServerHLS,
	}
	ticketIDs := make(map[string]string, len(transports))
	var lastTicketID int64
	for index, transport := range transports {
		at := now.Add(time.Duration(index) * time.Minute)
		ready := createReadyPlaybackTicket(
			t, store, fixture, at, []string{transport}, `{"container":"legacy"}`)
		claimed := claimPlaybackTransport(t, store, fixture, ready, transport, at.Add(time.Second))
		ticketIDs[transport] = claimed.TicketID
		if claimed.ID > lastTicketID {
			lastTicketID = claimed.ID
		}
		hash := sha256.Sum256([]byte("legacy-subtitle-" + transport))
		if _, err := store.DB.Exec(`INSERT INTO playback_subtitle_sessions
			(capability_hash, ticket_id, user_id, device_id, file_id, purpose,
			 transport, subtitle_id, state, start_seconds, version, created_at,
			 idle_expires_at, absolute_expires_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			hex.EncodeToString(hash[:]), claimed.TicketID, fixture.user.ID,
			fixture.device.ID, fixture.fileID, models.PlaybackPurposePlayback,
			transport, subtitleID, models.PlaybackSubtitleSessionActive, 0, 1,
			at.Add(2*time.Second), at.Add(17*time.Minute), at.Add(6*time.Hour),
			at.Add(2*time.Second)); err != nil {
			t.Fatalf("insert legacy subtitle session for %s: %v", transport, err)
		}
	}
	// Leave a gap above the copied ticket and subtitle ids. Rebuilding an
	// AUTOINCREMENT table without restoring sqlite_sequence makes SQLite reuse
	// the highest deleted id, which is no longer a valid public-id invariant.
	highWaterTicket := createReadyPlaybackTicket(
		t, store, fixture, now.Add(5*time.Minute),
		[]string{models.PlaybackTransportServerHLS}, `{"container":"legacy"}`)
	highWaterClaimed := claimPlaybackTransport(
		t, store, fixture, highWaterTicket, models.PlaybackTransportServerHLS,
		now.Add(5*time.Minute+time.Second))
	highWaterHash := sha256.Sum256([]byte("high-water-subtitle"))
	highWaterResult, err := store.DB.Exec(`INSERT INTO playback_subtitle_sessions
		(capability_hash, ticket_id, user_id, device_id, file_id, purpose,
		 transport, subtitle_id, state, start_seconds, version, created_at,
		 idle_expires_at, absolute_expires_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		hex.EncodeToString(highWaterHash[:]), highWaterClaimed.TicketID,
		fixture.user.ID, fixture.device.ID, fixture.fileID,
		models.PlaybackPurposePlayback, models.PlaybackTransportServerHLS,
		subtitleID, models.PlaybackSubtitleSessionActive, 0, 1,
		now.Add(5*time.Minute+2*time.Second),
		now.Add(5*time.Minute+17*time.Second),
		now.Add(11*time.Minute+2*time.Second),
		now.Add(5*time.Minute+2*time.Second))
	if err != nil {
		t.Fatalf("insert high-water subtitle session: %v", err)
	}
	highWaterSessionID, err := highWaterResult.LastInsertId()
	if err != nil {
		t.Fatalf("read high-water subtitle session id: %v", err)
	}
	if _, err := store.DB.Exec(`DELETE FROM playback_subtitle_sessions WHERE id = ?`,
		highWaterSessionID); err != nil {
		t.Fatalf("delete high-water subtitle session: %v", err)
	}
	if _, err := store.DB.Exec(`DELETE FROM playback_tickets WHERE ticket_id = ?`,
		highWaterClaimed.TicketID); err != nil {
		t.Fatalf("delete high-water ticket: %v", err)
	}
	lastTicketID = highWaterClaimed.ID
	var ticketHighWater, subtitleHighWater int64
	if err := store.DB.QueryRow(
		`SELECT seq FROM sqlite_sequence WHERE name = ?`, playbackTicketsTable).
		Scan(&ticketHighWater); err != nil {
		t.Fatalf("read ticket sequence high-water mark: %v", err)
	}
	if err := store.DB.QueryRow(
		`SELECT seq FROM sqlite_sequence WHERE name = ?`, "playback_subtitle_sessions").
		Scan(&subtitleHighWater); err != nil {
		t.Fatalf("read subtitle sequence high-water mark: %v", err)
	}
	if ticketHighWater != highWaterClaimed.ID {
		t.Fatalf("ticket sequence before migration = %d, want deleted id %d",
			ticketHighWater, highWaterClaimed.ID)
	}
	if err := store.DB.Close(); err != nil {
		t.Fatalf("close seeded playback store: %v", err)
	}

	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	defer legacy.Close()
	legacy.SetMaxOpenConns(1)
	if _, err := legacy.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
		_ = legacy.Close()
		t.Fatalf("disable legacy foreign keys: %v", err)
	}
	downgradePlaybackTransportCheckTable(
		t, legacy, playbackTicketsTable,
		playbackTicketsTableDefinition, playbackTicketColumns)
	downgradePlaybackTransportCheckTable(
		t, legacy, "playback_subtitle_sessions",
		playbackSubtitleSessionsTableDefinition, playbackSubtitleSessionMigrationColumns)
	for _, stmt := range []string{
		`CREATE INDEX idx_legacy_playback_ticket_marker ON playback_tickets(state)`,
		`CREATE TRIGGER trg_legacy_playback_ticket_marker
			AFTER INSERT ON playback_tickets
			BEGIN
				UPDATE playback_tickets SET updated_at = NEW.updated_at WHERE 0;
			END`,
		`CREATE INDEX idx_legacy_playback_subtitle_marker
			ON playback_subtitle_sessions(state)`,
		`CREATE TRIGGER trg_legacy_playback_subtitle_marker
			AFTER INSERT ON playback_subtitle_sessions
			BEGIN
				UPDATE playback_subtitle_sessions SET state = NEW.state WHERE 0;
			END`,
	} {
		if _, err := legacy.Exec(stmt); err != nil {
			_ = legacy.Close()
			t.Fatalf("create legacy playback schema object: %v", err)
		}
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("open migrated playback database: %v", err)
	}
	defer reopened.DB.Close()
	for attempt := 0; attempt < 2; attempt++ {
		if err := reopened.migrate(); err != nil {
			t.Fatalf("repeat direct-dash migration %d: %v", attempt+1, err)
		}
	}
	var ticketCount, subtitleCount int
	if err := reopened.DB.QueryRow(
		`SELECT COUNT(*) FROM playback_tickets`).Scan(&ticketCount); err != nil {
		t.Fatalf("count migrated tickets: %v", err)
	}
	if err := reopened.DB.QueryRow(
		`SELECT COUNT(*) FROM playback_subtitle_sessions`).Scan(&subtitleCount); err != nil {
		t.Fatalf("count migrated subtitle sessions: %v", err)
	}
	if ticketCount != len(transports) || subtitleCount != len(transports) {
		t.Fatalf("migrated rows = tickets %d, subtitles %d; want %d each",
			ticketCount, subtitleCount, len(transports))
	}
	for _, tc := range []struct {
		table string
		want  int64
	}{
		{playbackTicketsTable, ticketHighWater},
		{"playback_subtitle_sessions", subtitleHighWater},
	} {
		var got int64
		if err := reopened.DB.QueryRow(
			`SELECT seq FROM sqlite_sequence WHERE name = ?`, tc.table).Scan(&got); err != nil {
			t.Fatalf("read migrated %s sequence: %v", tc.table, err)
		}
		if got < tc.want {
			t.Fatalf("migrated %s sequence = %d, want at least %d", tc.table, got, tc.want)
		}
	}
	for _, transport := range transports {
		var storedTransport string
		if err := reopened.DB.QueryRow(`SELECT selected_transport
			FROM playback_tickets WHERE ticket_id = ?`,
			ticketIDs[transport]).Scan(&storedTransport); err != nil {
			t.Fatalf("read migrated %s ticket: %v", transport, err)
		}
		if storedTransport != transport {
			t.Fatalf("migrated ticket transport = %q, want %q", storedTransport, transport)
		}
		var storedSubtitleTransport string
		if err := reopened.DB.QueryRow(`SELECT transport
			FROM playback_subtitle_sessions WHERE ticket_id = ?`,
			ticketIDs[transport]).Scan(&storedSubtitleTransport); err != nil {
			t.Fatalf("read migrated %s subtitle session: %v", transport, err)
		}
		if storedSubtitleTransport != transport {
			t.Fatalf("migrated subtitle transport = %q, want %q",
				storedSubtitleTransport, transport)
		}
	}
	for _, object := range []struct {
		kind string
		name string
	}{
		{"index", "idx_legacy_playback_ticket_marker"},
		{"trigger", "trg_legacy_playback_ticket_marker"},
		{"index", "idx_legacy_playback_subtitle_marker"},
		{"trigger", "trg_legacy_playback_subtitle_marker"},
	} {
		var count int
		if err := reopened.DB.QueryRow(`SELECT COUNT(*) FROM sqlite_master
			WHERE type = ? AND name = ?`, object.kind, object.name).
			Scan(&count); err != nil {
			t.Fatalf("inspect preserved %s %s: %v", object.kind, object.name, err)
		}
		if count != 1 {
			t.Fatalf("preserved %s %s count = %d, want 1",
				object.kind, object.name, count)
		}
	}
	if defaultValue := playbackColumnDefault(
		t, reopened, "playback_tickets", "candidate_transports"); defaultValue != "'[]'" {
		t.Fatalf("candidate_transports default = %q, want '[]'", defaultValue)
	}
	if defaultValue := playbackColumnDefault(
		t, reopened, "playback_tickets", "subtitle_ids_json"); defaultValue != "'[]'" {
		t.Fatalf("subtitle_ids_json default = %q, want '[]'", defaultValue)
	}
	if defaultValue := playbackColumnDefault(
		t, reopened, "playback_tickets", "version"); defaultValue != "1" {
		t.Fatalf("version default = %q, want 1", defaultValue)
	}

	direct := createReadyPlaybackTicket(
		t, reopened, fixture, now.Add(10*time.Minute),
		[]string{models.PlaybackTransportDirectDASH}, `{}`)
	if direct.ID <= lastTicketID {
		t.Fatalf("post-migration ticket id = %d, want greater than %d",
			direct.ID, lastTicketID)
	}
	claimed := claimPlaybackTransport(
		t, reopened, fixture, direct, models.PlaybackTransportDirectDASH,
		now.Add(11*time.Minute))
	if err := reopened.ValidatePlaybackTransportIssuance(
		context.Background(), fixture.bindingWith(claimed.TicketID),
		claimed.Version, models.PlaybackTransportDirectDASH,
		claimed.ClaimToken, now.Add(11*time.Minute+15*time.Second)); err != nil {
		t.Fatalf("issue direct-dash after migration: %v", err)
	}
}

func TestPlaybackDirectDASHTransportCheckMigrationRollsBackOnIncomingOrphan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "direct-dash-orphan.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open playback store: %v", err)
	}
	now := time.Date(2026, 10, 27, 9, 30, 0, 0, time.UTC)
	fixture := addPlaybackTestFixture(
		t, store, "dash-orphan-user", "dash-orphan-device", now)
	subtitleID := addPlaybackSubtitle(
		t, store, fixture.fileID, "/dash-orphan/caption.srt", "subrip", false, 0)
	ready := createReadyPlaybackTicket(
		t, store, fixture, now, []string{models.PlaybackTransportServerHLS},
		`{"container":"legacy"}`)
	claimed := claimPlaybackTransport(
		t, store, fixture, ready, models.PlaybackTransportServerHLS,
		now.Add(time.Second))
	orphanHash := sha256.Sum256([]byte("orphan-subtitle"))
	if _, err := store.DB.Exec(`INSERT INTO playback_subtitle_sessions
		(capability_hash, ticket_id, user_id, device_id, file_id, purpose,
		 transport, subtitle_id, state, start_seconds, version, created_at,
		 idle_expires_at, absolute_expires_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		hex.EncodeToString(orphanHash[:]), claimed.TicketID, fixture.user.ID,
		fixture.device.ID, fixture.fileID, models.PlaybackPurposePlayback,
		models.PlaybackTransportServerHLS, subtitleID,
		models.PlaybackSubtitleSessionActive, 0, 1, now.Add(2*time.Second),
		now.Add(17*time.Minute), now.Add(6*time.Hour), now.Add(2*time.Second)); err != nil {
		t.Fatalf("create subtitle session: %v", err)
	}
	if _, err := store.CreatePlaybackMediaSession(
		context.Background(),
		models.PlaybackMediaSessionRequest{
			Ticket:       playbackSubtitleTicketBinding(claimed),
			Transport:    models.PlaybackTransportServerHLS,
			Mode:         "remux",
			AudioOrdinal: 0,
			StartSeconds: 0,
		},
		now.Add(2*time.Second),
	); err != nil {
		t.Fatalf("create media session: %v", err)
	}
	if err := store.DB.Close(); err != nil {
		t.Fatalf("close seeded playback store: %v", err)
	}

	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	legacy.SetMaxOpenConns(1)
	if _, err := legacy.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
		_ = legacy.Close()
		t.Fatalf("disable legacy foreign keys: %v", err)
	}
	downgradePlaybackTransportCheckTable(
		t, legacy, playbackTicketsTable,
		playbackTicketsTableDefinition, playbackTicketColumns)
	downgradePlaybackTransportCheckTable(
		t, legacy, "playback_subtitle_sessions",
		playbackSubtitleSessionsTableDefinition, playbackSubtitleSessionMigrationColumns)
	if _, err := legacy.Exec(
		`DELETE FROM playback_tickets WHERE ticket_id = ?`, claimed.TicketID); err != nil {
		_ = legacy.Close()
		t.Fatalf("leave orphan ticket children: %v", err)
	}
	for _, child := range []string{
		playbackMediaSessionsTable,
		"playback_subtitle_sessions",
	} {
		var count int
		if err := legacy.QueryRow(
			`SELECT COUNT(*) FROM `+child+` WHERE ticket_id = ?`,
			claimed.TicketID).Scan(&count); err != nil {
			_ = legacy.Close()
			t.Fatalf("count orphan rows in %s: %v", child, err)
		}
		if count != 1 {
			_ = legacy.Close()
			t.Fatalf("orphan rows in %s = %d, want 1", child, count)
		}
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("reopen legacy database: %v", err)
	}
	defer raw.Close()
	raw.SetMaxOpenConns(1)
	s := &Store{DB: raw, playbackWriteSem: make(chan struct{}, 1)}
	if err := s.migrate(); err == nil {
		t.Fatal("direct-dash migration accepted incoming orphan rows")
	} else if !strings.Contains(err.Error(), "orphan") {
		t.Fatalf("direct-dash orphan migration error = %v", err)
	}

	for _, table := range []string{playbackTicketsTable, "playback_subtitle_sessions"} {
		var definition string
		if err := raw.QueryRow(`SELECT sql FROM sqlite_master
			WHERE type = 'table' AND name = ?`, table).Scan(&definition); err != nil {
			t.Fatalf("read %s definition after rollback: %v", table, err)
		}
		if strings.Contains(strings.ToLower(definition), "direct-dash") {
			t.Fatalf("%s was partially migrated before rollback", table)
		}
	}
	for _, object := range []struct {
		kind string
		name string
	}{
		{"index", "idx_playback_tickets_public_id"},
		{"trigger", "trg_playback_tickets_binding_immutable"},
		{"index", "idx_playback_subtitle_sessions_capability"},
		{"trigger", "trg_playback_subtitle_sessions_binding_insert"},
	} {
		var count int
		if err := raw.QueryRow(`SELECT COUNT(*) FROM sqlite_master
			WHERE type = ? AND name = ?`, object.kind, object.name).Scan(&count); err != nil {
			t.Fatalf("inspect %s %s after rollback: %v", object.kind, object.name, err)
		}
		if count != 1 {
			t.Fatalf("%s %s count after rollback = %d, want 1",
				object.kind, object.name, count)
		}
	}
	var tickets, mediaSessions, subtitleSessions int
	for _, tc := range []struct {
		table string
		want  int
		dst   *int
	}{
		{playbackTicketsTable, 0, &tickets},
		{playbackMediaSessionsTable, 1, &mediaSessions},
		{"playback_subtitle_sessions", 1, &subtitleSessions},
	} {
		if err := raw.QueryRow(`SELECT COUNT(*) FROM ` + tc.table).Scan(tc.dst); err != nil {
			t.Fatalf("count %s after rollback: %v", tc.table, err)
		}
		if *tc.dst != tc.want {
			t.Fatalf("%s rows after rollback = %d, want %d",
				tc.table, *tc.dst, tc.want)
		}
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

func TestPlaybackDirectDASHPersistsClaimAndIssuance(t *testing.T) {
	store := openPlaybackTestStore(t)
	now := time.Date(2026, 10, 26, 15, 15, 0, 0, time.UTC)
	fixture := addPlaybackTestFixture(t, store, "direct-dash", "direct-dash", now)
	ready := createReadyPlaybackTicket(t, store, fixture, now,
		[]string{
			models.PlaybackTransportDirectDASH,
			models.PlaybackTransportServerHLS,
		}, `{"container":"dash"}`)

	claimed := claimPlaybackTransport(
		t, store, fixture, ready, models.PlaybackTransportDirectDASH, now.Add(time.Second))
	if claimed.State != models.PlaybackTicketConsuming || claimed.Version != ready.Version+1 ||
		len(claimed.AttemptedTransports) != 1 ||
		claimed.AttemptedTransports[0] != models.PlaybackTransportDirectDASH {
		t.Fatalf("direct-dash claim = %+v", claimed)
	}
	persisted, err := store.PlaybackTicketByID(
		fixture.bindingWith(claimed.TicketID), now.Add(2*time.Second))
	if err != nil {
		t.Fatalf("reload direct-dash claim: %v", err)
	}
	if persisted.SelectedTransport != models.PlaybackTransportDirectDASH ||
		persisted.ClaimToken == "" || persisted.ClaimExpiresAt == nil {
		t.Fatalf("persisted direct-dash claim = %+v", persisted)
	}

	if err := store.ValidatePlaybackTransportIssuance(
		context.Background(), fixture.bindingWith(claimed.TicketID),
		claimed.Version-1, models.PlaybackTransportDirectDASH,
		claimed.ClaimToken, now.Add(2*time.Second)); !errors.Is(
		err, ErrPlaybackTicketConflict) {
		t.Fatalf("stale direct-dash issuance error = %v", err)
	}
	if err := store.ValidatePlaybackTransportIssuance(
		context.Background(), fixture.bindingWith(claimed.TicketID),
		claimed.Version, models.PlaybackTransportDirectDASH,
		claimed.ClaimToken, now.Add(2*time.Second)); err != nil {
		t.Fatalf("validate direct-dash issuance: %v", err)
	}
	persisted, err = store.PlaybackTicketByID(
		fixture.bindingWith(claimed.TicketID), now.Add(3*time.Second))
	if err != nil {
		t.Fatalf("reload issued direct-dash ticket: %v", err)
	}
	if persisted.Version != claimed.Version+1 ||
		persisted.SelectedTransport != models.PlaybackTransportDirectDASH ||
		persisted.ClaimToken != "" || persisted.ClaimExpiresAt != nil {
		t.Fatalf("issued direct-dash ticket = %+v", persisted)
	}
	if err := store.ValidatePlaybackTransportIssuance(
		context.Background(), fixture.bindingWith(claimed.TicketID),
		claimed.Version, models.PlaybackTransportDirectDASH,
		claimed.ClaimToken, now.Add(3*time.Second)); !errors.Is(
		err, ErrPlaybackTicketConflict) {
		t.Fatalf("reused direct-dash issuance error = %v", err)
	}
}

func TestPlaybackDirectDASHDeviceOverrideBlocksClaimAndIssuance(t *testing.T) {
	store := openPlaybackTestStore(t)
	now := time.Date(2026, 10, 26, 15, 20, 0, 0, time.UTC)
	fixture := addPlaybackTestFixture(
		t, store, "direct-dash-override", "direct-dash-override", now)
	if _, err := store.UpdateDeviceFeatureOverrides(
		fixture.device.PublicID,
		models.DeviceFeatureOverrides{DirectDASHDisabled: true},
		fixture.user.ID,
		now,
	); err != nil {
		t.Fatalf("disable direct-dash: %v", err)
	}
	blocked := createReadyPlaybackTicket(t, store, fixture, now,
		[]string{models.PlaybackTransportDirectDASH}, `{}`)
	if _, err := store.ClaimNextPlaybackTransport(
		context.Background(), fixture.bindingWith(blocked.TicketID),
		blocked.Version, now.Add(time.Second)); !errors.Is(
		err, ErrDevicePlaybackDisabled) {
		t.Fatalf("direct-dash claim during device stop = %v", err)
	}

	if _, err := store.UpdateDeviceFeatureOverrides(
		fixture.device.PublicID, models.DeviceFeatureOverrides{},
		fixture.user.ID, now.Add(2*time.Second)); err != nil {
		t.Fatalf("clear direct-dash override: %v", err)
	}
	claimed := claimPlaybackTransport(
		t, store, fixture, blocked, models.PlaybackTransportDirectDASH,
		now.Add(3*time.Second))

	// Simulate a live emergency-stop update without the public method's global
	// ticket revocation, so issuance itself must recheck the transport override.
	if _, err := store.DB.Exec(`INSERT INTO device_feature_overrides
		(device_id, overrides_json, updated_at, updated_by)
		VALUES (?,?,?,?)
		ON CONFLICT(device_id) DO UPDATE SET
			overrides_json = excluded.overrides_json,
			updated_at = excluded.updated_at,
			updated_by = excluded.updated_by`,
		fixture.device.ID, `{"direct_dash_disabled":true}`,
		now.Add(4*time.Second), fixture.user.ID); err != nil {
		t.Fatalf("set live direct-dash override: %v", err)
	}
	if err := store.ValidatePlaybackTransportIssuance(
		context.Background(), fixture.bindingWith(claimed.TicketID),
		claimed.Version, models.PlaybackTransportDirectDASH,
		claimed.ClaimToken, now.Add(4*time.Second)); !errors.Is(
		err, ErrDevicePlaybackDisabled) {
		t.Fatalf("direct-dash issuance during device stop = %v", err)
	}
	if _, err := store.DB.Exec(`DELETE FROM device_feature_overrides
		WHERE device_id = ?`, fixture.device.ID); err != nil {
		t.Fatalf("clear live direct-dash override: %v", err)
	}
	if err := store.ValidatePlaybackTransportIssuance(
		context.Background(), fixture.bindingWith(claimed.TicketID),
		claimed.Version, models.PlaybackTransportDirectDASH,
		claimed.ClaimToken, now.Add(5*time.Second)); err != nil {
		t.Fatalf("direct-dash issuance after override clear: %v", err)
	}
}

func TestPlaybackMediaSessionRejectsDirectDASH(t *testing.T) {
	store := openPlaybackTestStore(t)
	now := time.Date(2026, 10, 26, 15, 25, 0, 0, time.UTC)
	fixture := addPlaybackTestFixture(
		t, store, "direct-dash-media", "direct-dash-media", now)
	ready := createReadyPlaybackTicket(t, store, fixture, now,
		[]string{models.PlaybackTransportDirectDASH}, `{}`)
	claimed := claimPlaybackTransport(
		t, store, fixture, ready, models.PlaybackTransportDirectDASH, now.Add(time.Second))

	if _, err := store.CreatePlaybackMediaSession(
		context.Background(),
		models.PlaybackMediaSessionRequest{
			Ticket:       fixture.bindingWith(claimed.TicketID),
			Transport:    models.PlaybackTransportDirectDASH,
			Mode:         "remux",
			AudioOrdinal: 0,
			StartSeconds: 0,
		},
		now.Add(2*time.Second),
	); !errors.Is(err, ErrInvalidPlaybackTransport) {
		t.Fatalf("direct-dash media session error = %v", err)
	}

	_, err := store.DB.Exec(`INSERT INTO playback_media_sessions
		(capability_hash, ticket_id, user_id, device_id, file_id, purpose,
		 transport, state, mode, audio_ordinal, video_index, start_seconds,
		 version, created_at, idle_expires_at, absolute_expires_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		strings.Repeat("a", 64), claimed.TicketID, fixture.user.ID,
		fixture.device.ID, fixture.fileID, models.PlaybackPurposePlayback,
		models.PlaybackTransportDirectDASH, models.PlaybackMediaSessionActive,
		"remux", 0, claimed.VideoIndex, 0, 1, now.Add(2*time.Second),
		now.Add(17*time.Minute), now.Add(6*time.Hour), now.Add(2*time.Second))
	if err == nil {
		t.Fatal("playback_media_sessions CHECK accepted direct-dash")
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

type playbackTicketContextSnapshot struct {
	id                  int64
	ticketID            string
	userID              int64
	deviceID            int64
	fileID              int64
	purpose             string
	state               string
	candidateTransports string
	attemptedTransports string
	selectedTransport   string
	mediaSummary        string
	clientCapabilities  string
	subtitleIDs         string
	failureCode         string
	videoIndex          int
	claimToken          string
	claimExpiresAt      sql.NullTime
	version             int64
	createdAt           time.Time
	expiresAt           time.Time
	updatedAt           time.Time
	consumedAt          sql.NullTime
	revokedAt           sql.NullTime
}

func readPlaybackTicketContextSnapshot(
	t *testing.T,
	store *Store,
	binding models.PlaybackTicketBinding,
) playbackTicketContextSnapshot {
	t.Helper()
	var snapshot playbackTicketContextSnapshot
	if err := store.DB.QueryRow(`SELECT id, ticket_id, user_id, device_id,
		file_id, purpose, state, candidate_transports, attempted_transports,
		selected_transport, media_summary_json, client_capabilities_json,
		subtitle_ids_json, failure_code, video_index, claim_token,
		claim_expires_at, version, created_at, expires_at, updated_at,
		consumed_at, revoked_at
		FROM playback_tickets
		WHERE ticket_id = ? AND user_id = ? AND device_id = ? AND file_id = ?`,
		binding.TicketID, binding.UserID, binding.DeviceID, binding.FileID,
	).Scan(
		&snapshot.id,
		&snapshot.ticketID,
		&snapshot.userID,
		&snapshot.deviceID,
		&snapshot.fileID,
		&snapshot.purpose,
		&snapshot.state,
		&snapshot.candidateTransports,
		&snapshot.attemptedTransports,
		&snapshot.selectedTransport,
		&snapshot.mediaSummary,
		&snapshot.clientCapabilities,
		&snapshot.subtitleIDs,
		&snapshot.failureCode,
		&snapshot.videoIndex,
		&snapshot.claimToken,
		&snapshot.claimExpiresAt,
		&snapshot.version,
		&snapshot.createdAt,
		&snapshot.expiresAt,
		&snapshot.updatedAt,
		&snapshot.consumedAt,
		&snapshot.revokedAt,
	); err != nil {
		t.Fatalf("read playback ticket context snapshot: %v", err)
	}
	return snapshot
}

type playbackMediaSessionContextSnapshot struct {
	id                int64
	capabilityHash    string
	ticketID          string
	userID            int64
	deviceID          int64
	fileID            int64
	purpose           string
	transport         string
	state             string
	mode              string
	audioOrdinal      int
	videoIndex        int
	startSeconds      float64
	quality           sql.NullString
	transcodeHeight   sql.NullInt64
	version           int64
	createdAt         time.Time
	lastUsedAt        sql.NullTime
	idleExpiresAt     time.Time
	absoluteExpiresAt time.Time
	updatedAt         time.Time
	revokedAt         sql.NullTime
	deviceLastSeen    time.Time
}

func readPlaybackMediaSessionContextSnapshot(
	t *testing.T,
	store *Store,
	sessionID,
	deviceID int64,
) playbackMediaSessionContextSnapshot {
	t.Helper()
	var snapshot playbackMediaSessionContextSnapshot
	if err := store.DB.QueryRow(`SELECT id, capability_hash, ticket_id, user_id,
		device_id, file_id, purpose, transport, state, mode, audio_ordinal,
		video_index, start_seconds, quality, transcode_height, version,
		created_at, last_used_at, idle_expires_at, absolute_expires_at,
		updated_at, revoked_at
		FROM playback_media_sessions WHERE id = ?`, sessionID,
	).Scan(
		&snapshot.id,
		&snapshot.capabilityHash,
		&snapshot.ticketID,
		&snapshot.userID,
		&snapshot.deviceID,
		&snapshot.fileID,
		&snapshot.purpose,
		&snapshot.transport,
		&snapshot.state,
		&snapshot.mode,
		&snapshot.audioOrdinal,
		&snapshot.videoIndex,
		&snapshot.startSeconds,
		&snapshot.quality,
		&snapshot.transcodeHeight,
		&snapshot.version,
		&snapshot.createdAt,
		&snapshot.lastUsedAt,
		&snapshot.idleExpiresAt,
		&snapshot.absoluteExpiresAt,
		&snapshot.updatedAt,
		&snapshot.revokedAt,
	); err != nil {
		t.Fatalf("read playback media session context snapshot: %v", err)
	}
	if err := store.DB.QueryRow(`SELECT last_seen_at FROM devices
		WHERE id = ?`, deviceID).Scan(&snapshot.deviceLastSeen); err != nil {
		t.Fatalf("read device last seen for playback context snapshot: %v", err)
	}
	return snapshot
}

func TestPlaybackProbeContextCancellationPreservesTicket(t *testing.T) {
	store := openPlaybackTestStore(t)
	now := time.Date(2026, 10, 26, 22, 30, 0, 0, time.UTC)
	fixture := addPlaybackTestFixture(t, store, "probe-context", "probe-context", now)
	ticket, err := store.CreatePlaybackTicket(models.PlaybackTicketRequest{
		UserID: fixture.user.ID, DeviceID: fixture.device.ID, FileID: fixture.fileID,
		CandidateTransports: []string{models.PlaybackTransportServerProxy},
		MediaSummary:        json.RawMessage(`{}`),
	}, now)
	if err != nil {
		t.Fatalf("create probing ticket: %v", err)
	}
	binding := fixture.bindingWith(ticket.TicketID)
	before := readPlaybackTicketContextSnapshot(t, store, binding)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	finished, err := store.FinishPlaybackTicketProbeContext(
		ctx,
		binding,
		ticket.Version,
		models.PlaybackTicketProbeResult{
			State:               models.PlaybackTicketReady,
			CandidateTransports: []string{models.PlaybackTransportServerProxy},
			MediaSummary:        json.RawMessage(`{}`),
			VideoIndex:          playbackVideoIndexPtr(0),
		},
		now.Add(time.Second),
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled probe finalization error = %v, want context.Canceled", err)
	}
	if finished != nil {
		t.Fatalf("canceled probe finalization returned ticket: %+v", finished)
	}
	after := readPlaybackTicketContextSnapshot(t, store, binding)
	if after != before {
		t.Fatalf("canceled probe finalization changed ticket\nbefore: %+v\nafter:  %+v",
			before, after)
	}
}

func TestPlaybackMediaSessionAuthorizationCancellationPreservesSessionAndDevice(t *testing.T) {
	store := openPlaybackTestStore(t)
	now := time.Date(2026, 10, 26, 22, 35, 0, 0, time.UTC)
	fixture := addPlaybackTestFixture(t, store, "authorize-context", "authorize-context", now)
	ticket, grant := createActivePlaybackCapability(t, store, fixture, now)
	before := readPlaybackMediaSessionContextSnapshot(
		t, store, grant.Session.ID, fixture.device.ID)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	access, err := store.AuthorizePlaybackMediaSessionContext(
		ctx, grant.Capability, false, now.Add(time.Minute))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled media authorization error = %v, want context.Canceled", err)
	}
	if access != nil {
		t.Fatalf("canceled media authorization returned access: %+v", access)
	}
	after := readPlaybackMediaSessionContextSnapshot(
		t, store, grant.Session.ID, ticket.DeviceID)
	if after != before {
		t.Fatalf("canceled media authorization changed session or device\nbefore: %+v\nafter:  %+v",
			before, after)
	}
}

func lockPlaybackTicketWriter(
	t *testing.T,
	store *Store,
	binding models.PlaybackTicketBinding,
) *sql.Tx {
	t.Helper()
	tx, err := store.DB.Begin()
	if err != nil {
		t.Fatalf("begin playback writer lock: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	if _, err := tx.Exec(`UPDATE playback_tickets SET version = version
		WHERE ticket_id = ? AND user_id = ? AND device_id = ? AND file_id = ?`,
		binding.TicketID, binding.UserID, binding.DeviceID, binding.FileID); err != nil {
		t.Fatalf("hold playback ticket writer lock: %v", err)
	}
	return tx
}

func waitForPlaybackConnections(t *testing.T, store *Store, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if got := store.DB.Stats().InUse; got >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("in-use database connections = %d, want at least %d",
				store.DB.Stats().InUse, want)
		}
		<-ticker.C
	}
}

func lockPlaybackMediaSessionWriter(
	t *testing.T,
	store *Store,
	capability string,
) *sql.Tx {
	t.Helper()
	tx, err := store.DB.Begin()
	if err != nil {
		t.Fatalf("begin playback media writer lock: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	if _, err := tx.Exec(`UPDATE playback_media_sessions SET version = version
		WHERE capability_hash = ?`, hashPlaybackCapability(capability)); err != nil {
		t.Fatalf("hold playback media writer lock: %v", err)
	}
	return tx
}

func TestPlaybackProbeContextDeadlineInterruptsWriterLockWait(t *testing.T) {
	store := openPlaybackTestStore(t)
	now := time.Date(2026, 10, 26, 22, 40, 0, 0, time.UTC)
	fixture := addPlaybackTestFixture(t, store, "probe-lock-deadline", "probe-lock-deadline", now)
	ticket, err := store.CreatePlaybackTicket(models.PlaybackTicketRequest{
		UserID: fixture.user.ID, DeviceID: fixture.device.ID, FileID: fixture.fileID,
		CandidateTransports: []string{models.PlaybackTransportServerProxy},
		MediaSummary:        json.RawMessage(`{}`),
	}, now)
	if err != nil {
		t.Fatalf("create probing ticket: %v", err)
	}
	binding := fixture.bindingWith(ticket.TicketID)
	before := readPlaybackTicketContextSnapshot(t, store, binding)
	writer := lockPlaybackTicketWriter(t, store, binding)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	started := time.Now()
	finished, err := store.FinishPlaybackTicketProbeContext(
		ctx,
		binding,
		ticket.Version,
		models.PlaybackTicketProbeResult{
			State:               models.PlaybackTicketReady,
			CandidateTransports: []string{models.PlaybackTransportServerProxy},
			MediaSummary:        json.RawMessage(`{}`),
			VideoIndex:          playbackVideoIndexPtr(0),
		},
		now.Add(time.Second),
	)
	elapsed := time.Since(started)
	if err := writer.Rollback(); err != nil {
		t.Fatalf("release playback ticket writer: %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("locked probe finalization error = %v, want context.DeadlineExceeded", err)
	}
	if finished != nil {
		t.Fatalf("locked probe finalization returned ticket: %+v", finished)
	}
	if elapsed > time.Second {
		t.Fatalf("locked probe finalization took %v, want <= 1s", elapsed)
	}
	after := readPlaybackTicketContextSnapshot(t, store, binding)
	if after != before {
		t.Fatalf("locked probe finalization changed ticket\nbefore: %+v\nafter:  %+v",
			before, after)
	}
}

func TestPlaybackMediaAuthorizationContextDeadlineInterruptsWriterLockWait(t *testing.T) {
	store := openPlaybackTestStore(t)
	now := time.Date(2026, 10, 26, 22, 45, 0, 0, time.UTC)
	fixture := addPlaybackTestFixture(t, store, "media-lock-deadline", "media-lock-deadline", now)
	ticket, grant := createActivePlaybackCapability(t, store, fixture, now)
	before := readPlaybackMediaSessionContextSnapshot(
		t, store, grant.Session.ID, fixture.device.ID)
	writer := lockPlaybackMediaSessionWriter(t, store, grant.Capability)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	started := time.Now()
	access, err := store.AuthorizePlaybackMediaSessionContext(
		ctx, grant.Capability, false, now.Add(time.Minute))
	elapsed := time.Since(started)
	if err := writer.Rollback(); err != nil {
		t.Fatalf("release playback media writer: %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("locked media authorization error = %v, want context.DeadlineExceeded", err)
	}
	if access != nil {
		t.Fatalf("locked media authorization returned access: %+v", access)
	}
	if elapsed > time.Second {
		t.Fatalf("locked media authorization took %v, want <= 1s", elapsed)
	}
	after := readPlaybackMediaSessionContextSnapshot(
		t, store, grant.Session.ID, ticket.DeviceID)
	if after != before {
		t.Fatalf("locked media authorization changed session or device\nbefore: %+v\nafter:  %+v",
			before, after)
	}
}

func TestPlaybackProbeContextCommitsExpiryTransition(t *testing.T) {
	store := openPlaybackTestStore(t)
	now := time.Date(2026, 10, 26, 22, 47, 0, 0, time.UTC)
	fixture := addPlaybackTestFixture(t, store, "probe-commit-expiry",
		"probe-commit-expiry", now)
	ticket, err := store.CreatePlaybackTicket(models.PlaybackTicketRequest{
		UserID: fixture.user.ID, DeviceID: fixture.device.ID, FileID: fixture.fileID,
		CandidateTransports: []string{models.PlaybackTransportServerProxy},
		MediaSummary:        json.RawMessage(`{}`),
	}, now)
	if err != nil {
		t.Fatalf("create probing ticket: %v", err)
	}
	binding := fixture.bindingWith(ticket.TicketID)
	before := readPlaybackTicketContextSnapshot(t, store, binding)
	finishTime := now.Add(models.PlaybackTicketTTL + time.Second)

	finished, err := store.FinishPlaybackTicketProbeContext(
		context.Background(),
		binding,
		ticket.Version,
		models.PlaybackTicketProbeResult{
			State:               models.PlaybackTicketReady,
			CandidateTransports: []string{models.PlaybackTransportServerProxy},
			MediaSummary:        json.RawMessage(`{}`),
			VideoIndex:          playbackVideoIndexPtr(0),
		},
		finishTime,
	)
	if !errors.Is(err, ErrPlaybackTicketExpired) {
		t.Fatalf("expired probe finalization error = %v, want ErrPlaybackTicketExpired", err)
	}
	if finished != nil {
		t.Fatalf("expired probe finalization returned ticket: %+v", finished)
	}
	after := readPlaybackTicketContextSnapshot(t, store, binding)
	if after.state != string(models.PlaybackTicketExpired) {
		t.Fatalf("expired probe ticket state = %q, want expired", after.state)
	}
	if after.version != before.version+1 {
		t.Fatalf("expired probe ticket version = %d, want %d",
			after.version, before.version+1)
	}
}

func TestPlaybackMediaAuthorizationCommitsExpiryTransition(t *testing.T) {
	store := openPlaybackTestStore(t)
	now := time.Date(2026, 10, 26, 22, 48, 0, 0, time.UTC)
	fixture := addPlaybackTestFixture(t, store, "media-commit-expiry",
		"media-commit-expiry", now)
	_, grant := createActivePlaybackCapability(t, store, fixture, now)
	if _, err := store.DB.Exec(`UPDATE playback_media_sessions
		SET idle_expires_at = ? WHERE id = ?`,
		now.Add(-time.Second), grant.Session.ID); err != nil {
		t.Fatalf("expire media session directly: %v", err)
	}
	before := readPlaybackMediaSessionContextSnapshot(
		t, store, grant.Session.ID, fixture.device.ID)

	access, err := store.AuthorizePlaybackMediaSessionContext(
		context.Background(), grant.Capability, false, now)
	if !errors.Is(err, ErrPlaybackMediaSessionExpired) {
		t.Fatalf("expired media authorization error = %v, want ErrPlaybackMediaSessionExpired", err)
	}
	if access != nil {
		t.Fatalf("expired media authorization returned access: %+v", access)
	}
	after := readPlaybackMediaSessionContextSnapshot(
		t, store, grant.Session.ID, fixture.device.ID)
	if after.state != string(models.PlaybackMediaSessionExpired) {
		t.Fatalf("expired media session state = %q, want expired", after.state)
	}
	if after.version != before.version+1 {
		t.Fatalf("expired media session version = %d, want %d",
			after.version, before.version+1)
	}
	if !after.deviceLastSeen.Equal(before.deviceLastSeen) {
		t.Fatalf("expired media authorization changed device last_seen_at: %v -> %v",
			before.deviceLastSeen, after.deviceLastSeen)
	}
}

func TestPlaybackProbeContextCancelInterruptsWriterLockWait(t *testing.T) {
	store := openPlaybackTestStore(t)
	now := time.Date(2026, 10, 26, 22, 50, 0, 0, time.UTC)
	fixture := addPlaybackTestFixture(t, store, "probe-lock-cancel", "probe-lock-cancel", now)
	ticket, err := store.CreatePlaybackTicket(models.PlaybackTicketRequest{
		UserID: fixture.user.ID, DeviceID: fixture.device.ID, FileID: fixture.fileID,
		CandidateTransports: []string{models.PlaybackTransportServerProxy},
		MediaSummary:        json.RawMessage(`{}`),
	}, now)
	if err != nil {
		t.Fatalf("create probing ticket: %v", err)
	}
	binding := fixture.bindingWith(ticket.TicketID)
	before := readPlaybackTicketContextSnapshot(t, store, binding)
	writer := lockPlaybackTicketWriter(t, store, binding)

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	started := time.Now()
	finished, err := store.FinishPlaybackTicketProbeContext(
		ctx,
		binding,
		ticket.Version,
		models.PlaybackTicketProbeResult{
			State:               models.PlaybackTicketReady,
			CandidateTransports: []string{models.PlaybackTransportServerProxy},
			MediaSummary:        json.RawMessage(`{}`),
			VideoIndex:          playbackVideoIndexPtr(0),
		},
		now.Add(time.Second),
	)
	elapsed := time.Since(started)
	cancel()
	if err := writer.Rollback(); err != nil {
		t.Fatalf("release playback ticket writer: %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled locked probe finalization error = %v, want context.Canceled", err)
	}
	if finished != nil {
		t.Fatalf("canceled locked probe finalization returned ticket: %+v", finished)
	}
	if elapsed > time.Second {
		t.Fatalf("canceled locked probe finalization took %v, want <= 1s", elapsed)
	}
	after := readPlaybackTicketContextSnapshot(t, store, binding)
	if after != before {
		t.Fatalf("canceled locked probe finalization changed ticket\nbefore: %+v\nafter:  %+v",
			before, after)
	}
}

func TestPlaybackWriteTxRetrySucceedsAfterWriterRelease(t *testing.T) {
	store := openPlaybackTestStore(t)
	now := time.Date(2026, 10, 26, 22, 55, 0, 0, time.UTC)
	fixture := addPlaybackTestFixture(t, store, "probe-lock-release", "probe-lock-release", now)
	ticket, err := store.CreatePlaybackTicket(models.PlaybackTicketRequest{
		UserID: fixture.user.ID, DeviceID: fixture.device.ID, FileID: fixture.fileID,
		CandidateTransports: []string{models.PlaybackTransportServerProxy},
		MediaSummary:        json.RawMessage(`{}`),
	}, now)
	if err != nil {
		t.Fatalf("create probing ticket: %v", err)
	}
	binding := fixture.bindingWith(ticket.TicketID)
	writer := lockPlaybackTicketWriter(t, store, binding)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var attempts atomic.Int32
	attempted := make(chan struct{}, 1)
	done := make(chan error, 1)
	started := time.Now()
	go func() {
		done <- store.withPlaybackWriteTx(
			ctx,
			func(ctx context.Context, tx *sql.Tx) error {
				attempts.Add(1)
				err := lockPlaybackTicketTxContext(ctx, tx, binding)
				if err != nil && isSQLiteBusy(err) {
					select {
					case attempted <- struct{}{}:
					default:
					}
				}
				return err
			},
			func(ctx context.Context, tx *sql.Tx) (bool, error) {
				res, err := tx.ExecContext(ctx, `UPDATE playback_tickets
					SET state = ?, updated_at = ?, version = version + 1
					WHERE id = ? AND state = ?`,
					models.PlaybackTicketReady, now.Add(time.Second), ticket.ID,
					models.PlaybackTicketProbing)
				if err != nil {
					return false, err
				}
				n, err := res.RowsAffected()
				if err != nil {
					return false, err
				}
				if n != 1 {
					return false, ErrPlaybackTicketConflict
				}
				return true, nil
			},
		)
	}()
	select {
	case <-attempted:
	case <-time.After(time.Second):
		t.Fatal("playback write did not attempt the locked transaction")
	}
	if err := writer.Rollback(); err != nil {
		t.Fatalf("release playback ticket writer: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("playback write after writer release: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("playback write did not resume after writer release")
	}
	if got := attempts.Load(); got < 2 {
		t.Fatalf("playback write lock attempts = %d, want at least 2", got)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("playback write after writer release took %v, want <= 1s", elapsed)
	}
	after := readPlaybackTicketContextSnapshot(t, store, binding)
	if after.state != string(models.PlaybackTicketReady) {
		t.Fatalf("ticket state after released lock = %q, want ready", after.state)
	}
}

func readPlaybackSQLiteBusyTimeout(t *testing.T, store *Store) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, err := store.DB.Conn(ctx)
	if err != nil {
		t.Fatalf("acquire connection for busy timeout check: %v", err)
	}
	defer conn.Close()
	var timeout int
	if err := conn.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&timeout); err != nil {
		t.Fatalf("read busy timeout: %v", err)
	}
	return timeout
}

func TestPlaybackWriteTxRestoresSQLiteBusyTimeout(t *testing.T) {
	for _, tc := range []struct {
		name            string
		expectedVersion func(*models.PlaybackTicket) int64
		wantErr         error
	}{
		{
			name: "commit",
			expectedVersion: func(ticket *models.PlaybackTicket) int64 {
				return ticket.Version
			},
		},
		{
			name: "rollback",
			expectedVersion: func(ticket *models.PlaybackTicket) int64 {
				return ticket.Version + 1
			},
			wantErr: ErrPlaybackTicketConflict,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := openPlaybackTestStore(t)
			now := time.Date(2026, 10, 26, 23, 0, 0, 0, time.UTC)
			fixture := addPlaybackTestFixture(t, store, "busy-timeout-"+tc.name,
				"busy-timeout-"+tc.name, now)
			ticket, err := store.CreatePlaybackTicket(models.PlaybackTicketRequest{
				UserID: fixture.user.ID, DeviceID: fixture.device.ID, FileID: fixture.fileID,
				CandidateTransports: []string{models.PlaybackTransportServerProxy},
				MediaSummary:        json.RawMessage(`{}`),
			}, now)
			if err != nil {
				t.Fatalf("create probing ticket: %v", err)
			}
			store.DB.SetMaxOpenConns(1)
			finished, err := store.FinishPlaybackTicketProbeContext(
				context.Background(),
				fixture.bindingWith(ticket.TicketID),
				tc.expectedVersion(ticket),
				models.PlaybackTicketProbeResult{
					State:               models.PlaybackTicketReady,
					CandidateTransports: []string{models.PlaybackTransportServerProxy},
					MediaSummary:        json.RawMessage(`{}`),
					VideoIndex:          playbackVideoIndexPtr(0),
				},
				now.Add(time.Second),
			)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("probe finalization error = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr != nil && finished != nil {
				t.Fatalf("failed probe finalization returned ticket: %+v", finished)
			}
			if timeout := readPlaybackSQLiteBusyTimeout(t, store); timeout != playbackSQLiteBusyTimeout {
				t.Fatalf("busy_timeout after %s = %d, want %d",
					tc.name, timeout, playbackSQLiteBusyTimeout)
			}
		})
	}
}

func TestDiscardPlaybackSQLConnDropsConnectionState(t *testing.T) {
	store := openPlaybackTestStore(t)
	store.DB.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, err := store.DB.Conn(ctx)
	if err != nil {
		t.Fatalf("acquire connection to discard: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA busy_timeout=123`); err != nil {
		t.Fatalf("set connection-local busy timeout: %v", err)
	}
	var before int
	if err := conn.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&before); err != nil {
		t.Fatalf("read modified busy timeout: %v", err)
	}
	if before != 123 {
		t.Fatalf("modified busy timeout = %d, want 123", before)
	}
	discardPlaybackSQLConn(conn)

	if after := readPlaybackSQLiteBusyTimeout(t, store); after != playbackSQLiteBusyTimeout {
		t.Fatalf("replacement connection busy timeout = %d, want %d",
			after, playbackSQLiteBusyTimeout)
	}
}

func TestPlaybackWriteTxBudgetCoversConnectionPoolWait(t *testing.T) {
	store := openPlaybackTestStore(t)
	store.DB.SetMaxOpenConns(1)
	held, err := store.DB.Conn(context.Background())
	if err != nil {
		t.Fatalf("hold the only database connection: %v", err)
	}
	defer held.Close()

	started := time.Now()
	err = store.withPlaybackWriteTxConfigured(
		context.Background(),
		200*time.Millisecond,
		restorePlaybackBusyTimeout,
		func(context.Context, *sql.Tx) error { return nil },
		func(context.Context, *sql.Tx) (bool, error) { return true, nil },
	)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("pool wait error = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("pool wait took %v, want <= 1s", elapsed)
	}
}

func TestPlaybackWriteTxRestoreFailureDiscardsConnection(t *testing.T) {
	store := openPlaybackTestStore(t)
	now := time.Date(2026, 10, 26, 23, 10, 0, 0, time.UTC)
	fixture := addPlaybackTestFixture(t, store, "restore-failure", "restore-failure", now)
	ticket, err := store.CreatePlaybackTicket(models.PlaybackTicketRequest{
		UserID: fixture.user.ID, DeviceID: fixture.device.ID, FileID: fixture.fileID,
		CandidateTransports: []string{models.PlaybackTransportServerProxy},
		MediaSummary:        json.RawMessage(`{}`),
	}, now)
	if err != nil {
		t.Fatalf("create probing ticket: %v", err)
	}
	binding := fixture.bindingWith(ticket.TicketID)
	before := readPlaybackTicketContextSnapshot(t, store, binding)
	store.DB.SetMaxOpenConns(1)
	errRestore := errors.New("forced busy timeout restore failure")

	err = store.withPlaybackWriteTxConfigured(
		context.Background(),
		time.Second,
		func(*sql.Conn, *sql.Tx) error { return errRestore },
		func(ctx context.Context, tx *sql.Tx) error {
			return lockPlaybackTicketTxContext(ctx, tx, binding)
		},
		func(context.Context, *sql.Tx) (bool, error) { return true, nil },
	)
	if !errors.Is(err, errRestore) {
		t.Fatalf("restore failure = %v, want %v", err, errRestore)
	}
	after := readPlaybackTicketContextSnapshot(t, store, binding)
	if after != before {
		t.Fatalf("restore failure changed ticket\nbefore: %+v\nafter:  %+v", before, after)
	}
	if timeout := readPlaybackSQLiteBusyTimeout(t, store); timeout != playbackSQLiteBusyTimeout {
		t.Fatalf("replacement connection busy timeout = %d, want %d",
			timeout, playbackSQLiteBusyTimeout)
	}
}

func TestPlaybackWriteTxNormalizesRestoreContextCancellation(t *testing.T) {
	store := openPlaybackTestStore(t)
	now := time.Date(2026, 10, 26, 23, 15, 0, 0, time.UTC)
	fixture := addPlaybackTestFixture(t, store, "restore-cancel", "restore-cancel", now)
	ticket, err := store.CreatePlaybackTicket(models.PlaybackTicketRequest{
		UserID: fixture.user.ID, DeviceID: fixture.device.ID, FileID: fixture.fileID,
		CandidateTransports: []string{models.PlaybackTransportServerProxy},
		MediaSummary:        json.RawMessage(`{}`),
	}, now)
	if err != nil {
		t.Fatalf("create probing ticket: %v", err)
	}
	binding := fixture.bindingWith(ticket.TicketID)
	ctx, cancel := context.WithCancel(context.Background())

	err = store.withPlaybackWriteTxConfigured(
		ctx,
		time.Second,
		func(*sql.Conn, *sql.Tx) error {
			cancel()
			return sql.ErrTxDone
		},
		func(ctx context.Context, tx *sql.Tx) error {
			return lockPlaybackTicketTxContext(ctx, tx, binding)
		},
		func(context.Context, *sql.Tx) (bool, error) { return true, nil },
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("restore cancellation error = %v, want context.Canceled", err)
	}
}

func TestPlaybackWriteTxNormalizesCommitContextCancellation(t *testing.T) {
	store := openPlaybackTestStore(t)
	now := time.Date(2026, 10, 26, 23, 16, 0, 0, time.UTC)
	fixture := addPlaybackTestFixture(t, store, "commit-cancel", "commit-cancel", now)
	ticket, err := store.CreatePlaybackTicket(models.PlaybackTicketRequest{
		UserID: fixture.user.ID, DeviceID: fixture.device.ID, FileID: fixture.fileID,
		CandidateTransports: []string{models.PlaybackTransportServerProxy},
		MediaSummary:        json.RawMessage(`{}`),
	}, now)
	if err != nil {
		t.Fatalf("create probing ticket: %v", err)
	}
	binding := fixture.bindingWith(ticket.TicketID)
	ctx, cancel := context.WithCancel(context.Background())

	err = store.withPlaybackWriteTxConfiguredAndCommit(
		ctx,
		time.Second,
		restorePlaybackBusyTimeout,
		func(ctx context.Context, tx *sql.Tx) error {
			return lockPlaybackTicketTxContext(ctx, tx, binding)
		},
		func(ctx context.Context, tx *sql.Tx) (bool, error) {
			if _, err := tx.ExecContext(ctx, `PRAGMA defer_foreign_keys=ON`); err != nil {
				return false, err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO subtitles
				(file_id, path, format, convertible) VALUES (?,?,?,?)`,
				int64(999999), "/commit-failure.srt", "subrip", true); err != nil {
				return false, err
			}
			return true, nil
		},
		func(tx *sql.Tx) error {
			cancel()
			return tx.Commit()
		},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("commit cancellation error = %v, want context.Canceled", err)
	}
	var invalidRows int
	if err := store.DB.QueryRow(`SELECT COUNT(*) FROM subtitles
		WHERE path = '/commit-failure.srt'`).Scan(&invalidRows); err != nil {
		t.Fatalf("count failed commit rows: %v", err)
	}
	if invalidRows != 0 {
		t.Fatalf("failed commit persisted %d subtitle row(s)", invalidRows)
	}
}

func TestPlaybackWriteTxCancellationAfterMutationRollsBack(t *testing.T) {
	store := openPlaybackTestStore(t)
	now := time.Date(2026, 10, 26, 23, 5, 0, 0, time.UTC)
	fixture := addPlaybackTestFixture(t, store, "write-ghost", "write-ghost", now)
	ticket, err := store.CreatePlaybackTicket(models.PlaybackTicketRequest{
		UserID: fixture.user.ID, DeviceID: fixture.device.ID, FileID: fixture.fileID,
		CandidateTransports: []string{models.PlaybackTransportServerProxy},
		MediaSummary:        json.RawMessage(`{}`),
	}, now)
	if err != nil {
		t.Fatalf("create probing ticket: %v", err)
	}
	binding := fixture.bindingWith(ticket.TicketID)
	before := readPlaybackTicketContextSnapshot(t, store, binding)
	ctx, cancel := context.WithCancel(context.Background())

	err = store.withPlaybackWriteTx(
		ctx,
		func(ctx context.Context, tx *sql.Tx) error {
			return lockPlaybackTicketTxContext(ctx, tx, binding)
		},
		func(ctx context.Context, tx *sql.Tx) (bool, error) {
			if _, err := tx.ExecContext(ctx, `UPDATE playback_tickets
				SET state = ?, updated_at = ? WHERE id = ?`,
				models.PlaybackTicketFailed, now.Add(time.Second), ticket.ID); err != nil {
				return false, err
			}
			cancel()
			return true, nil
		},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled playback write error = %v, want context.Canceled", err)
	}
	after := readPlaybackTicketContextSnapshot(t, store, binding)
	if after != before {
		t.Fatalf("canceled playback write left a ghost mutation\nbefore: %+v\nafter:  %+v",
			before, after)
	}
}

func TestPlaybackProbeMetadataCommitsWithTicket(t *testing.T) {
	store := openPlaybackTestStore(t)
	now := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	fixture := addPlaybackTestFixture(t, store, "probe-metadata", "probe-metadata", now)
	file, err := store.FileByID(fixture.fileID)
	if err != nil {
		t.Fatalf("load playback file: %v", err)
	}
	ticket, err := store.CreatePlaybackTicket(models.PlaybackTicketRequest{
		UserID: fixture.user.ID, DeviceID: fixture.device.ID, FileID: fixture.fileID,
		MediaSummary: json.RawMessage(`{}`),
	}, now)
	if err != nil {
		t.Fatalf("create probing ticket: %v", err)
	}
	metadata := &PlaybackTicketProbeMetadata{
		FileProbe: `{"format":{"format_name":"matroska,webm"},"streams":[{"index":0,"codec_type":"video","codec_name":"h264"}]}`,
		FileVersion: &PlaybackTicketFileVersion{
			Size: file.Size, MTime: file.MTime,
		},
		Subtitles: []models.Subtitle{{
			FileID: fixture.fileID, Lang: "zh-CN", Title: "Embedded",
			Format: "subrip", Embedded: true, StreamIdx: 2, Convertible: true,
		}},
	}
	ready, err := store.FinishPlaybackTicketProbeWithMetadataContext(
		context.Background(),
		fixture.bindingWith(ticket.TicketID),
		ticket.Version,
		models.PlaybackTicketProbeResult{
			State:               models.PlaybackTicketReady,
			CandidateTransports: []string{models.PlaybackTransportServerProxy},
			MediaSummary:        json.RawMessage(`{"container":"mkv"}`),
			VideoIndex:          playbackVideoIndexPtr(0),
		},
		metadata,
		now.Add(time.Second),
	)
	if err != nil {
		t.Fatalf("finish probe with metadata: %v", err)
	}
	if ready.State != models.PlaybackTicketReady || ready.Version != 2 {
		t.Fatalf("ready ticket = %+v", ready)
	}

	var storedProbe string
	if err := store.DB.QueryRow(`SELECT probe FROM files WHERE id = ?`,
		fixture.fileID).Scan(&storedProbe); err != nil {
		t.Fatalf("read committed probe: %v", err)
	}
	if storedProbe != metadata.FileProbe {
		t.Fatalf("stored probe = %q, want %q", storedProbe, metadata.FileProbe)
	}
	subs, err := store.SubtitlesByFile(fixture.fileID)
	if err != nil {
		t.Fatalf("read committed subtitles: %v", err)
	}
	if len(subs) != 1 || len(ready.SubtitleIDs) != 1 ||
		ready.SubtitleIDs[0] != subs[0].ID {
		t.Fatalf("committed subtitles = %+v, ticket grants = %v",
			subs, ready.SubtitleIDs)
	}
}

func TestPlaybackProbeMetadataRequiresFileVersion(t *testing.T) {
	cases := []struct {
		name     string
		metadata func(fileID int64) *PlaybackTicketProbeMetadata
	}{
		{
			name: "file probe",
			metadata: func(int64) *PlaybackTicketProbeMetadata {
				return &PlaybackTicketProbeMetadata{
					FileProbe: `{"streams":[{"index":0,"codec_type":"video"}]}`,
				}
			},
		},
		{
			name: "subtitle rows",
			metadata: func(fileID int64) *PlaybackTicketProbeMetadata {
				return &PlaybackTicketProbeMetadata{
					Subtitles: []models.Subtitle{{
						FileID: fileID, Format: "subrip", Convertible: true,
					}},
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := openPlaybackTestStore(t)
			now := time.Date(2026, 10, 27, 9, 1, 0, 0, time.UTC)
			fixture := addPlaybackTestFixture(
				t, store, "probe-version-required", "probe-version-required", now)
			ticket, err := store.CreatePlaybackTicket(models.PlaybackTicketRequest{
				UserID: fixture.user.ID, DeviceID: fixture.device.ID,
				FileID: fixture.fileID,
			}, now)
			if err != nil {
				t.Fatalf("create probing ticket: %v", err)
			}

			finished, err := store.FinishPlaybackTicketProbeWithMetadataContext(
				context.Background(),
				fixture.bindingWith(ticket.TicketID),
				ticket.Version,
				models.PlaybackTicketProbeResult{
					State:               models.PlaybackTicketReady,
					CandidateTransports: []string{models.PlaybackTransportServerProxy},
					MediaSummary:        json.RawMessage(`{"container":"mkv"}`),
					VideoIndex:          playbackVideoIndexPtr(0),
				},
				tc.metadata(fixture.fileID),
				now.Add(time.Second),
			)
			if !errors.Is(err, ErrInvalidPlaybackTicket) {
				t.Fatalf("metadata without file version error = %v, want invalid", err)
			}
			if finished != nil {
				t.Fatalf("metadata without file version returned ticket: %+v", finished)
			}
			assertPlaybackProbeMetadataAbsent(t, store, fixture.fileID)
			persisted, err := store.PlaybackTicketByID(
				fixture.bindingWith(ticket.TicketID), now.Add(time.Second))
			if err != nil {
				t.Fatalf("reload ticket: %v", err)
			}
			if persisted.State != models.PlaybackTicketProbing ||
				persisted.Version != ticket.Version {
				t.Fatalf("ticket changed after invalid metadata: %+v", persisted)
			}
		})
	}
}

func TestPlaybackProbeEmptyMetadataCommitsWithoutFileVersion(t *testing.T) {
	store := openPlaybackTestStore(t)
	now := time.Date(2026, 10, 27, 9, 1, 30, 0, time.UTC)
	fixture := addPlaybackTestFixture(
		t, store, "empty-probe-metadata", "empty-probe-metadata", now)
	ticket, err := store.CreatePlaybackTicket(models.PlaybackTicketRequest{
		UserID: fixture.user.ID, DeviceID: fixture.device.ID, FileID: fixture.fileID,
	}, now)
	if err != nil {
		t.Fatalf("create probing ticket: %v", err)
	}
	ready, err := store.FinishPlaybackTicketProbeWithMetadataContext(
		context.Background(),
		fixture.bindingWith(ticket.TicketID),
		ticket.Version,
		models.PlaybackTicketProbeResult{
			State:               models.PlaybackTicketReady,
			CandidateTransports: []string{models.PlaybackTransportServerProxy},
			MediaSummary:        json.RawMessage(`{"container":"mkv"}`),
			VideoIndex:          playbackVideoIndexPtr(0),
		},
		&PlaybackTicketProbeMetadata{},
		now.Add(time.Second),
	)
	if err != nil {
		t.Fatalf("finish with empty metadata: %v", err)
	}
	if ready.State != models.PlaybackTicketReady {
		t.Fatalf("empty metadata ticket state = %q, want ready", ready.State)
	}
	assertPlaybackProbeMetadataAbsent(t, store, fixture.fileID)
}

func TestPlaybackProbeMetadataRejectsChangedFileGeneration(t *testing.T) {
	store := openPlaybackTestStore(t)
	now := time.Date(2026, 10, 27, 9, 2, 0, 0, time.UTC)
	fixture := addPlaybackTestFixture(
		t, store, "probe-generation", "probe-generation", now)
	file, err := store.FileByID(fixture.fileID)
	if err != nil {
		t.Fatalf("load playback file: %v", err)
	}
	ticket, err := store.CreatePlaybackTicket(models.PlaybackTicketRequest{
		UserID: fixture.user.ID, DeviceID: fixture.device.ID, FileID: fixture.fileID,
	}, now)
	if err != nil {
		t.Fatalf("create probing ticket: %v", err)
	}
	file.Size++
	file.MTime++
	if _, err := store.UpsertFile(file); err != nil {
		t.Fatalf("replace file generation: %v", err)
	}

	_, err = store.FinishPlaybackTicketProbeWithMetadataContext(
		context.Background(),
		fixture.bindingWith(ticket.TicketID),
		ticket.Version,
		models.PlaybackTicketProbeResult{
			State:               models.PlaybackTicketReady,
			CandidateTransports: []string{models.PlaybackTransportServerProxy},
			MediaSummary:        json.RawMessage(`{"container":"mkv"}`),
			VideoIndex:          playbackVideoIndexPtr(0),
		},
		&PlaybackTicketProbeMetadata{
			FileProbe: `{"streams":[{"index":0,"codec_type":"video"}]}`,
			FileVersion: &PlaybackTicketFileVersion{
				Size: file.Size - 1, MTime: file.MTime - 1,
			},
			Subtitles: []models.Subtitle{{
				FileID: fixture.fileID, Path: "stale.srt",
				Format: "subrip", Convertible: true,
			}},
		},
		now.Add(time.Second),
	)
	if !errors.Is(err, ErrPlaybackTicketConflict) {
		t.Fatalf("stale generation finalize error = %v, want conflict", err)
	}
	assertPlaybackProbeMetadataAbsent(t, store, fixture.fileID)
}

func TestUpsertFileClearsProbeOnlyForChangedGeneration(t *testing.T) {
	store := openPlaybackTestStore(t)
	now := time.Date(2026, 10, 27, 9, 2, 30, 0, time.UTC)
	fixture := addPlaybackTestFixture(
		t, store, "probe-upsert", "probe-upsert", now)
	file, err := store.FileByID(fixture.fileID)
	if err != nil {
		t.Fatalf("load playback file: %v", err)
	}
	file.Probe = `{"streams":[{"index":0,"codec_type":"video"}]}`
	if _, err := store.UpsertFile(file); err != nil {
		t.Fatalf("store initial probe: %v", err)
	}

	unchanged := *file
	unchanged.Probe = ""
	if _, err := store.UpsertFile(&unchanged); err != nil {
		t.Fatalf("rescan unchanged generation: %v", err)
	}
	stored, err := store.FileByID(fixture.fileID)
	if err != nil {
		t.Fatalf("reload unchanged file: %v", err)
	}
	if stored.Probe != file.Probe {
		t.Fatalf("unchanged rescan cleared probe: %q", stored.Probe)
	}

	changed := *file
	changed.Probe = ""
	changed.Size++
	changed.MTime++
	if _, err := store.UpsertFile(&changed); err != nil {
		t.Fatalf("rescan changed generation: %v", err)
	}
	stored, err = store.FileByID(fixture.fileID)
	if err != nil {
		t.Fatalf("reload changed file: %v", err)
	}
	if stored.Probe != "" {
		t.Fatalf("changed generation retained stale probe: %q", stored.Probe)
	}
	if err := store.UpdateFileProbeForVersion(
		file.ID, file.Size, file.MTime, file.Probe); !errors.Is(err, ErrMediaFileChanged) {
		t.Fatalf("stale probe update error = %v, want ErrMediaFileChanged", err)
	}
}

func TestUpdateFileProbeAndEmbeddedSubtitlesReplacesOnlyEmbeddedRows(t *testing.T) {
	store := openPlaybackTestStore(t)
	now := time.Date(2026, 10, 27, 9, 2, 45, 0, time.UTC)
	fixture := addPlaybackTestFixture(
		t, store, "probe-subtitle-atomic", "probe-subtitle-atomic", now)
	external := &models.Subtitle{
		FileID: fixture.fileID, Path: "/subtitles/movie.en.srt",
		Lang: "en", Format: "subrip", Convertible: true,
	}
	if err := store.UpsertSubtitle(external); err != nil {
		t.Fatalf("seed external subtitle: %v", err)
	}
	keep := &models.Subtitle{
		FileID: fixture.fileID, Lang: "zh", Format: "subrip",
		Embedded: true, StreamIdx: 2, Convertible: true,
	}
	stale := &models.Subtitle{
		FileID: fixture.fileID, Lang: "en", Format: "ass",
		Embedded: true, StreamIdx: 3, Convertible: true,
	}
	for _, sub := range []*models.Subtitle{keep, stale} {
		if err := store.UpsertSubtitle(sub); err != nil {
			t.Fatalf("seed embedded subtitle: %v", err)
		}
	}
	seeded, err := store.SubtitlesByFile(fixture.fileID)
	if err != nil {
		t.Fatalf("read seeded subtitles: %v", err)
	}
	var keepID, externalID int64
	for _, sub := range seeded {
		switch {
		case sub.Embedded && sub.StreamIdx == 2:
			keepID = sub.ID
		case !sub.Embedded && sub.Path == external.Path:
			externalID = sub.ID
		}
	}
	if keepID == 0 || externalID == 0 {
		t.Fatalf("seeded subtitle ids missing: %+v", seeded)
	}
	file, err := store.FileByID(fixture.fileID)
	if err != nil {
		t.Fatalf("load file: %v", err)
	}
	replacement := []models.Subtitle{
		{
			FileID: fixture.fileID, Lang: "zh-Hans", Title: "Updated",
			Format: "subrip", Embedded: true, StreamIdx: 2, Convertible: true,
		},
		{
			FileID: fixture.fileID, Lang: "ja", Title: "New",
			Format: "ass", Embedded: true, StreamIdx: 4, Convertible: true,
		},
	}
	if err := store.UpdateFileProbeAndEmbeddedSubtitlesForVersionContext(
		context.Background(), file.ID, file.Size, file.MTime,
		`{"streams":[{"index":0,"codec_type":"video"}]}`, replacement); err != nil {
		t.Fatalf("update probe and embedded subtitles: %v", err)
	}

	stored, err := store.FileByID(file.ID)
	if err != nil {
		t.Fatalf("reload file: %v", err)
	}
	if stored.Probe != `{"streams":[{"index":0,"codec_type":"video"}]}` {
		t.Fatalf("stored probe = %q", stored.Probe)
	}
	subs, err := store.SubtitlesByFile(file.ID)
	if err != nil {
		t.Fatalf("read subtitles: %v", err)
	}
	if len(subs) != 3 {
		t.Fatalf("subtitle rows = %+v, want two embedded and one external", subs)
	}
	var keptUpdated, newEmbedded, externalKept bool
	for _, sub := range subs {
		switch {
		case sub.ID == keepID:
			keptUpdated = sub.Embedded && sub.StreamIdx == 2 &&
				sub.Lang == "zh-Hans" && sub.Title == "Updated"
		case sub.ID == externalID:
			externalKept = !sub.Embedded &&
				sub.Path == external.Path && sub.Lang == external.Lang
		case sub.Embedded && sub.StreamIdx == 4:
			newEmbedded = sub.Path == "" && sub.Lang == "ja" && sub.Title == "New"
		case sub.Embedded && sub.StreamIdx == 3:
			t.Fatalf("stale embedded subtitle survived: %+v", sub)
		}
	}
	if !keptUpdated || !newEmbedded || !externalKept {
		t.Fatalf("unexpected subtitle rows: %+v", subs)
	}
}

func TestUpdateFileProbeAndEmbeddedSubtitlesVersionMismatchWritesNothing(t *testing.T) {
	store := openPlaybackTestStore(t)
	now := time.Date(2026, 10, 27, 9, 2, 50, 0, time.UTC)
	fixture := addPlaybackTestFixture(
		t, store, "probe-subtitle-cas", "probe-subtitle-cas", now)
	file, err := store.FileByID(fixture.fileID)
	if err != nil {
		t.Fatalf("load file: %v", err)
	}
	initialProbe := `{"streams":[{"index":0,"codec_type":"video","codec_name":"h264"}]}`
	initial := []models.Subtitle{{
		FileID: file.ID, Lang: "zh", Format: "subrip",
		Embedded: true, StreamIdx: 2, Convertible: true,
	}}
	if err := store.UpdateFileProbeAndEmbeddedSubtitlesForVersionContext(
		context.Background(), file.ID, file.Size, file.MTime,
		initialProbe, initial); err != nil {
		t.Fatalf("seed atomic probe metadata: %v", err)
	}
	before, err := store.SubtitlesByFile(file.ID)
	if err != nil {
		t.Fatalf("read initial subtitles: %v", err)
	}

	err = store.UpdateFileProbeAndEmbeddedSubtitlesForVersionContext(
		context.Background(), file.ID, file.Size-1, file.MTime,
		`{"streams":[]}`,
		[]models.Subtitle{{
			FileID: file.ID, Lang: "en", Format: "ass",
			Embedded: true, StreamIdx: 3, Convertible: true,
		}})
	if !errors.Is(err, ErrMediaFileChanged) {
		t.Fatalf("stale atomic update error = %v, want ErrMediaFileChanged", err)
	}
	stored, err := store.FileByID(file.ID)
	if err != nil {
		t.Fatalf("reload file after stale update: %v", err)
	}
	if stored.Probe != initialProbe {
		t.Fatalf("stale atomic update changed probe to %q", stored.Probe)
	}
	after, err := store.SubtitlesByFile(file.ID)
	if err != nil {
		t.Fatalf("read subtitles after stale update: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("stale atomic update changed subtitles: before=%+v after=%+v",
			before, after)
	}
	for i := range before {
		if after[i].ID != before[i].ID ||
			after[i].Path != before[i].Path ||
			after[i].StreamIdx != before[i].StreamIdx ||
			after[i].Lang != before[i].Lang {
			t.Fatalf("stale atomic update changed subtitles: before=%+v after=%+v",
				before, after)
		}
	}
}

func TestPlaybackProbeExpiresWhileWaitingForWriterLock(t *testing.T) {
	store := openPlaybackTestStore(t)
	now := time.Date(2026, 10, 27, 9, 3, 0, 0, time.UTC)
	fixture := addPlaybackTestFixture(
		t, store, "probe-wait-expiry", "probe-wait-expiry", now)
	file, err := store.FileByID(fixture.fileID)
	if err != nil {
		t.Fatalf("load playback file: %v", err)
	}
	ticket, err := store.CreatePlaybackTicket(models.PlaybackTicketRequest{
		UserID: fixture.user.ID, DeviceID: fixture.device.ID, FileID: fixture.fileID,
	}, now)
	if err != nil {
		t.Fatalf("create probing ticket: %v", err)
	}
	binding := fixture.bindingWith(ticket.TicketID)
	writer := lockPlaybackTicketWriter(t, store, binding)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	type result struct {
		err error
	}
	done := make(chan result, 1)
	connectionBaseline := store.DB.Stats().InUse
	started := time.Now()
	go func() {
		_, err := store.FinishPlaybackTicketProbeWithMetadataContext(
			ctx,
			binding,
			ticket.Version,
			models.PlaybackTicketProbeResult{
				State:               models.PlaybackTicketReady,
				CandidateTransports: []string{models.PlaybackTransportServerProxy},
				MediaSummary:        json.RawMessage(`{"container":"mkv"}`),
				VideoIndex:          playbackVideoIndexPtr(0),
			},
			&PlaybackTicketProbeMetadata{
				FileProbe: `{"streams":[{"index":0,"codec_type":"video"}]}`,
				FileVersion: &PlaybackTicketFileVersion{
					Size: file.Size, MTime: file.MTime,
				},
				Subtitles: []models.Subtitle{{
					FileID: fixture.fileID, Path: "expired.srt",
					Format: "subrip", Convertible: true,
				}},
			},
			now.Add(models.PlaybackTicketTTL-150*time.Millisecond),
		)
		done <- result{err: err}
	}()
	waitForPlaybackConnections(t, store, connectionBaseline+1)
	releaseTimer := time.NewTimer(time.Until(started.Add(300 * time.Millisecond)))
	defer releaseTimer.Stop()
	select {
	case got := <-done:
		t.Fatalf("finalize returned before the writer lock was released: %v", got.err)
	case <-releaseTimer.C:
	}
	if err := writer.Rollback(); err != nil {
		t.Fatalf("release playback ticket writer: %v", err)
	}
	var got result
	select {
	case got = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("finalize did not return after the writer lock was released")
	}
	if !errors.Is(got.err, ErrPlaybackTicketExpired) {
		t.Fatalf("finalize after lock wait = %v, want expired", got.err)
	}
	assertPlaybackProbeMetadataAbsent(t, store, fixture.fileID)
	persisted, err := store.PlaybackTicketByID(
		binding, now.Add(models.PlaybackTicketTTL+time.Second))
	if err != nil {
		t.Fatalf("reload expired ticket: %v", err)
	}
	if persisted.State != models.PlaybackTicketExpired {
		t.Fatalf("ticket after lock-wait expiry = %q, want expired", persisted.State)
	}
}

func TestPlaybackProbeMetadataIsAbsentAfterRevocation(t *testing.T) {
	store := openPlaybackTestStore(t)
	now := time.Date(2026, 10, 27, 9, 5, 0, 0, time.UTC)
	fixture := addPlaybackTestFixture(t, store, "probe-revoke", "probe-revoke", now)
	ticket, err := store.CreatePlaybackTicket(models.PlaybackTicketRequest{
		UserID: fixture.user.ID, DeviceID: fixture.device.ID, FileID: fixture.fileID,
	}, now)
	if err != nil {
		t.Fatalf("create probing ticket: %v", err)
	}
	binding := fixture.bindingWith(ticket.TicketID)
	if _, err := store.RevokePlaybackTicket(binding, ticket.Version, now.Add(time.Second)); err != nil {
		t.Fatalf("revoke probing ticket: %v", err)
	}
	_, err = store.FinishPlaybackTicketProbeWithMetadataContext(
		context.Background(), binding, ticket.Version,
		models.PlaybackTicketProbeResult{
			State:               models.PlaybackTicketReady,
			CandidateTransports: []string{models.PlaybackTransportServerProxy},
			MediaSummary:        json.RawMessage(`{"container":"mkv"}`),
			VideoIndex:          playbackVideoIndexPtr(0),
		},
		&PlaybackTicketProbeMetadata{
			FileProbe: `{"streams":[{"index":0,"codec_type":"video"}]}`,
			FileVersion: &PlaybackTicketFileVersion{
				Size: 1024, MTime: now.Unix(),
			},
			Subtitles: []models.Subtitle{{
				FileID: fixture.fileID, Path: "late.srt", Format: "subrip",
				Convertible: true,
			}},
		},
		now.Add(2*time.Second),
	)
	if !errors.Is(err, ErrPlaybackTicketRevoked) {
		t.Fatalf("late probe error = %v, want revoked", err)
	}
	assertPlaybackProbeMetadataAbsent(t, store, fixture.fileID)
}

func TestPlaybackProbeMetadataRollsBackWhenTicketCASFails(t *testing.T) {
	store := openPlaybackTestStore(t)
	now := time.Date(2026, 10, 27, 9, 10, 0, 0, time.UTC)
	fixture := addPlaybackTestFixture(t, store, "probe-rollback", "probe-rollback", now)
	ticket, err := store.CreatePlaybackTicket(models.PlaybackTicketRequest{
		UserID: fixture.user.ID, DeviceID: fixture.device.ID, FileID: fixture.fileID,
	}, now)
	if err != nil {
		t.Fatalf("create probing ticket: %v", err)
	}
	if _, err := store.DB.Exec(`CREATE TRIGGER reject_playback_ready
		BEFORE UPDATE OF state ON playback_tickets
		WHEN NEW.state = 'ready'
		BEGIN
			SELECT RAISE(ABORT, 'forced ticket CAS failure');
		END`); err != nil {
		t.Fatalf("create forced failure trigger: %v", err)
	}
	_, err = store.FinishPlaybackTicketProbeWithMetadataContext(
		context.Background(), fixture.bindingWith(ticket.TicketID), ticket.Version,
		models.PlaybackTicketProbeResult{
			State:               models.PlaybackTicketReady,
			CandidateTransports: []string{models.PlaybackTransportServerProxy},
			MediaSummary:        json.RawMessage(`{"container":"mkv"}`),
			VideoIndex:          playbackVideoIndexPtr(0),
		},
		&PlaybackTicketProbeMetadata{
			FileProbe: `{"streams":[{"index":0,"codec_type":"video"}]}`,
			FileVersion: &PlaybackTicketFileVersion{
				Size: 1024, MTime: now.Unix(),
			},
			Subtitles: []models.Subtitle{{
				FileID: fixture.fileID, Path: "rollback.srt", Format: "subrip",
				Convertible: true,
			}},
		},
		now.Add(time.Second),
	)
	if err == nil {
		t.Fatal("forced ticket CAS failure unexpectedly committed")
	}
	assertPlaybackProbeMetadataAbsent(t, store, fixture.fileID)
	persisted, err := store.PlaybackTicketByID(
		fixture.bindingWith(ticket.TicketID), now.Add(2*time.Second))
	if err != nil {
		t.Fatalf("reload ticket after rollback: %v", err)
	}
	if persisted.State != models.PlaybackTicketProbing || persisted.Version != 1 {
		t.Fatalf("ticket after rollback = %+v", persisted)
	}
}

func TestFailInterruptedPlaybackProbesConvergesAndOldWorkerLoses(t *testing.T) {
	store := openPlaybackTestStore(t)
	now := time.Date(2026, 10, 27, 9, 15, 0, 0, time.UTC)
	fixture := addPlaybackTestFixture(t, store, "probe-restart", "probe-restart", now)
	expired, err := store.CreatePlaybackTicket(models.PlaybackTicketRequest{
		UserID: fixture.user.ID, DeviceID: fixture.device.ID, FileID: fixture.fileID,
	}, now)
	if err != nil {
		t.Fatalf("create expired probing ticket: %v", err)
	}
	live, err := store.CreatePlaybackTicket(models.PlaybackTicketRequest{
		UserID: fixture.user.ID, DeviceID: fixture.device.ID, FileID: fixture.fileID,
	}, now.Add(6*time.Minute))
	if err != nil {
		t.Fatalf("create live probing ticket: %v", err)
	}
	recoveryAt := now.Add(6 * time.Minute)
	interrupted, err := store.FailInterruptedPlaybackProbes(recoveryAt)
	if err != nil {
		t.Fatalf("recover interrupted probes: %v", err)
	}
	if interrupted != 1 {
		t.Fatalf("interrupted tickets = %d, want 1", interrupted)
	}
	expiredTicket, err := store.PlaybackTicketByID(
		fixture.bindingWith(expired.TicketID), recoveryAt)
	if err != nil {
		t.Fatalf("read expired recovered ticket: %v", err)
	}
	if expiredTicket.State != models.PlaybackTicketExpired {
		t.Fatalf("overdue ticket state = %q, want expired", expiredTicket.State)
	}
	failedTicket, err := store.PlaybackTicketByID(
		fixture.bindingWith(live.TicketID), recoveryAt)
	if err != nil {
		t.Fatalf("read interrupted ticket: %v", err)
	}
	if failedTicket.State != models.PlaybackTicketFailed ||
		failedTicket.FailureCode != "probe_interrupted" {
		t.Fatalf("interrupted ticket = %+v", failedTicket)
	}
	if failedTicket.Version != live.Version+1 {
		t.Fatalf("interrupted ticket version = %d, want %d",
			failedTicket.Version, live.Version+1)
	}

	_, err = store.FinishPlaybackTicketProbeWithMetadataContext(
		context.Background(),
		fixture.bindingWith(live.TicketID),
		live.Version,
		models.PlaybackTicketProbeResult{
			State:               models.PlaybackTicketReady,
			CandidateTransports: []string{models.PlaybackTransportServerProxy},
			MediaSummary:        json.RawMessage(`{"container":"mkv"}`),
			VideoIndex:          playbackVideoIndexPtr(0),
		},
		&PlaybackTicketProbeMetadata{
			FileProbe: `{"streams":[{"index":0,"codec_type":"video"}]}`,
			FileVersion: &PlaybackTicketFileVersion{
				Size: 1024, MTime: now.Unix(),
			},
			Subtitles: []models.Subtitle{{
				FileID: fixture.fileID, Path: "stale-worker.srt",
				Format: "subrip", Convertible: true,
			}},
		},
		recoveryAt.Add(time.Second),
	)
	if !errors.Is(err, ErrPlaybackTicketConflict) {
		t.Fatalf("old worker finalize error = %v, want conflict", err)
	}
	assertPlaybackProbeMetadataAbsent(t, store, fixture.fileID)

	again, err := store.FailInterruptedPlaybackProbes(recoveryAt.Add(time.Second))
	if err != nil {
		t.Fatalf("repeat interrupted recovery: %v", err)
	}
	if again != 0 {
		t.Fatalf("repeat interrupted recovery = %d, want 0", again)
	}
	unchanged, err := store.PlaybackTicketByID(
		fixture.bindingWith(live.TicketID), recoveryAt.Add(time.Second))
	if err != nil {
		t.Fatalf("read ticket after repeated recovery: %v", err)
	}
	if unchanged.Version != failedTicket.Version {
		t.Fatalf("repeated recovery advanced version %d -> %d",
			failedTicket.Version, unchanged.Version)
	}
}

func assertPlaybackProbeMetadataAbsent(t *testing.T, store *Store, fileID int64) {
	t.Helper()
	var probe string
	if err := store.DB.QueryRow(`SELECT probe FROM files WHERE id = ?`, fileID).
		Scan(&probe); err != nil {
		t.Fatalf("read file probe: %v", err)
	}
	if probe != "" {
		t.Fatalf("unexpected file probe write: %q", probe)
	}
	var subtitles int
	if err := store.DB.QueryRow(`SELECT COUNT(*) FROM subtitles WHERE file_id = ?`,
		fileID).Scan(&subtitles); err != nil {
		t.Fatalf("count subtitles: %v", err)
	}
	if subtitles != 0 {
		t.Fatalf("unexpected subtitle writes: %d", subtitles)
	}
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

func downgradePlaybackTransportCheckTable(
	t *testing.T,
	db *sql.DB,
	table, definition, columns string,
) {
	t.Helper()
	var legacyAlterTable int
	if err := db.QueryRow(`PRAGMA legacy_alter_table`).Scan(&legacyAlterTable); err != nil {
		t.Fatalf("read legacy ALTER TABLE mode: %v", err)
	}
	if legacyAlterTable == 0 {
		if _, err := db.Exec(`PRAGMA legacy_alter_table=ON`); err != nil {
			t.Fatalf("enable legacy ALTER TABLE mode: %v", err)
		}
		defer func() {
			if _, err := db.Exec(`PRAGMA legacy_alter_table=OFF`); err != nil {
				t.Errorf("restore legacy ALTER TABLE mode: %v", err)
			}
		}()
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin %s downgrade: %v", table, err)
	}
	defer tx.Rollback()
	var oldSequence sql.NullInt64
	err = tx.QueryRow(`SELECT seq FROM sqlite_sequence WHERE name = ?`, table).
		Scan(&oldSequence)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("read %s downgrade sequence: %v", table, err)
	}

	rows, err := tx.Query(`SELECT sql FROM sqlite_master
		WHERE tbl_name = ? AND type IN ('index', 'trigger') AND sql IS NOT NULL`,
		table)
	if err != nil {
		t.Fatalf("read %s schema objects: %v", table, err)
	}
	var objects []string
	for rows.Next() {
		var statement string
		if err := rows.Scan(&statement); err != nil {
			rows.Close()
			t.Fatalf("scan %s schema object: %v", table, err)
		}
		objects = append(objects, statement)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close %s schema objects: %v", table, err)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read %s schema objects: %v", table, err)
	}

	legacyTable := table + "_legacy"
	legacyDefinition := strings.Replace(
		definition,
		"CREATE TABLE IF NOT EXISTS "+table,
		"CREATE TABLE "+legacyTable,
		1,
	)
	if legacyDefinition == definition {
		t.Fatalf("build legacy %s definition", table)
	}
	legacyDefinition = strings.ReplaceAll(legacyDefinition, ",'direct-dash'", "")
	if strings.Contains(legacyDefinition, "direct-dash") {
		t.Fatalf("legacy %s definition still contains direct-dash", table)
	}
	if _, err := tx.Exec(`DROP TABLE IF EXISTS ` + legacyTable); err != nil {
		t.Fatalf("drop stale legacy %s table: %v", table, err)
	}
	if _, err := tx.Exec(legacyDefinition); err != nil {
		t.Fatalf("create legacy %s table: %v", table, err)
	}
	if _, err := tx.Exec(`INSERT INTO ` + legacyTable + ` (` + columns + `)
		SELECT ` + columns + ` FROM ` + table); err != nil {
		t.Fatalf("copy legacy %s rows: %v", table, err)
	}
	if _, err := tx.Exec(`DROP TABLE ` + table); err != nil {
		t.Fatalf("drop current %s table: %v", table, err)
	}
	if _, err := tx.Exec(`ALTER TABLE ` + legacyTable + ` RENAME TO ` + table); err != nil {
		t.Fatalf("rename legacy %s table: %v", table, err)
	}
	for _, statement := range objects {
		if _, err := tx.Exec(statement); err != nil {
			t.Fatalf("restore legacy %s schema object: %v", table, err)
		}
	}
	var maxID sql.NullInt64
	if err := tx.QueryRow(`SELECT MAX(id) FROM ` + table).Scan(&maxID); err != nil {
		t.Fatalf("read %s downgrade id high-water mark: %v", table, err)
	}
	highWater := int64(0)
	restoreSequence := oldSequence.Valid || maxID.Valid
	if oldSequence.Valid {
		highWater = oldSequence.Int64
	}
	if maxID.Valid && maxID.Int64 > highWater {
		highWater = maxID.Int64
	}
	if restoreSequence {
		if _, err := tx.Exec(`DELETE FROM sqlite_sequence WHERE name = ?`, table); err != nil {
			t.Fatalf("clear %s downgrade sequence: %v", table, err)
		}
		if _, err := tx.Exec(`INSERT INTO sqlite_sequence(name, seq) VALUES (?, ?)`,
			table, highWater); err != nil {
			t.Fatalf("restore %s downgrade sequence: %v", table, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit %s downgrade: %v", table, err)
	}
}

func playbackColumnDefault(
	t *testing.T,
	store *Store,
	table, column string,
) string {
	t.Helper()
	var defaultValue sql.NullString
	if err := store.DB.QueryRow(`SELECT dflt_value
		FROM pragma_table_info(?) WHERE name = ?`, table, column).
		Scan(&defaultValue); err != nil {
		t.Fatalf("read %s.%s default: %v", table, column, err)
	}
	if !defaultValue.Valid {
		t.Fatalf("%s.%s has no default", table, column)
	}
	return defaultValue.String
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
