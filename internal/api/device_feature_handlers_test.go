package api

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
	"time"

	"doriyamedia/internal/config"
	"doriyamedia/internal/models"
)

func TestEffectiveClientFeaturesAreDenyOnly(t *testing.T) {
	global := config.ClientFeatures{
		V1Enabled:             true,
		DeviceAuthEnabled:     true,
		PlaybackTicketEnabled: true,
		DirectHTTPEnabled:     true,
		ServerProxyV1Enabled:  true,
		ServerHLSV1Enabled:    true,
		DirectDASHEnabled:     true,
		LocalRelayEnabled:     true,
		NativePlayerEnabled:   true,
		LibMPVFallbackEnabled: true,
	}
	if got := v1EffectiveClientFeatures(global, models.DeviceFeatureOverrides{}); got != global {
		t.Fatalf("empty overrides changed global features: %+v", got)
	}

	allDenied := models.DeviceFeatureOverrides{
		ClientV1Disabled:       true,
		DeviceAuthDisabled:     true,
		PlaybackTicketDisabled: true,
		DirectHTTPDisabled:     true,
		ServerProxyV1Disabled:  true,
		ServerHLSV1Disabled:    true,
		DirectDASHDisabled:     true,
		LocalRelayDisabled:     true,
		NativePlayerDisabled:   true,
		LibMPVFallbackDisabled: true,
	}
	if got := v1EffectiveClientFeatures(global, allDenied); got != (config.ClientFeatures{}) {
		t.Fatalf("all deny overrides did not disable all features: %+v", got)
	}

	disabledGlobal := config.ClientFeatures{}
	if got := v1EffectiveClientFeatures(disabledGlobal, models.DeviceFeatureOverrides{}); got != disabledGlobal {
		t.Fatalf("empty overrides enabled globally disabled features: %+v", got)
	}
}

func TestAdminClientDevicesSafeProjectionAndIsolation(t *testing.T) {
	srv, ts := newDeviceAuthServer(t)
	srv.Cfg.Client.PlaybackTicketEnabled = true
	srv.Cfg.Client.ServerProxyV1Enabled = true
	admin, _ := v1SetupAdmin(t, ts.URL)
	v1CreateViewer(t, ts.URL, admin, "alice", "alice-pass")
	viewer, _ := v1Login(t, ts.URL, "alice", "alice-pass")
	_, targetPair := v1IssueDevice(t, ts.URL, viewer, "Emergency target")
	_, siblingPair := v1IssueDevice(t, ts.URL, viewer, "Sibling device")
	targetDevice := v1ObjectField(t, targetPair, "device")
	siblingDevice := v1ObjectField(t, siblingPair, "device")
	targetID := v1StringField(t, targetDevice, "id")
	siblingID := v1StringField(t, siblingDevice, "id")
	targetAccess := v1StringField(t, targetPair, "access_token")
	targetRefresh := v1StringField(t, targetPair, "refresh_token")
	siblingAccess := v1StringField(t, siblingPair, "access_token")

	list := v1Call(t, admin, http.MethodGet, ts.URL+"/api/admin/client-devices", nil, nil)
	if list.Status != http.StatusOK {
		t.Fatalf("admin device list status = %d; body %s", list.Status, list.Body)
	}
	devices, ok := list.JSON["devices"].([]any)
	if !ok || len(devices) != 2 {
		t.Fatalf("admin device list = %v", list.JSON)
	}
	for _, forbidden := range []string{
		"token_hash", "access_token", "refresh_token", "authorization_id",
		"source_url", "source_headers", "ip_hash",
	} {
		if bytes.Contains(list.Body, []byte(forbidden)) {
			t.Fatalf("admin device list leaked %q: %s", forbidden, list.Body)
		}
	}

	if nonAdmin := v1Call(t, viewer, http.MethodGet,
		ts.URL+"/api/admin/client-devices", nil, nil); nonAdmin.Status != http.StatusForbidden {
		t.Fatalf("non-admin device list status = %d; body %s", nonAdmin.Status, nonAdmin.Body)
	}
	nonAdminExisting := v1Call(t, viewer, http.MethodPut,
		ts.URL+"/api/admin/client-devices/"+targetID+"/features",
		models.DeviceFeatureOverrides{}, nil)
	nonAdminMissing := v1Call(t, viewer, http.MethodPut,
		ts.URL+"/api/admin/client-devices/dev_missing_00000001/features",
		models.DeviceFeatureOverrides{}, nil)
	if nonAdminExisting.Status != http.StatusForbidden ||
		nonAdminMissing.Status != nonAdminExisting.Status ||
		!bytes.Equal(nonAdminMissing.Body, nonAdminExisting.Body) {
		t.Fatalf("non-admin update leaked device existence: existing %d %s; missing %d %s",
			nonAdminExisting.Status, nonAdminExisting.Body,
			nonAdminMissing.Status, nonAdminMissing.Body)
	}
	if missing := v1Call(t, admin, http.MethodPut,
		ts.URL+"/api/admin/client-devices/dev_missing_00000001/features",
		models.DeviceFeatureOverrides{}, nil); missing.Status != http.StatusNotFound {
		t.Fatalf("missing device update status = %d; body %s", missing.Status, missing.Body)
	}
	if unknownField := v1Call(t, admin, http.MethodPut,
		ts.URL+"/api/admin/client-devices/"+targetID+"/features",
		map[string]any{"v1_enabled": true}, nil); unknownField.Status != http.StatusBadRequest {
		t.Fatalf("unknown feature update status = %d; body %s", unknownField.Status, unknownField.Body)
	}
	if nullBody := v1Call(t, admin, http.MethodPut,
		ts.URL+"/api/admin/client-devices/"+targetID+"/features",
		(*models.DeviceFeatureOverrides)(nil), nil); nullBody.Status != http.StatusBadRequest {
		t.Fatalf("null feature update status = %d; body %s", nullBody.Status, nullBody.Body)
	}

	playbackOff := v1Call(t, admin, http.MethodPut,
		ts.URL+"/api/admin/client-devices/"+targetID+"/features",
		models.DeviceFeatureOverrides{PlaybackTicketDisabled: true}, nil)
	if playbackOff.Status != http.StatusOK {
		t.Fatalf("disable target playback status = %d; body %s", playbackOff.Status, playbackOff.Body)
	}
	overrides := v1ObjectField(t, playbackOff.JSON, "overrides")
	if enabled, ok := overrides["playback_ticket_disabled"].(bool); !ok || !enabled {
		t.Fatalf("playback override was not returned: %v", overrides)
	}
	targetDevices := v1Call(t, admin, http.MethodGet,
		ts.URL+"/api/v1/devices", nil, map[string]string{
			"Authorization": "Bearer " + targetAccess,
		})
	if targetDevices.Status != http.StatusOK {
		t.Fatalf("playback-only override blocked device list: %d %s",
			targetDevices.Status, targetDevices.Body)
	}
	ticketDenied := v1Call(t, admin, http.MethodPost,
		ts.URL+"/api/v1/playback/tickets",
		map[string]any{"file_id": 1}, map[string]string{
			"Authorization": "Bearer " + targetAccess,
		})
	v1AssertError(t, ticketDenied, http.StatusNotFound, V1ErrorFeatureDisabled)
	siblingRequest := v1Call(t, admin, http.MethodPost,
		ts.URL+"/api/v1/playback/tickets",
		map[string]any{"file_id": 1}, map[string]string{
			"Authorization": "Bearer " + siblingAccess,
		})
	if siblingRequest.Status == http.StatusNotFound {
		t.Fatalf("sibling inherited target playback override: %s", siblingRequest.Body)
	}

	authOff := v1Call(t, admin, http.MethodPut,
		ts.URL+"/api/admin/client-devices/"+targetID+"/features",
		models.DeviceFeatureOverrides{DeviceAuthDisabled: true}, nil)
	if authOff.Status != http.StatusOK {
		t.Fatalf("disable target auth status = %d; body %s", authOff.Status, authOff.Body)
	}
	targetDevices = v1Call(t, admin, http.MethodGet,
		ts.URL+"/api/v1/devices", nil, map[string]string{
			"Authorization": "Bearer " + targetAccess,
		})
	v1AssertError(t, targetDevices, http.StatusForbidden, V1ErrorFeatureDisabled)
	refreshDenied := v1Call(t, admin, http.MethodPost,
		ts.URL+"/api/v1/auth/refresh",
		map[string]string{"refresh_token": targetRefresh}, nil)
	v1AssertError(t, refreshDenied, http.StatusUnauthorized, V1ErrorRefreshTokenInvalid)
	siblingDevices := v1Call(t, admin, http.MethodGet,
		ts.URL+"/api/v1/devices", nil, map[string]string{
			"Authorization": "Bearer " + siblingAccess,
		})
	if siblingDevices.Status != http.StatusOK {
		t.Fatalf("sibling device list after target auth disable: %d %s",
			siblingDevices.Status, siblingDevices.Body)
	}

	_, v1DisabledPair := v1IssueDevice(t, ts.URL, viewer, "V1 disabled device")
	v1Device := v1ObjectField(t, v1DisabledPair, "device")
	v1DeviceID := v1StringField(t, v1Device, "id")
	v1Access := v1StringField(t, v1DisabledPair, "access_token")
	v1Refresh := v1StringField(t, v1DisabledPair, "refresh_token")
	v1Off := v1Call(t, admin, http.MethodPut,
		ts.URL+"/api/admin/client-devices/"+v1DeviceID+"/features",
		models.DeviceFeatureOverrides{ClientV1Disabled: true}, nil)
	if v1Off.Status != http.StatusOK {
		t.Fatalf("disable target v1 status = %d; body %s", v1Off.Status, v1Off.Body)
	}
	for _, route := range []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodGet, "/api/v1/devices", nil},
		{http.MethodPost, "/api/v1/playback/tickets", map[string]any{"file_id": 1}},
		{http.MethodPost, "/api/v1/auth/logout", nil},
	} {
		resp := v1Call(t, admin, route.method, ts.URL+route.path, route.body,
			map[string]string{"Authorization": "Bearer " + v1Access})
		v1AssertError(t, resp, http.StatusForbidden, V1ErrorFeatureDisabled)
	}
	v1RefreshDenied := v1Call(t, admin, http.MethodPost,
		ts.URL+"/api/v1/auth/refresh",
		map[string]string{"refresh_token": v1Refresh}, nil)
	v1AssertError(t, v1RefreshDenied, http.StatusUnauthorized, V1ErrorRefreshTokenInvalid)

	if siblingID == "" || targetID == v1DeviceID {
		t.Fatal("test device ids are not distinct")
	}
}

func TestAdminClientDeviceListOmitsUnsafeFields(t *testing.T) {
	_, ts := newDeviceAuthServer(t)
	admin, _ := v1SetupAdmin(t, ts.URL)
	v1CreateViewer(t, ts.URL, admin, "bob", "bob-pass")
	viewer, _ := v1Login(t, ts.URL, "bob", "bob-pass")
	v1IssueDevice(t, ts.URL, viewer, "Safe projection")
	resp := v1Call(t, admin, http.MethodGet,
		ts.URL+"/api/admin/client-devices", nil, nil)
	if resp.Status != http.StatusOK {
		t.Fatalf("admin device list status = %d; body %s", resp.Status, resp.Body)
	}
	lower := strings.ToLower(string(resp.Body))
	for _, forbidden := range []string{"bearer ", "sha256", "password", "cookie"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("admin device list contains %q: %s", forbidden, resp.Body)
		}
	}
}

func TestDevicePlaybackOverrideRevokesExistingTicketAndMediaCapability(t *testing.T) {
	f := newPlaybackAPIFixture(t)
	f.server.Cfg.Client = config.ClientFeatures{
		V1Enabled:             true,
		DeviceAuthEnabled:     true,
		PlaybackTicketEnabled: true,
		ServerProxyV1Enabled:  true,
	}
	fileID := seedPlaybackAPIFile(t, f.server.Store,
		"https://cdn.example.test/clip.mp4", playbackAPITestProbe)
	create := f.call(t, http.MethodPost, "/api/v1/playback/tickets",
		playbackCreateBody(fileID, f.deviceID, "server-proxy"))
	if create.Status != http.StatusCreated {
		t.Fatalf("create playback ticket = %d; body %s", create.Status, create.Body)
	}
	ticketID := v1StringField(t, create.JSON, "ticket_id")
	transport := v1ObjectField(t, create.JSON, "transport")
	stream := transport["streams"].([]any)[0].(map[string]any)
	mediaURL := strings.TrimPrefix(stream["url"].(string), "https://media.example")

	var userID, deviceID int64
	if err := f.server.Store.DB.QueryRow(`SELECT user_id, id FROM devices
		WHERE public_id = ?`, f.deviceID).Scan(&userID, &deviceID); err != nil {
		t.Fatalf("read playback device: %v", err)
	}
	if _, err := f.server.Store.UpdateDeviceFeatureOverrides(
		f.deviceID,
		models.DeviceFeatureOverrides{ServerProxyV1Disabled: true},
		userID,
		time.Now().UTC(),
	); err != nil {
		t.Fatalf("disable target playback transport: %v", err)
	}

	refresh := f.call(t, http.MethodPost,
		"/api/v1/playback/tickets/"+ticketID+"/refresh", nil)
	v1AssertError(t, refresh, http.StatusForbidden, V1ErrorForbidden)

	media := v1Call(t, http.DefaultClient, http.MethodGet,
		f.http.URL+mediaURL, nil, nil)
	v1AssertError(t, media, http.StatusForbidden, V1ErrorForbidden)

	var ticketState, mediaState string
	if err := f.server.Store.DB.QueryRow(`SELECT state FROM playback_tickets
		WHERE ticket_id = ?`, ticketID).Scan(&ticketState); err != nil {
		t.Fatalf("read target ticket state: %v", err)
	}
	if ticketState != models.PlaybackTicketRevoked {
		t.Fatalf("target ticket state = %q", ticketState)
	}
	if err := f.server.Store.DB.QueryRow(`SELECT state FROM playback_media_sessions
		WHERE device_id = ?`, deviceID).Scan(&mediaState); err != nil {
		t.Fatalf("read target media state: %v", err)
	}
	if mediaState != models.PlaybackMediaSessionRevoked {
		t.Fatalf("target media state = %q", mediaState)
	}
}
