package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sort"
	"testing"

	"doriyamedia/internal/config"
)

func TestV1MetaPublicContract(t *testing.T) {
	s := &Server{Version: "v1.2.3"}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/meta", nil)
	rec := httptest.NewRecorder()

	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want application/json; charset=utf-8", got)
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &top); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	wantTopKeys := []string{
		"contract_version",
		"features",
		"minimum_client_versions",
		"server_version",
	}
	if got := sortedKeys(top); !reflect.DeepEqual(got, wantTopKeys) {
		t.Errorf("top-level fields = %v, want %v", got, wantTopKeys)
	}

	var contractVersion int
	if err := json.Unmarshal(top["contract_version"], &contractVersion); err != nil {
		t.Fatalf("contract_version is not an integer: %v", err)
	}
	if contractVersion != 1 {
		t.Errorf("contract_version = %d, want 1", contractVersion)
	}

	var serverVersion string
	if err := json.Unmarshal(top["server_version"], &serverVersion); err != nil {
		t.Fatalf("decode server_version: %v", err)
	}
	if serverVersion != "v1.2.3" {
		t.Errorf("server_version = %q, want v1.2.3", serverVersion)
	}

	var minimum map[string]string
	if err := json.Unmarshal(top["minimum_client_versions"], &minimum); err != nil {
		t.Fatalf("decode minimum_client_versions: %v", err)
	}
	wantMinimum := map[string]string{"android": "0.1.0", "windows": "0.1.0"}
	if !reflect.DeepEqual(minimum, wantMinimum) {
		t.Errorf("minimum_client_versions = %v, want %v", minimum, wantMinimum)
	}

	var features map[string]bool
	if err := json.Unmarshal(top["features"], &features); err != nil {
		t.Fatalf("decode features: %v", err)
	}
	wantFeatures := map[string]bool{
		"client_v1_enabled":       false,
		"device_auth_enabled":     false,
		"playback_ticket_enabled": false,
		"direct_http_enabled":     false,
		"server_proxy_v1_enabled": false,
		"server_hls_v1_enabled":   false,
		"direct_dash_enabled":     false,
		"local_relay_enabled":     false,
		"native_player_enabled":   false,
		"libmpv_fallback_enabled": false,
	}
	if !reflect.DeepEqual(features, wantFeatures) {
		t.Errorf("features = %v, want %v", features, wantFeatures)
	}
}

func TestV1MetaVersionFallback(t *testing.T) {
	rec := httptest.NewRecorder()
	(&Server{}).handleV1Meta(rec, httptest.NewRequest(http.MethodGet, "/api/v1/meta", nil))

	var got v1MetaResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.ServerVersion != "dev" {
		t.Errorf("server_version = %q, want dev", got.ServerVersion)
	}
}

func TestV1MetaReadsFeatureFlags(t *testing.T) {
	s := &Server{Cfg: &config.Config{Client: config.ClientFeatures{
		V1Enabled:         true,
		DeviceAuthEnabled: true,
		DirectHTTPEnabled: true,
	}}}
	rec := httptest.NewRecorder()
	s.handleV1Meta(rec, httptest.NewRequest(http.MethodGet, "/api/v1/meta", nil))

	var got v1MetaResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !got.Features.ClientV1Enabled || !got.Features.DeviceAuthEnabled || !got.Features.DirectHTTPEnabled {
		t.Errorf("configured features not reflected: %#v", got.Features)
	}
	if got.Features.PlaybackTicketEnabled || got.Features.LocalRelayEnabled {
		t.Errorf("unconfigured features should remain false: %#v", got.Features)
	}
}

func TestV1MetaMatchesGoldenFixture(t *testing.T) {
	rec := httptest.NewRecorder()
	(&Server{}).handleV1Meta(rec, httptest.NewRequest(http.MethodGet, "/api/v1/meta", nil))

	want, err := os.ReadFile("../../protocol/fixtures/meta.json")
	if err != nil {
		t.Fatalf("read meta fixture: %v", err)
	}
	var gotJSON, wantJSON any
	if err := json.Unmarshal(rec.Body.Bytes(), &gotJSON); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if err := json.Unmarshal(want, &wantJSON); err != nil {
		t.Fatalf("decode meta fixture: %v", err)
	}
	if !reflect.DeepEqual(gotJSON, wantJSON) {
		t.Errorf("meta response does not match golden fixture:\n got: %s\nwant: %s", rec.Body.Bytes(), want)
	}
}

func TestLegacyAPIErrorEnvelopeUnchanged(t *testing.T) {
	rec := httptest.NewRecorder()
	(&Server{}).Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/items", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body) != 1 || body["error"] != "\u672a\u767b\u5f55" {
		t.Errorf("legacy error body = %v, want one error string", body)
	}
}

func sortedKeys[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
