package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"doriyamedia/internal/auth"
	"doriyamedia/internal/models"
)

var (
	ErrDeviceAuthorizationNotFound       = errors.New("device authorization not found")
	ErrDeviceAuthorizationPending        = errors.New("device authorization is not approved")
	ErrDeviceAuthorizationExpired        = errors.New("device authorization expired")
	ErrDeviceAuthorizationDenied         = errors.New("device authorization denied")
	ErrDeviceAuthorizationConsumed       = errors.New("device authorization already consumed")
	ErrDeviceAuthorizationAlreadyDecided = errors.New("device authorization is no longer pending")
	ErrDeviceAuthorizationCSRF           = errors.New("device authorization CSRF token is invalid")
	ErrInvalidDeviceCode                 = errors.New("invalid device code")
	ErrInvalidDeviceUserCode             = errors.New("invalid device user code")
	ErrInvalidDeviceMetadata             = errors.New("invalid device metadata")
	ErrInvalidDevicePKCEChallenge        = errors.New("invalid device PKCE challenge")
	ErrInvalidDevicePKCEVerifier         = errors.New("invalid device PKCE verifier")
	ErrDeviceUserUnavailable             = errors.New("device user is not active")
	ErrInvalidDeviceAccessToken          = errors.New("invalid device access token")
	ErrDeviceAccessTokenExpired          = errors.New("device access token expired")
	ErrDeviceAccessTokenRevoked          = errors.New("device access token revoked")
	ErrInvalidDeviceRefreshToken         = errors.New("invalid device refresh token")
	ErrDeviceRefreshTokenExpired         = errors.New("device refresh token expired")
	ErrDeviceRefreshTokenRevoked         = errors.New("device refresh token revoked")
	ErrDeviceRefreshTokenReused          = errors.New("device refresh token reuse detected")
	ErrDeviceNotFound                    = errors.New("device not found")
	ErrDeviceRevoked                     = errors.New("device revoked")
)

// Device-auth tables are part of the normal startup migration. Bearer values
// never appear here: only SHA-256 hashes do. Users and devices cascade so a
// deleted account cannot leave usable credentials behind.
var deviceAuthMigrationStatements = []string{
	`CREATE TABLE IF NOT EXISTS device_authorizations (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		authorization_id TEXT NOT NULL,
		device_code_hash TEXT NOT NULL,
		user_code_hash TEXT NOT NULL,
		device_name TEXT NOT NULL,
		platform TEXT NOT NULL,
		client_version TEXT NOT NULL,
		code_challenge TEXT NOT NULL,
		code_challenge_method TEXT NOT NULL CHECK(code_challenge_method = 'S256'),
		state TEXT NOT NULL DEFAULT 'pending'
			CHECK(state IN ('pending','authorized','denied','consumed','expired')),
		user_id INTEGER REFERENCES users(id) ON DELETE CASCADE,
		auth_epoch INTEGER NOT NULL DEFAULT 0,
		csrf_token_hash TEXT NOT NULL DEFAULT '',
		csrf_user_id INTEGER REFERENCES users(id) ON DELETE CASCADE,
		csrf_expires_at TIMESTAMP,
		csrf_consumed_at TIMESTAMP,
		created_at TIMESTAMP NOT NULL,
		expires_at TIMESTAMP NOT NULL,
		updated_at TIMESTAMP NOT NULL,
		authorized_at TIMESTAMP,
		consumed_at TIMESTAMP
	)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_device_authorizations_device_code
		ON device_authorizations(device_code_hash)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_device_authorizations_user_code
		ON device_authorizations(user_code_hash)`,
	`CREATE INDEX IF NOT EXISTS idx_device_authorizations_state_expiry
		ON device_authorizations(state, expires_at)`,
	`CREATE INDEX IF NOT EXISTS idx_device_authorizations_user
		ON device_authorizations(user_id, state)`,
	`CREATE TABLE IF NOT EXISTS devices (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		public_id TEXT NOT NULL,
		user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		name TEXT NOT NULL,
		platform TEXT NOT NULL,
		client_version TEXT NOT NULL,
		authorization_id INTEGER REFERENCES device_authorizations(id) ON DELETE SET NULL,
		created_at TIMESTAMP NOT NULL,
		last_seen_at TIMESTAMP NOT NULL,
		revoked_at TIMESTAMP
	)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_devices_public_id ON devices(public_id)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_devices_authorization
		ON devices(authorization_id) WHERE authorization_id IS NOT NULL`,
	`CREATE INDEX IF NOT EXISTS idx_devices_user ON devices(user_id, revoked_at, created_at)`,
	`CREATE TABLE IF NOT EXISTS device_access_tokens (
		token_hash TEXT PRIMARY KEY,
		user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		device_id INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
		family_id TEXT NOT NULL,
		auth_epoch INTEGER NOT NULL DEFAULT 0,
		expires_at TIMESTAMP NOT NULL,
		created_at TIMESTAMP NOT NULL,
		last_used_at TIMESTAMP,
		revoked_at TIMESTAMP,
		revoked_reason TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE INDEX IF NOT EXISTS idx_device_access_tokens_device
		ON device_access_tokens(device_id, revoked_at)`,
	`CREATE INDEX IF NOT EXISTS idx_device_access_tokens_user
		ON device_access_tokens(user_id, revoked_at)`,
	`CREATE INDEX IF NOT EXISTS idx_device_access_tokens_family
		ON device_access_tokens(family_id)`,
	`CREATE TABLE IF NOT EXISTS device_refresh_tokens (
		token_hash TEXT PRIMARY KEY,
		user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		device_id INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
		family_id TEXT NOT NULL,
		auth_epoch INTEGER NOT NULL DEFAULT 0,
		expires_at TIMESTAMP NOT NULL,
		family_absolute_expires_at TIMESTAMP NOT NULL,
		created_at TIMESTAMP NOT NULL,
		rotated_at TIMESTAMP,
		revoked_at TIMESTAMP,
		replaced_by TEXT NOT NULL DEFAULT '',
		revoked_reason TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE INDEX IF NOT EXISTS idx_device_refresh_tokens_device
		ON device_refresh_tokens(device_id, revoked_at)`,
	`CREATE INDEX IF NOT EXISTS idx_device_refresh_tokens_user
		ON device_refresh_tokens(user_id, revoked_at)`,
	`CREATE INDEX IF NOT EXISTS idx_device_refresh_tokens_family
		ON device_refresh_tokens(family_id)`,
}

// migrateDeviceAuthorizationPublicID upgrades databases created before the
// public dva_ id existed. SQLite cannot add a UNIQUE NOT NULL column in place,
// so backfill first and then add the unique index.
func (s *Store) migrateDeviceAuthorizationPublicID() error {
	hasPublicID, err := s.hasColumn("device_authorizations", "authorization_id")
	if err != nil {
		return err
	}
	if !hasPublicID {
		if _, err := s.DB.Exec(`ALTER TABLE device_authorizations
			ADD COLUMN authorization_id TEXT`); err != nil {
			return fmt.Errorf("migrate device authorizations: %w", err)
		}
	}

	rows, err := s.DB.Query(`SELECT id FROM device_authorizations
		WHERE authorization_id IS NULL OR authorization_id = ''`)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		publicID, err := auth.NewDeviceAuthorizationID()
		if err != nil {
			return err
		}
		if _, err := s.DB.Exec(`UPDATE device_authorizations
			SET authorization_id = ? WHERE id = ?`, publicID, id); err != nil {
			return err
		}
	}
	if _, err := s.DB.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS
		idx_device_authorizations_public_id
		ON device_authorizations(authorization_id)`); err != nil {
		return err
	}
	return nil
}

// MigrateDeviceAuthSecurityColumns upgrades device-auth tables created before
// one-time approval CSRF and the refresh-family absolute deadline existed.
// Startup migration should call this after deviceAuthMigrationStatements.
func (s *Store) MigrateDeviceAuthSecurityColumns() error {
	for _, column := range []struct {
		table string
		name  string
		ddl   string
	}{
		{
			table: "device_authorizations",
			name:  "auth_epoch",
			ddl: `ALTER TABLE device_authorizations
				ADD COLUMN auth_epoch INTEGER NOT NULL DEFAULT 0`,
		},
		{
			table: "device_authorizations",
			name:  "csrf_token_hash",
			ddl: `ALTER TABLE device_authorizations
				ADD COLUMN csrf_token_hash TEXT NOT NULL DEFAULT ''`,
		},
		{
			table: "device_authorizations",
			name:  "csrf_user_id",
			ddl: `ALTER TABLE device_authorizations
				ADD COLUMN csrf_user_id INTEGER REFERENCES users(id) ON DELETE CASCADE`,
		},
		{
			table: "device_authorizations",
			name:  "csrf_expires_at",
			ddl:   `ALTER TABLE device_authorizations ADD COLUMN csrf_expires_at TIMESTAMP`,
		},
		{
			table: "device_authorizations",
			name:  "csrf_consumed_at",
			ddl:   `ALTER TABLE device_authorizations ADD COLUMN csrf_consumed_at TIMESTAMP`,
		},
		{
			table: "device_refresh_tokens",
			name:  "family_absolute_expires_at",
			ddl:   `ALTER TABLE device_refresh_tokens ADD COLUMN family_absolute_expires_at TIMESTAMP`,
		},
	} {
		if err := s.addColumn(column.table, column.name, column.ddl); err != nil {
			return fmt.Errorf("migrate device auth security: %w", err)
		}
	}

	// Existing families inherit one absolute deadline from their earliest
	// refresh row. Collect first because SQLite cannot reliably keep a read
	// cursor open while the same connection performs writes.
	rows, err := s.DB.Query(`SELECT family_id, created_at
		FROM device_refresh_tokens
		WHERE family_absolute_expires_at IS NULL
		ORDER BY family_id, created_at, rowid`)
	if err != nil {
		return fmt.Errorf("read refresh families for migration: %w", err)
	}
	deadlines := make(map[string]time.Time)
	for rows.Next() {
		var (
			familyID string
			created  time.Time
		)
		if err := rows.Scan(&familyID, &created); err != nil {
			rows.Close()
			return fmt.Errorf("scan refresh family for migration: %w", err)
		}
		if _, seen := deadlines[familyID]; !seen {
			deadlines[familyID] = created.UTC().Add(auth.DeviceRefreshTokenFamilyTTL)
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for familyID, deadline := range deadlines {
		if _, err := s.DB.Exec(`UPDATE device_refresh_tokens
			SET family_absolute_expires_at = ?
			WHERE family_id = ? AND family_absolute_expires_at IS NULL`,
			deadline, familyID); err != nil {
			return fmt.Errorf("backfill refresh family deadline: %w", err)
		}
	}
	return nil
}

// CreateDeviceAuthorization starts a browser-approved login. The returned
// plaintext device_code and user_code are the only copies; the database gets
// their hashes and the client's S256 PKCE challenge.
func (s *Store) CreateDeviceAuthorization(req models.DeviceAuthorizationRequest, now time.Time) (*models.DeviceAuthorizationGrant, error) {
	now = deviceAuthTime(now)
	req.DeviceName = strings.TrimSpace(req.DeviceName)
	req.Platform = strings.TrimSpace(req.Platform)
	req.ClientVersion = strings.TrimSpace(req.ClientVersion)
	req.CodeChallenge = strings.TrimSpace(req.CodeChallenge)
	req.CodeChallengeMethod = strings.TrimSpace(req.CodeChallengeMethod)
	if req.DeviceName == "" || req.Platform == "" || req.ClientVersion == "" {
		return nil, ErrInvalidDeviceMetadata
	}
	if req.CodeChallengeMethod != auth.PKCEMethodS256 || !auth.ValidatePKCEChallenge(req.CodeChallenge) {
		return nil, ErrInvalidDevicePKCEChallenge
	}

	authorizationID, err := auth.NewDeviceAuthorizationID()
	if err != nil {
		return nil, err
	}
	deviceCode, err := auth.NewDeviceCode()
	if err != nil {
		return nil, err
	}
	userCode, err := auth.NewDeviceUserCode()
	if err != nil {
		return nil, err
	}
	expiresAt := now.Add(auth.DeviceAuthorizationTTL)
	res, err := s.DB.Exec(`INSERT INTO device_authorizations
		(authorization_id, device_code_hash, user_code_hash, device_name, platform,
		 client_version, code_challenge, code_challenge_method, state,
		 created_at, expires_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		authorizationID, auth.HashDeviceCredential(deviceCode),
		auth.HashDeviceCredential(userCode), req.DeviceName, req.Platform,
		req.ClientVersion, req.CodeChallenge, auth.PKCEMethodS256,
		models.DeviceAuthorizationPending, now, expiresAt, now)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	authorization := &models.DeviceAuthorization{
		ID: id, AuthorizationID: authorizationID, DeviceName: req.DeviceName,
		Platform: req.Platform, ClientVersion: req.ClientVersion,
		State: models.DeviceAuthorizationPending, CreatedAt: now,
		ExpiresAt: expiresAt, UpdatedAt: now,
	}
	return &models.DeviceAuthorizationGrant{
		Authorization: authorization,
		DeviceCode:    deviceCode,
		UserCode:      userCode,
		ExpiresAt:     expiresAt,
	}, nil
}

// DeviceAuthorizationByUserCode resolves the short code shown in the browser.
// The returned record contains no device_code or hash, and consumed/denied
// authorizations are reported as errors rather than displayed again.
func (s *Store) DeviceAuthorizationByUserCode(userCode string, now time.Time) (*models.DeviceAuthorization, error) {
	now = deviceAuthTime(now)
	normalized, err := normalizeDeviceUserCode(userCode)
	if err != nil {
		return nil, err
	}
	authorization, err := scanDeviceAuthorization(s.DB.QueryRow(`SELECT
		id, authorization_id, device_name, platform, client_version, state, user_id,
		created_at, expires_at, updated_at, authorized_at, consumed_at
		FROM device_authorizations WHERE user_code_hash = ?`,
		auth.HashDeviceCredential(normalized)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrDeviceAuthorizationNotFound
	}
	if err != nil {
		return nil, err
	}

	switch authorization.State {
	case models.DeviceAuthorizationPending, models.DeviceAuthorizationAuthorized:
		if !now.Before(authorization.ExpiresAt) {
			if err := s.markDeviceAuthorizationExpired(authorization.ID, now); err != nil {
				return nil, err
			}
			return nil, ErrDeviceAuthorizationExpired
		}
		return authorization, nil
	case models.DeviceAuthorizationDenied:
		return nil, ErrDeviceAuthorizationDenied
	case models.DeviceAuthorizationConsumed:
		return nil, ErrDeviceAuthorizationConsumed
	case models.DeviceAuthorizationExpired:
		return nil, ErrDeviceAuthorizationExpired
	default:
		return nil, fmt.Errorf("device authorization %d has unknown state %q", authorization.ID, authorization.State)
	}
}

// DeviceAuthorizationByID returns the public status of an authorization by its
// high-entropy dva_ id. Unlike a bearer credential this value is safe for the
// browser URL and status endpoint; the response never contains device_code or
// either code hash. Expired timestamps are reflected as the expired state.
func (s *Store) DeviceAuthorizationByID(authorizationID string, now time.Time) (*models.DeviceAuthorization, error) {
	now = deviceAuthTime(now)
	authorizationID = strings.TrimSpace(authorizationID)
	if authorizationID == "" {
		return nil, ErrDeviceAuthorizationNotFound
	}
	authorization, err := scanDeviceAuthorization(s.DB.QueryRow(`SELECT
		id, authorization_id, device_name, platform, client_version, state, user_id,
		created_at, expires_at, updated_at, authorized_at, consumed_at
		FROM device_authorizations WHERE authorization_id = ?`, authorizationID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrDeviceAuthorizationNotFound
	}
	if err != nil {
		return nil, err
	}
	switch authorization.State {
	case models.DeviceAuthorizationPending, models.DeviceAuthorizationAuthorized:
		if !now.Before(authorization.ExpiresAt) {
			if err := s.markDeviceAuthorizationExpired(authorization.ID, now); err != nil {
				return nil, err
			}
			authorization.State = models.DeviceAuthorizationExpired
			authorization.UpdatedAt = now
		}
		return authorization, nil
	case models.DeviceAuthorizationDenied, models.DeviceAuthorizationConsumed, models.DeviceAuthorizationExpired:
		return authorization, nil
	default:
		return nil, fmt.Errorf("device authorization %d has unknown state %q", authorization.ID, authorization.State)
	}
}

// IssueDeviceAuthorizationCSRF rotates the one-time approval capability for a
// pending authorization. The returned token is plaintext only here; the DB
// stores its hash and binds it to the Web-session user that will make the
// decision.
func (s *Store) IssueDeviceAuthorizationCSRF(authorizationID string, userID int64, now time.Time) (*models.DeviceAuthorizationCSRF, error) {
	now = deviceAuthTime(now)
	authorizationID = strings.TrimSpace(authorizationID)
	if authorizationID == "" || userID <= 0 {
		return nil, ErrDeviceAuthorizationNotFound
	}
	token, err := auth.NewDeviceAuthorizationCSRFToken()
	if err != nil {
		return nil, err
	}

	tx, err := s.DB.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var (
		id        int64
		state     string
		expiresAt time.Time
	)
	err = tx.QueryRow(`SELECT id, state, expires_at
		FROM device_authorizations WHERE authorization_id = ?`, authorizationID).
		Scan(&id, &state, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrDeviceAuthorizationNotFound
	}
	if err != nil {
		return nil, err
	}
	if !now.Before(expiresAt) {
		if state == models.DeviceAuthorizationPending || state == models.DeviceAuthorizationAuthorized {
			if _, err := tx.Exec(`UPDATE device_authorizations
				SET state = ?, updated_at = ? WHERE id = ? AND state IN (?,?)`,
				models.DeviceAuthorizationExpired, now, id,
				models.DeviceAuthorizationPending, models.DeviceAuthorizationAuthorized); err != nil {
				return nil, err
			}
			if err := tx.Commit(); err != nil {
				return nil, err
			}
		}
		return nil, ErrDeviceAuthorizationExpired
	}
	if state != models.DeviceAuthorizationPending {
		return nil, ErrDeviceAuthorizationAlreadyDecided
	}

	csrfExpiresAt := now.Add(auth.DeviceAuthorizationCSRFTTL)
	if expiresAt.Before(csrfExpiresAt) {
		csrfExpiresAt = expiresAt
	}
	res, err := tx.Exec(`UPDATE device_authorizations
		SET csrf_token_hash = ?, csrf_user_id = ?, csrf_expires_at = ?,
			csrf_consumed_at = NULL
		WHERE id = ? AND state = ? AND expires_at > ?`,
		auth.HashDeviceCredential(token), userID, csrfExpiresAt, id,
		models.DeviceAuthorizationPending, now)
	if err != nil {
		return nil, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if affected != 1 {
		return nil, ErrDeviceAuthorizationAlreadyDecided
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &models.DeviceAuthorizationCSRF{Token: token, ExpiresAt: csrfExpiresAt}, nil
}

// ApproveDeviceAuthorization binds an active user to a pending request.
func (s *Store) ApproveDeviceAuthorization(userCode string, userID int64, now time.Time) (*models.DeviceAuthorization, error) {
	normalized, err := normalizeDeviceUserCode(userCode)
	if err != nil {
		return nil, err
	}
	return s.decideDeviceAuthorization("user_code_hash",
		auth.HashDeviceCredential(normalized), userID, true, now, "")
}

// DenyDeviceAuthorization records an explicit rejection, consuming the user
// code so the client cannot later exchange it.
func (s *Store) DenyDeviceAuthorization(userCode string, userID int64, now time.Time) (*models.DeviceAuthorization, error) {
	normalized, err := normalizeDeviceUserCode(userCode)
	if err != nil {
		return nil, err
	}
	return s.decideDeviceAuthorization("user_code_hash",
		auth.HashDeviceCredential(normalized), userID, false, now, "")
}

// ApproveDeviceAuthorizationByID is the browser-page path: the authenticated
// user approves the public authorization record shown in the URL.
func (s *Store) ApproveDeviceAuthorizationByID(authorizationID string, userID int64, now time.Time) (*models.DeviceAuthorization, error) {
	authorizationID = strings.TrimSpace(authorizationID)
	if authorizationID == "" {
		return nil, ErrDeviceAuthorizationNotFound
	}
	return s.decideDeviceAuthorization("authorization_id", authorizationID, userID, true, now, "")
}

// DenyDeviceAuthorizationByID records a browser-page rejection.
func (s *Store) DenyDeviceAuthorizationByID(authorizationID string, userID int64, now time.Time) (*models.DeviceAuthorization, error) {
	authorizationID = strings.TrimSpace(authorizationID)
	if authorizationID == "" {
		return nil, ErrDeviceAuthorizationNotFound
	}
	return s.decideDeviceAuthorization("authorization_id", authorizationID, userID, false, now, "")
}

// DecideDeviceAuthorizationByIDWithCSRF is the browser decision path. It
// consumes the approval capability and changes the authorization in one
// transaction; a repeated or cross-user token cannot decide anything.
func (s *Store) DecideDeviceAuthorizationByIDWithCSRF(authorizationID string, userID int64, csrfToken string, approve bool, now time.Time) (*models.DeviceAuthorization, error) {
	authorizationID = strings.TrimSpace(authorizationID)
	csrfToken = strings.TrimSpace(csrfToken)
	if authorizationID == "" {
		return nil, ErrDeviceAuthorizationNotFound
	}
	if csrfToken == "" {
		return nil, ErrDeviceAuthorizationCSRF
	}
	return s.decideDeviceAuthorization("authorization_id", authorizationID, userID, approve, now, csrfToken)
}

func (s *Store) decideDeviceAuthorization(lookupColumn string, lookupValue any, userID int64, approve bool, now time.Time, csrfToken string) (*models.DeviceAuthorization, error) {
	now = deviceAuthTime(now)
	tx, err := s.DB.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// Take the writer reservation before reading the account epoch. Approval
	// and a concurrent password/reset/revoke therefore serialize; either the
	// approval sees the new epoch or it is fenced out by the next mutation.
	res, err := tx.Exec(`UPDATE device_authorizations SET updated_at = updated_at
		WHERE `+lookupColumn+` = ?`, lookupValue)
	if err != nil {
		return nil, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if affected != 1 {
		return nil, ErrDeviceAuthorizationNotFound
	}

	authorization, err := scanDeviceAuthorization(tx.QueryRow(`SELECT
		id, authorization_id, device_name, platform, client_version, state, user_id,
		created_at, expires_at, updated_at, authorized_at, consumed_at
		FROM device_authorizations WHERE `+lookupColumn+` = ?`, lookupValue))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrDeviceAuthorizationNotFound
	}
	if err != nil {
		return nil, err
	}
	if !now.Before(authorization.ExpiresAt) {
		if authorization.State == models.DeviceAuthorizationPending ||
			authorization.State == models.DeviceAuthorizationAuthorized {
			if _, err := tx.Exec(`UPDATE device_authorizations
				SET state = ?, updated_at = ? WHERE id = ? AND state IN (?,?)`,
				models.DeviceAuthorizationExpired, now, authorization.ID,
				models.DeviceAuthorizationPending, models.DeviceAuthorizationAuthorized); err != nil {
				return nil, err
			}
			if err := tx.Commit(); err != nil {
				return nil, err
			}
		}
		return nil, ErrDeviceAuthorizationExpired
	}
	if authorization.State != models.DeviceAuthorizationPending {
		return nil, ErrDeviceAuthorizationAlreadyDecided
	}

	var (
		status    string
		authEpoch int64
	)
	if err := tx.QueryRow(`SELECT status, auth_epoch FROM users WHERE id = ?`, userID).
		Scan(&status, &authEpoch); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrDeviceUserUnavailable
		}
		return nil, err
	}
	if status != models.StatusActive {
		return nil, ErrDeviceUserUnavailable
	}

	if csrfToken != "" {
		res, err := tx.Exec(`UPDATE device_authorizations
			SET csrf_consumed_at = ?
			WHERE id = ? AND state = ? AND csrf_user_id = ?
				AND csrf_token_hash = ? AND csrf_expires_at > ?
				AND csrf_consumed_at IS NULL`,
			now, authorization.ID, models.DeviceAuthorizationPending, userID,
			auth.HashDeviceCredential(csrfToken), now)
		if err != nil {
			return nil, err
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return nil, err
		}
		if affected != 1 {
			return nil, ErrDeviceAuthorizationCSRF
		}
	}

	nextState := models.DeviceAuthorizationDenied
	if approve {
		nextState = models.DeviceAuthorizationAuthorized
	}
	var authorizedAt any
	authorizationEpoch := int64(0)
	if approve {
		authorizedAt = now
		authorizationEpoch = authEpoch
	}
	res, err = tx.Exec(`UPDATE device_authorizations
		SET state = ?, user_id = ?, authorized_at = ?, auth_epoch = ?, updated_at = ?
		WHERE id = ? AND state = ? AND expires_at > ?`,
		nextState, userID, authorizedAt, authorizationEpoch, now, authorization.ID,
		models.DeviceAuthorizationPending, now)
	if err != nil {
		return nil, err
	}
	affected, err = res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if affected != 1 {
		return nil, ErrDeviceAuthorizationAlreadyDecided
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	authorization.State = nextState
	authorization.UserID = &userID
	authorization.UpdatedAt = now
	if approve {
		authorized := now
		authorization.AuthorizedAt = &authorized
	}
	return authorization, nil
}

// ConsumeDeviceAuthorization exchanges an approved authorization for one
// device plus an access/refresh pair. The state transition, device insert and
// both token inserts share one transaction, so a request either produces a
// complete session or no rows at all.
func (s *Store) ConsumeDeviceAuthorization(deviceCode, codeVerifier string, now time.Time) (*models.DeviceTokenPair, error) {
	return s.ConsumeDeviceAuthorizationWithEmailPolicy(deviceCode, codeVerifier, false, now)
}

// ConsumeDeviceAuthorizationWithEmailPolicy applies the caller's email
// verification policy inside the issuance transaction, before a device or any
// token row exists.
func (s *Store) ConsumeDeviceAuthorizationWithEmailPolicy(deviceCode, codeVerifier string, requireEmailVerification bool, now time.Time) (*models.DeviceTokenPair, error) {
	now = deviceAuthTime(now)
	tx, err := s.DB.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	deviceCodeHash := auth.HashDeviceCredential(deviceCode)
	res, err := tx.Exec(`UPDATE device_authorizations SET updated_at = updated_at
		WHERE device_code_hash = ?`, deviceCodeHash)
	if err != nil {
		return nil, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if affected != 1 {
		return nil, ErrInvalidDeviceCode
	}

	var (
		authorizationID int64
		deviceName      string
		platform        string
		clientVersion   string
		challenge       string
		challengeMethod string
		state           string
		userID          sql.NullInt64
		authEpoch       int64
		expiresAt       time.Time
	)
	err = tx.QueryRow(`SELECT id, device_name, platform, client_version,
		code_challenge, code_challenge_method, state, user_id, auth_epoch, expires_at
		FROM device_authorizations WHERE device_code_hash = ?`,
		deviceCodeHash).
		Scan(&authorizationID, &deviceName, &platform, &clientVersion,
			&challenge, &challengeMethod, &state, &userID, &authEpoch, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrInvalidDeviceCode
	}
	if err != nil {
		return nil, err
	}

	if state == models.DeviceAuthorizationConsumed {
		return nil, ErrDeviceAuthorizationConsumed
	}
	if state == models.DeviceAuthorizationDenied {
		return nil, ErrDeviceAuthorizationDenied
	}
	if state == models.DeviceAuthorizationExpired || !now.Before(expiresAt) {
		if state == models.DeviceAuthorizationPending || state == models.DeviceAuthorizationAuthorized {
			if _, err := tx.Exec(`UPDATE device_authorizations
				SET state = ?, updated_at = ? WHERE id = ? AND state IN (?,?)`,
				models.DeviceAuthorizationExpired, now, authorizationID,
				models.DeviceAuthorizationPending, models.DeviceAuthorizationAuthorized); err != nil {
				return nil, err
			}
			if err := tx.Commit(); err != nil {
				return nil, err
			}
		}
		return nil, ErrDeviceAuthorizationExpired
	}
	if state != models.DeviceAuthorizationAuthorized || !userID.Valid {
		return nil, ErrDeviceAuthorizationPending
	}
	if challengeMethod != auth.PKCEMethodS256 || !auth.VerifyPKCE(codeVerifier, challenge) {
		return nil, ErrInvalidDevicePKCEVerifier
	}

	var (
		status        string
		email         string
		emailVerified int
		currentEpoch  int64
	)
	if err := tx.QueryRow(`SELECT status, email, email_verified, auth_epoch
		FROM users WHERE id = ?`, userID.Int64).
		Scan(&status, &email, &emailVerified, &currentEpoch); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrDeviceUserUnavailable
		}
		return nil, err
	}
	if authEpoch < 1 || authEpoch != currentEpoch {
		return nil, ErrInvalidDeviceCode
	}
	if !deviceAuthUserAllowed(status, email, emailVerified != 0, requireEmailVerification) {
		return nil, ErrDeviceUserUnavailable
	}

	res, err = tx.Exec(`UPDATE device_authorizations
		SET state = ?, consumed_at = ?, updated_at = ?
		WHERE id = ? AND state = ? AND auth_epoch = ? AND expires_at > ?`,
		models.DeviceAuthorizationConsumed, now, now, authorizationID,
		models.DeviceAuthorizationAuthorized, authEpoch, now)
	if err != nil {
		return nil, err
	}
	affected, err = res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if affected != 1 {
		return nil, ErrDeviceAuthorizationConsumed
	}

	device, err := createDeviceTx(tx, userID.Int64, deviceName, platform, clientVersion, authorizationID, now)
	if err != nil {
		return nil, err
	}
	pair, err := createDeviceTokenPairTx(tx, userID.Int64, device, now)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return pair, nil
}

// DeviceByAccessToken resolves an access-token hash and checks token expiry,
// revocation, device revocation and account status. The raw token is never
// scanned for in the database.
func (s *Store) DeviceByAccessToken(accessToken string, now time.Time) (*models.DeviceAuthContext, error) {
	return s.DeviceByAccessTokenWithEmailPolicy(accessToken, false, now)
}

// DeviceByAccessTokenWithEmailPolicy resolves and validates an access token
// while also enforcing the current account email policy.
func (s *Store) DeviceByAccessTokenWithEmailPolicy(accessToken string, requireEmailVerification bool, now time.Time) (*models.DeviceAuthContext, error) {
	now = deviceAuthTime(now)
	if accessToken == "" {
		return nil, ErrInvalidDeviceAccessToken
	}
	row := s.DB.QueryRow(`SELECT
		u.id, u.name, u.pass_hash, u.salt, u.auth_epoch, u.role, u.status, u.email,
		u.email_verified, u.created_at, u.last_login_at,
		d.id, d.public_id, d.user_id, d.name, d.platform, d.client_version,
		d.created_at, d.last_seen_at, d.revoked_at,
		t.auth_epoch, t.expires_at, t.revoked_at,
		COALESCE(o.overrides_json, '{}')
		FROM device_access_tokens t
		JOIN users u ON u.id = t.user_id
		JOIN devices d ON d.id = t.device_id
		LEFT JOIN device_feature_overrides o ON o.device_id = d.id
		WHERE t.token_hash = ?`, auth.HashDeviceCredential(accessToken))

	var (
		user          models.User
		userAuthEpoch int64
		emailVerified int
		lastLogin     sql.NullTime
		device        models.Device
		deviceRevoked sql.NullTime
		tokenEpoch    int64
		tokenExpires  time.Time
		tokenRevoked  sql.NullTime
		overridesRaw  string
	)
	err := row.Scan(&user.ID, &user.Name, &user.PassHash, &user.Salt, &userAuthEpoch, &user.Role,
		&user.Status, &user.Email, &emailVerified, &user.CreatedAt, &lastLogin,
		&device.ID, &device.PublicID, &device.UserID, &device.Name, &device.Platform,
		&device.ClientVersion, &device.CreatedAt, &device.LastSeenAt, &deviceRevoked,
		&tokenEpoch, &tokenExpires, &tokenRevoked, &overridesRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrInvalidDeviceAccessToken
	}
	if err != nil {
		return nil, err
	}
	overrides, err := decodeDeviceFeatureOverrides(overridesRaw)
	if err != nil {
		return nil, err
	}
	if !deviceAuthUserAllowed(user.Status, user.Email, emailVerified != 0, requireEmailVerification) {
		return nil, ErrDeviceUserUnavailable
	}
	if overrides.DisablesDeviceAuth() {
		if overrides.ClientV1Disabled {
			return nil, ErrDeviceV1Disabled
		}
		return nil, ErrDeviceAuthFeatureDisabled
	}
	if deviceRevoked.Valid {
		return nil, ErrDeviceRevoked
	}
	if tokenEpoch != userAuthEpoch {
		return nil, ErrDeviceRevoked
	}
	if tokenRevoked.Valid {
		return nil, ErrDeviceAccessTokenRevoked
	}
	if !now.Before(tokenExpires) {
		return nil, ErrDeviceAccessTokenExpired
	}

	user.EmailVerified = emailVerified != 0
	if lastLogin.Valid {
		t := lastLogin.Time
		user.LastLogin = &t
	}
	device.LastSeenAt = now
	if _, err := s.DB.Exec(`UPDATE devices SET last_seen_at = ?
		WHERE id = ? AND revoked_at IS NULL`, now, device.ID); err != nil {
		return nil, err
	}
	if _, err := s.DB.Exec(`UPDATE device_access_tokens SET last_used_at = ?
		WHERE token_hash = ? AND revoked_at IS NULL`, now, auth.HashDeviceCredential(accessToken)); err != nil {
		return nil, err
	}
	return &models.DeviceAuthContext{
		User: &user, Device: &device, Overrides: overrides,
		AccessExpiresAt: tokenExpires,
	}, nil
}

// RotateDeviceRefreshToken atomically consumes one refresh token and issues a
// new pair in the same family. Reusing an already-rotated token revokes that
// family and fails, including its current access and refresh tokens.
func (s *Store) RotateDeviceRefreshToken(refreshToken string, now time.Time) (*models.DeviceTokenPair, error) {
	return s.RotateDeviceRefreshTokenWithEmailPolicy(refreshToken, false, now)
}

// RotateDeviceRefreshTokenWithEmailPolicy rotates a token pair under the
// caller's live email policy. The old access token is revoked in the same
// transaction that inserts its replacement.
func (s *Store) RotateDeviceRefreshTokenWithEmailPolicy(refreshToken string, requireEmailVerification bool, now time.Time) (*models.DeviceTokenPair, error) {
	now = deviceAuthTime(now)
	if refreshToken == "" {
		return nil, ErrInvalidDeviceRefreshToken
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var (
		userID        int64
		userAuthEpoch int64
		userStatus    string
		userEmail     string
		emailVerified int
		device        models.Device
		deviceRevoked sql.NullTime
		familyID      string
		tokenEpoch    int64
		expiresAt     time.Time
		familyAbs     time.Time
		rotatedAt     sql.NullTime
		revokedAt     sql.NullTime
		replacedBy    string
		overridesRaw  string
	)
	oldHash := auth.HashDeviceCredential(refreshToken)
	err = tx.QueryRow(`SELECT
		r.user_id, u.auth_epoch, u.status, u.email, u.email_verified,
		d.id, d.public_id, d.user_id, d.name, d.platform, d.client_version,
		d.created_at, d.last_seen_at, d.revoked_at,
		r.family_id, r.auth_epoch, r.expires_at, r.family_absolute_expires_at,
		r.rotated_at, r.revoked_at, r.replaced_by,
		COALESCE(o.overrides_json, '{}')
		FROM device_refresh_tokens r
		JOIN users u ON u.id = r.user_id
		JOIN devices d ON d.id = r.device_id
		LEFT JOIN device_feature_overrides o ON o.device_id = d.id
		WHERE r.token_hash = ?`, oldHash).
		Scan(&userID, &userAuthEpoch, &userStatus, &userEmail, &emailVerified,
			&device.ID, &device.PublicID, &device.UserID, &device.Name, &device.Platform,
			&device.ClientVersion, &device.CreatedAt, &device.LastSeenAt, &deviceRevoked,
			&familyID, &tokenEpoch, &expiresAt, &familyAbs, &rotatedAt, &revokedAt, &replacedBy,
			&overridesRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrInvalidDeviceRefreshToken
	}
	if err != nil {
		return nil, err
	}
	if tokenEpoch != userAuthEpoch {
		return nil, ErrDeviceRevoked
	}
	overrides, err := decodeDeviceFeatureOverrides(overridesRaw)
	if err != nil {
		return nil, err
	}
	if overrides.DisablesDeviceAuth() {
		if overrides.ClientV1Disabled {
			return nil, ErrDeviceV1Disabled
		}
		return nil, ErrDeviceAuthFeatureDisabled
	}

	if rotatedAt.Valid || replacedBy != "" {
		if err := revokeDeviceSessionFamilyTx(tx, familyID, device.ID, now, "refresh_reuse"); err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return nil, ErrDeviceRefreshTokenReused
	}
	if revokedAt.Valid {
		return nil, ErrDeviceRefreshTokenRevoked
	}
	if !now.Before(expiresAt) || !now.Before(familyAbs) {
		if _, err := tx.Exec(`UPDATE device_refresh_tokens
			SET revoked_at = ?, revoked_reason = ? WHERE token_hash = ? AND revoked_at IS NULL`,
			now, "expired", oldHash); err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return nil, ErrDeviceRefreshTokenExpired
	}
	if !deviceAuthUserAllowed(userStatus, userEmail, emailVerified != 0, requireEmailVerification) {
		if err := revokeDeviceTokenFamilyTx(tx, familyID, now, "user_unavailable"); err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return nil, ErrDeviceUserUnavailable
	}
	if deviceRevoked.Valid {
		if err := revokeDeviceTokenFamilyTx(tx, familyID, now, "device_revoked"); err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return nil, ErrDeviceRevoked
	}

	newAccessToken, err := auth.NewDeviceAccessToken()
	if err != nil {
		return nil, err
	}
	newRefreshToken, err := auth.NewDeviceRefreshToken()
	if err != nil {
		return nil, err
	}
	newAccessHash := auth.HashDeviceCredential(newAccessToken)
	newRefreshHash := auth.HashDeviceCredential(newRefreshToken)
	accessExpiresAt := now.Add(auth.DeviceAccessTokenTTL)
	refreshExpiresAt := now.Add(auth.DeviceRefreshTokenTTL)
	if familyAbs.Before(refreshExpiresAt) {
		refreshExpiresAt = familyAbs
	}

	res, err := tx.Exec(`UPDATE device_refresh_tokens
		SET rotated_at = ?, replaced_by = ?
		WHERE token_hash = ? AND rotated_at IS NULL AND revoked_at IS NULL AND expires_at > ?`,
		now, newRefreshHash, oldHash, now)
	if err != nil {
		return nil, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if affected != 1 {
		if err := revokeDeviceSessionFamilyTx(tx, familyID, device.ID, now, "refresh_reuse"); err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return nil, ErrDeviceRefreshTokenReused
	}

	// Rotation replaces the family's bearer session. Revoke every older access
	// token before inserting the replacement so a stolen access token cannot
	// outlive the refresh token it came with.
	if _, err := tx.Exec(`UPDATE device_access_tokens
		SET revoked_at = COALESCE(revoked_at, ?), revoked_reason = ?
		WHERE family_id = ? AND revoked_at IS NULL`,
		now, "refresh_rotated", familyID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`INSERT INTO device_refresh_tokens
		(token_hash, user_id, device_id, family_id, auth_epoch, expires_at,
		 family_absolute_expires_at, created_at)
		VALUES (?,?,?,?,?,?,?,?)`,
		newRefreshHash, userID, device.ID, familyID, userAuthEpoch,
		refreshExpiresAt, familyAbs, now); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`INSERT INTO device_access_tokens
		(token_hash, user_id, device_id, family_id, auth_epoch, expires_at, created_at)
		VALUES (?,?,?,?,?,?,?)`,
		newAccessHash, userID, device.ID, familyID, userAuthEpoch,
		accessExpiresAt, now); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`UPDATE devices SET last_seen_at = ?
		WHERE id = ? AND revoked_at IS NULL`, now, device.ID); err != nil {
		return nil, err
	}

	device.LastSeenAt = now
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &models.DeviceTokenPair{
		AccessToken: newAccessToken, RefreshToken: newRefreshToken,
		TokenType: "Bearer", AccessExpiresAt: accessExpiresAt,
		RefreshExpiresAt: refreshExpiresAt,
		RefreshExpiresIn: int(refreshExpiresAt.Sub(now) / time.Second),
		Device:           &device,
	}, nil
}

// DevicesByUser lists active and revoked devices, newest first. The caller is
// responsible for passing the authenticated user's id.
func (s *Store) DevicesByUser(userID int64) ([]models.Device, error) {
	rows, err := s.DB.Query(`SELECT id, public_id, user_id, name, platform,
		client_version, created_at, last_seen_at, revoked_at
		FROM devices WHERE user_id = ? ORDER BY created_at DESC, id DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var devices []models.Device
	for rows.Next() {
		var (
			device  models.Device
			revoked sql.NullTime
		)
		if err := rows.Scan(&device.ID, &device.PublicID, &device.UserID, &device.Name,
			&device.Platform, &device.ClientVersion, &device.CreatedAt,
			&device.LastSeenAt, &revoked); err != nil {
			return nil, err
		}
		if revoked.Valid {
			t := revoked.Time
			device.RevokedAt = &t
		}
		devices = append(devices, device)
	}
	return devices, rows.Err()
}

// RevokeDevice revokes one device owned by userID and every token bound to it.
// A public id from another account is indistinguishable from a missing one.
func (s *Store) RevokeDevice(userID int64, publicID string, now time.Time) error {
	now = deviceAuthTime(now)
	publicID = strings.TrimSpace(publicID)
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Acquire SQLite's writer reservation before reading. A deferred
	// read-to-write upgrade can fail with SQLITE_BUSY_SNAPSHOT after another
	// connection commits, so the no-op update makes concurrent revocations
	// serialize without changing any stored value.
	if _, err := tx.Exec(`UPDATE devices SET last_seen_at = last_seen_at
		WHERE user_id = ? AND public_id = ?`, userID, publicID); err != nil {
		return err
	}

	var deviceID int64
	err = tx.QueryRow(`SELECT id FROM devices WHERE user_id = ? AND public_id = ?`,
		userID, publicID).Scan(&deviceID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrDeviceNotFound
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE devices SET revoked_at = COALESCE(revoked_at, ?)
		WHERE id = ? AND user_id = ?`, now, deviceID, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE device_access_tokens
		SET revoked_at = COALESCE(revoked_at, ?), revoked_reason = ?
		WHERE device_id = ? AND revoked_at IS NULL`, now, "device_revoked", deviceID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE device_refresh_tokens
		SET revoked_at = COALESCE(revoked_at, ?), revoked_reason = ?
		WHERE device_id = ? AND revoked_at IS NULL`, now, "device_revoked", deviceID); err != nil {
		return err
	}
	if err := revokePlaybackForDeviceTx(tx, deviceID, now); err != nil {
		return err
	}
	return tx.Commit()
}

// RevokeAllDevicesForUser is the account-wide switch used after a password
// change, disable, or an explicit "sign out everywhere" action.
func (s *Store) RevokeAllDevicesForUser(userID int64, now time.Time) (int64, error) {
	now = deviceAuthTime(now)
	tx, err := s.DB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	// A global device revocation also advances the epoch so any token row that
	// was inserted concurrently with the revocation cannot become valid again.
	if _, err := tx.Exec(`UPDATE users SET auth_epoch = auth_epoch + 1 WHERE id = ?`, userID); err != nil {
		return 0, err
	}
	revoked, err := revokeAllDevicesForUserTx(tx, userID, now)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return revoked, nil
}

func revokeAllDevicesForUserTx(tx *sql.Tx, userID int64, now time.Time) (int64, error) {
	res, err := tx.Exec(`UPDATE devices SET revoked_at = ?
		WHERE user_id = ? AND revoked_at IS NULL`, now, userID)
	if err != nil {
		return 0, err
	}
	revoked, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`UPDATE device_access_tokens
		SET revoked_at = COALESCE(revoked_at, ?), revoked_reason = ?
		WHERE user_id = ? AND revoked_at IS NULL`, now, "all_devices_revoked", userID); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`UPDATE device_refresh_tokens
		SET revoked_at = COALESCE(revoked_at, ?), revoked_reason = ?
		WHERE user_id = ? AND revoked_at IS NULL`, now, "all_devices_revoked", userID); err != nil {
		return 0, err
	}
	if err := revokePlaybackForUserTx(tx, userID, now); err != nil {
		return 0, err
	}
	return revoked, nil
}

func revokePlaybackForDeviceTx(tx *sql.Tx, deviceID int64, now time.Time) error {
	if _, err := tx.Exec(`UPDATE playback_tickets
		SET state = ?, revoked_at = ?, version = version + 1, updated_at = ?
		WHERE device_id = ? AND state IN (?,?,?)`,
		models.PlaybackTicketRevoked, now, now, deviceID,
		models.PlaybackTicketProbing, models.PlaybackTicketReady,
		models.PlaybackTicketConsuming); err != nil {
		return err
	}
	_, err := tx.Exec(`UPDATE playback_media_sessions
		SET state = ?, revoked_at = ?, version = version + 1, updated_at = ?
		WHERE device_id = ? AND state = ?`,
		models.PlaybackMediaSessionRevoked, now, now, deviceID,
		models.PlaybackMediaSessionActive)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE playback_subtitle_sessions
		SET state = ?, revoked_at = ?, version = version + 1, updated_at = ?
		WHERE device_id = ? AND state = ?`,
		models.PlaybackSubtitleSessionRevoked, now, now, deviceID,
		models.PlaybackSubtitleSessionActive)
	return err
}

func revokePlaybackForUserTx(tx *sql.Tx, userID int64, now time.Time) error {
	if _, err := tx.Exec(`UPDATE playback_tickets
		SET state = ?, revoked_at = ?, version = version + 1, updated_at = ?
		WHERE user_id = ? AND state IN (?,?,?)`,
		models.PlaybackTicketRevoked, now, now, userID,
		models.PlaybackTicketProbing, models.PlaybackTicketReady,
		models.PlaybackTicketConsuming); err != nil {
		return err
	}
	_, err := tx.Exec(`UPDATE playback_media_sessions
		SET state = ?, revoked_at = ?, version = version + 1, updated_at = ?
		WHERE user_id = ? AND state = ?`,
		models.PlaybackMediaSessionRevoked, now, now, userID,
		models.PlaybackMediaSessionActive)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE playback_subtitle_sessions
		SET state = ?, revoked_at = ?, version = version + 1, updated_at = ?
		WHERE user_id = ? AND state = ?`,
		models.PlaybackSubtitleSessionRevoked, now, now, userID,
		models.PlaybackSubtitleSessionActive)
	return err
}

// CleanupDeviceAuth removes expired authorizations and expired or revoked
// token rows. Rotated-but-unexpired refresh rows stay until their natural
// expiry so replay can still be distinguished from an unknown token.
func (s *Store) CleanupDeviceAuth(now time.Time) error {
	now = deviceAuthTime(now)
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, stmt := range []string{
		`DELETE FROM device_authorizations WHERE expires_at <= ?`,
		`DELETE FROM device_access_tokens WHERE expires_at <= ? OR revoked_at IS NOT NULL`,
		`DELETE FROM device_refresh_tokens WHERE expires_at <= ? OR revoked_at IS NOT NULL`,
	} {
		if _, err := tx.Exec(stmt, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func createDeviceTx(tx *sql.Tx, userID int64, name, platform, clientVersion string, authorizationID int64, now time.Time) (*models.Device, error) {
	publicID, err := auth.NewDevicePublicID()
	if err != nil {
		return nil, err
	}
	res, err := tx.Exec(`INSERT INTO devices
		(public_id, user_id, name, platform, client_version, authorization_id,
		 created_at, last_seen_at)
		VALUES (?,?,?,?,?,?,?,?)`,
		publicID, userID, name, platform, clientVersion, authorizationID, now, now)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &models.Device{
		ID: id, PublicID: publicID, UserID: userID, Name: name,
		Platform: platform, ClientVersion: clientVersion,
		CreatedAt: now, LastSeenAt: now,
	}, nil
}

func createDeviceTokenPairTx(tx *sql.Tx, userID int64, device *models.Device, now time.Time) (*models.DeviceTokenPair, error) {
	var authEpoch int64
	if err := tx.QueryRow(`SELECT auth_epoch FROM users WHERE id = ?`, userID).
		Scan(&authEpoch); err != nil {
		return nil, err
	}
	accessToken, err := auth.NewDeviceAccessToken()
	if err != nil {
		return nil, err
	}
	refreshToken, err := auth.NewDeviceRefreshToken()
	if err != nil {
		return nil, err
	}
	familyID, err := auth.NewDeviceTokenFamilyID()
	if err != nil {
		return nil, err
	}
	accessExpiresAt := now.Add(auth.DeviceAccessTokenTTL)
	refreshExpiresAt := now.Add(auth.DeviceRefreshTokenTTL)
	familyAbsoluteExpiresAt := now.Add(auth.DeviceRefreshTokenFamilyTTL)
	if familyAbsoluteExpiresAt.Before(refreshExpiresAt) {
		refreshExpiresAt = familyAbsoluteExpiresAt
	}
	if _, err := tx.Exec(`INSERT INTO device_access_tokens
		(token_hash, user_id, device_id, family_id, auth_epoch, expires_at, created_at)
		VALUES (?,?,?,?,?,?,?)`,
		auth.HashDeviceCredential(accessToken), userID, device.ID, familyID,
		authEpoch, accessExpiresAt, now); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`INSERT INTO device_refresh_tokens
		(token_hash, user_id, device_id, family_id, auth_epoch, expires_at,
		 family_absolute_expires_at, created_at)
		VALUES (?,?,?,?,?,?,?,?)`,
		auth.HashDeviceCredential(refreshToken), userID, device.ID, familyID,
		authEpoch, refreshExpiresAt, familyAbsoluteExpiresAt, now); err != nil {
		return nil, err
	}
	return &models.DeviceTokenPair{
		AccessToken: accessToken, RefreshToken: refreshToken, TokenType: "Bearer",
		AccessExpiresAt: accessExpiresAt, RefreshExpiresAt: refreshExpiresAt,
		RefreshExpiresIn: int(refreshExpiresAt.Sub(now) / time.Second),
		Device:           device,
	}, nil
}

func revokeDeviceTokenFamilyTx(tx *sql.Tx, familyID string, now time.Time, reason string) error {
	if _, err := tx.Exec(`UPDATE device_access_tokens
		SET revoked_at = COALESCE(revoked_at, ?), revoked_reason = ?
		WHERE family_id = ? AND revoked_at IS NULL`, now, reason, familyID); err != nil {
		return err
	}
	_, err := tx.Exec(`UPDATE device_refresh_tokens
		SET revoked_at = COALESCE(revoked_at, ?), revoked_reason = ?
		WHERE family_id = ? AND revoked_at IS NULL`, now, reason, familyID)
	return err
}

func revokeDeviceSessionFamilyTx(tx *sql.Tx, familyID string, deviceID int64, now time.Time, reason string) error {
	if err := revokeDeviceTokenFamilyTx(tx, familyID, now, reason); err != nil {
		return err
	}
	_, err := tx.Exec(`UPDATE devices SET revoked_at = COALESCE(revoked_at, ?)
		WHERE id = ?`, now, deviceID)
	return err
}

func deviceAuthUserAllowed(status, email string, emailVerified, requireEmailVerification bool) bool {
	if status != models.StatusActive {
		return false
	}
	if requireEmailVerification && strings.TrimSpace(email) != "" && !emailVerified {
		return false
	}
	return true
}

func (s *Store) markDeviceAuthorizationExpired(id int64, now time.Time) error {
	_, err := s.DB.Exec(`UPDATE device_authorizations SET state = ?, updated_at = ?
		WHERE id = ? AND state IN (?,?)`,
		models.DeviceAuthorizationExpired, now, id,
		models.DeviceAuthorizationPending, models.DeviceAuthorizationAuthorized)
	return err
}

func scanDeviceAuthorization(row interface{ Scan(...any) error }) (*models.DeviceAuthorization, error) {
	var (
		authorization models.DeviceAuthorization
		userID        sql.NullInt64
		authorizedAt  sql.NullTime
		consumedAt    sql.NullTime
	)
	if err := row.Scan(&authorization.ID, &authorization.AuthorizationID,
		&authorization.DeviceName, &authorization.Platform,
		&authorization.ClientVersion, &authorization.State, &userID,
		&authorization.CreatedAt, &authorization.ExpiresAt,
		&authorization.UpdatedAt, &authorizedAt, &consumedAt); err != nil {
		return nil, err
	}
	if userID.Valid {
		id := userID.Int64
		authorization.UserID = &id
	}
	if authorizedAt.Valid {
		t := authorizedAt.Time
		authorization.AuthorizedAt = &t
	}
	if consumedAt.Valid {
		t := consumedAt.Time
		authorization.ConsumedAt = &t
	}
	return &authorization, nil
}

func normalizeDeviceUserCode(userCode string) (string, error) {
	normalized, err := auth.NormalizeDeviceUserCode(userCode)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidDeviceUserCode, err)
	}
	return normalized, nil
}

func deviceAuthTime(now time.Time) time.Time {
	if now.IsZero() {
		now = time.Now()
	}
	return now.UTC()
}
