package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"doriyamedia/internal/models"
)

var (
	ErrPlaybackSubtitleSessionNotFound = errors.New("playback subtitle session not found")
	ErrPlaybackSubtitleSessionExpired  = errors.New("playback subtitle session expired")
	ErrPlaybackSubtitleSessionRevoked  = errors.New("playback subtitle session revoked")
	ErrPlaybackSubtitleSessionConflict = errors.New("playback subtitle session state conflict")
	ErrInvalidPlaybackSubtitleSession  = errors.New("invalid playback subtitle session")
)

// PlaybackSubtitleSessionAccess is the live authorization result for one
// subtitle-capability request. The row's ticket/user/device/file binding remains
// authoritative.
type PlaybackSubtitleSessionAccess struct {
	Session *models.PlaybackSubtitleSession
	User    *models.User
	Device  *models.Device
}

var playbackSubtitleMigrationStatements = []string{
	`CREATE TABLE IF NOT EXISTS playback_subtitle_sessions (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		capability_hash TEXT NOT NULL,
		ticket_id TEXT NOT NULL REFERENCES playback_tickets(ticket_id) ON DELETE CASCADE,
		user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		device_id INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
		file_id INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
		purpose TEXT NOT NULL DEFAULT 'playback' CHECK(purpose = 'playback'),
		transport TEXT NOT NULL CHECK(transport IN (
			'direct-http','server-proxy','server-hls'
		)),
		subtitle_id INTEGER NOT NULL REFERENCES subtitles(id) ON DELETE CASCADE,
		state TEXT NOT NULL DEFAULT 'active' CHECK(state IN ('active','expired','revoked')),
		start_seconds REAL NOT NULL CHECK(start_seconds >= 0),
		version INTEGER NOT NULL DEFAULT 1 CHECK(version > 0),
		created_at TIMESTAMP NOT NULL,
		last_used_at TIMESTAMP,
		idle_expires_at TIMESTAMP NOT NULL,
		absolute_expires_at TIMESTAMP NOT NULL CHECK(idle_expires_at <= absolute_expires_at),
		updated_at TIMESTAMP NOT NULL,
		revoked_at TIMESTAMP
	)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_playback_subtitle_sessions_capability
		ON playback_subtitle_sessions(capability_hash)`,
	`CREATE INDEX IF NOT EXISTS idx_playback_subtitle_sessions_binding
		ON playback_subtitle_sessions(
			user_id, device_id, file_id, ticket_id, subtitle_id, state
		)`,
	`CREATE INDEX IF NOT EXISTS idx_playback_subtitle_sessions_expiry
		ON playback_subtitle_sessions(state, idle_expires_at, absolute_expires_at)`,
	`CREATE TRIGGER IF NOT EXISTS trg_playback_subtitle_sessions_binding_insert
	BEFORE INSERT ON playback_subtitle_sessions
	WHEN NOT EXISTS (
		SELECT 1 FROM playback_tickets t
		JOIN subtitles s ON s.id = NEW.subtitle_id
		WHERE t.ticket_id = NEW.ticket_id
			AND t.user_id = NEW.user_id
			AND t.device_id = NEW.device_id
			AND t.file_id = NEW.file_id
			AND t.purpose = NEW.purpose
			AND t.selected_transport = NEW.transport
			AND s.file_id = NEW.file_id
	)
	BEGIN
		SELECT RAISE(ABORT, 'playback subtitle session binding mismatch');
	END`,
	`CREATE TRIGGER IF NOT EXISTS trg_playback_subtitle_sessions_params_immutable
	BEFORE UPDATE OF capability_hash, ticket_id, user_id, device_id, file_id,
		purpose, transport, subtitle_id, start_seconds, created_at,
		absolute_expires_at
	ON playback_subtitle_sessions
	WHEN OLD.capability_hash IS NOT NEW.capability_hash
		OR OLD.ticket_id IS NOT NEW.ticket_id
		OR OLD.user_id IS NOT NEW.user_id
		OR OLD.device_id IS NOT NEW.device_id
		OR OLD.file_id IS NOT NEW.file_id
		OR OLD.purpose IS NOT NEW.purpose
		OR OLD.transport IS NOT NEW.transport
		OR OLD.subtitle_id IS NOT NEW.subtitle_id
		OR OLD.start_seconds IS NOT NEW.start_seconds
		OR OLD.created_at IS NOT NEW.created_at
		OR OLD.absolute_expires_at IS NOT NEW.absolute_expires_at
	BEGIN
		SELECT RAISE(ABORT, 'playback subtitle session parameters are immutable');
	END`,
}

const playbackSubtitleSessionColumns = `id, ticket_id, user_id, device_id,
	file_id, purpose, transport, subtitle_id, state, start_seconds, version,
	created_at, last_used_at, idle_expires_at, absolute_expires_at, updated_at,
	revoked_at`

// CreatePlaybackSubtitleSession issues a separate short-lived capability for
// one subtitle already granted to the ticket. Only the SHA-256 hash is stored.
func (s *Store) CreatePlaybackSubtitleSession(
	req models.PlaybackSubtitleSessionRequest,
	now time.Time,
) (*models.PlaybackSubtitleSessionGrant, error) {
	now = playbackTime(now)
	req, err := normalizePlaybackSubtitleSessionRequest(req)
	if err != nil {
		return nil, err
	}
	capability, err := newPlaybackPublicID("ss_", 32)
	if err != nil {
		return nil, err
	}

	tx, err := s.DB.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := lockPlaybackTicketTx(tx, req.Ticket); err != nil {
		return nil, err
	}
	ticket, err := loadPlaybackTicketTx(tx, req.Ticket)
	if err != nil {
		return nil, err
	}
	if err := requirePlaybackTransportAllowedTx(tx, ticket.DeviceID, req.Transport); err != nil {
		return nil, err
	}
	switch ticket.State {
	case models.PlaybackTicketExpired:
		return nil, ErrPlaybackTicketExpired
	case models.PlaybackTicketRevoked:
		return nil, ErrPlaybackTicketRevoked
	case models.PlaybackTicketConsuming:
	default:
		return nil, ErrPlaybackTicketConflict
	}
	if !now.Before(ticket.ExpiresAt) {
		expired, err := expirePlaybackTicketTx(tx, ticket, now)
		if err != nil {
			return nil, err
		}
		if !expired {
			return nil, ErrPlaybackTicketConflict
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return nil, ErrPlaybackTicketExpired
	}
	if ticket.SelectedTransport != req.Transport ||
		!playbackTicketGrantsSubtitle(ticket, req.SubtitleID) {
		return nil, ErrInvalidPlaybackSubtitleSession
	}
	var fileID int64
	err = tx.QueryRow(`SELECT file_id FROM subtitles WHERE id = ?`,
		req.SubtitleID).Scan(&fileID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && fileID != ticket.FileID) {
		return nil, ErrInvalidPlaybackSubtitleSession
	}
	if err != nil {
		return nil, err
	}

	absoluteExpiresAt := now.Add(models.PlaybackSubtitleSessionAbsoluteTTL)
	idleExpiresAt := now.Add(models.PlaybackSubtitleSessionIdleTTL)
	if idleExpiresAt.After(absoluteExpiresAt) {
		idleExpiresAt = absoluteExpiresAt
	}
	res, err := tx.Exec(`INSERT INTO playback_subtitle_sessions
		(capability_hash, ticket_id, user_id, device_id, file_id, purpose,
		 transport, subtitle_id, state, start_seconds, version, created_at,
		 idle_expires_at, absolute_expires_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		hashPlaybackCapability(capability), req.Ticket.TicketID,
		req.Ticket.UserID, req.Ticket.DeviceID, req.Ticket.FileID,
		models.PlaybackPurposePlayback, req.Transport, req.SubtitleID,
		models.PlaybackSubtitleSessionActive, req.StartSeconds, 1, now,
		idleExpiresAt, absoluteExpiresAt, now)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	session := &models.PlaybackSubtitleSession{
		ID: id, TicketID: req.Ticket.TicketID, UserID: req.Ticket.UserID,
		DeviceID: req.Ticket.DeviceID, FileID: req.Ticket.FileID,
		Purpose: models.PlaybackPurposePlayback, Transport: req.Transport,
		SubtitleID: req.SubtitleID, State: models.PlaybackSubtitleSessionActive,
		StartSeconds: req.StartSeconds, Version: 1, CreatedAt: now,
		IdleExpiresAt: idleExpiresAt, AbsoluteExpiresAt: absoluteExpiresAt,
		UpdatedAt: now,
	}
	return &models.PlaybackSubtitleSessionGrant{
		Session: session, Capability: capability,
	}, nil
}

// AuthorizePlaybackSubtitleSession resolves a capability and rechecks the live
// account, device, and non-revoked ticket state in the same transaction that
// slides the idle deadline.
func (s *Store) AuthorizePlaybackSubtitleSession(
	capability string,
	requireEmailVerification bool,
	now time.Time,
) (*PlaybackSubtitleSessionAccess, error) {
	now = playbackTime(now)
	if strings.TrimSpace(capability) == "" {
		return nil, ErrPlaybackSubtitleSessionNotFound
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := lockPlaybackSubtitleSessionTx(tx, capability); err != nil {
		return nil, err
	}

	session, err := scanPlaybackSubtitleSession(tx.QueryRow(`SELECT `+
		playbackSubtitleSessionColumns+`
		FROM playback_subtitle_sessions WHERE capability_hash = ?`,
		hashPlaybackCapability(capability)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPlaybackSubtitleSessionNotFound
	}
	if err != nil {
		return nil, err
	}
	switch session.State {
	case models.PlaybackSubtitleSessionRevoked:
		return nil, ErrPlaybackSubtitleSessionRevoked
	case models.PlaybackSubtitleSessionExpired:
		return nil, ErrPlaybackSubtitleSessionExpired
	case models.PlaybackSubtitleSessionActive:
	default:
		return nil, ErrPlaybackSubtitleSessionConflict
	}
	if !now.Before(session.IdleExpiresAt) || !now.Before(session.AbsoluteExpiresAt) {
		res, err := tx.Exec(`UPDATE playback_subtitle_sessions
			SET state = ?, version = version + 1, updated_at = ?
			WHERE id = ? AND version = ? AND state = ?
				AND (idle_expires_at <= ? OR absolute_expires_at <= ?)`,
			models.PlaybackSubtitleSessionExpired, now, session.ID,
			session.Version, models.PlaybackSubtitleSessionActive, now, now)
		if err != nil {
			return nil, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return nil, err
		}
		if n != 1 {
			return nil, ErrPlaybackSubtitleSessionConflict
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return nil, ErrPlaybackSubtitleSessionExpired
	}

	var ticketState string
	err = tx.QueryRow(`SELECT state FROM playback_tickets
		WHERE ticket_id = ? AND user_id = ? AND device_id = ? AND file_id = ?`,
		session.TicketID, session.UserID, session.DeviceID, session.FileID).
		Scan(&ticketState)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPlaybackTicketNotFound
	}
	if err != nil {
		return nil, err
	}
	if ticketState == models.PlaybackTicketRevoked {
		return nil, ErrPlaybackTicketRevoked
	}

	var (
		user          models.User
		emailVerified int
		device        models.Device
		deviceRevoked sql.NullTime
	)
	err = tx.QueryRow(`SELECT
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
		return nil, ErrDeviceUserUnavailable
	}
	if err != nil {
		return nil, err
	}
	user.EmailVerified = emailVerified != 0
	if !deviceAuthUserAllowed(user.Status, user.Email, user.EmailVerified, requireEmailVerification) {
		return nil, ErrDeviceUserUnavailable
	}
	if deviceRevoked.Valid {
		return nil, ErrDeviceRevoked
	}
	overrides, err := loadDeviceFeatureOverrides(tx, device.ID)
	if err != nil {
		return nil, err
	}
	if overrides.DisablesPlaybackTickets() ||
		overrides.DisablesPlaybackTransport(session.Transport) {
		return nil, ErrDevicePlaybackDisabled
	}

	idleExpiresAt := now.Add(models.PlaybackSubtitleSessionIdleTTL)
	if idleExpiresAt.After(session.AbsoluteExpiresAt) {
		idleExpiresAt = session.AbsoluteExpiresAt
	}
	res, err := tx.Exec(`UPDATE playback_subtitle_sessions
		SET last_used_at = ?, idle_expires_at = ?, version = version + 1,
			updated_at = ?
		WHERE id = ? AND version = ? AND state = ?
			AND idle_expires_at > ? AND absolute_expires_at > ?`,
		now, idleExpiresAt, now, session.ID, session.Version,
		models.PlaybackSubtitleSessionActive, now, now)
	if err != nil {
		return nil, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n != 1 {
		return nil, ErrPlaybackSubtitleSessionConflict
	}
	if _, err := tx.Exec(`UPDATE devices SET last_seen_at = ?
		WHERE id = ? AND revoked_at IS NULL`, now, device.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	session.LastUsedAt = timePointer(now)
	session.IdleExpiresAt = idleExpiresAt
	session.Version++
	session.UpdatedAt = now
	return &PlaybackSubtitleSessionAccess{
		Session: session, User: &user, Device: &device,
	}, nil
}

// RevokePlaybackSubtitleSession revokes one capability with a version CAS.
func (s *Store) RevokePlaybackSubtitleSession(
	capability string,
	expectedVersion int64,
	now time.Time,
) (*models.PlaybackSubtitleSession, error) {
	now = playbackTime(now)
	if strings.TrimSpace(capability) == "" || expectedVersion <= 0 {
		return nil, ErrPlaybackSubtitleSessionConflict
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := lockPlaybackSubtitleSessionTx(tx, capability); err != nil {
		return nil, err
	}
	session, err := scanPlaybackSubtitleSession(tx.QueryRow(`SELECT `+
		playbackSubtitleSessionColumns+`
		FROM playback_subtitle_sessions WHERE capability_hash = ?`,
		hashPlaybackCapability(capability)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPlaybackSubtitleSessionNotFound
	}
	if err != nil {
		return nil, err
	}
	switch session.State {
	case models.PlaybackSubtitleSessionRevoked:
		return nil, ErrPlaybackSubtitleSessionRevoked
	case models.PlaybackSubtitleSessionExpired:
		return nil, ErrPlaybackSubtitleSessionExpired
	}
	if session.Version != expectedVersion {
		return nil, ErrPlaybackSubtitleSessionConflict
	}
	res, err := tx.Exec(`UPDATE playback_subtitle_sessions
		SET state = ?, revoked_at = ?, version = version + 1, updated_at = ?
		WHERE id = ? AND version = ? AND state = ?`,
		models.PlaybackSubtitleSessionRevoked, now, now, session.ID,
		expectedVersion, models.PlaybackSubtitleSessionActive)
	if err != nil {
		return nil, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n != 1 {
		return nil, ErrPlaybackSubtitleSessionConflict
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	session.State = models.PlaybackSubtitleSessionRevoked
	session.RevokedAt = timePointer(now)
	session.Version++
	session.UpdatedAt = now
	return session, nil
}

// RevokePlaybackSubtitleSessionsForTransport is used by emergency feature
// shutdown. It is restricted to the fixed transport enum.
func (s *Store) RevokePlaybackSubtitleSessionsForTransport(
	transport string,
	now time.Time,
) (int64, error) {
	if !validPlaybackTransport(transport) {
		return 0, ErrInvalidPlaybackTransport
	}
	now = playbackTime(now)
	res, err := s.DB.Exec(`UPDATE playback_subtitle_sessions
		SET state = ?, revoked_at = ?, version = version + 1, updated_at = ?
		WHERE transport = ? AND state = ?`,
		models.PlaybackSubtitleSessionRevoked, now, now, transport,
		models.PlaybackSubtitleSessionActive)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RevokePlaybackSubtitleSessionsForTicket revokes active subtitle capabilities
// created for one exact ticket and transport. It is used when issuing a
// transport fails after one or more subtitle grants were already committed.
func (s *Store) RevokePlaybackSubtitleSessionsForTicket(
	binding models.PlaybackTicketBinding,
	transport string,
	now time.Time,
) (int64, error) {
	if err := validatePlaybackTicketBinding(binding); err != nil {
		return 0, err
	}
	if !validPlaybackTransport(transport) {
		return 0, ErrInvalidPlaybackTransport
	}
	now = playbackTime(now)
	res, err := s.DB.Exec(`UPDATE playback_subtitle_sessions
		SET state = ?, revoked_at = ?, version = version + 1, updated_at = ?
		WHERE ticket_id = ? AND user_id = ? AND device_id = ? AND file_id = ?
			AND transport = ? AND state = ?`,
		models.PlaybackSubtitleSessionRevoked, now, now, binding.TicketID,
		binding.UserID, binding.DeviceID, binding.FileID, transport,
		models.PlaybackSubtitleSessionActive)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RevokeAllPlaybackSubtitleSessions is the global playback emergency stop.
func (s *Store) RevokeAllPlaybackSubtitleSessions(now time.Time) (int64, error) {
	now = playbackTime(now)
	res, err := s.DB.Exec(`UPDATE playback_subtitle_sessions
		SET state = ?, revoked_at = ?, version = version + 1, updated_at = ?
		WHERE state = ?`, models.PlaybackSubtitleSessionRevoked, now, now,
		models.PlaybackSubtitleSessionActive)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func normalizePlaybackSubtitleSessionRequest(
	req models.PlaybackSubtitleSessionRequest,
) (models.PlaybackSubtitleSessionRequest, error) {
	if err := validatePlaybackTicketBinding(req.Ticket); err != nil {
		return req, ErrInvalidPlaybackSubtitleSession
	}
	if !validPlaybackTransport(req.Transport) || req.SubtitleID <= 0 ||
		req.StartSeconds < 0 || math.IsNaN(req.StartSeconds) ||
		math.IsInf(req.StartSeconds, 0) {
		return req, ErrInvalidPlaybackSubtitleSession
	}
	return req, nil
}

func normalizePlaybackSubtitleIDs(ids []int64) ([]int64, error) {
	if len(ids) > models.PlaybackMaxSubtitleGrants {
		return nil, ErrInvalidPlaybackTicket
	}
	seen := make(map[int64]struct{}, len(ids))
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			return nil, ErrInvalidPlaybackTicket
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

func validatePlaybackSubtitleGrantsTx(
	tx *sql.Tx,
	fileID int64,
	ids []int64,
) error {
	return validatePlaybackSubtitleGrantsTxContext(
		context.Background(), tx, fileID, ids)
}

func validatePlaybackSubtitleGrantsTxContext(
	ctx context.Context,
	tx *sql.Tx,
	fileID int64,
	ids []int64,
) error {
	if len(ids) == 0 {
		return nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, 0, len(ids)+1)
	args = append(args, fileID)
	for i, id := range ids {
		placeholders[i] = "?"
		args = append(args, id)
	}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM subtitles
		WHERE file_id = ? AND id IN (`+strings.Join(placeholders, ",")+`)`,
		args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	found := make(map[int64]struct{}, len(ids))
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		found[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if _, ok := found[id]; !ok {
			return ErrInvalidPlaybackTicket
		}
	}
	return nil
}

func playbackTicketGrantsSubtitle(
	ticket *models.PlaybackTicket,
	subtitleID int64,
) bool {
	if ticket == nil {
		return false
	}
	for _, id := range ticket.SubtitleIDs {
		if id == subtitleID {
			return true
		}
	}
	return false
}

func scanPlaybackSubtitleSession(
	row interface{ Scan(...any) error },
) (*models.PlaybackSubtitleSession, error) {
	var (
		session    models.PlaybackSubtitleSession
		lastUsedAt sql.NullTime
		revokedAt  sql.NullTime
	)
	if err := row.Scan(&session.ID, &session.TicketID, &session.UserID,
		&session.DeviceID, &session.FileID, &session.Purpose,
		&session.Transport, &session.SubtitleID, &session.State,
		&session.StartSeconds, &session.Version, &session.CreatedAt,
		&lastUsedAt, &session.IdleExpiresAt, &session.AbsoluteExpiresAt,
		&session.UpdatedAt, &revokedAt); err != nil {
		return nil, err
	}
	if lastUsedAt.Valid {
		session.LastUsedAt = timePointer(lastUsedAt.Time)
	}
	if revokedAt.Valid {
		session.RevokedAt = timePointer(revokedAt.Time)
	}
	return &session, nil
}

// Acquire the writer reservation before the capability lookup so concurrent
// authorizations serialize with each other and with ticket/device revocation.
// The version assignment is deliberately a no-op; the real CAS follows.
func lockPlaybackSubtitleSessionTx(tx *sql.Tx, capability string) error {
	_, err := tx.Exec(`UPDATE playback_subtitle_sessions SET version = version
		WHERE capability_hash = ?`, hashPlaybackCapability(capability))
	return err
}

func decodePlaybackSubtitleIDs(raw string) ([]int64, error) {
	var ids []int64
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		return nil, fmt.Errorf("decode playback subtitle grants: %w", err)
	}
	return normalizePlaybackSubtitleIDs(ids)
}
