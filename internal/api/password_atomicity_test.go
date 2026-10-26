package api

import (
	"net/http"
	"testing"
)

func TestPasswordChangeRollsBackWhenDeviceRevocationFails(t *testing.T) {
	srv, ts := newDeviceAuthServer(t)
	admin, _ := v1SetupAdmin(t, ts.URL)
	v1CreateViewer(t, ts.URL, admin, "alice", "alice-pass")
	alice, _ := v1Login(t, ts.URL, "alice", "alice-pass")
	_, pair := v1IssueDevice(t, ts.URL, alice, "Alice desktop")

	if _, err := srv.Store.DB.Exec(`CREATE TRIGGER injected_api_password_revoke_failure
		BEFORE UPDATE ON device_refresh_tokens
		BEGIN
			SELECT RAISE(ABORT, 'injected API password revocation failure');
		END`); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}

	change := v1Call(t, alice, http.MethodPost, ts.URL+"/api/auth/password", map[string]string{
		"current_password": "alice-pass",
		"new_password":     "alice-pass-2",
	}, nil)
	if change.Status != http.StatusInternalServerError {
		t.Fatalf("password change status = %d, want 500; body %s", change.Status, change.Body)
	}

	if me := v1Call(t, alice, http.MethodGet, ts.URL+"/api/auth/me", nil, nil); me.Status != http.StatusOK {
		t.Errorf("current Web session after failed change = %d, want 200", me.Status)
	}
	oldLogin, _ := v1Login(t, ts.URL, "alice", "alice-pass")
	if oldLogin == nil {
		t.Fatal("old password login failed after a rolled-back rotation")
	}
	device := v1Call(t, browser(t), http.MethodGet, ts.URL+"/api/v1/devices", nil,
		map[string]string{"Authorization": "Bearer " + v1StringField(t, pair, "access_token")})
	if device.Status != http.StatusOK {
		t.Errorf("device token after failed change = %d, want 200; body %s", device.Status, device.Body)
	}
}
