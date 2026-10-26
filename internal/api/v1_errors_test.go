package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestV1ErrorCodesAndStatuses(t *testing.T) {
	tests := []struct {
		code     V1ErrorCode
		wantCode string
		status   int
	}{
		{V1ErrorInvalidRequest, "invalid_request", http.StatusBadRequest},
		{V1ErrorUnauthenticated, "unauthenticated", http.StatusUnauthorized},
		{V1ErrorForbidden, "forbidden", http.StatusForbidden},
		{V1ErrorFeatureDisabled, "feature_disabled", http.StatusNotFound},
		{V1ErrorClientUpgradeRequired, "client_upgrade_required", http.StatusUpgradeRequired},
		{V1ErrorDeviceRevoked, "device_revoked", http.StatusUnauthorized},
		{V1ErrorAuthorizationDenied, "authorization_denied", http.StatusForbidden},
		{V1ErrorAuthorizationExpired, "authorization_expired", http.StatusGone},
		{V1ErrorAuthorizationConsumed, "authorization_consumed", http.StatusConflict},
		{V1ErrorAuthorizationNotFound, "authorization_not_found", http.StatusNotFound},
		{V1ErrorInvalidGrant, "invalid_grant", http.StatusBadRequest},
		{V1ErrorRefreshTokenInvalid, "refresh_token_invalid", http.StatusUnauthorized},
		{V1ErrorRefreshReuseDetected, "refresh_reuse_detected", http.StatusConflict},
		{V1ErrorDeviceNotFound, "device_not_found", http.StatusNotFound},
		{V1ErrorTicketNotFound, "ticket_not_found", http.StatusNotFound},
		{V1ErrorTicketExpired, "ticket_expired", http.StatusGone},
		{V1ErrorUnsupportedMedia, "unsupported_media", http.StatusUnsupportedMediaType},
		{V1ErrorSourceUnavailable, "source_unavailable", http.StatusBadGateway},
		{V1ErrorTransportDisabled, "transport_disabled", http.StatusForbidden},
		{V1ErrorMediaSessionNotFound, "media_session_not_found", http.StatusNotFound},
		{V1ErrorMediaSessionExpired, "media_session_expired", http.StatusGone},
		{V1ErrorRateLimited, "rate_limited", http.StatusTooManyRequests},
		{V1ErrorInternal, "internal_error", http.StatusInternalServerError},
	}

	for _, tc := range tests {
		t.Run(tc.wantCode, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeV1Error(rec, tc.status, tc.code, "display message", nil)

			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d", rec.Code, tc.status)
			}
			var envelope map[string]map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if len(envelope) != 1 {
				t.Fatalf("top-level fields = %v, want only error", envelope)
			}
			if got := envelope["error"]["code"]; got != tc.wantCode {
				t.Errorf("code = %v, want %q", got, tc.wantCode)
			}
			if got := envelope["error"]["message"]; got != "display message" {
				t.Errorf("message = %v, want display message", got)
			}
			if _, present := envelope["error"]["details"]; present {
				t.Error("details should be omitted when nil")
			}
		})
	}
}

func TestWriteV1ErrorIncludesDetails(t *testing.T) {
	rec := httptest.NewRecorder()
	writeV1Error(rec, http.StatusTooManyRequests, V1ErrorRateLimited, "slow down", map[string]any{
		"retry_after_seconds": 30,
	})

	var got v1ErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	want := v1ErrorEnvelope{
		Error: v1ErrorBody{
			Code:    V1ErrorRateLimited,
			Message: "slow down",
			Details: map[string]any{"retry_after_seconds": float64(30)},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("envelope = %#v, want %#v", got, want)
	}
}

func TestV1ErrorFixturesMatchEnvelope(t *testing.T) {
	statuses := map[V1ErrorCode]int{
		// The only invalid_request fixture describes a refresh attempted while
		// probing is in progress. The code is also used with 400 elsewhere;
		// fixtures are keyed by code, so this test records the specific status
		// carried by the golden response.
		V1ErrorInvalidRequest:        http.StatusConflict,
		V1ErrorAuthorizationConsumed: http.StatusConflict,
		V1ErrorAuthorizationDenied:   http.StatusForbidden,
		V1ErrorAuthorizationExpired:  http.StatusGone,
		V1ErrorClientUpgradeRequired: http.StatusUpgradeRequired,
		V1ErrorDeviceRevoked:         http.StatusUnauthorized,
		V1ErrorRateLimited:           http.StatusTooManyRequests,
		V1ErrorRefreshTokenInvalid:   http.StatusUnauthorized,
		V1ErrorRefreshReuseDetected:  http.StatusConflict,
		V1ErrorSourceUnavailable:     http.StatusBadGateway,
		V1ErrorTicketNotFound:        http.StatusNotFound,
		V1ErrorTicketExpired:         http.StatusGone,
		V1ErrorUnsupportedMedia:      http.StatusUnsupportedMediaType,
		V1ErrorTransportDisabled:     http.StatusForbidden,
		V1ErrorMediaSessionNotFound:  http.StatusNotFound,
		V1ErrorMediaSessionExpired:   http.StatusGone,
		V1ErrorForbidden:             http.StatusForbidden,
	}
	files, err := filepath.Glob("../../protocol/fixtures/error-*.json")
	if err != nil {
		t.Fatalf("list error fixtures: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no error fixtures found")
	}

	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			var want v1ErrorEnvelope
			if err := json.Unmarshal(raw, &want); err != nil {
				t.Fatalf("decode fixture: %v", err)
			}
			status, ok := statuses[want.Error.Code]
			if !ok {
				t.Fatalf("fixture code %q has no test status", want.Error.Code)
			}

			rec := httptest.NewRecorder()
			writeV1Error(rec, status, want.Error.Code, want.Error.Message, want.Error.Details)

			var got v1ErrorEnvelope
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("response = %#v, want fixture %#v", got, want)
			}
		})
	}
}
