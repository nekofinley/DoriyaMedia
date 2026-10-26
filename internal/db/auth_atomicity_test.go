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

func TestCreateSessionIfAuthCurrentRejectsStalePasswordSnapshot(t *testing.T) {
	store := openPlaybackTestStore(t)
	now := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	oldHash, oldSalt := auth.HashPassword("old-password")
	user := &models.User{
		Name: "login-epoch", PassHash: oldHash, Salt: oldSalt,
		Role: models.RoleUser, Status: models.StatusActive,
	}
	if err := store.CreateUser(user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	snapshot, oldEpoch, err := store.UserAuthStateByName(user.Name)
	if err != nil {
		t.Fatalf("read login snapshot: %v", err)
	}
	if !auth.Verify("old-password", snapshot.PassHash, snapshot.Salt) {
		t.Fatal("login snapshot did not contain the old password")
	}

	newHash, newSalt := auth.HashPassword("new-password")
	if _, err := store.RotateUserPassword(
		user.ID, newHash, newSalt, now, PasswordRotationOptions{},
	); err != nil {
		t.Fatalf("rotate password: %v", err)
	}
	if err := store.CreateSessionIfAuthCurrent(
		"stale-login-session", user.ID, oldEpoch, now.Add(time.Hour),
	); !errors.Is(err, ErrAuthStateChanged) {
		t.Fatalf("stale login session error = %v, want ErrAuthStateChanged", err)
	}
	if _, err := store.SessionByToken("stale-login-session"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("stale session lookup error = %v, want sql.ErrNoRows", err)
	}

	_, currentEpoch, err := store.UserAuthStateByName(user.Name)
	if err != nil {
		t.Fatalf("read current auth state: %v", err)
	}
	if currentEpoch <= oldEpoch {
		t.Fatalf("auth epoch = %d after rotation, want > %d", currentEpoch, oldEpoch)
	}
	if err := store.CreateSessionIfAuthCurrent(
		"current-login-session", user.ID, currentEpoch, now.Add(time.Hour),
	); err != nil {
		t.Fatalf("create session at current epoch: %v", err)
	}
	if _, err := store.SessionByToken("current-login-session"); err != nil {
		t.Fatalf("current session lookup: %v", err)
	}
}

func TestStaleWebSessionEpochIsNotRevivedByRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session-epoch-restart.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	hash, salt := auth.HashPassword("restart-password")
	user := &models.User{
		Name: "restart-epoch", PassHash: hash, Salt: salt,
		Role: models.RoleUser, Status: models.StatusActive,
	}
	if err := store.CreateUser(user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := store.CreateSession("restart-session", user.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := store.DB.Exec(`UPDATE sessions SET auth_epoch = 0
		WHERE token = 'restart-session'`); err != nil {
		t.Fatalf("make session stale: %v", err)
	}
	if err := store.DB.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	store, err = Open(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer store.DB.Close()
	var epoch int64
	if err := store.DB.QueryRow(`SELECT auth_epoch FROM sessions
		WHERE token = 'restart-session'`).Scan(&epoch); err != nil {
		t.Fatalf("read stale session epoch after restart: %v", err)
	}
	if epoch != 0 {
		t.Fatalf("stale session epoch after restart = %d, want 0", epoch)
	}
	if _, err := store.SessionByToken("restart-session"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("stale session after restart error = %v, want sql.ErrNoRows", err)
	}
}

func TestConcurrentPasswordRotationCannotLeaveStaleWebSession(t *testing.T) {
	first, second := openDeviceAuthConcurrencyStores(t)
	base := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	oldHash, oldSalt := auth.HashPassword("race-old-password")
	newHash, newSalt := auth.HashPassword("race-new-password")

	for i := 0; i < 12; i++ {
		now := base.Add(time.Duration(i) * time.Minute)
		user := &models.User{
			Name:     fmt.Sprintf("web-login-race-%d", i),
			PassHash: oldHash, Salt: oldSalt,
			Role: models.RoleUser, Status: models.StatusActive,
		}
		if err := first.CreateUser(user); err != nil {
			t.Fatalf("iteration %d create user: %v", i, err)
		}
		snapshot, epoch, err := first.UserAuthStateByName(user.Name)
		if err != nil {
			t.Fatalf("iteration %d read login snapshot: %v", i, err)
		}
		if !auth.Verify("race-old-password", snapshot.PassHash, snapshot.Salt) {
			t.Fatalf("iteration %d snapshot has the wrong password", i)
		}
		token := fmt.Sprintf("web-login-race-session-%d", i)
		start := make(chan struct{})
		var (
			wg          sync.WaitGroup
			passwordErr error
			sessionErr  error
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, passwordErr = first.RotateUserPassword(
				user.ID, newHash, newSalt, now.Add(time.Second),
				PasswordRotationOptions{})
		}()
		go func() {
			defer wg.Done()
			<-start
			sessionErr = second.CreateSessionIfAuthCurrent(
				token, user.ID, epoch, now.Add(time.Hour))
		}()
		close(start)
		wg.Wait()

		if passwordErr != nil {
			t.Fatalf("iteration %d password rotation: %v", i, passwordErr)
		}
		if sessionErr != nil && !errors.Is(sessionErr, ErrAuthStateChanged) {
			t.Fatalf("iteration %d session creation error = %v", i, sessionErr)
		}
		if _, err := first.SessionByToken(token); err == nil {
			t.Fatalf("iteration %d stale login session remained usable", i)
		}
	}
}

type authAtomicFixture struct {
	playback playbackTestFixture
	pair     *models.DeviceTokenPair
	grants   playbackConcurrencyGrants
	session  string
	epoch    int64
}

func addAuthAtomicFixture(t *testing.T, store *Store, name string, now time.Time) authAtomicFixture {
	t.Helper()
	fixture := addPlaybackTestFixture(t, store, name, name, now)
	pair := createDeviceAuthConcurrencyTokens(t, store, fixture.user, fixture.device, now)
	grants := createPlaybackConcurrencyGrants(t, store, fixture, now.Add(time.Second))
	session := name + "-web-session"
	if err := store.CreateSession(session, fixture.user.ID, now.Add(24*time.Hour)); err != nil {
		t.Fatalf("create Web session: %v", err)
	}
	var epoch int64
	if err := store.DB.QueryRow(`SELECT auth_epoch FROM users WHERE id = ?`,
		fixture.user.ID).Scan(&epoch); err != nil {
		t.Fatalf("read auth epoch: %v", err)
	}
	return authAtomicFixture{
		playback: fixture, pair: pair, grants: grants, session: session, epoch: epoch,
	}
}

func assertAuthAtomicActive(t *testing.T, store *Store, f authAtomicFixture, now time.Time) {
	t.Helper()
	if _, err := store.SessionByToken(f.session); err != nil {
		t.Errorf("Web session is not active: %v", err)
	}
	if _, err := store.DeviceByAccessToken(f.pair.AccessToken, now.Add(5*time.Minute)); err != nil {
		t.Errorf("device access token is not active: %v", err)
	}
	for _, check := range []struct {
		label string
		query string
		args  []any
	}{
		{"device", `SELECT COUNT(*) FROM devices WHERE id = ? AND revoked_at IS NULL`,
			[]any{f.playback.device.ID}},
		{"access token", `SELECT COUNT(*) FROM device_access_tokens
			WHERE device_id = ? AND revoked_at IS NULL`, []any{f.playback.device.ID}},
		{"refresh token", `SELECT COUNT(*) FROM device_refresh_tokens
			WHERE device_id = ? AND revoked_at IS NULL`, []any{f.playback.device.ID}},
		{"ticket", `SELECT COUNT(*) FROM playback_tickets
			WHERE ticket_id = ? AND state = 'consuming'`, []any{f.grants.ticket.TicketID}},
		{"media", `SELECT COUNT(*) FROM playback_media_sessions
			WHERE id = ? AND state = 'active'`, []any{f.grants.media.Session.ID}},
		{"subtitle", `SELECT COUNT(*) FROM playback_subtitle_sessions
			WHERE id = ? AND state = 'active'`, []any{f.grants.subtitle.Session.ID}},
	} {
		var got int
		if err := store.DB.QueryRow(check.query, check.args...).Scan(&got); err != nil {
			t.Fatalf("check active %s: %v", check.label, err)
		}
		if got != 1 {
			t.Errorf("%s active rows = %d, want 1", check.label, got)
		}
	}
}

func assertAuthAtomicRevoked(t *testing.T, store *Store, f authAtomicFixture, now time.Time) {
	t.Helper()
	if _, err := store.SessionByToken(f.session); err == nil {
		t.Error("Web session survived revocation")
	}
	if _, err := store.DeviceByAccessToken(f.pair.AccessToken, now.Add(5*time.Minute)); err == nil {
		t.Error("device access token survived revocation")
	}
	assertDeviceAuthTokensRevoked(t, store, f.playback.device.ID)
	assertDeviceAuthRowsRevoked(t, store, f.playback.device.ID)
	var deviceRevoked int
	if err := store.DB.QueryRow(`SELECT COUNT(*) FROM devices
		WHERE id = ? AND revoked_at IS NOT NULL`, f.playback.device.ID).
		Scan(&deviceRevoked); err != nil {
		t.Fatalf("read device revocation: %v", err)
	}
	if deviceRevoked != 1 {
		t.Error("device row was not revoked")
	}
}

func TestUpdateUserStatusAndRevokeIsAtomic(t *testing.T) {
	store := openPlaybackTestStore(t)
	now := time.Date(2026, 10, 27, 11, 0, 0, 0, time.UTC)
	f := addAuthAtomicFixture(t, store, "atomic-disable", now)

	if _, err := store.DB.Exec(`CREATE TRIGGER injected_disable_revoke_failure
		BEFORE UPDATE ON device_refresh_tokens
		BEGIN
			SELECT RAISE(ABORT, 'injected disable revocation failure');
		END`); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}
	if _, err := store.UpdateUserStatusAndRevoke(
		f.playback.user.ID, models.StatusDisabled, now.Add(time.Minute),
	); err == nil {
		t.Fatal("disable unexpectedly succeeded despite injected revocation failure")
	}
	user, err := store.UserByID(f.playback.user.ID)
	if err != nil {
		t.Fatalf("read user after failed disable: %v", err)
	}
	if user.Status != models.StatusActive {
		t.Errorf("status after failed disable = %q, want active", user.Status)
	}
	var epoch int64
	if err := store.DB.QueryRow(`SELECT auth_epoch FROM users WHERE id = ?`,
		f.playback.user.ID).Scan(&epoch); err != nil {
		t.Fatalf("read epoch after failed disable: %v", err)
	}
	if epoch != f.epoch {
		t.Errorf("epoch after failed disable = %d, want %d", epoch, f.epoch)
	}
	assertAuthAtomicActive(t, store, f, now)

	if _, err := store.DB.Exec(`DROP TRIGGER injected_disable_revoke_failure`); err != nil {
		t.Fatalf("drop failure trigger: %v", err)
	}
	if _, err := store.UpdateUserStatusAndRevoke(
		f.playback.user.ID, models.StatusDisabled, now.Add(2*time.Minute),
	); err != nil {
		t.Fatalf("disable user: %v", err)
	}
	assertAuthAtomicRevoked(t, store, f, now.Add(2*time.Minute))

	if _, err := store.UpdateUserStatusAndRevoke(
		f.playback.user.ID, models.StatusActive, now.Add(3*time.Minute),
	); err != nil {
		t.Fatalf("re-enable user: %v", err)
	}
	user, err = store.UserByID(f.playback.user.ID)
	if err != nil || user.Status != models.StatusActive {
		t.Fatalf("user after re-enable = %+v, err %v", user, err)
	}
	if _, err := store.SessionByToken(f.session); err == nil {
		t.Error("old Web session revived after re-enable")
	}
	if _, err := store.DeviceByAccessToken(f.pair.AccessToken, now.Add(4*time.Minute)); err == nil {
		t.Error("old device token revived after re-enable")
	}
	_, currentEpoch, err := store.UserAuthStateByName(f.playback.user.Name)
	if err != nil {
		t.Fatalf("read current epoch after re-enable: %v", err)
	}
	if err := store.CreateSessionIfAuthCurrent(
		"post-enable-session", f.playback.user.ID, currentEpoch, now.Add(time.Hour),
	); err != nil {
		t.Fatalf("create post-enable session: %v", err)
	}
	if _, err := store.SessionByToken("post-enable-session"); err != nil {
		t.Errorf("post-enable session unavailable: %v", err)
	}
}

func TestRevokeUserSessionsAndDevicesIsAtomic(t *testing.T) {
	store := openPlaybackTestStore(t)
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	f := addAuthAtomicFixture(t, store, "atomic-kick", now)
	if err := store.CreateSession("actor-session", f.playback.user.ID, now.Add(24*time.Hour)); err != nil {
		t.Fatalf("create actor Web session: %v", err)
	}

	if _, err := store.DB.Exec(`CREATE TRIGGER injected_kick_revoke_failure
		BEFORE UPDATE ON device_refresh_tokens
		BEGIN
			SELECT RAISE(ABORT, 'injected kick revocation failure');
		END`); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}
	if _, err := store.RevokeUserSessionsAndDevices(
		f.playback.user.ID, "actor-session", now.Add(time.Minute),
	); err == nil {
		t.Fatal("kick unexpectedly succeeded despite injected revocation failure")
	}
	if _, err := store.SessionByToken(f.session); err != nil {
		t.Errorf("ordinary session after failed kick: %v", err)
	}
	if _, err := store.SessionByToken("actor-session"); err != nil {
		t.Errorf("preserved session after failed kick: %v", err)
	}
	var epoch int64
	if err := store.DB.QueryRow(`SELECT auth_epoch FROM users WHERE id = ?`,
		f.playback.user.ID).Scan(&epoch); err != nil {
		t.Fatalf("read epoch after failed kick: %v", err)
	}
	if epoch != f.epoch {
		t.Errorf("epoch after failed kick = %d, want %d", epoch, f.epoch)
	}
	assertAuthAtomicActive(t, store, f, now)

	if _, err := store.DB.Exec(`DROP TRIGGER injected_kick_revoke_failure`); err != nil {
		t.Fatalf("drop failure trigger: %v", err)
	}
	if _, err := store.RevokeUserSessionsAndDevices(
		f.playback.user.ID, "actor-session", now.Add(2*time.Minute),
	); err != nil {
		t.Fatalf("kick user: %v", err)
	}
	if _, err := store.SessionByToken(f.session); err == nil {
		t.Error("ordinary Web session survived kick")
	}
	if _, err := store.SessionByToken("actor-session"); err != nil {
		t.Errorf("preserved actor session after kick: %v", err)
	}
	var preservedEpoch, userEpoch int64
	if err := store.DB.QueryRow(`SELECT auth_epoch FROM sessions
		WHERE token = 'actor-session'`).Scan(&preservedEpoch); err != nil {
		t.Fatalf("read preserved session epoch: %v", err)
	}
	if err := store.DB.QueryRow(`SELECT auth_epoch FROM users WHERE id = ?`,
		f.playback.user.ID).Scan(&userEpoch); err != nil {
		t.Fatalf("read user epoch after kick: %v", err)
	}
	if preservedEpoch != userEpoch || userEpoch <= f.epoch {
		t.Errorf("preserved/user epoch after kick = %d/%d, prior %d",
			preservedEpoch, userEpoch, f.epoch)
	}
	assertAuthAtomicRevoked(t, store, f, now.Add(2*time.Minute))
}
