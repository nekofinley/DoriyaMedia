package db

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"doriyamedia/internal/auth"
	"doriyamedia/internal/models"
)

func TestDeviceAuthMigrationCreatesTables(t *testing.T) {
	store := openDeviceAuthStore(t)

	for _, table := range []string{
		"device_authorizations", "devices", "device_access_tokens", "device_refresh_tokens",
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
	for _, index := range []string{
		"idx_device_authorizations_device_code",
		"idx_device_authorizations_public_id",
		"idx_device_authorizations_user_code",
		"idx_devices_public_id",
		"idx_device_access_tokens_family",
		"idx_device_refresh_tokens_family",
	} {
		var count int
		if err := store.DB.QueryRow(`SELECT COUNT(*) FROM sqlite_master
			WHERE type = 'index' AND name = ?`, index).Scan(&count); err != nil {
			t.Fatalf("inspect index %s: %v", index, err)
		}
		if count != 1 {
			t.Errorf("index %s was not created", index)
		}
	}
}

func TestDeviceAuthMigrationBackfillsPublicAuthorizationID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-device-auth.db")
	old, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	now := time.Date(2026, 10, 26, 7, 0, 0, 0, time.UTC)
	if _, err := old.Exec(`CREATE TABLE device_authorizations (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		device_code_hash TEXT NOT NULL,
		user_code_hash TEXT NOT NULL,
		device_name TEXT NOT NULL,
		platform TEXT NOT NULL,
		client_version TEXT NOT NULL,
		code_challenge TEXT NOT NULL,
		code_challenge_method TEXT NOT NULL,
		state TEXT NOT NULL,
		user_id INTEGER,
		created_at TIMESTAMP NOT NULL,
		expires_at TIMESTAMP NOT NULL,
		updated_at TIMESTAMP NOT NULL,
		authorized_at TIMESTAMP,
		consumed_at TIMESTAMP
	)`); err != nil {
		t.Fatalf("create legacy authorization table: %v", err)
	}
	if _, err := old.Exec(`INSERT INTO device_authorizations
		(device_code_hash, user_code_hash, device_name, platform, client_version,
		 code_challenge, code_challenge_method, state, created_at, expires_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		"device-hash", "user-hash", "Legacy PC", "windows", "0.0.9",
		"E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM", "S256", "pending",
		now, now.Add(auth.DeviceAuthorizationTTL), now); err != nil {
		t.Fatalf("insert legacy authorization: %v", err)
	}
	if err := old.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatalf("migrate legacy device authorization: %v", err)
	}
	defer store.DB.Close()
	var publicID string
	if err := store.DB.QueryRow(`SELECT authorization_id FROM device_authorizations
		WHERE device_name = 'Legacy PC'`).Scan(&publicID); err != nil {
		t.Fatalf("read backfilled authorization id: %v", err)
	}
	if !strings.HasPrefix(publicID, "dva_") {
		t.Fatalf("backfilled authorization id = %q", publicID)
	}
	authorization, err := store.DeviceAuthorizationByID(publicID, now)
	if err != nil {
		t.Fatalf("lookup backfilled authorization: %v", err)
	}
	if authorization.DeviceName != "Legacy PC" {
		t.Fatalf("backfilled authorization = %+v", authorization)
	}
}

func TestDeviceAuthSecurityMigrationBackfillsLegacyTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-device-security.db")
	old, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	now := time.Date(2026, 10, 26, 7, 30, 0, 0, time.UTC)
	for _, stmt := range []string{
		`CREATE TABLE device_authorizations (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			device_code_hash TEXT NOT NULL,
			user_code_hash TEXT NOT NULL,
			device_name TEXT NOT NULL,
			platform TEXT NOT NULL,
			client_version TEXT NOT NULL,
			code_challenge TEXT NOT NULL,
			code_challenge_method TEXT NOT NULL,
			state TEXT NOT NULL,
			user_id INTEGER,
			created_at TIMESTAMP NOT NULL,
			expires_at TIMESTAMP NOT NULL,
			updated_at TIMESTAMP NOT NULL,
			authorized_at TIMESTAMP,
			consumed_at TIMESTAMP
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
		if _, err := old.Exec(stmt); err != nil {
			t.Fatalf("create legacy table: %v", err)
		}
	}
	if _, err := old.Exec(`INSERT INTO device_refresh_tokens
		(token_hash, user_id, device_id, family_id, expires_at, created_at)
		VALUES (?,?,?,?,?,?)`,
		"legacy-refresh-hash", 1, 1, "fam_legacy", now.Add(30*24*time.Hour), now); err != nil {
		t.Fatalf("insert legacy refresh row: %v", err)
	}
	if err := old.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatalf("open legacy device security database: %v", err)
	}
	defer store.DB.Close()
	if err := store.MigrateDeviceAuthSecurityColumns(); err != nil {
		t.Fatalf("migrate security columns: %v", err)
	}
	if err := store.MigrateDeviceAuthSecurityColumns(); err != nil {
		t.Fatalf("repeat security migration: %v", err)
	}

	for _, column := range []string{
		"auth_epoch", "csrf_token_hash", "csrf_user_id", "csrf_expires_at", "csrf_consumed_at",
	} {
		has, err := store.hasColumn("device_authorizations", column)
		if err != nil {
			t.Fatalf("inspect device_authorizations.%s: %v", column, err)
		}
		if !has {
			t.Errorf("device_authorizations.%s was not added", column)
		}
	}
	hasFamilyDeadline, err := store.hasColumn("device_refresh_tokens", "family_absolute_expires_at")
	if err != nil {
		t.Fatalf("inspect refresh family deadline: %v", err)
	}
	if !hasFamilyDeadline {
		t.Fatal("device_refresh_tokens.family_absolute_expires_at was not added")
	}
	var familyDeadline time.Time
	if err := store.DB.QueryRow(`SELECT family_absolute_expires_at
		FROM device_refresh_tokens WHERE token_hash = ?`, "legacy-refresh-hash").
		Scan(&familyDeadline); err != nil {
		t.Fatalf("read migrated family deadline: %v", err)
	}
	if !familyDeadline.Equal(now.Add(auth.DeviceRefreshTokenFamilyTTL)) {
		t.Errorf("migrated family deadline = %v, want %v", familyDeadline, now.Add(auth.DeviceRefreshTokenFamilyTTL))
	}
}

func TestDeviceAuthorizationCreateApproveConsume(t *testing.T) {
	store := openDeviceAuthStore(t)
	user := createDeviceAuthUser(t, store, "alice")
	now := time.Date(2026, 10, 26, 8, 0, 0, 0, time.UTC)
	verifier := newDeviceAuthVerifier(t)
	challenge := deviceAuthChallenge(t, verifier)

	grant, err := store.CreateDeviceAuthorization(models.DeviceAuthorizationRequest{
		DeviceName: "Alice's laptop", Platform: "windows", ClientVersion: "0.1.0",
		CodeChallenge: challenge, CodeChallengeMethod: auth.PKCEMethodS256,
	}, now)
	if err != nil {
		t.Fatalf("create authorization: %v", err)
	}
	if grant.DeviceCode == "" || !strings.HasPrefix(grant.DeviceCode, "dc_") {
		t.Fatalf("device code = %q", grant.DeviceCode)
	}
	if !strings.HasPrefix(grant.Authorization.AuthorizationID, "dva_") {
		t.Fatalf("public authorization id = %q", grant.Authorization.AuthorizationID)
	}
	if _, err := auth.NormalizeDeviceUserCode(grant.UserCode); err != nil {
		t.Fatalf("user code %q is not human-enterable: %v", grant.UserCode, err)
	}
	if !grant.ExpiresAt.Equal(now.Add(auth.DeviceAuthorizationTTL)) {
		t.Fatalf("authorization expiry = %v, want %v", grant.ExpiresAt, now.Add(auth.DeviceAuthorizationTTL))
	}
	if grant.Authorization.State != models.DeviceAuthorizationPending {
		t.Fatalf("new authorization state = %q", grant.Authorization.State)
	}

	typedCode := strings.ToLower(strings.ReplaceAll(grant.UserCode, "-", " "))
	pending, err := store.DeviceAuthorizationByUserCode(typedCode, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("lookup by normalized user code: %v", err)
	}
	if pending.ID != grant.Authorization.ID || pending.DeviceName != "Alice's laptop" ||
		pending.UserID != nil {
		t.Fatalf("pending authorization = %+v", pending)
	}
	status, err := store.DeviceAuthorizationByID(grant.Authorization.AuthorizationID, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("status by authorization id: %v", err)
	}
	if status.State != models.DeviceAuthorizationPending ||
		status.AuthorizationID != grant.Authorization.AuthorizationID {
		t.Fatalf("pending status = %+v", status)
	}

	approved, err := store.ApproveDeviceAuthorizationByID(grant.Authorization.AuthorizationID, user.ID, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if approved.State != models.DeviceAuthorizationAuthorized ||
		approved.UserID == nil || *approved.UserID != user.ID ||
		approved.AuthorizedAt == nil {
		t.Fatalf("approved authorization = %+v", approved)
	}
	if _, err := store.ApproveDeviceAuthorization(grant.UserCode, user.ID, now.Add(time.Minute)); !errors.Is(err, ErrDeviceAuthorizationAlreadyDecided) {
		t.Fatalf("second approval error = %v, want already decided", err)
	}
	status, err = store.DeviceAuthorizationByID(grant.Authorization.AuthorizationID, now.Add(time.Minute))
	if err != nil || status.State != models.DeviceAuthorizationAuthorized {
		t.Fatalf("authorized status = %+v, err %v", status, err)
	}

	consumeAt := now.Add(2 * time.Minute)
	pair, err := store.ConsumeDeviceAuthorization(grant.DeviceCode, verifier, consumeAt)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if !strings.HasPrefix(pair.AccessToken, "at_") || !strings.HasPrefix(pair.RefreshToken, "rt_") {
		t.Fatalf("token pair has wrong prefixes: %+v", pair)
	}
	if pair.TokenType != "Bearer" || pair.Device == nil || !strings.HasPrefix(pair.Device.PublicID, "dev_") {
		t.Fatalf("token pair = %+v", pair)
	}
	if !pair.AccessExpiresAt.Equal(consumeAt.Add(auth.DeviceAccessTokenTTL)) {
		t.Errorf("access expiry = %v, want %v", pair.AccessExpiresAt, consumeAt.Add(auth.DeviceAccessTokenTTL))
	}
	if !pair.RefreshExpiresAt.Equal(consumeAt.Add(auth.DeviceRefreshTokenTTL)) {
		t.Errorf("refresh expiry = %v, want %v", pair.RefreshExpiresAt, consumeAt.Add(auth.DeviceRefreshTokenTTL))
	}
	if _, err := store.ConsumeDeviceAuthorization(grant.DeviceCode, verifier, consumeAt.Add(time.Second)); !errors.Is(err, ErrDeviceAuthorizationConsumed) {
		t.Fatalf("repeated consume error = %v, want consumed", err)
	}
	status, err = store.DeviceAuthorizationByID(grant.Authorization.AuthorizationID, consumeAt.Add(time.Second))
	if err != nil || status.State != models.DeviceAuthorizationConsumed || status.ConsumedAt == nil {
		t.Fatalf("consumed status = %+v, err %v", status, err)
	}

	context, err := store.DeviceByAccessToken(pair.AccessToken, consumeAt.Add(time.Second))
	if err != nil {
		t.Fatalf("resolve access token: %v", err)
	}
	if context.User.ID != user.ID || context.Device.PublicID != pair.Device.PublicID {
		t.Fatalf("access context = user %d device %q", context.User.ID, context.Device.PublicID)
	}
	if !context.Device.LastSeenAt.Equal(consumeAt.Add(time.Second)) {
		t.Errorf("last seen = %v, want %v", context.Device.LastSeenAt, consumeAt.Add(time.Second))
	}
	if _, err := store.DeviceByAccessToken(pair.RefreshToken, consumeAt.Add(time.Second)); !errors.Is(err, ErrInvalidDeviceAccessToken) {
		t.Fatalf("refresh token used as access error = %v", err)
	}

	assertDeviceCredentialsStoredAsHashes(t, store, grant, pair)
}

func TestDeviceAuthorizationRejectsWrongPKCEExpiryAndDenial(t *testing.T) {
	store := openDeviceAuthStore(t)
	user := createDeviceAuthUser(t, store, "alice")
	now := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)

	t.Run("wrong verifier leaves no partial session", func(t *testing.T) {
		verifier := newDeviceAuthVerifier(t)
		grant := createDeviceAuthorization(t, store, verifier, "PC", now)
		if _, err := store.ApproveDeviceAuthorization(grant.UserCode, user.ID, now); err != nil {
			t.Fatalf("approve: %v", err)
		}
		if _, err := store.ConsumeDeviceAuthorization(grant.DeviceCode, newDeviceAuthVerifier(t), now); !errors.Is(err, ErrInvalidDevicePKCEVerifier) {
			t.Fatalf("wrong verifier error = %v", err)
		}
		assertDeviceAuthCount(t, store, `SELECT COUNT(*) FROM devices`, 0)
		assertDeviceAuthCount(t, store, `SELECT COUNT(*) FROM device_access_tokens`, 0)
		assertDeviceAuthCount(t, store, `SELECT COUNT(*) FROM device_refresh_tokens`, 0)
		var state string
		if err := store.DB.QueryRow(`SELECT state FROM device_authorizations WHERE id = ?`,
			grant.Authorization.ID).Scan(&state); err != nil {
			t.Fatalf("read state after wrong verifier: %v", err)
		}
		if state != models.DeviceAuthorizationAuthorized {
			t.Errorf("state after wrong verifier = %q, want authorized", state)
		}
		if _, err := store.ConsumeDeviceAuthorization(grant.DeviceCode, verifier, now); err != nil {
			t.Fatalf("consume with correct verifier after failed attempt: %v", err)
		}
	})

	t.Run("pending request cannot be consumed", func(t *testing.T) {
		verifier := newDeviceAuthVerifier(t)
		grant := createDeviceAuthorization(t, store, verifier, "Pending PC", now)
		if _, err := store.ConsumeDeviceAuthorization(grant.DeviceCode, verifier, now); !errors.Is(err, ErrDeviceAuthorizationPending) {
			t.Fatalf("pending consume error = %v", err)
		}
	})

	t.Run("denied request is closed", func(t *testing.T) {
		verifier := newDeviceAuthVerifier(t)
		grant := createDeviceAuthorization(t, store, verifier, "Denied PC", now)
		denied, err := store.DenyDeviceAuthorizationByID(grant.Authorization.AuthorizationID, user.ID, now)
		if err != nil {
			t.Fatalf("deny: %v", err)
		}
		if denied.State != models.DeviceAuthorizationDenied || denied.AuthorizedAt != nil {
			t.Fatalf("denied authorization = %+v", denied)
		}
		if _, err := store.DeviceAuthorizationByUserCode(grant.UserCode, now); !errors.Is(err, ErrDeviceAuthorizationDenied) {
			t.Fatalf("lookup denied error = %v", err)
		}
		status, err := store.DeviceAuthorizationByID(grant.Authorization.AuthorizationID, now)
		if err != nil || status.State != models.DeviceAuthorizationDenied {
			t.Fatalf("denied status = %+v, err %v", status, err)
		}
		if _, err := store.ConsumeDeviceAuthorization(grant.DeviceCode, verifier, now); !errors.Is(err, ErrDeviceAuthorizationDenied) {
			t.Fatalf("consume denied error = %v", err)
		}
	})

	t.Run("authorization expires at ten minutes", func(t *testing.T) {
		verifier := newDeviceAuthVerifier(t)
		grant := createDeviceAuthorization(t, store, verifier, "Expired PC", now)
		if _, err := store.ApproveDeviceAuthorization(grant.UserCode, user.ID, now); err != nil {
			t.Fatalf("approve: %v", err)
		}
		expiredAt := now.Add(auth.DeviceAuthorizationTTL)
		if _, err := store.ConsumeDeviceAuthorization(grant.DeviceCode, verifier, expiredAt); !errors.Is(err, ErrDeviceAuthorizationExpired) {
			t.Fatalf("expired consume error = %v", err)
		}
		var state string
		if err := store.DB.QueryRow(`SELECT state FROM device_authorizations WHERE id = ?`,
			grant.Authorization.ID).Scan(&state); err != nil {
			t.Fatalf("read expired state: %v", err)
		}
		if state != models.DeviceAuthorizationExpired {
			t.Errorf("expired state = %q", state)
		}
		if _, err := store.DeviceAuthorizationByUserCode(grant.UserCode, expiredAt); !errors.Is(err, ErrDeviceAuthorizationExpired) {
			t.Fatalf("lookup expired error = %v", err)
		}
	})
}

func TestDeviceRefreshRotationAndReplay(t *testing.T) {
	store := openDeviceAuthStore(t)
	user := createDeviceAuthUser(t, store, "alice")
	now := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	pair := createDeviceAuthSession(t, store, user, "Laptop", now)

	rotateAt := now.Add(time.Minute)
	rotated, err := store.RotateDeviceRefreshToken(pair.RefreshToken, rotateAt)
	if err != nil {
		t.Fatalf("rotate refresh: %v", err)
	}
	if rotated.Device.PublicID != pair.Device.PublicID {
		t.Errorf("rotated device = %q, want %q", rotated.Device.PublicID, pair.Device.PublicID)
	}
	if rotated.AccessToken == pair.AccessToken || rotated.RefreshToken == pair.RefreshToken {
		t.Fatal("rotation returned a reused credential")
	}
	if !rotated.AccessExpiresAt.Equal(rotateAt.Add(auth.DeviceAccessTokenTTL)) ||
		!rotated.RefreshExpiresAt.Equal(rotateAt.Add(auth.DeviceRefreshTokenTTL)) {
		t.Fatalf("rotated expiries = access %v refresh %v", rotated.AccessExpiresAt, rotated.RefreshExpiresAt)
	}
	if _, err := store.DeviceByAccessToken(rotated.AccessToken, rotateAt.Add(time.Second)); err != nil {
		t.Fatalf("rotated access token: %v", err)
	}
	if _, err := store.DeviceByAccessToken(pair.AccessToken, rotateAt.Add(time.Second)); !errors.Is(err, ErrDeviceAccessTokenRevoked) {
		t.Fatalf("old access token after rotation error = %v, want revoked", err)
	}

	// Presenting the old refresh token again is a reuse signal. It must fail and
	// revoke the current pair in that family, not merely return a fresh token.
	if _, err := store.RotateDeviceRefreshToken(pair.RefreshToken, rotateAt.Add(time.Second)); !errors.Is(err, ErrDeviceRefreshTokenReused) {
		t.Fatalf("replayed refresh error = %v, want reuse", err)
	}
	if _, err := store.RotateDeviceRefreshToken(rotated.RefreshToken, rotateAt.Add(2*time.Second)); !errors.Is(err, ErrDeviceRefreshTokenRevoked) {
		t.Fatalf("revoked replacement refresh error = %v", err)
	}
	if _, err := store.DeviceByAccessToken(rotated.AccessToken, rotateAt.Add(2*time.Second)); !errors.Is(err, ErrDeviceRevoked) {
		t.Fatalf("access token after reuse error = %v, want device revoked", err)
	}
	if _, err := store.RotateDeviceRefreshToken("rt_unknown", rotateAt); !errors.Is(err, ErrInvalidDeviceRefreshToken) {
		t.Fatalf("unknown refresh error = %v", err)
	}
}

func TestDeviceRefreshFamilyAbsoluteDeadline(t *testing.T) {
	store := openDeviceAuthStore(t)
	user := createDeviceAuthUser(t, store, "alice")
	now := time.Date(2026, 10, 26, 10, 30, 0, 0, time.UTC)
	pair := createDeviceAuthSession(t, store, user, "Laptop", now)
	familyAbsolute := now.Add(auth.DeviceRefreshTokenFamilyTTL)

	refreshToken := pair.RefreshToken
	for i := 1; i <= 6; i++ {
		rotateAt := now.Add(time.Duration(i*29) * 24 * time.Hour)
		rotated, err := store.RotateDeviceRefreshToken(refreshToken, rotateAt)
		if err != nil {
			t.Fatalf("rotation %d at %v: %v", i, rotateAt, err)
		}
		wantRefreshExpiry := rotateAt.Add(auth.DeviceRefreshTokenTTL)
		if familyAbsolute.Before(wantRefreshExpiry) {
			wantRefreshExpiry = familyAbsolute
		}
		if !rotated.RefreshExpiresAt.Equal(wantRefreshExpiry) {
			t.Fatalf("rotation %d refresh expiry = %v, want %v", i, rotated.RefreshExpiresAt, wantRefreshExpiry)
		}
		if i == 6 {
			if !rotated.RefreshExpiresAt.Equal(familyAbsolute) {
				t.Errorf("final refresh expiry = %v, want family deadline %v", rotated.RefreshExpiresAt, familyAbsolute)
			}
			if rotated.RefreshExpiresIn != int(6*24*time.Hour/time.Second) {
				t.Errorf("final refresh expires_in = %d, want 6d", rotated.RefreshExpiresIn)
			}
		}
		refreshToken = rotated.RefreshToken
	}
	if _, err := store.RotateDeviceRefreshToken(refreshToken, familyAbsolute); !errors.Is(err, ErrDeviceRefreshTokenExpired) {
		t.Fatalf("rotation at family absolute deadline error = %v, want expired", err)
	}
}

func TestDeviceAuthorizationCSRFOneTimeBinding(t *testing.T) {
	store := openDeviceAuthStore(t)
	alice := createDeviceAuthUser(t, store, "alice")
	bob := createDeviceAuthUser(t, store, "bob")
	now := time.Date(2026, 10, 26, 10, 45, 0, 0, time.UTC)
	verifier := newDeviceAuthVerifier(t)
	grant := createDeviceAuthorization(t, store, verifier, "CSRF PC", now)

	csrf, err := store.IssueDeviceAuthorizationCSRF(grant.Authorization.AuthorizationID, alice.ID, now)
	if err != nil {
		t.Fatalf("issue CSRF: %v", err)
	}
	if !csrf.ExpiresAt.Equal(now.Add(auth.DeviceAuthorizationCSRFTTL)) {
		t.Errorf("CSRF expiry = %v, want %v", csrf.ExpiresAt, now.Add(auth.DeviceAuthorizationCSRFTTL))
	}
	var storedHash string
	var storedUser int64
	if err := store.DB.QueryRow(`SELECT csrf_token_hash, csrf_user_id
		FROM device_authorizations WHERE id = ?`, grant.Authorization.ID).
		Scan(&storedHash, &storedUser); err != nil {
		t.Fatalf("read stored CSRF: %v", err)
	}
	if storedHash == csrf.Token || storedHash != auth.HashDeviceCredential(csrf.Token) {
		t.Error("CSRF token was not stored as its hash")
	}
	if storedUser != alice.ID {
		t.Errorf("CSRF user = %d, want %d", storedUser, alice.ID)
	}

	if _, err := store.DecideDeviceAuthorizationByIDWithCSRF(
		grant.Authorization.AuthorizationID, bob.ID, csrf.Token, true, now); !errors.Is(err, ErrDeviceAuthorizationCSRF) {
		t.Fatalf("cross-user CSRF error = %v, want invalid", err)
	}
	if _, err := store.DecideDeviceAuthorizationByIDWithCSRF(
		grant.Authorization.AuthorizationID, alice.ID, "csrf_wrong", true, now); !errors.Is(err, ErrDeviceAuthorizationCSRF) {
		t.Fatalf("wrong CSRF error = %v, want invalid", err)
	}
	approved, err := store.DecideDeviceAuthorizationByIDWithCSRF(
		grant.Authorization.AuthorizationID, alice.ID, csrf.Token, true, now)
	if err != nil {
		t.Fatalf("approve with CSRF: %v", err)
	}
	if approved.State != models.DeviceAuthorizationAuthorized {
		t.Fatalf("approved state = %q", approved.State)
	}
	var consumed int
	if err := store.DB.QueryRow(`SELECT csrf_consumed_at IS NOT NULL
		FROM device_authorizations WHERE id = ?`, grant.Authorization.ID).Scan(&consumed); err != nil {
		t.Fatalf("read consumed CSRF: %v", err)
	}
	if consumed != 1 {
		t.Error("CSRF token was not consumed")
	}

	expiringVerifier := newDeviceAuthVerifier(t)
	expiringGrant := createDeviceAuthorization(t, store, expiringVerifier, "Expired CSRF PC", now)
	expiringCSRF, err := store.IssueDeviceAuthorizationCSRF(
		expiringGrant.Authorization.AuthorizationID, alice.ID, now)
	if err != nil {
		t.Fatalf("issue expiring CSRF: %v", err)
	}
	if _, err := store.DecideDeviceAuthorizationByIDWithCSRF(
		expiringGrant.Authorization.AuthorizationID, alice.ID, expiringCSRF.Token,
		true, now.Add(auth.DeviceAuthorizationCSRFTTL)); !errors.Is(err, ErrDeviceAuthorizationCSRF) {
		t.Fatalf("expired CSRF error = %v, want invalid", err)
	}
}

func TestDeviceEmailPolicyBlocksIssuanceAndAccess(t *testing.T) {
	store := openDeviceAuthStore(t)
	user := createDeviceAuthUser(t, store, "alice")
	now := time.Date(2026, 10, 26, 11, 15, 0, 0, time.UTC)
	pair := createDeviceAuthSession(t, store, user, "Email pending", now)

	if err := store.SetUserEmail(user.ID, "alice@example.com", false); err != nil {
		t.Fatalf("set pending email: %v", err)
	}
	if _, err := store.DeviceByAccessTokenWithEmailPolicy(pair.AccessToken, true, now.Add(time.Second)); !errors.Is(err, ErrDeviceUserUnavailable) {
		t.Fatalf("email-gated access error = %v, want user unavailable", err)
	}
	if _, err := store.RotateDeviceRefreshTokenWithEmailPolicy(pair.RefreshToken, true, now.Add(time.Second)); !errors.Is(err, ErrDeviceUserUnavailable) {
		t.Fatalf("email-gated refresh error = %v, want user unavailable", err)
	}

	verifier := newDeviceAuthVerifier(t)
	grant := createDeviceAuthorization(t, store, verifier, "Email gate exchange", now)
	if _, err := store.ApproveDeviceAuthorization(grant.UserCode, user.ID, now); err != nil {
		t.Fatalf("approve exchange: %v", err)
	}
	if _, err := store.ConsumeDeviceAuthorizationWithEmailPolicy(
		grant.DeviceCode, verifier, true, now); !errors.Is(err, ErrDeviceUserUnavailable) {
		t.Fatalf("email-gated exchange error = %v, want user unavailable", err)
	}
	var accessCount int
	if err := store.DB.QueryRow(`SELECT COUNT(*) FROM device_access_tokens
		WHERE user_id = ?`, user.ID).Scan(&accessCount); err != nil {
		t.Fatalf("count access tokens after blocked exchange: %v", err)
	}
	if accessCount != 1 {
		t.Errorf("blocked exchange changed access token count to %d, want 1", accessCount)
	}
}

func TestDeviceAccessExpiryAndRevocationScope(t *testing.T) {
	store := openDeviceAuthStore(t)
	alice := createDeviceAuthUser(t, store, "alice")
	bob := createDeviceAuthUser(t, store, "bob")
	now := time.Date(2026, 10, 26, 11, 0, 0, 0, time.UTC)

	aliceFirst := createDeviceAuthSession(t, store, alice, "Alice desktop", now)
	aliceSecond := createDeviceAuthSession(t, store, alice, "Alice laptop", now.Add(time.Second))
	bobDevice := createDeviceAuthSession(t, store, bob, "Bob desktop", now.Add(2*time.Second))

	if _, err := store.DeviceByAccessToken(aliceFirst.AccessToken, now.Add(auth.DeviceAccessTokenTTL)); !errors.Is(err, ErrDeviceAccessTokenExpired) {
		t.Fatalf("expired access error = %v", err)
	}

	// A device id from another account must not be revocable, and must not
	// disclose whether that device exists.
	if err := store.RevokeDevice(alice.ID, bobDevice.Device.PublicID, now.Add(3*time.Minute)); !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("cross-user revoke error = %v", err)
	}
	if err := store.RevokeDevice(bob.ID, aliceFirst.Device.PublicID, now.Add(3*time.Minute)); !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("reverse cross-user revoke error = %v", err)
	}
	if _, err := store.DeviceByAccessToken(bobDevice.AccessToken, now.Add(3*time.Minute)); err != nil {
		t.Fatalf("bob token was revoked by alice's action: %v", err)
	}

	if err := store.RevokeDevice(alice.ID, aliceFirst.Device.PublicID, now.Add(4*time.Minute)); err != nil {
		t.Fatalf("revoke first alice device: %v", err)
	}
	if _, err := store.DeviceByAccessToken(aliceFirst.AccessToken, now.Add(4*time.Minute)); !errors.Is(err, ErrDeviceRevoked) {
		t.Fatalf("revoked device access error = %v", err)
	}
	if _, err := store.DeviceByAccessToken(aliceSecond.AccessToken, now.Add(4*time.Minute)); err != nil {
		t.Fatalf("second alice device was revoked too early: %v", err)
	}

	revoked, err := store.RevokeAllDevicesForUser(alice.ID, now.Add(5*time.Minute))
	if err != nil {
		t.Fatalf("revoke all alice devices: %v", err)
	}
	if revoked != 1 {
		t.Errorf("newly revoked devices = %d, want 1", revoked)
	}
	if _, err := store.DeviceByAccessToken(aliceSecond.AccessToken, now.Add(5*time.Minute)); !errors.Is(err, ErrDeviceRevoked) {
		t.Fatalf("second alice device access error = %v", err)
	}
	if _, err := store.DeviceByAccessToken(bobDevice.AccessToken, now.Add(5*time.Minute)); err != nil {
		t.Fatalf("bob device was revoked by alice-wide revoke: %v", err)
	}

	aliceDevices, err := store.DevicesByUser(alice.ID)
	if err != nil {
		t.Fatalf("list alice devices: %v", err)
	}
	if len(aliceDevices) != 2 {
		t.Fatalf("alice device count = %d, want 2", len(aliceDevices))
	}
	for _, device := range aliceDevices {
		if device.RevokedAt == nil {
			t.Errorf("alice device %q is not marked revoked", device.PublicID)
		}
	}
}

func TestDeviceRefreshRejectsDisabledUser(t *testing.T) {
	store := openDeviceAuthStore(t)
	user := createDeviceAuthUser(t, store, "alice")
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	pair := createDeviceAuthSession(t, store, user, "Laptop", now)

	if err := store.UpdateUserStatus(user.ID, models.StatusDisabled); err != nil {
		t.Fatalf("disable user: %v", err)
	}
	if _, err := store.DeviceByAccessToken(pair.AccessToken, now.Add(time.Minute)); !errors.Is(err, ErrDeviceUserUnavailable) {
		t.Fatalf("disabled user access error = %v", err)
	}
	if _, err := store.RotateDeviceRefreshToken(pair.RefreshToken, now.Add(time.Minute)); !errors.Is(err, ErrDeviceUserUnavailable) {
		t.Fatalf("disabled user refresh error = %v", err)
	}
	if _, err := store.RotateDeviceRefreshToken(pair.RefreshToken, now.Add(2*time.Minute)); !errors.Is(err, ErrDeviceRefreshTokenRevoked) {
		t.Fatalf("refresh after account disable error = %v", err)
	}
}

func TestDeviceAuthCascadesWhenUserIsDeleted(t *testing.T) {
	store := openDeviceAuthStore(t)
	user := createDeviceAuthUser(t, store, "alice")
	now := time.Date(2026, 10, 26, 13, 0, 0, 0, time.UTC)
	_ = createDeviceAuthSession(t, store, user, "Laptop", now)

	if err := store.DeleteUser(user.ID); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	for _, query := range []string{
		`SELECT COUNT(*) FROM device_authorizations WHERE user_id = ?`,
		`SELECT COUNT(*) FROM devices WHERE user_id = ?`,
		`SELECT COUNT(*) FROM device_access_tokens WHERE user_id = ?`,
		`SELECT COUNT(*) FROM device_refresh_tokens WHERE user_id = ?`,
	} {
		var count int
		if err := store.DB.QueryRow(query, user.ID).Scan(&count); err != nil {
			t.Fatalf("cascade count: %v", err)
		}
		if count != 0 {
			t.Errorf("%q left %d rows after user deletion", query, count)
		}
	}
}

func TestCleanupDeviceAuth(t *testing.T) {
	store := openDeviceAuthStore(t)
	user := createDeviceAuthUser(t, store, "alice")
	now := time.Date(2026, 10, 26, 14, 0, 0, 0, time.UTC)

	_ = createDeviceAuthSession(t, store, user, "Laptop", now)
	verifier := newDeviceAuthVerifier(t)
	_ = createDeviceAuthorization(t, store, verifier, "Pending tablet", now)

	if err := store.CleanupDeviceAuth(now.Add(auth.DeviceRefreshTokenTTL + time.Hour)); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	for _, table := range []string{
		"device_authorizations", "device_access_tokens", "device_refresh_tokens",
	} {
		assertDeviceAuthCount(t, store, `SELECT COUNT(*) FROM `+table, 0)
	}
}

func openDeviceAuthStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "device-auth.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := store.MigrateDeviceAuthSecurityColumns(); err != nil {
		store.DB.Close()
		t.Fatalf("migrate device auth security columns: %v", err)
	}
	t.Cleanup(func() { _ = store.DB.Close() })
	return store
}

func createDeviceAuthUser(t *testing.T, store *Store, name string) *models.User {
	t.Helper()
	user := &models.User{
		Name: name, PassHash: "hash", Salt: "salt",
		Role: models.RoleUser, Status: models.StatusActive,
	}
	if err := store.CreateUser(user); err != nil {
		t.Fatalf("create user %s: %v", name, err)
	}
	return user
}

func newDeviceAuthVerifier(t *testing.T) string {
	t.Helper()
	verifier, err := auth.NewPKCEVerifier()
	if err != nil {
		t.Fatalf("new PKCE verifier: %v", err)
	}
	return verifier
}

func deviceAuthChallenge(t *testing.T, verifier string) string {
	t.Helper()
	challenge, err := auth.PKCEChallengeS256(verifier)
	if err != nil {
		t.Fatalf("PKCE challenge: %v", err)
	}
	return challenge
}

func createDeviceAuthorization(t *testing.T, store *Store, verifier, name string, now time.Time) *models.DeviceAuthorizationGrant {
	t.Helper()
	grant, err := store.CreateDeviceAuthorization(models.DeviceAuthorizationRequest{
		DeviceName: name, Platform: "windows", ClientVersion: "0.1.0",
		CodeChallenge: deviceAuthChallenge(t, verifier), CodeChallengeMethod: auth.PKCEMethodS256,
	}, now)
	if err != nil {
		t.Fatalf("create device authorization: %v", err)
	}
	return grant
}

func createDeviceAuthSession(t *testing.T, store *Store, user *models.User, name string, now time.Time) *models.DeviceTokenPair {
	t.Helper()
	verifier := newDeviceAuthVerifier(t)
	grant := createDeviceAuthorization(t, store, verifier, name, now)
	if _, err := store.ApproveDeviceAuthorization(grant.UserCode, user.ID, now); err != nil {
		t.Fatalf("approve %s: %v", name, err)
	}
	pair, err := store.ConsumeDeviceAuthorization(grant.DeviceCode, verifier, now)
	if err != nil {
		t.Fatalf("consume %s: %v", name, err)
	}
	return pair
}

func assertDeviceCredentialsStoredAsHashes(t *testing.T, store *Store, grant *models.DeviceAuthorizationGrant, pair *models.DeviceTokenPair) {
	t.Helper()
	var deviceHash, userHash string
	if err := store.DB.QueryRow(`SELECT device_code_hash, user_code_hash
		FROM device_authorizations WHERE id = ?`, grant.Authorization.ID).
		Scan(&deviceHash, &userHash); err != nil {
		t.Fatalf("read authorization hashes: %v", err)
	}
	normalizedUserCode, _ := auth.NormalizeDeviceUserCode(grant.UserCode)
	if deviceHash != auth.HashDeviceCredential(grant.DeviceCode) {
		t.Errorf("stored device code is not its SHA-256 hash")
	}
	if userHash != auth.HashDeviceCredential(normalizedUserCode) {
		t.Errorf("stored user code is not its normalized SHA-256 hash")
	}
	if deviceHash == grant.DeviceCode || userHash == grant.UserCode {
		t.Error("a plaintext authorization code was stored")
	}

	var accessHash, refreshHash string
	if err := store.DB.QueryRow(`SELECT token_hash FROM device_access_tokens
		WHERE user_id = ? AND device_id = (SELECT id FROM devices WHERE public_id = ?)`,
		pair.Device.UserID, pair.Device.PublicID).Scan(&accessHash); err != nil {
		t.Fatalf("read access hash: %v", err)
	}
	if err := store.DB.QueryRow(`SELECT token_hash FROM device_refresh_tokens
		WHERE user_id = ? AND device_id = (SELECT id FROM devices WHERE public_id = ?)`,
		pair.Device.UserID, pair.Device.PublicID).Scan(&refreshHash); err != nil {
		t.Fatalf("read refresh hash: %v", err)
	}
	if accessHash != auth.HashDeviceCredential(pair.AccessToken) ||
		refreshHash != auth.HashDeviceCredential(pair.RefreshToken) {
		t.Error("token tables did not store the expected SHA-256 hashes")
	}
	if accessHash == pair.AccessToken || refreshHash == pair.RefreshToken {
		t.Error("a plaintext bearer token was stored")
	}

	for _, tc := range []struct {
		table string
		raw   string
	}{
		{"device_authorizations", grant.DeviceCode},
		{"device_authorizations", grant.UserCode},
		{"device_access_tokens", pair.AccessToken},
		{"device_refresh_tokens", pair.RefreshToken},
	} {
		var count int
		// The schema varies by table, so the direct hash comparison above is the
		// authoritative check. This query only verifies the raw bearer string is
		// absent from the columns that can hold credential material.
		column := "device_code_hash"
		if tc.table == "device_authorizations" && tc.raw != grant.DeviceCode {
			column = "user_code_hash"
		} else if tc.table == "device_access_tokens" {
			column = "token_hash"
		} else if tc.table == "device_refresh_tokens" {
			column = "token_hash"
		}
		query := `SELECT COUNT(*) FROM ` + tc.table + ` WHERE ` + column + ` = ?`
		if err := store.DB.QueryRow(query, tc.raw).Scan(&count); err != nil {
			t.Fatalf("query plaintext %s: %v", column, err)
		}
		if count != 0 {
			t.Errorf("raw credential found in %s.%s", tc.table, column)
		}
	}
}

func assertDeviceAuthCount(t *testing.T, store *Store, query string, want int) {
	t.Helper()
	var count int
	if err := store.DB.QueryRow(query).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != want {
		t.Fatalf("count = %d, want %d", count, want)
	}
}
