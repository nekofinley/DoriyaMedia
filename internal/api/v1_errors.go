package api

import "net/http"

// V1ErrorCode is a stable machine-readable error identifier. Display messages
// may change, but clients must branch only on these values.
type V1ErrorCode string

const (
	V1ErrorInvalidRequest        V1ErrorCode = "invalid_request"
	V1ErrorUnauthenticated       V1ErrorCode = "unauthenticated"
	V1ErrorForbidden             V1ErrorCode = "forbidden"
	V1ErrorFeatureDisabled       V1ErrorCode = "feature_disabled"
	V1ErrorClientUpgradeRequired V1ErrorCode = "client_upgrade_required"
	V1ErrorDeviceRevoked         V1ErrorCode = "device_revoked"
	V1ErrorAuthorizationDenied   V1ErrorCode = "authorization_denied"
	V1ErrorAuthorizationExpired  V1ErrorCode = "authorization_expired"
	V1ErrorAuthorizationConsumed V1ErrorCode = "authorization_consumed"
	V1ErrorAuthorizationNotFound V1ErrorCode = "authorization_not_found"
	V1ErrorInvalidGrant          V1ErrorCode = "invalid_grant"
	V1ErrorRefreshTokenInvalid   V1ErrorCode = "refresh_token_invalid"
	V1ErrorRefreshReuseDetected  V1ErrorCode = "refresh_reuse_detected"
	V1ErrorDeviceNotFound        V1ErrorCode = "device_not_found"
	V1ErrorTicketNotFound        V1ErrorCode = "ticket_not_found"
	V1ErrorTicketExpired         V1ErrorCode = "ticket_expired"
	V1ErrorUnsupportedMedia      V1ErrorCode = "unsupported_media"
	V1ErrorSourceUnavailable     V1ErrorCode = "source_unavailable"
	V1ErrorTransportDisabled     V1ErrorCode = "transport_disabled"
	V1ErrorMediaSessionNotFound  V1ErrorCode = "media_session_not_found"
	V1ErrorMediaSessionExpired   V1ErrorCode = "media_session_expired"
	V1ErrorRateLimited           V1ErrorCode = "rate_limited"
	V1ErrorInternal              V1ErrorCode = "internal_error"
)

type v1ErrorEnvelope struct {
	Error v1ErrorBody `json:"error"`
}

type v1ErrorBody struct {
	Code    V1ErrorCode    `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// writeV1Error writes the API v1 error envelope without changing the legacy
// /api error shape.
func writeV1Error(w http.ResponseWriter, status int, code V1ErrorCode, message string, details map[string]any) {
	writeJSON(w, status, v1ErrorEnvelope{
		Error: v1ErrorBody{
			Code:    code,
			Message: message,
			Details: details,
		},
	})
}
