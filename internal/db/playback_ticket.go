package db

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"doriyamedia/internal/models"
)

// Playback persistence treats missing and cross-bound resources as the same
// not-found case, so callers cannot probe another user's identifiers.
var (
	ErrPlaybackTicketNotFound       = errors.New("playback ticket not found")
	ErrPlaybackTicketExpired        = errors.New("playback ticket expired")
	ErrPlaybackTicketRevoked        = errors.New("playback ticket revoked")
	ErrPlaybackTicketExhausted      = errors.New("playback ticket exhausted")
	ErrPlaybackTicketConflict       = errors.New("playback ticket state conflict")
	ErrInvalidPlaybackTicket        = errors.New("invalid playback ticket")
	ErrInvalidPlaybackTransport     = errors.New("invalid playback transport")
	ErrPlaybackMediaSessionNotFound = errors.New("playback media session not found")
	ErrPlaybackMediaSessionExpired  = errors.New("playback media session expired")
	ErrPlaybackMediaSessionRevoked  = errors.New("playback media session revoked")
	ErrPlaybackMediaSessionConflict = errors.New("playback media session state conflict")
	ErrInvalidPlaybackMediaSession  = errors.New("invalid playback media session")
)

// PlaybackMediaSessionAccess is the live authorization result for one
// capability-addressed media request.
type PlaybackMediaSessionAccess struct {
	Session *models.PlaybackMediaSession
	User    *models.User
	Device  *models.Device
}

const (
	playbackTicketsTable             = "playback_tickets"
	playbackMediaSessionsTable       = "playback_media_sessions"
	maxPlaybackMediaSummary          = 64 << 10
	maxPlaybackCapabilities          = 16 << 10
	playbackWriteLockBudget          = 10 * time.Second
	playbackWriteLockMinBackoff      = time.Millisecond
	playbackWriteLockMaxBackoff      = 25 * time.Millisecond
	playbackSQLiteBusyTimeout        = 10000
	playbackSQLiteBusyTimeoutCleanup = 2 * time.Second
	maxPlaybackProbeJSON             = 4 << 20
)

// PlaybackTicketProbeMetadata is the shared cache written atomically with a
// ticket's final probe transition. Subtitles contains only candidates selected
// for this ticket; the transaction must not use it to delete other tracks.
type PlaybackTicketFileVersion struct {
	Size  int64
	MTime int64
}

type PlaybackTicketProbeMetadata struct {
	FileProbe   string
	FileVersion *PlaybackTicketFileVersion
	Subtitles   []models.Subtitle
}

// Playback persistence stores only opaque ticket/session identifiers, hashed
// session capabilities, transport names, and non-sensitive media summaries.
// Source URLs and source headers are deliberately absent.
const playbackTicketsTableDefinition = `CREATE TABLE IF NOT EXISTS playback_tickets (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		ticket_id TEXT NOT NULL,
		user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		device_id INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
		file_id INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
		purpose TEXT NOT NULL DEFAULT 'playback' CHECK(purpose = 'playback'),
		state TEXT NOT NULL DEFAULT 'probing' CHECK(state IN (
			'probing','ready','consuming','unsupported','failed','expired','revoked','exhausted'
		)),
		candidate_transports TEXT NOT NULL DEFAULT '[]',
		attempted_transports TEXT NOT NULL DEFAULT '[]',
		selected_transport TEXT NOT NULL DEFAULT '' CHECK(selected_transport IN (
			'','direct-http','direct-dash','server-proxy','server-hls'
		)),
		media_summary_json TEXT NOT NULL DEFAULT '{}',
		client_capabilities_json TEXT NOT NULL DEFAULT '{}',
		subtitle_ids_json TEXT NOT NULL DEFAULT '[]',
		failure_code TEXT NOT NULL DEFAULT '',
		video_index INTEGER NOT NULL DEFAULT -1 CHECK(video_index >= -1),
		claim_token TEXT NOT NULL DEFAULT '',
		claim_expires_at TIMESTAMP,
		version INTEGER NOT NULL DEFAULT 1 CHECK(version > 0),
		created_at TIMESTAMP NOT NULL,
		expires_at TIMESTAMP NOT NULL,
		updated_at TIMESTAMP NOT NULL,
		consumed_at TIMESTAMP,
		revoked_at TIMESTAMP
	)`

var playbackMigrationStatements = []string{
	playbackTicketsTableDefinition,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_playback_tickets_public_id
		ON playback_tickets(ticket_id)`,
	`CREATE INDEX IF NOT EXISTS idx_playback_tickets_binding
		ON playback_tickets(user_id, device_id, file_id, ticket_id)`,
	`CREATE INDEX IF NOT EXISTS idx_playback_tickets_state_expiry
		ON playback_tickets(state, expires_at)`,
	`CREATE INDEX IF NOT EXISTS idx_playback_tickets_device
		ON playback_tickets(device_id, state)`,
	`CREATE INDEX IF NOT EXISTS idx_playback_tickets_file
		ON playback_tickets(file_id, state)`,
	`CREATE TABLE IF NOT EXISTS playback_media_sessions (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		capability_hash TEXT NOT NULL,
		ticket_id TEXT NOT NULL REFERENCES playback_tickets(ticket_id) ON DELETE CASCADE,
		user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		device_id INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
		file_id INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
		purpose TEXT NOT NULL DEFAULT 'playback' CHECK(purpose = 'playback'),
		transport TEXT NOT NULL CHECK(transport IN ('server-proxy','server-hls')),
		state TEXT NOT NULL DEFAULT 'active' CHECK(state IN ('active','expired','revoked')),
		mode TEXT NOT NULL,
		audio_ordinal INTEGER NOT NULL CHECK(audio_ordinal >= -1),
		video_index INTEGER NOT NULL DEFAULT -1 CHECK(video_index >= -1),
		start_seconds REAL NOT NULL CHECK(start_seconds >= 0),
		quality TEXT,
		transcode_height INTEGER CHECK(
			transcode_height IS NULL OR (transcode_height > 0 AND transcode_height <= 16384)
		),
		version INTEGER NOT NULL DEFAULT 1 CHECK(version > 0),
		created_at TIMESTAMP NOT NULL,
		last_used_at TIMESTAMP,
		idle_expires_at TIMESTAMP NOT NULL,
		absolute_expires_at TIMESTAMP NOT NULL CHECK(idle_expires_at <= absolute_expires_at),
		updated_at TIMESTAMP NOT NULL,
		revoked_at TIMESTAMP
	)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_playback_media_sessions_capability
		ON playback_media_sessions(capability_hash)`,
	`CREATE INDEX IF NOT EXISTS idx_playback_media_sessions_binding
		ON playback_media_sessions(user_id, device_id, file_id, ticket_id, transport, state)`,
	`CREATE INDEX IF NOT EXISTS idx_playback_media_sessions_expiry
		ON playback_media_sessions(state, idle_expires_at, absolute_expires_at)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_playback_media_sessions_active_transport
		ON playback_media_sessions(ticket_id, transport) WHERE state = 'active'`,
	`CREATE TRIGGER IF NOT EXISTS trg_playback_tickets_binding_immutable
	BEFORE UPDATE OF ticket_id, user_id, device_id, file_id, purpose, created_at, expires_at
	ON playback_tickets
	WHEN OLD.ticket_id IS NOT NEW.ticket_id
		OR OLD.user_id IS NOT NEW.user_id
		OR OLD.device_id IS NOT NEW.device_id
		OR OLD.file_id IS NOT NEW.file_id
		OR OLD.purpose IS NOT NEW.purpose
		OR OLD.created_at IS NOT NEW.created_at
		OR OLD.expires_at IS NOT NEW.expires_at
	BEGIN
		SELECT RAISE(ABORT, 'playback ticket identity and TTL are immutable');
	END`,
	`CREATE TRIGGER IF NOT EXISTS trg_playback_media_sessions_binding_insert
	BEFORE INSERT ON playback_media_sessions
	WHEN NOT EXISTS (
		SELECT 1 FROM playback_tickets
		WHERE ticket_id = NEW.ticket_id
			AND user_id = NEW.user_id
			AND device_id = NEW.device_id
			AND file_id = NEW.file_id
			AND purpose = NEW.purpose
			AND selected_transport = NEW.transport
	)
	BEGIN
		SELECT RAISE(ABORT, 'playback media session binding mismatch');
	END`,
	`CREATE TRIGGER IF NOT EXISTS trg_playback_media_sessions_params_immutable
	BEFORE UPDATE OF capability_hash, ticket_id, user_id, device_id, file_id,
		purpose, transport, mode, audio_ordinal, start_seconds, quality,
		transcode_height, created_at, absolute_expires_at
	ON playback_media_sessions
	WHEN OLD.capability_hash IS NOT NEW.capability_hash
		OR OLD.ticket_id IS NOT NEW.ticket_id
		OR OLD.user_id IS NOT NEW.user_id
		OR OLD.device_id IS NOT NEW.device_id
		OR OLD.file_id IS NOT NEW.file_id
		OR OLD.purpose IS NOT NEW.purpose
		OR OLD.transport IS NOT NEW.transport
		OR OLD.mode IS NOT NEW.mode
		OR OLD.audio_ordinal IS NOT NEW.audio_ordinal
		OR OLD.start_seconds IS NOT NEW.start_seconds
		OR OLD.quality IS NOT NEW.quality
		OR OLD.transcode_height IS NOT NEW.transcode_height
		OR OLD.created_at IS NOT NEW.created_at
		OR OLD.absolute_expires_at IS NOT NEW.absolute_expires_at
	BEGIN
		SELECT RAISE(ABORT, 'playback media session parameters are immutable');
	END`,
}

const playbackTicketColumns = `id, ticket_id, user_id, device_id, file_id,
	purpose, state, candidate_transports, attempted_transports,
	selected_transport, media_summary_json, client_capabilities_json,
	subtitle_ids_json, failure_code, video_index, version, created_at, expires_at,
	updated_at, consumed_at, revoked_at, claim_token, claim_expires_at`

const playbackMediaSessionBindingInsertTrigger = `CREATE TRIGGER IF NOT EXISTS
	trg_playback_media_sessions_binding_insert
	BEFORE INSERT ON playback_media_sessions
	WHEN NOT EXISTS (
		SELECT 1 FROM playback_tickets
		WHERE ticket_id = NEW.ticket_id
			AND user_id = NEW.user_id
			AND device_id = NEW.device_id
			AND file_id = NEW.file_id
			AND purpose = NEW.purpose
			AND selected_transport = NEW.transport
			AND video_index = NEW.video_index
	)
	BEGIN
		SELECT RAISE(ABORT, 'playback media session binding mismatch');
	END`

const playbackMediaSessionParamsImmutableTrigger = `CREATE TRIGGER IF NOT EXISTS
	trg_playback_media_sessions_params_immutable
	BEFORE UPDATE OF capability_hash, ticket_id, user_id, device_id, file_id,
		purpose, transport, mode, audio_ordinal, video_index, start_seconds,
		quality, transcode_height, created_at, absolute_expires_at
	ON playback_media_sessions
	WHEN OLD.capability_hash IS NOT NEW.capability_hash
		OR OLD.ticket_id IS NOT NEW.ticket_id
		OR OLD.user_id IS NOT NEW.user_id
		OR OLD.device_id IS NOT NEW.device_id
		OR OLD.file_id IS NOT NEW.file_id
		OR OLD.purpose IS NOT NEW.purpose
		OR OLD.transport IS NOT NEW.transport
		OR OLD.mode IS NOT NEW.mode
		OR OLD.audio_ordinal IS NOT NEW.audio_ordinal
		OR OLD.video_index IS NOT NEW.video_index
		OR OLD.start_seconds IS NOT NEW.start_seconds
		OR OLD.quality IS NOT NEW.quality
		OR OLD.transcode_height IS NOT NEW.transcode_height
		OR OLD.created_at IS NOT NEW.created_at
		OR OLD.absolute_expires_at IS NOT NEW.absolute_expires_at
	BEGIN
		SELECT RAISE(ABORT, 'playback media session parameters are immutable');
	END`

const playbackTicketVideoIndexImmutableTrigger = `CREATE TRIGGER IF NOT EXISTS
	trg_playback_tickets_video_index_immutable
	BEFORE UPDATE OF video_index ON playback_tickets
	WHEN OLD.state <> 'probing' AND OLD.video_index IS NOT NEW.video_index
	BEGIN
		SELECT RAISE(ABORT, 'playback video index is immutable after probing');
	END`

// MigratePlaybackVideoIndexes adds the internal selected-video snapshot to
// legacy tickets and media sessions. Existing rows receive the unknown sentinel
// and must not be used for HLS playback until a new ticket is created.
func (s *Store) MigratePlaybackVideoIndexes() error {
	if err := s.addColumn("playback_tickets", "video_index",
		`ALTER TABLE playback_tickets
			ADD COLUMN video_index INTEGER NOT NULL DEFAULT -1 CHECK(video_index >= -1)`); err != nil {
		return err
	}
	if err := s.addColumn("playback_media_sessions", "video_index",
		`ALTER TABLE playback_media_sessions
			ADD COLUMN video_index INTEGER NOT NULL DEFAULT -1 CHECK(video_index >= -1)`); err != nil {
		return err
	}
	for _, stmt := range []string{
		`DROP TRIGGER IF EXISTS trg_playback_media_sessions_binding_insert`,
		playbackMediaSessionBindingInsertTrigger,
		`DROP TRIGGER IF EXISTS trg_playback_media_sessions_params_immutable`,
		playbackMediaSessionParamsImmutableTrigger,
		playbackTicketVideoIndexImmutableTrigger,
	} {
		if _, err := s.DB.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

// MigratePlaybackClientCapabilities upgrades playback_tickets created before
// immutable client capability persistence was added.
func (s *Store) MigratePlaybackClientCapabilities() error {
	if err := s.MigratePlaybackVideoIndexes(); err != nil {
		return err
	}
	return s.addColumn("playback_tickets", "client_capabilities_json",
		`ALTER TABLE playback_tickets
			ADD COLUMN client_capabilities_json TEXT NOT NULL DEFAULT '{}'`)
}

// MigratePlaybackSubtitleGrants adds the immutable subtitle-id snapshot to
// tickets created before S3.5. The trigger permits the initial probe result to
// populate the empty snapshot but rejects every later replacement.
func (s *Store) MigratePlaybackSubtitleGrants() error {
	if err := s.addColumn("playback_tickets", "subtitle_ids_json",
		`ALTER TABLE playback_tickets
			ADD COLUMN subtitle_ids_json TEXT NOT NULL DEFAULT '[]'`); err != nil {
		return err
	}
	_, err := s.DB.Exec(`CREATE TRIGGER IF NOT EXISTS trg_playback_tickets_subtitle_grants_immutable
	BEFORE UPDATE OF subtitle_ids_json ON playback_tickets
	WHEN OLD.subtitle_ids_json <> '[]'
		AND OLD.subtitle_ids_json IS NOT NEW.subtitle_ids_json
	BEGIN
		SELECT RAISE(ABORT, 'playback subtitle grants are immutable after probing');
	END`)
	return err
}

// MigratePlaybackTransportClaims adds the internal issuance lease used to
// serialize refresh attempts. Legacy rows have no lease and remain resumable.
func (s *Store) MigratePlaybackTransportClaims() error {
	if err := s.addColumn("playback_tickets", "claim_token",
		`ALTER TABLE playback_tickets
			ADD COLUMN claim_token TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	return s.addColumn("playback_tickets", "claim_expires_at",
		`ALTER TABLE playback_tickets ADD COLUMN claim_expires_at TIMESTAMP`)
}

// MigratePlaybackDirectDASHTransports rebuilds only tables whose legacy
// transport CHECK excludes direct-dash. SQLite cannot alter a CHECK in place,
// so the rebuild copies every column, replays the table's indexes and triggers,
// and verifies the copied foreign keys before committing. The operation is
// skipped for new databases and databases already migrated.
func (s *Store) MigratePlaybackDirectDASHTransports() (err error) {
	ctx := context.Background()
	conn, err := s.DB.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	var foreignKeysEnabled int
	if err := conn.QueryRowContext(ctx, `PRAGMA foreign_keys`).
		Scan(&foreignKeysEnabled); err != nil {
		return err
	}
	if foreignKeysEnabled != 0 {
		if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
			return err
		}
		defer func() {
			restoreCtx, cancel := context.WithTimeout(
				context.Background(), playbackSQLiteBusyTimeoutCleanup)
			defer cancel()
			if _, restoreErr := conn.ExecContext(
				restoreCtx, `PRAGMA foreign_keys=ON`); restoreErr != nil {
				err = errors.Join(err, fmt.Errorf(
					"restore playback migration foreign keys: %w", restoreErr))
			}
		}()
	}

	// Child-table triggers reference playback_tickets. Legacy rename semantics
	// avoid validating them while the old parent is briefly absent; the exact
	// table name and all captured indexes/triggers are restored before commit.
	var legacyAlterTable int
	if err := conn.QueryRowContext(ctx, `PRAGMA legacy_alter_table`).
		Scan(&legacyAlterTable); err != nil {
		return err
	}
	if legacyAlterTable == 0 {
		if _, err := conn.ExecContext(ctx, `PRAGMA legacy_alter_table=ON`); err != nil {
			return err
		}
		defer func() {
			if _, restoreErr := conn.ExecContext(
				context.Background(), `PRAGMA legacy_alter_table=OFF`); restoreErr != nil {
				err = errors.Join(err, fmt.Errorf(
					"restore playback migration ALTER TABLE behavior: %w", restoreErr))
			}
		}()
	}

	type migrationSpec struct {
		table      string
		definition string
		columns    string
	}
	var migrations []migrationSpec
	for _, migration := range []migrationSpec{
		{
			table:      playbackTicketsTable,
			definition: playbackTicketsTableDefinition,
			columns:    playbackTicketColumns,
		},
		{
			table:      "playback_subtitle_sessions",
			definition: playbackSubtitleSessionsTableDefinition,
			columns:    playbackSubtitleSessionMigrationColumns,
		},
	} {
		needsMigration, err := playbackTransportCheckNeedsDirectDASH(
			ctx, conn, migration.table)
		if err != nil {
			return err
		}
		if !needsMigration {
			continue
		}
		migrations = append(migrations, migration)
	}
	if len(migrations) == 0 {
		return nil
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := checkPlaybackTicketIncomingForeignKeys(ctx, tx); err != nil {
		return err
	}
	for _, migration := range migrations {
		if err := rebuildPlaybackTransportCheckTable(
			ctx, tx, migration.table, migration.definition, migration.columns); err != nil {
			return err
		}
	}
	if err := checkPlaybackTicketIncomingForeignKeys(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

func playbackTransportCheckNeedsDirectDASH(
	ctx context.Context,
	conn *sql.Conn,
	table string,
) (bool, error) {
	var definition string
	if err := conn.QueryRowContext(ctx, `SELECT sql FROM sqlite_master
		WHERE type = 'table' AND name = ?`, table).Scan(&definition); err != nil {
		return false, fmt.Errorf("inspect %s transport check: %w", table, err)
	}
	normalized := strings.ToLower(definition)
	if strings.Contains(normalized, "direct-dash") {
		return false, nil
	}
	return strings.Contains(normalized, "server-hls") ||
		strings.Contains(normalized, "server-proxy"), nil
}

type playbackSchemaObject struct {
	kind      string
	name      string
	statement string
}

func rebuildPlaybackTransportCheckTable(
	ctx context.Context,
	tx *sql.Tx,
	table, definition, columns string,
) error {
	rows, err := tx.QueryContext(ctx, `SELECT type, name, sql
		FROM sqlite_master
		WHERE tbl_name = ? AND type IN ('index', 'trigger') AND sql IS NOT NULL
		ORDER BY CASE type WHEN 'index' THEN 0 ELSE 1 END, name`, table)
	if err != nil {
		return err
	}
	objects := make([]playbackSchemaObject, 0)
	for rows.Next() {
		var object playbackSchemaObject
		if err := rows.Scan(&object.kind, &object.name, &object.statement); err != nil {
			rows.Close()
			return err
		}
		objects = append(objects, object)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}

	var oldSequence sql.NullInt64
	err = tx.QueryRowContext(ctx,
		`SELECT seq FROM sqlite_sequence WHERE name = ?`, table).
		Scan(&oldSequence)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read %s sequence: %w", table, err)
	}

	var before int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM `+table).Scan(&before); err != nil {
		return err
	}

	temporary := table + "_direct_dash_new"
	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS `+temporary); err != nil {
		return err
	}
	createTable := strings.Replace(
		definition,
		"CREATE TABLE IF NOT EXISTS "+table,
		"CREATE TABLE "+temporary,
		1,
	)
	if createTable == definition {
		return fmt.Errorf("build %s migration table definition", table)
	}
	if _, err := tx.ExecContext(ctx, createTable); err != nil {
		return fmt.Errorf("create %s migration table: %w", table, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO `+temporary+` (`+columns+`)
		SELECT `+columns+` FROM `+table); err != nil {
		return fmt.Errorf("copy %s migration data: %w", table, err)
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE `+table); err != nil {
		return fmt.Errorf("drop legacy %s table: %w", table, err)
	}
	if _, err := tx.ExecContext(ctx,
		`ALTER TABLE `+temporary+` RENAME TO `+table); err != nil {
		return fmt.Errorf("rename migrated %s table: %w", table, err)
	}

	for _, object := range objects {
		if _, err := tx.ExecContext(ctx, object.statement); err != nil {
			return fmt.Errorf("restore %s %s: %w", object.kind, object.name, err)
		}
	}

	var maxID sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT MAX(id) FROM `+table).Scan(&maxID); err != nil {
		return fmt.Errorf("read migrated %s id high-water mark: %w", table, err)
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
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM sqlite_sequence WHERE name = ?`, table); err != nil {
			return fmt.Errorf("clear migrated %s sequence: %w", table, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO sqlite_sequence(name, seq) VALUES (?, ?)`,
			table, highWater); err != nil {
			return fmt.Errorf("restore migrated %s sequence: %w", table, err)
		}
	}

	var after int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM `+table).Scan(&after); err != nil {
		return err
	}
	if after != before {
		return fmt.Errorf("migrated %s row count = %d, want %d", table, after, before)
	}

	violations, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check(`+table+`)`)
	if err != nil {
		return fmt.Errorf("check migrated %s foreign keys: %w", table, err)
	}
	defer violations.Close()
	if violations.Next() {
		var (
			childTable string
			rowID      sql.NullInt64
			parent     string
			fkID       int
		)
		if err := violations.Scan(&childTable, &rowID, &parent, &fkID); err != nil {
			return err
		}
		return fmt.Errorf(
			"migrated %s foreign key violation: table=%s row=%v parent=%s fk=%d",
			table, childTable, rowID, parent, fkID)
	}
	if err := violations.Close(); err != nil {
		return err
	}
	if err := violations.Err(); err != nil {
		return err
	}

	var migratedDefinition string
	if err := tx.QueryRowContext(ctx, `SELECT sql FROM sqlite_master
		WHERE type = 'table' AND name = ?`, table).Scan(&migratedDefinition); err != nil {
		return err
	}
	if !strings.Contains(strings.ToLower(migratedDefinition), "direct-dash") {
		return fmt.Errorf("migrated %s table still rejects direct-dash", table)
	}
	return nil
}

func checkPlaybackTicketIncomingForeignKeys(ctx context.Context, tx *sql.Tx) error {
	for _, child := range []string{
		playbackMediaSessionsTable,
		"playback_subtitle_sessions",
	} {
		var orphan int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM `+child+` c
			LEFT JOIN playback_tickets t ON t.ticket_id = c.ticket_id
			WHERE c.ticket_id IS NOT NULL AND t.ticket_id IS NULL
			LIMIT 1
		)`).Scan(&orphan); err != nil {
			return fmt.Errorf("check %s ticket foreign keys: %w", child, err)
		}
		if orphan != 0 {
			return fmt.Errorf(
				"migrated playback tickets left orphan rows in %s", child)
		}
	}
	return nil
}

const playbackMediaSessionColumns = `id, ticket_id, user_id, device_id, file_id,
	purpose, transport, state, mode, audio_ordinal, video_index, start_seconds, quality,
	transcode_height, version, created_at, last_used_at, idle_expires_at,
	absolute_expires_at, updated_at, revoked_at`

// CreatePlaybackTicket inserts a probing ticket with a five-minute absolute
// expiry. It verifies that the device belongs to the user and the file exists.
func (s *Store) CreatePlaybackTicket(req models.PlaybackTicketRequest, now time.Time) (*models.PlaybackTicket, error) {
	return s.CreatePlaybackTicketContext(context.Background(), req, now)
}

// CreatePlaybackTicketContext is CreatePlaybackTicket with request cancellation
// propagated through the shared playback writer transaction.
func (s *Store) CreatePlaybackTicketContext(
	ctx context.Context,
	req models.PlaybackTicketRequest,
	now time.Time,
) (*models.PlaybackTicket, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	now = playbackTime(now)
	if req.UserID <= 0 || req.DeviceID <= 0 || req.FileID <= 0 {
		return nil, ErrInvalidPlaybackTicket
	}
	purpose := strings.TrimSpace(req.Purpose)
	if purpose == "" {
		purpose = models.PlaybackPurposePlayback
	}
	if purpose != models.PlaybackPurposePlayback {
		return nil, ErrInvalidPlaybackTicket
	}
	candidates, err := normalizePlaybackTransports(req.CandidateTransports, true)
	if err != nil {
		return nil, err
	}
	summary, err := normalizePlaybackMediaSummary(req.MediaSummary)
	if err != nil {
		return nil, err
	}
	capabilities, err := normalizePlaybackClientCapabilities(req.ClientCapabilities)
	if err != nil {
		return nil, err
	}
	candidatesJSON, err := json.Marshal(candidates)
	if err != nil {
		return nil, err
	}
	ticketID, err := newPlaybackPublicID("pt_", 24)
	if err != nil {
		return nil, err
	}

	var created *models.PlaybackTicket
	err = s.withPlaybackWriteTx(
		ctx,
		func(ctx context.Context, tx *sql.Tx) error {
			// Validate the binding only after this transaction owns SQLite's
			// writer reservation. Reading first would create a snapshot that
			// cannot be upgraded when another connection commits before the
			// insert below.
			_, err := tx.ExecContext(ctx, `UPDATE devices SET last_seen_at = last_seen_at
				WHERE id = ? AND user_id = ?`, req.DeviceID, req.UserID)
			return err
		},
		func(ctx context.Context, tx *sql.Tx) (bool, error) {
			var exists int
			if err := tx.QueryRowContext(ctx, `SELECT 1 FROM devices
				WHERE id = ? AND user_id = ? AND revoked_at IS NULL`,
				req.DeviceID, req.UserID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
				return false, ErrInvalidPlaybackTicket
			} else if err != nil {
				return false, err
			}
			if err := tx.QueryRowContext(ctx, `SELECT 1 FROM files WHERE id = ?`,
				req.FileID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
				return false, ErrInvalidPlaybackTicket
			} else if err != nil {
				return false, err
			}
			if err := requirePlaybackTicketsAllowedTxContext(ctx, tx, req.DeviceID); err != nil {
				return false, err
			}

			expiresAt := now.Add(models.PlaybackTicketTTL)
			res, err := tx.ExecContext(ctx, `INSERT INTO playback_tickets
		(ticket_id, user_id, device_id, file_id, purpose, state,
		 candidate_transports, attempted_transports, selected_transport,
		 media_summary_json, client_capabilities_json, subtitle_ids_json,
		 failure_code, version, created_at, expires_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				ticketID, req.UserID, req.DeviceID, req.FileID, purpose,
				models.PlaybackTicketProbing, string(candidatesJSON), `[]`, "",
				string(summary), string(capabilities), `[]`, "", 1, now, expiresAt, now)
			if err != nil {
				return false, err
			}
			id, err := res.LastInsertId()
			if err != nil {
				return false, err
			}
			created = &models.PlaybackTicket{
				ID: id, TicketID: ticketID, UserID: req.UserID, DeviceID: req.DeviceID,
				FileID: req.FileID, Purpose: purpose, State: models.PlaybackTicketProbing,
				CandidateTransports: candidates, AttemptedTransports: []string{},
				MediaSummary: summary, ClientCapabilities: capabilities, SubtitleIDs: []int64{},
				VideoIndex: models.PlaybackVideoIndexUnknown,
				Version:    1, CreatedAt: now, ExpiresAt: expiresAt,
				UpdatedAt: now,
			}
			return true, nil
		},
	)
	if err != nil {
		return nil, err
	}
	return created, nil
}

// PlaybackTicketByID reads only an exact user/device/file/ticket binding. It
// marks an overdue ticket expired but does not consume or extend it.
func (s *Store) PlaybackTicketByID(binding models.PlaybackTicketBinding, now time.Time) (*models.PlaybackTicket, error) {
	return s.PlaybackTicketByIDContext(context.Background(), binding, now)
}

// PlaybackTicketByIDContext is PlaybackTicketByID with cancellation propagated
// through a lazy expiry write, when one is required.
func (s *Store) PlaybackTicketByIDContext(
	ctx context.Context,
	binding models.PlaybackTicketBinding,
	now time.Time,
) (*models.PlaybackTicket, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	now = playbackTime(now)
	if err := validatePlaybackTicketBinding(binding); err != nil {
		return nil, err
	}
	ticket, err := scanPlaybackTicket(s.DB.QueryRowContext(ctx, `SELECT `+playbackTicketColumns+`
		FROM playback_tickets
		WHERE ticket_id = ? AND user_id = ? AND device_id = ? AND file_id = ?`,
		binding.TicketID, binding.UserID, binding.DeviceID, binding.FileID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPlaybackTicketNotFound
	}
	if err != nil {
		return nil, err
	}
	if !playbackTicketExpirable(ticket.State) || now.Before(ticket.ExpiresAt) {
		return ticket, nil
	}

	updated := false
	err = s.withPlaybackWriteTx(
		ctx,
		func(ctx context.Context, tx *sql.Tx) error {
			return lockPlaybackTicketTxContext(ctx, tx, binding)
		},
		func(ctx context.Context, tx *sql.Tx) (bool, error) {
			res, err := tx.ExecContext(ctx, `UPDATE playback_tickets
				SET state = ?, updated_at = ?, version = version + 1
				WHERE id = ? AND version = ? AND state IN ('probing','ready','consuming')
					AND expires_at <= ? AND revoked_at IS NULL`,
				models.PlaybackTicketExpired, now, ticket.ID, ticket.Version, now)
			if err != nil {
				return false, err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return false, err
			}
			updated = n == 1
			return updated, nil
		},
	)
	if err != nil {
		return nil, err
	}
	if !updated {
		reloaded, err := scanPlaybackTicket(s.DB.QueryRowContext(ctx, `SELECT `+playbackTicketColumns+`
			FROM playback_tickets
			WHERE ticket_id = ? AND user_id = ? AND device_id = ? AND file_id = ?`,
			binding.TicketID, binding.UserID, binding.DeviceID, binding.FileID))
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrPlaybackTicketNotFound
		}
		return reloaded, err
	}
	ticket.State = models.PlaybackTicketExpired
	ticket.Version++
	ticket.UpdatedAt = now
	return ticket, nil
}

// PlaybackTicketByPublicID resolves the route's ticket id under the live
// user/device binding before the caller has a file id. The returned ticket's
// file id is authoritative for every follow-up binding.
func (s *Store) PlaybackTicketByPublicID(
	userID, deviceID int64,
	ticketID string,
	now time.Time,
) (*models.PlaybackTicket, error) {
	return s.PlaybackTicketByPublicIDContext(
		context.Background(), userID, deviceID, ticketID, now)
}

// PlaybackTicketByPublicIDContext is PlaybackTicketByPublicID with cancellation
// propagated through a lazy expiry write, when one is required.
func (s *Store) PlaybackTicketByPublicIDContext(
	ctx context.Context,
	userID, deviceID int64,
	ticketID string,
	now time.Time,
) (*models.PlaybackTicket, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	now = playbackTime(now)
	ticketID = strings.TrimSpace(ticketID)
	if userID <= 0 || deviceID <= 0 || ticketID == "" {
		return nil, ErrInvalidPlaybackTicket
	}
	ticket, err := scanPlaybackTicket(s.DB.QueryRowContext(ctx, `SELECT `+playbackTicketColumns+`
		FROM playback_tickets
		WHERE ticket_id = ? AND user_id = ? AND device_id = ?`,
		ticketID, userID, deviceID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPlaybackTicketNotFound
	}
	if err != nil {
		return nil, err
	}
	if !playbackTicketExpirable(ticket.State) || now.Before(ticket.ExpiresAt) {
		return ticket, nil
	}
	binding := models.PlaybackTicketBinding{
		UserID: userID, DeviceID: deviceID, FileID: ticket.FileID, TicketID: ticketID,
	}
	updated := false
	err = s.withPlaybackWriteTx(
		ctx,
		func(ctx context.Context, tx *sql.Tx) error {
			return lockPlaybackTicketTxContext(ctx, tx, binding)
		},
		func(ctx context.Context, tx *sql.Tx) (bool, error) {
			res, err := tx.ExecContext(ctx, `UPDATE playback_tickets
				SET state = ?, updated_at = ?, version = version + 1
				WHERE id = ? AND version = ? AND state IN ('probing','ready','consuming')
					AND expires_at <= ? AND revoked_at IS NULL`,
				models.PlaybackTicketExpired, now, ticket.ID, ticket.Version, now)
			if err != nil {
				return false, err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return false, err
			}
			updated = n == 1
			return updated, nil
		},
	)
	if err != nil {
		return nil, err
	}
	if !updated {
		reloaded, err := scanPlaybackTicket(s.DB.QueryRowContext(ctx, `SELECT `+playbackTicketColumns+`
			FROM playback_tickets
			WHERE ticket_id = ? AND user_id = ? AND device_id = ?`,
			ticketID, userID, deviceID))
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrPlaybackTicketNotFound
		}
		return reloaded, err
	}
	ticket.State = models.PlaybackTicketExpired
	ticket.Version++
	ticket.UpdatedAt = now
	return ticket, nil
}

// FinishPlaybackTicketProbe performs a CAS transition from probing to ready,
// unsupported, or failed. Callers should probe outside the transaction.
func (s *Store) FinishPlaybackTicketProbe(
	binding models.PlaybackTicketBinding,
	expectedVersion int64,
	result models.PlaybackTicketProbeResult,
	now time.Time,
) (*models.PlaybackTicket, error) {
	return s.FinishPlaybackTicketProbeContext(
		context.Background(), binding, expectedVersion, result, now)
}

// FinishPlaybackTicketProbeContext applies the probe result with cancellation
// covering transaction acquisition, reads, and the final CAS update.
func (s *Store) FinishPlaybackTicketProbeContext(
	ctx context.Context,
	binding models.PlaybackTicketBinding,
	expectedVersion int64,
	result models.PlaybackTicketProbeResult,
	now time.Time,
) (*models.PlaybackTicket, error) {
	return s.finishPlaybackTicketProbeContext(
		ctx, binding, expectedVersion, result, nil, now)
}

// FinishPlaybackTicketProbeWithMetadataContext commits shared probe/subtitle
// cache rows and the final ticket CAS in one playback write transaction. The
// ticket is checked first, so a revocation that wins the writer lock leaves the
// metadata untouched.
func (s *Store) FinishPlaybackTicketProbeWithMetadataContext(
	ctx context.Context,
	binding models.PlaybackTicketBinding,
	expectedVersion int64,
	result models.PlaybackTicketProbeResult,
	metadata *PlaybackTicketProbeMetadata,
	now time.Time,
) (*models.PlaybackTicket, error) {
	if metadata == nil {
		return nil, ErrInvalidPlaybackTicket
	}
	if result.State == models.PlaybackTicketFailed {
		return nil, ErrInvalidPlaybackTicket
	}
	return s.finishPlaybackTicketProbeContext(
		ctx, binding, expectedVersion, result, metadata, now)
}

func (s *Store) finishPlaybackTicketProbeContext(
	ctx context.Context,
	binding models.PlaybackTicketBinding,
	expectedVersion int64,
	result models.PlaybackTicketProbeResult,
	metadata *PlaybackTicketProbeMetadata,
	now time.Time,
) (*models.PlaybackTicket, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	now = playbackTime(now)
	started := time.Now()
	if err := validatePlaybackTicketBinding(binding); err != nil {
		return nil, err
	}
	if expectedVersion <= 0 {
		return nil, ErrPlaybackTicketConflict
	}
	switch result.State {
	case models.PlaybackTicketReady, models.PlaybackTicketUnsupported, models.PlaybackTicketFailed:
	default:
		return nil, ErrInvalidPlaybackTicket
	}
	candidates, err := normalizePlaybackTransports(result.CandidateTransports, true)
	if err != nil {
		return nil, err
	}
	if result.State == models.PlaybackTicketReady && len(candidates) == 0 {
		return nil, ErrInvalidPlaybackTicket
	}
	videoIndex := models.PlaybackVideoIndexUnknown
	if result.VideoIndex != nil {
		videoIndex = *result.VideoIndex
		if videoIndex < 0 {
			return nil, ErrInvalidPlaybackTicket
		}
	}
	if result.State == models.PlaybackTicketReady && result.VideoIndex == nil {
		return nil, ErrInvalidPlaybackTicket
	}
	failureCode, err := normalizePlaybackCode(result.FailureCode)
	if err != nil {
		return nil, err
	}
	if (result.State == models.PlaybackTicketUnsupported || result.State == models.PlaybackTicketFailed) &&
		failureCode == "" {
		return nil, ErrInvalidPlaybackTicket
	}
	summary, err := normalizePlaybackMediaSummary(result.MediaSummary)
	if err != nil {
		return nil, err
	}
	subtitleIDs, err := normalizePlaybackSubtitleIDs(result.SubtitleIDs)
	if err != nil {
		return nil, err
	}
	var normalizedMetadata *PlaybackTicketProbeMetadata
	if metadata != nil {
		if len(subtitleIDs) != 0 {
			return nil, ErrInvalidPlaybackTicket
		}
		normalizedMetadata, err = normalizePlaybackTicketProbeMetadata(
			binding.FileID, metadata)
		if err != nil {
			return nil, err
		}
	}
	candidatesJSON, err := json.Marshal(candidates)
	if err != nil {
		return nil, err
	}

	var finished *models.PlaybackTicket
	err = s.withPlaybackWriteTx(
		ctx,
		func(ctx context.Context, tx *sql.Tx) error {
			return lockPlaybackTicketTxContext(ctx, tx, binding)
		},
		func(ctx context.Context, tx *sql.Tx) (bool, error) {
			effectiveNow := playbackTime(now.Add(time.Since(started)))
			ticket, err := loadPlaybackTicketTxContext(ctx, tx, binding)
			if err != nil {
				return false, err
			}
			if err := requirePlaybackTicketsAllowedTxContext(ctx, tx, ticket.DeviceID); err != nil {
				return false, err
			}
			switch ticket.State {
			case models.PlaybackTicketExpired:
				return false, ErrPlaybackTicketExpired
			case models.PlaybackTicketRevoked:
				return false, ErrPlaybackTicketRevoked
			case models.PlaybackTicketProbing:
			default:
				return false, ErrPlaybackTicketConflict
			}
			if ticket.Version != expectedVersion {
				return false, ErrPlaybackTicketConflict
			}
			if !effectiveNow.Before(ticket.ExpiresAt) {
				expired, err := expirePlaybackTicketTxContext(
					ctx, tx, ticket, effectiveNow)
				if err != nil {
					return false, err
				}
				if !expired {
					return false, ErrPlaybackTicketConflict
				}
				return true, ErrPlaybackTicketExpired
			}
			if normalizedMetadata != nil {
				subtitleIDs, err = applyPlaybackTicketProbeMetadataTxContext(
					ctx, tx, ticket.FileID, normalizedMetadata)
				if err != nil {
					return false, err
				}
			}
			if err := validatePlaybackSubtitleGrantsTxContext(
				ctx, tx, ticket.FileID, subtitleIDs); err != nil {
				return false, err
			}
			subtitleIDsJSON, err := json.Marshal(subtitleIDs)
			if err != nil {
				return false, err
			}

			res, err := tx.ExecContext(ctx, `UPDATE playback_tickets
				SET state = ?, candidate_transports = ?, attempted_transports = ?,
					selected_transport = '', media_summary_json = ?, failure_code = ?,
					subtitle_ids_json = ?, video_index = ?, version = version + 1,
					updated_at = ?
				WHERE id = ? AND version = ? AND state = ? AND expires_at > ?
					AND revoked_at IS NULL`,
				result.State, string(candidatesJSON), `[]`, string(summary), failureCode,
				string(subtitleIDsJSON), videoIndex, effectiveNow, ticket.ID, expectedVersion,
				models.PlaybackTicketProbing, effectiveNow)
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
			ticket.State = result.State
			ticket.CandidateTransports = candidates
			ticket.AttemptedTransports = []string{}
			ticket.SelectedTransport = ""
			ticket.MediaSummary = summary
			ticket.SubtitleIDs = subtitleIDs
			ticket.FailureCode = failureCode
			ticket.VideoIndex = videoIndex
			ticket.Version++
			ticket.UpdatedAt = effectiveNow
			finished = ticket
			return true, nil
		},
	)
	if err != nil {
		return nil, err
	}
	return finished, nil
}

type playbackProbeSubtitleKey struct {
	path        string
	streamIndex int
}

func normalizePlaybackTicketProbeMetadata(
	fileID int64,
	metadata *PlaybackTicketProbeMetadata,
) (*PlaybackTicketProbeMetadata, error) {
	if metadata == nil || fileID <= 0 {
		return nil, ErrInvalidPlaybackTicket
	}
	if len(metadata.FileProbe) > maxPlaybackProbeJSON ||
		(metadata.FileProbe != "" && !json.Valid([]byte(metadata.FileProbe))) {
		return nil, ErrInvalidPlaybackTicket
	}
	if (metadata.FileProbe != "" || len(metadata.Subtitles) != 0) &&
		metadata.FileVersion == nil {
		return nil, ErrInvalidPlaybackTicket
	}
	normalized := &PlaybackTicketProbeMetadata{
		FileProbe: metadata.FileProbe,
		Subtitles: make([]models.Subtitle, 0, len(metadata.Subtitles)),
	}
	if metadata.FileVersion != nil {
		version := *metadata.FileVersion
		normalized.FileVersion = &version
	}
	seen := make(map[playbackProbeSubtitleKey]int, len(metadata.Subtitles))
	for _, sub := range metadata.Subtitles {
		if sub.FileID != fileID || sub.StreamIdx < 0 {
			return nil, ErrInvalidPlaybackTicket
		}
		key := playbackProbeSubtitleKey{
			path: sub.Path, streamIndex: sub.StreamIdx,
		}
		if index, exists := seen[key]; exists {
			// Embedded rows are collected last and must replace a colliding
			// external/driver row, matching the legacy upsert order.
			normalized.Subtitles[index] = sub
			continue
		}
		if len(normalized.Subtitles) == models.PlaybackMaxSubtitleGrants {
			return nil, ErrInvalidPlaybackTicket
		}
		seen[key] = len(normalized.Subtitles)
		normalized.Subtitles = append(normalized.Subtitles, sub)
	}
	return normalized, nil
}

func applyPlaybackTicketProbeMetadataTxContext(
	ctx context.Context,
	tx *sql.Tx,
	fileID int64,
	metadata *PlaybackTicketProbeMetadata,
) ([]int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if metadata == nil || metadata.FileProbe == "" && len(metadata.Subtitles) == 0 {
		return []int64{}, nil
	}
	if metadata.FileVersion == nil {
		return nil, ErrInvalidPlaybackTicket
	}
	var size, mtime int64
	err := tx.QueryRowContext(ctx,
		`SELECT size, mtime FROM files WHERE id = ?`, fileID).
		Scan(&size, &mtime)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrInvalidPlaybackTicket
	}
	if err != nil {
		return nil, err
	}
	if size != metadata.FileVersion.Size ||
		mtime != metadata.FileVersion.MTime {
		return nil, ErrPlaybackTicketConflict
	}
	if metadata.FileProbe != "" {
		res, err := tx.ExecContext(ctx,
			`UPDATE files SET probe = ? WHERE id = ?`,
			metadata.FileProbe, fileID)
		if err != nil {
			return nil, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return nil, err
		}
		if n != 1 {
			return nil, ErrInvalidPlaybackTicket
		}
	}

	keys := make(map[playbackProbeSubtitleKey]struct{}, len(metadata.Subtitles))
	for _, sub := range metadata.Subtitles {
		if _, err := tx.ExecContext(ctx, `INSERT INTO subtitles
			(file_id, path, lang, title, format, embedded, stream_idx, convertible)
			VALUES (?,?,?,?,?,?,?,?)
			ON CONFLICT(file_id, path, stream_idx) DO UPDATE SET
				lang=excluded.lang, title=excluded.title, format=excluded.format,
				embedded=excluded.embedded, convertible=excluded.convertible`,
			fileID, sub.Path, sub.Lang, sub.Title, sub.Format,
			sub.Embedded, sub.StreamIdx, sub.Convertible); err != nil {
			return nil, err
		}
		keys[playbackProbeSubtitleKey{
			path: sub.Path, streamIndex: sub.StreamIdx,
		}] = struct{}{}
	}
	if len(keys) == 0 {
		return []int64{}, nil
	}

	rows, err := tx.QueryContext(ctx, `SELECT id, path, stream_idx
		FROM subtitles WHERE file_id = ? ORDER BY id`, fileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make([]int64, 0, len(keys))
	for rows.Next() {
		var (
			id          int64
			path        string
			streamIndex int
		)
		if err := rows.Scan(&id, &path, &streamIndex); err != nil {
			return nil, err
		}
		key := playbackProbeSubtitleKey{path: path, streamIndex: streamIndex}
		if _, ok := keys[key]; ok {
			ids = append(ids, id)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if len(ids) != len(keys) {
		return nil, ErrInvalidPlaybackTicket
	}
	return normalizePlaybackSubtitleIDs(ids)
}

// CheckPlaybackTicketProbeActive verifies, without mutating ticket state, that
// an asynchronous probe still belongs to the exact live probing generation.
// Callers use it as a fail-closed fence before expensive discovery or shared
// metadata updates.
func (s *Store) CheckPlaybackTicketProbeActive(
	ctx context.Context,
	binding models.PlaybackTicketBinding,
	expectedVersion int64,
	now time.Time,
) error {
	now = playbackTime(now)
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validatePlaybackTicketBinding(binding); err != nil {
		return err
	}
	if expectedVersion <= 0 {
		return ErrPlaybackTicketConflict
	}

	var (
		state     string
		version   int64
		expiresAt time.Time
		revokedAt sql.NullTime
	)
	err := s.DB.QueryRowContext(ctx, `SELECT state, version, expires_at, revoked_at
		FROM playback_tickets
		WHERE ticket_id = ? AND user_id = ? AND device_id = ? AND file_id = ?`,
		binding.TicketID, binding.UserID, binding.DeviceID, binding.FileID,
	).Scan(&state, &version, &expiresAt, &revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrPlaybackTicketNotFound
	}
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if revokedAt.Valid || state == models.PlaybackTicketRevoked {
		return ErrPlaybackTicketRevoked
	}
	if version != expectedVersion {
		return ErrPlaybackTicketConflict
	}
	if state == models.PlaybackTicketExpired || !now.Before(expiresAt) {
		return ErrPlaybackTicketExpired
	}
	if state != models.PlaybackTicketProbing {
		return ErrPlaybackTicketConflict
	}
	return nil
}

// ClaimNextPlaybackTransport atomically claims the first candidate transport
// that has not been attempted. On success the ticket moves to consuming. If
// every candidate was attempted, the ticket moves to exhausted and the updated
// ticket is returned together with ErrPlaybackTicketExhausted.
func (s *Store) ClaimNextPlaybackTransport(
	ctx context.Context,
	binding models.PlaybackTicketBinding,
	expectedVersion int64,
	now time.Time,
) (*models.PlaybackTicket, error) {
	now = playbackTime(now)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validatePlaybackTicketBinding(binding); err != nil {
		return nil, err
	}
	if expectedVersion <= 0 {
		return nil, ErrPlaybackTicketConflict
	}
	claimToken, err := newPlaybackPublicID("pc_", 24)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var claimed *models.PlaybackTicket
	err = s.withPlaybackWriteTx(
		ctx,
		func(ctx context.Context, tx *sql.Tx) error {
			return lockPlaybackTicketTxContext(ctx, tx, binding)
		},
		func(ctx context.Context, tx *sql.Tx) (bool, error) {
			ticket, err := loadPlaybackTicketTxContext(ctx, tx, binding)
			if err != nil {
				return false, err
			}
			if err := requirePlaybackTicketsAllowedTxContext(ctx, tx, ticket.DeviceID); err != nil {
				return false, err
			}
			switch ticket.State {
			case models.PlaybackTicketExpired:
				return false, ErrPlaybackTicketExpired
			case models.PlaybackTicketRevoked:
				return false, ErrPlaybackTicketRevoked
			case models.PlaybackTicketReady, models.PlaybackTicketConsuming:
			default:
				return false, ErrPlaybackTicketConflict
			}
			if !now.Before(ticket.ExpiresAt) {
				expired, err := expirePlaybackTicketTxContext(ctx, tx, ticket, now)
				if err != nil {
					return false, err
				}
				if !expired {
					return false, ErrPlaybackTicketConflict
				}
				claimed = ticket
				return true, ErrPlaybackTicketExpired
			}
			if ticket.Version != expectedVersion {
				return false, ErrPlaybackTicketConflict
			}
			if ticket.ClaimToken != "" && ticket.ClaimExpiresAt != nil &&
				now.Before(*ticket.ClaimExpiresAt) {
				return false, ErrPlaybackTicketConflict
			}
			if _, err := normalizePlaybackTransports(ticket.CandidateTransports, false); err != nil {
				return false, err
			}
			attempted, err := normalizePlaybackTransports(ticket.AttemptedTransports, true)
			if err != nil {
				return false, err
			}
			candidates := append([]string(nil), ticket.CandidateTransports...)
			attemptedSet := make(map[string]struct{}, len(attempted))
			for _, transport := range attempted {
				attemptedSet[transport] = struct{}{}
			}
			next := ""
			for _, transport := range candidates {
				if _, seen := attemptedSet[transport]; !seen {
					next = transport
					break
				}
			}
			if next == "" {
				res, err := tx.ExecContext(ctx, `UPDATE playback_tickets
					SET state = ?, claim_token = '', claim_expires_at = NULL,
						version = version + 1, updated_at = ?
					WHERE id = ? AND version = ? AND state IN (?,?)
						AND expires_at > ? AND revoked_at IS NULL`,
					models.PlaybackTicketExhausted, now, ticket.ID, expectedVersion,
					models.PlaybackTicketReady, models.PlaybackTicketConsuming, now)
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
				ticket.State = models.PlaybackTicketExhausted
				ticket.AttemptedTransports = attempted
				ticket.ClaimToken = ""
				ticket.ClaimExpiresAt = nil
				ticket.Version++
				ticket.UpdatedAt = now
				claimed = ticket
				return true, ErrPlaybackTicketExhausted
			}
			if err := requirePlaybackTransportAllowedTxContext(
				ctx, tx, ticket.DeviceID, next); err != nil {
				return false, err
			}

			attempted = append(attempted, next)
			attemptedJSON, err := json.Marshal(attempted)
			if err != nil {
				return false, err
			}
			claimExpiresAt := now.Add(models.PlaybackTransportClaimTTL)
			if claimExpiresAt.After(ticket.ExpiresAt) {
				claimExpiresAt = ticket.ExpiresAt
			}
			res, err := tx.ExecContext(ctx, `UPDATE playback_tickets
				SET state = ?, attempted_transports = ?, selected_transport = ?,
					consumed_at = COALESCE(consumed_at, ?), claim_token = ?,
					claim_expires_at = ?, version = version + 1, updated_at = ?
				WHERE id = ? AND version = ? AND state IN (?,?) AND expires_at > ?
					AND revoked_at IS NULL`,
				models.PlaybackTicketConsuming, string(attemptedJSON), next, now,
				claimToken, claimExpiresAt, now, ticket.ID, expectedVersion,
				models.PlaybackTicketReady,
				models.PlaybackTicketConsuming, now)
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
			ticket.State = models.PlaybackTicketConsuming
			ticket.AttemptedTransports = attempted
			ticket.SelectedTransport = next
			ticket.ClaimToken = claimToken
			ticket.ClaimExpiresAt = timePointer(claimExpiresAt)
			ticket.Version++
			ticket.UpdatedAt = now
			if ticket.ConsumedAt == nil {
				consumed := now
				ticket.ConsumedAt = &consumed
			}
			claimed = ticket
			return true, nil
		},
	)
	if err != nil {
		if claimed != nil &&
			(errors.Is(err, ErrPlaybackTicketExpired) ||
				errors.Is(err, ErrPlaybackTicketExhausted)) {
			return claimed, err
		}
		return nil, err
	}
	return claimed, nil
}

// ValidatePlaybackTransportIssuance rechecks and consumes the exact issuance
// lease immediately before a one-time content description is returned. Any
// revocation, expiry, concurrent claim, wrong token, or transport-specific
// device override fails closed.
func (s *Store) ValidatePlaybackTransportIssuance(
	ctx context.Context,
	binding models.PlaybackTicketBinding,
	expectedVersion int64,
	transport string,
	claimToken string,
	now time.Time,
) error {
	now = playbackTime(now)
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validatePlaybackTicketBinding(binding); err != nil {
		return err
	}
	if expectedVersion <= 0 || !validPlaybackTransport(transport) ||
		strings.TrimSpace(claimToken) == "" {
		return ErrInvalidPlaybackTransport
	}

	return s.withPlaybackWriteTx(
		ctx,
		func(ctx context.Context, tx *sql.Tx) error {
			return lockPlaybackTicketTxContext(ctx, tx, binding)
		},
		func(ctx context.Context, tx *sql.Tx) (bool, error) {
			ticket, err := loadPlaybackTicketTxContext(ctx, tx, binding)
			if err != nil {
				return false, err
			}
			switch ticket.State {
			case models.PlaybackTicketRevoked:
				return false, ErrPlaybackTicketRevoked
			case models.PlaybackTicketExpired:
				return false, ErrPlaybackTicketExpired
			case models.PlaybackTicketConsuming:
			default:
				return false, ErrPlaybackTicketConflict
			}
			if !now.Before(ticket.ExpiresAt) {
				expired, err := expirePlaybackTicketTxContext(ctx, tx, ticket, now)
				if err != nil {
					return false, err
				}
				if !expired {
					return false, ErrPlaybackTicketConflict
				}
				return true, ErrPlaybackTicketExpired
			}
			if ticket.Version != expectedVersion || ticket.SelectedTransport != transport {
				return false, ErrPlaybackTicketConflict
			}
			if ticket.ClaimToken != claimToken || ticket.ClaimExpiresAt == nil ||
				!now.Before(*ticket.ClaimExpiresAt) {
				return false, ErrPlaybackTicketConflict
			}
			if err := requirePlaybackTransportAllowedTxContext(
				ctx, tx, ticket.DeviceID, transport); err != nil {
				return false, err
			}
			res, err := tx.ExecContext(ctx, `UPDATE playback_tickets
				SET claim_token = '', claim_expires_at = NULL, version = version + 1,
					updated_at = ?
				WHERE id = ? AND version = ? AND state = ? AND selected_transport = ?
					AND claim_token = ? AND claim_expires_at > ? AND expires_at > ?
					AND revoked_at IS NULL`,
				now, ticket.ID, expectedVersion, models.PlaybackTicketConsuming,
				transport, claimToken, now, now)
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
}

// ReleasePlaybackTransportClaim clears one exact unexpired issuance lease so
// the same request can continue its transport fallback. It does not remove the
// attempted transport from the ticket history.
func (s *Store) ReleasePlaybackTransportClaim(
	ctx context.Context,
	binding models.PlaybackTicketBinding,
	expectedVersion int64,
	transport string,
	claimToken string,
	now time.Time,
) error {
	now = playbackTime(now)
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validatePlaybackTicketBinding(binding); err != nil {
		return err
	}
	if expectedVersion <= 0 || !validPlaybackTransport(transport) ||
		strings.TrimSpace(claimToken) == "" {
		return ErrInvalidPlaybackTransport
	}
	return s.withPlaybackWriteTx(
		ctx,
		func(ctx context.Context, tx *sql.Tx) error {
			return lockPlaybackTicketTxContext(ctx, tx, binding)
		},
		func(ctx context.Context, tx *sql.Tx) (bool, error) {
			res, err := tx.ExecContext(ctx, `UPDATE playback_tickets
				SET claim_token = '', claim_expires_at = NULL, version = version + 1,
					updated_at = ?
				WHERE ticket_id = ? AND user_id = ? AND device_id = ? AND file_id = ?
					AND version = ? AND state = ? AND selected_transport = ?
					AND claim_token = ? AND claim_expires_at > ? AND expires_at > ?
					AND revoked_at IS NULL`,
				now, binding.TicketID, binding.UserID, binding.DeviceID, binding.FileID,
				expectedVersion, models.PlaybackTicketConsuming, transport, claimToken,
				now, now)
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
}

// RevokePlaybackTicket revokes one exact ticket binding and all active media
// sessions issued for it in the same transaction.
func (s *Store) RevokePlaybackTicket(
	binding models.PlaybackTicketBinding,
	expectedVersion int64,
	now time.Time,
) (*models.PlaybackTicket, error) {
	return s.RevokePlaybackTicketContext(
		context.Background(), binding, expectedVersion, now)
}

// RevokePlaybackTicketContext is RevokePlaybackTicket with cancellation and the
// shared write budget covering the ticket, media, and subtitle revocations.
func (s *Store) RevokePlaybackTicketContext(
	ctx context.Context,
	binding models.PlaybackTicketBinding,
	expectedVersion int64,
	now time.Time,
) (*models.PlaybackTicket, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	now = playbackTime(now)
	if err := validatePlaybackTicketBinding(binding); err != nil {
		return nil, err
	}
	if expectedVersion <= 0 {
		return nil, ErrPlaybackTicketConflict
	}
	var revoked *models.PlaybackTicket
	err := s.withPlaybackWriteTx(
		ctx,
		func(ctx context.Context, tx *sql.Tx) error {
			return lockPlaybackTicketTxContext(ctx, tx, binding)
		},
		func(ctx context.Context, tx *sql.Tx) (bool, error) {
			ticket, err := loadPlaybackTicketTxContext(ctx, tx, binding)
			if err != nil {
				return false, err
			}
			if ticket.State == models.PlaybackTicketRevoked {
				return false, ErrPlaybackTicketRevoked
			}
			if ticket.Version != expectedVersion {
				return false, ErrPlaybackTicketConflict
			}

			res, err := tx.ExecContext(ctx, `UPDATE playback_tickets
				SET state = ?, revoked_at = ?, version = version + 1, updated_at = ?
				WHERE id = ? AND version = ? AND state <> ?`,
				models.PlaybackTicketRevoked, now, now, ticket.ID, expectedVersion,
				models.PlaybackTicketRevoked)
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
			if _, err := tx.ExecContext(ctx, `UPDATE playback_media_sessions
				SET state = ?, revoked_at = ?, version = version + 1, updated_at = ?
				WHERE ticket_id = ? AND user_id = ? AND device_id = ? AND file_id = ?
					AND state = ?`,
				models.PlaybackMediaSessionRevoked, now, now, binding.TicketID,
				binding.UserID, binding.DeviceID, binding.FileID,
				models.PlaybackMediaSessionActive); err != nil {
				return false, err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE playback_subtitle_sessions
				SET state = ?, revoked_at = ?, version = version + 1, updated_at = ?
				WHERE ticket_id = ? AND user_id = ? AND device_id = ? AND file_id = ?
					AND state = ?`,
				models.PlaybackSubtitleSessionRevoked, now, now, binding.TicketID,
				binding.UserID, binding.DeviceID, binding.FileID,
				models.PlaybackSubtitleSessionActive); err != nil {
				return false, err
			}
			ticket.State = models.PlaybackTicketRevoked
			ticket.RevokedAt = timePointer(now)
			ticket.Version++
			ticket.UpdatedAt = now
			revoked = ticket
			return true, nil
		},
	)
	if err != nil {
		return nil, err
	}
	return revoked, nil
}

// ExpirePlaybackTickets conditionally expires tickets past their absolute TTL.
// Media sessions keep their own independent TTLs.
func (s *Store) ExpirePlaybackTickets(now time.Time) (int64, error) {
	now = playbackTime(now)
	res, err := s.DB.Exec(`UPDATE playback_tickets
		SET state = ?, version = version + 1, updated_at = ?
		WHERE state IN (?,?,?) AND expires_at <= ?`,
		models.PlaybackTicketExpired, now, models.PlaybackTicketProbing,
		models.PlaybackTicketReady, models.PlaybackTicketConsuming, now)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// FailInterruptedPlaybackProbes converges probing tickets left by a previous
// process. Probe workers are intentionally in-memory, so startup is the point
// at which every remaining probing row is known to have no worker. Overdue rows
// become expired; live rows become failed and bump their version so an old
// worker can never complete them. The operation is idempotent.
func (s *Store) FailInterruptedPlaybackProbes(now time.Time) (int64, error) {
	now = playbackTime(now)
	tx, err := s.DB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE playback_tickets
		SET state = ?, version = version + 1, updated_at = ?
		WHERE state = ? AND expires_at <= ? AND revoked_at IS NULL`,
		models.PlaybackTicketExpired, now, models.PlaybackTicketProbing, now); err != nil {
		return 0, err
	}
	res, err := tx.Exec(`UPDATE playback_tickets
		SET state = ?, failure_code = 'probe_interrupted',
			selected_transport = '', claim_token = '', claim_expires_at = NULL,
			version = version + 1, updated_at = ?
		WHERE state = ? AND expires_at > ? AND revoked_at IS NULL`,
		models.PlaybackTicketFailed, now, models.PlaybackTicketProbing, now)
	if err != nil {
		return 0, err
	}
	interrupted, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return interrupted, nil
}

// CreatePlaybackMediaSession issues a capability once and persists only its
// SHA-256 hash. The ticket must already have claimed the same transport.
func (s *Store) CreatePlaybackMediaSession(
	ctx context.Context,
	req models.PlaybackMediaSessionRequest,
	now time.Time,
) (*models.PlaybackMediaSessionGrant, error) {
	now = playbackTime(now)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	req, err := normalizePlaybackMediaSessionRequest(req)
	if err != nil {
		return nil, err
	}
	capability, err := newPlaybackCapability()
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var grant *models.PlaybackMediaSessionGrant
	commitAttempted := false
	err = s.withPlaybackWriteTxConfiguredAndCommit(
		ctx,
		playbackWriteLockBudget,
		restorePlaybackBusyTimeout,
		func(ctx context.Context, tx *sql.Tx) error {
			return lockPlaybackTicketTxContext(ctx, tx, req.Ticket)
		},
		func(ctx context.Context, tx *sql.Tx) (bool, error) {
			ticket, err := loadPlaybackTicketTxContext(ctx, tx, req.Ticket)
			if err != nil {
				return false, err
			}
			if err := requirePlaybackTransportAllowedTxContext(
				ctx, tx, ticket.DeviceID, req.Transport); err != nil {
				return false, err
			}
			switch ticket.State {
			case models.PlaybackTicketExpired:
				return false, ErrPlaybackTicketExpired
			case models.PlaybackTicketRevoked:
				return false, ErrPlaybackTicketRevoked
			case models.PlaybackTicketConsuming:
			default:
				return false, ErrPlaybackTicketConflict
			}
			if !now.Before(ticket.ExpiresAt) {
				expired, err := expirePlaybackTicketTxContext(ctx, tx, ticket, now)
				if err != nil {
					return false, err
				}
				if !expired {
					return false, ErrPlaybackTicketConflict
				}
				return true, ErrPlaybackTicketExpired
			}
			if ticket.SelectedTransport != req.Transport {
				return false, ErrInvalidPlaybackMediaSession
			}
			if ticket.VideoIndex < models.PlaybackVideoIndexUnknown {
				return false, ErrInvalidPlaybackMediaSession
			}
			if req.Transport == models.PlaybackTransportServerHLS &&
				ticket.VideoIndex == models.PlaybackVideoIndexUnknown {
				// A legacy ticket has no trustworthy video snapshot. HLS must not
				// infer one from whatever ffprobe returns for a later segment.
				return false, ErrInvalidPlaybackMediaSession
			}
			foundAttempt := false
			for _, transport := range ticket.AttemptedTransports {
				if transport == req.Transport {
					foundAttempt = true
					break
				}
			}
			if !foundAttempt {
				return false, ErrInvalidPlaybackMediaSession
			}
			var existing int
			err = tx.QueryRowContext(ctx, `SELECT 1 FROM playback_media_sessions
				WHERE ticket_id = ? AND transport = ? AND state = ?`,
				req.Ticket.TicketID, req.Transport,
				models.PlaybackMediaSessionActive).Scan(&existing)
			if err == nil {
				return false, ErrPlaybackMediaSessionConflict
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return false, err
			}

			absoluteExpiresAt := now.Add(models.PlaybackMediaSessionAbsoluteTTL)
			idleExpiresAt := now.Add(models.PlaybackMediaSessionIdleTTL)
			if idleExpiresAt.After(absoluteExpiresAt) {
				idleExpiresAt = absoluteExpiresAt
			}
			res, err := tx.ExecContext(ctx, `INSERT INTO playback_media_sessions
				(capability_hash, ticket_id, user_id, device_id, file_id, purpose,
				 transport, state, mode, audio_ordinal, video_index, start_seconds,
				 quality, transcode_height, version, created_at, idle_expires_at,
				 absolute_expires_at, updated_at)
				VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				hashPlaybackCapability(capability), req.Ticket.TicketID,
				req.Ticket.UserID, req.Ticket.DeviceID, req.Ticket.FileID,
				models.PlaybackPurposePlayback, req.Transport,
				models.PlaybackMediaSessionActive, req.Mode, req.AudioOrdinal,
				ticket.VideoIndex, req.StartSeconds, nullablePlaybackString(req.Quality),
				nullablePlaybackInt(req.TranscodeHeight), 1, now, idleExpiresAt,
				absoluteExpiresAt, now)
			if err != nil {
				return false, err
			}
			id, err := res.LastInsertId()
			if err != nil {
				return false, err
			}
			session := &models.PlaybackMediaSession{
				ID: id, TicketID: req.Ticket.TicketID, UserID: req.Ticket.UserID,
				DeviceID: req.Ticket.DeviceID, FileID: req.Ticket.FileID,
				Purpose: models.PlaybackPurposePlayback, Transport: req.Transport,
				State: models.PlaybackMediaSessionActive, Mode: req.Mode,
				AudioOrdinal: req.AudioOrdinal, VideoIndex: ticket.VideoIndex,
				StartSeconds: req.StartSeconds,
				Quality:      req.Quality, TranscodeHeight: req.TranscodeHeight, Version: 1,
				CreatedAt: now, IdleExpiresAt: idleExpiresAt,
				AbsoluteExpiresAt: absoluteExpiresAt, UpdatedAt: now,
			}
			grant = &models.PlaybackMediaSessionGrant{
				Session: session, Capability: capability,
			}
			return true, nil
		},
		func(tx *sql.Tx) error {
			commitAttempted = true
			return tx.Commit()
		},
	)
	if err != nil {
		if grant != nil && commitAttempted {
			// The insert may have committed before the context-visible error.
			// Return the grant so the caller can revoke any committed capability.
			return grant, err
		}
		return nil, err
	}
	return grant, nil
}

// PlaybackMediaSessionByCapability reads an exact ticket/user/device/file/
// transport binding. It marks an overdue session expired and returns the row
// with its state; authorization callers must use Touch before serving media.
func (s *Store) PlaybackMediaSessionByCapability(
	binding models.PlaybackMediaSessionBinding,
	capability string,
	now time.Time,
) (*models.PlaybackMediaSession, error) {
	return s.PlaybackMediaSessionByCapabilityContext(
		context.Background(), binding, capability, now)
}

// PlaybackMediaSessionByCapabilityContext is
// PlaybackMediaSessionByCapability with cancellation propagated through a lazy
// expiry write, when one is required.
func (s *Store) PlaybackMediaSessionByCapabilityContext(
	ctx context.Context,
	binding models.PlaybackMediaSessionBinding,
	capability string,
	now time.Time,
) (*models.PlaybackMediaSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	now = playbackTime(now)
	if err := validatePlaybackMediaSessionBinding(binding); err != nil {
		return nil, ErrPlaybackMediaSessionNotFound
	}
	session, err := scanPlaybackMediaSession(s.DB.QueryRowContext(ctx, `SELECT `+
		playbackMediaSessionColumns+`
		FROM playback_media_sessions
		WHERE capability_hash = ? AND ticket_id = ? AND user_id = ?
			AND device_id = ? AND file_id = ? AND transport = ?`,
		hashPlaybackCapability(capability), binding.Ticket.TicketID,
		binding.Ticket.UserID, binding.Ticket.DeviceID, binding.Ticket.FileID,
		binding.Transport))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPlaybackMediaSessionNotFound
	}
	if err != nil {
		return nil, err
	}
	if session.State != models.PlaybackMediaSessionActive ||
		(!now.Before(session.IdleExpiresAt) || !now.Before(session.AbsoluteExpiresAt)) {
		if session.State == models.PlaybackMediaSessionActive {
			updated := false
			err := s.withPlaybackWriteTx(
				ctx,
				func(ctx context.Context, tx *sql.Tx) error {
					return lockPlaybackMediaSessionTxContext(ctx, tx, capability)
				},
				func(ctx context.Context, tx *sql.Tx) (bool, error) {
					res, err := tx.ExecContext(ctx, `UPDATE playback_media_sessions
						SET state = ?, version = version + 1, updated_at = ?
						WHERE id = ? AND version = ? AND state = ?
							AND (idle_expires_at <= ? OR absolute_expires_at <= ?)`,
						models.PlaybackMediaSessionExpired, now, session.ID,
						session.Version, models.PlaybackMediaSessionActive, now, now)
					if err != nil {
						return false, err
					}
					n, err := res.RowsAffected()
					if err != nil {
						return false, err
					}
					updated = n == 1
					return updated, nil
				},
			)
			if err != nil {
				return nil, err
			}
			if updated {
				session.State = models.PlaybackMediaSessionExpired
				session.Version++
				session.UpdatedAt = now
			} else {
				reloaded, err := scanPlaybackMediaSession(s.DB.QueryRowContext(ctx, `SELECT `+
					playbackMediaSessionColumns+`
					FROM playback_media_sessions
					WHERE capability_hash = ? AND ticket_id = ? AND user_id = ?
						AND device_id = ? AND file_id = ? AND transport = ?`,
					hashPlaybackCapability(capability), binding.Ticket.TicketID,
					binding.Ticket.UserID, binding.Ticket.DeviceID,
					binding.Ticket.FileID, binding.Transport))
				if errors.Is(err, sql.ErrNoRows) {
					return nil, ErrPlaybackMediaSessionNotFound
				}
				if err != nil {
					return nil, err
				}
				session = reloaded
			}
		}
	}
	return session, nil
}

// AuthorizePlaybackMediaSession resolves a capability as a bearer value and
// re-checks the live user and device state in the same transaction that touches
// the idle deadline. The capability is never accepted as an identity on its own:
// its stored user/device binding remains authoritative.
func (s *Store) AuthorizePlaybackMediaSession(
	capability string,
	requireEmailVerification bool,
	now time.Time,
) (*PlaybackMediaSessionAccess, error) {
	return s.AuthorizePlaybackMediaSessionContext(
		context.Background(), capability, requireEmailVerification, now)
}

// AuthorizePlaybackMediaSessionContext reauthorizes and touches a media
// session with request cancellation propagated through the whole transaction.
func (s *Store) AuthorizePlaybackMediaSessionContext(
	ctx context.Context,
	capability string,
	requireEmailVerification bool,
	now time.Time,
) (*PlaybackMediaSessionAccess, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	now = playbackTime(now)
	if strings.TrimSpace(capability) == "" {
		return nil, ErrPlaybackMediaSessionNotFound
	}
	var access *PlaybackMediaSessionAccess
	if err := s.withPlaybackWriteTx(
		ctx,
		func(ctx context.Context, tx *sql.Tx) error {
			return lockPlaybackMediaSessionTxContext(ctx, tx, capability)
		},
		func(ctx context.Context, tx *sql.Tx) (bool, error) {
			session, err := scanPlaybackMediaSession(tx.QueryRowContext(ctx, `SELECT `+
				playbackMediaSessionColumns+`
				FROM playback_media_sessions WHERE capability_hash = ?`,
				hashPlaybackCapability(capability)))
			if errors.Is(err, sql.ErrNoRows) {
				return false, ErrPlaybackMediaSessionNotFound
			}
			if err != nil {
				return false, err
			}
			switch session.State {
			case models.PlaybackMediaSessionRevoked:
				return false, ErrPlaybackMediaSessionRevoked
			case models.PlaybackMediaSessionExpired:
				return false, ErrPlaybackMediaSessionExpired
			case models.PlaybackMediaSessionActive:
			default:
				return false, ErrPlaybackMediaSessionConflict
			}
			if !now.Before(session.IdleExpiresAt) || !now.Before(session.AbsoluteExpiresAt) {
				res, err := tx.ExecContext(ctx, `UPDATE playback_media_sessions
					SET state = ?, version = version + 1, updated_at = ?
					WHERE id = ? AND version = ? AND state = ?
						AND (idle_expires_at <= ? OR absolute_expires_at <= ?)`,
					models.PlaybackMediaSessionExpired, now, session.ID, session.Version,
					models.PlaybackMediaSessionActive, now, now)
				if err != nil {
					return false, err
				}
				n, err := res.RowsAffected()
				if err != nil {
					return false, err
				}
				if n != 1 {
					return false, ErrPlaybackMediaSessionConflict
				}
				return true, ErrPlaybackMediaSessionExpired
			}

			var (
				user          models.User
				emailVerified int
				device        models.Device
				deviceRevoked sql.NullTime
			)
			err = tx.QueryRowContext(ctx, `SELECT
				u.id, u.name, u.status, u.email, u.email_verified,
				d.id, d.public_id, d.user_id, d.name, d.platform, d.client_version,
				d.created_at, d.last_seen_at, d.revoked_at
				FROM users u JOIN devices d ON d.user_id = u.id
				WHERE u.id = ? AND d.id = ?`,
				session.UserID, session.DeviceID).
				Scan(&user.ID, &user.Name, &user.Status, &user.Email, &emailVerified,
					&device.ID, &device.PublicID, &device.UserID, &device.Name,
					&device.Platform, &device.ClientVersion, &device.CreatedAt,
					&device.LastSeenAt, &deviceRevoked)
			if errors.Is(err, sql.ErrNoRows) {
				return false, ErrDeviceUserUnavailable
			}
			if err != nil {
				return false, err
			}
			user.EmailVerified = emailVerified != 0
			if !deviceAuthUserAllowed(user.Status, user.Email, user.EmailVerified, requireEmailVerification) {
				return false, ErrDeviceUserUnavailable
			}
			if deviceRevoked.Valid {
				return false, ErrDeviceRevoked
			}
			overrides, err := loadDeviceFeatureOverridesContext(ctx, tx, device.ID)
			if err != nil {
				return false, err
			}
			if overrides.DisablesPlaybackTickets() ||
				overrides.DisablesPlaybackTransport(session.Transport) {
				return false, ErrDevicePlaybackDisabled
			}

			idleExpiresAt := now.Add(models.PlaybackMediaSessionIdleTTL)
			if idleExpiresAt.After(session.AbsoluteExpiresAt) {
				idleExpiresAt = session.AbsoluteExpiresAt
			}
			res, err := tx.ExecContext(ctx, `UPDATE playback_media_sessions
				SET last_used_at = ?, idle_expires_at = ?, version = version + 1,
					updated_at = ?
				WHERE id = ? AND version = ? AND state = ?
					AND idle_expires_at > ? AND absolute_expires_at > ?`,
				now, idleExpiresAt, now, session.ID, session.Version,
				models.PlaybackMediaSessionActive, now, now)
			if err != nil {
				return false, err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return false, err
			}
			if n != 1 {
				return false, ErrPlaybackMediaSessionConflict
			}
			if _, err := tx.ExecContext(ctx, `UPDATE devices SET last_seen_at = ?
				WHERE id = ? AND revoked_at IS NULL`, now, device.ID); err != nil {
				return false, err
			}
			session.LastUsedAt = timePointer(now)
			session.IdleExpiresAt = idleExpiresAt
			session.Version++
			session.UpdatedAt = now
			access = &PlaybackMediaSessionAccess{
				Session: session, User: &user, Device: &device,
			}
			return true, nil
		},
	); err != nil {
		return nil, err
	}
	return access, nil
}

// TouchPlaybackMediaSession authorizes one media request and slides the idle
// expiry without extending the absolute expiry. The write is a CAS update.
func (s *Store) TouchPlaybackMediaSession(
	binding models.PlaybackMediaSessionBinding,
	capability string,
	now time.Time,
) (*models.PlaybackMediaSession, error) {
	return s.TouchPlaybackMediaSessionContext(
		context.Background(), binding, capability, now)
}

// TouchPlaybackMediaSessionContext is TouchPlaybackMediaSession with
// cancellation covering the shared writer wait and the session CAS.
func (s *Store) TouchPlaybackMediaSessionContext(
	ctx context.Context,
	binding models.PlaybackMediaSessionBinding,
	capability string,
	now time.Time,
) (*models.PlaybackMediaSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	now = playbackTime(now)
	if err := validatePlaybackMediaSessionBinding(binding); err != nil {
		return nil, ErrPlaybackMediaSessionNotFound
	}
	var touched *models.PlaybackMediaSession
	err := s.withPlaybackWriteTx(
		ctx,
		func(ctx context.Context, tx *sql.Tx) error {
			return lockPlaybackMediaSessionTxContext(ctx, tx, capability)
		},
		func(ctx context.Context, tx *sql.Tx) (bool, error) {
			session, err := scanPlaybackMediaSession(tx.QueryRowContext(ctx, `SELECT `+
				playbackMediaSessionColumns+`
				FROM playback_media_sessions
				WHERE capability_hash = ? AND ticket_id = ? AND user_id = ?
					AND device_id = ? AND file_id = ? AND transport = ?`,
				hashPlaybackCapability(capability), binding.Ticket.TicketID,
				binding.Ticket.UserID, binding.Ticket.DeviceID, binding.Ticket.FileID,
				binding.Transport))
			if errors.Is(err, sql.ErrNoRows) {
				return false, ErrPlaybackMediaSessionNotFound
			}
			if err != nil {
				return false, err
			}
			switch session.State {
			case models.PlaybackMediaSessionRevoked:
				return false, ErrPlaybackMediaSessionRevoked
			case models.PlaybackMediaSessionExpired:
				return false, ErrPlaybackMediaSessionExpired
			case models.PlaybackMediaSessionActive:
			default:
				return false, ErrPlaybackMediaSessionConflict
			}
			if !now.Before(session.IdleExpiresAt) || !now.Before(session.AbsoluteExpiresAt) {
				res, err := tx.ExecContext(ctx, `UPDATE playback_media_sessions
					SET state = ?, version = version + 1, updated_at = ?
					WHERE id = ? AND version = ? AND state = ?
						AND (idle_expires_at <= ? OR absolute_expires_at <= ?)`,
					models.PlaybackMediaSessionExpired, now, session.ID, session.Version,
					models.PlaybackMediaSessionActive, now, now)
				if err != nil {
					return false, err
				}
				n, err := res.RowsAffected()
				if err != nil {
					return false, err
				}
				if n != 1 {
					return false, ErrPlaybackMediaSessionConflict
				}
				return true, ErrPlaybackMediaSessionExpired
			}
			overrides, err := loadDeviceFeatureOverridesContext(ctx, tx, session.DeviceID)
			if err != nil {
				return false, err
			}
			if overrides.DisablesPlaybackTickets() ||
				overrides.DisablesPlaybackTransport(session.Transport) {
				return false, ErrDevicePlaybackDisabled
			}

			idleExpiresAt := now.Add(models.PlaybackMediaSessionIdleTTL)
			if idleExpiresAt.After(session.AbsoluteExpiresAt) {
				idleExpiresAt = session.AbsoluteExpiresAt
			}
			res, err := tx.ExecContext(ctx, `UPDATE playback_media_sessions
				SET last_used_at = ?, idle_expires_at = ?, version = version + 1,
					updated_at = ?
				WHERE id = ? AND version = ? AND state = ?
					AND idle_expires_at > ? AND absolute_expires_at > ?`,
				now, idleExpiresAt, now, session.ID, session.Version,
				models.PlaybackMediaSessionActive, now, now)
			if err != nil {
				return false, err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return false, err
			}
			if n != 1 {
				return false, ErrPlaybackMediaSessionConflict
			}
			session.LastUsedAt = timePointer(now)
			session.IdleExpiresAt = idleExpiresAt
			session.Version++
			session.UpdatedAt = now
			touched = session
			return true, nil
		},
	)
	if err != nil {
		return nil, err
	}
	return touched, nil
}

// RevokePlaybackMediaSession revokes one capability using an exact binding and
// version check.
func (s *Store) RevokePlaybackMediaSession(
	binding models.PlaybackMediaSessionBinding,
	capability string,
	expectedVersion int64,
	now time.Time,
) (*models.PlaybackMediaSession, error) {
	return s.RevokePlaybackMediaSessionContext(
		context.Background(), binding, capability, expectedVersion, now)
}

// RevokePlaybackMediaSessionContext is RevokePlaybackMediaSession with
// cancellation covering the shared writer wait and the version CAS.
func (s *Store) RevokePlaybackMediaSessionContext(
	ctx context.Context,
	binding models.PlaybackMediaSessionBinding,
	capability string,
	expectedVersion int64,
	now time.Time,
) (*models.PlaybackMediaSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	now = playbackTime(now)
	if err := validatePlaybackMediaSessionBinding(binding); err != nil {
		return nil, ErrPlaybackMediaSessionNotFound
	}
	if expectedVersion <= 0 {
		return nil, ErrPlaybackMediaSessionConflict
	}
	var revoked *models.PlaybackMediaSession
	err := s.withPlaybackWriteTx(
		ctx,
		func(ctx context.Context, tx *sql.Tx) error {
			return lockPlaybackMediaSessionTxContext(ctx, tx, capability)
		},
		func(ctx context.Context, tx *sql.Tx) (bool, error) {
			session, err := scanPlaybackMediaSession(tx.QueryRowContext(ctx, `SELECT `+
				playbackMediaSessionColumns+`
				FROM playback_media_sessions
				WHERE capability_hash = ? AND ticket_id = ? AND user_id = ?
					AND device_id = ? AND file_id = ? AND transport = ?`,
				hashPlaybackCapability(capability), binding.Ticket.TicketID,
				binding.Ticket.UserID, binding.Ticket.DeviceID, binding.Ticket.FileID,
				binding.Transport))
			if errors.Is(err, sql.ErrNoRows) {
				return false, ErrPlaybackMediaSessionNotFound
			}
			if err != nil {
				return false, err
			}
			switch session.State {
			case models.PlaybackMediaSessionRevoked:
				return false, ErrPlaybackMediaSessionRevoked
			case models.PlaybackMediaSessionExpired:
				return false, ErrPlaybackMediaSessionExpired
			}
			if session.Version != expectedVersion {
				return false, ErrPlaybackMediaSessionConflict
			}
			res, err := tx.ExecContext(ctx, `UPDATE playback_media_sessions
				SET state = ?, revoked_at = ?, version = version + 1, updated_at = ?
				WHERE id = ? AND version = ? AND state = ?`,
				models.PlaybackMediaSessionRevoked, now, now, session.ID,
				expectedVersion, models.PlaybackMediaSessionActive)
			if err != nil {
				return false, err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return false, err
			}
			if n != 1 {
				return false, ErrPlaybackMediaSessionConflict
			}
			session.State = models.PlaybackMediaSessionRevoked
			session.RevokedAt = timePointer(now)
			session.Version++
			session.UpdatedAt = now
			revoked = session
			return true, nil
		},
	)
	if err != nil {
		return nil, err
	}
	return revoked, nil
}

// RevokePlaybackMediaSessionsForTicket revokes all active sessions for one
// exact binding. It is safe to call when a ticket is being administratively
// disabled but was already in a terminal state.
func (s *Store) RevokePlaybackMediaSessionsForTicket(
	binding models.PlaybackTicketBinding,
	now time.Time,
) (int64, error) {
	return s.RevokePlaybackMediaSessionsForTicketContext(
		context.Background(), binding, now)
}

// RevokePlaybackMediaSessionsForTicketContext is
// RevokePlaybackMediaSessionsForTicket using the shared playback writer budget.
func (s *Store) RevokePlaybackMediaSessionsForTicketContext(
	ctx context.Context,
	binding models.PlaybackTicketBinding,
	now time.Time,
) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	now = playbackTime(now)
	if err := validatePlaybackTicketBinding(binding); err != nil {
		return 0, err
	}
	var revoked int64
	err := s.withPlaybackWriteTx(
		ctx,
		func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `UPDATE playback_media_sessions
				SET version = version
				WHERE ticket_id = ? AND user_id = ? AND device_id = ? AND file_id = ?`,
				binding.TicketID, binding.UserID, binding.DeviceID, binding.FileID)
			return err
		},
		func(ctx context.Context, tx *sql.Tx) (bool, error) {
			res, err := tx.ExecContext(ctx, `UPDATE playback_media_sessions
				SET state = ?, revoked_at = ?, version = version + 1, updated_at = ?
				WHERE ticket_id = ? AND user_id = ? AND device_id = ? AND file_id = ?
					AND state = ?`,
				models.PlaybackMediaSessionRevoked, now, now, binding.TicketID,
				binding.UserID, binding.DeviceID, binding.FileID,
				models.PlaybackMediaSessionActive)
			if err != nil {
				return false, err
			}
			revoked, err = res.RowsAffected()
			return err == nil, err
		},
	)
	if err != nil {
		return 0, err
	}
	return revoked, nil
}

// RevokePlaybackMediaSessionsForTicketTransport revokes only the media sessions
// for one transport on an exact ticket. A failed response must not revoke a
// concurrently issued fallback transport whose credential reached its client.
func (s *Store) RevokePlaybackMediaSessionsForTicketTransport(
	binding models.PlaybackTicketBinding,
	transport string,
	now time.Time,
) (int64, error) {
	return s.RevokePlaybackMediaSessionsForTicketTransportContext(
		context.Background(), binding, transport, now)
}

// RevokePlaybackMediaSessionsForTicketTransportContext is
// RevokePlaybackMediaSessionsForTicketTransport using the shared playback
// writer budget.
func (s *Store) RevokePlaybackMediaSessionsForTicketTransportContext(
	ctx context.Context,
	binding models.PlaybackTicketBinding,
	transport string,
	now time.Time,
) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	now = playbackTime(now)
	if err := validatePlaybackTicketBinding(binding); err != nil {
		return 0, err
	}
	if !validPlaybackTransport(transport) {
		return 0, ErrInvalidPlaybackTransport
	}
	var revoked int64
	err := s.withPlaybackWriteTx(
		ctx,
		func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `UPDATE playback_media_sessions
				SET version = version
				WHERE ticket_id = ? AND user_id = ? AND device_id = ? AND file_id = ?
					AND transport = ?`,
				binding.TicketID, binding.UserID, binding.DeviceID, binding.FileID,
				transport)
			return err
		},
		func(ctx context.Context, tx *sql.Tx) (bool, error) {
			res, err := tx.ExecContext(ctx, `UPDATE playback_media_sessions
				SET state = ?, revoked_at = ?, version = version + 1, updated_at = ?
				WHERE ticket_id = ? AND user_id = ? AND device_id = ? AND file_id = ?
					AND transport = ? AND state = ?`,
				models.PlaybackMediaSessionRevoked, now, now, binding.TicketID,
				binding.UserID, binding.DeviceID, binding.FileID, transport,
				models.PlaybackMediaSessionActive)
			if err != nil {
				return false, err
			}
			revoked, err = res.RowsAffected()
			return err == nil, err
		},
	)
	if err != nil {
		return 0, err
	}
	return revoked, nil
}

// RevokePlaybackMediaSessionsForTransport revokes every active media session
// for one server transport. It is intentionally scoped to the fixed transport
// enum so feature-gate shutdowns cannot turn into arbitrary table writes.
func (s *Store) RevokePlaybackMediaSessionsForTransport(
	transport string,
	now time.Time,
) (int64, error) {
	return s.RevokePlaybackMediaSessionsForTransportContext(
		context.Background(), transport, now)
}

// RevokePlaybackMediaSessionsForTransportContext is
// RevokePlaybackMediaSessionsForTransport using the shared playback writer
// budget.
func (s *Store) RevokePlaybackMediaSessionsForTransportContext(
	ctx context.Context,
	transport string,
	now time.Time,
) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if transport != models.PlaybackTransportServerProxy &&
		transport != models.PlaybackTransportServerHLS {
		return 0, ErrInvalidPlaybackTransport
	}
	now = playbackTime(now)
	var revoked int64
	err := s.withPlaybackWriteTx(
		ctx,
		func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `UPDATE playback_media_sessions
				SET version = version WHERE transport = ?`, transport)
			return err
		},
		func(ctx context.Context, tx *sql.Tx) (bool, error) {
			res, err := tx.ExecContext(ctx, `UPDATE playback_media_sessions
				SET state = ?, revoked_at = ?, version = version + 1, updated_at = ?
				WHERE transport = ? AND state = ?`,
				models.PlaybackMediaSessionRevoked, now, now, transport,
				models.PlaybackMediaSessionActive)
			if err != nil {
				return false, err
			}
			revoked, err = res.RowsAffected()
			return err == nil, err
		},
	)
	if err != nil {
		return 0, err
	}
	return revoked, nil
}

// ExpirePlaybackMediaSessions conditionally expires sessions that reached
// either their idle or absolute deadline.
func (s *Store) ExpirePlaybackMediaSessions(now time.Time) (int64, error) {
	now = playbackTime(now)
	res, err := s.DB.Exec(`UPDATE playback_media_sessions
		SET state = ?, version = version + 1, updated_at = ?
		WHERE state = ? AND (idle_expires_at <= ? OR absolute_expires_at <= ?)`,
		models.PlaybackMediaSessionExpired, now,
		models.PlaybackMediaSessionActive, now, now)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func loadPlaybackTicketTx(tx *sql.Tx, binding models.PlaybackTicketBinding) (*models.PlaybackTicket, error) {
	return loadPlaybackTicketTxContext(context.Background(), tx, binding)
}

func loadPlaybackTicketTxContext(
	ctx context.Context,
	tx *sql.Tx,
	binding models.PlaybackTicketBinding,
) (*models.PlaybackTicket, error) {
	ticket, err := scanPlaybackTicket(tx.QueryRowContext(ctx, `SELECT `+playbackTicketColumns+`
		FROM playback_tickets
		WHERE ticket_id = ? AND user_id = ? AND device_id = ? AND file_id = ?`,
		binding.TicketID, binding.UserID, binding.DeviceID, binding.FileID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPlaybackTicketNotFound
	}
	return ticket, err
}

type playbackBusyTimeoutRestorer func(*sql.Conn, *sql.Tx) error

// withPlaybackWriteTx keeps cancellation responsive while waiting for SQLite's
// writer lock. The busy handler sleeps without observing interrupts, so the
// request context cannot interrupt its default 10 second wait. Pin one
// connection, disable that handler only there, and retry locked no-op updates
// with Go-side backoff.
func (s *Store) withPlaybackWriteTx(
	ctx context.Context,
	lock func(context.Context, *sql.Tx) error,
	fn func(context.Context, *sql.Tx) (bool, error),
) error {
	return s.withPlaybackWriteTxConfigured(
		ctx, playbackWriteLockBudget, restorePlaybackBusyTimeout, lock, fn)
}

func (s *Store) withPlaybackWriteTxConfigured(
	ctx context.Context,
	budget time.Duration,
	restore playbackBusyTimeoutRestorer,
	lock func(context.Context, *sql.Tx) error,
	fn func(context.Context, *sql.Tx) (bool, error),
) error {
	return s.withPlaybackWriteTxConfiguredAndCommit(
		ctx, budget, restore, lock, fn, func(tx *sql.Tx) error {
			return tx.Commit()
		})
}

func (s *Store) withPlaybackWriteTxConfiguredAndCommit(
	ctx context.Context,
	budget time.Duration,
	restore playbackBusyTimeoutRestorer,
	lock func(context.Context, *sql.Tx) error,
	fn func(context.Context, *sql.Tx) (bool, error),
	commitTx func(*sql.Tx) error,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.playbackWriteSem == nil {
		return errors.New("playback write semaphore is not initialized")
	}
	operationCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	select {
	case s.playbackWriteSem <- struct{}{}:
		defer func() { <-s.playbackWriteSem }()
	case <-operationCtx.Done():
		return operationCtx.Err()
	}

	conn, err := s.DB.Conn(operationCtx)
	if err != nil {
		return playbackContextError(operationCtx, err)
	}

	var tx *sql.Tx
	restored := false
	discard := false
	defer func() {
		if tx != nil {
			if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
				discard = true
			}
		}
		if !restored && !discard {
			if err := restore(conn, nil); err != nil {
				discard = true
			}
		}
		if discard {
			discardPlaybackSQLConn(conn)
		}
		_ = conn.Close()
	}()

	if _, err := conn.ExecContext(operationCtx, "PRAGMA busy_timeout=0"); err != nil {
		return playbackContextError(operationCtx, err)
	}

	lockDeadline, _ := operationCtx.Deadline()
	backoff := playbackWriteLockMinBackoff
	for {
		if err := operationCtx.Err(); err != nil {
			return err
		}
		tx, err = conn.BeginTx(operationCtx, nil)
		if err != nil {
			return playbackContextError(operationCtx, err)
		}
		err = lock(operationCtx, tx)
		if err == nil {
			break
		}
		rollbackErr := tx.Rollback()
		tx = nil
		if rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			discard = true
			return playbackContextError(operationCtx, rollbackErr)
		}
		if !isSQLiteBusy(err) {
			return playbackContextError(operationCtx, err)
		}

		remaining := time.Until(lockDeadline)
		if remaining <= 0 {
			if err := operationCtx.Err(); err != nil {
				return err
			}
			return context.DeadlineExceeded
		}
		wait := backoff
		if wait > remaining {
			wait = remaining
		}
		timer := time.NewTimer(wait)
		select {
		case <-operationCtx.Done():
			timer.Stop()
			return operationCtx.Err()
		case <-timer.C:
		}
		backoff *= 2
		if backoff > playbackWriteLockMaxBackoff {
			backoff = playbackWriteLockMaxBackoff
		}
	}

	commit, operationErr := fn(operationCtx, tx)
	if err := operationCtx.Err(); err != nil {
		return err
	}
	if !commit {
		return operationErr
	}
	if err := restore(conn, tx); err != nil {
		return playbackContextError(operationCtx, err)
	}
	restored = true
	if err := operationCtx.Err(); err != nil {
		return err
	}
	// Commit has no context. Once COMMIT starts, persistence wins over a
	// concurrent cancellation; checking the context afterward would report a
	// canceled request even though the write succeeded.
	if err := commitTx(tx); err != nil {
		return playbackContextError(operationCtx, err)
	}
	tx = nil
	return operationErr
}

func playbackContextError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return err
}

func restorePlaybackBusyTimeout(conn *sql.Conn, tx *sql.Tx) error {
	ctx, cancel := context.WithTimeout(
		context.Background(), playbackSQLiteBusyTimeoutCleanup)
	defer cancel()
	expression := fmt.Sprintf("PRAGMA busy_timeout=%d", playbackSQLiteBusyTimeout)
	var err error
	if tx != nil {
		_, err = tx.ExecContext(ctx, expression)
	} else {
		_, err = conn.ExecContext(ctx, expression)
	}
	return err
}

func discardPlaybackSQLConn(conn *sql.Conn) {
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	_ = conn.Close()
}

// Acquiring a no-op write before reading turns the short ticket transaction
// into a serialized SQLite writer. This avoids a deferred read-to-write upgrade
// racing another claim, while all real state changes still use version CAS.
func lockPlaybackTicketTx(tx *sql.Tx, binding models.PlaybackTicketBinding) error {
	return lockPlaybackTicketTxContext(context.Background(), tx, binding)
}

func lockPlaybackTicketTxContext(
	ctx context.Context,
	tx *sql.Tx,
	binding models.PlaybackTicketBinding,
) error {
	_, err := tx.ExecContext(ctx, `UPDATE playback_tickets SET version = version
		WHERE ticket_id = ? AND user_id = ? AND device_id = ? AND file_id = ?`,
		binding.TicketID, binding.UserID, binding.DeviceID, binding.FileID)
	return err
}

// A no-op row update acquires SQLite's writer reservation without changing the
// stored version. Session authorization and revocation can then read a stable
// snapshot and apply their real version CAS instead of failing with
// SQLITE_BUSY_SNAPSHOT after a deferred read-to-write upgrade.
func lockPlaybackMediaSessionTx(tx *sql.Tx, capability string) error {
	return lockPlaybackMediaSessionTxContext(context.Background(), tx, capability)
}

func lockPlaybackMediaSessionTxContext(
	ctx context.Context,
	tx *sql.Tx,
	capability string,
) error {
	_, err := tx.ExecContext(ctx, `UPDATE playback_media_sessions SET version = version
		WHERE capability_hash = ?`, hashPlaybackCapability(capability))
	return err
}

func scanPlaybackTicket(row interface{ Scan(...any) error }) (*models.PlaybackTicket, error) {
	var (
		ticket       models.PlaybackTicket
		candidates   string
		attempted    string
		summary      string
		capabilities string
		subtitleIDs  string
		consumedAt   sql.NullTime
		revokedAt    sql.NullTime
		claimExpires sql.NullTime
	)
	if err := row.Scan(&ticket.ID, &ticket.TicketID, &ticket.UserID,
		&ticket.DeviceID, &ticket.FileID, &ticket.Purpose, &ticket.State,
		&candidates, &attempted, &ticket.SelectedTransport, &summary,
		&capabilities, &subtitleIDs, &ticket.FailureCode, &ticket.VideoIndex,
		&ticket.Version, &ticket.CreatedAt, &ticket.ExpiresAt,
		&ticket.UpdatedAt, &consumedAt, &revokedAt, &ticket.ClaimToken,
		&claimExpires); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(candidates), &ticket.CandidateTransports); err != nil {
		return nil, fmt.Errorf("decode candidate transports: %w", err)
	}
	if err := json.Unmarshal([]byte(attempted), &ticket.AttemptedTransports); err != nil {
		return nil, fmt.Errorf("decode attempted transports: %w", err)
	}
	ids, err := decodePlaybackSubtitleIDs(subtitleIDs)
	if err != nil {
		return nil, err
	}
	ticket.SubtitleIDs = ids
	if ticket.CandidateTransports == nil {
		ticket.CandidateTransports = []string{}
	}
	if ticket.AttemptedTransports == nil {
		ticket.AttemptedTransports = []string{}
	}
	ticket.MediaSummary = append(json.RawMessage(nil), summary...)
	ticket.ClientCapabilities = append(json.RawMessage(nil), capabilities...)
	if consumedAt.Valid {
		ticket.ConsumedAt = timePointer(consumedAt.Time)
	}
	if revokedAt.Valid {
		ticket.RevokedAt = timePointer(revokedAt.Time)
	}
	if claimExpires.Valid {
		ticket.ClaimExpiresAt = timePointer(claimExpires.Time)
	}
	return &ticket, nil
}

func scanPlaybackMediaSession(row interface{ Scan(...any) error }) (*models.PlaybackMediaSession, error) {
	var (
		session         models.PlaybackMediaSession
		quality         sql.NullString
		transcodeHeight sql.NullInt64
		lastUsedAt      sql.NullTime
		revokedAt       sql.NullTime
	)
	if err := row.Scan(&session.ID, &session.TicketID, &session.UserID,
		&session.DeviceID, &session.FileID, &session.Purpose,
		&session.Transport, &session.State, &session.Mode,
		&session.AudioOrdinal, &session.VideoIndex, &session.StartSeconds,
		&quality, &transcodeHeight, &session.Version, &session.CreatedAt,
		&lastUsedAt, &session.IdleExpiresAt, &session.AbsoluteExpiresAt,
		&session.UpdatedAt, &revokedAt); err != nil {
		return nil, err
	}
	if quality.Valid {
		value := quality.String
		session.Quality = &value
	}
	if transcodeHeight.Valid {
		value := int(transcodeHeight.Int64)
		session.TranscodeHeight = &value
	}
	if lastUsedAt.Valid {
		session.LastUsedAt = timePointer(lastUsedAt.Time)
	}
	if revokedAt.Valid {
		session.RevokedAt = timePointer(revokedAt.Time)
	}
	return &session, nil
}

func expirePlaybackTicketTx(tx *sql.Tx, ticket *models.PlaybackTicket, now time.Time) (bool, error) {
	return expirePlaybackTicketTxContext(context.Background(), tx, ticket, now)
}

func expirePlaybackTicketTxContext(
	ctx context.Context,
	tx *sql.Tx,
	ticket *models.PlaybackTicket,
	now time.Time,
) (bool, error) {
	res, err := tx.ExecContext(ctx, `UPDATE playback_tickets
		SET state = ?, version = version + 1, updated_at = ?
		WHERE id = ? AND version = ? AND state IN (?,?,?)
			AND expires_at <= ? AND revoked_at IS NULL`,
		models.PlaybackTicketExpired, now, ticket.ID, ticket.Version,
		models.PlaybackTicketProbing, models.PlaybackTicketReady,
		models.PlaybackTicketConsuming, now)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n != 1 {
		return false, nil
	}
	ticket.State = models.PlaybackTicketExpired
	ticket.Version++
	ticket.UpdatedAt = now
	return true, nil
}

func normalizePlaybackMediaSessionRequest(
	req models.PlaybackMediaSessionRequest,
) (models.PlaybackMediaSessionRequest, error) {
	if err := validatePlaybackTicketBinding(req.Ticket); err != nil {
		return req, ErrInvalidPlaybackMediaSession
	}
	if req.Transport != models.PlaybackTransportServerProxy &&
		req.Transport != models.PlaybackTransportServerHLS {
		return req, ErrInvalidPlaybackTransport
	}
	req.Mode = strings.TrimSpace(req.Mode)
	if req.Mode == "" || len(req.Mode) > 32 || !safePlaybackToken(req.Mode) {
		return req, ErrInvalidPlaybackMediaSession
	}
	if req.AudioOrdinal < -1 {
		return req, ErrInvalidPlaybackMediaSession
	}
	if req.StartSeconds < 0 || math.IsNaN(req.StartSeconds) || math.IsInf(req.StartSeconds, 0) {
		return req, ErrInvalidPlaybackMediaSession
	}
	if req.Quality != nil {
		quality := strings.TrimSpace(*req.Quality)
		if quality == "" || len(quality) > 64 || !safePlaybackQuality(quality) {
			return req, ErrInvalidPlaybackMediaSession
		}
		req.Quality = &quality
	}
	if req.TranscodeHeight != nil {
		height := *req.TranscodeHeight
		if height <= 0 || height > 16384 {
			return req, ErrInvalidPlaybackMediaSession
		}
		req.TranscodeHeight = &height
	}
	return req, nil
}

func normalizePlaybackTransports(transports []string, allowEmpty bool) ([]string, error) {
	if len(transports) == 0 {
		if allowEmpty {
			return []string{}, nil
		}
		return nil, ErrInvalidPlaybackTransport
	}
	if len(transports) > 8 {
		return nil, ErrInvalidPlaybackTransport
	}
	out := make([]string, 0, len(transports))
	seen := make(map[string]struct{}, len(transports))
	for _, raw := range transports {
		transport := strings.TrimSpace(raw)
		if !validPlaybackTransport(transport) {
			return nil, ErrInvalidPlaybackTransport
		}
		if _, ok := seen[transport]; ok {
			return nil, ErrInvalidPlaybackTransport
		}
		seen[transport] = struct{}{}
		out = append(out, transport)
	}
	return out, nil
}

func validPlaybackTransport(transport string) bool {
	switch transport {
	case models.PlaybackTransportDirectHTTP,
		models.PlaybackTransportDirectDASH,
		models.PlaybackTransportServerProxy,
		models.PlaybackTransportServerHLS:
		return true
	default:
		return false
	}
}

func normalizePlaybackMediaSummary(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return json.RawMessage(`{}`), nil
	}
	if len(raw) > maxPlaybackMediaSummary || !json.Valid(raw) {
		return nil, ErrInvalidPlaybackTicket
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, ErrInvalidPlaybackTicket
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, ErrInvalidPlaybackTicket
	}
	if err := validatePlaybackSummaryValue(object, 0); err != nil {
		return nil, err
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return nil, ErrInvalidPlaybackTicket
	}
	return json.RawMessage(compact.Bytes()), nil
}

func normalizePlaybackClientCapabilities(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return json.RawMessage(`{}`), nil
	}
	if len(raw) > maxPlaybackCapabilities || !json.Valid(raw) {
		return nil, ErrInvalidPlaybackTicket
	}
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, ErrInvalidPlaybackTicket
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return nil, ErrInvalidPlaybackTicket
	}
	return json.RawMessage(compact.Bytes()), nil
}

func validatePlaybackSummaryValue(value any, depth int) error {
	if depth > 12 {
		return ErrInvalidPlaybackTicket
	}
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			normalized := normalizePlaybackKey(key)
			for _, fragment := range []string{
				"url", "header", "cookie", "authorization", "token",
				"credential", "capability", "secret", "password",
			} {
				if strings.Contains(normalized, fragment) {
					return ErrInvalidPlaybackTicket
				}
			}
			if err := validatePlaybackSummaryValue(child, depth+1); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range typed {
			if err := validatePlaybackSummaryValue(child, depth+1); err != nil {
				return err
			}
		}
	case string:
		value := strings.TrimSpace(typed)
		if strings.Contains(value, "://") || !safePlaybackValue(value) {
			return ErrInvalidPlaybackTicket
		}
		lower := strings.ToLower(value)
		for _, fragment := range []string{
			"authorization:", "cookie:", "set-cookie:", "referer:", "bearer ",
		} {
			if strings.Contains(lower, fragment) {
				return ErrInvalidPlaybackTicket
			}
		}
	}
	return nil
}

func normalizePlaybackKey(key string) string {
	key = strings.ToLower(strings.TrimSpace(key))
	replacer := strings.NewReplacer("_", "", "-", "", " ", "")
	return replacer.Replace(key)
}

func normalizePlaybackCode(code string) (string, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return "", nil
	}
	if len(code) > 128 || !safePlaybackToken(code) {
		return "", ErrInvalidPlaybackTicket
	}
	return code, nil
}

func safePlaybackToken(value string) bool {
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '-' || r == '.':
		default:
			return false
		}
	}
	return true
}

func safePlaybackValue(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func safePlaybackQuality(value string) bool {
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '-' || r == '.' || r == '+' || r == ' ':
		default:
			return false
		}
	}
	return true
}

func validatePlaybackTicketBinding(binding models.PlaybackTicketBinding) error {
	if binding.UserID <= 0 || binding.DeviceID <= 0 || binding.FileID <= 0 ||
		strings.TrimSpace(binding.TicketID) == "" {
		return ErrInvalidPlaybackTicket
	}
	return nil
}

func validatePlaybackMediaSessionBinding(binding models.PlaybackMediaSessionBinding) error {
	if err := validatePlaybackTicketBinding(binding.Ticket); err != nil {
		return err
	}
	if binding.Transport != models.PlaybackTransportServerProxy &&
		binding.Transport != models.PlaybackTransportServerHLS {
		return ErrInvalidPlaybackTransport
	}
	return nil
}

func playbackTicketExpirable(state string) bool {
	switch state {
	case models.PlaybackTicketProbing,
		models.PlaybackTicketReady,
		models.PlaybackTicketConsuming:
		return true
	default:
		return false
	}
}

func newPlaybackPublicID(prefix string, size int) (string, error) {
	raw := make([]byte, size)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

func newPlaybackCapability() (string, error) {
	return newPlaybackPublicID("ms_", 32)
}

func hashPlaybackCapability(capability string) string {
	sum := sha256.Sum256([]byte(capability))
	return hex.EncodeToString(sum[:])
}

func nullablePlaybackString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullablePlaybackInt(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}

func timePointer(value time.Time) *time.Time {
	return &value
}

func playbackTime(now time.Time) time.Time {
	if now.IsZero() {
		now = time.Now()
	}
	return now.UTC()
}
