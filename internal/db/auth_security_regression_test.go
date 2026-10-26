package db

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"doriyamedia/internal/auth"
	"doriyamedia/internal/models"
)

func TestDeviceAuthorizationEpochBoundAtApproval(t *testing.T) {
	store := openDeviceAuthStore(t)
	user := createDeviceAuthUser(t, store, "epoch-bound-device")
	now := time.Date(2026, 10, 27, 13, 0, 0, 0, time.UTC)

	verifier := newDeviceAuthVerifier(t)
	grant := createDeviceAuthorization(t, store, verifier, "Before rotation", now)
	if _, err := store.ApproveDeviceAuthorization(grant.UserCode, user.ID, now); err != nil {
		t.Fatalf("approve before rotation: %v", err)
	}
	var approvedEpoch int64
	if err := store.DB.QueryRow(`SELECT auth_epoch FROM device_authorizations
		WHERE id = ?`, grant.Authorization.ID).Scan(&approvedEpoch); err != nil {
		t.Fatalf("read approval epoch: %v", err)
	}
	if approvedEpoch != 1 {
		t.Fatalf("approval epoch = %d, want 1", approvedEpoch)
	}

	if _, err := store.RotateUserPassword(
		user.ID, "rotated-hash", "rotated-salt", now.Add(time.Second),
		PasswordRotationOptions{},
	); err != nil {
		t.Fatalf("rotate password: %v", err)
	}
	if _, err := store.ConsumeDeviceAuthorization(
		grant.DeviceCode, verifier, now.Add(2*time.Second),
	); !errors.Is(err, ErrInvalidDeviceCode) {
		t.Fatalf("stale authorization exchange error = %v, want ErrInvalidDeviceCode", err)
	}
	for _, table := range []string{"device_access_tokens", "device_refresh_tokens", "devices"} {
		var count int
		if err := store.DB.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE user_id = ?`,
			user.ID).Scan(&count); err != nil {
			t.Fatalf("count %s after stale exchange: %v", table, err)
		}
		if count != 0 {
			t.Errorf("%s rows after stale exchange = %d, want 0", table, count)
		}
	}
	var state string
	if err := store.DB.QueryRow(`SELECT state FROM device_authorizations
		WHERE id = ?`, grant.Authorization.ID).Scan(&state); err != nil {
		t.Fatalf("read stale authorization state: %v", err)
	}
	if state != models.DeviceAuthorizationAuthorized {
		t.Errorf("stale authorization state = %q, want authorized", state)
	}

	// A fresh approval after the rotation must bind the new epoch and work.
	freshVerifier := newDeviceAuthVerifier(t)
	fresh := createDeviceAuthorization(t, store, freshVerifier, "After rotation", now.Add(3*time.Second))
	if _, err := store.ApproveDeviceAuthorization(
		fresh.UserCode, user.ID, now.Add(3*time.Second),
	); err != nil {
		t.Fatalf("approve after rotation: %v", err)
	}
	if _, err := store.ConsumeDeviceAuthorization(
		fresh.DeviceCode, freshVerifier, now.Add(4*time.Second),
	); err != nil {
		t.Fatalf("fresh authorization exchange: %v", err)
	}
}

func TestConcurrentPasswordRotationFencesDeviceAuthorizationConsume(t *testing.T) {
	first, second := openDeviceAuthConcurrencyStores(t)
	base := time.Date(2026, 10, 27, 14, 0, 0, 0, time.UTC)

	for i := 0; i < 8; i++ {
		now := base.Add(time.Duration(i) * time.Minute)
		user := createDeviceAuthUser(t, first, fmt.Sprintf("epoch-device-race-%d", i))
		verifier := newDeviceAuthVerifier(t)
		grant := createDeviceAuthorization(t, first, verifier, "Race device", now)
		if _, err := first.ApproveDeviceAuthorization(grant.UserCode, user.ID, now); err != nil {
			t.Fatalf("iteration %d approve: %v", i, err)
		}

		start := make(chan struct{})
		var wg sync.WaitGroup
		var passwordErr, consumeErr error
		var pair *models.DeviceTokenPair
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, passwordErr = first.RotateUserPassword(
				user.ID, "rotated-hash", "rotated-salt", now.Add(time.Second),
				PasswordRotationOptions{},
			)
		}()
		go func() {
			defer wg.Done()
			<-start
			pair, consumeErr = second.ConsumeDeviceAuthorization(
				grant.DeviceCode, verifier, now.Add(time.Second),
			)
		}()
		close(start)
		wg.Wait()

		if passwordErr != nil {
			t.Fatalf("iteration %d password rotation: %v", i, passwordErr)
		}
		if consumeErr != nil && !errors.Is(consumeErr, ErrInvalidDeviceCode) {
			t.Fatalf("iteration %d consume error = %v, want nil or ErrInvalidDeviceCode", i, consumeErr)
		}
		if pair != nil {
			if _, err := first.DeviceByAccessToken(
				pair.AccessToken, now.Add(2*time.Second),
			); err == nil {
				t.Fatalf("iteration %d concurrently issued token survived password rotation", i)
			}
		}
		var active int
		if err := first.DB.QueryRow(`SELECT COUNT(*) FROM device_access_tokens
			WHERE user_id = ? AND revoked_at IS NULL`, user.ID).Scan(&active); err != nil {
			t.Fatalf("iteration %d count active tokens: %v", i, err)
		}
		if active != 0 {
			t.Fatalf("iteration %d active tokens after rotation = %d, want 0", i, active)
		}
	}
}

func TestAuthEpochMigrationResumesInterruptedBackfillAndPreservesStaleRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "interrupted-auth-epoch.db")
	legacy, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	now := time.Date(2026, 10, 27, 15, 0, 0, 0, time.UTC)
	for _, stmt := range []string{
		`CREATE TABLE users (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT UNIQUE NOT NULL,
			pass_hash TEXT NOT NULL,
			salt TEXT NOT NULL,
			auth_epoch INTEGER NOT NULL DEFAULT 1,
			role TEXT NOT NULL DEFAULT 'user',
			created_at TIMESTAMP NOT NULL
		)`,
		// This is the state left by an upgrade that added the column and then
		// stopped before backfilling it.
		`CREATE TABLE sessions (
			token TEXT PRIMARY KEY,
			user_id INTEGER NOT NULL,
			auth_epoch INTEGER NOT NULL DEFAULT 0,
			expires_at TIMESTAMP NOT NULL
		)`,
		`CREATE TABLE auth_tokens (
			token TEXT PRIMARY KEY,
			user_id INTEGER NOT NULL,
			kind TEXT NOT NULL,
			expires_at TIMESTAMP NOT NULL,
			created_at TIMESTAMP NOT NULL
		)`,
	} {
		if _, err := legacy.Exec(stmt); err != nil {
			t.Fatalf("create legacy schema: %v", err)
		}
	}
	if _, err := legacy.Exec(`INSERT INTO users
		(name, pass_hash, salt, auth_epoch, role, created_at)
		VALUES ('interrupted', 'hash', 'salt', 3, 'user', ?)`, now); err != nil {
		t.Fatalf("insert legacy user: %v", err)
	}
	for _, row := range []struct {
		token string
		epoch int64
	}{
		{"uninitialized-session", 0},
		{"deliberately-stale-session", 1},
	} {
		if _, err := legacy.Exec(`INSERT INTO sessions
			(token, user_id, auth_epoch, expires_at) VALUES (?,?,?,?)`,
			row.token, 1, row.epoch, now.Add(time.Hour)); err != nil {
			t.Fatalf("insert %s: %v", row.token, err)
		}
	}
	if _, err := legacy.Exec(`INSERT INTO auth_tokens
		(token, user_id, kind, expires_at, created_at)
		VALUES ('legacy-reset', 1, 'reset', ?, ?)`,
		now.Add(time.Hour), now); err != nil {
		t.Fatalf("insert legacy reset token: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatalf("resume interrupted migration: %v", err)
	}
	for _, check := range []struct {
		token string
		want  int64
	}{
		{"uninitialized-session", 3},
		{"deliberately-stale-session", 1},
	} {
		var epoch int64
		if err := store.DB.QueryRow(`SELECT auth_epoch FROM sessions WHERE token = ?`,
			check.token).Scan(&epoch); err != nil {
			t.Fatalf("read %s: %v", check.token, err)
		}
		if epoch != check.want {
			t.Errorf("%s epoch = %d, want %d", check.token, epoch, check.want)
		}
	}
	hasIssuedEmail, err := store.hasColumn("auth_tokens", "issued_email")
	if err != nil || !hasIssuedEmail {
		t.Fatalf("auth_tokens.issued_email migration: has=%v err=%v", hasIssuedEmail, err)
	}
	var issuedEmail string
	if err := store.DB.QueryRow(`SELECT issued_email FROM auth_tokens
		WHERE token = 'legacy-reset'`).Scan(&issuedEmail); err != nil {
		t.Fatalf("read migrated issued_email: %v", err)
	}
	if issuedEmail != "" {
		t.Errorf("legacy issued_email = %q, want empty", issuedEmail)
	}
	var marker int
	if err := store.DB.QueryRow(`SELECT COUNT(*) FROM auth_epoch_migrations
		WHERE name = ?`, authEpochBackfillMigration).Scan(&marker); err != nil {
		t.Fatalf("read migration marker: %v", err)
	}
	if marker != 1 {
		t.Fatalf("auth epoch migration markers = %d, want 1", marker)
	}
	if _, err := store.DB.Exec(`UPDATE sessions SET auth_epoch = 0
		WHERE token = 'deliberately-stale-session'`); err != nil {
		t.Fatalf("make session deliberately stale: %v", err)
	}
	if err := store.DB.Close(); err != nil {
		t.Fatalf("close migrated store: %v", err)
	}

	store, err = Open(path)
	if err != nil {
		t.Fatalf("reopen migrated store: %v", err)
	}
	defer store.DB.Close()
	var epoch int64
	if err := store.DB.QueryRow(`SELECT auth_epoch FROM sessions
		WHERE token = 'deliberately-stale-session'`).Scan(&epoch); err != nil {
		t.Fatalf("read stale session after restart: %v", err)
	}
	if epoch != 0 {
		t.Fatalf("deliberately stale session epoch after restart = %d, want 0", epoch)
	}
}

func TestPasswordResetTokenBoundToCurrentEmail(t *testing.T) {
	store := openPlaybackTestStore(t)
	now := time.Date(2026, 10, 27, 16, 0, 0, 0, time.UTC)
	hash, salt := auth.HashPassword("old-password")
	user := &models.User{
		Name: "reset-email-binding", PassHash: hash, Salt: salt,
		Role: models.RoleUser, Status: models.StatusActive,
	}
	if err := store.CreateUser(user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := store.SetUserEmail(user.ID, "old@example.com", true); err != nil {
		t.Fatalf("set old email: %v", err)
	}

	token := auth.NewToken()
	if err := store.CreateAuthTokenForEmail(
		token, user.ID, TokenResetPassword, "old@example.com", now.Add(time.Hour),
	); err != nil {
		t.Fatalf("create bound reset token: %v", err)
	}
	var issuedEmail string
	if err := store.DB.QueryRow(`SELECT issued_email FROM auth_tokens
		WHERE token = ?`, token).Scan(&issuedEmail); err != nil {
		t.Fatalf("read issued email: %v", err)
	}
	if issuedEmail != "old@example.com" {
		t.Fatalf("issued email = %q, want old@example.com", issuedEmail)
	}

	if err := store.SetUserEmail(user.ID, "new@example.com", false); err != nil {
		t.Fatalf("change email: %v", err)
	}
	var tokens int
	if err := store.DB.QueryRow(`SELECT COUNT(*) FROM auth_tokens WHERE token = ?`,
		token).Scan(&tokens); err != nil {
		t.Fatalf("count reset token after email change: %v", err)
	}
	if tokens != 0 {
		t.Fatalf("reset token rows after email change = %d, want 0", tokens)
	}
	newHash, newSalt := auth.HashPassword("new-password")
	if _, err := store.RotateUserPassword(
		user.ID, newHash, newSalt, now.Add(time.Minute),
		PasswordRotationOptions{ResetToken: token, VerifyCurrentEmail: true},
	); !errors.Is(err, ErrPasswordResetTokenInvalid) {
		t.Fatalf("reset after email change error = %v, want ErrPasswordResetTokenInvalid", err)
	}
	after, err := store.UserByID(user.ID)
	if err != nil {
		t.Fatalf("read user after rejected reset: %v", err)
	}
	if !auth.Verify("old-password", after.PassHash, after.Salt) {
		t.Error("rejected reset changed the password")
	}

	// A token issued for the new address works normally.
	newToken := auth.NewToken()
	if err := store.CreateAuthTokenForEmail(
		newToken, user.ID, TokenResetPassword, "new@example.com", now.Add(3*time.Minute),
	); err != nil {
		t.Fatalf("create new-email reset token: %v", err)
	}
	if _, err := store.RotateUserPassword(
		user.ID, newHash, newSalt, now.Add(2*time.Minute),
		PasswordRotationOptions{ResetToken: newToken, VerifyCurrentEmail: true},
	); err != nil {
		t.Fatalf("reset with current-email token: %v", err)
	}
}

func TestCreateAuthTokenForEmailRejectsStaleSnapshotAtomically(t *testing.T) {
	store := openPlaybackTestStore(t)
	now := time.Date(2026, 10, 27, 17, 0, 0, 0, time.UTC)
	user := &models.User{
		Name: "reset-email-snapshot", PassHash: "hash", Salt: "salt",
		Role: models.RoleUser, Status: models.StatusActive,
	}
	if err := store.CreateUser(user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := store.SetUserEmail(user.ID, "current@example.com", true); err != nil {
		t.Fatalf("set email: %v", err)
	}
	existing := auth.NewToken()
	if err := store.CreateAuthTokenForEmail(
		existing, user.ID, TokenResetPassword, "current@example.com", now.Add(time.Hour),
	); err != nil {
		t.Fatalf("create existing token: %v", err)
	}
	if err := store.CreateAuthTokenForEmail(
		auth.NewToken(), user.ID, TokenResetPassword, "stale@example.com", now.Add(time.Hour),
	); !errors.Is(err, ErrAuthStateChanged) {
		t.Fatalf("stale snapshot error = %v, want ErrAuthStateChanged", err)
	}
	var existingRows int
	if err := store.DB.QueryRow(`SELECT COUNT(*) FROM auth_tokens WHERE token = ?`,
		existing).Scan(&existingRows); err != nil {
		t.Fatalf("count existing token: %v", err)
	}
	if existingRows != 1 {
		t.Fatalf("failed issuance deleted the existing token")
	}
}

func TestResetTokenBindingRollsBackWithPasswordRotation(t *testing.T) {
	store := openPlaybackTestStore(t)
	now := time.Date(2026, 10, 27, 17, 30, 0, 0, time.UTC)
	fixture := addPlaybackTestFixture(t, store, "reset-rollback", "reset-rollback", now)
	if err := store.SetUserEmail(fixture.user.ID, "rollback@example.com", true); err != nil {
		t.Fatalf("set user email: %v", err)
	}
	token := auth.NewToken()
	if err := store.CreateAuthTokenForEmail(
		token, fixture.user.ID, TokenResetPassword, "rollback@example.com", now.Add(time.Hour),
	); err != nil {
		t.Fatalf("create reset token: %v", err)
	}
	createDeviceAuthConcurrencyTokens(t, store, fixture.user, fixture.device, now)
	if _, err := store.DB.Exec(`CREATE TRIGGER injected_reset_revoke_failure
		BEFORE UPDATE OF revoked_at ON device_refresh_tokens
		BEGIN
			SELECT RAISE(ABORT, 'injected reset revocation failure');
		END`); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}
	if _, err := store.RotateUserPassword(
		fixture.user.ID, "new-hash", "new-salt", now.Add(time.Minute),
		PasswordRotationOptions{ResetToken: token, VerifyCurrentEmail: true},
	); err == nil {
		t.Fatal("password rotation unexpectedly succeeded despite injected failure")
	}
	var issuedEmail string
	if err := store.DB.QueryRow(`SELECT issued_email FROM auth_tokens
		WHERE token = ?`, token).Scan(&issuedEmail); err != nil {
		t.Fatalf("reset token did not survive rollback: %v", err)
	}
	if issuedEmail != "rollback@example.com" {
		t.Errorf("issued email after rollback = %q", issuedEmail)
	}
	if _, err := store.DB.Exec(`DROP TRIGGER injected_reset_revoke_failure`); err != nil {
		t.Fatalf("drop failure trigger: %v", err)
	}
	if _, err := store.RotateUserPassword(
		fixture.user.ID, "new-hash", "new-salt", now.Add(2*time.Minute),
		PasswordRotationOptions{ResetToken: token, VerifyCurrentEmail: true},
	); err != nil {
		t.Fatalf("retry reset after rollback: %v", err)
	}
}

func TestResetTokenIssuanceAndEmailChangeAreAtomic(t *testing.T) {
	first, second := openDeviceAuthConcurrencyStores(t)
	base := time.Date(2026, 10, 27, 18, 0, 0, 0, time.UTC)

	for i := 0; i < 8; i++ {
		now := base.Add(time.Duration(i) * time.Minute)
		user := &models.User{
			Name: fmt.Sprintf("reset-email-race-%d", i), PassHash: "hash", Salt: "salt",
			Role: models.RoleUser, Status: models.StatusActive,
		}
		if err := first.CreateUser(user); err != nil {
			t.Fatalf("iteration %d create user: %v", i, err)
		}
		oldEmail := fmt.Sprintf("old-%d@example.com", i)
		newEmail := fmt.Sprintf("new-%d@example.com", i)
		if err := first.SetUserEmail(user.ID, oldEmail, true); err != nil {
			t.Fatalf("iteration %d set old email: %v", i, err)
		}
		token := auth.NewToken()

		start := make(chan struct{})
		var wg sync.WaitGroup
		var issueErr, emailErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			issueErr = first.CreateAuthTokenForEmail(
				token, user.ID, TokenResetPassword, oldEmail, now.Add(time.Hour))
		}()
		go func() {
			defer wg.Done()
			<-start
			emailErr = second.SetUserEmail(user.ID, newEmail, false)
		}()
		close(start)
		wg.Wait()

		if emailErr != nil {
			t.Fatalf("iteration %d email change: %v", i, emailErr)
		}
		if issueErr != nil && !errors.Is(issueErr, ErrAuthStateChanged) {
			t.Fatalf("iteration %d token issue error = %v, want nil or ErrAuthStateChanged", i, issueErr)
		}
		newHash, newSalt := auth.HashPassword("new-password")
		if _, err := first.RotateUserPassword(
			user.ID, newHash, newSalt, now.Add(time.Second),
			PasswordRotationOptions{ResetToken: token, VerifyCurrentEmail: true},
		); !errors.Is(err, ErrPasswordResetTokenInvalid) {
			t.Fatalf("iteration %d stale reset error = %v, want ErrPasswordResetTokenInvalid", i, err)
		}
	}
}

func TestLegacyUnboundAuthTokensFailClosed(t *testing.T) {
	store := openPlaybackTestStore(t)
	now := time.Date(2026, 10, 27, 19, 0, 0, 0, time.UTC)
	hash, salt := auth.HashPassword("old-password")
	user := &models.User{
		Name: "legacy-unbound-token", PassHash: hash, Salt: salt,
		Role: models.RoleUser, Status: models.StatusActive,
	}
	if err := store.CreateUser(user); err != nil {
		t.Fatalf("create user: %v", err)
	}

	resetToken := auth.NewToken()
	verifyToken := auth.NewToken()
	for _, token := range []string{resetToken, verifyToken} {
		kind := TokenResetPassword
		if token == verifyToken {
			kind = TokenVerifyEmail
		}
		if _, err := store.DB.Exec(`INSERT INTO auth_tokens
			(token, user_id, kind, issued_email, expires_at, created_at)
			VALUES (?,?,?,'',?,?)`,
			token, user.ID, kind, now.Add(time.Hour), now); err != nil {
			t.Fatalf("insert legacy %s token: %v", kind, err)
		}
	}

	if _, err := store.RotateUserPassword(
		user.ID, "new-hash", "new-salt", now,
		PasswordRotationOptions{ResetToken: resetToken},
	); !errors.Is(err, ErrPasswordResetTokenInvalid) {
		t.Fatalf("legacy unbound reset token error = %v, want ErrPasswordResetTokenInvalid", err)
	}
	afterReset, err := store.UserByID(user.ID)
	if err != nil {
		t.Fatalf("read user after rejected reset: %v", err)
	}
	if !auth.Verify("old-password", afterReset.PassHash, afterReset.Salt) {
		t.Error("legacy unbound reset token changed the password")
	}

	if _, err := store.ConsumeEmailVerificationToken(
		verifyToken, now,
	); !errors.Is(err, ErrEmailVerificationTokenInvalid) {
		t.Fatalf("legacy unbound verify token error = %v, want ErrEmailVerificationTokenInvalid", err)
	}
	afterVerify, err := store.UserByID(user.ID)
	if err != nil {
		t.Fatalf("read user after rejected verification: %v", err)
	}
	if afterVerify.EmailVerified {
		t.Error("legacy unbound verify token marked an empty email verified")
	}
}

func TestEmailVerificationTokenBoundToCurrentEmail(t *testing.T) {
	store := openPlaybackTestStore(t)
	now := time.Date(2026, 10, 27, 19, 30, 0, 0, time.UTC)
	user := &models.User{
		Name: "verify-email-binding", PassHash: "hash", Salt: "salt",
		Role: models.RoleUser, Status: models.StatusActive,
	}
	if err := store.CreateUser(user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := store.SetUserEmail(user.ID, "old@example.com", false); err != nil {
		t.Fatalf("set old email: %v", err)
	}
	token := auth.NewToken()
	if err := store.CreateAuthTokenForEmail(
		token, user.ID, TokenVerifyEmail, "old@example.com", now.Add(time.Hour),
	); err != nil {
		t.Fatalf("create verification token: %v", err)
	}
	if err := store.SetUserEmail(user.ID, "new@example.com", false); err != nil {
		t.Fatalf("change email: %v", err)
	}
	if _, err := store.ConsumeEmailVerificationToken(
		token, now,
	); !errors.Is(err, ErrEmailVerificationTokenInvalid) {
		t.Fatalf("old-email verification error = %v, want ErrEmailVerificationTokenInvalid", err)
	}
	afterOld, err := store.UserByID(user.ID)
	if err != nil {
		t.Fatalf("read user after old-email verification: %v", err)
	}
	if afterOld.EmailVerified {
		t.Error("old-email verification marked the new email verified")
	}

	freshToken := auth.NewToken()
	if err := store.CreateAuthTokenForEmail(
		freshToken, user.ID, TokenVerifyEmail, "new@example.com", now.Add(time.Hour),
	); err != nil {
		t.Fatalf("create fresh verification token: %v", err)
	}
	if _, err := store.ConsumeEmailVerificationToken(freshToken, now); err != nil {
		t.Fatalf("consume current-email verification: %v", err)
	}
	afterNew, err := store.UserByID(user.ID)
	if err != nil {
		t.Fatalf("read verified user: %v", err)
	}
	if !afterNew.EmailVerified {
		t.Error("current-email verification did not mark the email verified")
	}
	if _, err := store.ConsumeEmailVerificationToken(
		freshToken, now,
	); !errors.Is(err, ErrEmailVerificationTokenInvalid) {
		t.Fatalf("second verification error = %v, want ErrEmailVerificationTokenInvalid", err)
	}
}

func TestEmailVerificationConsumeAndEmailChangeAreAtomic(t *testing.T) {
	first, second := openDeviceAuthConcurrencyStores(t)
	base := time.Date(2026, 10, 27, 20, 0, 0, 0, time.UTC)

	for i := 0; i < 8; i++ {
		now := base.Add(time.Duration(i) * time.Minute)
		user := &models.User{
			Name: fmt.Sprintf("verify-email-race-%d", i), PassHash: "hash", Salt: "salt",
			Role: models.RoleUser, Status: models.StatusActive,
		}
		if err := first.CreateUser(user); err != nil {
			t.Fatalf("iteration %d create user: %v", i, err)
		}
		if err := first.SetUserEmail(user.ID, "old@example.com", false); err != nil {
			t.Fatalf("iteration %d set old email: %v", i, err)
		}
		token := auth.NewToken()
		if err := first.CreateAuthTokenForEmail(
			token, user.ID, TokenVerifyEmail, "old@example.com", now.Add(time.Hour),
		); err != nil {
			t.Fatalf("iteration %d create verification token: %v", i, err)
		}

		start := make(chan struct{})
		var wg sync.WaitGroup
		var consumeErr, emailErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, consumeErr = first.ConsumeEmailVerificationToken(token, now)
		}()
		go func() {
			defer wg.Done()
			<-start
			emailErr = second.SetUserEmail(user.ID, "new@example.com", false)
		}()
		close(start)
		wg.Wait()

		if emailErr != nil {
			t.Fatalf("iteration %d email change: %v", i, emailErr)
		}
		if consumeErr != nil && !errors.Is(consumeErr, ErrEmailVerificationTokenInvalid) {
			t.Fatalf("iteration %d consume error = %v", i, consumeErr)
		}
		after, err := first.UserByID(user.ID)
		if err != nil {
			t.Fatalf("iteration %d read user: %v", i, err)
		}
		if after.Email != "new@example.com" || after.EmailVerified {
			t.Fatalf("iteration %d final email state = %q verified=%v",
				i, after.Email, after.EmailVerified)
		}
	}
}
