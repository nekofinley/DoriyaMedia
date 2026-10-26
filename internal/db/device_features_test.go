package db

import (
	"errors"
	"testing"
	"time"

	"doriyamedia/internal/models"
)

func TestDeviceFeatureMigrationIsIdempotentAndProjectsSafeDevices(t *testing.T) {
	store := openDeviceAuthStore(t)
	now := time.Date(2026, 10, 26, 21, 0, 0, 0, time.UTC)
	user := createDeviceAuthUser(t, store, "feature-admin")
	device := addPlaybackTestDeviceForUser(t, store, user.ID, "Feature PC", "feature-pc", now)
	overrides := models.DeviceFeatureOverrides{
		DeviceAuthDisabled:     true,
		PlaybackTicketDisabled: true,
		ServerProxyV1Disabled:  true,
		NativePlayerDisabled:   true,
	}
	updated, err := store.UpdateDeviceFeatureOverrides(
		device.PublicID, overrides, user.ID, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("set device overrides: %v", err)
	}
	if updated.ID != device.PublicID || updated.UserName != user.Name ||
		updated.Overrides != overrides {
		t.Fatalf("updated client device = %+v", updated)
	}

	for i := 0; i < 2; i++ {
		if err := store.migrate(); err != nil {
			t.Fatalf("repeat migration %d: %v", i+1, err)
		}
	}
	persisted, err := store.DeviceFeatureOverridesByDeviceID(device.ID)
	if err != nil {
		t.Fatalf("read persisted overrides: %v", err)
	}
	if persisted != overrides {
		t.Fatalf("persisted overrides = %+v, want %+v", persisted, overrides)
	}
	devices, err := store.ClientDevices(10)
	if err != nil {
		t.Fatalf("list client devices: %v", err)
	}
	if len(devices) != 1 || devices[0].ID != device.PublicID ||
		devices[0].UserID != user.ID || devices[0].Overrides != overrides {
		t.Fatalf("client device projection = %+v", devices)
	}

	if _, err := store.UpdateDeviceFeatureOverrides(
		device.PublicID, models.DeviceFeatureOverrides{}, user.ID, now.Add(2*time.Minute)); err != nil {
		t.Fatalf("clear device overrides: %v", err)
	}
	persisted, err = store.DeviceFeatureOverridesByDeviceID(device.ID)
	if err != nil {
		t.Fatalf("read cleared overrides: %v", err)
	}
	if !persisted.Empty() {
		t.Fatalf("cleared overrides = %+v", persisted)
	}
}

func TestDevicePlaybackOverrideRevokesOnlyTargetCapabilities(t *testing.T) {
	store := openPlaybackTestStore(t)
	now := time.Date(2026, 10, 26, 22, 0, 0, 0, time.UTC)
	target := addPlaybackTestFixture(t, store, "feature-target", "feature-target", now)
	siblingDevice := addPlaybackTestDeviceForUser(
		t, store, target.user.ID, "Feature sibling", "feature-sibling", now)
	sibling := playbackTestFixture{
		store: store, user: target.user, device: siblingDevice, fileID: target.fileID,
	}
	other := addPlaybackTestFixture(t, store, "feature-other", "feature-other", now)

	targetTicket, targetMedia := createActivePlaybackCapability(t, store, target, now)
	siblingTicket, siblingMedia := createActivePlaybackCapability(
		t, store, sibling, now.Add(time.Second))
	otherTicket, otherMedia := createActivePlaybackCapability(
		t, store, other, now.Add(2*time.Second))

	subtitleID := addPlaybackSubtitle(
		t, store, target.fileID, "/feature-target/caption.srt", "subrip", false, 0)
	subtitleTicket := createConsumedPlaybackTicket(
		t, store, target, now.Add(3*time.Second), []int64{subtitleID})
	subtitleGrant, err := store.CreatePlaybackSubtitleSession(
		models.PlaybackSubtitleSessionRequest{
			Ticket:       playbackSubtitleTicketBinding(subtitleTicket),
			Transport:    models.PlaybackTransportDirectHTTP,
			SubtitleID:   subtitleID,
			StartSeconds: 0,
		},
		now.Add(4*time.Second),
	)
	if err != nil {
		t.Fatalf("create target subtitle session: %v", err)
	}

	overrideAt := now.Add(time.Minute)
	if _, err := store.UpdateDeviceFeatureOverrides(
		target.device.PublicID,
		models.DeviceFeatureOverrides{PlaybackTicketDisabled: true},
		target.user.ID,
		overrideAt,
	); err != nil {
		t.Fatalf("disable target playback: %v", err)
	}

	var ticketState, mediaState, subtitleState string
	if err := store.DB.QueryRow(`SELECT state FROM playback_tickets WHERE ticket_id = ?`,
		targetTicket.TicketID).Scan(&ticketState); err != nil {
		t.Fatalf("read target ticket state: %v", err)
	}
	if err := store.DB.QueryRow(`SELECT state FROM playback_media_sessions WHERE id = ?`,
		targetMedia.Session.ID).Scan(&mediaState); err != nil {
		t.Fatalf("read target media state: %v", err)
	}
	if err := store.DB.QueryRow(`SELECT state FROM playback_subtitle_sessions WHERE id = ?`,
		subtitleGrant.Session.ID).Scan(&subtitleState); err != nil {
		t.Fatalf("read target subtitle state: %v", err)
	}
	if ticketState != models.PlaybackTicketRevoked ||
		mediaState != models.PlaybackMediaSessionRevoked ||
		subtitleState != models.PlaybackSubtitleSessionRevoked {
		t.Fatalf("target states = ticket %q, media %q, subtitle %q",
			ticketState, mediaState, subtitleState)
	}

	for _, fixture := range []playbackTestFixture{sibling, other} {
		var state string
		ticketID := siblingTicket.TicketID
		if fixture.user.ID == other.user.ID {
			ticketID = otherTicket.TicketID
		}
		if err := store.DB.QueryRow(`SELECT state FROM playback_tickets
			WHERE ticket_id = ?`, ticketID).Scan(&state); err != nil {
			t.Fatalf("read unaffected ticket state: %v", err)
		}
		if state != models.PlaybackTicketConsuming {
			t.Fatalf("unaffected ticket %s state = %q", ticketID, state)
		}
	}
	if _, err := store.AuthorizePlaybackMediaSession(
		siblingMedia.Capability, false, overrideAt.Add(time.Second)); err != nil {
		t.Fatalf("authorize sibling media: %v", err)
	}
	if _, err := store.AuthorizePlaybackMediaSession(
		otherMedia.Capability, false, overrideAt.Add(time.Second)); err != nil {
		t.Fatalf("authorize other user's media: %v", err)
	}
	if _, err := store.AuthorizePlaybackMediaSession(
		targetMedia.Capability, false, overrideAt.Add(time.Second)); !errors.Is(
		err, ErrPlaybackMediaSessionRevoked) {
		t.Fatalf("disabled target media error = %v", err)
	}
	if _, err := store.AuthorizePlaybackSubtitleSession(
		subtitleGrant.Capability, false, overrideAt.Add(time.Second)); !errors.Is(
		err, ErrPlaybackSubtitleSessionRevoked) {
		t.Fatalf("disabled target subtitle error = %v", err)
	}
	if _, err := store.CreatePlaybackTicket(models.PlaybackTicketRequest{
		UserID: target.user.ID, DeviceID: target.device.ID, FileID: target.fileID,
		CandidateTransports: []string{models.PlaybackTransportServerProxy},
	}, overrideAt.Add(time.Second)); !errors.Is(err, ErrDevicePlaybackDisabled) {
		t.Fatalf("create ticket after target playback disable = %v", err)
	}
	if _, err := store.CreatePlaybackTicket(models.PlaybackTicketRequest{
		UserID: sibling.user.ID, DeviceID: sibling.device.ID, FileID: sibling.fileID,
		CandidateTransports: []string{models.PlaybackTransportServerProxy},
	}, overrideAt.Add(time.Second)); err != nil {
		t.Fatalf("create sibling ticket: %v", err)
	}
}

func TestDeviceAuthOverrideInvalidatesOnlyTargetTokens(t *testing.T) {
	store := openDeviceAuthStore(t)
	now := time.Date(2026, 10, 26, 23, 0, 0, 0, time.UTC)
	user := createDeviceAuthUser(t, store, "feature-auth-user")
	target := createDeviceAuthSession(t, store, user, "Target device", now)
	sibling := createDeviceAuthSession(t, store, user, "Sibling device", now.Add(time.Second))

	overrideAt := now.Add(time.Minute)
	if _, err := store.UpdateDeviceFeatureOverrides(
		target.Device.PublicID,
		models.DeviceFeatureOverrides{DeviceAuthDisabled: true},
		user.ID,
		overrideAt,
	); err != nil {
		t.Fatalf("disable target auth: %v", err)
	}
	if _, err := store.DeviceByAccessToken(
		target.AccessToken, overrideAt.Add(time.Second)); !errors.Is(
		err, ErrDeviceAuthFeatureDisabled) {
		t.Fatalf("disabled target access token error = %v", err)
	}
	if _, err := store.RotateDeviceRefreshToken(
		target.RefreshToken, overrideAt.Add(time.Second)); !errors.Is(
		err, ErrDeviceAuthFeatureDisabled) {
		t.Fatalf("disabled target refresh token error = %v", err)
	}
	if _, err := store.DeviceByAccessToken(
		sibling.AccessToken, overrideAt.Add(time.Second)); err != nil {
		t.Fatalf("sibling access token: %v", err)
	}
	if _, err := store.RotateDeviceRefreshToken(
		sibling.RefreshToken, overrideAt.Add(time.Second)); err != nil {
		t.Fatalf("sibling refresh token: %v", err)
	}

	var targetID int64
	if err := store.DB.QueryRow(`SELECT id FROM devices WHERE public_id = ?`,
		target.Device.PublicID).Scan(&targetID); err != nil {
		t.Fatalf("read target device id: %v", err)
	}
	var liveTokens int
	if err := store.DB.QueryRow(`SELECT
		(SELECT COUNT(*) FROM device_access_tokens
			WHERE device_id = ? AND revoked_at IS NULL) +
		(SELECT COUNT(*) FROM device_refresh_tokens
			WHERE device_id = ? AND revoked_at IS NULL)`,
		targetID, targetID).Scan(&liveTokens); err != nil {
		t.Fatalf("count target live tokens: %v", err)
	}
	if liveTokens != 0 {
		t.Fatalf("target has %d live tokens after auth disable", liveTokens)
	}

	if _, err := store.UpdateDeviceFeatureOverrides(
		target.Device.PublicID, models.DeviceFeatureOverrides{}, user.ID,
		overrideAt.Add(time.Minute)); err != nil {
		t.Fatalf("clear target auth disable: %v", err)
	}
	if _, err := store.DeviceByAccessToken(
		target.AccessToken, overrideAt.Add(2*time.Minute)); !errors.Is(
		err, ErrDeviceAccessTokenRevoked) {
		t.Fatalf("cleared override resurrected access token: %v", err)
	}
	if _, err := store.RotateDeviceRefreshToken(
		target.RefreshToken, overrideAt.Add(2*time.Minute)); !errors.Is(
		err, ErrDeviceRefreshTokenRevoked) {
		t.Fatalf("cleared override resurrected refresh token: %v", err)
	}
}

func TestDeviceFeatureOverridesRejectUnknownDevice(t *testing.T) {
	store := openDeviceAuthStore(t)
	user := createDeviceAuthUser(t, store, "feature-missing")
	if _, err := store.UpdateDeviceFeatureOverrides(
		"dev_missing_00000001",
		models.DeviceFeatureOverrides{PlaybackTicketDisabled: true},
		user.ID,
		time.Now().UTC(),
	); !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("unknown device override error = %v", err)
	}
}
