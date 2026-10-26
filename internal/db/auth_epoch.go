package db

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var (
	ErrUserNotFound                  = errors.New("user not found")
	ErrPasswordResetTokenInvalid     = errors.New("password reset token is invalid or expired")
	ErrEmailVerificationTokenInvalid = errors.New("email verification token is invalid or expired")
)

const authEpochBackfillMigration = "auth_epoch_columns_v1"

// PasswordRotationOptions controls the non-secret parts of an atomic password
// rotation. ResetToken is checked and consumed in the same transaction as the
// password change; PreserveSessionToken keeps the caller's current Web session
// alive for the existing self-service and administrator behavior.
type PasswordRotationOptions struct {
	ResetToken           string
	PreserveSessionToken string
	VerifyCurrentEmail   bool
}

// RotateUserPassword updates credentials, advances the device auth epoch and
// revokes every dependent credential in one SQLite transaction. A failure in
// any step leaves the password, sessions, token rows and playback capabilities
// exactly as they were.
func (s *Store) RotateUserPassword(
	userID int64,
	hash, salt string,
	now time.Time,
	opts PasswordRotationOptions,
) (int64, error) {
	if userID <= 0 {
		return 0, ErrUserNotFound
	}
	now = deviceAuthTime(now)
	tx, err := s.DB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	// A no-op write takes SQLite's writer reservation before any read. This
	// avoids a deferred read-to-write upgrade racing a concurrent revocation.
	res, err := tx.Exec(`UPDATE users SET auth_epoch = auth_epoch WHERE id = ?`, userID)
	if err != nil {
		return 0, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if affected != 1 {
		return 0, ErrUserNotFound
	}

	if opts.ResetToken != "" {
		res, err = tx.Exec(`DELETE FROM auth_tokens
			WHERE token = ? AND user_id = ? AND kind = ? AND expires_at > ?
				AND issued_email <> ''
				AND issued_email = (SELECT email FROM users WHERE id = ?)`,
			opts.ResetToken, userID, TokenResetPassword, now, userID)
		if err != nil {
			return 0, err
		}
		affected, err = res.RowsAffected()
		if err != nil {
			return 0, err
		}
		if affected != 1 {
			return 0, ErrPasswordResetTokenInvalid
		}
	}
	// Changing a password invalidates every outstanding recovery link too.
	if _, err := tx.Exec(`DELETE FROM auth_tokens WHERE user_id = ? AND kind = ?`,
		userID, TokenResetPassword); err != nil {
		return 0, err
	}

	res, err = tx.Exec(`UPDATE users
		SET pass_hash = ?, salt = ?
		WHERE id = ?`, hash, salt, userID)
	if err != nil {
		return 0, err
	}
	affected, err = res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if affected != 1 {
		return 0, ErrUserNotFound
	}
	if opts.VerifyCurrentEmail {
		if _, err := tx.Exec(`UPDATE users SET email_verified = 1
			WHERE id = ? AND email <> ''`, userID); err != nil {
			return 0, err
		}
	}

	oldEpoch, newEpoch, err := advanceUserAuthEpochTx(tx, userID)
	if err != nil {
		return 0, err
	}
	if err := revokeUserWebSessionsTx(
		tx, userID, opts.PreserveSessionToken, oldEpoch, newEpoch,
	); err != nil {
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

// UpdateUserStatusAndRevoke changes an account's state and invalidates every
// credential and playback capability in one transaction. It runs for both
// directions of a status change: re-enabling an account must not revive a
// session, device token or media capability that predates the disable.
func (s *Store) UpdateUserStatusAndRevoke(userID int64, status string, now time.Time) (int64, error) {
	now = deviceAuthTime(now)
	tx, err := s.DB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	res, err := tx.Exec(`UPDATE users SET auth_epoch = auth_epoch WHERE id = ?`, userID)
	if err != nil {
		return 0, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if affected != 1 {
		return 0, ErrUserNotFound
	}
	if _, err := tx.Exec(`UPDATE users SET status = ? WHERE id = ?`, status, userID); err != nil {
		return 0, err
	}
	_, newEpoch, err := advanceUserAuthEpochTx(tx, userID)
	if err != nil {
		return 0, err
	}
	if err := revokeUserWebSessionsTx(tx, userID, "", 0, newEpoch); err != nil {
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

// RevokeUserSessionsAndDevices implements the admin "sign out everywhere"
// action. Advancing the epoch fences concurrent token or session issuance,
// while the transaction keeps Web sessions, device tokens and playback
// capabilities in the same all-or-nothing revocation.
func (s *Store) RevokeUserSessionsAndDevices(userID int64, excludeSessionToken string, now time.Time) (int64, error) {
	now = deviceAuthTime(now)
	tx, err := s.DB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	res, err := tx.Exec(`UPDATE users SET auth_epoch = auth_epoch WHERE id = ?`, userID)
	if err != nil {
		return 0, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if affected != 1 {
		return 0, ErrUserNotFound
	}
	oldEpoch, newEpoch, err := advanceUserAuthEpochTx(tx, userID)
	if err != nil {
		return 0, err
	}
	if err := revokeUserWebSessionsTx(
		tx, userID, excludeSessionToken, oldEpoch, newEpoch,
	); err != nil {
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

func advanceUserAuthEpochTx(tx *sql.Tx, userID int64) (int64, int64, error) {
	var oldEpoch int64
	if err := tx.QueryRow(`SELECT auth_epoch FROM users WHERE id = ?`, userID).
		Scan(&oldEpoch); err != nil {
		return 0, 0, err
	}
	res, err := tx.Exec(`UPDATE users SET auth_epoch = auth_epoch + 1 WHERE id = ?`, userID)
	if err != nil {
		return 0, 0, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, 0, err
	}
	if affected != 1 {
		return 0, 0, ErrUserNotFound
	}
	return oldEpoch, oldEpoch + 1, nil
}

func revokeUserWebSessionsTx(
	tx *sql.Tx,
	userID int64,
	excludeToken string,
	oldEpoch, newEpoch int64,
) error {
	if excludeToken != "" {
		if _, err := tx.Exec(`DELETE FROM sessions WHERE user_id = ? AND token <> ?`,
			userID, excludeToken); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE sessions SET auth_epoch = ?
			WHERE user_id = ? AND token = ? AND auth_epoch = ?`,
			newEpoch, userID, excludeToken, oldEpoch)
		return err
	}
	_, err := tx.Exec(`DELETE FROM sessions WHERE user_id = ?`, userID)
	return err
}

// migrateAuthEpochColumns upgrades pre-epoch token tables without logging out
// devices or Web sessions simply because the server was upgraded. Legacy rows
// inherit their owner's current epoch; all later rotations move the user ahead
// of them. The durable completion marker makes the backfill re-entrant after an
// interrupted upgrade without reviving a stale epoch written after it finished.
func (s *Store) migrateAuthEpochColumns() error {
	if _, err := s.DB.Exec(`UPDATE users SET auth_epoch = 1
		WHERE auth_epoch IS NULL OR auth_epoch < 1`); err != nil {
		return fmt.Errorf("normalize users.auth_epoch: %w", err)
	}
	if _, err := s.DB.Exec(`CREATE TABLE IF NOT EXISTS auth_epoch_migrations (
		name TEXT PRIMARY KEY,
		completed_at TIMESTAMP NOT NULL
	)`); err != nil {
		return fmt.Errorf("create auth epoch migration marker table: %w", err)
	}
	if err := s.addColumn("sessions", "auth_epoch",
		`ALTER TABLE sessions ADD COLUMN auth_epoch INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	for _, table := range []string{"device_access_tokens", "device_refresh_tokens"} {
		if err := s.addColumn(table, "auth_epoch",
			`ALTER TABLE `+table+` ADD COLUMN auth_epoch INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
	}

	var completed int
	err := s.DB.QueryRow(`SELECT 1 FROM auth_epoch_migrations WHERE name = ?`,
		authEpochBackfillMigration).Scan(&completed)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read auth epoch migration marker: %w", err)
	}

	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, table := range []string{"sessions", "device_access_tokens", "device_refresh_tokens"} {
		if _, err := tx.Exec(`UPDATE ` + table + `
			SET auth_epoch = COALESCE((
				SELECT u.auth_epoch FROM users u WHERE u.id = ` + table + `.user_id
			), 1)
			WHERE auth_epoch IS NULL OR auth_epoch = 0`); err != nil {
			return fmt.Errorf("backfill %s.auth_epoch: %w", table, err)
		}
	}
	if _, err := tx.Exec(`INSERT INTO auth_epoch_migrations (name, completed_at)
		VALUES (?, ?)`, authEpochBackfillMigration, time.Now().UTC()); err != nil {
		return fmt.Errorf("mark auth epoch migration complete: %w", err)
	}
	return tx.Commit()
}
