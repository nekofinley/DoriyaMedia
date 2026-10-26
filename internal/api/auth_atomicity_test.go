package api

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"testing"

	"doriyamedia/internal/models"
)

type apiAuthAtomicFixture struct {
	srv       *Server
	base      string
	admin     *http.Client
	viewer    *http.Client
	userID    int64
	access    string
	authEpoch int64
}

func newAPIAuthAtomicFixture(t *testing.T) apiAuthAtomicFixture {
	t.Helper()
	srv, ts := newDeviceAuthServer(t)
	admin, _ := v1SetupAdmin(t, ts.URL)
	userID := v1CreateViewer(t, ts.URL, admin, "atomic-viewer", "viewer-pass")
	viewer, _ := v1Login(t, ts.URL, "atomic-viewer", "viewer-pass")
	_, pair := v1IssueDevice(t, ts.URL, viewer, "Atomic viewer device")
	var epoch int64
	if err := srv.Store.DB.QueryRow(`SELECT auth_epoch FROM users WHERE id = ?`,
		userID).Scan(&epoch); err != nil {
		t.Fatalf("read viewer auth epoch: %v", err)
	}
	return apiAuthAtomicFixture{
		srv: srv, base: ts.URL, admin: admin, viewer: viewer, userID: userID,
		access: v1StringField(t, pair, "access_token"), authEpoch: epoch,
	}
}

func (f apiAuthAtomicFixture) deviceCall(t *testing.T) v1HTTPResponse {
	t.Helper()
	return v1Call(t, browser(t), http.MethodGet, f.base+"/api/v1/devices", nil,
		map[string]string{"Authorization": "Bearer " + f.access})
}

func TestAdminDisableRollsBackCredentialRevocation(t *testing.T) {
	f := newAPIAuthAtomicFixture(t)
	if _, err := f.srv.Store.DB.Exec(`CREATE TRIGGER injected_api_disable_failure
		BEFORE UPDATE ON device_refresh_tokens
		BEGIN
			SELECT RAISE(ABORT, 'injected API disable failure');
		END`); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}

	disable := v1Call(t, f.admin, http.MethodPut,
		f.base+"/api/admin/users/"+strconv.FormatInt(f.userID, 10),
		map[string]string{"status": models.StatusDisabled}, nil)
	if disable.Status != http.StatusInternalServerError {
		t.Fatalf("disable with injected failure = %d, want 500; body %s",
			disable.Status, disable.Body)
	}
	user, err := f.srv.Store.UserByID(f.userID)
	if err != nil {
		t.Fatalf("read viewer after failed disable: %v", err)
	}
	if user.Status != models.StatusActive {
		t.Errorf("status after failed disable = %q, want active", user.Status)
	}
	var epoch int64
	if err := f.srv.Store.DB.QueryRow(`SELECT auth_epoch FROM users WHERE id = ?`,
		f.userID).Scan(&epoch); err != nil {
		t.Fatalf("read epoch after failed disable: %v", err)
	}
	if epoch != f.authEpoch {
		t.Errorf("epoch after failed disable = %d, want %d", epoch, f.authEpoch)
	}
	if me := v1Call(t, f.viewer, http.MethodGet, f.base+"/api/auth/me", nil, nil); me.Status != http.StatusOK {
		t.Errorf("Web session after failed disable = %d, want 200; body %s", me.Status, me.Body)
	}
	if devices := f.deviceCall(t); devices.Status != http.StatusOK {
		t.Errorf("device token after failed disable = %d, want 200; body %s",
			devices.Status, devices.Body)
	}

	if _, err := f.srv.Store.DB.Exec(`DROP TRIGGER injected_api_disable_failure`); err != nil {
		t.Fatalf("drop failure trigger: %v", err)
	}
	disable = v1Call(t, f.admin, http.MethodPut,
		f.base+"/api/admin/users/"+strconv.FormatInt(f.userID, 10),
		map[string]string{"status": models.StatusDisabled}, nil)
	if disable.Status != http.StatusOK {
		t.Fatalf("disable = %d, want 200; body %s", disable.Status, disable.Body)
	}
	if me := v1Call(t, f.viewer, http.MethodGet, f.base+"/api/auth/me", nil, nil); me.Status != http.StatusUnauthorized {
		t.Errorf("Web session after disable = %d, want 401; body %s", me.Status, me.Body)
	}
	if devices := f.deviceCall(t); devices.Status != http.StatusUnauthorized &&
		devices.Status != http.StatusForbidden {
		t.Errorf("device token after disable = %d, want 401 or 403; body %s",
			devices.Status, devices.Body)
	}

	reenable := v1Call(t, f.admin, http.MethodPut,
		f.base+"/api/admin/users/"+strconv.FormatInt(f.userID, 10),
		map[string]string{"status": models.StatusActive}, nil)
	if reenable.Status != http.StatusOK {
		t.Fatalf("re-enable = %d, want 200; body %s", reenable.Status, reenable.Body)
	}
	if me := v1Call(t, f.viewer, http.MethodGet, f.base+"/api/auth/me", nil, nil); me.Status != http.StatusUnauthorized {
		t.Errorf("old Web session revived after re-enable = %d, want 401", me.Status)
	}
	if devices := f.deviceCall(t); devices.Status != http.StatusUnauthorized {
		t.Errorf("old device token revived after re-enable = %d, want 401", devices.Status)
	}
	login := v1Call(t, browser(t), http.MethodPost, f.base+"/api/auth/login",
		map[string]string{"name": "atomic-viewer", "password": "viewer-pass"}, nil)
	if login.Status != http.StatusOK {
		t.Fatalf("login after re-enable = %d, want 200; body %s", login.Status, login.Body)
	}
}

func TestAdminKickRollsBackCredentialRevocation(t *testing.T) {
	f := newAPIAuthAtomicFixture(t)
	if _, err := f.srv.Store.DB.Exec(`CREATE TRIGGER injected_api_kick_failure
		BEFORE UPDATE ON device_refresh_tokens
		BEGIN
			SELECT RAISE(ABORT, 'injected API kick failure');
		END`); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}

	revokePath := f.base + "/api/admin/users/" + strconv.FormatInt(f.userID, 10) + "/revoke"
	kick := v1Call(t, f.admin, http.MethodPost, revokePath, nil, nil)
	if kick.Status != http.StatusInternalServerError {
		t.Fatalf("kick with injected failure = %d, want 500; body %s", kick.Status, kick.Body)
	}
	var epoch int64
	if err := f.srv.Store.DB.QueryRow(`SELECT auth_epoch FROM users WHERE id = ?`,
		f.userID).Scan(&epoch); err != nil {
		t.Fatalf("read epoch after failed kick: %v", err)
	}
	if epoch != f.authEpoch {
		t.Errorf("epoch after failed kick = %d, want %d", epoch, f.authEpoch)
	}
	if me := v1Call(t, f.viewer, http.MethodGet, f.base+"/api/auth/me", nil, nil); me.Status != http.StatusOK {
		t.Errorf("Web session after failed kick = %d, want 200; body %s", me.Status, me.Body)
	}
	if devices := f.deviceCall(t); devices.Status != http.StatusOK {
		t.Errorf("device token after failed kick = %d, want 200; body %s",
			devices.Status, devices.Body)
	}

	if _, err := f.srv.Store.DB.Exec(`DROP TRIGGER injected_api_kick_failure`); err != nil {
		t.Fatalf("drop failure trigger: %v", err)
	}
	kick = v1Call(t, f.admin, http.MethodPost, revokePath, nil, nil)
	if kick.Status != http.StatusOK {
		t.Fatalf("kick = %d, want 200; body %s", kick.Status, kick.Body)
	}
	if me := v1Call(t, f.viewer, http.MethodGet, f.base+"/api/auth/me", nil, nil); me.Status != http.StatusUnauthorized {
		t.Errorf("Web session after kick = %d, want 401; body %s", me.Status, me.Body)
	}
	if devices := f.deviceCall(t); devices.Status != http.StatusUnauthorized {
		t.Errorf("device token after kick = %d, want 401; body %s", devices.Status, devices.Body)
	}
	if err := f.srv.Store.DB.QueryRow(`SELECT auth_epoch FROM users WHERE id = ?`,
		f.userID).Scan(&epoch); err != nil {
		t.Fatalf("read epoch after kick: %v", err)
	}
	if epoch <= f.authEpoch {
		t.Errorf("epoch after kick = %d, want > %d", epoch, f.authEpoch)
	}

	login := v1Call(t, browser(t), http.MethodPost, f.base+"/api/auth/login",
		map[string]string{"name": "atomic-viewer", "password": "viewer-pass"}, nil)
	if login.Status != http.StatusOK {
		t.Fatalf("fresh login after kick = %d, want 200; body %s", login.Status, login.Body)
	}
}

func TestConcurrentLoginAndPasswordChangeLeaveNoOldWebSession(t *testing.T) {
	srv, ts := newTestServer(t)
	admin := setupAdmin(t, ts.URL)
	for i := 0; i < 8; i++ {
		name := "login-change-race-" + strconv.Itoa(i)
		v1CreateViewer(t, ts.URL, admin, name, "race-old-pass")
		changer, _ := v1Login(t, ts.URL, name, "race-old-pass")

		start := make(chan struct{})
		var (
			wg         sync.WaitGroup
			loginResp  v1HTTPResponse
			changeResp v1HTTPResponse
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			loginResp = v1Call(t, browser(t), http.MethodPost, ts.URL+"/api/auth/login",
				map[string]string{"name": name, "password": "race-old-pass"}, nil)
		}()
		go func() {
			defer wg.Done()
			<-start
			changeResp = v1Call(t, changer, http.MethodPost, ts.URL+"/api/auth/password",
				map[string]string{
					"current_password": "race-old-pass",
					"new_password":     "race-new-pass",
				}, nil)
		}()
		close(start)
		wg.Wait()

		if changeResp.Status != http.StatusOK {
			t.Fatalf("iteration %d password change = %d; body %s",
				i, changeResp.Status, changeResp.Body)
		}
		if loginResp.Status == http.StatusOK {
			token, _ := loginResp.JSON["token"].(string)
			if token == "" {
				t.Fatalf("iteration %d login response omitted its token: %v", i, loginResp.JSON)
			}
			if _, err := srv.Store.SessionByToken(token); err == nil {
				t.Fatalf("iteration %d old-password login created a usable session", i)
			} else if !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("iteration %d stale session lookup: %v", i, err)
			}
			me := v1Call(t, browser(t), http.MethodGet, ts.URL+"/api/auth/me", nil,
				map[string]string{"Authorization": "Bearer " + token})
			if me.Status != http.StatusUnauthorized {
				t.Fatalf("iteration %d stale login token on /me = %d, want 401",
					i, me.Status)
			}
		} else if loginResp.Status != http.StatusUnauthorized {
			t.Fatalf("iteration %d login = %d, want 200 or 401; body %s",
				i, loginResp.Status, loginResp.Body)
		}

		oldLogin := v1Call(t, browser(t), http.MethodPost, ts.URL+"/api/auth/login",
			map[string]string{"name": name, "password": "race-old-pass"}, nil)
		if oldLogin.Status != http.StatusUnauthorized {
			t.Fatalf("iteration %d old password after change = %d, want 401",
				i, oldLogin.Status)
		}
		newLogin := v1Call(t, browser(t), http.MethodPost, ts.URL+"/api/auth/login",
			map[string]string{"name": name, "password": "race-new-pass"}, nil)
		if newLogin.Status != http.StatusOK {
			t.Fatalf("iteration %d new password after change = %d, want 200; body %s",
				i, newLogin.Status, newLogin.Body)
		}
	}
}
