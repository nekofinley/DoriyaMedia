package models

import "time"

// Device authorization lifecycle states. Only an authorized, unexpired
// authorization may be exchanged for a token pair, and that transition is
// guarded by the database state itself.
const (
	DeviceAuthorizationPending    = "pending"
	DeviceAuthorizationAuthorized = "authorized"
	DeviceAuthorizationDenied     = "denied"
	DeviceAuthorizationConsumed   = "consumed"
	DeviceAuthorizationExpired    = "expired"
)

// DeviceAuthorizationRequest is the public metadata a native client submits
// before a user approves it in the browser. It contains a PKCE challenge, never
// a verifier.
type DeviceAuthorizationRequest struct {
	DeviceName          string `json:"device_name"`
	Platform            string `json:"platform"`
	ClientVersion       string `json:"client_version"`
	CodeChallenge       string `json:"code_challenge"`
	CodeChallengeMethod string `json:"code_challenge_method"`
}

// DeviceAuthorization is safe to show on the approval page. The database hashes
// are deliberately absent, so looking it up can never disclose device_code.
type DeviceAuthorization struct {
	ID              int64      `json:"-"`
	AuthorizationID string     `json:"authorization_id"`
	DeviceName      string     `json:"device_name"`
	Platform        string     `json:"platform"`
	ClientVersion   string     `json:"client_version"`
	State           string     `json:"state"`
	UserID          *int64     `json:"user_id,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	ExpiresAt       time.Time  `json:"expires_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	AuthorizedAt    *time.Time `json:"authorized_at,omitempty"`
	ConsumedAt      *time.Time `json:"consumed_at,omitempty"`
}

// DeviceAuthorizationGrant is returned exactly once when a client starts a
// device authorization. DeviceCode and UserCode are plaintext only here; the
// store keeps their SHA-256 hashes.
type DeviceAuthorizationGrant struct {
	Authorization *DeviceAuthorization `json:"authorization"`
	DeviceCode    string               `json:"device_code"`
	UserCode      string               `json:"user_code"`
	ExpiresAt     time.Time            `json:"expires_at"`
}

// DeviceAuthorizationCSRF is the short-lived approval capability returned by
// the authenticated Web lookup. Token is plaintext only in memory and in that
// response; the database stores its hash.
type DeviceAuthorizationCSRF struct {
	Token     string    `json:"csrf_token"`
	ExpiresAt time.Time `json:"-"`
}

// Device is a stable public identity for one native-client installation.
type Device struct {
	ID            int64      `json:"-"`
	PublicID      string     `json:"id"`
	UserID        int64      `json:"user_id"`
	Name          string     `json:"name"`
	Platform      string     `json:"platform"`
	ClientVersion string     `json:"client_version"`
	CreatedAt     time.Time  `json:"created_at"`
	LastSeenAt    time.Time  `json:"last_seen_at"`
	RevokedAt     *time.Time `json:"revoked_at,omitempty"`
}

// DeviceTokenPair contains plaintext bearer credentials for the response. The
// Database only ever receives their hashes.
type DeviceTokenPair struct {
	AccessToken      string    `json:"access_token"`
	RefreshToken     string    `json:"refresh_token"`
	TokenType        string    `json:"token_type"`
	AccessExpiresAt  time.Time `json:"access_expires_at"`
	RefreshExpiresAt time.Time `json:"refresh_expires_at"`
	// RefreshExpiresIn is relative to the issuance time and may be shorter than
	// the normal 30-day window near the family's absolute deadline.
	RefreshExpiresIn int     `json:"-"`
	Device           *Device `json:"device"`
}

// DeviceAuthContext is the authenticated user and device behind a valid access
// token. API handlers can authorize playback without looking at raw token
// storage.
type DeviceAuthContext struct {
	User            *User                  `json:"user"`
	Device          *Device                `json:"device"`
	Overrides       DeviceFeatureOverrides `json:"-"`
	AccessExpiresAt time.Time              `json:"access_expires_at"`
}
