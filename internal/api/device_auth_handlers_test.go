package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"doriyamedia/internal/auth"
	"doriyamedia/internal/config"
	"doriyamedia/internal/db"
	"doriyamedia/internal/models"
)

type v1HTTPResponse struct {
	Status int
	Header http.Header
	JSON   map[string]any
	Body   []byte
}

func newDeviceAuthServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	srv, ts := newTestServer(t)
	srv.Cfg.Client = config.ClientFeatures{V1Enabled: true, DeviceAuthEnabled: true}
	return srv, ts
}

func v1Call(t *testing.T, client *http.Client, method, endpoint string, body any, headers map[string]string) v1HTTPResponse {
	t.Helper()
	var reader io.Reader
	if body != nil {
		switch raw := body.(type) {
		case []byte:
			reader = bytes.NewReader(raw)
		default:
			encoded, err := json.Marshal(body)
			if err != nil {
				t.Fatalf("marshal request body: %v", err)
			}
			reader = bytes.NewReader(encoded)
		}
	}
	req, err := http.NewRequest(method, endpoint, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	if body != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, endpoint, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	out := v1HTTPResponse{Status: resp.StatusCode, Header: resp.Header.Clone(), Body: raw}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &out.JSON); err != nil {
			t.Fatalf("decode %s %s response: %v; body %q", method, endpoint, err, raw)
		}
	}
	return out
}

func v1SetupAdmin(t *testing.T, base string) (*http.Client, string) {
	t.Helper()
	client := browser(t)
	resp := v1Call(t, client, http.MethodPost, base+"/api/setup", map[string]string{
		"name": "admin", "password": "admin-pass",
	}, nil)
	if resp.Status != http.StatusOK {
		t.Fatalf("setup status = %d, want 200; body %s", resp.Status, resp.Body)
	}
	token, _ := resp.JSON["token"].(string)
	if token == "" {
		t.Fatal("setup response omitted the Web session token")
	}
	return client, token
}

func v1Login(t *testing.T, base, name, password string) (*http.Client, string) {
	t.Helper()
	client := browser(t)
	resp := v1Call(t, client, http.MethodPost, base+"/api/auth/login", map[string]string{
		"name": name, "password": password,
	}, nil)
	if resp.Status != http.StatusOK {
		t.Fatalf("login %s status = %d, want 200; body %s", name, resp.Status, resp.Body)
	}
	token, _ := resp.JSON["token"].(string)
	if token == "" {
		t.Fatalf("login %s omitted the Web session token", name)
	}
	return client, token
}

func v1CreateViewer(t *testing.T, base string, admin *http.Client, name, password string) int64 {
	t.Helper()
	resp := v1Call(t, admin, http.MethodPost, base+"/api/admin/users", map[string]string{
		"name": name, "password": password, "role": models.RoleUser,
	}, nil)
	if resp.Status != http.StatusOK {
		t.Fatalf("create viewer %s status = %d, want 200; body %s", name, resp.Status, resp.Body)
	}
	user, _ := resp.JSON["user"].(map[string]any)
	id, ok := user["id"].(float64)
	if !ok || id <= 0 {
		t.Fatalf("create viewer %s response has no id: %v", name, resp.JSON)
	}
	return int64(id)
}

func v1PKCE(t *testing.T) (string, string) {
	t.Helper()
	verifier, err := auth.NewPKCEVerifier()
	if err != nil {
		t.Fatalf("new PKCE verifier: %v", err)
	}
	challenge, err := auth.PKCEChallengeS256(verifier)
	if err != nil {
		t.Fatalf("PKCE challenge: %v", err)
	}
	return verifier, challenge
}

func v1StartRequest(challenge string, overrides map[string]any) map[string]any {
	body := map[string]any{
		"device_name":           "Test Windows Desktop",
		"platform":              "windows",
		"client_version":        "0.1.0",
		"code_challenge":        challenge,
		"code_challenge_method": "S256",
	}
	for key, value := range overrides {
		body[key] = value
	}
	return body
}

func v1StartAuthorization(t *testing.T, base string, client *http.Client, body map[string]any) v1HTTPResponse {
	t.Helper()
	return v1Call(t, client, http.MethodPost, base+"/api/v1/auth/device/authorizations", body, nil)
}

func v1LookupAuthorization(t *testing.T, base string, web *http.Client, userCode string, headers map[string]string) v1HTTPResponse {
	t.Helper()
	return v1Call(t, web, http.MethodGet,
		base+"/api/v1/auth/device/authorizations/lookup?user_code="+url.QueryEscape(userCode), nil, headers)
}

func v1LookupCSRF(t *testing.T, base string, web *http.Client, userCode string) string {
	t.Helper()
	lookup := v1LookupAuthorization(t, base, web, userCode, nil)
	if lookup.Status != http.StatusOK {
		t.Fatalf("lookup for CSRF = %d; body %s", lookup.Status, lookup.Body)
	}
	return v1StringField(t, lookup.JSON, "csrf_token")
}

func v1Decision(t *testing.T, base string, web *http.Client, authorizationID, decision string, headers map[string]string) v1HTTPResponse {
	t.Helper()
	merged := map[string]string{
		"Origin": base,
	}
	for key, value := range headers {
		merged[key] = value
	}
	return v1Call(t, web, http.MethodPost,
		base+"/api/v1/auth/device/authorizations/"+url.PathEscape(authorizationID)+"/decision",
		map[string]string{"decision": decision}, merged)
}

func v1TokenExchange(t *testing.T, base string, client *http.Client, deviceCode, verifier string) v1HTTPResponse {
	t.Helper()
	return v1Call(t, client, http.MethodPost, base+"/api/v1/auth/device/token", map[string]string{
		"device_code": deviceCode, "code_verifier": verifier,
	}, nil)
}

func v1Refresh(t *testing.T, base string, client *http.Client, refreshToken string) v1HTTPResponse {
	t.Helper()
	return v1Call(t, client, http.MethodPost, base+"/api/v1/auth/refresh", map[string]string{
		"refresh_token": refreshToken,
	}, nil)
}

func v1IssueDevice(t *testing.T, base string, web *http.Client, name string) (map[string]any, map[string]any) {
	t.Helper()
	verifier, challenge := v1PKCE(t)
	start := v1StartAuthorization(t, base, browser(t), v1StartRequest(challenge, map[string]any{
		"device_name": name,
	}))
	if start.Status != http.StatusCreated {
		t.Fatalf("start %q status = %d, want 201; body %s", name, start.Status, start.Body)
	}
	startJSON := start.JSON
	authorizationID := v1StringField(t, startJSON, "authorization_id")
	deviceCode := v1StringField(t, startJSON, "device_code")
	csrfToken := v1LookupCSRF(t, base, web, v1StringField(t, startJSON, "user_code"))
	decision := v1Decision(t, base, web, authorizationID, "approve", map[string]string{
		"X-CSRF-Token": csrfToken,
	})
	if decision.Status != http.StatusOK {
		t.Fatalf("approve %q status = %d, want 200; body %s", name, decision.Status, decision.Body)
	}
	token := v1TokenExchange(t, base, browser(t), deviceCode, verifier)
	if token.Status != http.StatusOK {
		t.Fatalf("token %q status = %d, want 200; body %s", name, token.Status, token.Body)
	}
	return startJSON, token.JSON
}

func v1StringField(t *testing.T, body map[string]any, key string) string {
	t.Helper()
	value, _ := body[key].(string)
	if value == "" {
		t.Fatalf("field %q is missing or empty: %v", key, body)
	}
	return value
}

func v1ObjectField(t *testing.T, body map[string]any, key string) map[string]any {
	t.Helper()
	value, ok := body[key].(map[string]any)
	if !ok {
		t.Fatalf("field %q is not an object: %v", key, body)
	}
	return value
}

func v1ErrorCode(t *testing.T, resp v1HTTPResponse) string {
	t.Helper()
	envelope, ok := resp.JSON["error"].(map[string]any)
	if !ok {
		t.Fatalf("response has no error envelope: status %d body %s", resp.Status, resp.Body)
	}
	code, _ := envelope["code"].(string)
	if code == "" {
		t.Fatalf("error envelope has no code: %v", envelope)
	}
	return code
}

func v1AssertError(t *testing.T, resp v1HTTPResponse, status int, code V1ErrorCode) {
	t.Helper()
	if resp.Status != status {
		t.Fatalf("status = %d, want %d; body %s", resp.Status, status, resp.Body)
	}
	if got := v1ErrorCode(t, resp); got != string(code) {
		t.Fatalf("error code = %q, want %q; body %s", got, code, resp.Body)
	}
}

func v1Keys(body map[string]any) []string {
	keys := make([]string, 0, len(body))
	for key := range body {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func v1AssertKeys(t *testing.T, body map[string]any, want []string) {
	t.Helper()
	sort.Strings(want)
	if got := v1Keys(body); !reflect.DeepEqual(got, want) {
		t.Fatalf("fields = %v, want %v; body %v", got, want, body)
	}
}

func TestDeviceAuthFeatureFlagsFailClosed(t *testing.T) {
	srv, ts := newTestServer(t)
	_, challenge := v1PKCE(t)
	body := v1StartRequest(challenge, nil)
	authorizationID := "dva_feature_disabled_00000001"
	deviceID := "dev_feature_disabled_00001"
	requests := []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodPost, "/api/v1/auth/device/authorizations", body},
		{http.MethodGet, "/api/v1/auth/device/authorizations/" + authorizationID, nil},
		{http.MethodGet, "/api/v1/auth/device/authorizations/lookup?user_code=ABCDE-FGHJK", nil},
		{http.MethodPost, "/api/v1/auth/device/authorizations/" + authorizationID + "/decision", map[string]string{"decision": "approve"}},
		{http.MethodPost, "/api/v1/auth/device/token", map[string]string{"device_code": strings.Repeat("d", 32), "code_verifier": strings.Repeat("v", 43)}},
		{http.MethodPost, "/api/v1/auth/refresh", map[string]string{"refresh_token": strings.Repeat("r", 32)}},
		{http.MethodPost, "/api/v1/auth/logout", nil},
		{http.MethodGet, "/api/v1/devices", nil},
		{http.MethodDelete, "/api/v1/devices/" + deviceID, nil},
	}

	for _, flags := range []config.ClientFeatures{
		{},
		{V1Enabled: true},
		{DeviceAuthEnabled: true},
	} {
		srv.Cfg.Client = flags
		if resp := v1Call(t, browser(t), http.MethodGet, ts.URL+"/api/v1/meta", nil, nil); resp.Status != http.StatusOK {
			t.Fatalf("meta status = %d with flags off, want 200", resp.Status)
		}
		for _, request := range requests {
			resp := v1Call(t, browser(t), request.method, ts.URL+request.path, request.body, nil)
			v1AssertError(t, resp, http.StatusNotFound, V1ErrorFeatureDisabled)
		}
	}

	srv.Cfg.Client = config.ClientFeatures{V1Enabled: true, DeviceAuthEnabled: true}
	if resp := v1Call(t, browser(t), http.MethodGet, ts.URL+"/api/v1/meta", nil, nil); resp.Status != http.StatusOK {
		t.Fatalf("meta status = %d with device flags on, want 200", resp.Status)
	}
	if resp := v1StartAuthorization(t, ts.URL, browser(t), body); resp.Status != http.StatusCreated {
		t.Fatalf("start status = %d with both flags on, want 201; body %s", resp.Status, resp.Body)
	}
}

func TestDeviceAuthorizationStartContractAndValidation(t *testing.T) {
	srv, ts := newDeviceAuthServer(t)
	srv.Cfg.Users.BaseURL = "https://auth.example/base/"
	admin, _ := v1SetupAdmin(t, ts.URL)
	verifier, challenge := v1PKCE(t)

	start := v1StartAuthorization(t, ts.URL, browser(t), v1StartRequest(challenge, map[string]any{
		"device_name": "  Work\nstation\t\x01  ",
		"platform":    "WINDOWS",
	}))
	if start.Status != http.StatusCreated {
		t.Fatalf("start status = %d, want 201; body %s", start.Status, start.Body)
	}
	v1AssertKeys(t, start.JSON, []string{
		"authorization_id", "device_code", "user_code", "verification_uri",
		"verification_uri_complete", "expires_in", "interval",
	})
	if got := start.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("start Cache-Control = %q, want no-store", got)
	}
	authorizationID := v1StringField(t, start.JSON, "authorization_id")
	deviceCode := v1StringField(t, start.JSON, "device_code")
	userCode := v1StringField(t, start.JSON, "user_code")
	if !v1AuthorizationIDPattern.MatchString(authorizationID) {
		t.Errorf("authorization_id %q does not match the public-id contract", authorizationID)
	}
	if !strings.HasPrefix(deviceCode, "dc_") || len(deviceCode) < 32 {
		t.Errorf("device_code has an unexpected shape")
	}
	if len(userCode) != 11 || userCode[5] != '-' {
		t.Errorf("user_code = %q, want XXXXX-XXXXX", userCode)
	}
	if got := start.JSON["verification_uri"]; got != "https://auth.example/base/device" {
		t.Errorf("verification_uri = %v", got)
	}
	if got := start.JSON["verification_uri_complete"]; got != "https://auth.example/base/device?code="+userCode {
		t.Errorf("verification_uri_complete = %v", got)
	}
	if got := start.JSON["expires_in"]; got != float64(600) {
		t.Errorf("expires_in = %v, want 600", got)
	}
	if got := start.JSON["interval"]; got != float64(5) {
		t.Errorf("interval = %v, want 5", got)
	}

	status := v1Call(t, browser(t), http.MethodGet,
		ts.URL+"/api/v1/auth/device/authorizations/"+url.PathEscape(authorizationID), nil, nil)
	if status.Status != http.StatusOK {
		t.Fatalf("status endpoint = %d, want 200; body %s", status.Status, status.Body)
	}
	v1AssertKeys(t, status.JSON, []string{"state", "expires_at"})
	if status.JSON["state"] != models.DeviceAuthorizationPending {
		t.Errorf("status state = %v, want pending", status.JSON["state"])
	}
	if _, leaked := status.JSON["user_id"]; leaked {
		t.Error("public status leaked user_id")
	}
	if _, leaked := status.JSON["device_code"]; leaked {
		t.Error("public status leaked device_code")
	}

	normalizedLookupCode := strings.ToLower(strings.ReplaceAll(userCode, "-", ""))
	lookup := v1LookupAuthorization(t, ts.URL, admin, normalizedLookupCode, nil)
	if lookup.Status != http.StatusOK {
		t.Fatalf("lookup = %d, want 200; body %s", lookup.Status, lookup.Body)
	}
	v1AssertKeys(t, lookup.JSON, []string{
		"authorization_id", "device_name", "platform", "client_version", "state", "expires_at", "csrf_token",
	})
	if lookup.JSON["device_name"] != "Workstation" || lookup.JSON["platform"] != "windows" {
		t.Errorf("lookup device metadata = %v", lookup.JSON)
	}
	for _, forbidden := range []string{"device_code", "user_code", "user_id", "access_token", "refresh_token"} {
		if _, present := lookup.JSON[forbidden]; present {
			t.Errorf("lookup leaked %s", forbidden)
		}
	}

	invalidBodies := []map[string]any{
		v1StartRequest(challenge, map[string]any{"platform": "linux"}),
		v1StartRequest(challenge, map[string]any{"client_version": strings.Repeat("a", 65)}),
		v1StartRequest(strings.Repeat("x", 43), map[string]any{"code_challenge_method": "plain"}),
		v1StartRequest("short", nil),
		v1StartRequest(challenge, map[string]any{"device_name": " \n\t\x01 "}),
		v1StartRequest(challenge, map[string]any{"unexpected": true}),
	}
	for i, invalid := range invalidBodies {
		resp := v1StartAuthorization(t, ts.URL, browser(t), invalid)
		if resp.Status != http.StatusBadRequest || v1ErrorCode(t, resp) != string(V1ErrorInvalidRequest) {
			t.Errorf("invalid start %d = %d %s, want 400 invalid_request", i, resp.Status, resp.Body)
		}
	}

	oversized := []byte(`{"device_name":"` + strings.Repeat("a", v1JSONBodyLimit+1) + `","platform":"windows","client_version":"0.1.0","code_challenge":"` + challenge + `","code_challenge_method":"S256"}`)
	tooLarge := v1Call(t, browser(t), http.MethodPost, ts.URL+"/api/v1/auth/device/authorizations", oversized, nil)
	v1AssertError(t, tooLarge, http.StatusBadRequest, V1ErrorInvalidRequest)

	// The verifier belongs only to the client; the start response must never
	// reflect it.
	if strings.Contains(string(start.Body), verifier) {
		t.Error("start response reflected the PKCE verifier")
	}

	notFound := v1Call(t, browser(t), http.MethodGet,
		ts.URL+"/api/v1/auth/device/authorizations/dva_"+strings.Repeat("A", 24), nil, nil)
	v1AssertError(t, notFound, http.StatusNotFound, V1ErrorAuthorizationNotFound)
}

func TestDeviceAuthorizationVerificationURIFallsBackToRequestHost(t *testing.T) {
	_, ts := newDeviceAuthServer(t)
	_, challenge := v1PKCE(t)
	start := v1StartAuthorization(t, ts.URL, browser(t), v1StartRequest(challenge, nil))
	if start.Status != http.StatusCreated {
		t.Fatalf("start = %d; body %s", start.Status, start.Body)
	}
	if got := start.JSON["verification_uri"]; got != ts.URL+"/device" {
		t.Errorf("verification_uri = %v, want %s/device", got, ts.URL)
	}
	if got := start.JSON["verification_uri_complete"]; got != ts.URL+"/device?code="+v1StringField(t, start.JSON, "user_code") {
		t.Errorf("verification_uri_complete = %v, want a fixed /device query URL", got)
	}
}

func TestDeviceAuthorizationPendingDeniedExpiredConsumed(t *testing.T) {
	t.Run("pending", func(t *testing.T) {
		_, ts := newDeviceAuthServer(t)
		verifier, challenge := v1PKCE(t)
		start := v1StartAuthorization(t, ts.URL, browser(t), v1StartRequest(challenge, nil))
		if start.Status != http.StatusCreated {
			t.Fatalf("start = %d; body %s", start.Status, start.Body)
		}
		pending := v1TokenExchange(t, ts.URL, browser(t),
			v1StringField(t, start.JSON, "device_code"), verifier)
		if pending.Status != http.StatusAccepted {
			t.Fatalf("pending token exchange = %d, want 202; body %s", pending.Status, pending.Body)
		}
		v1AssertKeys(t, pending.JSON, []string{"state", "retry_after_seconds", "expires_at"})
		if pending.JSON["state"] != models.DeviceAuthorizationPending || pending.JSON["retry_after_seconds"] != float64(5) {
			t.Errorf("pending body = %v", pending.JSON)
		}
		if _, err := time.Parse(time.RFC3339Nano, v1StringField(t, pending.JSON, "expires_at")); err != nil {
			t.Errorf("pending expires_at is not RFC3339: %v", err)
		}
	})

	t.Run("denied", func(t *testing.T) {
		_, ts := newDeviceAuthServer(t)
		admin, _ := v1SetupAdmin(t, ts.URL)
		verifier, challenge := v1PKCE(t)
		start := v1StartAuthorization(t, ts.URL, browser(t), v1StartRequest(challenge, nil))
		csrfToken := v1LookupCSRF(t, ts.URL, admin, v1StringField(t, start.JSON, "user_code"))
		decision := v1Decision(t, ts.URL, admin, v1StringField(t, start.JSON, "authorization_id"), "deny", map[string]string{
			"X-CSRF-Token": csrfToken,
		})
		if decision.Status != http.StatusOK || decision.JSON["state"] != models.DeviceAuthorizationDenied {
			t.Fatalf("deny = %d %v", decision.Status, decision.JSON)
		}
		resp := v1TokenExchange(t, ts.URL, browser(t), v1StringField(t, start.JSON, "device_code"), verifier)
		v1AssertError(t, resp, http.StatusForbidden, V1ErrorAuthorizationDenied)
	})

	t.Run("expired", func(t *testing.T) {
		srv, ts := newDeviceAuthServer(t)
		verifier, challenge := v1PKCE(t)
		grant, err := srv.Store.CreateDeviceAuthorization(models.DeviceAuthorizationRequest{
			DeviceName: "Expired device", Platform: "windows", ClientVersion: "0.1.0",
			CodeChallenge: challenge, CodeChallengeMethod: auth.PKCEMethodS256,
		}, time.Now().Add(-2*auth.DeviceAuthorizationTTL))
		if err != nil {
			t.Fatalf("create expired authorization: %v", err)
		}
		resp := v1TokenExchange(t, ts.URL, browser(t), grant.DeviceCode, verifier)
		v1AssertError(t, resp, http.StatusGone, V1ErrorAuthorizationExpired)
		status := v1Call(t, browser(t), http.MethodGet,
			ts.URL+"/api/v1/auth/device/authorizations/"+grant.Authorization.AuthorizationID, nil, nil)
		if status.Status != http.StatusOK || status.JSON["state"] != models.DeviceAuthorizationExpired {
			t.Fatalf("expired status = %d %v, want 200 expired", status.Status, status.JSON)
		}
	})

	t.Run("consumed", func(t *testing.T) {
		_, ts := newDeviceAuthServer(t)
		admin, _ := v1SetupAdmin(t, ts.URL)
		verifier, challenge := v1PKCE(t)
		start := v1StartAuthorization(t, ts.URL, browser(t), v1StartRequest(challenge, nil))
		if start.Status != http.StatusCreated {
			t.Fatalf("start = %d; body %s", start.Status, start.Body)
		}
		authorizationID := v1StringField(t, start.JSON, "authorization_id")
		deviceCode := v1StringField(t, start.JSON, "device_code")
		csrfToken := v1LookupCSRF(t, ts.URL, admin, v1StringField(t, start.JSON, "user_code"))
		decision := v1Decision(t, ts.URL, admin, authorizationID, "approve", map[string]string{
			"X-CSRF-Token": csrfToken,
		})
		if decision.Status != http.StatusOK {
			t.Fatalf("approve = %d; body %s", decision.Status, decision.Body)
		}
		first := v1TokenExchange(t, ts.URL, browser(t), deviceCode, verifier)
		if first.Status != http.StatusOK {
			t.Fatalf("first exchange = %d; body %s", first.Status, first.Body)
		}

		resp := v1TokenExchange(t, ts.URL, browser(t), deviceCode, verifier)
		v1AssertError(t, resp, http.StatusConflict, V1ErrorAuthorizationConsumed)
		status := v1Call(t, browser(t), http.MethodGet,
			ts.URL+"/api/v1/auth/device/authorizations/"+authorizationID, nil, nil)
		if status.Status != http.StatusOK || status.JSON["state"] != models.DeviceAuthorizationConsumed {
			t.Fatalf("consumed status = %d %v", status.Status, status.JSON)
		}
	})
}

func TestDeviceTokenRefreshRotationAndCredentialIsolation(t *testing.T) {
	_, ts := newDeviceAuthServer(t)
	admin, webToken := v1SetupAdmin(t, ts.URL)
	start, pair := v1IssueDevice(t, ts.URL, admin, "Primary desktop")

	v1AssertKeys(t, pair, []string{
		"access_token", "token_type", "expires_in", "refresh_token",
		"refresh_expires_in", "device", "user",
	})
	accessToken := v1StringField(t, pair, "access_token")
	refreshToken := v1StringField(t, pair, "refresh_token")
	if !strings.HasPrefix(accessToken, "at_") || !strings.HasPrefix(refreshToken, "rt_") {
		t.Error("token pair returned credentials with unexpected prefixes")
	}
	if pair["token_type"] != "Bearer" || pair["expires_in"] != float64(900) || pair["refresh_expires_in"] != float64(2592000) {
		t.Errorf("token pair lifetimes = %v", pair)
	}
	user := v1ObjectField(t, pair, "user")
	v1AssertKeys(t, user, []string{"id", "name"})
	if _, leaked := user["pass_hash"]; leaked {
		t.Error("token response leaked pass_hash")
	}
	if _, leaked := user["salt"]; leaked {
		t.Error("token response leaked salt")
	}
	device := v1ObjectField(t, pair, "device")
	v1AssertKeys(t, device, []string{
		"id", "name", "platform", "client_version", "created_at", "last_seen_at", "revoked_at",
	})

	authHeader := map[string]string{"Authorization": "Bearer " + accessToken}
	list := v1Call(t, browser(t), http.MethodGet, ts.URL+"/api/v1/devices", nil, authHeader)
	if list.Status != http.StatusOK {
		t.Fatalf("device list with access token = %d; body %s", list.Status, list.Body)
	}

	// Device bearer tokens are not Web sessions.
	webRoute := v1Call(t, browser(t), http.MethodGet, ts.URL+"/api/auth/me", nil, authHeader)
	if webRoute.Status != http.StatusUnauthorized {
		t.Fatalf("device token on Web session route = %d, want 401", webRoute.Status)
	}
	lookup := v1LookupAuthorization(t, ts.URL, browser(t), v1StringField(t, start, "user_code"), authHeader)
	v1AssertError(t, lookup, http.StatusUnauthorized, V1ErrorUnauthenticated)
	decision := v1Decision(t, ts.URL, browser(t), v1StringField(t, start, "authorization_id"), "approve", authHeader)
	v1AssertError(t, decision, http.StatusUnauthorized, V1ErrorUnauthenticated)

	// A Web cookie is not a device bearer token.
	cookieOnly := v1Call(t, admin, http.MethodGet, ts.URL+"/api/v1/devices", nil, nil)
	v1AssertError(t, cookieOnly, http.StatusUnauthorized, V1ErrorUnauthenticated)

	// The existing Web Bearer session remains valid for browser-session routes.
	pendingVerifier, pendingChallenge := v1PKCE(t)
	pendingStart := v1StartAuthorization(t, ts.URL, browser(t), v1StartRequest(pendingChallenge, nil))
	if pendingStart.Status != http.StatusCreated {
		t.Fatalf("pending start = %d; body %s", pendingStart.Status, pendingStart.Body)
	}
	webBearerLookup := v1LookupAuthorization(t, ts.URL, browser(t),
		v1StringField(t, pendingStart.JSON, "user_code"),
		map[string]string{"Authorization": "Bearer " + webToken})
	if webBearerLookup.Status != http.StatusOK {
		t.Fatalf("Web Bearer lookup = %d; body %s", webBearerLookup.Status, webBearerLookup.Body)
	}
	_ = pendingVerifier

	rotated := v1Refresh(t, ts.URL, browser(t), refreshToken)
	if rotated.Status != http.StatusOK {
		t.Fatalf("refresh = %d; body %s", rotated.Status, rotated.Body)
	}
	rotatedAccess := v1StringField(t, rotated.JSON, "access_token")
	rotatedRefresh := v1StringField(t, rotated.JSON, "refresh_token")
	if rotatedAccess == accessToken || rotatedRefresh == refreshToken {
		t.Error("refresh reused a credential")
	}
	oldAccess := v1Call(t, browser(t), http.MethodGet, ts.URL+"/api/v1/devices", nil, authHeader)
	v1AssertError(t, oldAccess, http.StatusUnauthorized, V1ErrorUnauthenticated)

	replay := v1Refresh(t, ts.URL, browser(t), refreshToken)
	v1AssertError(t, replay, http.StatusConflict, V1ErrorRefreshReuseDetected)
	afterReplay := v1Refresh(t, ts.URL, browser(t), rotatedRefresh)
	v1AssertError(t, afterReplay, http.StatusUnauthorized, V1ErrorRefreshTokenInvalid)
	revokedAccess := v1Call(t, browser(t), http.MethodGet, ts.URL+"/api/v1/devices", nil,
		map[string]string{"Authorization": "Bearer " + rotatedAccess})
	v1AssertError(t, revokedAccess, http.StatusUnauthorized, V1ErrorDeviceRevoked)

	// Logout revokes the current device and its whole token family.
	_, logoutPair := v1IssueDevice(t, ts.URL, admin, "Logout device")
	logoutAccess := v1StringField(t, logoutPair, "access_token")
	logout := v1Call(t, browser(t), http.MethodPost, ts.URL+"/api/v1/auth/logout", nil,
		map[string]string{"Authorization": "Bearer " + logoutAccess})
	if logout.Status != http.StatusNoContent || len(logout.Body) != 0 {
		t.Fatalf("logout = %d with %d body bytes, want 204 empty", logout.Status, len(logout.Body))
	}
	afterLogout := v1Call(t, browser(t), http.MethodGet, ts.URL+"/api/v1/devices", nil,
		map[string]string{"Authorization": "Bearer " + logoutAccess})
	v1AssertError(t, afterLogout, http.StatusUnauthorized, V1ErrorDeviceRevoked)
	logoutRefresh := v1Refresh(t, ts.URL, browser(t), v1StringField(t, logoutPair, "refresh_token"))
	v1AssertError(t, logoutRefresh, http.StatusUnauthorized, V1ErrorRefreshTokenInvalid)
}

func TestDeviceAuthorizationPKCEFailureLeavesAuthorizedGrantUsable(t *testing.T) {
	_, ts := newDeviceAuthServer(t)
	admin, _ := v1SetupAdmin(t, ts.URL)
	verifier, challenge := v1PKCE(t)
	start := v1StartAuthorization(t, ts.URL, browser(t), v1StartRequest(challenge, nil))
	if start.Status != http.StatusCreated {
		t.Fatalf("start = %d; body %s", start.Status, start.Body)
	}
	csrfToken := v1LookupCSRF(t, ts.URL, admin, v1StringField(t, start.JSON, "user_code"))
	decision := v1Decision(t, ts.URL, admin, v1StringField(t, start.JSON, "authorization_id"), "approve", map[string]string{
		"X-CSRF-Token": csrfToken,
	})
	if decision.Status != http.StatusOK {
		t.Fatalf("approve = %d; body %s", decision.Status, decision.Body)
	}
	wrongVerifier, _ := v1PKCE(t)
	bad := v1TokenExchange(t, ts.URL, browser(t), v1StringField(t, start.JSON, "device_code"), wrongVerifier)
	v1AssertError(t, bad, http.StatusBadRequest, V1ErrorInvalidGrant)
	good := v1TokenExchange(t, ts.URL, browser(t), v1StringField(t, start.JSON, "device_code"), verifier)
	if good.Status != http.StatusOK {
		t.Fatalf("correct verifier after wrong PKCE = %d; body %s", good.Status, good.Body)
	}
}

func TestDeviceAuthHonorsEmailVerificationGate(t *testing.T) {
	srv, ts := newDeviceAuthServer(t)
	admin, _ := v1SetupAdmin(t, ts.URL)
	aliceID := v1CreateViewer(t, ts.URL, admin, "alice", "alice-pass")
	alice, _ := v1Login(t, ts.URL, "alice", "alice-pass")
	_, pair := v1IssueDevice(t, ts.URL, alice, "Email gate device")

	// Leave one authorization approved but unexchanged so the DB issuance
	// transaction can be checked after the account becomes email-pending.
	verifier, challenge := v1PKCE(t)
	pendingExchange := v1StartAuthorization(t, ts.URL, browser(t), v1StartRequest(challenge, nil))
	if pendingExchange.Status != http.StatusCreated {
		t.Fatalf("pending exchange start = %d; body %s", pendingExchange.Status, pendingExchange.Body)
	}
	csrfToken := v1LookupCSRF(t, ts.URL, alice, v1StringField(t, pendingExchange.JSON, "user_code"))
	approved := v1Decision(t, ts.URL, alice, v1StringField(t, pendingExchange.JSON, "authorization_id"), "approve", map[string]string{
		"X-CSRF-Token": csrfToken,
	})
	if approved.Status != http.StatusOK {
		t.Fatalf("pre-verification approval = %d; body %s", approved.Status, approved.Body)
	}

	var devicesBefore int
	if err := srv.Store.DB.QueryRow(`SELECT COUNT(*) FROM devices`).Scan(&devicesBefore); err != nil {
		t.Fatalf("count devices before gate: %v", err)
	}
	if err := srv.Store.SetUserEmail(aliceID, "alice@example.com", false); err != nil {
		t.Fatalf("set pending email: %v", err)
	}
	srv.Cfg.Users.RequireEmail = true
	srv.Cfg.SMTP.Host = "smtp.example.com"

	me := v1Call(t, alice, http.MethodGet, ts.URL+"/api/auth/me", nil, nil)
	if me.Status != http.StatusUnauthorized {
		t.Fatalf("Web session for email-pending user = %d, want 401; body %s", me.Status, me.Body)
	}

	access := v1StringField(t, pair, "access_token")
	refresh := v1StringField(t, pair, "refresh_token")
	deviceList := v1Call(t, browser(t), http.MethodGet, ts.URL+"/api/v1/devices", nil,
		map[string]string{"Authorization": "Bearer " + access})
	v1AssertError(t, deviceList, http.StatusForbidden, V1ErrorForbidden)

	exchange := v1TokenExchange(t, ts.URL, browser(t),
		v1StringField(t, pendingExchange.JSON, "device_code"), verifier)
	v1AssertError(t, exchange, http.StatusForbidden, V1ErrorForbidden)
	var devicesAfter int
	if err := srv.Store.DB.QueryRow(`SELECT COUNT(*) FROM devices`).Scan(&devicesAfter); err != nil {
		t.Fatalf("count devices after gate: %v", err)
	}
	if devicesAfter != devicesBefore {
		t.Errorf("email-gated exchange created %d devices", devicesAfter-devicesBefore)
	}

	blockedLookup := v1LookupAuthorization(t, ts.URL, alice, "ABCD-EFGH", nil)
	v1AssertError(t, blockedLookup, http.StatusUnauthorized, V1ErrorUnauthenticated)
	blockedDecision := v1Decision(t, ts.URL, alice,
		v1StringField(t, pendingExchange.JSON, "authorization_id"), "deny", nil)
	v1AssertError(t, blockedDecision, http.StatusUnauthorized, V1ErrorUnauthenticated)

	rotated := v1Refresh(t, ts.URL, browser(t), refresh)
	v1AssertError(t, rotated, http.StatusUnauthorized, V1ErrorRefreshTokenInvalid)
}

func TestDeviceAuthorizationWebSessionAndCSRF(t *testing.T) {
	srv, ts := newDeviceAuthServer(t)
	_, challenge := v1PKCE(t)
	start := v1StartAuthorization(t, ts.URL, browser(t), v1StartRequest(challenge, nil))
	if start.Status != http.StatusCreated {
		t.Fatalf("start = %d; body %s", start.Status, start.Body)
	}
	authorizationID := v1StringField(t, start.JSON, "authorization_id")
	userCode := v1StringField(t, start.JSON, "user_code")

	unauthenticatedLookup := v1LookupAuthorization(t, ts.URL, browser(t), userCode, nil)
	v1AssertError(t, unauthenticatedLookup, http.StatusUnauthorized, V1ErrorUnauthenticated)
	unauthenticatedDecision := v1Decision(t, ts.URL, browser(t), authorizationID, "approve", nil)
	v1AssertError(t, unauthenticatedDecision, http.StatusUnauthorized, V1ErrorUnauthenticated)

	admin, _ := v1SetupAdmin(t, ts.URL)
	lookup := v1LookupAuthorization(t, ts.URL, admin, userCode, nil)
	if lookup.Status != http.StatusOK {
		t.Fatalf("authenticated lookup = %d; body %s", lookup.Status, lookup.Body)
	}
	csrfToken := v1StringField(t, lookup.JSON, "csrf_token")
	if got := lookup.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("lookup Cache-Control = %q, want no-store", got)
	}
	var storedHash string
	var consumed int
	if err := srv.Store.DB.QueryRow(`SELECT csrf_token_hash,
		CASE WHEN csrf_consumed_at IS NULL THEN 0 ELSE 1 END
		FROM device_authorizations WHERE authorization_id = ?`, authorizationID).
		Scan(&storedHash, &consumed); err != nil {
		t.Fatalf("read approval CSRF storage: %v", err)
	}
	if storedHash == csrfToken || storedHash != auth.HashDeviceCredential(csrfToken) {
		t.Error("approval CSRF was not stored as its hash")
	}
	if consumed != 0 {
		t.Error("new approval CSRF was already consumed")
	}

	missingCSRF := v1Decision(t, ts.URL, admin, authorizationID, "approve", nil)
	v1AssertError(t, missingCSRF, http.StatusForbidden, V1ErrorForbidden)

	missingOrigin := v1Decision(t, ts.URL, admin, authorizationID, "approve", map[string]string{
		"Origin":       "",
		"X-CSRF-Token": csrfToken,
	})
	v1AssertError(t, missingOrigin, http.StatusForbidden, V1ErrorForbidden)

	wrongSite := v1Decision(t, ts.URL, admin, authorizationID, "approve", map[string]string{
		"Sec-Fetch-Site": "cross-site",
		"X-CSRF-Token":   csrfToken,
	})
	v1AssertError(t, wrongSite, http.StatusForbidden, V1ErrorForbidden)

	wrongCSRF := v1Decision(t, ts.URL, admin, authorizationID, "approve", map[string]string{
		"X-CSRF-Token": "csrf_" + strings.Repeat("A", 43),
	})
	v1AssertError(t, wrongCSRF, http.StatusForbidden, V1ErrorForbidden)

	wrongType := v1Call(t, admin, http.MethodPost,
		ts.URL+"/api/v1/auth/device/authorizations/"+authorizationID+"/decision",
		[]byte(`{"decision":"approve"}`),
		map[string]string{
			"Content-Type": "text/plain", "Origin": ts.URL,
			"Sec-Fetch-Site": "same-origin", "X-CSRF-Token": csrfToken,
		})
	v1AssertError(t, wrongType, http.StatusBadRequest, V1ErrorInvalidRequest)

	wrongOrigin := v1Decision(t, ts.URL, admin, authorizationID, "approve", map[string]string{
		"Origin":       "https://evil.example",
		"X-CSRF-Token": csrfToken,
	})
	v1AssertError(t, wrongOrigin, http.StatusForbidden, V1ErrorForbidden)

	wrongScheme := v1Decision(t, ts.URL, admin, authorizationID, "approve", map[string]string{
		"Origin":       strings.Replace(ts.URL, "http://", "https://", 1),
		"X-CSRF-Token": csrfToken,
	})
	v1AssertError(t, wrongScheme, http.StatusForbidden, V1ErrorForbidden)

	sameOrigin := v1Decision(t, ts.URL, admin, authorizationID, "approve", map[string]string{
		"X-CSRF-Token": csrfToken,
	})
	if sameOrigin.Status != http.StatusOK || sameOrigin.JSON["state"] != models.DeviceAuthorizationAuthorized {
		t.Fatalf("same-origin approve = %d %v", sameOrigin.Status, sameOrigin.JSON)
	}
	if err := srv.Store.DB.QueryRow(`SELECT
		CASE WHEN csrf_consumed_at IS NULL THEN 0 ELSE 1 END
		FROM device_authorizations WHERE authorization_id = ?`, authorizationID).Scan(&consumed); err != nil {
		t.Fatalf("read consumed approval CSRF: %v", err)
	}
	if consumed != 1 {
		t.Error("approval CSRF was not consumed by the decision")
	}

	secondDecision := v1Decision(t, ts.URL, admin, authorizationID, "approve", map[string]string{
		"X-CSRF-Token": csrfToken,
	})
	if secondDecision.Status != http.StatusConflict {
		t.Fatalf("second decision = %d, want 409; body %s", secondDecision.Status, secondDecision.Body)
	}
}

func TestDeviceManagementAndCrossUserIsolation(t *testing.T) {
	_, ts := newDeviceAuthServer(t)
	admin, _ := v1SetupAdmin(t, ts.URL)
	aliceID := v1CreateViewer(t, ts.URL, admin, "alice", "alice-pass")
	v1CreateViewer(t, ts.URL, admin, "bob", "bob-pass")
	alice, _ := v1Login(t, ts.URL, "alice", "alice-pass")
	bob, _ := v1Login(t, ts.URL, "bob", "bob-pass")

	_, aliceA := v1IssueDevice(t, ts.URL, alice, "Alice desktop")
	_, aliceB := v1IssueDevice(t, ts.URL, alice, "Alice laptop")
	_, bobA := v1IssueDevice(t, ts.URL, bob, "Bob desktop")
	aliceAToken := v1StringField(t, aliceA, "access_token")
	aliceADevice := v1StringField(t, v1ObjectField(t, aliceA, "device"), "id")
	aliceBDevice := v1StringField(t, v1ObjectField(t, aliceB, "device"), "id")
	bobADevice := v1StringField(t, v1ObjectField(t, bobA, "device"), "id")
	aliceAuth := map[string]string{"Authorization": "Bearer " + aliceAToken}

	list := v1Call(t, browser(t), http.MethodGet, ts.URL+"/api/v1/devices", nil, aliceAuth)
	if list.Status != http.StatusOK {
		t.Fatalf("list = %d; body %s", list.Status, list.Body)
	}
	v1AssertKeys(t, list.JSON, []string{"devices"})
	devices, _ := list.JSON["devices"].([]any)
	if len(devices) != 2 {
		t.Fatalf("alice device count = %d, want 2", len(devices))
	}
	for _, raw := range devices {
		device, _ := raw.(map[string]any)
		v1AssertKeys(t, device, []string{
			"id", "name", "platform", "client_version", "created_at", "last_seen_at", "revoked_at",
		})
		if _, leaked := device["user_id"]; leaked {
			t.Error("device list leaked user_id")
		}
		if device["id"] == bobADevice {
			t.Error("alice's device list included bob's device")
		}
	}

	crossUser := v1Call(t, browser(t), http.MethodDelete, ts.URL+"/api/v1/devices/"+bobADevice, nil, aliceAuth)
	v1AssertError(t, crossUser, http.StatusNotFound, V1ErrorDeviceNotFound)

	revoked := v1Call(t, browser(t), http.MethodDelete, ts.URL+"/api/v1/devices/"+aliceBDevice, nil, aliceAuth)
	if revoked.Status != http.StatusOK || revoked.JSON["state"] != "revoked" {
		t.Fatalf("delete = %d %v", revoked.Status, revoked.JSON)
	}
	firstRevokedAt := v1StringField(t, revoked.JSON, "revoked_at")
	repeated := v1Call(t, browser(t), http.MethodDelete, ts.URL+"/api/v1/devices/"+aliceBDevice, nil, aliceAuth)
	if repeated.Status != http.StatusOK {
		t.Fatalf("repeated delete = %d; body %s", repeated.Status, repeated.Body)
	}
	if got := v1StringField(t, repeated.JSON, "revoked_at"); got != firstRevokedAt {
		t.Errorf("repeated delete changed revoked_at from %s to %s", firstRevokedAt, got)
	}

	list = v1Call(t, browser(t), http.MethodGet, ts.URL+"/api/v1/devices", nil, aliceAuth)
	if list.Status != http.StatusOK {
		t.Fatalf("post-delete list = %d; body %s", list.Status, list.Body)
	}
	devices, _ = list.JSON["devices"].([]any)
	foundRevoked := false
	for _, raw := range devices {
		device, _ := raw.(map[string]any)
		if device["id"] == aliceBDevice && device["revoked_at"] != nil {
			foundRevoked = true
		}
	}
	if !foundRevoked {
		t.Error("device list did not report the revoked_at timestamp")
	}

	bobDeleteAlice := v1Call(t, browser(t), http.MethodDelete, ts.URL+"/api/v1/devices/"+aliceADevice, nil,
		map[string]string{"Authorization": "Bearer " + v1StringField(t, bobA, "access_token")})
	v1AssertError(t, bobDeleteAlice, http.StatusNotFound, V1ErrorDeviceNotFound)

	_ = aliceID
}

func TestAccountSecurityChangesRevokeDeviceSessions(t *testing.T) {
	t.Run("self password change", func(t *testing.T) {
		srv, ts := newDeviceAuthServer(t)
		admin, _ := v1SetupAdmin(t, ts.URL)
		v1CreateViewer(t, ts.URL, admin, "alice", "alice-pass")
		alice, _ := v1Login(t, ts.URL, "alice", "alice-pass")
		_, pair := v1IssueDevice(t, ts.URL, alice, "Alice desktop")
		access := v1StringField(t, pair, "access_token")

		change := v1Call(t, alice, http.MethodPost, ts.URL+"/api/auth/password", map[string]string{
			"current_password": "alice-pass", "new_password": "alice-pass-2",
		}, nil)
		if change.Status != http.StatusOK {
			t.Fatalf("password change = %d; body %s", change.Status, change.Body)
		}
		if me := v1Call(t, alice, http.MethodGet, ts.URL+"/api/auth/me", nil, nil); me.Status != http.StatusOK {
			t.Fatalf("Web session after password change = %d, want 200", me.Status)
		}
		devices := v1Call(t, browser(t), http.MethodGet, ts.URL+"/api/v1/devices", nil,
			map[string]string{"Authorization": "Bearer " + access})
		v1AssertError(t, devices, http.StatusUnauthorized, V1ErrorDeviceRevoked)

		// The store helper is still present in the test to make the intended
		// account-wide operation explicit.
		if n, err := srv.Store.RevokeAllDevicesForUser(1, time.Now()); err != nil || n != 0 {
			t.Fatalf("idempotent account-wide revoke = %d, %v", n, err)
		}
	})

	t.Run("admin password disable and revoke all", func(t *testing.T) {
		_, ts := newDeviceAuthServer(t)
		admin, _ := v1SetupAdmin(t, ts.URL)
		aliceID := v1CreateViewer(t, ts.URL, admin, "alice", "alice-pass")
		alice, _ := v1Login(t, ts.URL, "alice", "alice-pass")
		_, adminResetPair := v1IssueDevice(t, ts.URL, alice, "Before admin reset")

		reset := v1Call(t, admin, http.MethodPost,
			ts.URL+"/api/admin/users/"+strconv.FormatInt(aliceID, 10)+"/password",
			map[string]string{"password": "alice-pass-2"}, nil)
		if reset.Status != http.StatusOK {
			t.Fatalf("admin password reset = %d; body %s", reset.Status, reset.Body)
		}
		oldDevice := v1Call(t, browser(t), http.MethodGet, ts.URL+"/api/v1/devices", nil,
			map[string]string{"Authorization": "Bearer " + v1StringField(t, adminResetPair, "access_token")})
		v1AssertError(t, oldDevice, http.StatusUnauthorized, V1ErrorDeviceRevoked)

		alice, _ = v1Login(t, ts.URL, "alice", "alice-pass-2")
		_, disablePair := v1IssueDevice(t, ts.URL, alice, "Before disable")
		disable := v1Call(t, admin, http.MethodPut,
			ts.URL+"/api/admin/users/"+strconv.FormatInt(aliceID, 10),
			map[string]string{"status": models.StatusDisabled}, nil)
		if disable.Status != http.StatusOK {
			t.Fatalf("disable = %d; body %s", disable.Status, disable.Body)
		}
		disabledDevice := v1Call(t, browser(t), http.MethodGet, ts.URL+"/api/v1/devices", nil,
			map[string]string{"Authorization": "Bearer " + v1StringField(t, disablePair, "access_token")})
		v1AssertError(t, disabledDevice, http.StatusForbidden, V1ErrorForbidden)
		disabledRefresh := v1Refresh(t, ts.URL, browser(t), v1StringField(t, disablePair, "refresh_token"))
		v1AssertError(t, disabledRefresh, http.StatusUnauthorized, V1ErrorRefreshTokenInvalid)

		if enable := v1Call(t, admin, http.MethodPut,
			ts.URL+"/api/admin/users/"+strconv.FormatInt(aliceID, 10),
			map[string]string{"status": models.StatusActive}, nil); enable.Status != http.StatusOK {
			t.Fatalf("re-enable = %d; body %s", enable.Status, enable.Body)
		}
		alice, _ = v1Login(t, ts.URL, "alice", "alice-pass-2")
		_, revokePair := v1IssueDevice(t, ts.URL, alice, "Before revoke-all")
		revoke := v1Call(t, admin, http.MethodPost,
			ts.URL+"/api/admin/users/"+strconv.FormatInt(aliceID, 10)+"/revoke", nil, nil)
		if revoke.Status != http.StatusOK {
			t.Fatalf("revoke-all = %d; body %s", revoke.Status, revoke.Body)
		}
		revokedDevice := v1Call(t, browser(t), http.MethodGet, ts.URL+"/api/v1/devices", nil,
			map[string]string{"Authorization": "Bearer " + v1StringField(t, revokePair, "access_token")})
		v1AssertError(t, revokedDevice, http.StatusUnauthorized, V1ErrorDeviceRevoked)
	})

	t.Run("mail password reset", func(t *testing.T) {
		srv, ts := newDeviceAuthServer(t)
		admin, _ := v1SetupAdmin(t, ts.URL)
		aliceID := v1CreateViewer(t, ts.URL, admin, "alice", "alice-pass")
		alice, _ := v1Login(t, ts.URL, "alice", "alice-pass")
		_, pair := v1IssueDevice(t, ts.URL, alice, "Before mail reset")

		if err := srv.Store.SetUserEmail(aliceID, "alice@example.com", true); err != nil {
			t.Fatalf("set reset email: %v", err)
		}
		resetToken := auth.NewToken()
		if err := srv.Store.CreateAuthToken(resetToken, aliceID, db.TokenResetPassword, time.Now().Add(time.Hour)); err != nil {
			t.Fatalf("create reset token: %v", err)
		}
		reset := v1Call(t, browser(t), http.MethodPost, ts.URL+"/api/auth/reset-password", map[string]string{
			"token": resetToken, "password": "alice-pass-2",
		}, nil)
		if reset.Status != http.StatusOK {
			t.Fatalf("mail reset = %d; body %s", reset.Status, reset.Body)
		}
		devices := v1Call(t, browser(t), http.MethodGet, ts.URL+"/api/v1/devices", nil,
			map[string]string{"Authorization": "Bearer " + v1StringField(t, pair, "access_token")})
		v1AssertError(t, devices, http.StatusUnauthorized, V1ErrorDeviceRevoked)
	})
}

func TestDeviceAuthorizationStartRateLimit(t *testing.T) {
	_, ts := newDeviceAuthServer(t)
	_, challenge := v1PKCE(t)
	client := browser(t)
	for i := 0; i < 20; i++ {
		resp := v1StartAuthorization(t, ts.URL, client, v1StartRequest(challenge, map[string]any{
			"device_name": "Rate limit device",
		}))
		if resp.Status != http.StatusCreated {
			t.Fatalf("request %d status = %d, want 201; body %s", i+1, resp.Status, resp.Body)
		}
	}
	limited := v1StartAuthorization(t, ts.URL, client, v1StartRequest(challenge, nil))
	v1AssertError(t, limited, http.StatusTooManyRequests, V1ErrorRateLimited)
	if limited.Header.Get("Retry-After") == "" {
		t.Error("rate-limited response omitted Retry-After")
	}
	details := v1ObjectField(t, limited.JSON["error"].(map[string]any), "details")
	if details["retry_after_seconds"] == nil {
		t.Error("rate-limited response omitted retry_after_seconds")
	}
}
