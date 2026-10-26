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

func TestAuthEpochMigrationPreservesLegacySessionsAndTokens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-auth-epoch.db")
	legacy, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	now := time.Date(2026, 10, 26, 20, 0, 0, 0, time.UTC)
	for _, stmt := range []string{
		`CREATE TABLE users (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT UNIQUE NOT NULL,
			pass_hash TEXT NOT NULL,
			salt TEXT NOT NULL,
			role TEXT NOT NULL DEFAULT 'user',
			created_at TIMESTAMP NOT NULL
		)`,
		`CREATE TABLE sessions (
			token TEXT PRIMARY KEY,
			user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			expires_at TIMESTAMP NOT NULL
		)`,
		`CREATE TABLE device_access_tokens (
			token_hash TEXT PRIMARY KEY,
			user_id INTEGER NOT NULL,
			device_id INTEGER NOT NULL,
			family_id TEXT NOT NULL,
			expires_at TIMESTAMP NOT NULL,
			created_at TIMESTAMP NOT NULL,
			last_used_at TIMESTAMP,
			revoked_at TIMESTAMP,
			revoked_reason TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE device_refresh_tokens (
			token_hash TEXT PRIMARY KEY,
			user_id INTEGER NOT NULL,
			device_id INTEGER NOT NULL,
			family_id TEXT NOT NULL,
			expires_at TIMESTAMP NOT NULL,
			created_at TIMESTAMP NOT NULL,
			rotated_at TIMESTAMP,
			revoked_at TIMESTAMP,
			replaced_by TEXT NOT NULL DEFAULT '',
			revoked_reason TEXT NOT NULL DEFAULT ''
		)`,
	} {
		if _, err := legacy.Exec(stmt); err != nil {
			t.Fatalf("create legacy schema: %v", err)
		}
	}
	if _, err := legacy.Exec(`INSERT INTO users
		(name, pass_hash, salt, role, created_at) VALUES (?,?,?,?,?)`,
		"legacy", "old-hash", "old-salt", models.RoleUser, now); err != nil {
		t.Fatalf("insert legacy user: %v", err)
	}
	if _, err := legacy.Exec(`INSERT INTO sessions (token, user_id, expires_at)
		VALUES (?,?,?)`, "legacy-web-session", 1, now.Add(24*time.Hour)); err != nil {
		t.Fatalf("insert legacy session: %v", err)
	}
	for _, stmt := range []string{
		`INSERT INTO device_access_tokens
			(token_hash, user_id, device_id, family_id, expires_at, created_at)
			VALUES ('legacy-access-hash', 1, 1, 'fam_legacy', ?, ?)`,
		`INSERT INTO device_refresh_tokens
			(token_hash, user_id, device_id, family_id, expires_at, created_at)
			VALUES ('legacy-refresh-hash', 1, 1, 'fam_legacy', ?, ?)`,
	} {
		if _, err := legacy.Exec(stmt, now.Add(24*time.Hour), now); err != nil {
			t.Fatalf("insert legacy device token: %v", err)
		}
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatalf("migrate legacy auth database: %v", err)
	}
	for _, check := range []struct {
		query string
		want  int64
	}{
		{`SELECT auth_epoch FROM users WHERE id = 1`, 1},
		{`SELECT auth_epoch FROM device_access_tokens WHERE token_hash = 'legacy-access-hash'`, 1},
		{`SELECT auth_epoch FROM device_refresh_tokens WHERE token_hash = 'legacy-refresh-hash'`, 1},
		{`SELECT auth_epoch FROM sessions WHERE token = 'legacy-web-session'`, 1},
		{`SELECT COUNT(*) FROM sessions WHERE token = 'legacy-web-session'`, 1},
	} {
		var got int64
		if err := store.DB.QueryRow(check.query).Scan(&got); err != nil {
			t.Fatalf("read migrated value: %v", err)
		}
		if got != check.want {
			t.Errorf("%s = %d, want %d", check.query, got, check.want)
		}
	}
	if _, err := store.SessionByToken("legacy-web-session"); err != nil {
		t.Fatalf("legacy Web session after upgrade: %v", err)
	}
	if _, err := store.DB.Exec(`UPDATE users SET auth_epoch = 5 WHERE id = 1`); err != nil {
		t.Fatalf("advance migrated epoch: %v", err)
	}
	for _, table := range []string{"device_access_tokens", "device_refresh_tokens", "sessions"} {
		if _, err := store.DB.Exec(`UPDATE ` + table + ` SET auth_epoch = 5`); err != nil {
			t.Fatalf("advance %s epoch: %v", table, err)
		}
	}
	if err := store.DB.Close(); err != nil {
		t.Fatalf("close migrated store: %v", err)
	}

	store, err = Open(path)
	if err != nil {
		t.Fatalf("repeat auth epoch migration: %v", err)
	}
	defer store.DB.Close()
	for _, table := range []string{"device_access_tokens", "device_refresh_tokens", "sessions"} {
		var epoch int64
		if err := store.DB.QueryRow(`SELECT auth_epoch FROM ` + table + ` LIMIT 1`).Scan(&epoch); err != nil {
			t.Fatalf("read repeated migration %s: %v", table, err)
		}
		if epoch != 5 {
			t.Errorf("%s.auth_epoch after repeat migration = %d, want 5", table, epoch)
		}
	}
	if _, err := store.SessionByToken("legacy-web-session"); err != nil {
		t.Errorf("legacy Web session after repeated migration: %v", err)
	}
}

func TestRotateUserPasswordRollsBackEveryRevocationOnFailure(t *testing.T) {
	store := openPlaybackTestStore(t)
	now := time.Date(2026, 10, 26, 21, 0, 0, 0, time.UTC)
	fixture := addPlaybackTestFixture(t, store, "atomic-user", "atomic-user", now)
	pair := createDeviceAuthConcurrencyTokens(t, store, fixture.user, fixture.device, now)
	grant := createPlaybackConcurrencyGrants(t, store, fixture, now.Add(time.Second))

	const sessionToken = "atomic-web-session"
	if err := store.CreateSession(sessionToken, fixture.user.ID, now.Add(24*time.Hour)); err != nil {
		t.Fatalf("create Web session: %v", err)
	}
	if err := store.SetUserEmail(fixture.user.ID, "atomic@example.com", false); err != nil {
		t.Fatalf("set user email: %v", err)
	}
	resetToken := auth.NewToken()
	if err := store.CreateAuthToken(resetToken, fixture.user.ID, TokenResetPassword, now.Add(time.Hour)); err != nil {
		t.Fatalf("create reset token: %v", err)
	}

	var beforeHash, beforeSalt string
	var beforeEpoch int64
	if err := store.DB.QueryRow(`SELECT pass_hash, salt, auth_epoch FROM users WHERE id = ?`,
		fixture.user.ID).Scan(&beforeHash, &beforeSalt, &beforeEpoch); err != nil {
		t.Fatalf("read credentials before rotation: %v", err)
	}

	if _, err := store.DB.Exec(`CREATE TRIGGER injected_password_revoke_failure
		BEFORE UPDATE OF state ON playback_subtitle_sessions
		BEGIN
			SELECT RAISE(ABORT, 'injected password revocation failure');
		END`); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}
	_, err := store.RotateUserPassword(
		fixture.user.ID,
		"new-hash",
		"new-salt",
		now.Add(2*time.Minute),
		PasswordRotationOptions{
			ResetToken:         resetToken,
			VerifyCurrentEmail: true,
		},
	)
	if err == nil {
		t.Fatal("password rotation unexpectedly succeeded despite injected revocation failure")
	}

	var afterHash, afterSalt string
	var afterEpoch int64
	var emailVerified bool
	if err := store.DB.QueryRow(`SELECT pass_hash, salt, auth_epoch, email_verified
		FROM users WHERE id = ?`, fixture.user.ID).
		Scan(&afterHash, &afterSalt, &afterEpoch, &emailVerified); err != nil {
		t.Fatalf("read credentials after failed rotation: %v", err)
	}
	if afterHash != beforeHash || afterSalt != beforeSalt || afterEpoch != beforeEpoch {
		t.Errorf("failed rotation committed credentials: hash=%q salt=%q epoch=%d, want %q/%q/%d",
			afterHash, afterSalt, afterEpoch, beforeHash, beforeSalt, beforeEpoch)
	}
	if emailVerified {
		t.Error("failed rotation committed email verification")
	}
	for _, check := range []struct {
		query string
		want  int
	}{
		{`SELECT COUNT(*) FROM sessions WHERE token = '` + sessionToken + `'`, 1},
		{`SELECT COUNT(*) FROM auth_tokens WHERE token = '` + resetToken + `'`, 1},
		{`SELECT COUNT(*) FROM device_access_tokens WHERE device_id = ` +
			fmt.Sprint(fixture.device.ID) + ` AND revoked_at IS NULL`, 1},
		{`SELECT COUNT(*) FROM device_refresh_tokens WHERE device_id = ` +
			fmt.Sprint(fixture.device.ID) + ` AND revoked_at IS NULL`, 1},
		{`SELECT COUNT(*) FROM devices WHERE id = ` + fmt.Sprint(fixture.device.ID) +
			` AND revoked_at IS NULL`, 1},
		{`SELECT COUNT(*) FROM playback_tickets WHERE ticket_id = '` + grant.ticket.TicketID +
			`' AND state = 'consuming'`, 1},
		{`SELECT COUNT(*) FROM playback_media_sessions WHERE id = ` +
			fmt.Sprint(grant.media.Session.ID) + ` AND state = 'active'`, 1},
		{`SELECT COUNT(*) FROM playback_subtitle_sessions WHERE id = ` +
			fmt.Sprint(grant.subtitle.Session.ID) + ` AND state = 'active'`, 1},
	} {
		var got int
		if err := store.DB.QueryRow(check.query).Scan(&got); err != nil {
			t.Fatalf("check rollback (%s): %v", check.query, err)
		}
		if got != check.want {
			t.Errorf("rollback check %q = %d, want %d", check.query, got, check.want)
		}
	}
	if _, err := store.DeviceByAccessToken(pair.AccessToken, now.Add(3*time.Minute)); err != nil {
		t.Errorf("failed rotation invalidated the existing access token: %v", err)
	}
}

func TestRotateUserPasswordRejectsOldEpochTokens(t *testing.T) {
	store := openPlaybackTestStore(t)
	now := time.Date(2026, 10, 26, 22, 0, 0, 0, time.UTC)
	fixture := addPlaybackTestFixture(t, store, "epoch-user", "epoch-user", now)
	oldPair := createDeviceAuthConcurrencyTokens(t, store, fixture.user, fixture.device, now)

	if _, err := store.RotateUserPassword(
		fixture.user.ID, "new-hash", "new-salt", now.Add(time.Minute), PasswordRotationOptions{},
	); err != nil {
		t.Fatalf("rotate password: %v", err)
	}
	// Remove the explicit revocations so this test proves the epoch check is an
	// independent fail-closed boundary, not merely a consequence of revoked_at.
	if _, err := store.DB.Exec(`UPDATE devices SET revoked_at = NULL WHERE id = ?`,
		fixture.device.ID); err != nil {
		t.Fatalf("unrevoke test device: %v", err)
	}
	for _, table := range []string{"device_access_tokens", "device_refresh_tokens"} {
		if _, err := store.DB.Exec(`UPDATE `+table+`
			SET revoked_at = NULL, revoked_reason = '' WHERE device_id = ?`,
			fixture.device.ID); err != nil {
			t.Fatalf("unrevoke test %s: %v", table, err)
		}
	}

	if _, err := store.DeviceByAccessToken(oldPair.AccessToken, now.Add(2*time.Minute)); !errors.Is(err, ErrDeviceRevoked) {
		t.Errorf("old access token error = %v, want ErrDeviceRevoked", err)
	}
	if _, err := store.RotateDeviceRefreshToken(oldPair.RefreshToken, now.Add(2*time.Minute)); !errors.Is(err, ErrDeviceRevoked) {
		t.Errorf("old refresh token error = %v, want ErrDeviceRevoked", err)
	}

	freshPair := createDeviceAuthConcurrencyTokens(
		t, store, fixture.user, fixture.device, now.Add(3*time.Minute))
	if _, err := store.DeviceByAccessToken(freshPair.AccessToken, now.Add(4*time.Minute)); err != nil {
		t.Errorf("fresh-epoch access token was rejected: %v", err)
	}
}

func TestRotateUserPasswordConcurrentRefreshCannotOutliveEpoch(t *testing.T) {
	first, second := openDeviceAuthConcurrencyStores(t)
	base := time.Date(2026, 10, 26, 23, 0, 0, 0, time.UTC)

	for i := 0; i < 6; i++ {
		now := base.Add(time.Duration(i) * time.Minute)
		name := fmt.Sprintf("epoch-race-%d", i)
		fixture := addPlaybackTestFixture(t, first, name, name, now)
		oldPair := createDeviceAuthConcurrencyTokens(t, first, fixture.user, fixture.device, now)

		start := make(chan struct{})
		var wg sync.WaitGroup
		var passwordErr, refreshErr error
		var refreshed *models.DeviceTokenPair
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, passwordErr = first.RotateUserPassword(
				fixture.user.ID, "rotated-hash", "rotated-salt",
				now.Add(time.Second), PasswordRotationOptions{})
		}()
		go func() {
			defer wg.Done()
			<-start
			refreshed, refreshErr = second.RotateDeviceRefreshToken(
				oldPair.RefreshToken, now.Add(time.Second))
		}()
		close(start)
		wg.Wait()

		if passwordErr != nil {
			t.Fatalf("iteration %d password rotation: %v", i, passwordErr)
		}
		_, oldAccessErr := first.DeviceByAccessToken(oldPair.AccessToken, now.Add(2*time.Second))
		if err := deviceCredentialMustBeRejected(oldAccessErr, "old access token"); err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		if _, err := first.RotateDeviceRefreshToken(
			oldPair.RefreshToken, now.Add(2*time.Second)); err == nil {
			t.Fatalf("iteration %d old refresh token survived password rotation", i)
		}
		if refreshErr == nil && refreshed != nil {
			_, refreshedAccessErr := first.DeviceByAccessToken(
				refreshed.AccessToken, now.Add(2*time.Second))
			if err := deviceCredentialMustBeRejected(
				refreshedAccessErr, "concurrent refreshed access token"); err != nil {
				t.Fatalf("iteration %d: %v", i, err)
			}
			if _, err := first.RotateDeviceRefreshToken(
				refreshed.RefreshToken, now.Add(2*time.Second)); err == nil {
				t.Fatalf("iteration %d concurrent refreshed token survived password rotation", i)
			}
		}
	}
}

func deviceCredentialMustBeRejected(err error, label string) error {
	if err == nil {
		return fmt.Errorf("%s was accepted after password rotation", label)
	}
	if !errors.Is(err, ErrDeviceRevoked) && !errors.Is(err, ErrDeviceAccessTokenRevoked) {
		return fmt.Errorf("%s error = %v, want revoked", label, err)
	}
	return nil
}
