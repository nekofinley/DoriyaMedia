package db

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"doriyamedia/internal/models"
)

type deviceAuthConcurrencyResult struct {
	operation string
	err       error
	allowed   []error
}

func openDeviceAuthConcurrencyStores(t *testing.T) (*Store, *Store) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "device-auth-concurrency.db")
	first, err := Open(path)
	if err != nil {
		t.Fatalf("open first file-backed device-auth store: %v", err)
	}
	t.Cleanup(func() { _ = first.DB.Close() })

	second, err := Open(path)
	if err != nil {
		t.Fatalf("open second file-backed device-auth store: %v", err)
	}
	t.Cleanup(func() { _ = second.DB.Close() })

	for _, store := range []*Store{first, second} {
		store.DB.SetMaxOpenConns(8)
		if store.DB.Stats().MaxOpenConnections < 2 {
			t.Fatal("device-auth concurrency test requires multiple SQLite connections")
		}
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file-backed device-auth database is missing: %v", err)
	}
	var journalMode string
	if err := first.DB.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatalf("read device-auth journal mode: %v", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		t.Fatalf("device-auth journal mode = %q, want WAL", journalMode)
	}
	return first, second
}

func createDeviceAuthConcurrencyTokens(
	t *testing.T,
	store *Store,
	user *models.User,
	device *models.Device,
	now time.Time,
) *models.DeviceTokenPair {
	t.Helper()
	tx, err := store.DB.Begin()
	if err != nil {
		t.Fatalf("begin device token fixture: %v", err)
	}
	defer tx.Rollback()
	pair, err := createDeviceTokenPairTx(tx, user.ID, device, now)
	if err != nil {
		t.Fatalf("create device token fixture: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit device token fixture: %v", err)
	}
	return pair
}

func assertDeviceAuthRowsRevoked(
	t *testing.T,
	store *Store,
	deviceID int64,
) {
	t.Helper()
	for _, tc := range []struct {
		table string
		where string
	}{
		{
			table: "playback_tickets",
			where: `state = 'revoked' AND revoked_at IS NOT NULL`,
		},
		{
			table: "playback_media_sessions",
			where: `state = 'revoked' AND revoked_at IS NOT NULL`,
		},
		{
			table: "playback_subtitle_sessions",
			where: `state = 'revoked' AND revoked_at IS NOT NULL`,
		},
	} {
		var total, correctlyRevoked int
		if err := store.DB.QueryRow(`SELECT COUNT(*) FROM `+tc.table+
			` WHERE device_id = ?`, deviceID).Scan(&total); err != nil {
			t.Fatalf("count %s for device %d: %v", tc.table, deviceID, err)
		}
		if total == 0 {
			t.Fatalf("%s has no rows for device %d", tc.table, deviceID)
		}
		if err := store.DB.QueryRow(`SELECT COUNT(*) FROM `+tc.table+
			` WHERE device_id = ? AND `+tc.where, deviceID).
			Scan(&correctlyRevoked); err != nil {
			t.Fatalf("count revoked %s for device %d: %v", tc.table, deviceID, err)
		}
		if correctlyRevoked != total {
			t.Errorf("%s revoked rows = %d, want all %d", tc.table, correctlyRevoked, total)
		}
	}
}

func assertDeviceAuthTokensRevoked(t *testing.T, store *Store, deviceID int64) {
	t.Helper()
	for _, table := range []string{"device_access_tokens", "device_refresh_tokens"} {
		var total, unrevoked int
		if err := store.DB.QueryRow(`SELECT COUNT(*) FROM `+table+
			` WHERE device_id = ?`, deviceID).Scan(&total); err != nil {
			t.Fatalf("count %s for device %d: %v", table, deviceID, err)
		}
		if total == 0 {
			t.Fatalf("%s has no rows for device %d", table, deviceID)
		}
		if err := store.DB.QueryRow(`SELECT COUNT(*) FROM `+table+
			` WHERE device_id = ? AND revoked_at IS NULL`, deviceID).
			Scan(&unrevoked); err != nil {
			t.Fatalf("count unrevoked %s for device %d: %v", table, deviceID, err)
		}
		if unrevoked != 0 {
			t.Errorf("%s unrevoked rows = %d, want 0", table, unrevoked)
		}
	}
}

func TestRevokeDeviceConcurrentFileSQLite(t *testing.T) {
	first, second := openDeviceAuthConcurrencyStores(t)
	stores := []*Store{first, second}
	now := time.Date(2026, 10, 26, 23, 30, 0, 0, time.UTC)

	target := addPlaybackTestFixture(t, first, "target", "target", now)
	targetTokens := createDeviceAuthConcurrencyTokens(
		t, first, target.user, target.device, now)
	sibling := addPlaybackTestDeviceForUser(
		t, first, target.user.ID, "sibling", "sibling", now)
	siblingTokens := createDeviceAuthConcurrencyTokens(
		t, first, target.user, sibling, now)
	other := addPlaybackTestFixture(t, first, "other", "other", now)
	otherTokens := createDeviceAuthConcurrencyTokens(
		t, first, other.user, other.device, now)

	targetGrants := createPlaybackConcurrencyGrants(t, first, target, now)
	otherGrants := createPlaybackConcurrencyGrants(t, first, other, now)

	const (
		revokeCalls   = 16
		accessCalls   = 12
		mediaCalls    = 8
		subtitleCalls = 8
		ticketCalls   = 8
		totalCalls    = revokeCalls + accessCalls + mediaCalls + subtitleCalls + ticketCalls
	)
	at := now.Add(time.Minute)
	start := make(chan struct{})
	results := make(chan deviceAuthConcurrencyResult, totalCalls)
	var wg sync.WaitGroup
	run := func(
		store *Store,
		operation string,
		allowed []error,
		fn func() error,
	) {
		defer wg.Done()
		<-start
		results <- deviceAuthConcurrencyResult{
			operation: operation,
			err:       fn(),
			allowed:   allowed,
		}
	}

	for i := 0; i < revokeCalls; i++ {
		store := stores[i%len(stores)]
		wg.Add(1)
		go run(store, fmt.Sprintf("revoke device %d", i), nil, func() error {
			return store.RevokeDevice(target.user.ID, target.device.PublicID, at)
		})
	}
	for i := 0; i < accessCalls; i++ {
		store := stores[i%len(stores)]
		wg.Add(1)
		go run(store, fmt.Sprintf("resolve access token %d", i), []error{
			ErrDeviceRevoked,
			ErrDeviceAccessTokenRevoked,
		}, func() error {
			_, err := store.DeviceByAccessToken(targetTokens.AccessToken, at)
			return err
		})
	}
	for i := 0; i < mediaCalls; i++ {
		store := stores[i%len(stores)]
		wg.Add(1)
		go run(store, fmt.Sprintf("authorize media %d", i), []error{
			ErrPlaybackMediaSessionRevoked,
			ErrPlaybackTicketRevoked,
			ErrPlaybackMediaSessionConflict,
		}, func() error {
			_, err := store.AuthorizePlaybackMediaSession(
				targetGrants.media.Capability, false, at)
			return err
		})
	}
	for i := 0; i < subtitleCalls; i++ {
		store := stores[i%len(stores)]
		wg.Add(1)
		go run(store, fmt.Sprintf("authorize subtitle %d", i), []error{
			ErrPlaybackSubtitleSessionRevoked,
			ErrPlaybackTicketRevoked,
			ErrPlaybackSubtitleSessionConflict,
		}, func() error {
			_, err := store.AuthorizePlaybackSubtitleSession(
				targetGrants.subtitle.Capability, false, at)
			return err
		})
	}
	for i := 0; i < ticketCalls; i++ {
		store := stores[i%len(stores)]
		wg.Add(1)
		go run(store, fmt.Sprintf("create playback ticket %d", i), []error{
			ErrInvalidPlaybackTicket,
		}, func() error {
			_, err := store.CreatePlaybackTicket(models.PlaybackTicketRequest{
				UserID:              target.user.ID,
				DeviceID:            target.device.ID,
				FileID:              target.fileID,
				CandidateTransports: []string{models.PlaybackTransportServerProxy},
			}, at)
			return err
		})
	}

	close(start)
	wg.Wait()
	close(results)

	for result := range results {
		if result.err == nil {
			continue
		}
		allowed := false
		for _, expected := range result.allowed {
			if errors.Is(result.err, expected) {
				allowed = true
				break
			}
		}
		if !allowed || isSQLiteBusy(result.err) {
			t.Errorf("%s returned unexpected error: %v (busy=%v)",
				result.operation, result.err, isSQLiteBusy(result.err))
		}
	}

	// Revocation is idempotent: a later call must not move the original
	// revocation timestamp forward.
	devices, err := first.DevicesByUser(target.user.ID)
	if err != nil {
		t.Fatalf("list target user's devices: %v", err)
	}
	var targetRevokedAt time.Time
	for _, device := range devices {
		if device.ID == target.device.ID {
			if device.RevokedAt == nil {
				t.Fatal("target device is not revoked")
			}
			targetRevokedAt = *device.RevokedAt
		}
	}
	if targetRevokedAt.IsZero() {
		t.Fatal("target device revocation timestamp is missing")
	}
	if err := second.RevokeDevice(
		target.user.ID, target.device.PublicID, at.Add(time.Minute)); err != nil {
		t.Fatalf("repeat target device revocation: %v", err)
	}
	devices, err = first.DevicesByUser(target.user.ID)
	if err != nil {
		t.Fatalf("relist target user's devices: %v", err)
	}
	for _, device := range devices {
		if device.ID == target.device.ID {
			if device.RevokedAt == nil || !device.RevokedAt.Equal(targetRevokedAt) {
				t.Errorf("repeat revocation moved timestamp from %v to %v",
					targetRevokedAt, device.RevokedAt)
			}
		}
	}

	assertDeviceAuthTokensRevoked(t, first, target.device.ID)
	assertDeviceAuthRowsRevoked(t, first, target.device.ID)

	devices, err = first.DevicesByUser(target.user.ID)
	if err != nil {
		t.Fatalf("verify sibling device: %v", err)
	}
	foundSibling := false
	for _, device := range devices {
		if device.ID != sibling.ID {
			continue
		}
		foundSibling = true
		if device.RevokedAt != nil {
			t.Errorf("sibling device was revoked at %v", device.RevokedAt)
		}
	}
	if !foundSibling {
		t.Fatal("sibling device is missing")
	}
	var unrevokedSiblingTokens int
	for _, table := range []string{"device_access_tokens", "device_refresh_tokens"} {
		var count int
		if err := first.DB.QueryRow(`SELECT COUNT(*) FROM `+table+
			` WHERE device_id = ? AND revoked_at IS NULL`, sibling.ID).Scan(&count); err != nil {
			t.Fatalf("count sibling tokens in %s: %v", table, err)
		}
		unrevokedSiblingTokens += count
	}
	if unrevokedSiblingTokens != 2 {
		t.Errorf("unrevoked sibling tokens = %d, want 2", unrevokedSiblingTokens)
	}
	if _, err := first.DeviceByAccessToken(siblingTokens.AccessToken, at); err != nil {
		t.Errorf("sibling access token after target revocation: %v", err)
	}

	otherDevices, err := first.DevicesByUser(other.user.ID)
	if err != nil {
		t.Fatalf("verify other user's device: %v", err)
	}
	if len(otherDevices) != 1 || otherDevices[0].RevokedAt != nil {
		t.Fatalf("other user's devices after target revocation = %+v", otherDevices)
	}
	var otherUnrevokedTokens int
	for _, table := range []string{"device_access_tokens", "device_refresh_tokens"} {
		var count int
		if err := first.DB.QueryRow(`SELECT COUNT(*) FROM `+table+
			` WHERE user_id = ? AND revoked_at IS NULL`, other.user.ID).Scan(&count); err != nil {
			t.Fatalf("count other user's tokens in %s: %v", table, err)
		}
		otherUnrevokedTokens += count
	}
	if otherUnrevokedTokens != 2 {
		t.Errorf("other user's unrevoked tokens = %d, want 2", otherUnrevokedTokens)
	}
	if _, err := first.DeviceByAccessToken(otherTokens.AccessToken, at); err != nil {
		t.Errorf("other user's access token after target revocation: %v", err)
	}
	var otherTicketState, otherMediaState, otherSubtitleState string
	if err := first.DB.QueryRow(`SELECT state FROM playback_tickets WHERE id = ?`,
		otherGrants.ticket.ID).Scan(&otherTicketState); err != nil {
		t.Fatalf("read other user's ticket: %v", err)
	}
	if err := first.DB.QueryRow(`SELECT state FROM playback_media_sessions WHERE id = ?`,
		otherGrants.media.Session.ID).Scan(&otherMediaState); err != nil {
		t.Fatalf("read other user's media session: %v", err)
	}
	if err := first.DB.QueryRow(`SELECT state FROM playback_subtitle_sessions WHERE id = ?`,
		otherGrants.subtitle.Session.ID).Scan(&otherSubtitleState); err != nil {
		t.Fatalf("read other user's subtitle session: %v", err)
	}
	if otherTicketState != models.PlaybackTicketConsuming ||
		otherMediaState != models.PlaybackMediaSessionActive ||
		otherSubtitleState != models.PlaybackSubtitleSessionActive {
		t.Errorf("other user's playback changed: ticket=%q media=%q subtitle=%q",
			otherTicketState, otherMediaState, otherSubtitleState)
	}

	if err := first.RevokeDevice(
		target.user.ID, other.device.PublicID, at.Add(2*time.Minute)); !errors.Is(
		err, ErrDeviceNotFound) {
		t.Fatalf("cross-user revoke error = %v, want ErrDeviceNotFound", err)
	}
}
