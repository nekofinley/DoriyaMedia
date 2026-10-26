package db

import (
	"database/sql"
	"errors"
	"time"

	"doriyamedia/internal/models"
)

var (
	ErrUserNameTaken             = errors.New("user name is already taken")
	ErrUserEmailTaken            = errors.New("user email is already taken")
	ErrLastActiveAdmin           = errors.New("at least one active admin is required")
	ErrAdminActorUnavailable     = errors.New("admin actor is no longer active")
	ErrAdminCannotChangeOwnState = errors.New("administrator cannot change own account status")
	ErrAdminCannotDeleteSelf     = errors.New("administrator cannot delete own account")
)

// AdminUserUpdate is a patch applied by an authenticated administrator. Every
// supplied field, the final-admin check, and any status-change revocation are
// committed together.
type AdminUserUpdate struct {
	ActorID       int64
	UserID        int64
	Name          *string
	Role          *string
	Status        *string
	Email         *string
	EmailVerified *bool
}

// UpdateUserAdmin updates one account atomically. It first takes SQLite's
// writer reservation, then re-reads both accounts, so a stale admin actor or a
// concurrent demotion cannot slip between the permission and final-admin
// checks and their writes.
func (s *Store) UpdateUserAdmin(update AdminUserUpdate, now time.Time) (*models.User, error) {
	if update.UserID <= 0 {
		return nil, ErrUserNotFound
	}
	now = deviceAuthTime(now)
	tx, err := s.DB.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	current, err := lockAdminUserTargetTx(tx, update.UserID)
	if err != nil {
		return nil, err
	}
	if err := requireActiveAdminActorTx(tx, update.ActorID); err != nil {
		return nil, err
	}

	name, role, status := current.Name, current.Role, current.Status
	email, emailVerified := current.Email, current.EmailVerified
	if update.Name != nil {
		name = *update.Name
	}
	if update.Role != nil {
		role = *update.Role
	}
	if update.Status != nil {
		status = *update.Status
	}
	if update.Email != nil {
		email = *update.Email
		if email != current.Email {
			emailVerified = false
		}
	}
	if update.EmailVerified != nil {
		emailVerified = *update.EmailVerified
	}
	if email == "" {
		emailVerified = false
	}
	if update.ActorID == update.UserID &&
		update.Status != nil && status != current.Status {
		return nil, ErrAdminCannotChangeOwnState
	}

	if name != current.Name {
		taken, err := adminNameTakenTx(tx, name, update.UserID)
		if err != nil {
			return nil, err
		}
		if taken {
			return nil, ErrUserNameTaken
		}
	}
	if email != "" && email != current.Email {
		taken, err := adminEmailTakenTx(tx, email, update.UserID)
		if err != nil {
			return nil, err
		}
		if taken {
			return nil, ErrUserEmailTaken
		}
	}

	// This UPDATE is the atomic final-admin condition. It permits leaving the
	// active-admin set only when another active admin exists at write time.
	res, err := tx.Exec(`UPDATE users
		SET name = ?, role = ?, status = ?, email = ?, email_verified = ?
		WHERE id = ?
			AND (
				role <> ? OR status <> ?
				OR (? = ? AND ? = ?)
				OR EXISTS (
					SELECT 1 FROM users AS other
					WHERE other.id <> ?
						AND other.role = ?
						AND other.status = ?
				)
			)`,
		name, role, status, email, boolToInt(emailVerified), update.UserID,
		models.RoleAdmin, models.StatusActive,
		role, models.RoleAdmin, status, models.StatusActive,
		update.UserID, models.RoleAdmin, models.StatusActive)
	if err != nil {
		return nil, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if affected != 1 {
		return nil, ErrLastActiveAdmin
	}

	// Preserve the existing API semantics: a status change revokes every auth
	// and playback capability, while profile/role/email edits do not by
	// themselves sign the account out.
	if status != current.Status {
		_, newEpoch, err := advanceUserAuthEpochTx(tx, update.UserID)
		if err != nil {
			return nil, err
		}
		if err := revokeUserWebSessionsTx(tx, update.UserID, "", 0, newEpoch); err != nil {
			return nil, err
		}
		if _, err := revokeAllDevicesForUserTx(tx, update.UserID, now); err != nil {
			return nil, err
		}
	}
	if email != current.Email {
		if _, err := tx.Exec(`DELETE FROM auth_tokens
			WHERE user_id = ? AND kind IN (?, ?)`,
			update.UserID, TokenVerifyEmail, TokenResetPassword); err != nil {
			return nil, err
		}
	}

	updated, err := scanUser(tx.QueryRow(`SELECT `+userCols+` FROM users WHERE id = ?`,
		update.UserID))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return updated, nil
}

// DeleteUserAdmin removes an account only if the authenticated actor is still
// an active admin and deleting the target leaves an active admin behind.
func (s *Store) DeleteUserAdmin(actorID, userID int64, now time.Time) error {
	if userID <= 0 {
		return ErrUserNotFound
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := lockAdminUserTargetTx(tx, userID); err != nil {
		return err
	}
	if err := requireActiveAdminActorTx(tx, actorID); err != nil {
		return err
	}
	if actorID == userID {
		return ErrAdminCannotDeleteSelf
	}

	res, err := tx.Exec(`DELETE FROM users
		WHERE id = ?
			AND (
				role <> ? OR status <> ?
				OR EXISTS (
					SELECT 1 FROM users AS other
					WHERE other.id <> ?
						AND other.role = ?
						AND other.status = ?
				)
			)`,
		userID, models.RoleAdmin, models.StatusActive,
		userID, models.RoleAdmin, models.StatusActive)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return ErrLastActiveAdmin
	}
	if _, err := tx.Exec(`DELETE FROM progress WHERE user_id = ?`, userID); err != nil {
		return err
	}
	return tx.Commit()
}

func lockAdminUserTargetTx(tx *sql.Tx, userID int64) (*models.User, error) {
	// Taking SQLite's writer reservation before any read prevents two deferred
	// transactions from both validating the same admin set and then writing.
	res, err := tx.Exec(`UPDATE users SET auth_epoch = auth_epoch WHERE id = ?`, userID)
	if err != nil {
		return nil, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if affected != 1 {
		return nil, ErrUserNotFound
	}
	user, err := scanUser(tx.QueryRow(`SELECT `+userCols+` FROM users WHERE id = ?`, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	return user, err
}

func requireActiveAdminActorTx(tx *sql.Tx, actorID int64) error {
	if actorID <= 0 {
		return ErrAdminActorUnavailable
	}
	var role, status string
	err := tx.QueryRow(`SELECT role, status FROM users WHERE id = ?`, actorID).
		Scan(&role, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrAdminActorUnavailable
	}
	if err != nil {
		return err
	}
	if role != models.RoleAdmin || status != models.StatusActive {
		return ErrAdminActorUnavailable
	}
	return nil
}

func adminNameTakenTx(tx *sql.Tx, name string, exceptID int64) (bool, error) {
	var found int
	err := tx.QueryRow(`SELECT 1 FROM users
		WHERE name = ? COLLATE NOCASE AND id <> ? LIMIT 1`,
		name, exceptID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func adminEmailTakenTx(tx *sql.Tx, email string, exceptID int64) (bool, error) {
	var found int
	err := tx.QueryRow(`SELECT 1 FROM users
		WHERE email <> '' AND email = ? COLLATE NOCASE AND id <> ? LIMIT 1`,
		email, exceptID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
