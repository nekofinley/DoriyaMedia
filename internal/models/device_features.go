package models

import "time"

// DeviceFeatureOverrides is the per-device emergency-stop layer. Every field
// is a deny flag: a false value means "no override", and can only turn a
// globally enabled feature off. There is deliberately no client- or
// request-supplied way to use this type to re-enable a globally disabled
// feature.
type DeviceFeatureOverrides struct {
	ClientV1Disabled       bool `json:"client_v1_disabled"`
	DeviceAuthDisabled     bool `json:"device_auth_disabled"`
	PlaybackTicketDisabled bool `json:"playback_ticket_disabled"`
	DirectHTTPDisabled     bool `json:"direct_http_disabled"`
	ServerProxyV1Disabled  bool `json:"server_proxy_v1_disabled"`
	ServerHLSV1Disabled    bool `json:"server_hls_v1_disabled"`
	DirectDASHDisabled     bool `json:"direct_dash_disabled"`
	LocalRelayDisabled     bool `json:"local_relay_disabled"`
	NativePlayerDisabled   bool `json:"native_player_disabled"`
	LibMPVFallbackDisabled bool `json:"libmpv_fallback_disabled"`
}

// Empty reports whether the device has no deny override.
func (o DeviceFeatureOverrides) Empty() bool {
	return o == DeviceFeatureOverrides{}
}

// DisablesPlayback reports whether this override requires all existing
// playback tickets and capabilities for the device to be revoked. Every
// current deny flag has either playback or device-auth consequences, so any
// non-empty override is treated as an emergency stop for issued capabilities.
func (o DeviceFeatureOverrides) DisablesPlayback() bool {
	return o.ClientV1Disabled ||
		o.DeviceAuthDisabled ||
		o.PlaybackTicketDisabled ||
		o.DirectHTTPDisabled ||
		o.ServerProxyV1Disabled ||
		o.ServerHLSV1Disabled ||
		o.DirectDASHDisabled ||
		o.LocalRelayDisabled ||
		o.NativePlayerDisabled ||
		o.LibMPVFallbackDisabled
}

// DisablesDeviceAuth reports whether the device may use or rotate its S2
// credentials. ClientV1Disabled subsumes device auth because all v1 routes,
// including refresh and logout, must fail closed.
func (o DeviceFeatureOverrides) DisablesDeviceAuth() bool {
	return o.ClientV1Disabled || o.DeviceAuthDisabled
}

// DisablesPlaybackTickets reports whether new playback tickets and every
// ticket state transition must be rejected. Transport-specific disables do not
// belong here: they narrow new ticket choices after existing capabilities have
// been revoked.
func (o DeviceFeatureOverrides) DisablesPlaybackTickets() bool {
	return o.ClientV1Disabled || o.DeviceAuthDisabled || o.PlaybackTicketDisabled
}

// DisablesPlaybackTransport reports whether one server-issued playback
// transport is denied for this device.
func (o DeviceFeatureOverrides) DisablesPlaybackTransport(transport string) bool {
	switch transport {
	case PlaybackTransportDirectHTTP:
		return o.DirectHTTPDisabled
	case PlaybackTransportServerProxy:
		return o.ServerProxyV1Disabled
	case PlaybackTransportServerHLS:
		return o.ServerHLSV1Disabled
	default:
		return false
	}
}

// ClientDevice is the admin-facing safe projection used to manage native
// client installations. It contains no token, token hash, IP address, or
// source credential.
type ClientDevice struct {
	ID            string                 `json:"id"`
	Name          string                 `json:"name"`
	Platform      string                 `json:"platform"`
	ClientVersion string                 `json:"client_version"`
	UserID        int64                  `json:"user_id"`
	UserName      string                 `json:"user_name"`
	CreatedAt     time.Time              `json:"created_at"`
	LastSeenAt    time.Time              `json:"last_seen_at"`
	RevokedAt     *time.Time             `json:"revoked_at,omitempty"`
	Overrides     DeviceFeatureOverrides `json:"overrides"`
}
